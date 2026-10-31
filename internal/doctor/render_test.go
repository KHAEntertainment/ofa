package doctor

import (
	"strings"
	"testing"

	"ofa/internal/catalog"
)

// --- terminal-safe rendering (B2) ---

// Every value below would, emitted raw, conceal review lines (ESC[8m
// conceal, ESC[2K erase-line), forge them (CR/LF rewrites, a bidi
// override in the name), or break the one-line-per-command promise. The
// rendered review must contain none of it raw, must show every value in
// full as visible escapes, and must keep exactly one labelled line per
// command.
func TestReviewEscapesTerminalControls(t *testing.T) {
	h := catalog.Harness{
		ID:       "x\u202eharness",
		Name:     "Evil\x1b[2K\rForged Name\x1b[0m\nSecond Line",
		Homepage: "https://example.com/\x1b[8mhidden\x1b[0m",
		Installs: []catalog.Install{{
			Method:  "native",
			Package: "pkg\x1b[8mconcealed\x1b[0m",
			Bin:     "x",
			Paths:   []string{"/usr/local/bin/x\r\noverwrite"},
			Version: catalog.CmdSpec{Cmd: []string{"x", "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"x", "update", "--flag\x1b[8mnope\x1b[0m", "line\nbreak"}},
		}},
	}
	ls := Review{H: h}.Lines()
	if len(ls) == 0 {
		t.Fatal("no review lines")
	}
	for i, line := range ls {
		// Assert on bytes: no control character may survive raw.
		for j := 0; j < len(line); j++ {
			if b := line[j]; b < 0x20 || b == 0x7f {
				t.Errorf("line %d carries raw control byte %#02x: %q", i, b, line)
			}
		}
		// No bidi or other directional-formatting rune may survive raw.
		for _, r := range line {
			switch r {
			case '\u200b', '\u200e', '\u200f', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
				'\u2066', '\u2067', '\u2068', '\u2069', '\ufeff':
				t.Errorf("line %d carries raw bidi/format rune %q", i, r)
			}
		}
	}
	joined := strings.Join(ls, "\n")
	for _, want := range []string{
		`\x1b[8m`, // the conceal sequence, visibly
		`\x1b[2K`, // the erase-line sequence
		`\r`,      // the carriage return
		`\u202e`,  // the bidi override in the id
		`\n`,      // the embedded line break
		"Forged Name",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rendered review missing the visible escape %q:\n%s", want, joined)
		}
	}
	// One labelled line per command, and the update line carries every
	// argument in full.
	updateLines := 0
	for _, l := range ls {
		if strings.HasPrefix(l, "      update command: ") {
			updateLines++
			for _, want := range []string{"'--flag\\x1b[8mnope\\x1b[0m'", `'line\nbreak'`} {
				if !strings.Contains(l, want) {
					t.Errorf("update line missing escaped argument %s: %s", want, l)
				}
			}
		}
	}
	if updateLines != 1 {
		t.Errorf("update command lines = %d, want exactly 1", updateLines)
	}
}

// Visible keeps a clean value untouched and makes a dirty one fully
// visible without losing anything.
func TestVisible(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"clean value", "clean value"},
		{"", "''"},
		{"unicode letters é ü", "unicode letters é ü"},
		{"conceal\x1b[8m", `'conceal\x1b[8m'`},
		{"evil\u202ename", `'evil\u202ename'`},
		{"a\r\nb", `'a\r\nb'`},
		{"del\x7f", `'del\x7f'`},
	}
	for _, c := range cases {
		if got := Visible(c.in); got != c.want {
			t.Errorf("Visible(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- the graphic gate (B2 defence in depth) ---

// A draft whose displayed fields carry control or bidi runes is turned
// back at the shape gate, with a reason that names the field and the
// rune so the one retry can fix it. The gate runs before anything from
// the draft executes.
func TestShapeGateRejectsNonGraphicRunes(t *testing.T) {
	base := catalog.Harness{
		ID:       "zz-probe",
		Name:     "ZZ Probe",
		Homepage: "https://example.com/zz",
		Installs: []catalog.Install{{
			Method:  "native",
			Bin:     "sh",
			Paths:   []string{"/bin/sh"},
			Version: catalog.CmdSpec{Cmd: []string{"sh", "--version"}},
			Update:  catalog.CmdSpec{Cmd: []string{"sh", "update"}},
		}},
	}
	if err := shapeGate(base, "sh", shReal(t)); err != nil {
		t.Fatalf("clean base rejected: %v", err)
	}
	mut := func(f func(*catalog.Harness)) catalog.Harness {
		h := base
		h.Installs = append([]catalog.Install(nil), base.Installs...)
		f(&h)
		return h
	}
	cases := []struct {
		name  string
		h     catalog.Harness
		field string
	}{
		{"bidi in id", mut(func(h *catalog.Harness) { h.ID = "zz\u202eprobe" }), "id"},
		{"DEL in id", mut(func(h *catalog.Harness) { h.ID = "zz\x7fprobe" }), "id"},
		{"bidi in name", mut(func(h *catalog.Harness) { h.Name = "Evil\u202eName" }), "name"},
		{"newline in name", mut(func(h *catalog.Harness) { h.Name = "Two\nLines" }), "name"},
		{"ESC in homepage", mut(func(h *catalog.Harness) { h.Homepage = "https://x/\x1b" }), "homepage"},
		{"ESC in package", mut(func(h *catalog.Harness) { h.Installs[0].Package = "p\x1b" }), "package"},
		{"CR in bin", mut(func(h *catalog.Harness) { h.Installs[0].Bin = "sh\r" }), "bin"},
		{"ZWSP in path", mut(func(h *catalog.Harness) { h.Installs[0].Paths = []string{"/bin/\u200bsh"} }), "path"},
		{"ESC in version arg", mut(func(h *catalog.Harness) { h.Installs[0].Version.Cmd = []string{"sh", "--versi\x1bon"} }), "version.cmd argument"},
		{"ESC in update arg", mut(func(h *catalog.Harness) { h.Installs[0].Update.Cmd = []string{"sh", "up\x1bdate"} }), "update.cmd argument"},
	}
	for _, c := range cases {
		err := shapeGate(c.h, "sh", shReal(t))
		if err == nil {
			t.Errorf("%s: accepted, want rejection", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), "non-printable") {
			t.Errorf("%s: error = %v, want it to name the %s field and the rune", c.name, err, c.field)
		}
	}
}

// End to end: the same rejection through ValidateProposal, before any
// runner call — the reason is what the retry prompt feeds back.
func TestValidateProposalRejectsNonGraphicNameBeforeExecution(t *testing.T) {
	draft := strings.Replace(nativeEntryFor("zztarget", "/bin/zztarget"), `"zztarget probe"`, `"Evil\u202eName"`, 1)
	gateRejectedBeforeExecution(t, draft, "zztarget", "/bin/zztarget")
}

// Invalid UTF-8 must never reach the terminal raw, on the clean-looking
// fast path too: byte 0x9b is a C1 control (CSI) in some terminals.
func TestVisibleEscapesInvalidUTF8(t *testing.T) {
	got := Visible("a\x9bb")
	if got != `'a\x9bb'` {
		t.Errorf("Visible(invalid byte) = %q, want %q", got, `'a\x9bb'`)
	}
	if _, bad := firstNonGraphic("a\xffb"); !bad {
		t.Error("firstNonGraphic missed an invalid UTF-8 byte")
	}
	if got := QuoteCmd([]string{"x", "a\x9bb"}); got != `x 'a\x9bb'` {
		t.Errorf("QuoteCmd(invalid byte) = %q", got)
	}
}

// RuneEscape and IsGraphicRune agree: every rune the latter refuses is
// rendered as an escape with no raw non-ASCII byte in it.
func TestRuneEscapeAgreesWithIsGraphicRune(t *testing.T) {
	for _, c := range []struct {
		r    rune
		want string
	}{
		{'\u00a0', `\u00a0`}, // NBSP
		{'\u2007', `\u2007`}, // figure space
		{'\u202f', `\u202f`}, // narrow NBSP
		{'\u3000', `\u3000`}, // ideographic space
		{'\u202e', `\u202e`}, // bidi override
		{'\ufffd', `\ufffd`}, // replacement character
		{'\x1b', `\x1b`},
		{'\n', `\n`},
	} {
		if IsGraphicRune(c.r) {
			t.Errorf("IsGraphicRune(%U) = true, want false", c.r)
		}
		if got := RuneEscape(c.r); got != c.want {
			t.Errorf("RuneEscape(%U) = %q, want %q", c.r, got, c.want)
		}
		if got := Visible("a" + string(c.r) + "b"); strings.ContainsRune(got, c.r) {
			t.Errorf("Visible leaked %U raw: %q", c.r, got)
		}
	}
}

// A literal backslash-n and a real newline must not render the same.
func TestVisibleBackslashIsUnambiguous(t *testing.T) {
	lit, nl := Visible(`a\nb`), Visible("a\nb")
	if lit != `'a\\nb'` {
		t.Errorf("Visible(literal backslash-n) = %q, want %q", lit, `'a\\nb'`)
	}
	if nl != `'a\nb'` {
		t.Errorf("Visible(newline) = %q, want %q", nl, `'a\nb'`)
	}
	if lit == nl {
		t.Error("a literal backslash-n and a newline render identically")
	}
	if got := QuoteCmd([]string{`a\nb`, "a\nb"}); got != `'a\\nb' 'a\nb'` {
		t.Errorf("QuoteCmd = %q", got)
	}
}
