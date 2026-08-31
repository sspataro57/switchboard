//go:build integration

package classify_test

// SWT-23 criteria 11, 13 and 17, against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ClassifyResidue ./internal/classify/
//
// WHY THESE THREE ARE HERE AND NOT IN lane_test.go. Every one of them turns on a
// value Postgres produces: `latest.action`, the `NOT EXISTS` keyed on
// `worker_type`, and the absence of an action predicate in the residue loader. A
// fake store would supply the very values the filters are supposed to compute —
// SWT-21's 6th landmine, which shipped twice in one ticket and whose rule is now
// standing: for any predicate whose input comes from a column, the regression
// test belongs in the integration suite, and it must fail when the column is
// dropped. The mutations that must turn each of these red are named inline.
//
// It reuses the ciSuite corpus (newCISuite / ciSeed / ciCleanup in
// store_integration_test.go), which already seeds all four decision states plus
// the superseded and outbound shapes. A second corpus would be a second reading
// of the same table.
//
// IMPOSED SURFACE beyond lane_test.go's, because the eval harness has to know
// which lane it is scoring:
//
//	Eval(ctx, store, router, cfg Config, labels []Label, w io.Writer) error
//	Store.MessagesByID(ctx context.Context, cfg Config, ids []int64) ([]PendingMessage, error)
//
// The lane threads through Config exactly as Model and Limit do (the SPEC's
// "Sibling patterns to copy"), so `run` and `eval` cannot disagree about which
// prompt, which worker_type or which loader they are using.
//
// GREENFIELD NOTE: LaneResidue, Config.Lane and the residue filter do not exist,
// and migration 0018 has not been applied to the compose db, so this file
// compile-FAILS and then fails at seed time on the missing ai_classify column.
// Expected red.

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ciResidueCfg is the residue lane's run config. `Since` is REQUIRED on this
// lane (criterion 16) and 24h comfortably covers the fixture, whose messages are
// minutes old.
func ciResidueCfg() classify.Config {
	return classify.Config{Model: ciModel, MaxTokens: 512, Lane: classify.LaneResidue, Since: 24 * time.Hour}
}

// ---- criterion 13: the four decision states, produced by Postgres ------------

// The three-state discipline restated for this lane, plus the two states that
// name a project. Conflating the first two is the trap capture-rules.md:8-18
// names: treating UNSEEN as unmatched hands every fresh message to the model
// before the rules have run, which inverts the whole capture-before-triage
// ordering — and would cost 7.2 s of GPU per message to do it.
func TestClassifyResidue_Integration_InboxIsTheUnmatchedPile(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)
	got := ciPending(t, ctx, c, classify.LaneResidue)

	// State 2 — latest decision 'unmatched'. OURS, and the whole population.
	m, ok := got[c.unmatchedID]
	if !ok {
		t.Fatalf("message %d (latest decision 'unmatched') is NOT in the residue inbox. That pile is "+
			"14,741 messages nothing has ever read — triage's inbox in name, with a prompt that asks "+
			"client-work questions of a Nextdoor digest. If it is not here, this lane classifies nothing",
			c.unmatchedID)
	}

	// LATEST means latest, in the other direction from the personal lane: a
	// message attributed and then re-evaluated as unmatched comes BACK to the
	// residue. That is the ordinary way a rule set is tuned.
	if _, ok := got[c.supersededID]; !ok {
		t.Errorf("message %d was attributed and then re-evaluated as 'unmatched', and is NOT in the residue "+
			"inbox. The filter must read the LATEST decision (ORDER BY cd.id DESC LIMIT 1) — a narrowed "+
			"rule puts its messages back in the residue, and a filter reading any-unmatched-decision-ever "+
			"would instead hold every message that was ever unmatched", c.supersededID)
	}

	// State 3 — latest 'attributed'. Never ours, on either project shape. THE
	// MUTATION: drop `AND latest.action = 'unmatched'` and these three appear.
	// If they do not, the test is proving its own fixture.
	for _, tc := range []struct {
		id   int64
		what string
	}{
		{c.localAttrID, "attributed to a local_only + ai_classify project — the PERSONAL lane's inbox"},
		{c.anyAttrID, "attributed to an ai_locality='any' project — routed client work"},
		{c.noClassifyAttrID, "attributed to a local_only + ai_classify=false project — claimed as bulk"},
	} {
		if _, ok := got[tc.id]; ok {
			t.Errorf("message %d (%s) was returned in the RESIDUE inbox. Only `latest.action = 'unmatched'` "+
				"excludes it: the project join cannot, because this filter LEFT JOINs projects — an "+
				"unmatched row has no project to join to (0015:126-127, "+
				"capture_decisions_unmatched_has_no_project). If you are reading this after dropping that "+
				"clause, good — that is the mutation criterion 13 names", tc.id, tc.what)
		}
	}

	// State 4 — 'task' and 'task_log'. Never ours: they already produced a task.
	for _, id := range []int64{c.taskAttrID, c.taskLogID} {
		if _, ok := got[id]; ok {
			t.Errorf("message %d has a latest decision of 'task'/'task_log' and was returned in the residue "+
				"inbox. capture_decisions.action has exactly four values (0015:112-113) and there is no "+
				"'ignore' verb — so a filter spelled `action <> 'attributed'` catches these two by "+
				"accident. Spell it as the positive: action = 'unmatched'", id)
		}
	}

	// State 1 — unseen. The engine has not looked.
	if _, ok := got[c.unseenID]; ok {
		t.Errorf("message %d has NO capture_decisions row and was returned as pending. UNSEEN is not "+
			"UNMATCHED (capture-rules.md:8-18): the rules have not run on it yet, and handing it to the "+
			"model first inverts the capture-before-triage ordering that exists so the model never spends "+
			"a call on a message a rule would have routed", c.unseenID)
	}

	// Outbound. Stated as honestly as its sibling in store_integration_test.go:
	// this assertion cannot discriminate the `direction='inbound'` clause and is
	// not pretending to. The capture engine reads direction='inbound'
	// (rules_store.go — that line IS invariant 5), so an outbound message can
	// never carry an 'unmatched' decision either, and the decision join already
	// excludes it. The clause is written anyway so a reader need not know that to
	// trust the query, and it must NEVER be described as the thing that keeps our
	// own sends out.
	if _, ok := got[c.outboundID]; ok {
		t.Errorf("message %d is outbound and was returned in the residue inbox", c.outboundID)
	}

	// ---- the row itself: NULL-safe, because there is no project -------------
	//
	// inboxSelect stays ONE constant (SWT-25 criterion 15's property: `run` and
	// `eval` cannot read different columns), so the projections have to be
	// COALESCE'd rather than duplicated into a second SELECT list.
	if m.ProjectID != 0 {
		t.Errorf("residue row ProjectID = %d, want 0. An unmatched decision has no project_id at all "+
			"(0015's CHECK), so the projection is COALESCE(p.id,0) over a LEFT JOIN — a plain p.id would "+
			"either fail to scan NULL or, with an inner join, return no rows and make this lane silently "+
			"empty", m.ProjectID)
	}
	if m.ProjectSlug != "" {
		t.Errorf("residue row ProjectSlug = %q, want empty (COALESCE(p.slug,''))", m.ProjectSlug)
	}
	if m.ProjectLocalOnly {
		t.Errorf("residue row ProjectLocalOnly = true with no project. The projection is " +
			"COALESCE(p.ai_locality = 'local_only', false) and the value must be FALSE here — which is " +
			"exactly why containment cannot rest on it: ClassOf restricts this message through the " +
			"`state != AttrProject` branch instead (provider/locality.go:195-198)")
	}
	if m.Attribution != provider.AttrUnmatched {
		t.Errorf("residue row Attribution = %v, want AttrUnmatched (criterion 14). scanMessages sets "+
			"AttrProject for the personal lane because that filter requires an 'attributed' decision; this "+
			"one requires the opposite, and the value is what makes classOf restrict the message and "+
			"classReasonOf file its skips under 'unmatched' rather than 'project_local_only'", m.Attribution)
	}
	if m.BodyText == "" || m.Subject == "" || m.Sender == "" {
		t.Errorf("residue row is missing prompt inputs: sender=%q subject=%q body=%q",
			m.Sender, m.Subject, m.BodyText)
	}
	if m.RawSourceItemID != c.unmatchedRaw {
		t.Errorf("residue row raw_source_item_id = %d, want %d — raw-first linkage is what the extraction "+
			"is keyed on, and what the NOT EXISTS reads back", m.RawSourceItemID, c.unmatchedRaw)
	}
}

// ---- criterion 11: worker_type is what keeps the two lanes off each other ----

// The defect a shared worker_type causes, seeded as the exact sequence that
// produces it: classify a residue message today, let a capture rule claim it
// into a local_only + ai_classify project tomorrow, and ask whether the personal
// lane can still see it. With one shared value the answer is NO, permanently —
// the message is already "classified", by a different prompt, with a verdict
// nobody would ever look for.
func TestClassifyResidue_Integration_DoesNotHideAMessageFromThePersonalLane(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)
	st := classify.NewStore(c.pool)

	// (0) The reverse direction FIRST, while the superseded message has no
	// classify_residue extraction of its own: a PERSONAL extraction must not
	// retire a message from the residue lane. This probe has to run before the
	// residue pass below, because that pass legitimately classifies every
	// latest-unmatched message — supersededID included — and its own lane's
	// extraction would then mask what this assertion isolates.
	var runID int64
	if err := c.pool.QueryRow(ctx,
		`INSERT INTO ai_runs (worker_type, provider, model, status) VALUES ('classify','fake',$1,'ok') RETURNING id`,
		ciModel).Scan(&runID); err != nil {
		t.Fatalf("insert classify run: %v", err)
	}
	var supersededRaw int64
	if err := c.pool.QueryRow(ctx,
		`SELECT raw_source_item_id FROM normalized_messages WHERE id=$1`, c.supersededID).Scan(&supersededRaw); err != nil {
		t.Fatalf("read raw item of the superseded message: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO ai_extractions (ai_run_id, raw_source_item_id, fields) VALUES ($1,$2,'{}')`,
		runID, supersededRaw); err != nil {
		t.Fatalf("insert classify extraction: %v", err)
	}
	if _, ok := ciPending(t, ctx, c, classify.LaneResidue)[c.supersededID]; !ok {
		t.Errorf("message %d left the RESIDUE inbox because a row with worker_type='classify' exists for "+
			"it. The residue lane's NOT EXISTS must key on 'classify_residue': this message was classified "+
			"by the personal lane while it was attributed, and it is unmatched again now — a different "+
			"question, a different prompt, a different verdict", c.supersededID)
	}

	// (1) A residue pass classifies it. The canned verdict is not-actionable —
	// which is still a VERDICT, and the extraction it writes is what removes the
	// message from the residue inbox.
	local := cfLocal()
	if _, err := classify.Run(ctx, st, provider.NewRouter(nil, local, time.Minute), ciResidueCfg()); err != nil {
		t.Fatalf("residue Run: %v", err)
	}
	if local.calls == 0 {
		t.Fatalf("the local client was never called; the residue inbox was empty and every assertion below " +
			"would be vacuous")
	}
	if n := ciCount(t, ctx, c.pool,
		`SELECT count(*) FROM ai_extractions e JOIN ai_runs r ON r.id=e.ai_run_id
		  WHERE r.worker_type='classify_residue' AND e.raw_source_item_id=$1`, c.unmatchedRaw); n != 1 {
		t.Fatalf("the residue message has %d ai_extractions row(s) under worker_type='classify_residue', "+
			"want 1. If it is 0, check that classifyAll writes cfg.Lane.WorkerType rather than the literal "+
			"'classify'", n)
	}
	if _, ok := ciPending(t, ctx, c, classify.LaneResidue)[c.unmatchedID]; ok {
		t.Errorf("message %d is still in the residue inbox after being classified; every pass would "+
			"reclassify the whole residue at 7.2 s each", c.unmatchedID)
	}

	// (2) Tomorrow, a capture rule claims it for the personal project.
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow','attributed',$2,'itest-classify: a rule was added')`,
		c.unmatchedID, c.localProject); err != nil {
		t.Fatalf("attribute the message: %v", err)
	}

	// (3) The personal lane must now see it. This is the assertion the separate
	// worker_type exists for.
	if _, ok := ciPending(t, ctx, c, classify.LanePersonal)[c.unmatchedID]; !ok {
		t.Errorf("message %d is invisible to the PERSONAL lane after the residue lane classified it and a "+
			"capture rule then attributed it to a local_only + ai_classify project. Both lanes' NOT EXISTS "+
			"clauses key on ai_runs.worker_type, so a shared value means the residue verdict — produced by "+
			"a prompt that never asked the personal lane's questions — permanently retires the message "+
			"here. That is criterion 11's whole argument, and it is a defect, not tidiness", c.unmatchedID)
	}

}

// ---- criterion 17: the eval loader has NO action and NO project predicate ----

// The whole point of Phase 1 is to move messages OUT of the residue. A loader
// that required `unmatched` would make the labelled set silently shrink every
// time a rule is added — label drift by another mechanism, and the existing
// drift report (eval.go:95-105) exists because a score computed over a set that
// quietly lost rows is a score nobody can reproduce.
//
// Loading without the predicate is safe: Eval refuses any lane but the local one
// before it reads anything, so a message that has since become client work is
// still only ever sent to the local model.
func TestClassifyResidue_Integration_EvalScoresMessagesARuleHasSinceClaimed(t *testing.T) {
	ctx := context.Background()
	c := newCISuite(t, ctx)

	// The label is written while the message is unmatched — which is when a
	// labelled set is drawn.
	var subject string
	if err := c.pool.QueryRow(ctx, `SELECT subject FROM normalized_messages WHERE id=$1`,
		c.unmatchedID).Scan(&subject); err != nil {
		t.Fatalf("read the labelled message's subject: %v", err)
	}
	labels := []classify.Label{{
		MessageID:     c.unmatchedID,
		Label:         "not",
		SubjectSHA256: classify.SubjectHash(subject),
		Stratum:       "uniform",
	}}

	// Control FIRST: it scores while the message is still unmatched. Without it,
	// "still scored after attribution" is satisfied by an Eval that scores
	// nothing at all.
	before := &bytes.Buffer{}
	local := cfLocal()
	// NOTE: no Since. `--since` is required for a residue RUN (criterion 16,
	// 14,737 × 7.2 s); an eval is bounded by the label file itself, and making it
	// mandatory here would refuse the one command the ticket's numbers come from.
	cfg := classify.Config{Model: ciModel, MaxTokens: 512, Lane: classify.LaneResidue}
	if err := classify.Eval(ctx, classify.NewStore(c.pool),
		provider.NewRouter(nil, local, time.Minute), cfg, labels, before); err != nil {
		t.Fatalf("Eval (message still unmatched): %v", err)
	}
	if local.calls != 1 {
		t.Fatalf("the model was called %d time(s) for 1 label; the control failed, so nothing below can be "+
			"read.\n%s", local.calls, before.String())
	}
	if regexp.MustCompile(`(?i)drift|not found`).MatchString(before.String()) &&
		strings.Contains(before.String(), "excluded") {
		t.Fatalf("the labelled message was excluded as drift before a rule ever touched it:\n%s",
			before.String())
	}

	// Now a rule claims it — to a project that is NOT local_only, the hardest
	// case: the message has become client work as far as the funnel is concerned.
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'shadow','attributed',$2,'itest-classify: a rule claimed it')`,
		c.unmatchedID, c.anyProject); err != nil {
		t.Fatalf("attribute the labelled message: %v", err)
	}

	after := &bytes.Buffer{}
	local2 := cfLocal()
	general := cfHosted()
	if err := classify.Eval(ctx, classify.NewStore(c.pool),
		provider.NewRouter(general, local2, time.Minute), cfg, labels, after); err != nil {
		t.Fatalf("Eval (message since attributed): %v", err)
	}
	text := after.String()

	if local2.calls != 1 {
		t.Errorf("the model was called %d time(s) after a rule claimed the labelled message, want 1. "+
			"MessagesByID for the residue lane loads BY ID with no action and no project predicate "+
			"(criterion 17) — with the predicate, the labelled set shrinks silently every time a rule is "+
			"added, and the score changes with no visible cause.\n%s", local2.calls, text)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client was called %d time(s). The message is now attributed to an "+
			"ai_locality='any' project, which is exactly the case criterion 17's rationale covers: Eval "+
			"refuses any lane but the local one before it reads anything, so loading without the predicate "+
			"is safe. If this is non-zero, that refusal is not doing the work the rationale claims",
			general.calls)
	}
	if !regexp.MustCompile(`(?i)no longer unmatched`).MatchString(text) {
		t.Errorf("the eval output has no \"no longer unmatched\" line (criterion 17). Phase 1 exists to "+
			"move messages out of the residue, so the harness must SAY how many of the scored labels a "+
			"rule has claimed since — otherwise a score drifting because the population changed looks "+
			"exactly like a prompt getting worse.\n%s", text)
	}
	if !strings.Contains(text, "1") {
		t.Errorf("the \"no longer unmatched\" line does not carry a count:\n%s", text)
	}
}
