package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"ofa/internal/detect"
)

// Bounded subprocess execution for every command doctor runs itself: the
// facts probes, the test-detect registry queries and version probes, and
// the drafting agent. Every command gets a deadline, an output cap enforced
// while reading (not after buffering), /dev/null on stdin unless the caller
// supplies one, a fresh empty working directory, and a process-group kill —
// so a hung binary, a chatty binary, or a grandchild holding the pipes can
// never hang or bloat `ofa doctor`.

const (
	// targetCommandDeadline bounds commands that are the target binary
	// itself: the facts probes and the version probes of test-detect.
	targetCommandDeadline = 10 * time.Second
	// registryCommandDeadline bounds registry listing commands, which are
	// slower by nature (brew/npm global lists).
	registryCommandDeadline = 20 * time.Second
	// commandStdoutCap / commandStderrCap bound subprocess output while it
	// is being read.
	commandStdoutCap = 64 * 1024
	commandStderrCap = 4 * 1024
	// pipeDrainDelay bounds how long a command's pipes stay open after the
	// process exits or is cancelled: a grandchild inheriting stdout cannot
	// hold Wait() hostage past this (a 120s agent timeout once stretched
	// to 301s exactly this way).
	pipeDrainDelay = 2 * time.Second
)

// registryCommands are the fixed registry probes detect itself issues; they
// are the one class of command that needs the caller's normal environment.
var registryCommands = map[string]bool{"brew": true, "npm": true, "pipx": true, "uv": true}

// minimalCommandEnv is the environment for commands that ARE the target
// binary: enough to execute and to read the user's identity, and nothing
// else. The target's --version/--help output is untrusted, so it gets no
// secrets, no proxy configuration, and no working directory it could learn
// from.
func minimalCommandEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TERM=dumb",
	}
	for _, k := range []string{"LANG", "USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// NewBoundedRunner returns the production Runner doctor uses for facts and
// test-detect. Commands that are the target binary — by path or by basename,
// the two forms detect uses — run with the short deadline and a minimal
// environment. Registry commands (brew/npm/pipx/uv, fixed argv issued by
// detect itself) inherit the caller's environment with the longer deadline.
// Anything else gets the target's conservative treatment: it could only
// come from a draft that failed the shape gate, and the safe default is the
// right default.
func NewBoundedRunner(targetPath string) detect.Runner {
	targetBase := filepath.Base(targetPath)
	return func(name string, args ...string) (string, error) {
		spec := execSpec{
			timeout:   targetCommandDeadline,
			env:       minimalCommandEnv(),
			maxStdout: commandStdoutCap,
			maxStderr: commandStderrCap,
		}
		base := filepath.Base(name)
		if base != targetBase && name != targetPath && registryCommands[base] {
			spec.timeout = registryCommandDeadline
			spec.env = nil // inherit: registry tools need the normal environment
		}
		out, errOut, _, err := execBounded(context.Background(), name, args, spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w%s", name, err, streamSnippet("", errOut))
		}
		return out, nil
	}
}

// BoundedRunnerFor swaps detect.DefaultRunner — the unbounded production
// default — for the bounded runner bound to the target's path. An injected
// test runner passes through untouched, so the Runner seam keeps working
// exactly as it did.
func BoundedRunnerFor(run detect.Runner, targetPath string) detect.Runner {
	if sameRunner(run, detect.DefaultRunner) {
		return NewBoundedRunner(targetPath)
	}
	return run
}

// sameRunner reports whether two Runner values denote the same function.
func sameRunner(a, b detect.Runner) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// execSpec configures one bounded subprocess.
type execSpec struct {
	// timeout bounds the call; the context's own deadline, when earlier,
	// still wins.
	timeout time.Duration
	// env is the exact environment; nil inherits the caller's.
	env []string
	// stdin replaces /dev/null when set (the agent's prompt).
	stdin io.Reader
	// maxStdout / maxStderr default to commandStdoutCap / commandStderrCap.
	maxStdout int
	maxStderr int
}

// execBounded runs one command with the guarantees above and returns
// stdout, stderr, whether stdout was truncated, and the error. A
// context deadline surfaces as the error; a clean process exit whose pipes
// a straggler kept open past WaitDelay is a success with whatever output
// was collected.
func execBounded(ctx context.Context, name string, args []string, spec execSpec) (string, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "ofa-doctor-run-")
	if err != nil {
		return "", "", false, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	stdin := spec.stdin
	if stdin == nil {
		f, err := os.Open(os.DevNull)
		if err != nil {
			return "", "", false, err
		}
		defer f.Close()
		stdin = f
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = spec.env
	cmd.Stdin = stdin
	stdout := &cappedBuffer{limit: orDefault(spec.maxStdout, commandStdoutCap)}
	stderr := &cappedBuffer{limit: orDefault(spec.maxStderr, commandStderrCap)}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The child leads its own process group so cancellation kills the
	// whole tree, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// WaitDelay bounds the pipe waits: once the process is gone, stragglers
	// holding the pipes cost at most this.
	cmd.WaitDelay = pipeDrainDelay
	cmd.Cancel = func() error { return killProcessGroup(cmd) }

	err = cmd.Run()
	// Reap the whole group after Wait: children the command left behind are
	// doctor's to clean up, not the user's.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), stderr.String(), stdout.truncated, ctxErr
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.Is(err, exec.ErrWaitDelay) && !errors.As(err, &ee) {
			// The process exited cleanly; a straggler kept the pipes open
			// past WaitDelay. The capped output is what it printed.
			err = nil
		}
	}
	if err != nil {
		return stdout.String(), stderr.String(), stdout.truncated, err
	}
	return stdout.String(), stderr.String(), stdout.truncated, nil
}

// killProcessGroup is the exec.Cancel hook: on context cancellation it
// kills the child's whole process group (the child is the group leader via
// Setpgid), so descendants die with it.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
	return nil
}

// streamSnippet renders a bounded diagnostic of what a failed subprocess
// printed: at most 512 bytes per stream, ≤1KB total, quoted to one line.
// claude reports its failures on stdout with a non-zero exit — stderr alone
// diagnosed nothing — so both streams are included.
func streamSnippet(stdout, stderr string) string {
	const budget = 512
	var parts []string
	if s := truncateBytes(stdout, budget); s != "" {
		parts = append(parts, "stdout: "+strconv.Quote(s))
	}
	if s := truncateBytes(stderr, budget); s != "" {
		parts = append(parts, "stderr: "+strconv.Quote(s))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// cappedBuffer is a bytes.Buffer that stops storing after limit bytes,
// flagging the cut. Write always reports success so the subprocess never
// sees a write error — the overflow is simply discarded while it streams.
type cappedBuffer struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
	} else {
		w.buf.Write(p)
	}
	return len(p), nil
}

func (w *cappedBuffer) String() string { return w.buf.String() }

// truncateBytes caps s at max bytes, cutting on a rune boundary so the
// result is always valid UTF-8.
func truncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
