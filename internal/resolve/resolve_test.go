package resolve

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// fixtureBrewJSON is the canned output of `brew outdated --json` for the
// three packages we want to assert on. Two formulae (qwen-code, opencode)
// and one cask (codex).
func fixtureBrewJSON() string {
	d := brewOutdated{
		Formulae: []brewEntry{
			{Name: "qwen-code", InstalledVersions: []string{"0.24.6"}, CurrentVersion: "0.25.1"},
			{Name: "opencode", InstalledVersions: []string{"1.19.0"}, CurrentVersion: "1.19.1"},
		},
		Casks: []brewEntry{
			{Name: "codex", InstalledVersions: []string{"0.162.1"}, CurrentVersion: "0.163.0"},
		},
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	return string(b)
}

// fixtureBrewJSONEmpty is brew outdated for a fully-up-to-date machine —
// used by TestResolve_BrewAbsentIsClean to assert that absent packages
// stay Latest="" and Outdated=false.
func fixtureBrewJSONEmpty() string {
	return `{"formulae": [], "casks": []}`
}

// stubRunner answers the small set of commands the resolvers issue. Any
// other command errors loudly so a missing stub is obvious in test output.
func stubRunner(brewJSON string) Runner {
	return func(name string, args ...string) (string, error) {
		switch name {
		case "brew":
			if len(args) >= 2 && args[0] == "outdated" && args[1] == "--json" {
				return brewJSON, nil
			}
			return "", fmt.Errorf("stubRunner: unsupported brew args %v", args)
		case "npm":
			if len(args) >= 3 && args[0] == "view" && args[2] == "version" {
				switch args[1] {
				case "@earendil-works/pi-coding-agent":
					return "1.3.0\n", nil
				case "@kilocode/cli":
					return "7.8.12\n", nil
				case "opencode-ai":
					return "1.19.1\n", nil
				case "@opencode-ai/cli":
					return "0.5.2\n", nil
				case "@qwen-code/qwen-code":
					return "0.22.5\n", nil
				case "mmx-cli":
					return "1.0.30\n", nil
				case "@github/copilot":
					return "1.0.95\n", nil
				}
				return "", fmt.Errorf("stubRunner: unknown npm pkg %q", args[1])
			}
			return "", fmt.Errorf("stubRunner: unsupported npm args %v", args)
		}
		return "", fmt.Errorf("stubRunner: unsupported command %s %v", name, args)
	}
}

// stubGetter routes canned answers by registry — npm package URLs get an
// npm document (versions keyed by package, matching stubRunner's npm
// view answers), PyPI URLs get pypiJSON. Unknown URLs error loudly so a
// mis-routed lookup is obvious in test output.
func stubGetter(pypiVersion string) Getter {
	npm := map[string]string{
		"@earendil-works/pi-coding-agent": "1.3.0",
		"@kilocode/cli":                   "7.8.12",
		"opencode-ai":                     "1.19.1",
		"@opencode-ai/cli":                "0.5.2",
		"@qwen-code/qwen-code":            "0.22.5",
		"mmx-cli":                         "1.0.30",
		"@github/copilot":                 "1.0.95",
	}
	return func(url string) ([]byte, error) {
		if strings.Contains(url, "registry.npmjs.org") {
			rest := strings.TrimPrefix(url, "https://registry.npmjs.org/")
			rest = strings.TrimSuffix(rest, "/latest")
			pkg := strings.ReplaceAll(rest, "%2F", "/")
			v, ok := npm[pkg]
			if !ok {
				return nil, fmt.Errorf("stubGetter: unknown npm pkg %q (url %s)", pkg, url)
			}
			return []byte(fmt.Sprintf(`{"name": %q, "version": %q}`, pkg, v)), nil
		}
		if strings.Contains(url, "pypi.org") {
			return []byte(pypiJSON(pypiVersion)), nil
		}
		return nil, fmt.Errorf("stubGetter: unexpected URL %s", url)
	}
}

func pypiJSON(version string) string {
	return fmt.Sprintf(`{"info": {"version": %q}, "releases": {}}`, version)
}

// catalogFixtures returns one catalog entry per (tier, behavior) pair we
// care about. Keeping the catalog minimal here so each test is self-
// contained — the production catalog is exercised by the integration test.
func catalogFixtures() []catalog.Harness {
	brewInstall := func(pkg, bin string) catalog.Install {
		return catalog.Install{
			Method:  "brew-formula",
			Package: pkg,
			Bin:     bin,
			Latest:  "brew",
			Version: catalog.CmdSpec{Cmd: []string{bin, "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"brew", "upgrade", pkg}},
		}
	}
	caskInstall := func(pkg string) catalog.Install {
		return catalog.Install{
			Method:  "brew-cask",
			Package: pkg,
			Bin:     pkg,
			Latest:  "brew",
			Version: catalog.CmdSpec{Cmd: []string{pkg, "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"brew", "upgrade", "--cask", pkg}},
		}
	}
	npmInstall := func(pkg, bin string) catalog.Install {
		return catalog.Install{
			Method:  "npm",
			Package: pkg,
			Bin:     bin,
			Latest:  "npm",
			Version: catalog.CmdSpec{Cmd: []string{bin, "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"npm", "install", "-g", pkg + "@latest"}},
		}
	}
	return []catalog.Harness{
		{ID: "codex", Name: "Codex CLI", Installs: []catalog.Install{caskInstall("codex")}},
		{ID: "qwen-brew", Name: "Qwen", Installs: []catalog.Install{brewInstall("qwen-code", "qwen")}},
		{ID: "opencode-brew", Name: "opencode", Installs: []catalog.Install{brewInstall("opencode", "opencode")}},
		{ID: "opencode-npm", Name: "opencode", Installs: []catalog.Install{npmInstall("opencode-ai", "opencode")}},
		{ID: "opencode2", Name: "opencode beta", Installs: []catalog.Install{npmInstall("@opencode-ai/cli", "opencode2")}},
		{ID: "pi", Name: "Pi", Installs: []catalog.Install{npmInstall("@earendil-works/pi-coding-agent", "pi")}},
		{ID: "pi-clean", Name: "Pi", Installs: []catalog.Install{npmInstall("@earendil-works/pi-coding-agent", "pi")}},
		{ID: "kilocode", Name: "Kilo Code", Installs: []catalog.Install{npmInstall("@kilocode/cli", "kilocode")}},
		{ID: "qwen-npm", Name: "Qwen", Installs: []catalog.Install{npmInstall("@qwen-code/qwen-code", "qwen")}},
		{ID: "kimi-uv", Name: "Kimi", Installs: []catalog.Install{
			{Method: "uv", Package: "kimi-cli", Bin: "kimi-cli", Latest: "pypi",
				Version: catalog.CmdSpec{Cmd: []string{"kimi-cli", "--version"}},
				Update:  catalog.CmdSpec{Cmd: []string{"uv", "tool", "upgrade", "kimi-cli"}}},
		}},
		{ID: "claude", Name: "Claude", Installs: []catalog.Install{
			{Method: "native", Bin: "claude", Paths: []string{"~/.local/bin/claude"}, Latest: "self",
				Version: catalog.CmdSpec{Cmd: []string{"claude", "--version"}},
				Update:  catalog.CmdSpec{Cmd: []string{"claude", "update"}}},
		}},
		{ID: "no-update", Name: "Report-only", Installs: []catalog.Install{
			{Method: "native", Bin: "no-update-bin", Latest: "none",
				Version: catalog.CmdSpec{Cmd: []string{"no-update-bin", "--version"}}},
		}},
		{ID: "mmx", Name: "MiniMax", Installs: []catalog.Install{
			{Method: "npm", Package: "mmx-cli", Bin: "mmx", Latest: "self",
				Version: catalog.CmdSpec{Cmd: []string{"mmx", "--version"}},
				Update:  catalog.CmdSpec{Cmd: []string{"mmx", "update"}}},
		}},
		{ID: "copilot", Name: "Copilot", Installs: []catalog.Install{
			{Method: "npm", Package: "@github/copilot", Bin: "copilot", Latest: "self",
				Version: catalog.CmdSpec{Cmd: []string{"copilot", "--version"}},
				Update:  catalog.CmdSpec{Cmd: []string{"copilot", "update"}}},
		}},
	}
}

func TestResolve(t *testing.T) {
	cat := catalogFixtures()
	run := stubRunner(fixtureBrewJSON())
	get := stubGetter("1.52.0")

	instances := []detect.Instance{
		// brew outdated: real cur → latest pairs
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1"},
		{HarnessID: "qwen-brew", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Version: "0.24.6"},
		{HarnessID: "opencode-brew", Method: "brew-formula", Package: "opencode", Bin: "opencode", Version: "1.19.0"},
		// npm: real cur → latest (outdated + at-latest)
		{HarnessID: "opencode-npm", Method: "npm", Package: "opencode-ai", Bin: "opencode", Version: "1.18.35"},
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.2.0"},
		{HarnessID: "pi-clean", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.3.0"},
		{HarnessID: "kilocode", Method: "npm", Package: "@kilocode/cli", Bin: "kilocode", Version: "7.7.6"},
		{HarnessID: "opencode2", Method: "npm", Package: "@opencode-ai/cli", Bin: "opencode2", Version: "0.5.0"},
		{HarnessID: "qwen-npm", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1"},
		// pypi: real cur → latest
		{HarnessID: "kimi-uv", Method: "uv", Package: "kimi-cli", Bin: "kimi-cli", Version: "1.49.0"},
		// self: no diff, "updater available"
		{HarnessID: "claude", Method: "native", Bin: "claude", Path: "/Users/bbrenner/.local/bin/claude", Version: "2.1.300"},
		{HarnessID: "mmx", Method: "npm", Package: "mmx-cli", Bin: "mmx", Version: "1.0.25"},
		{HarnessID: "copilot", Method: "npm", Package: "@github/copilot", Bin: "copilot", Version: "1.0.95"},
		// none: report-only
		{HarnessID: "no-update", Method: "native", Bin: "no-update-bin", Version: "0.1.0"},
	}

	got := Resolve(instances, cat, run, get, Config{})
	byID := map[string]Resolution{}
	for _, r := range got {
		byID[r.HarnessID] = r
	}

	checks := []struct {
		id   string
		want Resolution
	}{
		// brew: real diff when outdated
		{"codex", Resolution{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Cur: "0.162.1", Latest: "0.163.0", Outdated: true, Tier: "brew"}},
		{"qwen-brew", Resolution{HarnessID: "qwen-brew", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Cur: "0.24.6", Latest: "0.25.1", Outdated: true, Tier: "brew"}},
		{"opencode-brew", Resolution{HarnessID: "opencode-brew", Method: "brew-formula", Package: "opencode", Bin: "opencode", Cur: "1.19.0", Latest: "1.19.1", Outdated: true, Tier: "brew"}},
		// npm: real diff always populated
		{"opencode-npm", Resolution{HarnessID: "opencode-npm", Method: "npm", Package: "opencode-ai", Bin: "opencode", Cur: "1.18.35", Latest: "1.19.1", Outdated: true, Tier: "npm"}},
		{"pi", Resolution{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Cur: "1.2.0", Latest: "1.3.0", Outdated: true, Tier: "npm"}},
		{"pi-clean", Resolution{HarnessID: "pi-clean", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Cur: "1.3.0", Latest: "1.3.0", Outdated: false, Tier: "npm"}},
		{"kilocode", Resolution{HarnessID: "kilocode", Method: "npm", Package: "@kilocode/cli", Bin: "kilocode", Cur: "7.7.6", Latest: "7.8.12", Outdated: true, Tier: "npm"}},
		{"opencode2", Resolution{HarnessID: "opencode2", Method: "npm", Package: "@opencode-ai/cli", Bin: "opencode2", Cur: "0.5.0", Latest: "0.5.2", Outdated: true, Tier: "npm"}},
		{"qwen-npm", Resolution{HarnessID: "qwen-npm", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.22.5", Outdated: true, Tier: "npm"}},
		// pypi: real diff
		{"kimi-uv", Resolution{HarnessID: "kimi-uv", Method: "uv", Package: "kimi-cli", Bin: "kimi-cli", Cur: "1.49.0", Latest: "1.52.0", Outdated: true, Tier: "pypi"}},
		// self: no diff, no Latest
		{"claude", Resolution{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"}},
		{"mmx", Resolution{HarnessID: "mmx", Method: "npm", Package: "mmx-cli", Bin: "mmx", Cur: "1.0.25", Tier: "self"}},
		{"copilot", Resolution{HarnessID: "copilot", Method: "npm", Package: "@github/copilot", Bin: "copilot", Cur: "1.0.95", Tier: "self"}},
		// none: report-only
		{"no-update", Resolution{HarnessID: "no-update", Method: "native", Bin: "no-update-bin", Cur: "0.1.0", Tier: "none"}},
	}
	for _, c := range checks {
		r, ok := byID[c.id]
		if !ok {
			t.Errorf("%s: missing from Resolve output", c.id)
			continue
		}
		if r.HarnessID != c.want.HarnessID || r.Method != c.want.Method || r.Package != c.want.Package || r.Bin != c.want.Bin || r.Cur != c.want.Cur || r.Latest != c.want.Latest || r.Outdated != c.want.Outdated || r.Tier != c.want.Tier {
			t.Errorf("%s: got %+v, want %+v", c.id, r, c.want)
		}
		if r.ResolverErr != "" {
			t.Errorf("%s: unexpected ResolverErr %q", c.id, r.ResolverErr)
		}
	}
}

// TestResolve_BrewAbsentIsClean covers the case where brew outdated doesn't
// list a package — brew's authoritative "you are up to date" answer.
func TestResolve_BrewAbsentIsClean(t *testing.T) {
	cat := []catalog.Harness{
		{ID: "qwen-brew", Installs: []catalog.Install{
			{Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Latest: "brew"},
		}},
	}
	run := stubRunner(fixtureBrewJSONEmpty())
	get := stubGetter("0.0.0")
	in := []detect.Instance{
		{HarnessID: "qwen-brew", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Version: "0.25.1"},
	}
	got := Resolve(in, cat, run, get, Config{})
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	r := got[0]
	if r.Latest != "" {
		t.Errorf("absent brew: Latest = %q, want empty", r.Latest)
	}
	if r.Outdated {
		t.Errorf("absent brew: Outdated = true, want false")
	}
	if r.ResolverErr != "" {
		t.Errorf("absent brew: ResolverErr = %q, want empty", r.ResolverErr)
	}
	if r.Tier != "brew" {
		t.Errorf("absent brew: Tier = %q, want brew", r.Tier)
	}
}

func TestResolve_SkipsUnknownInstance(t *testing.T) {
	// An instance whose HarnessID is not in the catalog (shouldn't happen
	// in practice — detect.Scan walks the catalog — but the resolver must
	// not crash if it does).
	in := []detect.Instance{
		{HarnessID: "ghost", Method: "npm", Package: "@ghost/cli", Bin: "ghost", Version: "0.0.1"},
	}
	got := Resolve(in, []catalog.Harness{
		{ID: "real", Name: "Real", Installs: []catalog.Install{{Method: "npm", Package: "@real/cli", Bin: "real", Latest: "self"}}},
	}, stubRunner("{}"), stubGetter("{}"), Config{})
	if len(got) != 0 {
		t.Fatalf("expected zero resolutions for unknown instance, got %d: %+v", len(got), got)
	}
}

func TestResolve_NpmErrorRecorded(t *testing.T) {
	// Registry lookup failure surfaces as ResolverErr; the harness still
	// resolves (Latest stays empty, Outdated stays false) so the rest of the
	// run can proceed. Both lookup paths fail here and no cache exists to
	// fall back to, so the fetch error is a hard ResolverErr.
	run := func(name string, args ...string) (string, error) {
		return "", fmt.Errorf("network down")
	}
	get := func(_ string) ([]byte, error) {
		return nil, fmt.Errorf("network down")
	}
	got := Resolve(
		[]detect.Instance{
			{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.2.0"},
		},
		[]catalog.Harness{
			{ID: "pi", Installs: []catalog.Install{{Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Latest: "npm"}}},
		},
		run,
		get,
		Config{},
	)
	if len(got) != 1 {
		t.Fatalf("want 1 resolution, got %d", len(got))
	}
	if got[0].ResolverErr == "" {
		t.Errorf("expected ResolverErr, got %+v", got[0])
	}
	if got[0].Latest != "" || got[0].Outdated {
		t.Errorf("expected empty Latest / Outdated=false on error, got %+v", got[0])
	}
}

func TestResolve_PypiErrorRecorded(t *testing.T) {
	get := func(_ string) ([]byte, error) {
		return nil, fmt.Errorf("404")
	}
	got := Resolve(
		[]detect.Instance{
			{HarnessID: "kimi-uv", Method: "uv", Package: "kimi-cli", Bin: "kimi-cli", Version: "1.49.0"},
		},
		[]catalog.Harness{
			{ID: "kimi-uv", Installs: []catalog.Install{{Method: "uv", Package: "kimi-cli", Bin: "kimi-cli", Latest: "pypi"}}},
		},
		stubRunner("{}"),
		get,
		Config{},
	)
	if len(got) != 1 || got[0].ResolverErr == "" {
		t.Fatalf("expected 1 resolution with ResolverErr, got %+v", got)
	}
}

func TestParseBrewOutdated(t *testing.T) {
	in := `{
		"formulae": [
			{"name": "qwen-code", "installed_versions": ["0.24.6"], "current_version": "0.25.1"}
		],
		"casks": [
			{"name": "codex", "installed_versions": ["0.162.1"], "current_version": "0.163.0"}
		]
	}`
	r, err := parseBrewOutdated(in)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := r.Latest("qwen-code"); !ok || v != "0.25.1" {
		t.Errorf("qwen-code: got (%q, %v)", v, ok)
	}
	if v, ok := r.Latest("codex"); !ok || v != "0.163.0" {
		t.Errorf("codex: got (%q, %v)", v, ok)
	}
	if _, ok := r.Latest("not-there"); ok {
		t.Errorf("not-there should not be in outdated map")
	}
}

func TestParseBrewOutdated_Malformed(t *testing.T) {
	if _, err := parseBrewOutdated("{not json"); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		// semver
		{"1.18.35", "1.19.0", true},
		{"1.19.0", "1.18.35", false},
		{"0.162.1", "0.163.0", true},
		// equal
		{"1.2.0", "1.2.0", false},
		// date-shaped (lex compare would say 2026.10.0 < 2026.9.1; we want true)
		{"2026.09.18", "2026.10.0", true},
		{"2026.10.0", "2026.09.18", false},
		// brew revision suffix: same version, different revision → not newer
		{"4.0.0_1", "4.0.0", false},
		{"4.0.0", "4.0.0_1", false},
		{"4.0.0_1", "4.0.1", true},
		// padded zeros: 1.2 → 1.2.0 is not newer
		{"1.2", "1.2.0", false},
		{"1.2", "1.3", true},
		// unparseable: conservative no
		{"foo", "1.0", false},
		{"", "1.0", false},
		{"1.0", "", false},
	}
	for _, c := range cases {
		if got := isNewer(c.cur, c.latest); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestFindInstall(t *testing.T) {
	h := catalog.Harness{ID: "opencode", Installs: []catalog.Install{
		{Method: "npm", Package: "opencode-ai", Bin: "opencode"},
		{Method: "npm", Package: "@opencode-ai/cli", Bin: "opencode2"},
		{Method: "brew-formula", Package: "opencode", Bin: "opencode"},
	}}
	cases := []struct {
		inst   detect.Instance
		wantOK bool
		pkg    string
	}{
		{detect.Instance{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode"}, true, "opencode-ai"},
		{detect.Instance{HarnessID: "opencode", Method: "npm", Package: "@opencode-ai/cli", Bin: "opencode2"}, true, "@opencode-ai/cli"},
		{detect.Instance{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode"}, true, "opencode"},
		{detect.Instance{HarnessID: "opencode", Method: "npm", Package: "@opencode-ai/cli", Bin: "opencode"}, false, ""}, // wrong bin
	}
	for _, c := range cases {
		got, ok := findInstall(h, c.inst)
		if ok != c.wantOK {
			t.Errorf("findInstall(%+v) ok = %v, want %v", c.inst, ok, c.wantOK)
			continue
		}
		if ok && got.Package != c.pkg {
			t.Errorf("findInstall(%+v).Package = %q, want %q", c.inst, got.Package, c.pkg)
		}
	}
}

func TestStripBrewRev(t *testing.T) {
	cases := map[string]string{
		"4.0.0_1":  "4.0.0",
		"4.0.0":    "4.0.0",
		"4.0.0_12": "4.0.0",
		"":         "",
	}
	for in, want := range cases {
		if got := stripBrewRev(in); got != want {
			t.Errorf("stripBrewRev(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolve_EmptyCatalogNoCrash is a defensive guard: an empty catalog
// must produce an empty result, not panic on the byID lookup.
func TestResolve_EmptyCatalogNoCrash(t *testing.T) {
	got := Resolve(
		[]detect.Instance{{HarnessID: "x", Method: "native", Bin: "x", Version: "1"}},
		nil, stubRunner("{}"), stubGetter("{}"), Config{},
	)
	if len(got) != 0 {
		t.Errorf("want empty, got %d", len(got))
	}
}

// TestResolve_BrewOuterErrorRecorded asserts that a brew call that fails
// for any reason (not just "not on PATH") marks all brew installs with
// ResolverErr so the user sees the breakage instead of silent skip.
func TestResolve_BrewOuterErrorRecorded(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		return "", fmt.Errorf("permission denied")
	}
	got := Resolve(
		[]detect.Instance{
			{HarnessID: "qwen-brew", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Version: "0.24.6"},
		},
		[]catalog.Harness{
			{ID: "qwen-brew", Installs: []catalog.Install{{Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Latest: "brew"}}},
		},
		run,
		stubGetter("{}"),
		Config{},
	)
	if len(got) != 1 || got[0].ResolverErr == "" {
		t.Fatalf("expected 1 resolution with ResolverErr, got %+v", got)
	}
}

// TestIsNotFound asserts the local isNotFound heuristic matches brew's
// exec-missing error shape. Kept here rather than reusing detect's because
// the resolvers intentionally don't import the detect package — the
// heuristic is small and self-contained.
func TestIsNotFound(t *testing.T) {
	if isNotFound(nil) {
		t.Error("nil should not be not-found")
	}
	if !isNotFound(fmt.Errorf("exec: \"brew\": executable file not found in $PATH")) {
		t.Error("missing-exec error should be not-found")
	}
	if isNotFound(fmt.Errorf("exit status 1")) {
		t.Error("exit-1 should not be not-found")
	}
}

// Helper kept here so future fixtures can import a single place; currently
// only used by TestIsNotFound but documents the type signature.
var _ = strings.TrimSpace

// ---- issue #8: HTTP npm resolution, registry detection, latest cache ----

// memCache is a thread-safe in-memory Cache for tests.
type memCache struct {
	mu   sync.Mutex
	m    map[string]CacheEntry
	puts []string
}

func newMemCache() *memCache { return &memCache{m: map[string]CacheEntry{}} }

func (c *memCache) Get(key string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	return e, ok
}

func (c *memCache) Put(key string, e CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = e
	c.puts = append(c.puts, key)
}

// failRunner / failGetter assert a path was NOT taken ("no spawn on the
// HTTP path", "no fetch on a cache hit"). t.Errorf is goroutine-safe.
func failRunner(t *testing.T, msg string) Runner {
	return func(name string, args ...string) (string, error) {
		t.Errorf("%s: spawned %s %v", msg, name, args)
		return "", fmt.Errorf("unexpected spawn: %s", name)
	}
}

func failGetter(t *testing.T, msg string) Getter {
	return func(url string) ([]byte, error) {
		t.Errorf("%s: fetched %s", msg, url)
		return nil, fmt.Errorf("unexpected fetch: %s", url)
	}
}

// qwenNpm is the one npm instance the cache/HTTP tests resolve.
func qwenNpm() []detect.Instance {
	return []detect.Instance{
		{HarnessID: "qwen-npm", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1"},
	}
}

// TestResolve_NpmQueriesRegistryOverHTTP pins the issue #8 fast path:
// default registry → one direct GET to the exact package URL, no npm
// process spawn. The env var beats any machine-level .npmrc, keeping the
// test deterministic on developer machines with custom registries.
func TestResolve_NpmQueriesRegistryOverHTTP(t *testing.T) {
	t.Setenv("npm_config_registry", "https://registry.npmjs.org")
	wantURL := "https://registry.npmjs.org/@qwen-code%2Fqwen-code/latest"
	gotURL := ""
	get := func(url string) ([]byte, error) {
		gotURL = url
		return []byte(`{"name":"@qwen-code/qwen-code","version":"9.9.9"}`), nil
	}
	got := Resolve(qwenNpm(), catalogFixtures(),
		failRunner(t, "npm must not spawn on the default registry"), get, Config{})
	if len(got) != 1 {
		t.Fatalf("want 1 resolution, got %d", len(got))
	}
	r := got[0]
	if r.ResolverErr != "" {
		t.Errorf("ResolverErr = %q, want empty", r.ResolverErr)
	}
	if r.Latest != "9.9.9" {
		t.Errorf("Latest = %q, want 9.9.9", r.Latest)
	}
	if !r.Outdated {
		t.Error("Outdated = false, want true (0.22.1 < 9.9.9)")
	}
	if gotURL != wantURL {
		t.Errorf("fetched URL = %q, want %q", gotURL, wantURL)
	}
}

// TestResolve_NpmCustomRegistryFallsBackToSpawn: when npm is configured
// against a mirror/corp registry, OFA must defer to `npm view` — only npm
// can apply that config — and must not hit the public registry over HTTP.
func TestResolve_NpmCustomRegistryFallsBackToSpawn(t *testing.T) {
	t.Setenv("npm_config_registry", "https://npm.corp.example/")
	spawned := false
	run := func(name string, args ...string) (string, error) {
		if name == "npm" && len(args) == 3 && args[0] == "view" && args[2] == "version" {
			spawned = true
			return "4.2.0\n", nil
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	get := func(url string) ([]byte, error) {
		t.Errorf("must not fetch the public registry for a custom registry: %s", url)
		return nil, fmt.Errorf("unexpected fetch")
	}
	got := Resolve(qwenNpm(), catalogFixtures(), run, get, Config{})
	if len(got) != 1 {
		t.Fatalf("want 1 resolution, got %d", len(got))
	}
	if !spawned {
		t.Error("expected npm view spawn for custom registry")
	}
	if got[0].ResolverErr != "" || got[0].Latest != "4.2.0" {
		t.Errorf("got %+v, want Latest=4.2.0 with no error", got[0])
	}
}

// TestResolve_CacheFreshHitSkipsNetwork: a fresh entry answers the
// resolution with zero network traffic and zero writes.
func TestResolve_CacheFreshHitSkipsNetwork(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	c := newMemCache()
	c.m[CacheKey("npm", "@qwen-code/qwen-code")] = CacheEntry{
		Version: "9.9.9", ResolvedAt: now.Add(-time.Hour),
	}
	got := Resolve(qwenNpm(), catalogFixtures(),
		failRunner(t, "no spawn on a fresh cache hit"),
		failGetter(t, "no fetch on a fresh cache hit"),
		Config{Cache: c, Now: func() time.Time { return now }})
	if len(got) != 1 {
		t.Fatalf("want 1 resolution, got %d", len(got))
	}
	r := got[0]
	if r.ResolverErr != "" || r.Latest != "9.9.9" || r.Stale {
		t.Errorf("got %+v, want cached Latest=9.9.9, no error, not stale", r)
	}
	if !r.Outdated {
		t.Error("Outdated = false, want true (cache says 9.9.9)")
	}
	if len(c.puts) != 0 {
		t.Errorf("fresh hit must not write back, puts = %v", c.puts)
	}
}

// TestResolve_CacheExpiredRefetchesAndStores: past the TTL the entry is
// re-queried and the refreshed answer is written back with the run's clock.
func TestResolve_CacheExpiredRefetchesAndStores(t *testing.T) {
	// Pin the default registry so a machine-level custom registry can't
	// flip this test onto the spawn path (failRunner asserts no spawn).
	t.Setenv("npm_config_registry", "https://registry.npmjs.org")
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	key := CacheKey("npm", "@qwen-code/qwen-code")
	c := newMemCache()
	c.m[key] = CacheEntry{Version: "1.0.0", ResolvedAt: now.Add(-DefaultTTL - time.Minute)}
	get := func(url string) ([]byte, error) {
		return []byte(`{"version":"9.9.9"}`), nil
	}
	got := Resolve(qwenNpm(), catalogFixtures(), failRunner(t, "HTTP path must not spawn"), get,
		Config{Cache: c, Now: func() time.Time { return now }})
	if got[0].Latest != "9.9.9" {
		t.Errorf("Latest = %q, want refreshed 9.9.9", got[0].Latest)
	}
	e, ok := c.Get(key)
	if !ok || e.Version != "9.9.9" || !e.ResolvedAt.Equal(now) {
		t.Errorf("cache after refetch = %+v (ok=%v), want 9.9.9 @ %v", e, ok, now)
	}
}

// TestResolve_StaleCacheServedWhenRefreshFails: offline behavior with a
// cache — the stale answer is served, flagged, with the refresh error
// kept for the CLI's stderr warning. ResolverErr stays empty: the
// resolution is usable, not failed.
func TestResolve_StaleCacheServedWhenRefreshFails(t *testing.T) {
	// Pin the default registry: the refresh failure below must come from
	// the getter, not from a registry-config spawn on some dev machine.
	t.Setenv("npm_config_registry", "https://registry.npmjs.org")
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-DefaultTTL - time.Hour)
	key := CacheKey("npm", "@qwen-code/qwen-code")
	c := newMemCache()
	c.m[key] = CacheEntry{Version: "8.8.8", ResolvedAt: cachedAt}
	get := func(url string) ([]byte, error) { return nil, fmt.Errorf("network down") }
	got := Resolve(qwenNpm(), catalogFixtures(), failRunner(t, "HTTP path must not spawn"), get,
		Config{Cache: c, Now: func() time.Time { return now }})
	if len(got) != 1 {
		t.Fatalf("want 1 resolution, got %d", len(got))
	}
	r := got[0]
	if r.ResolverErr != "" {
		t.Errorf("ResolverErr = %q, want empty (stale answer is usable)", r.ResolverErr)
	}
	if !r.Stale || r.Latest != "8.8.8" {
		t.Errorf("got %+v, want Stale=true Latest=8.8.8", r)
	}
	if !r.CachedAt.Equal(cachedAt) {
		t.Errorf("CachedAt = %v, want %v", r.CachedAt, cachedAt)
	}
	if !strings.Contains(r.RefreshErr, "network down") {
		t.Errorf("RefreshErr = %q, want the refresh failure", r.RefreshErr)
	}
	if !r.Outdated {
		t.Error("Outdated = false, want true (stale 8.8.8 > 0.22.1)")
	}
	if len(c.puts) != 0 {
		t.Errorf("failed refresh must not overwrite the cache, puts = %v", c.puts)
	}
}

// TestResolve_RefreshBypassesFreshCache: --ignore-the-cache still reads
// through on failure but always queries, and stores the fresh answer.
func TestResolve_RefreshBypassesFreshCache(t *testing.T) {
	// Pin the default registry so --refresh queries over HTTP (the fetch
	// below) rather than spawning on a custom-registry machine.
	t.Setenv("npm_config_registry", "https://registry.npmjs.org")
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	key := CacheKey("npm", "@qwen-code/qwen-code")
	c := newMemCache()
	c.m[key] = CacheEntry{Version: "1.0.0", ResolvedAt: now.Add(-time.Minute)}
	fetched := false
	get := func(url string) ([]byte, error) {
		fetched = true
		return []byte(`{"version":"9.9.9"}`), nil
	}
	got := Resolve(qwenNpm(), catalogFixtures(), failRunner(t, "HTTP path must not spawn"), get,
		Config{Cache: c, Refresh: true, Now: func() time.Time { return now }})
	if !fetched {
		t.Fatal("Refresh must query even for a fresh entry")
	}
	if got[0].Latest != "9.9.9" {
		t.Errorf("Latest = %q, want fetched 9.9.9", got[0].Latest)
	}
	if e, _ := c.Get(key); e.Version != "9.9.9" {
		t.Errorf("cache = %+v, want refreshed 9.9.9", e)
	}
}

// TestResolve_PrefetchBrewIsConsumedExactlyOnce: a PrefetchBrew future
// passed through Config is the only `brew outdated` query — Resolve must
// not start a second one.
func TestResolve_PrefetchBrewIsConsumedExactlyOnce(t *testing.T) {
	brewCalls := 0
	run := func(name string, args ...string) (string, error) {
		if name == "brew" && len(args) >= 2 && args[0] == "outdated" {
			brewCalls++
			return fixtureBrewJSON(), nil
		}
		return "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	in := []detect.Instance{
		{HarnessID: "qwen-brew", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Version: "0.24.6"},
	}
	got := Resolve(in, catalogFixtures(), run, stubGetter("0.0.0"),
		Config{Prefetch: PrefetchBrew(run)})
	if len(got) != 1 || got[0].Latest != "0.25.1" {
		t.Fatalf("got %+v, want qwen-brew latest 0.25.1", got)
	}
	if brewCalls != 1 {
		t.Errorf("brew outdated called %d times, want exactly 1", brewCalls)
	}
}

// TestClassifyRegistry covers the default-vs-custom decision.
func TestClassifyRegistry(t *testing.T) {
	cases := []struct {
		in     string
		custom bool
	}{
		{"https://registry.npmjs.org", false},
		{"https://registry.npmjs.org/", false},
		{"https://npm.corp.example", true},
		{"https://npm.corp.example/", true},
		{"", false},
	}
	for _, c := range cases {
		got := classifyRegistry(c.in)
		if got.Custom != c.custom {
			t.Errorf("classifyRegistry(%q).Custom = %v, want %v", c.in, got.Custom, c.custom)
		}
	}
}

// TestNpmrcRegistryParsing: comment lines skipped, registry line parsed
// with optional spaces and quotes, missing file is not an error.
func TestNpmrcRegistryParsing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.npmrc"
	body := "# a comment\nregistry = https://mirror.example/\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	v, ok := npmrcRegistry(path)
	if !ok || v != "https://mirror.example/" {
		t.Errorf("npmrcRegistry = (%q, %v), want (https://mirror.example/, true)", v, ok)
	}
	if _, ok := npmrcRegistry(dir + "/missing"); ok {
		t.Error("missing file should not report a registry")
	}
}
