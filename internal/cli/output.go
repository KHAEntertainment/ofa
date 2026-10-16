package cli

import (
	"sort"

	"ofa/internal/state"
)

// sortStrings is a tiny helper kept here so scan.go / check.go don't have
// to import "sort" themselves. Centralizing means we can swap to a
// locale-aware sort later if we ever care.
func sortStrings(s []string) {
	sort.Strings(s)
}

// driftJSON is the JSON shape for a Drift. Distinct from state.Drift so
// the wire format can evolve without leaking internal struct tags.
type driftJSON struct {
	Added   []deltaJSON `json:"added"`
	Removed []deltaJSON `json:"removed"`
	Changed []deltaJSON `json:"changed"`
}

type deltaJSON struct {
	HarnessID  string `json:"harness_id"`
	Method     string `json:"method"`
	Package    string `json:"package,omitempty"`
	Bin        string `json:"bin"`
	OldVersion string `json:"old_version,omitempty"`
	NewVersion string `json:"new_version,omitempty"`
}

func driftJSONPtr(d state.Drift) *driftJSON {
	if len(d.Added)+len(d.Removed)+len(d.Changed) == 0 {
		return nil
	}
	out := &driftJSON{Added: []deltaJSON{}, Removed: []deltaJSON{}, Changed: []deltaJSON{}}
	for _, x := range d.Added {
		out.Added = append(out.Added, deltaJSON{HarnessID: x.HarnessID, Method: x.Method, Package: x.Package, Bin: x.Bin, NewVersion: x.NewVersion})
	}
	for _, x := range d.Removed {
		out.Removed = append(out.Removed, deltaJSON{HarnessID: x.HarnessID, Method: x.Method, Package: x.Package, Bin: x.Bin, OldVersion: x.OldVersion})
	}
	for _, x := range d.Changed {
		out.Changed = append(out.Changed, deltaJSON{HarnessID: x.HarnessID, Method: x.Method, Package: x.Package, Bin: x.Bin, OldVersion: x.OldVersion, NewVersion: x.NewVersion})
	}
	return out
}