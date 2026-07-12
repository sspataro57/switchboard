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
		Name:        "draft_delivery",
		Description: "Draft an outbound client communication as a delivery row (drafted; goes through approval before any send). THE only route for client-visible words.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"channel":{"type":"string","enum":["gmail","upwork_chat","jira_comment"]},"body":{"type":"string"},"subject":{"type":"string"},"thread_id":{"type":"integer","description":"required for gmail; From is resolved from the thread, never chosen"},"target_ref":{"type":"string","description":"required for upwork_chat: the thread_key"}},"required":["task_id","channel","body"]}`),
	},
	{
		Name:        "link_external_ref",
		Description: "Link a task to an external system object (jira issue, github PR, upwork thread). Idempotent.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"system":{"type":"string","enum":["jira","github","upwork_crm"]},"external_key":{"type":"string"},"external_url":{"type":"string"}},"required":["task_id","system","external_key"]}`),
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
