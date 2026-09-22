package policy_test

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) D6 and criterion 26:
// task_match falls through the static allow-list for EVERY actor shape that
// exists in the repo. It is in NEITHER humanOnly NOR mcpHumanOnly NOR
// snapshotGated — task_list's shape, because it writes nothing but its audit
// row.
//
// Criterion 26 names five shapes explicitly: mcp:manual:salvo, mcp:treetop,
// dashboard:salvo, opsctl:salvo and capture:google. They are enumerated here
// with the rest of the repo's shapes (IK: "an actor-prefix check is a transport
// label, not a trust boundary" — when a test is about a gate, enumerate the
// shapes that exist, because one of them is usually the hole). Here there is
// deliberately NO gate, so the enumeration proves that nothing keys on the
// caller.
//
// WHY A WORKER MAY ASK (D6): "A worker console asking 'which task does this
// line belong to' cannot act on the answer — task_attach is humanOnly — so this
// does not let it choose its own work." The two halves are inseparable: this
// file is only safe while matrix_attach_test.go stays green.
//
// Pure matrix core, ZERO I/O (invariant 7). matrix_tasklist_test.go is the
// template; it reuses recordingLoader and gmailSnap from matrix_test.go.
//
// IMPOSED SURFACE (SPEC's "API / MCP tool changes" table):
//
//	// internal/policy/matrix.go is NOT EDITED for task_match.
//	// internal/tools: task_match registered in tools.Register, which is what
//	// puts it on the static allow-list every main builds from reg.Names().
//
// GREENFIELD NOTE — EXPECTED RED, for the right reason. Two halves:
//   - "spec static list": NewStatic("task_match") exactly as the criterion
//     says. GREEN today and a GUARD: it can only go red if someone adds the
//     tool to humanOnly or snapshotGated.
//   - "real registry": the allow-list built from tools.Register, the way every
//     main builds it (a list the test supplies itself would allow the tool no
//     matter what the repo does). RED today — the tool is not registered, so
//     every case is deny/static-default "tool not in registered set".

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/tools"
)

const matchToolName = "task_match"

// The five shapes criterion 26 names, plus the rest of the repo's.
var matchActorShapes = []string{
	"mcp:manual:salvo",  // an interactive session: `swb match <id>`
	"mcp:treetop",       // a worker console (D6: it may ask; task_attach is what it cannot call)
	"mcp:treetop.main",  // a multi-console worker
	"dashboard:salvo",   // the board
	"opsctl:salvo",      // `opsctl task-match --message N`
	"capture:google",    // the capture engine's shape, named by the criterion
	"drafts:gpt",        // the draft worker, executor-direct
	"worker:treetop",    // bare worker, no transport prefix
	"orchestrator",      // the spine
	"ticketstatus:jira", // the SWT-32 reconciler
}

func matchToolArgs() json.RawMessage { return json.RawMessage(`{"message_id":8801}`) }

func TestMatrix_TaskMatch_FallsThroughForEveryActorShape(t *testing.T) {
	ctx := context.Background()

	reg := executor.NewRegistry()
	tools.Register(reg, nil) // nil pool: Register only builds closures.

	for _, fb := range []struct {
		name     string
		fallback policy.Checker
	}{
		{"spec static list", policy.NewStatic(matchToolName)},
		{"real registry", policy.NewStatic(reg.Names()...)},
	} {
		for _, actor := range matchActorShapes {
			fb, actor := fb, actor
			t.Run(fb.name+"/"+actor, func(t *testing.T) {
				l := &recordingLoader{snap: gmailSnap(0, false)}
				d, err := policy.NewMatrix(l, fb.fallback).Check(ctx,
					policy.Request{Tool: matchToolName, Actor: actor, Args: matchToolArgs()})
				if err != nil {
					t.Fatalf("Check(%s, %q): %v", matchToolName, actor, err)
				}
				if d.Decision != "allow" {
					t.Errorf("%s by %q = %q (rule %s: %s), want allow. Criterion 26: task_match is read-only "+
						"(task_list's shape) and registration in tools.Register is what puts it on the "+
						"static allow-list", matchToolName, actor, d.Decision, d.Rule, d.Reason)
				}
				if d.Rule == "human_only" {
					t.Errorf("%s by %q was denied human_only: the tool has been added to policy.humanOnly. D6: "+
						"a worker console asking which task a line belongs to CANNOT act on the answer "+
						"(task_attach is humanOnly), so asking is free", matchToolName, actor)
				}
				if l.called {
					t.Errorf("%s consulted the send-snapshot loader: it has become snapshotGated. Nothing "+
						"leaves the system, so the kill switch and rate limit have no claim on it — and a "+
						"frozen switch must not stop a session asking where a comm belongs", matchToolName)
				}
			})
		}
	}

	// The pure core, with a snapshot of nothing: allow, rule matrix-human — the
	// branch any non-sendShaped tool reaches once it is past the human_only
	// gate. That it is NOT human_only is the proof it is in neither map.
	for _, actor := range matchActorShapes {
		d := policy.Decide(policy.Request{Tool: matchToolName, Actor: actor, Args: matchToolArgs()}, policy.Snapshot{})
		if d.Decision != "allow" || d.Rule != "matrix-human" {
			t.Errorf("Decide(%s, %q) = %q/%q, want allow/matrix-human — never human_only and never "+
				"mcp_human_only (criterion 26)", matchToolName, actor, d.Decision, d.Rule)
		}
	}
}
