package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Run: the session end to end, always with a fake agent ---

func runOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Agent:     nil, // set per test
		Runner:    nil, // set per test
		LookPath:  foundOnPath,
		DoctorDir: t.TempDir(),
		Now:       func() time.Time { return fixedTime },
	}
}

func TestRunSuccessWritesProposalAndContext(t *testing.T) {
	opts := runOpts(t)
	opts.Agent = (&fakeAgent{responses: []string{"```toml\n" + nativeEntryFor("sh", "/bin/sh") + "\n```"}}).Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n"}}).Run

	res, err := Run("sh", opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	dir := filepath.Join(opts.DoctorDir, "history", "20261028T123456Z")
	if res.Dir != dir {
		t.Errorf("session dir = %q, want %q", res.Dir, dir)
	}
	proposal := filepath.Join(dir, "proposed-catalog.toml")
	if res.ProposalPath != proposal {
		t.Errorf("proposal path = %q, want %q", res.ProposalPath, proposal)
	}
	b, err := os.ReadFile(proposal)
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if !strings.Contains(string(b), `bin = "sh"`) {
		t.Errorf("proposal content = %q", string(b))
	}
	if res.Harness == nil || res.Harness.ID != "sh-probe" {
		t.Errorf("result harness = %+v", res.Harness)
	}
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(res.Attempts))
	}
	ctx, err := os.ReadFile(filepath.Join(dir, "context.md"))
	if err != nil {
		t.Fatalf("read context.md: %v", err)
	}
	for _, want := range []string{"## Facts", "## Attempt 1", "### Prompt sent", "### Raw response", "### Verdict", "accepted", "BEGIN UNTRUSTED DATA"} {
		if !strings.Contains(string(ctx), want) {
			t.Errorf("context.md missing %q", want)
		}
	}
	// The proposal must be fenced inside the audit trail without breaking it.
	if strings.Contains(string(ctx), "```toml\nid") {
		t.Error("context.md embeds an unfenced TOML block")
	}
}

func TestRunCreatesPrivateDirsAndFiles(t *testing.T) {
	opts := runOpts(t)
	opts.Agent = (&fakeAgent{responses: []string{nativeEntryFor("sh", "/bin/sh")}}).Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n"}}).Run
	res, err := Run("sh", opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	st, err := os.Stat(res.Dir)
	if err != nil {
		t.Fatalf("stat session dir: %v", err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("session dir mode = %o, want 700", st.Mode().Perm())
	}
	for _, name := range []string{"context.md", "proposed-catalog.toml"} {
		st, err := os.Stat(filepath.Join(res.Dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, st.Mode().Perm())
		}
	}
}

func TestRunRetriesOnceWithTheError(t *testing.T) {
	bad := "id = \"bad\"\nname = \"Bad\"\n\n[[installs]]\nmethod = \"npm\"\npackage = \"notthere\"\nbin = \"sh\"\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"npm\"\n"
	agent := &fakeAgent{responses: []string{bad, nativeEntryFor("sh", "/bin/sh")}}
	opts := runOpts(t)
	opts.Agent = agent.Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n", "npm": `{"dependencies":{}}`}}).Run

	res, err := Run("sh", opts)
	if err != nil {
		t.Fatalf("Run after retry: %v", err)
	}
	if agent.calls() != 2 {
		t.Errorf("agent calls = %d, want 2 (initial + one retry)", agent.calls())
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(res.Attempts))
	}
	if !strings.Contains(res.Attempts[1].Prompt, "rejected by ofa's mechanical validation") {
		t.Error("retry prompt does not carry the validation error")
	}
	if !strings.Contains(res.Attempts[1].Prompt, "does not detect") {
		t.Error("retry prompt does not name the concrete failure")
	}
	if res.Attempts[0].Err == "" {
		t.Error("first attempt verdict missing")
	}
}

func TestRunRejectedAfterRetrySavesRejectedTxt(t *testing.T) {
	agent := &fakeAgent{responses: []string{"garbage", "still not toml"}}
	opts := runOpts(t)
	opts.Agent = agent.Run
	opts.Runner = (&fakeRunner{}).Run

	res, err := Run("zzbin", opts)
	if err == nil {
		t.Fatal("expected rejection error")
	}
	if agent.calls() != 2 {
		t.Errorf("agent calls = %d, want 2", agent.calls())
	}
	rejected := filepath.Join(res.Dir, "rejected.txt")
	if res.RejectedPath != rejected {
		t.Fatalf("rejected path = %q, want %q", res.RejectedPath, rejected)
	}
	b, err := os.ReadFile(rejected)
	if err != nil {
		t.Fatalf("read rejected.txt: %v", err)
	}
	if !strings.Contains(string(b), "still not toml") {
		t.Errorf("rejected.txt missing the last raw output: %q", string(b))
	}
	if res.ProposalPath != "" || res.Harness != nil {
		t.Error("rejected run must not produce a proposal")
	}
}

func TestRunAgentFailureKeepsContextNoRejected(t *testing.T) {
	agent := &fakeAgent{errs: []error{errors.New("claude: signal: killed")}}
	opts := runOpts(t)
	opts.Agent = agent.Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"zzbin": "zzbin 1.0\n"}}).Run

	res, err := Run("zzbin", opts)
	if err == nil {
		t.Fatal("expected agent error")
	}
	if res == nil || res.Dir == "" {
		t.Fatal("agent failure must still report the session dir")
	}
	ctx, err := os.ReadFile(filepath.Join(res.Dir, "context.md"))
	if err != nil {
		t.Fatalf("context.md must survive an agent failure: %v", err)
	}
	if !strings.Contains(string(ctx), "claude: signal: killed") {
		t.Errorf("context.md missing the agent failure: %s", ctx)
	}
	if res.RejectedPath != "" {
		t.Error("agent failure is not a rejection: no rejected.txt expected")
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "proposed-catalog.toml")); err == nil {
		t.Error("agent failure must not produce a proposal")
	}
}

func TestRunBinNotOnPathCreatesNothing(t *testing.T) {
	opts := runOpts(t)
	opts.Agent = (&fakeAgent{responses: []string{"unused"}}).Run
	opts.Runner = (&fakeRunner{}).Run
	opts.LookPath = func(string) (string, error) { return "", errors.New("not found") }

	res, err := Run("zzbin", opts)
	if err == nil || res != nil {
		t.Fatalf("expected error without result, got (%+v, %v)", res, err)
	}
	if _, err := os.Stat(filepath.Join(opts.DoctorDir, "history")); !os.IsNotExist(err) {
		t.Error("bin not on PATH must create no files")
	}
}

// --- history ---

func TestNewSessionDirNamingAndPerms(t *testing.T) {
	root := t.TempDir()
	dir, err := NewSessionDir(filepath.Join(root, "history"), fixedTime)
	if err != nil {
		t.Fatalf("NewSessionDir: %v", err)
	}
	if filepath.Base(dir) != "20261028T123456Z" {
		t.Errorf("session dir name = %q, want 20261028T123456Z", filepath.Base(dir))
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", st.Mode().Perm())
	}
}

func TestNewSessionDirCollisionGetsSuffix(t *testing.T) {
	root := t.TempDir()
	first, err := NewSessionDir(filepath.Join(root, "history"), fixedTime)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := NewSessionDir(filepath.Dir(first), fixedTime)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second == first {
		t.Fatalf("collision silently reused %q", first)
	}
	if filepath.Base(second) != "20261028T123456Z-2" {
		t.Errorf("collision dir = %q, want 20261028T123456Z-2", filepath.Base(second))
	}
}

func TestRejectedContent(t *testing.T) {
	c := rejectedContent(errors.New("boom"), []Attempt{{Output: "raw draft"}})
	if !strings.Contains(c, "boom") || !strings.Contains(c, "raw draft") {
		t.Errorf("rejectedContent = %q", c)
	}
}
