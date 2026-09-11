# Runbook — the switchboard MCP at Claude Code user scope (SWT-35)

Install the read-only `ops-mcp-read` once for Claude Code at USER scope, so a
session opened in any repo on the workstation can read switchboard's queues — and
do nothing else. The per-repo binding — which switchboard project is this repo's
queue — lives in Claude Code's own per-project memory, not in switchboard.

## Install (once, from `main`)

```bash
cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-read
claude mcp add --scope user ops -e DATABASE_URL='${OPS_DATABASE_URL}' -e OPS_WORKER_ID=manual:salvo -- "$(go env GOPATH)/bin/ops-mcp-read"
claude mcp get ops
```

- **`ops-mcp-read` is the boundary.** It lists exactly `project_list`, `task_list`
  and `task_get_next` — each writes nothing but its audit row — refuses every other
  tool at the MCP layer, and wires no mail sender and no calendar booker: its
  `main` never calls a sender seam, so whatever the environment holds arms nothing.
  A session in an unrelated repo reads untrusted content (mail, Slack, web pages);
  it can look at the queues and cannot create, claim, close, draft, approve or send.
  It is a separate binary rather than a setting on `ops-mcp`, so there is no
  variable whose absence falls back to the full surface.
- **Omitting `OPS_TOKEN_KEY` is NOT a boundary.** A stdio MCP server inherits the
  environment of the shell that launched `claude`, and `~/.bashrc` exports
  `OPS_TOKEN_KEY`, so leaving it out of the `-e` flags withholds nothing: an
  `ops-mcp` (full) install there could approve and send mail as
  `mcp:manual:salvo`. Never install `ops-mcp` itself at user scope.
- **A built binary, never `go run`.** A user-scope `go run` would compile whatever
  branch is checked out in the switchboard repo when a session starts, so a
  half-built ticket branch would serve every project's session.
  `go install ./cmd/ops-mcp-read` run on `main` makes the installed version
  deliberate.
- **Re-install rule:** re-run `go install ./cmd/ops-mcp-read` (on `main`) after any
  merge touching `cmd/ops-mcp-read`, `internal/mcpserver` or `internal/tools`. The
  registration itself does not change.
- **`OPS_WORKER_ID=manual:salvo` is required.** ops-mcp refuses to start without an
  identity ("identity is never model-chosen"); audit rows from these sessions carry
  actor `mcp:manual:salvo`.
- **`${OPS_DATABASE_URL}`** is expanded by Claude Code from the launching shell's
  environment — verified at the first install (2026-09-10): the entry stores the
  literal `${OPS_DATABASE_URL}` and `claude mcp get ops` reports it Connected, which
  needs the startup db ping to pass. If `/mcp` shows the server failing with
  `DATABASE_URL is not set` or a
  connection error, re-add it with the literal DSN (the same exposure as
  `~/.pgpass`).

## Precedence: this repo keeps its own `ops`

The server name is `ops` in both places. Claude Code resolves a same-name server
local → project → user, so inside the switchboard repo the project-scope entry in
`.mcp.json` (the live `go run` checkout, full profile) shadows the user-scope one,
and every other repo gets the installed `ops-mcp-read`. `.mcp.json` is unchanged.

## Verify

1. `claude mcp get ops` shows the user-scope entry, its `ops-mcp-read` command and
   both `-e` values.
2. In another repo (e.g. `cd ~/projects/personal/kube && claude`), `/mcp` shows
   `ops` connected with exactly three tools: `project_list`, `task_get_next`,
   `task_list`.
3. In `~/projects/personal/switchboard`, a SESSION gets ONE `ops` — the
   project-scope `go run` entry with the full tool list (19 tools at install).
   Check this inside a session (`/mcp`, or ask it to list its `mcp__ops__*` tools),
   NOT with `claude mcp get ops` / `claude mcp list`: run inside this repo, those
   CLI commands display the user-scope `ops-mcp-read` entry even though a session
   loads `.mcp.json`'s (verified 2026-09-10).
4. From the other repo, `project_list` and `task_list(project=<slug>)` answer, and
   each project's `in_play` equals the `total` of its `task_list`.
5. `psql -h 192.168.50.49 -U ops -d ops -c "SELECT actor, tool, status FROM
   audit_events WHERE tool IN ('task_list','project_list') ORDER BY id DESC LIMIT 5"`
   shows those calls with actor `mcp:manual:salvo`.

## Use

- **"swb" is the shorthand for switchboard.** The server's initialize instructions
  and both tool descriptions teach it, so every session with the `ops` server
  understands it (after a re-install, in a NEW session). Every trigger says "swb",
  so an unrelated "my queue" elsewhere is left alone.
- "list swb projects" → `project_list`: every slug with its client and its count
  of tasks in play. Confirm the slug here before memorising it.
- "remember this repo's switchboard project is `saka`" → Claude Code saves the slug
  to that repo's memory. Switchboard does nothing.
- "swb queue" (or "what's in my swb queue") → `task_list(project=<the remembered slug>)`;
  "swb queue saka" names the slug directly. Either way it returns the project's
  work in the order a worker would take it, as compact rows plus counts by status.

`task_list` hides closed and delivered tasks by default — unlike the dashboard
board, which hides only closed — because every finished row in a model context is
tokens spent on nothing. Ask for them explicitly with `status` (e.g.
`status=delivered`). Rows never carry a task body; read a body on the dashboard or
from a session in the switchboard repo. `local_only` projects are listed like any
other (Salvador, 2026-09-10).

The same tools answer without Claude: `opsctl call --tool task_list --args
'{"project":"saka"}'`.
