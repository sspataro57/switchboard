//go:build integration

package tools_test

// SWT-42 (docs/tickets/mail-attachments_SPEC.md) criteria 1-3, 5 and 11-19
// against a real database: mail_list_attachments and mail_read_attachment
// through executor.Execute — the only route to a handler (invariant 3) — with
// the production policy matrix (queueMatrixExecutor), and through real
// mcpserver ProfileUser / ProfileFull servers for criterion 16.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run MailAttach ./internal/tools/
//
// COMPOSE DB ONLY. mailAttachPool refuses anything but localhost:5433 /
// 127.0.0.1:5433 and never touches 192.168.50.49: this suite seeds and deletes
// mail fixtures.
//
// POSTGRES PRODUCES EVERY CLASS INPUT (IK "test the column, not the fixture"):
// the latest capture_decisions row per message, projects.ai_locality, the raw
// row's source_account_id (the receiving mailbox, O2) and the thread's inbound
// members are all rows here, never Go values handed to the handler. The
// fixtures are shaped like production rather than like the assertion:
//   - outbound messages carry NO decision (IK landmine 7: capture filters
//     direction='inbound');
//   - (a)'s older decision is unmatched and its latest is a LIVE attribution;
//     (b)'s older decision is `any` and its latest `local_only` — the latest
//     row of ANY mode decides (ORDER BY id DESC LIMIT 1);
//   - the 19-filing mailbox also holds one message whose OLDER decision is an
//     `any` filing and whose latest is unmatched, so counting every decision
//     instead of the latest per message reaches 20 and turns (j) red.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (criterion 17, O2):
//   - `p.ai_locality = 'local_only'` → `false` in the class SELECT: (b) allowed.
//   - drop the outbound fold (outbound as its own unseen): (e) refused. (e)
//     sits on mailbox A, which is NOT clean, so O2 cannot rescue it.
//   - drop the mailbox rule's local-only clause: (i) allowed.
//   - mailboxCleanMinFiled → 0: (j) allowed.
//
// CLEANUP (IK "Task verbs over MCP" audit-FK landmine, and the mutual-cleanup
// pact): everything is scoped by the itest-mailattach- prefix. Audit rows are
// found by tool AND (actor, or args naming a fixture: an itest-mailattach
// string, a fixture raw id, or a fixture thread id) — that catches the
// mcp:manual:salvo rows the user-profile server writes and the drafts:gpt
// corpus row. policy_decisions go first, then audit_events, then the mail rows
// in FK order. Runs before AND after (t.Cleanup); run the suite twice.
//
// XDG_CACHE_HOME is a t.TempDir() for every test (criterion 8), so no file ever
// lands in the real ~/.cache.
//
// GREENFIELD NOTE — EXPECTED RED: the internal/tools test binary does not
// compile until mailattach.go exists (mailattach_test.go's helpers). With the
// helpers in place but the tools unregistered, every call here fails with
// `unknown tool "mail_list_attachments"`; the ProfileUser calls fail with
// `tool "mail_list_attachments" is not available over MCP`.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/mcpserver"
)

const (
	maAcctA     = "itest-mailattach-a@example.com"        // (a)-(g): NOT clean (a local_only filing, and < 20 filings)
	maAcctClean = "itest-mailattach-clean@example.com"    // O2 (h)/(k): 26 `any` filings, none local_only
	maAcctDirty = "itest-mailattach-dirty@example.com"    // O2 (i): 20 `any` filings + ONE local_only
	maAcct19    = "itest-mailattach-nineteen@example.com" // O2 (j): 19 `any` filings
	maSlugAny   = "itest-mailattach-any"
	maSlugLocal = "itest-mailattach-local"

	maHuman       = "mcp:manual:itest-mailattach-salvo"
	maSana        = "Sana Maryam <sana.itest-mailattach@collab.example>"
	maMainSubject = "Activities Integration – Request and Response Validation (itest-mailattach)"
	maPrivSubject = "itest-mailattach PRIVATE subject line"
	maMarker      = "itest-mailattach-MARKER-5b1e9c"  // appears only inside Request.json
	maBodyMarker  = "itest-mailattach-BODY-only-9f2d" // appears only in a body_text

	maTruncReason = "not stored: message was over the 1 MiB capture cap (MAIL_MAX_MESSAGE_BYTES)"
	maGmailReason = "this message came through the Gmail API/bridge path, which stores no attachment bytes"

	maList = "mail_list_attachments"
	maRead = "mail_read_attachment"
)

// ---- pool, guard, cleanup ----------------------------------------------------------

func mailAttachPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	guardRealDB(t)
	if !strings.Contains(url, "@localhost:5433/") && !strings.Contains(url, "@127.0.0.1:5433/") {
		t.Fatal("this suite runs on the compose db ONLY (postgres://ops:ops@localhost:5433/ops): it seeds and " +
			"deletes mail fixtures")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MAIL_MAX_MESSAGE_BYTES", "") // the default 1 MiB cap
	pool := newToolsPool(t, ctx)
	cleanupMailAttach(t, ctx, pool)
	t.Cleanup(func() {
		cleanupMailAttach(t, ctx, pool)
		pool.Close()
	})
	return pool
}

func cleanupMailAttach(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const accts = `(SELECT id FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-mailattach-%')`
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN ` + accts + `)`
	const ours = `(SELECT id FROM audit_events
	    WHERE tool IN ('mail_list_attachments','mail_read_attachment')
	      AND (actor ILIKE '%itest-mailattach%'
	           OR args::text ILIKE '%itest-mailattach%'
	           OR args->>'raw_source_item_id' IN (SELECT id::text FROM raw_source_items WHERE source_account_id IN ` + accts + `)
	           OR args->>'thread_id' IN (SELECT id::text FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-mailattach-%')))`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + ours,
		`DELETE FROM audit_events WHERE id IN ` + ours,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM capture_decisions WHERE raw_source_item_id IN ` + raws +
			` OR message_id IN (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)` +
			` OR project_id IN (SELECT id FROM projects WHERE slug LIKE 'itest-mailattach-%')`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'gmail:itest-mailattach-%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN ` + accts,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + accts,
		`DELETE FROM source_accounts WHERE provider='google' AND account_email LIKE 'itest-mailattach-%'`,
		`DELETE FROM projects WHERE slug LIKE 'itest-mailattach-%'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("mailattach cleanup %q: %v", q, err)
		}
	}
}

// ---- MIME ------------------------------------------------------------------------------

func maLeaf(headers []string, body string) string {
	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body
}

func maMulti(mediaType, boundary string, parts ...string) string {
	var b strings.Builder
	b.WriteString("Content-Type: " + mediaType + `; boundary="` + boundary + `"` + "\r\n\r\n")
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n" + p + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

func maB64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for len(s) > 76 {
		out.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	out.WriteString(s)
	return out.String()
}

func maAttach(mediaType, filename string, data []byte) string {
	return maLeaf([]string{
		"Content-Type: " + mediaType + `; name="` + filename + `"`,
		`Content-Disposition: attachment; filename="` + filename + `"`,
		"Content-Transfer-Encoding: base64",
	}, maB64(data))
}

// maWith is a multipart/mixed entity: a text/plain body, then attachments.
func maWith(body string, atts ...string) string {
	parts := append([]string{maLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, body)}, atts...)
	return maMulti("multipart/mixed", "itest-mailattach-mix", parts...)
}

func maRFC822(from, subject, mid string, at time.Time, entity string) string {
	return strings.Join([]string{
		"From: " + from,
		"To: " + maAcctA,
		"Subject: " + subject,
		"Message-ID: " + mid,
		"Date: " + at.Format(time.RFC1123Z),
		"MIME-Version: 1.0",
	}, "\r\n") + "\r\n" + entity
}

func maEnvelope(t *testing.T, uid int, msg string, truncated bool, parts []map[string]any) json.RawMessage {
	t.Helper()
	size := len(msg)
	if truncated {
		size = 3_000_000
	}
	env := map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 42, "uid": uid,
		"internaldate": "2026-09-10T22:06:00Z", "flags": []string{}, "size": size,
		"truncated": truncated, "rfc822_b64": base64.StdEncoding.EncodeToString([]byte(msg)),
	}
	if parts != nil {
		env["parts"] = parts
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// The Gmail API's format=full resource — also the bridge's shape. No bytes.
var maGmailRaw = json.RawMessage(`{"id":"18c0ffee","threadId":"18c0ffee","internalDate":"1789077960000",
 "payload":{"mimeType":"multipart/mixed","headers":[{"name":"Subject","value":"itest-mailattach gmail"}],
  "parts":[{"partId":"0","mimeType":"text/plain","filename":"","body":{"size":5,"data":"aGVsbG8"}},
           {"partId":"1","mimeType":"application/pdf","filename":"a.pdf","body":{"attachmentId":"ANGjdJ8","size":1234}}]}}`)

func maRequestJSON() []byte {
	return []byte("{\"request\":\"" + maMarker + "\",\n \"activities\":[{\"id\":1,\"kind\":\"volunteer\"},{\"id\":2}]}\n")
}

func maResponseJSON() []byte { return []byte(`{"pad":"` + strings.Repeat("x", 84274-10) + `"}`) }

func maBigCSV() []byte {
	var b bytes.Buffer
	b.WriteString("id,name,city\n")
	for i := 0; b.Len() < 150*1024; i++ {
		fmt.Fprintf(&b, "%d,name-%d,city-%d\n", i, i, i)
	}
	return b.Bytes()[:150*1024]
}

// ---- fixtures ---------------------------------------------------------------------------

type maMsg struct {
	raw, id, thread int64
	mid             string
}

type maFixture struct {
	anyProj, localProj int64
	accounts           []int64

	// (a)-(g) on mailbox A, which is NOT clean.
	main, out, local, unmatched, unseen, outAlone, g1, g2, g3 maMsg
	// O2.
	h, hUnseen, k, outAloneClean, outNoThread, i, j maMsg
	// finder, reading, unavailable bytes.
	sanaOld, sanaNoAtt, slackSana, bag, dup, trunc, gmail maMsg
	loserRaw                                              int64

	t1Key, t6Key string
	t1ID, t6ID   int64

	request, response, getAll, bigCSV, pdf []byte
}

type maSeed struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	uid  int
}

func (s *maSeed) id(sql string, args ...any) int64 {
	s.t.Helper()
	var id int64
	if err := s.pool.QueryRow(s.ctx, sql, args...).Scan(&id); err != nil {
		s.t.Fatalf("seed %q: %v", sql, err)
	}
	return id
}

func (s *maSeed) account(email string) int64 {
	return s.id(`INSERT INTO source_accounts (provider, account_email, send_enabled) VALUES ('google',$1,false) RETURNING id`, email)
}

func (s *maSeed) project(slug, locality string) int64 {
	return s.id(`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
	             VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest',$3) RETURNING id`, slug, slug+"-client", locality)
}

func (s *maSeed) thread(key string) int64 {
	return s.id(`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,'itest-mailattach','[]') RETURNING id`, key)
}

type maSpec struct {
	acct                 int64
	extID                string // "" = imap:INBOX:42:<uid>
	raw                  json.RawMessage
	thread               int64 // 0 = NULL
	direction            string
	mid, subject, sender string
	at                   time.Time
	channel              string // "" = gmail
	body                 string
}

func (s *maSeed) message(sp maSpec) maMsg {
	s.t.Helper()
	s.uid++
	ext := sp.extID
	if ext == "" {
		ext = fmt.Sprintf("imap:INBOX:42:%d", s.uid)
	}
	channel := sp.channel
	if channel == "" {
		channel = "gmail"
	}
	rawID := s.id(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	               VALUES ($1,$2,$3,$4,now()) RETURNING id`, sp.acct, ext, sp.raw, "itest-mailattach-h-"+ext)
	var thread any
	if sp.thread != 0 {
		thread = sp.thread
	}
	msgID := s.id(`INSERT INTO normalized_messages
	                 (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
	               VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		rawID, thread, sp.direction, sp.mid, sp.at, sp.body, sp.subject, sp.sender, channel)
	return maMsg{raw: rawID, id: msgID, thread: sp.thread, mid: sp.mid}
}

// decide appends a capture_decisions row; project 0 = unmatched.
func (s *maSeed) decide(m maMsg, mode string, project int64) {
	s.t.Helper()
	var err error
	if project == 0 {
		_, err = s.pool.Exec(s.ctx, `INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, action, reason)
		                             VALUES ($1,$2,$3,'unmatched','itest-mailattach')`, m.id, m.raw, mode)
	} else {
		_, err = s.pool.Exec(s.ctx, `INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, action, project_id, reason)
		                             VALUES ($1,$2,$3,'attributed',$4,'itest-mailattach')`, m.id, m.raw, mode, project)
	}
	if err != nil {
		s.t.Fatalf("seed decision for %s: %v", m.mid, err)
	}
}

func maKey(acct, root string) string { return "gmail:" + acct + ":" + root }

func seedMailAttach(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *maFixture {
	t.Helper()
	s := &maSeed{t: t, ctx: ctx, pool: pool}
	fx := &maFixture{
		request: maRequestJSON(), response: maResponseJSON(),
		getAll: []byte(`[{"id":1,"state":"open"},{"id":2,"state":"closed"}]`),
		bigCSV: maBigCSV(), pdf: []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n%%EOF\n"),
	}
	fx.anyProj = s.project(maSlugAny, "any")
	fx.localProj = s.project(maSlugLocal, "local_only")
	a, clean, dirty, nineteen := s.account(maAcctA), s.account(maAcctClean), s.account(maAcctDirty), s.account(maAcct19)
	fx.accounts = []int64{a, clean, dirty, nineteen}
	base := time.Date(2026, 9, 10, 22, 6, 0, 0, time.UTC)

	mail := func(acct, thread int64, dir, mid, subject, sender string, at time.Time, entity, body string) maMsg {
		s.uid++
		uid := s.uid
		return s.message(maSpec{acct: acct, extID: fmt.Sprintf("imap:INBOX:42:%d", 100000+uid),
			raw: maEnvelope(t, uid, maRFC822(sender, subject, mid, at, entity), false, nil), thread: thread,
			direction: dir, mid: mid, subject: subject, sender: sender, at: at, body: body})
	}
	one := func(name string) string {
		return maWith("one attachment", maAttach("text/plain", name, []byte("content of "+name)))
	}

	// ---- thread T1: (a) and (e) --------------------------------------------------------
	mainMID := "<itest-mailattach-main@collab.example>"
	fx.t1Key = maKey(maAcctA, mainMID)
	fx.t1ID = s.thread(fx.t1Key)
	mainEntity := maMulti("multipart/mixed", "itest-mailattach-outer",
		maMulti("multipart/alternative", "itest-mailattach-alt",
			maLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "Hi Salvador, request and responses attached. "+maBodyMarker),
			maLeaf([]string{`Content-Type: text/html; charset="utf-8"`}, "<p>Hi Salvador, request and responses attached.</p>"),
		),
		maAttach("application/octet-stream", "Request.json", fx.request),
		maAttach("application/json", "Response.json", fx.response),
		maAttach("application/octet-stream", "GetAll-Response.json", fx.getAll),
	)
	fx.main = mail(a, fx.t1ID, "inbound", mainMID, maMainSubject, maSana, base, mainEntity,
		"Hi Salvador, request and responses attached. "+maBodyMarker)
	s.decide(fx.main, "shadow", 0)        // older: unmatched
	s.decide(fx.main, "live", fx.anyProj) // latest (a different mode): filed under an `any` project
	fx.out = mail(a, fx.t1ID, "outbound", "<itest-mailattach-out@example.com>", "Re: "+maMainSubject, maAcctA,
		base.Add(time.Hour), maWith("Thanks, notes attached.", maAttach("text/plain", "notes.txt", []byte("reply notes"))), "Thanks")

	// ---- (b) (c) (d) (f) on mailbox A ------------------------------------------------------
	fx.local = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-local@collab.example>")), "inbound",
		"<itest-mailattach-local@collab.example>", maPrivSubject, maSana, base.Add(-2*time.Hour),
		maWith("private", maAttach("application/json", "private.json", []byte(`{"private":true}`))), "private body")
	s.decide(fx.local, "shadow", fx.anyProj)   // older: `any`
	s.decide(fx.local, "shadow", fx.localProj) // latest: local_only
	fx.unmatched = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-c@vendor.example>")), "inbound",
		"<itest-mailattach-c@vendor.example>", "itest-mailattach (c) unmatched", "Vendor <vendor.itest-mailattach@vendor.example>",
		base.Add(-3*time.Hour), one("c.txt"), "c")
	s.decide(fx.unmatched, "shadow", 0)
	fx.unseen = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-d@vendor.example>")), "inbound",
		"<itest-mailattach-d@vendor.example>", "itest-mailattach (d) unseen", "Vendor <vendor.itest-mailattach@vendor.example>",
		base.Add(-4*time.Hour), one("d.txt"), "d")
	fx.outAlone = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-f@example.com>")), "outbound",
		"<itest-mailattach-f@example.com>", "itest-mailattach (f) outbound alone", maAcctA, base.Add(-5*time.Hour), one("f.txt"), "f")

	// ---- (g): T6 holds an `any` inbound, a local_only inbound and an outbound ---------------
	fx.t6Key = maKey(maAcctA, "<itest-mailattach-g@collab.example>")
	fx.t6ID = s.thread(fx.t6Key)
	fx.g1 = mail(a, fx.t6ID, "inbound", "<itest-mailattach-g@collab.example>", "itest-mailattach (g)",
		"G <g.itest-mailattach@collab.example>", base.Add(-10*time.Hour), one("g1.txt"), "g1")
	s.decide(fx.g1, "shadow", fx.anyProj)
	fx.g2 = mail(a, fx.t6ID, "inbound", "<itest-mailattach-g2@collab.example>", "Re: itest-mailattach (g)",
		"G <g.itest-mailattach@collab.example>", base.Add(-9*time.Hour), one("g2.txt"), "g2")
	s.decide(fx.g2, "shadow", fx.localProj)
	fx.g3 = mail(a, fx.t6ID, "outbound", "<itest-mailattach-g3@example.com>", "Re: itest-mailattach (g)",
		maAcctA, base.Add(-8*time.Hour), one("g3.txt"), "g3")

	// ---- finder and reading fixtures on mailbox A (all filed under `any`) --------------------
	fx.sanaOld = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-old@collab.example>")), "inbound",
		"<itest-mailattach-old@collab.example>", "itest-mailattach older sana", maSana,
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), one("older.csv"), "older")
	s.decide(fx.sanaOld, "shadow", fx.anyProj)
	fx.sanaNoAtt = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-noatt@collab.example>")), "inbound",
		"<itest-mailattach-noatt@collab.example>", "itest-mailattach sana no attachment", maSana,
		time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC),
		maLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "just text"), "just text")
	s.decide(fx.sanaNoAtt, "shadow", fx.anyProj)
	fx.slackSana = mail(a, 0, "inbound", "slack:itest-mailattach:C1:p1", "itest-mailattach slack", maSana,
		time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC), one("slack.txt"), "slack")
	if _, err := pool.Exec(ctx, `UPDATE normalized_messages SET channel='slack' WHERE id=$1`, fx.slackSana.id); err != nil {
		t.Fatalf("slack channel: %v", err)
	}
	s.decide(fx.slackSana, "shadow", fx.anyProj)

	bag := maWith("reports attached",
		maAttach("text/csv", "big.csv", fx.bigCSV),
		maAttach("application/pdf", "report.pdf", fx.pdf),
		maLeaf([]string{
			`Content-Type: text/csv; charset=iso-8859-1; name="latin1.csv"`,
			`Content-Disposition: attachment; filename="latin1.csv"`,
			"Content-Transfer-Encoding: base64",
		}, maB64([]byte("name;city\nJos\xe9;Roma\n"))),
	)
	fx.bag = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-bag@acme.example>")), "inbound",
		"<itest-mailattach-bag@acme.example>", "itest-mailattach reports", "Reports <reports.itest-mailattach@acme.example>",
		base.Add(-20*time.Hour), bag, "reports")
	s.decide(fx.bag, "shadow", fx.anyProj)
	dup := maWith("two reports",
		maAttach("text/csv", "report.csv", []byte("a,b\n1,2\n")),
		maAttach("application/pdf", "summary.pdf", fx.pdf),
		maAttach("text/csv", "report.csv", []byte("a,b\n3,4\n")),
	)
	fx.dup = mail(a, s.thread(maKey(maAcctA, "<itest-mailattach-dup@acme.example>")), "inbound",
		"<itest-mailattach-dup@acme.example>", "itest-mailattach dup names", "Reports <reports.itest-mailattach@acme.example>",
		base.Add(-21*time.Hour), dup, "dup")
	s.decide(fx.dup, "shadow", fx.anyProj)

	// ---- criterion 11/12 fixtures ---------------------------------------------------------
	truncMID := "<itest-mailattach-trunc@acme.example>"
	s.uid++
	fx.trunc = s.message(maSpec{acct: a, extID: fmt.Sprintf("imap:INBOX:42:%d", 100000+s.uid),
		raw: maEnvelope(t, s.uid, maRFC822("Legal <legal.itest-mailattach@acme.example>", "itest-mailattach signed contract",
			truncMID, base, maLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, "The signed contract is attached.")),
			true, []map[string]any{{"part_id": "2", "filename": "contract.pdf", "content_type": "application/pdf", "size": 2_900_000}}),
		thread: s.thread(maKey(maAcctA, truncMID)), direction: "inbound", mid: truncMID,
		subject: "itest-mailattach signed contract", sender: "Legal <legal.itest-mailattach@acme.example>",
		at: base.Add(-30 * time.Hour), body: "The signed contract is attached.\n\n[Attachments not stored: contract.pdf (2.8 MB)]"})
	s.decide(fx.trunc, "shadow", fx.anyProj)
	fx.gmail = s.message(maSpec{acct: a, extID: "gmail:itest-mailattach-18c0ffee", raw: maGmailRaw,
		thread: s.thread(maKey(maAcctA, "<itest-mailattach-gmail@collab.example>")), direction: "inbound",
		mid: "<itest-mailattach-gmail@collab.example>", subject: "itest-mailattach gmail", sender: maSana,
		at: base.Add(-31 * time.Hour), body: "hello"})
	s.decide(fx.gmail, "shadow", fx.anyProj)

	// ---- mailbox CLEAN: 26 `any` filings (also the finder's bulk), (h), (k) -----------------
	for n := 1; n <= 26; n++ {
		m := mail(clean, 0, "inbound", fmt.Sprintf("<itest-mailattach-bulk-%02d@acme.example>", n),
			fmt.Sprintf("itest-mailattach bulk %02d", n), "Bulk <bulk.itest-mailattach@acme.example>",
			base.Add(-time.Duration(n)*time.Hour), one(fmt.Sprintf("bulk-%02d.txt", n)), "bulk")
		s.decide(m, "shadow", fx.anyProj)
	}
	hMID := "<itest-mailattach-h@acme.example>"
	th := s.thread(maKey(maAcctClean, hMID))
	fx.h = mail(clean, th, "inbound", hMID, "itest-mailattach (h)", "H <h.itest-mailattach@acme.example>",
		base.Add(-40*time.Hour), one("h.txt"), "h")
	s.decide(fx.h, "shadow", 0) // unmatched
	fx.k = mail(clean, th, "outbound", "<itest-mailattach-k@example.com>", "Re: itest-mailattach (h)", maAcctClean,
		base.Add(-39*time.Hour), one("k.txt"), "k")
	fx.hUnseen = mail(clean, s.thread(maKey(maAcctClean, "<itest-mailattach-h2@acme.example>")), "inbound",
		"<itest-mailattach-h2@acme.example>", "itest-mailattach (h) unseen", "H <h.itest-mailattach@acme.example>",
		base.Add(-41*time.Hour), one("h2.txt"), "h2") // no decision row at all
	fx.outAloneClean = mail(clean, s.thread(maKey(maAcctClean, "<itest-mailattach-f2@example.com>")), "outbound",
		"<itest-mailattach-f2@example.com>", "itest-mailattach (f) clean outbound alone", maAcctClean,
		base.Add(-42*time.Hour), one("f2.txt"), "f2")
	fx.outNoThread = mail(clean, 0, "outbound", "<itest-mailattach-f3@example.com>",
		"itest-mailattach (f) clean outbound unthreaded", maAcctClean, base.Add(-43*time.Hour), one("f3.txt"), "f3")

	// The dedup loser: the SAME Message-ID as (a), raw on mailbox CLEAN, no normalized row.
	s.uid++
	fx.loserRaw = s.id(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	                    VALUES ($1,$2,$3,'itest-mailattach-h-loser',now()) RETURNING id`, clean,
		fmt.Sprintf("imap:INBOX:42:%d", 900000+s.uid),
		maEnvelope(t, s.uid, maRFC822(maSana, maMainSubject, mainMID, base, mainEntity), false, nil))

	// ---- fillers: filings with no attachments -------------------------------------------------
	filler := func(acct int64, tag string, n int) maMsg {
		return s.message(maSpec{acct: acct, raw: json.RawMessage(`{"source":"imap"}`), direction: "inbound",
			mid:     fmt.Sprintf("<itest-mailattach-filler-%s-%d@x.example>", tag, n),
			subject: "itest-mailattach filler", sender: "Filler <filler.itest-mailattach@x.example>",
			at: base.Add(-72*time.Hour - time.Duration(n)*time.Minute), body: "filler"})
	}
	// DIRTY: 20 `any` filings + ONE local_only filing, then (i).
	for n := 1; n <= 20; n++ {
		s.decide(filler(dirty, "dirty", n), "shadow", fx.anyProj)
	}
	s.decide(filler(dirty, "dirty", 21), "shadow", fx.localProj)
	fx.i = mail(dirty, s.thread(maKey(maAcctDirty, "<itest-mailattach-i@acme.example>")), "inbound",
		"<itest-mailattach-i@acme.example>", "itest-mailattach (i)", "I <i.itest-mailattach@acme.example>",
		base.Add(-50*time.Hour), one("i.txt"), "i")
	s.decide(fx.i, "shadow", 0)
	// NINETEEN: 19 `any` filings, plus one message whose OLDER decision is an `any`
	// filing and whose LATEST is unmatched (not filed), then (j).
	for n := 1; n <= 19; n++ {
		s.decide(filler(nineteen, "nineteen", n), "shadow", fx.anyProj)
	}
	was := filler(nineteen, "nineteen", 20)
	s.decide(was, "shadow", fx.anyProj)
	s.decide(was, "shadow", 0)
	fx.j = mail(nineteen, s.thread(maKey(maAcct19, "<itest-mailattach-j@acme.example>")), "inbound",
		"<itest-mailattach-j@acme.example>", "itest-mailattach (j)", "J <j.itest-mailattach@acme.example>",
		base.Add(-51*time.Hour), one("j.txt"), "j")
	s.decide(fx.j, "shadow", 0)
	return fx
}

// ---- output shapes (the SPEC's API section) -------------------------------------------------

type maAtt struct {
	Index             int    `json:"index"`
	PartID            string `json:"part_id"`
	Filename          string `json:"filename"`
	ContentType       string `json:"content_type"`
	Disposition       string `json:"disposition"`
	SizeBytes         int    `json:"size_bytes"`
	SizeIsEncoded     bool   `json:"size_is_encoded"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason"`
}

type maListed struct {
	MessageID         string  `json:"message_id"`
	RawSourceItemID   int64   `json:"raw_source_item_id"`
	ThreadID          *int64  `json:"thread_id"`
	ThreadKey         string  `json:"thread_key"`
	Subject           string  `json:"subject"`
	Sender            string  `json:"sender"`
	SentAt            string  `json:"sent_at"`
	Direction         string  `json:"direction"`
	Source            string  `json:"source"`
	Truncated         bool    `json:"truncated"`
	Attachments       []maAtt `json:"attachments"`
	UnavailableReason string  `json:"unavailable_reason"`
}

type maListOut struct {
	Messages        []maListed `json:"messages"`
	WithheldPrivate int        `json:"withheld_private"`
	Truncated       bool       `json:"truncated"`
}

type maReadOut struct {
	MessageID     string `json:"message_id"`
	Index         int    `json:"index"`
	PartID        string `json:"part_id"`
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	SizeBytes     int    `json:"size_bytes"`
	Kind          string `json:"kind"`
	Text          string `json:"text"`
	Offset        int    `json:"offset"`
	ReturnedBytes int    `json:"returned_bytes"`
	TotalBytes    int    `json:"total_bytes"`
	Truncated     bool   `json:"truncated"`
	NextOffset    int    `json:"next_offset"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Hint          string `json:"hint"`
}

func maCall(ctx context.Context, ex *executor.Executor, actor, tool string, args map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	res, err := ex.Execute(ctx, executor.Call{Tool: tool, Actor: actor, Args: b})
	return res.Output, err
}

func maDecode(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func maListOK(t *testing.T, ctx context.Context, ex *executor.Executor, args map[string]any) maListOut {
	t.Helper()
	raw, err := maCall(ctx, ex, maHuman, maList, args)
	if err != nil {
		t.Fatalf("mail_list_attachments %v: %v", args, err)
	}
	var out maListOut
	maDecode(t, raw, &out)
	return out
}

func maReadOK(t *testing.T, ctx context.Context, ex *executor.Executor, args map[string]any) maReadOut {
	t.Helper()
	raw, err := maCall(ctx, ex, maHuman, maRead, args)
	if err != nil {
		t.Fatalf("mail_read_attachment %v: %v", args, err)
	}
	var out maReadOut
	maDecode(t, raw, &out)
	return out
}

func maMIDs(ms []maListed) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.MessageID)
	}
	return out
}

func maScan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// maNoLie: no refusal may say the bytes are missing (criteria 11-14).
func maNoLie(t *testing.T, label string, err error) {
	t.Helper()
	for _, lie := range []string{"not stored", "does not exist"} {
		if strings.Contains(strings.ToLower(err.Error()), lie) {
			t.Errorf("%s: the refusal %q says %q — a locality refusal must never read as missing bytes", label, err, lie)
		}
	}
}

func maCheckMainAttachments(t *testing.T, got []maAtt, fx *maFixture) {
	t.Helper()
	want := []maAtt{
		{Index: 1, PartID: "2", Filename: "Request.json", ContentType: "application/octet-stream", Disposition: "attachment", SizeBytes: len(fx.request), Available: true},
		{Index: 2, PartID: "3", Filename: "Response.json", ContentType: "application/json", Disposition: "attachment", SizeBytes: len(fx.response), Available: true},
		{Index: 3, PartID: "4", Filename: "GetAll-Response.json", ContentType: "application/octet-stream", Disposition: "attachment", SizeBytes: len(fx.getAll), Available: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attachments = %+v\nwant exactly the three attachment parts %+v (criterion 1)", got, want)
	}
}

// ---- criteria 1, 2, 12 (dedup loser) --------------------------------------------------------

func TestMailAttach_Integration_ListsByEveryIdentifier(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)

	t.Run("criterion 1: by raw_source_item_id", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.main.raw})
		if len(out.Messages) != 1 {
			t.Fatalf("messages = %v, want exactly the one message", maMIDs(out.Messages))
		}
		m := out.Messages[0]
		if m.MessageID != fx.main.mid || m.RawSourceItemID != fx.main.raw || m.ThreadID == nil || *m.ThreadID != fx.t1ID ||
			m.ThreadKey != fx.t1Key || m.Subject != maMainSubject || m.Sender != maSana || m.Direction != "inbound" ||
			m.Source != "imap" || m.Truncated || m.SentAt == "" || m.UnavailableReason != "" {
			t.Errorf("message = %+v, want (a)'s identity: %s raw %d thread %d %s", m, fx.main.mid, fx.main.raw, fx.t1ID, fx.t1Key)
		}
		maCheckMainAttachments(t, m.Attachments, fx)
	})

	t.Run("criterion 2: by message_id, the normalized winner", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"message_id": fx.main.mid})
		if len(out.Messages) != 1 || out.Messages[0].RawSourceItemID != fx.main.raw {
			t.Fatalf("by message_id = %+v, want (a) on raw %d — the normalized message, never the dedup loser %d (D6)",
				out.Messages, fx.main.raw, fx.loserRaw)
		}
		maCheckMainAttachments(t, out.Messages[0].Attachments, fx)
	})

	for _, args := range []map[string]any{{"thread_id": fx.t1ID}, {"thread_key": fx.t1Key}} {
		t.Run(fmt.Sprintf("criterion 2: by %v", args), func(t *testing.T) {
			out := maListOK(t, ctx, ex, args)
			by := map[string]maListed{}
			for _, m := range out.Messages {
				by[m.MessageID] = m
			}
			if len(out.Messages) != 2 || out.WithheldPrivate != 0 {
				t.Fatalf("thread form = %v (withheld %d), want (a) and (e), each with its own list, none withheld",
					maMIDs(out.Messages), out.WithheldPrivate)
			}
			maCheckMainAttachments(t, by[fx.main.mid].Attachments, fx)
			if o := by[fx.out.mid]; len(o.Attachments) != 1 || o.Attachments[0].Filename != "notes.txt" ||
				o.Direction != "outbound" || o.RawSourceItemID != fx.out.raw {
				t.Errorf("(e) in the thread form = %+v, want its own list [notes.txt]", o)
			}
		})
	}

	t.Run("criterion 12: a raw id with no normalized_messages row", func(t *testing.T) {
		_, err := maCall(ctx, ex, maHuman, maList, map[string]any{"raw_source_item_id": fx.loserRaw})
		if err == nil || !strings.Contains(err.Error(), "use message_id") {
			t.Errorf("listing the dedup loser raw %d = %v, want a refusal saying \"use message_id\" (D6: no second dedup spelling)",
				fx.loserRaw, err)
		}
		_, err = maCall(ctx, ex, maHuman, maRead, map[string]any{"raw_source_item_id": fx.loserRaw, "index": 1})
		if err == nil || !strings.Contains(err.Error(), "use message_id") {
			t.Errorf("reading from the dedup loser = %v, want \"use message_id\"", err)
		}
	})
}

// ---- criteria 3 and 15: the finder -----------------------------------------------------------

func TestMailAttach_Integration_FinderMatchesHeadersAndReturnsNoBodies(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)

	t.Run("from: case-insensitive, newest first, only messages with parts, restricted withheld", func(t *testing.T) {
		raw, err := maCall(ctx, ex, maHuman, maList, map[string]any{"from": "SANA.ITEST-MAILATTACH"})
		if err != nil {
			t.Fatalf("finder: %v", err)
		}
		var out maListOut
		maDecode(t, raw, &out)
		if got, want := maMIDs(out.Messages), []string{fx.main.mid, fx.sanaOld.mid}; !reflect.DeepEqual(got, want) {
			t.Errorf("finder(from) = %v, want %v — newest first; the no-attachment message (%s), the slack-channel "+
				"message (%s) and the local_only-filed one (%s) left out", got, want, fx.sanaNoAtt.mid, fx.slackSana.mid, fx.local.mid)
		}
		if out.WithheldPrivate != 1 {
			t.Errorf("withheld_private = %d, want 1 (the local_only-filed message from the same sender, criterion 15)", out.WithheldPrivate)
		}
		if out.Truncated {
			t.Errorf("truncated = true with 2 hits under the default limit")
		}
		// Criterion 3: headers and the attachment list, NEVER a body or snippet;
		// criterion 13: a restricted message's subject never appears.
		for _, forbidden := range []string{maBodyMarker, maPrivSubject, "private.json"} {
			if strings.Contains(string(raw), forbidden) {
				t.Errorf("finder output contains %q: %s", forbidden, raw)
			}
		}
		var keys struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		maDecode(t, raw, &keys)
		for _, m := range keys.Messages {
			for _, k := range []string{"body", "body_text", "snippet", "text"} {
				if _, ok := m[k]; ok {
					t.Errorf("a finder hit carries %q; the finder returns headers and attachment names only", k)
				}
			}
			for _, k := range []string{"message_id", "raw_source_item_id", "thread_key", "subject", "sender", "sent_at", "direction", "attachments"} {
				if _, ok := m[k]; !ok {
					t.Errorf("a finder hit lacks %q (criterion 3): %v", k, m)
				}
			}
		}
	})

	t.Run("subject: case-insensitive substring, newest first", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"subject": "VALIDATION (ITEST-MAILATTACH)"})
		if got, want := maMIDs(out.Messages), []string{fx.out.mid, fx.main.mid}; !reflect.DeepEqual(got, want) {
			t.Errorf("finder(subject) = %v, want %v (the reply is newer)", got, want)
		}
	})

	t.Run("limit and the limit+1 truncated rule", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"from": "sana.itest-mailattach", "limit": 1})
		if len(out.Messages) != 1 || out.Messages[0].MessageID != fx.main.mid || !out.Truncated {
			t.Errorf("limit 1 = %v truncated=%v, want [%s] truncated", maMIDs(out.Messages), out.Truncated, fx.main.mid)
		}
		out = maListOK(t, ctx, ex, map[string]any{"subject": "itest-mailattach bulk"})
		if len(out.Messages) != 10 || !out.Truncated {
			t.Errorf("default limit over 26 hits = %d truncated=%v, want 10, truncated", len(out.Messages), out.Truncated)
		}
		out = maListOK(t, ctx, ex, map[string]any{"subject": "itest-mailattach bulk", "limit": 30})
		if len(out.Messages) != 25 || !out.Truncated {
			t.Errorf("limit 30 over 26 hits = %d truncated=%v, want the cap 25, truncated", len(out.Messages), out.Truncated)
		}
		out = maListOK(t, ctx, ex, map[string]any{"subject": "itest-mailattach bulk", "limit": 26})
		if len(out.Messages) != 25 {
			t.Errorf("limit 26 = %d messages, want the cap 25", len(out.Messages))
		}
	})

	t.Run("since and until bound sent_at", func(t *testing.T) {
		base := time.Date(2026, 9, 10, 22, 6, 0, 0, time.UTC)
		out := maListOK(t, ctx, ex, map[string]any{"subject": "itest-mailattach bulk",
			"since": base.Add(-7*time.Hour - 30*time.Minute).Format(time.RFC3339),
			"until": base.Add(-4*time.Hour - 30*time.Minute).Format(time.RFC3339)})
		want := []string{"<itest-mailattach-bulk-05@acme.example>", "<itest-mailattach-bulk-06@acme.example>", "<itest-mailattach-bulk-07@acme.example>"}
		if got := maMIDs(out.Messages); !reflect.DeepEqual(got, want) {
			t.Errorf("since/until window = %v, want %v", got, want)
		}
	})

	t.Run("the user profile's only way to a message (fact 6)", func(t *testing.T) {
		user := mcpserver.NewWithProfile(ex, "manual:salvo", mcpserver.ProfileUser)
		raw, err := user.CallTool(ctx, maList, json.RawMessage(`{"from":"sana.itest-mailattach","subject":"validation (itest-mailattach)"}`))
		if err != nil {
			t.Fatalf("ProfileUser finder: %v — a session in another repo must reach Sana's mail by sender and subject", err)
		}
		var out maListOut
		maDecode(t, raw, &out)
		if got := maMIDs(out.Messages); len(got) != 1 || got[0] != fx.main.mid {
			t.Errorf("ProfileUser finder = %v, want [%s]", got, fx.main.mid)
		}
	})
}

// ---- criteria 5-8 and 20: reading ----------------------------------------------------------------

func TestMailAttach_Integration_ReadsTextInlineAndFilesToTheCache(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)
	cache := os.Getenv("XDG_CACHE_HOME")

	t.Run("criterion 5: octet-stream JSON inline, byte-identical, by every selector", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"raw_source_item_id": fx.main.raw, "index": 1},
			{"message_id": fx.main.mid, "filename": "Request.json"},
			{"raw_source_item_id": fx.main.raw, "part_id": "2"},
		} {
			r := maReadOK(t, ctx, ex, args)
			if r.Kind != "text" || r.Text != string(fx.request) {
				t.Errorf("read %v = kind %q, %d bytes of text; want kind text, byte-identical to Request.json (%d bytes)",
					args, r.Kind, len(r.Text), len(fx.request))
			}
			if r.MessageID != fx.main.mid || r.Index != 1 || r.PartID != "2" || r.Filename != "Request.json" ||
				r.ContentType != "application/octet-stream" || r.SizeBytes != len(fx.request) || r.Truncated {
				t.Errorf("read %v = %+v, want (a)'s Request.json identity", args, r)
			}
		}
		r := maReadOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.main.raw, "index": 2})
		if r.Kind != "text" || r.Truncated || r.Text != string(fx.response) || r.ReturnedBytes != 84274 || r.TotalBytes != 84274 {
			t.Errorf("Response.json = kind %q truncated %v returned %d total %d; want the 84,274 bytes inline in ONE call (D7)",
				r.Kind, r.Truncated, r.ReturnedBytes, r.TotalBytes)
		}
	})

	t.Run("criterion 7: the inline cap and offset paging", func(t *testing.T) {
		first := maReadOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.bag.raw, "index": 1})
		const n, total = 100 * 1024, 150 * 1024
		if first.Kind != "text" || !first.Truncated || first.ReturnedBytes != n || first.TotalBytes != total || first.NextOffset != n {
			t.Fatalf("big.csv page 1 = kind %q truncated %v returned %d total %d next %d; want text, truncated, %d of %d, next %d",
				first.Kind, first.Truncated, first.ReturnedBytes, first.TotalBytes, first.NextOffset, n, total, n)
		}
		if !strings.HasPrefix(first.Text, string(fx.bigCSV[:n])) ||
			!strings.HasSuffix(first.Text, fmt.Sprintf("[attachment truncated: showing bytes 0–%d of %d; call mail_read_attachment again with offset=%d, or to_file=true]", n, total, n)) {
			t.Errorf("big.csv page 1 is not the first %d bytes followed by the truncation line", n)
		}
		rest := maReadOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.bag.raw, "index": 1, "offset": first.NextOffset})
		if rest.Truncated || rest.Text != string(fx.bigCSV[n:]) || rest.Offset != n {
			t.Errorf("big.csv page 2 = truncated %v, %d bytes at offset %d; want the remaining %d bytes", rest.Truncated,
				len(rest.Text), rest.Offset, total-n)
		}
		if _, err := maCall(ctx, ex, maHuman, maRead, map[string]any{"raw_source_item_id": fx.bag.raw, "index": 1, "offset": total + 1}); err == nil {
			t.Errorf("an offset past the end was accepted")
		}
	})

	t.Run("criterion 6: a declared iso-8859-1 CSV comes back repaired", func(t *testing.T) {
		r := maReadOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.bag.raw, "filename": "latin1.csv"})
		if r.Kind != "text" || !strings.Contains(r.Text, "José") {
			t.Errorf("latin1.csv = kind %q text %q, want text containing José", r.Kind, r.Text)
		}
	})

	sha := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	checkFile := func(t *testing.T, r maReadOut, rawID int64, name string, want []byte) {
		t.Helper()
		wantPath := filepath.Join(cache, "switchboard", "attachments", fmt.Sprint(rawID), name)
		if r.Kind != "file" || r.Path != wantPath || r.SHA256 != sha(want) || r.SizeBytes != len(want) {
			t.Errorf("file result = kind %q path %q sha %q size %d; want file at %q with sha256 %s, %d bytes",
				r.Kind, r.Path, r.SHA256, r.SizeBytes, wantPath, sha(want), len(want))
		}
		if !strings.Contains(r.Hint, "Read") {
			t.Errorf("hint = %q, want it to point at Claude Code's Read tool (it opens PDFs and images)", r.Hint)
		}
		got, err := os.ReadFile(wantPath)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the file at %s = %d bytes, %v; want the decoded part", wantPath, len(got), err)
		}
		if st, _ := os.Stat(wantPath); st.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %o, want 0600", st.Mode().Perm())
		}
	}

	t.Run("criterion 8: a file-kind part is written, not inlined", func(t *testing.T) {
		r := maReadOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.bag.raw, "filename": "report.pdf"})
		checkFile(t, r, fx.bag.raw, "2-report.pdf", fx.pdf)
		if r.Text != "" {
			t.Errorf("a file-kind result carries %d bytes of text; binary never goes inline (D1)", len(r.Text))
		}
	})

	t.Run("criterion 8: to_file on a text part", func(t *testing.T) {
		r := maReadOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.main.raw, "index": 3, "to_file": true})
		checkFile(t, r, fx.main.raw, "3-GetAll-Response.json", fx.getAll)
		if r.ContentType != "application/octet-stream" {
			t.Errorf("content_type = %q, want the declared application/octet-stream", r.ContentType)
		}
	})

	t.Run("criterion 20: a part that is not there, and an ambiguous filename", func(t *testing.T) {
		if _, err := maCall(ctx, ex, maHuman, maRead, map[string]any{"raw_source_item_id": fx.main.raw, "index": 4}); err == nil {
			t.Error("index 4 of a 3-attachment message was accepted")
		}
		if _, err := maCall(ctx, ex, maHuman, maRead, map[string]any{"raw_source_item_id": fx.main.raw, "filename": "nope.json"}); err == nil {
			t.Error("a filename no part carries was accepted")
		}
		_, err := maCall(ctx, ex, maHuman, maRead, map[string]any{"raw_source_item_id": fx.dup.raw, "filename": "report.csv"})
		if err == nil {
			t.Fatal("filename report.csv matches parts 1 and 3 and was accepted")
		}
		if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "3") {
			t.Errorf("ambiguous-filename error = %q, want it to list the indexes 1 and 3", err)
		}
	})
}

// ---- criteria 11 and 12: say why, never "does not exist" -----------------------------------------

func TestMailAttach_Integration_UnavailableBytesSayWhy(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)

	t.Run("criterion 11: a truncated capture", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.trunc.raw})
		if len(out.Messages) != 1 || !out.Messages[0].Truncated || len(out.Messages[0].Attachments) != 1 {
			t.Fatalf("truncated listing = %+v, want one truncated message with its one manifest entry", out.Messages)
		}
		a := out.Messages[0].Attachments[0]
		if a.Available || a.UnavailableReason != maTruncReason || !a.SizeIsEncoded || a.SizeBytes != 2_900_000 ||
			a.Filename != "contract.pdf" || a.PartID != "2" || a.Index != 1 {
			t.Errorf("manifest entry = %+v, want contract.pdf part 2, unavailable (%q), size 2900000 encoded", a, maTruncReason)
		}
		_, err := maCall(ctx, ex, maHuman, maRead, map[string]any{"raw_source_item_id": fx.trunc.raw, "index": 1})
		if err == nil || !strings.Contains(err.Error(), "not stored") || !strings.Contains(err.Error(), "1 MiB") {
			t.Errorf("reading a truncated part = %v, want an error containing \"not stored\" and \"1 MiB\"", err)
		}
	})

	t.Run("criterion 12: a gmail:-prefixed raw row", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"raw_source_item_id": fx.gmail.raw})
		if len(out.Messages) != 1 || out.Messages[0].UnavailableReason != maGmailReason || len(out.Messages[0].Attachments) != 0 {
			t.Errorf("gmail listing = %+v, want the message with unavailable_reason %q and no attachments", out.Messages, maGmailReason)
		}
		_, err := maCall(ctx, ex, maHuman, maRead, map[string]any{"message_id": fx.gmail.mid, "index": 1})
		if err == nil || !strings.Contains(err.Error(), maGmailReason) {
			t.Errorf("reading from the gmail row = %v, want an error with %q", err, maGmailReason)
		}
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			t.Errorf("error %q says \"does not exist\"", err)
		}
	})
}

// ---- criteria 13, 14, 15: the class of a message ---------------------------------------------------

func TestMailAttach_Integration_LocalityCasesFromPostgres(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)

	for _, tc := range []struct {
		label   string
		m       maMsg
		allowed bool
		reason  string // checked when non-empty
	}{
		{"(a) inbound filed under an any project", fx.main, true, ""},
		{"(b) inbound filed under a local_only project", fx.local, false, "filed under a local-only project"},
		{"(c) inbound unmatched", fx.unmatched, false, "not filed under a project"},
		{"(d) inbound with no decision row", fx.unseen, false, "not filed under a project"},
		{"(e) outbound on (a)'s thread", fx.out, true, ""},
		{"(f) outbound alone on its thread", fx.outAlone, false, ""},
		{"(f) outbound alone on its thread, on a CLEAN mailbox (O2 is inbound-only)", fx.outAloneClean, false, ""},
		{"(f) outbound with no thread, on a CLEAN mailbox", fx.outNoThread, false, ""},
		{"(g) outbound on a thread with any- and local_only-filed inbound", fx.g3, false, ""},
		{"(h) O2: inbound unmatched on a mailbox with >=20 any filings", fx.h, true, ""},
		{"(h) O2: inbound with no decision row on the same clean mailbox", fx.hUnseen, true, ""},
		{"(i) O2: the same shape plus ONE local_only filing", fx.i, false, "not filed under a project"},
		{"(j) O2: only 19 any filings (latest decision per message)", fx.j, false, ""},
		{"(k) O2: outbound on (h)'s thread", fx.k, true, ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			calls := []struct {
				tool string
				args map[string]any
			}{
				{maList, map[string]any{"raw_source_item_id": tc.m.raw}},
				{maList, map[string]any{"message_id": tc.m.mid}},
				{maRead, map[string]any{"raw_source_item_id": tc.m.raw, "index": 1}},
			}
			for _, c := range calls {
				raw, err := maCall(ctx, ex, maHuman, c.tool, c.args)
				if tc.allowed {
					if err != nil {
						t.Errorf("%s %v refused: %v — want allowed (ClassGeneral)", c.tool, c.args, err)
						continue
					}
					if c.tool == maList {
						var out maListOut
						maDecode(t, raw, &out)
						// At least one available attachment: (a) is the main fixture with
						// three, every other allowed case has one. The case under test is
						// allowed-vs-refused, not the attachment count (criterion 1 pins that).
						if len(out.Messages) != 1 || out.Messages[0].RawSourceItemID != tc.m.raw ||
							len(out.Messages[0].Attachments) == 0 || !out.Messages[0].Attachments[0].Available {
							t.Errorf("%s %v = %+v, want the message with its available attachment(s)", c.tool, c.args, out)
						}
					}
					continue
				}
				// Refused: an explicit id is an ERROR naming the reason (criterion 15).
				if err == nil {
					t.Errorf("%s %v was ALLOWED: %s — want refused", c.tool, c.args, raw)
					continue
				}
				if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("%s %v refusal = %q, want it to say %q", c.tool, c.args, err, tc.reason)
				}
				maNoLie(t, c.tool, err)
			}
		})
	}

	t.Run("criterion 15: the thread form leaves restricted members out and counts them", func(t *testing.T) {
		out := maListOK(t, ctx, ex, map[string]any{"thread_key": fx.t6Key})
		if got := maMIDs(out.Messages); len(got) != 1 || got[0] != fx.g1.mid || out.WithheldPrivate != 2 {
			t.Errorf("thread (g) = %v withheld %d, want [%s] with withheld_private 2 (the local_only inbound and the outbound)",
				got, out.WithheldPrivate, fx.g1.mid)
		}
		if raw, _ := json.Marshal(out); strings.Contains(string(raw), "g2.txt") || strings.Contains(string(raw), "g3.txt") {
			t.Errorf("the thread form leaked a restricted member's attachment names: %s", raw)
		}
	})
}

// ---- criterion 16: the gate does not look at the caller or the profile -------------------------------

func TestMailAttach_Integration_GateIgnoresCallerAndProfile(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)

	actors := []string{
		"dashboard:itest-mailattach", "opsctl:itest-mailattach", "mcp:worker:itest-mailattach",
		"mcp:manual:itest-mailattach", "drafts:gpt", "worker:itest-mailattach",
	}
	for _, actor := range actors {
		t.Run(actor, func(t *testing.T) {
			for _, c := range []struct {
				tool string
				args map[string]any
			}{
				{maList, map[string]any{"raw_source_item_id": fx.local.raw}},
				{maRead, map[string]any{"raw_source_item_id": fx.local.raw, "index": 1}},
			} {
				raw, err := maCall(ctx, ex, actor, c.tool, c.args)
				if err == nil {
					t.Errorf("(b) %s by %s was ALLOWED: %s — the gate keys on the MESSAGE, never the actor "+
						"(IK: an actor prefix is a transport label, not a trust boundary)", c.tool, actor, raw)
				} else if !strings.Contains(err.Error(), "local-only") {
					t.Errorf("(b) %s by %s = %q, want the locality refusal", c.tool, actor, err)
				}
			}
			// And no actor is refused on (a): the tools are not humanOnly (criterion 18).
			if _, err := maCall(ctx, ex, actor, maList, map[string]any{"raw_source_item_id": fx.main.raw}); err != nil {
				t.Errorf("(a) by %s refused: %v — both tools fall through to the static-default allow", actor, err)
			}
		})
	}

	servers := []struct {
		name string
		srv  *mcpserver.Server
	}{
		{"ProfileUser as manual:salvo", mcpserver.NewWithProfile(ex, "manual:salvo", mcpserver.ProfileUser)},
		{"ProfileFull as manual:itest-mailattach", mcpserver.New(ex, "manual:itest-mailattach")},
		{"ProfileFull as a worker console", mcpserver.New(ex, "itest-mailattach-console")},
	}
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) {
			for _, c := range []struct{ tool, args string }{
				{maList, fmt.Sprintf(`{"raw_source_item_id":%d}`, fx.local.raw)},
				{maRead, fmt.Sprintf(`{"raw_source_item_id":%d,"index":1}`, fx.local.raw)},
			} {
				out, err := s.srv.CallTool(ctx, c.tool, json.RawMessage(c.args))
				if err == nil {
					t.Errorf("(b) %s through %s was ALLOWED: %s", c.tool, s.name, out)
				} else if !strings.Contains(err.Error(), "local-only") {
					t.Errorf("(b) %s through %s = %q, want the locality refusal (not an MCP-layer one)", c.tool, s.name, err)
				}
			}
			// Positive control: the same server reads (a).
			out, err := s.srv.CallTool(ctx, maRead, json.RawMessage(fmt.Sprintf(`{"raw_source_item_id":%d,"filename":"Request.json"}`, fx.main.raw)))
			if err != nil {
				t.Fatalf("(a) Request.json through %s: %v", s.name, err)
			}
			var r maReadOut
			maDecode(t, out, &r)
			if r.Text != string(fx.request) {
				t.Errorf("(a) Request.json through %s = %q, want the JSON inline", s.name, r.Text)
			}
		})
	}
}

// ---- criteria 18 and 19: audit, and nothing else written ---------------------------------------------

func TestMailAttach_Integration_AuditTrailAndNoWrites(t *testing.T) {
	ctx := context.Background()
	pool := mailAttachPool(t, ctx)
	fx := seedMailAttach(t, ctx, pool)
	ex := queueMatrixExecutor(pool)

	counts := func() map[string]int {
		out := map[string]int{}
		for _, table := range []string{"tasks", "task_events", "deliveries", "normalized_messages", "raw_source_items"} {
			out[table] = maScan(t, ctx, pool, `SELECT count(*) FROM `+table)
		}
		return out
	}
	hashes := func() map[int64]string {
		rows, err := pool.Query(ctx, `SELECT id, content_hash FROM raw_source_items WHERE source_account_id = ANY($1)`, fx.accounts)
		if err != nil {
			t.Fatalf("hashes: %v", err)
		}
		defer rows.Close()
		out := map[int64]string{}
		for rows.Next() {
			var id int64
			var h string
			if err := rows.Scan(&id, &h); err != nil {
				t.Fatalf("scan hash: %v", err)
			}
			out[id] = h
		}
		return out
	}
	countsBefore, hashesBefore := counts(), hashes()

	for i, c := range []struct {
		label, tool  string
		args         map[string]any
		status, text string // text: a substring of audit_events.error on a refusal
	}{
		{"list by raw id", maList, map[string]any{"raw_source_item_id": fx.main.raw}, "ok", ""},
		{"read Request.json inline", maRead, map[string]any{"raw_source_item_id": fx.main.raw, "index": 1}, "ok", ""},
		{"read to_file", maRead, map[string]any{"message_id": fx.main.mid, "filename": "Response.json", "to_file": true}, "ok", ""},
		{"list the thread", maList, map[string]any{"thread_key": fx.t1Key}, "ok", ""},
		{"list by thread_id", maList, map[string]any{"thread_id": fx.t6ID}, "ok", ""},
		{"finder", maList, map[string]any{"from": "sana.itest-mailattach", "limit": 5}, "ok", ""},
		{"page 2 by offset", maRead, map[string]any{"raw_source_item_id": fx.bag.raw, "index": 1, "offset": 100 * 1024}, "ok", ""},
		{"a file-kind part", maRead, map[string]any{"raw_source_item_id": fx.bag.raw, "index": 2}, "ok", ""},
		{"refused read (b)", maRead, map[string]any{"raw_source_item_id": fx.local.raw, "index": 1}, "error", "local-only"},
		{"refused list (c)", maList, map[string]any{"message_id": fx.unmatched.mid}, "error", "not filed under a project"},
		{"truncated read", maRead, map[string]any{"raw_source_item_id": fx.trunc.raw, "index": 1}, "error", "not stored"},
		{"gmail read", maRead, map[string]any{"raw_source_item_id": fx.gmail.raw, "index": 1}, "error", "Gmail API/bridge"},
		{"dedup loser", maList, map[string]any{"raw_source_item_id": fx.loserRaw}, "error", "use message_id"},
	} {
		actor := fmt.Sprintf("mcp:manual:itest-mailattach-audit-%02d", i)
		t.Run(c.label, func(t *testing.T) {
			out, err := maCall(ctx, ex, actor, c.tool, c.args)
			if (err == nil) != (c.status == "ok") {
				t.Errorf("%s %v: err = %v, want status %s", c.tool, c.args, err, c.status)
			}
			if c.label == "read Request.json inline" && !strings.Contains(string(out), maMarker) {
				t.Errorf("POSITIVE CONTROL: the Request.json read did not return the marker, so the scan below proves nothing")
			}

			rows, qerr := pool.Query(ctx, `SELECT id, args, status, COALESCE(error,'') FROM audit_events WHERE actor=$1 AND tool=$2`, actor, c.tool)
			if qerr != nil {
				t.Fatalf("audit query: %v", qerr)
			}
			type auditRow struct {
				id           int64
				args         []byte
				status, text string
			}
			var got []auditRow
			for rows.Next() {
				var r auditRow
				if err := rows.Scan(&r.id, &r.args, &r.status, &r.text); err != nil {
					t.Fatalf("audit scan: %v", err)
				}
				got = append(got, r)
			}
			rows.Close()
			if len(got) != 1 {
				t.Fatalf("%d audit_events rows for this call, want exactly 1 (invariant 3)", len(got))
			}
			a := got[0]
			if a.status != c.status {
				t.Errorf("audit status = %q, want %q", a.status, c.status)
			}
			if c.text != "" && !strings.Contains(a.text, c.text) {
				t.Errorf("audit error = %q, want it to name the reason (%q)", a.text, c.text)
			}
			var gotArgs, wantArgs map[string]any
			_ = json.Unmarshal(a.args, &gotArgs)
			b, _ := json.Marshal(c.args)
			_ = json.Unmarshal(b, &wantArgs)
			if !reflect.DeepEqual(gotArgs, wantArgs) {
				t.Errorf("audit args = %s, want exactly the identifiers and options sent %s", a.args, b)
			}
			if n := maScan(t, ctx, pool, `SELECT count(*) FROM policy_decisions WHERE audit_event_id=$1 AND decision='allow' AND rule='static-default'`, a.id); n != 1 {
				t.Errorf("policy_decisions allow/static-default rows for this call = %d, want 1 (not humanOnly, not snapshotGated)", n)
			}
		})
	}

	t.Run("criterion 18: attachment content never lands in the database", func(t *testing.T) {
		if n := maScan(t, ctx, pool, `SELECT count(*) FROM audit_events a WHERE a::text LIKE '%'||$1||'%'`, maMarker); n != 0 {
			t.Errorf("%d audit_events rows contain the Request.json marker; audit keeps args, never output (fact 9)", n)
		}
		if n := maScan(t, ctx, pool, `SELECT count(*) FROM policy_decisions p WHERE p::text LIKE '%'||$1||'%'`, maMarker); n != 0 {
			t.Errorf("%d policy_decisions rows contain the Request.json marker", n)
		}
	})

	t.Run("criterion 19: zero writes other than the audit row", func(t *testing.T) {
		if after := counts(); !reflect.DeepEqual(after, countsBefore) {
			t.Errorf("row counts moved: before %v, after %v", countsBefore, after)
		}
		if after := hashes(); !reflect.DeepEqual(after, hashesBefore) {
			t.Errorf("a read raw row's content_hash changed (invariant 1: the tools never write raw_source_items)")
		}
	})
}
