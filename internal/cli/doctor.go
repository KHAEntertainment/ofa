package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ofa/internal/catalog"
	"ofa/internal/detect"
	"ofa/internal/doctor"
)

// doctorAgentLookPath is swapped in tests to exercise the no-agent path
// without depending on whether Claude Code happens to be installed on the
// machine running the suite.
var doctorAgentLookPath = exec.LookPath

// permuteArgs reorders argv so flags precede positional arguments: Go's
// flag package stops at the first positional, which made
// `ofa doctor <bin> --agent claude` — the order the usage text implies —
// fail with a bogus "too many arguments" error. A flag's value token (for
// flags that take one) travels with its flag; `--` ends flag parsing.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	seenDashDash := false
	// "--" ends flag parsing: everything after it is positional, and the
	// sentinel itself is preserved in the output so flag.Parse stops there
	// too.
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			seenDashDash = true
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				continue // --agent=claude carries its own value
			}
			if fl := fs.Lookup(name); fl != nil && !boolFlag(fl) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	// The end-of-flags sentinel is preserved only when present: everything
	// after it stays positional, so `ofa doctor -- --agent=codex` drafts for
	// a bin literally named "--agent=codex" instead of parsing a flag the
	// user meant to escape.
	if seenDashDash {
		flags = append(flags, "--")
	}
	return append(flags, positional...)
}

// boolFlag reports whether fl is a boolean flag (no value token).
func boolFlag(fl *flag.Flag) bool {
	bv, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && bv.IsBoolFlag()
}

// stdinIsTTY is the default StdinIsTTY gate for doctor's install prompt.
// It mirrors stderrIsTTY (picker.go) on the cheap ModeCharDevice check,
// with one exception the mode check alone gets wrong: /dev/null IS a
// character device, so `ofa doctor < /dev/null` would look interactive
// and prompt at a bottomless EOF. A stdin that is the same file as
// /dev/null is therefore not a terminal. Doctor only ever prompts — and
// only ever installs — when this is true; a non-interactive stdin gets
// the --install command printed instead.
func stdinIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	if !isTerminal(f) {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if nullSt, nerr := os.Stat(os.DevNull); nerr == nil && os.SameFile(st, nullSt) {
		return false
	}
	return true
}

// doctorCmd implements `ofa doctor [<bin>] [--agent claude]`,
// `ofa doctor --install <session-id>`, and `ofa doctor --list`: opt-in,
// agent-assisted repair for catalog entries whose binary is installed but
// not detected. Without a bin argument it runs the gap check and, when
// exactly one gap exists, proceeds with it; several gaps are listed for
// the user to pick with `ofa doctor <bin>`. A session that produces a
// validated proposal ends with a review block — every command the entry
// declares, update.cmd verbatim — and an explicit Install? prompt that
// defaults to No; only a yes at an interactive terminal writes into the
// user catalog. update.cmd itself is never executed by doctor.
//
// Order of the early exits matters: a bin that is already detected or a
// gap listing needs no agent and must succeed without Claude installed,
// while a missing agent or a bin that is not on PATH must fail before any
// file is created.
func (a *App) doctorCmd(args []string) ExitCode {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	agent := fs.String("agent", "claude", "drafting agent to use (v1: claude only)")
	install := fs.String("install", "", "install a proposal saved by an earlier doctor run, by session id")
	list := fs.Bool("list", false, "list saved doctor sessions")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return ExitError
	}
	// v1 is Claude-only by owner decision: Codex has no tool-less mode, so
	// accepting it here would silently break the no-tools rule. The flag
	// stays so the interface does not change when another agent earns its
	// place behind the seam.
	if *agent != "claude" {
		fmt.Fprintf(a.Stderr, "ofa: doctor: --agent %q is not supported: v1 drafts with claude only\n", *agent)
		return ExitError
	}
	// An explicitly empty --install ('' or --install=) is a usage error,
	// not "flag absent": falling through would run the gap check and could
	// spend an agent run the user never asked for.
	installSupplied := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "install" {
			installSupplied = true
		}
	})
	if installSupplied && *install == "" {
		fmt.Fprintf(a.Stderr, "ofa: doctor: --install needs a session id\n")
		a.usage()
		return ExitError
	}
	// --install, --list, and a positional <bin> are three different ways
	// to say what this invocation does. Combining them is a usage error,
	// not something to guess an order for.
	modes := 0
	for _, on := range []bool{*install != "", *list, fs.NArg() > 0} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintf(a.Stderr, "ofa: doctor: --install, --list, and a <bin> argument are mutually exclusive\n")
		a.usage()
		return ExitError
	}
	if *list {
		return a.doctorListCmd()
	}
	if *install != "" {
		return a.doctorInstallCmd(*install)
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(a.Stderr, "ofa: doctor: expected at most one <bin> argument, got %d\n", fs.NArg())
		a.usage()
		return ExitError
	}
	bin := fs.Arg(0)

	cat, code := a.loadCatalog()
	if code != ExitOK {
		return code
	}
	insts, _, code := a.runDetect(cat)
	if code != ExitOK {
		return code
	}

	if bin == "" {
		switch gaps := doctor.Gaps(cat, insts, nil); len(gaps) {
		case 0:
			fmt.Fprintln(a.Stdout, "no gaps: every catalog binary on PATH is detected")
			return ExitOK
		case 1:
			bin = gaps[0].Bin
			fmt.Fprintf(a.Stdout, "one gap: %s (catalog: %s) — drafting an entry\n", bin, strings.Join(gaps[0].HarnessIDs, ", "))
		default:
			fmt.Fprintln(a.Stdout, "catalog gaps (bin on PATH, no install detected):")
			for _, g := range gaps {
				fmt.Fprintf(a.Stdout, "  %s  (%s)\n", g.Bin, strings.Join(g.HarnessIDs, ", "))
			}
			fmt.Fprintln(a.Stdout, "run `ofa doctor <bin>` to draft an entry for one of them")
			return ExitOK
		}
	}

	// Already detected through an existing entry: nothing for doctor to
	// do, and no agent run to spend.
	for _, in := range insts {
		if in.Bin == bin {
			fmt.Fprintf(a.Stdout, "%s is already detected as %s (%s); nothing to do\n", bin, in.HarnessID, in.Method)
			return ExitOK
		}
	}

	if _, err := exec.LookPath(bin); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: %q is not on PATH\n", bin)
		return ExitError
	}
	if _, err := doctorAgentLookPath(doctor.ClaudeCommand); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor needs claude (Claude Code) on PATH; v1 supports --agent claude only\n")
		return ExitError
	}

	res, err := doctor.Run(bin, doctor.Options{
		Agent:     a.AgentFn,
		Runner:    doctor.BoundedRunnerFor(a.DetectRunner, bin),
		Timeout:   doctor.DefaultTimeout,
		DoctorDir: a.DoctorDir,
		Now:       a.Now,
	})
	if err != nil {
		// doctor.Run's errors already carry their own "doctor:" prefix.
		if res != nil {
			if res.RejectedPath != "" {
				fmt.Fprintf(a.Stderr, "ofa: %v\nraw draft saved to %s\n", err, res.RejectedPath)
			} else {
				fmt.Fprintf(a.Stderr, "ofa: %v\naudit trail: %s\n", err, filepath.Join(res.Dir, "context.md"))
			}
		} else {
			fmt.Fprintf(a.Stderr, "ofa: %v\n", err)
		}
		return ExitError
	}

	// The review block says everything the entry declares — including a
	// shadowed embedded id, if any — and the install offer only ever
	// follows a validated proposal.
	return a.doctorOfferInstall(res)
}

// doctorOfferInstall ends a successful drafting session: the review
// block, then the Install? prompt. The default answer is No. Non-TTY
// stdin (piped, redirected, /dev/null, or absent) never prompts and
// never installs — an agent-drafted update command is executed later by
// `ofa apply`, so only a human at an interactive terminal says yes —
// and the exact --install command to run is printed instead.
//
// The validated bytes travel in memory from validation to write. The
// proposal file on disk is never re-read here, so nothing that swaps it
// between the review and the confirm can change what is installed: the
// bytes that were validated are the bytes that were reviewed are the
// bytes that are written.
func (a *App) doctorOfferInstall(res *doctor.Result) ExitCode {
	h := *res.Harness
	fmt.Fprintf(a.Stdout, "proposal ready: %s\n", res.ProposalPath)
	if err := a.printDoctorReview(h); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: could not display the review, so nothing was installed: %v\n", err)
		return ExitError
	}
	sessionID := filepath.Base(res.Dir)
	if !a.interactiveStdin() {
		fmt.Fprintf(a.Stdout, "to install it, run: ofa doctor --install %s\n", sessionID)
		return ExitOK
	}
	return a.doctorConfirmAndWrite(sessionID, res.Dir, h, res.ProposalBytes)
}

// doctorConfirmAndWrite asks the install question (default No) and, on a
// yes, performs the write: the second confirms, the atomic write, the
// session bookkeeping, and the post-install scan.
func (a *App) doctorConfirmAndWrite(sessionID, dir string, h catalog.Harness, src []byte) ExitCode {
	// The write is bound to the review one more time, right before the
	// question is asked: the bytes must parse to exactly the harness the
	// human is about to decide on. This re-check makes the binding a
	// property of the write path itself, not only of how the bytes
	// traveled to it.
	if err := doctor.BindCheck(h, src); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: the bytes to write no longer match the reviewed entry; nothing was written: %v\n", err)
		return ExitError
	}
	target := filepath.Join(a.CatalogDir, h.ID+".toml")
	ok, err := confirmPrompt(a.Stdin, a.Stderr, fmt.Sprintf("Install to %s? [y/N] ", doctor.Visible(target)))
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: confirm: %v; nothing was written\n", err)
		return ExitError
	}
	if !ok {
		fmt.Fprintln(a.Stdout, "declined; nothing was written")
		return ExitOK
	}
	return a.doctorWriteInstalled(sessionID, dir, h, src)
}

// doctorWriteInstalled performs the confirmed write. Before anything
// lands there are two second confirms, each with its own warning: an
// existing file for this id (overwriting destroys the user's own work),
// and another user catalog file already declaring the id (the user
// catalog merges first-file-wins, so one of the two would be silently
// ignored). Then the write itself is atomic, the session is marked
// installed, and the target is re-scanned so the win — or its absence —
// is visible.
func (a *App) doctorWriteInstalled(sessionID, dir string, h catalog.Harness, src []byte) ExitCode {
	target := filepath.Join(a.CatalogDir, h.ID+".toml")
	overwrite := false
	if _, err := os.Stat(target); err == nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: %s already exists\n", doctor.Visible(target))
		ok, err := confirmPrompt(a.Stdin, a.Stderr, fmt.Sprintf("Overwrite %s? [y/N] ", doctor.Visible(target)))
		if err != nil {
			fmt.Fprintf(a.Stderr, "ofa: confirm: %v; nothing was written\n", err)
			return ExitError
		}
		if !ok {
			fmt.Fprintln(a.Stdout, "declined; the existing file was left untouched")
			return ExitOK
		}
		overwrite = true
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(a.Stderr, "ofa: doctor: %v\n", err)
		return ExitError
	}
	if dups := doctor.DuplicateIDFiles(a.CatalogDir, h.ID); len(dups) > 0 {
		fmt.Fprintf(a.Stderr, "ofa: doctor: user catalog file(s) %s already define harness id %q; user files merge first-by-filename, so one of the two will be ignored\n", doctor.Visible(strings.Join(dups, ", ")), h.ID)
		ok, err := confirmPrompt(a.Stdin, a.Stderr, "Install anyway? [y/N] ")
		if err != nil {
			fmt.Fprintf(a.Stderr, "ofa: confirm: %v; nothing was written\n", err)
			return ExitError
		}
		if !ok {
			fmt.Fprintln(a.Stdout, "declined; nothing was written")
			return ExitOK
		}
	}
	path, err := doctor.WriteCatalogEntry(h.ID, src, a.CatalogDir, overwrite)
	if err != nil {
		var published *doctor.PublishedError
		if !errors.As(err, &published) {
			// The errors already carry the "doctor:" prefix, and may echo
			// the configured catalog path: render them terminal-safe.
			fmt.Fprintf(a.Stderr, "ofa: %s\n", doctor.Visible(err.Error()))
			return ExitError
		}
		// The entry is in place; only housekeeping after the publish
		// failed. A warning, never a failed install.
		fmt.Fprintf(a.Stderr, "ofa: warning: %s\n", doctor.Visible(err.Error()))
	}
	fmt.Fprintf(a.Stdout, "installed: %s\n", doctor.Visible(path))
	// Session bookkeeping. The entry is already published, so a failure
	// here is a warning on stderr and a successful exit — never a
	// rollback of a write the user confirmed.
	rec, lerr := doctor.LoadSessionRecord(dir)
	if lerr != nil {
		fmt.Fprintf(a.Stderr, "ofa: warning: could not mark the session installed: %v\n", lerr)
	} else {
		rec.Status = doctor.StatusInstalled
		rec.InstalledPath = path
		if serr := doctor.SaveSessionRecord(dir, rec); serr != nil {
			fmt.Fprintf(a.Stderr, "ofa: warning: could not mark the session installed: %v\n", serr)
		}
	}
	return a.doctorPostInstallScan(h)
}

// doctorPostInstallScan re-scans through the normal catalog loader — the
// same merged path every command uses — and reports whether the target is
// now detected. An ACTIVE instance is searched for across the whole scan
// before any conclusion is drawn: scan order follows catalog order, not
// PATH order, so an earlier shadowed install of the same binary must not
// hide the live one behind it. A miss is reported plainly, never papered
// over: the file stays in place for inspection, but the command does not
// claim success.
func (a *App) doctorPostInstallScan(h catalog.Harness) ExitCode {
	// The shape gate bound every install to the session's target binary.
	bin := h.Installs[0].Bin
	cat, code := a.loadCatalog()
	if code != ExitOK {
		return code
	}
	insts, _, err := a.DetectFn(cat, a.DetectRunner)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: could not scan after install: %v\n", err)
		return ExitError
	}
	var shadowed []detect.Instance
	for _, in := range insts {
		if in.Bin != bin {
			continue
		}
		if in.Status == detect.Active {
			fmt.Fprintf(a.Stdout, "detected: %s %s (%s, %s)\n",
				doctor.Visible(bin), doctor.Visible(in.Version), doctor.Visible(in.HarnessID), doctor.Visible(in.Method))
			return ExitOK
		}
		shadowed = append(shadowed, in)
	}
	// No active instance: report every shadowed one honestly — the
	// entry matches something on this machine, just not the PATH winner.
	for _, in := range shadowed {
		fmt.Fprintf(a.Stdout, "detected: %s %s (%s, %s) (not the live install on PATH)\n",
			doctor.Visible(bin), doctor.Visible(in.Version), doctor.Visible(in.HarnessID), doctor.Visible(in.Method))
	}
	if len(shadowed) > 0 {
		return ExitError
	}
	fmt.Fprintf(a.Stderr, "ofa: doctor: %s was NOT detected after install; the entry is in place at %s but does not match this machine\n", doctor.Visible(bin), doctor.Visible(filepath.Join(a.CatalogDir, h.ID+".toml")))
	return ExitError
}

// doctorInstallCmd implements `ofa doctor --install <session-id>`: it
// installs a saved proposal after RE-RUNNING the full validation. Nothing
// in the history directory is trusted — the proposal file may have been
// edited since the run wrote it — so the same mechanical gate runs again
// (parse with unknown keys rejected, the static shape gate, and
// test-detect through the pinned bounded runner) against the target
// recorded in session.json. The whole file, as-is, must parse as one
// valid entry: no fence extraction happens here, so an outer entry
// cannot wrap a benign inner one and smuggle unreviewed bytes past the
// gate. A target taken from the proposal itself would make the shape
// gate's identity check vacuous, which is why sessions without a record
// are refused rather than guessed. A proposal that fails any check is
// rejected with the reason and nothing is written.
func (a *App) doctorInstallCmd(sessionID string) ExitCode {
	if !doctor.ValidSessionID(sessionID) {
		fmt.Fprintf(a.Stderr, "ofa: doctor: invalid session id %q\n", sessionID)
		a.usage()
		return ExitError
	}
	dir := filepath.Join(a.DoctorDir, "history", sessionID)
	rec, err := doctor.LoadSessionRecord(dir)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: cannot install %s: %v\n", sessionID, err)
		return ExitError
	}
	src, err := doctor.LoadProposal(dir)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: cannot install %s: %v\n", sessionID, err)
		return ExitError
	}
	// The recorded target must still exist on PATH; a proposal for a
	// binary that is gone cannot be revalidated, and nothing is written.
	_, real, err := doctor.AuthorizedTarget(rec.Target)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: target %q is not on PATH anymore; nothing was written\n", rec.Target)
		a.markSessionRejected(dir, rec)
		return ExitError
	}
	cand, err := doctor.ValidateSavedProposal(src, rec.Target, real, doctor.BoundedRunnerFor(a.DetectRunner, rec.Target))
	if err != nil {
		// The reason can echo the file's own text; render it terminal-safe.
		fmt.Fprintf(a.Stderr, "ofa: doctor: proposal in session %s is no longer valid: %s\nnothing was written\n", sessionID, doctor.Visible(err.Error()))
		a.markSessionRejected(dir, rec)
		return ExitError
	}
	fmt.Fprintf(a.Stdout, "revalidated proposal from session %s (target %s)\n", sessionID, doctor.Visible(rec.Target))
	if err := a.printDoctorReview(cand.Harness); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: doctor: could not display the review, so nothing was installed: %v\n", err)
		return ExitError
	}
	if !a.interactiveStdin() {
		fmt.Fprintf(a.Stderr, "ofa: doctor: --install needs an interactive terminal to confirm; nothing was written\n")
		return ExitError
	}
	// cand is the one immutable snapshot: the harness the review just
	// showed and the exact bytes that will be written are the same fact.
	return a.doctorConfirmAndWrite(sessionID, dir, cand.Harness, cand.Bytes)
}

// markSessionRejected records a failed revalidation in session.json so
// --list shows the verdict. A bookkeeping failure here must not mask the
// rejection; the proposal still does not install.
func (a *App) markSessionRejected(dir string, rec doctor.SessionRecord) {
	// An installed session stays installed: its entry is in the catalog,
	// and a failed re-run of --install does not undo that.
	if rec.Status == doctor.StatusInstalled {
		return
	}
	rec.Status = doctor.StatusRejected
	if err := doctor.SaveSessionRecord(dir, rec); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: warning: could not record the rejection: %v\n", err)
	}
}

// doctorListCmd implements `ofa doctor --list`: one line per history
// session. Sessions from before session.json existed show "unknown"
// rather than failing the listing; pruning is out of scope.
func (a *App) doctorListCmd() ExitCode {
	root := filepath.Join(a.DoctorDir, "history")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(a.Stdout, "no doctor sessions yet")
			return ExitOK
		}
		fmt.Fprintf(a.Stderr, "ofa: doctor: %v\n", err)
		return ExitError
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n == 0 {
			fmt.Fprintln(a.Stdout, "doctor sessions:")
		}
		n++
		target, status, path := "unknown", "unknown", ""
		if rec, err := doctor.LoadSessionRecord(filepath.Join(root, e.Name())); err == nil {
			target, status, path = rec.Target, rec.Status, rec.InstalledPath
		}
		// Directory names, session fields, and paths all come from a
		// user-owned directory; nothing they carry is emitted raw.
		line := fmt.Sprintf("  %s  %s  %s", doctor.Visible(e.Name()), doctor.Visible(target), doctor.Visible(status))
		if path != "" {
			line += fmt.Sprintf("  -> %s", doctor.Visible(path))
		}
		fmt.Fprintln(a.Stdout, line)
	}
	if n == 0 {
		fmt.Fprintln(a.Stdout, "no doctor sessions yet")
	}
	return ExitOK
}

// printDoctorReview renders the review block to Stderr — the SAME stream
// the confirm prompt is printed on — so a redirected stdout can never
// separate the install question from the security review that justifies
// it. A write failure is returned rather than ignored: the caller must
// not install a proposal whose review could not be shown.
func (a *App) printDoctorReview(h catalog.Harness) error {
	var embedded *catalog.Harness
	if embeddedList, err := catalog.Embedded(); err == nil {
		for i := range embeddedList {
			if embeddedList[i].ID == h.ID {
				embedded = &embeddedList[i]
				break
			}
		}
	}
	w := a.Stderr
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	lines := doctor.Review{H: h, Overrides: embedded}.Lines()
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// interactiveStdin reports whether the install prompts may ask a human:
// stdin must be present and a terminal. Everything else — a pipe, a file,
// nil — is the non-interactive path.
func (a *App) interactiveStdin() bool {
	return a.Stdin != nil && a.StdinIsTTY(a.Stdin)
}
