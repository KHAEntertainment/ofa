package doctor

import (
	"os/exec"
	"sort"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// Gap is one catalog binary that is installed on PATH but detected nowhere:
// some catalog entry names it as a bin, exec.LookPath finds it, and the scan
// produced no instance for it. That is the signal doctor exists for — OFA
// knows about the harness but cannot see the install, usually because the
// detection method no longer matches how the binary is installed.
type Gap struct {
	Bin string
	// HarnessIDs are the catalog entries that declare Bin, sorted and
	// deduplicated (several entries can share one bin name).
	HarnessIDs []string
}

// Gaps computes the gap list from the catalog and one scan's instances.
// Pure over its inputs apart from lookPath — the same catalog, instances and
// PATH resolution always yield the same gaps — so it is unit-testable and
// cheap. lookPath may be nil for exec.LookPath.
//
// A bin that is detected through any install is not a gap even when another
// catalog entry for the same bin fails to detect: the binary is seen, and
// doctor's job is the unseen ones. A bin that is not on PATH is not a gap
// either: nothing is installed, so there is nothing to detect.
func Gaps(cat []catalog.Harness, insts []detect.Instance, lookPath func(string) (string, error)) []Gap {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	detected := make(map[string]bool, len(insts))
	for _, in := range insts {
		if in.Bin != "" {
			detected[in.Bin] = true
		}
	}
	// One entry per undetected bin, remembering every harness that names it
	// so a shared bin reports all of them.
	ids := map[string]map[string]bool{}
	var order []string
	for _, h := range cat {
		for _, in := range h.Installs {
			if in.Bin == "" || detected[in.Bin] {
				continue
			}
			if _, ok := ids[in.Bin]; !ok {
				ids[in.Bin] = map[string]bool{}
				order = append(order, in.Bin)
			}
			ids[in.Bin][h.ID] = true
		}
	}

	var out []Gap
	for _, bin := range order {
		if _, err := lookPath(bin); err != nil {
			continue
		}
		harnessIDs := make([]string, 0, len(ids[bin]))
		for id := range ids[bin] {
			harnessIDs = append(harnessIDs, id)
		}
		sort.Strings(harnessIDs)
		out = append(out, Gap{Bin: bin, HarnessIDs: harnessIDs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bin < out[j].Bin })
	return out
}
