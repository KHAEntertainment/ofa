package detect

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"time"

	"ofa/internal/procgroup"
)

// This file holds the deadline-bounded runner: the same scan, but every
// command it shells out to gets a deadline and a process-group kill.
//
// DefaultRunner is unbounded by design — `ofa scan` and `ofa check` are
// allowed to wait for a slow package manager. The doctor gap check is
// not: it is an interactive command whose whole job is to report what
// it found, so a hung `brew list` must degrade into a failure for that
// method, not stall the command forever.

// DefaultCommandTimeout bounds each command the bounded runner runs. A
// package manager that hangs — a `brew list` waiting on a lock, an
// `npm ls` waiting on the network — is cut off at this deadline and
// reported as a failure for its method.
const DefaultCommandTimeout = 30 * time.Second

// Command runs one command to completion under ctx and returns its
// stdout. It is the execution seam the bounded runner drives: the
// deadline, and the kill of the command's process group, live in the
// runner, not in the command.
type Command func(ctx context.Context, name string, args ...string) (string, error)

// TimeoutError reports a command that overran its deadline. It renders
// as "timed out after 30s running brew list --versions", so the method
// it belongs to reads as "brew-formula: timed out after 30s running
// brew list --versions".
type TimeoutError struct {
	// Command is the command line that timed out, as "name args...".
	Command string
	// Limit is the deadline that was overrun.
	Limit time.Duration
}

// Error renders the timeout.
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s running %s", e.Limit, e.Command)
}

// DefaultCommand runs name with args under ctx and returns its stdout.
//
// It is exec.Command(...).Output() with a deadline attached: the child
// inherits the caller's environment, reads from the null device, keeps its
// stderr off stdout (a failing command's stderr reaches the caller through
// the exec.ExitError), and its output is returned untrimmed — the same
// shape DefaultRunner returns for the same command.
//
// One thing differs from DefaultRunner: the WaitDelay. DefaultRunner waits
// on the pipes forever when a grandchild holds them open; DefaultCommand
// bounds that wait, and a command whose output was cut short by it is
// reported as the failure it is — never as a successful read. A truncated
// package inventory is a lie the caller cannot see, and a failed detector
// is one the caller can.
//
// The process group is procgroup's: the child leads its own group, and the
// group is killed while the leader still pins its PGID, so a timeout leaves
// no orphan behind and the kill can never name a recycled PGID.
func DefaultCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	out, err := procgroup.Output(cmd)
	return string(out), err
}

// commandWaitDelay bounds how long a command's pipes may stay open after
// the command itself is gone.
const commandWaitDelay = 2 * time.Second

// TimeoutRunner returns a Runner that runs every command through cmd
// under a deadline of limit.
//
// A command that overruns the deadline is reported as a timeout and the
// runner moves on: the caller sees a failure for that command, never a
// hang. The command is abandoned rather than waited for — the exec seam
// kills its process group when the deadline passes — so the runner
// returns on time even when the command ignores the deadline entirely.
//
// A limit of zero or less expires every command immediately; callers
// pass a positive limit.
func TimeoutRunner(cmd Command, limit time.Duration) Runner {
	return func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		call := strings.Join(append([]string{name}, args...), " ")
		type result struct {
			out string
			err error
		}
		done := make(chan result, 1)
		go func() {
			out, err := cmd(ctx, name, args...)
			done <- result{out, err}
		}()
		select {
		case r := <-done:
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return r.out, &TimeoutError{Command: call, Limit: limit}
			}
			return r.out, r.err
		case <-ctx.Done():
			return "", &TimeoutError{Command: call, Limit: limit}
		}
	}
}

// TimeoutRunnerFor swaps the unbounded production default for the
// bounded runner: DefaultRunner is the only runner that shells out
// without a deadline, so it is the only one that needs bounding. An
// injected runner passes through untouched — the same swap pattern as
// doctor.BoundedRunnerFor.
func TimeoutRunnerFor(run Runner, limit time.Duration) Runner {
	if !IsDefaultRunner(run) {
		return run
	}
	return TimeoutRunner(DefaultCommand, limit)
}

// IsDefaultRunner reports whether run is detect.DefaultRunner itself.
func IsDefaultRunner(run Runner) bool {
	if run == nil {
		return false
	}
	return reflect.ValueOf(run).Pointer() == reflect.ValueOf(Runner(DefaultRunner)).Pointer()
}
