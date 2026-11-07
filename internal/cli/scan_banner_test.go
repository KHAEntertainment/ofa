package cli

import (
	"io"
	"regexp"
	"strings"
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
//     branded too;
//   - a non-TTY stderr, a usage error and the `ofa scan` subcommand get no
//     banner bytes at all;
//   - NO_COLOR strips the banner's colors, while the spinner's cursor
//     control escapes stay.
//
// Nothing here reaches a real terminal, a real `ofa` install or the
// developer's own config: the app is built by newTestApp against temp
// paths, and the spinner frames come from the injected ticker.

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

// TestScanBanner_TTYDrawsBannerOnceAboveSpinner is the core of issue #43:
// on a terminal the banner and the short tagline sit above the spinner
// line, the scanning line carries the new caption, and the banner appears
// exactly once however many frames the spinner paints.
func TestScanBanner_TTYDrawsBannerOnceAboveSpinner(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // plain art: the bytes are exactly banner.Plain()

	a, _, stderr, sd, sr, _ := newTestApp(t, nil, nil)
	a.StderrIsTTY = func(io.Writer) bool { return true }
	sd.instances = oneClaude()

	// A slow resolve buys several spinner frames: the banner must not be
	// redrawn by any of them.
	a.ResolveFn = func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution {
		time.Sleep(400 * time.Millisecond)
		return sr.res
	}
	sr.res = []resolve.Resolution{{HarnessID: "claude", Tier: "self"}}

	// The two calls pickerCmd makes, in the order it makes them.
	a.drawScanBanner()
	if _, code := a.pickerPipeline(testCatalog(t)); code != ExitOK {
		t.Fatalf("pickerPipeline exit = %d, want %d", code, ExitOK)
	}

	got := stderr.String()
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
	if frames := len(braillePattern.FindAllString(got, -1)); frames < 2 {
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
