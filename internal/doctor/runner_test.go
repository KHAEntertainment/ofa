package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ofa/internal/detect"
)

// These tests run real subprocesses — /bin/echo, /bin/sleep, bash scripts —
// because the bounds they verify (deadlines, pipe waits, process trees,
// output caps) only exist against real processes. None of them spawns an
// agent or the test suite.

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

func TestMinimalCommandEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/Users/tester")
	t.Setenv("USER", "tester")
	t.Setenv("LOGNAME", "tester")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("GITHUB_TOKEN", "ghp-leak")
	env := minimalCommandEnv()
	want := []string{"PATH=/usr/bin:/bin", "HOME=/Users/tester", "TERM=dumb", "LANG=en_US.UTF-8", "USER=tester", "LOGNAME=tester"}
	if len(env) != len(want) {
		t.Fatalf("minimalCommandEnv() = %q, want %q", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
		}
	}
}

func TestExecBoundedBasic(t *testing.T) {
	out, errOut, _, err := execBounded(context.Background(), "/bin/echo", []string{"hello"}, execSpec{timeout: 5 * time.Second, env: nil})
	if err != nil {
		t.Fatalf("execBounded: %v (stderr %q)", err, errOut)
	}
	if out != "hello\n" {
		t.Errorf("stdout = %q, want hello\\n", out)
	}
}

func TestExecBoundedDeadline(t *testing.T) {
	slow := script(t, "/bin/sleep 60\n")
	start := time.Now()
	_, _, _, err := execBounded(context.Background(), slow, nil, execSpec{timeout: 200 * time.Millisecond, env: nil})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("deadline returned after %v for a 200ms budget", elapsed)
	}
}

func TestExecBoundedGrandchildReaped(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	s := script(t, fmt.Sprintf("/bin/sleep 60 &\necho $! > %s\necho done\nexit 0\n", pidfile))
	out, _, _, err := execBounded(context.Background(), s, nil, execSpec{timeout: 10 * time.Second, env: nil})
	if err != nil {
		t.Fatalf("execBounded: %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("stdout = %q, want the parent's output", out)
	}
	// The parent exited; the pipe-holding grandchild must be gone shortly.
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid := 0
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d still alive after the parent exited", pid)
}

func TestExecBoundedStderrCappedWhileReading(t *testing.T) {
	s := script(t, "/usr/bin/yes y | /usr/bin/head -c 200000 >&2\n")
	_, errOut, _, err := execBounded(context.Background(), s, nil, execSpec{timeout: 10 * time.Second, env: nil, maxStderr: 64})
	if err != nil {
		t.Fatalf("execBounded: %v", err)
	}
	if got := len(errOut); got > 64 {
		t.Errorf("stderr = %d bytes, want <= 64", got)
	}
}

func TestExecBoundedNonZeroExitCarriesStderr(t *testing.T) {
	s := script(t, "echo boom >&2\nexit 3\n")
	out, errOut, _, err := execBounded(context.Background(), s, nil, execSpec{timeout: 5 * time.Second, env: nil})
	if err == nil {
		t.Fatal("expected exit error")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(errOut, "boom") || !strings.Contains(out, "") {
		t.Errorf("streams = %q / %q", out, errOut)
	}
}

// The target binary runs with the minimal environment: no secrets cross.
func TestNewBoundedRunnerTargetModeMinimalEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-leak")
	run := NewBoundedRunner("/usr/bin/env")
	out, err := run("env")
	if err != nil {
		t.Fatalf("bounded runner: %v", err)
	}
	if !strings.Contains(out, "TERM=dumb") {
		t.Errorf("target-mode env missing TERM=dumb: %q", out)
	}
	for _, leak := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "ghp-leak"} {
		if strings.Contains(out, leak) {
			t.Errorf("target-mode env leaked %s", leak)
		}
	}
}

// Registry commands inherit the caller's environment and still run bounded.
func TestNewBoundedRunnerRegistryModeInheritsEnv(t *testing.T) {
	t.Setenv("OFABOUND_TEST_VAR", "inherited")
	run := NewBoundedRunner("/bin/zztarget")
	// brew is a registry command name; use it to run env via... no — the
	// runner executes the named command directly. Use a registry name only
	// detect uses; here assert the routing decision indirectly through a
	// command whose base IS a registry name.
	dir := t.TempDir()
	p := filepath.Join(dir, "npm")
	if err := os.WriteFile(p, []byte("#!/bin/bash\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run = NewBoundedRunner("/bin/zztarget")
	out, err := run(p)
	if err != nil {
		t.Fatalf("bounded runner: %v", err)
	}
	if !strings.Contains(out, "OFABOUND_TEST_VAR=inherited") {
		t.Errorf("registry-mode env did not inherit: %q", out)
	}
}

// BoundedRunnerFor passes injected runners through untouched and swaps only
// the unbounded production default.
func TestBoundedRunnerFor(t *testing.T) {
	fake := func(name string, args ...string) (string, error) { return "fake", nil }
	if got := BoundedRunnerFor(fake, "/bin/sh"); sameRunner(got, fake) == false {
		t.Error("injected runner was replaced")
	}
	bounded := BoundedRunnerFor(detect.DefaultRunner, "/bin/echo")
	if sameRunner(bounded, detect.DefaultRunner) {
		t.Fatal("detect.DefaultRunner was passed through unbounded")
	}
	out, err := bounded("/bin/echo", "swapped")
	if err != nil || out != "swapped\n" {
		t.Errorf("bounded runner = (%q, %v), want (swapped\\n, nil)", out, err)
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

func TestTruncateBytesRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100) // 200 bytes
	got := truncateBytes(s, 101)
	if len(got) > 101 {
		t.Errorf("len = %d, want <= 101", len(got))
	}
	// 101 bytes would cut mid-rune; the cut backs off to a boundary.
	for _, r := range got {
		if r == 0xFFFD {
			t.Error("cut produced an invalid rune")
		}
	}
	if truncateBytes("short", 100) != "short" {
		t.Error("string under the cap was modified")
	}
	if truncateBytes("anything", 0) != "" {
		t.Error("zero cap should yield empty")
	}
}

func TestStreamSnippetBoundedAndQuoted(t *testing.T) {
	long := strings.Repeat("a", 5000)
	got := streamSnippet(long, long)
	if len(got) > 1200 { // 512+512 quoted, plus overhead
		t.Errorf("snippet length = %d, want bounded", len(got))
	}
	if !strings.Contains(got, `"aaa`) || !strings.Contains(got, "stdout:") || !strings.Contains(got, "stderr:") {
		t.Errorf("snippet = %q, want both streams quoted", got)
	}
	if streamSnippet("", "") != "" {
		t.Error("empty streams should yield no snippet")
	}
}
