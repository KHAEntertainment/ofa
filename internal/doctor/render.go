package doctor

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Terminal-safe rendering for everything untrusted a human reads. Shell
// quoting makes a command's ARGUMENT BOUNDARIES unambiguous, but it says
// nothing about what the bytes between the quotes DO to a terminal:
// raw ESC sequences can conceal or erase review lines, CR/LF can break
// the one-command-per-line promise, and bidi overrides can reorder what
// the eye reads. Every value an agent or a saved file contributed — ids,
// names, paths, command arguments, session fields, error text that
// echoes them — is rendered through this file's helpers so none of that
// survives as raw output, while the full value stays visible.

// IsGraphicRune reports whether r may be emitted raw on a review line:
// a printable graphic character (letter, mark, number, punctuation,
// symbol) or the ASCII space. Everything else — control characters, DEL,
// bidi and other format runes, line and paragraph separators, the
// lookalike Unicode spaces, private-use and unassigned code points — is
// rendered as a visible escape instead.
func IsGraphicRune(r rune) bool {
	if r == ' ' {
		return true
	}
	return unicode.IsOneOf([]*unicode.RangeTable{
		unicode.L, unicode.M, unicode.N, unicode.P, unicode.S,
	}, r)
}

// firstNonGraphic returns the first rune of s that must not be emitted
// raw, and whether such a rune exists.
func firstNonGraphic(s string) (rune, bool) {
	for _, r := range s {
		if !IsGraphicRune(r) {
			return r, true
		}
	}
	return 0, false
}

// RuneEscape renders one non-graphic rune as the short visible escape
// strconv.QuoteToGraphic would produce: \x1b, \n, \r, \u202e. The rune
// stays identifiable — and inert.
func RuneEscape(r rune) string {
	q := strconv.QuoteRuneToGraphic(r)
	return q[1 : len(q)-1]
}

// Visible renders one untrusted value for terminal display: unchanged
// when every rune is a printable graphic, otherwise single-quoted with
// each non-graphic rune (and each invalid UTF-8 byte) as a visible
// escape. The result always fits on one line and always carries the
// full value.
func Visible(s string) string {
	if s == "" {
		return "''"
	}
	if _, bad := firstNonGraphic(s); !bad {
		return s
	}
	return "'" + escapeBody(s) + "'"
}

// escapeBody renders s for the inside of a single-quoted field: graphic
// runes as-is, a literal single quote as the shell '\” splice, and
// every other rune — or invalid UTF-8 byte — as a visible escape. It is
// the one escaping path shared by value rendering (Visible) and command
// rendering (QuoteCmd), so metadata and arguments can never diverge in
// what they show.
func escapeBody(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(fmt.Sprintf(`\x%02x`, s[i]))
			i++
			continue
		}
		i += size
		switch {
		case r == '\'':
			b.WriteString(`'\''`)
		case IsGraphicRune(r):
			b.WriteRune(r)
		default:
			b.WriteString(RuneEscape(r))
		}
	}
	return b.String()
}
