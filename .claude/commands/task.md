---
description: Manual mode — claim switchboard task N via the ops-mcp tools, dump its context, work it, finish with mark_done_local
---

Work switchboard task **$1** in manual mode, using the `ops` MCP server's tools
(they appear as mcp__ops__* — if absent, tell the user to check `/mcp`).

1. Call `task_claim` with `{"task_id": $1}`. If it fails because the task is
   not ready, report the error and stop — do not force it.
2. Call `task_context` with `{"task_id": $1}` and print a readable summary:
   title, body, project (slug, repo_path), decisions, open feedback, recent
   events.
3. Work the task following the worker rules in `prompts/worker-system.md`
   (never ask in-console when blocked — use `request_feedback`; log progress
   with `task_append_log`; no client-visible output; no AI attribution).
4. When complete and verified, call `mark_done_local` with a one-line summary.
