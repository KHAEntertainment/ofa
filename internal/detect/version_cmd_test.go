package detect

import (
	"errors"
	"strings"
	"testing"

	"ofa/internal/catalog"
)

// registryFor builds the map Scan would hand buildInstances from a
// package-manager query.
func registryFor(method, pkg, version string) map[string]registryInstalled {
	return map[string]registryInstalled{method: {pkg: version}}
}

// copilotHarness is the real shape of issue #3: an npm install whose harness
// self-updates out of band, so npm's record is stale but a version command
// exists in the catalog.
func copilotHarness(latest string) []catalog.Harness {
	return []catalog.Harness{{
		ID:   "copilot",
		Name: "GitHub Copilot CLI",
		Installs: []catalog.Install{{
			Method:  "npm",
			Package: "@github/copilot",
			Bin:     "copilot",
			Version: catalog.CmdSpec{Cmd: []string{"copilot", "--version"}},
			Latest:  latest,
		}},
	}}
}

// The reported bug: npm still said 1.0.80 while the binary was 1.0.103.
// A self-updater must report what its binary says.
func TestBuildInstances_SelfTierAsksTheBinary(t *testing.T) {
	regs := registryFor("npm", "@github/copilot", "1.0.80")
	run := func(name string, _ ...string) (string, error) {
		if name == "copilot" {
			return "GitHub Copilot CLI 1.0.103.\n", nil
		}
		return "", errors.New("unexpected command " + name)
	}

	insts, err := buildInstances(copilotHarness("self"), regs, run)
	if err != nil {
		t.Fatalf("buildInstances: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("instances = %d, want 1", len(insts))
	}
	if got, want := insts[0].Version, "GitHub Copilot CLI 1.0.103."; got != want {
		t.Errorf("version = %q, want %q (the binary's answer, not npm's stale record)", got, want)
	}
	if strings.Contains(insts[0].Version, "1.0.80") {
		t.Errorf("still reporting npm's stale 1.0.80: %q", insts[0].Version)
	}
}

// Registry tiers must keep using package-manager metadata: the package
// manager performed the install, so its record is the truth, and running
// version commands for every row would be both slower and noisier
// (`copilot --version` prints "GitHub Copilot CLI 1.0.103." while npm
// reports a clean 1.0.80-style value for npm-tier harnesses).
func TestBuildInstances_RegistryTierKeepsRegistryValue(t *testing.T) {
	regs := registryFor("npm", "@kilocode/cli", "7.8.17")
	run := func(name string, _ ...string) (string, error) {
		// Deliberately different: if this were consulted the test fails.
		return "SHOULD NOT BE USED", nil
	}
	h := []catalog.Harness{{
		ID:   "kilocode",
		Name: "Kilocode",
		Installs: []catalog.Install{{
			Method:  "npm",
			Package: "@kilocode/cli",
			Bin:     "kilocode",
			Version: catalog.CmdSpec{Cmd: []string{"kilocode", "--version"}},
			Latest:  "npm",
		}},
	}}

	insts, err := buildInstances(h, regs, run)
	if err != nil {
		t.Fatalf("buildInstances: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("instances = %d, want 1", len(insts))
	}
	if insts[0].Version != "7.8.17" {
		t.Errorf("version = %q, want the registry value 7.8.17", insts[0].Version)
	}
}

// A failing version command must not lose the install: fall back to whatever
// the package manager knows rather than dropping it from the scan.
func TestBuildInstances_VersionCmdFailureFallsBackToRegistry(t *testing.T) {
	regs := registryFor("npm", "@github/copilot", "1.0.80")
	run := func(name string, _ ...string) (string, error) {
		return "", errors.New("binary missing")
	}

	insts, err := buildInstances(copilotHarness("self"), regs, run)
	if err != nil {
		t.Fatalf("buildInstances: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("instances = %d, want 1 (a failed probe must not drop the install)", len(insts))
	}
	if insts[0].Version != "1.0.80" {
		t.Errorf("version = %q, want registry fallback 1.0.80", insts[0].Version)
	}
}

// No declared version command means nothing to ask.
func TestRunVersionCmd_NoCommandDeclared(t *testing.T) {
	if _, ok := runVersionCmd(catalog.CmdSpec{}, func(string, ...string) (string, error) {
		t.Fatal("runner must not be called when no command is declared")
		return "", nil
	}); ok {
		t.Error("empty CmdSpec should report false")
	}
}

// Empty output is treated as a failed probe, not as version "".
func TestRunVersionCmd_EmptyOutputIsFailure(t *testing.T) {
	v := catalog.CmdSpec{Cmd: []string{"tool", "--version"}}
	if _, ok := runVersionCmd(v, func(string, ...string) (string, error) {
		return "   \n", nil
	}); ok {
		t.Error("blank output should report false so the caller keeps the registry value")
	}
}

// Trailing newlines are trimmed — the version is compared as a string later.
func TestRunVersionCmd_TrimsOutput(t *testing.T) {
	v := catalog.CmdSpec{Cmd: []string{"tool", "--version"}}
	got, ok := runVersionCmd(v, func(string, ...string) (string, error) {
		return "1.2.3\n", nil
	})
	if !ok || got != "1.2.3" {
		t.Errorf("got (%q, %v), want (\"1.2.3\", true)", got, ok)
	}
}

// A CLI that appends a hint after the version must not smuggle a newline into
// the row: printUpdateRow renders one resolution per line, so a second line
// would split copilot's row across the table.
func TestRunVersionCmd_UsesFirstLineOnly(t *testing.T) {
	v := catalog.CmdSpec{Cmd: []string{"copilot", "--version"}}
	out := "GitHub Copilot CLI 1.0.103.\nRun 'copilot update' to check for updates.\n"
	got, ok := runVersionCmd(v, func(string, ...string) (string, error) {
		return out, nil
	})
	if !ok {
		t.Fatal("expected success")
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("version contains a newline: %q", got)
	}
	if got != "GitHub Copilot CLI 1.0.103." {
		t.Errorf("got %q, want only the first line", got)
	}
}
