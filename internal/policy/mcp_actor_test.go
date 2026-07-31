package policy_test

// Unit tests for the MCP transport-prefix rule on the human-only gate.
// ZERO I/O — the matrix core is a pure function of (Request, Snapshot),
// invariant 7.
//
// The rule: policy.humanActor() strips ONE optional leading "mcp:" transport
// prefix before checking the dashboard:/opsctl:/manual: set. An interactive
// session (ops-mcp sets Actor = "mcp:" + OPS_WORKER_ID, and .mcp.json's worker
// id is manual:salvo) therefore passes, while an autonomous worker identity
// ("mcp:worker:acme") is DENIED with rule human_only.
//
// Why the prefix is stripped in policy rather than never applied by the MCP
// adapter: the audit row keeps the FULL unmodified actor string, so "which
// surface triggered this send" stays answerable forever. Normalising in the
// adapter would make an MCP manual:salvo indistinguishable from an opsctl one.
// Recorded as OQ1 option A on the imap-mail-connector SPEC, 2026-07-26.
//
// Existing helpers reused from matrix_test.go (same package): assertDeny,
// gmailSnap, recordingLoader.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/policy"
)

// mcpHumanOnlyTools are the send-shaped tools an interactive session reaches
// over MCP. They stay in policy.humanOnly; the actor gate is the ONLY thing
// standing between an autonomous worker and a send.
var mcpHumanOnlyTools = []string{"approve_delivery", "send_delivery"}

func TestHumanActor_StripsOneMCPTransportPrefix(t *testing.T) {
	cases := []struct {
		actor     string
		wantAllow bool
		why       string
	}{
		{"manual:salvo", true, "interactive session, direct"},
		{"mcp:manual:salvo", true, "interactive session over MCP"},
		{"mcp:dashboard:salvo@example.com", true, "dashboard identity over MCP"},
		{"mcp:opsctl:salvo", true, "opsctl identity over MCP"},
		{"dashboard:salvo@example.com", true, "dashboard, direct"},
		{"opsctl:salvo", true, "opsctl, direct"},
		{"worker:acme", false, "autonomous worker, direct"},
		{"mcp:worker:acme", false, "autonomous worker over MCP — the case this gate exists for"},
		{"mcp:acme.main", false, "wrapper worker id over MCP"},
		{"mcp:mcp:manual:salvo", false, "exactly ONE transport prefix is stripped"},
		{"drafts:gpt", false, "the draft worker is not a human"},
		{"", false, "no actor at all"},
	}

	for _, tool := range mcpHumanOnlyTools {
		tool := tool
		for _, tc := range cases {
			tc := tc
			t.Run(tool+"/"+tc.actor, func(t *testing.T) {
				d := policy.Decide(policy.Request{Tool: tool, Actor: tc.actor}, gmailSnap(0, false))
				if tc.wantAllow {
					if d.Decision != "allow" {
						t.Fatalf("%s by %q = %q/%q (%s), want allow — %s",
							tool, tc.actor, d.Decision, d.Rule, d.Reason, tc.why)
					}
					return
				}
				if d.Decision == "allow" {
					t.Fatalf("%s by %q was ALLOWED; want deny/human_only — %s", tool, tc.actor, tc.why)
				}
				assertDeny(t, d, "human_only")
			})
		}
	}
}

// The deny must be reachable through the Matrix wrapper too — and WITHOUT
// consulting the snapshot loader, so a worker identity is refused before any
// db work happens.
func TestMatrix_MCPWorkerIdentityDeniedBeforeTheLoaderRuns(t *testing.T) {
	ctx := context.Background()
	fallback := policy.NewStatic("send_delivery", "approve_delivery")

	for _, tool := range mcpHumanOnlyTools {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			l := &recordingLoader{snap: gmailSnap(0, false)}
			m := policy.NewMatrix(l, fallback)
			d, err := m.Check(ctx, policy.Request{
				Tool:  tool,
				Actor: "mcp:worker:acme",
				Args:  json.RawMessage(`{"delivery_id":1}`),
			})
			if err != nil {
				t.Fatalf("Check(%s): %v", tool, err)
			}
			assertDeny(t, d, "human_only")
			if l.called {
				t.Errorf("the snapshot loader ran for a denied worker identity")
			}
		})
	}
}

// The read-only mail tools fall through the static allow-list: they are NOT
// humanOnly and NOT snapshotGated (criterion 16), so an ordinary worker can
// call them.
func TestMatrix_MailToolsFallThroughForWorkers(t *testing.T) {
	ctx := context.Background()
	fallback := policy.NewStatic("mail_search", "mail_read_thread")
	for _, tool := range []string{"mail_search", "mail_read_thread"} {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			l := &recordingLoader{snap: gmailSnap(0, false)}
			m := policy.NewMatrix(l, fallback)
			d, err := m.Check(ctx, policy.Request{Tool: tool, Actor: "mcp:worker:acme", Args: json.RawMessage(`{"query":"invoice"}`)})
			if err != nil {
				t.Fatalf("Check(%s): %v", tool, err)
			}
			if d.Decision != "allow" {
				t.Fatalf("%s by a worker = %q/%q, want allow (read-only, static fallthrough)", tool, d.Decision, d.Rule)
			}
			if l.called {
				t.Errorf("%s must not consult the send-snapshot loader", tool)
			}
		})
	}
}
