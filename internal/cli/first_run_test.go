package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ofa/internal/config"
	"ofa/internal/detect"
)

// First-run coverage (issue #46): on a machine with no ~/.config/ofa the
// state write failed with "no such file or directory" and every command that
// saves state exited 2. These tests drive the real command paths against a
// config directory that does not exist yet.
//
// Nothing here touches the developer's real config: every path is a
// t.TempDir() and OFA_HOME is set through t.Setenv.

func oneClaude() []detect.Instance {
	return []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
}

// missingStatePath returns a state path whose parent directory does not
// exist — the shape of a first run on a machine with no ~/.config/ofa.
func missingStatePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config", "ofa", "state.json")
}

// TestScanCmd_MissingConfigDirCreatesState: `ofa scan` on a machine with no
// config directory succeeds, creates the directory and writes state.json.
func TestScanCmd_MissingConfigDirCreatesState(t *testing.T) {
	a, out, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.StatePath = missingStatePath(t)
	sd.instances = oneClaude()

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}

	dir := filepath.Dir(a.StatePath)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("scan did not create the config dir %s: %v", dir, err)
	}
	if !st.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if _, err := os.Stat(a.StatePath); err != nil {
		t.Fatalf("scan did not write %s: %v", a.StatePath, err)
	}
	saved, err := loadStateFile(a.StatePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if saved == nil {
		t.Fatal("state file is missing after a successful scan")
	}
	if len(saved.Harnesses["claude"].Installs) != 1 {
		t.Errorf("saved state = %+v, want the detected claude install", saved.Harnesses)
	}
	if !strings.Contains(out.String(), "initial scan") {
		t.Errorf("first scan should say 'initial scan', got %q", out.String())
	}
}

// TestScanCmd_RespectsOFAHome: with OFA_HOME set, the directory created is
// the one config.StatePath() points into — not ~/.config/ofa.
func TestScanCmd_RespectsOFAHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "ofa-home")
	t.Setenv("OFA_HOME", home)

	a, _, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.StatePath = config.StatePath()
	if want := filepath.Join(home, "state.json"); a.StatePath != want {
		t.Fatalf("StatePath = %q, want %q", a.StatePath, want)
	}
	sd.instances = oneClaude()

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
	if st, err := os.Stat(home); err != nil {
		t.Fatalf("OFA_HOME dir not created: %v", err)
	} else if !st.IsDir() {
		t.Fatalf("%s is not a directory", home)
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); err != nil {
		t.Fatalf("state file not written under OFA_HOME: %v", err)
	}
}

// TestPickerCmd_FirstRunCreatesConfigDirAndState: the bare `ofa` first run
// creates the config directory and the state file before it reaches the
// picker. Without a TTY the run ends on the "no TTY detected" message, which
// is the same exit code it had before the directory existed (ExitError) —
// what changes is that the run now gets past the auto-scan and leaves a
// state file behind.
func TestPickerCmd_FirstRunCreatesConfigDirAndState(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, nil, nil) // no args → picker
	a.StatePath = missingStatePath(t)
	sd.instances = oneClaude()

	code := a.Run()
	if code != ExitError {
		t.Fatalf("bare ofa exit = %d, want %d (the non-TTY path's code)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "first run: scanning...") {
		t.Errorf("stderr should show the first-run scan, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "no TTY detected") {
		t.Errorf("stderr should reach the no-TTY message, got %q", stderr.String())
	}
	if st, err := os.Stat(filepath.Dir(a.StatePath)); err != nil {
		t.Errorf("first run did not create the config dir: %v", err)
	} else if !st.IsDir() {
		t.Errorf("%s is not a directory", filepath.Dir(a.StatePath))
	}
	if _, err := os.Stat(a.StatePath); err != nil {
		t.Errorf("first run did not write %s: %v", a.StatePath, err)
	}
}

// TestScanCmd_StateParentIsFileExitsError: the state path's parent exists
// as a regular file, so no directory can be created. The command has to
// report a clear error and exit non-zero — not panic, and not exit 0.
func TestScanCmd_StateParentIsFileExitsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "ofa")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	a, _, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.StatePath = filepath.Join(blocker, "state.json")
	sd.instances = oneClaude()

	code := a.Run()
	if code == ExitOK {
		t.Fatal("scan should fail when the state parent path is a regular file")
	}
	if code != ExitError {
		t.Errorf("scan exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "state:") {
		t.Errorf("stderr should report the state error, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), blocker) {
		t.Errorf("stderr should name the blocking path %s, got %q", blocker, stderr.String())
	}
}
