package capture

// SWT-40 Part B criterion B4 (docs/tickets/inquiry-promote_SPEC.md, B-D2,
// B-D4, B-D6, B-D7): capture.DecideRoute is PURE — one table row per step,
// plus the step-order and cutover rows. Zero I/O: no pool, no context, no
// clock (the arming instant and the verdict's instant arrive as values).
//
// ---- IMPOSED SURFACE (internal/capture/route.go) -----------------------------
//
// The SPEC fixes the call `DecideRoute(facts, candidates, verdict)`:
//
//	type RouteFacts struct {
//	    ThreadProjects []int64   // projects named by the LATEST decision of each OTHER
//	                             // inbound message on the thread (project_id NOT NULL)
//	    ArmedAt        time.Time // source_accounts.route_after (the driver never
//	                             // calls DecideRoute for an unarmed account)
//	}
//	type RouteCandidate struct {
//	    ProjectID int64
//	    IsDefault bool
//	}
//	type RouteVerdict struct {
//	    ExtractionID int64     // the classify_route ai_extractions.id
//	    ProjectID    int64     // fields.project_id: the resolved candidate, 0 = none chosen
//	    Grounded     bool      // fields.grounded (classify.Grounded, decided at classify time)
//	    RecordedAt   time.Time // ai_runs.created_at — the verdict clock (B-D7)
//	}
//	type RouteDecision struct {
//	    Step         string // "thread" | "single" | "model" | "default"; "" = write no row
//	    ProjectID    int64  // the row's project; 0 iff Step == ""
//	    ExtractionID int64  // set iff Step == "model"
//	    Reason       string // Step == "": "pending_verdict" | "no_default" | "verdict_before_arming"
//	}
//	func DecideRoute(facts RouteFacts, candidates []RouteCandidate, verdict *RouteVerdict) RouteDecision
//
//	// The route_apply driver (B-D6): writes capture_decisions rows directly
//	// (capture's own log), calls NO tool, takes capture's lock 0x5157_0015.
//	type RouteApplyConfig struct {
//	    Limit int
//	    Since time.Duration // REQUIRED > 0: the pass window steps 1-2 apply inside
//	}
//	type RouteStats struct {
//	    Written  int            // mode='route' rows inserted (pipelined's processed)
//	    ByStep   map[string]int // thread | single | model | default
//	    Unrouted map[string]int // pending_verdict | no_default | verdict_before_arming
//	}
//	var ErrRouteLockHeld error
//	func RunRouteApply(ctx context.Context, pool *pgxpool.Pool, cfg RouteApplyConfig) (RouteStats, error)
//
// CONSERVATIVE READINGS pinned here (each is listed in the test-author report):
//   - step 1 fires only when the thread's attributed projects are exactly ONE
//     project AND it is a candidate; a candidate plus a non-candidate is not
//     "exactly one candidate" for routing purposes (closed set, B-D1);
//   - a grounded verdict naming a project that is no longer a candidate is not
//     a confident choice: it falls to step 4;
//   - a verdict recorded BEFORE route_after is not applied at step 3 (B-D7) and
//     does NOT fall to the default either: the message stays unmatched with no
//     row (reason verdict_before_arming). Steps 1-2 still apply.
//   - RecordedAt == ArmedAt counts as after (>=, promote's cutover spelling).
//
// GREENFIELD NOTE — EXPECTED RED: none of the above exists; this file
// compile-FAILS the capture unit build ("undefined: DecideRoute").

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDecideRoute_Table(t *testing.T) {
	const (
		collab = int64(11)
		reeng  = int64(12)
		other  = int64(13)
	)
	armed := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	after := armed.Add(time.Hour)
	before := armed.Add(-time.Hour)

	two := []RouteCandidate{{ProjectID: collab, IsDefault: true}, {ProjectID: reeng}}
	twoNoDefault := []RouteCandidate{{ProjectID: collab}, {ProjectID: reeng}}
	one := []RouteCandidate{{ProjectID: collab}}

	facts := func(thread ...int64) RouteFacts { return RouteFacts{ThreadProjects: thread, ArmedAt: armed} }
	v := func(project int64, grounded bool, at time.Time) *RouteVerdict {
		return &RouteVerdict{ExtractionID: 55, ProjectID: project, Grounded: grounded, RecordedAt: at}
	}

	cases := []struct {
		name    string
		facts   RouteFacts
		cands   []RouteCandidate
		verdict *RouteVerdict
		step    string
		project int64
		ext     int64
		reason  string
	}{
		// ---- step 1: thread ----------------------------------------------------
		{"one-project thread", facts(reeng), two, nil, "thread", reeng, 0, ""},
		{"a one-project thread outranks a grounded verdict for the other candidate",
			facts(reeng), two, v(collab, true, after), "thread", reeng, 0, ""},
		{"a thread repeating one project counts it once", facts(reeng, reeng), two, nil, "thread", reeng, 0, ""},
		{"two-project thread, no verdict: pending, never the default",
			facts(collab, reeng), two, nil, "", 0, 0, "pending_verdict"},
		{"two-project thread, grounded verdict: model", facts(collab, reeng), two, v(reeng, true, after), "model", reeng, 55, ""},
		{"a thread on a non-candidate project is not a thread step (closed set)",
			facts(other), two, nil, "", 0, 0, "pending_verdict"},
		{"a thread mixing a candidate and a non-candidate is not a thread step",
			facts(reeng, other), two, nil, "", 0, 0, "pending_verdict"},

		// ---- step 2: single ----------------------------------------------------
		{"one candidate: single, no verdict needed", facts(), one, nil, "single", collab, 0, ""},
		{"one candidate outranks the model", facts(), one, v(collab, false, after), "single", collab, 0, ""},

		// ---- step 3: model -----------------------------------------------------
		{"grounded model", facts(), two, v(reeng, true, after), "model", reeng, 55, ""},
		{"grounded model choosing the default project is still a model row", facts(), two, v(collab, true, after), "model", collab, 55, ""},
		{"a verdict recorded AT arming counts (>=)", facts(), two, v(reeng, true, armed), "model", reeng, 55, ""},

		// ---- step 4: default -----------------------------------------------------
		{"ungrounded with a default → the default (O3)", facts(), two, v(reeng, false, after), "default", collab, 0, ""},
		{"no choice (null index) with a default → the default", facts(), two, v(0, false, after), "default", collab, 0, ""},
		{"a grounded verdict naming a project that is no longer a candidate → the default",
			facts(), two, v(other, true, after), "default", collab, 0, ""},
		{"ungrounded without a default → stays unmatched", facts(), twoNoDefault, v(reeng, false, after), "", 0, 0, "no_default"},

		// ---- no verdict ------------------------------------------------------------
		{"no verdict: pending_verdict, never the default", facts(), two, nil, "", 0, 0, "pending_verdict"},
		{"no verdict, no default: pending_verdict", facts(), twoNoDefault, nil, "", 0, 0, "pending_verdict"},

		// ---- B-D7: forward-only on the verdict clock for step 3 -------------------
		{"a grounded verdict recorded before arming is not applied, and not defaulted",
			facts(), two, v(reeng, true, before), "", 0, 0, "verdict_before_arming"},
		{"an ungrounded verdict recorded before arming is not defaulted either",
			facts(), two, v(reeng, false, before), "", 0, 0, "verdict_before_arming"},
		{"a verdict before arming still lets step 1 apply", facts(reeng), two, v(collab, true, before), "thread", reeng, 0, ""},
		{"a verdict before arming still lets step 2 apply", facts(), one, v(collab, true, before), "single", collab, 0, ""},
	}
	for _, c := range cases {
		got := DecideRoute(c.facts, c.cands, c.verdict)
		if got.Step != c.step || got.ProjectID != c.project || got.ExtractionID != c.ext {
			t.Errorf("%s: DecideRoute = %+v, want step %q project %d extraction %d", c.name, got, c.step, c.project, c.ext)
			continue
		}
		if c.reason != "" && got.Reason != c.reason {
			t.Errorf("%s: reason = %q, want %q", c.name, got.Reason, c.reason)
		}
	}
}

// The window is required, and refused before any I/O: a nil pool reached by a
// query panics, and that panic is the signal.
func TestRunRouteApply_RefusesAnUnboundedPassBeforeIO(t *testing.T) {
	_, err := RunRouteApply(context.Background(), nil, RouteApplyConfig{})
	if err == nil {
		t.Fatalf("RunRouteApply accepted Since = 0; the pass window steps 1-2 apply inside is required (B-D7)")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "since") {
		t.Errorf("the refusal does not name the missing window: %v", err)
	}
}
