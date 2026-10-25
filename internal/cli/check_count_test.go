package cli

import (
	"encoding/json"
	"regexp"
	"strconv"
	"testing"

	"ofa/internal/detect"
	"ofa/internal/resolve"
)

// countFixture returns the same mixed set for both renderings: two verified
// diffs and two self-tier updaters, plus one install that is simply current.
func countFixture() ([]detect.Instance, []resolve.Resolution) {
	insts := []detect.Instance{
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: detect.Active},
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Version: "1.3.0", Status: detect.Active},
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "agy", Method: "native", Bin: "agy", Version: "1.4.7", Status: detect.Active},
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Version: "0.164.0", Status: detect.Active},
	}
	res := []resolve.Resolution{
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
		{HarnessID: "pi", Method: "npm", Package: "@earendil-works/pi-coding-agent", Bin: "pi", Cur: "1.3.0", Latest: "1.4.0", Outdated: true, Tier: "npm"},
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "agy", Method: "native", Bin: "agy", Cur: "1.4.7", Tier: "self"},
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Cur: "0.164.0", Tier: "brew"},
	}
	return insts, res
}

var headlineRe = regexp.MustCompile(`scan: (\d+) harnesses, (\d+) with updates, (\d+) updaters available`)

// The human headline and the JSON payload must never disagree — a script
// reading update_count while the terminal says something else is the same bug
// in two coats.
func TestCheck_HumanHeadlineAgreesWithJSON(t *testing.T) {
	insts, res := countFixture()

	// Human mode.
	aHuman, outHuman, _, sdH, srH, _ := newTestApp(t, []string{"check"}, nil)
	sdH.instances, srH.res = insts, res
	humanCode := aHuman.Run()

	// JSON mode, identical inputs.
	aJSON, outJSON, _, sdJ, srJ, _ := newTestApp(t, []string{"check", "--json"}, nil)
	sdJ.instances, srJ.res = insts, res
	jsonCode := aJSON.Run()

	m := headlineRe.FindStringSubmatch(outHuman.String())
	if m == nil {
		t.Fatalf("headline did not match expected shape:\n%s", outHuman.String())
	}
	harnesses, humanUpdates, humanUpdaters := m[1], m[2], m[3]

	var payload struct {
		HarnessCount int `json:"harness_count"`
		UpdateCount  int `json:"update_count"`
		UpdaterCount int `json:"updater_available_count"`
	}
	if err := json.Unmarshal(outJSON.Bytes(), &payload); err != nil {
		t.Fatalf("JSON did not parse: %v\n%s", err, outJSON.String())
	}

	// Fixtures carry 2 verified diffs and 2 self-tier updaters.
	if humanUpdates != "2" {
		t.Errorf("human headline updates = %s, want 2", humanUpdates)
	}
	if humanUpdaters != "2" {
		t.Errorf("human headline updaters = %s, want 2", humanUpdaters)
	}
	if payload.UpdateCount != 2 || payload.UpdaterCount != 2 {
		t.Errorf("JSON counts = %d updates / %d updaters, want 2 / 2",
			payload.UpdateCount, payload.UpdaterCount)
	}
	// Compare as strings: the human line is text, and Itoa keeps the two
	// sides honest without re-parsing one into the other's type.
	if strconv.Itoa(payload.UpdateCount) != humanUpdates {
		t.Errorf("JSON update_count = %d, human headline says %s", payload.UpdateCount, humanUpdates)
	}
	if strconv.Itoa(payload.UpdaterCount) != humanUpdaters {
		t.Errorf("JSON updater_available_count = %d, human headline says %s", payload.UpdaterCount, humanUpdaters)
	}
	if strconv.Itoa(payload.HarnessCount) != harnesses {
		t.Errorf("JSON harness_count = %d, human headline says %s", payload.HarnessCount, harnesses)
	}

	// Both modes must also agree on the exit code: 2 verified diffs pending.
	if humanCode != ExitUpdatesPending || jsonCode != ExitUpdatesPending {
		t.Errorf("exit codes = human %d, json %d; both want %d",
			humanCode, jsonCode, ExitUpdatesPending)
	}
}

// The README contract: exit 0 means nothing is pending. A machine with only
// self-tier harnesses available must return 0 in both renderings, or a script
// gated on "no updates left" can never fire.
func TestCheck_OnlySelfTierExits0InBothModes(t *testing.T) {
	insts := []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "agy", Method: "native", Bin: "agy", Version: "1.4.7", Status: detect.Active},
	}
	res := []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "agy", Method: "native", Bin: "agy", Cur: "1.4.7", Tier: "self"},
	}

	aH, outH, _, sdH, srH, _ := newTestApp(t, []string{"check"}, nil)
	sdH.instances, srH.res = insts, res
	humanCode := aH.Run()

	aJ, outJ, _, sdJ, srJ, _ := newTestApp(t, []string{"check", "--json"}, nil)
	sdJ.instances, srJ.res = insts, res
	jsonCode := aJ.Run()

	if humanCode != ExitOK {
		t.Errorf("human check exit = %d, want %d (nothing known outdated)", humanCode, ExitOK)
	}
	if jsonCode != ExitOK {
		t.Errorf("JSON check exit = %d, want %d", jsonCode, ExitOK)
	}
	if !regexp.MustCompile(`0 with updates, 2 updaters available`).MatchString(outH.String()) {
		t.Errorf("headline should read 0 updates / 2 updaters, got:\n%s", outH.String())
	}

	var payload struct {
		UpdateCount  int `json:"update_count"`
		UpdaterCount int `json:"updater_available_count"`
	}
	if err := json.Unmarshal(outJ.Bytes(), &payload); err != nil {
		t.Fatalf("JSON did not parse: %v\n%s", err, outJ.String())
	}
	if payload.UpdateCount != 0 {
		t.Errorf("update_count = %d, want 0", payload.UpdateCount)
	}
	if payload.UpdaterCount != 2 {
		t.Errorf("updater_available_count = %d, want 2 (updaters must stay visible)", payload.UpdaterCount)
	}
}
