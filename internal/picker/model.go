package picker

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"ofa/internal/banner"
	"ofa/internal/resolve"
	"ofa/internal/update"
)

// phase is the flow's screen (issue #62). Run's select-only wrapper
// mode is always phaseSelect and never sees the other phases.
type phase int

const (
	phaseScan phase = iota
	phaseSelect
	phaseApply
	phaseSummary
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

	// Flow state (issue #62): f is nil in Run's wrapper mode, where the
	// model behaves exactly as it always has.
	f            *flowState
	phase        phase
	scanStopReq  bool // a scan stop was requested; waiting for the worker
	diags        []string
	rows         []applyRow
	applyStopReq bool // stop-after-current was requested
	summary      update.Summary
	summaryKind  summaryKind
	exitMsg      string
	exitCode     int
	anim         int // animation frame for spinner and bars
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

// newFlowModel builds the flow's model at the scanning screen. The scan
// worker is started by RunFlow; Init arms the queue waiter.
func newFlowModel(f *flowState) model {
	return model{selected: map[int]bool{}, f: f, phase: phaseScan}
}

// Init returns the initial command. The picker is driven entirely by
// keystrokes; the flow additionally arms its queue waiter and the
// animation tick.
func (m model) Init() tea.Cmd {
	if m.f == nil {
		return nil
	}
	return tea.Batch(waitCmd(m.f.q), m.animCmd())
}

// animCmd re-arms the single animation chain. It ticks for the whole
// program lifetime (one cheap tick per frame), so no phase transition
// can leak a second chain; screens that do not animate ignore the tick.
func (m model) animCmd() tea.Cmd {
	if m.quitting {
		return nil
	}
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return animTickMsg{} })
}

// queueWait re-arms the one queue waiter after a queued message was
// consumed. It returns nil once the model is quitting: the waiter left
// blocked at teardown is released by msgQueue.close after RunFlow joins
// the workers.
func (m model) queueWait() tea.Cmd {
	if m.quitting {
		return nil
	}
	return waitCmd(m.f.q)
}

// Update routes messages to the right handler. Exported via the bubbletea
// Model interface; tests drive it directly with tea.KeyPressMsg values
// and with the flow's worker messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case signalMsg:
		// External SIGINT/SIGTERM: routed here instead of bubbletea's
		// auto-quit, and handled exactly like the quit key — a stop
		// request where work is running, never an abandoned write.
		return m.handleQuitKey()
	case diagMsg:
		m.diags = append(m.diags, msg.line)
		return m, m.queueWait()
	case scanResultMsg:
		return m.handleScanResult(msg.res)
	case eventMsg:
		return m.handleEvent(msg.ev)
	case applyDoneMsg:
		m.summary = msg.summary
		m.phase = phaseSummary
		m.summaryKind = kindResults
		m.exitCode = msg.summary.ExitCode()
		return m, m.queueWait()
	case animTickMsg:
		m.anim++
		return m, m.animCmd()
	}
	return m, nil
}

// handleKey processes a single keystroke. The dispatch table is small
// enough that one switch is clearer than a key map; if it grows past ~12
// keys, split it.
func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.f != nil {
		switch m.phase {
		case phaseScan, phaseApply:
			// The quit key requests a stop — the flow never abandons
			// work — and every other key is ignored. The help line on
			// each screen says exactly that.
			if msg.String() == "ctrl+c" || msg.String() == "q" {
				return m.handleQuitKey()
			}
			return m, nil
		case phaseSummary:
			switch msg.String() {
			case "ctrl+c", "q", "enter":
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
		}
	}
	// phaseSelect, and Run's wrapper mode: the original dispatch table,
	// unchanged.
	switch msg.String() {
	case "ctrl+c", "q":
		return m.handleQuitKey()
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
		if m.f != nil {
			return m.startApply()
		}
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

// handleQuitKey handles q / ctrl+c / an external signal per phase. In the
// flow's scan and apply phases this requests a stop and waits for the
// join (the worker finishes its bounded work; nothing is torn down while
// it can still write). In the select phase and wrapper mode it quits
// with the Quit outcome; in the summary it dismisses.
func (m model) handleQuitKey() (tea.Model, tea.Cmd) {
	switch {
	case m.f == nil || m.phase == phaseSelect:
		m.quitting = true
		m.outcome = Outcome{Quit: true}
		return m, tea.Quit
	case m.phase == phaseScan:
		if !m.scanStopReq {
			m.scanStopReq = true
			close(m.f.scanStop)
		}
		return m, nil
	case m.phase == phaseApply:
		if !m.applyStopReq {
			m.applyStopReq = true
			close(m.f.applyStop)
		}
		return m, nil
	default: // phaseSummary
		m.quitting = true
		return m, tea.Quit
	}
}

// handleScanResult reacts to the scan worker's final answer. A requested
// stop wins over the result and quits clean (exit 0); a failed scan lands
// on a dismissible error screen carrying the caller's exit code; an empty
// scan lands on a dismissible "no updates" screen; otherwise the select
// screen opens with the same items Run would have built.
func (m model) handleScanResult(res ScanResult) (tea.Model, tea.Cmd) {
	if m.scanStopReq {
		m.quitting = true
		m.exitCode = 0
		return m, tea.Quit
	}
	if res.Code != 0 {
		m.phase = phaseSummary
		m.summaryKind = kindError
		m.exitMsg = "Scan failed."
		m.exitCode = res.Code
		return m, m.queueWait()
	}
	items := buildItems(res.Resolutions, m.f.cfg.Pins)
	if len(items) == 0 {
		m.phase = phaseSummary
		m.summaryKind = kindEmpty
		m.exitMsg = "No updates available."
		m.exitCode = 0
		return m, m.queueWait()
	}
	nm := newModel(items)
	m.items, m.cursor, m.selected = nm.items, nm.cursor, nm.selected
	m.phase = phaseSelect
	return m, m.queueWait()
}

// startApply builds the stable row list — one row per selected update,
// pre-skipped ones included — and launches the update worker. An empty
// selection, or a selection nothing can be built from, lands on the same
// dismissible screen with the old exit code (0).
func (m model) startApply() (tea.Model, tea.Cmd) {
	var selected []resolve.Resolution
	for i, it := range m.items {
		if m.selected[i] {
			selected = append(selected, it.Resolution)
		}
	}
	if len(selected) == 0 {
		m.phase = phaseSummary
		m.summaryKind = kindEmpty
		m.exitMsg = "Nothing selected."
		m.exitCode = 0
		return m, nil
	}
	requests, skipped := m.f.cfg.Build(selected)
	m.rows = make([]applyRow, 0, len(requests)+len(skipped))
	for _, sk := range skipped {
		m.rows = append(m.rows, applyRow{label: sk.Label, state: rowSkipped, reason: sk.Reason, reqIdx: -1})
	}
	for i, req := range requests {
		m.rows = append(m.rows, applyRow{label: req.Label, state: rowQueued, reqIdx: i})
	}
	if len(requests) == 0 {
		m.phase = phaseSummary
		m.summaryKind = kindEmpty
		m.exitMsg = "Nothing to apply."
		m.exitCode = 0
		return m, nil
	}
	m.phase = phaseApply
	m.f.startApply(requests)
	return m, nil
}

// handleEvent binds an update lifecycle event to its row. The event's
// Index is the request position — stable even when labels repeat — so
// the lookup never depends on names. A finished event without a result
// is an observer-path bug and shows as failed, never as silence.
func (m model) handleEvent(ev update.Event) (tea.Model, tea.Cmd) {
	row := -1
	for i := range m.rows {
		if m.rows[i].reqIdx == ev.Index {
			row = i
			break
		}
	}
	if row < 0 {
		return m, m.queueWait()
	}
	switch ev.Type {
	case update.EventStarted:
		m.rows[row].state = rowRunning
	case update.EventFinished:
		r := ev.Result
		switch {
		case r == nil:
			m.rows[row].state = rowFailed
			m.rows[row].reason = "no result reported"
		case r.Skipped:
			m.rows[row].state = rowSkipped
			m.rows[row].reason = r.SkippedReason
		case r.Failed || r.Err != nil:
			m.rows[row].state = rowFailed
			reason := fmt.Sprintf("exit %d", r.ExitCode)
			if r.Err != nil {
				reason = fmt.Sprintf("exit %d, err=%v", r.ExitCode, r.Err)
			}
			m.rows[row].reason = reason
			m.rows[row].tail = r.OutputTail
		default:
			m.rows[row].state = rowDone
		}
	}
	return m, m.queueWait()
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

// frame is the rendered body of the current screen, or the empty string
// once quitting.
func (m model) frame() string {
	if m.quitting {
		return ""
	}
	if m.f == nil || m.phase == phaseSelect {
		return m.selectFrame()
	}
	switch m.phase {
	case phaseScan:
		return m.scanFrame()
	case phaseApply:
		return m.applyFrame()
	case phaseSummary:
		return m.summaryFrame()
	}
	return ""
}

// bannerBlock renders the OFA banner centered above width columns,
// followed by a blank line, the given blurb wrapped and centered, and a
// closing blank line — the shared header of every screen. Its byte
// layout is the select screen's original one, unchanged.
func bannerBlock(width int, blurb string) string {
	var b strings.Builder

	// Banner is the OFA logomark (35x8) with SGR 24-bit color escapes
	// already inlined. bubbletea forwards these to the terminal verbatim,
	// so the fade between #e67a4c and #4282d7 lands on screen as baked.
	// Note: the picker always emits colors. NO_COLOR is honored by the
	// banner.Render path used for --help, but the picker requires a TTY
	// and the terminal handles color display from there.
	//
	// Center it above the content: each row gets (width-bannerWidth)/2
	// plain leading spaces and the remainder as trailing spaces, so every
	// banner row is exactly width wide and the logomark sits over the
	// middle of the content. Plain spaces carry no SGR code, so the
	// terminal's background color shows through and the colored cells
	// stay intact. A trailing blank line separates the banner from the
	// first row.
	const bannerWidth = banner.Cols
	extra := width - bannerWidth
	var leftPad, rightPad string
	if extra > 0 {
		leftPad = strings.Repeat(" ", (extra+1)/2) // bias extra to the left
		rightPad = strings.Repeat(" ", extra-len(leftPad))
	}
	for _, line := range strings.Split(banner.Art, "\n") {
		fmt.Fprintf(&b, "%s%s%s\n", leftPad, line, rightPad)
	}
	b.WriteString("\n")
	for _, line := range centerWrap(blurb, width) {
		fmt.Fprintln(&b, line)
	}
	b.WriteString("\n")
	return b.String()
}

// contentWidth is the widest rendered content line, used to center the
// banner above a screen body the same way the select screen does.
func contentWidth(lines []string) int {
	w := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > w {
			w = n
		}
	}
	return w
}

// diagLines renders the most recent diagnostics with an explicit counter
// for anything elided — a diagnostic is never dropped silently. max is
// how many of the most recent lines to show.
func diagLines(diags []string, max int) []string {
	if len(diags) == 0 {
		return nil
	}
	var out []string
	skip := len(diags) - max
	if skip > 0 {
		out = append(out, fmt.Sprintf("  … %d earlier diagnostic line(s)", skip))
	}
	lo := skip
	if lo < 0 {
		lo = 0
	}
	for _, line := range diags[lo:] {
		out = append(out, "  "+line)
	}
	return out
}

// scanFrame renders the scanning screen (issue #43): the banner, the
// exact caption and the short tagline — on every run, first run
// included — live diagnostics, and the stop hint. Stopping replaces the
// hint with an accurate status line; the screen never claims a
// synchronous detector was canceled while it still runs.
func (m model) scanFrame() string {
	content := []string{
		fmt.Sprintf("%s %s", spinnerFrame(m.anim), ScanCaption),
		ScanDetail,
		"",
	}
	content = append(content, diagLines(m.diags, 8)...)
	stop := "q/ctrl+c stop scanning"
	if m.scanStopReq {
		stop = "Stopping — waiting for the scan to finish…"
	}
	content = append(content, "", stop)

	var b strings.Builder
	b.WriteString(bannerBlock(contentWidth(content), ScanTagline))
	for _, line := range content {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// applyFrame renders the progress screen: one stable row per selected
// update with its small bar and state. Unknown installer progress is
// indeterminate — the running bar slides and never shows a number — and
// a row keeps its final state once its update ends.
func (m model) applyFrame() string {
	content := []string{"Applying selected updates", ""}
	for _, r := range m.rows {
		content = append(content, rowLine(r, m.anim))
	}
	content = append(content, "")
	if m.applyStopReq {
		content = append(content,
			"Stopping after current update…",
			"Current update will finish. Remaining updates are skipped as canceled.",
			"",
		)
	}
	content = append(content, "q/ctrl+c stop after current update")

	var b strings.Builder
	b.WriteString(bannerBlock(contentWidth(content), ScanTagline))
	for _, line := range content {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// summaryFrame renders the final screen: per-row results with failure
// tails and skip reasons, the counts, and any diagnostics. It stays
// visible until the user dismisses it — nothing here auto-exits.
func (m model) summaryFrame() string {
	var content []string
	switch m.summaryKind {
	case kindError, kindEmpty:
		content = append(content, m.exitMsg, "")
	default:
		content = append(content, "Updates finished", "")
		for _, r := range m.rows {
			content = append(content, rowLine(r, 0))
			for _, line := range strings.Split(r.tail, "\n") {
				if strings.TrimSpace(line) != "" {
					content = append(content, "      "+line)
				}
			}
		}
		// Counts cover every selected update's row — pre-skipped and
		// canceled included — so the numbers add up to what the user
		// picked, not only what actually executed.
		var done, failed, skipped int
		for _, r := range m.rows {
			switch r.state {
			case rowDone:
				done++
			case rowFailed:
				failed++
			default:
				skipped++
			}
		}
		content = append(content, "",
			fmt.Sprintf("%d succeeded · %d failed · %d skipped", done, failed, skipped))
	}
	if lines := diagLines(m.diags, 20); lines != nil {
		content = append(content, "")
		content = append(content, lines...)
	}
	content = append(content, "", "Enter / q / Ctrl+C dismiss · summary stays visible until then.")

	var b strings.Builder
	b.WriteString(bannerBlock(contentWidth(content), ScanTagline))
	for _, line := range content {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// selectFrame is the rendered picker body (banner, blurb, rows, footer),
// or the empty string when there is nothing to show. Byte layout is the
// original, unchanged (issue #62 keeps the picker's keys, layout and art).
func (m model) selectFrame() string {
	if len(m.items) == 0 {
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

	b.WriteString(bannerBlock(maxWidth, tagline))
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
