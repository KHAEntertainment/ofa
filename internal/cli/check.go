package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"sort"

	"ofa/internal/catalog"
	"ofa/internal/detect"
	"ofa/internal/resolve"
	"ofa/internal/state"
)

// checkCmd implements `ofa check [--json] [--only <id>]`. Reports every
// install that has an update available (Outdated=true OR Tier=self)
// together with drift. Returns ExitUpdatesPending if any are present so
// the command is scriptable.
func (a *App) checkCmd(args []string) ExitCode {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit JSON on stdout")
	only := fs.String("only", "", "limit output to one harness id")
	refresh := fs.Bool("refresh", false, "bypass cached latest versions; re-query registries")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	a.Refresh = *refresh

	cat, code := a.loadCatalog()
	if code != ExitOK {
		return code
	}
	if code := a.loadConfig(); code != ExitOK {
		return code
	}
	a.warnUnknownPins(cat)
	insts, resolutions, failures, code := a.runPipeline(cat)
	if code != ExitOK {
		return code
	}
	prev, code := a.loadState()
	if code != ExitOK {
		return code
	}
	current := reconcileDegraded(prev, state.FromInstances(insts, a.Now()), failures)
	drift := state.Diff(prev, current)

	if *jsonOut {
		return a.printCheckJSON(cat, resolutions, current, drift, failures, *only)
	}
	return a.printCheckHuman(cat, resolutions, current, drift, *only)
}

func (a *App) printCheckHuman(cat []catalog.Harness, resolutions []resolve.Resolution, current *stateFile, drift state.Drift, only string) ExitCode {
	filtered := filterByHarness(resolutions, only)
	updateCount := countUpdates(filtered, a.Pins)
	updaterCount := countUpdaters(filtered, a.Pins)

	// Both counts are always printed so the line keeps a fixed shape for
	// scripts. Only `updateCount` is known-pending, so only it decides the
	// exit code: an available updater is an offer, not an outstanding update.
	fmt.Fprintf(a.Stdout, "scan: %d harnesses, %d with updates, %d updaters available\n",
		harnessCount(current), updateCount, updaterCount)
	if len(drift.Added)+len(drift.Removed)+len(drift.Changed) > 0 {
		fmt.Fprintln(a.Stdout)
		printDrift(a.Stdout, drift)
	}
	fmt.Fprintln(a.Stdout)
	for _, r := range filtered {
		printUpdateRow(a.Stdout, r, a.Pins[r.HarnessID])
	}
	if updateCount > 0 {
		return ExitUpdatesPending
	}
	return ExitOK
}

func (a *App) printCheckJSON(cat []catalog.Harness, resolutions []resolve.Resolution, current *stateFile, drift state.Drift, failures []detect.Failure, only string) ExitCode {
	type installOut struct {
		Method           string `json:"method"`
		Package          string `json:"package,omitempty"`
		Bin              string `json:"bin"`
		Path             string `json:"path,omitempty"`
		Version          string `json:"version"`
		Status           string `json:"status"`
		Tier             string `json:"tier"`
		Latest           string `json:"latest,omitempty"`
		Outdated         bool   `json:"outdated"`
		UpdaterAvailable bool   `json:"updater_available"`
		Pinned           bool   `json:"pinned,omitempty"`
		ResolverErr      string `json:"resolver_error,omitempty"`
	}
	type harnessOut struct {
		Name     string       `json:"name,omitempty"`
		Installs []installOut `json:"installs"`
	}
	type checkOut struct {
		ScannedAt    string                `json:"scanned_at"`
		HarnessCount int                   `json:"harness_count"`
		InstallCount int                   `json:"install_count"`
		UpdateCount  int                   `json:"update_count"`
		UpdaterCount int                   `json:"updater_available_count"`
		Harnesses    map[string]harnessOut `json:"harnesses"`
		Drift        *driftJSON            `json:"drift,omitempty"`
		Failures     []failureOut          `json:"failures,omitempty"`
	}

	byID := map[string]catalog.Harness{}
	for _, h := range cat {
		byID[h.ID] = h
	}
	filtered := filterByHarness(resolutions, only)
	out := checkOut{
		ScannedAt:    current.ScannedAt,
		HarnessCount: harnessCount(current),
		InstallCount: installCount(current),
		UpdateCount:  countUpdates(filtered, a.Pins),
		UpdaterCount: countUpdaters(filtered, a.Pins),
		Harnesses:    map[string]harnessOut{},
		Drift:        driftJSONPtr(drift),
		Failures:     failureOutList(failures),
	}
	for _, r := range filtered {
		h, ok := byID[r.HarnessID]
		name := ""
		if ok {
			name = h.Name
		}
		ho := out.Harnesses[r.HarnessID]
		ho.Name = name
		ho.Installs = append(ho.Installs, installOut{
			Method:           r.Method,
			Package:          r.Package,
			Bin:              r.Bin,
			Version:          r.Cur,
			Status:           statusString(r),
			Tier:             r.Tier,
			Latest:           r.Latest,
			Outdated:         r.Outdated,
			UpdaterAvailable: r.Tier == "self",
			Pinned:           a.Pins[r.HarnessID],
			ResolverErr:      r.ResolverErr,
		})
		out.Harnesses[r.HarnessID] = ho
	}
	// Stable JSON: sort harness keys alphabetically. Encoding a map gives
	// sorted keys in Go 1.12+, but we keep this for older-toolchain users.
	ids := make([]string, 0, len(out.Harnesses))
	for k := range out.Harnesses {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	ordered := struct {
		ScannedAt    string                `json:"scanned_at"`
		HarnessCount int                   `json:"harness_count"`
		InstallCount int                   `json:"install_count"`
		UpdateCount  int                   `json:"update_count"`
		UpdaterCount int                   `json:"updater_available_count"`
		Harnesses    map[string]harnessOut `json:"harnesses"`
		Drift        *driftJSON            `json:"drift,omitempty"`
		Failures     []failureOut          `json:"failures,omitempty"`
	}{
		ScannedAt:    out.ScannedAt,
		HarnessCount: out.HarnessCount,
		InstallCount: out.InstallCount,
		UpdateCount:  out.UpdateCount,
		UpdaterCount: out.UpdaterCount,
		Harnesses:    map[string]harnessOut{},
		Drift:        out.Drift,
		Failures:     out.Failures,
	}
	for _, id := range ids {
		ordered.Harnesses[id] = out.Harnesses[id]
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ordered); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: json: %v\n", err)
		return ExitError
	}
	if out.UpdateCount > 0 {
		return ExitUpdatesPending
	}
	return ExitOK
}

// printUpdateRow renders one resolution as a human-readable line.
// Shape:
//
//	<name>  <version>  [<tier>]  → <latest>            (outdated)
//	<name>  <version>  [self]    → updater available  (self tier)
//	<name>  <version>  [<tier>]  (no resolution)      (absent / unknown)
//
// The "(no resolution)" branch exists so the user sees *something* when
// a resolver failed instead of an invisible blank row.
//
// Field widths: harness id is left-padded to 14 to align the column;
// version is left as-is because some harnesses (claude, devin,
// cursor-agent) embed extra context after the version, which a fixed
// width would clip or wrap. The slight ragged right edge is the
// trade-off for keeping the version visible in full.
func printUpdateRow(w interface{ Write([]byte) (int, error) }, r resolve.Resolution, pinned bool) {
	// Only annotate rows where the pin actually suppresses something; an
	// at-latest row says nothing about a pin either way.
	suffix := ""
	// Shadowed is a fact about the install, so it is always marked; the pin
	// is only worth marking where it suppresses something.
	if r.Shadowed {
		suffix += " (shadowed)"
	}
	if pinned && (r.Outdated || r.Tier == "self") {
		suffix += " (pinned)"
	}
	switch {
	case r.Outdated:
		fmt.Fprintf(w, "  %-14s %-32s [%s]  %s → %s%s\n", r.HarnessID, r.Cur, r.Tier, r.Bin, r.Latest, suffix)
	case r.Tier == "self" && updaterReachable(r):
		fmt.Fprintf(w, "  %-14s %-32s [self]  %s → updater available%s\n", r.HarnessID, r.Cur, r.Bin, suffix)
	case r.Tier == "none" || (r.Tier == "self" && !updaterReachable(r)):
		fmt.Fprintf(w, "  %-14s %-32s [none]  %s → no update path%s\n", r.HarnessID, r.Cur, r.Bin, suffix)
	case r.ResolverErr != "":
		fmt.Fprintf(w, "  %-14s %-32s [%s]  %s → (resolver error: %s)%s\n", r.HarnessID, r.Cur, r.Tier, r.Bin, r.ResolverErr, suffix)
	default:
		fmt.Fprintf(w, "  %-14s %-32s [%s]  %s (at latest)%s\n", r.HarnessID, r.Cur, r.Tier, r.Bin, suffix)
	}
}

// updaterReachable reports whether OFA can run an update that targets this
// install. Registry updaters name their package (`brew upgrade <pkg>`,
// `npm i -g <pkg>`, `uv tool upgrade <pkg>`), so they always reach the copy
// they manage. Native updaters are `<bin> update`, resolved via PATH: for a
// shadowed native install that runs the live copy instead, so the catalog
// command cannot update the copy it claims to target.
func updaterReachable(r resolve.Resolution) bool {
	return !(r.Shadowed && r.Method == "native")
}

// statusString names an install the way state.json does, so `check --json`
// and the state file agree on "active" vs "shadowed".
func statusString(r resolve.Resolution) string {
	if r.Shadowed {
		return "shadowed"
	}
	return "active"
}

// filterByHarness filters resolutions to those matching the given harness id.
// An empty `only` returns the input unchanged.
func filterByHarness(res []resolve.Resolution, only string) []resolve.Resolution {
	if only == "" {
		return res
	}
	out := make([]resolve.Resolution, 0, len(res))
	for _, r := range res {
		if r.HarnessID == only {
			out = append(out, r)
		}
	}
	return out
}

// countUpdates returns how many resolutions are known to be outdated —
// i.e. a package manager reported a newer version than what is installed.
// This is the number that drives the headline and the exit code: it is the
// only case where OFA can *prove* something is pending.
//
// Self-tier installs are deliberately excluded: they carry no Latest, so
// Outdated is always false for them. See countUpdaters.
func countUpdates(res []resolve.Resolution, pins map[string]bool) int {
	n := 0
	for _, r := range res {
		// A pinned harness's update is known but deliberately declined, so
		// it must not set the exit code. That is the whole point of a pin:
		// scripts keep seeing 0 while the harness is held back.
		if r.Outdated && !pins[r.HarnessID] {
			n++
		}
	}
	return n
}

// countUpdaters returns how many resolutions are self-tier — harnesses with
// an updater OFA can run but no version to compare against. Reporting one of
// these as a pending update is what made `ofa check` say "8 with updates"
// immediately after a fully successful apply; the two facts are now counted
// and reported separately.
func countUpdaters(res []resolve.Resolution, pins map[string]bool) int {
	n := 0
	for _, r := range res {
		if r.Tier == "self" && !pins[r.HarnessID] && updaterReachable(r) {
			n++
		}
	}
	return n
}
