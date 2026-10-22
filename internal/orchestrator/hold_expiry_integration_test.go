//go:build integration

package orchestrator_test

// SWT-109 criterion 26 against real columns: R12's facts query lists an offer
// only for a live, unkept, unreleased hold past hold_expires_at, and the tick
// releases exactly those holds with reason expired.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	orch "github.com/sspataro57/switchboard/internal/orchestrator"
)

const (
	holdSlug  = "itest-orch-hold-expiry"
	holdAcct  = "itest-orch-hold-expiry@example.com"
	holdActor = "opsctl:itest-orch-hold-expiry"
)

func cleanupHolds(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ours := `(SELECT id FROM tasks WHERE project_id IN (SELECT id FROM projects WHERE slug='` + holdSlug + `'))`
	for _, s := range []string{
		`DELETE FROM task_events WHERE task_id IN ` + ours,
		`DELETE FROM deliveries WHERE task_id IN ` + ours + ` AND hold_for_delivery_id IS NOT NULL`,
		`DELETE FROM deliveries WHERE task_id IN ` + ours,
		`DELETE FROM source_accounts WHERE account_email='` + holdAcct + `'`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
	cleanupOrch(t, ctx, pool) // tasks, audit rows and the project (slug itest-orch-…)
}

func TestOrchestrator_Integration_HoldExpiryReleasesOnlyExpiredUnkeptHolds(t *testing.T) {
	ctx := context.Background()
	pool := newOrchPool(t, ctx)
	defer pool.Close()
	cleanupHolds(t, ctx, pool)
	defer cleanupHolds(t, ctx, pool)
	seedProjectDelivery(t, ctx, pool, holdSlug, "itest-orch-hold-client", "dashboard")
	ex := newOrchExecutor(pool)
	taskID := createReadyTask(t, ctx, ex, holdActor, holdSlug, "offer with holds")

	var acct int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability) VALUES ('google',$1,true) RETURNING id`,
		holdAcct).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	offer := func() int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, body, subject, status, sent_at, offered_windows)
			 VALUES ($1,'gmail','t','Re: t','sent',now()-interval '4 days','[]'::jsonb) RETURNING id`, taskID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	hold := func(offerID int64, startIn time.Duration, expiresIn time.Duration, kept bool) int64 {
		var id int64
		start := time.Now().Add(startIn).Truncate(time.Minute)
		var keptAt, expires any
		if kept {
			keptAt = time.Now()
		} else {
			expires = time.Now().Add(expiresIn)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, target_ref, body, subject, status, approval_source, from_account_id,
			                         starts_at, ends_at, hold_for_delivery_id, hold_expires_at, hold_kept_at, push_next_attempt_at)
			 VALUES ($1,'calendar',$2,'Reserved.','Hold','approved','switchboard',$3,$4,$5,$6,$7,$8,now()) RETURNING id`,
			taskID, holdAcct, acct, start, start.Add(time.Hour), offerID, expires, keptAt).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	expiredOffer := offer()
	expired := hold(expiredOffer, 24*time.Hour, -time.Hour, false)
	kept := hold(expiredOffer, 26*time.Hour, 0, true)
	freshOffer := offer()
	fresh := hold(freshOffer, 28*time.Hour, 24*time.Hour, false)

	engine := orch.NewEngine(pool, ex, &recordingPublisher{}, orch.Config{})
	if err := engine.TickOnce(ctx, time.Now()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	reason := func(id int64) string {
		var r *string
		if err := pool.QueryRow(ctx, `SELECT release_reason FROM deliveries WHERE id=$1`, id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		if r == nil {
			return ""
		}
		return *r
	}
	if got := reason(expired); got != "expired" {
		t.Errorf("the expired hold's release_reason = %q, want expired", got)
	}
	if got := reason(kept); got != "" {
		t.Errorf("the KEPT hold was released (%q); a picked time is a booking", got)
	}
	if got := reason(fresh); got != "" {
		t.Errorf("a hold that has not expired was released (%q)", got)
	}
	// A second tick finds nothing new (the facts query stops listing the offer).
	if err := engine.TickOnce(ctx, time.Now()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
}
