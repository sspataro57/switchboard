//go:build integration

package tools_test

// gmail-delivery-cc (SWT-69) against a real database: criteria 4-12, 15 and 16.
// Every mutation goes through executor.Execute with the REAL policy matrix
// (invariant 3); the Gmail network call is an injected fake; NEVER a live send.
//
// Run against an ISOLATED scratch database, never the shared compose `ops`
// (IK landmine: the compose Postgres is shared by every worktree):
//
//	psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_swt69'
//	make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable'
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_swt69?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run Cc ./internal/tools/
//
// Reuses the SWT-8 fixture (seedDeliveryFixture / cleanupDeliveryData /
// deliveryExecutor / draftGmail / fakeGmailSender in
// delivery_lifecycle_integration_test.go), so it cleans up the same corpus in
// the same FK order and is rerunnable.
//
// EXPECTED RED: deliveries has no cc column (migration 0038 is unwritten), the
// tools do not know the argument, and tools.DeliveryContentHash takes two
// arguments — the package's integration binary compile-FAILs first, then these
// fail on the SQL.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   1  drop the gmail-only refusal          → TestDraftDelivery_..._CcIsGmailOnly
//   2  store the caller's raw string        → ..._CcStoresTheNormalizedList
//   3  drop/case-sensitive dedupe           → ..._CcStoresTheNormalizedList
//   5  read cc AFTER the phase-1 lock, or   → ..._CcOnTheWire (and replace
//      feed the send a literal '{}'            `d.cc` with `'{}'::text[]` in the
//                                              phase-1 SELECT: this MUST go red)
//   6  leave DeliveryContentHash on (s,b)   → ..._CcIsBoundToTheHash
//   7  let update_delivery edit an approved → ..._CcReplaceClearUnchanged
//   8  treat cc:[] as "unchanged"           → ..._CcReplaceClearUnchanged
//  12  remove the send-time From/To drop    → ..._CcEqualToTheResolvedToIsDropped

import (
	"context"
	"encoding/json"
	"net/mail"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	ccKatie   = "kevans@cecollaboratory.com"
	ccBilling = "billing@itest-del.example"
	// The send-drop stage's own rows, cleaned up by ccExtraCleanup.
	ccKatieMID = "<itest-del-cc-katie@example.com>"
	ccKatieRaw = "itest-del-cc-raw-2"
)

// ---- helpers -----------------------------------------------------------------

func readCc(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) []string {
	t.Helper()
	var cc []string
	if err := pool.QueryRow(ctx, `SELECT cc FROM deliveries WHERE id=$1`, id).Scan(&cc); err != nil {
		t.Fatalf("read delivery %d cc: %v (migration 0038 adds the column)", id, err)
	}
	return cc
}

func ccEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// draftGmailCc drafts a gmail delivery carrying the given raw cc JSON array.
func draftGmailCc(t *testing.T, ctx context.Context, ex *executor.Executor, parentID, threadID int64, ccJSON string) (int64, error) {
	t.Helper()
	args := `{"task_id":` + itoa(parentID) + `,"channel":"gmail","subject":"Re: login broken",` +
		`"body":"draft body","thread_id":` + itoa(threadID) + `,"cc":` + ccJSON + `}`
	out, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: delActor, Args: []byte(args)})
	if err != nil {
		return 0, err
	}
	var r struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	mustUnmarshal(t, out.Output, &r)
	return r.DeliveryID, nil
}

// ccExtraCleanup removes the rows THIS file adds on top of the SWT-8 fixture
// (a second inbound message and its raw item). It must run BEFORE
// cleanupDeliveryData: that one deletes the thread, whose FK the extra message
// still holds.
func ccExtraCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM normalized_messages WHERE external_message_id = ANY($1)`, []any{[]string{ccKatieMID}}},
		{`DELETE FROM raw_source_items WHERE external_id = ANY($1)`, []any{[]string{ccKatieRaw}}},
	} {
		if _, err := pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("cleanup %q: %v", st.sql, err)
		}
	}
}

func ccDeliveryCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}

// ---- criterion 5: what LANDS on the column -----------------------------------

func TestDraftDelivery_Integration_CcStoresTheNormalizedList(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	// D5 end to end: display name dropped, domain lower-cased, local part
	// preserved, duplicate folded away first-wins.
	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID,
		`["Katie <KEvans@CECollaboratory.COM>","billing@ITEST-DEL.example","kevans@cecollaboratory.com"]`)
	if err != nil {
		t.Fatalf("draft_delivery with a cc: %v", err)
	}
	want := []string{"KEvans@cecollaboratory.com", ccBilling}
	if got := readCc(t, ctx, pool, id); !ccEqual(got, want) {
		t.Errorf("stored cc = %q, want %q (address only, domain lower-cased, local part preserved, "+
			"case-insensitive dedupe first-wins)", got, want)
	}

	// Criterion 5, the other half: absent, null and [] all store {} — never
	// NULL, so no reader has to COALESCE (D4).
	for _, tc := range []struct{ name, ccJSON string }{
		{"empty list", `[]`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, tc.ccJSON)
			if err != nil {
				t.Fatalf("draft_delivery with cc %s: %v", tc.ccJSON, err)
			}
			if got := readCc(t, ctx, pool, id); len(got) != 0 {
				t.Errorf("cc %s stored %q, want the empty array", tc.ccJSON, got)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		id := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
		var isNull bool
		if err := pool.QueryRow(ctx, `SELECT cc IS NULL FROM deliveries WHERE id=$1`, id).Scan(&isNull); err != nil {
			t.Fatalf("read cc: %v", err)
		}
		if isNull {
			t.Error("a draft with no cc stored NULL; the column is NOT NULL DEFAULT '{}' so \"no Cc\" has ONE " +
				"representation (D4)")
		}
		if got := readCc(t, ctx, pool, id); len(got) != 0 {
			t.Errorf("a draft with no cc stored %q", got)
		}
	})

	// Criterion 16, second half: audit_events.args already carries the caller's
	// cc verbatim (the executor stores Call.Args). No new code — pinned so an
	// "args slimming" cannot quietly drop the record of who was copied.
	var args []byte
	if err := pool.QueryRow(ctx,
		`SELECT args FROM audit_events WHERE actor=$1 AND tool='draft_delivery'
		   AND jsonb_typeof(args->'cc') = 'array' AND args->'cc' <> '[]'::jsonb
		 ORDER BY id DESC LIMIT 1`, delActor).Scan(&args); err != nil {
		t.Fatalf("no draft_delivery audit row carrying cc: %v (invariant 3: validate → policy → audit start → "+
			"handler, with the caller's args recorded)", err)
	}
	var recorded struct {
		Cc []string `json:"cc"`
	}
	mustUnmarshal(t, args, &recorded)
	if len(recorded.Cc) == 0 {
		t.Errorf("audit_events.args = %s, want the caller's cc", args)
	}
}

// ---- criterion 6 + D7: a Cc may never be the From or the To ------------------

func TestDraftDelivery_Integration_CcMayNotBeTheFromOrTheTo(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	// The refusal must NAME the address and say which field it collided with
	// (the repo's "name the real reason" rule). anyOf lets the implementer
	// phrase the field naturally ("the To", "the recipient", "the reply goes
	// to") without this test dictating prose.
	refused := func(stage, ccJSON, addr string, anyOf []string) {
		t.Helper()
		before := ccDeliveryCount(t, ctx, pool, fx.parentID)
		_, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, ccJSON)
		if err == nil {
			t.Errorf("%s: a draft whose cc %s repeats a field of the same message was written; the recipient "+
				"would get the message twice and the review surface would show one person as two", stage, ccJSON)
			return
		}
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("%s: refusal = %q, want it to name the colliding address %q", stage, err, addr)
		}
		named := false
		for _, w := range anyOf {
			if strings.Contains(strings.ToLower(err.Error()), w) {
				named = true
			}
		}
		if !named {
			t.Errorf("%s: refusal = %q, want it to say WHICH field the address collided with (one of %v)",
				stage, err, anyOf)
		}
		if after := ccDeliveryCount(t, ctx, pool, fx.parentID); after != before {
			t.Errorf("%s: a refused draft changed the delivery count %d → %d", stage, before, after)
		}
	}
	var (
		asFrom = []string{"from", "sender", "sending mailbox", "account"}
		asTo   = []string{"to", "recipient", "reply"}
	)

	// The From is the resolved account email; the To is the thread's latest
	// inbound sender — the SAME value ResolveGmailRoute returns, so the rule
	// and the send agree on who the To is.
	refused("cc == From", `["`+delAcctEmail+`"]`, delAcctEmail, asFrom)
	// The comparison is case-insensitive on the ADDRESS part; what the refusal
	// quotes is the caller's own spelling, so assert on the domain both share.
	refused("cc == From, different case", `["`+strings.ToUpper(delAcctEmail)+`"]`, "ITEST-DEL-A@", asFrom)
	refused("cc == To", `["`+delInboundFrom+`"]`, delInboundFrom, asTo)
	refused("cc == To, display-name form", `["Client <`+delInboundFrom+`>"]`, delInboundFrom, asTo)
	refused("collision beside a good address", `["`+ccKatie+`","`+delInboundFrom+`"]`, delInboundFrom, asTo)

	// normalized_messages.sender holds the RAW From header for google rows (IK,
	// residue lane: "google writes the raw From header"), so the comparison
	// parses it before comparing. Without that parse, a sender stored as
	// `Client <client@…>` makes the rule compare a display-name string to a
	// bare address and admit the very address the reply is going to.
	if _, err := pool.Exec(ctx,
		`UPDATE normalized_messages SET sender=$2 WHERE external_message_id=$1`,
		delInboundMID, "Client Name <"+delInboundFrom+">"); err != nil {
		t.Fatalf("re-write the inbound sender in its raw form: %v", err)
	}
	refused("cc == To, with To stored as a raw From header", `["`+delInboundFrom+`"]`, delInboundFrom, asTo)

	// POSITIVE CONTROL: a third party is still accepted (D2 — any syntactically
	// valid address, no allowlist).
	if _, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `["`+ccKatie+`"]`); err != nil {
		t.Errorf("POSITIVE CONTROL: a third-party cc was refused: %v", err)
	}
}

// ---- criterion 4 + D12: gmail only, in the validator AND in the schema -------

func TestDraftDelivery_Integration_CcIsGmailOnly(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	_, err := ex.Execute(ctx, executor.Call{Tool: "draft_delivery", Actor: delActor,
		Args: []byte(`{"task_id":` + itoa(fx.parentID) + `,"channel":"slack_reply","body":"b",` +
			`"target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000","cc":["` + ccKatie + `"]}`)})
	if err == nil {
		t.Error("a slack_reply draft carrying a cc was written; slack has no carbon copy and its send path " +
			"would silently drop a recipient the caller asked for")
	} else if !strings.Contains(err.Error(), "cc") || !strings.Contains(err.Error(), "gmail") {
		t.Errorf("refusal = %q, want it to name cc and gmail", err)
	}

	// D12's backstop, against a DIRECT writer (not through the executor):
	// deliveries_cc_gmail_check.
	_, err = pool.Exec(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, cc, created_by)
		 VALUES ($1,'slack_reply','https://app.slack.com/client/TITEST/CITEST/p1750000000000000','b','drafted',
		         ARRAY[$2]::text[], $3)`, fx.parentID, ccKatie, delActor)
	if err == nil {
		t.Error("a direct INSERT put a cc on a slack_reply row; deliveries_cc_gmail_check must refuse it")
	} else if !strings.Contains(err.Error(), "deliveries_cc_gmail_check") {
		t.Errorf("the refusal came from something other than deliveries_cc_gmail_check: %v", err)
	}
}

// ---- criteria 7, 8, 9: update_delivery ---------------------------------------

func TestUpdateDelivery_Integration_CcReplaceClearUnchangedAndDraftedOnly(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `["`+ccKatie+`"]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	update := func(args string) error {
		_, err := ex.Execute(ctx, executor.Call{Tool: "update_delivery", Actor: delActor,
			Args: []byte(`{"delivery_id":` + itoa(id) + `,` + args + `}`)})
		return err
	}

	// (a) A list REPLACES wholesale, normalized on the way in (criterion 8).
	if err := update(`"cc":["Billing <` + upperDomain(ccBilling) + `>","` + ccKatie + `"]`); err != nil {
		t.Fatalf("update_delivery cc: %v", err)
	}
	if got, want := readCc(t, ctx, pool, id), []string{ccBilling, ccKatie}; !ccEqual(got, want) {
		t.Errorf("after a replace cc = %q, want %q", got, want)
	}

	// (b) cc ALONE is a valid update (criterion 7) — already proven by (a),
	// which sent no subject and no body.

	// (c) ABSENT leaves it unchanged...
	if err := update(`"body":"new words"`); err != nil {
		t.Fatalf("update_delivery body: %v", err)
	}
	if got, want := readCc(t, ctx, pool, id), []string{ccBilling, ccKatie}; !ccEqual(got, want) {
		t.Errorf("an update with no cc key changed the cc to %q; absent means unchanged (D6)", got)
	}
	// ...and so does null.
	if err := update(`"body":"newer words","cc":null`); err != nil {
		t.Fatalf("update_delivery cc null: %v", err)
	}
	if got, want := readCc(t, ctx, pool, id), []string{ccBilling, ccKatie}; !ccEqual(got, want) {
		t.Errorf("cc:null changed the cc to %q; null unmarshals to a nil pointer and reads as ABSENT (D6)", got)
	}

	// (d) [] CLEARS. The opposite direction of the same mutation (8).
	if err := update(`"cc":[]`); err != nil {
		t.Fatalf("update_delivery cc []: %v", err)
	}
	if got := readCc(t, ctx, pool, id); len(got) != 0 {
		t.Errorf("cc:[] left %q; an empty list CLEARS the recipients (D6) — Salvador deleting Katie from the "+
			"box and seeing the save succeed must not still send to her", got)
	}

	// (e) The same From/To rule as draft_delivery (criterion 8).
	if err := update(`"cc":["` + delInboundFrom + `"]`); err == nil {
		t.Error("update_delivery set the cc to the thread's inbound sender; the To collision rule applies here too")
	}
	if err := update(`"cc":["` + delAcctEmail + `"]`); err == nil {
		t.Error("update_delivery set the cc to the From account")
	}

	// (f) The same syntax rules.
	for _, bad := range []string{`"cc":["not an address"]`, `"cc":[""]`, `"cc":["josé@x.io"]`} {
		if err := update(bad); err == nil {
			t.Errorf("update_delivery accepted %s", bad)
		}
	}

	// (g) gmail only, under the row lock where the channel is read.
	var slackID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (task_id, channel, target_ref, body, status, created_by)
		 VALUES ($1,'slack_reply','https://app.slack.com/client/TITEST/CITEST/p1750000000000000','their words',
		         'drafted',$2) RETURNING id`, fx.parentID, delActor).Scan(&slackID); err != nil {
		t.Fatalf("seed slack draft: %v", err)
	}
	if _, err := ex.Execute(ctx, executor.Call{Tool: "update_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(slackID) + `,"cc":["` + ccKatie + `"]}`)}); err == nil {
		t.Error("update_delivery put a cc on a slack_reply row")
	}

	// (h) Criterion 9: a Cc cannot change after approval.
	if err := update(`"cc":["` + ccKatie + `"]`); err != nil {
		t.Fatalf("re-set the cc before approving: %v", err)
	}
	approve(t, ctx, ex, id)
	if s := deliveryStatus(t, ctx, pool, id); s != "approved" {
		t.Fatalf("status = %q, want approved", s)
	}
	err = update(`"cc":["` + ccBilling + `"]`)
	if err == nil {
		t.Error("update_delivery changed the cc of an APPROVED row; editing an approved draft bypasses the " +
			"approval Salvador gave to a specific recipient list (invariant 4a)")
	} else if !strings.Contains(err.Error(), "not drafted") {
		t.Errorf("refusal = %q, want the drafted-only refusal", err)
	}
	if got, want := readCc(t, ctx, pool, id), []string{ccKatie}; !ccEqual(got, want) {
		t.Errorf("after the refused edit the approved row's cc = %q, want %q unchanged", got, want)
	}

	// Criterion 16: update_delivery's audit args carry the cc too.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE actor=$1 AND tool='update_delivery'
		   AND jsonb_typeof(args->'cc') = 'array' AND args->'cc' <> '[]'::jsonb`,
		delActor).Scan(&n); err != nil {
		t.Fatalf("count update_delivery audit rows: %v", err)
	}
	if n == 0 {
		t.Error("no update_delivery audit_events row carries cc in its args")
	}
}

// ---- criterion 10 + D8: the approval is bound to the Cc ----------------------

func TestApproveDelivery_Integration_CcIsBoundToTheHash(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	ex := deliveryExecutor(pool)

	// draftGmail writes subject "Re: login broken", body "draft body", no cc.
	id := draftGmail(t, ctx, ex, fx.parentID, fx.threadID)
	shown := tools.DeliveryContentHash("Re: login broken", "draft body", nil)

	// A Cc is added between the page render and the Approve click.
	callOK(t, ctx, ex, delActor, "update_delivery", `{"delivery_id":`+itoa(id)+`,"cc":["`+ccKatie+`"]}`)

	_, err := ex.Execute(ctx, executor.Call{Tool: "approve_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `,"expect_content_hash":"` + shown + `"}`)})
	if err == nil {
		t.Fatal("approve_delivery with the hash of what Salvador SAW approved a row that gained a recipient " +
			"afterwards; without the cc in the hash, \"he saw every Cc\" is false in exactly that window (D8)")
	}
	if !strings.Contains(err.Error(), "changed since it was shown to you") {
		t.Errorf("stale approve refused with %q, want the reload-and-review message", err)
	}
	if s := deliveryStatus(t, ctx, pool, id); s != "drafted" {
		t.Errorf("after the refused approve status = %q, want drafted", s)
	}

	// The hash of the CURRENT row — the Cc included — approves.
	current := tools.DeliveryContentHash("Re: login broken", "draft body", []string{ccKatie})
	callOK(t, ctx, ex, delActor, "approve_delivery",
		`{"delivery_id":`+itoa(id)+`,"expect_content_hash":"`+current+`"}`)
	if s := deliveryStatus(t, ctx, pool, id); s != "approved" {
		t.Errorf("approve with the current hash left status %q, want approved", s)
	}

	// The same binding for Deny/Redo (reject_delivery shares the hash).
	id2, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `["`+ccKatie+`"]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	stale := tools.DeliveryContentHash("Re: login broken", "draft body", nil)
	if _, err := ex.Execute(ctx, executor.Call{Tool: "reject_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(id2) + `,"expect_content_hash":"` + stale + `"}`)}); err == nil {
		t.Error("reject_delivery accepted a hash computed without the row's cc")
	}
}

// ---- criteria 11, 12, 15, 16: the send --------------------------------------

func TestSendDelivery_Integration_CcOnTheWire(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)

	fx := seedDeliveryFixture(t, ctx, pool)
	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)

	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `["`+ccKatie+`","`+ccBilling+`"]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	approve(t, ctx, ex, id)
	callOK(t, ctx, ex, delActor, "send_delivery", `{"delivery_id":`+itoa(id)+`}`)

	if fake.calls != 1 {
		t.Fatalf("transport calls = %d, want 1", fake.calls)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(fake.lastRaw)))
	if err != nil {
		t.Fatalf("sent bytes are not a parseable message: %v\n%s", err, fake.lastRaw)
	}
	addrs, err := mail.ParseAddressList(msg.Header.Get("Cc"))
	if err != nil {
		t.Fatalf("sent message's Cc header %q does not parse: %v (criterion 11: the send builds its message "+
			"from the cc read in phase 1's locked SELECT)", msg.Header.Get("Cc"), err)
	}
	got := []string{}
	for _, a := range addrs {
		got = append(got, a.Address)
	}
	if want := []string{ccKatie, ccBilling}; !ccEqual(got, want) {
		t.Errorf("sent Cc = %q, want %q — the STORED list, in order", got, want)
	}
	// The envelope: a Cc that is not a recipient never arrives. (The transport
	// derives it from these headers — google.recipientsFromMIME — so the header
	// assertion above IS the envelope assertion; the unit test
	// TestSubmitSMTP_EnvelopeIncludesTheCc pins the derivation itself.)
	if !strings.Contains(string(fake.lastRaw), "Cc: ") {
		t.Errorf("the sent bytes carry no Cc header:\n%s", fake.lastRaw)
	}

	// Criterion 16: the delivery_sent payload records who was actually copied.
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='delivery_sent'
		 ORDER BY id DESC LIMIT 1`, fx.parentID).Scan(&payload); err != nil {
		t.Fatalf("read delivery_sent event: %v", err)
	}
	var ev struct {
		Cc []string `json:"cc"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("delivery_sent payload is not JSON: %v", err)
	}
	if want := []string{ccKatie, ccBilling}; !ccEqual(ev.Cc, want) {
		t.Errorf("delivery_sent payload cc = %q, want %q: send_delivery's audit args are {delivery_id} and "+
			"cannot carry it, so this event is the post-hoc record of who the message went to (D14). Payload: %s",
			ev.Cc, want, payload)
	}

	// Criterion 15: idempotency is untouched. A row carrying sent_external_id
	// refuses forever, Cc or no Cc.
	_, err = ex.Execute(ctx, executor.Call{Tool: "send_delivery", Actor: delActor,
		Args: []byte(`{"delivery_id":` + itoa(id) + `}`)})
	if err == nil || !strings.Contains(err.Error(), "invariant 4") {
		t.Errorf("re-sending a sent row with a cc = %v, want the invariant-4 refusal", err)
	}
	if fake.calls != 1 {
		t.Errorf("transport calls = %d after the refused resend, want 1", fake.calls)
	}
}

// D7's send-time half + criterion 12. The To is re-resolved at send (the known
// SWT-46 gap: a newer inbound message can change it between approval and Send).
// A stored Cc that now equals the To or the From is DROPPED, not refused:
// dropping narrows the recipient set and can never surprise anybody, while
// refusing would wedge an approved delivery over a cosmetic duplicate.
func TestSendDelivery_Integration_CcEqualToTheResolvedToIsDropped(t *testing.T) {
	ctx := context.Background()
	pool := newToolsPool(t, ctx)
	defer pool.Close()
	ccExtraCleanup(t, ctx, pool)
	cleanupDeliveryData(t, ctx, pool)
	defer cleanupDeliveryData(t, ctx, pool)
	defer ccExtraCleanup(t, ctx, pool) // LIFO: before cleanupDeliveryData

	fx := seedDeliveryFixture(t, ctx, pool)
	fake := &fakeGmailSender{pool: pool}
	tools.SetGmailSender(fake)
	ex := deliveryExecutor(pool)

	// Katie is Cc'd, plus a third party. Both are legal at draft time.
	id, err := draftGmailCc(t, ctx, ex, fx.parentID, fx.threadID, `["`+ccKatie+`","`+ccBilling+`"]`)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	approve(t, ctx, ex, id)

	// ...then Katie herself replies on the thread, so SHE is the To now.
	var rawID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash)
		 VALUES ($1,$2,'{}','itest-del-cc-hash-2') RETURNING id`, fx.accountID, ccKatieRaw).Scan(&rawID); err != nil {
		t.Fatalf("seed raw: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound',$4, now() + interval '1 hour',
		         'taking this over', 'login broken', $3, 'gmail')`, rawID, fx.threadID, ccKatie, ccKatieMID); err != nil {
		t.Fatalf("seed newer inbound from the Cc'd address: %v", err)
	}
	if r, err := tools.ResolveGmailRoute(ctx, pool, fx.accountID, fx.threadID); err != nil || r.To != ccKatie {
		t.Fatalf("premise: the re-resolved To = %+v (err %v), want %s", r, err, ccKatie)
	}

	callOK(t, ctx, ex, delActor, "send_delivery", `{"delivery_id":`+itoa(id)+`}`)

	msg, err := mail.ReadMessage(strings.NewReader(string(fake.lastRaw)))
	if err != nil {
		t.Fatalf("sent bytes are not parseable: %v", err)
	}
	if to := msg.Header.Get("To"); !strings.Contains(to, ccKatie) {
		t.Fatalf("premise: sent To = %q, want the re-resolved %s", to, ccKatie)
	}
	ccHeader := msg.Header.Get("Cc")
	if strings.Contains(ccHeader, ccKatie) {
		t.Errorf("sent Cc = %q: an address equal to the send-time To must be DROPPED (D7) — the same person "+
			"twice on one message", ccHeader)
	}
	if !strings.Contains(ccHeader, ccBilling) {
		t.Errorf("sent Cc = %q: the REST of the list must still be sent — dropping one address is not "+
			"dropping the Cc", ccHeader)
	}

	// The stored row is untouched (the drop is about this send, not the record)
	// and the event says who actually got it (D14).
	if got, want := readCc(t, ctx, pool, id), []string{ccKatie, ccBilling}; !ccEqual(got, want) {
		t.Errorf("stored cc = %q after the send, want %q: the send-time drop narrows the message, it does not "+
			"rewrite the approved row", got, want)
	}
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type='delivery_sent'
		 ORDER BY id DESC LIMIT 1`, fx.parentID).Scan(&payload); err != nil {
		t.Fatalf("read delivery_sent event: %v", err)
	}
	var ev struct {
		Cc []string `json:"cc"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("delivery_sent payload is not JSON: %v", err)
	}
	if want := []string{ccBilling}; !ccEqual(ev.Cc, want) {
		t.Errorf("delivery_sent payload cc = %q, want %q — the SENT set, after the drop (D14)", ev.Cc, want)
	}
}

// upperDomain upper-cases the DOMAIN only. D5 lower-cases the domain and leaves
// the local part as the caller gave it, so a fixture that shouted the whole
// address would (rightly) come back with a shouted local part.
func upperDomain(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return addr
	}
	return addr[:at] + strings.ToUpper(addr[at:])
}
