package doctor

import (
	"errors"
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
