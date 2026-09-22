//go:build integration

package dashboard_test

// task-detail-source-message (SWT-65, docs/tickets/task-detail-source-message_SPEC.md)
// test plan section C: criteria 1-5, 7-11, 15-17, 19-22 against a REAL database
// and the REAL dashboard.Server (dev-login auth). Build-tagged `integration` AND
// env-gated on DATABASE_URL. NO LLM, NO network, NO broker, and this page
// performs NO write (invariant 3 is satisfied by absence).
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_taskdetail"
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_taskdetail?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_taskdetail?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run TaskDetail ./internal/dashboard/
//
// Reuses dashGuard / dashPool / newDashServer / get / snippet
// (dashboard_integration_test.go), bdInsID (board_dismiss_integration_test.go)
// and countTracer (board_refresh_integration_test.go).
//
// CROSS-SUITE DISCIPLINE (the mutual-cleanup pact). This suite seeds inbound
// normalized_messages, which are visible to capture's GLOBAL pending filter and
// to classify's inboxes; and capture's suites delete capture_decisions
// WHOLESALE, which would silently remove this suite's branch-2 links. So:
// everything is scoped to the prefix `itest-taskdetail-`, cleanup runs before
// AND after in FK order — policy_decisions and audit_events BY task_id FIRST
// (the SWT-37 landmine) — and every assertion is scoped to a fixture id. Run
// the integration suite with -p 1, in an ISOLATED database (the 2026-09-12
// landmine); dashGuard refuses the production DSN outright.
//
// NO CLIENT CONTENT. Every sender, subject and body is a placeholder. The one
// real-world string is the bank-alert tracking HOST, which criterion 11 names.
//
// GREENFIELD NOTE — EXPECTED RED, BY ASSERTION NOT BY COMPILER. This file
// deliberately references no new Go identifier (the caps below are test-local
// copies, pinned to the exported ones by TestMailThreadCaps_OneSpelling in
// task_detail_structure_test.go), so every criterion fails where it is
// ASSERTED rather than at the compiler.
//
// The package's OTHER new test, task_detail_test.go, does reference
// sourceMessageHeading, so today the shared test binary does not link at all
// (`undefined: sourceMessageHeading`). To read this file's failures before the
// implementation exists, drop a two-line stub into internal/dashboard:
//
//	const sourceBodyCap = 262144
//	func sourceMessageHeading(channel string, viaThread bool) string { return "" }
//
// and delete it in the same command. With that stub in place, today:
//   - the section never appears        → criteria 1-5, 7-11, 18-21
//   - no statement reads normalized_messages → criterion 22
//   - criteria 15/16/17 (the no-link render and the 404) PASS today and must
//     still pass after the change. They are pins, not new behaviour; the golden
//     captured today IS the "pre-change binary" of criterion 15.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC section E; run them in a
// throwaway copy of the worktree, never in place):
//
//	1 branch 1 selects a literal message id        → Precedence (T1) or Channel (T4)
//	2 m.body_text → ''                             → Inert (T1), Channel (T4), BodyCap (T8)
//	3 m.channel → 'gmail'                          → Channel (T4: heading + no attachments)
//	4 m.direction → 'inbound'                      → Thread (T1: the outbound sibling)
//	5 drop action IN ('task','review') on branch 1 → Precedence (T1 resolves the 'attached' loser)
//	6 drop action = 'task' on branch 2             → AttachedFallsThrough (T2 resolves the task_log loser)
//	7 swap branches 1 and 2                        → Precedence (T1)
//	8 branch 3 sent_at ASC / no direction filter   → LatestInbound (T3)
//	9 thread block keyed on tasks.source_thread_id → Precedence (T1's thread is TA, its source_thread_id is TB)
//	10 body rendered with template.HTML            → Inert (the tag scan)
//	11 bare URLs wrapped in <a href>               → Inert (the tag scan)
//	12 heading rendered unconditionally            → NoLink (T5 golden), Unresolvable (T6)
//	15 MailAttachmentsForRawItem on a non-gmail msg → Channel (T4: a raw_json statement)
//	16 a mailClassJudge gate on the load path      → Precedence (the fixture project is local_only)
//	18 sourceBodyCap raised/lowered                → BodyCap (T8)

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

const (
	tdSlug   = "itest-taskdetail-proj"
	tdClient = "itest-taskdetail-client"
	tdAcct   = "itest-taskdetail-a@local.test"
	tdExtPfx = "itest-taskdetail:"
	tdKeyPfx = "itest-taskdetail:thread:"

	// Test-local copies of the three D8 numbers. They are NOT a second
	// spelling of the rule: TestMailThreadCaps_OneSpelling pins the code to a
	// single exported declaration of each, and this file asserts the RENDER
	// against these values. Referencing tools.MailThreadBodyCap here would make
	// the whole binary fail to compile today and hide every other criterion.
	tdThreadMax     = 50     // tools.MailThreadMaxMessages
	tdThreadBodyCap = 8192   // tools.MailThreadBodyCap (8 * 1024)
	tdSourceBodyCap = 262144 // dashboard sourceBodyCap (D3, deliberately NOT the same number)

	tdSummaryPrefix = 120 // D3: the summary's first 120 runes, via textmatch.NormalizedPrefix
)

// ---- fixture markers. Placeholders only; no client content. -------------------

const (
	tdSourceMark    = "AAA-SOURCE-BODY-MARKER"
	tdBelowQuote    = "AAA-QUESTION-BELOW-THE-QUOTED-CHAIN"
	tdOlderSibling  = "AAA-OLDER-SIBLING-MARKER"
	tdPastThreadCap = "AAA-PAST-THE-THREAD-BODY-CAP"
	tdOutbound      = "AAA-OUTBOUND-SIBLING-MARKER"
	tdLoser         = "BBB-WEAKER-LINK-LOSER-MARKER"
	tdCaptureSource = "CCC-CAPTURE-TASK-SOURCE-MARKER"
	tdAttachedLoser = "DDD-ATTACHED-PROMOTION-LOSER"
	tdTaskLogLoser  = "DDD-TASK-LOG-DECISION-LOSER"
	tdSlackSource   = "SSS-SLACK-SOURCE-MARKER"
	tdSlackSibling  = "SSS-SLACK-SIBLING-MARKER"
	tdTruncSource   = "TTT-TRUNCATED-SOURCE-MARKER"
	tdBigHead       = "EEE-OVERSIZE-BODY-HEAD"
	tdPastSourceCap = "EEE-PAST-THE-SOURCE-BODY-CAP"
	tdLongThread    = "LLL-LONG-THREAD-MARKER"

	// The two no-link fixtures carry NO DIGIT in their title or body: the
	// golden comparison masks the task id, and a "T5" in the title would be
	// masked with it.
	tdNoLinkTitle       = "TASKDETAIL NOLINK no link at all"
	tdNoLinkBody        = "NOLINK placeholder card body.\nsecond line."
	tdUnresolvableTitle = "TASKDETAIL UNRESOLVABLE links"
	tdUnresolvableBody  = "UNRESOLVABLE placeholder card body."

	// Criterion 11's three payloads, verbatim.
	tdScript  = "<script>alert(1)</script>"
	tdImg     = "<img src=x onerror=alert(1)>"
	tdTrackTo = "https://click.ealerts.bankofamerica.com/f/a/TRACKINGTOKEN"
)

// ---- cleanup ------------------------------------------------------------------

func cleanupTaskDetail(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + tdSlug + `'))`
	const rawOf = `(SELECT id FROM raw_source_items WHERE external_id LIKE '` + tdExtPfx + `%')`
	const msgsOf = `(SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + rawOf + `)`
	for _, q := range []string{
		// audit rows by task_id FIRST: audit_events.task_id has no cascade
		// (the SWT-37 landmine). This page writes none, but a prior run of a
		// sibling suite may have.
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM audit_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM classify_promotions WHERE normalized_message_id IN ` + msgsOf,
		`DELETE FROM capture_decisions WHERE message_id IN ` + msgsOf,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM task_dismissals WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + tdSlug + `')`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + rawOf,
		`DELETE FROM normalized_threads WHERE thread_key LIKE '` + tdKeyPfx + `%'`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + rawOf,
		`DELETE FROM ai_runs WHERE worker_type='classify' AND input->>'itest'='taskdetail'`,
		`DELETE FROM raw_source_items WHERE external_id LIKE '` + tdExtPfx + `%'`,
		`DELETE FROM projects WHERE slug='` + tdSlug + `'`,
		`DELETE FROM source_accounts WHERE account_email='` + tdAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// ---- seed ---------------------------------------------------------------------

type tdMsg struct {
	id, raw, thread int64
	body            string
}

type tdFix struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool

	project  int64
	account  int64
	extract  int64 // one ai_extraction, reused by every classify_promotions row
	uid      int
	base     time.Time
	threadOf map[string]int64

	ma1, ma2, mao      tdMsg // thread A (gmail): older inbound, THE source, newest outbound
	mb1                tdMsg // thread B: the loser named by branch 2 AND by an 'attached' promotion
	mc1                tdMsg // thread C: T2's branch-2 source
	mc2, md1           tdMsg // thread D: T2's two losers
	so1, so2           tdMsg // thread S (slack)
	mo1                tdMsg // thread O: outbound only
	mp1                tdMsg // thread P: outbound, and a promotion names it
	mm0, mm1           tdMsg // thread M: a sibling whose body is multi-byte and over the cap
	mt1                tdMsg // truncated gmail capture
	mbig               tdMsg // body longer than sourceBodyCap
	ml0                tdMsg // head of the 55-sibling thread
	tA, tB, tS, tO, tL int64

	t1, t2, t3, t4, t5, t6, t7, t8, t9 int64
	t10, t11                           int64
}

func (f *tdFix) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

func (f *tdFix) thread(label, subject string) int64 {
	f.t.Helper()
	id := bdInsID(f.t, f.ctx, f.pool,
		`INSERT INTO normalized_threads (thread_key, subject) VALUES ($1,$2) RETURNING id`, tdKeyPfx+label, subject)
	f.threadOf[label] = id
	return id
}

type tdSpec struct {
	thread    int64
	channel   string
	direction string
	sender    string
	subject   string
	at        time.Time
	body      string
	rawJSON   json.RawMessage
}

func (f *tdFix) message(label string, sp tdSpec) tdMsg {
	f.t.Helper()
	f.uid++
	ext := fmt.Sprintf("%s%s:%d", tdExtPfx, label, f.uid)
	raw := sp.rawJSON
	if raw == nil {
		raw = json.RawMessage(`{"source":"` + sp.channel + `","itest":"taskdetail"}`)
	}
	rawID := bdInsID(f.t, f.ctx, f.pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,$2,$3,$4,now()) RETURNING id`, f.account, ext, raw, ext)
	var thread any
	if sp.thread != 0 {
		thread = sp.thread
	}
	msgID := bdInsID(f.t, f.ctx, f.pool,
		`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id,
		                                  sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		rawID, thread, sp.direction, "<"+ext+">", sp.at, sp.body, sp.subject, sp.sender, sp.channel)
	return tdMsg{id: msgID, raw: rawID, thread: sp.thread, body: sp.body}
}

// promote writes a classify_promotions row. taskID 0 means SQL NULL — the
// criterion-16 shape (a crash between claim and create).
func (f *tdFix) promote(m tdMsg, action string, taskID int64) {
	f.t.Helper()
	var task any
	if taskID != 0 {
		task = taskID
	}
	f.exec(`INSERT INTO classify_promotions (normalized_message_id, raw_source_item_id, ai_extraction_id,
	                                         project_id, kind, action, task_id, reason)
	        VALUES ($1,$2,$3,$4,'itest','`+action+`',$5,'itest-taskdetail')`,
		m.id, m.raw, f.extract, f.project, task)
}

func (f *tdFix) decide(m tdMsg, action string, taskID int64) {
	f.t.Helper()
	var task any
	if taskID != 0 {
		task = taskID
	}
	f.exec(`INSERT INTO capture_decisions (message_id, raw_source_item_id, mode, action, project_id, task_id, reason)
	        VALUES ($1,$2,'live','`+action+`',$3,$4,'itest-taskdetail')`, m.id, m.raw, f.project, task)
}

func (f *tdFix) task(title, body string, sourceThread int64) int64 {
	f.t.Helper()
	var th any
	if sourceThread != 0 {
		th = sourceThread
	}
	return bdInsID(f.t, f.ctx, f.pool,
		`INSERT INTO tasks (project_id, title, body, assignee_type, status, priority, source_thread_id)
		 VALUES ($1,$2,$3,'human','ready',1,$4) RETURNING id`, f.project, title, body, th)
}

// ---- RFC822 / IMAP envelope fixtures (the maEnvelope shape) -------------------

func tdLeaf(headers []string, body string) string {
	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body + "\r\n"
}

func tdAttach(ctype, filename string, content []byte) string {
	return tdLeaf([]string{
		"Content-Type: " + ctype,
		"Content-Disposition: attachment; filename=\"" + filename + "\"",
		"Content-Transfer-Encoding: base64",
	}, base64.StdEncoding.EncodeToString(content))
}

func tdMulti(boundary string, parts ...string) string {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n" + p)
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

func tdRFC822(from, subject, mid string, at time.Time, entity string) string {
	return strings.Join([]string{
		"From: " + from,
		"To: " + tdAcct,
		"Subject: " + subject,
		"Message-ID: " + mid,
		"Date: " + at.Format(time.RFC1123Z),
		"MIME-Version: 1.0",
	}, "\r\n") + "\r\n" + entity
}

func tdEnvelope(t *testing.T, uid int, msg string, truncated bool, parts []map[string]any) json.RawMessage {
	t.Helper()
	size := len(msg)
	if truncated {
		size = 3_000_000
	}
	env := map[string]any{
		"source": "imap", "folder": "INBOX", "uidvalidity": 4242, "uid": uid,
		"internaldate": "2026-09-16T10:00:00Z", "flags": []string{}, "size": size,
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

// ---- the bodies ---------------------------------------------------------------

// tdSourceBody is M(A2)'s stored body_text: a quoted chain with the sentence
// that matters BELOW it (D5's regression — a naive "cut at the first quote
// marker" stripper deletes exactly that line), plus criterion 11's three
// payloads. Placeholders throughout; the bank host is the one real string and
// it is a HOST, not an address.
func tdSourceBody() string {
	return strings.Join([]string{
		tdSourceMark + " placeholder alert body.",
		"Amount: 100.00 at PLACEHOLDER MERCHANT, card ending 0000.",
		"",
		// Task #513: the whitespace-only lines an HTML-table email converts to.
		// The page shows ONE blank line here; body_text keeps every one.
		" \r",
		"\u00a0",
		"\t ",
		"",
		tdScript,
		tdImg,
		"Track: " + tdTrackTo,
		"",
		"On 9 Sep 2026, Salvador wrote:",
		"> placeholder quoted line one",
		"> placeholder quoted line two",
		"> placeholder quoted line three",
		"-----Original Message-----",
		"> placeholder quoted line four",
		"",
		tdBelowQuote + " — the second question lives below the quote.",
	}, "\n")
}

// tdOlderBody is whitespace-RICH on purpose: its raw first 120 characters and
// its textmatch.NormalizedPrefix(…, 120) differ, so a summary spelled
// `left(body_text,120)` in SQL cannot pass (the SWT-16 one-spelling rule). It is
// also longer than tdThreadBodyCap, with a sentinel past the cut (criterion 9).
func tdOlderBody() string {
	head := tdOlderSibling + "    line one with     wide gaps\n\n\n" +
		"line two   with more     gaps and enough words to run past one hundred and twenty runes of prefix easily\n\n"
	return head + strings.Repeat("filler-placeholder-line\n", 600) + tdPastThreadCap
}

// tdBigBody pads with a TWO-BYTE rune on purpose. sourceBodyCap cuts
// CHARACTERS, so the cut text is ~524 KiB of bytes while the whole body is
// 262,172 characters: a marker that compares byte lengths finds the shown text
// "longer" than the stored body and prints nothing at all, on the one message
// that was actually cut. An ASCII pad cannot tell the two units apart.
func tdBigBody() string {
	head := tdBigHead + " placeholder oversize body.\n"
	pad := strings.Repeat("é", tdSourceBodyCap-len(head))
	return head + pad + tdPastSourceCap
}

func seedTaskDetail(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *tdFix {
	t.Helper()
	f := &tdFix{t: t, ctx: ctx, pool: pool, threadOf: map[string]int64{},
		base: time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)}

	// ai_locality='local_only' deliberately: D9 says the SWT-21 locality gate
	// does NOT apply to this page (it is Salvador's own dashboard, port-forward
	// only, rendering his own mailboxes to his own eyes). Mutation 16 — adding
	// a mailClassJudge gate to the load path — must blank this fixture out and
	// turn the precedence test red. `personal`, the project that prompted the
	// request, is local_only in production.
	f.project = bdInsID(t, ctx, pool,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,$2,'manual','dashboard','/tmp/itest-taskdetail','local_only') RETURNING id`, tdSlug, tdClient)
	f.account = bdInsID(t, ctx, pool,
		`INSERT INTO source_accounts (provider, account_email) VALUES ('google',$1) RETURNING id`, tdAcct)

	extRaw := bdInsID(t, ctx, pool,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,'{"itest":"taskdetail"}'::jsonb,$2) RETURNING id`, f.account, tdExtPfx+"extraction")
	runID := bdInsID(t, ctx, pool,
		`INSERT INTO ai_runs (worker_type, provider, model, input, output, status)
		 VALUES ('classify','openai','gpt-5-mini','{"itest":"taskdetail"}'::jsonb,'{}'::jsonb,'ok') RETURNING id`)
	f.extract = bdInsID(t, ctx, pool,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{}'::jsonb) RETURNING id`,
		runID, extRaw)

	// ---- thread A (gmail): the source thread of T1 and T3 ---------------------
	f.tA = f.thread("A", "itest-taskdetail thread A subject")
	f.uid++
	aEntity := tdMulti("itest-taskdetail-mix",
		tdLeaf([]string{`Content-Type: text/plain; charset="utf-8"`}, tdSourceMark),
		tdAttach("text/plain", "PlaceholderOne.txt", []byte(strings.Repeat("a", 1234))),
		tdAttach("application/json", "PlaceholderTwo.json", []byte(`{"placeholder":"`+strings.Repeat("b", 4321)+`"}`)),
	)
	f.ma1 = f.message("ma1", tdSpec{thread: f.tA, channel: "gmail", direction: "inbound",
		sender: "Placeholder Sender <sender.itest-taskdetail@example.test>", subject: "itest-taskdetail A older",
		at: f.base.Add(-3 * time.Hour), body: tdOlderBody()})
	f.ma2 = f.message("ma2", tdSpec{thread: f.tA, channel: "gmail", direction: "inbound",
		sender: "Placeholder Alerts <alerts.itest-taskdetail@example.test>", subject: "itest-taskdetail A source",
		at: f.base.Add(-2 * time.Hour), body: tdSourceBody(),
		rawJSON: tdEnvelope(t, 9001, tdRFC822("alerts.itest-taskdetail@example.test", "itest-taskdetail A source",
			"<itest-taskdetail-ma2@example.test>", f.base.Add(-2*time.Hour), aEntity), false, nil)})
	f.mao = f.message("mao", tdSpec{thread: f.tA, channel: "gmail", direction: "outbound",
		sender: tdAcct, subject: "Re: itest-taskdetail A source",
		at: f.base.Add(-1 * time.Hour), body: tdOutbound + " placeholder own reply."})

	// ---- thread B: every loser of T1, in one place ----------------------------
	f.tB = f.thread("B", "itest-taskdetail thread B subject")
	f.mb1 = f.message("mb1", tdSpec{thread: f.tB, channel: "gmail", direction: "inbound",
		sender: "Placeholder B <b.itest-taskdetail@example.test>", subject: "itest-taskdetail B loser",
		at: f.base.Add(-90 * time.Minute), body: tdLoser + " placeholder body of the weaker link."})

	// ---- threads C and D: T2's winner and its two losers ----------------------
	tC := f.thread("C", "itest-taskdetail thread C subject")
	tD := f.thread("D", "itest-taskdetail thread D subject")
	f.mc1 = f.message("mc1", tdSpec{thread: tC, channel: "gmail", direction: "inbound",
		sender: "Placeholder C <c.itest-taskdetail@example.test>", subject: "itest-taskdetail C capture source",
		at: f.base.Add(-4 * time.Hour), body: tdCaptureSource + " placeholder capture-created body."})
	f.mc2 = f.message("mc2", tdSpec{thread: tD, channel: "gmail", direction: "inbound",
		sender: "Placeholder D <d.itest-taskdetail@example.test>", subject: "itest-taskdetail D attached",
		at: f.base.Add(-5 * time.Hour), body: tdAttachedLoser + " placeholder later message that joined the task."})
	f.md1 = f.message("md1", tdSpec{thread: tD, channel: "gmail", direction: "inbound",
		sender: "Placeholder D <d.itest-taskdetail@example.test>", subject: "itest-taskdetail D task_log",
		at: f.base.Add(-6 * time.Hour), body: tdTaskLogLoser + " placeholder appended message."})

	// ---- thread S (slack) -----------------------------------------------------
	f.tS = f.thread("S", "itest-taskdetail thread S subject")
	f.so1 = f.message("so1", tdSpec{thread: f.tS, channel: "slack", direction: "inbound",
		sender: "placeholder.slack.user", subject: "",
		at: f.base.Add(-7 * time.Hour), body: tdSlackSource + " placeholder slack message body."})
	f.so2 = f.message("so2", tdSpec{thread: f.tS, channel: "slack", direction: "inbound",
		sender: "placeholder.slack.other", subject: "",
		at: f.base.Add(-6*time.Hour - 30*time.Minute), body: tdSlackSibling + " placeholder slack reply."})

	// ---- thread O: outbound only (criterion 16) -------------------------------
	f.tO = f.thread("O", "itest-taskdetail thread O subject")
	f.mo1 = f.message("mo1", tdSpec{thread: f.tO, channel: "gmail", direction: "outbound",
		sender: tdAcct, subject: "itest-taskdetail O outbound only",
		at: f.base.Add(-8 * time.Hour), body: "placeholder outbound-only body."})

	// ---- thread P: outbound, and a promotion names it (criterion 7) -----------
	//
	// Separate from thread O because classify_promotions is unique per message
	// and O's promotion is deliberately task-less.
	tP := f.thread("P", "itest-taskdetail thread P subject")
	f.mp1 = f.message("mp1", tdSpec{thread: tP, channel: "gmail", direction: "outbound",
		sender: tdAcct, subject: "itest-taskdetail P our own send",
		at: f.base.Add(-7 * time.Hour), body: "placeholder our-own-send body."})

	// ---- the truncated capture (criterion 20) ---------------------------------
	tT := f.thread("T", "itest-taskdetail thread T subject")
	f.mt1 = f.message("mt1", tdSpec{thread: tT, channel: "gmail", direction: "inbound",
		sender: "Placeholder T <t.itest-taskdetail@example.test>", subject: "itest-taskdetail T truncated",
		at: f.base.Add(-9 * time.Hour), body: tdTruncSource + " placeholder body of an oversize capture.",
		rawJSON: tdEnvelope(t, 9002, tdRFC822("t.itest-taskdetail@example.test", "itest-taskdetail T truncated",
			"<itest-taskdetail-mt1@example.test>", f.base.Add(-9*time.Hour), "Content-Type: text/plain\r\n\r\nbody\r\n"),
			true, []map[string]any{
				{"part_id": "2", "filename": "PlaceholderBig.pdf", "content_type": "application/pdf", "size": 987654},
				{"part_id": "3", "filename": "PlaceholderBig.csv", "content_type": "text/csv", "size": 123456},
			})})

	// ---- the oversize body (criterion 8) --------------------------------------
	tE := f.thread("E", "itest-taskdetail thread E subject")
	f.mbig = f.message("mbig", tdSpec{thread: tE, channel: "gmail", direction: "inbound",
		sender: "Placeholder E <e.itest-taskdetail@example.test>", subject: "itest-taskdetail E oversize",
		at: f.base.Add(-10 * time.Hour), body: tdBigBody()})

	// ---- thread M: a multi-byte sibling over the thread cap (criterion 9) -----
	//
	// Every other oversize fixture is ASCII, where a character count and a byte
	// count agree and a marker written in either unit looks right. This body is
	// 20,000 three-byte runes: left(body_text, 8192) cuts 8,192 CHARACTERS, whose
	// 24,576 BYTES exceed the stored character count — so a marker guarded by
	// len(body) is silently skipped on exactly the message it describes.
	tM := f.thread("M", "itest-taskdetail thread M subject")
	f.mm0 = f.message("mm0", tdSpec{thread: tM, channel: "gmail", direction: "inbound",
		sender: "Placeholder M <m.itest-taskdetail@example.test>", subject: "itest-taskdetail M source",
		at: f.base.Add(-6 * time.Hour), body: "placeholder M source body."})
	f.mm1 = f.message("mm1", tdSpec{thread: tM, channel: "gmail", direction: "inbound",
		sender: "Placeholder M <m.itest-taskdetail@example.test>", subject: "itest-taskdetail M sibling",
		at: f.base.Add(-5 * time.Hour), body: strings.Repeat("€", tdMultiByteChars)})

	// ---- thread L: the head plus 55 siblings (criterion 10) -------------------
	f.tL = f.thread("L", "itest-taskdetail thread L subject")
	f.ml0 = f.message("ml0", tdSpec{thread: f.tL, channel: "gmail", direction: "inbound",
		sender: "Placeholder L <l.itest-taskdetail@example.test>", subject: "itest-taskdetail L head",
		at: f.base.Add(-20 * time.Hour), body: tdLongThread + " placeholder head of a long thread."})
	f.exec(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
	        SELECT $1, $2 || g, '{"itest":"taskdetail"}'::jsonb, $2 || g, now()
	          FROM generate_series(1,$3) g`, f.account, tdExtPfx+"ml-sib-", tdLongThreadSiblings)
	f.exec(`INSERT INTO normalized_messages (raw_source_item_id, thread_id, direction, external_message_id,
	                                         sent_at, body_text, subject, sender, channel)
	        SELECT r.id, $1, 'inbound', '<' || r.external_id || '>',
	               $2::timestamptz + make_interval(mins => (substring(r.external_id from '[0-9]+$'))::int),
	               $3 || ' sibling ' || substring(r.external_id from '[0-9]+$'),
	               'itest-taskdetail L sibling', 'Placeholder L <l.itest-taskdetail@example.test>', 'gmail'
	          FROM raw_source_items r WHERE r.external_id LIKE $4`,
		f.tL, f.base.Add(-19*time.Hour), tdLongThread, tdExtPfx+"ml-sib-%")

	// ---- the tasks ------------------------------------------------------------
	//
	// T1 carries THREE links naming TWO different messages, and the two weaker
	// ones both point at MB1 / thread B:
	//   branch 1  classify_promotions action='task'     → MA2   (wins)
	//   branch 1' classify_promotions action='attached' → MB1   (inserted FIRST, so it has the LOWER id)
	//   branch 2  capture_decisions  action='task'      → MB1
	//   branch 3  tasks.source_thread_id                → thread B
	// Production currently has ZERO such disagreements (measured 2026-09-17: 17
	// open tasks carry 2+ links, 0 of them disagree), so this fixture is the
	// ONLY place the precedence is exercised. It has to be deliberate.
	f.t1 = f.task("TASKDETAIL T1 promotion wins", "T1 placeholder card body.", f.tB)
	f.promote(f.mb1, "attached", f.t1)
	f.promote(f.ma2, "task", f.t1)
	f.decide(f.mb1, "task", f.t1)

	// T2: the only promotion is 'attached' (criterion 2 → falls through), and the
	// task_log decision is inserted FIRST so a branch 2 that forgets
	// `action='task'` picks the loser by id (criterion 3, mutation 6).
	f.t2 = f.task("TASKDETAIL T2 capture decision wins", "T2 placeholder card body.", 0)
	f.promote(f.mc2, "attached", f.t2)
	f.decide(f.md1, "task_log", f.t2)
	f.decide(f.mc1, "task", f.t2)

	// T3: thread only. MAO is the NEWEST message on thread A and is outbound;
	// MA1 is an older inbound. Criterion 4 resolves MA2 and never MAO.
	f.t3 = f.task("TASKDETAIL T3 thread only", "T3 placeholder card body.", f.tA)

	// T4: slack (criteria 18 rendered, 21).
	f.t4 = f.task("TASKDETAIL T4 slack", "T4 placeholder card body.", 0)
	f.promote(f.so1, "task", f.t4)

	// T5: NOTHING. 33 of 51 open production tasks look like this — the majority
	// rendering (criteria 15, 22).
	f.t5 = f.task(tdNoLinkTitle, tdNoLinkBody, 0)

	// T6: an unresolvable link is the no-link case, not an error (criterion 16):
	// a promotion row whose task_id is NULL, plus a source_thread_id whose
	// thread has no inbound message.
	f.t6 = f.task(tdUnresolvableTitle, tdUnresolvableBody, f.tO)
	f.promote(f.mo1, "task", 0)

	// T7, T8, T9.
	f.t7 = f.task("TASKDETAIL T7 truncated capture", "T7 placeholder card body.", 0)
	f.promote(f.mt1, "task", f.t7)
	f.t8 = f.task("TASKDETAIL T8 oversize body", "T8 placeholder card body.", 0)
	f.promote(f.mbig, "task", f.t8)
	f.t9 = f.task("TASKDETAIL T9 long thread", "T9 placeholder card body.", 0)
	f.promote(f.ml0, "task", f.t9)

	// T10: a branch-1 promotion naming an OUTBOUND message. Nothing in the
	// schema forbids it — only branch 3 filters on direction — and without this
	// task every message the section can resolve is inbound, so the rendered
	// direction would be satisfied by a literal and mutation 4 would survive on
	// the load statement. Thread O holds this message alone, so the assertion
	// is about the header field and nothing else.
	f.t10 = f.task("TASKDETAIL T10 outbound source", "T10 placeholder card body.", 0)
	f.promote(f.mp1, "task", f.t10)

	// T11: the multi-byte sibling (criterion 9, in characters).
	f.t11 = f.task("TASKDETAIL T11 multibyte sibling", "T11 placeholder card body.", 0)
	f.promote(f.mm0, "task", f.t11)
	return f
}

const tdLongThreadSiblings = 55 // > tdThreadMax, so criterion 10's marker must fire

// tdMultiByteChars is over tdThreadBodyCap in CHARACTERS while the cut text is
// over that many BYTES — the gap the cap marker has to be measured in.
const tdMultiByteChars = 20000

// ---- rendering helpers --------------------------------------------------------

const tdSectionOpen = `<section class="source-message">`

// tdSection slices the RENDERED section.
//
// CONTRACT DECISION (confirm at implementation): html/template STRIPS HTML
// comments from its output — verified — so D4's `<!-- source-message: begin -->`
// markers exist in task.html's SOURCE (where the structure test slices on them)
// but can never appear in a response. The rendered section is therefore
// delimited by a single `<section class="source-message">` … `</section>`,
// with no nested <section>, placed INSIDE the marker comments. Slicing is the
// whole point of the markers, and a scan of the whole page would let the nav's
// own <a href> mask a failure.
func tdSection(t *testing.T, page string) (string, bool) {
	t.Helper()
	i := strings.Index(page, tdSectionOpen)
	if i < 0 {
		return "", false
	}
	rest := page[i+len(tdSectionOpen):]
	j := strings.Index(rest, "</section>")
	if j < 0 {
		t.Fatalf("the page opens %s and never closes it", tdSectionOpen)
	}
	if k := strings.Index(rest[:j], "<section"); k >= 0 {
		t.Fatalf("the source-message section nests another <section> at offset %d: the slice must be unambiguous", k)
	}
	return rest[:j], true
}

func tdMustSection(t *testing.T, name, page string) string {
	t.Helper()
	sec, ok := tdSection(t, page)
	if !ok {
		// Dump from <body>: the <style> block is 600 characters of noise.
		body := page
		if i := strings.Index(body, "<body"); i >= 0 {
			body = body[i:]
		}
		t.Fatalf("%s: the page has no %s block. The task detail page must show the message the task came from "+
			"(SPEC Goal)\n%s", name, tdSectionOpen, snippet(body))
	}
	return sec
}

// tdSplit cuts the section into the part rendered INLINE (the source message)
// and the collapsed part (everything from the first <details>). D3: one message
// in full, the rest of the thread collapsed.
func tdSplit(sec string) (inline, collapsed string) {
	i := strings.Index(sec, "<details")
	if i < 0 {
		return sec, ""
	}
	return sec[:i], sec[i:]
}

func tdEsc(s string) string { return html.EscapeString(s) }

func tdWant(t *testing.T, name, where, hay, needle, why string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("%s: %s does not contain %q — %s", name, where, needle, why)
	}
}

func tdNotWant(t *testing.T, name, where, hay, needle, why string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Errorf("%s: %s contains %q and must not — %s", name, where, needle, why)
	}
}

func tdGetTask(t *testing.T, client *http.Client, base string, id int64) string {
	t.Helper()
	code, body := get(t, client, base+"/tasks/"+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("GET /tasks/%d = %d\n%s", id, code, snippet(body))
	}
	return body
}

// ---- criteria 1, 2, 5, 7, 9, 10, 11, 19: T1 -----------------------------------

func TestTaskDetail_Integration_PrecedenceAndSourceMessage(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// POSITIVE CONTROLS on the fixture itself, BEFORE any render assertion. A
	// fixture shaped like the assertion certifies nothing (the SWT-21(6)
	// lesson), so each of these states a fact about the seeded COLUMNS that the
	// assertions below depend on.
	atts, info, err := google.ListAttachments(tdRawJSON(t, ctx, pool, f.ma2.raw))
	if err != nil {
		t.Fatalf("T1: google.ListAttachments on the fixture envelope: %v", err)
	}
	if len(atts) != 2 || info.Truncated {
		t.Fatalf("T1: POSITIVE CONTROL FAILED — the fixture envelope lists %d attachments (want 2), truncated=%v "+
			"(want false)", len(atts), info.Truncated)
	}
	if raw := string([]rune(tdOlderBody())[:tdSummaryPrefix]); raw == textmatch.NormalizedPrefix(tdOlderBody(), tdSummaryPrefix) {
		t.Fatalf("T1: POSITIVE CONTROL FAILED — the older sibling's raw 120-rune prefix equals its normalized one, so " +
			"a summary spelled left(body_text,120) in SQL would pass (the SWT-16 landmine: a matcher test whose two " +
			"bodies are the same string tests nothing)")
	}
	if utf8.RuneCountInString(tdOlderBody()) <= tdThreadBodyCap {
		t.Fatalf("T1: POSITIVE CONTROL FAILED — the older sibling's body is %d bytes, not longer than the thread body "+
			"cap %d", utf8.RuneCountInString(tdOlderBody()), tdThreadBodyCap)
	}
	var threadA, outboundA int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE direction='outbound') FROM normalized_messages WHERE thread_id=$1`,
		f.tA).Scan(&threadA, &outboundA); err != nil {
		t.Fatalf("count thread A: %v", err)
	}
	if threadA != 3 || outboundA != 1 {
		t.Fatalf("T1: POSITIVE CONTROL FAILED — thread A holds %d messages, %d outbound (want 3, 1): the direction "+
			"assertions need two rows that DISAGREE on that column, or a literal satisfies them", threadA, outboundA)
	}

	page := tdGetTask(t, client, ts.URL, f.t1)
	sec := tdMustSection(t, "T1", page)
	inline, collapsed := tdSplit(sec)

	// --- criterion 1: the promotion wins over the capture decision AND the thread.
	tdWant(t, "T1", "the inline source block", inline, tdEsc(tdSourceMark),
		"criterion 1: the lowest-id classify_promotions row with action in (task,review) names MA2 — the most "+
			"precise claim in the database (the row exists BECAUSE that message made that task)")
	tdNotWant(t, "T1", "the page", page, tdEsc(tdLoser),
		"criteria 1-2: MB1 is named by an 'attached' promotion (lower id), by a capture_decisions action='task' row "+
			"AND by tasks.source_thread_id. A weaker claim never displaces a stronger one, and the OTHER candidates "+
			"do not appear anywhere on the page (D1)")

	// --- criterion 5: the thread shown is the RESOLVED message's own thread_id,
	// never tasks.source_thread_id (which is thread B here).
	tdWant(t, "T1", "the collapsed block", collapsed, tdEsc(tdOlderSibling),
		"criterion 5: the thread is the resolved message's own thread (A), so MA1 is a sibling")
	tdWant(t, "T1", "the collapsed block", collapsed, tdEsc(tdOutbound),
		"criterion 5 / invariant 5: our own send re-entered through ingestion is shown as CONTEXT on the thread")

	// --- criterion 7: the header fields and the body, in full, verbatim.
	for _, want := range []string{
		tdEsc("Placeholder Alerts <alerts.itest-taskdetail@example.test>"),
		tdEsc("itest-taskdetail A source"),
		"inbound",
	} {
		tdWant(t, "T1", "the inline source block", inline, want, "criterion 7: sender, subject, sent_at, direction")
	}
	if !regexp.MustCompile(`2026-09-16`).MatchString(inline) {
		t.Errorf("T1: the inline source block carries no sent_at date (criterion 7)")
	}
	// D5: no quote stripper. The sentence BELOW the quoted chain is exactly what
	// a naive "cut at the first quote marker" rule deletes.
	tdWant(t, "T1", "the inline source block", inline, tdEsc(tdBelowQuote),
		"D5: the source body is VERBATIM — no > stripping, no 'On <date> wrote:' stripping, no "+
			"-----Original Message----- stripping. Collapsing is reversible in one tap; stripping is invisible")
	tdWant(t, "T1", "the inline source block", inline, tdEsc("> placeholder quoted line four"),
		"D5: the quoted chain itself is kept, collapsed by POSITION (it lives in the other messages), never parsed")

	// --- task #513: whitespace-only lines are tidied for DISPLAY, never in the column.
	var storedRun bool
	if err := pool.QueryRow(ctx, `SELECT strpos(body_text, E'0000.\n\n \r\n') > 0 FROM normalized_messages WHERE id = $1`,
		f.ma2.id).Scan(&storedRun); err != nil || !storedRun {
		t.Fatalf("T1: POSITIVE CONTROL FAILED — the stored source body does not carry the whitespace-only run "+
			"(err=%v): the display tidy below would pass on a fixture that never had one", err)
	}
	tdWant(t, "T1", "the inline source block", inline, tdEsc("card ending 0000.\n\n"+tdScript),
		"task #513: a run of blank and whitespace-only lines (CR, U+00A0, tab) renders as ONE blank line")

	// --- criterion 10: the source message is never shown twice.
	if n := strings.Count(sec, tdEsc(tdSourceMark)); n != 1 {
		t.Errorf("T1: the source marker appears %d times in the section, want exactly 1 — the source message is "+
			"excluded from the thread list BY ID and never shown twice (criterion 10)", n)
	}
	if n := strings.Count(collapsed, "<details"); n != 2 {
		t.Errorf("T1: the collapsed block holds %d <details>, want 2 (thread A has three messages and one of them "+
			"is the source) — criteria 9, 10", n)
	}

	// --- criterion 9: ordering, the summary spelling, the 8 KiB body cut.
	if i, j := strings.Index(collapsed, tdEsc(tdOlderSibling)), strings.Index(collapsed, tdEsc(tdOutbound)); i < 0 || j < 0 || i > j {
		t.Errorf("T1: the thread is not ordered `sent_at NULLS LAST, id` ascending (older sibling at %d, outbound at "+
			"%d) — byte-identical to mailReadThread's ORDER BY (criterion 9 / D3)", i, j)
	}
	tdWant(t, "T1", "the collapsed block", collapsed, "outbound",
		"criterion 9: the summary line is direction, sender, sent_at, prefix — and the direction comes from the "+
			"COLUMN: thread A holds both an inbound and an outbound sibling, so a literal cannot satisfy both")
	prefix := textmatch.NormalizedPrefix(tdOlderBody(), tdSummaryPrefix)
	tdWant(t, "T1", "the collapsed block", collapsed, tdEsc(prefix),
		"criterion 9 / SWT-16: the summary's first 120 runes come from textmatch.NormalizedPrefix — ONE spelling. "+
			"This body's raw prefix and its whitespace-collapsed prefix differ, so left(body_text,120) in SQL fails here")
	tdNotWant(t, "T1", "the collapsed block", collapsed, tdEsc(tdPastThreadCap),
		fmt.Sprintf("criterion 9: a thread message's body is cut at tools.MailThreadBodyCap (%d), the same width "+
			"mail_read_thread uses (D8)", tdThreadBodyCap))
	tdCapMarker(t, "T1", collapsed, tdThreadBodyCap, utf8.RuneCountInString(tdOlderBody()))

	// --- criterion 11: untrusted text is INERT.
	tdWant(t, "T1", "the page", page, "&lt;script&gt;alert(1)&lt;/script&gt;",
		"criterion 11: the script payload renders as TEXT")
	tdWant(t, "T1", "the inline source block", inline, tdEsc(tdImg), "criterion 11: the img payload renders as TEXT")
	tdWant(t, "T1", "the inline source block", inline, tdTrackTo,
		"criterion 11: the tracking URL renders as TEXT — inert, selectable, not tappable")
	tdAssertInert(t, "T1", sec)

	// --- criterion 19: the attachment manifest — names, types, sizes. No bytes.
	for _, a := range atts {
		tdWant(t, "T1", "the section", sec, tdEsc(a.Filename), "criterion 19 / D6: filename")
		tdWant(t, "T1", "the section", sec, tdEsc(a.ContentType), "criterion 19 / D6: type, the mail_list_attachments vocabulary")
		tdWant(t, "T1", "the section", sec, strconv.Itoa(a.SizeBytes), "criterion 19 / D6: size from google.Attachment.SizeBytes")
	}
	tdNotWant(t, "T1", "the section", sec, "(encoded)",
		"D6: `(encoded)` is suffixed only when SizeIsEncoded — a complete capture reports DECODED sizes")
	tdNotWant(t, "T1", "the page", page, strings.Repeat("a", 200),
		"criterion 19 / D6: NO content, no download link, no byte is served. The dashboard never calls "+
			"google.ReadAttachment and adds no route")
	tdNotWant(t, "T1", "the page", page, "/tasks/"+strconv.FormatInt(f.t1, 10)+"/attachments",
		"criterion 19: no new route")
}

// tdCapMarker asserts ONE line naming both counts (criteria 8, 9: "exactly one
// marker line naming the shown and stored character counts"). The wording is
// the implementation's; the two numbers are the contract.
func tdCapMarker(t *testing.T, name, hay string, shown, stored int) {
	t.Helper()
	s, st := strconv.Itoa(shown), strconv.Itoa(stored)
	hits := 0
	for _, line := range strings.Split(hay, "\n") {
		if strings.Contains(line, s) && strings.Contains(line, st) {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("%s: %d lines name both the shown (%s) and the stored (%s) character counts, want exactly 1 — "+
			"when the stored body is longer the page prints ONE explicit marker line (criteria 8, 9 / D3)",
			name, hits, s, st)
	}
}

// tdInertProblems is criterion 12 applied to a RENDERED section: every HTML TAG
// in the slice must come from the sink-free set, and none may carry href, src or
// an on* handler. Escaped body text (`&lt;img src=x onerror=alert(1)&gt;`) is not
// a tag and is deliberately invisible here — asserting "the slice contains no
// `src=`" would fail on a CORRECT render, because the escaped payload contains
// that text.
//
// It returns its findings rather than reporting them so that
// TestTaskDetail_InertScannerIsNotVacuous can prove the scanner bites. A scan
// that silently finds nothing is the shape of every landmine in
// INSTITUTIONAL_KNOWLEDGE.md.
func tdInertProblems(sec string) []string {
	allowed := map[string]bool{
		"section": true, "h2": true, "h3": true, "p": true, "pre": true, "div": true, "span": true,
		"details": true, "summary": true, "table": true, "thead": true, "tbody": true,
		"tr": true, "th": true, "td": true, "ul": true, "li": true, "code": true,
		"em": true, "strong": true, "br": true, "hr": true, "small": true,
	}
	banned := regexp.MustCompile(`(?i)\s(href|src|srcset|style|on[a-z]+)\s*=`)
	var out []string
	for _, m := range regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)([^>]*)>`).FindAllStringSubmatch(sec, -1) {
		tag := strings.ToLower(m[2])
		if !allowed[tag] {
			out = append(out, fmt.Sprintf("the section renders a <%s> element (%q). Criteria 11-12 / D4: no "+
				"auto-linking (a URL in a body is inert text he opens deliberately — turning bank-alert URLs into "+
				"one-tap anchors is a phishing surface he did not ask for), no remote images, no HTML-mail rendering",
				tag, strings.TrimSpace(m[0])))
		}
		if m[1] == "" {
			if a := banned.FindString(m[3]); a != "" {
				out = append(out, fmt.Sprintf("the section's <%s> carries %q (%q). Criterion 12: NO attribute takes a "+
					"body-derived value, so there is no sink to escape out of", tag, strings.TrimSpace(a), strings.TrimSpace(m[0])))
			}
		}
		if tag == "details" && regexp.MustCompile(`(?i)\bopen\b`).MatchString(m[3]) {
			out = append(out, fmt.Sprintf("the section renders %q — no <details> is rendered open (criteria 9, 12)",
				strings.TrimSpace(m[0])))
		}
	}
	for _, b := range []string{"<script", "<img", "<iframe", "<a ", "<object", "<embed"} {
		if strings.Contains(strings.ToLower(sec), b) {
			out = append(out, fmt.Sprintf("the section contains %q (criterion 11)", b))
		}
	}
	return out
}

func tdAssertInert(t *testing.T, name, sec string) {
	t.Helper()
	for _, p := range tdInertProblems(sec) {
		t.Errorf("%s: %s", name, p)
	}
}

// The scanner's own positive control: it runs over slices that are EMPTY today
// (the section does not exist), so nothing else proves it can fail.
func TestTaskDetail_InertScannerIsNotVacuous(t *testing.T) {
	clean := `<section class="source-message"><h2>Source email</h2><pre>` +
		html.EscapeString(tdScript+" "+tdImg+" "+tdTrackTo) +
		`</pre><details><summary>inbound</summary><pre>body</pre></details></section>`
	if got := tdInertProblems(clean); len(got) != 0 {
		t.Errorf("the inert scanner rejects a CORRECT render: %v", got)
	}
	for _, bad := range []struct{ name, markup string }{
		{"mutation 11 (bare URLs linkified)", `<p>Track: <a href="` + tdTrackTo + `">` + tdTrackTo + `</a></p>`},
		{"mutation 10 (body rendered with template.HTML)", `<pre>` + tdScript + `</pre>`},
		{"mutation 10 (an img payload rendered as markup)", `<pre>` + tdImg + `</pre>`},
		{"mutation 14 (<details open>)", `<details open><summary>x</summary></details>`},
		{"a style attribute fed from a body", `<pre style="x">body</pre>`},
	} {
		if got := tdInertProblems(bad.markup); len(got) == 0 {
			t.Errorf("POSITIVE CONTROL FAILED: the inert scanner accepts %s: %q", bad.name, bad.markup)
		}
	}
}

func tdRawJSON(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rawID int64) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT raw_json FROM raw_source_items WHERE id=$1`, rawID).Scan(&raw); err != nil {
		t.Fatalf("read raw_json %d: %v", rawID, err)
	}
	return raw
}

// ---- criteria 2, 3: T2 ---------------------------------------------------------

func TestTaskDetail_Integration_AttachedAndTaskLogNeverResolve(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	page := tdGetTask(t, client, ts.URL, f.t2)
	sec := tdMustSection(t, "T2", page)
	inline, _ := tdSplit(sec)

	tdWant(t, "T2", "the inline source block", inline, tdEsc(tdCaptureSource),
		"criterion 3: with no qualifying promotion the section shows the lowest-id capture_decisions row with "+
			"action='task'")
	tdNotWant(t, "T2", "the page", page, tdEsc(tdAttachedLoser),
		"criterion 2: an action='attached' promotion carries a task_id too, and names a LATER message that joined an "+
			"existing task — not the one that raised it. The action filter is load-bearing")
	tdNotWant(t, "T2", "the page", page, tdEsc(tdTaskLogLoser),
		"criterion 3: action='task_log' is capture's later-message append (rules_store.go:1515) and never resolves "+
			"the source. It was inserted FIRST, so it has the lower id: a branch that forgets the action filter picks it")
	tdWant(t, "T2", "the section", sec, "Source email",
		"criterion 18 / D7: a capture-resolved gmail message is a branch-2 claim — 'Source email', not the branch-3 wording")
}

// ---- criterion 4: T3 -----------------------------------------------------------

func TestTaskDetail_Integration_LatestInboundOnTheSourceThread(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	page := tdGetTask(t, client, ts.URL, f.t3)
	sec := tdMustSection(t, "T3", page)
	inline, collapsed := tdSplit(sec)

	tdWant(t, "T3", "the inline source block", inline, tdEsc(tdSourceMark),
		"criterion 4: thread A's LATEST INBOUND message by tools.LatestInboundOrder (sent_at DESC, id DESC) — the "+
			"same predicate and order tools.latestInboundMessage uses, so the page picks the message a reply would answer")
	tdNotWant(t, "T3", "the inline source block", inline, tdEsc(tdOutbound),
		"criterion 4 / invariant 5: MAO is the NEWEST message on the thread and is OUTBOUND — our own send, "+
			"re-entered through ingestion. It must never look like the thing that raised the task")
	tdNotWant(t, "T3", "the inline source block", inline, tdEsc(tdOlderSibling),
		"criterion 4: a later inbound message supersedes an earlier one")
	tdWant(t, "T3", "the collapsed block", collapsed, tdEsc(tdOutbound),
		"criterion 4: the outbound message is still THERE, as context, in the collapsed thread")

	// D7: branch 3 picks a message the database never claimed raised the task,
	// and the heading must not overclaim.
	tdWant(t, "T3", "the section", sec, "Latest email on the source thread",
		"criterion 18 / D7: the branch-3 wording is not cosmetic")
	tdNotWant(t, "T3", "the section", sec, "Source email",
		"criterion 18 / D7: branch 3 must not use branch 1/2's wording")
}

// ---- criterion 7 (the direction COLUMN): T10 -----------------------------------

// The rendered direction must come from normalized_messages.direction, not from
// the fact that almost every source message happens to be inbound. Thread O's
// only message is outbound and a branch-1 promotion names it, so a literal
// 'inbound' in the load statement puts a word on the page that is not in the
// database (mutation 4).
func TestTaskDetail_Integration_SourceDirectionComesFromTheColumn(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// POSITIVE CONTROL: the fixture only proves anything while this message is
	// outbound and alone on its thread.
	var direction string
	var onThread int
	if err := pool.QueryRow(ctx,
		`SELECT m.direction, (SELECT count(*) FROM normalized_messages x WHERE x.thread_id = m.thread_id)
		   FROM normalized_messages m WHERE m.id = $1`, f.mp1.id).Scan(&direction, &onThread); err != nil {
		t.Fatalf("T10: read the fixture message: %v", err)
	}
	if direction != "outbound" || onThread != 1 {
		t.Fatalf("T10: POSITIVE CONTROL FAILED — the fixture message is %q with %d messages on its thread "+
			"(want outbound, 1): the assertion below cannot tell a column from a literal", direction, onThread)
	}

	page := tdGetTask(t, client, ts.URL, f.t10)
	sec := tdMustSection(t, "T10", page)
	inline, _ := tdSplit(sec)

	tdWant(t, "T10", "the inline source block", inline, "outbound",
		"criterion 7: the direction is read from the COLUMN")
	// "outbound" does not contain "inbound", so this is exact: the only way the
	// word appears is a literal in the load statement.
	tdNotWant(t, "T10", "the inline source block", inline, "inbound",
		"criterion 7 / mutation 4: this message is outbound in the database. A load statement that selects a "+
			"literal 'inbound' — or renders a constant — labels it wrongly, and no other fixture would notice")
}

// ---- criterion 9 (the cap marker, in characters): T11 --------------------------

// The thread cap cuts characters, so the marker that reports it must count
// characters. Byte-counting silently suppresses the marker on any body whose
// runes are wider than one byte — the text is dropped and the page says nothing.
func TestTaskDetail_Integration_CapMarkerCountsCharactersNotBytes(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	// POSITIVE CONTROL, asked of Postgres: the cut text must be LONGER in bytes
	// than the whole body is in characters. That inequality is the entire point
	// of the fixture — without it a byte-counting marker would look correct.
	var chars, cutBytes int
	if err := pool.QueryRow(ctx,
		`SELECT length(body_text), octet_length(left(body_text, $2)) FROM normalized_messages WHERE id = $1`,
		f.mm1.id, tdThreadBodyCap).Scan(&chars, &cutBytes); err != nil {
		t.Fatalf("T11: read the fixture body: %v", err)
	}
	if chars != tdMultiByteChars || cutBytes <= chars {
		t.Fatalf("T11: POSITIVE CONTROL FAILED — the sibling is %d characters and its first %d characters are %d "+
			"bytes (want %d characters and MORE than that many bytes): a byte-counting marker would pass here",
			chars, tdThreadBodyCap, cutBytes, tdMultiByteChars)
	}

	page := tdGetTask(t, client, ts.URL, f.t11)
	sec := tdMustSection(t, "T11", page)
	_, collapsed := tdSplit(sec)

	tdCapMarker(t, "T11", collapsed, tdThreadBodyCap, tdMultiByteChars)
}

// ---- criteria 18 (rendered), 21: T4 --------------------------------------------

func TestTaskDetail_Integration_SlackSourceHasNoAttachmentSubSection(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	page := tdGetTask(t, client, ts.URL, f.t4)
	sec := tdMustSection(t, "T4", page)
	inline, collapsed := tdSplit(sec)

	// D7: four connectors write normalized_messages and the section renders ANY
	// of them. The channel is read from the COLUMN — T1 resolves a gmail message
	// and T4 a slack one, so a literal 'gmail' in the load statement cannot
	// satisfy both.
	tdWant(t, "T4", "the section", sec, "Source Slack message", "criterion 18 / D7: the heading names the channel")
	tdNotWant(t, "T4", "the section", sec, "Source email", "criterion 18: a slack message is not an email")
	tdWant(t, "T4", "the inline source block", inline, tdEsc(tdSlackSource), "criterion 21: the body renders normally")
	tdWant(t, "T4", "the collapsed block", collapsed, tdEsc(tdSlackSibling), "criterion 21: the thread renders normally")
	if strings.Contains(strings.ToLower(sec), "attachment") {
		t.Errorf("T4: the section mentions attachments. Criterion 21 / D6: google.ListAttachments walks the IMAP "+
			"rfc822_b64 envelope, and a slack / upwork / jira raw_json has no such envelope — a non-gmail source "+
			"message renders NO attachment sub-section, not an empty one\n%s", snippet(sec))
	}
	tdAssertInert(t, "T4", sec)
}

// ---- criteria 15, 16, 17: the no-link case, the majority rendering -------------

const tdGoldenPath = "testdata/task_detail_nolink.golden.html"

var tdStyleRE = regexp.MustCompile(`(?s)<style>.*?</style>`)

// tdNormalize makes a rendered page comparable across runs: the task id, the
// updated_at stamp and the <style> block are replaced by placeholders.
//
// CONTRACT DECISION (confirm at implementation): criterion 15 says the no-link
// page is "byte-identical to the same page rendered by the pre-change binary".
// The <style> block is excluded from that comparison and ONLY from that
// comparison, because "Files likely to touch" gives task.html "CSS for the
// summary line only" — new CSS lands in the shared <style> block on every page,
// so a literal whole-file byte comparison is unsatisfiable as written. The
// existing rules are pinned instead, verbatim, by the structure test. Everything
// else — every byte of markup, every newline the {{if .SourceMessage}} block
// might leave behind — is compared exactly.
func tdNormalize(page string, id int64) string {
	s := tdStyleRE.ReplaceAllString(page, "<style>{{STYLE}}</style>")
	// The stamp goes first: a timestamp can contain the id's digits. Its zone
	// offset arrives HTML-escaped (`&#43;00`), which is itself worth pinning —
	// html/template escaped it, nothing else did.
	s = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)?(&#43;|\+|-)\d{2}`).ReplaceAllString(s, "{{STAMP}}")
	// The id is masked only where the page STRUCTURALLY renders it (the <title>
	// and the <h1>), never as a bare number: `<td>1</td>` is the priority, and
	// masking it would make the golden depend on which serial ids this run drew.
	n := strconv.FormatInt(id, 10)
	s = strings.ReplaceAll(s, "task "+n+"</title>", "task {{ID}}</title>")
	return strings.ReplaceAll(s, "<h1>#"+n+" ", "<h1>#{{ID}} ")
}

func TestTaskDetail_Integration_NoLinkRendersExactlyAsBefore(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	page := tdGetTask(t, client, ts.URL, f.t5)
	got := tdNormalize(page, f.t5)

	if os.Getenv("TD_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(tdGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Fatalf("golden %s written from the CURRENT binary; re-run without TD_UPDATE_GOLDEN to compare against it",
			tdGoldenPath)
	}
	want, err := os.ReadFile(tdGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v — criterion 15 is a BYTE comparison against the page as the PRE-CHANGE binary "+
			"rendered it. Capture it once, on unmodified main, with TD_UPDATE_GOLDEN=1", tdGoldenPath, err)
	}
	if got != string(want) {
		// 33 of 51 open production tasks have no link at all. This is not an
		// edge case to be handled; it is the majority rendering (D2).
		if strings.Join(strings.Fields(got), " ") == strings.Join(strings.Fields(string(want)), " ") {
			t.Errorf("criterion 15: the no-link page differs from the pre-change render in WHITESPACE only. The "+
				"section is a single {{if .SourceMessage}} block and nothing outside it changes — use {{- -}} trim "+
				"markers, the {{if .Body}} one-line idiom already in task.html\n%s", tdDiff(string(want), got))
		}
		t.Errorf("criterion 15: the no-link page is not byte-identical to the pre-change render: no heading, no "+
			"<details>, no '(none)', no muted 'no source message' line, no error, no extra <h2>\n%s",
			tdDiff(string(want), got))
	}
	if _, ok := tdSection(t, page); ok {
		t.Errorf("criterion 15: a task with none of the three links renders a %s block", tdSectionOpen)
	}
	tdNotWant(t, "T5", "the page", page, "<details", "criterion 15: no empty <details>")
}

func TestTaskDetail_Integration_UnresolvableLinkIsTheNoLinkCase(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	page := tdGetTask(t, client, ts.URL, f.t6)
	if _, ok := tdSection(t, page); ok {
		t.Errorf("criterion 16: T6's only links are a classify_promotions row with task_id NULL (the claim-before-act " +
			"crash shape) and a source_thread_id whose thread holds no inbound message. An unresolvable link is the " +
			"no-link case, not an error — and not a heading over nothing")
	}
	// T5 and T6 are both no-link pages; only the title and the card body differ,
	// and both are fixture text. Anything else that differs is the section leaking.
	erase := func(s, title, body string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, title, "{{TITLE}}"), body, "{{BODY}}")
	}
	want := erase(tdNormalize(tdGetTask(t, client, ts.URL, f.t5), f.t5), tdNoLinkTitle, tdNoLinkBody)
	got := erase(tdNormalize(page, f.t6), tdUnresolvableTitle, tdUnresolvableBody)
	if got != want {
		t.Errorf("criterion 16: T6 does not render like T5\n%s", tdDiff(want, got))
	}

	// criterion 17: a nonexistent id still 404s, unchanged.
	code, _ := get(t, client, ts.URL+"/tasks/999000999")
	if code != http.StatusNotFound {
		t.Errorf("GET /tasks/999000999 = %d, want 404 (criterion 17: http.NotFound, unchanged)", code)
	}
}

func tdDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var lw, lg string
		if i < len(w) {
			lw = w[i]
		}
		if i < len(g) {
			lg = g[i]
		}
		if lw != lg {
			return fmt.Sprintf("first difference at line %d:\n  pre-change: %q\n  now:        %q", i+1, lw, lg)
		}
	}
	return "(no line differs; the files differ in their trailing bytes)"
}

// ---- criterion 8: the source body cap -----------------------------------------

func TestTaskDetail_Integration_SourceBodyCap(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	body := tdBigBody()
	chars := utf8.RuneCountInString(body)
	if chars <= tdSourceBodyCap {
		t.Fatalf("POSITIVE CONTROL FAILED: the oversize fixture body is %d characters, not longer than the cap %d",
			chars, tdSourceBodyCap)
	}
	// The units have to DISAGREE or the assertion below cannot tell them apart.
	if cut := len(string([]rune(body)[:tdSourceBodyCap])); cut <= chars {
		t.Fatalf("POSITIVE CONTROL FAILED: the first %d characters of the fixture body are %d bytes, not more than "+
			"its %d characters — a byte-counting cap marker would pass this test", tdSourceBodyCap, cut, chars)
	}
	page := tdGetTask(t, client, ts.URL, f.t8)
	sec := tdMustSection(t, "T8", page)

	tdWant(t, "T8", "the section", sec, tdEsc(tdBigHead), "criterion 8: the first sourceBodyCap characters render")
	tdNotWant(t, "T8", "the section", sec, tdEsc(tdPastSourceCap),
		fmt.Sprintf("criterion 8 / D3: the body is cut with left(body_text,%d) — the 256 KiB safety cap is the ONLY "+
			"thing on this page that may drop text", tdSourceBodyCap))
	tdCapMarker(t, "T8", sec, tdSourceBodyCap, chars)
	if tdSourceBodyCap == tdThreadBodyCap {
		t.Fatalf("POSITIVE CONTROL FAILED: sourceBodyCap and MailThreadBodyCap are the same number; D8 says they are " +
			"deliberately different (mutation 18)")
	}
}

// ---- criterion 10: a thread longer than the cap --------------------------------

func TestTaskDetail_Integration_LongThreadIsCapped(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	var onL int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM normalized_messages WHERE thread_id=$1`, f.tL).Scan(&onL); err != nil {
		t.Fatalf("count thread L: %v", err)
	}
	if onL != tdLongThreadSiblings+1 {
		t.Fatalf("T9: POSITIVE CONTROL FAILED — thread L holds %d messages, want %d (the head plus %d siblings)",
			onL, tdLongThreadSiblings+1, tdLongThreadSiblings)
	}
	page := tdGetTask(t, client, ts.URL, f.t9)
	sec := tdMustSection(t, "T9", page)
	_, collapsed := tdSplit(sec)

	if n := strings.Count(collapsed, "<details"); n != tdThreadMax {
		t.Errorf("T9: the collapsed block holds %d <details>, want tools.MailThreadMaxMessages = %d. Criterion 10: "+
			"thread L holds the source plus %d others; the cap takes the OLDEST, matching mail_read_thread (D3)",
			n, tdThreadMax, tdLongThreadSiblings)
	}
	// D3 spells the heading `Thread (N more)`, with N the TRUE count.
	if !regexp.MustCompile(`Thread \(` + strconv.Itoa(tdLongThreadSiblings) + ` more\)`).MatchString(sec) {
		t.Errorf("T9: the section carries no `Thread (%d more)` heading naming the TRUE count. Criterion 10: a thread "+
			"longer than the cap renders the cap's worth PLUS a marker naming the true count", tdLongThreadSiblings)
	}
}

// ---- criterion 20: the truncated capture and the repair ------------------------

func TestTaskDetail_Integration_TruncatedCaptureNamesTheRepair(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupTaskDetail(t, ctx, pool)
	defer cleanupTaskDetail(t, ctx, pool)
	f := seedTaskDetail(t, ctx, pool)
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	atts, info, err := google.ListAttachments(tdRawJSON(t, ctx, pool, f.mt1.raw))
	if err != nil {
		t.Fatalf("T7: google.ListAttachments: %v", err)
	}
	if !info.Truncated || len(atts) != 2 {
		t.Fatalf("T7: POSITIVE CONTROL FAILED — truncated=%v, %d manifest parts (want true, 2)", info.Truncated, len(atts))
	}

	page := tdGetTask(t, client, ts.URL, f.t7)
	sec := tdMustSection(t, "T7", page)

	for _, a := range atts {
		tdWant(t, "T7", "the section", sec, tdEsc(a.Filename), "criterion 20: the manifest still names the file")
		tdWant(t, "T7", "the section", sec, strconv.Itoa(a.SizeBytes), "criterion 20: the manifest's size")
		if !a.SizeIsEncoded {
			t.Fatalf("T7: POSITIVE CONTROL FAILED — a manifest part reports a DECODED size")
		}
		if a.UnavailableReason == "" {
			t.Fatalf("T7: POSITIVE CONTROL FAILED — a manifest part carries no UnavailableReason")
		}
		// The reason comes from the STORED bytes, through google's own walk —
		// not from a string the dashboard invents.
		tdWant(t, "T7", "the section", sec, tdEsc(a.UnavailableReason),
			"criterion 20 / D6: `stored` is .Available, else .UnavailableReason VERBATIM — the 'not stored: message "+
				"was over the … cap' row explains itself")
	}
	tdWant(t, "T7", "the section", sec, "(encoded)",
		"criterion 19 / D6: the size is suffixed `(encoded)` when SizeIsEncoded — a truncated capture's manifest "+
			"reports the ENCODED size")
	tdWant(t, "T7", "the section", sec, "opsctl mail refetch",
		"criterion 20 / D6 / SWT-64: since the refetch shipped, that reason has a fix worth naming at the point of "+
			"frustration")
	tdAssertInert(t, "T7", sec)
}

// ---- criterion 22: the cost ----------------------------------------------------

func TestTaskDetail_Integration_StatementCost(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	plain := dashPool(t, ctx)
	defer plain.Close()
	cleanupTaskDetail(t, ctx, plain)
	defer cleanupTaskDetail(t, ctx, plain)
	f := seedTaskDetail(t, ctx, plain)

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	tr := &countTracer{}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	defer pool.Close()
	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()

	var seen []string
	count := func(id int64) (total int, resolution int, rawRead int) {
		tdGetTask(t, client, ts.URL, id) // warm-up: prepared-statement chatter
		tr.take()
		tdGetTask(t, client, ts.URL, id)
		for _, s := range tr.take() {
			seen = append(seen, s)
			total++
			if strings.Contains(s, "normalized_messages") || strings.Contains(s, "classify_promotions") ||
				strings.Contains(s, "capture_decisions") {
				resolution++
			}
			if strings.Contains(s, "raw_json") {
				rawRead++
			}
		}
		return total, resolution, rawRead
	}

	t5Total, t5Res, t5Raw := count(f.t5)
	if t5Total == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: the tracer saw no statement for a task detail render")
	}
	if t5Res != 1 {
		t.Errorf("the no-link render issued %d resolution statements, want exactly 1. Criterion 22: showTask adds at "+
			"most THREE statements — one resolution-and-load (always), one thread read (skipped when the message has "+
			"no thread_id), one raw_json read (gmail with a raw item only); the no-link case adds exactly one", t5Res)
	}
	if t5Raw != 0 {
		t.Errorf("the no-link render read raw_json %d times, want 0 (criterion 22)", t5Raw)
	}

	t1Total, _, t1Raw := count(f.t1)
	if d := t1Total - t5Total; d != 2 {
		t.Errorf("a gmail source render issues %d more statements than the no-link render, want 2 (the thread read "+
			"and the raw_json read on top of the shared resolution-and-load) — criterion 22 caps this path at three", d)
	}
	if t1Raw != 1 {
		t.Errorf("the gmail source render read raw_json %d times, want exactly 1 (D6: one manifest, for the SOURCE "+
			"message only — a per-message manifest for the collapsed thread is Future work precisely because of "+
			"this cost)", t1Raw)
	}

	_, _, t4Raw := count(f.t4)
	if t4Raw != 0 {
		t.Errorf("the slack source render read raw_json %d times, want 0. Criterion 22 / D6: the raw_json read is "+
			"gmail-with-a-raw-item ONLY — a slack raw_json has no IMAP envelope, so the read is pure cost and the "+
			"walk would have nothing to say (criterion 21)", t4Raw)
	}

	// Invariant 3, negatively: this page performs NO write.
	for _, s := range seen {
		if regexp.MustCompile(`(?i)^\s*(insert|update|delete)\b`).MatchString(s) {
			t.Errorf("the task detail render issued a write: %q. Invariant 3 is satisfied by ABSENCE here — showTask "+
				"is a read-only handler and stays one (criterion 23)", s)
		}
	}
}
