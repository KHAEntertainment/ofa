package cli

import (
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

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

// doctorCmd implements `ofa doctor [<bin>] [--agent claude]`: opt-in,
// agent-assisted repair for catalog entries whose binary is installed but
// not detected. Without a bin argument it runs the gap check and, when
// exactly one gap exists, proceeds with it; several gaps are listed for the
// user to pick with `ofa doctor <bin>`. It never loops over targets and
// never installs anything: the session ends with a validated proposal on
// disk (or rejected.txt) under the doctor history directory, for the
// follow-on install flow.
//
// Order of the early exits matters: a bin that is already detected or a
// gap listing needs no agent and must succeed without Claude installed,
// while a missing agent or a bin that is not on PATH must fail before any
// file is created.
func (a *App) doctorCmd(args []string) ExitCode {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	agent := fs.String("agent", "claude", "drafting agent to use (v1: claude only)")
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

	// A proposal that shadows an embedded id is allowed — the user catalog
	// wins by design — but it must be said out loud at the earliest moment
	// the human looks at the result.
	overrides := ""
	for _, h := range cat {
		if h.ID == res.Harness.ID {
			overrides = fmt.Sprintf(" (overrides embedded %q)", h.ID)
			break
		}
	}
	fmt.Fprintf(a.Stdout, "proposal ready: %s%s\n", res.ProposalPath, overrides)
	fmt.Fprintf(a.Stdout, "validate it yourself, then install it into your user catalog; ofa did not touch any catalog\n")
	return ExitOK
}
