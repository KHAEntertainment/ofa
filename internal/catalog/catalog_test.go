package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedCatalog(t *testing.T) {
	hs, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	assertCatalog(t, hs)
}

func TestLoadCatalogDir(t *testing.T) {
	hs, err := Load("data")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertCatalog(t, hs)
}

func assertCatalog(t *testing.T, hs []Harness) {
	t.Helper()
	if len(hs) != 13 {
		t.Fatalf("want 13 harnesses, got %d", len(hs))
	}
	byID := map[string]Harness{}
	for _, h := range hs {
		byID[h.ID] = h
	}
	for _, want := range []string{"claude", "codex", "opencode", "qwen", "kimi", "pi", "kilocode", "cursor-agent", "copilot", "coderabbit", "devin", "agy", "mmx"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("missing harness %q", want)
		}
	}
	if got := len(byID["opencode"].Installs); got != 3 {
		t.Errorf("opencode installs = %d, want 3", got)
	}
	if got := len(byID["qwen"].Installs); got != 2 {
		t.Errorf("qwen installs = %d, want 2", got)
	}
	if got := len(byID["kimi"].Installs); got != 2 {
		t.Errorf("kimi installs = %d, want 2", got)
	}
}

func TestLoadRejectsMalformedTOML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.toml", "id = [unclosed\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for malformed TOML")
	}
}

func TestLoadRejectsUnknownMethod(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x.toml", `id = "x"
name = "X"

[[installs]]
method = "carrier-pigeon"
bin = "x"
version = { cmd = ["x", "--version"] }
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestLoadRejectsDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.toml", `id = "dup"
name = "A"

[[installs]]
method = "native"
bin = "a"
paths = ["~/.local/bin/a"]
version = { cmd = ["a", "--version"] }
`)
	writeFile(t, dir, "b.toml", `id = "dup"
name = "B"

[[installs]]
method = "native"
bin = "b"
paths = ["~/.local/bin/b"]
version = { cmd = ["b", "--version"] }
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for duplicate id")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
