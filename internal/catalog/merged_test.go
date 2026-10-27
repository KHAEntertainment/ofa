package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// nativeTOML is a minimal valid catalog entry using the native method, so a
// test can write a user file without dragging in a package manager.
func nativeTOML(id, name string) string {
	return `id = "` + id + `"
name = "` + name + `"

[[installs]]
method = "native"
bin = "` + id + `"
paths = ["~/.local/bin/` + id + `"]
version = { cmd = ["` + id + `", "--version"] }
`
}

func mustEmbedded(t *testing.T) []Harness {
	t.Helper()
	hs, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	return hs
}

func byID(hs []Harness) map[string]Harness {
	m := make(map[string]Harness, len(hs))
	for _, h := range hs {
		m[h.ID] = h
	}
	return m
}

func indexOf(hs []Harness, id string) int {
	for i, h := range hs {
		if h.ID == id {
			return i
		}
	}
	return -1
}

// An absent dir, an empty dir, and the empty path must all be the embedded
// catalog itself — same harnesses, same order, nothing to report. This is the
// "byte-identical to v0.4.1" guarantee at its source.
func TestMergedWithoutUserCatalogMatchesEmbedded(t *testing.T) {
	want := mustEmbedded(t)
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"absent", filepath.Join(t.TempDir(), "catalog")},
		{"empty", t.TempDir()},
		{"empty path", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problems, err := Merged(tc.dir)
			if err != nil {
				t.Fatalf("Merged: %v", err)
			}
			if len(problems) != 0 {
				t.Errorf("want no problems, got %v", problems)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Merged(%q) differs from Embedded():\ngot  %+v\nwant %+v", tc.dir, got, want)
			}
		})
	}
}

func TestMergedAddsUserHarness(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "local.toml", nativeTOML("local-harness", "Local Harness"))

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("want no problems, got %v", problems)
	}
	if len(got) != len(mustEmbedded(t))+1 {
		t.Fatalf("harness count = %d, want embedded+1", len(got))
	}
	added, ok := byID(got)["local-harness"]
	if !ok {
		t.Fatal("user harness not merged in")
	}
	if added.Name != "Local Harness" {
		t.Errorf("name = %q", added.Name)
	}
	// Appended, not prepended: embedded entries keep their positions.
	if indexOf(got, "local-harness") != len(got)-1 {
		t.Errorf("user harness should be appended last, got index %d", indexOf(got, "local-harness"))
	}
	if !reflect.DeepEqual(got[:len(mustEmbedded(t))], mustEmbedded(t)) {
		t.Errorf("embedded entries and their order must survive the merge, got %+v", got)
	}
}

// A user file whose id matches an embedded one replaces the *whole* entry: no
// field-level merge, so an entry that ships two installs and is overridden with
// one ends up with exactly one.
func TestMergedUserEntryReplacesEmbeddedWhole(t *testing.T) {
	embedded := mustEmbedded(t)
	shipped := byID(embedded)["claude"]
	if len(shipped.Installs) < 2 {
		t.Fatalf("fixture assumption broken: claude ships %d installs", len(shipped.Installs))
	}

	dir := t.TempDir()
	writeFile(t, dir, "claude.toml", nativeTOML("claude", "Claude Local"))

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("override must not be reported as a problem, got %v", problems)
	}
	if len(got) != len(embedded) {
		t.Fatalf("override changed the harness count: %d, want %d", len(got), len(embedded))
	}
	claude := byID(got)["claude"]
	if claude.Name != "Claude Local" {
		t.Errorf("name = %q, want the user's name", claude.Name)
	}
	if len(claude.Installs) != 1 {
		t.Errorf("installs = %d, want 1 (whole-entry replace, no field merge)", len(claude.Installs))
	}
	if claude.Installs[0].Bin != "claude" || claude.Installs[0].Method != "native" {
		t.Errorf("install = %+v, want the user's native entry", claude.Installs[0])
	}
	// An override takes the embedded entry's position, so shipped ordering is
	// the binary's decision, not the user's.
	if want := indexOf(embedded, "claude"); indexOf(got, "claude") != want {
		t.Errorf("overridden entry moved: index %d, want %d", indexOf(got, "claude"), want)
	}
}

// One bad file is skipped, reported, and costs nothing else: the good user file
// and the embedded catalog both still load.
func TestMergedSkipsBadUserFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a-broken.toml", "id = [unclosed\n")
	writeFile(t, dir, "b-badmethod.toml", `id = "bird"
name = "Bird"

[[installs]]
method = "carrier-pigeon"
bin = "bird"
version = { cmd = ["bird", "--version"] }
`)
	writeFile(t, dir, "c-good.toml", nativeTOML("good", "Good"))

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("bad user files must not be a hard error, got %v", err)
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want 2", problems)
	}
	if problems[0].File != "a-broken.toml" {
		t.Errorf("first problem file = %q, want a-broken.toml (filename order)", problems[0].File)
	}
	if !strings.Contains(problems[0].Error(), "a-broken.toml") {
		t.Errorf("problem message must name the file, got %q", problems[0].Error())
	}
	if problems[1].File != "b-badmethod.toml" {
		t.Errorf("second problem file = %q", problems[1].File)
	}
	if !strings.Contains(problems[1].Error(), "carrier-pigeon") {
		t.Errorf("problem message must carry the reason, got %q", problems[1].Error())
	}
	ids := byID(got)
	if _, ok := ids["good"]; !ok {
		t.Error("valid user file was not loaded alongside the bad ones")
	}
	if _, ok := ids["bird"]; ok {
		t.Error("invalid user entry must be skipped, not loaded")
	}
	if len(got) != len(mustEmbedded(t))+1 {
		t.Errorf("harness count = %d, want embedded+1", len(got))
	}
}

// Two user files claiming one id: the first in filename order wins, and the
// second is reported. Nothing here may shadow the embedded catalog.
func TestMergedDuplicateUserIDFirstFileWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a-first.toml", nativeTOML("dup", "First"))
	writeFile(t, dir, "b-second.toml", nativeTOML("dup", "Second"))

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	if problems[0].File != "b-second.toml" {
		t.Errorf("problem file = %q, want b-second.toml", problems[0].File)
	}
	if !strings.Contains(problems[0].Error(), "a-first.toml") {
		t.Errorf("problem should name the winning file, got %q", problems[0].Error())
	}
	if name := byID(got)["dup"].Name; name != "First" {
		t.Errorf("winner = %q, want First", name)
	}
	if len(got) != len(mustEmbedded(t))+1 {
		t.Errorf("harness count = %d, want embedded+1", len(got))
	}
}

// The embedded catalog is the floor: even when every user file is unusable the
// command still has a catalog to work from.
func TestMergedAllUserFilesBadKeepsEmbedded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.toml", "id = [unclosed\n")
	writeFile(t, dir, "b.toml", `name = "No id"

[[installs]]
method = "native"
bin = "x"
version = { cmd = ["x", "--version"] }
`)
	writeFile(t, dir, "c.toml", "")

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 3 {
		t.Errorf("problems = %v, want 3", problems)
	}
	if !reflect.DeepEqual(got, mustEmbedded(t)) {
		t.Errorf("all-bad user catalog must leave Embedded() intact, got %d harnesses", len(got))
	}
}

// A user catalog that is not a directory of TOML (here: a regular file) is
// indistinguishable from no catalog at all — normal, not a problem.
func TestMergedNonDirectoryUserDirIsQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, problems, err := Merged(path)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("want no problems, got %v", problems)
	}
	if !reflect.DeepEqual(got, mustEmbedded(t)) {
		t.Error("non-directory user dir must leave Embedded() intact")
	}
}