package banner

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderDefaultIsColored(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var b bytes.Buffer
	if err := Render(&b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.Contains(got, "\x1b[38;2;") {
		t.Error("default Render should include SGR 24-bit color escapes")
	}
	// Cols×Rows characters of art, joined by Rows-1 newlines.
	if strings.Count(got, "\n") != Rows-1 {
		t.Errorf("expected %d newlines, got %d", Rows-1, strings.Count(got, "\n"))
	}
	// Length matches the baked Art exactly.
	if got != Art {
		t.Error("default Render should return Art verbatim")
	}
}

func TestRenderNoColorStrips(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var b bytes.Buffer
	if err := Render(&b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if strings.Contains(got, "\x1b[") {
		t.Error("NO_COLOR should strip ANSI escapes")
	}
	// Plain text must still draw the letter glyphs.
	if !strings.Contains(got, "▓") {
		t.Error("plain banner should still contain letter cells")
	}
}

func TestPlainStripsRegardlessOfEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	got := Plain()
	if strings.Contains(got, "\x1b[") {
		t.Error("Plain() should strip ANSI escapes regardless of NO_COLOR")
	}
	if !strings.Contains(got, "▓") {
		t.Error("Plain() should still contain letter cells")
	}
}

func TestArtMatchesDims(t *testing.T) {
	lines := splitLines()
	if len(lines) != Rows {
		t.Fatalf("expected %d rows, got %d", Rows, len(lines))
	}
	for i, line := range lines {
		// strip ANSI for an honest width measurement; count runes because
		// the letter cells are 3-byte UTF-8 (▓, ║, ╔, ╗, ╚, ╝, ═).
		plain := ansiEscape.ReplaceAllString(line, "")
		if w := utf8.RuneCountInString(plain); w != Cols {
			t.Errorf("row %d: expected %d runes, got %d", i, Cols, w)
		}
	}
}

func TestArtContainsFadeEndpoints(t *testing.T) {
	// The fade must include both endpoints: orange (#e67a4c) on the left,
	// blue (#4282d7) on the right.
	if !strings.Contains(Art, "230;122;76") {
		t.Error("banner missing orange endpoint #e67a4c = 230;122;76")
	}
	if !strings.Contains(Art, "66;130;215") {
		t.Error("banner missing blue endpoint #4282d7 = 66;130;215")
	}
}

func TestRenderToNonWriterFails(t *testing.T) {
	// Sanity: Render must not panic on a closed writer; it should
	// surface the error.
	w := failingWriter{}
	if err := Render(&w); err == nil {
		t.Error("Render should propagate writer error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errWriter
}

var errWriter = stringError("writer failed")

type stringError string

func (e stringError) Error() string { return string(e) }
