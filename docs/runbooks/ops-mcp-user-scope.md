# Runbook — the switchboard MCP at Claude Code user scope (SWT-35, SWT-37)

Install `ops-mcp-user` once for Claude Code at USER scope, so a session opened in
any repo on the workstation can read switchboard's queues and dismiss, close or
mark delivered a task — and nothing else. The per-repo binding — which switchboard
project is this repo's queue — lives in Claude Code's own per-project memory, not in
switchboard.

It serves six tools: `project_list`, `task_list`, `task_get_next`, `task_dismiss`,
`task_close` and `task_mark_delivered`.

## Fresh install (once, from `main`)

```bash
cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-user
claude mcp add --scope user ops -e DATABASE_URL='${OPS_DATABASE_URL}' -e OPS_WORKER_ID=manual:salvo -- "$(go env GOPATH)/bin/ops-mcp-user"
claude mcp get ops
```

Then open a NEW session: tools and instructions are fetched when a session starts.

## Migrating from `ops-mcp-read` (SWT-35's install)

SWT-37 renamed the binary. In this order — until the `add`, the old registration
still serves the old read-only binary (less capability, never more), and deleting
the old binary last makes a forgotten step fail loudly in `/mcp`:

```bash
cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-user
claude mcp remove --scope user ops
claude mcp add --scope user ops -e DATABASE_URL='${OPS_DATABASE_URL}' -e OPS_WORKER_ID=manual:salvo -- "$(go env GOPATH)/bin/ops-mcp-user"
rm -f "$(go env GOPATH)/bin/ops-mcp-read"
```

Then open a NEW session.

## What the install can and cannot do

- **`ops-mcp-user` is the boundary.** It lists exactly the six tools above, refuses
  every other tool at the MCP layer, and wires no mail sender and no calendar
  booker: its `main` never calls a sender seam, so whatever the environment holds
  arms nothing. It cannot create, claim, draft, approve, send, book, link, log,
  decide, read mail or reopen. It is a separate binary rather than a setting on
  `ops-mcp`, so there is no variable whose absence falls back to the full surface.
- **Omitting `OPS_TOKEN_KEY` is NOT a boundary.** A stdio MCP server inherits the
  environment of the shell that launched `claude`, and `~/.bashrc` exports
  `OPS_TOKEN_KEY`, so leaving it out of the `-e` flags withholds nothing: an
  `ops-mcp` (full) install there could approve and send mail as
  `mcp:manual:salvo`. Never install `ops-mcp` itself at user scope.
- **Workers are refused by policy, not by the tool list.** The full `ops-mcp` (worker
  consoles, this repo's `.mcp.json`) also lists the three verbs now, 22 tools in
  all. A worker console (`mcp:{client}`) is refused `task_dismiss` by `human_only`
  and `task_close` / `task_mark_delivered` by `mcp_human_only`; the orchestrator and
  the Jira reconciler keep closing and delivering as before.

## Accepted risk (Salvador, 2026-09-10)

Every session reads untrusted text — an email, a Slack message, a web page, a file
in a cloned repo. Such text can tell a session to dismiss, close or mark delivered
ANY switchboard task in ANY project — undone with a reopen (below) — and the policy
sees `mcp:manual:salvo`, a human. The session instructions say to act only when Salvador asks for that task id;
that is a prompt rule, not a boundary. What cannot happen: nothing is sent, and no
delivery is created or changed. A wrong close or dismiss drops the task from the
queue and unblocks its dependents; a wrong dismiss also writes a training label
and stops the Jira status sync from reopening a Jira-linked task.
Recovery:

- a wrong close or dismiss: `opsctl call --tool task_reopen --args '{"task_id":N,"reason":"…"}'`;
- a wrong "delivered": `task_close`, then `task_reopen` with `"status":"done_locally"`;
- once SWT-36 (`dismiss-reopen-on-activity`) ships, a dismissed task also reopens
  by itself on the next inbound message routed to it (by a classify promotion or a
  capture rule).

**Dismissal provenance.** `task_dismissals.dismissed_by` records the actor
unmodified: `dashboard:…` means Salvador picked the reason code from the board's
select; `mcp:…` means a model mapped his words to a code. A precision or eval pass
over dismissals can split on `dismissed_by LIKE 'mcp:%'` and weigh that tier lower.

## Notes

- **A built binary, never `go run`.** A user-scope `go run` would compile whatever
  branch is checked out in the switchboard repo when a session starts, so a
  half-built ticket branch would serve every project's session.
  `go install ./cmd/ops-mcp-user` run on `main` makes the installed version
  deliberate.
- **Re-install rule:** re-run `go install ./cmd/ops-mcp-user` (on `main`) after any
  merge touching `cmd/ops-mcp-user`, `internal/mcpserver`, `internal/tools` or
  `internal/policy`, then open a new session. The registration itself does not
  change.
- **`OPS_WORKER_ID=manual:salvo` is required.** The server refuses to start without
  an identity ("identity is never model-chosen"); audit rows from these sessions
  carry actor `mcp:manual:salvo`.
- **`${OPS_DATABASE_URL}`** is expanded by Claude Code from the launching shell's
  environment — verified at the first install (2026-09-10). If `/mcp` shows the
  server failing with `DATABASE_URL is not set` or a connection error, re-add it
  with the literal DSN (the same exposure as `~/.pgpass`).

## Precedence: this repo keeps its own `ops`

The server name is `ops` in both places. Claude Code resolves a same-name server
local → project → user, so inside the switchboard repo the project-scope entry in
`.mcp.json` (the live `go run` checkout, full profile) shadows the user-scope one,
and every other repo gets the installed `ops-mcp-user`. `.mcp.json` is unchanged.

## Verify

1. `claude mcp get ops` shows the user-scope entry, its `ops-mcp-user` command and
   both `-e` values.
2. In another repo (e.g. `cd ~/projects/personal/kube && claude`), `/mcp` shows
   `ops` connected with exactly six tools.
3. In `~/projects/personal/switchboard`, a SESSION gets ONE `ops` — the
   project-scope `go run` entry with the full tool list (22 tools). Check this
   inside a session, NOT with `claude mcp get ops` / `claude mcp list`: run inside
   this repo, those CLI commands display the user-scope entry even though a session
   loads `.mcp.json`'s (verified 2026-09-10).
4. From the other repo, `project_list` and `task_list(project=<slug>)` answer.
5. `psql -h 192.168.50.49 -U ops -d ops -c "SELECT actor, tool, status FROM
   audit_events WHERE tool IN ('task_list','project_list','task_dismiss','task_close','task_mark_delivered')
   ORDER BY id DESC LIMIT 5"` shows those calls with actor `mcp:manual:salvo`.

## Use

- **"swb" is the shorthand for switchboard.** The server's initialize instructions
  and the tool descriptions teach it, so every session with the `ops` server
  understands it (after a re-install, in a NEW session). Every trigger says "swb",
  so an unrelated "my queue" elsewhere is left alone.
- "list swb projects" → `project_list`: every slug with its client and its count
  of tasks in play. Confirm the slug here before memorising it.
- "remember this repo's switchboard project is `saka`" → Claude Code saves the slug
  to that repo's memory. Switchboard does nothing.
- "swb queue" (or "what's in my swb queue") → `task_list(project=<the remembered slug>)`;
  "swb queue saka" names the slug directly. Either way it returns the project's
  work in the order a worker would take it, as compact rows plus counts by status.
- "swb dismiss 412" → `task_dismiss`: the task should never have existed (someone
  else is handling it: `handled_elsewhere`). The session picks the reason code from
  what you said and names it.
- "swb close 412" → `task_close` with a short reason: the work is finished or no
  longer needed.
- "swb delivered 412" → `task_mark_delivered`: a `done_locally` task was delivered
  outside switchboard; its pending Deliver task is no longer drafted.

`task_list` hides closed and delivered tasks by default — unlike the dashboard
board, which hides only closed — because every finished row in a model context is
tokens spent on nothing. Ask for them explicitly with `status` (e.g.
`status=delivered`). Rows never carry a task body; read a body on the dashboard or
from a session in the switchboard repo. `local_only` projects are listed like any
other (Salvador, 2026-09-10).

The same tools answer without Claude: `opsctl call --tool task_list --args
'{"project":"saka"}'`.
