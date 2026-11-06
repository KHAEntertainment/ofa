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

// registryFixtureLoader returns a catalog with one harness installed two
// ways: through npm (which answers) and through brew (which can hang).
// The bins are names that are not on PATH, so the gap list is empty
// whatever the machine running the suite looks like.
//
// One harness, not two: a harness that some detector DID find is not
// reported as undetectable, so the failed brew method is reported once
// and only once.
func registryFixtureLoader() func() ([]catalog.Harness, []catalog.Problem, error) {
	return func() ([]catalog.Harness, []catalog.Problem, error) {
		return []catalog.Harness{
			{
				ID:   "tool",
				Name: "tool",
				Installs: []catalog.Install{
					{
						Method:  "npm",
						Package: "ofa-npm-tool",
						Bin:     "ofa-npm-tool-bin",
						Version: catalog.CmdSpec{Cmd: []string{"ofa-npm-tool-bin", "--version"}},
					},
					{
						Method:  "brew-formula",
						Package: "ofa-brew-tool",
						Bin:     "ofa-brew-tool-bin",
						Version: catalog.CmdSpec{Cmd: []string{"ofa-brew-tool-bin", "--version"}},
					},
				},
			},
		}, nil, nil
	}
}

// hungBrewCommand answers npm and hangs on brew until its deadline.
func hungBrewCommand() detect.Command {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		switch name {
		case "brew":
			<-ctx.Done()
			return "", ctx.Err()
		case "npm":
			return `{"dependencies":{"ofa-npm-tool":{"version":"1.2.3"}}}`, nil
		}
		return "", errors.New("hungBrewCommand: no response for " + name)
	}
}

// A hung registry query must degrade into a failure for that method:
// doctor prints which method timed out, and the gap check still
// completes with the installs it did get.
func TestDoctorCmd_GapCheckTimesOutAHungRegistry(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor"}, nil)
	agent := &stubAgent{}
	wireDoctor(t, a, agent, shRunner())
	a.CatalogLoader = registryFixtureLoader()
	// The real scan, over a runner whose commands have a deadline.
	a.DetectFn = detect.Scan
	a.DetectRunner = detect.TimeoutRunner(hungBrewCommand(), 50*time.Millisecond)

	start := time.Now()
	code := a.Run()
	elapsed := time.Since(start)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (the gap check continues); stderr: %s", code, ExitOK, stderr.String())
	}
	// The failure is reported once, naming the method and the reason, in
	// the shared format every command uses.
	if got := strings.Count(stderr.String(), "brew-formula"); got != 1 {
		t.Errorf("stderr names brew-formula %d times, want 1: %s", got, stderr.String())
	}
	if got := strings.Count(stderr.String(), "timed out"); got != 1 {
		t.Errorf("stderr reports the timeout %d times, want 1: %s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "brew-formula detector failed: timed out") {
		t.Errorf("stderr = %q, want the failed method and its reason", stderr.String())
	}
	// The method that answered is not reported as failed.
	if strings.Contains(stderr.String(), "npm detector failed") {
		t.Errorf("stderr = %q, want the healthy method left alone", stderr.String())
	}
	// The gap check completed with what it had.
	if !strings.Contains(stdout.String(), "no gaps") {
		t.Errorf("stdout = %q, want the gap check to complete", stdout.String())
	}
	if agent.calls != 0 {
		t.Errorf("the agent ran %d times for a gap check with no gaps", agent.calls)
	}
	// Generous bound: the deadline is 50ms, so a runner that waited for
	// the hung command would blow straight past this.
	if elapsed > 10*time.Second {
		t.Errorf("the gap check took %v for a 50ms deadline", elapsed)
	}
}

// The gap check bounds the production default and leaves an injected
// runner exactly as it was.
func TestDoctorScanBoundsOnlyTheDefaultRunner(t *testing.T) {
	a, _, _, _, _, _ := newTestApp(t, []string{"doctor"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	a.CatalogLoader = fixtureLoader("sh")
	var got detect.Runner
	a.DetectFn = func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		got = run
		return nil, nil, nil
	}
	a.DetectRunner = detect.DefaultRunner
	a.Run()
	if got == nil {
		t.Fatal("the gap check passed no runner to the scan")
	}
	if detect.IsDefaultRunner(got) {
		t.Error("the gap check ran with the unbounded default runner")
	}

	// An injected runner is passed through untouched.
	b, _, _, _, _, _ := newTestApp(t, []string{"doctor"}, nil)
	wireDoctor(t, b, &stubAgent{}, shRunner())
	b.CatalogLoader = fixtureLoader("sh")
	var gotInjected detect.Runner
	b.DetectFn = func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		gotInjected = run
		return nil, nil, nil
	}
	injected := func(name string, args ...string) (string, error) { return "sentinel", nil }
	b.DetectRunner = injected
	b.Run()
	if gotInjected == nil {
		t.Fatal("the gap check passed no runner to the scan")
	}
	if out, err := gotInjected("anything"); out != "sentinel" || err != nil {
		t.Errorf("the injected runner was replaced: out = %q, err = %v", out, err)
	}
}

// scan and check keep the unbounded default: only doctor's own scans are
// bounded, so the other commands behave exactly as before.
func TestScanAndCheckKeepTheUnboundedRunner(t *testing.T) {
	for _, cmd := range []string{"scan", "check"} {
		t.Run(cmd, func(t *testing.T) {
			a, _, _, _, _, _ := newTestApp(t, []string{cmd}, nil)
			var got detect.Runner
			a.DetectFn = func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
				got = run
				return nil, nil, nil
			}
			a.DetectRunner = detect.DefaultRunner
			a.Run()
			if got == nil {
				t.Fatalf("%s passed no runner to the scan", cmd)
			}
			if !detect.IsDefaultRunner(got) {
				t.Errorf("%s received a wrapped runner; it must keep the unbounded default", cmd)
			}
		})
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
	// The review block is printed on stderr — the same stream the
	// confirm prompt uses — so a redirected stdout can never separate
	// the question from the security review.
	out := stderr.String()
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
	if !strings.Contains(stdout.String(), "proposal ready") {
		t.Errorf("stdout missing the proposal-ready line:\n%s", stdout.String())
	}
	// Non-TTY stdin: no prompt, no install, the --install command instead.
	if strings.Contains(stderr.String(), "Install to") {
		t.Errorf("non-TTY stdin was prompted: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "ofa doctor --install ") {
		t.Errorf("stdout missing the --install command:\n%s", stdout.String())
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
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{claudeEntry}}, shRunner())
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	// The override block rides the review, on stderr.
	out := stderr.String()
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
	// The override comparison shows the old and new commands, not just
	// names and counts.
	for _, want := range []string{
		"embedded install 1:",
		"proposed install 1:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("override block missing the command comparison %q:\n%s", want, out)
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

// writerFunc adapts a function to io.Writer. The tests that use it fail
// only the y/N question and pass every other write through, so the one
// thing wrong is that the question never appeared.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// recordingFeeder answers "y" like lineFeeder but counts reads, so a test
// can prove no answer was consumed for a question the user never saw.
type recordingFeeder struct{ reads int }

func (r *recordingFeeder) Read(p []byte) (int, error) {
	r.reads++
	return copy(p, "y\n"), nil
}

// TestDoctorCmd_UnprintableQuestionNeverInstalls: when the Install or
// Overwrite question cannot be written, stdin is not read, nothing is
// installed or replaced, and the exit is nonzero.
func TestDoctorCmd_UnprintableQuestionNeverInstalls(t *testing.T) {
	existing := "id = \"sh-probe\"\nname = \"Mine\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\n"
	for _, c := range []struct {
		name     string
		preexist bool
		// reads the question that may fail; the initial question fails
		// first when there is no prior file, the overwrite one when a
		// prior file exists and the initial one is let through.
		failOnly string
	}{
		{"initial confirm", false, "Install to"},
		{"overwrite confirm", true, "Overwrite"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
			wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
			feeder := &recordingFeeder{}
			a.Stdin = feeder
			a.StdinIsTTY = func(io.Reader) bool { return true }
			failOnly := c.failOnly
			a.Stderr = writerFunc(func(p []byte) (int, error) {
				if strings.Contains(string(p), "[y/N]") && strings.Contains(string(p), failOnly) {
					return 0, errors.New("broken stderr")
				}
				return stderr.Write(p)
			})
			path := filepath.Join(a.CatalogDir, "sh-probe.toml")
			if c.preexist {
				if err := os.MkdirAll(a.CatalogDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			detectingAfterInstall(a)
			sd.instances = nil
			code := a.Run()
			if code == ExitOK {
				t.Fatalf("exit = 0, want nonzero; stderr: %s", stderr.String())
			}
			if c.preexist {
				// The initial question printed and consumed one answer;
				// the overwrite question must not have consumed another.
				if feeder.reads != 1 {
					t.Errorf("stdin reads = %d, want 1 (only the initial question)", feeder.reads)
				}
				b, _ := os.ReadFile(path)
				if string(b) != existing {
					t.Errorf("existing file was modified: %q", string(b))
				}
			} else {
				if feeder.reads != 0 {
					t.Errorf("stdin reads = %d, want 0", feeder.reads)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("catalog entry exists after an unprintable question: %v", err)
				}
			}
			if !strings.Contains(stderr.String(), "nothing was written") {
				t.Errorf("stderr missing the plain message: %s", stderr.String())
			}
		})
	}
}

// A failed write reports one "doctor:" prefix, not two.
func TestDoctorCmd_WriteFailureHasSinglePrefix(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y", "y")
	// A directory where the file should go: the overwrite rename fails.
	if err := os.MkdirAll(filepath.Join(a.CatalogDir, "sh-probe.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	sd.instances = nil
	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitError, stderr.String())
	}
	if strings.Contains(stderr.String(), "doctor: doctor:") {
		t.Errorf("doubled prefix in stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "ofa: doctor: ") {
		t.Errorf("stderr missing the single prefix: %s", stderr.String())
	}
}

// The configured catalog directory is untrusted text on the terminal: it
// is shown through the same escaping as every other value.
func TestDoctorCmd_CatalogDirRenderedSafely(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y", "n")
	a.CatalogDir = filepath.Join(a.CatalogDir, "cat\x1b[8m")
	if err := os.MkdirAll(a.CatalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.CatalogDir, "sh-probe.toml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sd.instances = nil
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.ContainsRune(stderr.String(), '\x1b') {
		t.Errorf("raw ESC in stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), `cat\x1b[8m`) {
		t.Errorf("stderr missing the visible escape: %q", stderr.String())
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
	if !strings.Contains(stdout.String(), "revalidated proposal") || !strings.Contains(stderr.String(), "harness id: sh-probe") {
		t.Errorf("review flow missing: stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
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

// --- review-fix round: B1 binding, B4 scan order, A1/A2 ---

// swapOnConfirm answers the first confirm prompt and, in that same
// Read, rewrites the saved proposal file on disk — a local writer
// racing the human's decision (the reviewer's writer-hook fixture). The
// install must write the bytes that were validated and reviewed, not
// whatever the file holds by the time the "y" arrives.
type swapOnConfirm struct {
	doctorDir   string
	replacement string
	answered    bool
}

func (f *swapOnConfirm) Read(p []byte) (int, error) {
	if f.answered {
		return 0, io.EOF
	}
	f.answered = true
	if err := swapProposalFile(f.doctorDir, f.replacement); err != nil {
		return 0, err
	}
	return copy(p, "y\n"), nil
}

// swapDuringReview wraps the review stream and rewrites the saved
// proposal file the moment the review starts rendering — the other side
// of the reviewer's hook: a writer swapping the file after validation
// but before any post-review read of the file.
type swapDuringReview struct {
	inner       io.Writer
	doctorDir   string
	replacement string
	swapped     bool
}

func (w *swapDuringReview) Write(p []byte) (int, error) {
	if !w.swapped && strings.Contains(string(p), "harness id:") {
		w.swapped = true
		if err := swapProposalFile(w.doctorDir, w.replacement); err != nil {
			return 0, err
		}
	}
	return w.inner.Write(p)
}

// swapProposalFile overwrites the single session's proposed-catalog.toml.
func swapProposalFile(doctorDir, replacement string) error {
	history := filepath.Join(doctorDir, "history")
	entries, err := os.ReadDir(history)
	if err != nil || len(entries) == 0 {
		return fmt.Errorf("swap: no session directory under %s (%v)", history, err)
	}
	return os.WriteFile(filepath.Join(history, entries[0].Name(), "proposed-catalog.toml"), []byte(replacement), 0o600)
}

func TestDoctorCmd_SwapAfterReviewInstallsReviewedBytes(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	detectingAfterInstall(a)
	replacement := strings.Replace(bareToml, `name = "Shell Probe"`, `name = "Swapped Probe"`, 1)
	// The file is swapped once as the review renders and again as the
	// prompt is answered: neither may change what is installed.
	a.Stderr = &swapDuringReview{inner: stderr, doctorDir: a.DoctorDir, replacement: replacement}
	a.Stdin = &swapOnConfirm{doctorDir: a.DoctorDir, replacement: replacement}
	a.StdinIsTTY = func(io.Reader) bool { return true }

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(a.CatalogDir, "sh-probe.toml"))
	if err != nil {
		t.Fatalf("catalog entry not written: %v", err)
	}
	// The fresh flow's validated bytes are the fence's inner TOML,
	// trailing newline trimmed by extraction.
	if want := strings.TrimSpace(bareToml); string(b) != want {
		t.Errorf("installed = %q, want the reviewed bytes %q", string(b), want)
	}
	if strings.Contains(string(b), "Swapped Probe") {
		t.Error("bytes swapped after the review were installed")
	}
	if !strings.Contains(stderr.String(), "name: Shell Probe") {
		t.Errorf("review missing the reviewed name:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "detected: sh 1.0") {
		t.Errorf("stdout missing the post-install detection:\n%s", stdout.String())
	}
}

// The reviewer's B1 fixture at --install: the saved file is a valid
// outer entry whose multiline name wraps a fenced, benign inner entry,
// and the outer entry declares an unreviewed version vector. The file
// must be validated as-is (no fence extraction at install time): the
// outer entry IS the proposal and it is rejected, with nothing written.
func TestDoctorInstallCmd_FenceWrappedProposalRejected(t *testing.T) {
	inner := "id = \"sh-probe\"\nname = \"Inner\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"self\"\n"
	outer := "id = \"sh-probe\"\nname = \"\"\"\nShell Probe\n```toml\n" + inner + "```\n\"\"\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"-c\", \"UNREVIEWED_VERSION_MARKER\"] }\nlatest = \"self\"\n"
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	dir := seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", outer)

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (fence-wrapped proposal)", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "no longer valid") {
		t.Errorf("stderr missing the rejection reason: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("fence-wrapped proposal was installed")
	}
	rec, err := doctor.LoadSessionRecord(dir)
	if err != nil || rec.Status != "rejected" {
		t.Errorf("session record = %+v, %v; want rejected", rec, err)
	}
}

// The binding counterpart: a clean saved file whose name merely
// MENTIONS a fence is a legitimate whole-file proposal. The review shows
// the entry the file actually declares, and the installed bytes are the
// whole file — not an inner fence extraction.
func TestDoctorInstallCmd_InstallsWholeFileBytes(t *testing.T) {
	single := "id = \"sh-probe\"\nname = \"Shell Probe ```toml id = \\\"sh-inner\\\" name = \\\"Inner\\\" ``` tail\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"sh\"\npaths = [\"/bin/sh\"]\nversion = { cmd = [\"sh\", \"--version\"] }\nlatest = \"self\"\n"
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", single)
	detectingAfterInstall(a)

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	// The reviewed entry and the installed bytes are the same whole
	// file: the fence text stayed text inside the name.
	if !strings.Contains(stderr.String(), "harness id: sh-probe") || !strings.Contains(stderr.String(), "Shell Probe ```toml") {
		t.Errorf("review did not show the file's own entry:\n%s", stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(a.CatalogDir, "sh-probe.toml"))
	if err != nil || string(b) != single {
		t.Errorf("installed = %q, %v; want the whole file's exact bytes", string(b), err)
	}
	if !strings.Contains(stdout.String(), "installed: ") {
		t.Errorf("stdout missing the installed line:\n%s", stdout.String())
	}
}

// B4: the post-install scan must find the ACTIVE instance even when a
// shadowed one comes first — scan order follows catalog order, not PATH
// order.
func TestDoctorCmd_PostInstallScanPrefersActiveInstance(t *testing.T) {
	ordered := []detect.Instance{
		{HarnessID: "sh-probe", Method: "native", Bin: "sh", Status: detect.Shadowed, Version: "1.0"},
		{HarnessID: "sh-probe", Method: "npm", Bin: "sh", Status: detect.Active, Version: "2.0"},
	}
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y")
	a.DetectFn = func([]catalog.Harness, detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		if _, err := os.Stat(filepath.Join(a.DoctorDir, "history")); err == nil {
			return ordered, nil, nil
		}
		return nil, nil, nil
	}
	sd.instances = nil
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0 (an active instance exists); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "detected: sh 2.0 (sh-probe, npm)") {
		t.Errorf("stdout missing the active instance:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "not the live install") {
		t.Errorf("a shadowed instance was reported ahead of the active one:\n%s", stdout.String())
	}

	// Only a shadowed instance: reported honestly, never a success.
	a2, stdout2, _, sd2, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a2, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a2, "y")
	shadowedOnly := []detect.Instance{ordered[0]}
	a2.DetectFn = func([]catalog.Harness, detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		if _, err := os.Stat(filepath.Join(a2.DoctorDir, "history")); err == nil {
			return shadowedOnly, nil, nil
		}
		return nil, nil, nil
	}
	sd2.instances = nil
	if code := a2.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (shadowed only)", code, ExitError)
	}
	if !strings.Contains(stdout2.String(), "(not the live install on PATH)") {
		t.Errorf("stdout missing the shadowed note:\n%s", stdout2.String())
	}
}

// postInstallScanRun drives the install flow and returns what the
// post-install re-scan reported, together with the runner it was given.
// The re-scan's scan is stubbed: it fails, and records the runner it
// received.
func postInstallScanRun(t *testing.T, runner detect.Runner) (string, detect.Runner) {
	t.Helper()
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y")
	a.CatalogLoader = fixtureLoader("sh")
	var got detect.Runner
	a.DetectFn = func(_ []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error) {
		// The gap check runs before the install; the re-scan after it.
		if _, err := os.Stat(filepath.Join(a.DoctorDir, "history")); err == nil {
			got = run
			return nil, []detect.Failure{{
				Method: "brew-formula",
				Err:    errors.New("timed out after 30s running brew list --versions"),
			}}, nil
		}
		return nil, nil, nil
	}
	a.DetectRunner = runner
	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (the install was not detected); stderr: %s", code, ExitError, stderr.String())
	}
	return stderr.String(), got
}

// The post-install re-scan used to discard its failures outright. It now
// reports them, in the same shared format the gap check uses.
func TestDoctorCmd_PostInstallScanReportsFailures(t *testing.T) {
	stderr, got := postInstallScanRun(t, detect.DefaultRunner)
	if !strings.Contains(stderr, "brew-formula detector failed: timed out after 30s running brew list --versions") {
		t.Errorf("stderr = %q, want the post-install failure reported", stderr)
	}
	if n := strings.Count(stderr, "brew-formula"); n != 1 {
		t.Errorf("stderr names brew-formula %d times, want 1: %s", n, stderr)
	}
	// The re-scan ran over the bounded runner, not the unbounded default:
	// the swap is the only thing that can change the runner here.
	if got == nil {
		t.Fatal("the post-install re-scan passed no runner to the scan")
	}
	if detect.IsDefaultRunner(got) {
		t.Error("the post-install re-scan ran with the unbounded default runner")
	}
}

// The post-install re-scan leaves an injected runner exactly as it was.
func TestDoctorCmd_PostInstallScanKeepsAnInjectedRunner(t *testing.T) {
	injected := func(name string, args ...string) (string, error) { return "sentinel", nil }
	_, got := postInstallScanRun(t, injected)
	if got == nil {
		t.Fatal("the post-install re-scan passed no runner to the scan")
	}
	if out, err := got("anything"); out != "sentinel" || err != nil {
		t.Errorf("the injected runner was replaced: out = %q, err = %v", out, err)
	}
}

// A2: the review goes to the same stream as the prompt, and a review
// that cannot be written means no install.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("review stream broken") }

func TestDoctorCmd_ReviewWriteErrorRefusesInstall(t *testing.T) {
	a, _, _, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y")
	detectingAfterInstall(a)
	sd.instances = nil
	a.Stderr = failWriter{}

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d (review not displayable)", code, ExitError)
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("installed despite the review not being displayable")
	}
}

// A1: /dev/null is a character device, so the mode check alone mistakes
// it for a terminal. The SameFile exception must route it to the
// non-interactive path: no prompt, no install, the --install command.
func TestDoctorCmd_DevNullStdinIsNotInteractive(t *testing.T) {
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	a.Stdin = f
	sd.instances = nil

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "Install to") {
		t.Errorf("a /dev/null stdin was prompted: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "to install it, run: ofa doctor --install") {
		t.Errorf("stdout missing the --install suggestion:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("a /dev/null stdin installed into the user catalog")
	}
}

// The install prompt may only be shown to someone who is actually
// there. A regular file, a directory, /dev/null, /dev/zero and a pipe
// are all non-interactive — and the last two are character devices, so
// the mode check alone calls them terminals.
func TestStdinIsTTY_NonTerminalInputs(t *testing.T) {
	type tc struct {
		name string
		r    io.Reader
		want bool
	}
	dir := t.TempDir()

	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	regularFile, err := os.Open(regular)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { regularFile.Close() })

	// A directory: openable and statable, and not something to read an
	// answer from.
	dirFile, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dirFile.Close() })

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devNull.Close() })

	// /dev/zero is where the device-node case shows up: a character
	// device that is not a terminal. It is not present everywhere.
	devZero, err := os.Open("/dev/zero")
	if err != nil {
		t.Logf("/dev/zero is not available here: %v", err)
	} else {
		t.Cleanup(func() { devZero.Close() })
	}

	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipeReader.Close(); pipeWriter.Close() })

	cases := []tc{
		{"regular file", regularFile, false},
		{"directory", dirFile, false},
		{"/dev/null", devNull, false},
		{"pipe", pipeReader, false},
		{"bytes buffer", &bytes.Buffer{}, false},
		{"nil", nil, false},
	}
	if devZero != nil {
		cases = append(cases, tc{"/dev/zero", devZero, false})
	}
	for _, c := range cases {
		if got := stdinIsTTY(c.r); got != c.want {
			t.Errorf("stdinIsTTY(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	// The positive case needs a real terminal, which a machine without a
	// controlling terminal (a CI runner) does not have. It is a skip
	// rather than a pass: the default output says SKIP, so a run that
	// never checked the positive case cannot read as one that did.
	t.Run("a controlling terminal is a terminal", func(t *testing.T) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			t.Skipf("no controlling terminal here (%v)", err)
		}
		defer tty.Close()
		if !stdinIsTTY(tty) {
			t.Error("stdinIsTTY(a controlling terminal) = false, want true")
		}
	})
}

// The issue's exact case: /dev/zero is a character device, so the mode
// check alone mistakes it for a terminal. The install prompt must not
// appear for it — a device that never answers cannot be answered — and
// nothing may be installed.
func TestDoctorCmd_DevZeroStdinIsNotInteractive(t *testing.T) {
	f, err := os.Open("/dev/zero")
	if err != nil {
		t.Skipf("/dev/zero is not available: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	a.Stdin = f
	sd.instances = nil

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "Install to") {
		t.Errorf("a /dev/zero stdin was prompted: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "to install it, run: ofa doctor --install") {
		t.Errorf("stdout missing the --install suggestion:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("a /dev/zero stdin installed into the user catalog")
	}
}

// A non-interactive stdin must never install, whatever it carries: a
// file or a pipe holding "y" is not a person saying yes.
func TestDoctorCmd_NonInteractiveStdinNeverInstalls(t *testing.T) {
	type tc struct {
		name string
		r    io.Reader
	}
	dir := t.TempDir()

	yesFile := filepath.Join(dir, "yes")
	if err := os.WriteFile(yesFile, []byte("y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(yesFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })

	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipeReader.Close(); pipeWriter.Close() })
	if _, err := pipeWriter.WriteString("y\n"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []tc{
		{"a file holding y", file},
		{"a pipe holding y", pipeReader},
	} {
		a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
		wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
		a.Stdin = c.r
		sd.instances = nil

		if code := a.Run(); code != ExitOK {
			t.Errorf("%s: exit = %d, want 0; stderr: %s", c.name, code, stderr.String())
		}
		if strings.Contains(stderr.String(), "Install to") {
			t.Errorf("%s: was prompted: %s", c.name, stderr.String())
		}
		if !strings.Contains(stdout.String(), "to install it, run: ofa doctor --install") {
			t.Errorf("%s: stdout missing the --install suggestion:\n%s", c.name, stdout.String())
		}
		if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
			t.Errorf("%s: installed into the user catalog", c.name)
		}
	}
}

// B2: --list renders session.json fields — which come from a user-owned
// directory — through the terminal-safe renderer.
func TestDoctorListCmdEscapesUntrustedFields(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--list"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	rec := doctor.SessionRecord{
		Target:        "sh\x1b[8m",
		Status:        "proposed\r",
		Time:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		InstalledPath: "/x/\u202epath",
	}
	dir := filepath.Join(a.DoctorDir, "history", "20260101T000000Z")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := doctor.SaveSessionRecord(dir, rec); err != nil {
		t.Fatal(err)
	}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Assert on bytes: no raw ESC, CR, or bidi override anywhere.
	if i := strings.IndexByte(out, 0x1b); i >= 0 {
		t.Errorf("listing carries a raw ESC at %d: %q", i, out)
	}
	if i := strings.IndexByte(out, '\r'); i >= 0 {
		t.Errorf("listing carries a raw CR at %d: %q", i, out)
	}
	if strings.ContainsRune(out, '\u202e') {
		t.Errorf("listing carries a raw bidi override: %q", out)
	}
	for _, want := range []string{`\x1b[8m`, `\r`, `\u202e`} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing the visible escape %q:\n%s", want, out)
		}
	}
}

// F1: an explicitly empty --install is a usage error, not "flag absent";
// it must not fall into the gap check and spend an agent run.
func TestDoctorCmd_EmptyInstallIsUsageError(t *testing.T) {
	for _, args := range [][]string{{"doctor", "--install", ""}, {"doctor", "--install="}} {
		a, _, stderr, sd, _, _ := newTestApp(t, args, nil)
		agent := &stubAgent{responses: []string{shEntry}}
		wireDoctor(t, a, agent, shRunner())
		a.CatalogLoader = fixtureLoader("sh") // exactly one gap
		sd.instances = nil
		if code := a.Run(); code != ExitError {
			t.Fatalf("%q: exit = %d, want %d; stderr: %s", args, code, ExitError, stderr.String())
		}
		if agent.calls != 0 {
			t.Errorf("%q: agent invoked %d time(s), want 0", args, agent.calls)
		}
		if !strings.Contains(stderr.String(), "--install needs a session id") {
			t.Errorf("%q: stderr missing the usage reason: %s", args, stderr.String())
		}
	}
}

// F2: a failed revalidation of an already-installed session reports the
// rejection but does not downgrade the session record.
func TestDoctorInstallCmd_RevalidationFailureKeepsInstalledStatus(t *testing.T) {
	tampered := "id = \"cat-probe\"\nname = \"Cat Probe\"\n\n[[installs]]\nmethod = \"native\"\nbin = \"cat\"\npaths = [\"/bin/cat\"]\nversion = { cmd = [\"cat\", \"--version\"] }\nlatest = \"self\"\n"
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	dir := seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", doctor.StatusInstalled, tampered)
	rec, err := doctor.LoadSessionRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec.InstalledPath = filepath.Join(a.CatalogDir, "sh-probe.toml")
	if err := doctor.SaveSessionRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "no longer valid") {
		t.Errorf("stderr missing the rejection reason: %s", stderr.String())
	}
	got, err := doctor.LoadSessionRecord(dir)
	if err != nil || got.Status != doctor.StatusInstalled || got.InstalledPath != rec.InstalledPath {
		t.Errorf("session record = %+v, %v; want still installed with its path", got, err)
	}
}

// F3: a directory-sync failure after the publish is a warning, not a
// failed install: exit 0, the entry stays, the session is marked installed.
func TestDoctorInstallCmd_DirSyncFailureIsWarning(t *testing.T) {
	old := doctor.SyncDirFn
	doctor.SyncDirFn = func(string) error { return errors.New("sync boom") }
	t.Cleanup(func() { doctor.SyncDirFn = old })

	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	dir := seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)
	sd.instances = []detect.Instance{shInstance()}
	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning") || !strings.Contains(stderr.String(), "could not sync the directory") {
		t.Errorf("stderr missing the sync warning: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "installed: ") {
		t.Errorf("stdout missing the install result: %s", stdout.String())
	}
	if b, err := os.ReadFile(filepath.Join(a.CatalogDir, "sh-probe.toml")); err != nil || string(b) != bareToml {
		t.Errorf("entry was not left in place: %q, %v", string(b), err)
	}
	if rec, err := doctor.LoadSessionRecord(dir); err != nil || rec.Status != doctor.StatusInstalled {
		t.Errorf("session record = %+v, %v; want installed", rec, err)
	}
}

// A session directory that is a symlink is listed when it resolves
// inside the sessions directory — --install can install from it — and
// is named as skipped, with the reason, when it does not. It is never
// dropped silently: a session the listing hides is a session the user
// cannot find.
func TestDoctorListCmd_SymlinkedSession(t *testing.T) {
	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--list"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	root := filepath.Join(a.DoctorDir, "history")

	// A plain session.
	seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)
	// A symlink to a session that is also inside the sessions directory.
	inside := seedSession(t, a.DoctorDir, "20260102T000000Z", "cat", "proposed", bareToml)
	if err := os.Symlink(inside, filepath.Join(root, "20260103T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink that leaves the sessions directory.
	outside := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "20260104T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink that points nowhere.
	if err := os.Symlink(filepath.Join(root, "20260109T000000Z"), filepath.Join(root, "20260105T000000Z")); err != nil {
		t.Fatal(err)
	}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"20260101T000000Z  sh  proposed",
		"20260102T000000Z  cat  proposed",
		"20260103T000000Z  cat  proposed",
		"skipped 20260104T000000Z: symlink leaves the sessions directory",
		"skipped 20260105T000000Z: dangling symlink",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q:\n%s", want, out)
		}
	}
	// A skipped entry is not listed as a session.
	for _, name := range []string{"20260104T000000Z", "20260105T000000Z"} {
		if strings.Contains(out, "  "+name+"  ") {
			t.Errorf("a skipped entry was listed as a session: %q", name)
		}
	}
	// The reason says what kind of entry it is; it never carries the
	// path the link points at.
	if strings.Contains(out, "outside-secret") {
		t.Errorf("the listing echoed the link target:\n%s", out)
	}
}

// A session directory that is a symlink out of the sessions directory
// is refused: the proposal is not read from outside, and the install
// record is not written outside either.
func TestDoctorInstallCmd_SymlinkOutsideRootRefused(t *testing.T) {
	a, _, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")

	// A complete, valid session — but OUTSIDE the sessions directory.
	outside := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := doctor.SessionRecord{
		Target:    "sh",
		HarnessID: "sh-probe",
		Time:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:    doctor.StatusProposed,
	}
	if err := doctor.SaveSessionRecord(outside, rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "proposed-catalog.toml"), []byte(bareToml), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(outside, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The session directory is a symlink to it.
	root := filepath.Join(a.DoctorDir, "history")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}

	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot install") {
		t.Errorf("stderr = %q, want the refusal", stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink leaves the sessions directory") {
		t.Errorf("stderr = %q, want the reason", stderr.String())
	}
	if strings.Contains(stderr.String(), "outside-secret") {
		t.Errorf("the refusal echoed the link target: %q", stderr.String())
	}
	// Nothing outside the sessions directory was written.
	after, err := os.ReadFile(filepath.Join(outside, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the record outside the sessions directory was rewritten:\n%s", after)
	}
	// And nothing was installed.
	if _, err := os.Stat(filepath.Join(a.CatalogDir, "sh-probe.toml")); !os.IsNotExist(err) {
		t.Error("installed from a session outside the sessions directory")
	}
}

// A symlink that stays inside the sessions directory is a session:
// --install installs from it, so the listing and the install agree.
func TestDoctorInstallCmd_SymlinkInsideRootInstalls(t *testing.T) {
	a, stdout, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	makeInteractive(a, "y")
	// The real session, and a symlink to it that is also inside the
	// sessions directory.
	real := seedSession(t, a.DoctorDir, "20260102T000000Z", "sh", "proposed", bareToml)
	if err := os.Symlink(real, filepath.Join(a.DoctorDir, "history", "20260101T000000Z")); err != nil {
		t.Fatal(err)
	}
	sd.instances = []detect.Instance{shInstance()}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "installed: ") {
		t.Errorf("stdout missing the install result:\n%s", stdout.String())
	}
	b, err := os.ReadFile(filepath.Join(a.CatalogDir, "sh-probe.toml"))
	if err != nil || string(b) != bareToml {
		t.Errorf("installed content = %q, %v", string(b), err)
	}
	// The record lands in the session the link points at.
	rec, err := doctor.LoadSessionRecord(real)
	if err != nil || rec.Status != doctor.StatusInstalled {
		t.Errorf("session record = %+v, %v; want installed", rec, err)
	}
}

// A sessions directory that is itself a symlink — a config directory
// kept in a dotfiles repo is a common way to get one — still contains
// its own sessions: the boundary is the RESOLVED root.
func TestDoctorCmd_SessionsRootIsSymlink(t *testing.T) {
	// The doctor directory's history/ is a symlink to somewhere else.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "history"), 0o700); err != nil {
		t.Fatal(err)
	}
	doctorDir := filepath.Join(t.TempDir(), "doctor")
	if err := os.MkdirAll(doctorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "history"), filepath.Join(doctorDir, "history")); err != nil {
		t.Fatal(err)
	}

	a, stdout, stderr, _, _, _ := newTestApp(t, []string{"doctor", "--list"}, nil)
	wireDoctor(t, a, &stubAgent{}, shRunner())
	a.DoctorDir = doctorDir
	seedSession(t, a.DoctorDir, "20260101T000000Z", "sh", "proposed", bareToml)

	if code := a.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "20260101T000000Z  sh  proposed") {
		t.Errorf("the listing missed the session behind a symlinked root:\n%s", stdout.String())
	}

	// And the install reaches it too.
	b, _, stderr2, sd, _, _ := newTestApp(t, []string{"doctor", "--install", "20260101T000000Z"}, nil)
	wireDoctor(t, b, &stubAgent{}, shRunner())
	b.DoctorDir = doctorDir
	makeInteractive(b, "y")
	sd.instances = []detect.Instance{shInstance()}

	if code := b.Run(); code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr2.String())
	}
	if _, err := os.Stat(filepath.Join(b.CatalogDir, "sh-probe.toml")); err != nil {
		t.Errorf("installed behind a symlinked sessions root: %v", err)
	}
}

// Advisory: a write error that echoes the configured catalog path is
// rendered terminal-safe.
func TestDoctorCmd_WriteErrorRenderedSafely(t *testing.T) {
	a, _, stderr, sd, _, _ := newTestApp(t, []string{"doctor", "sh"}, nil)
	wireDoctor(t, a, &stubAgent{responses: []string{shEntry}}, shRunner())
	makeInteractive(a, "y", "y")
	a.CatalogDir = filepath.Join(a.CatalogDir, "cat\x1b[8m")
	// A directory where the file should go: the overwrite rename fails
	// with an error that names the path.
	if err := os.MkdirAll(filepath.Join(a.CatalogDir, "sh-probe.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	sd.instances = nil
	if code := a.Run(); code != ExitError {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, ExitError, stderr.String())
	}
	if strings.ContainsRune(stderr.String(), '\x1b') {
		t.Errorf("raw ESC in stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), `cat\x1b[8m`) {
		t.Errorf("stderr missing the visible escape: %q", stderr.String())
	}
}
