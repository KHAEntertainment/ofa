package spinner

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// signalWriter is a bytes.Buffer that reports each write on a channel, so a
// test can wait for a frame to land without sleeping.
type signalWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes chan string
}

func newSignalWriter() *signalWriter {
	return &signalWriter{writes: make(chan string, 16)}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()
	w.writes <- string(p)
	return len(p), nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// idleTicker is a TickerFactory that never advances; the spinner only draws
// its first frame.
func idleTicker() (TickerFactory, chan time.Time) {
	ticks := make(chan time.Time)
	return func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }, ticks
}

func TestStart_DrawsFirstFrameImmediately(t *testing.T) {
	out := newSignalWriter()
	newTicker, _ := idleTicker()

	s := Start(out, "working", newTicker)
	defer s.Stop()

	got := <-out.writes
	if !strings.HasPrefix(got, "\r\033[2K") {
		t.Fatalf("first frame = %q, want a clear-line prefix", got)
	}
	if !strings.Contains(got, "⠋ working") {
		t.Fatalf("first frame = %q, want the first braille frame and caption", got)
	}
}

func TestStart_AdvancesFramesAndStopsTicker(t *testing.T) {
	out := newSignalWriter()
	ticks := make(chan time.Time)
	tickerStopped := false
	newTicker := func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() { tickerStopped = true }
	}

	s := Start(out, "loading", newTicker)
	<-out.writes // discard the immediate first frame

	for _, want := range []string{"⠙", "⠹", "⠸"} {
		ticks <- time.Now()
		if got := <-out.writes; !strings.Contains(got, want+" loading") {
			t.Fatalf("frame = %q, want %q", got, want)
		}
	}

	s.Stop()
	// Stop waits for the goroutine, which stops the ticker before it exits.
	if !tickerStopped {
		t.Error("Stop did not stop the ticker")
	}
}

func TestStop_ClearsLineOnceAndIsIdempotent(t *testing.T) {
	out := newSignalWriter()
	newTicker, _ := idleTicker()

	s := Start(out, "x", newTicker)
	<-out.writes // first frame

	s.Stop()
	if got := <-out.writes; got != "\r\033[2K" {
		t.Fatalf("Stop wrote %q, want a single clear-line", got)
	}

	before := out.String()
	s.Stop()
	if after := out.String(); after != before {
		t.Fatalf("second Stop appended %q", after[len(before):])
	}
}

func TestWriter_ClearsLineBeforeMessage(t *testing.T) {
	out := newSignalWriter()
	newTicker, _ := idleTicker()
	s := Start(out, "x", newTicker)
	defer s.Stop()
	<-out.writes // first frame

	var msgs bytes.Buffer
	if _, err := s.Writer(&msgs).Write([]byte("ofa: scan: boom\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := msgs.String(); got != "ofa: scan: boom\n" {
		t.Fatalf("message = %q, want it unchanged", got)
	}
	if got := <-out.writes; got != "\r\033[2K" {
		t.Fatalf("clear before message = %q, want %q", got, "\r\033[2K")
	}
}

func TestWriter_AppendsNewlineWhenMissing(t *testing.T) {
	out := newSignalWriter()
	newTicker, _ := idleTicker()
	s := Start(out, "x", newTicker)
	defer s.Stop()
	<-out.writes

	var msgs bytes.Buffer
	if _, err := s.Writer(&msgs).Write([]byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := msgs.String(); got != "partial\n" {
		t.Fatalf("message = %q, want a trailing newline", got)
	}
}

func TestNilSpinnerIsInert(t *testing.T) {
	var s *Spinner
	s.Stop() // must not panic

	var buf bytes.Buffer
	w := s.Writer(&buf)
	if w == nil {
		t.Fatal("nil Spinner.Writer returned nil writer")
	}
	if _, err := w.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); got != "hi" {
		t.Fatalf("nil spinner wrote %q, want it untouched", got)
	}

	if got := Start(nil, "x", DefaultTicker); got != nil {
		t.Fatalf("Start(nil) = %v, want nil", got)
	}
}

func TestWriter_NilWriterIsPassthrough(t *testing.T) {
	var buf bytes.Buffer
	var w io.Writer = &buf
	if got := (*Spinner)(nil).Writer(w); got != w {
		t.Fatal("nil Spinner.Writer should return the writer unchanged")
	}
}
