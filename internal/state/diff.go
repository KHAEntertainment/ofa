package state

import "sort"

// Delta is one drift entry.
type Delta struct {
	HarnessID  string
	Method     string
	Package    string
	Bin        string
	OldVersion string
	NewVersion string
}

// Drift reports the drift between a previous scan and the current one.
type Drift struct {
	Added   []Delta
	Removed []Delta
	Changed []Delta
}

// Diff computes drift between prev and current. A nil prev (initial scan)
// yields an empty drift.
func Diff(prev, current *File) Drift {
	var d Drift
	if prev == nil {
		return d
	}
	old := flatten(prev)
	cur := flatten(current)

	for key, ci := range cur {
		oi, ok := old[key]
		if !ok {
			d.Added = append(d.Added, Delta{HarnessID: key.harness, Method: ci.Method, Package: ci.Package, Bin: ci.Bin, NewVersion: ci.Version})
		} else if oi.Version != ci.Version {
			d.Changed = append(d.Changed, Delta{HarnessID: key.harness, Method: ci.Method, Package: ci.Package, Bin: ci.Bin, OldVersion: oi.Version, NewVersion: ci.Version})
		}
	}
	for key, oi := range old {
		if _, ok := cur[key]; !ok {
			d.Removed = append(d.Removed, Delta{HarnessID: key.harness, Method: oi.Method, Package: oi.Package, Bin: oi.Bin, OldVersion: oi.Version})
		}
	}
	sortDeltas(d.Added)
	sortDeltas(d.Removed)
	sortDeltas(d.Changed)
	return d
}

type installKey struct {
	harness, method, pkg, bin string
}

func flatten(f *File) map[installKey]Install {
	m := map[installKey]Install{}
	for id, h := range f.Harnesses {
		for _, in := range h.Installs {
			m[installKey{id, in.Method, in.Package, in.Bin}] = in
		}
	}
	return m
}

func sortDeltas(ds []Delta) {
	sort.Slice(ds, func(i, j int) bool {
		a := ds[i].HarnessID + "|" + ds[i].Method + "|" + ds[i].Package
		b := ds[j].HarnessID + "|" + ds[j].Method + "|" + ds[j].Package
		return a < b
	})
}
