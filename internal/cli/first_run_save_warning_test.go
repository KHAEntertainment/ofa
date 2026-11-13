package cli

// First-run state-save failure coverage (issue #66): a state.json that
// cannot be written must not block the interactive picker, while `ofa
// scan` and non-interactive runs keep failing exactly as they did before
// the fix.
//
// Every path here is a t.TempDir() fixture; nothing touches HOME, the
// network, a real detector/installer/agent/config, or a real terminal or
// TUI. The two interactive-path seams (stdinIsInteractive, openPicker)
// are overridden per test and restored with t.Cleanup; no test in this
// file runs in parallel, so the package variables are safe under -race.
// The deterministic save failure is a dangling symlink — os.Stat reports
// ErrNotExist (the first-run trigger) while the write fails with ENOENT
// naming the state path — which behaves identically for root and
// unprivileged users on Linux and macOS.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ofa/internal/catalog"
	"ofa/internal/picker"
	"ofa/internal/resolve"
)

// pickerProbe records what reached the picker seam.
type pickerProbe struct {
	calls int
	res   []resolve.Resolution
}

// stubOpenPicker replaces openPicker with a recorder that captures the
// resolutions handed to the picker and returns a quit outcome (so the
// run ends where a quit at the picker would). Restored by t.Cleanup.
func stubOpenPicker(t *testing.T) *pickerProbe {
	t.Helper()
	orig := openPicker
	p := &pickerProbe{}
	openPicker = func(res []resolve.Resolution, _ map[string]bool) (picker.Outcome, error) {
		p.calls++
		p.res = append(p.res, res...)
		return picker.Outcome{Quit: true}, nil
	}
	t.Cleanup(func() { openPicker = orig })
	return p
}

// stubInteractiveGate forces the stdin gate to answer "terminal" so a
// test can drive the interactive path with an ordinary reader instead of
// a real pty. Restored by t.Cleanup.
func stubInteractiveGate(t *testing.T) {
	t.Helper()
	orig := stdinIsInteractive
	stdinIsInteractive = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsInteractive = orig })
}

// danglingStatePath returns a state path shaped like a first run whose
// write cannot succeed: a dangling symlink whose target sits under a
// directory that does not exist. os.Stat follows the link and reports
// ErrNotExist — so the first-run scan triggers — while os.WriteFile
// follows it and fails on the missing parent, naming the state path
// itself. Deterministic for every user including root: no permission
// bits are involved.
func danglingStatePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	link := filepath.Join(dir, "state.json")
	target := filepath.Join(dir, "missing", "state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create dangling state symlink: %v", err)
	}
	return link
}

// TestPickerCmd_SaveFailureReachesPicker is the core of issue #66: the
// first-run state write fails, yet the interactive run reaches the
// picker — proven by the picker seam receiving this run's in-memory
// resolutions, not by an exit code — and reports the failure exactly
// once as a warning naming the path and the cause. state.json stays
// absent throughout: the picker never needs it on disk.
func TestPickerCmd_SaveFailureReachesPicker(t *testing.T) {
	statePath := danglingStatePath(t)
	a, out, stderr, sd, sr, _ := newTestApp(t, nil, bytes.NewReader(nil))
	a.StatePath = statePath
	sd.instances = oneClaude()
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self", Package: "claude-code"}}
	stubInteractiveGate(t)
	probe := stubOpenPicker(t)

	if code := a.Run(); code != ExitOK {
		t.Fatalf("bare ofa exit = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
	if probe.calls != 1 {
		t.Fatalf("picker opened %d times, want 1 (stderr: %q)", probe.calls, stderr.String())
	}
	if len(probe.res) != 1 || probe.res[0].HarnessID != "claude" {
		t.Errorf("picker resolutions = %+v, want this run's in-memory claude result", probe.res)
	}

	got := stderr.String()
	if !strings.Contains(got, "first run: scanning...") {
		t.Errorf("stderr should show the first-run scan, got %q", got)
	}
	if n := strings.Count(got, "cannot save state"); n != 1 {
		t.Errorf("save failure reported %d times, want exactly 1: %q", n, got)
	}
	if !strings.Contains(got, statePath) {
		t.Errorf("warning should name the state path %s, got %q", statePath, got)
	}
	if !strings.Contains(got, "no such file or directory") {
		t.Errorf("warning should name the cause, got %q", got)
	}
	if strings.Contains(got, "ofa: state:") {
		t.Errorf("interactive path must warn once, not print the fatal state line: %q", got)
	}
	if strings.Contains(got, "could not cache latest versions") {
		t.Errorf("no cache write may be attempted on a first run with no state file: %q", got)
	}
	if !strings.Contains(out.String(), "initial scan") || !strings.Contains(out.String(), "claude") {
		t.Errorf("stdout should show the in-memory scan report, got %q", out.String())
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file should still be absent after the failed save, stat err = %v", err)
	}
}

// TestPickerCmd_UnwritableStateDirStillReachesPicker stages the issue's
// own reproduction — a config directory with mode 500 — and asserts the
// same recovery as the deterministic fixture. Guarded: when the process
// can still write at mode 500 (root), the scenario cannot be staged, so
// the test skips rather than asserting a failure that did not happen.
func TestPickerCmd_UnwritableStateDirStillReachesPicker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ofa")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // let TempDir cleanup remove it
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	probePath := filepath.Join(dir, "probe")
	if f, err := os.Create(probePath); err == nil {
		f.Close()
		_ = os.Remove(probePath)
		t.Skipf("state dir %s still writable at mode 0500 (privileged user); scenario cannot be staged", dir)
	}

	statePath := filepath.Join(dir, "state.json")
	a, _, stderr, sd, sr, _ := newTestApp(t, nil, bytes.NewReader(nil))
	a.StatePath = statePath
	sd.instances = oneClaude()
	sr.res = []resolve.Resolution{{HarnessID: "claude"}}
	stubInteractiveGate(t)
	probe := stubOpenPicker(t)

	if code := a.Run(); code != ExitOK {
		t.Fatalf("bare ofa exit = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
	if probe.calls != 1 {
		t.Fatalf("picker opened %d times, want 1 (stderr: %q)", probe.calls, stderr.String())
	}
	got := stderr.String()
	if n := strings.Count(got, "cannot save state"); n != 1 {
		t.Errorf("save failure reported %d times, want exactly 1: %q", n, got)
	}
	if !strings.Contains(got, statePath) || !strings.Contains(got, "permission denied") {
		t.Errorf("warning should name path and cause, got %q", got)
	}
	if strings.Contains(got, "ofa: state:") {
		t.Errorf("interactive path must warn once, not print the fatal state line: %q", got)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file should still be absent after the failed save, stat err = %v", err)
	}
}

// TestPickerCmd_NonInteractiveSaveFailureKeepsOriginalBytesAndExit:
// recovery is the TTY path only. Without a terminal the run must be
// byte-for-byte what it was before the fix — the fatal `ofa: state:` line
// naming path and cause, exit 2, no warning — and it never reaches the
// gate, so no no-TTY message appears either. Uses the real
// stdinIsInteractive predicate (a bytes.Reader is not a terminal).
func TestPickerCmd_NonInteractiveSaveFailureKeepsOriginalBytesAndExit(t *testing.T) {
	statePath := danglingStatePath(t)
	a, _, stderr, sd, sr, _ := newTestApp(t, nil, bytes.NewReader(nil))
	a.StatePath = statePath
	sd.instances = oneClaude()
	sr.res = []resolve.Resolution{{HarnessID: "claude"}}
	probe := stubOpenPicker(t)

	code := a.Run()
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %q)", code, ExitError, stderr.String())
	}
	want := "first run: scanning...\n" +
		fmt.Sprintf("ofa: state: open %s: no such file or directory\n", statePath)
	if stderr.String() != want {
		t.Errorf("stderr bytes = %q, want exactly %q", stderr.String(), want)
	}
	if probe.calls != 0 {
		t.Errorf("picker opened %d times on a non-interactive run, want 0", probe.calls)
	}
}

// TestPickerCmd_FirstRunCatalogFailureStaysFatal: an interactive first
// run whose scan fails for a non-save reason must not be "recovered" —
// only the save failure is recoverable, so a catalog error keeps its
// fatal exit and the picker never opens (issue #66).
func TestPickerCmd_FirstRunCatalogFailureStaysFatal(t *testing.T) {
	statePath := danglingStatePath(t)
	a, _, stderr, _, _, _ := newTestApp(t, nil, bytes.NewReader(nil))
	a.StatePath = statePath
	a.CatalogLoader = func() ([]catalog.Harness, []catalog.Problem, error) {
		return nil, nil, errors.New("catalog exploded")
	}
	stubInteractiveGate(t)
	probe := stubOpenPicker(t)

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %q)", code, ExitError, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "ofa: catalog: catalog exploded") {
		t.Errorf("stderr should report the catalog failure, got %q", got)
	}
	if strings.Contains(got, "cannot save state") {
		t.Errorf("a catalog failure must never be reported as a save failure: %q", got)
	}
	if probe.calls != 0 {
		t.Errorf("picker opened %d times after a catalog failure, want 0", probe.calls)
	}
}

// TestPickerCmd_SaveRecoveryNotInheritedAcrossRuns: the recovery lives in
// scanCmd's per-invocation return value, not in App state. Two Runs on
// the same App: the first recovers from the save failure and opens the
// picker once; the second, whose scan fails at detection, must fail
// fatally — no inherited recovery, no second warning, no second picker
// call — so detection failures stay fatal on the interactive path too.
func TestPickerCmd_SaveRecoveryNotInheritedAcrossRuns(t *testing.T) {
	statePath := danglingStatePath(t)
	a, _, stderr, sd, sr, _ := newTestApp(t, nil, bytes.NewReader(nil))
	a.StatePath = statePath
	sd.instances = oneClaude()
	sr.res = []resolve.Resolution{{HarnessID: "claude"}}
	stubInteractiveGate(t)
	probe := stubOpenPicker(t)

	if code := a.Run(); code != ExitOK {
		t.Fatalf("first Run exit = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
	if probe.calls != 1 {
		t.Fatalf("first Run opened the picker %d times, want 1", probe.calls)
	}

	sd.err = errors.New("detect exploded")
	code := a.Run()
	if code != ExitError {
		t.Fatalf("second Run exit = %d, want %d (stderr: %q)", code, ExitError, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "ofa: scan: detect exploded") {
		t.Errorf("second Run should report the detection failure, got %q", got)
	}
	if n := strings.Count(got, "cannot save state"); n != 1 {
		t.Errorf("save-failure warnings across both Runs = %d, want 1 (recovery not inherited): %q", n, got)
	}
	if probe.calls != 1 {
		t.Errorf("picker opened %d times across both Runs, want 1", probe.calls)
	}
}

// TestScanCmd_SaveFailureExitsError keeps the `ofa scan` contract scripts
// rely on (issue #66): the human report is complete on stdout, the state
// failure reaches stderr once as the original fatal `ofa: state:` line
// naming path and cause — never the interactive warning — and the exit is
// non-zero. The dangling-symlink fixture makes load succeed (file
// missing) and only the save fail.
func TestScanCmd_SaveFailureExitsError(t *testing.T) {
	statePath := danglingStatePath(t)
	a, out, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.StatePath = statePath
	sd.instances = oneClaude()

	code := a.Run()
	if code != ExitError {
		t.Fatalf("scan exit = %d, want %d (stderr: %q)", code, ExitError, stderr.String())
	}
	if !strings.Contains(out.String(), "initial scan") || !strings.Contains(out.String(), "claude") {
		t.Errorf("stdout should hold the full scan report, got %q", out.String())
	}
	want := fmt.Sprintf("ofa: state: open %s: no such file or directory\n", statePath)
	if stderr.String() != want {
		t.Errorf("stderr bytes = %q, want exactly %q", stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "cannot save state") {
		t.Errorf("scan must keep the fatal diagnostic, not the interactive warning: %q", stderr.String())
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file should still be absent after the failed save, stat err = %v", err)
	}
}

// TestScanJSON_SaveFailureExitsError: `ofa scan --json` on a save failure
// keeps valid JSON on stdout (consumers already got it), reports the path
// and cause once on stderr, and exits non-zero.
func TestScanJSON_SaveFailureExitsError(t *testing.T) {
	statePath := danglingStatePath(t)
	a, out, stderr, sd, _, _ := newTestApp(t, []string{"scan", "--json"}, nil)
	a.StatePath = statePath
	sd.instances = oneClaude()

	code := a.Run()
	if code != ExitError {
		t.Fatalf("scan --json exit = %d, want %d (stderr: %q)", code, ExitError, stderr.String())
	}
	if !strings.Contains(out.String(), `"harness_count": 1`) || !strings.Contains(out.String(), "claude") {
		t.Errorf("stdout should hold the scan JSON, got %q", out.String())
	}
	got := stderr.String()
	if n := strings.Count(got, "ofa: state:"); n != 1 {
		t.Errorf("state failure reported %d times, want 1: %q", n, got)
	}
	if !strings.Contains(got, statePath) || !strings.Contains(got, "no such file or directory") {
		t.Errorf("stderr should name path and cause, got %q", got)
	}
	if strings.Contains(got, "cannot save state") {
		t.Errorf("scan must keep the fatal diagnostic, not the interactive warning: %q", got)
	}
}
