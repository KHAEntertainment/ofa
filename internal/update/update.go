// Package update runs the delegated update commands for a set of installs.
// Streams stdout/stderr to the terminal, supports --dry-run, continues on
// failure, and skips installs whose binary has been removed since scan.
//
// This package owns no knowledge of catalog, detect, or resolve. CLI layers
// (06/07) translate their types into update.Request and call Apply.
package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CmdSpec is a command and its arguments. Mirrors catalog.CmdSpec so the
// CLI can translate one to the other without update importing catalog.
type CmdSpec struct {
	Cmd []string
}

// first returns the program name (argv[0]) or "" when empty.
func (c CmdSpec) first() string {
	if len(c.Cmd) == 0 {
		return ""
	}
	return c.Cmd[0]
}

// String renders the command for human display (space-joined, no shell
// quoting — these are trusted catalog data, not user input).
func (c CmdSpec) String() string {
	return strings.Join(c.Cmd, " ")
}

// Request describes one update to apply. The CLI builds one Request per
// selected install, pulling Cmd from the catalog's install.Update field.
type Request struct {
	Cmd     CmdSpec
	Label   string // human-readable: "claude (native)"
	DryRun  bool   // print and return; do not execute
	Verbose bool   // stream installer output to stdout; suppress progress lines
}

// Result is the outcome of one Request. Exactly one of Skipped / Failed /
// (neither) is meaningful; Apply categorizes each Result into one Summary
// bucket.
type Result struct {
	Label         string
	Cmd           CmdSpec
	Skipped       bool
	SkippedReason string
	Failed        bool
	ExitCode      int
	Err           error  // non-nil if the executor couldn't even start
	OutputTail    string // last N lines, populated on failure
}

// Executor runs one command and returns its Result. DefaultExecutor shells
// out and streams output; tests inject stubs to avoid touching the system.
type Executor func(req Request) Result

// Summary aggregates Apply's per-request results.
type Summary struct {
	Succeeded []Result
	Failed    []Result
	Skipped   []Result
}

// HasFailures reports whether any updater failed.
func (s Summary) HasFailures() bool { return len(s.Failed) > 0 }

// ExitCode is 0 when nothing failed, 1 when at least one did. Used as the
// process exit code by the non-interactive CLI.
func (s Summary) ExitCode() int {
	if s.HasFailures() {
		return 1
	}
	return 0
}

// ExecLookPath is exec.LookPath behind a var so tests can stub PATH.
// Exported so cross-package tests can swap it.
var ExecLookPath = exec.LookPath

// Apply runs requests sequentially. A failing updater does not abort the
// run — the rest still execute. Skip-if-removed applies unless DryRun is
// set; an empty Cmd short-circuits to a skipped Result with a clear reason.
//
// Bucketing rules: Skipped goes to Skipped; Failed/Err goes to Failed; all
// else (including DryRun) goes to Succeeded. DryRun is a flag on the
// request, not a bucket; DefaultExecutor prints the command at execution
// time so the dry-run transcript reads in order.
func Apply(requests []Request, exec Executor) Summary {
	var s Summary
	for _, req := range requests {
		// Empty Cmd is a malformed request — skip without calling exec.
		// Defends against a stray catalog entry or a bad CLI translation.
		if len(req.Cmd.Cmd) == 0 {
			s.Skipped = append(s.Skipped, Result{
				Label:         req.Label,
				Cmd:           req.Cmd,
				Skipped:       true,
				SkippedReason: "empty command",
			})
			continue
		}
		if !req.DryRun {
			bin := req.Cmd.first()
			if bin != "" {
				if _, err := ExecLookPath(bin); err != nil {
					s.Skipped = append(s.Skipped, Result{
						Label:         req.Label,
						Cmd:           req.Cmd,
						Skipped:       true,
						SkippedReason: fmt.Sprintf("%s not on PATH", bin),
					})
					continue
				}
			}
		}
		r := exec(req)
		switch {
		case r.Skipped:
			s.Skipped = append(s.Skipped, r)
		case r.Failed || r.Err != nil:
			s.Failed = append(s.Failed, r)
		default:
			s.Succeeded = append(s.Succeeded, r)
		}
	}
	return s
}

// DefaultExecutor actually runs req.Cmd via os/exec, streaming stdout and
// stderr to the terminal and capturing for OutputTail on failure.
//
// Two modes:
//
//   - Verbose (req.Verbose=true): print bracket banners to stdout, stream
//     the installer's full output to stdout. Best when you want to debug
//     a particular install or watch a noisy `brew upgrade` flow.
//
//   - Quiet (default): print a single progress line to stderr that
//     updates in place — `⟳ label` while running, replaced by `✓ label`
//     (or `✗ label (exit N)` on failure). The installer's own output
//     is captured for the failure tail but not displayed live. Best
//     when you have a dozen updates queued and don't want the screen
//     drowning in each tool's idiosyncratic status lines.
//
// In both modes the installer's stdout/stderr are still captured to a
// safeBuffer so the failure summary can show a tail. stdout/stderr go
// separately to the terminal in verbose mode (preserving their
// distinction); in quiet mode both go to io.Discard.
func DefaultExecutor(req Request) Result {
	r := Result{Label: req.Label, Cmd: req.Cmd}

	if req.DryRun {
		fmt.Printf("  DRY-RUN  %s\n    %s\n", req.Label, req.Cmd.String())
		return r
	}

	if len(req.Cmd.Cmd) == 0 {
		r.Failed = true
		r.Err = errors.New("empty command")
		return r
	}

	start, finish, sink := announcePair(req)
	start()

	cmd := exec.Command(req.Cmd.Cmd[0], req.Cmd.Cmd[1:]...)
	var buf safeBuffer
	cmd.Stdout = buf.tee(sink)
	cmd.Stderr = buf.tee(sink)

	err := cmd.Run()
	r = buildResult(req, err, &buf)
	finish(r)
	return r
}

// announcePair returns (start hook, finish hook, sink) for the requested
// mode. In verbose mode the start hook prints the bracket banner; in
// quiet mode it writes `⟳ label` (no newline) to stderr so the line can
// be overwritten by the finish hook with the same `\033[2K\r` clear
// regardless of label width.
func announcePair(req Request) (func(), func(Result), io.Writer) {
	if req.Verbose {
		start := func() {
			fmt.Printf("\n── %s ──\n$ %s\n", req.Label, req.Cmd.String())
		}
		finish := func(r Result) {
			fmt.Printf("── %s ── exit %d ──\n", r.Label, r.ExitCode)
		}
		return start, finish, os.Stdout
	}
	start := func() {
		fmt.Fprintf(os.Stderr, "\033[2K\r⟳ %s", req.Label)
	}
	finish := func(r Result) {
		marker := "✓"
		if r.Failed {
			marker = "✗"
		}
		fmt.Fprintf(os.Stderr, "\033[2K\r%s %s (exit %d)\n", marker, r.Label, r.ExitCode)
	}
	return start, finish, io.Discard
}

// buildResult converts an exec.Cmd outcome into a Result. Pulled out of
// DefaultExecutor so the verbose and quiet paths share the same
// exit-code / tail logic. buf is a pointer because safeBuffer embeds a
// sync.Mutex, which Go forbids copying.
func buildResult(req Request, err error, buf *safeBuffer) Result {
	r := Result{Label: req.Label, Cmd: req.Cmd}
	if err == nil {
		r.ExitCode = 0
		return r
	}
	r.Failed = true
	r.Err = err
	if ee, ok := err.(*exec.ExitError); ok {
		r.ExitCode = ee.ExitCode()
	} else {
		r.ExitCode = -1
	}
	r.OutputTail = tailLines(buf.String(), 20)
	return r
}

// safeBuffer accumulates teed output for tail capture. Writes are serialized
// so concurrent stdout+stderr don't tear at byte boundaries — and the
// serialization covers the live mirror too (see teeWriter), so a sink
// shared by both streams never sees concurrent writes either (issue #10).
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// tee returns a writer that mirrors to w live and accumulates into b.
//
// All writers teed from the same safeBuffer serialize on b's mutex —
// capture and mirror alike — which is what makes a shared sink safe:
// exec.Cmd copies stdout and stderr in separate goroutines, and
// DefaultExecutor points both at one sink. The sink must not write back
// into the same safeBuffer (it can't: announcePair builds it before the
// buffer exists), or the shared mutex would deadlock.
func (b *safeBuffer) tee(w io.Writer) io.Writer {
	return &teeWriter{b: b, w: w}
}

type teeWriter struct {
	b *safeBuffer
	w io.Writer
}

// Write holds b's mutex across BOTH the capture and the mirror. The
// original unlocked before t.w.Write, leaving the shared sink (issue
// #10's data race) — and the test's strings.Builder sink — unprotected.
func (t *teeWriter) Write(p []byte) (int, error) {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	t.b.buf.Write(p)
	return t.w.Write(p)
}

// tailLines returns the last n lines of s, joined by '\n'. Used in the
// failure summary so the user sees the *end* of noisy brew/npm output,
// not the (usually uninteresting) beginning.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
