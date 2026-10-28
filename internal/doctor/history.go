package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionDirFormat names a history session: UTC, sortable, and self-describing
// enough that `ofa doctor --install <ts>` (the follow-on flow) can name it
// back.
const sessionDirFormat = "20060102T150405Z"

// Session file names.
const (
	contextName  = "context.md"
	proposalName = "proposed-catalog.toml"
	rejectedName = "rejected.txt"
)

// maxDirCollisions bounds the numeric-suffix search when two sessions land
// in the same second. Beyond it something is wrong with the clock or the
// filesystem, and failing loudly beats spinning.
const maxDirCollisions = 100

// NewSessionDir creates history/<ts> under root, with root itself created on
// first use (doctor's directories never exist until doctor runs). The name
// is the session's UTC timestamp; a same-second collision gets a numeric
// suffix rather than silently reusing — and so appending into — an existing
// session. The leaf uses os.Mkdir, not MkdirAll, so a race with another
// doctor run fails into a new name instead of sharing a directory.
func NewSessionDir(root string, now time.Time) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("history: %w", err)
	}
	base := now.UTC().Format(sessionDirFormat)
	for n := 0; n < maxDirCollisions; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n+1)
		}
		dir := filepath.Join(root, name)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("history: %w", err)
		}
	}
	return "", fmt.Errorf("history: could not create session dir for %s", base)
}

// writeSession writes context.md, the audit trail: the facts, every prompt
// sent, every raw response, and every verdict. Raw responses are redacted
// first — a successful draft that echoes a credential must not persist it
// here. 0600, like everything doctor writes: the facts contain local paths
// and binary output the user may not want world-readable. Prompts and
// responses are indented as code blocks so the TOML fences inside them
// cannot restructure the document.
func writeSession(dir, bin string, f Facts, attempts []Attempt) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# ofa doctor — %s\n\nSession directory: %s\n\n## Facts\n\n%s\n", bin, dir, f)
	for i, att := range attempts {
		fmt.Fprintf(&b, "## Attempt %d\n\n### Prompt sent\n\n%s\n\n", i+1, indentBlock(att.Prompt))
		out := redactSecrets(att.Output)
		if out == "" {
			out = "(no output)"
		}
		fmt.Fprintf(&b, "### Raw response\n\n%s\n\n", indentBlock(out))
		if att.Err == "" {
			fmt.Fprintf(&b, "### Verdict\n\naccepted\n\n")
		} else {
			fmt.Fprintf(&b, "### Verdict\n\nrejected: %s\n\n", att.Err)
		}
	}
	return os.WriteFile(filepath.Join(dir, contextName), []byte(b.String()), 0o600)
}

// rejectedContent renders rejected.txt: the reason the draft failed after
// the retry, plus the raw output of the last attempt so nothing is lost.
func rejectedContent(reason error, attempts []Attempt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ofa doctor rejected the agent's draft.\n\nReason: %s\n", reason)
	if len(attempts) > 0 {
		fmt.Fprintf(&b, "\n--- raw output of the final attempt ---\n%s\n", attempts[len(attempts)-1].Output)
	}
	return b.String()
}

// indentBlock renders s as an indented (4-space) code block, one line per
// input line, so embedded triple backticks cannot break out of the block.
func indentBlock(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "(empty)"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
