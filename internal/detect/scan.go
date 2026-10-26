package detect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"ofa/internal/catalog"
)

// detectConcurrency bounds the parallel registry sweeps and per-install
// version probes (issue #8). Eight matches resolve's cap: enough to keep
// every subprocess in flight on a typical machine without a burst.
const detectConcurrency = 8

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
//
// Registry sweeps run concurrently (issue #8): the package-manager
// queries are independent, so the stage costs the slowest sweep instead
// of their sum. Failures are collected in registryMethods order, so
// reports stay deterministic regardless of completion order.
func Scan(harnesses []catalog.Harness, run Runner) ([]Instance, []Failure, error) {
	methods := make([]string, 0, len(registryMethods))
	for _, m := range registryMethods {
		if usesRegistryMethod(harnesses, m) {
			methods = append(methods, m)
		}
	}

	type regResult struct {
		reg registryInstalled
		err error
	}
	results := make([]regResult, len(methods))
	var wg sync.WaitGroup
	sem := make(chan struct{}, detectConcurrency)
	for i, method := range methods {
		wg.Add(1)
		go func(i int, method string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			reg, err := detectRegistry(method, run)
			results[i] = regResult{reg: reg, err: err}
		}(i, method)
	}
	wg.Wait()

	regs := map[string]registryInstalled{}
	var failures []Failure
	for i, method := range methods {
		if results[i].err != nil {
			failures = append(failures, Failure{Method: method, Err: results[i].err})
			continue
		}
		regs[method] = results[i].reg
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
	// One job per (harness, install) pair, evaluated concurrently — this
	// is where the per-binary `--version` spawns live (issue #8). Slots
	// keep catalog order, so output (and any first error) is deterministic.
	type slot struct {
		inst *Instance
		err  error
	}
	n := 0
	for _, h := range harnesses {
		n += len(h.Installs)
	}
	slots := make([]slot, n)
	var wg sync.WaitGroup
	sem := make(chan struct{}, detectConcurrency)
	i := 0
	for _, h := range harnesses {
		for _, in := range h.Installs {
			wg.Add(1)
			go func(i int, h catalog.Harness, in catalog.Install) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				inst, _, err := instanceFor(h, in, regs, run)
				slots[i] = slot{inst: inst, err: err}
			}(i, h, in)
			i++
		}
	}
	wg.Wait()

	var out []Instance
	for _, s := range slots {
		if s.err != nil {
			return nil, s.err
		}
		if s.inst != nil {
			out = append(out, *s.inst)
		}
	}
	return out, nil
}

// instanceFor computes one (harness, install) instance — the body of the
// old serial buildInstances loop, unchanged in behavior: native installs
// probe their binary, registry installs take the package manager's value,
// and self-tier installs ask the binary itself because its updater
// rewrites it behind the registry's back (issue #3: `copilot update`
// replaced the binary while npm still recorded the old version). A
// failing version command falls back rather than losing the install.
func instanceFor(h catalog.Harness, in catalog.Install, regs map[string]registryInstalled, run Runner) (*Instance, bool, error) {
	switch in.Method {
	case "native":
		return detectNative(h, in, run)
	default:
		reg, ok := regs[in.Method]
		if !ok {
			return nil, false, nil
		}
		ver, found := reg[in.Package]
		if !found {
			return nil, false, nil
		}
		if in.Latest == "self" {
			if v, ok := runVersionCmd(in.Version, run); ok {
				ver = v
			}
		}
		return &Instance{
			HarnessID: h.ID,
			Method:    in.Method,
			Package:   in.Package,
			Bin:       in.Bin,
			Version:   ver,
			Status:    Active,
		}, true, nil
	}
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
		return versionToken(s), true
	}
	return "", false
}

// versionToken extracts the version from a harness's raw `--version` output,
// which is frequently decorated:
//
//	"GitHub Copilot CLI 1.0.103."   -> "1.0.103"
//	"devin 3000.11.92 (7f4e6498)"   -> "3000.11.92"
//	"claude 2.1.310 (Claude Code)"  -> "2.1.310"
//
// A single token that already starts with a digit is kept whole, so
// date-shaped and hash-suffixed versions survive intact
// ("2026.10.22-27dcf5f"). Otherwise the first whitespace-separated token that
// starts with a digit wins, with trailing sentence punctuation trimmed.
// Anything without such a token is returned trimmed but otherwise untouched:
// showing what the tool actually printed beats inventing a version.
func versionToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.ContainsAny(s, " \t") && startsDigit(s) {
		return strings.TrimRight(s, ".")
	}
	for _, f := range strings.Fields(s) {
		if startsDigit(f) {
			return strings.TrimRight(f, ".,;:")
		}
	}
	return s
}

func startsDigit(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
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
			Path: expanded, Version: versionToken(out), Status: Active,
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

// resolveShadowing resolves active vs shadowed for every instance that
// shares a bin name, including native installs. Native paths are probed from
// the catalog rather than a registry, but they compete for the same PATH
// entry as an npm or brew copy — leaving them out made a script-installed
// binary look absent while the registry copy was reported as live.
func resolveShadowing(instances []Instance) {
	live := map[string]liveBin{}
	for i := range instances {
		if instances[i].Bin == "" {
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
		if instances[i].Bin == "" {
			continue
		}
		byBin[instances[i].Bin] = append(byBin[instances[i].Bin], i)
	}
	for bin, idxs := range byBin {
		lb := live[bin]
		if len(idxs) == 1 {
			i := idxs[0]
			// A lone native install only owns the live binary if PATH
			// resolves back to its probe path. Otherwise `<bin> update`
			// would run the other executable, so report shadowing and
			// keep the probe path rather than claiming reachability.
			if instances[i].Method == "native" && lb.path != "" && !ownsReal(instances[i], lb.real) {
				instances[i].Status = Shadowed
				continue
			}
			if lb.path != "" {
				instances[i].Path = lb.path
			}
			instances[i].Status = Active
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
				if lb.path != "" {
					instances[i].Path = lb.path
				}
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
	case "native":
		// Native installs carry the probed path instead of a registry
		// package marker, so ownership means "this path is the live one".
		if inst.Path == "" {
			return false
		}
		p := inst.Path
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		return p == real
	}
	return false
}
