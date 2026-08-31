//go:build integration

package google_test

// SWT-25 (link-preservation) criteria 11, 12 and 13, against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run GmailLinks ./internal/connector/google/
//
// WHAT ONLY POSTGRES CAN PROVE HERE, and why this is not a unit test:
//
//   - that `upsertMessage` actually WRITES the column (criterion 11). A unit test
//     of NormalizeRFC822 supplies and then asserts the same value; only the round
//     trip shows the INSERT and the DO UPDATE list carry `links`. This repo's
//     sixth landmine is exactly this shape — drafts' locality guard passed its
//     unit test for weeks while `DeliverTasks` never selected the column.
//   - that the migration's `NOT NULL DEFAULT '[]'` plus the array CHECK does not
//     break the connectors that know nothing about links (criterion 13). Upwork,
//     Jira and slackweb insert normalized_messages WITHOUT naming the column;
//     that is precisely the sort of constraint that turns another connector's
//     insert into a runtime error nobody sees until a cron job fails.
//   - that a re-normalize (the `--normalize-only --all` backfill) is IDEMPOTENT:
//     same row, same id, refreshed value, no duplicate. The upsert keys on
//     raw_source_item_id, which is what keeps capture_decisions, ai_extractions
//     and the eval labels attached across a full corpus rebuild.
//
// GREENFIELD NOTE: migration 0017 and the `links` column do not exist yet, so
// every assertion below fails with `column "links" does not exist` — the
// expected red state. (Under `-tags integration` the package also compile-fails
// on links_test.go's use of google.ExtractLinks until links.go lands.)
//
// CROSS-SUITE DISCIPLINE (the mutual-cleanup pact; `make integration` runs -p 1):
// this suite seeds provider='google' rows under 'itest-glinks-%' and asserts
// NOTHING globally — every count is scoped to its own account. It cleans up at
// start and end, in FK order. Its inbound messages are visible to triage's and
// classify's global pending filters, hence the scoped-only assertions.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	glEmail   = "itest-glinks-a@example.com"
	glOther   = "itest-glinks-other@example.com"
	glRawAlt  = "imap:INBOX:71:1001" // the multipart/alternative notice
	glRawMany = "imap:INBOX:71:1002" // the 30-anchor marketing mail
)

// glLink is the stored element shape: {"text": "...", "url": "..."}, with the
// array POSITION as the identity — no id, no ordinal column, and nothing may
// reorder it after it is written.
type glLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

func cleanupGmailLinks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-glinks-%')`
	stmts := []string{
		`DELETE FROM capture_decisions WHERE message_id IN (SELECT id FROM normalized_messages
		   WHERE raw_source_item_id IN (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `))`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-glinks-%'`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-glinks:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-glinks-%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// glMultipartRaw is the production shape this ticket exists for: a templated
// notice shipping BOTH text/plain and an HTML alternative with anchors.
func glMultipartRaw() []byte {
	body := strings.Join([]string{
		"--lp42",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"Your account statement is ready.",
		"--lp42",
		`Content-Type: text/html; charset="utf-8"`,
		"",
		lpHTMLAlternative,
		"--lp42--",
		"",
	}, "\r\n")
	return rfc822([]string{
		`Message-ID: <itest-glinks-alt@acme.example>`,
		`From: notices@pinespropertymanagement.example`,
		`To: ` + glEmail,
		`Subject: Your account statement is ready`,
		`Date: Sat, 11 Jul 2026 10:00:00 +0000`,
		`MIME-Version: 1.0`,
		`Content-Type: multipart/alternative; boundary="lp42"`,
	}, body)
}

func glMarketingRaw() []byte {
	return rfc822([]string{
		`Message-ID: <itest-glinks-mkt@acme.example>`,
		`From: deals@shop.example`,
		`To: ` + glEmail,
		`Subject: September deals`,
		`Date: Sat, 11 Jul 2026 11:00:00 +0000`,
		`Content-Type: text/html; charset="utf-8"`,
	}, lpMarketing30())
}

// glReadLinks reads the stored array for one raw item's message.
func glReadLinks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, externalID string) []glLink {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(ctx,
		`SELECT nm.links FROM normalized_messages nm
		   JOIN raw_source_items r ON r.id = nm.raw_source_item_id
		  WHERE r.external_id = $1`, externalID).Scan(&raw)
	if err != nil {
		t.Fatalf("read normalized_messages.links for %s: %v\n\nCriterion 12 adds the column in "+
			"migrations/0017_normalized_message_links.sql:\n"+
			"  ALTER TABLE normalized_messages ADD COLUMN links JSONB NOT NULL DEFAULT '[]'::jsonb;\n"+
			"  ALTER TABLE normalized_messages ADD CONSTRAINT normalized_messages_links_is_array\n"+
			"    CHECK (jsonb_typeof(links) = 'array');", externalID, err)
	}
	var out []glLink
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("links for %s is not an array of {text,url}: %v (%s)", externalID, err, raw)
	}
	return out
}

func TestGmailLinks_Integration_RoundTripAndReNormalize(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupGmailLinks(t, ctx, pool)
	defer cleanupGmailLinks(t, ctx, pool)

	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ('google', $1, false) RETURNING id`, glEmail).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	seedRaw := func(externalID string, msg []byte, hash string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
			 VALUES ($1,$2,$3,$4)`, acctID, externalID,
			newIMAPEnvelope(imapINBOX, 71, uint32(len(externalID)), imapInternalDate, nil, msg, false),
			hash); err != nil {
			t.Fatalf("seed raw %s: %v", externalID, err)
		}
	}
	seedRaw(glRawAlt, glMultipartRaw(), "itest-glinks-h-alt")
	seedRaw(glRawMany, glMarketingRaw(), "itest-glinks-h-mkt")

	if _, err := google.Normalize(ctx, google.NewPGSink(pool), google.Config{}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	// ---- criterion 11 + 13: the array is written, in document order ----------
	got := glReadLinks(t, ctx, pool, glRawAlt)
	want := []glLink{
		{Text: "VIEW ACCOUNT", URL: "https://portal.pinespropertymanagement.com/account/12345"},
		{Text: "PAY NOW", URL: "https://portal.pinespropertymanagement.com/pay"},
	}
	if len(got) != len(want) {
		t.Fatalf("stored links = %+v, want %+v. The message is multipart/alternative: extractBodyText "+
			"discards its HTML part, so an extractor wired to body_text stores nothing here", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stored links[%d] = %+v, want %+v. The ARRAY POSITION IS THE IDENTITY (criterion 12): "+
				"there is no id and no ordinal column, the model answers with a 1-based index into this "+
				"array, and anything that reorders it after write resolves a different URL from the same "+
				"answer — silently", i, got[i], want[i])
		}
	}

	// The cap survives the round trip (criterion 6). Production check is the
	// same shape: `max(jsonb_array_length(links))` must be <= 8 after the backfill.
	if n := len(glReadLinks(t, ctx, pool, glRawMany)); n != 8 {
		t.Errorf("the 30-anchor marketing mail stored %d links, want 8 (criterion 6's cap, enforced by the "+
			"extractor and visible here as the column's contents)", n)
	}

	// ---- criterion 13: re-normalize is idempotent ----------------------------
	var msgIDBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT nm.id FROM normalized_messages nm JOIN raw_source_items r ON r.id = nm.raw_source_item_id
		  WHERE r.external_id = $1`, glRawAlt).Scan(&msgIDBefore); err != nil {
		t.Fatalf("read message id: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE raw_source_items SET normalized_at = NULL WHERE external_id IN ($1,$2)`,
		glRawAlt, glRawMany); err != nil {
		t.Fatalf("reset normalized_at: %v", err)
	}
	if _, err := google.Normalize(ctx, google.NewPGSink(pool), google.Config{}); err != nil {
		t.Fatalf("Normalize (re-run): %v", err)
	}

	var msgIDAfter int64
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(min(nm.id),0) FROM normalized_messages nm
		   JOIN raw_source_items r ON r.id = nm.raw_source_item_id
		  WHERE r.external_id = $1`, glRawAlt).Scan(&rows, &msgIDAfter); err != nil {
		t.Fatalf("read message after re-normalize: %v", err)
	}
	if rows != 1 {
		t.Errorf("re-normalizing produced %d normalized_messages rows for one raw item, want 1. "+
			"upsertMessage's ON CONFLICT (raw_source_item_id) DO UPDATE is what makes the backfill safe", rows)
	}
	if msgIDAfter != msgIDBefore {
		t.Errorf("message id moved from %d to %d across a re-normalize. IDS MUST BE STABLE: "+
			"capture_decisions cascades on message_id, ai_extractions link the verdicts, and "+
			"docs/evals/personal-actionability.jsonl is keyed on it — a re-inserted row silently voids "+
			"the eval set and every recorded verdict", msgIDBefore, msgIDAfter)
	}
	if after := glReadLinks(t, ctx, pool, glRawAlt); len(after) != len(want) || after[0] != want[0] {
		t.Errorf("links after re-normalize = %+v, want %+v (unchanged). links is written in the SAME "+
			"statement as the body (criterion 11), so a row can never carry a fresh body with stale links",
			after, want)
	}
}

// ---- criterion 13: the other connectors keep '[]' and nothing breaks ----------

// Upwork, Jira and slackweb insert normalized_messages without naming `links`.
// NOT NULL + DEFAULT + a CHECK is exactly the sort of column that turns their
// insert into a runtime error — on a CronJob, at 15-minute intervals, with the
// failure visible only in pod logs.
func TestGmailLinks_Integration_OtherConnectorsKeepAnEmptyArray(t *testing.T) {
	requireCompose(t)
	ctx := context.Background()

	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	defer pool.Close()

	cleanupGmailLinks(t, ctx, pool)
	defer cleanupGmailLinks(t, ctx, pool)

	var acctID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ('upwork_crm', $1, false) RETURNING id`, glOther).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	var rawID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,'itest-glinks-upwork-1','{}','itest-glinks-h-upwork') RETURNING id`, acctID).Scan(&rawID); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	var threadID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ('itest-glinks:thread-1','itest upwork thread','[]') RETURNING id`).Scan(&threadID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// EXACTLY the shape upworkcrm/jira/slackweb write: no `links` in the column
	// list. This must succeed, and the row must come back with an empty array.
	var msgID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound','itest-glinks-upwork-msg-1', now(), 'plain upwork chat body', 'sub', 'them', 'upwork_chat')
		 RETURNING id`, rawID, threadID).Scan(&msgID); err != nil {
		t.Fatalf("a connector that does not know about links could not insert a message: %v\n\n"+
			"Criterion 13: upwork / jira / slackweb name no `links` column. The migration's NOT NULL must "+
			"come with DEFAULT '[]'::jsonb, or every non-google connector starts failing on a CronJob", err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT links FROM normalized_messages WHERE id=$1`, msgID).Scan(&raw); err != nil {
		t.Fatalf("read links: %v", err)
	}
	if string(raw) != "[]" {
		t.Errorf("links = %s for a message written by another connector, want []. Their bodies are not HTML "+
			"mail and nothing has asked for links on them (SPEC out-of-scope)", raw)
	}

	// The CHECK is what makes "an array" structural rather than a convention:
	// the model's index is a position, and a position into an object means
	// nothing.
	if _, err := pool.Exec(ctx,
		`UPDATE normalized_messages SET links = '{"a":1}'::jsonb WHERE id=$1`, msgID); err == nil {
		t.Errorf("a JSON OBJECT was accepted into normalized_messages.links. Criterion 12 requires " +
			"CHECK (jsonb_typeof(links) = 'array'): the contract downstream is a 1-based POSITION, and " +
			"nothing that is not an array has positions")
	}
}
