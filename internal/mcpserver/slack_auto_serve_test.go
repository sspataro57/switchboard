package mcpserver

// slack-auto-tier (SWT-77) Verification Step 1, "MCP round trip": send_slack_reply
// called over the in-memory transport under BOTH shipped profiles, the way a
// Claude Code session reaches it — tools/list over the wire, then tools/call,
// into the executor with the MCP actor, and the executor's result back
// verbatim. serve_test.go's shape (internal: sdkServer is unexported).
//
// GREENFIELD NOTE — EXPECTED RED: send_slack_reply is in neither agentTools nor
// userProfileTools, so it is absent from tools/list and the call comes back
// IsError ("not an MCP tool") without reaching the executor.

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sspataro57/switchboard/internal/executor"
)

type ssrRecExec struct {
	calls []executor.Call
}

func (r *ssrRecExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	r.calls = append(r.calls, call)
	return executor.Result{Output: []byte(`{"delivery_id":57,"status":"sending","queued":true,"job_id":"send-1","queued_at":"2026-09-22T20:52:00Z"}`)}, nil
}

func TestSendSlackReply_MCPRoundTripBothProfiles(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		worker  string
	}{
		{ProfileFull, "avviato"},
		{ProfileUser, "manual:salvo"},
	} {
		tc := tc
		t.Run(string(tc.profile), func(t *testing.T) {
			ctx := context.Background()
			rec := &ssrRecExec{}
			ct, st := mcp.NewInMemoryTransports()
			ss, err := NewWithProfile(rec, tc.worker, tc.profile).sdkServer("ops-test").Connect(ctx, st, nil)
			if err != nil {
				t.Fatalf("server connect: %v", err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatalf("client connect: %v", err)
			}
			defer cs.Close()

			list, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			listed := false
			for _, tl := range list.Tools {
				if tl.Name == "send_slack_reply" {
					listed = true
				}
			}
			if !listed {
				t.Errorf("send_slack_reply is not in tools/list over the wire for the %s profile (criterion 17)", tc.profile)
			}

			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_slack_reply", Arguments: map[string]any{
				"task_id":    506,
				"target_ref": "https://app.slack.com/client/T0360B84U/DSA806DHA",
				"text":       "switchboard auto-tier smoke — ignore",
			}})
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if res.IsError {
				msg := ""
				if len(res.Content) > 0 {
					if txt, ok := res.Content[0].(*mcp.TextContent); ok {
						msg = txt.Text
					}
				}
				t.Fatalf("tools/call send_slack_reply came back IsError under the %s profile: %s", tc.profile, msg)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("executor calls = %d, want 1", len(rec.calls))
			}
			if c := rec.calls[0]; c.Tool != "send_slack_reply" || c.Actor != "mcp:"+tc.worker {
				t.Errorf("executor got %s as %q, want send_slack_reply as %q", c.Tool, c.Actor, "mcp:"+tc.worker)
			}
			if len(res.Content) != 1 {
				t.Fatalf("result content = %d items, want 1", len(res.Content))
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok || text.Text != `{"delivery_id":57,"status":"sending","queued":true,"job_id":"send-1","queued_at":"2026-09-22T20:52:00Z"}` {
				t.Errorf("result = %+v, want the executor's queued result verbatim (the session must see queued: true)", res.Content[0])
			}
		})
	}
}
