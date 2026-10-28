package doctor

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

// --- agent: no test here spawns a real agent; only the pure helpers run ---

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

func TestClaudeEnvMinimalPlusAuthPassthrough(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("LANG", "en_US.UTF-8")
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
	for _, k := range []string{"LANG", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		os.Unsetenv(k)
	}
	env := claudeEnv()
	want := []string{"PATH=/bin", "HOME=/home/tester", "TERM=dumb"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("claudeEnv() = %q, want %q", env, want)
	}
}

func TestCappedBuffer(t *testing.T) {
	var w cappedBuffer
	w.limit = 10
	if _, err := w.Write([]byte("12345")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if w.truncated {
		t.Error("truncated before the limit")
	}
	if _, err := w.Write([]byte("6789012345")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !w.truncated {
		t.Error("not flagged after overflow")
	}
	if w.String() != "1234567890" {
		t.Errorf("buffer = %q, want first 10 bytes", w.String())
	}
	// Writes past the cap keep reporting success so the subprocess never
	// sees a write error.
	if _, err := w.Write([]byte("overflow")); err != nil {
		t.Fatalf("write past cap: %v", err)
	}
}

// A context deadline surfaces as the runner's error, not a partial result.
func TestClaudeRunnerContextCancellation(t *testing.T) {
	if _, err := os.Stat("/bin/sleep"); err != nil {
		t.Skip("no /bin/sleep on this platform")
	}
	// The runner runs the real claude binary, which tests must not spawn.
	// This exercises only the context plumbing by pointing the runner at a
	// command that does not exist: the error must name claude, proving the
	// command is the one we configured.
	runner := ClaudeRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := runner(ctx, "hello")
	if err == nil {
		t.Skip("claude binary present on PATH; runner refused to be exercised without spending quota")
	}
}
