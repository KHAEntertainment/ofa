package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// registryArgv is the exact set of registry command lines detect itself
// issues (internal/detect/methods.go) — keyed by command name, each entry a
// fixed argv. They are the one class of command that needs the caller's
// normal environment, and the only argv a registry tool will ever be run
// with: a name that looks like a registry tool but carries any other argv
// is refused, so a draft cannot smuggle arguments past the runner.
var registryArgv = map[string][][]string{
	"brew": {{"list", "--versions"}, {"list", "--cask", "--versions"}},
	"npm":  {{"ls", "-g", "--depth=0", "--json"}},
	"pipx": {{"list", "--json"}},
	"uv":   {{"tool", "list"}},
}

// targetArgs is the only argv the pinned target may be probed with: the
// version flag the drafts are gated to, and the help flag the facts phase
// itself uses.
var targetArgs = map[string]bool{"--version": true, "--help": true}

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
// test-detect, with the target PINNED at execution time (R2-B2): the
// authorized binary is resolved once, up front, to its canonical real path,
// and every command that claims to be the target — by name or by any path
// whose basename is the bin — is executed as that pinned path, never as a
// path a draft supplied. A symlink swapped, or a file created, between the
// static gate and the exec therefore changes nothing: the drafted path is
// not what runs. Registry commands run only with the exact fixed argv
// detect itself defines. Everything else is refused without executing —
// fail closed.
func NewBoundedRunner(bin, pinned string) detect.Runner {
	return func(name string, args ...string) (string, error) {
		if name == bin || filepath.Base(name) == bin {
			if len(args) != 1 || !targetArgs[args[0]] {
				return "", fmt.Errorf("doctor runner: refused to probe %s with %q: only --version/--help is allowed", bin, args)
			}
			out, errOut, _, err := execBounded(context.Background(), pinned, args, execSpec{
				timeout:   targetCommandDeadline,
				env:       minimalCommandEnv(),
				argv0:     bin,
				maxStdout: commandStdoutCap,
				maxStderr: commandStderrCap,
			})
			if err != nil {
				return out, fmt.Errorf("%s: %w%s", bin, err, streamSnippet("", errOut))
			}
			return out, nil
		}
		if isRegistryCommand(name, args) {
			out, errOut, _, err := execBounded(context.Background(), name, args, execSpec{
				timeout:   registryCommandDeadline,
				env:       nil, // inherit: registry tools need the normal environment
				maxStdout: commandStdoutCap,
				maxStderr: commandStderrCap,
			})
			if err != nil {
				return out, fmt.Errorf("%s: %w%s", name, err, streamSnippet("", errOut))
			}
			return out, nil
		}
		return "", fmt.Errorf("doctor runner: refused to execute %q: not the pinned target %q or a fixed registry command", name, bin)
	}
}

// isRegistryCommand reports whether (name, args) is one of the exact
// command lines detect itself issues for registry sweeps. The name must be
// the LITERAL command name — detect never issues a path — so a path whose
// basename happens to be a registry name is refused like anything else
// (R3-A1): the helper's fail-closed claim must not have a path-shaped hole.
func isRegistryCommand(name string, args []string) bool {
	argvs, ok := registryArgv[name]
	if !ok {
		return false
	}
	for _, want := range argvs {
		if len(args) == len(want) {
			match := true
			for i := range want {
				if args[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// canonicalTarget resolves the authorized executable for bin once: the
// PATH winner, with every symlink resolved. A resolution failure is an
// ERROR, never a fallback to the unresolved alias: the alias is exactly
// what an attacker with write access to that PATH entry controls, and a
// resolution race means the file changed under us between LookPath and
// EvalSymlinks (R3-B1). The caller refuses to build a runner on error.
func canonicalTarget(bin string) (string, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	return resolveCanonical(p)
}

// AuthorizedTarget resolves bin to its PATH winner and its canonical real
// path (every symlink resolved). The install flow uses it to recover the
// target a recorded session drafted for, so revalidation binds native
// probe paths to the same executable identity the original run used —
// never to an alias the proposal file itself could have chosen.
func AuthorizedTarget(bin string) (path, real string, err error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", "", err
	}
	real, err = resolveCanonical(p)
	if err != nil {
		return "", "", err
	}
	return p, real, nil
}

// resolveCanonical resolves one path to its canonical real path, failing
// on any resolution error — a dangling or vanishing symlink must not
// silently degrade into the alias itself.
func resolveCanonical(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve authorized target %q: %w", p, err)
	}
	return real, nil
}

// BoundedRunnerFor swaps detect.DefaultRunner — the unbounded production
// default — for the pinned bounded runner: the target's canonical path is
// resolved once, here, before any draft is even asked for. An injected test
// runner passes through untouched, so the Runner seam keeps working exactly
// as it did. If the canonical target cannot be resolved — the bin vanished,
// or its PATH alias changed under us mid-resolution (R3-B1) — the result
// refuses every command rather than executing anything unpinned.
func BoundedRunnerFor(run detect.Runner, bin string) detect.Runner {
	if !sameRunner(run, detect.DefaultRunner) {
		return run
	}
	pinned, err := canonicalTarget(bin)
	if err != nil {
		return func(name string, args ...string) (string, error) {
			return "", fmt.Errorf("doctor runner: refused to execute %q: could not resolve the authorized target: %v", name, err)
		}
	}
	return NewBoundedRunner(bin, pinned)
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
	// argv0, when set, is the child's argv[0] instead of the executable
	// path — the pinned target runs as the bin name, exactly as a direct
	// `run(bin, ...)` would have presented it.
	argv0 string
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
	if spec.argv0 != "" {
		cmd.Args[0] = spec.argv0
	}
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
	guard := &groupGuard{}
	cmd.Cancel = func() error { return killProcessGroup(cmd, guard) }

	if err = cmd.Start(); err == nil {
		killGroupBeforeReap(cmd.Process.Pid, guard)
		// From here on no group kill may be sent: Wait is about to reap the
		// leader and free its PID.
		guard.release()
		if beforeWait != nil {
			beforeWait(cmd, guard)
		}
		err = cmd.Wait()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.Is(err, exec.ErrWaitDelay) && !errors.As(err, &ee) {
			// The process itself exited cleanly; a straggler outside the
			// group (setsid) kept the pipes open past WaitDelay. Treat it as
			// success with the output we collected.
			err = nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), stderr.String(), stdout.truncated, ctxErr
	}
	if err != nil {
		return stdout.String(), stderr.String(), stdout.truncated, err
	}
	return stdout.String(), stderr.String(), stdout.truncated, nil
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
// execBounded calls release before Wait. Every kill (the post-exit group
// kill, and Cancel's group and leader kills) runs under the same mutex and
// is skipped after release, so no kill can run after the reap. This matters because
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

// streamSnippet renders a bounded diagnostic of what a failed subprocess
// printed: at most 512 bytes per stream, ≤1KB total, quoted to one line,
// with credential values redacted before truncation. claude reports its
// failures on stdout with a non-zero exit — stderr alone diagnosed nothing
// — so both streams are included, and a failing agent may echo the
// credentials it was passed: the snippet must not persist them into error
// messages and context.md.
func streamSnippet(stdout, stderr string) string {
	const budget = 512
	var parts []string
	if s := truncateBytes(redactSecrets(stdout), budget); s != "" {
		parts = append(parts, "stdout: "+strconv.Quote(s))
	}
	if s := truncateBytes(redactSecrets(stderr), budget); s != "" {
		parts = append(parts, "stderr: "+strconv.Quote(s))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// credentialVars are the environment variables doctor passes to the agent;
// their exact values are replaced wherever they appear in subprocess
// diagnostics and persisted output.
var credentialVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// credentialForms returns every form of the credential values that
// redaction and the proposal gate match: each set value raw, and its
// url.QueryEscape and url.PathEscape encodings (the two encodings an agent
// echoing a token into a URL or a query string produces). Duplicates are
// dropped, and the result is sorted longest first so an overlapping pair
// (one form a prefix or substring of another) cannot leave the longer
// form's remainder behind.
func credentialForms() []string {
	seen := map[string]bool{}
	var forms []string
	for _, k := range credentialVars {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		for _, f := range []string{v, url.QueryEscape(v), url.PathEscape(v)} {
			if !seen[f] {
				seen[f] = true
				forms = append(forms, f)
			}
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// redactSecrets replaces every credentialForms form of the credential
// variables doctor passes to the agent with [redacted]. Matching is by
// exact value and these two encodings only: it is not a credential-format
// scanner, and values that never crossed the agent boundary are not
// guessed at. PARTIAL echoes (a prefix, a suffix, a value split across
// lines) are out of scope by decision: any fixed-length fragment rule
// either misses a shorter fragment or starts redacting ordinary text that
// happens to share characters with a token, and a corrupted audit trail is
// worse than a documented limit.
func redactSecrets(s string) string {
	for _, f := range credentialForms() {
		s = strings.ReplaceAll(s, f, "[redacted]")
	}
	return s
}

// containsSecret reports whether s holds any credentialForms form.
func containsSecret(s string) bool {
	for _, f := range credentialForms() {
		if strings.Contains(s, f) {
			return true
		}
	}
	return false
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
