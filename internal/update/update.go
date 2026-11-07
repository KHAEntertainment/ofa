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
	// Silent suppresses every terminal write the executor would make on
	// its own (quiet-mode progress lines, verbose banners, the dry-run
	// print). The interactive TUI renders its own per-row progress, so
	// these bytes would only corrupt the alt screen. Installer output is
	// still captured for Result.OutputTail. The picker sets this on the
	// requests it runs; non-interactive callers leave it false and keep
	// the existing output exactly.
	Silent bool
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

// EventType identifies which lifecycle transition an Event reports.
type EventType int

const (
	// EventStarted fires just before a request's executor runs. Rows
	// that are skipped without executing only get EventFinished.
	EventStarted EventType = iota
	// EventFinished fires exactly once per request — including requests
	// skipped without executing (empty command, binary not on PATH,
	// canceled before start).
	EventFinished
)

// Event reports one request's lifecycle to an observer.
//
// Index is the request's position in the slice passed to ApplyObserved:
// a stable identity that stays unique when several installs share a
// label, so a UI can bind events to rows without guessing.
//
// Progress is deliberately absent: no built-in executor reports
// trustworthy installer progress, so a row driven by these events is
// always indeterminate rather than a fabricated percentage. An executor
// that ever reports real progress can extend this struct then; nothing
// here pretends to know more than the installer says.
type Event struct {
	Type   EventType
	Index  int
	Label  string
	Result *Result // set on EventFinished; points at the bucketed Result
}

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
//
// Apply is ApplyObserved with no observer and no stop signal: behavior
// and callers are unchanged.
func Apply(requests []Request, exec Executor) Summary {
	return ApplyObserved(requests, exec, nil, nil)
}

// ApplyObserved runs requests exactly as Apply does and additionally
// reports each request's lifecycle through observe (nil to observe
// nothing).
//
// stop, when non-nil, is a race-safe cancellation signal consumed between
// requests: once it is closed, no further request is started and each
// remaining one becomes a Skipped result with reason "canceled". A
// request already in flight always runs to completion — Apply never kills
// a running installer. Closing stop concurrently is safe; the channel is
// only ever closed by its owner.
func ApplyObserved(requests []Request, exec Executor, observe func(Event), stop <-chan struct{}) Summary {
	var s Summary
	for i, req := range requests {
		emit := func(ev Event) {
			if observe != nil {
				observe(ev)
			}
		}
		if stop != nil {
			select {
			case <-stop:
				r := Result{
					Label:         req.Label,
					Cmd:           req.Cmd,
					Skipped:       true,
					SkippedReason: "canceled",
				}
				s.Skipped = append(s.Skipped, r)
				emit(Event{Type: EventFinished, Index: i, Label: req.Label, Result: &s.Skipped[len(s.Skipped)-1]})
				continue
			default:
			}
		}
		// Empty Cmd is a malformed request — skip without calling exec.
		// Defends against a stray catalog entry or a bad CLI translation.
		if len(req.Cmd.Cmd) == 0 {
			s.Skipped = append(s.Skipped, Result{
				Label:         req.Label,
				Cmd:           req.Cmd,
				Skipped:       true,
				SkippedReason: "empty command",
			})
			emit(Event{Type: EventFinished, Index: i, Label: req.Label, Result: &s.Skipped[len(s.Skipped)-1]})
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
					emit(Event{Type: EventFinished, Index: i, Label: req.Label, Result: &s.Skipped[len(s.Skipped)-1]})
					continue
				}
			}
		}
		emit(Event{Type: EventStarted, Index: i, Label: req.Label})
		r := exec(req)
		switch {
		case r.Skipped:
			s.Skipped = append(s.Skipped, r)
		case r.Failed || r.Err != nil:
			s.Failed = append(s.Failed, r)
		default:
			s.Succeeded = append(s.Succeeded, r)
		}
		var bucket *Result
		switch {
		case r.Skipped:
			bucket = &s.Skipped[len(s.Skipped)-1]
		case r.Failed || r.Err != nil:
			bucket = &s.Failed[len(s.Failed)-1]
		default:
			bucket = &s.Succeeded[len(s.Succeeded)-1]
		}
		emit(Event{Type: EventFinished, Index: i, Label: req.Label, Result: bucket})
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
		if !req.Silent {
			fmt.Printf("  DRY-RUN  %s\n    %s\n", req.Label, req.Cmd.String())
		}
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
	if req.Silent {
		// The TUI renders this request's progress itself; any terminal
		// write here would corrupt the alt screen. Output is still
		// captured by DefaultExecutor's safeBuffer for failure tails.
		return func() {}, func(Result) {}, io.Discard
	}
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
