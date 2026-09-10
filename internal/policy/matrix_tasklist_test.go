package policy_test

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criterion 17 and L12: task_list
// and project_list fall through the static allow-list for EVERY actor shape that
// exists in the repo. Neither is humanOnly, neither is snapshotGated, and
// internal/policy (non-test) is NOT edited by this ticket. Pure matrix core,
// ZERO I/O (invariant 7). Reuses recordingLoader and gmailSnap from
// matrix_test.go (same package).
//
// WHY EVERY ACTOR SHAPE, when the expectation is uniform: IK's "an actor-prefix
// check is a transport label, not a trust boundary" — when a test is about a
// gate, enumerate the shapes that exist, because one of them is usually the
// hole. Here there is deliberately NO gate (L2a: cross-client privacy is not a
// goal, Salvador 2026-09-10; and since the same day task_list does not refuse
// local_only projects either), so the enumeration proves that nothing keys on
// the caller. Note the shapes are the REAL ones: a worker console is
// `mcp:{client}` / `mcp:{client}.{sub}` (opsworker sets OPS_WORKER_ID to the
// bare client — SPEC fact 3); `mcp:worker:acme` appears only in tests, and is
// kept because a test-only shape that got denied would still be a gate.
//
// GREENFIELD NOTE — EXPECTED RED, for the right reason. Two halves, as the SPEC
// writes it and as production wires it:
//   - "spec static list": NewStatic("task_list","project_list") exactly as the
//     criterion says. GREEN today and a GUARD: it can only go red if someone
//     adds either tool to humanOnly or snapshotGated.
//   - "real registry": the allow-list built from tools.Register, the way every
//     main builds it (matrix_reopen_test.go's reason: a list the test supplies
//     itself allows the tool no matter what the repo does). RED today —
//     neither tool is registered, so every case is deny/static-default "tool
//     not in registered set".
//   - Decide: GREEN today, a guard that neither tool enters humanOnly.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

var queueReadTools = []string{"task_list", "project_list"}

// The nine actor shapes of SPEC criterion 12, which criterion 17 reuses.
var queueActorShapes = []string{
	"dashboard:salvo",   // the board
	"opsctl:salvo",      // `opsctl call --tool task_list` (fact 10)
	"mcp:manual:salvo",  // the user-scope install and .mcp.json (L13)
	"mcp:acme",          // a single-console worker: OPS_WORKER_ID = bare client
	"mcp:acme.main",     // a multi-console worker
	"mcp:worker:acme",   // test-only shape, kept (see header)
	"drafts:gpt",        // the draft worker, executor-direct
	"worker:acme",       // bare worker, no transport prefix
	"ticketstatus:jira", // the SWT-32 reconciler
}

func queueToolArgs(tool string) json.RawMessage {
	if tool == "task_list" {
		return json.RawMessage(`{"project":"collaboratory","worker_id":"acme"}`)
	}
	return json.RawMessage(`{"worker_id":"acme"}`)
}

func TestMatrix_QueueReadToolsFallThroughForEveryActorShape(t *testing.T) {
	ctx := context.Background()

	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures.

	for _, fb := range []struct {
		name     string
		fallback policy.Checker
	}{
		{"spec static list", policy.NewStatic("task_list", "project_list")},
		{"real registry", policy.NewStatic(reg.Names()...)},
	} {
		for _, tool := range queueReadTools {
			for _, actor := range queueActorShapes {
				fb, tool, actor := fb, tool, actor
				t.Run(fb.name+"/"+tool+"/"+actor, func(t *testing.T) {
					l := &recordingLoader{snap: gmailSnap(0, false)}
					d, err := policy.NewMatrix(l, fb.fallback).Check(ctx,
						policy.Request{Tool: tool, Actor: actor, Args: queueToolArgs(tool)})
					if err != nil {
						t.Fatalf("Check(%s, %q): %v", tool, actor, err)
					}
					if d.Decision != "allow" {
						t.Errorf("%s by %q = %q (rule %s: %s), want allow. L12: both tools fall through the "+
							"static allow-list, exactly like mail_search — and registration in tools.Register "+
							"is what puts them on it", tool, actor, d.Decision, d.Rule, d.Reason)
					}
					if d.Rule == "human_only" {
						t.Errorf("%s by %q was denied human_only: the tool has been added to policy.humanOnly. "+
							"It writes nothing but its audit row, and L2a leaves every caller free to read any "+
							"project's queue", tool, actor)
					}
					if l.called {
						t.Errorf("%s consulted the send-snapshot loader: it has become snapshotGated. Nothing "+
							"leaves the system, so the kill switch and rate limit have no claim on it — and a "+
							"frozen switch must not stop a session reading its queue", tool)
					}
				})
			}
		}
	}

	// The pure core, with a snapshot of nothing: allow, rule matrix-human — the
	// branch any non-sendShaped tool reaches once it is past the human_only gate.
	// That it is NOT human_only is the proof neither tool is in humanOnly.
	for _, tool := range queueReadTools {
		for _, actor := range queueActorShapes {
			d := policy.Decide(policy.Request{Tool: tool, Actor: actor, Args: queueToolArgs(tool)}, policy.Snapshot{})
			if d.Decision != "allow" || d.Rule != "matrix-human" {
				t.Errorf("Decide(%s, %q) = %q/%q, want allow/matrix-human — never human_only (L12)",
					tool, actor, d.Decision, d.Rule)
			}
		}
	}
}
