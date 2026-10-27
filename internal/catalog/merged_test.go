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

// An absent dir and an empty dir must both be the embedded catalog itself —
// same harnesses, same order, nothing to report. This is the "byte-identical to
// v0.4.1" guarantee at its source.
func TestMergedWithoutUserCatalogMatchesEmbedded(t *testing.T) {
	want := mustEmbedded(t)
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"absent", filepath.Join(t.TempDir(), "catalog")},
		{"empty", t.TempDir()},
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

// A user file with a misspelled key is skipped like any other invalid file, and
// its Problem names both the file and the key: the warning has to tell the user
// which line to fix, not just that something was wrong.
func TestMergedSkipsUserFileWithUnknownKey(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "local-harness.toml", `id = "local-harness"
name = "Local Harness"

[[installs]]
method = "npm"
package = "local-harness"
bin = "local-harness"
lates = "npm"
version = { cmd = ["local-harness", "--version"] }
`)
	writeFile(t, dir, "good.toml", nativeTOML("good", "Good"))

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("a bad user file must not be a hard error: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	want := "local-harness.toml: unknown key(s): installs.lates"
	if problems[0].Error() != want {
		t.Errorf("problem = %q, want %q", problems[0].Error(), want)
	}
	if _, ok := byID(got)["local-harness"]; ok {
		t.Error("entry with an unknown key must be skipped, not merged")
	}
	if _, ok := byID(got)["good"]; !ok {
		t.Error("the valid user file must still load")
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

// The empty path means "no user catalog", never "the process's working
// directory". The CWD here holds a perfectly valid catalog entry, so a loader
// that fell back to "." would merge it in and fail.
func TestMergedEmptyDirNeverReadsWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, cwd, "sneaky.toml", nativeTOML("sneaky", "Sneaky"))
	t.Chdir(cwd)

	got, problems, err := Merged("")
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("want no problems, got %v", problems)
	}
	if !reflect.DeepEqual(got, mustEmbedded(t)) {
		t.Fatalf("Merged(\"\") read the working directory: got %d harnesses", len(got))
	}
}

// The user dir is a real path, not a glob pattern. A directory whose name holds
// glob metacharacters must be listed literally; before this test, `[` in the
// path made filepath.Glob return ErrBadPattern and `*`/`?` silently matched
// whatever the pattern happened to hit.
func TestMergedUserDirIsNotAGlobPattern(t *testing.T) {
	for _, name := range []string{"catalog[", "catalog*", "catalog?x", "cat[ab]"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "local.toml", nativeTOML("local-harness", "Local"))

			got, problems, err := Merged(dir)
			if err != nil {
				t.Fatalf("Merged(%q): %v", dir, err)
			}
			if len(problems) != 0 {
				t.Errorf("want no problems, got %v", problems)
			}
			if _, ok := byID(got)["local-harness"]; !ok {
				t.Errorf("harness in %q was not read", dir)
			}
		})
	}
}

// A bracketed dir name must not be expanded into a character class: with
// `[ab]` and a sibling `a` both present, only `[ab]`'s entry may be read.
func TestMergedBracketDirDoesNotReadSibling(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(root, "a")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sibling, "sibling.toml", nativeTOML("sibling", "Sibling"))
	bracketed := filepath.Join(root, "[ab]")
	if err := os.MkdirAll(bracketed, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bracketed, "mine.toml", nativeTOML("mine", "Mine"))

	got, problems, err := Merged(bracketed)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("want no problems, got %v", problems)
	}
	ids := byID(got)
	if _, ok := ids["mine"]; !ok {
		t.Error("entry in the bracketed dir was not read")
	}
	if _, ok := ids["sibling"]; ok {
		t.Error("Merged read the sibling directory: the dir must be listed literally")
	}
}

// A dir path that merely looks like a broken pattern (and does not exist) is
// still just an absent user catalog: quiet, embedded-only, no error.
func TestMergedAbsentBracketDirIsQuiet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog[")
	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged(%q) must not error on a glob-looking path: %v", dir, err)
	}
	if len(problems) != 0 {
		t.Errorf("want no problems, got %v", problems)
	}
	if !reflect.DeepEqual(got, mustEmbedded(t)) {
		t.Errorf("absent bracket dir must leave Embedded() intact, got %d harnesses", len(got))
	}
}

// A user dir OFA cannot list is one problem naming the dir; the embedded
// catalog still loads and the command can still run.
func TestMergedUnlistableUserDirIsOneProblem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	writeFile(t, dir, "local.toml", nativeTOML("local-harness", "Local"))
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("an unreadable user dir must not be a hard error: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly 1", problems)
	}
	if problems[0].File != dir {
		t.Errorf("problem names %q, want the dir %q", problems[0].File, dir)
	}
	if !reflect.DeepEqual(got, mustEmbedded(t)) {
		t.Errorf("unreadable user dir must leave Embedded() intact, got %d harnesses", len(got))
	}
}

// One unreadable file is reported and skipped; its siblings and the embedded
// catalog still load.
func TestMergedUnreadableFileIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	writeFile(t, dir, "a-locked.toml", nativeTOML("locked", "Locked"))
	writeFile(t, dir, "b-good.toml", nativeTOML("good", "Good"))
	if err := os.Chmod(filepath.Join(dir, "a-locked.toml"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "a-locked.toml"), 0o644) })

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	if problems[0].File != "a-locked.toml" {
		t.Errorf("problem names %q, want a-locked.toml", problems[0].File)
	}
	ids := byID(got)
	if _, ok := ids["good"]; !ok {
		t.Error("valid sibling was not loaded alongside the unreadable file")
	}
	if _, ok := ids["locked"]; ok {
		t.Error("unreadable file must be skipped, not loaded")
	}
}

// The dangerous shape for the doctor flow: a user file that overrides a shipped
// id but carries a typo. It must be skipped — so the embedded entry survives
// untouched — rather than half-applied, and reported.
func TestMergedUnknownKeyInOverrideKeepsEmbeddedEntry(t *testing.T) {
	embedded := byID(mustEmbedded(t))["claude"]
	dir := t.TempDir()
	writeFile(t, dir, "claude.toml", `id = "claude"
name = "Claude Local"

[[installs]]
method = "npm"
package = "@anthropic-ai/claude-code"
bin = "claude"
version = { cmd = ["claude", "--version"] }
updat = { cmd = ["npm", "install", "-g", "@anthropic-ai/claude-code@latest"] }
`)

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want 1", problems)
	}
	want := "claude.toml: unknown key(s): installs.updat, installs.updat.cmd"
	if problems[0].Error() != want {
		t.Errorf("problem = %q, want %q", problems[0].Error(), want)
	}
	if h := byID(got)["claude"]; !reflect.DeepEqual(h, embedded) {
		t.Errorf("a rejected override must leave the shipped entry intact:\ngot  %+v\nwant %+v", h, embedded)
	}
}

// A symlinked file is read; a symlinked directory works as a user dir; and a
// directory named *.toml is not mistaken for an entry.
func TestMergedFollowsFileSymlinksAndSkipsDirNamedToml(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real.toml")
	if err := os.WriteFile(real, []byte(nativeTOML("linked", "Linked")), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "catalog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link.toml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "decoy.toml"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, problems, err := Merged(dir)
	if err != nil {
		t.Fatalf("Merged: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("want no problems, got %v", problems)
	}
	if _, ok := byID(got)["linked"]; !ok {
		t.Error("symlinked catalog file was not read")
	}

	// A symlink *to* the directory is an ordinary way to keep a catalog on
	// another volume; it must behave exactly like the directory itself.
	linkDir := filepath.Join(root, "catalog-link")
	if err := os.Symlink(dir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linked, problems, err := Merged(linkDir)
	if err != nil {
		t.Fatalf("Merged(symlinked dir): %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("want no problems through the symlinked dir, got %v", problems)
	}
	if _, ok := byID(linked)["linked"]; !ok {
		t.Error("symlinked-dir catalog file was not read")
	}
}
