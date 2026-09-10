package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Serve runs s over stdio until the client disconnects. Every tools/call goes
// through CallTool, and so through the executor. Shared by ops-mcp (full) and
// ops-mcp-read (read), so the two binaries differ only in the profile they build.
func (s *Server) Serve(ctx context.Context, name string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.1.0"}, nil)
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
