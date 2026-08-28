//go:build integration

package triage_test

// SWT-21 post-review: triage's neighbour fold must skip OUTBOUND messages, for
// the same structural reason drafts must.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run NeighbourDirection ./internal/triage/
//
// WHY IT IS ASSERTED ON AssembleContext RATHER THAN THROUGH Run.
// Triage's inbox filter serves only messages whose latest capture decision is
// `unmatched`, and unmatched is ClassRestricted — so the focus message is
// restricted by construction and the fold can never change what Run does today.
// A test written through Run would therefore pass no matter what the neighbour
// query returns: it would be measuring the inbox filter, not the fold.
//
// The rule still has to be pinned. The capture engine filters
// `direction = 'inbound'` (invariant 5), so an outbound message can NEVER carry
// a capture decision — classifying one as `unseen` means "restricted forever",
// not "not looked at yet". That defect was live in drafts, where the fold does
// decide, and it would become live here the moment SWT-22 widens triage's inbox.
// Pinning it now costs one test; finding it later costs a permanently stalled
// worker with no error anywhere.
//
// Mutual-cleanup pact: owns the source account `itest-tnd@triage.example.test`
// and the thread key `itest-tnd:thread`, deleted before and after.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/provider"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/triage"
)

const (
	tndAccount = "itest-tnd@triage.example.test"
	tndThread  = "itest-tnd:thread"
)

func tndCleanup(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DELETE FROM capture_decisions WHERE message_id IN (
		   SELECT id FROM normalized_messages WHERE thread_id IN (
		     SELECT id FROM normalized_threads WHERE thread_key = 'itest-tnd:thread'))`,
		`DELETE FROM normalized_messages WHERE thread_id IN (
		   SELECT id FROM normalized_threads WHERE thread_key = 'itest-tnd:thread')`,
		`DELETE FROM normalized_threads WHERE thread_key = 'itest-tnd:thread'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN (
		   SELECT id FROM source_accounts WHERE account_email = 'itest-tnd@triage.example.test')`,
		`DELETE FROM source_accounts WHERE account_email = 'itest-tnd@triage.example.test'`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func TestNeighbourDirection_Integration_OutboundIsNotFolded(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tndCleanup(t, ctx, pool)
	t.Cleanup(func() { tndCleanup(t, ctx, pool) })

	ins := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", q, err)
		}
		return id
	}

	account := ins(`INSERT INTO source_accounts (provider, account_email, scopes, send_enabled, calendar_in_availability)
	                VALUES ('google',$1,'{}',true,false) RETURNING id`, tndAccount)
	threadID := ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
	                 VALUES ($1,'itest-tnd','[]') RETURNING id`, tndThread)

	msg := func(external, direction, body string, minsAgo int) (msgID, rawID int64) {
		t.Helper()
		rawID = ins(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		             VALUES ($1,$2,'{}',$3, now()) RETURNING id`, account, external, "h-"+external)
		msgID = ins(`INSERT INTO normalized_messages
		               (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		             VALUES ($1,$2,$3,$4, now() - make_interval(mins => $5), $6,'itest-tnd','x@itest-tnd.example','gmail')
		             RETURNING id`, rawID, threadID, direction, external, minsAgo, body)
		return msgID, rawID
	}

	focusID, focusRaw := msg("itest-tnd-focus", "inbound", "the message being triaged", 10)
	inboundID, _ := msg("itest-tnd-inbound", "inbound", "an earlier inbound message", 30)
	outboundID, _ := msg("itest-tnd-outbound", "outbound", "our own reply", 20)

	// The focus message is triage's inbox shape. The inbound neighbour is
	// evaluated-but-unmatched. The outbound one gets NOTHING, which is what the
	// capture engine guarantees for every outbound message forever.
	for _, id := range []int64{focusID, inboundID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO capture_decisions (message_id, mode, action, reason)
			 VALUES ($1,'shadow','unmatched','itest-tnd')`, id); err != nil {
			t.Fatalf("seed decision for %d: %v", id, err)
		}
	}

	// Guard the premise instead of assuming it.
	var outboundDecisions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM capture_decisions WHERE message_id = $1`, outboundID).Scan(&outboundDecisions); err != nil {
		t.Fatalf("count outbound decisions: %v", err)
	}
	if outboundDecisions != 0 {
		t.Fatalf("the outbound message has %d decision(s); this fixture represents the structurally "+
			"undecidable case and proves nothing with one present", outboundDecisions)
	}

	mc, err := triage.NewStore(pool).AssembleContext(ctx, triage.PendingMessage{
		MessageID:       focusID,
		RawSourceItemID: focusRaw,
		ThreadID:        threadID,
		Direction:       "inbound",
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// Two neighbours exist on the thread; exactly ONE may be folded.
	if len(mc.NeighbourAttribution) != 1 {
		t.Fatalf("NeighbourAttribution has %d entries, want 1. The thread holds one inbound neighbour and one "+
			"OUTBOUND message, and the outbound one must not be folded: it can never carry a capture "+
			"decision (the engine filters direction='inbound', which is invariant 5), so folding it would "+
			"mean 'restricted forever' rather than 'not classified yet'", len(mc.NeighbourAttribution))
	}
	if got := mc.NeighbourAttribution[0].State; got != provider.AttrUnmatched {
		t.Errorf("the folded neighbour's state = %v, want AttrUnmatched — the inbound one, which HAS a "+
			"decision. Getting AttrUnseen here means the outbound message was folded instead", got)
	}

	// The focus message itself is unchanged by any of this.
	if mc.Attribution != provider.AttrUnmatched {
		t.Errorf("focus Attribution = %v, want AttrUnmatched", mc.Attribution)
	}
}
