// Package mcpserver is the thin MCP adapter over the executor (SPEC
// 04-mcp-task-tools): tools/list serves a hardcoded agent-facing allowlist
// with JSON schemas; tools/call maps to executor.Execute with
// Actor = "mcp:" + OPS_WORKER_ID. No business logic, no SQL — the executor
// pipeline is the gate (invariant 3).
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sspataro57/switchboard/internal/executor"
)

// Executor is the one dependency — satisfied by *executor.Executor.
type Executor interface {
	Execute(ctx context.Context, call executor.Call) (executor.Result, error)
}

// Tool is one tools/list entry.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Server adapts MCP tool calls onto the executor for one worker identity.
type Server struct {
	ex       Executor
	workerID string
}

// New builds the adapter. workerID comes from OPS_WORKER_ID — identity is
// never model-chosen.
func New(ex Executor, workerID string) *Server {
	return &Server{ex: ex, workerID: workerID}
}

// ListTools returns exactly the agent-facing allowlist. Spine-facing tools
// (task_release, answer_feedback) are registered on the executor but never
// listed or callable here.
func (s *Server) ListTools() []Tool {
	out := make([]Tool, len(agentTools))
	copy(out, agentTools)
	return out
}

// CallTool maps one MCP tools/call onto the executor. A model-supplied
// worker_id is force-overwritten from the server's identity.
func (s *Server) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if !agentToolNames[name] {
		return nil, fmt.Errorf("tool %q is not available over MCP", name)
	}
	if name == "task_append_log" {
		if err := rejectSessionKind(args); err != nil {
			return nil, err
		}
	}
	if name == "create_task" {
		if err := rejectParentID(args); err != nil {
			return nil, err
		}
	}

	injected, err := injectWorkerID(args, s.workerID)
	if err != nil {
		return nil, fmt.Errorf("prepare args for %s: %w", name, err)
	}

	res, err := s.ex.Execute(ctx, executor.Call{
		Tool:  name,
		Actor: "mcp:" + s.workerID,
		Args:  injected,
	})
	if err != nil {
		return nil, err
	}
	return res.Output, nil
}

// rejectSessionKind reserves the 'session' event tag for the wrapper's
// in-process calls: a model must not be able to forge the resume pointer.
func rejectSessionKind(args json.RawMessage) error {
	var a struct {
		Kind string `json:"kind"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	if a.Kind == "session" {
		return fmt.Errorf(`kind "session" is reserved for the worker wrapper`)
	}
	return nil
}

// rejectParentID reserves create_task's parent_id for the spine (orchestrator
// lifecycle tasks): agents link tasks via create_child_task, which inherits
// the project from the parent instead of trusting an arbitrary pair.
func rejectParentID(args json.RawMessage) error {
	var a struct {
		ParentID *int64 `json:"parent_id"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	if a.ParentID != nil {
		return fmt.Errorf("parent_id is reserved for the spine; use create_child_task")
	}
	return nil
}

// injectWorkerID overwrites (or sets) the worker_id field in the args object.
func injectWorkerID(args json.RawMessage, workerID string) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return nil, fmt.Errorf("args are not a JSON object: %w", err)
		}
	}
	quoted, err := json.Marshal(workerID)
	if err != nil {
		return nil, fmt.Errorf("marshal worker id: %w", err)
	}
	m["worker_id"] = quoted
	return json.Marshal(m)
}
