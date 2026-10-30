package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ofa/internal/catalog"
	"ofa/internal/detect"
	"ofa/internal/doctor"
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

// --- PR C: review, confirm, install ---

// shEntryUpdate is shEntry plus an update command that is deliberately
// non-standard (sh -c ...), so the review must both show it verbatim and
// flag it.
const shEntryUpdate = "```toml\nid = \"sh-probe\"\nname = \"Shell Probe\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"self\"\nupdate = { cmd = [\"sh\", \"-c\", \"echo hi there\"] }\n```\n"

// lineFeeder serves one line per Read, the way a terminal's line
// discipline does. A plain strings.Reader does not work for sequences of
// prompts: bufio.Scanner read-ahead would swallow every queued answer on
// the first prompt, leaving the second one at EOF (a spurious decline).
type lineFeeder struct {
	lines []string
	i     int
}

func (f *lineFeeder) Read(p []byte) (int, error) {
	if f.i >= len(f.lines) {
		return 0, io.EOF
	}
	n := copy(p, f.lines[f.i]+"\n")
	f.i++
	return n, nil
}

// makeInteractive points Stdin at the given answers (one per prompt) and
// makes StdinIsTTY report a terminal, so confirm prompts can be driven
// from a table.
func makeInteractive(a *App, answers ...string) {
	a.Stdin = &lineFeeder{lines: answers}
	a.StdinIsTTY = func(io.Reader) bool { return true }
}

// shInstance is a detected sh, as the post-install scan should find it.
func shInstance() detect.Instance {
	return detect.Instance{HarnessID: "sh-probe", Method: "native", Bin: "sh", Status: detect.Active, Version: "1.0"}
}

// detectingAfterInstall swaps DetectFn for one that reports sh only once
// a doctor session exists: the initial scan (which runs before any
// session directory is created) sees nothing — doctor has work to do —
// and the post-install scan sees the win. The catalog file itself cannot
// be the trigger: the overwrite tests pre-create it.
func detectingAfterInstall(a *App) {
	a.DetectFn = func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		if _, err := os.Stat(filepath.Join(a.DoctorDir, "history")); err == nil {
			return []detect.Instance{shInstance()}, nil, nil
		}
		return nil, nil, nil
	}
}

func TestDoctorCmd_ReviewBlockShownVerbatim(t *testing.T) {
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntryUpdate}}, shRunner())
	sd.instances = nil

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"harness id: sh-probe",
		"name: Shell Probe",
		"method: native",
		"bin: sh",
		"version command: sh --version",
		"update command: sh -c 'echo hi there'",
		"non-standard update command, read it carefully",
		"ofa apply will run the update command above",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("review block missing %q:\n%s", want, out)
		}
	}
	// Non-TTY stdin: no prompt, no install, the --install command instead.
	if strings.Contains(stderr.String(), "Install to") {
		t.Errorf("non-TTY stdin was prompted: %s", stderr.String())
	}
	if !strings.Contains(out, "ofa doctor --install ") {
		t.Errorf("stdout missing the --install command:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("non-TTY run installed into the user catalog")
	}
}

// claudeEntry drafts an entry whose id is a real embedded id (claude),
// for the same target bin sh: the proposal must be called out as an
// override of the embedded entry.
const claudeEntry = "```toml\nid = \"claude\"\nname = \"Claude (local draft)\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"self\"\n```\n"

func TestDoctorCmd_OverridesEmbeddedEntry(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{claudeEntry}}, shRunner())
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`overrides embedded "claude"`,
		"user catalog wins by id",
		`name "Claude Code"`,
		`name "Claude (local draft)"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("override block missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorCmd_ConfirmDeclineWritesNothing(t *testing.T) {
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "n")
	sd.instances = nil

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Install to") || !strings.Contains(stderr.String(), "[y/N]") {
		t.Errorf("stderr missing the prompt: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "declined; nothing was written") {
		t.Errorf("stdout missing the decline notice: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("declined confirm still installed")
	}
	// The session stays proposed: install later is still possible.
	if rec, err := readSessionRecord(t, a.DoctorDir); err != nil || rec.Status != "proposed" {
		t.Errorf("session record = %+v, %v; want status proposed", rec, err)
	}
}

func TestDoctorCmd_ConfirmYesInstalls(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y")
	detectingAfterInstall(a)

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	// Default answer is No: only an explicit y installs.
	path := filepath.Join(a.CatalogDir, "sh-probe.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("catalog entry not written: %v", err)
	}
	// The written bytes are the proposal's exact TOML.
	proposal := findSessionFile(t, a.DoctorDir, "proposed-catalog.toml")
	if string(b) != proposal {
		t.Errorf("installed content = %q, want the proposal's %q", string(b), proposal)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", st.Mode().Perm())
	}
	if !strings.Contains(stdout.String(), "installed: ") {
		t.Errorf("stdout missing the installed line: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "detected: sh 1.0 (sh-probe, native)") {
		t.Errorf("stdout missing the post-install detection: %s", stdout.String())
	}
	rec, err := readSessionRecord(t, a.DoctorDir)
	if err != nil || rec.Status != "installed" || rec.InstalledPath != path || rec.Target != "sh" {
		t.Errorf("session record = %+v, %v; want installed with the path", rec, err)
	}
}

func TestDoctorCmd_InstalledButNotDetectedReports(t *testing.T) {
	a, _, _, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y")
	// The post-install scan never finds sh.
	a.DetectFn = func([]catalog.Harness, detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		return nil, nil, nil
	}
	sd.instances = nil

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (not detected after install)", code, ExitError)
	}
	// The file stays in place, but the miss is said plainly.
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); err != nil {
		t.Errorf("entry should stay in place after a detection miss: %v", err)
	}
}

func TestDoctorCmd_OverwriteNeedsSecondConfirm(t *testing.T) {
	existing := "id = \"sh-probe\"\nname = \"Mine\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\n"
	path := func(a *App) string { return filepath.Join(a.CatalogDir, "sh-probe.toml") }

	// Decline the overwrite: the user's file is untouched.
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y", "n")
	if err := os.MkdirAll(a.CatalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(a), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	detectingAfterInstall(a)
	sd.instances = nil
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists") || !strings.Contains(stderr.String(), "Overwrite") {
		t.Errorf("stderr missing the overwrite warning: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "left untouched") {
		t.Errorf("stdout missing the untouched notice: %s", stdout.String())
	}
	b, _ := os.ReadFile(path(a))
	if string(b) != existing {
		t.Errorf("existing file was modified: %q", string(b))
	}

	// Confirm the overwrite: the proposal replaces it.
	a2, _, _, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a2, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a2, "y", "y")
	if err := os.MkdirAll(a2.CatalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path(a2), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	detectingAfterInstall(a2)
	if code := a2.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	b, _ = os.ReadFile(path(a2))
	if string(b) == existing {
		t.Error("overwrite confirm did not replace the file")
	}
}

func TestDoctorCmd_DuplicateIDNeedsSecondConfirm(t *testing.T) {
	other := "id = \"sh-probe\"\nname = \"Old\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\n"
	decline := func(t *testing.T, answers ...string) (stdout, stderr *bytes.Buffer, installed bool) {
		t.Helper()
		a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
		wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
		makeInteractive(a, answers...)
		if err := os.MkdirAll(a.CatalogDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.CatalogDir, "a-old.toml"), []byte(other), 0o600); err != nil {
			t.Fatal(err)
		}
		detectingAfterInstall(a)
		sd.instances = nil
		code := a.Run()
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
		}
		_, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml"))
		return stdout, stderr, err == nil
	}
	stdout, stderr, installed := decline(t, "y", "n")
	if installed {
		t.Error("duplicate-id warning was not enough to stop the install")
	}
	if !strings.Contains(stderr.String(), "a-old.toml") || !strings.Contains(stderr.String(), "already define harness id") {
		t.Errorf("stderr missing the duplicate warning: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "declined") {
		t.Errorf("stdout missing the decline notice: %s", stdout.String())
	}
	_, _, installed = decline(t, "y", "y")
	if !installed {
		t.Error("explicit confirm did not install past the duplicate warning")
	}
}

// --- --install ---

// seedSession writes a history session directory by hand: session.json and
// the proposed TOML. The TOML is what --install revalidates, so tests can
// tamper with it.
func seedSession(t *testing.T, doctorDir, id, target, status string, proposal string) string {
	t.Helper()
	dir := filepath.Join(doctorDir, "history", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := doctor.SessionRecord{
		Target:    target,
		HarnessID: "sh-probe",
		Time:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:    status,
	}
	if err := doctor.SaveSessionRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	if proposal != "" {
		if err := os.WriteFile(filepath.Join(dir, "proposed-catalog.toml"), []byte(proposal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// bareToml is shEntry's TOML without the code fence.
const bareToml = "id = \"sh-probe\"\nname = \"Shell Probe\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"self\"\n"

func TestDoctorInstallCmd_HappyPath(t *testing.T) {
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)
	sd.instances = []detect.Instance{shInstance()}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"revalidated proposal from session 20260101T000000Z", "harness id: sh-probe", "Install to"} {
		_ = want
	}
	if !strings.Contains(out, "revalidated proposal") || !strings.Contains(out, "harness id: sh-probe") {
		t.Errorf("stdout missing the review flow:\n%s", out)
	}
	if !strings.Contains(out, "installed: ") || !strings.Contains(out, "detected: sh 1.0") {
		t.Errorf("stdout missing the install result:\n%s", out)
	}
	b, err := os.ReadFile(filepath.Join(a.CatalogDir, "sh-probe.toml"))
	if err != nil || string(b) != bareToml {
		t.Errorf("installed content = %q, %v", string(b), err)
	}
	rec, err := doctor.LoadSessionRecord(filepath.Join(a.DoctorDir, "history", "20260101T000000Z"))
	if err != nil || rec.Status != "installed" {
		t.Errorf("session record = %+v, %v; want installed", rec, err)
	}
}

func TestDoctorInstallCmd_RevalidationRejectsTamperedProposal(t *testing.T) {
	// The recorded target is sh, but the file now describes cat: the shape
	// gate must catch it, and nothing may be written.
	tampered := "id = \"cat-probe\"\nname = \"Cat Probe\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"cat\"\npaths = [\"/bin/cat\"]\nversion = { cmd = [\"cat\", \"--version\"] }\nlatest = \"self\"\n"
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	dir := seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", tampered)

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (tampered proposal)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "no longer valid") {
		t.Errorf("stderr missing the rejection reason: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "cat-probe.toml")); !os.IsNotExist(err) {
		t.Error("tampered proposal was installed")
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("tampered proposal was installed under the recorded id")
	}
	rec, err := doctor.LoadSessionRecord(dir)
	if err != nil || rec.Status != "rejected" {
		t.Errorf("session record = %+v, %v; want rejected", rec, err)
	}
}

func TestDoctorInstallCmd_TargetGoneFromPath(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	seedSession(t, a.DoctorDir, "20260101T000000Z", "ofa-no-such-bin-xyz", "proposed", bareToml)

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (target gone)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "not on PATH anymore") {
		t.Errorf("stderr missing the target-gone reason: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("install wrote despite a missing target")
	}
}

func TestDoctorInstallCmd_MissingSessionRecord(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	// A PR B session: proposal, but no session.json.
	dir := seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)
	if err := os.Remove(filepath.Join(dir, "session.json")); err != nil {
		t.Fatal(err)
	}
	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (no session record)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "cannot install") {
		t.Errorf("stderr missing the refusal: %s", stderr.String())
	}
}

func TestDoctorInstallCmd_NonTTYRefuses(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)
	sd.instances = nil
	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (non-interactive)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Errorf("stderr missing the interactive-only notice: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("non-interactive --install wrote a file")
	}
}

// --- --list and usage ---

func TestDoctorListCmd(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--list"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)
	// An installed session with its path.
	dir := seedSession(t, a.DoctorDir, "20260102T000000Z", "cat", "installed", bareToml)
	rec, err := doctor.LoadSessionRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.InstalledPath = "/x/catalog/sh-probe.toml"
	if err := doctor.SaveSessionRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	// A PR B session with no session.json: unknown, not an error.
	prb := seedSession(t, a.DoctorDir, "20260103T000000Z", "sh", "proposed", bareToml)
	if err := os.Remove(filepath.Join(prb, "session.json")); err != nil {
		t.Fatal(err)
	}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"doctor sessions:",
		"20260101T000000Z  sh  proposed",
		"20260102T000000Z  cat  installed  -> /x/catalog/sh-probe.toml",
		"20260103T000000Z  unknown  unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q:\n%s", want, out)
		}
	}

	// No history at all: a quiet success, not an error.
	a2, stdout2, _, _, _, _ := newTestApp(t, []string{"doctor", "--list"}, nil)
	wireDoctor(t, a2, &stubAgent{}, shRunner())
	if code := a2.Run(); code != ExitOK {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout2.String(), "no doctor sessions yet") {
		t.Errorf("stdout = %q, want the empty notice", stdout2.String())
	}
}

func TestDoctorCmd_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"install and list", []string{"doctor", "--install", "20260101T000000Z", "--list"}, "mutually exclusive"},
		{"install and bin", []string{"doctor", "--install", "20260101T000000Z", "sh"}, "mutually exclusive"},
		{"list and bin", []string{"doctor", "--list", "sh"}, "mutually exclusive"},
		{"bad session id", []string{"doctor", "--install", "../evil"}, "invalid session id"},
		{"traversal session id", []string{"doctor", "--install", "20260101T000000Z-../../x"}, "invalid session id"},
	}
	for _, c := range cases {
		a, _, stderr, _, _, _ := newTestApp(t, c.args, nil)
		wireDoctor(t, a, &stubAgent{}, shRunner())
		if code := a.Run(); code != ExitError {
			t.Errorf("%s: exit = %d, want %d (usage)", c.name, code, ExitError)
		}
		if !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: stderr = %q, want %q", c.name, stderr.String(), c.want)
		}
	}
}

// readSessionRecord loads session.json from the single session directory.
func readSessionRecord(t *testing.T, doctorDir string) (doctor.SessionRecord, error) {
	t.Helper()
	history := filepath.Join(doctorDir, "history")
	entries, err := os.ReadDir(history)
	if err != nil || len(entries) == 0 {
		return doctor.SessionRecord{}, fmt.Errorf("no session dir under %s (%v)", history, err)
	}
	return doctor.LoadSessionRecord(filepath.Join(history, entries[0].Name()))
}
