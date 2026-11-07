package picker

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"ofa/internal/banner"
	"ofa/internal/resolve"
	"ofa/internal/update"
)

// Tests for the single interactive lifecycle (issue #62): scan → select →
// apply → summary inside one bubbletea program. Everything here is driven
// through the model's Update with messages — no TTY, no real installers —
// plus two RunFlow integration tests with fake streams that pin the
// worker-join and summary-gating behavior.

// syncBuffer is a goroutine-safe capture buffer: the tea program renders
// from its own goroutine while a test reads the frames.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForOutput blocks until the rendered output contains substr. The
// frames are stable (the string stays rendered), so a one-way poll is a
// synchronization point, not a sleep. Bounded at five seconds.
func waitForOutput(t *testing.T, b *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output never contained %q:\n%s", substr, b.String())
}

// updateMsg drives the model through one non-key message.
func updateMsg(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	updated, _ := m.Update(msg)
	got, ok := updated.(model)
	if !ok {
		t.Fatalf("Update(%T) returned wrong type %T", msg, updated)
	}
	return got
}

// scanFixture builds a flow state with a minimal config and a model at
// the scanning screen — the starting point of every flow run.
func scanFixture() (*flowState, model) {
	f := &flowState{
		cfg: &FlowConfig{
			Build: func([]resolve.Resolution) ([]update.Request, []SkippedRow) { return nil, nil },
			Exec:  func(req update.Request) update.Result { return update.Result{Label: req.Label} },
		},
		q:         newMsgQueue(),
		scanStop:  make(chan struct{}),
		applyStop: make(chan struct{}),
	}
	return f, newFlowModel(f)
}

// flowFixture builds a flow model already at the select screen (scan
// answered), with the same three items fixture() uses.
func flowFixture(t *testing.T) (*flowState, model) {
	t.Helper()
	f, m := scanFixture()
	m = updateMsg(t, m, scanResultMsg{res: ScanResult{Resolutions: []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}}})
	if m.phase != phaseSelect {
		t.Fatalf("fixture phase = %v, want select", m.phase)
	}
	return f, m
}

// --- scanning screen (issue #43) ---

// TestFlowModel_ScanFrameShowsBannerCaptionTagline pins issue #43's
// completion: the OFA banner, the exact caption and the short tagline are
// on the scanning screen — on every run, first run included (the same
// frame serves the first run; there is no separate plain notice).
func TestFlowModel_ScanFrameShowsBannerCaptionTagline(t *testing.T) {
	t.Setenv("NO_COLOR", "") // hermetic: ambient NO_COLOR must not decide this
	_, m := scanFixture()
	v := m.View().Content
	for _, want := range []string{
		ScanCaption,
		"Scanning System for Harnesses...",
		ScanTagline,
		ScanDetail,
		"q/ctrl+c stop scanning",
	} {
		if !contains(v, want) {
			t.Errorf("scan frame missing %q\n%s", want, v)
		}
	}
	// The banner's baked colors are the art's presence marker, same as
	// the select screen's TestView_HeaderIsBanner.
	if !contains(v, "\x1b[38;2;") {
		t.Errorf("scan frame should carry the banner art\n%s", v)
	}
	bannerIdx := indexOf(v, "\x1b[38;2;")
	capIdx := indexOf(v, ScanCaption)
	if bannerIdx < 0 || capIdx < 0 || bannerIdx > capIdx {
		t.Errorf("banner should appear before the caption (banner@%d, caption@%d)", bannerIdx, capIdx)
	}
}

// TestFlowModel_ScanFrameAnimatesSpinner: the scanning screen animates
// with the braille cycle the old stderr spinner used.
func TestFlowModel_ScanFrameAnimatesSpinner(t *testing.T) {
	_, m := scanFixture()
	frame := m.frame()
	if !contains(frame, "⠋") {
		t.Errorf("scan frame = %q, want a braille spinner frame", frame)
	}
	m = updateMsg(t, m, animTickMsg{})
	if !contains(m.frame(), spinnerFrame(m.anim)) {
		t.Errorf("scan frame = %q, want the next spinner frame %q", m.frame(), spinnerFrame(m.anim))
	}
}

// TestFlowModel_BannerNOColorMatchesBannerRender: the in-TUI banner
// honors NO_COLOR exactly as banner.Render does on the plain paths (issue
// #43 AC7): same art, plain, with colors intact when NO_COLOR is unset or
// empty.
func TestFlowModel_BannerNOColorMatchesBannerRender(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, m := scanFixture()
	v := m.View().Content
	if strings.Contains(v, "\x1b[38;2;") {
		t.Errorf("NO_COLOR left banner colors in the scan frame\n%s", v)
	}
	for _, line := range strings.Split(banner.Plain(), "\n") {
		if line != "" && !contains(v, line) {
			t.Errorf("NO_COLOR scan frame missing plain banner line %q", line)
		}
	}
	if !contains(v, ScanCaption) {
		t.Errorf("NO_COLOR scan frame missing the caption")
	}

	t.Setenv("NO_COLOR", "") // present but empty: no-color.org keeps colors
	_, m2 := scanFixture()
	if !contains(m2.View().Content, "\x1b[38;2;") {
		t.Error("empty NO_COLOR must keep the banner colors")
	}
}

// TestFlowModel_ScanDiagnosticsShownAndCounted: diagnostics surface in
// the TUI and anything elided is counted — never silently dropped.
func TestFlowModel_ScanDiagnosticsShownAndCounted(t *testing.T) {
	_, m := scanFixture()
	for i := 0; i < 12; i++ {
		m = updateMsg(t, m, diagMsg{line: "diag-line-" + string(rune('a'+i))})
	}
	v := m.frame()
	if !contains(v, "diag-line-l") {
		t.Errorf("scan frame should show the most recent diagnostics\n%s", v)
	}
	if !contains(v, "4 earlier diagnostic line(s)") {
		t.Errorf("scan frame should count elided diagnostics\n%s", v)
	}
	if contains(v, "diag-line-a") {
		t.Errorf("scan frame should elide the oldest lines\n%s", v)
	}
}

// TestFlowModel_ScanQuitRequestsStopThenExitZero: q/ctrl+c during the
// scan requests a stop and waits — it does not tear down while the
// worker may still write. The frame says so; the exit (0, the quit
// semantic) happens only once the worker's result has arrived.
func TestFlowModel_ScanQuitRequestsStopThenExitZero(t *testing.T) {
	f, m := scanFixture()
	m = updateKey(t, m, "q")
	select {
	case <-f.scanStop:
	default:
		t.Fatal("q during scan must close the scan stop channel")
	}
	if m.quitting {
		t.Error("q during scan must not quit before the worker is joined")
	}
	if !contains(m.frame(), "Stopping — waiting for the scan to finish…") {
		t.Errorf("scan frame = %q, want the stopping status", m.frame())
	}
	// The worker finishes (even with a failure code) and the flow exits 0.
	m = updateMsg(t, m, scanResultMsg{res: ScanResult{Code: 2}})
	if !m.quitting {
		t.Error("after the stopped scan reports, the flow must quit")
	}
	if m.exitCode != 0 {
		t.Errorf("canceled scan exit = %d, want 0 (the quit semantic)", m.exitCode)
	}
	if v := m.View().Content; v != "" {
		t.Errorf("quitting View = %q, want empty", v)
	}
}

// TestFlowModel_ScanErrorIsDismissibleWithOldExitCode: a failed scan
// lands on a dismissible screen carrying the caller's exit code (e.g. 2),
// and dismisses with q / enter / ctrl+c.
func TestFlowModel_ScanErrorIsDismissibleWithOldExitCode(t *testing.T) {
	for _, key := range []string{"q", "enter", "ctrl+c"} {
		_, m := scanFixture()
		m = updateMsg(t, m, diagMsg{line: "ofa: catalog: broken"})
		m = updateMsg(t, m, scanResultMsg{res: ScanResult{Code: 2}})
		if m.quitting {
			t.Fatal("the error screen must wait for dismissal")
		}
		if m.exitCode != 2 {
			t.Errorf("exit = %d, want the scan's code 2", m.exitCode)
		}
		v := m.frame()
		if !contains(v, "Scan failed.") || !contains(v, "ofa: catalog: broken") {
			t.Errorf("error screen = %q, want the message and the diagnostics", v)
		}
		m = updateKey(t, m, key)
		if !m.quitting || m.View().Content != "" {
			t.Errorf("key %q must dismiss the error screen (quitting=%v)", key, m.quitting)
		}
	}
}

// TestFlowModel_EmptyScanIsDismissibleExitZero: nothing to update is a
// dismissible in-TUI state with the old exit code (0), not silent output.
func TestFlowModel_EmptyScanIsDismissibleExitZero(t *testing.T) {
	_, m := scanFixture()
	m = updateMsg(t, m, scanResultMsg{res: ScanResult{}})
	if m.quitting || m.exitCode != 0 {
		t.Fatalf("empty scan: quitting=%v exit=%d, want waiting/0", m.quitting, m.exitCode)
	}
	if !contains(m.frame(), "No updates available.") {
		t.Errorf("empty screen = %q, want the message", m.frame())
	}
	m = updateKey(t, m, "q")
	if !m.quitting {
		t.Error("q must dismiss the empty screen")
	}
}

// --- apply screen ---

// TestFlowModel_SelectEnterBuildsStableRows: Enter builds one stable row
// per selected update — pre-skipped ones included — and moves to apply.
func TestFlowModel_SelectEnterBuildsStableRows(t *testing.T) {
	stubLookPathAllowed(t) // the apply worker's PATH preflight must be hermetic
	f, m := flowFixture(t)
	f.cfg.Build = func(selected []resolve.Resolution) ([]update.Request, []SkippedRow) {
		if len(selected) != 2 {
			t.Errorf("Build got %d selections, want 2", len(selected))
		}
		return []update.Request{
				{Label: "claude (native)", Cmd: update.CmdSpec{Cmd: []string{"x"}}},
				{Label: "qwen (npm)", Cmd: update.CmdSpec{Cmd: []string{"y"}}},
			}, []SkippedRow{
				{Label: "pinned one", Reason: "pinned in config"},
			}
	}
	m = updateKey(t, m, "a") // select all
	m = updateKey(t, m, "enter")
	if m.phase != phaseApply {
		t.Fatalf("phase = %v, want apply", m.phase)
	}
	if len(m.rows) != 3 {
		t.Fatalf("rows = %d, want 3 (one per selected update)", len(m.rows))
	}
	if m.rows[0].state != rowSkipped || m.rows[0].reason != "pinned in config" || m.rows[0].reqIdx != -1 {
		t.Errorf("pre-skipped row = %+v, want skipped/-1 with reason", m.rows[0])
	}
	if m.rows[1].state != rowQueued || m.rows[2].state != rowQueued {
		t.Errorf("request rows = %+v / %+v, want queued", m.rows[1], m.rows[2])
	}
	v := m.frame()
	for _, want := range []string{"Applying selected updates", "claude (native)", "qwen (npm)", "pinned one", "q/ctrl+c stop after current update"} {
		if !contains(v, want) {
			t.Errorf("apply frame missing %q\n%s", want, v)
		}
	}
	// Join the apply worker before returning: the ExecLookPath seam must
	// only be restored after the worker can no longer touch it.
	f.wg.Wait()
}

// TestFlowModel_EmptySelectionEnterShowsNothingSelected: the old
// "nothing selected" behavior is a dismissible screen with exit 0, and
// Build is never called.
func TestFlowModel_EmptySelectionEnterShowsNothingSelected(t *testing.T) {
	f, m := flowFixture(t)
	f.cfg.Build = func([]resolve.Resolution) ([]update.Request, []SkippedRow) {
		t.Error("Build must not be called with an empty selection")
		return nil, nil
	}
	m = updateKey(t, m, "enter")
	if m.phase != phaseSummary || m.exitCode != 0 {
		t.Fatalf("phase=%v exit=%d, want summary/0", m.phase, m.exitCode)
	}
	if !contains(m.frame(), "Nothing selected.") {
		t.Errorf("summary = %q, want the message", m.frame())
	}
}

// TestFlowModel_EventRowsLifecycle: events drive rows queued → running →
// final state, identity by request index. Labels may repeat; rows never
// merge.
func TestFlowModel_EventRowsLifecycle(t *testing.T) {
	_, m := flowFixture(t)
	m.phase = phaseApply
	m.rows = []applyRow{
		{label: "claude (native)", state: rowQueued, reqIdx: 0},
		{label: "claude (native)", state: rowQueued, reqIdx: 1},
		{label: "claude (native)", state: rowQueued, reqIdx: 2},
	}
	m = updateMsg(t, m, eventMsg{ev: update.Event{Type: update.EventStarted, Index: 0, Label: "claude (native)"}})
	if m.rows[0].state != rowRunning || m.rows[1].state != rowQueued {
		t.Errorf("rows after start = %+v", m.rows)
	}
	m = updateMsg(t, m, eventMsg{ev: update.Event{Type: update.EventFinished, Index: 0, Label: "claude (native)",
		Result: &update.Result{Label: "claude (native)"}}})
	m = updateMsg(t, m, eventMsg{ev: update.Event{Type: update.EventFinished, Index: 1, Label: "claude (native)",
		Result: &update.Result{Label: "claude (native)", Failed: true, ExitCode: 7, Err: errors.New("boom"), OutputTail: "last line"}}})
	m = updateMsg(t, m, eventMsg{ev: update.Event{Type: update.EventFinished, Index: 2, Label: "claude (native)",
		Result: &update.Result{Label: "claude (native)", Skipped: true, SkippedReason: "canceled"}}})
	if m.rows[0].state != rowDone {
		t.Errorf("row 0 = %+v, want done", m.rows[0])
	}
	if m.rows[1].state != rowFailed || m.rows[1].tail != "last line" || !strings.Contains(m.rows[1].reason, "exit 7") {
		t.Errorf("row 1 = %+v, want failed with tail and exit note", m.rows[1])
	}
	if m.rows[2].state != rowSkipped || m.rows[2].reason != "canceled" {
		t.Errorf("row 2 = %+v, want skipped/canceled", m.rows[2])
	}
}

// TestBar_RunningIsIndeterminateWithoutNumbers: the bar is a visual aid,
// never a percentage no installer reported.
func TestBar_RunningIsIndeterminateWithoutNumbers(t *testing.T) {
	numOrPercent := regexp.MustCompile(`[0-9%]`)
	for anim := 0; anim < barWidth*3; anim++ {
		b := bar(rowRunning, anim)
		if numOrPercent.MatchString(b) {
			t.Errorf("running bar %q contains a number or percent", b)
		}
		if len(b) != barWidth+2 || b[0] != '[' || b[len(b)-1] != ']' {
			t.Errorf("running bar %q is not a %d-cell bar", b, barWidth)
		}
		if !strings.Contains(b, "=") || !strings.Contains(b, "-") {
			t.Errorf("running bar %q is not an indeterminate slider (no moving segment over an unfilled track)", b)
		}
	}
	for _, st := range []rowState{rowQueued, rowRunning, rowDone, rowFailed, rowSkipped} {
		if l := rowLine(applyRow{label: "x", state: st}, 3); numOrPercent.MatchString(l) && st != rowRunning {
			// state words carry no digits; only reason text may, which is
			// empty here — so no row line may show a number.
			t.Errorf("row line %q contains a number or percent", l)
		}
	}
	if b := bar(rowDone, 0); b != "[##########]" {
		t.Errorf("done bar = %q, want full", b)
	}
}

// TestFlowModel_ApplyStopShowsStoppingState: q/ctrl+c during apply is a
// stop-after-current request, not an abandon. The frame says exactly
// what happens.
func TestFlowModel_ApplyStopShowsStoppingState(t *testing.T) {
	f, m := flowFixture(t)
	m.phase = phaseApply
	m.rows = []applyRow{
		{label: "a", state: rowRunning, reqIdx: 0},
		{label: "b", state: rowQueued, reqIdx: 1},
	}
	m = updateKey(t, m, "q")
	select {
	case <-f.applyStop:
	default:
		t.Fatal("q during apply must close the apply stop channel")
	}
	if m.quitting {
		t.Error("q during apply must not quit")
	}
	v := m.frame()
	for _, want := range []string{"Stopping after current update…", "Current update will finish. Remaining updates are skipped as canceled."} {
		if !contains(v, want) {
			t.Errorf("apply frame missing %q\n%s", want, v)
		}
	}
	// ctrl+c is the same request; a second press changes nothing.
	m = updateKey(t, m, "ctrl+c")
	if m.quitting {
		t.Error("ctrl+c during apply must not quit either")
	}
}

// --- summary screen ---

// summaryFixture returns a model at the results summary after one done,
// one failed and one skipped update.
func summaryFixture(t *testing.T) (*flowState, model) {
	t.Helper()
	f, m := flowFixture(t)
	m.phase = phaseApply
	m.rows = []applyRow{
		{label: "claude (native)", state: rowDone, reqIdx: 0},
		{label: "codex (brew-cask)", state: rowFailed, reason: "exit 1, err=boom", tail: "tail line", reqIdx: 1},
		{label: "opencode (native)", state: rowSkipped, reason: "pinned in config", reqIdx: -1},
	}
	m = updateMsg(t, m, applyDoneMsg{summary: update.Summary{
		Succeeded: []update.Result{{Label: "claude (native)"}},
		Failed:    []update.Result{{Label: "codex (brew-cask)", Failed: true, ExitCode: 1, Err: errors.New("boom")}},
		Skipped:   []update.Result{{Label: "opencode (native)", Skipped: true, SkippedReason: "pinned in config"}},
	}})
	return f, m
}

// TestFlowModel_SummaryPersistsUntilDismissed: the summary shows counts
// and per-row results and stays visible; only q / enter / ctrl+c dismiss.
func TestFlowModel_SummaryPersistsUntilDismissed(t *testing.T) {
	_, m := summaryFixture(t)
	if m.phase != phaseSummary {
		t.Fatalf("phase = %v, want summary", m.phase)
	}
	if m.exitCode != 1 {
		t.Errorf("exit = %d, want 1 (Summary.ExitCode: a real failure)", m.exitCode)
	}
	v := m.frame()
	for _, want := range []string{
		"Updates finished",
		"claude (native)",
		"done",
		"failed",
		"skipped",
		"tail line",
		"pinned in config",
		"1 succeeded · 1 failed · 1 skipped",
		"Enter / q / Ctrl+C dismiss · summary stays visible until then.",
	} {
		if !contains(v, want) {
			t.Errorf("summary missing %q\n%s", want, v)
		}
	}
	// Gating: other keys do not dismiss; the summary stays.
	for _, key := range []string{"a", "space", "up", "x"} {
		m = updateKey(t, m, key)
		if m.quitting {
			t.Fatalf("key %q must not dismiss the summary", key)
		}
	}
	if m.frame() != v {
		t.Error("summary must be stable while keys are ignored")
	}
	for _, key := range []string{"q", "enter", "ctrl+c"} {
		mm := updateKey(t, m, key)
		if !mm.quitting {
			t.Errorf("key %q must dismiss the summary", key)
		}
		if got := mm.View().Content; got != "" {
			t.Errorf("dismissed View = %q, want empty", got)
		}
	}
}

// TestFlowModel_CanceledRowsDoNotFailTheExit: skipped/canceled rows never
// flip the exit code — only real failures do (Summary.ExitCode, unchanged).
func TestFlowModel_CanceledRowsDoNotFailTheExit(t *testing.T) {
	_, m := flowFixture(t)
	m.phase = phaseApply
	m.rows = []applyRow{
		{label: "a", state: rowDone, reqIdx: 0},
		{label: "b", state: rowSkipped, reason: "canceled", reqIdx: 1},
	}
	m = updateMsg(t, m, applyDoneMsg{summary: update.Summary{
		Succeeded: []update.Result{{Label: "a"}},
		Skipped:   []update.Result{{Label: "b", Skipped: true, SkippedReason: "canceled"}},
	}})
	if m.exitCode != 0 {
		t.Errorf("exit = %d, want 0 (canceled rows are skipped, not failed)", m.exitCode)
	}
}

// --- RunFlow integration (fake streams, no real installers) ---
//
// RunFlow blocks until the flow is dismissed, so each test drives the
// scripted keystrokes on the test goroutine while RunFlow runs beside it.

// TestRunFlow_ScanCancelJoinsWorkerBeforeReturning: a stopped scan waits
// for its worker — RunFlow returns only after the worker has finished —
// and exits 0.
func TestRunFlow_ScanCancelJoinsWorkerBeforeReturning(t *testing.T) {
	var scanReturned atomic.Bool
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	done := make(chan FlowResult, 1)
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan: func(diag io.Writer, stop <-chan struct{}) ScanResult {
				<-stop                            // a synchronous detector: finishes only when told to
				time.Sleep(20 * time.Millisecond) // bounded work in flight
				scanReturned.Store(true)
				return ScanResult{}
			},
			Build:  func([]resolve.Resolution) ([]update.Request, []SkippedRow) { return nil, nil },
			Exec:   func(req update.Request) update.Result { return update.Result{Label: req.Label} },
			Input:  pr,
			Output: out,
		})
		if err != nil {
			t.Error("RunFlow:", err)
		}
		done <- res
	}()

	waitForOutput(t, out, ScanCaption) // the scanning screen is up
	_, _ = pw.Write([]byte("q"))       // request the stop
	waitForOutput(t, out, "Stopping — waiting for the scan to finish…")
	res := <-done
	if !scanReturned.Load() {
		t.Error("RunFlow returned before the scan worker finished — the join is broken")
	}
	if res.Code != 0 {
		t.Errorf("canceled scan exit = %d, want 0", res.Code)
	}
}

// TestRunFlow_ApplyStopAfterCurrentSkipsRestAndGatesSummary: the full
// lifecycle over fake streams — select, apply, stop-after-current, and a
// summary that waits for dismissal. The second update is never started.
func TestRunFlow_ApplyStopAfterCurrentSkipsRestAndGatesSummary(t *testing.T) {
	stubLookPathAllowed(t) // RunFlow's apply worker PATH preflight
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	release := make(chan struct{})
	var ranSecond atomic.Bool
	done := make(chan FlowResult, 1)
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan: func(diag io.Writer, stop <-chan struct{}) ScanResult {
				return ScanResult{Resolutions: []resolve.Resolution{
					{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "1", Tier: "self"},
					{HarnessID: "qwen", Method: "npm", Bin: "qwen", Cur: "1", Latest: "2", Outdated: true, Tier: "npm"},
				}}
			},
			Build: func(selected []resolve.Resolution) ([]update.Request, []SkippedRow) {
				return []update.Request{
						{Label: "first", Cmd: update.CmdSpec{Cmd: []string{"x", "1"}}},
						{Label: "second", Cmd: update.CmdSpec{Cmd: []string{"x", "2"}}},
					}, []SkippedRow{
						{Label: "pinned one", Reason: "pinned in config"},
					}
			},
			Exec: func(req update.Request) update.Result {
				if req.Label == "second" {
					ranSecond.Store(true)
				}
				<-release // the first installer runs until the test releases it
				return update.Result{Label: req.Label, Cmd: req.Cmd}
			},
			Input:  pr,
			Output: out,
		})
		if err != nil {
			t.Error("RunFlow:", err)
		}
		done <- res
	}()

	waitForOutput(t, out, "enter apply") // the select screen is up
	_, _ = pw.Write([]byte("a\r"))       // select all, apply
	waitForOutput(t, out, "Applying selected updates")
	_, _ = pw.Write([]byte("q")) // stop after the current update
	waitForOutput(t, out, "Stopping after current update…")
	close(release) // the in-flight installer finishes
	waitForOutput(t, out, "Updates finished")
	// Summary gating: nothing auto-exits; the run waits for dismissal.
	select {
	case <-done:
		t.Fatal("RunFlow returned before the summary was dismissed")
	case <-time.After(100 * time.Millisecond):
	}
	_, _ = pw.Write([]byte("q")) // dismiss
	res := <-done
	if res.Code != 0 {
		t.Errorf("exit = %d, want 0 (canceled rows are skipped, not failed)", res.Code)
	}
	if ranSecond.Load() {
		t.Error("the second update ran; stop-after-current must skip it")
	}
	got := out.String()
	for _, want := range []string{"1 succeeded · 0 failed · 2 skipped", "canceled", "pinned in config"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

// stubLookPathAllowed makes the ApplyObserved PATH preflight pass for the
// fixture commands ("x", "y" — never executed; the Exec stub runs
// instead), so tests never depend on the machine's PATH. Restored via
// t.Cleanup, which runs only after the test body has returned — after
// RunFlow's worker join (or an explicit flowState join) — and no test in
// this package runs in parallel, so the seam is never shared.
func stubLookPathAllowed(t *testing.T) {
	t.Helper()
	orig := update.ExecLookPath
	update.ExecLookPath = func(string) (string, error) { return "/usr/bin/true", nil }
	t.Cleanup(func() { update.ExecLookPath = orig })
}

// TestFlowModel_ScanPinsAreAuthoritative: pins loaded by the scan travel
// with the resolutions and win over FlowConfig.Pins — including an empty
// map, so unpinning can never fall back to stale pins. Only a scan that
// carried no snapshot leaves the fixed pins in force.
func TestFlowModel_ScanPinsAreAuthoritative(t *testing.T) {
	res := []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "1", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Bin: "qwen", Cur: "1", Latest: "2", Outdated: true, Tier: "npm"},
	}

	// Freshly loaded pins beat the stale fixed set.
	_, m := scanFixture()
	m.f.cfg.Pins = map[string]bool{"qwen": true}
	m = updateMsg(t, m, scanResultMsg{res: ScanResult{Resolutions: res, Pins: map[string]bool{"claude": true}}})
	if !m.items[0].Pinned || m.items[1].Pinned {
		t.Errorf("scan pins must win: items = %+v", m.items)
	}

	// An empty (but present) snapshot means "nothing pinned".
	_, m2 := scanFixture()
	m2.f.cfg.Pins = map[string]bool{"claude": true}
	m2 = updateMsg(t, m2, scanResultMsg{res: ScanResult{Resolutions: res, Pins: map[string]bool{}}})
	if m2.items[0].Pinned {
		t.Errorf("empty scan pins are authoritative (unpinning): items = %+v", m2.items)
	}

	// No snapshot at all: the fixed FlowConfig.Pins stay in force.
	_, m3 := scanFixture()
	m3.f.cfg.Pins = map[string]bool{"qwen": true}
	m3 = updateMsg(t, m3, scanResultMsg{res: ScanResult{Resolutions: res}})
	if !m3.items[1].Pinned {
		t.Errorf("fixed pins must remain without a scan snapshot: items = %+v", m3.items)
	}
}
