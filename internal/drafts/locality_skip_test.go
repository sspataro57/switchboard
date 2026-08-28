package drafts_test

// SWT-21 acceptance criterion 24: the draft worker routes through the same
// boundary, with the same fold, and logs its refusals onto the Deliver task
// instead of erroring. ZERO network, ZERO model, ZERO Postgres.
//
// Identifiers are prefixed `dl` so this file coexists with worker_test.go's
// fakes in the same package.
//
// HONESTY LABEL, the SPEC's own (criterion 24): the personal-project half of
// this is ARMED BUT INERT — `personal` is attribution-only, so it has no tasks
// and no Deliver tasks. What is NOT inert, and what these tests are actually
// about, is (a) criterion 10's fixture consequence, which fires on ordinary
// client projects the moment migration 0016 lands, and (b) the thread fold,
// which fires whenever a client thread contains a message the capture engine has
// not placed.
//
// IMPOSED NAMES (the SPEC fixes the behaviour, not the spelling): DeliverTask
// gains `ProjectLocalOnly bool` and `NeighbourAttribution []NeighbourClass`,
// mirroring internal/triage's already-landed spelling — criterion 16 requires
// ONE spelling of this rule in both workers so the two cannot drift. If you
// spell them differently, these assertions are the contract; rename freely.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/drafts"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- stubs -------------------------------------------------------------------

type dlClient struct {
	desc  provider.Descriptor
	calls int
}

func (c *dlClient) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	c.calls++
	raw, _ := json.Marshal(map[string]string{"subject": "Re: work", "body": "Done. Salvador"})
	return provider.Response{Raw: raw, Model: "stub"}, nil
}

func (c *dlClient) Describe() provider.Descriptor { return c.desc }

func dlHosted() *dlClient {
	return &dlClient{desc: provider.Descriptor{Name: "openai", Endpoint: "https://api.openai.com/v1"}}
}

type dlStore struct {
	tasks []drafts.DeliverTask
	runs  []drafts.AIRun
	next  int64
}

func (s *dlStore) DeliverTasks(_ context.Context, _ drafts.Config) ([]drafts.DeliverTask, error) {
	return s.tasks, nil
}

func (s *dlStore) RecordRun(_ context.Context, run drafts.AIRun) (int64, error) {
	s.next++
	s.runs = append(s.runs, run)
	return s.next, nil
}

type dlExec struct{ calls []executor.Call }

func (e *dlExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	e.calls = append(e.calls, call)
	return executor.Result{}, nil
}

func (e *dlExec) tools() []string {
	var out []string
	for _, c := range e.calls {
		out = append(out, c.Tool)
	}
	return out
}

func dlTask() drafts.DeliverTask {
	tid := int64(77)
	return drafts.DeliverTask{
		DeliverTaskID: 2,
		ParentTaskID:  1,
		ProjectSlug:   "acme",
		Channel:       "gmail",
		ThreadID:      &tid,
		ParentTitle:   "Fix the importer",
		ParentSummary: "shipped",
		ClientName:    "Acme",
	}
}

// ---- criteria 10 + 24: a local-only project is skipped, not drafted ----------

// Both halves in one test, and the control comes first: a project with
// ai_locality='any' still drafts through the general lane. Without it, an
// implementation that skipped every Deliver task would pass the interesting half
// — the fixture would not discriminate anything.
func TestRun_LocalOnlyProject_SkipsInsteadOfDrafting(t *testing.T) {
	t.Run("control: ai_locality=any still drafts", func(t *testing.T) {
		general := dlHosted()
		task := dlTask()
		task.ProjectLocalOnly = false
		store := &dlStore{tasks: []drafts.DeliverTask{task}}
		exec := &dlExec{}

		stats, err := drafts.Run(context.Background(), store,
			provider.NewRouter(general, nil, 0), exec, drafts.Config{Model: "gpt-5-mini", MaxTokens: 512})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if general.calls != 1 || stats.Drafted != 1 {
			t.Fatalf("control failed: general calls=%d stats=%+v. If a project marked 'any' cannot reach the "+
				"hosted lane, the restricted case below proves nothing", general.calls, stats)
		}
	})

	t.Run("ai_locality=local_only with no local model", func(t *testing.T) {
		general := dlHosted()
		task := dlTask()
		task.ProjectLocalOnly = true
		store := &dlStore{tasks: []drafts.DeliverTask{task}}
		exec := &dlExec{}

		stats, err := drafts.Run(context.Background(), store,
			provider.NewRouter(general, nil, 0), exec, drafts.Config{Model: "gpt-5-mini", MaxTokens: 512})
		if err != nil {
			t.Fatalf("Run returned an error for a locality skip: %v. Criterion 24: same skip semantics as "+
				"criterion 17 — logged, not errored", err)
		}
		if general.calls != 0 {
			t.Errorf("the hosted client recorded %d call(s) for a local_only project", general.calls)
		}
		if stats.Drafted != 0 {
			t.Errorf("stats.Drafted = %d, want 0", stats.Drafted)
		}
		if stats.Skipped != 1 {
			t.Errorf("stats.Skipped = %d, want 1 — the refusal is a skip, and drafts already counts those "+
				"separately from errors", stats.Skipped)
		}
		if stats.Errors != 0 {
			t.Errorf("stats.Errors = %d, want 0", stats.Errors)
		}

		// Invariant 3: the only write is the existing task_append_log path.
		for _, tool := range exec.tools() {
			if tool == "draft_delivery" {
				t.Errorf("a delivery was drafted for a project whose content may not be sent to a hosted model")
			}
		}
		var logged string
		for _, c := range exec.calls {
			if c.Tool != "task_append_log" {
				continue
			}
			logged = string(c.Args)
		}
		if logged == "" {
			t.Fatalf("no task_append_log call; criterion 24 logs the skip onto the Deliver task through the "+
				"existing draft_skip path. Calls made: %v", exec.tools())
		}
		if !strings.Contains(logged, "draft_skip") {
			t.Errorf("the skip log does not use kind=draft_skip: %s", logged)
		}
		// Criterion 10 depends on this sentence existing: a fixture that forgets
		// ai_locality produces a SKIP, not a failure message about locality, so the
		// log line is the ONLY thing that tells its author which of the two they
		// are looking at.
		if !strings.Contains(logged, "no_local_provider") {
			t.Errorf("the skip log does not name the reason (%q expected somewhere in %s). Criterion 15/24: "+
				"'a fixture that forgets it produces a skip, not a failure message about locality' — the "+
				"reason string is what makes that debuggable", "no_local_provider", logged)
		}
	})
}

// Criterion 24's other half, and the one the SPEC calls load-bearing HERE rather
// than in triage: the class folds over the project AND every thread message in
// the prompt. A project cleared for the hosted lane can still have a thread
// neighbour nobody has placed, and that neighbour's body goes in the same
// request.
func TestRun_FoldOverThreadContext_RestrictedNeighbourBlocksAGeneralProject(t *testing.T) {
	general := dlHosted()
	task := dlTask()
	task.ProjectLocalOnly = false // the project itself is cleared
	task.Thread = []drafts.ThreadMessage{{
		Direction: "inbound", Sender: "alerts@bankofamerica.example.test",
		Subject: "Statement", BodyText: "account ending 1234",
	}}
	task.NeighbourAttribution = []drafts.NeighbourClass{{State: provider.AttrUnseen}}

	store := &dlStore{tasks: []drafts.DeliverTask{task}}
	exec := &dlExec{}

	stats, err := drafts.Run(context.Background(), store,
		provider.NewRouter(general, nil, 0), exec, drafts.Config{Model: "gpt-5-mini", MaxTokens: 512})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if general.calls != 0 {
		t.Errorf("the hosted client recorded %d call(s): the project is cleared but a thread neighbour is "+
			"unseen, and renderUser puts that neighbour's BODY in the prompt. The class of a request is the "+
			"class of its most restricted part", general.calls)
	}
	if stats.Drafted != 0 {
		t.Errorf("stats.Drafted = %d, want 0", stats.Drafted)
	}
}
