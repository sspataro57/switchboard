package mcpserver

import "encoding/json"

// The agent-facing allowlist with JSON schemas — the MCP contract surface.
// worker_id is deliberately absent from every schema: it is injected from
// OPS_WORKER_ID, never model-supplied.

func schema(s string) json.RawMessage { return json.RawMessage(s) }

var agentTools = []Tool{
	{
		Name:        "create_task",
		Description: "Create a new task in a project (status ready).",
		InputSchema: schema(`{"type":"object","properties":{"project":{"type":"string","description":"project slug"},"title":{"type":"string"},"body":{"type":"string"},"assignee_type":{"type":"string","enum":["human","claude"]},"priority":{"type":"integer"},"subproject":{"type":"string"}},"required":["project","title"]}`),
	},
	{
		Name:        "task_get_next",
		Description: "Peek the highest-priority ready task for a client (read-only; does not claim).",
		InputSchema: schema(`{"type":"object","properties":{"client":{"type":"string"},"subproject":{"type":"string"}},"required":["client"]}`),
	},
	{
		Name:        "task_claim",
		Description: "Claim a ready task by id. Fails cleanly if already claimed.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"}},"required":["task_id"]}`),
	},
	{
		Name:        "task_context",
		Description: "Fetch the full context document for a task (task, project, decisions, parent/children, dependencies, feedback, recent events). As the claim holder this marks work started.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"}},"required":["task_id"]}`),
	},
	{
		Name:        "task_append_log",
		Description: "Append a progress log entry to a task.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"message":{"type":"string"},"kind":{"type":"string"}},"required":["task_id","message"]}`),
	},
	{
		Name:        "request_feedback",
		Description: "Ask Salvador a blocking question about your claimed task and end your turn. The task parks in needs_feedback until answered.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"question":{"type":"string"}},"required":["task_id","question"]}`),
	},
	{
		Name:        "mark_done_local",
		Description: "Mark your claimed task done locally and release the claim.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"summary":{"type":"string"}},"required":["task_id"]}`),
	},
	{
		Name:        "create_child_task",
		Description: "Create a child task under a parent (new discovered work; cross-boundary coordination uses subproject 'main' + worker_type 'coordination').",
		InputSchema: schema(`{"type":"object","properties":{"parent_task_id":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},"assignee_type":{"type":"string","enum":["human","claude"]},"priority":{"type":"integer"},"subproject":{"type":"string"},"worker_type":{"type":"string"}},"required":["parent_task_id","title"]}`),
	},
	{
		Name:        "record_decision",
		Description: "Record a project-scoped decision; it is injected into every future task context for the project.",
		InputSchema: schema(`{"type":"object","properties":{"project":{"type":"string","description":"project slug"},"title":{"type":"string"},"body":{"type":"string"}},"required":["project","title"]}`),
	},
}

var agentToolNames = func() map[string]bool {
	m := make(map[string]bool, len(agentTools))
	for _, t := range agentTools {
		m[t.Name] = true
	}
	return m
}()
