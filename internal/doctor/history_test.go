package doctor

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Persisted-output redaction (#28): every file doctor writes into a session
// must carry [redacted] wherever a credential value — raw, query-escaped or
// path-escaped — would have landed. All values below are fake.

const (
	// fakeAPIKey has '/', '+', '=' and a space, so its raw, QueryEscape
	// and PathEscape forms are three different strings.
	fakeAPIKey    = "sk-ant/fake+key=0001 x"
	fakeAuthToken = "fake-auth-token-aaaa"
	fakeOAuth     = "fake-oauth/token-bbbb"
)

// setFakeCredentials sets all three credential variables to fake values and
// returns every form redaction must catch.
func setFakeCredentials(t *testing.T) []string {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", fakeAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", fakeAuthToken)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", fakeOAuth)
	var forms []string
	for _, v := range []string{fakeAPIKey, fakeAuthToken, fakeOAuth} {
		forms = append(forms, v, url.QueryEscape(v), url.PathEscape(v))
	}
	return forms
}

func assertNoCredential(t *testing.T, what, s string, forms []string) {
	t.Helper()
	for _, f := range forms {
		if strings.Contains(s, f) {
			t.Errorf("%s leaks credential form %q:\n%s", what, f, s)
		}
	}
}

func TestCredentialFormsDistinctAndLongestFirst(t *testing.T) {
	setFakeCredentials(t)
	forms := credentialForms()
	// fakeAPIKey yields 3 distinct forms; fakeAuthToken encodes to itself
	// (1); fakeOAuth's QueryEscape and PathEscape agree (2).
	if len(forms) != 6 {
		t.Fatalf("forms = %q, want 6 distinct", forms)
	}
	for i := 1; i < len(forms); i++ {
		if len(forms[i]) > len(forms[i-1]) {
			t.Fatalf("forms not longest first: %q", forms)
		}
	}
}

func TestCredentialFormsNoneSet(t *testing.T) {
	for _, k := range credentialVars {
		t.Setenv(k, "")
	}
	if got := credentialForms(); len(got) != 0 {
		t.Errorf("forms with nothing set = %q, want none", got)
	}
	if got := redactSecrets("plain text"); got != "plain text" {
		t.Errorf("redactSecrets with nothing set = %q", got)
	}
	if containsSecret("plain text") {
		t.Error("containsSecret matched with nothing set")
	}
}

func TestRedactSecretsEncodedForms(t *testing.T) {
	forms := setFakeCredentials(t)
	for _, f := range forms {
		got := redactSecrets("before " + f + " after")
		if got != "before [redacted] after" {
			t.Errorf("redactSecrets(%q) = %q", f, got)
		}
	}
}

// Partial echoes are out of scope by decision (see redactSecrets): a
// fragment survives. This pins the documented limit, so widening it is a
// deliberate change, not an accident.
func TestRedactSecretsPartialEchoIsOutOfScope(t *testing.T) {
	setFakeCredentials(t)
	const fragment = "sk-ant/fake"
	if got := redactSecrets("prefix " + fragment); got != "prefix "+fragment {
		t.Errorf("redactSecrets redacted a partial echo: %q", got)
	}
}

func TestWriteSessionRedactsEverySection(t *testing.T) {
	forms := setFakeCredentials(t)
	dir := t.TempDir()
	facts := Facts{Bin: "sh", Path: "/bin/sh", RealPath: "/bin/sh", HelpOut: "help mentions " + fakeOAuth}
	attempts := []Attempt{
		{Prompt: "prompt with " + fakeAuthToken, Output: "draft echoes " + fakeAPIKey, Err: "verdict quotes " + url.QueryEscape(fakeAPIKey)},
		{Prompt: "retry with " + url.PathEscape(fakeAPIKey), Output: "", Err: ""},
	}
	if err := writeSession(dir, "sh", facts, attempts); err != nil {
		t.Fatalf("writeSession: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, contextName))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	assertNoCredential(t, "context.md", got, forms)
	if n := strings.Count(got, "[redacted]"); n != 5 {
		t.Errorf("context.md has %d [redacted] markers, want 5 (facts, 2 prompts, response, verdict):\n%s", n, got)
	}
}

func TestRejectedContentRedacted(t *testing.T) {
	forms := setFakeCredentials(t)
	c := rejectedContent(errors.New("reason quotes "+fakeOAuth), []Attempt{
		{Output: "first " + fakeAuthToken},
		{Output: "raw " + fakeAPIKey + " q " + url.QueryEscape(fakeAPIKey) + " p " + url.PathEscape(fakeAPIKey)},
	})
	assertNoCredential(t, "rejected.txt", c, forms)
	if n := strings.Count(c, "[redacted]"); n != 4 {
		t.Errorf("rejected.txt has %d [redacted] markers, want 4:\n%s", n, c)
	}
}

// A validated proposal is never redacted — its bytes are what is reviewed
// and installed — so one carrying a credential is refused before parsing,
// with a reason that names no value. Both entry points share the gate.
func TestValidateProposalRejectsCredentialForms(t *testing.T) {
	forms := setFakeCredentials(t)
	for _, f := range forms {
		draft := strings.Replace(nativeEntryFor("sh", "/bin/sh"), `name = "sh probe"`, `name = "sh `+f+`"`, 1)
		if !strings.Contains(draft, f) {
			t.Fatalf("fixture did not embed %q", f)
		}
		gateRejectedBeforeExecution(t, draft, "sh", "/bin/sh")
		_, err := ValidateProposal(draft, "sh", "/bin/sh", (&fakeRunner{}).Run)
		if err == nil || !strings.Contains(err.Error(), "credential") {
			t.Fatalf("err = %v, want the credential rejection", err)
		}
		assertNoCredential(t, "rejection reason", err.Error(), forms)
		if _, err := ValidateSavedProposal([]byte(draft), "sh", "/bin/sh", (&fakeRunner{}).Run); err == nil {
			t.Errorf("saved proposal embedding %q accepted", f)
		}
	}
}

// End to end: a failed draft that echoes credentials leaves every session
// file, the retry prompt and the returned error free of them.
func TestRunRejectedDraftPersistsRedacted(t *testing.T) {
	forms := setFakeCredentials(t)
	draft := func(form string) string {
		return "Here you go, " + url.PathEscape(fakeOAuth) + ":\n```toml\n" +
			strings.Replace(nativeEntryFor("sh", "/bin/sh"), `name = "sh probe"`, `name = "sh `+form+`"`, 1) + "\n```"
	}
	agent := &fakeAgent{responses: []string{draft(fakeAPIKey), draft(url.QueryEscape(fakeAPIKey))}}
	opts := runOpts(t)
	opts.Agent = agent.Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n"}}).Run

	res, err := Run("sh", opts)
	if err == nil || !strings.Contains(err.Error(), "proposal rejected") {
		t.Fatalf("Run err = %v, want proposal rejected", err)
	}
	assertNoCredential(t, "returned error", err.Error(), forms)
	if agent.calls() != 2 {
		t.Fatalf("agent calls = %d, want 2 (initial + retry)", agent.calls())
	}
	assertNoCredential(t, "retry prompt", agent.prompts[1], forms)
	if res.RejectedPath == "" || res.ProposalPath != "" {
		t.Fatalf("rejected = %q, proposal = %q", res.RejectedPath, res.ProposalPath)
	}
	entries, err := os.ReadDir(res.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(res.Dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		assertNoCredential(t, e.Name(), string(b), forms)
	}
	for _, name := range []string{contextName, rejectedName} {
		b, _ := os.ReadFile(filepath.Join(res.Dir, name))
		if !strings.Contains(string(b), "[redacted]") {
			t.Errorf("%s lacks [redacted]:\n%s", name, b)
		}
	}
}

// tomlEscape spells s entirely as TOML \uXXXX escapes. The source text
// then holds no credential form, but the decoded string is the value.
func tomlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, `\u%04X`, r)
	}
	return b.String()
}

// escapedDrafts are entries whose credential is rebuilt only by decoding:
// in id, in a version.cmd argument, in update.cmd, in homepage, and in a
// method the parser itself rejects (its error must still not echo it).
func escapedDrafts() map[string]string {
	base := nativeEntryFor("sh", "/bin/sh")
	return map[string]string{
		"id":          strings.Replace(base, `id = "sh-probe"`, `id = "`+tomlEscape(fakeAuthToken)+`"`, 1),
		"version.cmd": strings.Replace(base, `version = { cmd = ["sh", "--version"] }`, `version = { cmd = ["sh", "`+tomlEscape(fakeAuthToken)+`"] }`, 1),
		"update.cmd":  base + `update = { cmd = ["sh", "` + tomlEscape(fakeOAuth) + `"] }` + "\n",
		"homepage":    strings.Replace(base, `name = "sh probe"`, `name = "sh probe"`+"\nhomepage = \""+tomlEscape(fakeAPIKey)+`"`, 1),
		"method":      strings.Replace(base, `method = "native"`, `method = "`+tomlEscape(fakeAuthToken)+`"`, 1),
	}
}

// B2: TOML escapes rebuild a credential the source-text check cannot see.
// The decoded entry is checked too, and every error leaving the gate is
// redacted.
func TestValidateProposalRejectsEscapedCredentials(t *testing.T) {
	forms := setFakeCredentials(t)
	for field, draft := range escapedDrafts() {
		t.Run(field, func(t *testing.T) {
			if containsSecret(draft) {
				t.Fatalf("fixture leaks a credential form in its source text:\n%s", draft)
			}
			run := &fakeRunner{}
			_, err := ValidateProposal(draft, "sh", "/bin/sh", run.Run)
			if err == nil {
				t.Fatal("escaped credential accepted")
			}
			if len(run.calls) != 0 {
				t.Errorf("a command ran before the rejection: %v", run.calls)
			}
			assertNoCredential(t, "rejection reason", err.Error(), forms)
			assertNoCredential(t, "retry prompt", RetryPrompt("p", err), forms)
			if field != "method" && err.Error() != errCredentialInProposal.Error() {
				t.Errorf("err = %q, want the decoded-field credential rejection", err)
			}
			_, serr := ValidateSavedProposal([]byte(draft), "sh", "/bin/sh", (&fakeRunner{}).Run)
			if serr == nil {
				t.Fatal("saved proposal with an escaped credential accepted")
			}
			assertNoCredential(t, "saved-proposal rejection", serr.Error(), forms)
		})
	}
}

// redactErr is the boundary: a deep error quoting a decoded value leaves
// redacted, and an error with nothing to redact is passed through as is.
func TestRedactErrBoundary(t *testing.T) {
	forms := setFakeCredentials(t)
	assertNoCredential(t, "redactErr", redactErr(fmt.Errorf("bad value %q", fakeAuthToken)).Error(), forms)
	plain := errors.New("plain")
	if redactErr(plain) != plain {
		t.Error("redactErr replaced an error that needed no redaction")
	}
	if redactErr(nil) != nil {
		t.Error("redactErr(nil) != nil")
	}
}

// End to end with escaped drafts: no session file, the retry prompt and the
// returned error hold a credential form.
func TestRunEscapedCredentialDraftPersistsNothing(t *testing.T) {
	forms := setFakeCredentials(t)
	drafts := escapedDrafts()
	agent := &fakeAgent{responses: []string{drafts["version.cmd"], drafts["update.cmd"]}}
	opts := runOpts(t)
	opts.Agent = agent.Run
	opts.Runner = (&fakeRunner{responses: map[string]string{"sh": "sh 1.0\n", "/bin/sh": "sh 1.0\n", "/usr/bin/sh": "sh 1.0\n"}}).Run

	res, err := Run("sh", opts)
	if err == nil {
		t.Fatal("Run accepted an escaped credential")
	}
	assertNoCredential(t, "returned error", err.Error(), forms)
	if agent.calls() != 2 {
		t.Fatalf("agent calls = %d, want 2", agent.calls())
	}
	assertNoCredential(t, "retry prompt", agent.prompts[1], forms)
	entries, err := os.ReadDir(res.Dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
		b, err := os.ReadFile(filepath.Join(res.Dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		assertNoCredential(t, e.Name(), string(b), forms)
	}
	for _, want := range []string{contextName, rejectedName, "session.json"} {
		if !seen[want] {
			t.Errorf("session lacks %s; found %v", want, entries)
		}
	}
}

// session.json is redacted at the writer, whatever its caller passes.
func TestSaveSessionRecordRedacts(t *testing.T) {
	forms := setFakeCredentials(t)
	dir := t.TempDir()
	rec := SessionRecord{Target: "sh", HarnessID: "id-" + fakeAuthToken, Status: StatusProposed, InstalledPath: "/x/" + url.PathEscape(fakeAPIKey)}
	if err := SaveSessionRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoCredential(t, "session.json", string(b), forms)
	if strings.Count(string(b), "[redacted]") != 2 {
		t.Errorf("session.json = %s, want 2 [redacted] markers", b)
	}
}
