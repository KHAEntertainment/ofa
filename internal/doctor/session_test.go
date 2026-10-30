package doctor

import (
	"errors"
	"os"
	"path/filepath"
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
