package detect

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"ofa/internal/catalog"
)

// degradedCatalog has one harness detectable only through brew and one only
// through npm, so a failure in either is unambiguous about what survives.
func degradedCatalog() []catalog.Harness {
	return []catalog.Harness{
		{ID: "codex", Name: "Codex", Installs: []catalog.Install{
			{Method: "brew-formula", Package: "codex", Bin: "codex"},
		}},
		{ID: "pi", Name: "Pi", Installs: []catalog.Install{
			{Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi"},
		}},
	}
}

// A present-but-broken package manager must degrade the scan, not abort it:
// the healthy detector's results still come back and the failure is reported.
func TestScan_FailingDetectorDoesNotAbort(t *testing.T) {
	run := func(name string, _ ...string) (string, error) {
		if name == "brew" {
			return "", errors.New("exit status 1")
		}
		if name == "npm" {
			return `{"dependencies":{"@earendil-works/pi-coding-agent":{"version":"1.2.0"}}}`, nil
		}
		return "", nil
	}

	insts, failures, err := Scan(degradedCatalog(), run)
	if err != nil {
		t.Fatalf("Scan: a failing detector must not fail the scan, got %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %d (%v), want 1", len(failures), failures)
	}
	if failures[0].Method != "brew-formula" {
		t.Errorf("failure method = %q, want brew-formula", failures[0].Method)
	}
	if failures[0].Err == nil || !strings.Contains(failures[0].Err.Error(), "exit status 1") {
		t.Errorf("failure must preserve the reason, got %v", failures[0].Err)
	}
	if len(insts) != 1 || insts[0].HarnessID != "pi" {
		t.Errorf("the healthy npm detector should still report; got %+v", insts)
	}
}

// An absent package manager is a different fact from a broken one: it is an
// empty set, not a failure, and must not be reported as one.
func TestScan_PackageManagerAbsentIsNotAFailure(t *testing.T) {
	run := func(name string, _ ...string) (string, error) {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}

	insts, failures, err := Scan(degradedCatalog(), run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("an absent package manager is not a failure; got %+v", failures)
	}
	if len(insts) != 0 {
		t.Errorf("no installs expected, got %+v", insts)
	}
}

// Both detectors down must still be reported as two failures, not collapsed
// into one — the caller has to name each package manager.
func TestScan_EveryDetectorFailingReportsEach(t *testing.T) {
	run := func(_ string, _ ...string) (string, error) {
		return "", errors.New("boom")
	}

	_, failures, err := Scan(degradedCatalog(), run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %d (%v), want 2", len(failures), failures)
	}
	methods := map[string]bool{}
	for _, f := range failures {
		methods[f.Method] = true
	}
	if !methods["brew-formula"] || !methods["npm"] {
		t.Errorf("failures should name both methods, got %+v", failures)
	}
}

func TestFailureErrorMessage(t *testing.T) {
	f := Failure{Method: "brew-formula", Err: errors.New("exit status 1")}
	if got, want := f.Error(), "brew-formula: exit status 1"; got != want {
		t.Errorf("Failure.Error() = %q, want %q", got, want)
	}
}
