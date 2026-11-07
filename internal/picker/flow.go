// The single Bubble Tea lifecycle for the interactive flow (issue #62):
// scan → select → apply → summary inside one alt-screen program.
//
// The CLI keeps its pipeline and update logic and injects it through
// FlowConfig callbacks; this package only renders the state those
// callbacks report. Workers talk to the model through a msgQueue, the
// model requests cancellation through stop channels, and RunFlow joins
// every worker before the TUI is torn down — nothing can write to the
// terminal after the alt screen closes.

package picker

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	tea "charm.land/bubbletea/v2"

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
	// Code is the caller's exit code for the failure case (0 = success).
	// The TUI shows a dismissible error state and the flow exits with
	// this code, so non-interactive exit-code semantics carry over.
	Code int
}

// SkippedRow is a selected update that cannot be applied (pinned,
// shadowed, missing catalog entry, …). It keeps its place on the
// progress screen as a stable row in the skipped state, so no selection
// ever silently disappears.
type SkippedRow struct {
	Label  string
	Reason string
}

// FlowConfig wires the CLI's existing pipeline and update logic into the
// one TUI lifecycle. Nothing here duplicates that logic: the flow calls
// back into it and renders only what the callbacks report.
type FlowConfig struct {
	// Pins names harness ids the user froze: those rows are still shown
	// but are dimmed and cannot be selected.
	Pins map[string]bool

	// Scan runs detect+resolve (and the first-run scan) on the flow's
	// worker goroutine while the scanning screen animates. It must stop
	// launching further pipeline stages once stop is closed; bounded
	// work already in flight may run to completion. Diagnostics go to
	// diag as complete lines and surface inside the TUI — never printed
	// to the normal buffer, never dropped. RunFlow joins this goroutine
	// before the TUI is torn down.
	Scan func(diag io.Writer, stop <-chan struct{}) ScanResult

	// Build turns the user's selection into runnable requests and
	// pre-skipped rows. Every selected update appears exactly once
	// across the two lists, so every selection owns a stable row.
	Build func(selected []resolve.Resolution) (requests []update.Request, skipped []SkippedRow)

	// Exec runs one update request. Usually update.DefaultExecutor.
	Exec update.Executor

	// Input and Output override the tea program's streams (tests).
	// Nil means tea's own defaults — the real terminal.
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
// single Update goroutine; workers only observe the stop channels.
type flowState struct {
	cfg       *FlowConfig
	q         *msgQueue
	scanStop  chan struct{}
	applyStop chan struct{}
	wg        sync.WaitGroup
}

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

// RunFlow runs the whole interactive flow in one bubbletea program:
// the harness scan (banner visible, issue #43), the picker, per-update
// progress rows, and a summary that stays until dismissed. It returns
// once the user has dismissed the flow and every worker has been joined.
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
		opts = append(opts, tea.WithInput(cfg.Input))
	}
	if cfg.Output != nil {
		opts = append(opts, tea.WithOutput(cfg.Output))
	}
	p := tea.NewProgram(m, opts...)

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

	final, err := p.Run()
	// Join before teardown completes: once this returns no worker can
	// write anywhere, so the CLI may safely restore its streams and the
	// terminal is left with the normal buffer untouched.
	f.wg.Wait()
	q.close()
	fm, ok := final.(model)
	if !ok {
		return FlowResult{}, fmt.Errorf("picker: unexpected final model type %T", final)
	}
	return FlowResult{Code: fm.exitCode}, err
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
// and bar change. reqIdx is the row's request index — the stable identity
// update.Event carries — or -1 for a pre-skipped row. Rows may share a
// label (two installs of one harness); identity never depends on it.
type applyRow struct {
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

// rowLine renders one progress row: label, optional reason, state word
// and the bar. Fixed column widths keep rows aligned and stable across
// frames; nothing here is colored, matching the picker's plain rows.
func rowLine(r applyRow, anim int) string {
	return fmt.Sprintf("  %-24s %-40s %-8s %s", r.label, r.reason, r.state.String(), bar(r.state, anim))
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
