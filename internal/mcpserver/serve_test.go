package mcpserver

// The wiring behind the "swb" shorthand: what a real MCP client receives at
// initialize, for EVERY profile (ops-mcp and ops-mcp-user share this path;
// ProfileRead is kept as the fail-closed floor although no binary builds it
// since SWT-37). TestInstructions_TeachTheSwbShorthand checks the text of the
// const; this checks the const is what goes over the wire — a nil
// ServerOptions would pass the text test and silently drop the shorthand from
// every session.
//
// SWT-37 criterion 15 adds the {ProfileUser, len(userProfileTools)} row.
// GREENFIELD — EXPECTED RED: ProfileUser and userProfileTools do not exist, so
// package mcpserver's tests compile-FAIL until adapter.go declares them.
//
// SWT-38 (mcp-task-capture) criterion 19: the counts are now PINNED as literals
// rather than len(agentTools)/len(userProfileTools), which were true of any
// slice. Full: 22 (SWT-37) + task_set_priority = 23. User: 6 (SWT-37) +
// create_task, task_append_log and task_set_priority = 9. Read: 3, unchanged.
//
// SWT-42 (mail-attachments) criteria 21 and 22: mail_list_attachments and
// mail_read_attachment join BOTH profiles (owner decision O1). Full: 23 + 2 =
// 25. User: 9 + 2 = 11. Read: 3, unchanged — the fail-closed floor gains
// nothing. EXPECTED RED until schemas.go and userProfileTools gain both names.
//
// SWT-44 (user-profile-drafts): update_delivery becomes MCP-listed (still
// policy.humanOnly), so Full: 25 + 1 = 26. The user profile gains
// draft_delivery and update_delivery: User: 11 + 2 = 13. Read: 3, unchanged.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sspataro57/switchboard/internal/executor"
)

type nopExec struct{}

func (nopExec) Execute(context.Context, executor.Call) (executor.Result, error) {
	return executor.Result{}, nil
}

func TestServe_InitializeCarriesInstructionsAndProfileTools(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		tools   int
	}{
		// Literal counts (SWT-38 criterion 19; SWT-42 criteria 21/22): see the header.
		{ProfileFull, 26},
		{ProfileRead, 3},
		{ProfileUser, 13},
	} {
		t.Run(string(tc.profile), func(t *testing.T) {
			ctx := context.Background()
			ct, st := mcp.NewInMemoryTransports()
			ss, err := NewWithProfile(nopExec{}, "manual:test", tc.profile).sdkServer("ops-test").Connect(ctx, st, nil)
			if err != nil {
				t.Fatalf("server connect: %v", err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatalf("client connect: %v", err)
			}
			defer cs.Close()

			if got := cs.InitializeResult().Instructions; got != Instructions {
				t.Errorf("initialize instructions = %q, want mcpserver.Instructions — without them no session "+
					"learns the swb shorthand", got)
			}
			res, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			var names []string
			for _, tool := range res.Tools {
				names = append(names, tool.Name)
			}
			sort.Strings(names)
			if len(names) != tc.tools {
				t.Errorf("tools/list over the wire = %d tools (%s), want %d", len(names), strings.Join(names, ","), tc.tools)
			}
		})
	}
}
