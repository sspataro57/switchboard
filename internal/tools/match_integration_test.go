//go:build integration

package tools_test

// task_match against a real database — comms-inbox (SWT-74,
// docs/tickets/comms-inbox_SPEC.md) D6 and criteria 23, 24, 25 and 46: capture's
// OWN decision, run read-only, over a message, a task or a pasted line.
//
// The fixture is criterion 42's in miniature, built the way production builds
// it: a capture_rules ROW (the body_regex / jira-key shape rule 75 has), an
// external_refs row written by link_external_ref through the executor, and an
// inbound message. Nothing here fabricates a proposal — every field the tool
// returns comes from capture.ExplainMessage running decideMessage.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops_comms?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run TaskMatch ./internal/tools/
//
// Reuses revive_integration_test.go's rvSuite (its cleanup pact covers
// capture_rules and external_refs for its project) and
// activity_integration_test.go's amRequire0039 / activityArgs.
//
// GREENFIELD NOTE — EXPECTED RED: task_match is not registered and
// capture.ExplainMessage does not exist, so every call fails "unknown tool".
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - task_match infers the message from source_thread_id when
//     activity_by_message_id is NULL -> ATaskWithoutActivityIsRefusedByName.
//   - a second matcher instead of decideMessage -> TheWhyIsCapturesOwnReason.
//   - source_thread ranked before rule_ref -> RuleRefLeadsAndDedupes.
//   - {text} not marked partial -> PastedTextIsPartial.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

const (
	mtKey    = "MTC-4242"
	mtWorker = "mcp:treetop" // criterion 26: a worker console may ASK; it cannot act on the answer
)

type mtProposal struct {
	TaskID         int64  `json:"task_id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	AssigneeType   string `json:"assignee_type"`
	Project        string `json:"project"`
	Source         string `json:"source"`
	RuleID         int64  `json:"rule_id"`
	RuleKind       string `json:"rule_kind"`
	ExternalSystem string `json:"external_system"`
	ExternalKey    string `json:"external_key"`
	Partial        bool   `json:"partial"`
	Why            string `json:"why"`
	Body           string `json:"body"`
}

type mtResult struct {
	Input     string       `json:"input"`
	Matched   bool         `json:"matched"`
	Reason    string       `json:"reason"`
	Proposals []mtProposal `json:"proposals"`
}

// mtRun calls task_match with NO TaskID: the verb is a read about a message or
// a line, not an act on a task (D6), so the audit row carries no task.
func mtRun(ctx context.Context, s *rvSuite, actor, args string) (mtResult, error) {
	var r mtResult
	res, err := s.ex.Execute(ctx, executor.Call{Tool: matchTool, Actor: actor, Args: json.RawMessage(args)})
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(res.Output, &r); err != nil {
		return r, fmt.Errorf("decode %s result %s: %w (D6's result shape: {input, matched, reason, proposals})",
			matchTool, res.Output, err)
	}
	return r, nil
}

func mtMatch(t *testing.T, ctx context.Context, s *rvSuite, actor, args string) mtResult {
	t.Helper()
	r, err := mtRun(ctx, s, actor, args)
	if err != nil {
		t.Fatalf("%s(%s) as %s: %v", matchTool, args, actor, err)
	}
	return r
}

// mtFixture is the José case: a jira-keyed ticket task, an armed-shaped
// body_regex rule, and the message a rule would file onto it.
type mtFixture struct {
	rule, ticket, message, thread int64
}

func newMTFixture(t *testing.T, ctx context.Context, s *rvSuite) mtFixture {
	t.Helper()
	var f mtFixture
	f.rule = s.id(t, ctx,
		`INSERT INTO capture_rules (project_id, criteria_type, pattern, external_system, key_regex, url_template,
		                            priority, enabled, note)
		 VALUES ($1,'body_regex',$2,'jira',$3,'https://itest.jira.example/browse/{key}',90,true,'itest-revive match')
		 RETURNING id`, s.project, `\bMTC-[0-9]+\b`, `\b(MTC-[0-9]+)\b`)
	f.ticket, _ = s.task(t, ctx, "match-ticket", "ready")
	// external_refs through the EXECUTOR, like capture writes it (invariant 3):
	// this row IS the rule_ref resolution the tool's rank-0 proposal comes from.
	s.call(t, ctx, "link_external_ref", rvSpine, f.ticket,
		`{"task_id":`+itoa(f.ticket)+`,"system":"jira","external_key":"`+mtKey+
			`","external_url":"https://itest.jira.example/browse/`+mtKey+`"}`)
	now := s.dbNow(t, ctx)
	var dummy int64
	dummy, f.thread = s.task(t, ctx, "match-thread-owner", "ready")
	_ = dummy
	f.message = s.message(t, ctx, "match-msg", f.thread, "inbound", now, now)
	s.exec(t, ctx, `UPDATE normalized_messages SET body_text=$2, sender=$3 WHERE id=$1`,
		f.message, "Hi Salvador, "+mtKey+" still blocks the import", "José Garcia <jose.g@avviato.example>")
	return f
}

// ---- criteria 23 and 46: {message_id} ------------------------------------------------

func TestTaskMatch_Integration_RuleRefLeadsAndDedupes(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)
	f := newMTFixture(t, ctx, s)

	// A SECOND open task on the same thread: D6's rank-1 evidence source.
	sibling := s.id(t, ctx,
		`INSERT INTO tasks (project_id, title, status, source_thread_id) VALUES ($1,'itest-revive sibling','ready',$2)
		 RETURNING id`, s.project, f.thread)

	r := mtMatch(t, ctx, s, atDash, `{"message_id":`+itoa(f.message)+`}`)
	if !r.Matched || len(r.Proposals) == 0 {
		t.Fatalf("task_match{message_id} = %+v, want a match. Criterion 46: over criterion 42's message it "+
			"returns the TICKET task first", r)
	}
	first := r.Proposals[0]
	if first.TaskID != f.ticket {
		t.Errorf("the first proposal is task %d, want the ticket task %d. D6: rule_ref ranks 0 — it is the "+
			"rule's OWN external-ref resolution, the strongest evidence there is", first.TaskID, f.ticket)
	}
	if first.Source != "rule_ref" {
		t.Errorf("the first proposal's source is %q, want \"rule_ref\" (D6's two evidence sources, in order)", first.Source)
	}
	if first.RuleID != f.rule || first.RuleKind != "body_regex" {
		t.Errorf("the first proposal names rule %d (%s), want rule %d (body_regex) — the answer has to say "+
			"WHICH rule matched, or \"swb match\" cannot be acted on", first.RuleID, first.RuleKind, f.rule)
	}
	if first.ExternalSystem != "jira" || first.ExternalKey != mtKey {
		t.Errorf("the first proposal names %s %s, want jira %s", first.ExternalSystem, first.ExternalKey, mtKey)
	}
	if first.Title == "" {
		t.Errorf("the first proposal carries no title; D6: titles only, and a proposal without one is unreadable")
	}
	if first.Body != "" {
		t.Errorf("the proposal carries a body (%q). D6: titles only, never bodies — rows in a model context "+
			"cost tokens, and task_context is the per-task read (task_list's rule)", first.Body)
	}
	if first.Status == "" || first.AssigneeType == "" || first.Project == "" {
		t.Errorf("the proposal is missing status/assignee_type/project: %+v (D6's proposal shape)", first)
	}
	if first.Partial {
		t.Errorf("a {message_id} proposal is marked partial; criterion 25: only {text} is partial — a stored " +
			"message runs the own-action guard and the PR-trust check")
	}

	// The sibling shows up as source_thread, AFTER the rule_ref proposal.
	var sawSibling bool
	for i, p := range r.Proposals {
		if p.TaskID == sibling {
			sawSibling = true
			if i == 0 {
				t.Errorf("the source_thread proposal leads; D6 ranks rule_ref 0 and source_thread 1")
			}
			if p.Source != "source_thread" {
				t.Errorf("the sibling's source is %q, want \"source_thread\"", p.Source)
			}
		}
	}
	if !sawSibling {
		t.Errorf("no source_thread proposal for the open task on the message's thread. D6: \"open tasks whose "+
			"source_thread_id is the message's thread, oldest first (threadTask's shape, NOT project-scoped: a "+
			"thread is a conversation and the human decides)\". Proposals: %+v", r.Proposals)
	}
	// Deduped by task id, with rule_ref winning when a task is both.
	seen := map[int64]int{}
	for _, p := range r.Proposals {
		seen[p.TaskID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("task %d appears %d times; criterion 23: deduped by task id, and a task that is both "+
				"keeps the rule_ref source", id, n)
		}
	}
}

// Criterion 23: the `why` is capture's OWN reason string — not a sentence this
// tool composes. That is what makes the answer checkable against
// capture_decisions after the fact.
func TestTaskMatch_Integration_TheWhyIsCapturesOwnReason(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)
	f := newMTFixture(t, ctx, s)

	r := mtMatch(t, ctx, s, atDash, `{"message_id":`+itoa(f.message)+`}`)
	if len(r.Proposals) == 0 {
		t.Fatalf("no proposals: %+v", r)
	}
	why := r.Proposals[0].Why
	for _, frag := range []string{"rule " + itoa(f.rule), "body_regex", mtKey, "task " + itoa(f.ticket)} {
		if !strings.Contains(why, frag) {
			t.Errorf("the proposal's why is %q, want decideMessage's own reason (missing %q). D6: the tool "+
				"calls ONE exported entry point that runs decideMessage ITSELF — \"it must not be a second "+
				"matcher\", and a hand-written sentence here is exactly that", why, frag)
		}
	}
	// Read-only: the question wrote no decision row and no task event.
	if n := s.n(t, ctx, `SELECT count(*) FROM capture_decisions WHERE message_id=$1`, f.message); n != 0 {
		t.Errorf("task_match wrote %d capture_decisions row(s). Criterion 20: it writes NOTHING but its audit "+
			"row — a live decision is FOREVER, and claiming the message here would stop the next real pass "+
			"from acting on it", n)
	}
	if n := s.n(t, ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, f.ticket); n != 0 {
		t.Errorf("task_match wrote %d task_events row(s), want 0", n)
	}
	// Criterion 26: a worker console may ASK. It cannot act on the answer —
	// task_attach is humanOnly.
	if _, err := mtRun(ctx, s, mtWorker, `{"message_id":`+itoa(f.message)+`}`); err != nil {
		t.Errorf("task_match as %s was refused: %v. Criterion 26 / D6: both profiles, no humanOnly — \"a worker "+
			"console asking which task this line belongs to cannot act on the answer\"", mtWorker, err)
	}
}

// ---- criterion 24: {task_id} resolves through activity_by_message_id ---------------------

func TestTaskMatch_Integration_ATaskWithoutActivityIsRefusedByName(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)
	f := newMTFixture(t, ctx, s)

	// A COMM-shaped task: marked with the message, exactly as capture marks one.
	comm, _ := s.task(t, ctx, "match-comm", "ready")
	s.call(t, ctx, "task_mark_activity", maActor, comm, activityArgs(comm, f.message))

	r := mtMatch(t, ctx, s, atDash, `{"task_id":`+itoa(comm)+`}`)
	if len(r.Proposals) == 0 || r.Proposals[0].TaskID != f.ticket {
		t.Fatalf("task_match{task_id: comm} = %+v, want the same answer as {message_id} (criterion 46). "+
			"Criterion 24: {task_id} resolves through tasks.activity_by_message_id — which THIS ticket sets on "+
			"every comm task", r)
	}

	// A task with no activity is refused BY NAME, never guessed from the thread.
	// The thread would "work" here (the task has a source_thread_id), which is
	// what makes the refusal meaningful rather than vacuous.
	quiet, quietThread := s.task(t, ctx, "match-quiet", "ready")
	if quietThread == 0 {
		t.Fatalf("CONTROL: the quiet task has no source_thread_id, so inferring from it could not have worked " +
			"anyway and the refusal below proves nothing")
	}
	_, err := mtRun(ctx, s, atDash, `{"task_id":`+itoa(quiet)+`}`)
	if err == nil {
		t.Fatalf("task_match on a task with no activity message succeeded. Criterion 24: it is refused BY NAME " +
			"(\"task N carries no activity message; pass message_id or text\"), NEVER guessed from " +
			"source_thread_id — a guess would answer a different question than the one asked")
	}
	for _, frag := range []string{itoa(quiet), "message_id", "text"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("the refusal is %q, want it to name the task and both alternatives (missing %q)", err, frag)
		}
	}
	if strings.Contains(err.Error(), "source_thread") {
		t.Errorf("the refusal mentions source_thread (%q); criterion 24: nothing is inferred from it", err)
	}
}

// ---- criterion 25: {text} is partial -----------------------------------------------------

func TestTaskMatch_Integration_PastedTextIsPartial(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)
	f := newMTFixture(t, ctx, s)

	r := mtMatch(t, ctx, s, atDash, `{"text":"José: `+mtKey+` still blocks the import"}`)
	if !r.Matched || len(r.Proposals) == 0 {
		t.Fatalf("task_match{text} = %+v, want the ticket task. This is his own ask: \"a tool to access/"+
			"exercise the matcher rules\" for a particular line", r)
	}
	if r.Proposals[0].TaskID != f.ticket {
		t.Errorf("the first proposal is task %d, want the ticket task %d", r.Proposals[0].TaskID, f.ticket)
	}
	if !r.Proposals[0].Partial {
		t.Errorf("a {text} proposal is not marked partial:true. Criterion 25 / D6: ExplainText cannot run the " +
			"own-action guard or the PR-trust check (both need a STORED message), and saying so is the " +
			"difference between an answer and a claim")
	}
	low := strings.ToLower(r.Reason + " " + r.Proposals[0].Why)
	for _, frag := range []string{"own", "pr"} {
		if !strings.Contains(low, frag) {
			t.Errorf("the {text} reason %q does not say WHICH checks did not run (missing %q); criterion 25",
				r.Reason+" / "+r.Proposals[0].Why, frag)
		}
	}

	// An unmatched line still answers, with a reason: "so a session can say why
	// it has nothing".
	empty := mtMatch(t, ctx, s, atDash, `{"text":"nothing in this line names a ticket"}`)
	if empty.Matched {
		t.Errorf("an unmatched text reported matched:true (%+v)", empty)
	}
	if len(empty.Proposals) != 0 {
		t.Errorf("an unmatched text returned %d proposal(s), want none", len(empty.Proposals))
	}
	if !strings.Contains(strings.ToLower(empty.Reason), "no enabled rule matched") {
		t.Errorf("an unmatched text's reason is %q, want capture's own \"no enabled rule matched\" (D6: "+
			"matched:false with an empty list STILL returns the reason)", empty.Reason)
	}
}

// ---- criterion 23: project and limit ------------------------------------------------------

func TestTaskMatch_Integration_ProjectFiltersAndLimitCaps(t *testing.T) {
	ctx := context.Background()
	s := newRVSuite(t, ctx)
	amRequire0039(t, ctx, s.pool)
	f := newMTFixture(t, ctx, s)
	for i := 0; i < 6; i++ {
		s.id(t, ctx,
			`INSERT INTO tasks (project_id, title, status, source_thread_id)
			 VALUES ($1,'itest-revive thread sibling','ready',$2) RETURNING id`, s.project, f.thread)
	}

	if r := mtMatch(t, ctx, s, atDash, `{"message_id":`+itoa(f.message)+`}`); len(r.Proposals) > 5 {
		t.Errorf("task_match returned %d proposals with no limit, want at most the DEFAULT 5 (D6)", len(r.Proposals))
	}
	if r := mtMatch(t, ctx, s, atDash, `{"message_id":`+itoa(f.message)+`,"limit":2}`); len(r.Proposals) > 2 {
		t.Errorf("task_match(limit=2) returned %d proposals (criterion 23)", len(r.Proposals))
	}
	// project filters by SLUG.
	other := mtMatch(t, ctx, s, atDash, `{"message_id":`+itoa(f.message)+`,"project":"itest-revive-nosuch"}`)
	if len(other.Proposals) != 0 {
		t.Errorf("task_match(project=itest-revive-nosuch) returned %d proposal(s), want 0 — criterion 23: "+
			"`project`, when given, filters proposals by project slug", len(other.Proposals))
	}
	mine := mtMatch(t, ctx, s, atDash, `{"message_id":`+itoa(f.message)+`,"project":"`+rvSlug+`"}`)
	if len(mine.Proposals) == 0 {
		t.Errorf("task_match(project=%s) returned nothing although every fixture task is in that project", rvSlug)
	}
}
