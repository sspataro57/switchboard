> Jira: SWT-4

# 04-mcp-task-tools — MCP task tools + worker wrapper + one real Claude loop

## Source

Build-order step 4, quoted from CLAUDE.md:

> 4. MCP task tools (task_get_next, claim, context, append_log,
>    request_feedback, mark_done_local, create_child) + wrapper + ONE Claude
>    loop against one real client. Manual task creation is fine here.

Constrained by the Workers section (execution workers = Claude Code headless
loops; wrapper ~150 lines: heartbeat(idle) → get_next → claim →
`claude -p "$(context)" --output-format json --dangerously-skip-permissions` →
capture session_id → done | park; resume via `claude --resume $SESSION_ID -p
"Feedback on #N: ..."`; loop rules baked into the worker prompt), the Manual
mode paragraph (same MCP via `.mcp.json`; `/task N` claims + dumps context),
the task status machine, invariant 3, and the 0001 schema (`tasks`,
`task_claims`, `task_events`, `feedback_requests`, `decisions` all exist).

## Goal

Ship the agent-facing task lifecycle as executor tools exposed over a stdio
MCP server (`cmd/ops-mcp`), plus a Go worker wrapper (`cmd/opsworker`) that
runs the CLAUDE.md loop — heartbeat, claim, spawn `claude -p`, capture
session_id, park or finish — and prove it by running one real task through a
real Claude Code session to `done_locally`.

**Usable alone means:** with `ops-mcp` in `.mcp.json`, any Claude Code session
(interactive or the wrapper's headless one) can walk a task through
ready → claimed → in_progress → needs_feedback ⇄ in_progress → done_locally
against the real `ops` db, with every mutation an executor-audited tool call,
heartbeats visible in `opsctl fleet`, and the session_id recorded so step 5's
resume machinery has something to resume. Tasks are created manually
(`opsctl create-task`) — triage is step 6, orchestration is step 5.

## Tool contract (normative — names are the CLAUDE.md build-order vocabulary)

All tools are executor tools (`internal/tools`), registered exactly like
`create_task`; `Executor.Execute` is the only route to every handler
(invariant 3). Two exposure tiers:

- **Agent-facing (listed by ops-mcp):** `task_get_next`, `task_claim`,
  `task_context`, `task_append_log`, `request_feedback`, `mark_done_local`,
  `create_child_task`, `record_decision`, plus the existing `create_task`.
- **Spine-facing (registered on the executor, callable via
  `opsctl call` / the wrapper in-process, NOT listed by ops-mcp):**
  `task_release`, `answer_feedback`. Agents must not park or answer their own
  questions; the registry stays the single gate either way.

Identity rule: `worker_id` is never model-chosen. ops-mcp reads
`OPS_WORKER_ID` from its environment (set by the wrapper when spawning
`claude`, or `manual:$USER` for interactive sessions) and force-overwrites any
`worker_id` field in tool args before calling the executor. The wrapper's own
in-process calls pass its worker_id directly.

### task_get_next — peek, read-only

Args `{client: string (required), subproject?: string}`. SELECT (no lock, no
write): tasks joined to projects, `projects.client = $client`,
`tasks.status = 'ready'`, `tasks.assignee_type = 'claude'`, optional
`tasks.subproject = $subproject`; `ORDER BY priority DESC, plan_order ASC
NULLS LAST, created_at ASC, id ASC LIMIT 1`. Returns
`{task: {id, project, subproject, title, priority} | null}`. Peek and claim
are separate on purpose (CLAUDE.md lists them separately): the losing racer's
claim fails cleanly and it re-peeks.

### task_claim

Args `{task_id, worker_id}`. One transaction:
1. `SELECT id FROM tasks WHERE id = $1 AND status = 'ready'
   FOR UPDATE SKIP LOCKED` — zero rows (already claimed, wrong status, or
   concurrently locked) → error, no writes. Same idiom as jobagent, applied
   by-id: a concurrent claimer never blocks, it fails fast.
2. `UPDATE tasks SET status = 'claimed', updated_at = now()`.
3. `INSERT task_claims (task_id, worker_id, expires_at = now() + ClaimTTL)`
   (ClaimTTL constant = 2h; expiry *enforcement* is step 5).
4. `INSERT task_events (event_type 'claimed', payload {worker_id, claim_id})`.

Returns `{claim_id, task_id, expires_at}`.

### task_context

Args `{task_id, worker_id?}`. Returns one JSON document: the task row;
its project (name, slug, client, repo_path, execution, delivery); ALL
project `decisions` ordered by created_at (CLAUDE.md: injected into every
task context); parent summary; children summaries; `task_dependencies`
(ids + titles + statuses); `feedback_requests` for the task (question,
answer, status); last 50 `task_events`.

Side effect (the claimed→in_progress transition, no extra tool invented):
when `worker_id` matches the task's active (unreleased) claim AND status is
`claimed` or `needs_feedback`, set status `in_progress` + `task_events`
row (`status_changed`). Fetching context IS the moment work (re)starts.
Without `worker_id`, or from a non-holder, it is a pure read.

### task_append_log

Args `{task_id, message, kind?: string = "log", worker_id?}`. INSERT
`task_events` (`event_type 'log'`, payload `{message, kind, worker_id}`).
Returns `{event_id}`. No status change. The wrapper also uses the same table
via a dedicated event: after every `claude -p` run it writes
`event_type 'session'`, payload `{session_id, is_error, num_turns?,
cost_usd?}` — the durable session pointer resume reads.

### request_feedback

Args `{task_id, worker_id, question}`. Transaction: verify active claim by
`worker_id`; INSERT `feedback_requests` (status `open`); UPDATE task
`in_progress → needs_feedback`; `task_events` row (`feedback_requested`,
payload `{feedback_request_id, question}`). Returns
`{feedback_request_id}`.

### mark_done_local

Args `{task_id, worker_id, summary?}`. Transaction: verify active claim by
`worker_id`; UPDATE task `in_progress → done_locally`; UPDATE the claim
`released_at = now()`; `task_events` row (`done_local`, payload
`{summary}`). Returns `{task_id, status}`.

### create_child_task

Args `{parent_task_id, title, body?, assignee_type? = "claude", priority?,
subproject?, worker_type?}`. Inherits `project_id` (and `subproject`, unless
overridden — the cross-boundary coordination case passes
`subproject: "main", worker_type: "coordination"`) from the parent; inserts
with `parent_id` set, status `ready`; `task_events` row on the PARENT
(`child_created`, payload `{child_task_id}`). Returns `{task_id}`.
Implementation shares `createTask`'s insert path — extend, don't duplicate.

### record_decision

Args `{project: slug, title, body?}` (+ injected `worker_id` →
`created_by = "worker:" + worker_id`). INSERT `decisions`. Returns
`{decision_id}`. Included now rather than step 5 because `task_context`
already injects decisions — a coordinator that can read them but not write
them is half a contract, and the handler is one INSERT.

### task_release (spine-facing)

Args `{task_id, worker_id, reason}`. Transaction: verify active claim;
release it (`released_at = now()`); status (`claimed`|`in_progress`) →
`ready`; `task_events` row (`released`, payload `{reason}`). Used by the
wrapper on subprocess failure / `is_error: true` — CLAUDE.md's retry shape is
"same task, never a new one".

### answer_feedback (spine-facing)

Args `{feedback_request_id, answer}`. UPDATE `feedback_requests` SET answer,
status `answered`, answered_at; `task_events` row (`feedback_answered`).
Task STAYS `needs_feedback` — the flip to `in_progress` happens at resume
time (status machine: needs_feedback ⇄ in_progress). Surfaced as
`opsctl answer-feedback --id N --answer "..." [--resume]` where `--resume`
additionally publishes the fleet `resume` command (below). Dashboard approve
UI is step 8/10; psql-as-workflow would dodge the executor, so this small
tool ships now.

## The wrapper (`cmd/opsworker`)

Go, not bash — CLAUDE.md says "~150 lines", not "~150 lines of bash", and the
two things the wrapper must own (a persistent MQTT connection carrying the
LWT, and the fleet payload contract) already live in `internal/fleet` as Go.
A bash wrapper would need a co-process just to hold the will.

Flags/env: `--client` (= worker_id for single console), `--subproject?`,
`--once` (single iteration, for smokes/tests), `MQTT_BROKER`, `DATABASE_URL`,
`CLAUDE_BIN` (default `claude`; tests point it at a stub).

Loop (each numbered action that mutates task state is an in-process
`executor.Execute` call — the wrapper is a client of the executor exactly
like opsctl, never a direct table writer):

1. Connect via `fleet.NewWorkerClient` (mandatory retained dead LWT);
   subscribe own `cmd` topic. Heartbeat ticker at `fleet.HeartbeatInterval`
   republishes current state for the whole process lifetime.
2. Publish `idle`. Poll `task_get_next(client[, subproject])`; empty → sleep
   (30s), repeat.
3. `task_claim` → on conflict error, re-peek (lost the race, normal).
4. `task_context(task_id, worker_id)` → flips to `in_progress`; render the
   JSON to the prompt (template in the wrapper) and publish
   `working` + task_id.
5. Spawn `claude -p "$PROMPT" --output-format json
   --dangerously-skip-permissions --append-system-prompt
   "$(cat prompts/worker-system.md)"` with `OPS_WORKER_ID` set and cwd =
   project `repo_path`, MCP config pointing at ops-mcp (the client repo's
   `.mcp.json`, or `--mcp-config` with a generated file — implementer's
   pick). Parse the result JSON; write the `session` task_event
   (session_id, is_error).
6. Read back task status: `done_locally` → publish `idle`, loop.
   `needs_feedback` → publish `needs_feedback` + task_id, park (stay
   connected, keep heartbeating, wait for cmd). Still `in_progress` with
   subprocess failure or `is_error` → `task_append_log` the error,
   `task_release(reason)`, publish `idle`, loop.
7. On `cmd` message `{action: "resume", args: {task_id,
   feedback_request_id?}}` (this pins step 3's open args schema for
   `resume`): look up the latest `session` event for the task,
   `task_context(task_id, worker_id)` (flips needs_feedback → in_progress),
   run `claude --resume $SESSION_ID -p "Feedback on #N: <answer>" ...` with
   the answered feedback text, then step 6 again. `pause` sets a flag that
   stops claiming after the current task; `dispatch` is logged and ignored
   (step 5's orchestrator defines it).

Worker prompt (`prompts/worker-system.md`) bakes in the loop rules verbatim
from CLAUDE.md: never choose your own work; you have exactly one task —
never claim another; never ask in the console — call
`request_feedback(task_id, question)` and end your turn; cross-boundary
questions → `create_child_task(..., subproject: "main",
worker_type: "coordination")`; git/gh in your worktree is free, but words on
any client-visible surface are FORBIDDEN this step (delivery tools arrive in
step 8 — until then there is no permitted channel, full stop); finish with
`mark_done_local`.

## Manual mode (thin slice)

- `.mcp.json` gains an `ops-mcp` stdio entry (command `go run ./cmd/ops-mcp`
  or a built binary; env `DATABASE_URL`, `OPS_WORKER_ID=manual:salvo`) next
  to the existing `atlassian` server.
- `.claude/commands/task.md` — `/task N`: call `task_claim` then
  `task_context` via the MCP tools and print the context. Finish with
  `mark_done_local`.
- The session hook publishing `state=manual` is deferred (Future work): a
  one-shot publish cannot carry an LWT and a stale retained `manual` is worse
  than absence; it needs the same persistent-connection treatment as the
  wrapper and earns its own slice.

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass; all new unit tests run with
   zero network and zero Postgres.
2. All ten tools are registered through `tools.Register` and reachable ONLY
   via `Executor.Execute`; every call (success, validation failure, policy
   denial, handler error) produces `audit_events` (+ `policy_decisions`)
   rows — no new audit code, the existing pipeline covers it. Grep-check: no
   exported handlers, no `tasks`/`task_claims`/`task_events`/
   `feedback_requests`/`decisions` writes outside `internal/tools`.
3. Unit tests cover every tool's `Validate` (missing/illegal args) and the
   MCP adapter with a fake executor: tools/list returns exactly the
   agent-facing set with JSON schemas; tools/call maps to
   `executor.Call{Tool, Actor, Args}`; a model-supplied `worker_id` is
   overwritten from `OPS_WORKER_ID`; spine-facing tools are absent from
   tools/list and rejected by name at the MCP layer.
4. Integration (build tag `integration`, skips without `DATABASE_URL`): full
   lifecycle in one test — create → get_next returns it → claim → context
   (status flips to `in_progress`, document contains project, decisions,
   events) → append_log → request_feedback (task `needs_feedback`, open
   feedback row) → answer_feedback (+row answered, task unchanged) → context
   as holder (flips back to `in_progress`) → mark_done_local (task
   `done_locally`, claim `released_at` set). Every transition has its
   `task_events` row. Rerunnable: test-owned slug, cleanup in FK order.
5. Integration, claim contention: two tasks, N≥8 goroutines racing
   get_next+claim — every task claimed exactly once, no goroutine blocks,
   losers get clean errors; direct double-claim of one id yields exactly one
   winner and one `task_claims` row.
6. Integration, ordering: get_next respects priority DESC, then plan_order
   ASC NULLS LAST, then created_at; filters by client and subproject;
   ignores `assignee_type='human'` and non-`ready` statuses.
7. MCP smoke (integration): spawn `ops-mcp` over stdio, complete
   `initialize`, `tools/list`, and a `tools/call` round-trip
   (`task_get_next`), and verify the corresponding `audit_events` row exists.
8. Wrapper against compose broker + db with a stub `CLAUDE_BIN` (a script
   emitting canned `--output-format json` result JSON): registers the LWT;
   publishes `idle` → `working`+task_id → outcome state; writes the
   `session` task_event with the stubbed session_id; on stub `is_error`,
   task returns to `ready` via `task_release` with the log appended; on stub
   calling request_feedback (or a pre-seeded `needs_feedback` state), parks
   in `needs_feedback` and a published `resume` cmd triggers
   `--resume <captured session_id>` (stub asserts argv) and the
   needs_feedback → in_progress flip.
9. Real smoke ("usable alone"): seed the real client project (psql INSERT
   into `projects`: slug `switchboard`, client `switchboard`, repo_path this
   repo, execution `manual`), `opsctl create-task` one trivial real
   housekeeping task (assignee claude), run `opsworker --client switchboard
   --once` against the REAL broker and REAL ops db with the real `claude`
   binary: task reaches `done_locally`, session_id captured, `opsctl fleet`
   showed idle→working→idle, audit trail complete, retained status cleared
   afterwards (production-broker hygiene).
10. Nothing else moves: no migration (`schema_migrations` max unchanged — the
    0001 tables carry everything), no `deliveries` writes, no orchestrator
    rules, no LISTEN/NOTIFY trigger, no dashboard.

## Data model changes

**None — no 0003.** Verified against `migrations/0001_initial.sql`: `tasks`
(status CHECK already includes every state this step touches, lines 114–134),
`task_claims` (worker_id, expires_at, released_at — lines 150–157),
`task_events` (event_type + JSONB payload, lines 142–148),
`feedback_requests` (lines 168–176), `decisions` (lines 207–214). The
session_id lives in a `task_events` payload (`event_type 'session'`), not a
new column — step 5 reads the latest one per task.

Event-type vocabulary written this step (payloads above): `claimed`,
`status_changed`, `log`, `session`, `feedback_requested`,
`feedback_answered`, `done_local`, `child_created`, `released`. Rows only —
the NOTIFY trigger on `task_events` is step 5.

## API / MCP tool changes

- Executor registry grows from 1 tool (`create_task`) to 11 (contract above).
  Policy stays `policy.NewStatic(reg.Names()...)` — every one of these is an
  internal task-state action on our own db, squarely "auto" in the policy
  matrix; nothing here is a client-visible channel. The matrix
  implementation itself is later work.
- `cmd/ops-mcp` (new): stdio MCP server. Thin adapter only — builds
  tools/list from a hardcoded agent-facing allowlist + per-tool JSON schemas,
  maps tools/call → `executor.Execute` with
  `Actor = "mcp:" + OPS_WORKER_ID`, returns handler output / error as the MCP
  result. No business logic, no SQL.
- `cmd/opsworker` (new): the loop above; executor client in-process,
  `Actor = "opsworker:" + worker_id`.
- `cmd/opsctl`: new `answer-feedback` subcommand (executor call +
  optional `fleet.PublishCommand` resume). Existing `create-task`/`call`/
  `fleet` untouched.

## MQTT topics

All shapes are step 3's frozen contract (`internal/fleet`); this step adds
the first real publisher/consumer, no new topics:

| topic                            | this step's use                                        | retained | QoS |
|----------------------------------|--------------------------------------------------------|----------|-----|
| `ops/workers/{client}/status`    | opsworker publishes idle/working/needs_feedback + LWT dead | yes  | 1   |
| `ops/workers/{client}/cmd`       | opsworker subscribes; `opsctl answer-feedback --resume` publishes `resume` | no | 1 |

This step pins the `resume` args schema:
`{"task_id": N, "feedback_request_id": M}` (step 3 left cmd args open).
`pause` = stop claiming after current task; `dispatch` stays unspecified
until step 5.

## Files likely to touch

Existing (verified in repo):

- `go.mod` — add the MCP SDK (see Decisions).
- `internal/tools/createtask.go` — `Register` grows; `createTask`'s insert
  gains an internal parent_id/worker_type path shared with
  `create_child_task`.
- `cmd/opsctl/main.go` — `answer-feedback` subcommand in the existing
  switch.
- `.mcp.json` — `ops-mcp` stdio entry.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — record: task_events event-type
  vocabulary, resume-cmd args schema, OPS_WORKER_ID injection rule,
  stub-CLAUDE_BIN test trick.

New:

- `internal/tools/` — one file per tool family, mirroring createtask.go's
  shape (parse/validate/handler closures over `*pgxpool.Pool`):
  `getnext.go`, `claim.go` (the SKIP-LOCKED tx), `context.go`,
  `appendlog.go`, `feedback.go` (request + answer), `donelocal.go`
  (+ release), `childtask.go`, `decision.go`; `*_test.go` per file;
  `integration_test.go` (criteria 4–6).
- `internal/mcpserver/` — SDK wiring + schema definitions + worker-id
  injection; unit tests with a fake executor (criterion 3). (Package name
  avoids clashing with the SDK's `mcp` import.)
- `cmd/ops-mcp/main.go` — pool + registry + executor + stdio serve.
- `internal/worker/loop.go` — the wrapper loop as a testable type (executor,
  fleet client, and process-runner behind small interfaces);
  `loop_test.go`; `integration_test.go` (criterion 8, gated on
  `DATABASE_URL` + `MQTT_BROKER`).
- `cmd/opsworker/main.go` — flags/env wiring, signal handling.
- `prompts/worker-system.md` — the loop rules.
- `.claude/commands/task.md` — `/task N` manual-mode command.
- `testdata` stub for `CLAUDE_BIN` (shell script emitting canned result
  JSON; lives next to the worker tests).

## In scope

- The ten executor tools (8 agent-facing incl. record_decision + 2
  spine-facing) with audit coverage via the existing pipeline.
- `cmd/ops-mcp` stdio server; `.mcp.json` entry; `/task` command file.
- `cmd/opsworker` + `internal/worker` loop, resume-cmd consumption,
  worker prompt.
- `opsctl answer-feedback`.
- One real end-to-end run against the seeded `switchboard` project.

## Out of scope (do not bundle)

- **Step 5 (orchestrator):** claim-expiry ENFORCEMENT (we only stamp
  `expires_at`), dependency gating in get_next (`blocked ⇄ ready` is the
  orchestrator's), done_locally → delivery-task rule, needs_feedback →
  feedback-task rule, orchestrator-published resume, the `task_events`
  NOTIFY trigger, cron templates, `dispatch` semantics.
- **Step 6 (triage):** any automatic task creation; `find_related_tasks`.
- **Step 8 (deliveries):** delivery tools, drafts, approve/send. The worker
  prompt forbids client-visible output entirely until then.
- **Step 10:** dashboard board, plan import.
- Multi-console coordination in practice (dotted worker ids work by
  construction via `internal/fleet`, but only ONE single-console loop ships).
- Manual-mode session hook (`state=manual` publisher) — Future work.
- `pr_open`/`awaiting_ci`/`awaiting_merge` transitions (step 9 wires CI
  events; no tool sets them yet).
- Deploy packaging; opsworker runs from the workstation.

## Invariants that apply

- **3. Everything through the executor** — the heart of this step. All ten
  handlers are unexported closures registered via `tools.Register`; ops-mcp
  and opsworker are both executor *clients*. Review checks: no tool SQL
  outside `internal/tools`; ops-mcp contains zero business logic; the
  wrapper never touches task tables directly (its only direct-DB read is
  none — even the post-run status read goes through `task_context`'s pure
  path or a registered read; fleet heartbeats are telemetry, not actions,
  exactly as scoped in step 3). No raw_sql/raw_api tool exists; the MCP
  allowlist is hardcoded, so a future spine-facing registration never leaks
  to agents by default.
- **2. One funnel** — no new tables, no queue tables: `task_get_next` is a
  filter over `tasks` (CLAUDE.md: "queues are filters, not tables").
  Coordination questions become `tasks` rows via `create_child_task`, not a
  side channel.
- **7. Orchestrator purity (discipline transfer)** — no orchestrator ships,
  and nothing here pre-empts it: tools record state and events; no tool
  decides what happens *next* (no auto-created follow-up tasks). The wrapper
  calls no LLM API — it execs the `claude` binary as a subprocess and parses
  its result envelope; provider adapters stay untouched.
- **6. Stealth attribution** — the worker prompt's repo actions land in
  client worktrees eventually; the prompt states the no-byline rule, and the
  smoke task's output (this repo) is checked for `Co-Authored-By` absence.
  Adapter-level enforcement is step 8/9 work.
- **4. Nothing external without a delivery row** — honored by absence: this
  step ships no sending capability and the prompt forbids substitutes.
- (1 and 5 have no surface: nothing is ingested, nothing is sent to
  re-enter.)

## Sibling patterns to copy

- **Claim idiom:** `~/GolandProjects/job-agent/internal/queue/queue.go` —
  `SELECT id ... FOR UPDATE SKIP LOCKED LIMIT 1` nested in the claiming
  UPDATE, `ErrEmpty` sentinel for "nothing to do", worker loops sleep on it.
  Ours differs deliberately: peek and claim are separate tools (CLAUDE.md
  vocabulary), claim is by-id, and status lives in the `tasks` CHECK — but
  the locking idiom and the fail-fast-never-block behavior transfer 1:1.
  Its `Reap` is the shape of step 5's claim expiry — do NOT port it now.
- **Tool registration/handler shape:** `internal/tools/createtask.go` in
  this repo — parse/validate/handle closures, NULLIF for optionals, slug →
  id resolution error shape.
- **Executor client wiring:** `cmd/opsctl/main.go` `run()` — pool → registry
  → `policy.NewStatic(reg.Names()...)` → `audit.NewPGStore` → `Execute`.
  ops-mcp and opsworker reuse this exact assembly.
- **Fleet usage:** `internal/fleet/client.go` (`NewWorkerClient` mandatory
  LWT, `PublishStatus` strict vocabulary, OnConnect resubscribe) and
  `cmd/fleetd/main.go` for the long-running daemon skeleton
  (signals, env wiring).
- **Integration hygiene:** `internal/executor/integration_test.go` +
  `internal/fleet/integration_test.go` — build tag + env-gate, test-owned
  slug/worker prefix, FK-ordered cleanup FIRST (the 2026-07-11 rerun
  landmine), retained-message cleanup on brokers.
- **Multi-statement tx in a handler:** `internal/connector/upworkcrm/`
  sink/ingest for pgx tx + wrapped-error style.

## Verification protocol

1. `go test ./...` — unit green, offline.
2. `make integration` — compose Postgres + Mosquitto; migrations apply (all
   no-ops past 0002); criteria 4–8 run against
   `localhost:5433` / `tcp://localhost:1884`.
3. MCP manual smoke: add the `.mcp.json` entry, open an interactive Claude
   Code session in this repo, `/mcp` shows ops-mcp connected, tools/list
   shows the agent-facing nine; `/task <id>` claims and dumps context; check
   `audit_events` via psql for each call.
4. Real smoke (criterion 9), real broker + real ops db:
   - `psql -h 192.168.50.49 -U ops -d ops` → INSERT the `switchboard`
     project row.
   - `opsctl create-task --project switchboard --assignee claude --title
     "<trivial housekeeping task>"`.
   - `MQTT_BROKER=tcp://192.168.50.45:1883 DATABASE_URL=$OPS_DATABASE_URL
     go run ./cmd/opsworker --client switchboard --once` (real `claude`).
   - Watch: `mosquitto_sub -h 192.168.50.45 -t 'ops/#' -v` and
     `opsctl fleet` (idle → working #N → idle); afterwards psql: task
     `done_locally`, claim released, `session` event with session_id,
     audit_events rows for every tool call.
   - Feedback leg: second trivial task whose body instructs asking a
     question → worker parks `needs_feedback` → `opsctl answer-feedback
     --id N --answer "..." --resume` → worker resumes with the captured
     session_id → `done_locally`.
   - **Cleanup (production broker):** `mosquitto_pub -h 192.168.50.45 -r -n
     -t ops/workers/switchboard/status`; smoke rows may stay (they're real
     ops-db history now) — that's the point of the step.
5. Criterion 10 checks: `SELECT max(version) FROM schema_migrations;`
   unchanged; `SELECT count(*) FROM deliveries;` = 0.
6. Commit via `/ticket-deliver` after review.

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **Tool names use the build-order forms** (`task_get_next`, `task_claim`,
  `task_context`, `task_append_log`, `request_feedback`, `mark_done_local`,
  `create_child_task`). The Workers prose says `get_next_task`; the build
  order is the tool-name list and one of the two had to win. `create_task`
  keeps its shipped name.
- **MCP SDK: `github.com/modelcontextprotocol/go-sdk` (official).** Stdio
  transport, typed tool registration with JSON schemas — exactly the three
  methods we need (initialize, tools/list, tools/call). Pin the current
  release at implementation time. Fallback if the SDK version fights the
  adapter shape: hand-rolled JSON-RPC over stdio is acceptable (the protocol
  surface used is tiny), but try the SDK first.
- **Wrapper in Go (`cmd/opsworker`), not bash.** The LWT demands a
  persistent connection; the fleet contract, claim calls, and result-JSON
  parsing are all Go already. A bash wrapper would shell out to a Go
  co-process for MQTT and to psql-avoiding CLI calls for everything else —
  more moving parts, same line count. CLAUDE.md's "~150 lines" names a size,
  not a language.
- **Wrapper calls the executor in-process; only the Claude subprocess goes
  through MCP.** Both routes end at `Executor.Execute` (invariant 3 is about
  the gate, not the transport); an MCP round-trip from the wrapper to its
  own child server would add a failure mode and nothing else.
- **claimed → in_progress rides on `task_context` from the claim holder**
  (and needs_feedback → in_progress at resume, same rule). The status
  machine has both states but the vocabulary has no `task_start`; fetching
  context as the holder is precisely "work started". Idempotent; pure read
  for everyone else.
- **`worker_id` injected from `OPS_WORKER_ID` at the MCP boundary.** A model
  choosing its identity could claim/complete as another worker; the wrapper
  sets the env, the adapter overwrites the arg. Env-based because stdio MCP
  servers are spawned per-session — the env IS the session identity.
- **`task_release` and `answer_feedback` are executor tools but not
  MCP-listed.** Parking is the wrapper's business (loop rule: never choose
  your own work — un-choosing is choosing); answering is Salvador's. Going
  through the executor anyway keeps the audit trail whole and honors
  "dashboard, MCP, internal — all through the executor".
- **`record_decision` included now** (build order defers it nowhere, Workers
  says coordinators use it, `task_context` already injects decisions, and
  it's one INSERT). Coordinator *worker* behavior stays step-5+.
- **Ordering: priority DESC** (higher number = more urgent), then
  `plan_order ASC NULLS LAST`, then FIFO. Nothing pins the direction;
  DESC keeps `0`-default tasks at the back so priority is opt-in.
- **get_next ignores `task_dependencies`.** Dependency gating is the
  step-5 orchestrator's `blocked ⇄ ready` job; duplicating it in SQL now
  creates two half-owners of one rule. Manual creation sets `ready`
  consciously.
- **ClaimTTL = 2h, stamped not enforced.** `claude -p` runs are
  long-tailed; 2h is generous without being infinite. Constant next to the
  claim code so step 5 inherits it.
- **session_id in `task_events` payload, not a `task_claims` column.** Avoids
  a migration, survives multiple sessions per task (retry after release gets
  a fresh session), and "latest session event" is exactly the resume lookup.
- **Failed run → `ready` on the SAME task** via `task_release` with logs
  appended (CLAUDE.md's red-CI shape generalized). No attempt counter this
  step — a permanently-failing task visibly ping-pongs in the fleet view,
  and retry budgets are an orchestrator policy (step 5).
- **Real client for the smoke = the `switchboard` project itself**
  (repo_path this repo, execution manual). It's a real project with a real
  repo and zero client-visible blast radius — the correct first loop.
  Projects are seeded via psql: project onboarding is operator
  configuration, not an agent action; a create_project tool can come when
  the dashboard needs it.
- **`pause` implemented minimally, `dispatch` stubbed.** Pause is a safety
  affordance worth having the day a loop runs unattended; dispatch has no
  producer until step 5 and guessing its semantics now would bind the
  orchestrator.

## Future work (not this SPEC)

- Step 5: claim-expiry enforcement (jobagent `Reap` shape), NOTIFY trigger
  on `task_events`, orchestrator-published resume/dispatch, dependency
  gating, done_locally → delivery task.
- Step 8: delivery tools joining the MCP allowlist; the worker prompt's
  "no permitted channel" line gets replaced with the delivery-tool rule.
- Manual-mode session hook publishing `state=manual` (persistent-connection
  helper).
- Retry budget / attempt counter on tasks once the orchestrator owns
  lifecycle policy.
- `create_project` tool + dashboard project onboarding.
- Worker deploy packaging (systemd/k8s) when a loop runs unattended.

---

No open questions arose — the ambiguities found (peek/claim split, the
in_progress transition, wrapper language, session_id storage, resume args)
were all resolvable from CLAUDE.md plus the shipped step 1–3 code and are
recorded under "Decisions made unilaterally". This SPEC is ready for
`test-author`.
