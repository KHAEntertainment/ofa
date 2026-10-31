package doctor

import (
	"fmt"
	"strings"

	"ofa/internal/catalog"
)

// The review block: the last thing a human sees before deciding whether an
// agent-drafted entry lands in their user catalog. It shows everything the
// entry declares — each install, the version command, and the update
// command VERBATIM and untruncated — because `ofa apply` will execute that
// update command later. Doctor itself never runs it.

// Review renders the human-review block for one proposed harness. Lines
// carries everything; the caller prints them as-is, one per line.
type Review struct {
	// H is the proposed, validated harness entry.
	H catalog.Harness
	// Overrides is the embedded entry whose id the proposal shadows, or
	// nil when the id is new. The user catalog wins by id either way; the
	// review just says so.
	Overrides *catalog.Harness
}

// Lines renders the review block. Every untrusted value goes through
// Visible (terminal controls, bidi runes, and line breaks become visible
// escapes — nothing raw can conceal or forge a line), every command
// appears on exactly one clearly labelled line, shell-quoted so spaces
// and metacharacters are unambiguous, and never truncated.
func (r Review) Lines() []string {
	var ls []string
	ls = append(ls, fmt.Sprintf("harness id: %s", Visible(r.H.ID)))
	ls = append(ls, fmt.Sprintf("name: %s", Visible(r.H.Name)))
	ls = append(ls, "installs:")
	hasUpdate := false
	for i, in := range r.H.Installs {
		ls = append(ls, fmt.Sprintf("  [%d] method: %s", i+1, Visible(in.Method)))
		if in.Package != "" {
			ls = append(ls, fmt.Sprintf("      package: %s", Visible(in.Package)))
		}
		ls = append(ls, fmt.Sprintf("      bin: %s", Visible(in.Bin)))
		for _, p := range in.Paths {
			ls = append(ls, fmt.Sprintf("      path: %s", Visible(p)))
		}
		ls = append(ls, fmt.Sprintf("      version command: %s", QuoteCmd(in.Version.Cmd)))
		if len(in.Update.Cmd) > 0 {
			hasUpdate = true
			ls = append(ls, fmt.Sprintf("      update command: %s", QuoteCmd(in.Update.Cmd)))
		} else {
			ls = append(ls, "      update command: (none)")
		}
		if !StandardUpdate(in) {
			ls = append(ls, "      non-standard update command, read it carefully")
		}
	}
	if r.Overrides != nil {
		ls = append(ls, fmt.Sprintf("overrides embedded %q — the user catalog wins by id", r.H.ID))
		ls = append(ls, fmt.Sprintf("      embedded: name %q, %d install(s)", r.Overrides.Name, len(r.Overrides.Installs)))
		ls = append(ls, fmt.Sprintf("      proposed: name %q, %d install(s)", r.H.Name, len(r.H.Installs)))
		for i, in := range r.Overrides.Installs {
			ls = append(ls, fmt.Sprintf("      embedded install %d: version %s, update %s", i+1, cmdLine(in.Version.Cmd), cmdLine(in.Update.Cmd)))
		}
		for i, in := range r.H.Installs {
			ls = append(ls, fmt.Sprintf("      proposed install %d: version %s, update %s", i+1, cmdLine(in.Version.Cmd), cmdLine(in.Update.Cmd)))
		}
	}
	if hasUpdate {
		ls = append(ls, "ofa apply will run the update command above when it updates this harness")
	} else {
		ls = append(ls, "this entry declares no update command; ofa apply reports it without running anything")
	}
	return ls
}

// cmdLine renders one command spec for the override comparison, with
// "(none)" so an empty side is explicit next to a non-empty one.
func cmdLine(cmd []string) string {
	if len(cmd) == 0 {
		return "(none)"
	}
	return QuoteCmd(cmd)
}

// QuoteCmd renders a command as one shell line, POSIX-style: each
// argument is printed bare when it contains only characters no shell
// would touch, and single-quoted otherwise (with ' escaped the usual '\”
// way). Any argument holding a control, bidi, or other non-graphic rune
// — or invalid UTF-8 — is single-quoted with every such rune as a
// visible escape, so terminal controls can never conceal or forge what
// the human reads. The arguments themselves are never altered or
// truncated — the human must see exactly what `ofa apply` would run.
func QuoteCmd(cmd []string) string {
	parts := make([]string, 0, len(cmd))
	for _, a := range cmd {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// quoteArg renders one argument. The bare set is deliberately small:
// letters, digits, and the punctuation POSIX leaves uninterpreted inside
// an unquoted word. Everything else — spaces, quotes, globs, operators,
// and above all any non-graphic rune — gets single quotes with the
// shared escaping in escapeBody.
func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !bareRune(r) {
			return "'" + escapeBody(s) + "'"
		}
	}
	return s
}

// bareRune reports whether r is safe inside an unquoted shell word.
func bareRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case strings.ContainsRune("._/@:=+,-", r):
		return true
	}
	return false
}

// StandardUpdate reports whether in's update command is one of the two
// recognisable forms: a known package manager upgrading the install's own
// package, or the target binary updating itself with exactly one
// update-ish argument. Anything else is still installable — the human is
// the gate — but the review flags it so unusual commands get read, not
// skimmed.
//
// The self-update form is exact: `[bin, "update"]`-shaped and nothing
// more. An update-ish word anywhere ELSE in the vector means nothing —
// `claude -p update` is a model prompt, not a self-updater, and must not
// wear the standard label. This is a reading aid, not a security
// boundary.
func StandardUpdate(in catalog.Install) bool {
	cmd := in.Update.Cmd
	if len(cmd) == 0 {
		return true // nothing to run, nothing to flag
	}
	// The target binary updating itself: exactly two words.
	if len(cmd) == 2 && cmd[0] == in.Bin {
		switch cmd[1] {
		case "update", "upgrade", "self-update", "--update", "--upgrade":
			return true
		}
	}
	if in.Package == "" {
		return false
	}
	// The install's own package must be the ONLY non-flag argument after
	// the subcommand (npm also allows pkg@latest, and must be the global
	// form): a second package would be installed or upgraded by ofa apply
	// with no warning.
	switch cmd[0] {
	case "brew":
		// brew upgrade [--cask|--formula] <pkg> [flags...]
		return len(cmd) >= 3 && cmd[1] == "upgrade" && onlyPackage(cmd[2:], in.Package, false)
	case "npm":
		// npm install|i|update -g <pkg|pkg@latest> [flags...]
		return len(cmd) >= 4 &&
			(cmd[1] == "install" || cmd[1] == "i" || cmd[1] == "update") &&
			containsArg(cmd[2:], "-g") && onlyPackage(cmd[2:], in.Package, true)
	case "pipx":
		// pipx upgrade <pkg> [flags...]
		return len(cmd) >= 3 && cmd[1] == "upgrade" && onlyPackage(cmd[2:], in.Package, false)
	case "uv":
		// uv tool upgrade <pkg> [flags...]
		return len(cmd) >= 4 && cmd[1] == "tool" && cmd[2] == "upgrade" && onlyPackage(cmd[3:], in.Package, false)
	}
	return false
}

// onlyPackage reports whether args, after skipping flags (arguments that
// start with "-"), name pkg exactly once and nothing else. pkg@latest
// counts as pkg only when allowLatest is set (npm).
func onlyPackage(args []string, pkg string, allowLatest bool) bool {
	found := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if found || !(a == pkg || allowLatest && a == pkg+"@latest") {
			return false
		}
		found = true
	}
	return found
}

// containsArg reports whether args contains arg.
func containsArg(args []string, arg string) bool {
	for _, a := range args {
		if a == arg {
			return true
		}
	}
	return false
}
