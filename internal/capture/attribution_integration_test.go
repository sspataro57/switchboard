//go:build integration

package capture_test

// SWT-29 (docs/tickets/funnel-view_SPEC.md, criteria 14, 15 and 16):
// capture.AttributionTrend against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run AttributionTrend ./internal/capture/
//
// WHY INTEGRATION. The three states are produced by a LEFT JOIN LATERAL over
// capture_decisions and a direction filter on normalized_messages — both are
// predicates whose input comes from COLUMNS, so a fixture that supplied the
// values would be certifying itself (IK: "the guard whose column no query
// selected"; "test the column, not the fixture"). The mutation checks in the
// SPEC's Verification step 3 are aimed at this file: change the LEFT JOIN
// LATERAL to a JOIN and the "not yet evaluated" assertion must go red.
//
// CROSS-POLLUTION PACT. normalized_messages and capture_decisions are shared
// with 19 other suites under `-p 1`, and AttributionTrend is GLOBAL by design
// (it is a trend over the whole funnel), so every assertion here is a DELTA
// around this suite's own seeding. An absolute "today shows 3 matched" would be
// a flake, not a check. capture_decisions.message_id is ON DELETE CASCADE; the
// decisions are still deleted explicitly so a failure reads as a cleanup
// failure rather than as a mysterious FK error two statements later.
//
// ANTI-DATE-ROT: every timestamp is `now()` or `now() - interval '...'`.
//
// GREENFIELD NOTE — EXPECTED RED. capture.AttributionTrend and
// capture.DayAttribution do not exist, so this file compile-FAILS the
// integration build of internal/capture.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	atProvider = "itest-swt29-attr-src"
	atAccount  = "itest-swt29-attr@pg-main"
	atProject  = "itest-swt29-attr-proj"
)

type atSuite struct {
	pool      *pgxpool.Pool
	accountID int64
	projectID int64
}

func newATSuite(t *testing.T, ctx context.Context) *atSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); " +
			"use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &atSuite{pool: pool}
	s.cleanup(t, ctx)
	t.Cleanup(func() { s.cleanup(t, context.Background()) })
	return s
}

func (s *atSuite) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN
	                (SELECT id FROM source_accounts WHERE provider='` + atProvider + `'))`
	for _, q := range []string{
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-swt29-attr:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + atProvider + `')`,
		`DELETE FROM source_accounts WHERE provider='` + atProvider + `'`,
		`DELETE FROM projects WHERE slug='` + atProject + `'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

func (s *atSuite) seedParents(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, send_enabled)
		 VALUES ($1,$2,false) RETURNING id`, atProvider, atAccount).Scan(&s.accountID); err != nil {
		t.Fatalf("seed source_account: %v", err)
	}
	// ai_locality and ai_classify are named EXPLICITLY: a fixture that omits
	// them inherits defaults that have silently turned whole suites into skips
	// before (IK, residue lane / 0016's ai_locality trap).
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality, ai_classify)
		 VALUES ($1,$1,'itest-swt29','manual','dashboard','/tmp/itest','any',false) RETURNING id`,
		atProject).Scan(&s.projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// message inserts one normalized message `daysAgo` days back on the PIPELINE
// clock (created_at), which is the axis the whole page shares. sent_at is set to
// a deliberately different instant so a section that bucketed by the provider's
// clock would land on the wrong day and be caught.
func (s *atSuite) message(t *testing.T, ctx context.Context, label, direction string, daysAgo int) int64 {
	t.Helper()
	var rawID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, ingested_at, normalized_at)
		 VALUES ($1,$2,'{}'::jsonb,$3, now() - make_interval(days => $4), now()) RETURNING id`,
		s.accountID, "itest-swt29-attr-"+label, "itest-swt29-attr-h-"+label, daysAgo).Scan(&rawID); err != nil {
		t.Fatalf("seed raw item %s: %v", label, err)
	}
	var threadID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_threads (thread_key, subject, participants) VALUES ($1,$2,'[]') RETURNING id`,
		"itest-swt29-attr:"+label, "itest-swt29 "+label).Scan(&threadID); err != nil {
		t.Fatalf("seed thread %s: %v", label, err)
	}
	var msgID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, created_at,
		    body_text, subject, sender, channel)
		 VALUES ($1,$2,$3,$4, now() - interval '400 days', now() - make_interval(days => $5),
		         'itest body','itest-swt29 '||$6,'itest@swt29.example','gmail') RETURNING id`,
		rawID, threadID, direction, "itest-swt29-attr-"+label, daysAgo, label).Scan(&msgID); err != nil {
		t.Fatalf("seed message %s: %v", label, err)
	}
	return msgID
}

func (s *atSuite) decision(t *testing.T, ctx context.Context, messageID int64, action string) {
	t.Helper()
	var project any
	if action != "unmatched" {
		project = s.projectID
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow',$2,$3,'itest-swt29')`, messageID, action, project); err != nil {
		t.Fatalf("seed decision %s: %v", action, err)
	}
}

func atTrend(t *testing.T, ctx context.Context, pool *pgxpool.Pool, days int) map[string]capture.DayAttribution {
	t.Helper()
	rows, err := capture.AttributionTrend(ctx, pool, days)
	if err != nil {
		t.Fatalf("capture.AttributionTrend(days=%d): %v", days, err)
	}
	out := map[string]capture.DayAttribution{}
	var prev time.Time
	for i, r := range rows {
		key := r.Day.Format("2006-01-02")
		if _, dup := out[key]; dup {
			t.Fatalf("AttributionTrend returned two rows for %s; it is one row per day", key)
		}
		out[key] = r
		if i > 0 && !r.Day.Before(prev) {
			t.Fatalf("AttributionTrend is not ordered newest-first: %s follows %s",
				key, prev.Format("2006-01-02"))
		}
		prev = r.Day
	}
	return out
}

// ---- criteria 14, 15, 16 ------------------------------------------------------

func TestAttributionTrend_Integration_ThreeStatesInboundOnly(t *testing.T) {
	ctx := context.Background()
	s := newATSuite(t, ctx)
	s.seedParents(t, ctx)

	today := time.Now().Format("2006-01-02")
	before := atTrend(t, ctx, s.pool, 14)[today]

	// matched: the three routed actions, named positively.
	s.decision(t, ctx, s.message(t, ctx, "attributed", "inbound", 0), "attributed")
	s.decision(t, ctx, s.message(t, ctx, "task", "inbound", 0), "task")
	s.decision(t, ctx, s.message(t, ctx, "tasklog", "inbound", 0), "task_log")

	// LATEST wins. Shadow is re-runnable by design and writes a decision per
	// pass, so a message routinely carries several. This one was unmatched on an
	// earlier pass and attributed on a later one: it is MATCHED, once.
	relabelled := s.message(t, ctx, "relabelled", "inbound", 0)
	s.decision(t, ctx, relabelled, "unmatched")
	s.decision(t, ctx, relabelled, "attributed")

	// unmatched: the residue/triage inbox.
	s.decision(t, ctx, s.message(t, ctx, "unmatched", "inbound", 0), "unmatched")

	// not yet evaluated: NO decision row at all. Treating this as unmatched
	// hands every fresh message to the model before the rules run.
	s.message(t, ctx, "unseen", "inbound", 0)

	// OUTBOUND with no decision. It must land in NO column. capture filters
	// direction='inbound' (invariant 5), so this row can never carry a decision
	// on any pass in any mode — absent-because-impossible, not pending.
	s.message(t, ctx, "outbound", "outbound", 0)

	after := atTrend(t, ctx, s.pool, 14)[today]

	if d := after.Matched - before.Matched; d != 4 {
		t.Errorf("matched moved by %d, want 4 (attributed + task + task_log + the relabelled message whose "+
			"LATEST decision is attributed)", d)
	}
	if d := after.Unmatched - before.Unmatched; d != 1 {
		t.Errorf("unmatched moved by %d, want 1. The relabelled message must NOT be counted here: its "+
			"earlier unmatched row is history, and counting decision ROWS instead of MESSAGES makes every "+
			"number grow with the number of times someone ran the pass", d)
	}
	if d := after.Unevaluated - before.Unevaluated; d != 1 {
		t.Errorf("'not yet evaluated' moved by %d, want 1. Criterion 14: THREE states. No decision row is "+
			"UNSEEN — the engine has not looked at it — and it is neither matched nor the residue. If this "+
			"is 0, the lateral is an inner join and the third column is structurally always empty", d)
	}

	total := (after.Matched - before.Matched) + (after.Unmatched - before.Unmatched) + (after.Unevaluated - before.Unevaluated)
	if total != 6 {
		t.Errorf("the three columns moved by %d in total for 7 seeded messages, want 6 — the OUTBOUND "+
			"message must appear in no column at all (criterion 15). Counting it as 'not yet evaluated' "+
			"would be a lie about 21k production rows", total)
	}
}

// The window is the ?days= window the whole page shares, and it is applied on
// the PIPELINE clock. The old message below carries a sent_at inside the window
// (400 days back is outside; every seeded message's sent_at is 400 days back, so
// a section bucketing by the provider's clock would put ALL of them on one
// ancient day and none on today).
func TestAttributionTrend_Integration_WindowIsTheCreatedAtClock(t *testing.T) {
	ctx := context.Background()
	s := newATSuite(t, ctx)
	s.seedParents(t, ctx)

	s.decision(t, ctx, s.message(t, ctx, "recent", "inbound", 2), "unmatched")
	s.decision(t, ctx, s.message(t, ctx, "ancient", "inbound", 40), "unmatched")

	twoDaysAgo := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	fortyDaysAgo := time.Now().AddDate(0, 0, -40).Format("2006-01-02")

	got := atTrend(t, ctx, s.pool, 14)
	if _, ok := got[twoDaysAgo]; !ok {
		t.Errorf("the 2-day-old message produced no row for %s. Days are bucketed on "+
			"normalized_messages.created_at — the pipeline clock", twoDaysAgo)
	}
	if _, ok := got[fortyDaysAgo]; ok {
		t.Errorf("a 40-day-old message appears in a 14-day window (%s). ?days= is clamped to [1,90] and an "+
			"unbounded window lets a URL parameter schedule a full scan", fortyDaysAgo)
	}

	wide := atTrend(t, ctx, s.pool, 90)
	if _, ok := wide[fortyDaysAgo]; !ok {
		t.Errorf("POSITIVE CONTROL FAILED: the 40-day-old message is absent from a 90-day window too (%s), "+
			"so the assertion above proves nothing about the window", fortyDaysAgo)
	}
}
