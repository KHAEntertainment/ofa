package detect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ofa/internal/catalog"
)

// Scan joins the catalog with what is installed, returning one Instance per
// detected install with active/shadowed resolved.
func Scan(harnesses []catalog.Harness, run Runner) ([]Instance, error) {
	regs := map[string]registryInstalled{}
	for _, method := range registryMethods {
		if !usesRegistryMethod(harnesses, method) {
			continue
		}
		reg, err := detectRegistry(method, run)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		regs[method] = reg
	}

	instances, err := buildInstances(harnesses, regs, run)
	if err != nil {
		return nil, err
	}
	resolveShadowing(instances)
	return instances, nil
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
