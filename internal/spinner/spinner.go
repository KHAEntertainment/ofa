// Package spinner renders a single-line loading indicator on a terminal.
//
// The indicator owns exactly one line: every frame redraws in place with
// carriage return + ANSI clear-line, and Stop erases the line so the caller
// can hand the screen (or the terminal's alt screen) over cleanly.
//
// Timing is injected: tests drive frames through their own channel instead of
// sleeping, so a frame boundary is a synchronization point, not a race.
package spinner

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// frames is the braille spinner sequence (the npm-style indicator).
var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// DefaultInterval is the delay between frames.
const DefaultInterval = 80 * time.Millisecond

// TickerFactory starts a ticker and returns its tick channel plus a stop
// function. Production uses DefaultTicker; tests inject their own so they can
// advance frames without sleeping.
type TickerFactory func(interval time.Duration) (<-chan time.Time, func())

// DefaultTicker is the production TickerFactory, backed by time.Ticker.
func DefaultTicker(interval time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(interval)
	return t.C, t.Stop
}

// Spinner is a running indicator. A nil *Spinner is valid and inert, so
// callers can decide once whether they want one and then treat the result
// uniformly.
type Spinner struct {
	out      io.Writer
	caption  string
	mu       sync.Mutex
	frame    int
	done     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// Start draws the first frame immediately and then advances on every tick
// until Stop. A nil out returns nil: there is nowhere to draw.
func Start(out io.Writer, caption string, newTicker TickerFactory) *Spinner {
	if out == nil || newTicker == nil {
		return nil
	}

	s := &Spinner{out: out, caption: caption, done: make(chan struct{})}
	s.render()

	ticks, stopTicker := newTicker(DefaultInterval)
	if stopTicker == nil {
		stopTicker = func() {}
	}
	s.wg.Add(1)
	go func() {
		// stopTicker runs before wg.Done, so once Stop returns the ticker
		// is guaranteed stopped (tests rely on that ordering).
		defer s.wg.Done()
		defer stopTicker()
		for {
			select {
			case <-s.done:
				return
			case <-ticks:
				s.render()
			}
		}
	}()
	return s
}

// Stop halts the ticker, waits for the drawing goroutine, and erases the
// line. Safe to call more than once and on a nil receiver, so callers can
// `defer sp.Stop()` without branching.
func (s *Spinner) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
		s.clear()
	})
}

// Writer wraps w so each write first erases the indicator line, writes w's
// bytes, and terminates them with a newline when the caller did not. The next
// tick redraws underneath the message.
//
// This is how the pipeline's own diagnostics (detection failures, stale-cache
// warnings) coexist with the spinner instead of overwriting it. A nil receiver
// returns w untouched.
func (s *Spinner) Writer(w io.Writer) io.Writer {
	if s == nil {
		return w
	}
	return &lineWriter{s: s, w: w}
}

// lineWriter is the spinner-aware passthrough returned by Writer.
type lineWriter struct {
	s *Spinner
	w io.Writer
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()

	if _, err := io.WriteString(l.s.out, "\r\033[2K"); err != nil {
		return 0, err
	}
	n, err := l.w.Write(p)
	if err != nil {
		return n, err
	}
	if len(p) > 0 && p[len(p)-1] != '\n' {
		if _, err := io.WriteString(l.w, "\n"); err != nil {
			return n, err
		}
	}
	return n, nil
}

// render draws the current frame in place and advances the sequence.
func (s *Spinner) render() {
	s.mu.Lock()
	defer s.mu.Unlock()

	f := frames[s.frame%len(frames)]
	s.frame++
	fmt.Fprintf(s.out, "\r\033[2K%s %s", f, s.caption)
}

// clear erases the indicator line, leaving the cursor at its start.
func (s *Spinner) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	io.WriteString(s.out, "\r\033[2K")
}
