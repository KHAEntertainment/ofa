package cli

import (
	"encoding/json"
	"flag"
	"fmt"

	"ofa/internal/detect"
	"ofa/internal/state"
)

// scanCmd implements `ofa scan [--json]`. Detects installs, reports drift
// vs. the saved state, and writes the new state. Resolution is *not*
// performed — scan answers "what's installed?" not "what's outdated?".
//
// The error result is non-nil only when the catalog, detection and the
// human/JSON report all succeeded and only the state write failed: the
// separation the interactive first run needs to treat that one failure
// as recoverable without ever mistaking a catalog or detection failure
// for it (issue #66). Every other failure returns nil.
func (a *App) scanCmd(args []string) (ExitCode, error) {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "emit JSON on stdout")
	if err := fs.Parse(args); err != nil {
		return ExitError, nil
	}

	cat, code := a.loadCatalog()
	if code != ExitOK {
		return code, nil
	}
	// Detect only — scan answers "what's installed?", and resolving here
	// would run every registry query for results scan discards (issue #8).
	insts, failures, code := a.runDetect(cat)
	if code != ExitOK {
		return code, nil
	}
	prev, code := a.loadState()
	if code != ExitOK {
		return code, nil
	}
	current := reconcileDegraded(prev, state.FromInstances(insts, a.Now()), failures)
	// Carry the latest-version cache forward: detect doesn't refresh it
	// (resolve does), so a scan rewrite must not drop what check/picker
	// cached.
	if prev != nil {
		current.Latest = prev.Latest
	}
	drift := state.Diff(prev, current)

	if *jsonOut {
		return a.printScanJSON(current, drift, failures)
	}
	return a.printScanHuman(current, prev, drift)
}

// printScanHuman renders the human report and persists the scan. The
// returned error is the state-save failure, if any — it is not printed
// here; the caller reports it (issue #66).
func (a *App) printScanHuman(current *stateFile, prev *stateFile, drift state.Drift) (ExitCode, error) {
	fmt.Fprintf(a.Stdout, "%d harnesses (%d installs)\n", harnessCount(current), installCount(current))
	if prev == nil {
		fmt.Fprintln(a.Stdout, "initial scan")
	} else if len(drift.Added)+len(drift.Removed)+len(drift.Changed) == 0 {
		fmt.Fprintln(a.Stdout, "no drift since last scan")
	} else {
		printDrift(a.Stdout, drift)
	}
	printInstancesTable(a.Stdout, current)
	if err := a.saveState(current); err != nil {
		return ExitError, err
	}
	return ExitOK, nil
}

// printScanJSON renders the JSON report and persists the scan. The
// returned error is the state-save failure, if any — it is not printed
// here; the caller reports it (issue #66). A JSON encoding failure has
// already printed its own diagnostic, so it returns a nil error.
func (a *App) printScanJSON(current *stateFile, drift state.Drift, failures []detect.Failure) (ExitCode, error) {
	type scanOut struct {
		ScannedAt    string                   `json:"scanned_at"`
		HarnessCount int                      `json:"harness_count"`
		InstallCount int                      `json:"install_count"`
		InitialScan  bool                     `json:"initial_scan"`
		Harnesses    map[string]state.Harness `json:"harnesses"`
		Drift        *driftJSON               `json:"drift,omitempty"`
		Failures     []failureOut             `json:"failures,omitempty"`
	}
	out := scanOut{
		ScannedAt:    current.ScannedAt,
		HarnessCount: harnessCount(current),
		InstallCount: installCount(current),
		InitialScan:  false, // filled below
		Harnesses:    current.Harnesses,
		Drift:        driftJSONPtr(drift),
		Failures:     failureOutList(failures),
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: json: %v\n", err)
		return ExitError, nil
	}
	// Save after stdout succeeded so a JSON consumer never sees output
	// without a state file updated.
	if err := a.saveState(current); err != nil {
		return ExitError, err
	}
	return ExitOK, nil
}

// harnessCount returns the number of distinct harnesses in f.
func harnessCount(f *stateFile) int {
	return len(f.Harnesses)
}

// installCount returns the total number of installs across all harnesses.
func installCount(f *stateFile) int {
	n := 0
	for _, h := range f.Harnesses {
		n += len(h.Installs)
	}
	return n
}

// printInstancesTable renders the per-install table to w.
func printInstancesTable(w interface{ Write([]byte) (int, error) }, f *stateFile) {
	fmt.Fprintln(w)
	// Sort by harness id for stable output.
	ids := make([]string, 0, len(f.Harnesses))
	for id := range f.Harnesses {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		h := f.Harnesses[id]
		for _, in := range h.Installs {
			pkg := in.Package
			if pkg == "" {
				pkg = in.Method
			}
			fmt.Fprintf(w, "  %-14s %-22s %-10s %s\n", id, pkg, in.Version, in.Status)
		}
	}
}

// printDrift renders the +/-/~ deltas.
func printDrift(w interface{ Write([]byte) (int, error) }, d state.Drift) {
	for _, x := range d.Added {
		fmt.Fprintf(w, "  + added   %-14s %-22s %s\n", x.HarnessID, methodOrPkg(x), x.NewVersion)
	}
	for _, x := range d.Removed {
		fmt.Fprintf(w, "  - removed %-14s %-22s %s\n", x.HarnessID, methodOrPkg(x), x.OldVersion)
	}
	for _, x := range d.Changed {
		fmt.Fprintf(w, "  ~ changed %-14s %-22s %s -> %s\n", x.HarnessID, methodOrPkg(x), x.OldVersion, x.NewVersion)
	}
}

func methodOrPkg(x state.Delta) string {
	if x.Package != "" {
		return x.Package
	}
	return x.Method
}
