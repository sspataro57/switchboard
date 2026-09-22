package tools_test

// task_match's no-database half — comms-inbox (SWT-74,
// docs/tickets/comms-inbox_SPEC.md) D6 and criterion 22: the routing READ is
// registered on the executor and validates its input shape before any handler
// runs. "Routing is one verb and one read": this is the read.
//
// Driven through executor.Execute with a NIL pool (requeue_test.go's shape):
// every refusal stops at Validate, so nothing dereferences the pool, and an
// "unknown tool" error is explicitly rejected as NOT a validation failure.
//
// ---- IMPOSED SURFACE (SPEC D6 and the "API / MCP tool changes" table) ---------
//
//	task_match {message_id? | task_id? | text?, project?, limit?}
//	  -> {input, matched, reason,
//	      proposals:[{task_id,title,status,assignee_type,project,source,rule_id,
//	                  rule_kind,external_system,external_key,partial,why}]}
//	  registered in tools.Register (internal/tools/createtask.go's table) with
//	  validateMatch / matchTask, implemented in internal/tools/match.go.
//	  EXACTLY ONE of message_id, task_id, text (zero or two is an error naming
//	  all three); limit within 1..20, default 5; a non-empty text.
//	  Policy: allow / static default — NOT humanOnly, NOT mcpHumanOnly, NOT
//	  snapshotGated (task_list's shape). MCP: agentTools + userProfileTools.
//	  source ∈ {"rule_ref","source_thread"}, rule_ref first.
//
//	// internal/capture/explain.go — the ONE matcher the tool calls
//	func ExplainMessage(ctx, pool, messageID int64) (capture.Explanation, error)
//	func ExplainText(ctx, pool, msg capture.Message) (capture.Explanation, error)
//
// GREENFIELD NOTE, EXPECTED RED: task_match is not registered and match.go does
// not exist, so every test here fails on its own assertion (the registration
// test by name, the validation tests with "unknown tool").
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - accept two inputs at once -> RefusesAnythingButExactlyOneInput.
//   - drop the limit bounds -> RefusesALimitOutsideTheRange.
//   - accept an empty text -> RefusesAnEmptyText.
//   - task_match writes anything -> TheHandlerIsReadOnly (the source scan).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const matchTool = "task_match"

func matchExecutor() *executor.Executor {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	// The STATIC allow-list, not the matrix: this file tests validation; the
	// policy row is internal/policy's matrix_match_test.go (criterion 26).
	return executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
}

// Criterion 22: registered like every other tool, in createtask.go's table.
func TestRegister_TaskMatchIsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	for _, n := range reg.Names() {
		if n == matchTool {
			return
		}
	}
	t.Errorf("tool %q is not registered by tools.Register. D6: \"Routing is one verb and one read\" — "+
		"task_match runs capture's OWN matcher over a message, a task or a pasted line and proposes the task "+
		"it belongs to, through the executor like everything else (invariant 3)", matchTool)
}

// Criterion 22: EXACTLY one of message_id, task_id, text. Zero or two is an
// error NAMING ALL THREE, because the caller is a model that has to be told
// what the alternatives are.
func TestValidate_TaskMatch_RefusesAnythingButExactlyOneInput(t *testing.T) {
	ex := matchExecutor()
	ctx := context.Background()
	for _, args := range []string{
		`{}`,
		`{"project":"collaboratory"}`,
		`{"limit":5}`,
		`{"message_id":12,"task_id":452}`,
		`{"message_id":12,"text":"WEB-10469"}`,
		`{"task_id":452,"text":"WEB-10469"}`,
		`{"message_id":12,"task_id":452,"text":"WEB-10469"}`,
		`{"message_id":0}`,
		`{"task_id":-1}`,
	} {
		args := args
		t.Run(args, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: matchTool, Actor: "mcp:manual:salvo", Args: json.RawMessage(args)})
			if err == nil {
				t.Fatalf("%s(%s) succeeded; want a validation failure (criterion 22: EXACTLY one of message_id, "+
					"task_id, text)", matchTool, args)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", matchTool, err)
			}
			msg := err.Error()
			for _, name := range []string{"message_id", "task_id", "text"} {
				if !strings.Contains(msg, name) {
					t.Errorf("%s(%s) = %q, want a refusal naming ALL THREE inputs (%s missing). A session that "+
						"guessed wrong has to be told what the alternatives are", matchTool, args, msg, name)
				}
			}
		})
	}
}

// Criterion 22: limit within 1..20. The cap is D6's ("titles only, never
// bodies… rows in a model context cost tokens" — task_list's rule).
func TestValidate_TaskMatch_RefusesALimitOutsideTheRange(t *testing.T) {
	ex := matchExecutor()
	ctx := context.Background()
	for _, limit := range []int{0, -1, 21, 100} {
		args, _ := json.Marshal(map[string]any{"message_id": 12, "limit": limit})
		_, err := ex.Execute(ctx, executor.Call{Tool: matchTool, Actor: "mcp:manual:salvo", Args: args})
		if err == nil {
			t.Errorf("%s(limit=%d) succeeded; criterion 22: limit is within 1..20 (default 5)", matchTool, limit)
			continue
		}
		if strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("%s is not registered (%v)", matchTool, err)
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("%s(limit=%d) = %q, want a refusal naming limit", matchTool, limit, err)
		}
	}
}

// Criterion 22: a non-empty text. `{text:""}` is not "no input" — it is the
// pasted-line case with nothing pasted, and answering "no enabled rule matched"
// for it would be a lie about the corpus.
func TestValidate_TaskMatch_RefusesAnEmptyText(t *testing.T) {
	ex := matchExecutor()
	ctx := context.Background()
	for _, args := range []string{`{"text":""}`, `{"text":"   "}`, `{"text":"\n\t"}`} {
		_, err := ex.Execute(ctx, executor.Call{Tool: matchTool, Actor: "mcp:manual:salvo", Args: json.RawMessage(args)})
		if err == nil {
			t.Errorf("%s(%s) succeeded; criterion 22: text must be non-empty", matchTool, args)
			continue
		}
		if strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("%s is not registered (%v)", matchTool, err)
		}
		if !strings.Contains(err.Error(), "text") {
			t.Errorf("%s(%s) = %q, want a refusal naming text", matchTool, args, err)
		}
	}
}

// The three shapes that MUST pass validation and reach the handler (where a nil
// pool panics or errors — either answer proves the ARGUMENTS were accepted).
// Only a "validate task_match" failure is a refusal.
func TestValidate_TaskMatch_AcceptsEachInputAlone(t *testing.T) {
	ex := matchExecutor()
	for _, args := range []string{
		`{"message_id":12}`,
		`{"task_id":452}`,
		`{"text":"José: WEB-10469 still blocks the import"}`,
		`{"message_id":12,"project":"collaboratory"}`,
		`{"message_id":12,"limit":1}`,
		`{"message_id":12,"limit":20}`,
	} {
		args := args
		t.Run(args, func(t *testing.T) {
			var err error
			func() {
				defer func() { _ = recover() }() // a nil pool panics: that is past validation
				_, err = ex.Execute(context.Background(),
					executor.Call{Tool: matchTool, Actor: "mcp:manual:salvo", Args: json.RawMessage(args)})
			}()
			if err == nil {
				return
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v)", matchTool, err)
			}
			if strings.Contains(err.Error(), "validate "+matchTool) {
				t.Errorf("%s(%s) was refused by the VALIDATOR: %v. D6: each of the three inputs stands alone, "+
					"and project/limit are optional", matchTool, args, err)
			}
		})
	}
}

// ---- criterion 20/23's structural half: it is a READ, and not a second matcher ----

func TestTaskMatch_TheHandlerIsReadOnlyAndCallsCapturesOwnMatcher(t *testing.T) {
	b, err := os.ReadFile("match.go")
	if err != nil {
		t.Fatalf("read internal/tools/match.go: %v — criterion 22: task_match lives in its own file", err)
	}
	src := string(b)
	for _, banned := range []struct{ tok, why string }{
		{"INSERT", "D6: read-only — it writes nothing but its audit row, which the executor writes"},
		{"UPDATE ", "D6: read-only"},
		{"DELETE", "D6: read-only"},
		{"inTx(", "D6: nothing is written, so nothing needs a transaction"},
		{"lockTask(", "D6: a read takes no row lock"},
	} {
		if strings.Contains(src, banned.tok) {
			t.Errorf("internal/tools/match.go contains %q — %s", banned.tok, banned.why)
		}
	}
	for _, want := range []struct{ tok, why string }{
		{"capture.ExplainMessage", "D6: the tool calls ONE new exported entry point in capture, which runs " +
			"decideMessage itself. A second matcher is two answers to the same question"},
		{"capture.ExplainText", "D6: the pasted-line case (\"I would like a help for claude to match a " +
			"particular line to a task\")"},
		{"source_thread_id", "D6's rank-1 evidence source: open tasks whose source_thread_id is the message's " +
			"thread, oldest first (threadTask's shape, deliberately NOT project-scoped — a thread is a " +
			"conversation and the human decides)"},
		{`"rule_ref"`, "D6: the rank-0 source name"},
		{`"source_thread"`, "D6: the rank-1 source name"},
	} {
		if !strings.Contains(src, want.tok) {
			t.Errorf("internal/tools/match.go does not mention %s — %s", want.tok, want.why)
		}
	}
	// D6's explicit refusal: NO recency source. "The 10 most recent open tasks
	// in the project is not evidence, it is a list that looks like matches."
	if strings.Contains(src, "ORDER BY t.created_at DESC") || strings.Contains(src, "ORDER BY id DESC LIMIT") {
		t.Errorf("internal/tools/match.go looks like it ranks by recency. D6 (unilateral): there is NO recency " +
			"source — task_list(project=…) already returns that list and is on both profiles")
	}
	// Titles only, never bodies (task_list's rule: rows in a model context cost
	// tokens; task_context is the per-task read).
	if strings.Contains(src, "t.body") || strings.Contains(src, `"body"`) {
		t.Errorf("internal/tools/match.go selects a task BODY. D6: titles only — task_context is the per-task read")
	}
}
