//go:build integration

package classify_test

// SWT-25 (link-preservation) criteria 14, 15, 16 and 22, against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ClassifyLinks ./internal/classify/
//
// THE COLUMN TEST, NOT THE FIXTURE TEST. links_test.go supplies
// PendingMessage.Links itself, so nothing there can show the value ever comes
// out of Postgres — this repo's sixth landmine, shipped twice inside SWT-21:
// drafts' locality guard passed its unit test for weeks while `DeliverTasks`
// never selected the column, so every real task folded to the wrong class.
//
// THE MUTATION THAT MUST TURN THIS RED, named so a reviewer can run it:
// replace `COALESCE(nm.links, '[]')` in `inboxSelect` (internal/classify/store.go)
// with the literal `'[]'` and
// TestClassifyLinks_Integration_TheColumnFeedsTheRenderedPrompt must fail. If it
// stays green, it is proving its own fixture.
//
// THE SEAM. internal/classify does not import internal/connector/google — the
// contract between the extractor and the classifier is the COLUMN, not a Go
// type. Two structs agreeing by inspection is how two spellings of one fact
// drift, so the seam gets a real test: normalize a raw item with the REAL google
// normalizer and read it back through classify.PGStore.PendingMessages
// (criterion 14). Note the direction of the import here — a TEST may cross the
// seam to prove it; the packages may not.
//
// GREENFIELD NOTE: migration 0017, the `links` column and PendingMessage.Links
// do not exist yet, so these fail (and the package compile-fails on
// links_test.go's use of classify.ResolveLink) — the expected red state.
//
// CROSS-SUITE DISCIPLINE: this file reuses store_integration_test.go's corpus
// (newCISuite / ciCleanup, scoped to 'itest-classify-%' and model
// 'itest-classify-model') and adds ONE provider='google' account of its own,
// 'itest-clinks-%', which it cleans up itself in FK order at start and end.
// `make integration` runs -p 1 for exactly this reason.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/provider"
)

const (
	clSeamEmail = "itest-clinks-seam@example.com"
	clSeamRawID = "imap:INBOX:73:2001"
)

// clLinks is the JSONB payload seeded directly into the column: the element
// shape is {"text","url"} and the ARRAY POSITION is the identity.
const clLinks = `[{"text":"VIEW DETAILS","url":"https://portal.pinespropertymanagement.com/violations/887"},` +
	`{"text":"PAY NOW","url":"https://portal.pinespropertymanagement.com/pay"}]`

func clCleanupSeam(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE account_email LIKE 'itest-clinks-%')`
	const msgs = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN
	                (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `))`
	stmts := []string{
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgs,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN
		   (SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-clinks-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE account_email LIKE 'itest-clinks-%'`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

// clSeedLinkedMessage adds one inbound message WITH links to the ci corpus,
// attributed to its local_only project so the classify inbox picks it up.
func clSeedLinkedMessage(t *testing.T, ctx context.Context, c *ciCorpus) int64 {
	t.Helper()
	var acct int64
	if err := c.pool.QueryRow(ctx,
		`SELECT id FROM source_accounts WHERE provider=$1`, ciProvider).Scan(&acct); err != nil {
		t.Fatalf("read suite account: %v", err)
	}
	var rawID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'itest-classify-linked','{}','itest-classify-h-linked', now()) RETURNING id`,
		acct).Scan(&rawID); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	var threadID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants)
		 VALUES ('itest-classify:linked','Violation notice','[]') RETURNING id`).Scan(&threadID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	var msgID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		    body_text, subject, sender, channel, links)
		 VALUES ($1,$2,'inbound','itest-classify-linked', now() - interval '5 minutes',
		         'A violation was recorded on your property. Cure by 2026-09-15.',
		         '[#XN123456] Message from Pines Association',
		         'notices@pinespropertymanagement.example','gmail',$3::jsonb)
		 RETURNING id`, rawID, threadID, clLinks).Scan(&msgID); err != nil {
		t.Fatalf("seed a message WITH links: %v\n\nCriterion 12 adds the column in "+
			"migrations/0017_normalized_message_links.sql (JSONB NOT NULL DEFAULT '[]'::jsonb, "+
			"CHECK jsonb_typeof(links)='array')", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow','attributed',$2,'itest-classify: personal rule')`,
		msgID, c.localProject); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	return msgID
}

// ---- criteria 15 + 16: the COLUMN reaches the prompt --------------------------

func TestClassifyLinks_Integration_TheColumnFeedsTheRenderedPrompt(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)
	msgID := clSeedLinkedMessage(t, ctx, c)

	// (a) the reader: inboxSelect must SELECT the column and the scan must land
	//     it on PendingMessage.Links. Criterion 15 puts it in the ONE constant
	//     shared by PendingMessages and MessagesByID, so `eval` scores what `run`
	//     classifies.
	got := ciPending(t, ctx, c, classify.LanePersonal)
	m, ok := got[msgID]
	if !ok {
		t.Fatalf("the seeded linked message %d is not in the inbox at all", msgID)
	}
	if len(m.Links) != 2 {
		t.Fatalf("PendingMessage.Links = %+v, want 2 entries read from normalized_messages.links. "+
			"THIS IS THE COLUMN TEST: if you just replaced COALESCE(nm.links,'[]') in inboxSelect with a "+
			"literal '[]', this is the assertion that is supposed to go red", m.Links)
	}
	if m.Links[0].Text != "VIEW DETAILS" ||
		m.Links[0].URL != "https://portal.pinespropertymanagement.com/violations/887" {
		t.Errorf("Links[0] = %+v, want {VIEW DETAILS, …/violations/887} — the array POSITION is the "+
			"identity and the scan must not reorder it", m.Links[0])
	}
	if m.Links[1].Text != "PAY NOW" {
		t.Errorf("Links[1] = %+v, want PAY NOW second", m.Links[1])
	}

	// (b) MessagesByID sees the same thing, because it shares inboxSelect. A
	//     second spelling here is how `eval` and `run` start scoring different
	//     prompts.
	byID, err := classify.NewStore(c.pool).MessagesByID(ctx,
		classify.Config{Lane: classify.LanePersonal}, []int64{msgID})
	if err != nil {
		t.Fatalf("MessagesByID: %v", err)
	}
	if len(byID) != 1 || len(byID[0].Links) != 2 {
		t.Errorf("MessagesByID returned %+v; the eval path must read links from the SAME constant "+
			"(criterion 15), or a re-run of the SWT-22 eval measures a different prompt than the one "+
			"`classify run` uses", byID)
	}

	// (c) the prompt: the anchor TEXTS reach the model, sourced from the column.
	local := cfLocal()
	if _, err := classify.Run(ctx, classify.NewStore(c.pool),
		provider.NewRouter(nil, local, time.Minute), classify.Config{Model: ciModel, MaxTokens: 512, Lane: classify.LanePersonal}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if local.calls == 0 {
		t.Fatalf("control failed: the local client was never called, so 'the prompt carries the texts' " +
			"below could be satisfied by a worker that classified nothing")
	}
	var carried bool
	for _, req := range local.requests {
		if strings.Contains(req.User, "VIEW DETAILS") && strings.Contains(req.User, "PAY NOW") {
			carried = true
			if strings.Contains(req.User, "https://portal.pinespropertymanagement.com/") {
				t.Errorf("the rendered prompt carries a URL:\n%s\nCriterion 17 renders anchor TEXTS ONLY", req.User)
			}
		}
	}
	if !carried {
		t.Errorf("no rendered prompt in this pass carried the anchor texts from the column. The candidate "+
			"list is fed by nm.links: a unit test that supplies Links proves its own fixture, so this is "+
			"the assertion that has to hold. Requests seen: %d", len(local.requests))
	}
}

// ---- criterion 14: the seam, google.Normalize -> classify.PGStore -------------

func TestClassifyLinks_Integration_SeamFromTheGoogleNormalizer(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)
	clCleanupSeam(t, ctx, c.pool)
	t.Cleanup(func() { clCleanupSeam(t, ctx, c.pool) })

	var acctID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ('google', $1, false) RETURNING id`, clSeamEmail).Scan(&acctID); err != nil {
		t.Fatalf("seed google account: %v", err)
	}

	// A raw IMAP envelope in the production shape: multipart/alternative, plain
	// text plus an HTML alternative carrying two anchors and an unsubscribe link
	// the extractor must drop.
	msg := strings.Join([]string{
		"Message-ID: <itest-clinks-seam@acme.example>",
		"From: notices@pinespropertymanagement.example",
		"To: " + clSeamEmail,
		"Subject: Violation notice",
		"Date: Sat, 11 Jul 2026 10:00:00 +0000",
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="cl42"`,
		"",
		"--cl42",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"A violation was recorded on your property.",
		"--cl42",
		`Content-Type: text/html; charset="utf-8"`,
		"",
		`<html><body><p>A violation was recorded on your property.</p>` +
			`<p><a href="https://portal.pinespropertymanagement.com/violations/887">VIEW DETAILS</a></p>` +
			`<p><a href="https://portal.pinespropertymanagement.com/pay">PAY NOW</a></p>` +
			`<p><a href="https://lists.example.com/unsubscribe?u=9">Unsubscribe</a></p>` +
			`</body></html>`,
		"--cl42--",
		"",
	}, "\r\n")

	env, err := json.Marshal(map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 73, "uid": 2001,
		"internaldate": "2026-07-11T09:59:00Z", "flags": []string{}, "size": len(msg),
		"truncated": false, "rfc822_b64": encodeB64(msg),
	})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,$3,'itest-clinks-h-seam')`, acctID, clSeamRawID, env); err != nil {
		t.Fatalf("seed raw item: %v", err)
	}

	// The REAL normalizer, not a fixture: this is the half of the seam that
	// writes.
	if _, err := google.Normalize(ctx, google.NewPGSink(c.pool), google.Config{}); err != nil {
		t.Fatalf("google.Normalize: %v", err)
	}

	var msgID int64
	if err := c.pool.QueryRow(ctx,
		`SELECT nm.id FROM normalized_messages nm JOIN raw_source_items r ON r.id = nm.raw_source_item_id
		  WHERE r.external_id = $1`, clSeamRawID).Scan(&msgID); err != nil {
		t.Fatalf("read normalized message: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow','attributed',$2,'itest-clinks: personal rule')`,
		msgID, c.localProject); err != nil {
		t.Fatalf("seed decision: %v", err)
	}

	// The half that reads. Different package, different struct, one column.
	rows, err := classify.NewStore(c.pool).MessagesByID(ctx,
		classify.Config{Lane: classify.LanePersonal}, []int64{msgID})
	if err != nil {
		t.Fatalf("MessagesByID: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("MessagesByID returned %d rows, want 1", len(rows))
	}
	links := rows[0].Links
	want := []struct{ text, url string }{
		{"VIEW DETAILS", "https://portal.pinespropertymanagement.com/violations/887"},
		{"PAY NOW", "https://portal.pinespropertymanagement.com/pay"},
	}
	if len(links) != len(want) {
		t.Fatalf("classify read %+v from a message the google normalizer wrote, want %v. THE SEAM: the "+
			"connector writes normalized_messages.links and the classifier scans it — the two packages "+
			"share no Go type on purpose, so only this test can show they agree. (The Unsubscribe anchor "+
			"is dropped by the extractor, which is why two survive and not three.)", links, want)
	}
	for i, w := range want {
		if links[i].Text != w.text || links[i].URL != w.url {
			t.Errorf("links[%d] = %+v, want {%s %s} — position is the identity, and an index resolved "+
				"against a reordered array is a plausible, silently wrong URL", i, links[i], w.text, w.url)
		}
	}
}

// ---- criterion 22: the report prints the resolved URL -------------------------

func TestClassifyLinks_Integration_ReportPrintsTheResolvedURL(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)

	var runID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, status) VALUES ('classify','ollama',$1,'ok') RETURNING id`,
		ciModel).Scan(&runID); err != nil {
		t.Fatalf("seed ai_run: %v", err)
	}
	ext := func(fields string) {
		t.Helper()
		if _, err := c.pool.Exec(ctx,
			`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,$3::jsonb)`,
			runID, c.localRaw, fields); err != nil {
			t.Fatalf("seed extraction: %v", err)
		}
	}
	// The four states of criterion 21, as the report will meet them.
	ext(`{"actionable":true,"kind":"payment_due","title":"itest verdict alpha","reason":"r",
	      "sender":"notices@pines.example","subject":"Violation notice","normalized_message_id":901,
	      "link_candidates":2,"link_index":1,"link_url":"https://portal.pines.example/violations/887",
	      "link_text":"VIEW DETAILS"}`)
	ext(`{"actionable":true,"kind":"deadline","title":"itest verdict bravo","reason":"r",
	      "sender":"notices@pines.example","subject":"First notice","normalized_message_id":902,
	      "link_candidates":0}`)
	ext(`{"actionable":true,"kind":"deadline","title":"itest verdict charlie","reason":"r",
	      "sender":"notices@pines.example","subject":"Second notice","normalized_message_id":903,
	      "link_candidates":3,"link_index":null}`)
	ext(`{"actionable":true,"kind":"deadline","title":"itest verdict delta","reason":"r",
	      "sender":"notices@pines.example","subject":"Third notice","normalized_message_id":904,
	      "link_candidates":3,"link_index_rejected":99}`)

	var out bytes.Buffer
	if err := classify.Report(ctx, c.pool, &out, time.Hour); err != nil {
		t.Fatalf("Report: %v", err)
	}
	text := out.String()

	linked := clLineContaining(text, "itest verdict alpha")
	if linked == "" {
		t.Fatalf("the flagged verdict does not appear in the report at all:\n%s", text)
	}
	if !strings.Contains(linked, "https://portal.pines.example/violations/887") {
		t.Errorf("the flagged line does not print the resolved URL:\n%s\n\nCriterion 22: `classify report` "+
			"prints the link on each flagged line. That IS the usable-alone claim of this ticket — a "+
			"flagged HOA or bank notice has to be actionable from the report instead of sending the reader "+
			"back to the mailbox", linked)
	}

	unlinked := clLineContaining(text, "itest verdict bravo")
	if unlinked == "" {
		t.Fatalf("the flagged verdict with no link does not appear in the report:\n%s", text)
	}
	if !regexp.MustCompile(`(?i)(—|--|\(none\)|\bnone\b|no link|n/a|-\s*$)`).MatchString(unlinked) {
		t.Errorf("the flagged line with NO link prints an empty column:\n%q\n\nCriterion 22 wants a "+
			"placeholder: an empty column reads as a rendering bug, and no-candidates is the COMMON case — "+
			"the reader has to be able to tell 'there was no link' from 'the report lost it'", unlinked)
	}

	// One counts line covering the four states (criterion 21's table). A counter
	// that cannot separate them is an alarm nobody can read.
	//
	// The verdict LINES are removed before this scan, and that is not fussiness:
	// the first version of this test searched the whole report for "link" and
	// "reject" and passed because the seeded TITLES contained those words. A
	// scan that its own fixture satisfies is the landmine this repo keeps
	// meeting; the summary has to say it, not the data.
	summary := clWithoutVerdictLines(text)
	lower := strings.ToLower(summary)
	if !strings.Contains(lower, "link") {
		t.Errorf("the report's SUMMARY never mentions links — criterion 22 adds one counts line covering "+
			"the four states of criterion 21. Summary:\n%s", summary)
	}
	if !strings.Contains(lower, "reject") {
		t.Errorf("the report's summary has no rejected count. That is the state that says the MODEL is "+
			"answering nonsense; folded into 'not linked' it is invisible, and since link_index:null is the "+
			"common case nobody would ever notice. Summary:\n%s", summary)
	}
	if !regexp.MustCompile(`(?i)candidat|none|no link|not chosen|declin`).MatchString(lower) {
		t.Errorf("the report's summary cannot distinguish 'nothing was offered' from 'the model declined'. "+
			"Criterion 21's four states exist for the same reason SWT-22's skip reasons do: a counter that "+
			"merges them is an alarm nobody can read. Summary:\n%s", summary)
	}

	// The SWT-22 note must survive this change untouched.
	if strings.Contains(text, "classified:") && !strings.Contains(lower, "flagged") {
		t.Errorf("the report lost its flagged summary line:\n%s", text)
	}
}

// encodeB64 is the rfc822_b64 encoding the ingest phase writes: RFC822 is
// 8-bit-clean and would not survive UTF-8 validation as a JSON string.
func encodeB64(msg string) string {
	return base64.StdEncoding.EncodeToString([]byte(msg))
}

// clWithoutVerdictLines drops the per-verdict lines this test seeded, so the
// counts assertions cannot be satisfied by the fixture's own text.
func clWithoutVerdictLines(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "itest verdict ") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// clLineContaining returns the first line of s containing needle, or "".
func clLineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
