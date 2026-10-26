package detect

import (
	"os"
	"path/filepath"
	"testing"

	"ofa/internal/catalog"
)

func TestParseKeyVersion(t *testing.T) {
	m := parseKeyVersion("opencode 1.19.0\nqwen-code 0.24.6\n\nfoo 1.0\n")
	if m["opencode"] != "1.19.0" {
		t.Errorf("opencode = %q", m["opencode"])
	}
	if m["qwen-code"] != "0.24.6" {
		t.Errorf("qwen-code = %q", m["qwen-code"])
	}
	if m["foo"] != "1.0" {
		t.Errorf("foo = %q", m["foo"])
	}
}

func TestParseNpmJSON(t *testing.T) {
	out := `{"dependencies":{"opencode-ai":{"version":"1.18.35"},"@opencode-ai/cli":{"version":"0.0.0-beta-18743"}}}`
	m, err := parseNpmJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if m["opencode-ai"] != "1.18.35" {
		t.Errorf("opencode-ai = %q", m["opencode-ai"])
	}
	if m["@opencode-ai/cli"] != "0.0.0-beta-18743" {
		t.Errorf("@opencode-ai/cli = %q", m["@opencode-ai/cli"])
	}
}

func TestParseUvList(t *testing.T) {
	out := "kimi-cli v1.49.0\n- kimi\n- kimi-cli\nmcp-proxy v0.12.0\n- mcp-proxy\n"
	m := parseUvList(out)
	if m["kimi-cli"] != "1.49.0" {
		t.Errorf("kimi-cli = %q", m["kimi-cli"])
	}
	if m["mcp-proxy"] != "0.12.0" {
		t.Errorf("mcp-proxy = %q", m["mcp-proxy"])
	}
	if _, ok := m["kimi"]; ok {
		t.Error("entrypoint line parsed as a tool")
	}
}

func TestParsePipxJSON(t *testing.T) {
	out := `{"venvs":{"superclaude":{"metadata":{"main_package":{"package_version":"4.3.0"}}}}}`
	m, err := parsePipxJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if m["superclaude"] != "4.3.0" {
		t.Errorf("superclaude = %q", m["superclaude"])
	}
}

func TestClassifyShadowed(t *testing.T) {
	instances := []Instance{
		{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode", Version: "1.18.35", Status: Active},
		{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode", Version: "1.19.0", Status: Active},
	}
	live := map[string]liveBin{
		"opencode": {path: "/opt/homebrew/bin/opencode", real: "/opt/homebrew/lib/node_modules/opencode-ai/bin/opencode.exe"},
	}
	classify(instances, live)
	if instances[0].Status != Active || instances[0].Path != "/opt/homebrew/bin/opencode" {
		t.Errorf("npm install should be active: %+v", instances[0])
	}
	if instances[1].Status != Shadowed || instances[1].Path != "" {
		t.Errorf("brew install should be shadowed: %+v", instances[1])
	}
}

func TestClassifyUniqueBinActive(t *testing.T) {
	instances := []Instance{
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1", Status: Active},
	}
	live := map[string]liveBin{
		"codex": {path: "/opt/homebrew/bin/codex", real: "/opt/homebrew/Caskroom/codex/0.162.1/codex"},
	}
	classify(instances, live)
	if instances[0].Status != Active || instances[0].Path != "/opt/homebrew/bin/codex" {
		t.Errorf("unique bin should be active with path: %+v", instances[0])
	}
}

func TestClassifyUnresolvable(t *testing.T) {
	// when the live realpath matches no install, don't fabricate shadowing
	instances := []Instance{
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Status: Active},
		{HarnessID: "qwen", Method: "brew-formula", Package: "qwen-code", Bin: "qwen", Status: Active},
	}
	live := map[string]liveBin{
		"qwen": {path: "/opt/homebrew/bin/qwen", real: "/somewhere/else/qwen"},
	}
	classify(instances, live)
	for i, inst := range instances {
		if inst.Status != Active {
			t.Errorf("instance %d should remain active (unresolvable), got %d", i, inst.Status)
		}
	}
}

func TestClassifyNativeOwnsLivePath(t *testing.T) {
	// A script-installed binary (opencode's curl installer) and an npm copy
	// share the `opencode` bin. The live PATH binary is the native one, so
	// the native install must be Active and the npm install Shadowed.
	instances := []Instance{
		{HarnessID: "opencode", Method: "npm", Package: "opencode-ai", Bin: "opencode", Version: "1.18.35", Status: Active},
		{HarnessID: "opencode", Method: "native", Bin: "opencode", Path: "/home/x/.opencode/bin/opencode", Version: "1.19.0", Status: Active},
	}
	live := map[string]liveBin{
		"opencode": {path: "/home/x/.opencode/bin/opencode", real: "/home/x/.opencode/bin/opencode"},
	}
	classify(instances, live)
	if instances[0].Status != Shadowed || instances[0].Path != "" {
		t.Errorf("npm install should be shadowed: %+v", instances[0])
	}
	if instances[1].Status != Active || instances[1].Path != "/home/x/.opencode/bin/opencode" {
		t.Errorf("native install should be active: %+v", instances[1])
	}
}

func TestClassifyNativeSingleKeepsPathWhenNotOnPath(t *testing.T) {
	// A native binary exists at its probe path but is not on PATH. The
	// install is still real; classify must not blank its path.
	instances := []Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Path: "/home/x/.local/bin/claude"},
	}
	classify(instances, map[string]liveBin{"claude": {}})
	if instances[0].Status != Active || instances[0].Path != "/home/x/.local/bin/claude" {
		t.Errorf("native path should survive a failed LookPath: %+v", instances[0])
	}
}

func TestClassifyNativeSingleShadowedWhenPathResolvesElsewhere(t *testing.T) {
	// A lone native install must not claim the live binary when PATH
	// resolves to a different executable: `<bin> update` would run that
	// other copy, so report shadowing and keep the probe path.
	instances := []Instance{
		{HarnessID: "opencode", Method: "native", Bin: "opencode", Path: "/home/x/.opencode/bin/opencode"},
	}
	live := map[string]liveBin{
		"opencode": {path: "/opt/homebrew/bin/opencode", real: "/opt/homebrew/lib/node_modules/opencode-ai/bin/opencode.exe"},
	}
	classify(instances, live)
	if instances[0].Status != Shadowed {
		t.Errorf("lone native install pointing at another executable should be shadowed: %+v", instances[0])
	}
	if instances[0].Path != "/home/x/.opencode/bin/opencode" {
		t.Errorf("shadowed native should keep its probe path, got %q", instances[0].Path)
	}
}

func TestClassifyNativeSymlinkOwnsRealPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "claude-2.1.310")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// lookUp always reports an EvalSymlinks-resolved real path; mirror that.
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	instances := []Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Path: link},
		{HarnessID: "claude", Method: "npm", Package: "@anthropic-ai/claude-code", Bin: "claude"},
	}
	classify(instances, map[string]liveBin{"claude": {path: link, real: resolved}})
	if instances[0].Status != Active {
		t.Errorf("native symlink should own the live path: %+v", instances[0])
	}
	if instances[1].Status != Shadowed {
		t.Errorf("npm install should be shadowed: %+v", instances[1])
	}
}

func TestOwnsReal(t *testing.T) {
	cases := []struct {
		inst Instance
		real string
		want bool
	}{
		{Instance{Method: "npm", Package: "opencode-ai"}, "/opt/homebrew/lib/node_modules/opencode-ai/bin/opencode.exe", true},
		{Instance{Method: "brew-formula", Package: "opencode"}, "/opt/homebrew/Cellar/opencode/1.19.0/bin/opencode", true},
		{Instance{Method: "brew-cask", Package: "codex"}, "/opt/homebrew/Caskroom/codex/0.162.1/codex", true},
		{Instance{Method: "pipx", Package: "superclaude"}, "/Users/x/.local/pipx/venvs/superclaude/bin/superclaude", true},
		{Instance{Method: "uv", Package: "kimi-cli"}, "/Users/x/.local/share/uv/tools/kimi-cli/bin/kimi-cli", true},
		{Instance{Method: "npm", Package: "opencode-ai"}, "/opt/homebrew/Cellar/opencode/1.19.0/bin/opencode", false},
		{Instance{Method: "npm", Package: "opencode-ai"}, "", false},
		{Instance{Method: "native", Path: "/home/x/.local/bin/claude"}, "/home/x/.local/bin/claude", true},
		{Instance{Method: "native", Path: "/home/x/.local/bin/claude"}, "/opt/homebrew/bin/claude", false},
		{Instance{Method: "native"}, "/home/x/.local/bin/claude", false},
	}
	for _, c := range cases {
		if got := ownsReal(c.inst, c.real); got != c.want {
			t.Errorf("ownsReal(%+v, %q) = %v, want %v", c.inst, c.real, got, c.want)
		}
	}
}

func TestDetectNative(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakebin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '1.2.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := catalog.Harness{ID: "fake", Name: "Fake", Installs: []catalog.Install{{
		Method:  "native",
		Bin:     "fakebin",
		Paths:   []string{bin},
		Version: catalog.CmdSpec{Cmd: []string{"fakebin", "--version"}},
	}}}
	run := func(name string, args ...string) (string, error) {
		if name != bin {
			t.Fatalf("unexpected run of %q", name)
		}
		return "1.2.3\n", nil
	}
	inst, ok, err := detectNative(h, h.Installs[0], run)
	if err != nil || !ok {
		t.Fatalf("detectNative = %v, %v, %v", inst, ok, err)
	}
	if inst.Version != "1.2.3" || inst.Path != bin || inst.Status != Active {
		t.Errorf("bad instance: %+v", inst)
	}
}

func TestDetectNativeMissing(t *testing.T) {
	h := catalog.Harness{ID: "fake", Name: "Fake", Installs: []catalog.Install{{
		Method:  "native",
		Bin:     "ghost",
		Paths:   []string{"~/.ghost/ghost"},
		Version: catalog.CmdSpec{Cmd: []string{"ghost", "--version"}},
	}}}
	_, ok, err := detectNative(h, h.Installs[0], DefaultRunner)
	if err != nil || ok {
		t.Fatalf("missing native should be absent: ok=%v err=%v", ok, err)
	}
}
