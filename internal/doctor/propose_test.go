package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// fakeAgent is an AgentRunner substitute: queued responses (or errors), one
// per call, recording every prompt so tests can assert retry behavior. No
// test ever spawns a real agent.
type fakeAgent struct {
	responses []string
	errs      []error
	prompts   []string
	n         int
}

func (a *fakeAgent) Run(_ context.Context, prompt string) (string, error) {
	a.prompts = append(a.prompts, prompt)
	i := a.n
	a.n++
	if i < len(a.errs) && a.errs[i] != nil {
		return "", a.errs[i]
	}
	if i < len(a.responses) {
		return a.responses[i], nil
	}
	return "", errors.New("fakeAgent: no response queued")
}

func (a *fakeAgent) calls() int { return a.n }

// fakeRunner is a detect.Runner substitute: canned stdout keyed by command
// name, recording every invocation (name + args) so tests can prove the
// shape gate ran before anything executed.
type fakeRunner struct {
	calls     []string
	responses map[string]string
}

func (r *fakeRunner) Run(name string, args ...string) (string, error) {
	call := name
	if len(args) > 0 {
		call = name + " " + strings.Join(args, " ")
	}
	r.calls = append(r.calls, call)
	// Full "name args" key first, so --version and --help can answer
	// differently; bare name as fallback for any argument shape.
	if resp, ok := r.responses[call]; ok {
		return resp, nil
	}
	if resp, ok := r.responses[name]; ok {
		return resp, nil
	}
	return "", fmt.Errorf("fakeRunner: no canned response for %q", call)
}

// foundOnPath is a LookPath substitute that always succeeds.
func foundOnPath(bin string) (string, error) { return "/bin/" + bin, nil }

// fixedTime pins session directory names in tests.
var fixedTime = time.Date(2026, 10, 28, 12, 34, 56, 0, time.UTC)

// nativeEntryFor returns a minimal valid native entry for bin probing path.
func nativeEntryFor(bin, path string) string {
	return fmt.Sprintf("id = %q\nname = %q\n\n[[installs]]\nmethod = \"native\"\nbin = %q\npaths = [%q]\nversion = { cmd = [%q, \"--version\"] }\nlatest = \"self\"\n", bin+"-probe", bin+" probe", bin, path, bin)
}

func instanceForBin(bin, harnessID string) detect.Instance {
	return detect.Instance{HarnessID: harnessID, Method: "native", Bin: bin, Status: detect.Active}
}

// --- gaps ---

func harnessWithBin(id string, bins ...string) catalog.Harness {
	h := catalog.Harness{ID: id, Name: id}
	for _, b := range bins {
		h.Installs = append(h.Installs, catalog.Install{Method: "native", Bin: b, Version: catalog.CmdSpec{Cmd: []string{b, "--version"}}})
	}
	return h
}

func TestGapsUndetectedBinOnPathIsAGap(t *testing.T) {
	cat := []catalog.Harness{harnessWithBin("aaa", "aaa-bin"), harnessWithBin("bbb", "shared-bin")}
	gaps := Gaps(cat, nil, foundOnPath)
	if len(gaps) != 2 {
		t.Fatalf("gaps = %d, want 2: %+v", len(gaps), gaps)
	}
	if gaps[0].Bin != "aaa-bin" || gaps[1].Bin != "shared-bin" {
		t.Errorf("gaps not sorted by bin: %+v", gaps)
	}
	if gaps[0].HarnessIDs[0] != "aaa" {
		t.Errorf("aaa-bin harness ids = %v, want [aaa]", gaps[0].HarnessIDs)
	}
}

func TestGapsSharedBinReportsEveryHarness(t *testing.T) {
	cat := []catalog.Harness{harnessWithBin("zzz", "shared-bin"), harnessWithBin("aaa", "shared-bin")}
	gaps := Gaps(cat, nil, foundOnPath)
	if len(gaps) != 1 {
		t.Fatalf("gaps = %d, want 1 deduplicated gap", len(gaps))
	}
	if got := gaps[0].HarnessIDs; len(got) != 2 || got[0] != "aaa" || got[1] != "zzz" {
		t.Errorf("shared-bin harness ids = %v, want [aaa zzz]", got)
	}
}

func TestGapsDetectedBinIsNotAGap(t *testing.T) {
	cat := []catalog.Harness{harnessWithBin("aaa", "aaa-bin")}
	insts := []detect.Instance{instanceForBin("aaa-bin", "aaa")}
	if gaps := Gaps(cat, insts, foundOnPath); len(gaps) != 0 {
		t.Errorf("detected bin reported as gap: %+v", gaps)
	}
}

func TestGapsBinNotOnPathIsNotAGap(t *testing.T) {
	cat := []catalog.Harness{harnessWithBin("aaa", "aaa-bin")}
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	if gaps := Gaps(cat, nil, notOnPath); len(gaps) != 0 {
		t.Errorf("bin absent from PATH reported as gap: %+v", gaps)
	}
}

// --- facts ---

func TestCollectFactsTruncatesHelp(t *testing.T) {
	long := strings.Repeat("line of help output that is quite long indeed\n", 200)
	run := &fakeRunner{responses: map[string]string{
		"zzbin --version": "zzbin 1.2.3\nsecond line\n",
		"zzbin --help":    long,
	}}
	f := CollectFacts("zzbin", "/bin/zzbin", run.Run)
	if f.VersionOut != "zzbin 1.2.3\nsecond line" {
		t.Errorf("version output = %q, want first 2 lines of 4-line cap", f.VersionOut)
	}
	if got := len(f.HelpOut); got > helpMax {
		t.Errorf("help output = %d bytes, want <= %d", got, helpMax)
	}
	if lines := strings.Count(f.HelpOut, "\n") + 1; lines > helpLines {
		t.Errorf("help output = %d lines, want <= %d", lines, helpLines)
	}
	if !strings.Contains(f.HelpOut, "…[truncated]") {
		t.Errorf("truncated help output lacks truncation marker: %q", f.HelpOut)
	}
	if f.VersionErr != "" || f.HelpErr != "" {
		t.Errorf("unexpected command errors: %q / %q", f.VersionErr, f.HelpErr)
	}
}

func TestCollectFactsRecordsCommandFailures(t *testing.T) {
	run := &fakeRunner{} // no responses: every command fails
	f := CollectFacts("zzbin", "/bin/zzbin", run.Run)
	if f.VersionErr == "" || f.HelpErr == "" {
		t.Errorf("expected recorded failures, got %q / %q", f.VersionErr, f.HelpErr)
	}
	if f.VersionOut != "" || f.HelpOut != "" {
		t.Errorf("failed commands must not produce output: %q / %q", f.VersionOut, f.HelpOut)
	}
}

func TestCollectFactsResolvesSymlinks(t *testing.T) {
	run := &fakeRunner{responses: map[string]string{"sh": "sh 1.0\n"}}
	f := CollectFacts("sh", "/bin/sh", run.Run)
	if f.RealPath == "" {
		t.Error("real path is empty")
	}
	// /bin -> /usr/bin on macOS: the resolved path must differ or match, but
	// always be an absolute path that EvalSymlinks accepted.
	if !strings.HasPrefix(f.RealPath, "/") {
		t.Errorf("real path = %q, want absolute", f.RealPath)
	}
}

func TestFactsStringRendersAllSections(t *testing.T) {
	f := Facts{
		Bin: "zzbin", Path: "/bin/zzbin", RealPath: "/npm/node_modules/zzbin/bin/zzbin",
		VersionOut: "zzbin 1.0", HelpOut: "usage: zzbin",
	}
	s := f.String()
	for _, want := range []string{"zzbin", "/bin/zzbin", "/npm/node_modules/zzbin/bin/zzbin", "zzbin 1.0", "usage: zzbin", "--version output", "--help output"} {
		if !strings.Contains(s, want) {
			t.Errorf("facts rendering missing %q:\n%s", want, s)
		}
	}
}

// --- prompt ---

func TestBuildPromptContent(t *testing.T) {
	f := Facts{Bin: "zzbin", Path: "/bin/zzbin", RealPath: "/usr/local/Cellar/zzbin/1.0/bin/zzbin", VersionOut: "zzbin 1.0"}
	p := BuildPrompt("zzbin", f)
	for _, want := range []string{
		"zzbin",
		untrustedStart, untrustedStop,
		"not instructions",
		"/node_modules/<package>/", "/Cellar/<formula>/", "/Caskroom/<cask>/", "/pipx/venvs/<package>/", "/uv/tools/<package>/",
		"Claude Code", "Kimi Code",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// The schema's valid values must come from the catalog package itself.
	for _, m := range catalog.ValidMethods() {
		if !strings.Contains(p, m) {
			t.Errorf("prompt missing valid method %q", m)
		}
	}
	for _, l := range catalog.ValidLatest() {
		if !strings.Contains(p, l) {
			t.Errorf("prompt missing valid latest %q", l)
		}
	}
	// The facts travel only inside the untrusted block.
	if !strings.Contains(p, "/usr/local/Cellar/zzbin/1.0/bin/zzbin") {
		t.Error("prompt missing real path fact")
	}
}

func TestBuildPromptFactsInsideUntrustedBlock(t *testing.T) {
	f := Facts{Bin: "zzbin", VersionOut: "zzbin 1.0"}
	p := BuildPrompt("zzbin", f)
	// The explanatory sentence names both markers, so anchor on the
	// standalone block markers: the real start is the marker on its own
	// line, the real stop the last occurrence.
	start := strings.Index(p, untrustedStart+"\n")
	stop := strings.LastIndex(p, untrustedStop)
	if start < 0 || stop < start {
		t.Fatalf("untrusted markers missing or out of order in prompt")
	}
	block := p[start:stop]
	if !strings.Contains(block, "zzbin 1.0") {
		t.Error("facts not inside the untrusted block")
	}
	before := p[:start]
	if strings.Contains(before, "zzbin 1.0") {
		t.Error("fact leaked outside the untrusted block")
	}
}

// --- propose: fence extraction ---

func TestExtractTOML(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"fenced", "prose\n```toml\nid = \"x\"\n```\nmore prose", "id = \"x\""},
		{"no fence", "id = \"x\"", "id = \"x\""},
		{"first fence wins", "```toml\nfirst\n```\n```toml\nsecond\n```", "first"},
		{"unterminated fence", "```toml\nid = \"x\"", "id = \"x\""},
		{"only prose", "I cannot help with that.", "I cannot help with that."},
	}
	for _, c := range cases {
		if got := ExtractTOML(c.in); got != c.want {
			t.Errorf("%s: ExtractTOML = %q, want %q", c.name, got, c.want)
		}
	}
}

// --- propose: the shape gate runs before anything executes (R1) ---

func gateRejectedBeforeExecution(t *testing.T, draft, bin string) {
	t.Helper()
	run := &fakeRunner{}
	_, _, err := ValidateProposal(draft, bin, run.Run)
	if err == nil {
		t.Fatal("draft accepted, want rejection")
	}
	if len(run.calls) != 0 {
		t.Fatalf("shape gate let a command execute: runner saw %v", run.calls)
	}
}

func TestValidateProposalRejectsForeignVersionCmdBeforeExecution(t *testing.T) {
	draft := "id = \"evil\"\nname = \"Evil\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"zztarget\"\npaths = [\"/bin/zztarget\"]\nversion = { cmd = [\"sh\", \"-c\", \"echo pwned\"] }\nlatest = \"self\"\n"
	gateRejectedBeforeExecution(t, draft, "zztarget")
}

func TestValidateProposalRejectsShellMetacharactersBeforeExecution(t *testing.T) {
	draft := "id = \"evil\"\nname = \"Evil\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"zztarget\"\npaths = [\"/bin/zztarget\"]\nversion = { cmd = [\"zztarget\", \"--flag;rm -rf /\"] }\nlatest = \"self\"\n"
	gateRejectedBeforeExecution(t, draft, "zztarget")
}

func TestValidateProposalRejectsForeignNativePathBeforeExecution(t *testing.T) {
	draft := "id = \"evil\"\nname = \"Evil\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"zztarget\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"zztarget\", \"--version\"] }\nlatest = \"self\"\n"
	gateRejectedBeforeExecution(t, draft, "zztarget")
}

func TestValidateProposalRejectsBinMismatchBeforeExecution(t *testing.T) {
	draft := "id = \"evil\"\nname = \"Evil\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"otherbin\"\npaths = [\"/bin/otherbin\"]\nversion = { cmd = [\"otherbin\", \"--version\"] }\nlatest = \"self\"\n"
	gateRejectedBeforeExecution(t, draft, "zztarget")
}

func TestValidateProposalRejectsUnknownKey(t *testing.T) {
	draft := "id = \"x\"\nname = \"X\"\nlates = \"npm\"\n\n[[installs]]\nmethod = \"npm\"\npackage = \"x\"\nbin = \"zztarget\"\nversion = { cmd = [\"zztarget\", \"--version\"] }\n"
	gateRejectedBeforeExecution(t, draft, "zztarget")
}

// --- propose: the correctness gate ---

func TestValidateProposalAcceptsDetectingNativeEntry(t *testing.T) {
	run := &fakeRunner{responses: map[string]string{
		"sh":          "sh 1.0\n",
		"/bin/sh":     "sh 1.0\n", // detectNative probes by expanded path
		"/usr/bin/sh": "sh 1.0\n",
	}}
	h, tomlSrc, err := ValidateProposal(nativeEntryFor("sh", "/bin/sh"), "sh", run.Run)
	if err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	if h.ID != "sh-probe" {
		t.Errorf("harness id = %q, want sh-probe", h.ID)
	}
	if tomlSrc == "" || !strings.Contains(tomlSrc, `bin = "sh"`) {
		t.Errorf("extracted toml = %q", tomlSrc)
	}
}

func TestValidateProposalRejectsEntryThatDoesNotDetect(t *testing.T) {
	// Parses, validates, and passes the shape gate — but the native probe
	// path does not exist, so the real detect path finds nothing. This is
	// the key correctness gate: a syntactically perfect draft is worthless
	// if it cannot see the binary.
	run := &fakeRunner{}
	_, _, err := ValidateProposal(nativeEntryFor("zztarget", "/nonexistent/zztarget"), "zztarget", run.Run)
	if err == nil {
		t.Fatal("non-detecting entry accepted")
	}
	if !strings.Contains(err.Error(), "does not detect") {
		t.Errorf("error = %v, want does-not-detect reason", err)
	}
}

func TestValidateProposalDetectsViaRegistry(t *testing.T) {
	draft := "id = \"reg\"\nname = \"Reg\"\n\n[[installs]]\nmethod = \"npm\"\npackage = \"somepkg\"\nbin = \"zztarget\"\nversion = { cmd = [\"zztarget\", \"--version\"] }\nlatest = \"npm\"\n"
	run := &fakeRunner{responses: map[string]string{
		"npm": `{"dependencies":{"somepkg":{"version":"1.2.3"}}}`,
	}}
	_, _, err := ValidateProposal(draft, "zztarget", run.Run)
	if err != nil {
		t.Fatalf("registry-detecting entry rejected: %v", err)
	}
	for _, c := range run.calls {
		if !strings.HasPrefix(c, "npm ls -g") {
			t.Errorf("registry detection ran unexpected command %q", c)
		}
	}
}

// An id that collides with an embedded id must still validate: the collision
// is an override the install flow reports, not a defect of the entry.
func TestValidateProposalEmbeddedIDCollisionStillDetects(t *testing.T) {
	run := &fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n"}}
	draft := strings.Replace(nativeEntryFor("sh", "/bin/sh"), `id = "sh-probe"`, `id = "claude"`, 1)
	if _, _, err := ValidateProposal(draft, "sh", run.Run); err != nil {
		t.Errorf("embedded-id collision rejected: %v", err)
	}
}
