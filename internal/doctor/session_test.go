package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidSessionID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"20261028T123456Z", true},
		{"20261028T123456Z-2", true},
		{"20261028T123456Z-13", true},
		{"", false},
		{"20261028T123456", false},        // missing Z
		{"20261028t123456Z", false},       // lowercase t
		{"20261028T123456Z-2-3", false},   // double suffix
		{"../20261028T123456Z", false},    // traversal
		{"/20261028T123456Z", false},      // separator
		{"20261028T123456Z/../..", false}, // traversal in suffix
		{".", false},
		{"..", false},
		{"x20261028T123456Z", false},
	}
	for _, c := range cases {
		if got := ValidSessionID(c.id); got != c.want {
			t.Errorf("ValidSessionID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

func TestSessionRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := SessionRecord{
		Target:    "sh",
		HarnessID: "sh-probe",
		Time:      time.Date(2026, 10, 28, 12, 34, 56, 0, time.UTC),
		Status:    StatusProposed,
	}
	if err := SaveSessionRecord(dir, rec); err != nil {
		t.Fatalf("SaveSessionRecord: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, sessionName))
	if err != nil {
		t.Fatalf("stat session.json: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("session.json mode = %o, want 600", st.Mode().Perm())
	}
	got, err := LoadSessionRecord(dir)
	if err != nil {
		t.Fatalf("LoadSessionRecord: %v", err)
	}
	if got != rec {
		t.Errorf("round trip = %+v, want %+v", got, rec)
	}
	// Installed transitions keep the fields the record already had.
	rec.Status = StatusInstalled
	rec.InstalledPath = "/home/u/.config/ofa/catalog/sh-probe.toml"
	if err := SaveSessionRecord(dir, rec); err != nil {
		t.Fatalf("SaveSessionRecord (installed): %v", err)
	}
	got, err = LoadSessionRecord(dir)
	if err != nil {
		t.Fatalf("LoadSessionRecord (installed): %v", err)
	}
	if got.Status != StatusInstalled || got.InstalledPath != rec.InstalledPath || got.Target != "sh" {
		t.Errorf("installed record = %+v", got)
	}
}

func TestLoadSessionRecordMissingAndIncomplete(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadSessionRecord(dir); err == nil {
		t.Error("missing session.json loaded without error")
	}
	// PR B sessions have no session.json; a record without a target must
	// not be usable either — --install never guesses.
	if err := os.WriteFile(filepath.Join(dir, sessionName), []byte(`{"status":"proposed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionRecord(dir); err == nil {
		t.Error("record without target loaded without error")
	}
}

func TestRunRecordsSessionVerdicts(t *testing.T) {
	// Accepted: proposed, with the target and harness id.
	opts := runOpts(t)
	opts.Agent = (&fakeAgent{responses: []string{nativeEntryFor("sh", "/bin/sh")}}).Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n"}}).Run
	res, err := Run("sh", opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, err := LoadSessionRecord(res.Dir)
	if err != nil {
		t.Fatalf("LoadSessionRecord: %v", err)
	}
	if rec.Target != "sh" || rec.Status != StatusProposed || rec.HarnessID != "sh-probe" {
		t.Errorf("accepted record = %+v, want target sh, proposed, sh-probe", rec)
	}

	// Rejected after the retry: rejected, with the target.
	opts = runOpts(t)
	opts.Agent = (&fakeAgent{responses: []string{"garbage", "still garbage"}}).Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n"}}).Run
	res, err = Run("sh", opts)
	if err == nil {
		t.Fatal("Run accepted garbage")
	}
	rec, err = LoadSessionRecord(res.Dir)
	if err != nil {
		t.Fatalf("LoadSessionRecord (rejected): %v", err)
	}
	if rec.Target != "sh" || rec.Status != StatusRejected {
		t.Errorf("rejected record = %+v, want target sh, rejected", rec)
	}

	// Agent failure: also a verdict, so --list shows the session ended.
	opts = runOpts(t)
	opts.Agent = (&fakeAgent{errs: []error{errors.New("claude: killed")}}).Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n"}}).Run
	res, err = Run("sh", opts)
	if err == nil {
		t.Fatal("Run survived an agent failure")
	}
	rec, err = LoadSessionRecord(res.Dir)
	if err != nil {
		t.Fatalf("LoadSessionRecord (agent failure): %v", err)
	}
	if rec.Target != "sh" || rec.Status != StatusRejected {
		t.Errorf("agent-failure record = %+v, want target sh, rejected", rec)
	}
}

func TestLoadProposal(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadProposal(dir); err == nil {
		t.Error("missing proposal loaded without error")
	}
	if err := os.WriteFile(filepath.Join(dir, proposalName), []byte("id = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadProposal(dir)
	if err != nil || string(b) != "id = \"x\"\n" {
		t.Errorf("LoadProposal = %q, %v", string(b), err)
	}
}

// A3: the record is published atomically and its mode is forced to 0600
// even over an older, looser file — a crash mid-write must not destroy
// the record --install needs later.
func TestSaveSessionRecordAtomicAndForcedMode(t *testing.T) {
	dir := t.TempDir()
	loose := "{\n  \"target\": \"sh\",\n  \"time\": \"2026-01-01T00:00:00Z\",\n  \"status\": \"proposed\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, sessionName), []byte(loose), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := SessionRecord{
		Target: "sh",
		Time:   time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC),
		Status: StatusInstalled,
	}
	if err := SaveSessionRecord(dir, rec); err != nil {
		t.Fatalf("SaveSessionRecord: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, sessionName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600 forced over the old 644", st.Mode().Perm())
	}
	got, err := LoadSessionRecord(dir)
	if err != nil || got != rec {
		t.Errorf("round trip = %+v, %v; want %+v", got, err, rec)
	}
	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".session-") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

// sessionsFixture builds a sessions directory with one real session, one
// symlink that stays inside it, and one that leaves it.
func sessionsFixture(t *testing.T) (root string, real string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "history")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	real = filepath.Join(root, "20260101T000000Z")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, real
}

// A session directory can be a symlink. It is followed when its target
// stays inside the sessions directory, and refused — with a reason that
// names no path — when it does not.
func TestResolveSessionDir(t *testing.T) {
	root, real := sessionsFixture(t)

	// A symlink to a session that is inside the sessions directory.
	if err := os.Symlink(real, filepath.Join(root, "20260102T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink out of it.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "20260103T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A sibling directory whose name shares the sessions directory's as
	// a prefix: containment is component-wise, so a prefix match is not
	// "inside".
	sibling := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-evil")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, filepath.Join(root, "20260104T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink to nowhere.
	if err := os.Symlink(filepath.Join(root, "nope"), filepath.Join(root, "20260105T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink to a file inside the sessions directory.
	plain := filepath.Join(root, "plain-file")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plain, filepath.Join(root, "20260106T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink to a file outside it: it leaves the sessions directory
	// before the shape is even looked at.
	outsideFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "20260109T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink to the sessions directory itself.
	if err := os.Symlink(root, filepath.Join(root, "20260107T000000Z")); err != nil {
		t.Fatal(err)
	}

	// A real session resolves to itself.
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSessionDir(root, "20260101T000000Z")
	if err != nil {
		t.Fatalf("a real session was refused: %v", err)
	}
	if got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
	// A symlink that stays inside resolves to the session it names.
	got, err = ResolveSessionDir(root, "20260102T000000Z")
	if err != nil {
		t.Fatalf("a symlink inside the sessions directory was refused: %v", err)
	}
	if got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}

	// Everything else is refused, with a reason that names no path.
	for _, c := range []struct{ id, reason string }{
		{"20260103T000000Z", "symlink leaves the sessions directory"},
		{"20260104T000000Z", "symlink leaves the sessions directory"},
		{"20260105T000000Z", "dangling symlink"},
		{"20260106T000000Z", "not a directory"},
		{"20260107T000000Z", "symlink points at the sessions directory itself"},
		{"20260108T000000Z", "no such session directory"},
		{"20260109T000000Z", "symlink leaves the sessions directory"},
	} {
		_, err := ResolveSessionDir(root, c.id)
		if err == nil {
			t.Errorf("%s: resolved, want the refusal %q", c.id, c.reason)
			continue
		}
		if !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: error = %q, want %q", c.id, err.Error(), c.reason)
		}
		if strings.Contains(err.Error(), filepath.Dir(root)) {
			t.Errorf("%s: the error carried a path: %q", c.id, err.Error())
		}
	}
}

// A sessions directory that is itself a symlink still contains its own
// sessions: the boundary is the RESOLVED root.
func TestResolveSessionDir_RootIsSymlink(t *testing.T) {
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "history"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "doctor", "history")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "history"), root); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "20260101T000000Z")
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(session)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSessionDir(root, "20260101T000000Z")
	if err != nil {
		t.Fatalf("a session behind a symlinked sessions directory was refused: %v", err)
	}
	if got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

// The listing names the entries it cannot use, with the reason, and
// never names a path.
func TestListSessions(t *testing.T) {
	root, real := sessionsFixture(t)

	// A symlink to a session inside the sessions directory.
	if err := os.Symlink(real, filepath.Join(root, "20260102T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink out of it.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "20260103T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A symlink to nowhere.
	if err := os.Symlink(filepath.Join(root, "nope"), filepath.Join(root, "20260104T000000Z")); err != nil {
		t.Fatal(err)
	}
	// A plain file is not a session and carries no reason.
	if err := os.WriteFile(filepath.Join(root, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := ListSessions(root)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	byName := map[string]SessionEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if len(byName) != 4 {
		t.Errorf("listed %d entries, want 4 (the plain file is not a session): %+v", len(byName), entries)
	}
	for _, name := range []string{"20260101T000000Z", "20260102T000000Z"} {
		e, ok := byName[name]
		if !ok {
			t.Errorf("%s: not listed", name)
			continue
		}
		if e.Skip != "" {
			t.Errorf("%s: skipped with %q, want a session", name, e.Skip)
		}
		if e.Dir == "" {
			t.Errorf("%s: listed with no directory", name)
		}
	}
	for _, c := range []struct{ name, reason string }{
		{"20260103T000000Z", "symlink leaves the sessions directory"},
		{"20260104T000000Z", "dangling symlink"},
	} {
		e, ok := byName[c.name]
		if !ok {
			t.Errorf("%s: not listed, want a skipped entry", c.name)
			continue
		}
		if e.Skip != c.reason {
			t.Errorf("%s: skip = %q, want %q", c.name, e.Skip, c.reason)
		}
		if e.Dir != "" {
			t.Errorf("%s: a skipped entry carries a directory: %q", c.name, e.Dir)
		}
		if strings.Contains(e.Skip, filepath.Dir(root)) {
			t.Errorf("%s: the reason carried a path: %q", c.name, e.Skip)
		}
	}
	if _, ok := byName["stray"]; ok {
		t.Error("a plain file was listed as an entry")
	}
}
