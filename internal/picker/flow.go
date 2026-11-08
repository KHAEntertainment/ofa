// The single Bubble Tea lifecycle for the interactive flow (issue #62):
// scan → select → apply → summary inside one alt-screen program.
//
// The CLI keeps its pipeline and update logic and injects it through
// FlowConfig callbacks; this package only renders the state those
// callbacks report. Workers talk to the model through a msgQueue, the
// model requests cancellation through stop channels, and RunFlow joins
// every worker before returning — nothing can outlive the flow. The
// terminal itself is observed: the first write error, short write, read
// error or EOF records its original cause and requests the idempotent
// stop synchronously, so no queued work can start invisibly and the run
// never waits forever on a terminal that is gone.

package picker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"ofa/internal/resolve"
	"ofa/internal/update"
)

// ScanCaption is the caption on the scanning screen (issue #43). Shown
// exactly this way while the harness scan runs, first run included.
const ScanCaption = "Scanning System for Harnesses..."

// ScanTagline is the short brand line under the banner on the scanning
// and progress screens. The select screen keeps its own longer blurb.
const ScanTagline = "One-For-All. Harness Updater."

// ScanDetail is the secondary line under the scanning caption.
const ScanDetail = "Detecting installed harnesses and checking available versions."

// ScanResult is the outcome of FlowConfig.Scan.
type ScanResult struct {
	// Resolutions are the scan's findings; nil on failure.
	Resolutions []resolve.Resolution
	// Pins is the pin snapshot loaded alongside the resolutions and
	// consumed together with them (scan-result message, explicit
	// happens-before). Non-nil is authoritative — including an empty
	// map, so unpinning takes effect and stale pins can never linger.
	// Nil leaves FlowConfig.Pins in force, for callers with fixed pins.
	Pins map[string]bool
	// Code is the caller's exit code for the failure case (0 = success).
	// The TUI shows a dismissible error state and the flow exits with
	// this code, so non-interactive exit-code semantics carry over.
	Code int
}

// FlowPlan is the apply plan for a selection: the ordered rows to render
// plus the requests to execute. Rows carry one entry per selected
// update, in selection order — display order can never diverge from
// what the user picked, and duplicate labels stay distinct rows.
type FlowPlan struct {
	// Rows are in selection order. RowID is each row's stable identity:
	// its index in the Build's selected slice, never label-derived.
	// RequestIndex indexes Requests, or is -1 for a row that will not
	// run (its Reason says why).
	Rows []FlowRow
	// Requests to execute, in row order; update.Event.Index indexes
	// here. A row's RequestIndex points into this slice.
	Requests []update.Request
}

// FlowRow is one selected update's row in the plan.
type FlowRow struct {
	RowID        int    // stable identity: index in the selection
	Label        string // display label
	RequestIndex int    // index into FlowPlan.Requests, or -1 when pre-skipped
	Reason       string // skip reason when RequestIndex < 0
}

// FlowConfig wires the CLI's existing pipeline and update logic into the
// one TUI lifecycle. Nothing here duplicates that logic: the flow calls
// back into it and renders only what the callbacks report.
type FlowConfig struct {
	// Pins names harness ids the user froze: those rows are still shown
	// but are dimmed and cannot be selected. A scan that reports its own
	// Pins snapshot (the CLI always does) supersedes this fixed set.
	Pins map[string]bool

	// Scan runs detect+resolve (and the first-run scan) on the flow's
	// worker goroutine while the scanning screen animates. It must stop
	// launching further pipeline stages once stop is closed; bounded
	// work already in flight may run to completion. Diagnostics go to
	// diag as complete lines and surface inside the TUI — never printed
	// to the normal buffer, never dropped. RunFlow joins this goroutine
	// before returning.
	Scan func(diag io.Writer, stop <-chan struct{}) ScanResult

	// Build turns the user's selection into the ordered row plan and
	// runnable requests. Every selected update appears exactly once.
	Build func(selected []resolve.Resolution) FlowPlan

	// Exec runs one update request. Usually update.DefaultExecutor.
	Exec update.Executor

	// Input and Output override the tea program's streams (tests).
	// Nil means tea's own defaults — the real terminal. Both are
	// observed: the first read/write failure or EOF stops the run.
	Input  io.Reader
	Output io.Writer
}

// FlowResult is what RunFlow returns.
type FlowResult struct {
	// Code is the process exit code for the caller, with unchanged
	// semantics: 0 on success, quit and cancellation; 1 when a real
	// update failed (skipped and canceled rows do not fail); otherwise
	// the scan's own error code. Cancellation adds no new code.
	Code int
}

// Messages from workers to the model.
type (
	diagMsg       struct{ line string }
	scanResultMsg struct{ res ScanResult }
	eventMsg      struct{ ev update.Event }
	applyDoneMsg  struct{ summary update.Summary }
	animTickMsg   struct{}
	signalMsg     struct{}
)

// msgQueue is an unbounded FIFO from worker goroutines to the model's
// single waiter. Pushes never block — so a worker can never wedge
// against a model that is shutting down — and pushes after close are
// dropped. wait returns nil once the queue is closed and drained, which
// is how a waiter left blocked at teardown is released.
type msgQueue struct {
	mu     sync.Mutex
	items  []tea.Msg
	notify chan struct{}
	closed bool
}

func newMsgQueue() *msgQueue {
	return &msgQueue{notify: make(chan struct{}, 1)}
}

func (q *msgQueue) push(m tea.Msg) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, m)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *msgQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// wait blocks for the next queued message. It returns nil only when the
// queue is closed and drained.
func (q *msgQueue) wait() tea.Msg {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			m := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return m
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil
		}
		<-q.notify
	}
}

// waitCmd is the model's one and only queue waiter. Exactly one stays
// armed at a time — armed by Init and by every queue-message handler —
// so messages are delivered in order and never split across competing
// waiters.
func waitCmd(q *msgQueue) tea.Cmd {
	return func() tea.Msg { return q.wait() }
}

// diagWriter splits the CLI pipeline's writes into complete lines and
// queues each as a diagMsg. A trailing partial line is flushed when the
// scan finishes, so a missing final newline never loses a diagnostic.
type diagWriter struct {
	q       *msgQueue
	mu      sync.Mutex
	partial []byte
}

func (w *diagWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.partial[:i]), "\r")
		w.partial = w.partial[i+1:]
		if strings.TrimSpace(line) != "" {
			w.q.push(diagMsg{line: line})
		}
	}
	return len(p), nil
}

// flush emits any trailing partial line. Called by the scan worker after
// Scan returns, before the scan result is queued, so ordering holds.
func (w *diagWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if line := strings.TrimSpace(string(w.partial)); line != "" {
		w.q.push(diagMsg{line: line})
	}
	w.partial = nil
}

// flowState is the per-run state shared between RunFlow, the workers and
// the (value-copied) model. The model touches it only from bubbletea's
// single Update goroutine; workers and the failure observers only stop
// it. Every stop is idempotent (sync.Once), so the model's per-phase
// stops, the failure recorder and RunFlow's teardown can all request
// cancellation without ever double-closing a channel.
type flowState struct {
	cfg       *FlowConfig
	q         *msgQueue
	scanStop  chan struct{}
	applyStop chan struct{}
	scanOnce  sync.Once
	applyOnce sync.Once
	wg        sync.WaitGroup
}

// stopScan requests that no further scan stage launches.
func (f *flowState) stopScan() { f.scanOnce.Do(func() { close(f.scanStop) }) }

// stopApply requests stop-after-current: ApplyObserved consumes it
// between requests; the in-flight installer always finishes.
func (f *flowState) stopApply() { f.applyOnce.Do(func() { close(f.applyStop) }) }

// stopAll requests both stops. Idempotent and safe from any goroutine.
func (f *flowState) stopAll() { f.stopScan(); f.stopApply() }

// startApply launches the update worker. Requests are silenced — the TUI
// renders per-row progress itself, and the executor's own terminal
// writes would corrupt the alt screen — while installer output is still
// captured by the executor for failure tails. The worker feeds lifecycle
// events into the queue and the final summary last; the stop channel is
// update.ApplyObserved's between-request cancellation (race-safe, and
// never kills an in-flight installer).
func (f *flowState) startApply(requests []update.Request) {
	for i := range requests {
		requests[i].Silent = true
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		summary := update.ApplyObserved(requests, f.cfg.Exec, func(ev update.Event) {
			f.q.push(eventMsg{ev: ev})
		}, f.applyStop)
		f.q.push(applyDoneMsg{summary: summary})
	}()
}

// failureRecorder captures the FIRST terminal failure — a write error, a
// short write, a read error or EOF — with its original cause, and
// requests the idempotent stop synchronously from the very call that saw
// it. Stopping is never deferred to a watcher: by the time the failure
// is observable, no queued work may start any more. The watcher's only
// job is program shutdown.
type failureRecorder struct {
	stop func()
	ch   chan struct{} // closed on first failure
	once sync.Once
	mu   sync.Mutex
	err  error
}

func (r *failureRecorder) record(err error) {
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		r.stop()
		close(r.ch)
	})
}

func (r *failureRecorder) firstErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// observedWriter forwards to the wrapped writer and records the first
// error or short write. The original error is always returned to the
// caller — never swallowed, never replaced.
type observedWriter struct {
	w   io.Writer
	rec *failureRecorder
}

func (o *observedWriter) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	switch {
	case err != nil:
		o.rec.record(err)
	case n < len(p):
		o.rec.record(io.ErrShortWrite)
	}
	return n, err
}

// observedReader forwards to the wrapped reader and records the first
// read error. EOF is terminal loss here, not a normal end: after EOF the
// user cannot dismiss anything, so it is treated exactly like an
// abnormal terminal failure — stop, finish in-flight work, return the
// original cause — instead of waiting forever. The original error is
// always returned to the caller.
type observedReader struct {
	r   io.Reader
	rec *failureRecorder
}

func (o *observedReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	if err != nil {
		o.rec.record(err)
	}
	return n, err
}

// RunFlow runs the whole interactive flow in one bubbletea program:
// the harness scan (banner visible, issue #43), the picker, per-update
// progress rows, and a summary that stays until dismissed. It returns
// once the user has dismissed the flow and every worker has been joined,
// or once a terminal failure (input error/EOF, output error/short write,
// tea error) has stopped the work and joined it.
//
// Nothing is written to the normal terminal buffer by this function: all
// screens render inside the alt screen, which tears down empty.
func RunFlow(cfg FlowConfig) (FlowResult, error) {
	q := newMsgQueue()
	f := &flowState{
		cfg:       &cfg,
		q:         q,
		scanStop:  make(chan struct{}),
		applyStop: make(chan struct{}),
	}
	rec := &failureRecorder{ch: make(chan struct{}), stop: f.stopAll}
	m := newFlowModel(f)

	// WithoutSignalHandler: bubbletea's own handler turns SIGINT into
	// InterruptMsg, whose event-loop case returns ErrInterrupted before
	// the model ever sees it — an auto-quit that would tear the TUI down
	// while a worker can still write. Signals are routed to the model
	// instead and become graceful stop requests that wait for the join.
	//
	// WithWindowSize is the no-TTY fallback size: a real terminal
	// reports its own size and overrides it (Run measures the output
	// TTY), while tests driving the flow over pipes and buffers need a
	// non-zero frame to render into — the same seam bubbletea's own
	// tests use.
	opts := []tea.ProgramOption{tea.WithoutSignalHandler(), tea.WithWindowSize(120, 40)}
	if cfg.Input != nil {
		opts = append(opts, tea.WithInput(&observedReader{r: cfg.Input, rec: rec}))
	}
	if cfg.Output != nil {
		opts = append(opts, tea.WithOutput(&observedWriter{w: cfg.Output, rec: rec}))
	}
	p := tea.NewProgram(m, opts...)

	// The failure watcher's only job is program shutdown — never
	// stopping work (the recorder already did, synchronously). It is
	// stopped and joined on every exit, including tea startup failure.
	watcherStop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-rec.ch:
			p.Quit()
		case <-watcherStop:
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	stopSig := make(chan struct{})
	defer close(stopSig)
	defer signal.Stop(sigs)
	go func() {
		for {
			select {
			case <-stopSig:
				return
			case <-sigs:
				p.Send(signalMsg{})
			}
		}
	}()

	dw := &diagWriter{q: q}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		res := cfg.Scan(dw, f.scanStop)
		dw.flush()
		q.push(scanResultMsg{res: res})
	}()

	final, pErr := p.Run()

	// Whatever ended the program — user dismissal, a terminal failure,
	// or a tea error — stop before joining: no queued work may start
	// now, in-flight bounded work finishes, and every worker is joined
	// before this function returns. (Tea restores the terminal inside
	// Run; no claim is made about that ordering — the guarantee here is
	// join-before-return and no work after termination.)
	f.stopAll()
	f.wg.Wait()
	q.close()
	close(watcherStop)
	<-watcherDone

	fm, ok := final.(model)
	if !ok {
		return FlowResult{}, errors.Join(pErr, fmt.Errorf("picker: unexpected final model type %T", final))
	}

	// Honor the original failure. If the model chose to quit, any
	// recorded error is shutdown noise and the exit is the model's;
	// otherwise a recorded read/write/EOF error is the root cause and is
	// returned verbatim, with any tea error kept as context — never
	// swallowed either way.
	runErr := pErr
	if root := rec.firstErr(); root != nil && !fm.quitting {
		switch {
		case pErr == nil:
			runErr = root
		case errors.Is(pErr, root):
			runErr = root
		default:
			runErr = fmt.Errorf("%w (program: %v)", root, pErr)
		}
	}
	return FlowResult{Code: fm.exitCode}, runErr
}

// rowState is one row's lifecycle on the progress screen.
type rowState int

const (
	rowQueued rowState = iota
	rowRunning
	rowDone
	rowFailed
	rowSkipped
)

// String is the state word shown beside the row.
func (s rowState) String() string {
	switch s {
	case rowRunning:
		return "running"
	case rowDone:
		return "done"
	case rowFailed:
		return "failed"
	case rowSkipped:
		return "skipped"
	default:
		return "queued"
	}
}

// applyRow is one selected update's stable row. A row exists from the
// moment the user applies until the summary is dismissed; only its state
// and bar change. rowID is the row's stable identity (its selection
// index); reqIdx is the request index events carry, or -1 for a
// pre-skipped row. Rows may share a label (two installs of one harness);
// identity never depends on it.
type applyRow struct {
	rowID  int
	label  string
	state  rowState
	reason string // skip reason or failure note
	tail   string // failure output tail
	reqIdx int
}

// barWidth is the small bar's width in cells.
const barWidth = 10

// bar renders the row's small progress bar. No installer reports
// trustworthy progress, so the running bar is a moving indeterminate
// slider — never a number or a percentage — and a finished bar shows
// only the outcome: full for done, an x-fill for failed, empty for
// queued and skipped. The state word beside the bar is authoritative.
func bar(st rowState, anim int) string {
	cells := make([]byte, barWidth)
	fill := byte('-')
	switch st {
	case rowDone:
		fill = '#'
	case rowFailed:
		fill = 'x'
	}
	for i := range cells {
		cells[i] = fill
	}
	if st == rowRunning {
		// Indeterminate slider: three cells sweep across the empty bar.
		for i := range cells {
			cells[i] = '-'
		}
		pos := anim % barWidth
		for i := 0; i < 3; i++ {
			cells[(pos+i)%barWidth] = '='
		}
	}
	return "[" + string(cells) + "]"
}

// rowSuffix is the always-visible tail of a row line: the state word and
// the bar. Width-aware truncation may shorten the label and reason but
// never these — the row's state must stay readable at any width.
func rowSuffix(r applyRow, anim int) string {
	return fmt.Sprintf("%-8s %s", r.state.String(), bar(r.state, anim))
}

// rowLine renders one progress row: label, optional reason, state word
// and the bar. With width <= 0 (unknown size) it renders the full
// legacy line. With a known width it keeps the state and bar visible and
// truncates the label/reason to fit, measuring with ANSI cell width so
// wide graphemes and style escapes count correctly.
func rowLine(r applyRow, anim int, width int) string {
	suffix := rowSuffix(r, anim)
	if width <= 0 {
		return fmt.Sprintf("  %-24s %-40s %s", r.label, r.reason, suffix)
	}
	budget := width - 2 - ansi.StringWidth(suffix) - 1
	if budget < 1 {
		return ansi.Truncate(suffix, width, "…")
	}
	text := r.label
	if r.reason != "" {
		text += "  " + r.reason
	}
	text = ansi.Truncate(text, budget, "…")
	pad := budget - ansi.StringWidth(text)
	if pad < 0 {
		pad = 0
	}
	return "  " + text + strings.Repeat(" ", pad) + " " + suffix
}

// detailLines expands one row to its full, untruncated detail: label,
// state, reason and failure tail, wrapped to the terminal width so no
// content is lost — only moved to more lines (the viewport pages it).
func detailLines(r applyRow, anim int, width int) []string {
	lines := []string{
		r.label,
		"  " + rowSuffix(r, anim),
	}
	if r.reason != "" {
		lines = append(lines, "  reason: "+r.reason)
	}
	if r.tail != "" {
		lines = append(lines, "  output tail:")
		for _, line := range strings.Split(r.tail, "\n") {
			lines = append(lines, "    "+line)
		}
	}
	if width <= 0 {
		return lines
	}
	var out []string
	for _, line := range lines {
		out = append(out, wrapToWidth(line, width)...)
	}
	return out
}

// wrapToWidth hard-wraps one logical line to width cells, ANSI-aware,
// so a detail or diagnostic line is fully readable via paging instead of
// being truncated away.
func wrapToWidth(line string, width int) []string {
	if width <= 0 || ansi.StringWidth(line) <= width {
		return []string{line}
	}
	wrapped := ansi.Hardwrap(line, width, true)
	return strings.Split(wrapped, "\n")
}

// spinnerFrames is the braille cycle the scanning screen animates with —
// the same visual language as the old stderr spinner it replaces.
const spinnerFrames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

// spinnerRunes is spinnerFrames indexed by rune: the braille glyphs are
// multi-byte UTF-8, so byte indexing would render mojibake.
var spinnerRunes = []rune(spinnerFrames)

// spinnerFrame returns the spinner glyph for animation frame anim.
func spinnerFrame(anim int) string {
	if anim < 0 {
		anim = 0
	}
	return string(spinnerRunes[anim%len(spinnerRunes)])
}

// summaryKind selects what the summary screen leads with.
type summaryKind int

const (
	kindResults summaryKind = iota
	kindEmpty
	kindError
)
