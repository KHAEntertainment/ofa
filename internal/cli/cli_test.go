package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ofa/internal/banner"
	"ofa/internal/catalog"
	"ofa/internal/config"
	"ofa/internal/detect"
	"ofa/internal/picker"
	"ofa/internal/resolve"
	"ofa/internal/state"
	"ofa/internal/update"
)

// mustTime parses an RFC3339 string or panics. Tests use it to pin
// ScannedAt without dragging time.Parse through every fixture.
func mustTime(s string) time.Time {
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tt
}

// stubDetect is a DetectFn substitute. Returns the canned instances and the
// detector failures to degrade with, if any. It records the catalog it was
// handed so tests can assert what the loader actually produced.
type stubDetect struct {
	instances []detect.Instance
	failures  []detect.Failure
	err       error
	cat       []catalog.Harness
}

func (s *stubDetect) Run(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
	s.cat = cat
	return s.instances, s.failures, s.err
}

// stubResolve returns canned resolutions. It also records the Config it
// was handed so tests can assert cache/refresh/prefetch wiring.
type stubResolve struct {
	res  []resolve.Resolution
	cfgs []resolve.Config
}

func (s *stubResolve) Run(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
	s.cfgs = append(s.cfgs, cfg)
	return s.res
}

// stubExec returns canned results keyed by label.
type stubExec struct {
	results map[string]update.Result
	calls   []update.Request
}

func (s *stubExec) Fn(req update.Request) update.Result {
	s.calls = append(s.calls, req)
	if r, ok := s.results[req.Label]; ok {
		return r
	}
	return update.Result{Label: req.Label, Cmd: req.Cmd, ExitCode: 0}
}

// noopRunner is the default for DetectRunner/ResolveRunner in tests. Most
// tests bypass the runners entirely via stubDetect/stubResolve.
func noopRunner(_ string, _ ...string) (string, error) {
	return "", errors.New("noopRunner: should not be called")
}

func noopGetter(_ string) ([]byte, error) {
	return nil, errors.New("noopGetter: should not be called")
}

// testCatalog returns the real embedded catalog. Tests stub DetectFn /
// ResolveFn so the catalog is only consulted for InstallBlock matching
// in buildRequests / findInstall.
func testCatalog(t *testing.T) []catalog.Harness {
	t.Helper()
	cat, err := catalog.Embedded()
	if err != nil {
		t.Fatalf("catalog.Embedded: %v", err)
	}
	return cat
}

// newTestApp builds an App wired with stubbed dependencies.
func newTestApp(t *testing.T, args []string, stdin io.Reader) (*App, *bytes.Buffer, *bytes.Buffer, *stubDetect, *stubResolve, *stubExec) {
	t.Helper()
	// Stub exec.LookPath so apply tests don't depend on whether the
	// harness binary is installed on the test machine. Real installs
	// are exercised by the smoke binaries in cmd/*-smoke, not here.
	origLookPath := update.ExecLookPath
	update.ExecLookPath = func(string) (string, error) { return "/usr/bin/true", nil }
	t.Cleanup(func() { update.ExecLookPath = origLookPath })

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	sd := &stubDetect{}
	sr := &stubResolve{}
	se := &stubExec{results: map[string]update.Result{}}
	a := New(args, stdin, stdout, stderr)
	a.DetectFn = sd.Run
	a.ResolveFn = sr.Run
	a.DetectRunner = noopRunner
	a.ResolveRunner = noopRunner
	a.ResolveGetter = noopGetter
	a.UpdateExec = se.Fn
	a.StatePath = t.TempDir() + "/state.json"
	// Never read the developer's real ~/.config/ofa/config.toml; a pin on
	// this machine would otherwise change test behavior.
	a.ConfigLoader = func() (config.Config, error) { return config.Config{}, nil }
	// Same for the user catalog: keep the real merge path wired, but point
	// it at an absent tempdir so a harness someone added on this machine
	// can never change a test's results.
	a.CatalogDir = filepath.Join(t.TempDir(), "catalog")
	return a, stdout, stderr, sd, sr, se
}

// --- App.Run dispatch ---

func TestRun_Version(t *testing.T) {
	a, out, _, _, _, _ := newTestApp(t, []string{"--version"}, nil)
	if code := a.Run(); code != ExitOK {
		t.Errorf("--version exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "ofa ") {
		t.Errorf("--version output = %q, want it to mention ofa", out.String())
	}
}

func TestRun_Help(t *testing.T) {
	a, _, err, _, _, _ := newTestApp(t, []string{"--help"}, nil)
	if code := a.Run(); code != ExitOK {
		t.Errorf("--help exit = %d, want 0", code)
	}
	if !strings.Contains(err.String(), "Usage:") {
		t.Errorf("--help stderr = %q, want it to contain Usage:", err.String())
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	a, _, err, _, _, _ := newTestApp(t, []string{"bogus"}, nil)
	if code := a.Run(); code != ExitError {
		t.Errorf("unknown command exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(err.String(), "unknown command") {
		t.Errorf("stderr = %q, want 'unknown command'", err.String())
	}
}

func TestRun_NoArgs(t *testing.T) {
	// No args opens the picker. Without a TTY (test stdin is nil-or-piped),
	// the picker bails with an error pointing the user at the
	// non-interactive subcommands.
	a, _, err, _, _, _ := newTestApp(t, nil, nil)
	if code := a.Run(); code != ExitError {
		t.Errorf("no-args (no TTY) exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(err.String(), "no TTY detected") {
		t.Errorf("stderr should mention no-TTY, got %q", err.String())
	}
}

// TestPickerCmd_AutoScansOnMissingState verifies the first-run path: if
// there's no state file, pickerCmd runs scan before opening the picker.
// We can't open the picker in a test (no TTY), but we can confirm scan
// ran by checking that the state file now exists.
func TestPickerCmd_AutoScansOnMissingState(t *testing.T) {
	a, _, _, sd, _, _ := newTestApp(t, nil, nil) // no args → picker
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	// State path is set by newTestApp to a tempdir file that doesn't exist yet.
	// After pickerCmd runs scan, the file should exist.
	a.Run()
	if _, err := os.Stat(a.StatePath); err != nil {
		t.Errorf("pickerCmd should have run scan on missing state, but state file still missing: %v", err)
	}
}

// --- interactive flow (issue #62) ---
//
// The loading spinner that used to run on stderr before the picker
// (issue #12) is gone: the scan now animates inside the TUI's scanning
// screen. The TUI renders to the validated terminal (stderr, per the
// two-stream gate) — so redirected stdout must receive no bytes at all —
// and nothing is printed to the normal terminal buffer after teardown.

// syncBuf is a goroutine-safe capture buffer: the TUI renders from its
// own goroutine while a test polls the frames.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForBuf blocks until the captured stream contains substr. The
// frames are stable history, so a one-way poll is a synchronization
// point, not a sleep. Bounded at five seconds.
func waitForBuf(t *testing.T, b *syncBuf, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stream never contained %q:\n%s", substr, b.String())
}

// TestPickerFlow_InteractivePathPrintsNothingToNormalStreams: with real
// terminals on both streams the whole flow runs in the alt screen. The
// TUI targets the validated stderr; redirected stdout gets zero bytes;
// every phase frame lands on the TUI stream; and no plain "first run:"
// notice survives anywhere.
func TestPickerFlow_InteractivePathPrintsNothingToNormalStreams(t *testing.T) {
	a, stdout, _, sd, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.StdinIsTTY = func(io.Reader) bool { return true }
	sd.instances = oneClaude()
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}
	a.StatePath = missingStatePath(t) // first run: the scan phase has real work

	// The validated terminal is the TUI's destination; the test watches
	// it through a race-safe buffer.
	tui := &syncBuf{}
	a.Stderr = tui
	// Hold the pipeline inside Resolve until the scanning frame has been
	// painted, so every phase frame is observably rendered.
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !strings.Contains(tui.String(), picker.ScanCaption) {
			time.Sleep(5 * time.Millisecond)
		}
		return sr.res
	}

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	a.Stdin = pr
	done := make(chan ExitCode, 1)
	go func() { done <- a.Run() }()

	waitForBuf(t, tui, picker.ScanCaption)  // phase 1: the scanning frame
	waitForBuf(t, tui, "enter apply")       // phase 2: the picker frame
	_, _ = pw.Write([]byte("\r"))           // empty selection → summary
	waitForBuf(t, tui, "Nothing selected.") // phase 3: the summary frame
	_, _ = pw.Write([]byte("q"))            // dismiss
	if code := <-done; code != ExitOK {
		t.Fatalf("interactive run exit = %d, want %d", code, ExitOK)
	}

	if stdout.Len() != 0 {
		t.Errorf("redirected stdout = %q, want zero bytes (the TUI targets the validated stderr)", stdout.String())
	}
	got := tui.String()
	for _, want := range []string{picker.ScanCaption, "enter apply", "Nothing selected."} {
		if !strings.Contains(got, want) {
			t.Errorf("TUI stream missing phase frame %q", want)
		}
	}
	if strings.Contains(got, "first run: scanning...") {
		t.Errorf("TUI stream = %q, want no plain first-run notice on the interactive path", got)
	}
}

// TestPickerFlow_ConfigPinsReachThePicker: pins loaded from the real
// ConfigLoader seam during the scan must reach the picker — a pinned row
// renders pinned and cannot be selected or applied, while unpinned rows
// still can. Regression: the flow used to capture a.Pins before
// loadConfig populated it, so configured pins never reached the model.
func TestPickerFlow_ConfigPinsReachThePicker(t *testing.T) {
	a, stdout, _, sd, sr, se := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.StdinIsTTY = func(io.Reader) bool { return true }
	a.ConfigLoader = func() (config.Config, error) {
		return config.Config{Pins: []string{"claude"}}, nil
	}
	sd.instances = oneClaude()
	sd.instances = append(sd.instances, detect.Instance{
		HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active,
	})
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"}, // pinned in config
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}
	se.results["qwen (npm)"] = update.Result{Label: "qwen (npm)", ExitCode: 0}

	tui := &syncBuf{}
	a.Stderr = tui
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	a.Stdin = pr
	done := make(chan ExitCode, 1)
	go func() { done <- a.Run() }()

	waitForBuf(t, tui, "enter apply")
	if !strings.Contains(tui.String(), "(pinned)") {
		t.Errorf("config pins never reached the picker:\n%s", tui.String())
	}
	// Select all and apply. The pinned row must never reach the apply
	// stage: no request row for it, no "pinned in config" skip row (what
	// buildRequests would report if a pinned selection slipped through),
	// and a one-row summary.
	_, _ = pw.Write([]byte("a\r"))
	waitForBuf(t, tui, "qwen (npm)")
	if strings.Contains(tui.String(), "claude (native)") {
		t.Error("pinned row was applied")
	}
	if strings.Contains(tui.String(), "pinned in config") {
		t.Error("pinned row was selectable (it reached buildRequests)")
	}
	_, _ = pw.Write([]byte("q")) // stop after current, or dismiss
	waitForBuf(t, tui, "Updates finished")
	// The summary counts one row — the unpinned update — proving select-all
	// took only it. (Full lines render fresh on the summary screen, unlike
	// in-place checkbox toggles, so they are observable in the stream.)
	waitForBuf(t, tui, "1 succeeded · 0 failed · 0 skipped")
	_, _ = pw.Write([]byte("q"))
	if code := <-done; code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if strings.Contains(tui.String(), "claude (native)") || strings.Contains(tui.String(), "pinned in config") {
		t.Error("pinned row leaked into the apply stage")
	}
	if stdout.Len() != 0 {
		t.Errorf("redirected stdout = %q, want zero bytes", stdout.String())
	}
}

// TestPickerFlow_StopDuringDetectNeverLaunchesResolve (review B3): a
// stop observed while detection runs must prevent the resolve stage
// from ever launching — the scan stops between stages, not only between
// pipeline calls.
func TestPickerFlow_StopDuringDetectNeverLaunchesResolve(t *testing.T) {
	a, _, _, sd, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.StdinIsTTY = func(io.Reader) bool { return true }
	sd.instances = oneClaude()
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}
	// A seeded state file skips the first-run scan, so the only detect
	// call is the pipeline's own.
	if err := os.WriteFile(a.StatePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	tui := &syncBuf{}
	a.Stderr = tui
	detectStarted := make(chan struct{})
	releaseDetect := make(chan struct{})
	resolveCalls := 0
	a.DetectFn = func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		close(detectStarted)
		<-releaseDetect // the in-flight stage runs to completion
		return sd.instances, nil, nil
	}
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		resolveCalls++ // read after the run's join: no data race
		return sr.res
	}

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	a.Stdin = pr
	done := make(chan ExitCode, 1)
	go func() { done <- a.Run() }()

	<-detectStarted
	waitForBuf(t, tui, "Scanning System for Harnesses...") // the scan screen is up
	_, _ = pw.Write([]byte("q"))                           // stop during detect
	// The stopping state is the sync point: the stop has been requested
	// (and keeps rendering) while detection is still in flight, so the
	// release below can never race ahead of the request.
	waitForBuf(t, tui, "Stopping — waiting for the scan to finish…")
	close(releaseDetect) // the in-flight stage finishes
	if code := <-done; code != ExitOK {
		t.Fatalf("exit = %d, want %d (canceled scan quits clean)", code, ExitOK)
	}
	if resolveCalls != 0 {
		t.Errorf("resolve launched %d time(s) after the stop was observed, want 0", resolveCalls)
	}
}

// TestRunPipelineUntil_JoinsBrewPrefetchOnDetectFailure (review B4): the
// spawned `brew outdated` is joined on the detect-failure exit path —
// background work must not outlive the pipeline. The join is the cached
// BrewFuture.Join, never a second consumptive wait.
func TestRunPipelineUntil_JoinsBrewPrefetchOnDetectFailure(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t, nil, nil)
	runnerStarted := make(chan struct{})
	runnerRelease := make(chan struct{})
	runnerDone := make(chan struct{})
	a.ResolveRunner = func(name string, args ...string) (string, error) {
		if name == "brew" {
			close(runnerStarted)
			<-runnerRelease
			close(runnerDone)
			return "", nil
		}
		return "", nil
	}
	a.DetectFn = func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		return nil, nil, errors.New("detect failed")
	}

	ret := make(chan struct{})
	go func() {
		_, _, _, _ = a.runPipelineUntil(testCatalog(t), nil)
		close(ret)
	}()

	<-runnerStarted // the prefetch query is genuinely in flight
	select {
	case <-ret:
		t.Fatal("runPipelineUntil returned while the brew query was still in flight")
	case <-time.After(50 * time.Millisecond): // the pass path: still waiting
	}
	close(runnerRelease)
	select {
	case <-ret:
	case <-time.After(5 * time.Second):
		t.Fatal("runPipelineUntil never returned after the query finished")
	}
	<-runnerDone
}

// TestPickerFlow_DuplicateLabelSkipsStayDistinct (review B6): two
// selections that share a label and both skip keep two stable rows with
// their reasons — never collapsed into one.
func TestPickerFlow_DuplicateLabelSkipsStayDistinct(t *testing.T) {
	a, _, _, sd, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.StdinIsTTY = func(io.Reader) bool { return true }
	sd.instances = []detect.Instance{
		{HarnessID: "qwen", Method: "npm", Package: "pkg-a", Bin: "qwen", Version: "1", Status: detect.Active},
		{HarnessID: "qwen", Method: "npm", Package: "pkg-b", Bin: "qwen", Version: "1", Status: detect.Active},
	}
	// Both packages match no catalog install block → both selections
	// skip with the same label "qwen (npm)".
	sr.res = []resolve.Resolution{
		{HarnessID: "qwen", Method: "npm", Package: "pkg-a", Bin: "qwen", Cur: "1", Latest: "2", Outdated: true, Tier: "npm"},
		{HarnessID: "qwen", Method: "npm", Package: "pkg-b", Bin: "qwen", Cur: "1", Latest: "2", Outdated: true, Tier: "npm"},
	}

	tui := &syncBuf{}
	a.Stderr = tui
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	a.Stdin = pr
	done := make(chan ExitCode, 1)
	go func() { done <- a.Run() }()

	waitForBuf(t, tui, "enter apply")
	_, _ = pw.Write([]byte("a\r")) // select both, apply
	waitForBuf(t, tui, "Updates finished")
	got := tui.String()
	if !strings.Contains(got, "0 succeeded · 0 failed · 2 skipped") {
		t.Errorf("summary counts wrong for two same-label skips:\n%s", got)
	}
	if n := strings.Count(got, "no matching install block"); n < 2 {
		t.Errorf("skip reasons rendered %d time(s), want 2 distinct rows:\n%s", n, got)
	}
	_, _ = pw.Write([]byte("q"))
	if code := <-done; code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
}

// TestPickerFlow_NonTTYStderrDoesNotOpenAltScreen pins issue #62's gate:
// the alt screen needs a real terminal on both streams. A TTY stdin with
// a non-TTY stderr takes the plain fallback — pre-#62 this combination
// opened the alt screen and spilled its escape bytes into the redirected
// stderr, a prior bug corrected here and pinned by this test.
func TestPickerFlow_NonTTYStderrDoesNotOpenAltScreen(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, nil, nil)
	a.StdinIsTTY = func(io.Reader) bool { return true }   // TTY stdin
	a.StderrIsTTY = func(io.Writer) bool { return false } // but a non-TTY stderr
	a.StatePath = missingStatePath(t)
	sd.instances = oneClaude()

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (the plain fallback's code)", code, ExitError)
	}
	got := stderr.String()
	if !strings.Contains(got, "first run: scanning...") {
		t.Errorf("stderr = %q, want the first-run notice on the fallback", got)
	}
	if !strings.Contains(got, "no TTY detected") {
		t.Errorf("stderr = %q, want the no-TTY message", got)
	}
	if strings.Contains(stripSGR(got), banner.Plain()) {
		t.Errorf("stderr = %q, want no banner bytes (non-TTY stderr)", got)
	}
}

func TestStderrIsTTY(t *testing.T) {
	if stderrIsTTY(&bytes.Buffer{}) {
		t.Error("bytes.Buffer must not be treated as a TTY")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if stderrIsTTY(f) {
		t.Error("a regular file must not be treated as a TTY")
	}
}

// TestSubcommands_EmitNoSpinnerBytes pins the other half of the issue's
// contract: the loading indicator is picker-only, so script-facing
// subcommands must never emit carriage returns or braille frames.
func TestSubcommands_EmitNoSpinnerBytes(t *testing.T) {
	for _, cmd := range []string{"check", "scan"} {
		t.Run(cmd, func(t *testing.T) {
			a, _, stderr, sd, sr, _ := newTestApp(t, []string{cmd}, nil)
			sd.instances = []detect.Instance{
				{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
			}
			sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}

			a.Run()

			if got := stderr.String(); strings.ContainsAny(got, "\r⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
				t.Errorf("%s stderr = %q, want no spinner bytes", cmd, got)
			}
		})
	}
}

// --- filterByHarness / countUpdates / buildRequests ---

func TestFilterByHarness(t *testing.T) {
	res := []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Outdated: true},
		{HarnessID: "qwen", Method: "brew-formula", Outdated: true},
	}
	if got := filterByHarness(res, ""); len(got) != 3 {
		t.Errorf("empty filter should return all, got %d", len(got))
	}
	if got := filterByHarness(res, "qwen"); len(got) != 2 {
		t.Errorf("--only qwen should return 2, got %d", len(got))
	}
	if got := filterByHarness(res, "claude"); len(got) != 1 {
		t.Errorf("--only claude should return 1, got %d", len(got))
	}
	if got := filterByHarness(res, "nonexistent"); len(got) != 0 {
		t.Errorf("unknown id should return 0, got %d", len(got))
	}
}

func TestCountUpdates(t *testing.T) {
	res := []resolve.Resolution{
		{HarnessID: "claude", Tier: "self"},
		{HarnessID: "qwen", Outdated: true},
		{HarnessID: "codex", Tier: "brew", Outdated: false},
		{HarnessID: "kimi", Tier: "self"},
		{HarnessID: "no-update", Tier: "none"},
	}
	// Only a verified newer version counts as pending.
	if got := countUpdates(res, nil); got != 1 {
		t.Errorf("countUpdates = %d, want 1 (qwen only)", got)
	}
	// Self-tier is counted separately: an available updater is not an
	// outstanding update.
	if got := countUpdaters(res, nil); got != 2 {
		t.Errorf("countUpdaters = %d, want 2 (claude, kimi)", got)
	}
	// Nothing outdated at all -> 0, even though two updaters exist. This is
	// the whole point of issue #4.
	onlySelf := []resolve.Resolution{
		{HarnessID: "claude", Tier: "self"},
		{HarnessID: "agy", Tier: "self"},
	}
	if got := countUpdates(onlySelf, nil); got != 0 {
		t.Errorf("countUpdates with only self-tier = %d, want 0", got)
	}
}

func TestCountUpdaters_ShadowedNativeHasNoReachableUpdater(t *testing.T) {
	res := []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Tier: "self"},
		// A shadowed native install's "<bin> update" resolves via PATH to
		// the live binary, so it cannot update this copy.
		{HarnessID: "opencode", Method: "native", Tier: "self", Shadowed: true},
	}
	if got := countUpdaters(res, nil); got != 1 {
		t.Errorf("countUpdaters = %d, want 1 (only the active native install)", got)
	}
}

func TestBuildRequests_ShadowedNativeSkipped(t *testing.T) {
	cat := testCatalog(t)
	res := []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Tier: "self", Shadowed: true},
	}
	reqs, skip := buildRequests(cat, res, false, nil)
	if len(reqs) != 0 {
		t.Errorf("shadowed native install must not produce a request, got %d", len(reqs))
	}
	if _, ok := skip["claude (native)"]; !ok {
		t.Errorf("shadowed native should be skipped with a reason, got %+v", skip)
	}
}

func TestBuildRequests(t *testing.T) {
	cat := testCatalog(t)
	res := []resolve.Resolution{
		// self + claude in catalog → 1 update req
		{HarnessID: "claude", Method: "native", Bin: "claude", Tier: "self"},
		// outdated + qwen-npm in catalog → 1 update req
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Outdated: true, Tier: "npm"},
		// outdated + qwen-brew-formula in catalog → 1 update req
		{HarnessID: "qwen", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Outdated: true, Tier: "brew"},
		// at-latest → skipped (no update available)
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Tier: "brew"},
		// unknown harness → skipReasons
		{HarnessID: "ghost", Method: "native", Bin: "ghost", Tier: "self"},
	}
	reqs, skip := buildRequests(cat, res, false, nil)
	if len(reqs) != 3 {
		t.Errorf("want 3 requests, got %d: %+v", len(reqs), reqs)
	}
	if _, ok := skip["ghost (native)"]; !ok {
		t.Errorf("ghost should be in skipReasons, got %+v", skip)
	}
	for _, r := range reqs {
		if len(r.Cmd.Cmd) == 0 {
			t.Errorf("request %q has empty Cmd: %+v", r.Label, r)
		}
	}
}

func TestBuildRequests_NoCmdSkipped(t *testing.T) {
	cat := []catalog.Harness{
		{ID: "manual", Name: "Manual-only",
			Installs: []catalog.Install{
				{Method: "native", Bin: "manual-bin", Latest: "none",
					Version: catalog.CmdSpec{Cmd: []string{"manual-bin", "--version"}},
					/* no Update */},
			}},
	}
	res := []resolve.Resolution{
		{HarnessID: "manual", Method: "native", Bin: "manual-bin", Tier: "self"},
	}
	reqs, skip := buildRequests(cat, res, false, nil)
	if len(reqs) != 0 {
		t.Errorf("want 0 requests (no update cmd), got %d", len(reqs))
	}
	if _, ok := skip["manual (native)"]; !ok {
		t.Errorf("manual should be in skipReasons, got %+v", skip)
	}
}

// --- check command ---

func TestCheckCmd_NoUpdatesExits0(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Cur: "0.162.1", Tier: "brew"},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("check with no updates exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), "0 with updates") {
		t.Errorf("output should report 0 updates, got %q", out.String())
	}
}

// A verified newer version is what makes an update "pending": only then does
// check exit 1. (Self-tier alone exits 0 — see TestCheckCmd_SelfTierAloneExits0.)
func TestCheckCmd_WithUpdatesExits1(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}
	if code := a.Run(); code != ExitUpdatesPending {
		t.Errorf("check with a real version diff exit = %d, want %d", code, ExitUpdatesPending)
	}
	if !strings.Contains(out.String(), "1 with updates") {
		t.Errorf("output should report 1 update, got %q", out.String())
	}
	if !strings.Contains(out.String(), "0 updaters available") {
		t.Errorf("output should report no self-tier updaters, got %q", out.String())
	}
}

func TestCheckCmd_OnlyFilter(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check", "--only", "claude"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}
	// claude is self-tier, and the filter drops the outdated qwen, so
	// nothing verifiable is pending — exit 0, not 1.
	if code := a.Run(); code != ExitOK {
		t.Errorf("--only claude (self-tier) exit = %d, want %d", code, ExitOK)
	}
	if strings.Contains(out.String(), "qwen") {
		t.Errorf("--only claude output should not mention qwen, got %q", out.String())
	}
	if !strings.Contains(out.String(), "claude") {
		t.Errorf("--only claude output should mention claude, got %q", out.String())
	}
}

func TestCheckCmd_JSON(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check", "--json"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}
	code := a.Run()
	if code != ExitUpdatesPending {
		t.Errorf("JSON check exit = %d, want %d", code, ExitUpdatesPending)
	}
	// Parse the JSON to confirm it's valid jq-able.
	var parsed struct {
		ScannedAt    string `json:"scanned_at"`
		HarnessCount int    `json:"harness_count"`
		InstallCount int    `json:"install_count"`
		UpdateCount  int    `json:"update_count"`
		UpdaterCount int    `json:"updater_available_count"`
		Harnesses    map[string]struct {
			Name     string `json:"name"`
			Installs []struct {
				Method   string `json:"method"`
				Bin      string `json:"bin"`
				Version  string `json:"version"`
				Tier     string `json:"tier"`
				Outdated bool   `json:"outdated"`
			} `json:"installs"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON output did not parse: %v\noutput: %s", err, out.String())
	}
	// update_count must count only verified diffs (qwen), not self-tier.
	if parsed.UpdateCount != 1 {
		t.Errorf("update_count = %d, want 1 (qwen only)", parsed.UpdateCount)
	}
	if parsed.UpdaterCount != 1 {
		t.Errorf("updater_available_count = %d, want 1 (claude)", parsed.UpdaterCount)
	}
	if _, ok := parsed.Harnesses["claude"]; !ok {
		t.Error("claude missing from JSON output")
	}
	if _, ok := parsed.Harnesses["qwen"]; !ok {
		t.Error("qwen missing from JSON output")
	}
}

func TestCheckCmd_OnlyUnknownNoError(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check", "--only", "ghost"}, nil)
	sd.instances = []detect.Instance{}
	sr.res = []resolve.Resolution{}
	code := a.Run()
	if code != ExitOK {
		t.Errorf("--only ghost exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), "0 with updates") {
		t.Errorf("--only ghost output should report 0 updates, got %q", out.String())
	}
}

// A machine whose only actionable installs are self-tier must exit 0: the
// README promises exit 1 means something is *pending*, and an updater with
// no comparable version is not a pending update. Before issue #4 this exited
// 1 forever, which made `ofa check` unusable as a script gate.
func TestCheckCmd_SelfTierAloneExits0(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("self-tier only check exit = %d, want %d (ExitOK)", code, ExitOK)
	}
	if !strings.Contains(out.String(), "0 with updates") {
		t.Errorf("output should report 0 updates, got %q", out.String())
	}
	// The updater must stay visible rather than disappear.
	if !strings.Contains(out.String(), "1 updaters available") {
		t.Errorf("output should report the available updater, got %q", out.String())
	}
	if !strings.Contains(out.String(), "updater available") {
		t.Errorf("row should still mention updater available, got %q", out.String())
	}
}

// --- scan command ---

func TestScanCmd_Initial(t *testing.T) {
	a, out, _, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1", Status: detect.Active},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("scan exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), "initial scan") {
		t.Errorf("first scan should say 'initial scan', got %q", out.String())
	}
}

func TestScanCmd_DriftAfterTamper(t *testing.T) {
	a, _, _, sd, _, _ := newTestApp(t, []string{"scan"}, nil)

	// First scan: write a baseline state.
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	a.Now = func() time.Time { return mustTime("2026-10-15T12:00:00Z") }
	a.Run()

	// Second scan: same set + an added harness.
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.301", Status: detect.Active},
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1", Status: detect.Active},
	}
	out := &bytes.Buffer{}
	a.Stdout = out
	a.Now = func() time.Time { return mustTime("2026-10-15T12:05:00Z") }
	if code := a.Run(); code != ExitOK {
		t.Errorf("scan exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), "+ added") {
		t.Errorf("drift should include + added, got %q", out.String())
	}
	if !strings.Contains(out.String(), "~ changed") {
		t.Errorf("drift should include ~ changed, got %q", out.String())
	}
}

func TestScanCmd_JSON(t *testing.T) {
	a, out, _, sd, _, _ := newTestApp(t, []string{"scan", "--json"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("scan --json exit = %d, want %d", code, ExitOK)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("scan --json output did not parse: %v\noutput: %s", err, out.String())
	}
	if _, ok := parsed["harness_count"]; !ok {
		t.Errorf("JSON should contain harness_count, got keys: %v", keys(parsed))
	}
	if _, ok := parsed["harnesses"]; !ok {
		t.Errorf("JSON should contain harnesses, got keys: %v", keys(parsed))
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- apply command ---

func TestApplyCmd_DryRun(t *testing.T) {
	a, out, _, sd, sr, exec := newTestApp(t, []string{"apply", "--dry-run"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("apply --dry-run exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), "(dry-run)") {
		t.Errorf("output should mention dry-run, got %q", out.String())
	}
	// The apply command wraps a.UpdateExec to force DryRun=true on every
	// request. Verify the wrapped executor saw DryRun=true so a real
	// DefaultExecutor would print instead of running.
	if len(exec.calls) == 0 {
		t.Fatal("executor was not called")
	}
	for _, c := range exec.calls {
		if !c.DryRun {
			t.Errorf("dry-run request had DryRun=false: %+v", c)
		}
	}
	// And no actual exec should have happened: the stubExec only records
	// calls; production DefaultExecutor would print 'DRY-RUN  ...' lines.
	// Here we just confirm the call was recorded with the right label.
	if exec.calls[0].Label != "claude (native)" {
		t.Errorf("dry-run call label = %q, want %q", exec.calls[0].Label, "claude (native)")
	}
}

func TestApplyCmd_YesRunsWithoutPrompt(t *testing.T) {
	a, _, _, sd, sr, exec := newTestApp(t, []string{"apply", "--yes"}, strings.NewReader("n\n")) // user types 'n' but --yes should ignore it
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("apply --yes exit = %d, want %d", code, ExitOK)
	}
	if len(exec.calls) != 1 {
		t.Errorf("executor calls = %d, want 1", len(exec.calls))
	}
}

func TestApplyCmd_NoPromptSkipsOnN(t *testing.T) {
	a, _, err, sd, sr, exec := newTestApp(t, []string{"apply"}, strings.NewReader("n\n"))
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	if code := a.Run(); code != ExitCancelled {
		t.Errorf("apply with 'n' exit = %d, want %d", code, ExitCancelled)
	}
	if !strings.Contains(err.String(), "aborted") {
		t.Errorf("stderr should say aborted, got %q", err.String())
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor should not run when user says 'n', got %d calls", len(exec.calls))
	}
}

// An EOF on stdin (empty input — a pipe, /dev/null, a closed fd) is a
// declined confirmation: it must exit ExitCancelled, not ExitOK, so scripts
// can tell a cancelled update from a completed one.
func TestApplyCmd_DeclinedEOF(t *testing.T) {
	a, _, err, sd, sr, exec := newTestApp(t, []string{"apply"}, strings.NewReader(""))
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	if code := a.Run(); code != ExitCancelled {
		t.Errorf("apply with EOF stdin exit = %d, want %d", code, ExitCancelled)
	}
	if !strings.Contains(err.String(), "aborted") {
		t.Errorf("stderr should say aborted, got %q", err.String())
	}
	if len(exec.calls) != 0 {
		t.Errorf("executor should not run on EOF, got %d calls", len(exec.calls))
	}
}

// The usage text documents the exit codes; this pins ExitCancelled's
// meaning there so the docs cannot silently drift from the constant.
func TestUsage_MentionsExitCancelled(t *testing.T) {
	var out bytes.Buffer
	a := New(nil, nil, io.Discard, &out)
	a.usage()
	got := out.String()
	if !strings.Contains(got, "3  cancelled") {
		t.Errorf("usage text missing exit code 3 line, got:\n%s", got)
	}
	if !strings.Contains(got, "confirmation declined") {
		t.Errorf("usage text missing ExitCancelled meaning, got:\n%s", got)
	}
}

func TestApplyCmd_PromptYesRuns(t *testing.T) {
	a, _, _, sd, sr, exec := newTestApp(t, []string{"apply"}, strings.NewReader("y\n"))
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	if code := a.Run(); code != ExitOK {
		t.Errorf("apply with 'y' exit = %d, want %d", code, ExitOK)
	}
	if len(exec.calls) != 1 {
		t.Errorf("executor calls = %d, want 1", len(exec.calls))
	}
}

func TestApplyCmd_EmptyExitsOK(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"apply", "--yes"}, nil)
	sd.instances = []detect.Instance{}
	sr.res = []resolve.Resolution{}
	if code := a.Run(); code != ExitOK {
		t.Errorf("apply with no updates exit = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out.String(), "nothing to apply") {
		t.Errorf("output should say 'nothing to apply', got %q", out.String())
	}
}

func TestApplyCmd_OnlyFilter(t *testing.T) {
	a, out, _, sd, sr, exec := newTestApp(t, []string{"apply", "--yes", "--only", "claude"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}
	a.Run()
	if len(exec.calls) != 1 {
		t.Errorf("apply --only claude should run 1 command, got %d", len(exec.calls))
	}
	if len(exec.calls) > 0 && !strings.Contains(exec.calls[0].Label, "claude") {
		t.Errorf("only-claude ran %q", exec.calls[0].Label)
	}
	if strings.Contains(out.String(), "qwen") {
		t.Errorf("--only claude should not mention qwen, got %q", out.String())
	}
}

func TestApplyCmd_PlanAlias(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"apply", "--plan"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	a.Run()
	if !strings.Contains(out.String(), "(dry-run)") {
		t.Errorf("--plan should be alias for --dry-run, got %q", out.String())
	}
}

func TestApplyCmd_FailureExits1(t *testing.T) {
	a, _, _, sd, sr, exec := newTestApp(t, []string{"apply", "--yes"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
	}
	exec.results["claude (native)"] = update.Result{
		Label:    "claude (native)",
		Cmd:      update.CmdSpec{Cmd: []string{"claude", "update"}},
		Failed:   true,
		ExitCode: 1,
		Err:      errors.New("exit 1"),
	}
	if code := a.Run(); code != ExitUpdatesPending {
		// update.ExitCode() returns 1 for failure; we map to ExitUpdatesPending
		// for the CLI to keep "1 = pending or failed" semantics.
		t.Errorf("apply with failure exit = %d, want %d", code, ExitUpdatesPending)
	}
}

// --- confirmPrompt unit test ---

func TestConfirmPrompt(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false}, // EOF
		{"maybe\n", false},
	}
	for _, c := range cases {
		ok, err := confirmPrompt(strings.NewReader(c.in), io.Discard, "")
		if err != nil {
			t.Errorf("confirmPrompt(%q) err = %v", c.in, err)
			continue
		}
		if ok != c.want {
			t.Errorf("confirmPrompt(%q) = %v, want %v", c.in, ok, c.want)
		}
	}
}

// --- printApplySummary smoke ---

func TestPrintApplySummary_NoFailures(t *testing.T) {
	var w bytes.Buffer
	printApplySummary(&w, update.Summary{
		Succeeded: []update.Result{{Label: "a"}, {Label: "b"}},
	})
	if !strings.Contains(w.String(), "2 succeeded") {
		t.Errorf("summary should report 2 succeeded, got %q", w.String())
	}
}

func TestPrintApplySummary_WithFailure(t *testing.T) {
	var w bytes.Buffer
	printApplySummary(&w, update.Summary{
		Succeeded: []update.Result{{Label: "a"}},
		Failed:    []update.Result{{Label: "b", ExitCode: 1, Err: errors.New("boom"), OutputTail: "line1\nline2"}},
		Skipped:   []update.Result{{Label: "c", Skipped: true, SkippedReason: "missing"}},
	})
	if !strings.Contains(w.String(), "FAILURES:") {
		t.Errorf("summary should have FAILURES section, got %q", w.String())
	}
	if !strings.Contains(w.String(), "line1") {
		t.Errorf("summary should include output tail, got %q", w.String())
	}
	if !strings.Contains(w.String(), "SKIPPED:") {
		t.Errorf("summary should have SKIPPED section, got %q", w.String())
	}
}

// --- findInstall ---

func TestFindInstall(t *testing.T) {
	h := catalog.Harness{
		ID: "opencode",
		Installs: []catalog.Install{
			{Method: "npm", Package: "opencode-ai", Bin: "opencode"},
			{Method: "brew-formula", Package: "opencode", Bin: "opencode"},
		},
	}
	if got, ok := findInstall(h, resolve.Resolution{Method: "npm", Package: "opencode-ai", Bin: "opencode"}); !ok || got.Package != "opencode-ai" {
		t.Errorf("findInstall npm = (%+v, %v)", got, ok)
	}
	if got, ok := findInstall(h, resolve.Resolution{Method: "brew-formula", Package: "opencode", Bin: "opencode"}); !ok || got.Package != "opencode" {
		t.Errorf("findInstall brew = (%+v, %v)", got, ok)
	}
	if _, ok := findInstall(h, resolve.Resolution{Method: "pipx", Package: "opencode", Bin: "opencode"}); ok {
		t.Error("pipx should not match")
	}
}

// ---- issue #8: latest-version cache wiring, --refresh, stale warnings ----

// TestCheck_RefreshFlagReachesResolve: --refresh must arrive in the
// resolve.Config, and the default (no flag) must be false.
func TestCheck_RefreshFlagReachesResolve(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"check"}, false},
		{[]string{"check", "--refresh"}, true},
	} {
		a, _, _, _, sr, _ := newTestApp(t, tc.args, nil)
		if code := a.Run(); code != ExitOK {
			t.Errorf("%v: exit = %d, want 0", tc.args, code)
		}
		if len(sr.cfgs) != 1 {
			t.Fatalf("%v: resolve called %d times, want 1", tc.args, len(sr.cfgs))
		}
		if sr.cfgs[0].Refresh != tc.want {
			t.Errorf("%v: cfg.Refresh = %v, want %v", tc.args, sr.cfgs[0].Refresh, tc.want)
		}
		if sr.cfgs[0].Cache == nil {
			t.Errorf("%v: resolve must always get a cache (may be read-through)", tc.args)
		}
	}
}

// TestCheck_PersistsLatestCache: a resolving run writes refreshed
// entries back into an existing state file.
func TestCheck_PersistsLatestCache(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t, []string{"check"}, nil)
	if err := saveStateFile(a.StatePath, state.FromInstances(nil, time.Now())); err != nil {
		t.Fatal(err)
	}
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		cfg.Cache.Put(resolve.CacheKey("npm", "@x/y"), resolve.CacheEntry{Version: "9.9.9", ResolvedAt: a.Now()})
		return nil
	}
	if code := a.Run(); code != ExitOK {
		t.Fatalf("check exit = %d, want 0", code)
	}
	f, err := loadStateFile(a.StatePath)
	if err != nil || f == nil {
		t.Fatalf("load state: %v (f=%v)", err, f)
	}
	e, ok := f.Latest[resolve.CacheKey("npm", "@x/y")]
	if !ok || e.Version != "9.9.9" {
		t.Errorf("state latest = %+v (ok=%v), want npm:@x/y = 9.9.9", e, ok)
	}
}

// TestCheck_NoStateFileCreatesNoCacheFile: without a scan record the
// cache must not create one — a latest-only state file would corrupt
// drift detection.
func TestCheck_NoStateFileCreatesNoCacheFile(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t, []string{"check"}, nil)
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		cfg.Cache.Put(resolve.CacheKey("npm", "@x/y"), resolve.CacheEntry{Version: "9.9.9", ResolvedAt: a.Now()})
		return nil
	}
	if code := a.Run(); code != ExitOK {
		t.Fatalf("check exit = %d, want 0", code)
	}
	if _, err := os.Stat(a.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file created without a scan (err=%v); cache must be read-through only", err)
	}
}

// TestRunPipeline_WarnsOnStaleCacheServe: a stale fallback goes to
// stderr with the refresh error, never to stdout (JSON stays clean).
func TestRunPipeline_WarnsOnStaleCacheServe(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"check"}, nil)
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		return []resolve.Resolution{{
			HarnessID: "x", Tier: "npm", Package: "@x/y", Bin: "x",
			Cur: "1.0.0", Latest: "2.0.0", Outdated: true,
			Stale: true, CachedAt: a.Now().Add(-13 * time.Hour), RefreshErr: "network down",
		}}
	}
	// Outdated=true on the stale answer → check reports pending updates,
	// which is the point of serving the stale entry at all.
	if code := a.Run(); code != ExitUpdatesPending {
		t.Fatalf("check exit = %d, want %d (updates pending)", code, ExitUpdatesPending)
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "served from cache") || !strings.Contains(errOut, "network down") {
		t.Errorf("stderr missing stale warning, got:\n%s", errOut)
	}
	if strings.Contains(stdout.String(), "served from cache") {
		t.Errorf("stale warning leaked to stdout:\n%s", stdout.String())
	}
}

// TestScan_CarriesLatestCache: scan rewrites the state file for drift —
// it must not drop the latest-version cache it didn't refresh.
func TestScan_CarriesLatestCache(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t, []string{"scan"}, nil)
	seed := state.FromInstances(nil, time.Now())
	seed.Latest = map[string]state.LatestEntry{
		resolve.CacheKey("npm", "@x/y"): {Version: "9.9.9", ResolvedAt: time.Now().Format(time.RFC3339)},
	}
	if err := saveStateFile(a.StatePath, seed); err != nil {
		t.Fatal(err)
	}
	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d, want 0", code)
	}
	f, err := loadStateFile(a.StatePath)
	if err != nil || f == nil {
		t.Fatalf("load state: %v (f=%v)", err, f)
	}
	if e, ok := f.Latest[resolve.CacheKey("npm", "@x/y")]; !ok || e.Version != "9.9.9" {
		t.Errorf("latest cache lost across scan: %+v", f.Latest)
	}
}

// TestRun_RefreshDispatchesToPicker: `ofa --refresh` reaches the picker
// AND the flag takes effect (it must survive dispatch into pickerCmd's
// FlagSet — the original PR stripped it, making the flag a silent
// no-op). Tests have no TTY, so the picker bails at its own guard after
// the flag is parsed — which is exactly the evidence dispatch worked.
func TestRun_RefreshDispatchesToPicker(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"--refresh"}, nil)
	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (no TTY)", code, ExitError)
	}
	if !a.Refresh {
		t.Error("--refresh did not survive dispatch: a.Refresh = false, want true")
	}
	out := stderr.String()
	if !strings.Contains(out, "no TTY") {
		t.Errorf("expected picker's no-TTY bail-out, got:\n%s", out)
	}
	if strings.Contains(out, "unknown command") {
		t.Errorf("--refresh was not dispatched to the picker:\n%s", out)
	}
}

// --- pins (config) ---

// pinnedQwen stages an outdated npm install of qwen, the harness the pin
// tests freeze.
func pinnedQwen(t *testing.T, args []string) (*App, *bytes.Buffer, *bytes.Buffer, *stubExec) {
	t.Helper()
	a, stdout, stderr, sd, sr, se := newTestApp(t, args, nil)
	a.ConfigLoader = func() (config.Config, error) {
		return config.Config{Pins: []string{"qwen"}}, nil
	}
	sd.instances = []detect.Instance{{
		HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code",
		Bin: "qwen", Version: "0.22.1", Status: detect.Active,
	}}
	sr.res = []resolve.Resolution{{
		HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code",
		Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm",
	}}
	return a, stdout, stderr, se
}

func TestCheckCmd_PinAnnotatesAndExitsZero(t *testing.T) {
	a, stdout, stderr, _ := pinnedQwen(t, []string{"check"})

	if code := a.Run(); code != ExitOK {
		t.Fatalf("check with a pinned update exit = %d, want %d", code, ExitOK)
	}
	got := stdout.String()
	if !strings.Contains(got, "0 with updates") {
		t.Errorf("headline should count 0 updates (pinned), got:\n%s", got)
	}
	if !strings.Contains(got, "qwen → 0.25.2 (pinned)") {
		t.Errorf("row should be annotated (pinned), got:\n%s", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want no warnings for a known pin", stderr.String())
	}
}

func TestCheckCmd_PinJSONMarksInstall(t *testing.T) {
	a, stdout, _, _ := pinnedQwen(t, []string{"check", "--json"})

	if code := a.Run(); code != ExitOK {
		t.Fatalf("check --json with a pinned update exit = %d, want %d", code, ExitOK)
	}
	var out struct {
		UpdateCount int `json:"update_count"`
		Harnesses   map[string]struct {
			Installs []struct {
				Outdated bool `json:"outdated"`
				Pinned   bool `json:"pinned"`
			} `json:"installs"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout.String())
	}
	if out.UpdateCount != 0 {
		t.Errorf("update_count = %d, want 0 (pinned)", out.UpdateCount)
	}
	installs := out.Harnesses["qwen"].Installs
	if len(installs) != 1 {
		t.Fatalf("qwen installs = %d, want 1", len(installs))
	}
	if !installs[0].Pinned || !installs[0].Outdated {
		t.Errorf("install = %+v, want pinned and outdated both true", installs[0])
	}
}

func TestApplyCmd_PinSkipsExecution(t *testing.T) {
	a, stdout, stderr, se := pinnedQwen(t, []string{"apply", "--yes"})

	if code := a.Run(); code != ExitOK {
		t.Fatalf("apply with only pinned updates exit = %d, want %d", code, ExitOK)
	}
	if len(se.calls) != 0 {
		t.Errorf("executed %d update(s), want none: %+v", len(se.calls), se.calls)
	}
	if !strings.Contains(stdout.String(), "nothing to apply") {
		t.Errorf("stdout = %q, want 'nothing to apply'", stdout.String())
	}
	if !strings.Contains(stderr.String(), "pinned in config") {
		t.Errorf("stderr = %q, want the pin skip reason", stderr.String())
	}
}

func TestCheckCmd_UnknownPinWarns(t *testing.T) {
	a, _, stderr, sd, sr, _ := newTestApp(t, []string{"check"}, nil)
	a.ConfigLoader = func() (config.Config, error) {
		return config.Config{Pins: []string{"ghost"}}, nil
	}
	sd.instances = []detect.Instance{}
	sr.res = nil

	a.Run()

	if !strings.Contains(stderr.String(), `config pins "ghost", which is not in the catalog`) {
		t.Errorf("stderr = %q, want the unknown-pin warning", stderr.String())
	}
}

func TestCheckCmd_MalformedConfigIsError(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"check"}, nil)
	a.ConfigLoader = func() (config.Config, error) {
		return config.Config{}, errors.New("config.toml: unknown key(s): pin")
	}

	if code := a.Run(); code != ExitError {
		t.Fatalf("check with a broken config exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "ofa: config:") {
		t.Errorf("stderr = %q, want a config error", stderr.String())
	}
}

func TestCheckCmd_ReportsShadowedInstalls(t *testing.T) {
	a, stdout, _, sd, sr, _ := newTestApp(t, []string{"check"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode", Version: "1.18.35", Status: detect.Active},
		{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode", Version: "1.19.0", Status: detect.Shadowed},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode", Cur: "1.18.35", Tier: "npm", Latest: "1.19.1", Outdated: true},
		{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode", Cur: "1.19.0", Tier: "brew", Shadowed: true},
	}

	a.Run()

	lines := strings.Split(stdout.String(), "\n")
	var shadowed, active int
	for _, l := range lines {
		switch {
		case strings.Contains(l, "[brew]") && strings.Contains(l, "(shadowed)"):
			shadowed++
		case strings.Contains(l, "[npm]") && !strings.Contains(l, "(shadowed)"):
			active++
		}
	}
	if shadowed != 1 || active != 1 {
		t.Errorf("want one shadowed and one unmarked row, got %d/%d:\n%s", shadowed, active, stdout.String())
	}
}

func TestCheckCmd_JSONReportsShadowedStatus(t *testing.T) {
	a, stdout, _, sd, sr, _ := newTestApp(t, []string{"check", "--json"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode", Version: "1.18.35", Status: detect.Active},
		{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode", Version: "1.19.0", Status: detect.Shadowed},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode", Cur: "1.18.35", Tier: "npm"},
		{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode", Cur: "1.19.0", Tier: "brew", Shadowed: true},
	}

	a.Run()

	var out struct {
		Harnesses map[string]struct {
			Installs []struct {
				Method string `json:"method"`
				Status string `json:"status"`
			} `json:"installs"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout.String())
	}
	got := map[string]string{}
	for _, in := range out.Harnesses["opencode"].Installs {
		got[in.Method] = in.Status
	}
	if got["npm"] != "active" || got["brew-formula"] != "shadowed" {
		t.Errorf("statuses = %v, want npm=active brew-formula=shadowed", got)
	}
}
