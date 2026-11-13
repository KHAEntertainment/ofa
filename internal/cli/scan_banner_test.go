package cli

import (
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"ofa/internal/banner"
)

// The OFA banner on the harness scanning screen (issue #43), and the
// interactive flow's byte contract (issue #62).
//
// Issue #62 moved the scan inside the TUI: the banner, the short tagline
// and the "Scanning System for Harnesses..." caption now live in the alt
// screen's scanning frame (pinned in internal/picker), and the old
// stderr spinner before the picker is gone. What remains on the normal
// streams is the non-interactive fallback only. These tests pin the
// contract of what is left:
//
//   - the fallback's banner and short tagline are written to stderr,
//     above the plain scan output, when stderr is a terminal;
//   - they are written exactly once, before the first-run notice, so a
//     first run is branded too, and a returning user is branded exactly
//     once;
//   - the interactive path writes no banner, spinner or notice bytes to
//     the normal streams at all (pinned in cli_test.go and the picker
//     package);
//   - a non-TTY stderr, a usage error and the `ofa scan` subcommand get
//     no banner bytes at all — including a stderr that is a character
//     device but not a terminal;
//   - NO_COLOR strips the banner's colors.
//
// Nothing here reaches a real terminal, a real `ofa` install or the
// developer's own config: the app is built by newTestApp against temp
// paths.

// sgrPattern matches the SGR escape sequences — the color and style codes.
var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripSGR removes the color escapes from a captured terminal stream, so a
// test can look for the banner's plain glyphs whether or not the banner
// was drawn with colors.
func stripSGR(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// TestScanBanner_FirstRunShowsBannerBeforeScanNotice: the first run
// ("first run: scanning...") is branded too, and the banner comes before
// the notice — exactly once. This is the non-interactive fallback path
// (issue #62); the interactive path brands its first run inside the TUI
// (pinned in internal/picker).
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
	// rather than a pass — but note a skip is only visible under `go test
	// -v` (or -json); the default output does not show it. CI must run
	// this package verbosely to tell a run that checked the positive
	// case from one that did not.
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
