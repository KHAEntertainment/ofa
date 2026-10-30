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

// Lines renders the review block. Every command appears on exactly one
// clearly labelled line, shell-quoted so spaces and metacharacters are
// unambiguous, and never truncated.
func (r Review) Lines() []string {
	var ls []string
	ls = append(ls, fmt.Sprintf("harness id: %s", r.H.ID))
	ls = append(ls, fmt.Sprintf("name: %s", r.H.Name))
	ls = append(ls, "installs:")
	hasUpdate := false
	for i, in := range r.H.Installs {
		ls = append(ls, fmt.Sprintf("  [%d] method: %s", i+1, in.Method))
		if in.Package != "" {
			ls = append(ls, fmt.Sprintf("      package: %s", in.Package))
		}
		ls = append(ls, fmt.Sprintf("      bin: %s", in.Bin))
		for _, p := range in.Paths {
			ls = append(ls, fmt.Sprintf("      path: %s", p))
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
	}
	if hasUpdate {
		ls = append(ls, "ofa apply will run the update command above when it updates this harness")
	} else {
		ls = append(ls, "this entry declares no update command; ofa apply reports it without running anything")
	}
	return ls
}

// QuoteCmd renders a command as one shell line, POSIX-style: each argument
// is printed bare when it contains only characters no shell would touch,
// and single-quoted otherwise (with ' escaped the usual '\” way). The
// arguments themselves are never altered or truncated — the human must see
// exactly what `ofa apply` would run.
func QuoteCmd(cmd []string) string {
	parts := make([]string, 0, len(cmd))
	for _, a := range cmd {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// quoteArg renders one argument. The safe set is deliberately small:
// letters, digits, and the punctuation POSIX leaves uninterpreted inside
// an unquoted word. Everything else — spaces, quotes, globs, operators —
// gets single quotes.
func quoteArg(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._/@:=+,-", r):
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// updateishArgs are the arguments that make `bin <arg> ...` recognisably
// an update command for the binary itself: the self-updater pattern the
// self-tier harnesses use (`claude update`, `agy update`, and the dashed
// spellings).
var updateishArgs = map[string]bool{
	"update": true, "upgrade": true,
	"self-update": true, "selfupdate": true,
	"--update": true, "--upgrade": true, "-u": true,
}

// StandardUpdate reports whether in's update command is one of the two
// recognisable forms: a known package manager upgrading the install's own
// package, or the target binary updating itself with an update-ish
// argument. Anything else is still installable — the human is the gate —
// but the review flags it so unusual commands get read, not skimmed.
//
// The check is deliberately permissive within those two shapes (extra
// flags after the package name, pkg@latest) and deliberately closed
// outside them: it is a reading aid, not a security boundary.
func StandardUpdate(in catalog.Install) bool {
	cmd := in.Update.Cmd
	if len(cmd) == 0 {
		return true // nothing to run, nothing to flag
	}
	// The target binary updating itself.
	if cmd[0] == in.Bin && len(cmd) > 1 {
		for _, a := range cmd[1:] {
			if updateishArgs[a] {
				return true
			}
		}
	}
	if in.Package == "" {
		return false
	}
	// The package must be named (npm also allows pkg@latest), and npm
	// must be the global form.
	hasPkg := func() bool {
		for _, a := range cmd[1:] {
			if a == in.Package || a == in.Package+"@latest" {
				return true
			}
		}
		return false
	}
	switch cmd[0] {
	case "brew":
		// brew upgrade [--cask|--formula] <pkg> [flags...]
		return len(cmd) >= 3 && cmd[1] == "upgrade" && hasPkg()
	case "npm":
		// npm install|i|update -g <pkg|pkg@latest> [flags...]
		return len(cmd) >= 4 &&
			(cmd[1] == "install" || cmd[1] == "i" || cmd[1] == "update") &&
			containsArg(cmd[2:], "-g") && hasPkg()
	case "pipx":
		// pipx upgrade <pkg> [flags...]
		return len(cmd) >= 3 && cmd[1] == "upgrade" && hasPkg()
	case "uv":
		// uv tool upgrade <pkg> [flags...]
		return len(cmd) >= 4 && cmd[1] == "tool" && cmd[2] == "upgrade" && hasPkg()
	}
	return false
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
