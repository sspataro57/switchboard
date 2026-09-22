package tools_test

// task_mark_activity's no-database half — activity-resurfaces (SWT-72,
// docs/tickets/activity-resurfaces_SPEC.md) D3 and criteria 2, 3, 5 and 6: the
// tool is registered on the executor, validates task_id / message_id / reason
// before any handler runs, is ABSENT from both MCP profiles, and its file
// touches ONLY the two activity columns.
//
// Driven through executor.Execute with a NIL pool, surfaced_test.go's shape:
// every refusal stops at Validate, so nothing dereferences the pool, and an
// "unknown tool" error is explicitly rejected as NOT a validation failure.
//
// IMPOSED SURFACE (the SPEC's "API / MCP tool changes" table — greenfield, so
// the SPEC's contract defines the signature):
//
//	task_mark_activity {task_id, message_id, reason} -> {task_id, marked, skipped?}
//	  registered in tools.Register (internal/tools/createtask.go) with
//	  validateMarkActivity / markActivity, implemented in internal/tools/activity.go;
//	  not humanOnly (capture:{connector} and promote:{lane} call it);
//	  no policy rule (static-default); never in internal/mcpserver/schemas.go
//	  or adapter.go's profile slices.
//
// GREENFIELD NOTE, EXPECTED RED: the tool is not registered, so the
// registration and validation tests fail on their own assertions, and
// activity.go does not exist, so the structure test fails by name. The two
// MCP-absence checks are GUARDS, green today and required to stay green (F7:
// an argument hidden from a schema is still reachable, so the tool's ABSENCE
// from every profile is the only boundary).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - list task_mark_activity in schemas.go or adapter.go -> StaysOffBothMCPProfiles.
//   - activity.go writes surfaced_at / reviewed_at / status / priority /
//     updated_at, or inserts a task_events row -> TouchesOnlyTheActivityColumns.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const markActivity = "task_mark_activity"

// Criterion 2.
func TestRegister_TaskMarkActivityIsRegistered(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	for _, n := range reg.Names() {
		if n == markActivity {
			return
		}
	}
	t.Errorf("tool %q is not registered by tools.Register. D3/criterion 2: capture and promote record inbound "+
		"activity through the executor (validate -> policy -> audit), never by writing tasks.activity_* "+
		"themselves (invariant 3)", markActivity)
}

// Criterion 3: validation refuses task_id <= 0, message_id <= 0 and an empty
// reason. Each refusal names the field.
func TestValidate_TaskMarkActivity_RefusesIncompleteArgs(t *testing.T) {
	reg := executor.NewRegistry()
	tools.Register(reg, nil)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewMemStore())
	ctx := context.Background()

	for _, tc := range []struct{ name, args, wantIn string }{
		{"empty object", `{}`, "task_id"},
		{"zero task_id", `{"task_id":0,"message_id":9,"reason":"capture: jira comment"}`, "task_id"},
		{"negative task_id", `{"task_id":-1,"message_id":9,"reason":"capture: jira comment"}`, "task_id"},
		{"missing message_id", `{"task_id":7,"reason":"capture: jira comment"}`, "message_id"},
		{"zero message_id", `{"task_id":7,"message_id":0,"reason":"capture: jira comment"}`, "message_id"},
		{"negative message_id", `{"task_id":7,"message_id":-2,"reason":"capture: jira comment"}`, "message_id"},
		{"missing reason", `{"task_id":7,"message_id":9}`, "reason"},
		{"empty reason", `{"task_id":7,"message_id":9,"reason":""}`, "reason"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := ex.Execute(ctx, executor.Call{Tool: markActivity, Actor: "capture:google", Args: json.RawMessage(tc.args)})
			if err == nil {
				t.Fatalf("%s(%s) succeeded; want a validation failure", markActivity, tc.args)
			}
			if strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("%s is not registered (%v); that is not a validation failure", markActivity, err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("%s(%s) = %q, want a refusal naming %q", markActivity, tc.args, err, tc.wantIn)
			}
		})
	}
}

// Criterion 6: absent from BOTH MCP profiles' schemas — the SWT-45
// TestTaskMarkSurfaced_StaysOffBothMCPProfiles scan, EXTENDED to both tool
// names (the SPEC's words). F7 is why this matters: the MCP adapter passes
// arguments through, so if a worker could REACH this tool it could surface any
// task into INCOMING with any inbound message, and Requeue is the only way to
// clear it.
//
// GUARD: green today, and must stay green.
func TestTaskMarkActivity_StaysOffBothMCPProfiles(t *testing.T) {
	for _, rel := range []string{"internal/mcpserver/schemas.go", "internal/mcpserver/adapter.go"} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		src := string(b)
		// Control: the scan must be looking at files that list tools at all.
		if !strings.Contains(src, `"task_list"`) {
			t.Fatalf("%s does not mention \"task_list\"; this scan is not looking at a tool list", rel)
		}
		for _, name := range []string{markActivity, markSurfaced} {
			if strings.Contains(src, `"`+name+`"`) {
				t.Errorf("%s names %q. D3/F7: both marking tools are spine-facing (capture and promote only); "+
					"a transport to either lets a worker move a task onto the board with an inbound message "+
					"it chose", rel, name)
			}
		}
	}
}

// Criterion 5: the handler writes activity_at and activity_by_message_id and
// NOTHING else — not surfaced_at (D1: reusing it would hold every done Jira
// ticket's task open forever), not reviewed_at (D2: only a review clears),
// not status, priority or updated_at (D3: updated_at moves the board's
// `updated` cell and the SWT-45 revive guard's fallback) — and it writes no
// task_events row (the caller appended the log line one statement earlier).
func TestActivityFile_TouchesOnlyTheActivityColumns(t *testing.T) {
	b, err := os.ReadFile("activity.go")
	if err != nil {
		t.Fatalf("read internal/tools/activity.go: %v — criterion 5: task_mark_activity lives in its own file, "+
			"modelled line for line on surfaced.go", err)
	}
	// Strip // comments: the prose explains what the code must NOT do, and a
	// scan that read it would be satisfied by the explanation.
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	src := strings.Join(code, "\n")

	if !strings.Contains(src, "activity_at") || !strings.Contains(src, "activity_by_message_id") {
		t.Fatalf("activity.go does not mention activity_at / activity_by_message_id; it writes neither of D1's "+
			"two columns:\n%s", src)
	}
	if !strings.Contains(src, "FOR UPDATE") && !strings.Contains(src, "lockTask") {
		t.Errorf("activity.go takes no row lock (lockTask / FOR UPDATE). D3: the three rules are decided under " +
			"the tasks row lock, exactly as task_mark_surfaced decides its own")
	}
	for _, banned := range []struct{ tok, why string }{
		{"surfaced_at", "D1: surfaced_at keeps its SWT-45 meaning; reusing it holds every done ticket's task open forever"},
		{"surfaced_by_message_id", "D1: the SWT-45 pair is not reused"},
		{"reviewed_at", "D2: only a review (task_requeue, closeTransition) stamps it"},
		{"insertTaskEvent", "D3: no task_events row — the caller appended the log line one statement earlier, " +
			"and a second event is noise on the orchestrator's feed (D9)"},
		{"task_events", "D3: no task_events row"},
		{"updated_at", "D3: updated_at would move the board's `updated` cell and the SWT-45 revive guard's fallback"},
		{"priority", "D3: it touches nothing else"},
	} {
		if strings.Contains(src, banned.tok) {
			t.Errorf("activity.go mentions %s — %s", banned.tok, banned.why)
		}
	}
	// `status` appears legitimately as the value lockTask returns (the closed-task
	// skip reads it). What must not appear is a WRITE of it.
	if strings.Contains(strings.ToLower(src), "set status") {
		t.Errorf("activity.go writes tasks.status; D3: it touches nothing but the two activity columns")
	}
}
