package cli

import (
	"errors"
	"fmt"
	"os"

	"ofa/internal/picker"
	"ofa/internal/update"
)

// isTerminal reports whether f is a character device (terminal, console,
// pipe). Used to bail out of the interactive picker when OFA is invoked
// from a non-interactive context (CI, piped input, etc.).
//
// Cheap stat-based check; avoids the extra dep that golang.org/x/term
// would bring. The downstream bubbletea.Program will do its own
// isatty-style detection on top of this.
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

// pickerCmd is the interactive default: `ofa` (no subcommand). On a
// machine with no state.json it auto-scans first (per cli-surface spec)
// so the very first run still works without a separate `ofa scan` step.
//
// The flow:
//
//  1. If state file is missing → run scan to populate it. The scan's
//     human output goes to stderr so it isn't erased when the alt-screen
//     picker takes over.
//  2. Detect + resolve against the catalog.
//  3. Hand resolutions to the picker TUI.
//  4. If the user pressed Enter on a selection → run update.Apply with
//     the chosen items and return its exit code.
//  5. If the user pressed q (or Ctrl-C) → exit cleanly with 0.
//
// The picker is the only piece of OFA that requires a TTY. When stdin
// isn't a TTY, the bubbletea program will fail to start; we surface a
// clear "use a subcommand instead" message.
func (a *App) pickerCmd(args []string) ExitCode {
	// First-run auto-scan. Run scan only if there's no state file; the
	// scan writes the file as a side effect, so subsequent runs skip.
	// This runs even when stdin is not a TTY — a user piping `ofa` into
	// a non-interactive context still benefits from a populated state.
	if _, err := os.Stat(a.StatePath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(a.Stderr, "first run: scanning...")
		if code := a.scanCmd([]string{}); code != ExitOK {
			return code
		}
	}

	// Use a.Stdin (not os.Stdin) so tests can inject a non-TTY reader
	// and exercise the bail-out path. isTerminal on a nil reader
	// returns false, which is the correct behavior for tests.
	stdin, ok := a.Stdin.(*os.File)
	if !ok || !isTerminal(stdin) {
		fmt.Fprintln(a.Stderr, "ofa: no TTY detected; use `ofa scan`, `ofa check`, or `ofa apply` instead")
		return ExitError
	}

	cat, code := a.loadCatalog()
	if code != ExitOK {
		return code
	}
	_, resolutions, _, code := a.runPipeline(cat)
	if code != ExitOK {
		return code
	}

	outcome, err := picker.Run(resolutions)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: picker: %v\n", err)
		return ExitError
	}

	if outcome.Quit || !outcome.Apply {
		return ExitOK
	}
	if len(outcome.Selected) == 0 {
		fmt.Fprintln(a.Stdout, "nothing selected")
		return ExitOK
	}

	requests, skip := buildRequests(cat, outcome.Selected, false /* picker is always quiet */)
	for label, reason := range skip {
		fmt.Fprintf(a.Stderr, "ofa: skip %s: %s\n", label, reason)
	}
	if len(requests) == 0 {
		fmt.Fprintln(a.Stdout, "nothing to apply")
		return ExitOK
	}

	fmt.Fprintln(a.Stdout)
	fmt.Fprintf(a.Stdout, "Applying %d update(s)\n", len(requests))
	summary := update.Apply(requests, a.UpdateExec)
	printApplySummary(a.Stdout, summary)
	return ExitCode(summary.ExitCode())
}