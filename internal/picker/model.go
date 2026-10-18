package picker

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"ofa/internal/banner"
)

// model is the bubbletea state machine. Public for tea.Program's
// final-model assertion in Run(), but its fields are unexported to keep
// invariants (selected only indexes existing items) enforceable.
type model struct {
	items    []item
	cursor   int            // index of the highlighted row
	selected map[int]bool   // selection set, keyed by row index
	quitting bool           // true once user has chosen to leave
	outcome  Outcome        // what Run() should return
}

// newModel builds an empty-selection model for the given items.
func newModel(items []item) model {
	return model{items: items, selected: map[int]bool{}}
}

// Init returns the initial command. Nothing to do — the picker is
// driven entirely by keystrokes.
func (m model) Init() tea.Cmd { return nil }

// Update routes messages to the right handler. Exported via the bubbletea
// Model interface; tests drive it directly with tea.KeyMsg values.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey processes a single keystroke. The dispatch table is small
// enough that one switch is clearer than a key map; if it grows past ~12
// keys, split it.
func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.quitting = true
		m.outcome = Outcome{Quit: true}
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
		return m, nil
	case " ":
		if m.selected[m.cursor] {
			delete(m.selected, m.cursor)
		} else {
			m.selected[m.cursor] = true
		}
		return m, nil
	case "a":
		// Toggle: if every row is selected, clear all; otherwise select all.
		// Three-state would be confusing here; the user can always space
		// individual rows off after `a`.
		allSelected := len(m.items) > 0
		for i := range m.items {
			if !m.selected[i] {
				allSelected = false
				break
			}
		}
		for i := range m.items {
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

// View renders the picker. Returns empty string when quitting so the
// alt-screen tears down cleanly.
func (m model) View() string {
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
		if w > maxWidth {
			maxWidth = w
		}
	}

	// Banner is the OFA logomark (35x8) with SGR 24-bit color escapes
	// already inlined. bubbletea forwards these to the terminal verbatim,
	// so the fade between #e67a4c and #4282d7 lands on screen as baked.
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
	for i, it := range m.items {
		cursor := "  "
		if m.cursor == i {
			cursor = "> "
		}
		check := " "
		if m.selected[i] {
			check = "x"
		}
		fmt.Fprintf(&b, "%s[%s] %s\n", cursor, check, it.Label)
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