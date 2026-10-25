package detect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ofa/internal/catalog"
)

// Failure records a registry detector that could not run, and why. A failure
// degrades the scan instead of aborting it: other methods still report, and
// the harnesses this method would have covered are surfaced as undetectable.
type Failure struct {
	Method string
	Err    error
}

// Error renders the failure as "method: reason".
func (f Failure) Error() string { return f.Method + ": " + f.Err.Error() }

// Scan joins the catalog with what is installed, returning one Instance per
// detected install with active/shadowed resolved, plus one Failure for each
// registry method that exists but could not be queried.
//
// A detector that is present-but-broken (e.g. `brew list` exiting non-zero)
// is recorded as a Failure and the loop continues, so one broken package
// manager can no longer zero the whole scan. A package manager that is simply
// not installed is an empty set, not a failure (see detectRegistry).
//
// A non-nil error is reserved for faults that leave nothing to report at all.
func Scan(harnesses []catalog.Harness, run Runner) ([]Instance, []Failure, error) {
	regs := map[string]registryInstalled{}
	var failures []Failure
	for _, method := range registryMethods {
		if !usesRegistryMethod(harnesses, method) {
			continue
		}
		reg, err := detectRegistry(method, run)
		if err != nil {
			failures = append(failures, Failure{Method: method, Err: err})
			continue
		}
		regs[method] = reg
	}

	instances, err := buildInstances(harnesses, regs, run)
	if err != nil {
		return nil, failures, err
	}
	resolveShadowing(instances)
	return instances, failures, nil
}

func usesRegistryMethod(harnesses []catalog.Harness, method string) bool {
	for _, h := range harnesses {
		for _, in := range h.Installs {
			if in.Method == method {
				return true
			}
		}
	}
	return false
}

func buildInstances(harnesses []catalog.Harness, regs map[string]registryInstalled, run Runner) ([]Instance, error) {
	var out []Instance
	for _, h := range harnesses {
		for _, in := range h.Installs {
			switch in.Method {
			case "native":
				inst, ok, err := detectNative(h, in, run)
				if err != nil {
					return nil, err
				}
				if ok {
					out = append(out, *inst)
				}
			default:
				reg, ok := regs[in.Method]
				if !ok {
					continue
				}
				ver, found := reg[in.Package]
				if !found {
					continue
				}
				// A self-updater rewrites its binary outside the package
				// manager's knowledge — `copilot update` replaced the
				// binary while npm's tree still recorded 1.0.80, so the
				// registry value was wrong forever (issue #3). For those
				// installs ask the binary itself; every other tier keeps
				// the registry value, which is correct because the package
				// manager performed the install. A failing version command
				// falls back rather than losing the install.
				if in.Latest == "self" {
					if v, ok := runVersionCmd(in.Version, run); ok {
						ver = v
					}
				}
				out = append(out, Instance{
					HarnessID: h.ID,
					Method:    in.Method,
					Package:   in.Package,
					Bin:       in.Bin,
					Version:   ver,
					Status:    Active,
				})
			}
		}
	}
	return out, nil
}

// runVersionCmd runs an install's declared version command and returns its
// trimmed output. The catalog carries `version.cmd` for every install
// (copilot, mmx, kimi, …) but it was only ever executed for method =
// "native", so the field read as active config while doing nothing for the
// other seven entries. Self-tier installs need it: their package manager's
// record cannot be trusted, because the harness updates its own binary.
//
// Returns false when no command is declared or it fails, so callers keep the
// registry value rather than losing the install entirely.
func runVersionCmd(v catalog.CmdSpec, run Runner) (string, bool) {
	if len(v.Cmd) == 0 {
		return "", false
	}
	out, err := run(v.Cmd[0], v.Cmd[1:]...)
	if err != nil {
		return "", false
	}
	// Only the first line is the version. Some CLIs append a hint after it —
	// `copilot --version` prints "Run 'copilot update' to check for updates."
	// as line two — and printUpdateRow renders one resolution per line, so a
	// bare TrimSpace would embed a newline and split the row in half.
	line, _, _ := strings.Cut(out, "\n")
	if s := strings.TrimSpace(line); s != "" {
		return s, true
	}
	return "", false
}

func detectNative(h catalog.Harness, in catalog.Install, run Runner) (*Instance, bool, error) {
	for _, p := range in.Paths {
		expanded := expandHome(p)
		if _, err := os.Stat(expanded); err != nil {
			continue
		}
		out, err := run(expanded, in.Version.Cmd[1:]...)
		if err != nil {
			// binary exists but --version failed; still report it, version empty
			return &Instance{
				HarnessID: h.ID, Method: in.Method, Bin: in.Bin,
				Path: expanded, Version: "", Status: Active,
			}, true, nil
		}
		return &Instance{
			HarnessID: h.ID, Method: in.Method, Bin: in.Bin,
			Path: expanded, Version: strings.TrimSpace(out), Status: Active,
		}, true, nil
	}
	return nil, false, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// liveBin is the on-PATH binary and its resolved real path for a bin name.
type liveBin struct {
	path string
	real string
}

// resolveShadowing resolves active vs shadowed for registry-backed instances
// that share a bin name. Native instances already carry a concrete path.
func resolveShadowing(instances []Instance) {
	live := map[string]liveBin{}
	for i := range instances {
		if instances[i].Method == "native" {
			continue
		}
		if _, ok := live[instances[i].Bin]; ok {
			continue
		}
		live[instances[i].Bin] = lookUp(instances[i].Bin)
	}
	classify(instances, live)
}

func lookUp(bin string) liveBin {
	lb := liveBin{}
	if p, err := exec.LookPath(bin); err == nil {
		lb.path = p
		lb.real = p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			lb.real = r
		}
	}
	return lb
}

// classify marks active/shadowed using a live-path map (pure, testable).
func classify(instances []Instance, live map[string]liveBin) {
	byBin := map[string][]int{}
	for i := range instances {
		if instances[i].Method == "native" {
			continue
		}
		byBin[instances[i].Bin] = append(byBin[instances[i].Bin], i)
	}
	for bin, idxs := range byBin {
		lb := live[bin]
		if len(idxs) == 1 {
			instances[idxs[0]].Path = lb.path
			instances[idxs[0]].Status = Active
			continue
		}
		owner := -1
		for _, i := range idxs {
			if ownsReal(instances[i], lb.real) {
				owner = i
				break
			}
		}
		if owner < 0 {
			continue // cannot determine: leave all active, no shadowing claim
		}
		for _, i := range idxs {
			if i == owner {
				instances[i].Status = Active
				instances[i].Path = lb.path
			} else {
				instances[i].Status = Shadowed
				instances[i].Path = ""
			}
		}
	}
}

func ownsReal(inst Instance, real string) bool {
	if real == "" {
		return false
	}
	switch inst.Method {
	case "npm":
		return strings.Contains(real, "/node_modules/"+inst.Package+"/")
	case "brew-formula":
		return strings.Contains(real, "/Cellar/"+inst.Package+"/")
	case "brew-cask":
		return strings.Contains(real, "/Caskroom/"+inst.Package+"/")
	case "pipx":
		return strings.Contains(real, "/pipx/venvs/"+inst.Package+"/")
	case "uv":
		return strings.Contains(real, "/uv/tools/"+inst.Package+"/")
	}
	return false
}
