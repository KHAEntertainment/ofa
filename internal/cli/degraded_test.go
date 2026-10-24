package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"ofa/internal/detect"
	"ofa/internal/resolve"
	"ofa/internal/state"
)

// One broken package manager alongside a healthy one must not abort check:
// the scan still reports, the failure is named on stderr, and the harnesses
// lost to it are named too.
func TestCheckCmd_DegradedScanStillReports(t *testing.T) {
	a, out, errOut, sd, sr, _ := newTestApp(t, []string{"check"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.2.0", Status: detect.Active},
	}
	sd.failures = []detect.Failure{{Method: "brew-formula", Err: errors.New("exit status 1")}}
	sr.res = []resolve.Resolution{
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Cur: "1.2.0", Tier: "npm"},
	}

	code := a.Run()
	if code == ExitError {
		t.Fatalf("check exit = %d (ExitError): a failing detector must not abort a scan that produced results", code)
	}
	if code != ExitOK {
		t.Errorf("check exit = %d, want %d (no updates pending)", code, ExitOK)
	}
	if !strings.Contains(errOut.String(), "ofa: warning: brew-formula detector failed") {
		t.Errorf("stderr should name the failing detector, got %q", errOut.String())
	}
	// opencode is installable through brew-formula and was not detected, so it
	// is precisely the loss the failure caused.
	if !strings.Contains(errOut.String(), "ofa: undetectable: opencode [brew-formula]") {
		t.Errorf("stderr should name the harnesses lost to the failure, got %q", errOut.String())
	}
	if !strings.Contains(out.String(), "1 harnesses") {
		t.Errorf("stdout should still report the healthy half of the scan, got %q", out.String())
	}
}

// When nothing at all was detected and something did fail, the empty report
// would be a lie about the machine — that is the only case exit 2 covers.
func TestCheckCmd_NothingDetectedWithFailuresExits2(t *testing.T) {
	a, out, errOut, sd, _, _ := newTestApp(t, []string{"check"}, nil)
	sd.instances = nil
	sd.failures = []detect.Failure{
		{Method: "brew-formula", Err: errors.New("exit status 1")},
		{Method: "npm", Err: errors.New("exit status 1")},
	}

	if code := a.Run(); code != ExitError {
		t.Errorf("check exit = %d, want %d (ExitError)", code, ExitError)
	}
	if out.Len() != 0 {
		t.Errorf("a total failure should not print a report, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "ofa: warning: npm detector failed") {
		t.Errorf("stderr should name every failing detector, got %q", errOut.String())
	}
}

// A failing detector must not masquerade as an uninstall: codex stays in
// state and does not appear under Removed, or the damage outlives the fault.
func TestScanCmd_DegradedDoesNotMarkRemoved(t *testing.T) {
	a, _, _, sd, _, _ := newTestApp(t, []string{"scan"}, nil)

	// Baseline: both a native harness and a brew-cask harness detected.
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1", Status: detect.Active},
	}
	a.Now = func() time.Time { return mustTime("2026-10-15T12:00:00Z") }
	if code := a.Run(); code != ExitOK {
		t.Fatalf("baseline scan exit = %d, want %d", code, ExitOK)
	}

	// Next scan: brew-cask is broken, so codex is invisible. claude is not.
	sd.instances = []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
	}
	sd.failures = []detect.Failure{{Method: "brew-cask", Err: errors.New("exit status 1")}}

	out := &bytes.Buffer{}
	a.Stdout = out
	a.Now = func() time.Time { return mustTime("2026-10-15T12:05:00Z") }
	if code := a.Run(); code != ExitOK {
		t.Fatalf("degraded scan exit = %d, want %d (a healthy detector still ran)", code, ExitOK)
	}

	if strings.Contains(out.String(), "- removed") {
		t.Errorf("a broken detector must not report its harnesses as removed:\n%s", out.String())
	}

	f, err := loadStateFile(a.StatePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	codex, ok := f.Harnesses["codex"]
	if !ok {
		t.Fatalf("codex lost from saved state by a brew-cask failure; state = %+v", f.Harnesses)
	}
	if len(codex.Installs) != 1 || codex.Installs[0].Version != "0.162.1" {
		t.Errorf("codex install should carry forward unchanged, got %+v", codex.Installs)
	}
}

// An install still visible through a healthy method is not displaced by the
// carried-forward one — that detector's answer wins, versions included.
func TestReconcileDegraded_HealthyDetectorWins(t *testing.T) {
	prev := &stateFile{Harnesses: map[string]state.Harness{
		"opencode": {Installs: []state.Install{
			{Method: "brew-formula", Package: "opencode", Bin: "opencode", Version: "1.19.0", Status: "active"},
		}},
	}}
	current := &stateFile{Harnesses: map[string]state.Harness{
		"opencode": {Installs: []state.Install{
			{Method: "npm", Package: "opencode-ai", Bin: "opencode", Version: "1.19.3", Status: "active"},
		}},
	}}
	failures := []detect.Failure{{Method: "brew-formula", Err: errors.New("boom")}}

	got := reconcileDegraded(prev, current, failures)
	// The npm install is healthy and present; the brew install belongs to the
	// failed method and carries forward, so opencode keeps both.
	insts := got.Harnesses["opencode"].Installs
	if len(insts) != 2 {
		t.Fatalf("opencode should keep its npm install and carry its brew install, got %+v", insts)
	}
	seen := map[string]string{}
	for _, in := range insts {
		seen[in.Method] = in.Version
	}
	if seen["npm"] != "1.19.3" {
		t.Errorf("healthy npm install overwritten: %v", seen)
	}
	if seen["brew-formula"] != "1.19.0" {
		t.Errorf("failed-method install not carried forward: %v", seen)
	}
}

func TestReconcileDegraded_NoopWithoutFailures(t *testing.T) {
	prev := &stateFile{Harnesses: map[string]state.Harness{
		"codex": {Installs: []state.Install{{Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.162.1"}}},
	}}
	current := &stateFile{Harnesses: map[string]state.Harness{}}

	got := reconcileDegraded(prev, current, nil)
	if _, ok := got.Harnesses["codex"]; ok {
		t.Errorf("without failures, nothing should carry forward")
	}
}

func TestReconcileDegraded_NoopWithoutPrev(t *testing.T) {
	current := &stateFile{Harnesses: map[string]state.Harness{}}
	failures := []detect.Failure{{Method: "brew-cask", Err: errors.New("boom")}}

	got := reconcileDegraded(nil, current, failures)
	if len(got.Harnesses) != 0 {
		t.Errorf("a first scan has nothing to carry forward, got %+v", got.Harnesses)
	}
}

// --json consumers need to know the data is partial, or the machine-readable
// form repeats the same silent-loss bug in a different coat.
func TestCheckCmd_JSONReportsFailures(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check", "--json"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.2.0", Status: detect.Active},
	}
	sd.failures = []detect.Failure{{Method: "brew-formula", Err: errors.New("exit status 1")}}
	sr.res = []resolve.Resolution{
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Cur: "1.2.0", Tier: "npm"},
	}

	if code := a.Run(); code == ExitError {
		t.Fatalf("check --json exit = %d (ExitError), degraded run should still report", code)
	}

	var payload struct {
		HarnessCount int `json:"harness_count"`
		Failures     []struct {
			Method string `json:"method"`
			Error  string `json:"error"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if payload.HarnessCount != 1 {
		t.Errorf("harness_count = %d, want 1", payload.HarnessCount)
	}
	if len(payload.Failures) != 1 {
		t.Fatalf("failures = %+v, want one entry", payload.Failures)
	}
	if payload.Failures[0].Method != "brew-formula" {
		t.Errorf("failure method = %q, want brew-formula", payload.Failures[0].Method)
	}
	if payload.Failures[0].Error != "exit status 1" {
		t.Errorf("failure error = %q, want the underlying reason", payload.Failures[0].Error)
	}
}

// A healthy scan must not grow a failures field at all — consumers should
// read absence as "everything was queried".
func TestCheckCmd_JSONOmitsFailuresWhenHealthy(t *testing.T) {
	a, out, _, sd, sr, _ := newTestApp(t, []string{"check", "--json"}, nil)
	sd.instances = []detect.Instance{
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.2.0", Status: detect.Active},
	}
	sr.res = []resolve.Resolution{
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Cur: "1.2.0", Tier: "npm"},
	}

	if code := a.Run(); code != ExitOK {
		t.Fatalf("check exit = %d, want %d", code, ExitOK)
	}
	if strings.Contains(out.String(), `"failures"`) {
		t.Errorf("healthy scan should omit the failures key, got %s", out.String())
	}
}
