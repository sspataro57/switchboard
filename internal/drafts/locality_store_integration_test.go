//go:build integration

package drafts_test

// SWT-21 criterion 24, against a real database — the half internal/drafts
// shipped WITHOUT in the first cut of this ticket.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DraftsLocality ./internal/drafts/
//
// WHY THIS FILE EXISTS, stated plainly because it is the whole lesson.
// internal/drafts/locality_skip_test.go asserts that a Deliver task on a
// local_only project is skipped instead of drafted. It passed from the day it
// was written — while the guard was completely inert in production. It sets
// `DeliverTask.ProjectLocalOnly` on its own fixture, and NOTHING read
// `projects.ai_locality` into that field: `DeliverTasks` never selected the
// column. Every real Deliver task arrived with the field false, folded to
// ClassGeneral, and went to the hosted lane — including tasks on the one project
// the boundary exists to protect.
//
// That is this repo's recurring landmine wearing yet another costume: a
// predicate whose discriminating column is a constant in production. An inert
// time floor, a constant `communications.channel`, an indistinguishable stats
// payload, a sibling room column, a constant model confidence — and now a field
// only test fixtures ever wrote. The unit test cannot catch it BY CONSTRUCTION,
// because the unit test is the thing supplying the value. Only a test that makes
// Postgres produce the value can.
//
// Cross-suite discipline: reuses store_integration_test.go's dsFixture, so it
// owns the same itest-dstore-% slugs and inherits its cleanup pact.

import (
	"context"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/provider"
)

// The control comes first and it is not a formality: if a project marked 'any'
// does not come back with ProjectLocalOnly false, the local_only half below
// proves nothing.
func TestDraftsLocality_Integration_ColumnReachesTheStruct(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	f.thread(t, ctx, dsGmailThread, 30)
	generalID, _, _ := f.project(t, ctx, projectSpec{
		name: "loc-any", client: "Any Client", withSendFrom: true,
		refSystem: "gmail", refKey: dsGmailThread,
	})
	restrictedID, _, _ := f.project(t, ctx, projectSpec{
		name: "loc-restricted", client: "Restricted Client", withSendFrom: true,
		localOnly: true,
	})

	if dt := deliverTask(t, ctx, f.pool, generalID); dt.ProjectLocalOnly {
		t.Errorf("a project with ai_locality='any' produced ProjectLocalOnly=true; the control must be false "+
			"or the assertion below cannot discriminate anything (task %d)", generalID)
	}

	dt := deliverTask(t, ctx, f.pool, restrictedID)
	if !dt.ProjectLocalOnly {
		t.Fatalf("a project with ai_locality='local_only' produced ProjectLocalOnly=FALSE. DeliverTasks does " +
			"not select p.ai_locality, so the locality guard in drafts.Run is inert in production and " +
			"internal/drafts/locality_skip_test.go passes only because it sets the field itself. This is the " +
			"exact assertion that was missing when the guard shipped inert")
	}
}

// End to end through the real queue: a Deliver task on a local_only project,
// with a hosted client available and no local one, must make ZERO provider calls
// and draft NOTHING — while the SAME fixture on an 'any' project drafts normally.
//
// It runs drafts.Run rather than inspecting the struct, because the struct being
// right is only half of criterion 24: the value has to reach the fold and change
// the routing.
//
// THE THREAD MESSAGES MUST BE ATTRIBUTED, and that is not fixture noise — the
// first version of this test omitted it and PASSED with the guard fully inert.
// Every message on the thread had no capture_decisions row, so each neighbour
// was AttrUnseen, MostRestrictive folded the request to restricted regardless of
// the project, and the task skipped for a reason that had nothing to do with
// locality at all. A skip assertion satisfied by the wrong cause is the same
// class of hollow test as the one this file exists to prevent, one layer up.
// Attributing them makes each neighbour follow its PROJECT's locality instead of
// being unconditionally restricted, which is what lets the control reach the
// hosted lane and makes the contrast mean something.
//
// What this test does NOT isolate, said plainly: with the neighbours attributed
// to the same project, the project column and the neighbour column say the same
// thing, so either read alone would satisfy the local_only case. That is fine —
// it is the end-to-end leak assertion. TestDraftsLocality_Integration_
// ColumnReachesTheStruct above is what pins the project column on its own, and
// it is the one that went red when DeliverTasks stopped selecting ai_locality.
func TestDraftsLocality_Integration_LocalOnlyProjectIsNeverDraftedByTheHostedLane(t *testing.T) {
	run := func(t *testing.T, localOnly bool) (*locCountingClient, *locRecordingExec, drafts.Stats) {
		t.Helper()
		ctx := context.Background()
		f := newDSFixture(t, ctx)

		threadID := f.thread(t, ctx, dsGmailThread, 30)
		deliverID, _, projectID := f.project(t, ctx, projectSpec{
			name: "loc-e2e", client: "Client", withSendFrom: true,
			refSystem: "gmail", refKey: dsGmailThread,
			localOnly: localOnly,
		})
		f.attributeThread(t, ctx, threadID, projectID)

		// Confirm the fixture reaches the queue as a draftable task before
		// asserting about what the queue does with it — otherwise "zero calls"
		// could just mean "no task".
		if dt := deliverTask(t, ctx, f.pool, deliverID); dt.Channel != "gmail" || dt.ThreadID == nil {
			t.Fatalf("fixture does not resolve to a draftable gmail task (channel=%q thread=%v)",
				dt.Channel, dt.ThreadID)
		}

		hosted := &locCountingClient{desc: provider.Descriptor{
			Name: "itest-hosted", Endpoint: "https://api.example.test/v1"}}
		exec := &locRecordingExec{}
		stats, err := drafts.Run(ctx, drafts.NewStore(f.pool),
			provider.NewRouter(hosted, nil, time.Minute), exec,
			drafts.Config{Model: "itest-locality-model", MaxTokens: 256, Limit: 50})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return hosted, exec, stats
	}

	// The control runs FIRST. Without it, an implementation that skipped every
	// Deliver task would satisfy the interesting half.
	t.Run("control: ai_locality=any drafts through the hosted lane", func(t *testing.T) {
		hosted, exec, stats := run(t, false)
		if hosted.calls != 1 {
			t.Fatalf("hosted calls = %d, want 1. If an 'any' project cannot reach the hosted lane in this "+
				"fixture, the local_only case below proves nothing — it would skip for some other reason "+
				"(an unattributed neighbour, an unresolvable channel) and still look like the guard working",
				hosted.calls)
		}
		if stats.Drafted != 1 {
			t.Errorf("stats = %+v, want 1 drafted", stats)
		}
		var drafted bool
		for _, c := range exec.calls {
			if c.Tool == "draft_delivery" {
				drafted = true
			}
		}
		if !drafted {
			t.Errorf("no draft_delivery call in the control; calls = %v", exec.tools())
		}
	})

	t.Run("ai_locality=local_only skips", func(t *testing.T) {
		hosted, exec, stats := run(t, true)
		if hosted.calls != 0 {
			t.Errorf("the hosted client was called %d time(s) for a local_only project. This is the leak: full "+
				"client-facing thread bodies for a project marked local-only, sent to a hosted API, with no "+
				"error anywhere", hosted.calls)
		}
		if stats.Skipped != 1 || stats.Drafted != 0 {
			t.Errorf("stats = %+v, want 1 skipped and 0 drafted", stats)
		}
		for _, c := range exec.calls {
			if c.Tool == "draft_delivery" {
				t.Errorf("a delivery was drafted for a local_only project")
			}
		}
	})
}

// The NEIGHBOUR half of the fold, which the two cases above cannot reach: an
// ordinary `any` project whose thread contains a message attributed to a
// local_only project.
//
// This is the case the fold exists for. `loadThreadContext` pulls thread messages
// by thread alone, with no reference to their own attribution, and their bodies
// go into the prompt — so without the fold a restricted sibling's content rides
// along to the hosted lane inside a perfectly ordinary client draft.
//
// It earns its place: neutering ONLY the neighbour locality read in
// loadThreadContext leaves every other test in this package green, because the
// project column alone satisfies them.
func TestDraftsLocality_Integration_ARestrictedNeighbourRestrictsTheWholeRequest(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	threadID := f.thread(t, ctx, dsGmailThread, 30)
	deliverID, _, projectID := f.project(t, ctx, projectSpec{
		name: "loc-neighbour", client: "Any Client", withSendFrom: true,
		refSystem: "gmail", refKey: dsGmailThread,
	})
	// The Deliver task's own project is 'any'.
	f.attributeThread(t, ctx, threadID, projectID)

	// Now add ONE more message on the same thread, attributed to a local_only
	// project. Nothing about the Deliver task changes; only a sibling's body.
	restrictedProject := f.ins(t, ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, ai_locality)
		 VALUES ('itest-dstore neighbour-restricted', $1, 'Personal', 'manual', 'dashboard', 'local_only')
		 RETURNING id`, dsSlug("loc-neighbour-restricted"))
	raw := f.ins(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'itest-dstore-neighbour','{}','itest-dstore-h-neighbour', now()) RETURNING id`, f.account)
	sibling := f.ins(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound','itest-dstore-neighbour-msg', now() - interval '20 minutes',
		         'account ending 1234 statement','statement','alerts@bank.example','gmail') RETURNING id`,
		raw, threadID)
	f.ins(t, ctx,
		`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		 VALUES ($1,'live','attributed',$2,'itest-dstore: restricted sibling') RETURNING id`,
		sibling, restrictedProject)

	dt := deliverTask(t, ctx, f.pool, deliverID)
	if dt.ProjectLocalOnly {
		t.Fatalf("the Deliver task's OWN project is 'any'; ProjectLocalOnly must be false, or this test is "+
			"just the project-column case again (task %d)", deliverID)
	}

	hosted := &locCountingClient{desc: provider.Descriptor{
		Name: "itest-hosted", Endpoint: "https://api.example.test/v1"}}
	exec := &locRecordingExec{}
	stats, err := drafts.Run(ctx, drafts.NewStore(f.pool),
		provider.NewRouter(hosted, nil, time.Minute), exec,
		drafts.Config{Model: "itest-locality-model", MaxTokens: 256, Limit: 50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hosted.calls != 0 {
		t.Errorf("the hosted client was called %d time(s). The thread carries a message attributed to a "+
			"local_only project, and loadThreadContext puts its body in the prompt — so this call would "+
			"have sent restricted content to a hosted API inside an ordinary client draft", hosted.calls)
	}
	if stats.Skipped != 1 || stats.Drafted != 0 {
		t.Errorf("stats = %+v, want 1 skipped and 0 drafted", stats)
	}
}

// An UNSEEN neighbour restricts too, and this is the operationally surprising
// one: a Deliver task on an ordinary `any` project, whose thread simply contains
// a message the capture pass has not looked at yet.
//
// Unseen is restricted by design — the alternative is shipping a personal
// sibling's body to a hosted API because nobody had classified it yet. The
// practical consequence is that drafts stalls on any thread with fresh,
// uncaptured mail on it, which is documented in
// docs/runbooks/provider-locality.md so an operator does not read it as a bug.
func TestDraftsLocality_Integration_AnUnseenNeighbourAlsoRestricts(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	threadID := f.thread(t, ctx, dsGmailThread, 30)
	deliverID, _, projectID := f.project(t, ctx, projectSpec{
		name: "loc-unseen", client: "Any Client", withSendFrom: true,
		refSystem: "gmail", refKey: dsGmailThread,
	})
	f.attributeThread(t, ctx, threadID, projectID)

	// One more message on the thread with NO capture_decisions row at all — the
	// shape of mail that arrived since the last capture pass.
	raw := f.ins(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'itest-dstore-unseen','{}','itest-dstore-h-unseen', now()) RETURNING id`, f.account)
	f.ins(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'inbound','itest-dstore-unseen-msg', now() - interval '10 minutes',
		         'just arrived','re: itest-dstore','someone@itest-dstore.example','gmail') RETURNING id`,
		raw, threadID)

	// The task is otherwise perfectly draftable — its own project is 'any' and
	// its channel resolves. Only the sibling's missing decision row differs.
	if dt := deliverTask(t, ctx, f.pool, deliverID); dt.ProjectLocalOnly || dt.Channel != "gmail" {
		t.Fatalf("fixture is not the intended shape (localOnly=%v channel=%q)", dt.ProjectLocalOnly, dt.Channel)
	}

	hosted := &locCountingClient{desc: provider.Descriptor{
		Name: "itest-hosted", Endpoint: "https://api.example.test/v1"}}
	exec := &locRecordingExec{}
	stats, err := drafts.Run(ctx, drafts.NewStore(f.pool),
		provider.NewRouter(hosted, nil, time.Minute), exec,
		drafts.Config{Model: "itest-locality-model", MaxTokens: 256, Limit: 50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hosted.calls != 0 {
		t.Errorf("the hosted client was called %d time(s) for a task whose thread carries an UNSEEN message. "+
			"Unseen is restricted (ClassOf), and collapsing it to 'general' here would mean any message the "+
			"capture pass has not reached yet travels to a hosted API", hosted.calls)
	}
	if stats.Skipped != 1 || stats.Drafted != 0 {
		t.Errorf("stats = %+v, want 1 skipped and 0 drafted", stats)
	}
}

// An OUTBOUND neighbour must NOT restrict — the case that made the first
// implementation of this fold permanently wrong.
//
// The capture engine filters `direction = 'inbound'`; that line is invariant 5,
// so an outbound message will NEVER carry a capture_decisions row, on any pass,
// in any mode. Measured against production when this was found: 21,194 outbound
// messages, zero decisions, 100% of them.
//
// Folding them therefore did not mean "not classified yet", it meant "restricted
// forever". And because a Deliver task exists precisely to REPLY on a thread,
// the first send re-entering through ingestion (invariant 5 again) would have
// blocked every later Deliver task on that thread permanently — no operator
// remedy, no error, just a task that never drafts again.
//
// The fixture is the shape that broke it: an ordinary `any` project, inbound
// messages attributed, and one outbound reply of our own with no decision row.
func TestDraftsLocality_Integration_AnOutboundNeighbourDoesNotRestrict(t *testing.T) {
	ctx := context.Background()
	f := newDSFixture(t, ctx)

	threadID := f.thread(t, ctx, dsGmailThread, 30)
	deliverID, _, projectID := f.project(t, ctx, projectSpec{
		name: "loc-outbound", client: "Any Client", withSendFrom: true,
		refSystem: "gmail", refKey: dsGmailThread,
	})
	f.attributeThread(t, ctx, threadID, projectID)

	// Our own send, re-entered through ingestion. NO capture_decisions row, and
	// there never will be one.
	raw := f.ins(t, ctx,
		`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		 VALUES ($1,'itest-dstore-outbound','{}','itest-dstore-h-outbound', now()) RETURNING id`, f.account)
	sent := f.ins(t, ctx,
		`INSERT INTO normalized_messages
		   (raw_source_item_id, thread_id, direction, external_message_id, sent_at, body_text, subject, sender, channel)
		 VALUES ($1,$2,'outbound','itest-dstore-outbound-msg', now() - interval '15 minutes',
		         'we are on it','re: itest-dstore','me@sb.example','gmail') RETURNING id`,
		raw, threadID)

	// Guard the premise rather than assuming it: if something ever DID decide an
	// outbound message, this test would silently stop testing what it claims to.
	var decisions int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM capture_decisions WHERE message_id = $1`, sent).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("the outbound message has %d capture decision(s); this fixture is meant to represent the "+
			"structurally-undecidable case (invariant 5), and with a decision present it proves nothing",
			decisions)
	}

	hosted := &locCountingClient{desc: provider.Descriptor{
		Name: "itest-hosted", Endpoint: "https://api.example.test/v1"}}
	exec := &locRecordingExec{}
	stats, err := drafts.Run(ctx, drafts.NewStore(f.pool),
		provider.NewRouter(hosted, nil, time.Minute), exec,
		drafts.Config{Model: "itest-locality-model", MaxTokens: 256, Limit: 50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hosted.calls != 1 || stats.Drafted != 1 {
		t.Errorf("hosted calls = %d, stats = %+v; want the task DRAFTED. An outbound message on the thread "+
			"must not restrict the request: it can never be given a capture decision (invariant 5), so it "+
			"inherits its conversation's class instead of defaulting to unseen. Restricting on it blocks "+
			"every thread the system has ever replied on, permanently", hosted.calls, stats)
	}
	// The fixture's thread holds exactly two messages: one inbound and our
	// outbound reply. Both must still be in the PROMPT — the outbound one is
	// excluded from the class fold, not from the conversation. A draft that
	// cannot see what we last said is a different bug.
	if dt := deliverTask(t, ctx, f.pool, deliverID); len(dt.Thread) != 2 {
		t.Errorf("the prompt context carries %d thread messages, want 2 (inbound + our reply)", len(dt.Thread))
	}
}

// ---- stubs -------------------------------------------------------------------

type locCountingClient struct {
	desc  provider.Descriptor
	calls int
}

func (c *locCountingClient) Describe() provider.Descriptor { return c.desc }

func (c *locCountingClient) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	c.calls++
	// Deliberately returns a VALID draft. A stub that errored would make the
	// "nothing was drafted" assertion pass even if the boundary had let the call
	// through.
	return provider.Response{
		Raw:   []byte(`{"subject":"itest","body":"itest"}`),
		Model: "itest",
	}, nil
}

type locRecordingExec struct{ calls []executor.Call }

func (e *locRecordingExec) Execute(_ context.Context, c executor.Call) (executor.Result, error) {
	e.calls = append(e.calls, c)
	return executor.Result{}, nil
}

func (e *locRecordingExec) tools() []string {
	out := make([]string, 0, len(e.calls))
	for _, c := range e.calls {
		out = append(out, c.Tool)
	}
	return out
}
