package picker

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"ofa/internal/resolve"
)

// keyMsg is a tiny helper so tests read like "press space" rather than
// constructing a bubbletea v2 KeyPressMsg literal each time. Special
// keys map to their v2 Code constants; everything else is a printable
// keystroke whose Text drives String(). The default branch assumes a
// single-rune key — multi-rune cases would need their own mapping.
func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case " ":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	default:
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
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
	}, nil)
	return newModel(items)
}

func TestBuildItems_FiltersAtLatest(t *testing.T) {
	items := buildItems([]resolve.Resolution{
		{HarnessID: "codex", Tier: "brew"},               // at-latest
		{HarnessID: "claude", Tier: "self"},              // include (updater available)
		{HarnessID: "qwen", Outdated: true, Tier: "npm"}, // include (outdated)
	}, nil)
	if len(items) != 2 {
		t.Errorf("want 2 items (at-latest dropped), got %d", len(items))
	}
}

func TestBuildItems_FiltersUnreachableShadowedOnly(t *testing.T) {
	items := buildItems([]resolve.Resolution{
		{HarnessID: "opencode", Method: "native", Tier: "self", Shadowed: true},         // native + shadowed → drop
		{HarnessID: "qwen", Method: "npm", Outdated: true, Tier: "npm", Shadowed: true}, // registry updater → include
		{HarnessID: "claude", Method: "native", Tier: "self"},                           // include
	}, nil)
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d: %+v", len(items), items)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Resolution.HarnessID)
	}
	if got[0] != "qwen" || got[1] != "claude" {
		t.Errorf("items = %v, want [qwen claude]", got)
	}
	if !strings.Contains(items[0].Label, "(shadowed)") {
		t.Errorf("shadowed registry row should be marked: %q", items[0].Label)
	}
	if strings.Contains(items[1].Label, "(shadowed)") {
		t.Errorf("active row should not be marked: %q", items[1].Label)
	}
}

func TestRun_NoItemsReturnsEmptyOutcome(t *testing.T) {
	out, err := Run(nil, nil)
	if err != nil {
		t.Fatalf("Run(nil, nil) err = %v", err)
	}
	if out.Quit || out.Apply || len(out.Selected) != 0 {
		t.Errorf("Run(nil, nil) outcome = %+v, want empty", out)
	}
}

func TestRun_AllAtLatestReturnsEmptyOutcome(t *testing.T) {
	res := []resolve.Resolution{{HarnessID: "x", Tier: "brew"}}
	out, err := Run(res, nil)
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
	m = updateKey(t, m, " ") // select item 0
	m = updateKey(t, m, "a") // a now sees not-all-selected, fills the rest
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
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
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
	v := m.View().Content
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
	v := m.View().Content
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
	if v := m.View().Content; v != "" {
		t.Errorf("quitting View = %q, want empty", v)
	}
}

// TestView_UsesAltScreen pins the v2 migration contract: bubbletea v2
// removed the WithAltScreen program option, so the model must declare
// alt screen on every view itself or the picker loses its full-screen
// takeover (and the startup transcript below it).
func TestView_UsesAltScreen(t *testing.T) {
	m := fixture()
	if !m.View().AltScreen {
		t.Error("View should set AltScreen = true (v2 replaced WithAltScreen)")
	}
	m.quitting = true
	if !m.View().AltScreen {
		t.Error("quitting View should keep AltScreen until teardown")
	}
}

func TestView_CursorMarkOnFirstByDefault(t *testing.T) {
	m := fixture()
	v := m.View().Content
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

// TestView_BlurbBetweenBannerAndFirstRow pins the stack order:
// banner → blank → blurb → blank → rows → blank → footer, and checks the
// blurb text survives wrapping intact (trimmed lines rejoin to tagline).
func TestView_BlurbBetweenBannerAndFirstRow(t *testing.T) {
	m := fixture()
	v := m.View().Content
	lines := viewLines(v)
	start, end := blurbBlock(t, v)

	bannerLine := -1
	rowLine := -1
	for i, l := range lines {
		if bannerLine < 0 && strings.Contains(l, "\x1b[38;2;") {
			bannerLine = i
		}
		if rowLine < 0 && strings.Contains(l, "[ ] claude") {
			rowLine = i
		}
	}
	if bannerLine < 0 || bannerLine >= start {
		t.Errorf("banner (line %d) should end before blurb starts (line %d)\n%s", bannerLine, start, v)
	}
	if start == 0 || lines[start-1] != "" {
		t.Errorf("blurb should be preceded by a blank separator, got %q\n%s", lines[start-1], v)
	}
	if end+1 >= len(lines) || lines[end+1] != "" {
		t.Errorf("blurb should be followed by a blank separator\n%s", v)
	}
	if rowLine != end+2 {
		t.Errorf("first harness row at line %d, want %d (right after blurb + blank)\n%s", rowLine, end+2, v)
	}
	// No words lost or invented by the wrap: the trimmed blurb lines
	// rejoin (whitespace-normalized) to the exact tagline constant.
	joined := strings.Join(strings.Fields(strings.Join(lines[start:end+1], " ")), " ")
	if joined != tagline {
		t.Errorf("blurb text = %q, want %q", joined, tagline)
	}
}

// TestView_BlurbLinesFitFrame checks centering: every blurb line's total
// width (padding included) equals maxWidth, the same frame the banner
// rows are padded to. A line longer than maxWidth would drift off-center.
func TestView_BlurbLinesFitFrame(t *testing.T) {
	m := fixture()
	v := m.View().Content
	lines := viewLines(v)
	start, end := blurbBlock(t, v)

	width := 0
	for _, it := range m.items {
		if w := 6 + utf8.RuneCountInString(it.Label); w > width {
			width = w
		}
	}
	if end == start {
		t.Errorf("tagline (%d runes) should wrap to >1 line within a %d-col frame", utf8.RuneCountInString(tagline), width)
	}
	for i := start; i <= end; i++ {
		if got := utf8.RuneCountInString(lines[i]); got != width {
			t.Errorf("blurb line %d width = %d, want maxWidth %d: %q", i-start+1, got, width, lines[i])
		}
	}
}

// TestView_BlurbHasNoSGR: the blurb must be plain spaces + text. An SGR
// escape would recolor the rest of the line and clash with the banner's
// baked 24-bit colors.
func TestView_BlurbHasNoSGR(t *testing.T) {
	m := fixture()
	v := m.View().Content
	_, end := blurbBlock(t, v)
	lines := viewLines(v)
	for i := range lines {
		if i > end {
			break
		}
		if strings.HasPrefix(strings.TrimLeft(lines[i], " "), "One-For-All.") || strings.HasPrefix(strings.TrimLeft(lines[i], " "), "Choose which harnesses") {
			if strings.Contains(lines[i], "\x1b") {
				t.Errorf("blurb line %q contains an SGR escape", lines[i])
			}
		}
	}
}

// blurbBlock returns the [start, end] line indexes of the blurb inside a
// rendered View: the first line containing the tagline's opening, through
// consecutive non-blank lines.
func blurbBlock(t *testing.T, v string) (int, int) {
	t.Helper()
	lines := viewLines(v)
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "One-For-All. Harness Updater.") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("View missing tagline line 1:\n%s", v)
	}
	end := start
	for end+1 < len(lines) && lines[end+1] != "" {
		end++
	}
	return start, end
}

// TestCenterWrap_NarrowWidthDoesNotPanic: when a single unbreakable word
// exceeds the frame (narrow terminal / long label), greedy wrapping
// cannot break it — padCenter must clamp instead of passing a negative
// count to strings.Repeat (which panics), and the wrapper must still
// return non-empty lines.
func TestCenterWrap_NarrowWidthDoesNotPanic(t *testing.T) {
	lines := centerWrap(tagline, 5) // narrower than "One-For-All."
	if len(lines) == 0 {
		t.Fatal("centerWrap at width 5 returned no lines")
	}
	for i, l := range lines {
		if l == "" {
			t.Errorf("line %d is empty: %q", i, lines)
		}
	}
}

// TestCenterWrap_ShortTextIsOneLine: text already inside the frame comes
// back as exactly one padded line — no trailing blank line.
func TestCenterWrap_ShortTextIsOneLine(t *testing.T) {
	lines := centerWrap("One-For-All. Harness Updater.", 81)
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 line, got %d: %q", len(lines), lines)
	}
	if got := utf8.RuneCountInString(lines[0]); got != 81 {
		t.Errorf("padded line width = %d, want 81: %q", got, lines[0])
	}
}

// TestCenterWrap_BlankReturnsNoLines: blank input must yield zero lines,
// not one empty padded line.
func TestCenterWrap_BlankReturnsNoLines(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n "} {
		if lines := centerWrap(in, 81); len(lines) != 0 {
			t.Errorf("centerWrap(%q) = %q, want no lines", in, lines)
		}
	}
}

// viewLines splits a rendered View into lines.
func viewLines(v string) []string { return strings.Split(v, "\n") }

// contains is a tiny strings.Contains shim retained for the tests below.
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

func TestBuildItems_MarksPinned(t *testing.T) {
	items := buildItems([]resolve.Resolution{
		{HarnessID: "qwen", Outdated: true, Tier: "npm"},
		{HarnessID: "claude", Tier: "self"},
	}, map[string]bool{"qwen": true})
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (pinned rows stay visible)", len(items))
	}
	if !items[0].Pinned {
		t.Error("qwen should be marked pinned")
	}
	if items[1].Pinned {
		t.Error("claude should not be marked pinned")
	}
}

func TestModel_PinnedRowsAreNotSelectable(t *testing.T) {
	m := newModel(buildItems([]resolve.Resolution{
		{HarnessID: "qwen", Outdated: true, Tier: "npm"},
		{HarnessID: "claude", Tier: "self"},
	}, map[string]bool{"qwen": true}))

	// The cursor starts on the first selectable row, skipping the pin.
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (first unpinned row)", m.cursor)
	}

	m = updateKey(t, m, "k")
	if m.cursor != 1 {
		t.Errorf("up from the first unpinned row moved to %d, want to stay at 1", m.cursor)
	}

	m = updateKey(t, m, "a")
	if len(m.SelectedIndices()) != 1 || !m.selected[1] {
		t.Errorf("a should select only the unpinned row, got %v", m.SelectedIndices())
	}

	// A space while a pinned row is somehow highlighted is a no-op.
	m2 := newModel(buildItems([]resolve.Resolution{
		{HarnessID: "qwen", Outdated: true, Tier: "npm"},
	}, map[string]bool{"qwen": true}))
	m2 = updateKey(t, m2, " ")
	if len(m2.SelectedIndices()) != 0 {
		t.Errorf("space selected a pinned row: %v", m2.SelectedIndices())
	}
}

func TestView_PinnedRowIsAnnotated(t *testing.T) {
	m := newModel(buildItems([]resolve.Resolution{
		{HarnessID: "qwen", Bin: "qwen", Cur: "0.22.1", Latest: "0.25.2", Outdated: true, Tier: "npm"},
		{HarnessID: "claude", Tier: "self"},
	}, map[string]bool{"qwen": true}))

	v := m.View().Content
	if !contains(v, "(pinned)") {
		t.Errorf("View should annotate the pinned row, got:\n%s", v)
	}
}
