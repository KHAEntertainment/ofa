package doctor

import (
	"encoding/json"
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
func SaveSessionRecord(dir string, rec SessionRecord) error {
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
