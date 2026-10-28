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

// printfScript writes an executable /bin/sh script that prints its
// arguments one per line — the deterministic stand-in for system binaries
// whose flag behavior differs between BSD and GNU (`echo --version` prints
// its argument on macOS and a version banner on Linux, which is exactly
// what broke CI).
func printfScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "zzprintf")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
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
	// A printf fixture, not /bin/echo: the fixture's behavior cannot differ
	// between BSD and GNU userlands.
	s := printfScript(t)
	out, errOut, _, err := execBounded(context.Background(), s, []string{"hello"}, execSpec{timeout: 5 * time.Second, env: nil})
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

// R2-B2: execution is pinned. A draft-supplied path is never what runs —
// not a symlink swapped after the static gate, and not a path that was
// absent at the gate and created before the exec.
func TestNewBoundedRunnerPinsTheAuthorizedTarget(t *testing.T) {
	dir := t.TempDir()
	authorized := filepath.Join(dir, "zzpin")
	if err := os.WriteFile(authorized, []byte("#!/bin/bash\necho zzpin 1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The evil double: writes a marker, so any execution of it is visible.
	evilDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	evil := filepath.Join(evilDir, "zzpin")
	if err := os.WriteFile(evil, []byte(fmt.Sprintf("#!/bin/bash\ntouch %s\necho evil\n", marker)), 0o755); err != nil {
		t.Fatal(err)
	}
	run := NewBoundedRunner("zzpin", authorized)

	// A symlink that pointed at the authorized target when the static gate
	// checked it, then swapped to the evil double before detection execs.
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "zzpin")
	if err := os.Symlink(authorized, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	out, err := run(link, "--version")
	if err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	if !strings.Contains(out, "zzpin 1.0") {
		t.Errorf("output = %q, want the authorized target's output", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the swapped symlink was executed: marker file exists")
	}

	// A path that was absent at gate time and created before the exec.
	if _, err := run(evil, "--version"); err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the late-created path was executed: marker file exists")
	}

	// The bin name itself is pinned too.
	if out, err = run("zzpin", "--version"); err != nil || !strings.Contains(out, "zzpin 1.0") {
		t.Errorf("run(bin) = (%q, %v)", out, err)
	}
}

// Anything that is not the pinned target or a fixed registry command is
// refused without executing — fail closed.
func TestNewBoundedRunnerRefusesEverythingElse(t *testing.T) {
	run := NewBoundedRunner("zzpin", "/bin/true")
	// A different name with its own basename.
	if _, err := run("/bin/otherbin", "--version"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("foreign name: err = %v, want refusal", err)
	}
	// The right name with the wrong argv.
	if _, err := run("zzpin", "-c", "touch", "x"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("wrong argv: err = %v, want refusal", err)
	}
	// A registry tool with an argv detect never issues.
	if _, err := run("npm", "install", "-g", "evil"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("smuggled registry argv: err = %v, want refusal", err)
	}
}

// The pinned target runs with the minimal environment: no secrets cross.
func TestNewBoundedRunnerTargetModeMinimalEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-leak")
	zzenv := script(t, "env\n")
	// The pinned path's basename need not match the bin; the runner pins by
	// decision, not by name.
	run := NewBoundedRunner("zzenv", zzenv)
	out, err := run("zzenv", "--version")
	if err != nil {
		t.Fatalf("pinned run: %v", err)
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

// Registry commands inherit the caller's environment and run only with
// detect's fixed argv.
func TestNewBoundedRunnerRegistryModeInheritsEnv(t *testing.T) {
	t.Setenv("OFABOUND_TEST_VAR", "inherited")
	regDir := t.TempDir()
	fakeNpm := filepath.Join(regDir, "npm")
	if err := os.WriteFile(fakeNpm, []byte("#!/bin/bash\n/usr/bin/env\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", regDir)
	run := NewBoundedRunner("zzpin", "/bin/true")
	out, err := run("npm", "ls", "-g", "--depth=0", "--json")
	if err != nil {
		t.Fatalf("registry run: %v", err)
	}
	if !strings.Contains(out, "OFABOUND_TEST_VAR=inherited") {
		t.Errorf("registry-mode env did not inherit: %q", out)
	}
}

// BoundedRunnerFor passes injected runners through untouched and swaps only
// the unbounded production default, pinning the target once.
func TestBoundedRunnerFor(t *testing.T) {
	fake := func(name string, args ...string) (string, error) { return "fake", nil }
	if got := BoundedRunnerFor(fake, "sh"); !sameRunner(got, fake) {
		t.Error("injected runner was replaced")
	}
	// A fixture bin on PATH, not a system binary: the pinned target's flag
	// behavior must not depend on the platform (GNU echo --version prints a
	// version banner; BSD echo prints its argument — this exact difference
	// broke CI on the ubuntu runner).
	dir := t.TempDir()
	fixture := filepath.Join(dir, "zzpin")
	if err := os.WriteFile(fixture, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	bounded := BoundedRunnerFor(detect.DefaultRunner, "zzpin")
	if sameRunner(bounded, detect.DefaultRunner) {
		t.Fatal("detect.DefaultRunner was passed through unbounded")
	}
	// Any probe of the bin — by name or by a path with its basename —
	// executes the pinned canonical fixture, with argv[0] presented as the
	// bin name.
	out, err := bounded("zzpin", "--version")
	if err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	if out != "--version\n" {
		t.Errorf("pinned output = %q, want --version\\n", out)
	}
	if out, err = bounded(fixture, "--version"); err != nil || out != "--version\n" {
		t.Errorf("pinned run by path = (%q, %v), want (--version\\n, nil)", out, err)
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

// R3-B1: an unresolvable target — a dangling PATH alias — must make the
// factory refuse, never fall back to executing the alias.
func TestBoundedRunnerForRefusesUnresolvableTarget(t *testing.T) {
	dir := t.TempDir()
	dangling := filepath.Join(dir, "zzdangle")
	if err := os.Symlink(filepath.Join(dir, "zzgone"), dangling); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	run := BoundedRunnerFor(detect.DefaultRunner, "zzdangle")
	out, err := run("zzdangle", "--version")
	if err == nil {
		t.Fatalf("unresolvable target accepted: out = %q", out)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

func TestResolveCanonical(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A resolving chain collapses to the canonical file. On macOS
	// /var → /private/var, so the expected value is itself resolved.
	want, err := resolveCanonical(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCanonical(link)
	if err != nil || got != want {
		t.Errorf("resolveCanonical(link) = (%q, %v), want (%q, nil)", got, err, want)
	}
	// A dangling symlink is an error, not a silent fallback to the alias.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCanonical(dangling); err == nil {
		t.Error("resolveCanonical(dangling) succeeded, want resolution error")
	}
}

// R3-A1: registry names are matched literally — a path whose basename is a
// registry command is refused even with the correct argv.
func TestNewBoundedRunnerRefusesRegistryPathAliases(t *testing.T) {
	dir := t.TempDir()
	fakeNpm := filepath.Join(dir, "npm")
	if err := os.WriteFile(fakeNpm, []byte("#!/bin/bash\n/usr/bin/env\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	run := NewBoundedRunner("zzpin", "/bin/true")
	_, err := run(fakeNpm, "ls", "-g", "--depth=0", "--json")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("path-form registry name: err = %v, want refusal", err)
	}
	// The literal name with the same argv still runs.
	if _, err := run("npm", "ls", "-g", "--depth=0", "--json"); err != nil {
		t.Errorf("literal registry name: err = %v, want success", err)
	}
}

// R3-A2: overlapping credential values are redacted longest-first, so a
// secret that is a prefix of another cannot leave its suffix behind.
func TestRedactSecretsOverlappingValues(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-abc")
	t.Setenv("ANTHROPIC_API_KEY", "tok-abc-VERY-SECRET")
	got := redactSecrets("here: tok-abc-VERY-SECRET and tok-abc")
	if strings.Contains(got, "VERY-SECRET") || strings.Contains(got, "tok-abc") {
		t.Errorf("overlapping secrets survived: %q", got)
	}
	if got != "here: [redacted] and [redacted]" {
		t.Errorf("got = %q", got)
	}
}
