package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run real subprocesses — /bin/sh, /bin/sleep — because the
// guarantees they verify (a deadline, a process-group kill, a bounded pipe
// wait) only exist against real processes. None of them spawns an agent or
// the test suite.

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
//
// The deadline is armed only after the readiness handshake. The script
// records its child's pid — atomically, so a reader never sees a partial
// file — and the test reads that pid BEFORE the command is cancelled, so
// the test never asserts about a process it has not seen, and a slow
// machine cannot make the deadline fire before the child exists.
func TestDefaultCommandKillsTheProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	// The shell starts a long sleep in the background and then waits.
	// Both are in the shell's process group, so the group kill takes the
	// grandchild with it.
	script := writeScript(t, fmt.Sprintf(
		"/bin/sleep 60 &\necho $! > '%s.tmp'\nmv '%s.tmp' '%s'\n/bin/sleep 60\n",
		pidfile, pidfile, pidfile))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := DefaultCommand(ctx, script)
		done <- err
	}()

	// The readiness handshake: the pid is read before the deadline is
	// armed, and the cleanup kills the group if the test fails first.
	pid := readPID(t, pidfile)
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	cancel() // the deadline fires
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled command returned no error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the cancelled command never returned")
	}

	// The grandchild must be gone: an orphaned `brew` (or `npm`) would
	// keep running after doctor moved on.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d is still alive after the timeout", pid)
}

// TestDefaultCommandReportsATruncatedRead proves a command whose output
// was cut short is a failure, not a successful read: a package inventory
// read from a truncated stream is a lie the caller cannot see, while a
// failed detector is one it can.
func TestDefaultCommandReportsATruncatedRead(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	// The leader succeeds at once. The child holds the pipe open past
	// the WaitDelay and prints a line too late to be read.
	script := writeScript(t, fmt.Sprintf(
		"printf 'first line\\n'\n( /bin/sleep 3; printf 'late line\\n' ) &\necho $! > '%s.tmp'\nmv '%s.tmp' '%s'\nexit 0\n",
		pidfile, pidfile, pidfile))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := DefaultCommand(ctx, script)
	// The leader recorded its child before it exited, so the pid is
	// there now; the cleanup kills it if the test fails first.
	pid := readPID(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err == nil {
		t.Fatalf("a truncated read returned no error (out = %q)", out)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("err = %v (%T), want an ErrWaitDelay", err, err)
	}
	if strings.Contains(out, "late line") {
		t.Errorf("out = %q, want the late line to be missing", out)
	}
	if !strings.Contains(out, "first line") {
		t.Errorf("out = %q, want the output collected before the cut", out)
	}
}

// TestTimeoutRunnerEndsTheAbandonedCommand proves the runner does not
// wait on a command whose detached grandchild holds the pipe open: the
// deadline ends the call, and WaitDelay ends the abandoned goroutine
// behind it.
//
// The readiness handshake runs while the command is running: the helper
// records its pid and the group it ended up in, and the test reads both
// before the deadline fires.
func TestTimeoutRunnerEndsTheAbandonedCommand(t *testing.T) {
	statusfile := filepath.Join(t.TempDir(), "status")
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}
	// The helper leaves the process group, so the group kill cannot
	// reach it: only WaitDelay can end the wait on the pipe it holds.
	script := writeScript(t, fmt.Sprintf(
		"OFA_DETACH_HELPER=1 OFA_DETACH_STATUS='%s' '%s' -test.run=TestDetachedGrandchildHelper &\n/bin/sleep 60\n",
		statusfile, self))

	finished := make(chan struct{})
	cmd := func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := DefaultCommand(ctx, name, args...)
		close(finished)
		return out, err
	}
	run := TimeoutRunner(cmd, 5*time.Second)

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := run(script)
		done <- err
	}()

	// The readiness handshake: the helper reports the pid and the group
	// it detached into, before the deadline fires.
	pid := readDetachedStatus(t, statusfile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("the hung command returned no error")
		}
		var te *TimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v (%T), want a TimeoutError", err, err)
		}
		// The deadline ends the call; it does not wait for the grandchild.
		if elapsed > 15*time.Second {
			t.Errorf("the runner returned after %v for a 5s deadline", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the runner never returned")
	}

	// The grandchild left the process group, so the group kill could not
	// reach it: it is still holding the pipe the command was reading.
	if syscall.Kill(pid, syscall.Signal(0)) != nil {
		t.Fatalf("grandchild %d did not survive the group kill: it never detached", pid)
	}
	// The abandoned goroutine must finish once WaitDelay expires, rather
	// than waiting for the grandchild to let go of the pipe.
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the abandoned command never finished")
	}
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

// writeScript writes an executable shell script into a temp dir and
// returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// readPID waits for a script to record its child's pid and returns it.
// The file is written atomically, so the content is always complete.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
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

// readDetachedStatus reads the detached helper's status file: its pid and
// the process group it ended up in. A helper that could not leave the
// group is a failure, not a status to wait for.
func readDetachedStatus(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			s := strings.TrimSpace(string(b))
			if strings.HasPrefix(s, "failed") {
				t.Fatalf("the detached helper could not detach: %s", s)
			}
			fields := strings.Fields(s)
			if len(fields) == 2 {
				pid, perr := strconv.Atoi(fields[0])
				pgid, gerr := strconv.Atoi(fields[1])
				if perr != nil || gerr != nil {
					t.Fatalf("unreadable helper status %q", s)
				}
				if pgid != pid {
					t.Fatalf("the helper is in group %d, not its own group %d: it never detached", pgid, pid)
				}
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the detached helper never reported its status to %s", path)
	return 0
}

// TestDetachedGrandchildHelper is not a standalone test: it is the child
// the detached-grandchild test spawns. With OFA_DETACH_HELPER set it
// leaves the process group — a new session — records its pid and the
// group it ended up in, and holds stdout open, which is exactly what a
// process-group kill cannot reach.
func TestDetachedGrandchildHelper(t *testing.T) {
	if os.Getenv("OFA_DETACH_HELPER") != "1" {
		t.Skip("helper process for TestTimeoutRunnerEndsTheAbandonedCommand")
	}
	status := os.Getenv("OFA_DETACH_STATUS")
	if _, err := syscall.Setsid(); err != nil {
		// The helper must leave the process group, or the group kill
		// would reach it and the test would prove nothing.
		writeStatus(status, fmt.Sprintf("failed to detach: %v", err))
		os.Exit(1)
	}
	// A session leader leads its own process group: that is the
	// relationship the test asserts.
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		writeStatus(status, fmt.Sprintf("failed to read the process group: %v", err))
		os.Exit(1)
	}
	if pgid != os.Getpid() {
		writeStatus(status, fmt.Sprintf("failed to detach: pid %d is in group %d", os.Getpid(), pgid))
		os.Exit(1)
	}
	writeStatus(status, fmt.Sprintf("%d %d", os.Getpid(), pgid))
	// Hold stdout open for longer than any bound in the test.
	time.Sleep(60 * time.Second)
}

// writeStatus records a status atomically, so a reader never sees a
// half-written file.
func writeStatus(path, content string) {
	if path == "" {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
