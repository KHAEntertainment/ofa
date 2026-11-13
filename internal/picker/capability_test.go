package picker

// Capability-preservation tests for the stream observers (issue #62
// review N1): wrapping a terminal must keep every interface the original
// carried — tea asserts term.File (io.ReadWriteCloser + Fd) for raw
// mode and real sizing, and muesli/cancelreader asserts its own File
// (ReadWriteCloser + Fd + Name) to pick a genuinely cancellable reader.
// Nothing here fakes Fd or Name: the delegating wrappers expose the
// original handle's own values or nothing at all.
//
// Static conformance below is necessary but deliberately not sufficient —
// the behavioral tests use a real pollable pipe and a real PTY for
// cancelreader recognition and raw-mode/size/restore, so a regression
// fails rather than hiding.

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
)

var (
	// tea's assertions stay satisfiable in every wrapper case.
	_ term.File = (*observedInputFile)(nil)
	_ term.File = (*observedInputNoName)(nil)
	_ term.File = (*observedOutputFile)(nil)
	_ term.File = (*observedOutputNoName)(nil)
	// cancelreader's stricter File (needs Name) on the full-capability case.
	_ cancelreader.File = (*observedInputFile)(nil)
	_ cancelreader.File = (*observedOutputFile)(nil)
	// the *os.File production path really carries the full superset.
	_ fileCap = (*os.File)(nil)
)

// noNameFile is a ReadWriteCloser+Fd handle without Name — exactly
// term.File, deliberately short of cancelreader's File.
type noNameFile struct{ f *os.File }

func (n noNameFile) Read(p []byte) (int, error)  { return n.f.Read(p) }
func (n noNameFile) Write(p []byte) (int, error) { return n.f.Write(p) }
func (n noNameFile) Close() error                { return n.f.Close() }
func (n noNameFile) Fd() uintptr                 { return n.f.Fd() }

func tempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cap")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestWrapInput_PreservesFileCapabilities: the full-superset case keeps
// Fd and Name exactly the original's — never fabricated — while reads
// stay observed.
func TestWrapInput_PreservesFileCapabilities(t *testing.T) {
	tf := tempFile(t)
	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	w := wrapInput(tf, rec)

	f, ok := w.(fileCap)
	if !ok {
		t.Fatalf("wrapped input lost the File capability set: %T", w)
	}
	if f.Fd() != tf.Fd() {
		t.Errorf("Fd() = %d, want the original %d", f.Fd(), tf.Fd())
	}
	if f.Name() != tf.Name() {
		t.Errorf("Name() = %q, want the original %q", f.Name(), tf.Name())
	}
	// Reads flow through and are observed.
	if _, err := tf.WriteString("data"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tf.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, 4)
	if n, err := w.Read(buf); n != 4 || err != nil {
		t.Errorf("Read through wrapper = %d, %v", n, err)
	}
	// Close delegates to the caller's own handle — the wrapper closes
	// nothing the caller would not have.
	if err := f.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := tf.Seek(0, io.SeekStart); err == nil {
		t.Error("Close must delegate to the original handle")
	}
}

// TestWrapInput_TermFileOnlyKeepsTeaSupport: a ReadWriteCloser+Fd
// original without Name keeps raw-mode support but must never grow a
// faked Name.
func TestWrapInput_TermFileOnlyKeepsTeaSupport(t *testing.T) {
	tf := tempFile(t)
	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	w := wrapInput(noNameFile{tf}, rec)

	if _, ok := w.(term.File); !ok {
		t.Errorf("wrapped input lost term.File: %T", w)
	}
	if _, ok := w.(fileCap); ok {
		t.Errorf("wrapped input grew a Name it does not have: %T", w)
	}
	if _, ok := w.(cancelreader.File); ok {
		t.Errorf("wrapped input must not satisfy cancelreader.File without Name: %T", w)
	}
}

// TestWrapStreams_GenericStayGeneric: plain streams keep the generic
// observers — no capability is invented for them.
func TestWrapStreams_GenericStayGeneric(t *testing.T) {
	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	if _, ok := wrapInput(strings.NewReader(""), rec).(*observedReader); !ok {
		t.Errorf("generic input wrapped as %T, want *observedReader", wrapInput(strings.NewReader(""), rec))
	}
	if _, ok := wrapOutput(io.Discard, rec).(*observedWriter); !ok {
		t.Errorf("generic output wrapped as %T, want *observedWriter", wrapOutput(io.Discard, rec))
	}
}

// TestWrapInput_ClosedHandleFailsSafely: a closed original errors
// through the wrapper and is recorded — no panic, no invented state.
func TestWrapInput_ClosedHandleFailsSafely(t *testing.T) {
	tf := tempFile(t)
	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	w := wrapInput(tf, rec)
	if err := tf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := w.Read(make([]byte, 1)); err == nil {
		t.Fatal("reading a closed handle must error")
	}
	if rec.failure() == nil {
		t.Error("the closed-handle error must be recorded")
	}
}

// TestCancelReader_RecognizesWrappedPipe: a pollable file-backed input
// keeps its genuinely cancellable reader after wrapping — a regular
// file would not be suitable here (the OS cannot poll one); the pipe is
// the minimal pollable fixture.
func TestCancelReader_RecognizesWrappedPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	wrapped := wrapInput(r, rec)
	cr, err := cancelreader.NewReader(wrapped)
	if err != nil {
		t.Fatalf("cancelreader.NewReader(wrapped pipe): %v", err)
	}
	defer cr.Close()
	if !cr.Cancel() {
		t.Error("wrapped pipe lost cancelreader cancellability (fell back to the uncancelable reader)")
	}

	// The generic case keeps the fallback — and says so honestly.
	rec2 := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	cr2, err := cancelreader.NewReader(wrapInput(strings.NewReader(""), rec2))
	if err != nil {
		t.Fatalf("cancelreader.NewReader(generic): %v", err)
	}
	defer cr2.Close()
	if cr2.Cancel() {
		t.Error("a generic reader must not pretend to be cancellable")
	}
}

// TestPTY_WrappedTerminalKeepsRawModeSizeRestore: with a real PTY the
// wrapped streams still look like a terminal to tea's own assertions —
// raw mode enters/restores and real dimensions read back after a size
// change (the resize data source), all through the wrapper's delegated
// Fd. Runs wherever PTY infrastructure exists; skips only for missing
// infrastructure, with the exact reason.
func TestPTY_WrappedTerminalKeepsRawModeSizeRestore(t *testing.T) {
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	in := wrapInput(slave, rec)
	out := wrapOutput(slave, rec)

	for name, w := range map[string]interface{}{"input": in, "output": out} {
		fw, ok := w.(interface{ Fd() uintptr })
		if !ok {
			t.Fatalf("wrapped %s lost Fd: %T", name, w)
		}
		if !term.IsTerminal(fw.Fd()) {
			t.Errorf("wrapped %s is not seen as a terminal (Fd=%d)", name, fw.Fd())
		}
	}

	f, ok := in.(term.File)
	if !ok {
		t.Fatalf("wrapped terminal input lost term.File: %T", in)
	}
	state, err := term.MakeRaw(f.Fd())
	if err != nil {
		t.Fatalf("MakeRaw through the wrapper: %v", err)
	}
	if err := term.Restore(f.Fd(), state); err != nil {
		t.Fatalf("Restore through the wrapper: %v", err)
	}

	setPTYWinsize(t, master, 37, 91)
	w, h, err := term.GetSize(f.Fd())
	if err != nil {
		t.Fatalf("GetSize through the wrapper: %v", err)
	}
	if w != 91 || h != 37 {
		t.Errorf("GetSize = %dx%d, want the pty's 91x37 (real dimensions, resize source)", w, h)
	}
}

// TestPTY_CancelReaderRecognizesWrappedTerminal: the PTY-backed wrapped
// input keeps a genuinely cancellable reader.
func TestPTY_CancelReaderRecognizesWrappedTerminal(t *testing.T) {
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	rec := &failureRecorder{ch: make(chan struct{}), stop: func() {}}
	cr, err := cancelreader.NewReader(wrapInput(slave, rec))
	if err != nil {
		t.Fatalf("cancelreader.NewReader(wrapped pty): %v", err)
	}
	defer cr.Close()
	if !cr.Cancel() {
		t.Error("wrapped PTY lost cancelreader cancellability")
	}
}
