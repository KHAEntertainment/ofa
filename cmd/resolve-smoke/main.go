// Command resolve-smoke runs the resolver against the live machine and
// prints what it would report. It's not part of the `ofa` binary — it's
// a developer-only diagnostic that exercises the resolver end-to-end
// against real brew/npm/pypi.
//
// Build:  go build -tags=integration -o /tmp/resolve-smoke ./cmd/resolve-smoke
// Run:    /tmp/resolve-smoke
package main

import (
	"fmt"
	"os"
	"time"

	"ofa/internal/catalog"
	"ofa/internal/detect"
	"ofa/internal/resolve"
)

func main() {
	cat, err := catalog.Embedded()
	if err != nil {
		fmt.Fprintf(os.Stderr, "catalog: %v\n", err)
		os.Exit(1)
	}
	insts, err := detect.Scan(cat, detect.DefaultRunner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "detect: %v\n", err)
		os.Exit(1)
	}
	t0 := time.Now()
	res := resolve.Resolve(insts, cat, resolve.DefaultRunner, resolve.DefaultGetter)
	fmt.Printf("Resolved %d installs in %s\n\n", len(res), time.Since(t0).Truncate(time.Millisecond))
	for _, r := range res {
		row := fmt.Sprintf("  %-12s %-12s %-22s %-10s", r.HarnessID, r.Method, label(r.Package, r.Bin), r.Tier)
		switch r.Tier {
		case "self":
			row += fmt.Sprintf(" %s — updater available", r.Cur)
		case "none":
			row += fmt.Sprintf(" %s — no update path", r.Cur)
		default:
			if r.Outdated {
				row += fmt.Sprintf(" %s → %s  (outdated)", r.Cur, r.Latest)
			} else if r.Latest != "" {
				row += fmt.Sprintf(" %s (at latest)", r.Cur)
			} else {
				row += fmt.Sprintf(" %s (no resolution)", r.Cur)
			}
		}
		if r.ResolverErr != "" {
			row += "  err=" + r.ResolverErr
		}
		fmt.Println(row)
	}
}

func label(pkg, bin string) string {
	if pkg != "" {
		return pkg
	}
	return bin
}