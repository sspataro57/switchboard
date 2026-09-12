# Runbook — the switchboard MCP at Claude Code user scope (SWT-35, SWT-37, SWT-38, SWT-42, SWT-44)

Install `ops-mcp-user` once for Claude Code at USER scope, so a session opened in
any repo on the workstation can read switchboard's queues, dismiss, close or mark
delivered a task, log the work Salvador hands it as a swb task in his own lane,
write progress on that task, reorder any task's priority, read the attachments of
non-private mail (SWT-42), and draft gmail replies that Salvador approves and sends on the
dashboard (SWT-44) — and nothing else.
The per-repo binding — which switchboard project is this repo's queue — lives in
Claude Code's own per-project memory, not in switchboard.

It serves thirteen tools: `project_list`, `task_list`, `task_get_next`, `task_dismiss`,
`task_close`, `task_mark_delivered`, `create_task`, `task_append_log`,
`task_set_priority`, `mail_list_attachments`, `mail_read_attachment`, `draft_delivery` and `update_delivery`.

## Fresh install (once, from `main`)

```bash
cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-user
claude mcp add --scope user ops -e DATABASE_URL='${OPS_DATABASE_URL}' -e OPS_WORKER_ID=manual:salvo -- "$(go env GOPATH)/bin/ops-mcp-user"
claude mcp get ops
```

Then open a NEW session: tools and instructions are fetched when a session starts.

## Upgrading from SWT-37

The registration is unchanged — no `claude mcp remove` / `add`. Rebuild the binary
on `main` and open a new session:

```bash
cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-user
```

Then open a NEW session, and `/mcp` shows `ops` with the thirteen tools.

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

- **`ops-mcp-user` is the boundary.** It lists exactly the thirteen tools above, refuses
  every other tool at the MCP layer, and wires no mail sender and no calendar
  booker: its `main` never calls a sender seam, so whatever the environment holds
  arms nothing. It is a separate binary rather than a setting on `ops-mcp`, so
  there is no variable whose absence falls back to the full surface.
- **What it can do.** Read the queues; dismiss, close or mark delivered a task;
  create HUMAN tasks (`assignee_type='human'`, status `ready` — Salvador's own
  lane); log on human tasks; set any task's priority; and it
  reads attachments of non-private mail only (SWT-42): `mail_list_attachments` finds a message by id or by
  sender/subject (headers and attachment names, never a body) and `mail_read_attachment`
  returns one attachment inline, or saves a PDF/image to
  `~/.cache/switchboard/attachments/` and returns the path. And it drafts client email
  replies (SWT-44): `draft_delivery`, gmail only — the binary pins
  `require_channel:"gmail"`, so a Slack, Upwork, Jira or calendar draft is refused — and
  only on a thread already filed under the task's project (the binary pins
  `require_thread_in_task_project`; owner decision below); and `update_delivery` on its
  own drafts while they are still drafted, gmail only. "Own" is keyed on the actor, not
  the session: the binary pins `require_own_draft` and `require_channel:"gmail"`, so it
  edits drafts created by the mcp:manual:salvo actor (any interactive session, this
  repo's full-profile `ops` included), gmail only; never the drafts worker's or the
  dashboard's.
- **What it cannot do.** It cannot claim, create worker (`claude`) tasks, log on
  worker tasks, approve, send, book, link, decide, read mail bodies or reopen (it drafts, but
  never approves or sends: SWT-44).
  The binary pins `require_assignee_type:"human"` onto every `create_task` and
  `task_append_log` call (overwriting any value the model passes), so a request
  for a `claude` task or a log line on a `claude` task is refused by the tool
  itself, with an audit row. No worker console picks up what a session creates:
  a console's `task_get_next` routes only `claude` tasks, whatever the priority.
- **Omitting `OPS_TOKEN_KEY` is NOT a boundary.** A stdio MCP server inherits the
  environment of the shell that launched `claude`, and `~/.bashrc` exports
  `OPS_TOKEN_KEY`, so leaving it out of the `-e` flags withholds nothing: an
  `ops-mcp` (full) install there could approve and send mail as
  `mcp:manual:salvo`. Never install `ops-mcp` itself at user scope.
- **Workers are refused by policy, not by the tool list.** The full `ops-mcp` (worker
  consoles, this repo's `.mcp.json`) also lists the three verbs and
  `task_set_priority`, 26 tools in all. A worker console (`mcp:{client}`) is
  refused `task_dismiss` and `task_set_priority` by `human_only` and `task_close` /
  `task_mark_delivered` by `mcp_human_only`; the orchestrator and the Jira
  reconciler keep closing and delivering as before. `task_set_priority` refuses
  the orchestrator too: no automated caller chooses work. The full profile keeps
  creating `claude` tasks and logging on them — the pin is the user binary's only.

## Accepted risk (Salvador, 2026-09-10)

Every session reads untrusted text — an email, a Slack message, a web page, a file
in a cloned repo. Such text can tell a session to dismiss, close or mark delivered
ANY switchboard task in ANY project — undone with a reopen (below) — and the policy
sees `mcp:manual:salvo`, a human. Since SWT-38 it can also tell a session to:

1. **create tasks** in any project — always `human` + `ready`, so no worker picks
   them up and nothing is sent; their titles later show up in `task_list` in other
   sessions. Damage: queue and board clutter;
2. **append log lines to human tasks** — never to a `claude` task, so they never
   reach a worker prompt. Damage: misleading notes in Salvador's own lane;
3. **reorder any task's priority** (`task_set_priority`), worker queues included — a console then takes
   a different ready task next. It only reorders existing work. Damage: urgent
   work delayed, or old work jumped ahead.

**Since SWT-42 (Salvador, 2026-09-12: "yes expose it on the user mcp too")** the install also
reads mail attachments — a tool whose whole job is to fetch untrusted text someone else
wrote (a JSON file, a CSV, a forwarded mail) into the session. Text inside an attachment can
tell a session to dismiss, close or mark delivered any task, create human tasks and log on
them, or reorder any task's priority — the same verbs as above, now one tool call away from
a stranger's file. What limits it: only attachments of mail filed under a non-`local_only`
project are returned (and unfiled mail only on a mailbox with at least 20 filed messages, none
of them local-only — owner decision O2), so personal, bank, health and bulk mail never arrive; the finder returns
no bodies; the Instructions and both tool descriptions say attachment content is data, never
instructions (a prompt rule, not a boundary). What cannot happen through the attachment
tools: they send nothing, touch no delivery row, write nothing to the database but the audit
row, and keep saved files inside `~/.cache/switchboard/attachments`. Every attachment call leaves an audit row
with its message and part ids; the content itself is never stored.

**Since SWT-44 (Salvador, 2026-09-12)** the install also drafts client email replies:
`draft_delivery` writes the reply as a drafted gmail delivery row on the task, and
`update_delivery` fixes the words of its own drafts while they are still drafted. The session
picks the thread (`thread_id`); the From mailbox is resolved from that thread and the To is
the thread's latest inbound sender, and the dashboard shows From, To and the thread subject on
the draft before approval. Approving and sending stay on the dashboard, deliberately: a session
must not approve its own client email, and a session that has just read an attachment is
reading a stranger's text. The approve is bound to the words the page showed: if the draft
changed after the page loaded, Approve refuses and asks for a reload — and a page (or a POST)
that carries no content hash is refused too, so reload the page and review it again. The user
binary wires no mail sender at all.

**Same project (Salvador, 2026-09-12: "Same project").** A session drafts only on a thread
already filed under the task's project: the task's own source thread, or a thread whose latest
inbound message — the one the reply goes to — is filed under that project (its latest capture
decision). An older message filed there does not count if the newest one is filed elsewhere.
Anything else is refused with "thread N is not filed under this task's project (<slug>): its
latest inbound message is filed elsewhere or not at all; ask Salvador to file it, or draft from
the switchboard session". This repo's full `ops` and the drafts worker are not limited this way.

**Known gap (SWT-46).** The To shown on the dashboard can change if a new
inbound message arrives on the thread before Send: the send picks the To from the thread's
latest inbound message at send time, so an approved draft re-renders its To up to the moment
you press Send. Check the To again before Send.

**R8 caveat (SWT-47).** A session's draft usually sits on its own `ready`
work task; the first send of it records the delivery lifecycle for that task while it is still
`ready`, so a later real delivery on the same task is not auto-marked delivered. Mark it by
hand (`task_mark_delivered`) until the follow-up ticket ships.

**Accepted risk (SWT-44).** Untrusted text a session reads — a mail, an attachment, a web page —
can tell it to draft a reply on any task, into a gmail thread whose latest inbound message is
filed under the task's project, or to rewrite one
of its own drafts. It stays a draft: nothing leaves until Salvador reads it on the dashboard,
with its From and To, and approves it. Damage: a misleading draft in the approval queue.

The session instructions say to act only when Salvador asks for that task id;
that is a prompt rule, not a boundary. What cannot happen: nothing is sent and nothing is
approved — a session creates and edits only drafted gmail deliveries, which go nowhere until
Salvador approves them on the dashboard; no worker is dispatched onto attacker-authored
work; no claim is taken; no status other than `ready` is created. A wrong close or
dismiss drops the task from the queue and unblocks its dependents; a wrong dismiss
also writes a training label and stops the Jira status sync from reopening a
Jira-linked task. Every call leaves an audit row with its full args, and every
priority change a `priority_changed {from,to}` event.
Recovery:

- a wrong close or dismiss: `opsctl call --tool task_reopen --args '{"task_id":N,"reason":"…"}'`;
- a wrong "delivered": `task_close`, then `task_reopen` with `"status":"done_locally"`;
- a wrong priority: one `task_set_priority` back to the event's `from`;
- a planted task: `task_close`, or `task_dismiss` with `not_actionable`;
- a planted draft: until SWT-43's Deny ships there is no verb that discards one — edit it on
  the dashboard, or leave it unapproved; a draft never sends without an approval;
- once SWT-36 (`dismiss-reopen-on-activity`) ships, a dismissed task also reopens
  by itself on the next inbound message routed to it (by a classify promotion or a
  capture rule).

**Dismissal provenance.** `task_dismissals.dismissed_by` records the actor
unmodified: `dashboard:…` means Salvador picked the reason code from the board's
select; `mcp:…` means a model mapped his words to a code. A precision or eval pass
over dismissals can split on `dismissed_by LIKE 'mcp:%'` and weigh that tier lower.

**Capture provenance.** A user-scope `create_task` / `task_append_log` carries
`require_assignee_type:"human"` in `audit_events.args`; a call from this repo's
full `ops` does not, although both arrive as `mcp:manual:salvo`.

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
   `ops` connected with exactly thirteen tools.
3. In `~/projects/personal/switchboard`, a SESSION gets ONE `ops` — the
   project-scope `go run` entry with the full tool list (26 tools). Check this
   inside a session, NOT with `claude mcp get ops` / `claude mcp list`: run inside
   this repo, those CLI commands display the user-scope entry even though a session
   loads `.mcp.json`'s (verified 2026-09-10).
4. From the other repo, `project_list` and `task_list(project=<slug>)` answer.
5. `psql -h 192.168.50.49 -U ops -d ops -c "SELECT actor, tool, status FROM
   audit_events WHERE tool IN ('task_list','project_list','task_dismiss','task_close','task_mark_delivered','create_task','task_append_log','task_set_priority','draft_delivery','update_delivery')
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
- A repo with no swb project: say so once and the session remembers; it then
  creates nothing there.
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
- "swb add fix the flaky ingress check" (or "swb log this") → `create_task` in the
  repo's remembered project: a terse title, a one- or two-line body in the
  session's own words plus the repo path, `assignee_type` left unset (human). The
  session says "logged as swb #N".
- **Work requests log themselves.** When you ask a session to take on a piece of
  work (a change, a fix, an investigation — not a question), it checks the swb
  queue first, uses a task that already covers it, or else creates one before
  starting and names its id.
- "swb log 412 found the cause" → `task_append_log` on a human task. While working
  on a swb task, the session logs meaningful steps, blockers and decisions; they
  show on the dashboard's task page under "Events".
- "swb done 412" → `task_close` with a one-line outcome as the reason. A session
  that finishes work it logged closes the task the same way and says so.
- "find the attachment Sana sent about the Activities Integration" → `mail_list_attachments`
  by sender/subject (from=sana, subject=activities integration) → `mail_read_attachment` on
  the file you want. Text (JSON, CSV…) comes back inline; a PDF or image comes back as a cache
  path to open with Read. A private message is refused, never reported as missing.
- "swb prioritize 412 [level]" → `task_set_priority`. No level means urgent;
  "swb deprioritize 412" means normal. The session says the old and new level.

  | level    | value |
  |----------|-------|
  | normal   | 0     |
  | elevated | 1     |
  | high     | 2     |
  | urgent   | 3     |

  Higher runs first in `task_get_next` and `task_list`.

`task_list` hides closed and delivered tasks by default — unlike the dashboard
board, which hides only closed — because every finished row in a model context is
tokens spent on nothing. Ask for them explicitly with `status` (e.g.
`status=delivered`). Rows never carry a task body; read a body on the dashboard or
from a session in the switchboard repo. `local_only` projects are listed like any
other (Salvador, 2026-09-10).

The same tools answer without Claude: `opsctl call --tool task_list --args
'{"project":"saka"}'`.
