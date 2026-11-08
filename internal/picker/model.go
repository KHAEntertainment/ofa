package picker

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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

	// Rendering state (issue #62 review B7/B8): terminal size from
	// WindowSizeMsg (0 = unknown → full, unwindowed frames), body scroll,
	// the all-diagnostics log view, and expanded row detail.
	width, height int
	scroll        int  // body scroll offset of the current screen
	logView       bool // full diagnostics overlay (key l)
	logScroll     int  // scroll offset inside the log view
	detail        bool // expanded, untruncated row detail (key d)
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
	case tea.WindowSizeMsg:
		// Real terminal size (and resizes). Not a queue message: the
		// queue waiter stays untouched. Offsets normalize at the new
		// geometry — no stale scroll position survives a resize.
		m.width, m.height = msg.Width, msg.Height
		m.scroll, m.logScroll = 0, 0
		return m, nil
	}
	return m, nil
}

// dismiss is every model-initiated exit: it marks a normal dismissal on
// the failure recorder (ordering terminal failures against it) and then
// quits. Wrapper mode has no recorder and is unaffected.
func (m model) dismiss() (tea.Model, tea.Cmd) {
	m.f.requestDismissal()
	m.quitting = true
	return m, tea.Quit
}

// handleKey processes a single keystroke. The dispatch table is small
// enough that one switch is clearer than a key map; if it grows past ~12
// keys, split it.
//
// Keys introduced for the log view and row detail never change the
// existing select/apply/quit semantics: q and ctrl+c always keep their
// phase meaning (quit / stop-after-current / dismiss), even while an
// overlay is open.
func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// The log view owns scrolling while open; l or esc closes it.
	if m.logView {
		switch k {
		case "l", "esc":
			m.logView = false
			return m, nil
		case "up":
			m.logScroll++
			return m, nil
		case "down":
			if m.logScroll > 0 {
				m.logScroll--
			}
			return m, nil
		case "pgup":
			m.logScroll += 10
			return m, nil
		case "pgdown":
			m.logScroll -= 10
			if m.logScroll < 0 {
				m.logScroll = 0
			}
			return m, nil
		case "ctrl+c", "q":
			return m.handleQuitKey()
		}
		return m, nil
	}

	if m.f != nil {
		switch m.phase {
		case phaseScan:
			switch k {
			case "ctrl+c", "q":
				return m.handleQuitKey()
			case "l":
				m.logView = true
				return m, nil
			case "up", "down", "pgup", "pgdown":
				m = m.scrollBody(k)
				return m, nil
			}
			return m, nil
		case phaseApply:
			switch k {
			case "ctrl+c", "q":
				return m.handleQuitKey()
			case "l":
				m.logView = true
				return m, nil
			case "d":
				m.detail = !m.detail
				m.scroll = 0
				return m, nil
			case "up", "down", "pgup", "pgdown":
				m = m.scrollBody(k)
				return m, nil
			}
			return m, nil
		case phaseSummary:
			switch k {
			case "ctrl+c", "q", "enter":
				return m.dismiss()
			case "l":
				m.logView = true
				return m, nil
			case "d":
				if m.summaryKind == kindResults {
					m.detail = !m.detail
					m.scroll = 0
				}
				return m, nil
			case "up", "down", "pgup", "pgdown":
				m = m.scrollBody(k)
				return m, nil
			}
			return m, nil
		}
	}
	// phaseSelect, and Run's wrapper mode: the original dispatch table,
	// unchanged (plus l for the flow's diagnostics — a no-op in wrapper
	// mode, which has none).
	switch k {
	case "ctrl+c", "q":
		return m.handleQuitKey()
	case "l":
		if m.f != nil {
			m.logView = true
		}
		return m, nil
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
		m.outcome = Outcome{Quit: true}
		return m.dismiss()
	case m.phase == phaseScan:
		m.scanStopReq = true
		m.f.stopScan() // idempotent: RunFlow's teardown may stop too
		return m, nil
	case m.phase == phaseApply:
		m.applyStopReq = true
		m.f.stopApply() // idempotent: the failure recorder may stop too
		return m, nil
	default: // phaseSummary
		return m.dismiss()
	}
}

// handleScanResult reacts to the scan worker's final answer. A requested
// stop wins over the result and quits clean (exit 0); a failed scan lands
// on a dismissible error screen carrying the caller's exit code; an empty
// scan lands on a dismissible "no updates" screen; otherwise the select
// screen opens with the same items Run would have built.
func (m model) handleScanResult(res ScanResult) (tea.Model, tea.Cmd) {
	if m.scanStopReq {
		m.exitCode = 0
		return m.dismiss()
	}
	if res.Code != 0 {
		m.phase = phaseSummary
		m.summaryKind = kindError
		m.exitMsg = "Scan failed."
		m.exitCode = res.Code
		return m, m.queueWait()
	}
	// Pins loaded by the scan are authoritative (an empty map means
	// "nothing pinned", never "fall back"); only a scan that carried no
	// snapshot at all leaves FlowConfig.Pins in force.
	pins := res.Pins
	if pins == nil {
		pins = m.f.cfg.Pins
	}
	items := buildItems(res.Resolutions, pins)
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

// startApply builds the ordered row plan — one stable row per selected
// update, pre-skipped ones included — and launches the update worker.
// An empty selection lands on the dismissible "Nothing selected." screen
// with the old exit code (0). A selection that is entirely pre-skipped
// renders the full skipped summary (rows, reasons, counts), never an
// empty placeholder.
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
	plan := m.f.cfg.Build(selected)
	m.rows = make([]applyRow, 0, len(plan.Rows))
	for _, row := range plan.Rows {
		st := rowQueued
		if row.RequestIndex < 0 {
			st = rowSkipped
		}
		m.rows = append(m.rows, applyRow{
			rowID:  row.RowID,
			label:  row.Label,
			state:  st,
			reason: row.Reason,
			reqIdx: row.RequestIndex,
		})
	}
	if len(plan.Requests) == 0 {
		m.phase = phaseSummary
		m.summaryKind = kindResults
		m.exitCode = 0
		return m, nil
	}
	m.phase = phaseApply
	m.f.startApply(plan.Requests)
	return m, nil
}

// scrollBody moves the body viewport by one line or one page. Log-style
// bodies (scan, diagnostics) are tail-relative — 0 follows the newest
// line and up walks back through history — while list-style bodies
// (rows) scroll down from the top.
func (m model) scrollBody(key string) model {
	tail := m.logView || m.phase == phaseScan
	step := 1
	if key == "pgup" || key == "pgdown" {
		step = 10
	}
	switch key {
	case "up", "pgup":
		if tail {
			m.scroll += step
		} else if m.scroll > 0 {
			m.scroll -= step
			if m.scroll < 0 {
				m.scroll = 0
			}
		}
	case "down", "pgdown":
		if tail {
			m.scroll -= step
			if m.scroll < 0 {
				m.scroll = 0
			}
		} else {
			m.scroll += step
		}
	}
	return m
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
	if m.logView {
		return m.logFrame()
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
	// NO_COLOR (non-empty, per no-color.org) strips the colors exactly as
	// banner.Render does on the plain paths — the same art, plain — so
	// the convention holds on every screen.
	//
	// Center it above the content: each row gets (width-bannerWidth)/2
	// plain leading spaces and the remainder as trailing spaces, so every
	// banner row is exactly width wide and the logomark sits over the
	// middle of the content. Plain spaces carry no SGR code, so the
	// terminal's background color shows through and the colored cells
	// stay intact. A trailing blank line separates the banner from the
	// first row.
	const bannerWidth = banner.Cols
	art := banner.Art
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		art = banner.Plain()
	}
	extra := width - bannerWidth
	var leftPad, rightPad string
	if extra > 0 {
		leftPad = strings.Repeat(" ", (extra+1)/2) // bias extra to the left
		rightPad = strings.Repeat(" ", extra-len(leftPad))
	}
	for _, line := range strings.Split(art, "\n") {
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

// truncateLine shortens one line to width cells, ANSI- and
// wide-grapheme-aware. Width <= 0 leaves the line untouched.
func truncateLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	return ansi.Truncate(line, width, "…")
}

// windowLines returns the visible slice of body for the given scroll
// offset, and the offset clamped to the last page. visible <= 0 means
// unknown height: the whole body. In tail mode the offset counts hidden
// lines above the newest page (0 = follow the tail).
func windowLines(body []string, scroll, visible int, tail bool) ([]string, int) {
	if visible <= 0 || len(body) <= visible {
		return body, 0
	}
	maxScroll := len(body) - visible
	if tail {
		scroll = maxScroll - scroll
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	return body[scroll : scroll+visible], scroll
}

// headerFor picks the header by actual fit, never by a guessed height
// threshold (issue #43, review N3): the full banner art whenever the
// real frame budget — header rows plus this frame's pinned status and
// count lines, the footer, and at least one body row — can hold it, so a
// standard 80x24 screen keeps its art; the compact tagline only when the
// art genuinely cannot fit.
func (m model) headerFor(width, pinned int) []string {
	full := strings.Split(strings.TrimSuffix(bannerBlock(width, ScanTagline), "\n"), "\n")
	compact := []string{truncateLine(centerTagline(ScanTagline, width), width), ""}
	fits := func(header []string) bool {
		if m.height <= 0 {
			return true // unknown size: render everything
		}
		// header + pinned status lines + footer + at least one body row
		return len(header)+pinned+1+1 <= m.height
	}
	switch {
	case (width <= 0 || width >= banner.Cols) && fits(full):
		return full
	case fits(compact):
		return compact
	default:
		return nil
	}
}

// layout assembles a non-select screen: the banner header (compacted at
// tiny heights), pinned status lines, a scrollable viewport over body
// lines, and a footer that always stays visible. Width or height <= 0
// means unknown: everything renders full and unwindowed.
//
// The banner may compact before anything else: states, counts and
// navigation must stay reachable at any terminal size.
func (m model) layout(body, pinned []string, footer string, scroll int, tail bool) string {
	w := m.width
	if w <= 0 {
		probe := append(append(append([]string{}, body...), pinned...), footer)
		w = contentWidth(probe)
	}

	out := m.headerFor(w, len(pinned))
	for _, line := range pinned {
		out = append(out, truncateLine(line, w))
	}

	visible := -1
	if m.height > 0 {
		visible = m.height - len(out) - 1 // the footer line stays visible
		if visible < 1 {
			visible = 1
		}
	}
	body, _ = windowLines(body, scroll, visible, tail)
	for _, line := range body {
		out = append(out, truncateLine(line, w))
	}
	out = append(out, truncateLine(footer, w))
	return strings.Join(out, "\n") + "\n"
}

// centerTagline is the compact header's single tagline line.
func centerTagline(s string, width int) string {
	return padCenter(s, width)
}

// countsLine is the summary's counts — shown pinned so they stay
// visible at any size.
func (m model) countsLine() string {
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
	return fmt.Sprintf("%d succeeded · %d failed · %d skipped", done, failed, skipped)
}

// rowBody renders the rows — expanded to full, wrapped detail when
// requested — with failure tails under the row in summary mode.
func (m model) rowBody(anim int, withTails bool) []string {
	var body []string
	for _, r := range m.rows {
		if m.detail {
			body = append(body, detailLines(r, anim, m.width)...)
			body = append(body, "")
			continue
		}
		body = append(body, rowLine(r, anim, m.width))
		if withTails && r.tail != "" {
			for _, line := range strings.Split(r.tail, "\n") {
				if strings.TrimSpace(line) != "" {
					body = append(body, wrapToWidth("      "+line, m.width)...)
				}
			}
		}
	}
	return body
}

// scanFrame renders the scanning screen (issue #43): the banner, the
// exact caption and the short tagline — on every run, first run
// included — and every diagnostic in a scrollable viewport. Stopping is
// shown as pinned status; the screen never claims a synchronous detector
// was canceled while it still runs.
func (m model) scanFrame() string {
	pinned := []string{
		fmt.Sprintf("%s %s", spinnerFrame(m.anim), ScanCaption),
		ScanDetail,
		"",
	}
	if m.scanStopReq {
		pinned = append(pinned, "Stopping — waiting for the scan to finish…", "")
	}
	var body []string
	for _, line := range m.diags {
		body = append(body, wrapToWidth("  "+line, m.width)...)
	}
	return m.layout(body, pinned, "q/ctrl+c stop scanning · ↑/↓/pgup/pgdn scroll · l logs", m.scroll, true)
}

// applyFrame renders the progress screen: one stable row per selected
// update with its small bar and state. Unknown installer progress is
// indeterminate — the running bar slides and never shows a number — and
// a row keeps its final state once its update ends. State words and bars
// stay visible at any width (B7); full row detail is the d view.
func (m model) applyFrame() string {
	pinned := []string{"Applying selected updates", ""}
	if m.applyStopReq {
		pinned = append(pinned,
			"Stopping after current update…",
			"Current update will finish. Remaining updates are skipped as canceled.",
			"",
		)
	}
	return m.layout(m.rowBody(m.anim, false), pinned,
		"q/ctrl+c stop after current update · ↑/↓/pgup/pgdn scroll · d details · l logs", m.scroll, false)
}

// summaryFrame renders the final screen: per-row results with failure
// tails and skip reasons, the counts pinned at the bottom of the header
// so they survive any height. It stays visible until the user dismisses
// it — nothing here auto-exits.
func (m model) summaryFrame() string {
	switch m.summaryKind {
	case kindError, kindEmpty:
		var body []string
		for _, line := range m.diags {
			body = append(body, wrapToWidth("  "+line, m.width)...)
		}
		return m.layout(body, []string{m.exitMsg, ""},
			"q/enter/ctrl+c dismiss · l logs", m.scroll, true)
	default:
		pinned := []string{"Updates finished", m.countsLine(), ""}
		return m.layout(m.rowBody(0, true), pinned,
			"q/enter/ctrl+c dismiss · ↑/↓/pgup/pgdn scroll · d details · l logs", m.scroll, false)
	}
}

// logFrame renders the full diagnostics log (B8): every line, wrapped
// and paged — never elided — reachable from every phase with l.
func (m model) logFrame() string {
	var body []string
	for _, line := range m.diags {
		body = append(body, wrapToWidth(line, m.width)...)
	}
	if len(body) == 0 {
		body = []string{"(no diagnostics)"}
	}
	pinned := []string{fmt.Sprintf("Diagnostics — %d line(s), all shown", len(m.diags)), ""}
	return m.layout(body, pinned, "↑/↓/pgup/pgdn scroll · l/esc close", m.logScroll, true)
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
	if m.f != nil {
		// Flow mode only (review B8): fast-scan warnings must be
		// discoverable from the select screen. Run's wrapper layout is
		// untouched below.
		if n := len(m.diags); n > 0 {
			fmt.Fprintf(&b, "\n  ! %d diagnostic line(s) — press l to view\n", n)
		} else {
			b.WriteString("\n")
		}
		b.WriteString("↑/↓ navigate · space toggle · a all · enter apply · q quit · l logs\n")
		return b.String()
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
