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

// Profile selects which slice of the agent-facing allowlist a Server serves.
// Each binary fixes its own — there is no environment setting, so nothing can
// fall back from one to the other.
type Profile string

const (
	// ProfileFull serves the whole agentTools allowlist: cmd/ops-mcp (worker
	// consoles and this repo's .mcp.json).
	ProfileFull Profile = "full"
	// ProfileUser serves the queue reads plus task_dismiss, task_close and
	// task_mark_delivered: cmd/ops-mcp-user, the user-scope install every other
	// repo's session sees (SWT-37 V3; Salvador's decision of 2026-09-10). It
	// can look at the queues and dismiss, close or mark delivered — nothing that
	// creates, claims, drafts, approves, sends, books, links, logs, decides,
	// reads mail or reopens. Policy refuses the three verbs to worker identities.
	ProfileUser Profile = "user"
	// ProfileRead serves the queue reads only. No binary builds it since
	// SWT-37; it is the named fail-closed floor an unknown profile lands on.
	ProfileRead Profile = "read"
)

// readProfileTools write nothing but their audit row. task_context is left out
// deliberately: fetched by the claim holder it flips claimed → in_progress.
var readProfileTools = []string{"project_list", "task_list", "task_get_next"}

// userProfileTools is the read slice plus the three task verbs (SWT-37 V3).
var userProfileTools = append(append([]string(nil), readProfileTools...),
	"task_dismiss", "task_close", "task_mark_delivered")

// Server adapts MCP tool calls onto the executor for one worker identity.
type Server struct {
	ex       Executor
	workerID string
	tools    []Tool
	allowed  map[string]bool
}

// New builds the full-profile adapter. workerID comes from OPS_WORKER_ID —
// identity is never model-chosen.
func New(ex Executor, workerID string) *Server {
	return NewWithProfile(ex, workerID, ProfileFull)
}

// NewWithProfile builds the adapter serving profile p. A narrow profile's tools
// are taken FROM agentTools, so their schemas cannot drift. Anything but
// ProfileFull and ProfileUser gets the read slice: fail closed to the smallest.
func NewWithProfile(ex Executor, workerID string, p Profile) *Server {
	s := &Server{ex: ex, workerID: workerID, allowed: map[string]bool{}}
	keep := agentToolNames
	if p != ProfileFull {
		names := readProfileTools
		if p == ProfileUser {
			names = userProfileTools
		}
		keep = map[string]bool{}
		for _, n := range names {
			keep[n] = true
		}
	}
	for _, t := range agentTools {
		if keep[t.Name] {
			s.tools = append(s.tools, t)
			s.allowed[t.Name] = true
		}
	}
	return s
}

// ListTools returns exactly this profile's slice of the agent-facing allowlist.
// Spine-facing tools (task_release, answer_feedback) are registered on the
// executor but never listed or callable here.
func (s *Server) ListTools() []Tool {
	out := make([]Tool, len(s.tools))
	copy(out, s.tools)
	return out
}

// CallTool maps one MCP tools/call onto the executor. A model-supplied
// worker_id is force-overwritten from the server's identity.
func (s *Server) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if !s.allowed[name] {
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
