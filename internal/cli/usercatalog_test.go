package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ofa/internal/detect"
	"ofa/internal/resolve"
)

// userCatalogTOML is one minimal valid user catalog entry. Native method keeps
// the fixture free of package managers.
func userCatalogTOML(id, name string) string {
	return `id = "` + id + `"
name = "` + name + `"

[[installs]]
method = "native"
bin = "` + id + `"
paths = ["~/.local/bin/` + id + `"]
version = { cmd = ["` + id + `", "--version"] }
`
}

// writeUserCatalog writes files into a fresh directory and returns its path.
func writeUserCatalog(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A harness the user added to ~/.config/ofa/catalog is in the catalog the scan
// runs against, and shows up in scan's report — while every embedded harness is
// still there.
func TestScanIncludesUserCatalogHarness(t *testing.T) {
	a, out, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.CatalogDir = writeUserCatalog(t, map[string]string{
		"local-harness.toml": userCatalogTOML("local-harness", "Local Harness"),
	})
	sd.instances = []detect.Instance{
		{HarnessID: "local-harness", Method: "native", Bin: "local-harness", Version: "1.0.0", Status: detect.Active},
	}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	ids := map[string]bool{}
	for _, h := range sd.cat {
		ids[h.ID] = true
	}
	if !ids["local-harness"] {
		t.Error("user catalog harness never reached detection")
	}
	for _, want := range []string{"claude", "codex", "opencode"} {
		if !ids[want] {
			t.Errorf("embedded harness %q lost from the merged catalog", want)
		}
	}
	if !strings.Contains(out.String(), "local-harness") {
		t.Errorf("scan output should list the user harness, got %q", out.String())
	}
	if strings.Contains(stderr.String(), "skipping user catalog file") {
		t.Errorf("a clean user catalog must not warn, got %q", stderr.String())
	}
}

// A user entry that reuses an embedded id replaces it, so scan reports the
// user's install set rather than the shipped one.
func TestScanUsesUserOverrideOfEmbeddedHarness(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.CatalogDir = writeUserCatalog(t, map[string]string{
		"claude.toml": userCatalogTOML("claude", "Claude Local"),
	})

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr.String())
	}
	var claudeIdx = -1
	for i, h := range sd.cat {
		if h.ID == "claude" {
			claudeIdx = i
		}
	}
	if claudeIdx < 0 {
		t.Fatal("claude missing from the merged catalog")
	}
	claude := sd.cat[claudeIdx]
	if claude.Name != "Claude Local" {
		t.Errorf("claude.Name = %q, want the user's override", claude.Name)
	}
	if len(claude.Installs) != 1 {
		t.Errorf("claude installs = %d, want 1 (whole-entry replace)", len(claude.Installs))
	}
}

// check/scan/apply must survive a broken user catalog: same exit code, same
// stdout, warnings on stderr only. --json in particular must keep its shape —
// byte-for-byte identical to the run with no user catalog at all.
func TestBrokenUserCatalogWarnsButDoesNotChangeOutput(t *testing.T) {
	pinned := func() time.Time { return mustTime("2025-01-02T03:04:05Z") }

	// run executes one command against dir and returns its full result.
	run := func(t *testing.T, args []string, dir string) (ExitCode, string, string) {
		t.Helper()
		a, out, errOut, sd, sr, _ := newTestApp(t, args, nil)
		a.CatalogDir = dir
		a.Now = pinned
		sd.instances = []detect.Instance{
			{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
			{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active},
		}
		sr.res = []resolve.Resolution{
			{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
			{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
		}
		code := a.Run()
		return code, out.String(), errOut.String()
	}

	broken := writeUserCatalog(t, map[string]string{"broken.toml": "id = [unclosed\n"})
	absent := filepath.Join(t.TempDir(), "catalog")

	for _, args := range [][]string{
		{"scan"},
		{"scan", "--json"},
		{"check"},
		{"check", "--json"},
		{"apply", "--dry-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			wantCode, wantOut, wantErr := run(t, args, absent)
			gotCode, gotOut, gotErr := run(t, args, broken)

			if gotCode != wantCode {
				t.Errorf("exit = %d, want %d unchanged by a skipped user file", gotCode, wantCode)
			}
			if gotOut != wantOut {
				t.Errorf("stdout changed:\nwith broken user file: %q\nwithout:              %q", gotOut, wantOut)
			}
			// Stderr must be unchanged except for the one added warning line,
			// and a clean catalog must add nothing at all.
			if strings.Contains(wantErr, "skipping user catalog file") {
				t.Fatalf("no user catalog must not warn, got stderr %q", wantErr)
			}
			warning, rest, found := strings.Cut(gotErr, "\n")
			if !found {
				t.Fatalf("expected at least the warning line on stderr, got %q", gotErr)
			}
			if !strings.Contains(warning, "ofa: warning: skipping user catalog file") {
				t.Errorf("warning must say what it skipped, got %q", warning)
			}
			if !strings.Contains(warning, "broken.toml") {
				t.Errorf("warning must name the file, got %q", warning)
			}
			if rest != wantErr {
				t.Errorf("stderr beyond the warning changed:\ngot  %q\nwant %q", rest, wantErr)
			}
			if n := strings.Count(gotErr, "skipping user catalog file"); n != 1 {
				t.Errorf("one bad file must warn once, got %d warnings: %q", n, gotErr)
			}
		})
	}
}

// --json stays machine-readable with warnings in play: the text goes to stderr,
// so stdout still parses.
func TestBrokenUserCatalogKeepsJSONParsable(t *testing.T) {
	a, out, stderr, sd, sr, _ := newTestApp(t, []string{"scan", "--json"}, nil)
	a.CatalogDir = writeUserCatalog(t, map[string]string{"broken.toml": "id = [unclosed\n"})
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sr.res = nil

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan --json exit = %d (stderr: %s)", code, stderr.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\noutput: %q", err, out.String())
	}
	if !strings.Contains(stderr.String(), "broken.toml") {
		t.Errorf("warning missing from stderr, got %q", stderr.String())
	}
}

// Two user files claiming one id: the first by filename wins, the second warns.
func TestDuplicateUserCatalogIDWarnsForSecondFile(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.CatalogDir = writeUserCatalog(t, map[string]string{
		"a-first.toml":  userCatalogTOML("dup", "First"),
		"b-second.toml": userCatalogTOML("dup", "Second"),
	})

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "b-second.toml") {
		t.Errorf("warning must name the losing file, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "duplicate id") {
		t.Errorf("warning must say it is a duplicate, got %q", stderr.String())
	}
	name := ""
	for _, h := range sd.cat {
		if h.ID == "dup" {
			name = h.Name
		}
	}
	if name != "First" {
		t.Errorf("winning entry = %q, want First (first by filename)", name)
	}
}