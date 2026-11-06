//go:build linux || darwin

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// #28: the post-exit group kill is sent while the leader is an unreaped
// zombie, so it can only reach the group our child created. These tests pin
// the no-reap wait itself and the runner behaviour built on it. Fixtures are
// /bin/sh and /bin/sleep only; nothing here runs a real agent.

// waitLeaderExitWithin runs waitLeaderExit with a budget, so a regression
// that blocks forever fails the test instead of hanging the suite.
func waitLeaderExitWithin(t *testing.T, pid int, budget time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- waitLeaderExit(pid) }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		t.Fatalf("waitLeaderExit(%d) still blocked after %v", pid, budget)
		return nil
	}
}

// The wait must not reap: Wait afterwards still collects the real exit
// status. Had waitLeaderExit reaped the child, Wait would fail with ECHILD.
func TestWaitLeaderExitDoesNotReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitLeaderExitWithin(t, cmd.Process.Pid, 5*time.Second); err != nil {
		t.Fatalf("waitLeaderExit: %v", err)
	}
	// Still a zombie: the PID is held, so signal 0 reaches it.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Errorf("leader not held as a zombie after the wait: %v", err)
	}
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Errorf("Wait after the no-reap wait = %v, want exit status 7", err)
	}
}

// A leader that is already a zombie when the wait starts returns at once
// (on macOS this is the ESRCH-at-registration path).
func TestWaitLeaderExitAlreadyExited(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let it exit; nothing reaps it yet
	if err := waitLeaderExitWithin(t, cmd.Process.Pid, 2*time.Second); err != nil {
		t.Fatalf("waitLeaderExit on a zombie: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("Wait = %v", err)
	}
}

// waitGroupGone polls until no process is left in group pgid.
func waitGroupGone(t *testing.T, pgid int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d still has members after %v", pgid, budget)
}

// A same-group, pipe-holding grandchild dies when the leader exits, so the
// runner no longer waits out WaitDelay for it, on a clean and on a failing
// exit alike (behaviour change of #28: it used to cost the full 2s).
func TestExecBoundedKillsGroupAtLeaderExit(t *testing.T) {
	for _, code := range []int{0, 3} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "pid")
			s := script(t, fmt.Sprintf("/bin/sleep 60 &\necho \"$! $$\" > %[1]s.tmp && /bin/mv %[1]s.tmp %[1]s\necho done\nexit %[2]d\n", pidfile, code))
			start := time.Now()
			out, _, _, err := execBounded(context.Background(), s, nil, execSpec{timeout: 10 * time.Second})
			elapsed := time.Since(start)
			if code == 0 && err != nil {
				t.Fatalf("execBounded: %v", err)
			}
			if code != 0 && err == nil {
				t.Fatal("expected the exit error")
			}
			if out != "done\n" {
				t.Errorf("stdout = %q, want the parent's output", out)
			}
			if elapsed >= pipeDrainDelay {
				t.Errorf("returned after %v; the grandchild should die with the leader, not after WaitDelay (%v)", elapsed, pipeDrainDelay)
			}
			child, leader, ok := parsePIDFile(pidfile)
			if !ok {
				t.Fatal("fixture pidfile unreadable")
			}
			waitProcessGone(t, child, 3*time.Second)
			waitGroupGone(t, leader, 3*time.Second)
		})
	}
}

// The Cancel path is unchanged: cancelling while the leader is blocked
// kills the whole group promptly, the no-reap wait is released by the
// leader's death, and nothing is left running — no group member and no
// goroutine.
func TestExecBoundedCancelWhileLeaderBlocked(t *testing.T) {
	before := runtime.NumGoroutine()
	pidfile := filepath.Join(t.TempDir(), "pid")
	s := script(t, fmt.Sprintf(hangScript, pidfile))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := execBounded(ctx, s, nil, execSpec{timeout: 30 * time.Second})
		done <- err
	}()
	child := waitInLeaderGroup(t, pidfile, 5*time.Second)
	_, leader, _ := parsePIDFile(pidfile)

	start := time.Now()
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("execBounded did not return within 5s of cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancellation returned after %v, want about a second at most", elapsed)
	}
	waitProcessGone(t, child, 3*time.Second)
	waitGroupGone(t, leader, 3*time.Second)

	// Every goroutine execBounded started (pipe copiers, the context
	// watcher) must be gone; the no-reap wait itself starts none.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("goroutines: %d before, %d after; execBounded leaked", before, n)
	}
}
