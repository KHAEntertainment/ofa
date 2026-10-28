package catalog

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if got := len(byID["opencode"].Installs); got != 4 {
		t.Errorf("opencode installs = %d, want 4", got)
	}
	if got := len(byID["qwen"].Installs); got != 2 {
		t.Errorf("qwen installs = %d, want 2", got)
	}
	if got := len(byID["kimi"].Installs); got != 2 {
		t.Errorf("kimi installs = %d, want 2", got)
	}
	if got := len(byID["claude"].Installs); got != 2 {
		t.Errorf("claude installs = %d, want 2", got)
	}
	if got := len(byID["codex"].Installs); got != 2 {
		t.Errorf("codex installs = %d, want 2", got)
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

// TestLoadRejectsUnknownKeys pins the whole point of the check: a key the
// structs do not consume means a field the author meant to set that silently
// stayed empty, so the file is rejected and the message carries the full dotted
// key path. loadEntries supplies the "file: " prefix, so a user of the CLI
// always learns which file to fix.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown top-level key",
			body: `id = "x"
name = "X"
pinskills = 3

[[installs]]
method = "native"
bin = "x"
version = { cmd = ["x", "--version"] }
`,
			want: "x.toml: unknown key(s): pinskills",
		},
		{
			// The bug from the issue: `latest` misspelled decodes to an
			// empty Latest, which then resolves as "report-only".
			name: "misspelled latest inside [[installs]]",
			body: `id = "x"
name = "X"

[[installs]]
method = "npm"
package = "x-cli"
bin = "x"
lates = "npm"
version = { cmd = ["x", "--version"] }
`,
			want: "x.toml: unknown key(s): installs.lates",
		},
		{
			name: "unknown key in the second [[installs]]",
			body: `id = "x"
name = "X"

[[installs]]
method = "native"
bin = "x"
version = { cmd = ["x", "--version"] }

[[installs]]
method = "npm"
package = "x-cli"
bin = "x"
version = { cmd = ["x", "--version"] }
updat = { cmd = ["npm", "install", "-g", "x-cli"] }
`,
			want: "x.toml: unknown key(s): installs.updat, installs.updat.cmd",
		},
		{
			name: "unknown key inside the version table",
			body: `id = "x"
name = "X"

[[installs]]
method = "native"
bin = "x"
version = { cmd = ["x", "--version"], lag = 1 }
`,
			want: "x.toml: unknown key(s): installs.version.lag",
		},
		{
			name: "unknown key inside the update table",
			body: `id = "x"
name = "X"

[[installs]]
method = "native"
bin = "x"
version = { cmd = ["x", "--version"] }
update = { cmd = ["x", "update"], shim = "z" }
`,
			want: "x.toml: unknown key(s): installs.update.shim",
		},
		{
			name: "unknown key in a [installs.version] header table",
			body: `id = "x"
name = "X"

[[installs]]
method = "native"
bin = "x"

[installs.version]
cmd = ["x", "--version"]
verbose = true
`,
			want: "x.toml: unknown key(s): installs.version.verbose",
		},
		{
			// An unknown table is reported by its head and by each key under
			// it, the same way config.toml reports them: nothing is collapsed,
			// so no key that a typo hid is left out of the message.
			name: "unknown table names its head and its children",
			body: `id = "x"
name = "X"

[[installs]]
method = "native"
bin = "x"
version = { cmd = ["x", "--version"] }

[extra]
foo = 1
bar = 2
`,
			want: "x.toml: unknown key(s): extra, extra.bar, extra.foo",
		},
		{
			name: "several unknowns are reported together, sorted",
			body: `id = "x"
name = "X"
pinskills = 3

[[installs]]
method = "npm"
package = "x-cli"
bin = "x"
lates = "npm"
version = { cmd = ["x", "--version"] }
update = { cmd = ["x", "update"], shim = "z" }
`,
			want: "x.toml: unknown key(s): installs.lates, installs.update.shim, pinskills",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "x.toml", tc.body)
			hs, err := Load(dir)
			if err == nil {
				t.Fatalf("file with an unknown key was accepted: %+v", hs)
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q, want %q", err.Error(), tc.want)
			}
			if hs != nil {
				t.Errorf("a rejected file must yield no harnesses, got %d", len(hs))
			}
		})
	}
}

// TestEveryEmbeddedEntryParses is the shipped-catalog guard: every embedded
// data/*.toml must decode with no unknown key. Embedded stops at the first bad
// file, so this checks the entries one by one and names each one that breaks.
// It also pins the id == file name convention the embedded files follow.
func TestEveryEmbeddedEntryParses(t *testing.T) {
	names, err := fs.Glob(catalogFS, "data/*.toml")
	if err != nil {
		t.Fatalf("glob embedded catalog: %v", err)
	}
	if len(names) != 13 {
		t.Fatalf("embedded catalog files = %d, want 13", len(names))
	}
	for _, name := range names {
		b, err := fs.ReadFile(catalogFS, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		base := filepath.Base(name)
		h, err := parseEntry(entry{name: base, data: b})
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if want := strings.TrimSuffix(base, ".toml"); h.ID != want {
			t.Errorf("%s declares id %q, want it to match the file name", base, h.ID)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidMethodsMatchesValidate(t *testing.T) {
	got := ValidMethods()
	want := []string{"brew-cask", "brew-formula", "native", "npm", "pipx", "uv"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ValidMethods() = %v, want %v", got, want)
	}
	// The view must stay a view: every advertised method must be accepted by
	// validate, or doctor's prompt would advertise values the catalog rejects.
	for _, m := range got {
		if !validMethods[m] {
			t.Errorf("ValidMethods advertises %q, which validate rejects", m)
		}
	}
}

func TestValidLatestMatchesValidate(t *testing.T) {
	got := ValidLatest()
	want := []string{"brew", "none", "npm", "pypi", "self"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ValidLatest() = %v, want %v", got, want)
	}
	for _, l := range got {
		if !validLatest[l] {
			t.Errorf("ValidLatest advertises %q, which validate rejects", l)
		}
	}
}

func TestParseEntry(t *testing.T) {
	h, err := ParseEntry("draft.toml", []byte(`id = "x"
name = "X"

[[installs]]
method = "npm"
package = "x"
bin = "x"
version = { cmd = ["x", "--version"] }
latest = "npm"
`))
	if err != nil {
		t.Fatalf("ParseEntry: %v", err)
	}
	if h.ID != "x" {
		t.Errorf("id = %q, want x", h.ID)
	}
}

func TestParseEntryPrefixesNameAndValidates(t *testing.T) {
	// An unknown key is the typo class an agent draft actually produces.
	_, err := ParseEntry("draft.toml", []byte(`id = "x"
name = "X"
lates = "npm"

[[installs]]
method = "npm"
package = "x"
bin = "x"
version = { cmd = ["x", "--version"] }
`))
	if err == nil {
		t.Fatal("expected unknown-key error")
	}
	if !strings.HasPrefix(err.Error(), "draft.toml:") {
		t.Errorf("error = %q, want it prefixed with the entry name", err)
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error = %q, want unknown key reason", err)
	}
}
