package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// sessionName is the bookkeeping file inside one history session directory.
// It records the target binary and the session's verdict so
// `ofa doctor --install <session-id>` can revalidate against the right
// target, and `ofa doctor --list` can show what each session produced.
const sessionName = "session.json"

// Session verdicts, recorded in session.json.
const (
	// StatusProposed marks a session whose draft was validated and is
	// waiting for a human decision.
	StatusProposed = "proposed"
	// StatusRejected marks a session that ended without a valid
	// proposal — a rejected draft or a failed agent run.
	StatusRejected = "rejected"
	// StatusInstalled marks a proposal that was written into the user
	// catalog.
	StatusInstalled = "installed"
)

// SessionRecord is the small, machine-written record of one doctor
// session. Nothing here is trusted at install time — the proposal itself
// is fully revalidated — but Target is the one fact --install cannot
// recover from the proposal file: the shape gate binds every install's
// bin to the target, so taking the target from the file would make that
// check vacuous.
type SessionRecord struct {
	// Target is the binary the session drafted an entry for.
	Target string `json:"target"`
	// HarnessID is the proposed entry's id, recorded when known.
	HarnessID string `json:"harness_id,omitempty"`
	// Time is the session's start time, UTC.
	Time time.Time `json:"time"`
	// Status is one of StatusProposed / StatusRejected / StatusInstalled.
	Status string `json:"status"`
	// InstalledPath is the user catalog file written on install.
	InstalledPath string `json:"installed_path,omitempty"`
}

// sessionIDPattern matches the directory names NewSessionDir mints
// (UTC timestamp, optional numeric suffix for a same-second collision).
// `--install <id>` joins the id straight into a path, so anything outside
// this shape — separators, "..", or an id that merely looks like one — is
// rejected before the join: a session id is a name, not a path.
var sessionIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z(-\d+)?$`)

// ValidSessionID reports whether id can name a history session directory.
func ValidSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// SaveSessionRecord writes rec to dir/session.json: a private temp file
// in the same directory, fsynced, then renamed into place — atomic, like
// the catalog write, so a crash mid-write can never destroy the record
// --install needs to work later. The rename also forces 0600 even over
// an older, looser file: the record names local binaries and paths.
//
// Every string field is redacted before encoding, as defence in depth: the
// proposal gate already refuses an entry carrying a credential, but the
// writer must not depend on that (redacting the JSON bytes instead would
// miss a value that JSON encoding escaped).
func SaveSessionRecord(dir string, rec SessionRecord) error {
	rec.Target = redactSecrets(rec.Target)
	rec.HarnessID = redactSecrets(rec.HarnessID)
	rec.Status = redactSecrets(rec.Status)
	rec.InstalledPath = redactSecrets(rec.InstalledPath)
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	name := tmp.Name()
	published := false
	defer func() {
		if !published {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, sessionName)); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	published = true
	return nil
}

// LoadSessionRecord reads dir/session.json. A missing or unreadable file
// is an error the callers report as "cannot install this session": PR B
// sessions predate the record, and --install never guesses the target.
func LoadSessionRecord(dir string) (SessionRecord, error) {
	b, err := os.ReadFile(filepath.Join(dir, sessionName))
	if err != nil {
		return SessionRecord{}, fmt.Errorf("session: %w", err)
	}
	var rec SessionRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return SessionRecord{}, fmt.Errorf("session: %w", err)
	}
	if rec.Target == "" || rec.Status == "" {
		return SessionRecord{}, fmt.Errorf("session: incomplete record (target or status missing)")
	}
	return rec, nil
}

// LoadProposal reads the session's proposed-catalog.toml. The bytes are
// untrusted: they are revalidated in full before anything is installed.
func LoadProposal(dir string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, proposalName))
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	return b, nil
}

// SessionEntry is one entry in the doctor sessions directory, as
// `ofa doctor --list` renders it.
type SessionEntry struct {
	// Name is the entry's name inside the sessions directory.
	Name string
	// Dir is the session directory: resolved, and proven to be inside
	// the sessions directory. It is empty when Skip is set.
	Dir string
	// Skip explains why the entry is not a session. It is empty for a
	// session. It never carries a path — it says what KIND of entry it
	// is — so a link target crafted to look like something else cannot
	// put a path into the output.
	Skip string
}

// ListSessions enumerates the entries of the sessions directory root.
//
// Three kinds of entry come back:
//
//   - a session: a real directory, or a symlink whose target resolves
//     inside root. Both are listed, and both can be installed from.
//   - a skipped entry: a symlink that leaves the sessions directory,
//     dangles, or names a file. It carries the reason, so the caller
//     can say so out loud instead of dropping it silently.
//   - nothing at all: a plain file is not a session and carries no
//     reason.
//
// A symlink is followed only while its target stays inside root, so
// nothing here reads or writes outside the sessions directory.
func ListSessions(root string) ([]SessionEntry, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]SessionEntry, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		// A plain file is not a session. A symlink to one is, and the
		// reason it cannot be is reported rather than swallowed.
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		dir, reason := resolveSession(root, name)
		if reason != "" {
			out = append(out, SessionEntry{Name: name, Skip: reason})
			continue
		}
		out = append(out, SessionEntry{Name: name, Dir: dir})
	}
	return out, nil
}

// ResolveSessionDir resolves the session directory named id inside the
// sessions directory root.
//
// A session directory can be a symlink: `history/<id>` pointing at
// another session directory is a legal thing for a user to make, and it
// is followed when its target stays inside root. A link that leaves the
// sessions directory, dangles, or names a file is refused with the
// reason instead — nothing is read from it and nothing is written to
// it. The boundary is the RESOLVED root, so a sessions directory that
// is itself a symlink (a config directory kept in a dotfiles repo)
// still contains its own sessions.
//
// The path returned is the resolved directory, not the link: every read
// and write after this call goes to the place that was verified, so a
// link swapped afterwards cannot redirect them.
func ResolveSessionDir(root, id string) (string, error) {
	dir, reason := resolveSession(root, id)
	if reason != "" {
		return "", errors.New("session: " + reason)
	}
	return dir, nil
}

// resolveSession resolves one entry of the sessions directory to the
// directory doctor may read and write, and the reason it cannot when it
// cannot. An empty reason means the entry is a usable session.
//
// The reasons never name a path: they say what kind of entry it is, so
// a link target crafted to look like something else cannot put a path
// into the output.
func resolveSession(root, name string) (dir, reason string) {
	entry := filepath.Join(root, name)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "the sessions directory could not be resolved"
	}
	real, err := filepath.EvalSymlinks(entry)
	if err != nil {
		if os.IsNotExist(err) {
			// The entry does not resolve. A symlink that points
			// nowhere is one thing; an entry that is not there at all
			// is another, and the two are told apart here.
			if fi, lerr := os.Lstat(entry); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
				return "", "dangling symlink"
			}
			return "", "no such session directory"
		}
		return "", "the session directory could not be resolved"
	}
	if real == realRoot {
		return "", "symlink points at the sessions directory itself"
	}
	// Containment is checked BEFORE the shape: a target outside the
	// sessions directory is refused without anything else being looked
	// at, so nothing outside is even stat'ed.
	if !withinRoot(realRoot, real) {
		return "", "symlink leaves the sessions directory"
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", "the session directory could not be resolved"
	}
	if !st.IsDir() {
		return "", "not a directory"
	}
	return real, ""
}

// withinRoot reports whether path is inside root. Both are absolute and
// already resolved.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return filepath.IsLocal(rel)
}
