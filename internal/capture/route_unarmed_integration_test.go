//go:build integration

package capture_test

// REGRESSION — bug sana-email-not-captured (Jira SWT-58).
// docs/bugs/sana-email-not-captured_DIAGNOSIS.md, "Regression tests" (the two
// internal/capture items). Same suite, fixtures and isolation rules as
// route_integration_test.go (raSuite; isolated DB only, FATAL on 192.168.50.49):
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_sanabug?sslmode=disable' \
//	  TZ=UTC go test -tags integration -p 1 -count=1 -run 'TestRegression_SanaEmailNotCaptured' ./internal/capture/
//
// The bug: routeInbox drops an unarmed account's messages in SQL
// (`sa.route_after IS NOT NULL`), so DecideRoute never runs for them and every
// counter route_apply prints stays 0. `written=0` with every reason at 0 read
// the same as an empty inbox while client mail (Sana, 291568) sat verdicted and
// waiting on an account nobody had armed. The fix: a SEPARATE count of messages
// that match routeInbox's predicates except that the account is unarmed —
// inbound; a live 'unmatched' decision exists; the latest decision (any mode)
// is 'unmatched'; the receiving account has candidate rows AND
// route_after IS NULL; sent within cfg.Since. routeInbox itself is unchanged,
// and the count NEVER adds to Written (pipelined's processed, which re-runs the
// stage at once when it fills the limit and publishes `routed` when >= 1).
//
// THE SURFACE IS NOT PINNED BY NAME (DIAGNOSIS open question 4): raUnarmed
// reads either a RouteStats field `Unarmed` (an int) or the Unrouted key
// "account_unarmed". The map key must be PRESENT every pass, zero included, as
// the log line prints it every pass. Neither exists today, so every test here
// FAILS at raUnarmed before the fix (reported with Errorf, so the rest of each
// test still runs and shows what already holds).
//
// TEST THE COLUMN, NOT THE FIXTURE: the unarmed state is the real
// source_accounts.route_after column, flipped inline by UPDATE (test code may
// arm; route_structure_test.go forbids non-test Go from doing so).

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
)

// raUnarmed returns route_apply's "waiting on an unarmed account" counter from
// one pass's stats; ok=false (and a test error) if RouteStats carries none.
func raUnarmed(t *testing.T, st capture.RouteStats) (int, bool) {
	t.Helper()
	if f := reflect.ValueOf(st).FieldByName("Unarmed"); f.IsValid() {
		if !f.CanInt() {
			t.Errorf("RouteStats.Unarmed is a %s; want an int count of messages waiting on an unarmed account", f.Kind())
			return 0, false
		}
		return int(f.Int()), true
	}
	if n, ok := st.Unrouted["account_unarmed"]; ok {
		return n, true
	}
	t.Errorf("RouteStats carries no unarmed-waiting counter (neither a field `Unarmed` nor a PRESENT "+
		"Unrouted[\"account_unarmed\"] key; stats = %+v). SWT-58: routeInbox drops route_after-NULL accounts in SQL, "+
		"so without this count route_apply's pass reads `written=0` with every reason at 0 — the same as an empty "+
		"inbox — while verdicted client mail waits on an account nobody armed", st)
	return 0, false
}

// The counter counts exactly the messages routeInbox would take if the account
// were armed, and nothing else. Then the column is flipped and the count goes
// to 0 while the message routes.
//
// MUTATIONS (each must turn this red once the fix exists):
//   - delete the count query, or never set the field → raUnarmed fails / n = 0;
//   - drop `route_after IS NULL` from it → the two armed controls count (n = 3);
//   - drop the candidate-rows clause → the no-candidate account counts;
//   - drop inbound / EXISTS live unmatched / latest unmatched / the window →
//     the matching negative counts;
//   - fold the count into Written → Written != 0 on the first pass.
func TestRegression_SanaEmailNotCaptured_UnarmedWaitingIsCounted(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx) // s.unarmed: route_after NULL, candidates collab (default) + reeng
	h := time.Hour

	// COUNTED — Sana's shape: inbound, live unmatched, an ok grounded route verdict.
	sana, sanaRaw := s.msg(t, ctx, s.unarmed, "t-sana", "inbound", 2*h)
	s.dec(t, ctx, sana, sanaRaw, "live", "unmatched", 0)
	s.verdict(t, ctx, sana, sanaRaw, s.unarmed, s.collab, true, 110*time.Minute)

	// NOT counted: an unarmed account, but routeInbox's OTHER predicates fail.
	out, outRaw := s.msg(t, ctx, s.unarmed, "t-u-outbound", "outbound", 2*h) // not inbound (synthetic live row isolates the clause)
	s.dec(t, ctx, out, outRaw, "live", "unmatched", 0)
	sh, shRaw := s.msg(t, ctx, s.unarmed, "t-u-shadowonly", "inbound", 2*h) // no live decision
	s.dec(t, ctx, sh, shRaw, "shadow", "unmatched", 0)
	rp, rpRaw := s.msg(t, ctx, s.unarmed, "t-u-repointed", "inbound", 2*h) // latest decision is not unmatched
	s.dec(t, ctx, rp, rpRaw, "live", "unmatched", 0)
	s.dec(t, ctx, rp, rpRaw, "shadow", "attributed", s.collab)
	old, oldRaw := s.msg(t, ctx, s.unarmed, "t-u-old", "inbound", 800*h) // outside the 720h window
	s.dec(t, ctx, old, oldRaw, "live", "unmatched", 0)
	s.verdict(t, ctx, old, oldRaw, s.unarmed, s.collab, true, 0)
	nocand := s.id(t, ctx, `INSERT INTO source_accounts (provider, account_email, send_enabled)
	                         VALUES ($1,'itest-caproute-unarmed-nocand@example.test',false) RETURNING id`, raProvider)
	nc, ncRaw := s.msg(t, ctx, nocand, "t-u-nocand", "inbound", 2*h) // unarmed, but no candidate rows (B-D1)
	s.dec(t, ctx, nc, ncRaw, "live", "unmatched", 0)

	// ARMED controls that stay unrouted this pass (so a count query that lost
	// its `route_after IS NULL` would see them): no verdict on an armed account,
	// and a verdict recorded before s.late was armed (an hour ago).
	ap, apRaw := s.msg(t, ctx, s.hoc, "t-armed-pending", "inbound", 2*h)
	s.dec(t, ctx, ap, apRaw, "live", "unmatched", 0)
	ab, abRaw := s.msg(t, ctx, s.late, "t-armed-before", "inbound", 3*h)
	s.dec(t, ctx, ab, abRaw, "live", "unmatched", 0)
	s.verdict(t, ctx, ab, abRaw, s.late, s.reeng, true, 2*h)

	st := s.apply(t, ctx)
	if st.Unrouted[capture.RouteReasonPendingVerdict] != 1 || st.Unrouted[capture.RouteReasonBeforeArming] != 1 {
		t.Errorf("POSITIVE CONTROL: Unrouted = %v, want pending_verdict 1 and verdict_before_arming 1 (the two armed "+
			"controls were read)", st.Unrouted)
	}
	if st.Written != 0 {
		t.Errorf("Written = %d, want 0: an unarmed account writes nothing (B7), and the unarmed count must never "+
			"feed Written", st.Written)
	}
	if r, ok := s.route(t, ctx, sana); ok {
		t.Errorf("the message on the unarmed account was routed: %+v (B7: route_after NULL writes nothing)", r)
	}
	if n, ok := raUnarmed(t, st); ok && n != 1 {
		t.Errorf("unarmed-waiting count = %d, want exactly 1 (the Sana-shaped message). Not counted: outbound, "+
			"shadow-only, re-pointed, outside the window, an unarmed account with no candidates, and the two ARMED "+
			"controls (pending_verdict, verdict_before_arming)", n)
	}

	// The column: arm the account a day ago (the verdict, 110 min old, is after
	// it) → the message routes by the model step and the count drops to 0.
	s.exec(t, ctx, `UPDATE source_accounts SET route_after = now() - interval '1 day' WHERE id = $1`, s.unarmed)
	st = s.apply(t, ctx)
	if r, ok := s.route(t, ctx, sana); !ok || r.step != capture.RouteStepModel || r.project != s.collab {
		t.Errorf("after arming, the message = %+v (found %v), want a model row for collab — the positive control", r, ok)
	}
	if n, ok := raUnarmed(t, st); ok && n != 0 {
		t.Errorf("after arming its account, unarmed-waiting count = %d, want 0 (the no-candidate account's message "+
			"never counted; the rest are not routable)", n)
	}
	_, _, _, _, _ = out, sh, rp, old, nc
}

// The hot-loop guard: a pass whose only routable-if-armed work sits on an
// unarmed account moves NOTHING — Written 0, no step, no unrouted reason (the
// driver never decided them) — and a second pass reports the same numbers, so
// pipelined's processed (= Written) stays 0 and the stage never re-runs at once
// over the same waiting rows.
//
// MUTATIONS: add the count to Written → Written 3; fold unarmed rows into
// routeInbox (a zero ArmedAt reaches DecideRoute) → the thread and model rows
// get written and pending_verdict counts.
func TestRegression_SanaEmailNotCaptured_UnarmedCountNeverFeedsWritten(t *testing.T) {
	ctx := context.Background()
	s := newRASuite(t, ctx)
	h := time.Hour
	// Would be step 1 (thread) if armed.
	a1, a1r := s.msg(t, ctx, s.unarmed, "t-u-thread", "inbound", 3*h)
	s.dec(t, ctx, a1, a1r, "live", "attributed", s.reeng)
	a2, a2r := s.msg(t, ctx, s.unarmed, "t-u-thread", "inbound", 2*h)
	s.dec(t, ctx, a2, a2r, "live", "unmatched", 0)
	// Would be step 3 (model) if armed.
	c1, c1r := s.msg(t, ctx, s.unarmed, "t-u-model", "inbound", 2*h)
	s.dec(t, ctx, c1, c1r, "live", "unmatched", 0)
	s.verdict(t, ctx, c1, c1r, s.unarmed, s.reeng, true, 0)
	// Would be pending_verdict if armed.
	e1, e1r := s.msg(t, ctx, s.unarmed, "t-u-pending", "inbound", 2*h)
	s.dec(t, ctx, e1, e1r, "live", "unmatched", 0)

	for pass := 1; pass <= 2; pass++ {
		st := s.apply(t, ctx)
		if n, ok := raUnarmed(t, st); ok && n != 3 {
			t.Errorf("pass %d: unarmed-waiting count = %d, want 3 (thread-, model- and pending-shaped messages; the "+
				"attributed neighbour is not routable)", pass, n)
		}
		steps := 0
		for _, n := range st.ByStep {
			steps += n
		}
		if st.Written != 0 || steps != 0 {
			t.Errorf("pass %d: Written = %d, ByStep = %v; want 0 and none. The unarmed count must NEVER contribute to "+
				"Written: pipelined returns Written as processed, and processed >= 1 publishes `routed` while a full "+
				"pass re-runs at once — a hot loop over rows that cannot move until a human arms the account", pass,
				st.Written, st.ByStep)
		}
		for _, reason := range []string{capture.RouteReasonPendingVerdict, capture.RouteReasonNoDefault,
			capture.RouteReasonBeforeArming, capture.RouteReasonCandidateRevoked} {
			if st.Unrouted[reason] != 0 {
				t.Errorf("pass %d: Unrouted[%q] = %d, want 0 — an unarmed message is never DECIDED (routeInbox stays "+
					"unchanged; DecideRoute is never handed a zero ArmedAt)", pass, reason, st.Unrouted[reason])
			}
		}
	}
	for _, m := range []int64{a2, c1, e1} {
		if r, ok := s.route(t, ctx, m); ok {
			t.Errorf("message %d on an unarmed account was routed: %+v", m, r)
		}
	}
}
