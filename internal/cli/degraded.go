package cli

import (
	"fmt"
	"strings"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// This file holds the degraded-scan behaviour: what OFA prints when a
// detector could not run, and how it keeps a partial scan from writing a
// partial — and therefore wrong — picture into the saved state.
//
// The guiding rule is that a detector failing and a package being absent are
// different facts. The first is "I don't know"; the second is "not there".
// Collapsing them is how a broken `brew` would silently uninstall codex.

// failedMethods indexes the methods whose detectors errored this run.
func failedMethods(failures []detect.Failure) map[string]bool {
	m := make(map[string]bool, len(failures))
	for _, f := range failures {
		m[f.Method] = true
	}
	return m
}

// reportDetectionFailures warns about each detector that could not run and
// names the harnesses that were lost with it, so a degraded scan is visibly
// degraded instead of quietly partial.
//
// Only harnesses that were detected by *no* method are named: one whose other
// install method still answered is reported normally above, and listing it
// here would read as a loss it did not suffer.
func (a *App) reportDetectionFailures(cat []catalog.Harness, insts []detect.Instance, failures []detect.Failure) {
	detected := make(map[string]bool, len(insts))
	for _, i := range insts {
		detected[i.HarnessID] = true
	}

	failed := failedMethods(failures)
	reason := make(map[string]string, len(failures))
	for _, f := range failures {
		fmt.Fprintf(a.Stderr, "ofa: warning: %s detector failed: %v\n", f.Method, f.Err)
		reason[f.Method] = f.Err.Error()
	}

	for _, h := range cat {
		if detected[h.ID] {
			continue
		}
		seen := map[string]bool{}
		for _, in := range h.Installs {
			if !failed[in.Method] || seen[in.Method] {
				continue
			}
			seen[in.Method] = true
			fmt.Fprintf(a.Stderr, "ofa: undetectable: %s [%s]: %s\n",
				h.ID, in.Method, reason[in.Method])
		}
	}
}

// reconcileDegraded carries forward, from prev, every install whose detector
// failed this run and that the healthy detectors did not report.
//
// Without this, a failing package manager is indistinguishable from an
// uninstall: Diff would list its harnesses under Removed and scan would then
// persist that fiction, so the damage would outlive the failure. Installs
// still visible through a healthy method are left alone — that detector's
// answer wins over a remembered one.
//
// prev may be nil (first scan); failures may be empty (healthy scan). Either
// leaves current untouched.
func reconcileDegraded(prev, current *stateFile, failures []detect.Failure) *stateFile {
	if prev == nil || len(failures) == 0 || current == nil {
		return current
	}
	failed := failedMethods(failures)

	// Index what the healthy detectors did report, on the same identity Diff
	// uses, so a remembered install is not duplicated beside a fresh one.
	present := map[[4]string]bool{}
	for id, h := range current.Harnesses {
		for _, in := range h.Installs {
			present[[4]string{id, in.Method, in.Package, in.Bin}] = true
		}
	}

	for id, h := range prev.Harnesses {
		for _, in := range h.Installs {
			if !failed[in.Method] {
				continue
			}
			if present[[4]string{id, in.Method, in.Package, in.Bin}] {
				continue
			}
			cur := current.Harnesses[id]
			cur.Installs = append(cur.Installs, in)
			current.Harnesses[id] = cur
		}
	}
	return current
}

// undetectableIDs lists the harnesses no detector could see this run, for
// callers that need to say so in structured output.
func undetectableIDs(cat []catalog.Harness, insts []detect.Instance, failures []detect.Failure) []string {
	if len(failures) == 0 {
		return nil
	}
	detected := map[string]bool{}
	for _, i := range insts {
		detected[i.HarnessID] = true
	}
	failed := failedMethods(failures)

	var out []string
	for _, h := range cat {
		if detected[h.ID] {
			continue
		}
		for _, in := range h.Installs {
			if failed[in.Method] {
				out = append(out, h.ID)
				break
			}
		}
	}
	return out
}

// failureOut is the JSON shape for one degraded detector.
type failureOut struct {
	Method string `json:"method"`
	Error  string `json:"error"`
}

func failureOutList(failures []detect.Failure) []failureOut {
	if len(failures) == 0 {
		return nil
	}
	out := make([]failureOut, 0, len(failures))
	for _, f := range failures {
		out = append(out, failureOut{Method: f.Method, Error: f.Err.Error()})
	}
	return out
}

// summarizeFailures renders failures as one compact line for stderr footers.
func summarizeFailures(failures []detect.Failure) string {
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		parts = append(parts, f.Method)
	}
	return strings.Join(parts, ", ")
}
