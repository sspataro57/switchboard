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
		Name:        "mark_delivery_sent",
		Description: "Resolve a Slack reply that switchboard already tried to send and left in 'sending' (the click may or may not have landed): confirm the message IS in the channel. Records an outcome; it never sends anything. Over MCP this is the ONLY transition allowed — recording an approved or still-drafted delivery as sent must go through the dashboard or `opsctl call`, because it would assert a send that switchboard never made. Human-only: a worker identity is refused by policy even though this tool is listed.",
		InputSchema: schema(`{"type":"object","properties":{"delivery_id":{"type":"integer"}},"required":["delivery_id"]}`),
	},
	{
		Name:        "draft_delivery",
		Description: "Draft an outbound client communication as a delivery row (drafted; goes through approval before any send). THE only route for client-visible words. For channel \"calendar\" (an own calendar block): target_ref is the account email, subject becomes the event summary, body the description, and start/end MUST come from propose_slots.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"channel":{"type":"string","enum":["gmail","upwork_chat","jira_comment","slack_reply","calendar"]},"body":{"type":"string"},"subject":{"type":"string"},"thread_id":{"type":"integer","description":"required for gmail; From is resolved from the thread, never chosen"},"target_ref":{"type":"string","description":"required for upwork_chat, jira_comment, slack_reply, and calendar; Slack uses the exact conversation or thread URL, calendar the account email"},"start":{"type":"string","description":"RFC3339; calendar only — block start, from propose_slots"},"end":{"type":"string","description":"RFC3339; calendar only — block end, at most 12h after start"}},"required":["task_id","channel","body"]}`),
	},
	{
		Name:        "link_external_ref",
		Description: "Link a task to an external system object (jira issue, github PR, upwork thread). Idempotent.",
		InputSchema: schema(`{"type":"object","properties":{"task_id":{"type":"integer"},"system":{"type":"string","enum":["jira","github","upwork_crm"]},"external_key":{"type":"string"},"external_url":{"type":"string"}},"required":["task_id","system","external_key"]}`),
	},
	{
		// SWT-35: the queue read for Salvador's per-repo sessions (the slug
		// comes from Claude Code's own per-project memory) and for workers.
		Name: "task_list",
		Description: "List one project's task queue, read-only: it does not claim — a worker takes work only via task_get_next. " +
			"project is the caller's project slug (confirm it with project_list). Returns compact rows in task_get_next " +
			"order plus counts by status. Closed and delivered tasks are hidden by default; ask for them with status.",
		InputSchema: schema(`{"type":"object","properties":{"project":{"type":"string","description":"project slug (see project_list)"},"status":{"type":"string","enum":["holding","ready","claimed","in_progress","needs_feedback","pr_open","awaiting_ci","awaiting_merge","done_locally","delivered","closed","blocked"],"description":"one status; default: everything except closed and delivered"},"assignee_type":{"type":"string","enum":["human","claude"]},"subproject":{"type":"string"},"limit":{"type":"integer","description":"default 25, max 200; counts always cover the full set"}},"required":["project"]}`),
	},
	{
		Name: "project_list",
		Description: "List every switchboard project slug, read-only, with its client and count of tasks in play. " +
			"Use it to confirm a slug before memorising it as this repo's queue.",
		InputSchema: schema(`{"type":"object"}`),
	},
	{
		Name:        "mail_search",
		Description: "Search ingested mail (subject, sender, body) served from switchboard's normalized store, NOT from a live mailbox — you see only what ingestion has captured, so a result set is bounded by the backfill window rather than by the mailbox. At least one of query/from/thread_key is required.",
		InputSchema: schema(`{"type":"object","properties":{"query":{"type":"string","description":"case-insensitive substring over subject, sender and body"},"from":{"type":"string"},"thread_key":{"type":"string"},"since":{"type":"string","description":"RFC3339 or a Postgres timestamp"},"until":{"type":"string"},"direction":{"type":"string","enum":["inbound","outbound"]},"limit":{"type":"integer","description":"default 20, max 50"}}}`),
	},
	{
		Name:        "mail_read_thread",
		Description: "Read one ingested mail thread in order, oldest first. Served from the normalized store, not a live mailbox. Bodies are capped; give thread_id or thread_key.",
		InputSchema: schema(`{"type":"object","properties":{"thread_id":{"type":"integer"},"thread_key":{"type":"string"},"limit":{"type":"integer","description":"max 50 messages"}}}`),
	},
	{
		// SWT-28 (Q1 = b): agent-facing where send_delivery is not, because the
		// auto tier's whole point is a worker booking with no human in the
		// loop. What an INJECTED call can do: put a block on Salvador's OWN
		// calendar, at a time provably free (the LoadBusy conflict/freshness/
		// horizon refusal), on an account a human explicitly write-enabled
		// (calendar_write_enabled, re-checked at send), at most ten per hour,
		// every call audited, stoppable with set_sending_frozen. What it
		// cannot do: choose a time, a calendar, a summary or an attendee — all
		// fields come from the drafted row, and the write envelope has no
		// attendees field at all.
		Name:        "book_calendar_block",
		Description: "Approve and book a DRAFTED calendar delivery (an own calendar block) in one audited call — the calendar channel's auto tier. Refuses any non-calendar delivery (channel_mismatch), a conflicted or stale-calendar slot (the propose_slots fail-closed rules, re-checked at send), a write-disabled account, and anything while the kill switch (set_sending_frozen) is on. Rate-limited per hour. The block's time, calendar and summary all come from the drafted row.",
		InputSchema: schema(`{"type":"object","properties":{"delivery_id":{"type":"integer"}},"required":["delivery_id"]}`),
	},
	{
		Name:        "approve_delivery",
		Description: "Approve a drafted delivery so it becomes sendable. HUMAN IDENTITIES ONLY — an autonomous worker identity is denied by policy. Approving does not send; send_delivery is a separate call.",
		InputSchema: schema(`{"type":"object","properties":{"delivery_id":{"type":"integer"}},"required":["delivery_id"]}`),
	},
	{
		Name:        "send_delivery",
		Description: "Send an APPROVED delivery to the client-visible surface. HUMAN IDENTITIES ONLY — an autonomous worker identity is denied by policy. There is no compose-and-send: draft, approve and send are three separate calls.",
		InputSchema: schema(`{"type":"object","properties":{"delivery_id":{"type":"integer"}},"required":["delivery_id"]}`),
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
