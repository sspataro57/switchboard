//go:build integration

package tools_test

// Regression suite for bug gmail-reply-empty-subject-off-thread (Jira SWT-61).
//
// CONVERTED FROM THE REPRODUCTION. This file was
// TestGmailReply_EmptySubject_SendsReOnThread, which asserted the sent Subject
// was "Re: " + the THREAD's subject. The owner's decision (2026-09-16) is that a
// reply carries "Re: " + the subject of the LATEST INBOUND message — the one
// In-Reply-To names — because normalized_threads.subject is frozen
// first-writer-wins (google/sink.go:248-251, COALESCE(existing, new)) and goes
// stale when a correspondent renames a thread. That is prod thread 159886
// exactly: the stored subject is the 10 Sep inbound's, the 15 Sep inbound we
// replied to has a different one. So the assertion moved from the thread's
// subject to the latest inbound's, and the fixture (which already gave the
// latest inbound a DIFFERENT subject) became load-bearing rather than incidental.
//
// WHAT PRODUCED THE BUG. draft_delivery {task_id, channel:"gmail", thread_id,
// body} with NO subject key (prod audit 1848) stored deliveries.subject NULL
// (NULLIF($5,'')); approve hashed it fine; send copied "" into
// OutboundMessage.Subject and BuildOutboundMIME skipped the header. It reached a
// university client as a standalone, subject-less email.
//
// THE FIX THIS PINS (owner decisions 1, 3, 5): draftDelivery's gmail branch
// fills an absent subject SERVER-SIDE, inside the executor, so no caller can
// bypass it — the same "never caller-chosen" property From already has. Both
// subjects empty => the draft is REFUSED; nothing subject-less may be drafted.
// Plus the send-path floor (decision 2): a gmail row with an empty subject is
// refused at send, by name, for rows written before the fix or straight to SQL.
//
// TEST-THE-COLUMN (IK rule). The drafted subject comes from a DB COLUMN
// (normalized_messages.subject of the latest inbound), so the fixture makes the
// thread's stored subject DIFFER from it. MUTATION TO RUN BY HAND: point the
// fill at normalized_threads.subject (or drop `subject` from
// latestInboundMessage's SELECT and fall back) → FollowsLatestInbound goes red
// naming both strings. A fixture where the two agree would certify nothing.
//
// The network is faked at the tools.GmailSender seam (fakeGmailSender, defined
// in delivery_lifecycle_integration_test.go), which captures the raw RFC822
// bytes SMTPSender would submit. NOTHING IS SENT: no SMTP server, no Gmail API,
// no broker. Client address, subject and body text appear nowhere — every
// string here is a placeholder.
//
// Run against the ISOLATED database (NEVER prod, never the shared compose `ops`):
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_gmailsubject?sslmode=disable' \
//	  go test -tags integration -count=1 -run TestRegression_SWT61 -v ./internal/tools/

import (
	"context"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	gsubActor        = "dashboard:itest-gsub@example.com"
	gsubSlug         = "itest-gsub-proj"
	gsubClient       = "itest-gsub-client"
	gsubAcctEmail    = "itest-gsub-a@example.com"
	gsubThreadPrefix = "gmail:itest-gsub-a@example.com:gthread-gsub-"
	gsubThreadLike   = gsubThreadPrefix + "%"
	// Placeholders, never the client's words. The two differ ON PURPOSE: the
	// stored thread subject is the first-ingested message's and is stale.
	gsubThreadSubj = "Question about the program" // normalized_threads.subject
	gsubLatestSubj = "Follow-up question"         // the LATEST INBOUND message's
	gsubClientFrom = "client@itest-gsub.example"  // placeholder recipient
	gsubMIDA       = "<inbound-a-itest-gsub@itest-gsub.example>"
	gsubMIDB       = "<sb-1-111-itest-gsub@example.com>"
	gsubMIDC       = "<inbound-c-itest-gsub@itest-gsub.example>"
)

func cleanupGsub(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE actor=$1)`, []any{gsubActor}},
		{`DELETE FROM audit_events WHERE actor=$1`, []any{gsubActor}},
		{`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN
			(SELECT id FROM deliveries WHERE task_id IN
				(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)))`, []any{gsubSlug}},
		{`DELETE FROM task_events WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{gsubSlug}},
		{`DELETE FROM deliveries WHERE task_id IN
			(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1))`, []any{gsubSlug}},
		{`DELETE FROM normalized_messages WHERE thread_id IN
			(SELECT id FROM normalized_threads WHERE thread_key LIKE $1)`, []any{gsubThreadLike}},
		{`DELETE FROM normalized_threads WHERE thread_key LIKE $1`, []any{gsubThreadLike}},
		{`DELETE FROM raw_source_items WHERE source_account_id IN
			(SELECT id FROM source_accounts WHERE account_email=$1)`, []any{gsubAcctEmail}},
		{`DELETE FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{gsubSlug}},
		{`DELETE FROM decisions WHERE project_id IN (SELECT id FROM projects WHERE slug=$1)`, []any{gsubSlug}},
		{`DELETE FROM projects WHERE slug=$1`, []any{gsubSlug}},
		{`DELETE FROM source_accounts WHERE account_email=$1`, []any{gsubAcctEmail}},
	}
	for _, st := range stmts {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

// ---- fixture ------------------------------------------------------------------

type gsubMsg struct{ dir, mid, subj, sender, ago string }

// gsubFixture seeds the send-enabled app-password mailbox (prod account 1009's
// shape; the password bytes are a placeholder — the transport is faked at the
// seam, so they are never decrypted) and a task to hang deliveries on.
func gsubFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (acctID, taskID int64) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled, auth_type, app_password_encrypted)
		 VALUES ('google', $1, true, 'app_password', decode('00','hex')) RETURNING id`, gsubAcctEmail).Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	projID := seedProject(t, ctx, pool, gsubSlug, gsubClient)
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1, 'itest-gsub client question', 'human', 'ready') RETURNING id`, projID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return acctID, taskID
}

// gsubThread seeds one thread and its messages. threadSubject is a pointer so a
// NULL stored subject (29 gmail threads on prod have one) can be seeded.
func gsubThread(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	acctID int64, suffix string, threadSubject *string, msgs []gsubMsg) int64 {
	t.Helper()
	var threadID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject) VALUES ($1, $2) RETURNING id`,
		gsubThreadPrefix+suffix, threadSubject).Scan(&threadID); err != nil {
		t.Fatalf("seed thread %s: %v", suffix, err)
	}
	for i, m := range msgs {
		// One raw item per message (normalized_messages_raw_item_idx).
		var rawID int64
		extID := "itest-gsub-raw-" + suffix + "-" + itoa(int64(i))
		if err := pool.QueryRow(ctx,
			`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
			 VALUES ($1, $2, '{}', $2) RETURNING id`, acctID, extID).Scan(&rawID); err != nil {
			t.Fatalf("seed raw %s: %v", extID, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO normalized_messages
			   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
			 VALUES ($1, $2, $3, $4, now() - $5::interval, 'placeholder body', $6, $7, 'gmail')`,
			rawID, threadID, m.dir, m.mid, m.ago, m.subj, m.sender); err != nil {
			t.Fatalf("seed message %s: %v", m.mid, err)
		}
	}
	return threadID
}

// gsubProdShape is prod thread 159886's shape: inbound A, our earlier reply B,
// latest inbound C — and C's subject differs from the thread's stored one.
func gsubProdShape() []gsubMsg {
	return []gsubMsg{
		{"inbound", gsubMIDA, gsubThreadSubj, gsubClientFrom, "5 days"},
		{"outbound", gsubMIDB, "Re: " + gsubThreadSubj, gsubAcctEmail, "3 days"},
		{"inbound", gsubMIDC, gsubLatestSubj, gsubClientFrom, "4 hours"},
	}
}

// gsubShow formats a nullable subject readably: "<nil>" or the quoted words.
// Printing the *string itself yields a pointer address, which says nothing about
// which subject shipped.
func gsubShow(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *s)
}

func gsubDraftSubject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deliveryID int64) *string {
	t.Helper()
	var s *string
	if err := pool.QueryRow(ctx, `SELECT subject FROM deliveries WHERE id=$1`, deliveryID).Scan(&s); err != nil {
		t.Fatalf("read delivery %d subject: %v", deliveryID, err)
	}
	return s
}

func gsubDraft(t *testing.T, ctx context.Context, ex *executor.Executor, taskID, threadID int64, extra string) int64 {
	t.Helper()
	args := `{"task_id":` + itoa(taskID) + `,"channel":"gmail","body":"placeholder reply","thread_id":` + itoa(threadID) + extra + `}`
	out := callOK(t, ctx, ex, gsubActor, "draft_delivery", args)
	var d struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out, &d)
	if d.DeliveryID == 0 {
		t.Fatal("draft_delivery returned delivery_id 0")
	}
	return d.DeliveryID
}

// ---- 1. the converted reproduction --------------------------------------------

// The bug, end to end: draft with no subject, approve, send — and the message on
// the wire carries "Re: <latest inbound's subject>", not a missing header and
// not the stale thread subject.
func TestRegression_SWT61_GmailReplySubjectFollowsLatestInbound(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupGsub(t, ctx, pool)
	defer cleanupGsub(t, ctx, pool)

	acctID, taskID := gsubFixture(t, ctx, pool)
	stored := gsubThreadSubj
	threadID := gsubThread(t, ctx, pool, acctID, "1", &stored, gsubProdShape())

	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)

	// draft_delivery with NO subject key — prod audit 1848's arg shape.
	deliveryID := gsubDraft(t, ctx, ex, taskID, threadID, "")

	// (a) Decision 1: the subject is filled at DRAFT time, inside the executor,
	// so it is visible on the dashboard before approval, part of
	// DeliveryContentHash, and editable with update_delivery.
	want := "Re: " + gsubLatestSubj
	got := gsubDraftSubject(t, ctx, pool, deliveryID)
	switch {
	case got == nil:
		t.Errorf("delivery %d: subject IS NULL after draft_delivery (prod #36 exactly). Decision 1: "+
			"draftDelivery's gmail branch fills an absent subject server-side — a subject-less gmail row must "+
			"not be representable", deliveryID)
	case *got != want:
		t.Errorf("drafted subject = %q, want %q", *got, want)
	}
	// (b) ...from the LATEST INBOUND message, not the thread's stored subject.
	// This is the column assertion: the two differ by construction.
	if got != nil && *got == "Re: "+gsubThreadSubj {
		t.Errorf("drafted subject followed normalized_threads.subject (%q). It is frozen first-writer-wins "+
			"(sink.go: COALESCE(existing, new)) and goes stale on a rename — prod thread 159886. The reply must "+
			"carry the subject of the message In-Reply-To names: %q", gsubThreadSubj, gsubLatestSubj)
	}

	callOK(t, ctx, ex, gsubActor, "approve_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)
	callOK(t, ctx, ex, gsubActor, "send_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)

	if fake.calls != 1 {
		t.Fatalf("transport calls = %d, want 1", fake.calls)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(fake.lastRaw)))
	if err != nil {
		t.Fatalf("parse rendered message: %v", err)
	}
	var names []string
	for k := range msg.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	_, hasSubject := msg.Header["Subject"]
	t.Logf("rendered header names: %v", names)
	t.Logf("Subject present=%v value=%q", hasSubject, msg.Header.Get("Subject"))

	// (c) ...and the words that were approved are the words on the wire.
	if s := msg.Header.Get("Subject"); s != want {
		t.Errorf("Subject = %q (header present: %v), want %q. Prod #36 went out with NO Subject header at all "+
			"and reached the client as a standalone email", s, hasSubject, want)
	}
	// Unchanged by the fix, and asserted so a subject fix cannot break threading.
	if got := msg.Header.Get("In-Reply-To"); got != gsubMIDC {
		t.Errorf("In-Reply-To = %q, want %q (the latest inbound — the same message the subject comes from)", got, gsubMIDC)
	}
	refs := msg.Header.Get("References")
	for _, mid := range []string{gsubMIDA, gsubMIDB, gsubMIDC} {
		if !strings.Contains(refs, mid) {
			t.Errorf("References %q missing %s", refs, mid)
		}
	}
}

// ---- 2. no doubled prefix (decision 3) ----------------------------------------

// 247 latest-inbound subjects on prod ALREADY start with a reply prefix, so a
// naive "Re: " + subject would double on hundreds of live threads. The thread's
// stored subject equals the latest inbound's in every case here, so a failure
// means doubling and nothing else.
func TestRegression_SWT61_NoDoubledRePrefix(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupGsub(t, ctx, pool)
	defer cleanupGsub(t, ctx, pool)

	acctID, taskID := gsubFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	for i, tc := range []struct {
		name, inbound, want string
	}{
		{"no prefix gains one", "Follow-up question", "Re: Follow-up question"},
		{"one prefix stays one", "Re: Follow-up question", "Re: Follow-up question"},
		{"doubled collapses", "Re: Re: Follow-up question", "Re: Follow-up question"},
		{"upper case", "RE: Follow-up question", "Re: Follow-up question"},
		{"lower case, no space", "re:Follow-up question", "Re: Follow-up question"},
		{"counted form", "Re[2]: Follow-up question", "Re: Follow-up question"},
		{"odd spacing", "  Re:   Follow-up question", "Re: Follow-up question"},
		{"forward is replied to", "Fwd: Follow-up question", "Re: Fwd: Follow-up question"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := "dbl-" + itoa(int64(i))
			stored := tc.inbound // same as the inbound: this case tests doubling only
			threadID := gsubThread(t, ctx, pool, acctID, suffix, &stored, []gsubMsg{
				{"inbound", "<inbound-" + suffix + "@itest-gsub.example>", tc.inbound, gsubClientFrom, "2 hours"},
			})
			deliveryID := gsubDraft(t, ctx, ex, taskID, threadID, "")
			got := gsubDraftSubject(t, ctx, pool, deliveryID)
			if got == nil {
				t.Fatalf("subject IS NULL for inbound subject %q", tc.inbound)
			}
			if *got != tc.want {
				t.Errorf("inbound %q -> drafted subject %q, want %q (exactly one \"Re: \", the rest of the words "+
					"verbatim — they are client-visible)", tc.inbound, *got, tc.want)
			}
		})
	}
}

// ---- 3. both empty: refuse (decision 5) ---------------------------------------

// 29 gmail threads on prod have an empty stored subject. When the latest inbound
// has none either, there is nothing to reply about — the draft is REFUSED and
// the caller is told to pass a subject. Nothing subject-less may be drafted.
func TestRegression_SWT61_BothSubjectsEmpty_DraftIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupGsub(t, ctx, pool)
	defer cleanupGsub(t, ctx, pool)

	acctID, taskID := gsubFixture(t, ctx, pool)
	threadID := gsubThread(t, ctx, pool, acctID, "empty", nil, []gsubMsg{
		{"inbound", "<inbound-empty-itest-gsub@itest-gsub.example>", "", gsubClientFrom, "2 hours"},
	})
	ex := deliveryExecutor(pool)

	_, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: gsubActor,
		Args: []byte(`{"task_id":` + itoa(taskID) + `,"channel":"gmail","body":"placeholder reply","thread_id":` + itoa(threadID) + `}`)})
	if err == nil {
		t.Errorf("draft_delivery created a gmail draft on a thread with no subject anywhere. Decision 5: refuse — " +
			"a subject-less row is what reached the client as delivery #36")
	} else if !strings.Contains(strings.ToLower(err.Error()), "subject") {
		t.Errorf("refusal = %q, want it to name `subject` and tell the caller to pass one", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, taskID).Scan(&rows); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d delivery row(s) exist after the refusal, want 0: the refusal must leave no subject-less "+
			"row behind for a later approve to find", rows)
	}
}

// Control: when only the LATEST inbound's subject is empty, the thread's stored
// subject is the fallback — refusing needs BOTH to be empty.
func TestRegression_SWT61_FallsBackToTheThreadSubject(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupGsub(t, ctx, pool)
	defer cleanupGsub(t, ctx, pool)

	acctID, taskID := gsubFixture(t, ctx, pool)
	stored := gsubThreadSubj
	threadID := gsubThread(t, ctx, pool, acctID, "fallback", &stored, []gsubMsg{
		{"inbound", "<inbound-fallback-itest-gsub@itest-gsub.example>", "", gsubClientFrom, "2 hours"},
	})
	ex := deliveryExecutor(pool)

	deliveryID := gsubDraft(t, ctx, ex, taskID, threadID, "")
	want := "Re: " + gsubThreadSubj
	got := gsubDraftSubject(t, ctx, pool, deliveryID)
	if got == nil || *got != want {
		t.Errorf("drafted subject = %s, want %q (the latest inbound has no subject, so the thread's stored one "+
			"is the fallback — refuse only when BOTH are empty)", gsubShow(got), want)
	}
}

// ---- 4. the send-path floor (decision 2, first floor) -------------------------

// Unreachable for rows created after the fix; this is the floor for rows created
// BEFORE it and for anything that writes the table directly. It REFUSES rather
// than fills, so the approved words and the sent words can never differ.
func TestRegression_SWT61_SendRefusesAGmailRowWithNoSubject(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	for _, tc := range []struct {
		name string
		set  any // what to force deliveries.subject to, by SQL
	}{
		{"null subject", nil},        // prod #36 exactly
		{"blank subject", "   "},     // NULLIF cannot produce this; direct SQL can
		{"empty string subject", ""}, // ...nor this
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanupGsub(t, ctx, pool)
			defer cleanupGsub(t, ctx, pool)

			acctID, taskID := gsubFixture(t, ctx, pool)
			stored := gsubThreadSubj
			threadID := gsubThread(t, ctx, pool, acctID, "floor", &stored, gsubProdShape())

			fake := &fakeGmailSender{pool: pool}
			tools.SetGmailSender(fake)
			ex := deliveryExecutor(pool)

			// Drafted and approved WITH a subject, so the draft-time fill is not
			// what is under test here...
			deliveryID := gsubDraft(t, ctx, ex, taskID, threadID, `,"subject":"Re: placeholder subject"`)
			callOK(t, ctx, ex, gsubActor, "approve_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)
			// ...then the subject is removed behind the executor's back.
			if _, err := pool.Exec(ctx, `UPDATE deliveries SET subject=$2 WHERE id=$1`, deliveryID, tc.set); err != nil {
				t.Fatalf("force subject: %v", err)
			}

			_, err := ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: gsubActor,
				Args: []byte(`{"delivery_id":` + itoa(deliveryID) + `}`)})
			if err == nil {
				t.Errorf("send_delivery sent an approved gmail row with %s. Decision 2: phase 1 refuses it BY NAME "+
					"— the send path already had the subject once and discarded it (SWT-61 landmine)", tc.name)
			} else if !strings.Contains(strings.ToLower(err.Error()), "subject") {
				t.Errorf("refusal = %q, want it to name `subject`", err)
			}
			if fake.calls != 0 {
				t.Errorf("transport calls = %d, want 0: nothing may reach SMTP", fake.calls)
			}

			var status string
			var sentID *string
			if err := pool.QueryRow(ctx,
				`SELECT status, sent_external_id FROM deliveries WHERE id=$1`, deliveryID).Scan(&status, &sentID); err != nil {
				t.Fatalf("read delivery: %v", err)
			}
			if status == "sent" || status == "sending" {
				t.Errorf("delivery status = %q after the refusal, want it left approved", status)
			}
			if sentID != nil {
				t.Errorf("sent_external_id = %q after a refused send, want NULL", *sentID)
			}
		})
	}
}

// ---- 5. the words a human or the model chose are never rewritten --------------

// Control (GREEN today, must stay green): a non-empty subject ships verbatim,
// prefix or no prefix. The fill is for an ABSENT subject only — if someone chose
// words, they are the words.
func TestRegression_SWT61_ACallerSubjectShipsVerbatim(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	cleanupGsub(t, ctx, pool)
	defer cleanupGsub(t, ctx, pool)

	acctID, taskID := gsubFixture(t, ctx, pool)
	stored := gsubThreadSubj
	threadID := gsubThread(t, ctx, pool, acctID, "verbatim", &stored, gsubProdShape())

	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)

	const chosen = "Placeholder subject the caller chose"
	deliveryID := gsubDraft(t, ctx, ex, taskID, threadID, `,"subject":"`+chosen+`"`)
	if got := gsubDraftSubject(t, ctx, pool, deliveryID); got == nil || *got != chosen {
		t.Fatalf("drafted subject = %s, want %q unchanged (no \"Re: \" added to words a human chose)", gsubShow(got), chosen)
	}
	callOK(t, ctx, ex, gsubActor, "approve_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)
	callOK(t, ctx, ex, gsubActor, "send_delivery", `{"delivery_id":`+itoa(deliveryID)+`}`)

	msg, err := mail.ReadMessage(strings.NewReader(string(fake.lastRaw)))
	if err != nil {
		t.Fatalf("parse rendered message: %v", err)
	}
	if s := msg.Header.Get("Subject"); s != chosen {
		t.Errorf("Subject = %q, want the caller's words %q verbatim", s, chosen)
	}
}

// ---- 6. the subject that scrubs to nothing (go-reviewer finding, 2026-09-16) --

// ScrubAIAttribution (invariant 6's belt) drops any LINE carrying an
// attribution marker, and a subject is exactly one line. So a subject that
// passes the fill's emptiness check can still scrub to "" at the INSERT, where
// NULLIF turns it into SQL NULL — delivery #36's exact shape, produced through
// the executor by the fix itself. Both paths are covered, because either can
// carry a marker: a subject FILLED from the latest inbound, and one the CALLER
// chose. The words here are placeholders that happen to contain a marker
// ("generated with"); nothing about them is client text.
func TestRegression_SWT61_SubjectThatScrubsToNothingIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	for _, tc := range []struct{ name, inbound, extra string }{
		// Filled from an inbound subject that is entirely an attribution line.
		{"filled from the inbound", "generated with the placeholder tool", ""},
		// ...and a subject the caller passed that scrubs away the same way.
		{"chosen by the caller", gsubLatestSubj, `,"subject":"generated with the placeholder tool"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanupGsub(t, ctx, pool)
			defer cleanupGsub(t, ctx, pool)

			acctID, taskID := gsubFixture(t, ctx, pool)
			// The thread's stored subject scrubs away too, so the fallback
			// cannot quietly rescue the case under test.
			stored := "generated with the placeholder tool"
			threadID := gsubThread(t, ctx, pool, acctID, "scrub", &stored, []gsubMsg{
				{"inbound", "<inbound-scrub-itest-gsub@itest-gsub.example>", tc.inbound, gsubClientFrom, "2 hours"},
			})
			ex := deliveryExecutor(pool)

			_, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: gsubActor,
				Args: []byte(`{"task_id":` + itoa(taskID) + `,"channel":"gmail","body":"placeholder reply","thread_id":` +
					itoa(threadID) + tc.extra + `}`)})
			if err == nil {
				t.Errorf("draft_delivery stored a gmail subject that ScrubAIAttribution empties: NULLIF turns it "+
					"into a NULL-subject row — #36's shape, through the executor (%s)", tc.name)
			} else if !strings.Contains(strings.ToLower(err.Error()), "subject") {
				t.Errorf("refusal = %q, want it to name `subject`", err)
			}

			var rows int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, taskID).Scan(&rows); err != nil {
				t.Fatalf("count deliveries: %v", err)
			}
			if rows != 0 {
				t.Errorf("%d delivery row(s) exist after the refusal, want 0", rows)
			}
		})
	}
}

// ---- 7. the approve gate (owner decision, 2026-09-16) -------------------------

// The THIRD gate, after draft and edit. Without it a subject-less gmail row
// could be approved and would then fail at send — stranded in `approved`, where
// update_delivery (drafted rows only) can no longer edit it and recovery means
// Deny/Redo. The row is planted by SQL because the draft-time fill and the edit
// refusal together make it otherwise unreachable, which is the point.
func TestRegression_SWT61_ApproveRefusesAGmailRowWithNoSubject(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()

	for _, tc := range []struct {
		name string
		set  any
	}{
		{"null subject", nil},    // prod #36 exactly
		{"blank subject", "   "}, // NULLIF cannot produce this; direct SQL can
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanupGsub(t, ctx, pool)
			defer cleanupGsub(t, ctx, pool)

			acctID, taskID := gsubFixture(t, ctx, pool)
			stored := gsubThreadSubj
			threadID := gsubThread(t, ctx, pool, acctID, "approvegate", &stored, gsubProdShape())
			ex := deliveryExecutor(pool)

			deliveryID := gsubDraft(t, ctx, ex, taskID, threadID, `,"subject":"Re: placeholder subject"`)
			if _, err := pool.Exec(ctx, `UPDATE deliveries SET subject=$2 WHERE id=$1`, deliveryID, tc.set); err != nil {
				t.Fatalf("force subject: %v", err)
			}

			_, err := ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: gsubActor,
				Args: []byte(`{"delivery_id":` + itoa(deliveryID) + `}`)})
			if err == nil {
				t.Errorf("approve_delivery approved a gmail row with %s: it would fail at send and strand the row "+
					"in `approved`, past the point update_delivery can fix it", tc.name)
			} else if !strings.Contains(strings.ToLower(err.Error()), "subject") {
				t.Errorf("refusal = %q, want it to name `subject`", err)
			}

			if s := deliveryStatus(t, ctx, pool, deliveryID); s != "drafted" {
				t.Errorf("status = %q after a refused approve, want it left drafted", s)
			}
			var approvals int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM approvals WHERE subject_type='delivery' AND subject_id=$1`,
				deliveryID).Scan(&approvals); err != nil {
				t.Fatalf("count approvals: %v", err)
			}
			if approvals != 0 {
				t.Errorf("a refused approve left %d approvals row(s)", approvals)
			}
		})
	}
}
