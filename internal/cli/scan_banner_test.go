package cli

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ofa/internal/banner"
	"ofa/internal/catalog"
	"ofa/internal/detect"
	"ofa/internal/resolve"
)

// The OFA banner on the harness scanning screen (issue #43).
//
// The scanning screen is the one place the brand was missing: the picker
// draws the banner, but the wait before it only showed a spinner line.
// These tests pin the contract of the fix:
//
//   - the banner and its short tagline are written to stderr, above the
//     spinner line, when stderr is a terminal;
//   - they are written exactly once, no matter how many spinner frames
//     are painted, and the pipeline itself never redraws them;
//   - they are written before the first-run notice, so the first run is
//     branded too, and a returning user is branded exactly once;
//   - a non-TTY stderr, a usage error and the `ofa scan` subcommand get no
//     banner bytes at all — including a stderr that is a character device
//     but not a terminal;
//   - NO_COLOR strips the banner's colors, while the spinner's cursor
//     control escapes stay.
//
// Nothing here reaches a real terminal, a real `ofa` install or the
// developer's own config: the app is built by newTestApp against temp
// paths, and the spinner frames are observed rather than waited for.

// sgrPattern matches the SGR escape sequences — the color and style codes.
// Cursor control escapes such as "\x1b[2K" are deliberately not matched:
// the spinner needs them, and only the colors are at stake in the
// NO_COLOR contract.
var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// braillePattern matches the spinner's braille frames.
var braillePattern = regexp.MustCompile(`[\x{2800}-\x{28ff}]`)

// stripSGR removes the color escapes from a captured terminal stream, so a
// test can look for the banner's plain glyphs whether or not the banner
// was drawn with colors.
func stripSGR(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// frameSignal is a mutex-guarded buffer that reports when the spinner has
// written its second frame.
//
// The spinner draws from its own goroutine, so a test must never read the
// buffer while a frame may be in flight. This writer takes the lock for
// every write and for every read, and closes secondFrame once the second
// frame has landed — a synchronization point, not a sleep.
type frameSignal struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	frames      int
	secondFrame chan struct{}
	once        sync.Once
}

func newFrameSignal() *frameSignal {
	return &frameSignal{secondFrame: make(chan struct{})}
}

func (w *frameSignal) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	if braillePattern.Match(p) {
		w.frames++
		if w.frames >= 2 {
			w.once.Do(func() { close(w.secondFrame) })
		}
	}
	w.mu.Unlock()
	return len(p), nil
}

// String returns what has been written so far. Safe to read once the
// writer is done — the tests read only after Stop joined the goroutine.
func (w *frameSignal) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Frames returns how many spinner frames have been written.
func (w *frameSignal) Frames() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.frames
}

// TestScanBanner_TTYDrawsBannerOnceAboveSpinner is the core of issue #43:
// on a terminal the banner and the short tagline sit above the spinner
// line, the scanning line carries the new caption, and the banner appears
// exactly once however many frames the spinner paints.
func TestScanBanner_TTYDrawsBannerOnceAboveSpinner(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // plain art: the bytes are exactly banner.Plain()

	a, _, _, sd, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	sd.instances = oneClaude()

	// The spinner paints from its own goroutine, so the test observes its
	// frames instead of sleeping and hoping. The resolve waits for the
	// second frame to have been written, so the pipeline cannot finish
	// before the spinner has painted twice. The wait is bounded: on a
	// timeout the resolve returns and the assertions below fail with the
	// real frame count.
	sig := newFrameSignal()
	a.Stderr = sig
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		select {
		case <-sig.secondFrame:
		case <-time.After(5 * time.Second):
		}
		return sr.res
	}

	// The two calls pickerCmd makes, in the order it makes them.
	a.drawScanBanner()
	if _, code := a.pickerPipeline(testCatalog(t)); code != ExitOK {
		t.Fatalf("pickerPipeline exit = %d, want %d", code, ExitOK)
	}

	// Safe to read now: pickerPipeline's deferred Stop joined the
	// drawing goroutine before it returned.
	got := sig.String()
	if n := strings.Count(got, banner.Plain()); n != 1 {
		t.Errorf("banner art appears %d times in %q, want exactly 1", n, got)
	}
	if !strings.Contains(got, scanTagline) {
		t.Errorf("stderr = %q, want the short tagline %q", got, scanTagline)
	}
	if !strings.Contains(got, scanCaption) {
		t.Errorf("stderr = %q, want the scanning caption %q", got, scanCaption)
	}

	// The banner belongs above the spinner line.
	art := strings.Index(got, banner.Plain())
	frame := strings.Index(got, "⠋")
	if art < 0 || frame < 0 || art > frame {
		t.Errorf("stderr = %q, want the banner before the first spinner frame", got)
	}

	// The spinner really did paint more than one frame, so "drawn once"
	// was tested against a redraw opportunity.
	if frames := sig.Frames(); frames < 2 {
		t.Errorf("spinner painted %d frames, want at least 2 to prove the banner is not redrawn", frames)
	}
}

// TestScanBanner_SpinnerKeepsCursorControlEscapes: the NO_COLOR contract
// is about colors. The spinner still needs its cursor control escapes to
// redraw in place.
func TestScanBanner_SpinnerKeepsCursorControlEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, _, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}

	a.drawScanBanner()
	if _, code := a.pickerPipeline(testCatalog(t)); code != ExitOK {
		t.Fatalf("pickerPipeline exit = %d, want %d", code, ExitOK)
	}

	got := stderr.String()
	if !strings.Contains(got, "\x1b[2K") {
		t.Errorf("stderr = %q, want the spinner's cursor control escape", got)
	}
	if sgr := sgrPattern.FindAllString(got, -1); len(sgr) != 0 {
		t.Errorf("stderr carries %d SGR escapes under NO_COLOR, want none: %q", len(sgr), got)
	}
}

// TestPickerPipeline_AloneDrawsNoBanner: the banner is drawn once, before
// the scan. The pipeline must not draw a second copy of it — that is what
// would double the brand on the scanning screen.
func TestPickerPipeline_AloneDrawsNoBanner(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, _, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}

	if _, code := a.pickerPipeline(testCatalog(t)); code != ExitOK {
		t.Fatalf("pickerPipeline exit = %d, want %d", code, ExitOK)
	}

	got := stderr.String()
	if strings.Contains(stripSGR(got), banner.Plain()) {
		t.Errorf("the pipeline drew the banner: %q", got)
	}
	if strings.Contains(got, scanTagline) {
		t.Errorf("the pipeline drew the tagline: %q", got)
	}
	if !strings.Contains(got, scanCaption) {
		t.Errorf("stderr = %q, want the scanning caption %q", got, scanCaption)
	}
}

// TestScanBanner_FirstRunShowsBannerBeforeScanNotice: the first run
// ("first run: scanning...") is branded too, and the banner comes before
// the notice — exactly once.
func TestScanBanner_FirstRunShowsBannerBeforeScanNotice(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, sd, _, _ := newTestApp(t, nil, nil) // no args → the picker
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.StatePath = missingStatePath(t) // no state file → the first-run path
	sd.instances = oneClaude()

	// The run ends on the no-TTY stdin check; the banner is drawn before
	// that, so it is on screen even on a machine without a terminal.
	if code := a.Run(); code != ExitError {
		t.Fatalf("bare ofa exit = %d, want %d", code, ExitError)
	}

	got := stderr.String()
	if n := strings.Count(got, banner.Plain()); n != 1 {
		t.Errorf("banner art appears %d times in %q, want exactly 1", n, got)
	}
	if !strings.Contains(got, scanTagline) {
		t.Errorf("stderr = %q, want the short tagline %q", got, scanTagline)
	}
	art := strings.Index(got, banner.Plain())
	notice := strings.Index(got, "first run: scanning...")
	if art < 0 || notice < 0 || art > notice {
		t.Errorf("stderr = %q, want the banner before the first-run notice", got)
	}
}

// TestScanBanner_SeededStateShowsBannerOnceAndNoFirstRunNotice: a
// returning user — one whose state file already exists — is branded
// exactly once and never sees the first-run notice.
func TestScanBanner_SeededStateShowsBannerOnceAndNoFirstRunNotice(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, _, _, _ := newTestApp(t, nil, nil) // no args → the picker
	a.StderrIsTTY = func(io.Writer) bool { return true }
	// Seed the state file: the first run already happened.
	if err := os.WriteFile(a.StatePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if code := a.Run(); code != ExitError {
		t.Fatalf("bare ofa exit = %d, want %d", code, ExitError)
	}

	got := stderr.String()
	if n := strings.Count(got, banner.Plain()); n != 1 {
		t.Errorf("banner art appears %d times in %q, want exactly 1", n, got)
	}
	if !strings.Contains(got, scanTagline) {
		t.Errorf("stderr = %q, want the short tagline %q", got, scanTagline)
	}
	if strings.Contains(got, "first run: scanning...") {
		t.Errorf("a returning user must not see the first-run notice: %q", got)
	}
}

// TestScanBanner_NonTTYWritesNoBannerBytes: piped, redirected and CI runs
// stay byte-identical. Only the banner is withheld — the diagnostics the
// path already wrote are still there.
func TestScanBanner_NonTTYWritesNoBannerBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, sd, _, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return false } // non-TTY stderr
	a.StatePath = missingStatePath(t)
	sd.instances = oneClaude()

	a.Run()

	got := stderr.String()
	if strings.Contains(stripSGR(got), banner.Plain()) {
		t.Errorf("a non-TTY stderr got banner bytes: %q", got)
	}
	if strings.Contains(got, scanTagline) {
		t.Errorf("a non-TTY stderr got the tagline: %q", got)
	}
	if !strings.Contains(got, "first run: scanning...") {
		t.Errorf("stderr = %q, want the first-run notice to survive", got)
	}
	if !strings.Contains(got, "no TTY detected") {
		t.Errorf("stderr = %q, want the no-TTY message to survive", got)
	}
}

// TestScanBanner_DefaultGateSuppressesBannerOnPipe: the production path —
// the default gate, not an injected one — must keep a non-terminal stderr
// free of banner bytes. The pipe makes the bytes observable.
func TestScanBanner_DefaultGateSuppressesBannerOnPipe(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close() })

	a, _, _, sd, _, _ := newTestApp(t, nil, nil)
	a.Stderr = w // a pipe: the default gate must reject it
	a.StatePath = missingStatePath(t)
	sd.instances = oneClaude()

	if code := a.Run(); code != ExitError {
		t.Fatalf("bare ofa exit = %d, want %d", code, ExitError)
	}
	// Close the write end so the read sees EOF.
	w.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	got := string(data)
	if strings.Contains(stripSGR(got), banner.Plain()) {
		t.Errorf("a piped stderr got banner bytes: %q", got)
	}
	if strings.Contains(got, scanTagline) {
		t.Errorf("a piped stderr got the tagline: %q", got)
	}
	if !strings.Contains(got, "first run: scanning...") {
		t.Errorf("stderr = %q, want the first-run notice to survive", got)
	}
}

// TestStderrIsTTY_CharacterDeviceIsNotATerminal: /dev/null and /dev/zero
// are character devices, so a stat-only mode check would let them through
// and the banner and the spinner would be written to a device that
// discards every byte. Only a real terminal passes the gate.
func TestStderrIsTTY_CharacterDeviceIsNotATerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { devNull.Close() })
	if stderrIsTTY(devNull) {
		t.Errorf("%s must not pass the stderr terminal gate", os.DevNull)
	}

	// /dev/zero is where the device-node case shows up: a character
	// device that is not a terminal. It is not present everywhere.
	devZero, err := os.Open("/dev/zero")
	if err != nil {
		t.Logf("/dev/zero is not available here: %v", err)
	} else {
		t.Cleanup(func() { devZero.Close() })
		if stderrIsTTY(devZero) {
			t.Error("/dev/zero must not pass the stderr terminal gate")
		}
	}

	// The positive case needs a real terminal, which a machine without a
	// controlling terminal (a CI runner) does not have. It is a skip
	// rather than a pass: the default output says SKIP, so a run that
	// never checked the positive case cannot read as one that did.
	t.Run("a controlling terminal is a terminal", func(t *testing.T) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			t.Skipf("no controlling terminal here (%v)", err)
		}
		defer tty.Close()
		if !stderrIsTTY(tty) {
			t.Error("stderrIsTTY(a controlling terminal) = false, want true")
		}
	})
}

// TestScanBanner_UsageErrorWritesNoBannerBytes: the banner is drawn after
// the flags parse, so a usage error never gains banner output.
func TestScanBanner_UsageErrorWritesNoBannerBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, _, _, _ := newTestApp(t, []string{"--refresh", "--bogus"}, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }

	if code := a.Run(); code != ExitError {
		t.Fatalf("ofa --refresh --bogus exit = %d, want %d", code, ExitError)
	}

	got := stderr.String()
	if strings.Contains(stripSGR(got), banner.Plain()) {
		t.Errorf("a usage error got banner bytes: %q", got)
	}
	if strings.Contains(got, scanTagline) {
		t.Errorf("a usage error got the tagline: %q", got)
	}
}

// TestScanCmd_WritesNoBannerBytes: the `ofa scan` subcommand's output is
// unchanged — the banner belongs to the interactive path only.
func TestScanCmd_WritesNoBannerBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, out, stderr, sd, _, _ := newTestApp(t, []string{"scan"}, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	sd.instances = oneClaude()

	if code := a.Run(); code != ExitOK {
		t.Fatalf("scan exit = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
	if strings.Contains(stripSGR(stderr.String()), banner.Plain()) {
		t.Errorf("ofa scan drew the banner: %q", stderr.String())
	}
	if strings.Contains(out.String(), scanTagline) {
		t.Errorf("ofa scan output changed: %q", out.String())
	}
	if !strings.Contains(out.String(), "initial scan") {
		t.Errorf("ofa scan output = %q, want the usual scan report", out.String())
	}
}

// TestScanBanner_NO_COLORStripsColors: NO_COLOR strips the banner's
// colors, per no-color.org — the art's glyphs stay, the SGR codes go.
func TestScanBanner_NO_COLORStripsColors(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	a, _, stderr, _, _, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.drawScanBanner()

	got := stderr.String()
	if sgr := sgrPattern.FindAllString(got, -1); len(sgr) != 0 {
		t.Errorf("NO_COLOR left %d SGR escapes in the banner: %q", len(sgr), got)
	}
	if !strings.Contains(got, banner.Plain()) {
		t.Errorf("stderr = %q, want the plain art", got)
	}
}

// TestScanBanner_ColorsWithoutNO_COLOR: with NO_COLOR unset the banner
// keeps its baked brand colors.
func TestScanBanner_ColorsWithoutNO_COLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "") // present but empty: no-color.org keeps colors

	a, _, stderr, _, _, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	a.drawScanBanner()

	got := stderr.String()
	if sgr := sgrPattern.FindAllString(got, -1); len(sgr) == 0 {
		t.Errorf("the banner lost its colors without NO_COLOR: %q", got)
	}
	if !strings.Contains(stripSGR(got), banner.Plain()) {
		t.Errorf("stderr = %q, want the banner art", got)
	}
}
