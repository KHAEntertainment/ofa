package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- agent: no test here spawns the real agent. The runner's subprocess
// behavior is exercised against a fake `claude` script on a PATH that
// contains nothing else, so the real binary is unreachable. ---

func TestClaudeArgsToolLessInvocation(t *testing.T) {
	want := []string{
		"-p",
		"--safe-mode",
		"--tools", "",
		"--no-session-persistence",
		"--permission-mode", "dontAsk",
	}
	if got := claudeArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("claudeArgs() = %v, want %v", got, want)
	}
}

func TestClaudeEnvMinimalPlusAuthAndIdentity(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("USER", "tester")
	t.Setenv("LOGNAME", "tester")
	t.Setenv("XDG_CONFIG_HOME", "/Users/tester/.config")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok")
	t.Setenv("HTTPS_PROXY", "http://leak.example") // must NOT cross the boundary
	t.Setenv("GITHUB_TOKEN", "ghp-leak")           // must NOT cross the boundary

	env := claudeEnv()
	want := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/tester",
		"TERM=dumb",
		"LANG=en_US.UTF-8",
		"USER=tester",
		"LOGNAME=tester",
		"XDG_CONFIG_HOME=/Users/tester/.config",
		"ANTHROPIC_API_KEY=sk-test",
		"CLAUDE_CODE_OAUTH_TOKEN=tok",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("claudeEnv() = %q, want %q", env, want)
	}
}

func TestClaudeEnvOmitsUnsetVariables(t *testing.T) {
	t.Setenv("PATH", "/bin")
	t.Setenv("HOME", "/home/tester")
	for _, k := range []string{"LANG", "USER", "LOGNAME", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		os.Unsetenv(k)
	}
	env := claudeEnv()
	want := []string{"PATH=/bin", "HOME=/home/tester", "TERM=dumb"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("claudeEnv() = %q, want %q", env, want)
	}
}

// fakeClaudeDir writes an executable `claude` script into a fresh temp
// directory and makes it the ONLY directory on PATH, so the runner cannot
// reach the real binary. It fails the test if claude resolves anywhere
// else — the guard that keeps this suite from ever spending quota.
func fakeClaudeDir(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	lp, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("fake claude not found on PATH: %v", err)
	}
	if !strings.HasPrefix(lp, dir) {
		t.Fatalf("claude resolved to %q, outside the fake dir %q — refusing to run the real agent", lp, dir)
	}
	return dir
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

// grandchildScript: prints answer, spawns /bin/sleep 60 recording its pid,
// then exits 0. The grandchild inherits stdout, so it holds the pipes open
// after the parent is gone.
const grandchildScript = `#!/bin/bash
echo "the answer"
/bin/sleep 60 &
echo $! > %[1]s
exit 0
`

// hangScript: spawns a /bin/sleep 60 grandchild, then sleeps 60 itself.
const hangScript = `#!/bin/bash
echo started
/bin/sleep 60 &
echo $! > %[1]s
/bin/sleep 60
`

// B6: claude prints its failure on stdout and exits non-zero; the returned
// error must carry a bounded snippet of both streams so failures are
// diagnosable ("exit status 1" alone explained nothing).
func TestClaudeRunnerFakeFailureCarriesStdoutSnippet(t *testing.T) {
	fakeClaudeDir(t, "claude", `#!/bin/bash
echo "Failed to authenticate: OAuth session expired"
exit 1
`)
	start := time.Now()
	_, err := ClaudeRunner()(context.Background(), "draft me an entry")
	if err == nil {
		t.Fatal("expected failure from exit-1 fake")
	}
	for _, want := range []string{"exit status 1", "Failed to authenticate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, missing %q", err.Error(), want)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("failure took %v; the runner must not wait out the 120s timeout for a dead process", elapsed)
	}
}

// The prompt travels on stdin: the fake claude cats stdin back.
func TestClaudeRunnerFakeReadsPromptFromStdin(t *testing.T) {
	fakeClaudeDir(t, "claude", "#!/bin/bash\n/bin/cat\n")
	out, err := ClaudeRunner()(context.Background(), "PROMPT-ON-STDIN-MARKER")
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if !strings.Contains(out, "PROMPT-ON-STDIN-MARKER") {
		t.Errorf("stdout = %q, want the prompt echoed from stdin", out)
	}
}

// B4: a grandchild holding the stdout pipe cannot block the runner past
// WaitDelay, and is killed with the process group afterwards.
func TestClaudeRunnerGrandchildCannotBlockReturn(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	fakeClaudeDir(t, "claude", fmt.Sprintf(grandchildScript, pidfile))
	start := time.Now()
	out, err := ClaudeRunner()(context.Background(), "draft")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if !strings.Contains(out, "the answer") {
		t.Errorf("stdout = %q, want the parent's answer", out)
	}
	// The 120s default timeout is irrelevant here: the parent exited at
	// once and only the pipe-holding grandchild remained. WaitDelay plus
	// the group reap must bound that to seconds.
	if elapsed > 10*time.Second {
		t.Errorf("grandchild held the runner for %v; want ≤ a few seconds", elapsed)
	}
	pid := readPID(t, pidfile)
	waitProcessGone(t, pid, 3*time.Second)
}

// B4: a timeout kills the whole process tree, promptly — the 301s hang.
func TestClaudeRunnerTimeoutKillsProcessTree(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	fakeClaudeDir(t, "claude", fmt.Sprintf(hangScript, pidfile))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ClaudeRunner()(ctx, "draft")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %v, want deadline exceeded", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout returned after %v for a 200ms deadline", elapsed)
	}
	pid := readPID(t, pidfile)
	waitProcessGone(t, pid, 3*time.Second)
}

// B5: the output cap is enforced while reading.
func TestClaudeRunnerCapsOutput(t *testing.T) {
	fakeClaudeDir(t, "claude", "#!/bin/bash\n/usr/bin/yes x | /usr/bin/head -c 200000\n")
	out, err := ClaudeRunner()(context.Background(), "draft")
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if len(out) > maxAgentOutput+len("\n…[output truncated by ofa]") {
		t.Errorf("output length = %d, want ≤ %d + marker", len(out), maxAgentOutput)
	}
	if !strings.Contains(out, "…[output truncated by ofa]") {
		t.Error("truncated output lacks the truncation marker")
	}
}

// readPID reads the grandchild's pid from the script's pidfile.
func readPID(t *testing.T, pidfile string) int {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", b, err)
	}
	return pid
}
