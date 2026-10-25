// Package picker is the bubbletea TUI for selecting and applying harness
// updates interactively. Run() opens the alt-screen picker, lets the user
// navigate and toggle rows, and returns an Outcome describing what (if
// anything) to apply next.
//
// The picker is a thin UI shell over a pure state model — the bubbletea
// Update() method is exercised by unit tests that send tea.KeyPressMsg values
// directly, without a TTY. See picker_test.go for the test surface.
package picker

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"ofa/internal/resolve"
)

// Outcome is what Run() returns when the picker exits. Exactly one of
// Quit or Apply is true (or neither, if the program was canceled).
type Outcome struct {
	// Quit means the user pressed q / Ctrl-C and selected nothing.
	Quit bool
	// Apply means the user pressed Enter on at least one selection.
	Apply bool
	// Selected lists the resolutions the user picked, in picker order.
	Selected []resolve.Resolution
}

// Run opens the picker over the given resolutions. It blocks until the
// user quits or applies. The Outcome tells the caller what to do next.
//
// pins names harness ids the user froze: those rows are still shown (so the
// screen matches `ofa check`) but are dimmed and cannot be selected.
func Run(resolutions []resolve.Resolution, pins map[string]bool) (Outcome, error) {
	items := buildItems(resolutions, pins)
	if len(items) == 0 {
		// Nothing to do — short-circuit so the caller doesn't have to
		// spin up a TUI just to display "nothing to apply".
		return Outcome{}, nil
	}
	m := newModel(items)
	// Alt screen is no longer a program option — bubbletea v2 made it a
	// field the model declares in View(), so NewProgram takes no opts here.
	p := tea.NewProgram(m)
	final, err := p.Run()
	if err != nil {
		return Outcome{}, err
	}
	fm, ok := final.(model)
	if !ok {
		return Outcome{}, fmt.Errorf("picker: unexpected final model type %T", final)
	}
	return fm.outcome, nil
}

// item is one row in the picker. Label is pre-rendered for the View; the
// Resolution is preserved so the caller can map selections back to
// update.Request values.
type item struct {
	Resolution resolve.Resolution
	Label      string
	// Pinned marks a harness frozen in the user config. Kept as a row so
	// the picker shows the same set as `ofa check`, but never selectable.
	Pinned bool
}

func buildItems(res []resolve.Resolution, pins map[string]bool) []item {
	out := make([]item, 0, len(res))
	for _, r := range res {
		// Only items with an update available appear in the picker. At-
		// latest installs (Tier=brew/npm/pypi with Outdated=false) are
		// reported by `ofa check` but not actionable here. Shadowed
		// installs never appear — detect.Scan marks them but they have
		// no path to be the live binary, so updating them is wasted work.
		if r.Shadowed {
			continue
		}
		if !r.Outdated && r.Tier != "self" {
			continue
		}
		out = append(out, item{Resolution: r, Label: labelFor(r), Pinned: pins[r.HarnessID]})
	}
	return out
}

func labelFor(r resolve.Resolution) string {
	var action string
	switch {
	case r.Outdated:
		action = fmt.Sprintf("%s → %s", r.Bin, r.Latest)
	case r.Tier == "self":
		action = fmt.Sprintf("%s → updater available", r.Bin)
	default:
		action = r.Bin
	}
	return fmt.Sprintf("%-14s %-32s  %s", r.HarnessID, r.Cur, action)
}