package google_test

// Criterion 16: the runbook's Microsoft section (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md). A test on prose earns its place
// here the way internal/mcpserver/runbook_test.go does: nothing in code can stop
// the next session — or Salvador in six months — re-deriving the Azure
// registration, onboarding before the deploy lands (which makes every in-cluster
// pass log a D9 error row), or looking for a client secret that does not exist.
//
// EXPECTED FAILURE MODE: **assertion failures** — docs/runbooks/imap-mail-connector.md
// exists but has no Microsoft section, so every token below is missing today.
//
// Scope, deliberately: the six things criterion 16 enumerates (the Azure click
// path, what Salvador must produce, the env vars, the onboarding command, the
// rollout order, the revocation/re-consent procedure) plus the two facts that
// cost real time when absent — that IMAP basic auth is refused by the server,
// and that this mailbox cannot send. Nothing else; a doc test that pins every
// sentence makes the doc unmaintainable.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRunbook_DocumentsTheMicrosoftMailbox(t *testing.T) {
	const rel = "docs/runbooks/imap-mail-connector.md"
	b, err := os.ReadFile(filepath.Join(msxRepoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	doc := string(b)

	// ---- literal tokens, exactly as they must be typed ------------------------
	for _, want := range []struct{ tok, why string }{
		{"google-auth add-microsoft", "the onboarding command, verbatim (criterion 16)"},
		{"MS_OAUTH_CLIENT_ID", "the one required env var (D10); absent, every pass writes a D9 error row"},
		{"MS_OAUTH_AUTHORITY", "the optional authority override, and where its default comes from (D10)"},
		{"https://login.microsoftonline.com/consumers", "the audience Q2 chose: personal accounts ONLY"},
		{"IMAP.AccessAsUser.All", "the delegated permission to tick in the API permissions blade"},
		{"offline_access", "without it no refresh token is issued and the mailbox dies in an hour (D2)"},
		{"Application (client) ID", "what Salvador hands switchboard — the exact label on the Azure blade"},
		{"App registrations", "the click path: Entra ID -> App registrations -> New registration"},
		{"Allow public client flows", "the ONE Authentication setting the device flow needs (D1)"},
		{"Personal Microsoft accounts only", "the supported-account-types choice, verbatim (Q2)"},
		{"0037", "the migration to apply BEFORE anything else (merging one is not applying it)"},
	} {
		if !strings.Contains(doc, want.tok) {
			t.Errorf("%s never mentions %q — %s", rel, want.tok, want.why)
		}
	}

	// ---- prose, case-insensitive ---------------------------------------------
	for _, want := range []struct{ re, why string }{
		{`(?i)no (client )?secret`,
			"stated plainly: a public client is issued NO secret, so nobody goes looking for one to store (D1)"},
		{`(?i)logindisabled`,
			"the forcing fact: outlook.office365.com:993 refuses password logins, so an app password is " +
				"irrelevant for IMAP — the thing Salvador spent an onboarding attempt discovering"},
		{`(?i)device`,
			"the flow is device code: the browser step happens on any device while the CLI polls (D1)"},
		{`(?i)send_enabled`,
			"this mailbox cannot send: no SMTP.Send consent, send_enabled=false, and MailSender refuses it " +
				"by name (D6). Without saying so, the first reply attempt looks like a bug"},
		{`(?i)(revoke|revocation|re-?consent)`,
			"the recovery procedure: what a revoked consent looks like and how to re-consent (criterion 16)"},
		// The rollout BARRIER, in order: apply 0037 -> deploy the env var -> only
		// then onboard. Onboarding first means every in-cluster pass logs a D9
		// error row for the mailbox until the deploy lands.
		{`(?s)0037.{0,1000}MS_OAUTH_CLIENT_ID.{0,1000}add-microsoft`,
			"the rollout ORDER (D-rollout): migration, then the image bump plus MS_OAUTH_CLIENT_ID on the " +
				"connector CronJob and the watch Deployment, and only then the onboarding"},
		{`(?i)cronjob`,
			"…naming the connector CronJob as one of the two workloads that needs the variable"},
		{`(?i)(watch (deployment|deploy)|deployment)`,
			"…and the watch Deployment as the other"},
		{`(?i)invalid_grant`,
			"the symptom of a revoked/expired refresh token, as it appears in sync_runs.error (D9), so the " +
				"operator can match what they see to the procedure"},
	} {
		re, err := regexp.Compile(want.re)
		if err != nil {
			t.Fatalf("bad regexp %q: %v", want.re, err)
		}
		if !re.MatchString(doc) {
			t.Errorf("%s does not match %q — %s", rel, want.re, want.why)
		}
	}

	// The runbook must not promise a capability this ticket deliberately did not
	// build: a sentence telling him to send from the MSN mailbox would be a false
	// claim about the system (IK: "a comment can be a defect", applied to docs).
	for _, forbidden := range []string{"SMTP.Send", "Mail.Send"} {
		if idx := strings.Index(doc, forbidden); idx >= 0 {
			window := doc[max0(idx-200):min0(idx+200, len(doc))]
			if !regexp.MustCompile(`(?i)(not requested|out of scope|do not|don't|never|future)`).MatchString(window) {
				t.Errorf("%s names %s without saying it is NOT requested; the scope is IMAP read only", rel, forbidden)
			}
		}
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

func min0(i, n int) int {
	if i > n {
		return n
	}
	return i
}
