package mcpserver

// The wiring behind the "swb" shorthand: what a real MCP client receives at
// initialize, for BOTH profiles (ops-mcp and ops-mcp-read share this path).
// TestInstructions_TeachTheSwbShorthand checks the text of the const; this
// checks the const is what goes over the wire — a nil ServerOptions would pass
// the text test and silently drop the shorthand from every session.

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
		{ProfileFull, len(agentTools)},
		{ProfileRead, len(readProfileTools)},
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
