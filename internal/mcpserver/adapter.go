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
	// ProfileUser serves the queue reads, task_dismiss, task_close and
	// task_mark_delivered (SWT-37 V3), plus create_task, task_append_log and
	// task_set_priority (SWT-38): cmd/ops-mcp-user, the user-scope install every
	// other repo's session sees (Salvador's decisions of 2026-09-10). It can
	// look at the queues, dismiss, close or mark delivered, create HUMAN tasks,
	// log on human tasks and set priority. It cannot claim, create or log on
	// worker (claude) tasks — the profile pins below refuse both — draft,
	// approve, send, book, link, decide, read mail or reopen. Policy refuses
	// the verbs and task_set_priority to worker identities.
	ProfileUser Profile = "user"
	// ProfileRead serves the queue reads only. No binary builds it since
	// SWT-37; it is the named fail-closed floor an unknown profile lands on.
	ProfileRead Profile = "read"
)

// readProfileTools write nothing but their audit row. task_context is left out
// deliberately: fetched by the claim holder it flips claimed → in_progress.
var readProfileTools = []string{"project_list", "task_list", "task_get_next"}

// userProfileTools is the read slice plus the three task verbs (SWT-37 V3)
// and the three capture tools (SWT-38 C3/C5).
var userProfileTools = append(append([]string(nil), readProfileTools...),
	"task_dismiss", "task_close", "task_mark_delivered",
	"create_task", "task_append_log", "task_set_priority")

// userProfilePins (SWT-38 C4) are args the user profile force-sets on a call,
// by OVERWRITE, after injectWorkerID. require_assignee_type:"human" makes the
// create_task validator refuse a claude task and the task_append_log handler
// refuse a line on a claude task: work from another repo's session stays in
// Salvador's lane, which no worker console routes. The enforcement lives in the
// validator and handler (inside the executor path); this only injects. It
// also marks a user-scope call in audit_events.args.
var userProfilePins = map[string]map[string]string{
	"create_task":     {"require_assignee_type": "human"},
	"task_append_log": {"require_assignee_type": "human"},
}

// Server adapts MCP tool calls onto the executor for one worker identity.
type Server struct {
	ex       Executor
	workerID string
	tools    []Tool
	allowed  map[string]bool
	// pins is fixed by NewWithProfile from the profile alone — no environment
	// input. ProfileFull and ProfileRead have none.
	pins map[string]map[string]string
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
			s.pins = userProfilePins
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
// worker_id is force-overwritten from the server's identity, and then the
// profile's pins (SWT-38 C4) are force-overwritten the same way — last, so no
// earlier injection can undo them.
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
	if pins := s.pins[name]; len(pins) > 0 {
		if injected, err = overwriteArgs(injected, pins); err != nil {
			return nil, fmt.Errorf("prepare args for %s: %w", name, err)
		}
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
	return overwriteArgs(args, map[string]string{"worker_id": workerID})
}

// overwriteArgs sets each string field in set on the args object, replacing
// any value the model supplied.
func overwriteArgs(args json.RawMessage, set map[string]string) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return nil, fmt.Errorf("args are not a JSON object: %w", err)
		}
	}
	for k, v := range set {
		quoted, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", k, err)
		}
		m[k] = quoted
	}
	return json.Marshal(m)
}
