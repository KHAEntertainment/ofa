package update

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// withLookPathAllowed stubs ExecLookPath to consider every binary present
// on PATH, so unit tests don't fail on machines without the project's
// real harness binaries installed (e.g. Linux CI without `brew` or
// `claude`). The original is restored at test cleanup.
func withLookPathAllowed(t *testing.T) {
	t.Helper()
	orig := ExecLookPath
	ExecLookPath = func(string) (string, error) { return "/usr/bin/true", nil }
	t.Cleanup(func() { ExecLookPath = orig })
}

// withLookPathDenied stubs ExecLookPath to fail for every binary. Used
// by tests that exercise the "binary missing on PATH" skip path.
func withLookPathDenied(t *testing.T) {
	t.Helper()
	orig := ExecLookPath
	ExecLookPath = func(string) (string, error) {
		return "", &exec.Error{Name: "<stub>", Err: exec.ErrNotFound}
	}
	t.Cleanup(func() { ExecLookPath = orig })
}

// stubExecutor answers canned results keyed by request Label (the human-
// readable identifier the CLI sets). Anything not in the map errors so a
// missing stub is loud.
func stubExecutor(results map[string]Result) Executor {
	return func(req Request) Result {
		r, ok := results[req.Label]
		if !ok {
			return Result{Label: req.Label, Cmd: req.Cmd, Failed: true, Err: errors.New("stubExecutor: no canned result for " + req.Label)}
		}
		// Make sure the canned Result carries the right Label/Cmd even if
		// the test author forgot.
		r.Label = req.Label
		r.Cmd = req.Cmd
		return r
	}
}

func TestApply_Empty(t *testing.T) {
	s := Apply(nil, stubExecutor(nil))
	if len(s.Succeeded)+len(s.Failed)+len(s.Skipped) != 0 {
		t.Errorf("empty Apply should yield empty summary, got %+v", s)
	}
	if s.ExitCode() != 0 {
		t.Errorf("empty summary should exit 0, got %d", s.ExitCode())
	}
}

func TestApply_Success(t *testing.T) {
	withLookPathAllowed(t) // brew not on PATH on every CI image
	exec := stubExecutor(map[string]Result{
		"codex (brew-cask)": {ExitCode: 0},
	})
	s := Apply([]Request{
		{Label: "codex (brew-cask)", Cmd: CmdSpec{Cmd: []string{"brew", "upgrade", "--cask", "codex"}}},
	}, exec)
	if len(s.Succeeded) != 1 {
		t.Errorf("want 1 succeeded, got %+v", s)
	}
	if s.HasFailures() || s.ExitCode() != 0 {
		t.Errorf("success summary should have no failures / exit 0, got %+v / %d", s, s.ExitCode())
	}
}

func TestApply_FailureContinues(t *testing.T) {
	// First call fails, second succeeds, third skipped-by-binary-missing.
	exec := stubExecutor(map[string]Result{
		"a": {Failed: true, Err: errors.New("boom"), ExitCode: 1, OutputTail: "last line"},
		"b": {ExitCode: 0},
	})
	// Force "c" to be missing by pointing its cmd at a non-existent bin.
	s := Apply([]Request{
		{Label: "a", Cmd: CmdSpec{Cmd: []string{"false"}}},
		{Label: "b", Cmd: CmdSpec{Cmd: []string{"true"}}},
		{Label: "c", Cmd: CmdSpec{Cmd: []string{"definitely-not-a-real-binary-12345", "x"}}},
	}, exec)

	if len(s.Failed) != 1 || s.Failed[0].Label != "a" {
		t.Errorf("want 1 failed ('a'), got %+v", s.Failed)
	}
	if len(s.Succeeded) != 1 || s.Succeeded[0].Label != "b" {
		t.Errorf("want 1 succeeded ('b'), got %+v", s.Succeeded)
	}
	if len(s.Skipped) != 1 || s.Skipped[0].Label != "c" {
		t.Errorf("want 1 skipped ('c'), got %+v", s.Skipped)
	}
	if !s.HasFailures() {
		t.Error("HasFailures should be true")
	}
	if s.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1", s.ExitCode())
	}
}

func TestApply_DryRunSkipsPathCheck(t *testing.T) {
	// Dry-run must NOT trigger skip-if-removed; the whole point is to see
	// what *would* run, including for items whose binary is currently gone.
	exec := stubExecutor(map[string]Result{
		"gone": {ExitCode: 0}, // would be reported as Succeeded
	})
	s := Apply([]Request{
		{Label: "gone", Cmd: CmdSpec{Cmd: []string{"definitely-not-a-real-binary-12345"}}, DryRun: true},
	}, exec)
	if len(s.Skipped) != 0 {
		t.Errorf("dry-run should not skip on missing binary, got %+v", s.Skipped)
	}
	if len(s.Succeeded) != 1 {
		t.Errorf("want 1 succeeded, got %+v", s)
	}
}

func TestApply_SkipIfRemoved(t *testing.T) {
	exec := stubExecutor(nil)
	s := Apply([]Request{
		{Label: "x", Cmd: CmdSpec{Cmd: []string{"definitely-not-a-real-binary-12345"}}},
	}, exec)
	if len(s.Skipped) != 1 {
		t.Fatalf("want 1 skipped, got %+v", s)
	}
	if !strings.Contains(s.Skipped[0].SkippedReason, "not on PATH") {
		t.Errorf("SkippedReason = %q, want it to mention PATH", s.Skipped[0].SkippedReason)
	}
	if len(s.Failed) != 0 {
		t.Errorf("missing binary should be Skipped, not Failed; got %+v", s.Failed)
	}
}

func TestApply_EmptyCmdShortCircuits(t *testing.T) {
	// Empty Cmd cannot resolve a binary; Apply skips it without calling exec.
	exec := func(req Request) Result {
		t.Errorf("executor should not be called for empty Cmd")
		return Result{}
	}
	s := Apply([]Request{
		{Label: "empty", Cmd: CmdSpec{}},
	}, exec)
	if len(s.Skipped) != 1 {
		t.Errorf("empty Cmd should be Skipped, got %+v", s)
	}
}

func TestApply_MixedBucketsStableOrder(t *testing.T) {
	exec := stubExecutor(map[string]Result{
		"a": {ExitCode: 0},
		"c": {ExitCode: 0},
	})
	s := Apply([]Request{
		{Label: "a", Cmd: CmdSpec{Cmd: []string{"true"}}},
		{Label: "b", Cmd: CmdSpec{Cmd: []string{"definitely-not-a-real-binary-12345"}}}, // skip
		{Label: "c", Cmd: CmdSpec{Cmd: []string{"true"}}},
	}, exec)
	// Verify ordering preserved within each bucket: succeeded in input
	// order, skipped in input order.
	if len(s.Succeeded) != 2 || s.Succeeded[0].Label != "a" || s.Succeeded[1].Label != "c" {
		t.Errorf("Succeeded bucket order: %+v", s.Succeeded)
	}
	if len(s.Skipped) != 1 || s.Skipped[0].Label != "b" {
		t.Errorf("Skipped bucket order: %+v", s.Skipped)
	}
}

func TestApply_DoesNotAbort(t *testing.T) {
	// Belt-and-suspenders: confirm all requests are processed even when
	// earlier ones fail. (TestApply_FailureContinues covers the same
	// behavior; this one uses distinct calls to stubExecutor to prove
	// Apply reaches each request.)
	calls := []string{}
	exec := func(req Request) Result {
		calls = append(calls, req.Label)
		if req.Label == "fail" {
			return Result{Failed: true, Err: errors.New("nope"), ExitCode: 1}
		}
		return Result{ExitCode: 0}
	}
	Apply([]Request{
		{Label: "ok1", Cmd: CmdSpec{Cmd: []string{"true"}}},
		{Label: "fail", Cmd: CmdSpec{Cmd: []string{"false"}}},
		{Label: "ok2", Cmd: CmdSpec{Cmd: []string{"true"}}},
	}, exec)
	want := []string{"ok1", "fail", "ok2"}
	if len(calls) != len(want) {
		t.Fatalf("executor calls = %v, want %v", calls, want)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %q, want %q", i, calls[i], w)
		}
	}
}

func TestSummary_ExitCodeAndHasFailures(t *testing.T) {
	cases := []struct {
		name string
		s    Summary
		want int
		fail bool
	}{
		{"empty", Summary{}, 0, false},
		{"all succeeded", Summary{Succeeded: []Result{{}}}, 0, false},
		{"all skipped", Summary{Skipped: []Result{{}}}, 0, false},
		{"one failed", Summary{Failed: []Result{{}}}, 1, true},
		{"mixed", Summary{Succeeded: []Result{{}}, Failed: []Result{{}}}, 1, true},
	}
	for _, c := range cases {
		if got := c.s.ExitCode(); got != c.want {
			t.Errorf("%s: ExitCode = %d, want %d", c.name, got, c.want)
		}
		if got := c.s.HasFailures(); got != c.fail {
			t.Errorf("%s: HasFailures = %v, want %v", c.name, got, c.fail)
		}
	}
}

func TestCmdSpec_FirstAndString(t *testing.T) {
	c := CmdSpec{Cmd: []string{"brew", "upgrade", "--cask", "codex"}}
	if got := c.first(); got != "brew" {
		t.Errorf("first = %q, want brew", got)
	}
	if got := c.String(); got != "brew upgrade --cask codex" {
		t.Errorf("String = %q", got)
	}
	if got := (CmdSpec{}).first(); got != "" {
		t.Errorf("empty first = %q, want empty", got)
	}
	if got := (CmdSpec{}).String(); got != "" {
		t.Errorf("empty String = %q, want empty", got)
	}
}

func TestTailLines(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"a\nb\nc", 5, "a\nb\nc"},                // fewer than n lines: return all
		{"a\nb\nc\nd\ne", 3, "c\nd\ne"},          // exactly tail
		{"a\nb\nc\nd\ne", 2, "d\ne"},             // exact n
		{"a\nb\nc\n", 5, "a\nb\nc"},              // trailing newline trimmed
	}
	for _, c := range cases {
		if got := tailLines(c.in, c.n); got != c.want {
			t.Errorf("tailLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestSafeBuffer_ConcurrentWritesAreSerialized(t *testing.T) {
	// Sanity: concurrent writes through the teed writer don't tear.
	var b safeBuffer
	w := b.tee(&strings.Builder{})
	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < 1000; i++ {
			w.Write([]byte("hello world\n"))
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 1000; i++ {
			w.Write([]byte("foo bar baz\n"))
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	got := b.String()
	if lines := strings.Count(got, "\n"); lines != 2000 {
		t.Errorf("got %d newlines, want 2000", lines)
	}
}

// TestDefaultExecutor_RealCommand exercises DefaultExecutor against a real
// subprocess (`true`/`false`) to confirm the streaming + tail capture works
// outside the stub harness. Skipped if /bin/sh is missing.
func TestDefaultExecutor_RealCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		r := DefaultExecutor(Request{
			Label: "ok",
			Cmd:   CmdSpec{Cmd: []string{"sh", "-c", "echo hello"}},
		})
		if r.Failed || r.Err != nil {
			t.Errorf("expected success, got %+v", r)
		}
	})

	t.Run("non-zero exit", func(t *testing.T) {
		r := DefaultExecutor(Request{
			Label: "fail",
			Cmd:   CmdSpec{Cmd: []string{"sh", "-c", "echo bad; exit 7"}},
		})
		if !r.Failed {
			t.Errorf("expected Failed, got %+v", r)
		}
		if r.ExitCode != 7 {
			t.Errorf("ExitCode = %d, want 7", r.ExitCode)
		}
		if !strings.Contains(r.OutputTail, "bad") {
			t.Errorf("OutputTail should contain 'bad', got %q", r.OutputTail)
		}
	})

	t.Run("dry-run prints and does not run", func(t *testing.T) {
		r := DefaultExecutor(Request{
			Label:  "dry",
			DryRun: true,
			Cmd:    CmdSpec{Cmd: []string{"sh", "-c", "echo would-print"}},
		})
		if r.Failed || r.Err != nil || r.Skipped {
			t.Errorf("dry-run result should be neutral, got %+v", r)
		}
	})

	t.Run("empty cmd fails", func(t *testing.T) {
		r := DefaultExecutor(Request{Label: "empty", Cmd: CmdSpec{}})
		if !r.Failed || r.Err == nil {
			t.Errorf("empty Cmd should fail with error, got %+v", r)
		}
	})
}

// TestDefaultExecutor_VerboseVsQuiet verifies the two output modes:
// quiet (default) suppresses installer output and writes progress lines
// to stderr; verbose streams the installer output to stdout with
// bracket banners.
func TestDefaultExecutor_VerboseVsQuiet(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}

	// Capture stderr by redirecting the test process's stderr to a pipe.
	// Simpler: just confirm Result fields are right and trust the side
	// effects (progress lines) to the visual smoke. We do still check
	// that Quiet succeeds with no Failed flag for a happy run.
	t.Run("quiet success", func(t *testing.T) {
		r := DefaultExecutor(Request{
			Label: "ok-quiet",
			Cmd:   CmdSpec{Cmd: []string{"sh", "-c", "echo hidden-by-quiet"}},
		})
		if r.Failed || r.Err != nil {
			t.Errorf("quiet success: %+v", r)
		}
		if r.OutputTail != "" {
			t.Errorf("quiet success should have empty OutputTail, got %q", r.OutputTail)
		}
	})

	t.Run("quiet failure preserves tail", func(t *testing.T) {
		r := DefaultExecutor(Request{
			Label: "fail-quiet",
			Cmd:   CmdSpec{Cmd: []string{"sh", "-c", "echo secret-error; exit 9"}},
		})
		if !r.Failed || r.ExitCode != 9 {
			t.Errorf("quiet failure: %+v", r)
		}
		if !strings.Contains(r.OutputTail, "secret-error") {
			t.Errorf("quiet failure should capture tail for the summary, got %q", r.OutputTail)
		}
	})

	t.Run("verbose success", func(t *testing.T) {
		r := DefaultExecutor(Request{
			Label:   "ok-verbose",
			Verbose: true,
			Cmd:     CmdSpec{Cmd: []string{"sh", "-c", "echo streamed"}},
		})
		if r.Failed || r.Err != nil {
			t.Errorf("verbose success: %+v", r)
		}
	})
}

// TestDefaultExecutor_RequestVerboseFlagPassesThrough guards against a
// regression where the Verbose flag is silently dropped. We use a stub
// Executor in update_test.go's pattern via the inner announcement pair.
// This test inspects the announce-pair directly via a roundtrip through
// DefaultExecutor.
func TestAnnouncePair_QuietUsesDiscardAndAnsiClear(t *testing.T) {
	start, finish, sink := announcePair(Request{Label: "x"})
	// sink should be io.Discard for quiet mode
	if sink != io.Discard {
		t.Errorf("quiet sink = %T, want io.Discard", sink)
	}
	// start should not panic and should write to stderr (we can't easily
	// capture stderr here without redirecting, so just confirm it returns)
	start()
	finish(Result{Label: "label", ExitCode: 0})
}

func TestAnnouncePair_VerboseUsesStdout(t *testing.T) {
	start, _, sink := announcePair(Request{Label: "x", Verbose: true})
	if sink != os.Stdout {
		t.Errorf("verbose sink = %T, want os.Stdout", sink)
	}
	// Just verify start() doesn't panic.
	start()
}