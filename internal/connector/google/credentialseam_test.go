package google_test

// STRUCTURE tests for the one-credential-seam rule (SPEC
// docs/tickets/microsoft-oauth-mail_SPEC.md, acceptance criteria 7 and 8;
// decision D7). Plain unit tests: no build tag, no database, so they run on
// every `go test ./...` — the shape internal/textmatch/callsites_test.go
// established.
//
// WHY A SOURCE SCAN: D7's whole argument is that three callers × two credential
// kinds is six branches and a guaranteed divergence — one caller silently
// ingesting three mailboxes while another does four, with nothing erroring. That
// failure is invisible to every behavioural test, because each caller passes its
// own test in isolation. The repo's most expensive recurring defect is a
// half-restated shared predicate (IK: SWT-18's constant discriminator, SWT-21's
// inert guard, the two upwork room columns), and this is the cheap mechanical
// guard against the next instance.
//
// EXPECTED FAILURE MODE: **assertion failures**, today, on the positive controls
// — internal/connector/google/credential.go does not exist and nothing names
// AuthTypeXOAuth2, so the scans find nothing. A scan that certifies an empty set
// certifies nothing, which is why each one fails loudly when its input is empty.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is this package's path back to the module root.
const msxRepoRoot = "../../.."

// msxScanFiles returns every non-test .go file under internal/ and cmd/, keyed by
// its repo-relative slash path.
func msxScanFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(msxRepoRoot, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(msxRepoRoot)+"/"))
			out[rel] = string(b)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(out) < 50 {
		t.Fatalf("the scan found only %d source files under internal/ and cmd/; the walk is broken and "+
			"every assertion built on it would pass vacuously", len(out))
	}
	return out
}

// msxCodeLines strips `//` comment LINES, deliberately keeping everything else
// (the upworkcrm keyspelling precedent): prose may explain a spelling that is
// gone, but a backticked SQL example in a comment is still code as far as the
// next person copying it is concerned.
func msxCodeLines(body string) string {
	var keep []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// ---- criterion 8: ONE place branches on credential kind ------------------------

func TestOnlyTheCredentialSeamBranchesOnAuthType(t *testing.T) {
	// The two files allowed to name both constants, and why each is not a
	// divergence risk:
	//   credential.go — OpenIMAPSource, the IMAP credential seam itself (D7).
	//   mailsender.go — the constants' declaration plus the SEND router's
	//                   refusal (D6). It decides a transport, not which
	//                   credential to resolve, and a send that fell through to
	//                   `default` would hand a Microsoft refresh token to the
	//                   Gmail-API sender.
	allowed := map[string]string{
		"internal/connector/google/credential.go": "OpenIMAPSource, the one credential seam (D7)",
		"internal/connector/google/mailsender.go": "the auth_type constants and the send router's refusal (D6)",
	}

	files := msxScanFiles(t)
	found := map[string]bool{}
	for rel, body := range files {
		code := msxCodeLines(body)
		if !strings.Contains(code, "AuthTypeAppPassword") || !strings.Contains(code, "AuthTypeXOAuth2") {
			continue
		}
		found[rel] = true
		if _, ok := allowed[rel]; !ok {
			t.Errorf("%s names both AuthTypeAppPassword and AuthTypeXOAuth2. Criterion 8: "+
				"google.OpenIMAPSource is the ONLY place that branches on credential kind. Three callers "+
				"(runIMAPIngest, idleOnce, opsctl mail refetch) × two credential kinds is six branches and "+
				"a guaranteed divergence — the failure being one caller ingesting three mailboxes while "+
				"another does four, with no error anywhere (D7)", rel)
		}
	}

	// POSITIVE CONTROL. Without this the test passes on the day the constants are
	// renamed, the seam is deleted, or the scan stops matching — certifying an
	// empty set.
	for rel, why := range allowed {
		if !found[rel] {
			t.Errorf("%s does not name both AuthTypeAppPassword and AuthTypeXOAuth2, so this scan proves "+
				"nothing: it is supposed to hold %s", rel, why)
		}
	}
}

// ---- criterion 7: ONE spelling of "the IMAP account set" -----------------------

// A predicate over the SET of auth_types. The single-value forms
// (`auth_type='app_password'` in UpsertAppPasswordAccount's INSERT, the
// calendar-phase guard) are NOT this rule: they say "this row is that kind", not
// "these are the mailboxes we read".
var msxAuthTypeSetPredicate = regexp.MustCompile(`(?i)auth_type\s*(=\s*ANY\s*\(|IN\s*\()`)

func TestAuthTypeAccountSetHasOneSpelling(t *testing.T) {
	const owner = "internal/connector/google/mailsender.go"

	files := msxScanFiles(t)
	hits := 0
	for rel, body := range files {
		code := msxCodeLines(body)
		if !msxAuthTypeSetPredicate.MatchString(code) {
			continue
		}
		hits++
		if rel != owner {
			t.Errorf("%s spells an auth_type SET predicate of its own. Criterion 7: ListIMAPAccounts "+
				"(%s) is the one spelling, `auth_type = ANY('{app_password,xoauth2}')`. A second spelling "+
				"is how one caller keeps ingesting three mailboxes while the other does four", rel, owner)
		}
	}
	if hits == 0 {
		t.Errorf("no auth_type set predicate found anywhere under internal/ or cmd/. Criterion 7 requires " +
			"ListIMAPAccounts to select `auth_type = ANY('{app_password,xoauth2}')`; either it does not " +
			"exist yet (the expected red) or this scan's regexp no longer matches the SQL")
	}
}

func TestListIMAPAccountsIsUsedByAllThreeCallers(t *testing.T) {
	// D7: renaming rather than adding a sibling is deliberate. Two functions
	// meaning "the IMAP account set" is the divergence this ticket exists to
	// avoid, so the old name must be GONE as an identifier — prose may still
	// explain it.
	oldCall := regexp.MustCompile(`ListAppPasswordAccounts\s*\(`)
	files := msxScanFiles(t)
	for rel, body := range files {
		if oldCall.MatchString(msxCodeLines(body)) {
			t.Errorf("%s still calls or declares ListAppPasswordAccounts. D7 RENAMES it ListIMAPAccounts "+
				"with the predicate auth_type = ANY('{app_password,xoauth2}'); leaving the old name as a "+
				"sibling is exactly the two-spellings failure", rel)
		}
	}

	callers := map[string]string{
		"cmd/connectors/google/mailsource.go": "runIMAPIngest, the one-shot pass",
		"cmd/connectors/google/watch.go":      "the watch Deployment's account list",
		"cmd/opsctl/mailrefetch.go":           "opsctl mail refetch",
	}
	seen := 0
	for rel, why := range callers {
		body, ok := files[rel]
		if !ok {
			t.Fatalf("%s is not in the scan; the file moved and this test has stopped checking anything", rel)
		}
		code := msxCodeLines(body)
		if !strings.Contains(code, "ListIMAPAccounts") {
			t.Errorf("%s (%s) does not call ListIMAPAccounts", rel, why)
			continue
		}
		if !strings.Contains(code, "OpenIMAPSource") {
			t.Errorf("%s (%s) does not call google.OpenIMAPSource. D7: all three callers resolve the "+
				"credential through the one seam; a caller that keeps its own "+
				"decrypt-then-construct sequence is the fourth branch", rel, why)
		}
		seen++
	}
	if seen != 3 {
		t.Errorf("only %d of the 3 known call sites use the seam; criterion 7 says all three do", seen)
	}
}

// ---- criterion 6 (the argv/env half): there is no Microsoft secret -------------

func TestNoMicrosoftClientSecretExists(t *testing.T) {
	files := msxScanFiles(t)

	clientIDReaders := 0
	for rel, body := range files {
		code := msxCodeLines(body)
		for _, forbidden := range []string{"MS_OAUTH_CLIENT_SECRET", "MS_OAUTH_SECRET", "MS_OAUTH_REFRESH_TOKEN"} {
			if strings.Contains(code, forbidden) {
				t.Errorf("%s names %s. D1/D10: the Azure app is a PUBLIC client — no secret is issued, none "+
					"is stored, and a refresh token lives only in refresh_token_encrypted (never in an env "+
					"var, where it would leak through /proc/<pid>/environ to every child process)", rel, forbidden)
			}
		}
		if strings.Contains(code, "MS_OAUTH_CLIENT_ID") {
			clientIDReaders++
		}
	}

	// POSITIVE CONTROL: the scan must have seen the real variable somewhere, or
	// the absence of the forbidden ones proves only that the walk found nothing.
	if clientIDReaders == 0 {
		t.Errorf("no file reads MS_OAUTH_CLIENT_ID. D10 requires it in every binary that resolves an " +
			"xoauth2 credential (google-auth, cmd/connectors/google one-shot AND watch, opsctl mail refetch), " +
			"so this scan is currently certifying an empty set")
	}
}
