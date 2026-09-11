package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Instructions is sent at initialize; Claude Code puts it in the session's
// system prompt. "swb" is Salvador's shorthand for switchboard, so "list swb
// projects" and "swb queue" resolve to the right tool without the long name.
const Instructions = `This is switchboard ("swb"), Salvador's task system. "swb" always means switchboard.
- "list swb projects" / "swb projects" → call project_list.
- "swb queue" / "what's in my swb queue" / "my queue" → call task_list with this repo's memorised switchboard project slug; if none is memorised, call project_list and ask which slug is this repo's.
- "swb queue <slug>" → call task_list with project=<slug>.`

// Serve runs s over stdio until the client disconnects. Every tools/call goes
// through CallTool, and so through the executor. Shared by ops-mcp (full) and
// ops-mcp-read (read), so the two binaries differ only in the profile they build.
func (s *Server) Serve(ctx context.Context, name string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.1.0"}, &mcp.ServerOptions{Instructions: Instructions})
	for _, t := range s.ListTools() {
		srv.AddTool(
			&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				out, err := s.CallTool(ctx, req.Params.Name, req.Params.Arguments)
				if err != nil {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
					}, nil
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
				}, nil
			})
	}
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
