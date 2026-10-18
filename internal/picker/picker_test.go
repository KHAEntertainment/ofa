package picker

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ofa/internal/resolve"
)

// keyMsg is a tiny helper so tests read like "press space" rather than
// constructing a bubbletea KeyMsg literal each time.
func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// updateKey drives the model through one keystroke and returns the
// updated model. Discards the bubbletea Cmd (the picker issues only
// tea.Quit on exit).
func updateKey(t *testing.T, m model, k string) model {
	t.Helper()
	updated, _ := m.Update(keyMsg(k))
	got, ok := updated.(model)
	if !ok {
		t.Fatalf("Update(%q) returned wrong type %T", k, updated)
	}
	return got
}

// fixture builds a model with three test items: a self-updater, an
// outdated npm install, and an at-latest install (which should not appear
// in the picker).
func fixture() model {
	items := buildItems([]resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
		{HarnessID: "codex", Method: "brew-cask", Package: "codex", Bin: "codex", Cur: "0.162.1", Tier: "brew"}, // at-latest → dropped
	})
	return newModel(items)
}

func TestBuildItems_FiltersAtLatest(t *testing.T) {
	items := buildItems([]resolve.Resolution{
		{HarnessID: "codex", Tier: "brew"},                  // at-latest
		{HarnessID: "claude", Tier: "self"},                  // include (updater available)
		{HarnessID: "qwen", Outdated: true, Tier: "npm"},    // include (outdated)
	})
	if len(items) != 2 {
		t.Errorf("want 2 items (at-latest dropped), got %d", len(items))
	}
}

func TestRun_NoItemsReturnsEmptyOutcome(t *testing.T) {
	out, err := Run(nil)
	if err != nil {
		t.Fatalf("Run(nil) err = %v", err)
	}
	if out.Quit || out.Apply || len(out.Selected) != 0 {
		t.Errorf("Run(nil) outcome = %+v, want empty", out)
	}
}

func TestRun_AllAtLatestReturnsEmptyOutcome(t *testing.T) {
	res := []resolve.Resolution{{HarnessID: "x", Tier: "brew"}}
	out, err := Run(res)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Apply || out.Quit || len(out.Selected) != 0 {
		t.Errorf("Run(at-latest-only) outcome = %+v, want empty", out)
	}
}

func TestModel_NavigationMovesCursor(t *testing.T) {
	m := fixture()
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}
	m = updateKey(t, m, "down")
	if m.cursor != 1 {
		t.Errorf("after down: cursor = %d, want 1", m.cursor)
	}
	m = updateKey(t, m, "j") // vim-style alias
	if m.cursor != 1 {
		// j at the bottom should not wrap
		t.Errorf("j at bottom: cursor = %d, want 1", m.cursor)
	}
	m = updateKey(t, m, "up")
	m = updateKey(t, m, "k")
	if m.cursor != 0 {
		t.Errorf("after up+k: cursor = %d, want 0", m.cursor)
	}
	m = updateKey(t, m, "up") // up at top should not wrap
	if m.cursor != 0 {
		t.Errorf("up at top: cursor = %d, want 0", m.cursor)
	}
}

func TestModel_SpaceToggles(t *testing.T) {
	m := fixture()
	m = updateKey(t, m, " ") // toggle item 0 on
	if !m.selected[0] {
		t.Error("after space: item 0 should be selected")
	}
	m = updateKey(t, m, " ") // toggle item 0 off
	if m.selected[0] {
		t.Error("after second space: item 0 should be deselected")
	}
}

func TestModel_ATogglesAllOnThenOff(t *testing.T) {
	m := fixture()
	m = updateKey(t, m, "a")
	for i := range m.items {
		if !m.selected[i] {
			t.Errorf("after first a: item %d not selected", i)
		}
	}
	m = updateKey(t, m, "a")
	for i := range m.items {
		if m.selected[i] {
			t.Errorf("after second a: item %d should be cleared", i)
		}
	}
}

func TestModel_APartialThenAFillsRest(t *testing.T) {
	m := fixture()
	m = updateKey(t, m, " ")        // select item 0
	m = updateKey(t, m, "a")        // a now sees not-all-selected, fills the rest
	for i := range m.items {
		if !m.selected[i] {
			t.Errorf("after partial+a: item %d not selected", i)
		}
	}
}

func TestModel_EnterReturnsApplyWithSelection(t *testing.T) {
	m := fixture()
	m = updateKey(t, m, "down") // move to item 1 (qwen)
	m = updateKey(t, m, " ")    // select qwen
	updated, cmd := m.Update(keyMsg("enter"))
	got, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned wrong type %T", updated)
	}
	if !got.outcome.Apply {
		t.Errorf("outcome.Apply = false, want true")
	}
	if got.outcome.Quit {
		t.Errorf("outcome.Quit = true, want false")
	}
	if len(got.outcome.Selected) != 1 {
		t.Fatalf("want 1 selected, got %d", len(got.outcome.Selected))
	}
	if got.outcome.Selected[0].HarnessID != "qwen" {
		t.Errorf("selected HarnessID = %q, want qwen", got.outcome.Selected[0].HarnessID)
	}
	if cmd == nil {
		t.Error("Update(enter) should issue a tea.Quit cmd")
	}
}

func TestModel_EnterWithNoSelectionStillAppliesButEmpty(t *testing.T) {
	// Edge case: user pressed Enter without selecting anything. The picker
	// exits cleanly with Apply=true and an empty Selected list. The caller
	// (cli) treats this as a no-op apply.
	m := fixture()
	updated, _ := m.Update(keyMsg("enter"))
	got := updated.(model)
	if !got.outcome.Apply {
		t.Error("outcome.Apply should be true even with no selection")
	}
	if len(got.outcome.Selected) != 0 {
		t.Errorf("want empty Selected, got %d", len(got.outcome.Selected))
	}
}

func TestModel_QuitReturnsQuitOutcome(t *testing.T) {
	m := fixture()
	m = updateKey(t, m, " ") // select something first so we can verify it isn't returned
	updated, cmd := m.Update(keyMsg("q"))
	got := updated.(model)
	if !got.outcome.Quit {
		t.Error("outcome.Quit should be true")
	}
	if got.outcome.Apply {
		t.Error("outcome.Apply should be false on q")
	}
	if len(got.outcome.Selected) != 0 {
		t.Errorf("q should not carry selections, got %d", len(got.outcome.Selected))
	}
	if cmd == nil {
		t.Error("Update(q) should issue a tea.Quit cmd")
	}
	if !got.quitting {
		t.Error("quitting flag should be set")
	}
}

func TestModel_CtrlCQuits(t *testing.T) {
	m := fixture()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := updated.(model)
	if !got.outcome.Quit {
		t.Error("ctrl+c should set Quit")
	}
}

func TestModel_UnknownKeyIsNoop(t *testing.T) {
	m := fixture()
	before := m
	updated, _ := m.Update(keyMsg("x"))
	got := updated.(model)
	if got.cursor != before.cursor {
		t.Errorf("unknown key changed cursor from %d to %d", before.cursor, got.cursor)
	}
	if got.quitting {
		t.Error("unknown key set quitting")
	}
}

func TestView_ContainsItemsAndFooter(t *testing.T) {
	m := fixture()
	v := m.View()
	for _, want := range []string{"claude", "qwen", "navigate", "space toggle", "apply", "quit"} {
		if !contains(v, want) {
			t.Errorf("View missing %q\n%s", want, v)
		}
	}
	if contains(v, "codex") {
		t.Errorf("View should not contain at-latest 'codex', got:\n%s", v)
	}
}

func TestView_HeaderIsBanner(t *testing.T) {
	m := fixture()
	v := m.View()
	// The banner is baked with SGR 24-bit color escapes (the orange→blue
	// fade). Its presence in the View is the contract: every refresh of
	// the picker re-emits it above the rows.
	if !contains(v, "\x1b[38;2;") {
		t.Errorf("View should contain banner ANSI escapes, got:\n%s", v)
	}
	// The banner must come before the first item row so the order is
	// banner → blank → rows → footer.
	bannerIdx := indexOf(v, "\x1b[38;2;")
	rowIdx := indexOf(v, "[ ] claude")
	if bannerIdx < 0 || rowIdx < 0 || bannerIdx > rowIdx {
		t.Errorf("banner should appear before item rows (banner@%d, claude@%d)\n%s",
			bannerIdx, rowIdx, v)
	}
}

func TestView_QuittingReturnsEmpty(t *testing.T) {
	m := fixture()
	m.quitting = true
	if v := m.View(); v != "" {
		t.Errorf("quitting View = %q, want empty", v)
	}
}

func TestView_CursorMarkOnFirstByDefault(t *testing.T) {
	m := fixture()
	v := m.View()
	if !contains(v, ">") {
		t.Errorf("View should mark cursor with '>', got:\n%s", v)
	}
}

func TestSelectedIndices(t *testing.T) {
	m := fixture()
	m = updateKey(t, m, "down")
	m = updateKey(t, m, " ")
	got := m.SelectedIndices()
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("SelectedIndices = %v, want [1]", got)
	}
}

func TestLabelFor(t *testing.T) {
	cases := []resolve.Resolution{
		{HarnessID: "claude", Method: "native", Bin: "claude", Cur: "2.1.300", Tier: "self"},
		{HarnessID: "qwen", Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
	}
	for _, r := range cases {
		got := labelFor(r)
		if got == "" {
			t.Errorf("labelFor(%+v) returned empty", r)
		}
		// Each label should contain the harness id so the picker is
		// scannable without parsing structured data.
		if !contains(got, r.HarnessID) {
			t.Errorf("labelFor(%+v) = %q does not contain harness id", r, got)
		}
		// Self-tier labels should mention the updater.
		if r.Tier == "self" && !contains(got, "updater available") {
			t.Errorf("self label should say 'updater available', got %q", got)
		}
		// Outdated labels should mention the latest version.
		if r.Outdated && !contains(got, r.Latest) {
			t.Errorf("outdated label should mention latest %q, got %q", r.Latest, got)
		}
	}
}

// contains is a tiny strings.Contains shim so we don't have to import
// "strings" in this file just for that.
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// indexOf returns the byte index of sub in s, or -1 if not found.
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
