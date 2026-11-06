// Package procgroup runs a command in its own process group and kills the
// whole group without ever signalling a group that may no longer be ours.
//
// A PGID is the leader's PID, and the kernel (Linux and XNU alike) keeps a
// PID allocated while its process exists, zombie or not. So while a child
// is unreaped, -pgid can only name the group that child created, which
// holds our descendants and nothing else. Once the child is reaped its PID
// is free: when the last member exits, an unrelated process can be given
// the PID, make itself a group leader, and receive a SIGKILL meant for us.
//
// The lifecycle here keeps every group kill apart from that reap: the
// leader's exit is waited for WITHOUT reaping it, the group kill is sent
// while the leader still pins the PGID, and the guard is released before
// Wait on every path. os/exec may call Cancel AFTER Wait has reaped the
// leader — its context watcher can see ctx.Done in the same moment Wait
// completes — so the guard, not the caller's ordering, is what makes the
// kill safe.
//
// The package is unix-only: it signals a process group, which is a unix
// concept. ofa ships only for Linux and macOS.
package procgroup

import (
	"bytes"
	"errors"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// Run starts cmd, kills what is left of its process group once the leader
// has exited (but before the leader is reaped), and waits for the command
// to finish.
//
// cmd must come from exec.CommandContext: Run installs the Cancel hook
// that stops the command when its context is done, and os/exec only calls
// that hook for a command created that way.
//
// Run owns the process group: the child leads its own group, so the
// cancellation kill reaches its descendants too. The caller owns
// everything else — the context, the environment, the working directory,
// the standard streams and the WaitDelay.
func Run(cmd *exec.Cmd) error {
	// The child leads its own process group so cancellation kills the
	// whole tree, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	guard := &groupGuard{}
	cmd.Cancel = func() error { return killProcessGroup(cmd, guard) }

	if err := cmd.Start(); err != nil {
		return err
	}
	// The leader's exit is waited for without reaping it, so its PID — and
	// therefore its PGID — stays allocated. What is left of the group is
	// killed while the leader still pins it.
	killGroupBeforeReap(cmd.Process.Pid, guard)
	// From here on no group kill may be sent: Wait is about to reap the
	// leader and free its PID.
	guard.release()
	if beforeWait != nil {
		beforeWait(cmd, guard)
	}
	return cmd.Wait()
}

// Output runs cmd and returns its stdout, the way exec.Cmd.Output does:
// the child's stderr is kept out of stdout and reaches the caller through
// the ExitError, and a caller that set its own stderr writer keeps it.
//
// The stderr window mirrors exec's: the first and the last 32KB of what the
// command wrote, with the count of the bytes left out between them.
func Output(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("procgroup: Stdout already set")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	captureErr := cmd.Stderr == nil
	stderr := stderrSaver{limit: stderrWindow}
	if captureErr {
		cmd.Stderr = &stderr
	}

	err := Run(cmd)
	if err != nil && captureErr {
		if ee, ok := err.(*exec.ExitError); ok {
			ee.Stderr = stderr.Bytes()
		}
	}
	return stdout.Bytes(), err
}

// groupKill sends SIGKILL to process group pgid. It is a variable only so
// tests can observe which group kills are sent, and when.
var groupKill = func(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }

// beforeWait, when a test sets it, runs between guard.release and Wait:
// the one point where a late Cancel must already be a no-op.
var beforeWait func(cmd *exec.Cmd, g *groupGuard)

// groupGuard keeps every group kill apart from the reap. A PGID is the
// leader's PID, and the kernel (Linux and XNU alike) keeps a PID allocated
// while its process exists, zombie or not. So while the leader is unreaped,
// -pgid can only name the group the leader created, which holds our
// descendants and nothing else. Once Wait reaps the leader the PID is free:
// when the last member exits, an unrelated process can be given the PID,
// make itself a group leader, and receive a SIGKILL meant for us.
//
// Run calls release before Wait. Every kill (the post-exit group kill, and
// Cancel's group and leader kills) runs under the same mutex and is skipped
// after release, so no kill can run after the reap. This matters because
// os/exec may call Cancel AFTER Wait has reaped the leader: its context
// watcher can see ctx.Done in the same moment Wait completes.
type groupGuard struct {
	mu       sync.Mutex
	released bool
}

// do runs signal under the guard unless the guard has been released, and
// reports whether it ran.
func (g *groupGuard) do(signal func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return false
	}
	signal()
	return true
}

// release forbids every later group kill.
func (g *groupGuard) release() {
	g.mu.Lock()
	g.released = true
	g.mu.Unlock()
}

// killGroupBeforeReap kills what is left of the child's process group once
// the leader has exited, on EVERY outcome: a parent that exits non-zero
// still leaves its same-group, pipe-holding children behind (R3-B2).
// waitLeaderExit blocks until the leader exits WITHOUT reaping it, so the
// kill is sent while the zombie leader still pins the PGID (see groupGuard).
//
// Behaviour (#28): same-group descendants die when the leader exits. They
// used to get up to WaitDelay (2s) longer. Output they would have written
// after the leader's exit is lost; output written before it is kept.
//
// What is guaranteed: no group kill is sent once the leader may have been
// reaped. What is not: deliberately detached (setsid) descendants are
// outside the group and are not chased; WaitDelay alone bounds how long
// they can hold the pipes. When the no-reap wait is unavailable
// (reap_other.go) or fails, no kill is sent here at all: a straggler that
// WaitDelay bounds is better than a signal to a group that may not be ours.
func killGroupBeforeReap(pid int, g *groupGuard) {
	if waitLeaderExit(pid) == nil {
		g.do(func() { groupKill(pid) })
	}
}

// killProcessGroup is the exec.Cancel hook. On context cancellation while
// the leader is unreaped (alive, or a zombie still pinning its PGID), it
// kills the whole group and then the leader, so descendants die with it and
// the leader's exit releases the no-reap wait.
//
// Not guaranteed: when Cancel runs after the guard has been released
// (os/exec's watcher lost the race with a finished child), it sends
// nothing, because the PGID may already be recycled. On the fallback path
// (the no-reap wait unavailable or failed), the guard is released before
// Wait while the leader may still be running. A timeout there is enforced
// only by os/exec's own Process.Kill of the leader after WaitDelay, and
// same-group descendants are not chased.
func killProcessGroup(cmd *exec.Cmd, g *groupGuard) error {
	if cmd.Process != nil {
		pid := cmd.Process.Pid
		g.do(func() {
			groupKill(pid)
			_ = cmd.Process.Kill()
		})
	}
	return nil
}

// stderrWindow is the size of the head and of the tail Output keeps: the
// same 32KB window exec.Cmd.Output exposes.
const stderrWindow = 32 << 10

// stderrSaver keeps the first and the last limit bytes of a stream: the
// window exec.Cmd.Output exposes through ExitError.Stderr. A command that
// wrote more sees its head, its tail, and how much was left out.
type stderrSaver struct {
	limit   int
	prefix  []byte
	suffix  []byte
	off     int
	skipped int64
}

// Write keeps the head and the tail, and always reports success: the
// subprocess must never see a write error.
func (s *stderrSaver) Write(p []byte) (int, error) {
	n := len(p)
	p = s.fill(&s.prefix, p)
	// Only the last limit bytes of the remainder are kept.
	if over := len(p) - s.limit; over > 0 {
		p = p[over:]
		s.skipped += int64(over)
	}
	p = s.fill(&s.suffix, p)
	// The suffix is full now if p is non-empty. Overwrite it in a circle.
	for len(p) > 0 {
		c := copy(s.suffix[s.off:], p)
		p = p[c:]
		s.skipped += int64(c)
		s.off += c
		if s.off == s.limit {
			s.off = 0
		}
	}
	return n, nil
}

// fill appends to dst until it holds limit bytes, and returns the rest.
func (s *stderrSaver) fill(dst *[]byte, p []byte) []byte {
	if room := s.limit - len(*dst); room > 0 {
		add := min(room, len(p))
		*dst = append(*dst, p[:add]...)
		p = p[add:]
	}
	return p
}

// Bytes renders the saved window: the head, the tail, and the count of the
// bytes left out between them.
func (s *stderrSaver) Bytes() []byte {
	if s.suffix == nil {
		return s.prefix
	}
	if s.skipped == 0 {
		return append(s.prefix, s.suffix...)
	}
	var buf bytes.Buffer
	buf.Write(s.prefix)
	buf.WriteString("\n... omitting ")
	buf.WriteString(strconv.FormatInt(s.skipped, 10))
	buf.WriteString(" bytes ...\n")
	buf.Write(s.suffix[s.off:])
	buf.Write(s.suffix[:s.off])
	return buf.Bytes()
}
