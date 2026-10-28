package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// The doctor command's tests never spawn a real agent: AgentFn is always a
// stub, and the agent-availability check is a package var the no-agent test
// swaps. "sh" and "cat" stand in for target bins because they exist on PATH
// on every unix the suite runs on, and no catalog entry names them.

// stubAgent is an AgentFn substitute with queued responses or errors.
type stubAgent struct {
	responses []string
	errs      []error
	calls     int
}

func (s *stubAgent) Run(_ context.Context, _ string) (string, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return "", s.errs[i]
	}
	if i < len(s.responses) {
		return s.responses[i], nil
	}
	return "", errors.New("stubAgent: no response queued")
}

// cannedRunner answers commands from a canned map (full "name args" key
// first, bare name fallback), like the doctor package's fakeRunner.
type cannedRunner struct {
	responses map[string]string
}

func (r cannedRunner) Run(name string, args ...string) (string, error) {
	call := name
	if len(args) > 0 {
		call = name + " " + strings.Join(args, " ")
	}
	if resp, ok := r.responses[call]; ok {
		return resp, nil
	}
	if resp, ok := r.responses[name]; ok {
		return resp, nil
	}
	return "", errors.New("cannedRunner: no response for " + call)
}

// shEntry is a valid native catalog entry for the sh binary, in the fenced
// shape a drafting agent would return.
const shEntry = "```toml\nid = \"sh-probe\"\nname = \"Shell Probe\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"self\"\n```\n"

// shRunner answers both the facts calls (by bin name) and the native probe
// (by expanded path; /bin and /usr/bin both resolve on macOS).
func shRunner() cannedRunner {
	return cannedRunner{responses: map[string]string{
		"sh":          "sh 1.0\n",
		"/bin/sh":     "sh 1.0\n",
		"/usr/bin/sh": "sh 1.0\n",
	}}
}

// wireDoctor equips an App from newTestApp for doctor tests: a stub agent,
// a canned runner, a temp doctor dir, no detected instances, and the
// agent-availability check overridden to succeed so tests pass on machines
// without Claude Code installed.
func wireDoctor(t *testing.T, a *App, agent *stubAgent, runner cannedRunner) {
	t.Helper()
	a.AgentFn = agent.Run
	a.DetectRunner = runner.Run
	a.DoctorDir = filepath.Join(t.TempDir(), "doctor")
	orig := doctorAgentLookPath
	doctorAgentLookPath = func(string) (string, error) { return "/usr/local/bin/claude", nil }
	t.Cleanup(func() { doctorAgentLookPath = orig })
}

// fixtureLoader points the catalog at a fixture with the given bins.
func fixtureLoader(bins ...string) func() ([]catalog.Harness, []catalog.Problem, error) {
	return func() ([]catalog.Harness, []catalog.Problem, error) {
		var hs []catalog.Harness
		for _, b := range bins {
			hs = append(hs, catalog.Harness{
				ID:   b + "-harness",
				Name: b + " harness",
				Installs: []catalog.Install{{
					Method:  "native",
					Bin:     b,
					Paths:   []string{"/bin/" + b},
					Version: catalog.CmdSpec{Cmd: []string{b, "--version"}},
				}},
			})
		}
		return hs, nil, nil
	}
}

func TestDoctorCmd_NoAgentOnPath(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{"must not run"}}, shRunner())
	sd.instances = nil
	orig := doctorAgentLookPath
	doctorAgentLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { doctorAgentLookPath = orig })

	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (no agent)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "doctor needs claude") {
		t.Errorf("stderr = %q, want the doctor-needs-claude message", stderr.String())
	}
	// No files may exist after the failure: the history tree must not
	// have been created.
	if _, err := os.Stat(filepath.Join(a.DoctorDir, "history")); !os.IsNotExist(err) {
		t.Error("no-agent failure created files")
	}
}

func TestDoctorCmd_BinNotOnPath(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "ofa-no-such-bin-xyz"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	sd.instances = nil
	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (bin not on PATH)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "not on PATH") {
		t.Errorf("stderr = %q, want not-on-PATH message", stderr.String())
	}
}

func TestDoctorCmd_AlreadyDetectedExitsWithoutAgent(t *testing.T) {
	a, stdout, _, sd, _, _ := newTestApp(t, []string{"doctor", "claude"}, nil)
	agent := &stubAgent{responses: []string{"must not run"}}
	wireDoctor(t, a, agent, shRunner())
	sd.instances = []detect.Instance{{HarnessID: "claude", Method: "native", Bin: "claude", Status: detect.Active}}
	if code := a.Run(); code != ExitOK {
		t.Errorf("exit = %d, want %d", code, ExitOK)
	}
	if agent.calls != 0 {
		t.Errorf("agent ran %d times for an already-detected bin", agent.calls)
	}
	if !strings.Contains(stdout.String(), "already detected") {
		t.Errorf("stdout = %q, want already-detected message", stdout.String())
	}
}

func TestDoctorCmd_SeveralGapsListedWithoutAgent(t *testing.T) {
	a, stdout, _, sd, _, _ := newTestApp(t, []string{"doctor"}, nil)
	agent := &stubAgent{}
	wireDoctor(t, a, agent, shRunner())
	a.CatalogLoader = fixtureLoader("sh", "cat")
	sd.instances = nil
	if code := a.Run(); code != ExitOK {
		t.Errorf("exit = %d, want %d (gap listing)", code, ExitOK)
	}
	if agent.calls != 0 {
		t.Errorf("agent ran %d times for a gap listing", agent.calls)
	}
	out := stdout.String()
	for _, want := range []string{"catalog gaps", "sh", "cat", "ofa doctor <bin>"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: %s", want, out)
		}
	}
}

func TestDoctorCmd_SingleGapProceedsToProposal(t *testing.T) {
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor"}, nil)
	agent := &stubAgent{responses: []string{shEntry}}
	wireDoctor(t, a, agent, shRunner())
	a.CatalogLoader = fixtureLoader("sh")
	sd.instances = nil

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
	if agent.calls != 1 {
		t.Errorf("agent calls = %d, want 1", agent.calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "one gap: sh") || !strings.Contains(out, "proposal ready") {
		t.Errorf("stdout = %q, want gap announcement and proposal-ready", out)
	}
	// The proposal must exist in the single session directory, next to
	// context.md.
	history := filepath.Join(a.DoctorDir, "history")
	entries, err := os.ReadDir(history)
	if err != nil || len(entries) != 1 {
		t.Fatalf("history dirs = %v, err = %v, want exactly one session", entries, err)
	}
	dir := filepath.Join(history, entries[0].Name())
	for _, name := range []string{"proposed-catalog.toml", "context.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("session missing %s: %v", name, err)
		}
	}
}

func TestDoctorCmd_ExplicitBinProducesProposal(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	agent := &stubAgent{responses: []string{shEntry}}
	wireDoctor(t, a, agent, shRunner())
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proposal ready") {
		t.Errorf("stdout = %q, want proposal-ready", stdout.String())
	}
}

func TestDoctorCmd_RejectedAfterRetry(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	agent := &stubAgent{responses: []string{"not toml", "still not toml"}}
	wireDoctor(t, a, agent, shRunner())
	sd.instances = nil

	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (rejected)", code, ExitError)
	}
	if agent.calls != 2 {
		t.Errorf("agent calls = %d, want 2 (initial + one retry)", agent.calls)
	}
	if !strings.Contains(stderr.String(), "rejected") {
		t.Errorf("stderr = %q, want rejection report", stderr.String())
	}
	// Both evidence files must exist in the session directory.
	if rejected := findSessionFile(t, a.DoctorDir, "rejected.txt"); !strings.Contains(rejected, "still not toml") {
		t.Errorf("rejected.txt = %q, want the last raw output", rejected)
	}
	findSessionFile(t, a.DoctorDir, "context.md")
}

func TestDoctorCmd_AgentFailureKeepsAuditTrail(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	agent := &stubAgent{errs: []error{errors.New("claude: signal: killed")}}
	wireDoctor(t, a, agent, shRunner())
	sd.instances = nil

	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (agent failure)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "agent run failed") {
		t.Errorf("stderr = %q, want the agent failure", stderr.String())
	}
	findSessionFile(t, a.DoctorDir, "context.md")
}

func TestDoctorCmd_AgentFlagClaudeOnly(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--agent", "codex"}, nil)
	agent := &stubAgent{}
	wireDoctor(t, a, agent, shRunner())
	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (--agent codex)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "claude only") {
		t.Errorf("stderr = %q, want claude-only message", stderr.String())
	}
	if agent.calls != 0 {
		t.Errorf("agent ran %d times for a rejected --agent value", agent.calls)
	}
}

func TestDoctorCmd_DetectFailurePropagates(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	agent := &stubAgent{}
	wireDoctor(t, a, agent, shRunner())
	sd.err = errors.New("scan exploded")
	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d (detect failure)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "scan exploded") {
		t.Errorf("stderr = %q, want the scan failure", stderr.String())
	}
}

// findSessionFile reads name from the single session directory under the
// doctor history root and returns its content.
func findSessionFile(t *testing.T, doctorDir, name string) string {
	t.Helper()
	history := filepath.Join(doctorDir, "history")
	entries, err := os.ReadDir(history)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no session directory under %s (err %v)", history, err)
	}
	dir := filepath.Join(history, entries[0].Name())
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// A3: the usage text implies `ofa doctor <bin> --agent claude`, but Go's
// flag package stops at the first positional, so that order used to fail
// with a bogus too-many-arguments error. permuteArgs fixes the order.
func TestDoctorCmd_FlagsAfterPositional(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh", "--agent", "claude"}, nil)
	agent := &stubAgent{responses: []string{shEntry}}
	wireDoctor(t, a, agent, shRunner())
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proposal ready") {
		t.Errorf("stdout = %q, want proposal-ready", stdout.String())
	}
}

func TestDoctorCmd_FlagRejectedAfterPositional(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh", "--agent", "codex"}, nil)
	agent := &stubAgent{}
	wireDoctor(t, a, agent, shRunner())
	if code := a.Run(); code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "claude only") {
		t.Errorf("stderr = %q, want the claude-only message", stderr.String())
	}
	if strings.Contains(stderr.String(), "expected at most one") {
		t.Error("flag after positional was misparsed as an extra positional")
	}
}

func TestPermuteArgs(t *testing.T) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.String("agent", "claude", "")
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flag after positional", []string{"sh", "--agent", "claude"}, []string{"--agent", "claude", "sh"}},
		{"flag before positional", []string{"--agent", "claude", "sh"}, []string{"--agent", "claude", "sh"}},
		{"flag with = value", []string{"sh", "--agent=claude"}, []string{"--agent=claude", "sh"}},
		{"positional only", []string{"sh"}, []string{"sh"}},
		{"two positionals preserved", []string{"sh", "extra"}, []string{"sh", "extra"}},
		{"dashdash preserved", []string{"--", "sh"}, []string{"--", "sh"}},
		{"dashdash escapes a flag-looking bin", []string{"--", "--agent=codex"}, []string{"--", "--agent=codex"}},
		{"missing value stays flag", []string{"--agent"}, []string{"--agent"}},
	}
	for _, c := range cases {
		if got := permuteArgs(fs, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: permuteArgs(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
