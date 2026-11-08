package picker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
func waitForOutput(t *testing.T, b interface{ String() string }, substr string) {
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
			Build: func([]resolve.Resolution) FlowPlan { return FlowPlan{} },
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
// TestFlowModel_DiagnosticsAllReachable: no diagnostic is ever dropped
// (issue #62 review B8): the scan body pages every line, scrolling
// reaches the oldest, and the log view (l) shows the complete set from
// any phase — there is no elision counter because nothing is elided.
func TestFlowModel_DiagnosticsAllReachable(t *testing.T) {
	_, m := scanFixture()
	m = updateMsg(t, m, tea.WindowSizeMsg{Width: 80, Height: 12}) // a small screen: only a window fits
	for i := 0; i < 12; i++ {
		m = updateMsg(t, m, diagMsg{line: "diag-line-" + string(rune('a'+i))})
	}
	// The live view shows the newest lines at the bottom of the viewport.
	if v := m.frame(); !contains(v, "diag-line-l") {
		t.Errorf("scan frame should show the most recent diagnostics\n%s", v)
	}
	// Scrolling back reaches the oldest line — paging, never truncation.
	m = updateMsg(t, m, keyMsg("pgup"))
	if v := m.frame(); !contains(v, "diag-line-a") {
		t.Errorf("scrolling up should reach the oldest diagnostic\n%s", v)
	}
	// The log view renders every line (unwindowed when height is unknown).
	_, lm := scanFixture()
	for i := 0; i < 12; i++ {
		lm = updateMsg(t, lm, diagMsg{line: "diag-line-" + string(rune('a'+i))})
	}
	lm = updateMsg(t, lm, keyMsg("l"))
	v := lm.frame()
	for i := 0; i < 12; i++ {
		want := "diag-line-" + string(rune('a'+i))
		if !contains(v, want) {
			t.Errorf("log view dropped %q", want)
		}
	}
	if !contains(v, "Diagnostics — 12 line(s), all shown") {
		t.Errorf("log view should announce the complete set\n%s", v)
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
	f.cfg.Build = func(selected []resolve.Resolution) FlowPlan {
		if len(selected) != 2 {
			t.Errorf("Build got %d selections, want 2", len(selected))
		}
		return FlowPlan{
			Rows: []FlowRow{
				{RowID: 2, Label: "pinned one", RequestIndex: -1, Reason: "pinned in config"},
				{RowID: 0, Label: "claude (native)", RequestIndex: 0},
				{RowID: 1, Label: "qwen (npm)", RequestIndex: 1},
			},
			Requests: []update.Request{
				{Label: "claude (native)", Cmd: update.CmdSpec{Cmd: []string{"x"}}},
				{Label: "qwen (npm)", Cmd: update.CmdSpec{Cmd: []string{"y"}}},
			},
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
	f.cfg.Build = func([]resolve.Resolution) FlowPlan {
		t.Error("Build must not be called with an empty selection")
		return FlowPlan{}
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
		if l := rowLine(applyRow{label: "x", state: st}, 3, 0); numOrPercent.MatchString(l) && st != rowRunning {
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
		"q/enter/ctrl+c dismiss",
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
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing test must never wedge the worker: cleanup always releases.
	t.Cleanup(func() { pw.Close(); releaseAll() })
	done := make(chan FlowResult, 1)
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan: func(diag io.Writer, stop <-chan struct{}) ScanResult {
				<-stop // a synchronous detector: finishes only when told to
				// Bounded in-flight work — released by observing the stopping
				// state on screen, never by a timer (review A1).
				<-release
				scanReturned.Store(true)
				return ScanResult{}
			},
			Build:  func([]resolve.Resolution) FlowPlan { return FlowPlan{} },
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
	// The stopping state must be observable before the worker can finish:
	// that is the sync point — release only once it is on screen.
	waitForOutput(t, out, "Stopping — waiting for the scan to finish…")
	releaseAll()
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
			Build: func(selected []resolve.Resolution) FlowPlan {
				return FlowPlan{
					Rows: []FlowRow{
						{RowID: 2, Label: "pinned one", RequestIndex: -1, Reason: "pinned in config"},
						{RowID: 0, Label: "first", RequestIndex: 0},
						{RowID: 1, Label: "second", RequestIndex: 1},
					},
					Requests: []update.Request{
						{Label: "first", Cmd: update.CmdSpec{Cmd: []string{"x", "1"}}},
						{Label: "second", Cmd: update.CmdSpec{Cmd: []string{"x", "2"}}},
					},
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

// --- terminal failure boundaries (issue #62 review B1/B2 + EOF) ---

// errAfterKeysReader hands out one scripted chunk, then blocks until the
// first installer is in flight, then fails like a lost terminal.
type errAfterKeysReader struct {
	sendKeys     chan struct{} // closed when the select screen is up
	firstStarted chan struct{}
	err          error
	reads        int
}

func (r *errAfterKeysReader) Read(p []byte) (int, error) {
	r.reads++
	switch r.reads {
	case 1:
		<-r.sendKeys               // keys must not race the scan phase
		return copy(p, "a\r"), nil // select all, apply
	case 2:
		<-r.firstStarted
		return 0, r.err
	default:
		return 0, r.err
	}
}

// flakyWriter writes happily until armed, then fails every write and
// signals the first failure.
type flakyWriter struct {
	mu     sync.Mutex
	armed  bool
	failed chan struct{}
	once   sync.Once
	buf    syncBuffer // frames are observable until the writer is armed
}

// String reports everything written before the writer was armed.
func (w *flakyWriter) String() string { return w.buf.String() }

func (w *flakyWriter) arm() {
	w.mu.Lock()
	w.armed = true
	w.mu.Unlock()
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	armed := w.armed
	w.mu.Unlock()
	if armed {
		w.once.Do(func() { close(w.failed) })
		return 0, errors.New("output write failed")
	}
	return w.buf.Write(p)
}

// TestObservedStreams_RecordAndStopSynchronously: the first write error,
// short write, read error or EOF is recorded with its original cause and
// the idempotent stop is requested INSIDE the call that saw it — never
// deferred to a watcher. Repeats are no-ops (review B1/B2 bounds).
func TestObservedStreams_RecordAndStopSynchronously(t *testing.T) {
	// Read error / EOF.
	for _, err := range []error{io.EOF, errors.New("input died")} {
		stops := 0
		rec := &failureRecorder{ch: make(chan struct{}), stop: func() { stops++ }}
		or := &observedReader{r: errReader{err}, rec: rec}
		if _, got := or.Read(make([]byte, 4)); got != err {
			t.Errorf("Read error = %v, want the original %v", got, err)
		}
		if stops != 1 {
			t.Errorf("stop requests = %d, want 1 synchronous", stops)
		}
		if rec.failure() != err {
			t.Errorf("recorded error = %v, want the original %v", rec.failure(), err)
		}
		// Repeated failure: first error wins, stop stays single.
		_, _ = or.Read(make([]byte, 4))
		if stops != 1 || rec.failure() != err {
			t.Errorf("repeat changed state: stops=%d err=%v", stops, rec.failure())
		}
	}

	// Write error and short write.
	for _, tc := range []struct {
		w io.Writer
	}{
		{errWriter{errors.New("output died")}},
		{shortWriter{}},
	} {
		stops := 0
		rec := &failureRecorder{ch: make(chan struct{}), stop: func() { stops++ }}
		ow := &observedWriter{w: tc.w, rec: rec}
		if _, _ = ow.Write([]byte("frame")); stops != 1 {
			t.Errorf("stop requests = %d, want 1 synchronous", stops)
		}
		if rec.failure() == nil {
			t.Error("the original write failure must be recorded")
		}
	}
}

// errReader and friends are one-shot failure sources.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type errWriter struct{ err error }

func (w errWriter) Write(p []byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (w shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

// failurePlan is the shared fixture for the failure-boundary runs: two
// requests, the first in flight and released by the test, the second
// tracked for "never started after the failure".
type failurePlan struct {
	firstStarted chan struct{}
	release      chan struct{}
	releaseOnce  sync.Once
	ranSecond    atomic.Bool
	firstDone    atomic.Bool
}

func (p *failurePlan) releaseFirst() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *failurePlan) exec(req update.Request) update.Result {
	if req.Label == "second" {
		p.ranSecond.Store(true)
		return update.Result{Label: req.Label, Cmd: req.Cmd}
	}
	close(p.firstStarted)
	<-p.release
	p.firstDone.Store(true)
	return update.Result{Label: req.Label, Cmd: req.Cmd}
}

func failureBuild(_ []resolve.Resolution) FlowPlan {
	return FlowPlan{
		Rows: []FlowRow{
			{RowID: 0, Label: "first", RequestIndex: 0},
			{RowID: 1, Label: "second", RequestIndex: 1},
		},
		Requests: []update.Request{
			{Label: "first", Cmd: update.CmdSpec{Cmd: []string{"x", "1"}}},
			{Label: "second", Cmd: update.CmdSpec{Cmd: []string{"x", "2"}}},
		},
	}
}

func failureScan(diag io.Writer, stop <-chan struct{}) ScanResult {
	return ScanResult{Resolutions: []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "1", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Bin: "qwen", Cur: "1", Latest: "2", Outdated: true, Tier: "npm"},
	}}
}

// TestRunFlow_InputErrorStopsQueuedWorkAndJoins (review B1/B2): a
// terminal input failure mid-apply stops the queued installer, lets the
// in-flight one finish and be joined, and returns the original error.
func TestRunFlow_InputErrorStopsQueuedWorkAndJoins(t *testing.T) {
	stubLookPathAllowed(t)
	out := &syncBuffer{}
	plan := &failurePlan{
		firstStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
	t.Cleanup(plan.releaseFirst)
	errInput := errors.New("terminal input failed")
	sendKeys := make(chan struct{})
	rdr := &errAfterKeysReader{sendKeys: sendKeys, firstStarted: plan.firstStarted, err: errInput}

	done := make(chan FlowResult, 1)
	var runErr error
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan:   failureScan,
			Build:  failureBuild,
			Exec:   plan.exec,
			Input:  rdr,
			Output: out,
		})
		runErr = err
		done <- res
	}()

	waitForOutput(t, out, "enter apply") // the select screen is up
	close(sendKeys)                      // now deliver "a\r"
	<-plan.firstStarted                  // apply is running: first installer in flight
	// The input error fires while "first" is in flight and the program
	// shuts down (its teardown bytes are the observable that record +
	// quit already happened). Only then release the in-flight installer:
	// anything queued must already have been stopped.
	waitForOutput(t, out, "[?1049l")
	plan.releaseFirst()
	<-done
	if !errors.Is(runErr, errInput) {
		t.Errorf("error = %v, want the original input error %v", runErr, errInput)
	}
	if plan.ranSecond.Load() {
		t.Error("queued installer ran after the input failure was observed")
	}
	if !plan.firstDone.Load() {
		t.Error("the in-flight installer must be allowed to finish and be joined")
	}
}

// TestRunFlow_WriteFailureReturnsOriginalErrorAndStops (review B2): a
// broken output stops the run and surfaces the original write error —
// never a silent success — and repeated failures stay idempotent.
func TestRunFlow_WriteFailureReturnsOriginalErrorAndStops(t *testing.T) {
	stubLookPathAllowed(t)
	plan := &failurePlan{
		firstStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
	t.Cleanup(plan.releaseFirst)
	out := &flakyWriter{failed: make(chan struct{})}
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	done := make(chan FlowResult, 1)
	var runErr error
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan:   failureScan,
			Build:  failureBuild,
			Exec:   plan.exec,
			Input:  pr,
			Output: out,
		})
		runErr = err
		done <- res
	}()

	// Reaching the apply phase needs keys from the pipe — delivered only
	// once the select screen is observable. Then break the output while
	// the first installer is in flight.
	waitForOutput(t, out, "enter apply")
	_, _ = pw.Write([]byte("a\r"))
	<-plan.firstStarted
	out.arm()
	<-out.failed // record + synchronous stop happened by now
	plan.releaseFirst()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunFlow hung after an output failure — invisible waiting")
	}
	if runErr == nil {
		t.Fatal("write failure must surface as an error, never silent success")
	}
	if !strings.Contains(runErr.Error(), "output write failed") {
		t.Errorf("error = %v, want the original write failure", runErr)
	}
	if plan.ranSecond.Load() {
		t.Error("queued installer ran after the write failure was observed")
	}
	if !plan.firstDone.Load() {
		t.Error("the in-flight installer must be allowed to finish and be joined")
	}
}

// TestRunFlow_InputEOFDuringApplyStopsQueuedWork (QA probe A2, elevated):
// input EOF is terminal loss — the queued installer is stopped, the
// in-flight one finishes and is joined, and the original EOF is returned.
func TestRunFlow_InputEOFDuringApplyStopsQueuedWork(t *testing.T) {
	stubLookPathAllowed(t)
	out := &syncBuffer{}
	plan := &failurePlan{
		firstStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
	t.Cleanup(plan.releaseFirst)
	pr, pw := io.Pipe()

	done := make(chan FlowResult, 1)
	var runErr error
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan:   failureScan,
			Build:  failureBuild,
			Exec:   plan.exec,
			Input:  pr,
			Output: out,
		})
		runErr = err
		done <- res
	}()

	waitForOutput(t, out, "enter apply")
	_, _ = pw.Write([]byte("a\r")) // select all, apply
	<-plan.firstStarted
	_ = pw.Close() // terminal loss: EOF while the first installer runs
	// The program shuts down on the observed EOF (the teardown bytes are
	// the observable that record + quit already happened); only then is
	// the in-flight installer released.
	waitForOutput(t, out, "[?1049l")
	plan.releaseFirst()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunFlow hung after input EOF")
	}
	if !errors.Is(runErr, io.EOF) {
		t.Errorf("error = %v, want the original EOF", runErr)
	}
	if plan.ranSecond.Load() {
		t.Error("queued installer ran after input EOF was observed")
	}
	if !plan.firstDone.Load() {
		t.Error("the in-flight installer must be allowed to finish and be joined")
	}
}

// TestRunFlow_InputEOFAtSummaryReturnsRatherThanHanging: the exact QA
// probe A2 repro — EOF while the summary waits for dismissal must not
// wait forever; the run returns with the original terminal failure.
func TestRunFlow_InputEOFAtSummaryReturnsRatherThanHanging(t *testing.T) {
	stubLookPathAllowed(t)
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	done := make(chan FlowResult, 1)
	var runErr error
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan: failureScan,
			Build: func([]resolve.Resolution) FlowPlan {
				return FlowPlan{
					Rows:     []FlowRow{{RowID: 0, Label: "first", RequestIndex: 0}},
					Requests: []update.Request{{Label: "first", Cmd: update.CmdSpec{Cmd: []string{"x", "1"}}}},
				}
			},
			Exec: func(req update.Request) update.Result {
				return update.Result{Label: req.Label, Cmd: req.Cmd}
			},
			Input:  pr,
			Output: out,
		})
		runErr = err
		done <- res
	}()

	waitForOutput(t, out, "enter apply")
	_, _ = pw.Write([]byte("a\r")) // select all, apply
	waitForOutput(t, out, "Updates finished")
	_ = pw.Close() // input EOF with the summary on screen
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunFlow never returns after input EOF — the summary waits on a dead terminal")
	}
	if !errors.Is(runErr, io.EOF) {
		t.Errorf("error = %v, want the original EOF", runErr)
	}
}

// --- row plan and rendering (issue #62 review B5/B6/B7) ---

// TestFlowModel_AllSkippedSelectionShowsFullSummary (review B5): a
// selection that is entirely pre-skipped renders the full skipped
// summary — rows, reasons, counts — never an empty placeholder.
func TestFlowModel_AllSkippedSelectionShowsFullSummary(t *testing.T) {
	f, m := flowFixture(t)
	f.cfg.Build = func([]resolve.Resolution) FlowPlan {
		return FlowPlan{Rows: []FlowRow{
			{RowID: 0, Label: "claude (native)", RequestIndex: -1, Reason: "pinned in config"},
			{RowID: 1, Label: "qwen (npm)", RequestIndex: -1, Reason: "no update command (catalog)"},
		}}
	}
	m = updateKey(t, m, "a")
	m = updateKey(t, m, "enter")
	if m.phase != phaseSummary || m.exitCode != 0 {
		t.Fatalf("phase=%v exit=%d, want summary/0", m.phase, m.exitCode)
	}
	v := m.frame()
	for _, want := range []string{
		"claude (native)", "pinned in config",
		"qwen (npm)", "no update command (catalog)",
		"0 succeeded · 0 failed · 2 skipped",
	} {
		if !contains(v, want) {
			t.Errorf("all-skipped summary missing %q\n%s", want, v)
		}
	}
	if contains(v, "Nothing to apply.") {
		t.Errorf("all-skipped summary must show rows, not an empty placeholder\n%s", v)
	}
}

// TestFlowModel_PlanOrderAndDuplicateLabelsPreserved (review B6): rows
// render in plan order with stable distinct identities even when labels
// repeat — never collapsed, never reordered.
func TestFlowModel_PlanOrderAndDuplicateLabelsPreserved(t *testing.T) {
	stubLookPathAllowed(t) // the apply worker's PATH preflight must be hermetic
	f, m := flowFixture(t)
	f.cfg.Build = func([]resolve.Resolution) FlowPlan {
		return FlowPlan{
			Rows: []FlowRow{
				{RowID: 0, Label: "qwen (npm)", RequestIndex: -1, Reason: "no matching install block"},
				{RowID: 1, Label: "qwen (npm)", RequestIndex: 0},
				{RowID: 2, Label: "qwen (npm)", RequestIndex: -1, Reason: "no matching install block"},
			},
			Requests: []update.Request{{Label: "qwen (npm)", Cmd: update.CmdSpec{Cmd: []string{"x"}}}},
		}
	}
	m = updateKey(t, m, "a")
	m = updateKey(t, m, "enter")
	if len(m.rows) != 3 {
		t.Fatalf("rows = %d, want 3 distinct rows for duplicate labels", len(m.rows))
	}
	for i, want := range []struct {
		rowID int
		state rowState
	}{{0, rowSkipped}, {1, rowQueued}, {2, rowSkipped}} {
		if m.rows[i].rowID != want.rowID || m.rows[i].state != want.state {
			t.Errorf("row %d = %+v, want rowID %d state %v", i, m.rows[i], want.rowID, want.state)
		}
	}
	// Join the apply worker before returning: the ExecLookPath seam must
	// only be restored after the worker can no longer touch it.
	f.wg.Wait()
}

// TestFlowModel_WidthTruncationKeepsStateAndBar (review B7): at 80 and
// at narrow widths every row line fits the terminal in ANSI cell width —
// wide graphemes counted as two cells — and the state word and bar are
// never truncated away.
func TestFlowModel_WidthTruncationKeepsStateAndBar(t *testing.T) {
	for _, width := range []int{80, 40, 24} {
		_, m := flowFixture(t)
		m = updateMsg(t, m, tea.WindowSizeMsg{Width: width, Height: 24})
		m.phase = phaseApply
		m.rows = []applyRow{
			{label: "ハーネス更新ツール-very-long-harness-name (native)", state: rowRunning, reason: "a very long reason that must not push the state and bar out of the frame"},
			{label: "x", state: rowFailed, reason: "r"},
		}
		v := m.frame()
		for _, line := range strings.Split(v, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: line of %d cells: %q", width, w, line)
			}
		}
		for _, want := range []string{"running", "failed", "["} {
			if !contains(v, want) {
				t.Errorf("width %d: row state/bar %q missing\n%s", width, want, v)
			}
		}
	}
}

// TestFlowModel_ResizeShrinkAndExpand (review B7): both directions of
// WindowSizeMsg re-layout correctly.
func TestFlowModel_ResizeShrinkAndExpand(t *testing.T) {
	_, m := flowFixture(t)
	m.phase = phaseApply
	m.rows = []applyRow{{label: "harness-name-that-is-quite-long-indeed (npm)", state: rowDone}}

	m = updateMsg(t, m, tea.WindowSizeMsg{Width: 40, Height: 12}) // shrink
	for _, line := range strings.Split(m.frame(), "\n") {
		if w := ansi.StringWidth(line); w > 40 {
			t.Errorf("shrunk line of %d cells: %q", w, line)
		}
	}
	if !contains(m.frame(), "done") {
		t.Errorf("shrunk frame lost the state word\n%s", m.frame())
	}

	m = updateMsg(t, m, tea.WindowSizeMsg{Width: 120, Height: 40}) // expand
	if !contains(m.frame(), "harness-name-that-is-quite-long-indeed (npm)") {
		t.Errorf("expanded frame should show the full label\n%s", m.frame())
	}
}

// TestFlowModel_TinyHeightKeepsCountsAndNav (review B7): the banner may
// compact first; counts, states and navigation must survive.
func TestFlowModel_TinyHeightKeepsCountsAndNav(t *testing.T) {
	_, m := summaryFixture(t)
	m = updateMsg(t, m, tea.WindowSizeMsg{Width: 80, Height: 8})
	v := m.frame()
	if contains(v, "▓") {
		t.Errorf("the banner must compact at tiny heights\n%s", v)
	}
	for _, want := range []string{"1 succeeded · 1 failed · 1 skipped", "q/enter/ctrl+c dismiss"} {
		if !contains(v, want) {
			t.Errorf("tiny frame lost %q\n%s", want, v)
		}
	}
	if lines := strings.Split(strings.TrimSuffix(v, "\n"), "\n"); len(lines) > 8 {
		t.Errorf("frame is %d lines at height 8", len(lines))
	}
}

// TestFlowModel_DetailViewShowsFullContent (review B7): the d view
// exposes full, wrapped detail — reason and tail — at any width.
func TestFlowModel_DetailViewShowsFullContent(t *testing.T) {
	_, m := flowFixture(t)
	m = updateMsg(t, m, tea.WindowSizeMsg{Width: 40, Height: 24})
	m.phase = phaseApply
	m.rows = []applyRow{{
		label:  "claude (native)",
		state:  rowFailed,
		reason: "REASON-START installer exited with an error after a long struggle REASON-END",
		tail:   "TAIL-START boom\nmore output TAIL-END",
	}}
	m = updateKey(t, m, "d")
	v := m.frame()
	// Wrap boundaries may split a marker across lines: join the rendered
	// lines before checking, which is exactly what paging lets the user
	// read in full.
	joined := strings.ReplaceAll(v, "\n", "")
	for _, frag := range []string{"REASON-START", "REASON-END", "TAIL-START", "TAIL-END"} {
		if !contains(joined, frag) {
			t.Errorf("detail view lost %q\n%s", frag, v)
		}
	}
}

// --- lifecycle ordering and terminal failures (issue #62 review N2) ---

// TestFailureRecorder_OrderingPolicy: failures are ordered against the
// normal dismissal by the recorder's own lifecycle state. Pre-dismissal
// failures are always honored — genuine EOF/EIO/short writes are never
// converted to success because dismissal raced them. Post-dismissal
// failures are suppressed only when identified as our own shutdown's
// cancellation noise (an already-closed handle).
func TestFailureRecorder_OrderingPolicy(t *testing.T) {
	// Record before dismissal: honored even after dismissal is marked.
	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	rec.record(io.EOF)
	rec.markDismissing()
	if got := rec.failure(); got != io.EOF {
		t.Errorf("pre-dismissal failure = %v, want io.EOF", got)
	}

	// Record after dismissal, own-shutdown noise: suppressed.
	rec2 := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	rec2.markDismissing()
	rec2.record(os.ErrClosed)
	if got := rec2.failure(); got != nil {
		t.Errorf("post-dismissal shutdown noise = %v, want nil", got)
	}

	// Record after dismissal, genuine terminal failure: honored.
	rec3 := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	rec3.markDismissing()
	rec3.record(io.EOF)
	if got := rec3.failure(); got != io.EOF {
		t.Errorf("post-dismissal genuine EOF = %v, want io.EOF", got)
	}
	// A wrapped genuine failure is never noise either.
	rec4 := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	rec4.markDismissing()
	rec4.record(fmt.Errorf("write: %w", errors.New("EIO")))
	if got := rec4.failure(); got == nil {
		t.Error("post-dismissal write failure must not be suppressed")
	}
}

// TestFailureRecorder_ConcurrentDismissalAndFailure: whichever side of
// the mutex wins, the outcome is consistent with the observed order —
// a genuine failure is reported unless it was noise after dismissal, and
// nothing is ever fabricated.
func TestFailureRecorder_ConcurrentDismissalAndFailure(t *testing.T) {
	for i := 0; i < 200; i++ {
		rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; rec.markDismissing() }()
		go func() { defer wg.Done(); <-start; rec.record(io.EOF) }()
		close(start)
		wg.Wait()
		// io.EOF is never shutdown noise, so it must surface regardless
		// of which side won the race.
		if got := rec.failure(); got != io.EOF {
			t.Fatalf("iteration %d: failure = %v, want io.EOF (q/error race must not swallow)", i, got)
		}
	}
}

// dataEOFReader delivers data and EOF in one Read — the (n>0, EOF) case.
type dataEOFReader struct{}

func (dataEOFReader) Read(p []byte) (int, error) {
	n := copy(p, "a\r")
	return n, io.EOF
}

// TestObservedReader_RecordBeforeReturnOnDataEOF: when a Read returns
// data AND EOF, the record and the synchronous stop happen before the
// bytes and the error reach tea — consumer processing can never outrun
// the observation. The Reader contract (bytes + original error) is
// preserved.
func TestObservedReader_RecordBeforeReturnOnDataEOF(t *testing.T) {
	stopped := false
	rec := &failureRecorder{ch: make(chan struct{}), stop: func() { stopped = true }}
	or := &observedReader{r: dataEOFReader{}, rec: rec}
	buf := make([]byte, 8)
	n, err := or.Read(buf)
	if n != 2 || err != io.EOF || string(buf[:2]) != "a\r" {
		t.Errorf("Read contract broken: n=%d err=%v data=%q", n, err, buf[:n])
	}
	if !stopped {
		t.Error("record/stop must be synchronous before Read returns bytes to tea")
	}
	if rec.failure() != io.EOF {
		t.Error("the EOF must be recorded as the original failure")
	}
}

// TestRunFlow_QThenEOFSurfacesTheFailure: dismissal racing a genuine
// terminal failure never converts it to success — the conservative
// policy. (Fixture input is closed deliberately here; ordinary tests
// hold their input through dismissal/join.)
func TestRunFlow_QThenEOFSurfacesTheFailure(t *testing.T) {
	stubLookPathAllowed(t)
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	done := make(chan FlowResult, 1)
	var runErr error
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan:   failureScan,
			Build:  func([]resolve.Resolution) FlowPlan { return FlowPlan{} },
			Exec:   func(req update.Request) update.Result { return update.Result{Label: req.Label} },
			Input:  pr,
			Output: out,
		})
		runErr = err
		done <- res
	}()
	waitForOutput(t, out, "enter apply")
	_, _ = pw.Write([]byte("q")) // the user quits…
	_ = pw.Close()               // …and the terminal dies in the same breath
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunFlow hung on the q+EOF race")
	}
	if !errors.Is(runErr, io.EOF) {
		t.Errorf("error = %v, want the genuine EOF surfaced despite the dismissal race", runErr)
	}
}

// TestRunFlow_SummaryWriteFailureReturnsErrorAndJoins (QA ENV4): a
// pre-dismissal write failure while the summary waits for a dismissal
// that never comes must terminate the run with the original error — the
// watcher must deliver the shutdown — with every worker joined. The
// writer is armed only once the summary is painted and no dismissal key
// is ever sent.
func TestRunFlow_SummaryWriteFailureReturnsErrorAndJoins(t *testing.T) {
	stubLookPathAllowed(t)
	out := &flakyWriter{failed: make(chan struct{})}
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	done := make(chan FlowResult, 1)
	var runErr error
	go func() {
		res, err := RunFlow(FlowConfig{
			Scan: failureScan,
			Build: func([]resolve.Resolution) FlowPlan {
				return FlowPlan{
					Rows:     []FlowRow{{RowID: 0, Label: "first", RequestIndex: 0}},
					Requests: []update.Request{{Label: "first", Cmd: update.CmdSpec{Cmd: []string{"x", "1"}}}},
				}
			},
			Exec:   func(req update.Request) update.Result { return update.Result{Label: req.Label, Cmd: req.Cmd} },
			Input:  pr,
			Output: out,
		})
		runErr = err
		done <- res
	}()
	waitForOutput(t, out, "enter apply")
	_, _ = pw.Write([]byte("a\r"))
	waitForOutput(t, out, "Updates finished") // summary painted
	out.arm()                                 // …and only now the output breaks
	// One non-dismissal redraw so the broken output is exercised even
	// though the summary frame is otherwise static.
	_, _ = pw.Write([]byte("d"))
	select {
	case <-out.failed:
	case <-time.After(5 * time.Second):
		t.Fatal("no write reached the armed writer at the summary")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunFlow hung after a summary write failure — the watcher never terminated the program")
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "output write failed") {
		t.Errorf("error = %v, want the original write failure", runErr)
	}
}

// --- header budget and select diagnostics (issue #62 review N3/B8) ---

// TestFlowModel_FullArtAt80x24PerPhase (review N3): the standard 80x24
// terminal keeps the banner art on every screen — compaction is driven
// by actual fit, not by a guessed height threshold.
func TestFlowModel_FullArtAt80x24PerPhase(t *testing.T) {
	_, scan := scanFixture()
	scan = updateMsg(t, scan, tea.WindowSizeMsg{Width: 80, Height: 24})
	if v := scan.frame(); !contains(v, "▓") {
		t.Errorf("scan frame lost the banner art at 80x24\n%s", v)
	}

	_, apply := flowFixture(t)
	apply.phase = phaseApply
	apply.rows = []applyRow{{label: "first", state: rowRunning}}
	apply = updateMsg(t, apply, tea.WindowSizeMsg{Width: 80, Height: 24})
	if v := apply.frame(); !contains(v, "▓") {
		t.Errorf("apply frame lost the banner art at 80x24\n%s", v)
	}

	_, summary := summaryFixture(t)
	summary = updateMsg(t, summary, tea.WindowSizeMsg{Width: 80, Height: 24})
	if v := summary.frame(); !contains(v, "▓") {
		t.Errorf("summary frame lost the banner art at 80x24\n%s", v)
	}
}

// TestFlowModel_HeaderCompactsOnlyWhenRequired (review N3): per-phase
// boundary — the art stays for every height where header + pinned +
// footer + one body row fit, and compacts on the first height that
// cannot.
func TestFlowModel_HeaderCompactsOnlyWhenRequired(t *testing.T) {
	// Scan: pinned = caption/detail/blank (3) → full art needs
	// 11 + 3 + 1 + 1 = 16 rows.
	_, scan := scanFixture()
	scan = updateMsg(t, scan, tea.WindowSizeMsg{Width: 80, Height: 16})
	if v := scan.frame(); !contains(v, "▓") {
		t.Errorf("scan should keep the art at height 16 (fits exactly)\n%s", v)
	}
	scan = updateMsg(t, scan, tea.WindowSizeMsg{Width: 80, Height: 15})
	if v := scan.frame(); contains(v, "▓") || !contains(v, ScanTagline) {
		t.Errorf("scan should compact at height 15 but keep the tagline\n%s", v)
	}

	// Apply: pinned = heading/blank (2) → full art needs 15 rows.
	_, apply := flowFixture(t)
	apply.phase = phaseApply
	apply.rows = []applyRow{{label: "first", state: rowQueued}}
	apply = updateMsg(t, apply, tea.WindowSizeMsg{Width: 80, Height: 15})
	if v := apply.frame(); !contains(v, "▓") {
		t.Errorf("apply should keep the art at height 15 (fits exactly)\n%s", v)
	}
	apply = updateMsg(t, apply, tea.WindowSizeMsg{Width: 80, Height: 14})
	if v := apply.frame(); contains(v, "▓") {
		t.Errorf("apply should compact at height 14\n%s", v)
	}

	// Summary: pinned = heading/counts/blank (3) → full art needs 16 rows.
	_, summary := summaryFixture(t)
	summary = updateMsg(t, summary, tea.WindowSizeMsg{Width: 80, Height: 16})
	if v := summary.frame(); !contains(v, "▓") {
		t.Errorf("summary should keep the art at height 16 (fits exactly)\n%s", v)
	}
	summary = updateMsg(t, summary, tea.WindowSizeMsg{Width: 80, Height: 15})
	if v := summary.frame(); contains(v, "▓") {
		t.Errorf("summary should compact at height 15\n%s", v)
	}
}

// TestFlowModel_SelectShowsDiagnosticsHint (review B8): in flow mode the
// select screen advertises the log view and counts live diagnostics, so
// fast-scan warnings are discoverable before any apply.
func TestFlowModel_SelectShowsDiagnosticsHint(t *testing.T) {
	_, m := flowFixture(t) // flow mode, select phase
	m = updateMsg(t, m, diagMsg{line: "warning: npm registry stale"})
	v := m.frame()
	if !contains(v, "1 diagnostic line(s) — press l to view") {
		t.Errorf("flow select screen missing the diagnostics notice\n%s", v)
	}
	if !contains(v, "l logs") {
		t.Errorf("flow select screen missing the l-logs key hint\n%s", v)
	}
	m = updateKey(t, m, "l")
	if v := m.frame(); !contains(v, "warning: npm registry stale") {
		t.Errorf("log view should open from select\n%s", v)
	}

	// Without diagnostics the notice is absent but the key stays discoverable.
	_, m2 := flowFixture(t)
	v2 := m2.frame()
	if contains(v2, "diagnostic line(s)") {
		t.Errorf("no-notice expected without diagnostics\n%s", v2)
	}
	if !contains(v2, "l logs") {
		t.Errorf("the l-logs hint must be discoverable even before warnings\n%s", v2)
	}

	// Run's wrapper mode keeps the original footer byte-identical.
	wm := fixture()
	wv := wm.frame()
	if contains(wv, "l logs") || contains(wv, "diagnostic line(s)") {
		t.Errorf("wrapper select layout must be unchanged\n%s", wv)
	}
}
