//go:build linux || darwin

package procgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// #28: group kills are sent only while the leader is unreaped, so they can
// only reach the group our child created. These tests pin the no-reap wait,
// the guard that keeps every group kill apart from the reap, and the runner
// behaviour built on them. Fixtures are /bin/sh and /bin/sleep only; nothing
// here runs a real agent. The host may be heavily loaded: no assertion
// depends on wall-clock speed. Waits are bounded only to turn a hang into a
// failure, and their budgets stay below the fixtures' 60s sleeps, so a
// grandchild that was NOT killed still fails the test.

// generous bounds every wait. It is far above any plausible scheduling
// delay and well below the fixtures' 60s sleeps.
const generous = 20 * time.Second

// script writes an executable bash script into a temp dir and returns its
// path.
func script(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(p, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// parsePIDFile reads a fixture's "<grandchild> <leader>" pidfile.
func parsePIDFile(path string) (child, leader int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, 0, false
	}
	child, err = strconv.Atoi(f[0])
	if err != nil {
		return 0, 0, false
	}
	leader, err = strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, false
	}
	return child, leader, true
}

// processGone reports whether pid is no longer running.
func processGone(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) != nil
}

// waitProcessGone polls until pid disappears or the budget expires.
func waitProcessGone(t *testing.T, pid int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d still alive after %v", pid, budget)
}

// waitInLeaderGroup is the readiness handshake that lets timing-sensitive
// assertions arm only after the fixture is in place. It returns once the
// grandchild named in the pidfile is a member of the leader's process
// group (its pgid is the leader's pid, which Setpgid made the group id).
// A pidfile that merely exists proves only that the shell ran the echo,
// and the group kill is what the assertions depend on. The fixtures write
// "<grandchild> <leader>" to a temp file and rename it into place, so a
// half-written pidfile is never read.
func waitInLeaderGroup(t *testing.T, path string, budget time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if child, leader, ok := parsePIDFile(path); ok {
			if pgid, err := syscall.Getpgid(child); err == nil && pgid == leader {
				return child
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild in %s never joined the leader's process group within %v", path, budget)
	return 0
}

// hangScript: spawns a /bin/sleep 60 grandchild, then sleeps 60 itself.
const hangScript = `#!/bin/bash
echo started
/bin/sleep 60 &
echo "$! $$" > %[1]s.tmp && /bin/mv %[1]s.tmp %[1]s
/bin/sleep 60
`

// killFixtureOnFailure kills the processes a fixture recorded when the test
// ends, so a failed assertion leaves no sleep 60 behind. The fixtures
// record their pids in a pidfile, so the cleanup reads it at the end: a
// test that dies before the pidfile exists has no process to clean up.
func killFixtureOnFailure(t *testing.T, pidfile string) {
	t.Helper()
	t.Cleanup(func() {
		child, leader, ok := parsePIDFile(pidfile)
		if !ok {
			return
		}
		if child > 0 {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
		if leader > 0 {
			_ = syscall.Kill(leader, syscall.SIGKILL)
		}
	})
}

// startChild starts cmd and registers a cleanup that kills and reaps it if
// the test ends before it did, so a fatal path leaves no process behind.
// The returned func reaps it and must be the only Wait call.
func startChild(t *testing.T, cmd *exec.Cmd) func() error {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var werr error
	wait := func() error {
		once.Do(func() { werr = cmd.Wait() })
		return werr
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = wait()
	})
	return wait
}

// waitLeaderExitWithin runs waitLeaderExit with a bound, so a regression
// that blocks forever fails the test instead of hanging the suite.
func waitLeaderExitWithin(t *testing.T, pid int) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- waitLeaderExit(pid) }()
	select {
	case err := <-done:
		return err
	case <-time.After(generous):
		t.Fatalf("waitLeaderExit(%d) still blocked after %v", pid, generous)
		return nil
	}
}

// waitZombie polls until pid is observed in the zombie state: exited, not
// yet reaped. ps reads the state without touching it.
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(generous)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never observed as a zombie within %v", pid, generous)
}

// The wait must not reap: Wait afterwards still collects the real exit
// status. Had waitLeaderExit reaped the child, Wait would fail with ECHILD.
func TestWaitLeaderExitDoesNotReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	wait := startChild(t, cmd)
	if err := waitLeaderExitWithin(t, cmd.Process.Pid); err != nil {
		t.Fatalf("waitLeaderExit: %v", err)
	}
	waitZombie(t, cmd.Process.Pid) // exited, and still held unreaped
	var ee *exec.ExitError
	if err := wait(); !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Errorf("Wait after the no-reap wait = %v, want exit status 7", err)
	}
}

// A leader that is already a zombie when the wait starts returns at once.
// On macOS this exercises the ESRCH-at-registration branch.
func TestWaitLeaderExitAlreadyExited(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	wait := startChild(t, cmd)
	waitZombie(t, cmd.Process.Pid)
	if err := waitLeaderExitWithin(t, cmd.Process.Pid); err != nil {
		t.Fatalf("waitLeaderExit on a zombie: %v", err)
	}
	if err := wait(); err != nil {
		t.Errorf("Wait = %v", err)
	}
}

// killRecord is one observed groupKill call.
type killRecord struct {
	pgid         int
	afterRelease bool
}

// recordGroupKills swaps groupKill for a recorder that still sends the
// real kill, so fixtures die exactly as in production. beforeWait flips the
// phase flag, and then runs extra, if extra is non-nil. Both package
// variables are restored at cleanup.
func recordGroupKills(t *testing.T, extra func(*exec.Cmd, *groupGuard)) (calls func() []killRecord) {
	t.Helper()
	var (
		mu       sync.Mutex
		records  []killRecord
		released atomic.Bool
	)
	realKill, realBefore := groupKill, beforeWait
	groupKill = func(pgid int) {
		mu.Lock()
		records = append(records, killRecord{pgid, released.Load()})
		mu.Unlock()
		realKill(pgid)
	}
	beforeWait = func(cmd *exec.Cmd, g *groupGuard) {
		released.Store(true)
		if extra != nil {
			extra(cmd, g)
		}
	}
	t.Cleanup(func() { groupKill, beforeWait = realKill, realBefore })
	return func() []killRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]killRecord(nil), records...)
	}
}

func TestGroupGuardRefusesAfterRelease(t *testing.T) {
	var g groupGuard
	ran := 0
	if !g.do(func() { ran++ }) || ran != 1 {
		t.Fatalf("before release: ran = %d, want 1", ran)
	}
	g.release()
	if g.do(func() { ran++ }) || ran != 1 {
		t.Fatalf("after release: ran = %d, want still 1", ran)
	}
}

// B1: os/exec may call Cancel after Wait has reaped the leader. This drives
// that interleaving deterministically: the Cancel hook runs between the
// guard's release and Wait, exactly where a late Cancel would land. The
// post-exit kill must have been sent before release, and the late Cancel
// must send nothing.
func TestRunLateCancelSendsNoGroupKill(t *testing.T) {
	lateCancelRan := false
	calls := recordGroupKills(t, func(cmd *exec.Cmd, g *groupGuard) {
		_ = killProcessGroup(cmd, g)
		lateCancelRan = true
	})
	pidfile := filepath.Join(t.TempDir(), "pid")
	killFixtureOnFailure(t, pidfile)
	s := script(t, fmt.Sprintf("/bin/sleep 60 &\necho \"$! $$\" > %[1]s.tmp && /bin/mv %[1]s.tmp %[1]s\nexit 0\n", pidfile))
	cmd := exec.CommandContext(context.Background(), s)
	cmd.WaitDelay = generous
	if err := Run(cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !lateCancelRan {
		t.Fatal("beforeWait seam never ran")
	}
	_, leader, ok := parsePIDFile(pidfile)
	if !ok {
		t.Fatal("fixture pidfile unreadable")
	}
	got := calls()
	if len(got) != 1 || got[0] != (killRecord{leader, false}) {
		t.Fatalf("group kills = %+v, want exactly one, before release, to %d", got, leader)
	}
}

// A same-group, pipe-holding grandchild is killed when the leader exits,
// before release, on a clean and on a failing exit alike. Behaviour change
// of #28: it used to get WaitDelay longer. Without the kill, the grandchild
// would outlive these bounded waits (it sleeps 60s).
func TestRunKillsGroupAtLeaderExit(t *testing.T) {
	for _, code := range []int{0, 3} {
		t.Run(fmt.Sprintf("exit %d", code), func(t *testing.T) {
			calls := recordGroupKills(t, nil)
			pidfile := filepath.Join(t.TempDir(), "pid")
			killFixtureOnFailure(t, pidfile)
			s := script(t, fmt.Sprintf("/bin/sleep 60 &\necho \"$! $$\" > %[1]s.tmp && /bin/mv %[1]s.tmp %[1]s\necho done\nexit %[2]d\n", pidfile, code))
			var stdout bytes.Buffer
			cmd := exec.CommandContext(context.Background(), s)
			cmd.Stdout = &stdout
			cmd.WaitDelay = generous
			err := Run(cmd)
			if code == 0 && err != nil {
				t.Fatalf("Run: %v", err)
			}
			if code != 0 && err == nil {
				t.Fatal("expected the exit error")
			}
			if stdout.String() != "done\n" {
				t.Errorf("stdout = %q, want the parent's output", stdout.String())
			}
			child, leader, ok := parsePIDFile(pidfile)
			if !ok {
				t.Fatal("fixture pidfile unreadable")
			}
			if got := calls(); len(got) != 1 || got[0] != (killRecord{leader, false}) {
				t.Errorf("group kills = %+v, want exactly one, before release, to %d", got, leader)
			}
			waitProcessGone(t, child, generous)
			waitGroupGone(t, leader)
		})
	}
}

// waitGroupGone polls until no process is left in group pgid.
func waitGroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(generous)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d still has members after %v", pgid, generous)
}

// Cancelling while the leader is blocked kills the whole group through the
// guarded Cancel, before release. The no-reap wait is then released by the
// leader's death and Run returns: no group member is left, and no goroutine
// either (Run returning is the proof: the no-reap wait runs on its caller's
// goroutine and starts none).
func TestRunCancelWhileLeaderBlocked(t *testing.T) {
	calls := recordGroupKills(t, nil)
	pidfile := filepath.Join(t.TempDir(), "pid")
	killFixtureOnFailure(t, pidfile)
	s := script(t, fmt.Sprintf(hangScript, pidfile))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		cmd := exec.CommandContext(ctx, s)
		cmd.WaitDelay = 2 * generous
		done <- Run(cmd)
	}()
	child := waitInLeaderGroup(t, pidfile, generous)
	_, leader, _ := parsePIDFile(pidfile)

	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(generous):
		t.Fatalf("Run did not return within %v of cancellation", generous)
	}
	// Run reports the kill; the caller maps a cancelled context to its own
	// error, which is what doctor's execBounded does with the very same
	// call.
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	got := calls()
	if len(got) == 0 {
		t.Fatal("no group kill was sent on cancellation")
	}
	for _, c := range got {
		if c != (killRecord{leader, false}) {
			t.Errorf("group kill %+v, want only kills to %d before release", c, leader)
		}
	}
	waitProcessGone(t, child, generous)
	waitGroupGone(t, leader)
}

// A failing command's stderr reaches the caller through the ExitError, the
// way exec.Cmd.Output delivers it, and never through stdout.
func TestOutputCarriesStderrOnFailure(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf oops >&2; exit 3")
	out, err := Output(cmd)
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want an ExitError", err, err)
	}
	if len(out) != 0 {
		t.Errorf("stdout = %q, want it empty", out)
	}
	if !strings.Contains(string(ee.Stderr), "oops") {
		t.Errorf("ExitError.Stderr = %q, want the command's stderr", ee.Stderr)
	}
}

// A command that wrote more than the window keeps its head and its tail,
// and says how many bytes were left out — the window exec.Cmd.Output
// exposes.
func TestOutputKeepsTheStderrWindow(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c",
		`printf HEAD-MARKER >&2; head -c 100000 /dev/zero | tr "\0" "x" >&2; printf TAIL-MARKER >&2; exit 3`)
	out, err := Output(cmd)
	if err == nil {
		t.Fatal("a failing command returned no error")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want an ExitError", err, err)
	}
	if len(out) != 0 {
		t.Errorf("stdout = %q, want it empty", out)
	}
	stderr := string(ee.Stderr)
	if !strings.Contains(stderr, "HEAD-MARKER") {
		t.Error("the stderr window lost the head")
	}
	if !strings.Contains(stderr, "TAIL-MARKER") {
		t.Error("the stderr window lost the tail")
	}
	if !strings.Contains(stderr, "omitting") {
		t.Errorf("stderr = %q, want it to report the bytes left out", stderr)
	}
	if len(stderr) > 64*1024+64 {
		t.Errorf("stderr window = %d bytes, want at most 64KB plus the marker", len(stderr))
	}
}
