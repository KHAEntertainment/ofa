package cli

import (
	"bufio"
	"flag"
	"fmt"
	"strings"

	"ofa/internal/catalog"
	"ofa/internal/resolve"
	"ofa/internal/update"
)

// applyCmd implements `ofa apply [--yes] [--only <id>] [--dry-run|--plan] [--verbose]`.
//
// Runs every install with an update available (Outdated=true OR Tier=self)
// by delegating to the catalog's update.cmd. Confirmation prompts for the
// interactive case unless --yes is set.
//
// --verbose streams each installer's full output to stdout (with bracket
// banners). Default is quiet: only progress lines on stderr, with the
// installer's own output captured for the failure tail.
func (a *App) applyCmd(args []string) ExitCode {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	only := fs.String("only", "", "limit to one harness id")
	dryRun := fs.Bool("dry-run", false, "print commands without executing")
	// --plan is the alias the cli-surface spec calls out. Identical
	// semantics to --dry-run.
	plan := fs.Bool("plan", false, "alias for --dry-run")
	verbose := fs.Bool("verbose", false, "stream installer output; default is progress lines only")
	refresh := fs.Bool("refresh", false, "bypass cached latest versions; re-query registries")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	a.Refresh = *refresh
	if *plan {
		*dryRun = true
	}

	cat, code := a.loadCatalog()
	if code != ExitOK {
		return code
	}
	if code := a.loadConfig(); code != ExitOK {
		return code
	}
	a.warnUnknownPins(cat)
	_, resolutions, _, code := a.runPipeline(cat)
	if code != ExitOK {
		return code
	}

	requests, skipReasons := buildRequests(cat, filterByHarness(resolutions, *only), *verbose, a.Pins)
	for label, reason := range skipReasons {
		fmt.Fprintf(a.Stderr, "ofa: skip %s: %s\n", label, reason)
	}

	if len(requests) == 0 {
		fmt.Fprintln(a.Stdout, "nothing to apply")
		return ExitOK
	}

	fmt.Fprintln(a.Stdout)
	fmt.Fprintf(a.Stdout, "Will run %d update(s)", len(requests))
	if *dryRun {
		fmt.Fprint(a.Stdout, " (dry-run)")
	}
	fmt.Fprintln(a.Stdout)
	for _, r := range requests {
		fmt.Fprintf(a.Stdout, "  - %s\n", r.Label)
	}
	fmt.Fprintln(a.Stdout)

	if !*dryRun && !*yes {
		ok, err := confirmPrompt(a.Stdin, a.Stderr, "Apply? [y/N] ")
		if err != nil {
			fmt.Fprintf(a.Stderr, "ofa: confirm: %v\n", err)
			return ExitError
		}
		if !ok {
			fmt.Fprintln(a.Stderr, "aborted")
			return ExitOK
		}
	}

	// In --dry-run, set DryRun=true on every request *before* handing
	// them to update.Apply. That way update.Apply's PATH-existence
	// preflight (intended only for real runs) is bypassed — the request
	// models a dry-run whether the binary is installed or not.
	for i := range requests {
		requests[i].DryRun = *dryRun
	}
	summary := update.Apply(requests, a.UpdateExec)
	printApplySummary(a.Stdout, summary)

	return ExitCode(summary.ExitCode())
}

// buildRequests converts applicable resolutions into update.Requests,
// matching each resolution against its catalog install block to pull the
// update.cmd. Resolutions whose harness isn't in the catalog or whose
// update.cmd is empty are dropped (with a reason) — shouldn't happen in
// practice, but a stray catalog entry shouldn't crash the run.
func buildRequests(cat []catalog.Harness, resolutions []resolve.Resolution, verbose bool, pins map[string]bool) ([]update.Request, map[string]string) {
	byID := map[string]catalog.Harness{}
	for _, h := range cat {
		byID[h.ID] = h
	}
	var reqs []update.Request
	skipReasons := map[string]string{}
	for _, r := range resolutions {
		// Only outdated + self-tier have updates to apply. Tier=none and
		// at-latest installs are reported by check but skipped here.
		if !(r.Outdated || r.Tier == "self") {
			continue
		}
		// A pin is authoritative: it suppresses the update even under
		// --only, so `ofa apply --only <pinned>` cannot quietly ignore a
		// freeze. Unpin in the config to update.
		if pins[r.HarnessID] {
			skipReasons[labelFor(r)] = "pinned in config"
			continue
		}
		// A shadowed native install's catalog command is "<bin> update",
		// resolved via PATH. That reaches the live binary, not the copy this
		// resolution names, so running it would update something else.
		if r.Shadowed && r.Method == "native" {
			skipReasons[labelFor(r)] = "shadowed; its update command targets the live binary"
			continue
		}
		h, ok := byID[r.HarnessID]
		if !ok {
			skipReasons[labelFor(r)] = "harness not in catalog"
			continue
		}
		in, ok := findInstall(h, r)
		if !ok {
			skipReasons[labelFor(r)] = "no matching install block"
			continue
		}
		if len(in.Update.Cmd) == 0 {
			skipReasons[labelFor(r)] = "no update command (catalog)"
			continue
		}
		reqs = append(reqs, update.Request{
			Label:   labelFor(r),
			Cmd:     update.CmdSpec{Cmd: in.Update.Cmd},
			Verbose: verbose,
		})
	}
	return reqs, skipReasons
}

// findInstall matches a Resolution back to its catalog install block.
// Mirrors resolve.findInstall — duplicated here so the cli package doesn't
// need to expose internal resolve helpers.
func findInstall(h catalog.Harness, r resolve.Resolution) (catalog.Install, bool) {
	for _, in := range h.Installs {
		if in.Method != r.Method {
			continue
		}
		if in.Bin != r.Bin {
			continue
		}
		if in.Package != r.Package {
			continue
		}
		return in, true
	}
	return catalog.Install{}, false
}

// labelFor returns "harness (method)" — the human identifier used in
// per-update logs.
func labelFor(r resolve.Resolution) string {
	return fmt.Sprintf("%s (%s)", r.HarnessID, r.Method)
}

// confirmPrompt asks the user a y/N question on stdin. Returns true for
// "y" / "Y", false for anything else (including EOF / empty input).
func confirmPrompt(stdin interface{ Read([]byte) (int, error) }, stderr interface{ Write([]byte) (int, error) }, prompt string) (bool, error) {
	fmt.Fprint(stderr, prompt)
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, err
		}
		return false, nil // EOF
	}
	ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return ans == "y" || ans == "yes", nil
}

// printApplySummary renders the succeeded / failed / skipped buckets.
func printApplySummary(w interface{ Write([]byte) (int, error) }, s update.Summary) {
	fmt.Fprintf(w, "\nSummary: %d succeeded, %d failed, %d skipped\n",
		len(s.Succeeded), len(s.Failed), len(s.Skipped))
	if len(s.Failed) > 0 {
		fmt.Fprintln(w, "FAILURES:")
		for _, f := range s.Failed {
			fmt.Fprintf(w, "  - %s: exit %d, err=%v\n", f.Label, f.ExitCode, f.Err)
			if f.OutputTail != "" {
				for _, line := range strings.Split(f.OutputTail, "\n") {
					fmt.Fprintf(w, "      %s\n", line)
				}
			}
		}
	}
	if len(s.Skipped) > 0 {
		fmt.Fprintln(w, "SKIPPED:")
		for _, sk := range s.Skipped {
			fmt.Fprintf(w, "  - %s: %s\n", sk.Label, sk.SkippedReason)
		}
	}
}
