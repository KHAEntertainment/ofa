// Package resolve turns detected installs into per-install "cur → latest"
// resolutions. For package-managed installs (brew, npm, pypi) it queries the
// authoritative source for the latest version; for self-updaters it just
// records that an updater is available with no fabricated diff; for
// report-only installs it does the same.
//
// Lookups run concurrently (see Config.Concurrency): the single
// `brew outdated` query starts in its own goroutine — or arrives via
// Config.Prefetch, already overlapping detection — and overlaps the
// per-package npm/PyPI lookups. Results are emitted in input order
// regardless of completion order.
package resolve

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// Resolution is one install's resolved latest version. For self/none tiers
// Latest stays empty and Outdated stays false; the caller renders
// "updater available" / "no update path" using Tier.
type Resolution struct {
	HarnessID   string
	Method      string
	Package     string
	Bin         string
	Cur         string // installed version, may be empty
	Latest      string // empty for self/none, and for brew when not outdated
	Outdated    bool   // true iff Latest != "" and cur differs from latest
	Tier        string // "self" | "brew" | "npm" | "pypi" | "none"
	ResolverErr string // non-empty when latest lookup failed; resolution continues

	// Stale marks a latest version served from cache because a refresh
	// failed. CachedAt is when it was fetched, RefreshErr why the refresh
	// failed. These are deliberately not part of `check --json`: the CLI
	// warns on stderr, and the resolution itself is a usable answer.
	Stale      bool
	CachedAt   time.Time
	RefreshErr string
}

// Runner runs a shell command and returns stdout. Same shape as detect.Runner
// so tests can reuse fixtures.
type Runner func(name string, args ...string) (string, error)

// Getter fetches a URL and returns the body. Pulled out so tests can serve
// canned JSON without hitting PyPI.
type Getter func(url string) ([]byte, error)

// DefaultRunner shells out for real.
var DefaultRunner Runner = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// DefaultGetter is the real HTTP client, with a timeout so one hung
// registry can't stall the whole parallel lookup (issue #8 makes these
// requests run concurrently; an unbounded hang would hold all of them).
var DefaultGetter Getter = func(url string) ([]byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Resolve produces a Resolution per detected install. Tier=brew resolves via
// a single `brew outdated` query shared across installs; tier=npm and
// tier=pypi resolve per-package concurrently; tier=self and tier=none
// short-circuit. Input order is preserved in the output.
func Resolve(insts []detect.Instance, harnesses []catalog.Harness, run Runner, get Getter, cfg Config) []Resolution {
	cfg = cfg.withDefaults()
	byID := map[string]catalog.Harness{}
	for _, h := range harnesses {
		byID[h.ID] = h
	}

	// Classify every instance first (no I/O), so the network phase knows
	// its work list and skipped instances keep their input positions.
	out := make([]*Resolution, len(insts))
	type netJob struct {
		res  *Resolution
		tier string
		pkg  string
	}
	var netJobs []netJob
	var brewIdx []int
	hasNpm := false

	for i := range insts {
		inst := insts[i]
		h, ok := byID[inst.HarnessID]
		if !ok {
			continue
		}
		in, ok := findInstall(h, inst)
		if !ok {
			continue
		}
		res := &Resolution{
			HarnessID: inst.HarnessID,
			Method:    inst.Method,
			Package:   inst.Package,
			Bin:       inst.Bin,
			Cur:       inst.Version,
			Tier:      in.Latest,
		}
		if res.Tier == "" {
			res.Tier = "self" // default for catalog entries that omit it
		}
		out[i] = res
		switch res.Tier {
		case "self", "none":
			// Latest stays empty; Outdated stays false.
		case "brew":
			brewIdx = append(brewIdx, i)
		case "npm":
			hasNpm = true
			netJobs = append(netJobs, netJob{res: res, tier: "npm", pkg: inst.Package})
		case "pypi":
			netJobs = append(netJobs, netJob{res: res, tier: "pypi", pkg: inst.Package})
		default:
			res.ResolverErr = fmt.Sprintf("unknown latest tier %q", res.Tier)
		}
	}

	// One shared `brew outdated`, started now so it overlaps the npm and
	// PyPI lookups below — brew is the slowest single query (seconds).
	var brewF *BrewFuture
	if len(brewIdx) > 0 {
		if cfg.Prefetch != nil {
			brewF = cfg.Prefetch
		} else {
			brewF = PrefetchBrew(run)
		}
	}

	// npm's registry configuration is read once, only when npm lookups
	// will actually happen.
	var npmReg npmRegistry
	if hasNpm {
		npmReg = detectNpmRegistry()
	}

	// Each job mutates exactly its own Resolution, so no result lock is
	// needed — only the semaphore bounds fan-out.
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for _, j := range netJobs {
		wg.Add(1)
		go func(j netJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var fetch func() (string, error)
			if j.tier == "npm" {
				fetch = func() (string, error) { return resolveNpm(j.pkg, run, get, npmReg) }
			} else {
				fetch = func() (string, error) { return resolvePypi(j.pkg, get) }
			}
			latest, stale, cachedAt, err := fetchLatest(cfg, j.tier, j.pkg, fetch)
			switch {
			case err != nil && stale:
				// Refresh failed but a cached answer exists: use it,
				// flagged so the CLI can warn.
				j.res.Latest = latest
				j.res.Outdated = isNewer(j.res.Cur, latest)
				j.res.Stale = true
				j.res.CachedAt = cachedAt
				j.res.RefreshErr = err.Error()
			case err != nil:
				j.res.ResolverErr = err.Error()
			default:
				j.res.Latest = latest
				j.res.Outdated = isNewer(j.res.Cur, latest)
			}
		}(j)
	}
	wg.Wait()

	if len(brewIdx) > 0 {
		r, err := brewF.wait()
		for _, i := range brewIdx {
			res := out[i]
			if err != nil {
				res.ResolverErr = fmt.Sprintf("brew: %v", err)
				continue
			}
			if v, ok := r.Latest(res.Package); ok {
				res.Latest = v
				res.Outdated = isNewer(res.Cur, v)
			}
			// not in outdated list → at latest, Latest stays empty
		}
	}

	compact := make([]Resolution, 0, len(out))
	for _, p := range out {
		if p != nil {
			compact = append(compact, *p)
		}
	}
	return compact
}

// findInstall picks the catalog install block that matches a detected
// instance. Match key is (method, package, bin). Multiple catalog blocks for
// the same harness share none of those — opencode has npm opencode-ai, npm
// @opencode-ai/cli, and brew opencode; each instance lands on exactly one.
func findInstall(h catalog.Harness, inst detect.Instance) (catalog.Install, bool) {
	for _, in := range h.Installs {
		if in.Method != inst.Method {
			continue
		}
		if in.Bin != inst.Bin {
			continue
		}
		// For registry methods, the package disambiguates; for native, it's empty.
		if in.Package != inst.Package {
			continue
		}
		return in, true
	}
	return catalog.Install{}, false
}

// stripBrewRev trims the trailing "_N" revision suffix brew appends to
// installed versions (e.g. "4.0.0_1" → "4.0.0") so equality comparison
// ignores the rebuild counter. Latest from brew is unversioned, so this
// prevents false "outdated" matches.
func stripBrewRev(v string) string {
	if i := strings.Index(v, "_"); i >= 0 {
		return v[:i]
	}
	return v
}
