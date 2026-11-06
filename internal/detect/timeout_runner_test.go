package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run real subprocesses — /bin/sh, /bin/sleep — because the
// guarantees they verify (a deadline, a process-group kill) only exist
// against real processes. None of them spawns an agent or the test suite.

// TestDefaultCommandMatchesDefaultRunner pins the bounded runner's
// execution to the unbounded one's: the same command must produce the
// same stdout, the same error text and the same error shape through
// both, so swapping the runner changes the deadline and nothing else.
func TestDefaultCommandMatchesDefaultRunner(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"stdout", []string{"/bin/sh", "-c", "printf hello"}},
		{"stderr and non-zero exit", []string{"/bin/sh", "-c", "printf oops >&2; exit 3"}},
		{"missing command", []string{"ofa-no-such-command-xyz"}},
	}
	bounded := TimeoutRunner(DefaultCommand, 10*time.Second)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantOut, wantErr := DefaultRunner(tc.argv[0], tc.argv[1:]...)
			gotOut, gotErr := bounded(tc.argv[0], tc.argv[1:]...)
			if gotOut != wantOut {
				t.Errorf("stdout = %q, want %q", gotOut, wantOut)
			}
			if errorText(gotErr) != errorText(wantErr) {
				t.Errorf("error = %q, want %q", errorText(gotErr), errorText(wantErr))
			}
			if gotErr != nil && wantErr != nil {
				if fmt.Sprintf("%T", gotErr) != fmt.Sprintf("%T", wantErr) {
					t.Errorf("error type = %T, want %T", gotErr, wantErr)
				}
			}
			// A failing command's stderr reaches the caller through the
			// exit error, not through stdout — the same split both
			// runners must make.
			wantStderr := exitStderr(wantErr)
			if gotStderr := exitStderr(gotErr); gotStderr != wantStderr {
				t.Errorf("stderr = %q, want %q", gotStderr, wantStderr)
			}
		})
	}
}

// errorText renders an error the way a caller would report it.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// exitStderr returns the stderr a failing command captured, if any.
func exitStderr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return ""
}

// TestTimeoutRunnerTimesOutAHungCommand proves the deadline: a command
// that never answers is reported as a timeout, and the runner returns
// instead of waiting for it.
func TestTimeoutRunnerTimesOutAHungCommand(t *testing.T) {
	hung := func(ctx context.Context, name string, args ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	run := TimeoutRunner(hung, 50*time.Millisecond)
	start := time.Now()
	out, err := run("brew", "list", "--versions")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("a hung command returned %q with no error", out)
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T), want a TimeoutError", err, err)
	}
	if te.Command != "brew list --versions" {
		t.Errorf("timed-out command = %q, want %q", te.Command, "brew list --versions")
	}
	if te.Limit != 50*time.Millisecond {
		t.Errorf("limit = %v, want 50ms", te.Limit)
	}
	if !strings.Contains(err.Error(), "timed out after 50ms running brew list --versions") {
		t.Errorf("err = %q, want it to name the command and the limit", err.Error())
	}
	// Generous bound: the deadline is 50ms, so anything near a second
	// means the runner waited for the command instead of abandoning it.
	if elapsed > 5*time.Second {
		t.Errorf("the runner returned after %v for a 50ms deadline", elapsed)
	}
}

// TestTimeoutRunnerAbandonsACommandThatIgnoresTheDeadline proves the
// runner does not wait for a command that ignores its context: the
// deadline is the only thing that ends the call.
func TestTimeoutRunnerAbandonsACommandThatIgnoresTheDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stuck := func(ctx context.Context, name string, args ...string) (string, error) {
		<-release
		return "", errors.New("released")
	}
	run := TimeoutRunner(stuck, 50*time.Millisecond)
	start := time.Now()
	_, err := run("npm", "ls", "-g")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a command that ignored its deadline returned no error")
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T), want a TimeoutError", err, err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the runner returned after %v for a 50ms deadline", elapsed)
	}
}

// TestTimeoutRunnerForSwapsOnlyTheDefaultRunner pins the swap: the
// unbounded production default is bounded, and an injected runner is
// left exactly as it was.
func TestTimeoutRunnerForSwapsOnlyTheDefaultRunner(t *testing.T) {
	injected := func(name string, args ...string) (string, error) { return "sentinel", nil }
	got := TimeoutRunnerFor(injected, time.Second)
	if out, err := got("anything"); out != "sentinel" || err != nil {
		t.Errorf("the injected runner was replaced: out = %q, err = %v", out, err)
	}
	bounded := TimeoutRunnerFor(DefaultRunner, time.Second)
	if IsDefaultRunner(bounded) {
		t.Error("DefaultRunner was passed through unbounded")
	}
	if !IsDefaultRunner(DefaultRunner) {
		t.Error("DefaultRunner was not recognised as the default runner")
	}
	if IsDefaultRunner(injected) {
		t.Error("an injected runner was reported as the default runner")
	}
	var nilRunner Runner
	if IsDefaultRunner(nilRunner) {
		t.Error("a nil runner was reported as the default runner")
	}
}

// TestDefaultCommandKillsTheProcessGroup proves the timeout leaves no
// orphan behind: the command's own child dies with it.
func TestDefaultCommandKillsTheProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	// The shell starts a long sleep in the background and then waits.
	// Both are in the shell's process group, so the group kill takes the
	// grandchild with it.
	body := fmt.Sprintf("/bin/sleep 60 &\necho $! > %s\n/bin/sleep 60\n", pidfile)
	run := TimeoutRunner(DefaultCommand, time.Second)
	start := time.Now()
	_, err := run("/bin/sh", "-c", body)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the long-running command returned no error")
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T), want a TimeoutError", err, err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the runner returned after %v for a 1s deadline", elapsed)
	}
	// The grandchild must be gone: an orphaned `brew` (or `npm`) would
	// keep running after doctor moved on.
	pid := readPID(t, pidfile)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d is still alive after the timeout", pid)
}

// readPID waits for the script to write its child's pid and returns it.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			pid := 0
			if _, serr := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); serr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no pid written to %s", path)
	return 0
}

// TestTimeoutErrorMessage pins the wording the failure carries into the
// scan report: "method: reason" needs the reason to name the command
// and the limit it overran.
func TestTimeoutErrorMessage(t *testing.T) {
	te := &TimeoutError{Command: "brew list --versions", Limit: 30 * time.Second}
	if got, want := te.Error(), "timed out after 30s running brew list --versions"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// The failure a scan reports renders as "method: reason".
	f := Failure{Method: "brew-formula", Err: te}
	if got, want := f.Error(), "brew-formula: timed out after 30s running brew list --versions"; got != want {
		t.Errorf("Failure.Error() = %q, want %q", got, want)
	}
}

// TestTimeoutRunnerKeepsAMissingCommandMissing pins the error shape a
// missing package manager produces: detect reads it as "not installed",
// not as a failure, and the bounded runner must not turn it into one.
func TestTimeoutRunnerKeepsAMissingCommandMissing(t *testing.T) {
	run := TimeoutRunner(DefaultCommand, time.Second)
	_, err := run("ofa-no-such-command-xyz")
	if err == nil {
		t.Fatal("a missing command returned no error")
	}
	if !isNotFound(err) {
		t.Errorf("err = %v (%T), want a not-found error", err, err)
	}
}
