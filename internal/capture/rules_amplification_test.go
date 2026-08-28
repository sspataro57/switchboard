//go:build integration

package capture_test

// The amplification guard (SWT-17, from the go-reviewer pass).
//
// The pending filter used to skip only messages carrying a LIVE decision. In
// shadow mode no live rows are ever written, so nothing was excluded and every
// pass re-evaluated the whole inbound corpus — measured at 49,415 messages and
// ~65 MB of body_text on production, times four connector mains on */15 plus a
// watch loop firing on every IMAP IDLE wake.
//
// Neither the unit suite nor `make integration` could see it: the fixture corpus
// is ten messages, so the amplification is invisible at test scale and arrives in
// production as a disk alert. This test makes the PROPERTY visible at fixture
// scale instead — a second shadow pass must consider nothing.
//
// It also pins the half that makes the fix safe. Keying the skip on cfg.Mode
// rather than "any decision at all" is what keeps the shadow -> live transition
// working: after a shadow period every message carries a shadow row, and a
// first live pass must still see all of them. "Any decision" would decide
// nothing, which is the failure this test would otherwise invite a future reader
// to introduce.

import (
	"context"
	"testing"

	"github.com/sspataro57/switchboard/internal/capture"
)

func TestCaptureRules_Integration_ShadowDoesNotReEvaluateItsOwnDecisions(t *testing.T) {
	ctx := context.Background()
	s := newCRSuite(t, ctx)

	first, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow"})
	if err != nil {
		t.Fatalf("first shadow pass: %v", err)
	}
	if first.Considered == 0 {
		t.Fatalf("first shadow pass considered 0 messages; the fixture corpus is empty and this test proves nothing")
	}

	second, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "shadow"})
	if err != nil {
		t.Fatalf("second shadow pass: %v", err)
	}
	if second.Considered != 0 {
		t.Errorf("second shadow pass considered %d messages, want 0. The pass is re-evaluating messages it has "+
			"already decided, which on the production corpus means ~49k messages and ~65 MB of body_text per "+
			"run, per main, every 15 minutes — an unbounded write amplification that no fixture-scale suite "+
			"can otherwise see", second.Considered)
	}

	// The half that keeps the fix safe: live must still see the whole corpus
	// after a shadow period, or going live decides nothing.
	live, err := capture.EvaluateRules(ctx, s.pool, s.ex, capture.RulesConfig{Mode: "live"})
	if err != nil {
		t.Fatalf("first live pass: %v", err)
	}
	// Deliberately "more than zero" rather than "equal to the shadow count":
	// live applies DefaultRulesHorizon while shadow runs unbounded, so the two
	// legitimately differ (9 vs 11 on this fixture). What must hold is that live
	// is not STARVED by the shadow rows — skipping on "any decision" would make
	// this exactly 0, and the transition to live would silently do nothing.
	if live.Considered == 0 {
		t.Errorf("first live pass considered 0 messages after a shadow period. Every message already carries "+
			"a shadow decision, so a filter keyed on ANY decision rather than the pass's OWN MODE starves "+
			"the transition: going live would decide nothing, forever, with no error. Shadow considered %d",
			first.Considered)
	}
}
