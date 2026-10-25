package picker

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"ofa/internal/banner"
)

// model is the bubbletea state machine. Public for tea.Program's
// final-model assertion in Run(), but its fields are unexported to keep
// invariants (selected only indexes existing items) enforceable.
type model struct {
	items    []item
	cursor   int          // index of the highlighted row
	selected map[int]bool // selection set, keyed by row index
	quitting bool         // true once user has chosen to leave
	outcome  Outcome      // what Run() should return
}

// newModel builds an empty-selection model for the given items, with the
// cursor on the first selectable (unpinned) row.
func newModel(items []item) model {
	m := model{items: items, selected: map[int]bool{}}
	for i, it := range items {
		if !it.Pinned {
			m.cursor = i
			break
		}
	}
	return m
}

// nextSelectable moves the cursor by delta until it lands on an unpinned row.
// It is a no-op when there is nothing selectable in that direction, so the
// cursor never rests on a pinned row.
func (m model) nextSelectable(delta int) model {
	for i := m.cursor + delta; i >= 0 && i < len(m.items); i += delta {
		if !m.items[i].Pinned {
			m.cursor = i
			return m
		}
	}
	return m
}

// Init returns the initial command. Nothing to do — the picker is
// driven entirely by keystrokes.
func (m model) Init() tea.Cmd { return nil }

// Update routes messages to the right handler. Exported via the bubbletea
// Model interface; tests drive it directly with tea.KeyPressMsg values.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey processes a single keystroke. The dispatch table is small
// enough that one switch is clearer than a key map; if it grows past ~12
// keys, split it.
func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		m.outcome = Outcome{Quit: true}
		return m, tea.Quit
	case "up", "k":
		return m.nextSelectable(-1), nil
	case "down", "j":
		return m.nextSelectable(1), nil
	case "space":
		if m.items[m.cursor].Pinned {
			return m, nil
		}
		if m.selected[m.cursor] {
			delete(m.selected, m.cursor)
		} else {
			m.selected[m.cursor] = true
		}
		return m, nil
	case "a":
		// Toggle: if every selectable row is selected, clear all; otherwise
		// select all. Three-state would be confusing here; the user can
		// always space individual rows off after `a`. Pinned rows are
		// neither selected nor cleared — they have no checkbox state.
		allSelected := false
		for i, it := range m.items {
			if it.Pinned {
				continue
			}
			allSelected = true
			if !m.selected[i] {
				allSelected = false
				break
			}
		}
		for i, it := range m.items {
			if it.Pinned {
				continue
			}
			m.selected[i] = !allSelected
		}
		return m, nil
	case "enter":
		m.outcome = Outcome{Apply: true}
		for i, it := range m.items {
			if m.selected[i] {
				m.outcome.Selected = append(m.outcome.Selected, it.Resolution)
			}
		}
		return m, tea.Quit
	}
	return m, nil
}

// tagline is the centered blurb under the OFA banner. The lowercase "a"
// is deliberate: the key handler binds case "a" only, and the footer
// reads "a all" — uppercase A does nothing.
const tagline = "One-For-All. Harness Updater. Choose which harnesses to update or press a to update all at once."

// centerWrap wraps text into lines no wider than width columns, then
// pads every line to exactly width with the same left-biased padding
// the banner rows use ((extra+1)/2 left), so the blurb sits
// symmetrically in the same frame as the logomark. Breaking prefers
// sentence boundaries (readable prose), falling back to greedy word
// wrap within a sentence that cannot fit the frame; breaks only ever
// land on spaces. Padding is plain spaces only — no SGR escapes — so
// the terminal background shows through and the banner's baked colors
// stay intact.
func centerWrap(text string, width int) []string {
	var lines []string
	var cur []string
	curW := 0
	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, padCenter(strings.Join(cur, " "), width))
			cur, curW = nil, 0
		}
	}
	addWord := func(word string) {
		w := utf8.RuneCountInString(word)
		if curW > 0 && curW+1+w > width {
			flush()
		}
		if curW > 0 {
			curW++ // the joining space
		}
		cur = append(cur, word)
		curW += w
	}
	for _, group := range sentenceGroups(text) {
		gw := groupWidth(group)
		if curW > 0 && curW+1+gw > width {
			flush()
		}
		if curW == 0 && gw > width {
			// The sentence alone overflows the frame; word-wrap it.
			for _, w := range group {
				addWord(w)
			}
			continue
		}
		if curW > 0 {
			curW++
		}
		cur = append(cur, group...)
		curW += gw
	}
	flush()
	return lines
}

// padCenter pads s to exactly width columns of plain spaces, biased
// (extra+1)/2 to the left — the same frame the banner rows use. Content
// already at or beyond width is returned unpadded: strings.Repeat panics
// on a negative count, and breaking an over-long line is centerWrap's
// job, not the pad's.
func padCenter(s string, width int) string {
	extra := width - utf8.RuneCountInString(s)
	if extra <= 0 {
		return s
	}
	left := strings.Repeat(" ", (extra+1)/2)
	return left + s + strings.Repeat(" ", extra-len(left))
}

// sentenceGroups splits words into sentence-sized groups: a group closes
// after a word ending in '.', '!' or '?' (or when the input runs out).
// A single unwrapped sentence is one group, so short text yields one
// line and blank input yields none.
func sentenceGroups(text string) [][]string {
	var groups [][]string
	var cur []string
	for _, w := range strings.Fields(text) {
		cur = append(cur, w)
		if strings.HasSuffix(w, ".") || strings.HasSuffix(w, "!") || strings.HasSuffix(w, "?") {
			groups = append(groups, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// groupWidth is the rune width of a sentence group including its
// internal joining spaces.
func groupWidth(group []string) int {
	w := 0
	for i, word := range group {
		if i > 0 {
			w++
		}
		w += utf8.RuneCountInString(word)
	}
	return w
}

// View renders the picker as a v2 tea.View. The frame is empty when
// quitting so the alt-screen tears down cleanly. Alt screen is declared
// here because bubbletea v2 dropped the WithAltScreen program option —
// the view owns terminal modes. The picker is always full-screen.
func (m model) View() tea.View {
	v := tea.NewView(m.frame())
	v.AltScreen = true
	return v
}

// frame is the rendered picker body (banner, blurb, rows, footer), or
// the empty string once quitting.
func (m model) frame() string {
	if m.quitting || len(m.items) == 0 {
		return ""
	}
	var b strings.Builder

	// Find the widest item row so we can center the banner above it.
	// Each row is `cursor + "[" + check + "] " + label` — the prefix is
	// always 6 chars ("> " or "  ", then "[x]" or "[ ]", then " "), so
	// only the label length varies. Rune count because labels may carry
	// unicode (e.g., the "→" in `claude → updater available`).
	maxWidth := 0
	for _, it := range m.items {
		w := 6 + utf8.RuneCountInString(it.Label)
		if it.Pinned {
			w += len(" (pinned)")
		}
		if w > maxWidth {
			maxWidth = w
		}
	}

	// Banner is the OFA logomark (35x8) with SGR 24-bit color escapes
	// already inlined. bubbletea forwards these to the terminal verbatim,
	// so the fade between #e67a4c and #4282d7 lands on screen as baked.
	// Note: the picker always emits colors. NO_COLOR is honored by the
	// banner.Render path used for --help, but the picker requires a TTY
	// and the terminal handles color display from there.
	//
	// Center it above the items: each row gets (maxWidth-bannerWidth)/2
	// plain leading spaces and the remainder as trailing spaces, so every
	// banner row is exactly maxWidth wide and the logomark sits over the
	// middle of the items. Plain spaces carry no SGR code, so the
	// terminal's background color shows through and the colored cells
	// stay intact. A trailing blank line separates the banner from the
	// first row.
	const bannerWidth = banner.Cols
	extra := maxWidth - bannerWidth
	var leftPad, rightPad string
	if extra > 0 {
		leftPad = strings.Repeat(" ", (extra+1)/2) // bias extra to the left
		rightPad = strings.Repeat(" ", extra-len(leftPad))
	}
	for _, line := range strings.Split(banner.Art, "\n") {
		fmt.Fprintf(&b, "%s%s%s\n", leftPad, line, rightPad)
	}
	b.WriteString("\n")
	for _, line := range centerWrap(tagline, maxWidth) {
		fmt.Fprintln(&b, line)
	}
	b.WriteString("\n")
	for i, it := range m.items {
		cursor := "  "
		if m.cursor == i {
			cursor = "> "
		}
		check := " "
		if m.selected[i] {
			check = "x"
		}
		label := it.Label
		if it.Pinned {
			// Dim + explicit suffix: the row matches `ofa check` instead of
			// vanishing, and explains why space does nothing.
			label = "\x1b[2m" + label + " (pinned)\x1b[0m"
		}
		fmt.Fprintf(&b, "%s[%s] %s\n", cursor, check, label)
	}
	b.WriteString("\n↑/↓ navigate · space toggle · a all · enter apply · q quit\n")
	return b.String()
}

// SelectedIndices is exposed for tests that want to inspect the
// picker's internal selection state without going through Outcome (which
// is set only at exit).
func (m model) SelectedIndices() []int {
	out := make([]int, 0, len(m.selected))
	for i := range m.selected {
		if m.selected[i] {
			out = append(out, i)
		}
	}
	return out
}
