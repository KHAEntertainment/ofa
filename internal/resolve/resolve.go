// Package resolve turns detected installs into per-install "cur → latest"
// resolutions. For package-managed installs (brew, npm, pypi) it queries the
// authoritative source for the latest version; for self-updaters it just
// records that an updater is available with no fabricated diff; for
// report-only installs it does the same.
package resolve

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// Resolution is one install's resolved latest version. For self/none tiers
// Latest stays empty and Outdated stays false; the caller renders
// "updater available" / "no update path" using Tier.
type Resolution struct {
	HarnessID string
	Method    string
	Package   string
	Bin       string
	Cur       string // installed version, may be empty
	Latest    string // empty for self/none, and for brew when not outdated
	Outdated  bool   // true iff Latest != "" and cur differs from latest
	Tier      string // "self" | "brew" | "npm" | "pypi" | "none"
	ResolverErr string // non-empty when latest lookup failed; resolution continues
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

// DefaultGetter is the real HTTP client.
var DefaultGetter Getter = func(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Resolve produces a Resolution per detected install. Tier=brew resolves via
// a single `brew outdated --json` call shared across installs; tier=npm and
// tier=pypi resolve per-package (cheap for our scale); tier=self and tier=none
// short-circuit.
func Resolve(insts []detect.Instance, harnesses []catalog.Harness, run Runner, get Getter) []Resolution {
	byID := map[string]catalog.Harness{}
	for _, h := range harnesses {
		byID[h.ID] = h
	}

	brew, brewErr := newBrewResolver(run)

	var out []Resolution
	for _, inst := range insts {
		h, ok := byID[inst.HarnessID]
		if !ok {
			continue
		}
		in, ok := findInstall(h, inst)
		if !ok {
			continue
		}
		res := Resolution{
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
		switch res.Tier {
		case "self", "none":
			// Latest stays empty; Outdated stays false.
		case "brew":
			if brewErr != nil {
				res.ResolverErr = fmt.Sprintf("brew: %v", brewErr)
				break
			}
			if v, ok := brew.Latest(inst.Package); ok {
				res.Latest = v
				res.Outdated = isNewer(inst.Version, v)
			}
			// not in outdated list → at latest, Latest stays empty
		case "npm":
			v, err := resolveNpm(inst.Package, run)
			if err != nil {
				res.ResolverErr = err.Error()
				break
			}
			res.Latest = v
			res.Outdated = isNewer(inst.Version, v)
		case "pypi":
			v, err := resolvePypi(inst.Package, get)
			if err != nil {
				res.ResolverErr = err.Error()
				break
			}
			res.Latest = v
			res.Outdated = isNewer(inst.Version, v)
		default:
			res.ResolverErr = fmt.Sprintf("unknown latest tier %q", res.Tier)
		}
		out = append(out, res)
	}
	return out
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