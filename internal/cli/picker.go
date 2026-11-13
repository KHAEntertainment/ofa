package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"ofa/internal/banner"
	"ofa/internal/catalog"
	"ofa/internal/picker"
	"ofa/internal/resolve"
)

// isTerminal reports whether f is a character device — a necessary but
// not sufficient condition for being a terminal. Used only where a rough
// pre-check is wanted (doctor's open prompt); gates that must not fire on
// /dev/null or /dev/zero use stderrIsTTY / stdinIsTTY, which ask the
// device itself via termios.
//
// Cheap stat-based check; avoids the extra dep that golang.org/x/term
// would bring.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// stderrIsTTY is the default StderrIsTTY gate for the scanning screen's
// banner on the non-interactive fallback path, and half of the gate that
// opens the interactive TUI (issue #62).
//
// It asks the device itself, through stdinIsTTY, instead of a cheap
// stat-only character-device test. /dev/null and /dev/zero are character
// devices too, so a mode check alone would let them through — and a
// banner or a spinner written to one is seen by nobody: it only pollutes
// a device that discards bytes. stdinIsTTY asks for a termios ioctl, so a
// pipe, a regular file and every non-terminal character device answer no,
// and only a real terminal answers yes.
//
// stdinIsTTY is doctor's stdin gate; the question it asks — is this
// device a real terminal? — is the same one for stderr, so it is reused
// verbatim rather than duplicated. The check never reads, so it cannot
// block, and it can only ever turn an interactive stderr into a
// non-interactive one — never the other way round.
func stderrIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && stdinIsTTY(f)
}

// scanTagline is the short brand line shown under the OFA banner on the
// scanning screen. The picker draws its own, longer blurb (internal/picker);
// this is the short form of the same sentence, kept local so the cli
// package owns no part of the picker's layout.
const scanTagline = "One-For-All. Harness Updater."

// drawScanBanner writes the OFA banner and the short tagline to stderr,
// so the brand is on screen while the harness scan runs (issue #43).
//
// Nothing is written when stderr is not a terminal: piped, redirected and
// CI runs stay byte-identical.
//
// The banner is written once, above the plain scan output. Issue #62
// moved the interactive flow into one alt-screen TUI, where the banner
// and the scanning screen live inside the alt screen; this normal-buffer
// banner belongs to the non-interactive fallback only, so the two copies
// (normal buffer + alt screen) and the blank frame between phases can
// never coexist.
func (a *App) drawScanBanner() {
	if !a.StderrIsTTY(a.Stderr) {
		return
	}
	// Render bakes the brand colors into the art and strips them when
	// NO_COLOR is set, so the no-color.org convention needs no work here.
	_ = banner.Render(a.Stderr)
	// Render ends on the art's last row without a newline: close that
	// row, leave the blank line that separates the art from the tagline,
	// then the tagline itself.
	fmt.Fprint(a.Stderr, "\n\n")
	fmt.Fprintln(a.Stderr, centerUnderBanner(scanTagline))
}

// centerUnderBanner pads s with plain spaces so it sits centered under the
// banner's frame, biased to the left like the picker's own banner block.
// Content at or beyond the frame width is returned unpadded.
func centerUnderBanner(s string) string {
	extra := banner.Cols - utf8.RuneCountInString(s)
	if extra <= 0 {
		return s
	}
	left := strings.Repeat(" ", (extra+1)/2)
	return left + s + strings.Repeat(" ", extra-len(left))
}

// pickerCmd is the interactive default: `ofa` (no subcommand). On a
// machine with no state.json it auto-scans first (per cli-surface spec)
// so the very first run still works without a separate `ofa scan` step.
//
// The flow (issue #62 — one TUI for everything):
//
//  1. When stdin AND stderr are real terminals, run the whole flow in
//     one alt-screen TUI (picker.pickerFlow): scan (banner visible,
//     first run included), picker, per-update progress rows, and a
//     summary that stays until dismissed. Nothing is printed to the
//     normal terminal afterwards — the process exit code is the only
//     output.
//  2. Otherwise fall back to plain output, byte-identical to the
//     pre-#62 behavior: banner (when stderr is a terminal), the
//     first-run scan and its notice, then a clear "use a subcommand
//     instead" message.
//
// The picker is the only piece of OFA that requires a TTY. When stdin
// isn't a TTY, the fallback surfaces the same message it always has.
func (a *App) pickerCmd(args []string) ExitCode {
	// The bare picker accepts only --refresh (issue #8); anything else is
	// a usage error, matching the subcommand FlagSets.
	fs := flag.NewFlagSet("ofa", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	refresh := fs.Bool("refresh", false, "bypass cached latest versions; re-query registries")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	a.Refresh = *refresh

	// The TUI needs a real terminal on both streams (issue #62): a
	// non-TTY stderr with a TTY stdin must not open the alt screen, or
	// its escape bytes would land in whatever that stream points at.
	// Both gates are real terminal checks (termios), not stat tests.
	if a.StdinIsTTY(a.Stdin) && a.StderrIsTTY(a.Stderr) {
		return a.pickerFlow()
	}

	// Non-interactive fallback — plain output, byte-identical to the
	// pre-#62 behavior. The banner brand the first-run scan (#43); the
	// scan itself writes its usual plain report. Then the no-TTY notice.
	a.drawScanBanner()
	if _, err := os.Stat(a.StatePath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(a.Stderr, "first run: scanning...")
		if code := a.scanCmd([]string{}); code != ExitOK {
			return code
		}
	}
	fmt.Fprintln(a.Stderr, "ofa: no TTY detected; use `ofa scan`, `ofa check`, or `ofa apply` instead")
	return ExitError
}

// emitSink captures everything the CLI pipeline writes during the
// interactive flow and forwards it to the TUI's diag writer once bound,
// so diagnostics are rendered inside the active TUI — never printed to
// the normal buffer, never dropped (issue #62). Writes made before the
// bind are buffered and flushed on bind. App.Stdout and App.Stderr are
// reassigned only around RunFlow — never while the TUI is rendering —
// and restored after RunFlow has joined its workers.
type emitSink struct {
	mu  sync.Mutex
	w   io.Writer // bound by the Scan callback before any pipeline stage runs
	buf bytes.Buffer
}

func (s *emitSink) bind(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > 0 {
		_, _ = w.Write(s.buf.Bytes())
		s.buf.Reset()
	}
	s.w = w
}

func (s *emitSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return s.buf.Write(p)
	}
	return s.w.Write(p)
}

// pickerFlow runs the whole interactive flow inside one alt-screen TUI
// (issue #62): scan and detection (banner visible, #43), the picker, the
// per-update progress rows, and a summary that stays until dismissed.
// The CLI keeps its pipeline and update logic; the flow calls back into
// it through Scan and Build and renders only what they report. After the
// flow returns, nothing is printed to the normal terminal — the returned
// exit code is the run's only output.
func (a *App) pickerFlow() ExitCode {
	sink := &emitSink{}
	prevOut, prevErr := a.Stdout, a.Stderr
	a.Stdout, a.Stderr = sink, sink
	defer func() { a.Stdout, a.Stderr = prevOut, prevErr }()

	var cat []catalog.Harness
	res, err := picker.RunFlow(picker.FlowConfig{
		Pins:  a.Pins,
		Input: a.Stdin,
		// The TUI renders to the validated terminal: the gate checked
		// stderr, so a redirected stdout must never receive alt-screen bytes.
		Output: prevErr,
		Scan: func(diag io.Writer, stop <-chan struct{}) picker.ScanResult {
			sink.bind(diag)
			return a.pickerScan(stop, &cat)
		},
		// Build adapts each selection through buildRequests individually:
		// one row per selection, in selection order, with the selection
		// index as stable identity. Per-selection calls keep the
		// label-keyed skip map from collapsing duplicate installs.
		Build: func(selected []resolve.Resolution) picker.FlowPlan {
			var plan picker.FlowPlan
			for i, r := range selected {
				reqs, skips := buildRequests(cat, []resolve.Resolution{r}, false /* picker is always quiet */, a.Pins)
				if len(reqs) == 1 {
					plan.Rows = append(plan.Rows, picker.FlowRow{
						RowID:        i,
						Label:        reqs[0].Label,
						RequestIndex: len(plan.Requests),
					})
					plan.Requests = append(plan.Requests, reqs[0])
					continue
				}
				label, reason := labelFor(r), "not actionable"
				for lk, rv := range skips {
					label, reason = lk, rv
				}
				plan.Rows = append(plan.Rows, picker.FlowRow{
					RowID:        i,
					Label:        label,
					RequestIndex: -1,
					Reason:       reason,
				})
			}
			return plan
		},
		Exec: a.UpdateExec,
	})
	if err != nil {
		fmt.Fprintf(prevErr, "ofa: picker: %v\n", err)
		return ExitError
	}
	return ExitCode(res.Code)
}

// pickerScan is the flow's Scan callback: the existing detect+resolve
// pipeline (plus the first-run scan) with its diagnostics routed into
// the TUI. Stages are launched only while no stop has been requested —
// once one is, no further stage starts and in-flight bounded work runs
// to completion. The synchronous detectors expose no cancellation API,
// so the flow waits for them and never claims they were canceled.
func (a *App) pickerScan(stop <-chan struct{}, catOut *[]catalog.Harness) picker.ScanResult {
	var res picker.ScanResult

	// First-run auto-scan (cli-surface spec): populate the state file so
	// later runs skip it. Its report lands in the TUI like any other
	// diagnostic. Same order as the plain path: scan first, then
	// catalog/config.
	if _, err := os.Stat(a.StatePath); errors.Is(err, os.ErrNotExist) {
		if code := a.scanCmd([]string{}); code != ExitOK {
			res.Code = int(code)
			return res
		}
	}
	if stopped(stop) {
		return res
	}
	cat, code := a.loadCatalog()
	if code != ExitOK {
		res.Code = int(code)
		return res
	}
	if code := a.loadConfig(); code != ExitOK {
		res.Code = int(code)
		return res
	}
	a.warnUnknownPins(cat)
	if stopped(stop) {
		return res
	}
	_, resolutions, _, code := a.runPipelineUntil(cat, stop)
	if code != ExitOK {
		res.Code = int(code)
		return res
	}
	*catOut = cat
	res.Resolutions = resolutions
	// The pin snapshot travels with the resolutions (loadConfig just
	// populated a.Pins). Normalize nil to an empty map: "loaded, nothing
	// pinned" is authoritative — stale pins must never survive unpinning.
	res.Pins = a.Pins
	if res.Pins == nil {
		res.Pins = map[string]bool{}
	}
	return res
}
