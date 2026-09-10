> Jira: SWT-35

# task-list-mcp — read a project's queue over MCP (`task_list` + `project_list`), installed once at user scope

All three open questions were answered on 2026-09-10 and are folded in below.
`docs/tickets/task-list-mcp_OPEN_QUESTIONS.md` keeps each answer with its rationale.
Answers are marked **[Q1]**, **[Q2]** and **[Q3]** where they land.

## Source

Ad-hoc, not a build-order step. Salvador, verbatim, 2026-09-10:

> "what about switchboard mcp to query the q per project?"

Clarified the same day, verbatim:

> "the idea is I would install mcp in claude then on each project tell claude to save
> in memory which q is it"

Scope confirmed the same day, verbatim:

> "yep project_list is ok also a way to pull q for a particular project"

Q1 and Q2 answered the same day, verbatim:

> "privacy is not important. filter so no junk and wasted tockens filtering"

Q3 answered the same day, verbatim, to "task_list only returns task titles, status
and priority, never message bodies. What should task_list do for local-only
projects?":

> "List them anyway"

The **primary consumer is Salvador's own interactive Claude Code session in each
project repo**. Worker consoles get the same tools through the one agent-facing
allowlist. **"No junk and wasted tokens" is a design driver for the whole response
shape**: every field, every default and every status in the default set is judged
by whether a "what's in my queue" answer needs it (L15).

## Goal

Add two read-only executor tools, MCP-listed:
- `project_list`: a compact directory of project slugs;
- `task_list(project, …)`: one project's work still in play, in `task_get_next`
  order, with per-status counts.

Also make `cmd/ops-mcp` installable once at Claude Code USER scope, so any repo's
session can answer "what's in my queue" for the slug it has memorised.

**Usable alone means:** after one `go install` and one `claude mcp add --scope user`
(runbook below), a Claude Code session opened in ANY repo on the workstation (for
example `~/projects/personal/kube`) shows the `ops` server connected.
- Asking it "list the switchboard projects" returns every slug, with its client and
  its count of tasks in play.
- Telling it "remember this repo's switchboard project is `saka`" saves the slug to
  that repo's memory. Claude Code does this; switchboard does nothing.
- From then on, "what's in my queue" returns saka's in-play tasks in the order a
  worker would take them, as compact rows plus "N ready, M blocked…" counts.
  - Closed and delivered work is not in the answer unless asked for by `status`.
  - This works the same for every project, whatever its `ai_locality` **[Q3]**.
- Every call leaves one `audit_events` row, and nothing else in the database
  changes.
- An unknown slug is refused by name, not answered with an empty list.
- The same two tools answer `opsctl call --tool task_list …` with no opsctl change.

No migration, no new table, no dashboard change, no deploy.

## What the investigation established

1. **`task_get_next` filters by CLIENT, not project**, and accepts any client the
   caller names (`internal/tools/getnext.go:48-56`).
   - The filter: `p.client = $1`, `status='ready'`, `assignee_type='claude'`,
     optional subproject.
   - The order:
     `ORDER BY t.priority DESC, t.plan_order ASC NULLS LAST, t.created_at ASC, t.id ASC`.
   - The ordering is pinned by `internal/tools/getnext_ordering_integration_test.go`.
2. **The board hides only `closed`, not `delivered`** (`internal/dashboard/board.go:69-73`:
   `t.status <> 'closed'` when no `?status=`).
   - Nothing moves `delivered → closed` except a `task_close` call:
     `close.go:32` `openStatuses` includes `delivered`, and R8 closes only the
     *Deliver* task (`delivery.go:902-916`).
   - So delivered work sits on the board until someone closes it by hand.
   - `task_list` deliberately diverges here **[Q2]** (L4).
3. **The real actor shapes over MCP.**
   - ops-mcp sets `Actor = "mcp:" + OPS_WORKER_ID` (`internal/mcpserver/adapter.go:73`).
   - The worker wrapper sets `OPS_WORKER_ID` to the **bare client**: `loop.go:401`,
     `cmd/opsworker/main.go:91` → `worker.WriteMCPConfig(..., client)`. So a worker
     console's actor is `mcp:acme` or `mcp:acme.main`. It is **not**
     `mcp:worker:acme`, a string that appears only in tests.
   - Manual sessions use `mcp:manual:salvo` (`.mcp.json:22`).
   - opsctl uses `opsctl:$USER`.
4. **What ops-mcp needs to start.**
   - `OPS_WORKER_ID` is REQUIRED; startup fails with "identity is never
     model-chosen" (`cmd/ops-mcp/main.go:38-41`). A manual install therefore sets
     it to `manual:salvo`; it cannot be left unset.
   - `DATABASE_URL` is required, and the pool pings at startup
     (`internal/store/pg.go:14-31`).
   - `OPS_TOKEN_KEY` is OPTIONAL. Without it, and without `GMAIL_CONNECTOR_BRIDGE`,
     no mail sender is wired (`internal/connector/google/wire.go:27,46`).
   - **Amended after review (2026-09-10): omitting a variable from the install does
     not unset it.** A stdio MCP server inherits the environment of the shell that
     launched `claude`: this repo's own ops-mcp processes carry `OPS_TOKEN_KEY`,
     `JIRA_TOKEN_PERSONAL` and `OPS_DATABASE_URL` in `/proc/<pid>/environ`, none of
     which `.mcp.json` sets, and `~/.bashrc` exports `OPS_TOKEN_KEY`.
5. **Today's launch cannot work at user scope.** `.mcp.json` runs `go run ./cmd/ops-mcp`
   (a RELATIVE package path), which only resolves when the session's cwd is this
   repo. A user-scope install must name an absolute binary.
6. **Nothing on the MCP surface is scoped by client, and this ticket keeps it that
   way [Q1].**
   - `task_get_next` takes any `client` argument.
   - `task_context` returns any task by id (`taskcontext.go:53-58`).
   - The worker's claude process inherits the wrapper's whole environment,
     `DATABASE_URL` included (`internal/worker/loop.go:45`, `cmd/opsworker/main.go:9`),
     and runs with `--dangerously-skip-permissions` (`loop.go:392`).
   - Cross-client privacy is explicitly not a goal (Salvador, 2026-09-10). This is
     recorded as a standing fact, not a defect.
7. **SIX projects are `local_only` in production, not two, and most are client
   work.**
   - The migrations create only `personal` and `bulk` as `local_only`, both with
     `client IS NULL` (`migrations/0016_provider_locality.sql:66-67`, `0018:36-37`).
   - But 0016's column DEFAULT is `'local_only'`, and every project created by hand
     since took it. Verification step 0 on prod, 2026-09-10, found six `local_only`
     projects: `bulk`, `homelab`, `personal`, `foundry`, `saka` and `town-ai`.
     The last four have clients, and saka and town-ai each have a ready task.
   - **Lesson (for INSTITUTIONAL_KNOWLEDGE):** reading the migrations gave the wrong
     count. A column with a fail-closed default describes the rows the migration
     wrote, not the rows operators wrote since. Measure the table.
   - A `local_only` refusal would therefore have broken Salvador's use case in most
     of his repos. The SPEC's own step 5 example, `homelab`, was one of them. This
     is what prompted Q3.
8. **The read-only-tool precedent is `mail_search`/`mail_read_thread`**
   (`internal/tools/mail.go:25-26`).
   - Not `humanOnly`, not `snapshotGated`, allowed by the static fallthrough.
   - Pinned by `TestMatrix_MailToolsFallThroughForWorkers`
     (`internal/policy/mcp_actor_test.go:108`).
   - Limit convention: default/max, clamped, with `limit+1` fetched to report
     `truncated` honestly (`mail.go:93-147`).
9. **Every MCP call's args carry an injected `worker_id`** (`adapter.go:66,113-127`).
   Validators must tolerate that key. `json.Unmarshal` ignores unknown fields by
   default, and a test pins that, because a future `DisallowUnknownFields` would
   break every MCP call to these tools.
10. **`opsctl call` is generic** (`cmd/opsctl/main.go:137-151`), so both tools are
    reachable as `opsctl call --tool task_list --args '{…}'` with zero opsctl code.
11. **`TestValidate_RejectsMissingRequiredArgs` states "Empty args are illegal for
    every tool"** (`internal/tools/tools_unit_test.go:124-125`). `project_list` is
    the first registered tool with NO required field. It must be kept out of that
    list, and the comment amended to say why.
12. **The status vocabulary's only authority is the CHECK**
    (`migrations/0001_initial.sql:123-126`, twelve values, never altered since). Go
    holds partial lists: `openStatuses` (`close.go:32`) and the board's
    `boardStatusOrder`.
    - **"open" is already taken:** `openStatuses` means `holding, ready, blocked,
      done_locally, delivered`, the set `task_close` accepts. The default set below
      is therefore named `in_play`, never "open".
    - `internal/promote` already spells `NOT IN ('closed','delivered')` for its own
      purpose (`promote/store.go:309`, `promote.go:86`). That is a sibling concept
      in another package, and it is NOT shared (see "Out of scope").
13. **opsworker rebuilds ops-mcp from the checkout at every start**
    (`cmd/opsworker/main.go:87`, `buildOpsMCP`). Worker consoles pick up the new
    tools on their next start, with no deploy.

## Decisions

Numbered **L…** so they never collide with another SPEC's `D`/`E` numbers.

**L0 — Scope confirmed by Salvador, 2026-09-10: BOTH tools, read-only, MCP-listed,
through the executor.** `project_list` exists so a session can confirm a slug before
memorising it. `task_list(project, …)` pulls one named project's queue. Neither
changes anything.

**L1 — Names: `task_list` and `project_list`.** `task_*` is the task-tool family, and
`project_list` names the `projects` table. Not `queue_list`/`get_queue`, because
CLAUDE.md says queues are filters, not objects.

**L2 — `project` is an explicit, REQUIRED argument, and the server infers nothing,
for any actor.** The per-repo binding lives in Claude Code's own per-project memory,
supplied by the caller. Inferring it from `OPS_WORKER_ID` would be a second,
invisible source of "which project am I", with no answer for `manual:salvo`.

**L2a [Q1] — No client scoping, for any actor, including worker consoles.** Answered
(a) on 2026-09-10: "privacy is not important". Every caller sees whatever project it
names, as with `task_get_next` and `task_context` today.
*Not done, and why:* a wrapper-written client scope was dropped, because privacy is
not a goal and it would have scoped one reader in three while the worker keeps its
DB credential.

**L3 [Q3] — `task_list` does NOT refuse `local_only` projects. They list normally,
for every actor.** Owner decision, 2026-09-10: "List them anyway".
- **Ground:**
  - `task_list` rows carry only `title, status, priority, assignee_type` (plus
    `subproject`/`parent_id` when set), and NEVER a body. L6 and criterion 7 pin
    that key set, so the body cannot creep back in.
  - The locality rule (SWT-21, `ai_locality`) keeps guarding message BODIES where
    they are processed: the classify lanes, drafts and triage all still read the
    column.
  - Salvador, as owner, weighed the residual (below) and chose to list.
- **Consequence, stated plainly: `task_list` and `project_list` no longer read
  `projects.ai_locality` at all.** No clause, no refusal, no surfaced flag.
  `ai_locality` appears nowhere in `tasklist.go`.
- **The honest residual, recorded rather than hidden (Future work):**
  - A promoted personal task's TITLE may be derived from private mail, and it now
    reaches a hosted model's context through `task_list(project="personal")`.
  - Separately, and pre-existing, `task_context` still returns any task's BODY by
    id, whatever its project's locality.
- **Why the reversal was right.** L3 was first written as a refusal on SWT-30's
  "any future reader of `tasks` without one of those clauses inherits a leak". It
  assumed two `local_only` projects. Production has six, four of them client work
  (fact 7). The refusal would have turned the tool off in most of Salvador's repos,
  protecting titles he did not ask to protect.

**L4 [Q2] — The default status set is `in_play`: every status except `closed` AND
`delivered`.** Answered (b) on 2026-09-10.
- **Differs from the board on purpose.** The board is a human view with room for
  finished work. `task_list`'s output lands in a model context, where every
  delivered row is tokens spent restating work that needs nothing more ("filter so
  no junk and wasted tockens").
- **Explicit `status` still works.** `status=delivered` or `status=closed` returns
  exactly that set when a caller means it.
- **The predicate is spelled once**, as `t.status NOT IN ('closed','delivered')`, in
  a const shared by both tools.

**L4a — `holding` stays IN the default.** A holding task is a review decision owed
by Salvador, which is work in play for him, not junk. It is counted separately, so
it is visible rather than mixed in. `done_locally` stays in for the same reason: its
delivery is still pending.

**L5 — `status` is ONE value from a Go list pinned to the CHECK.**
- `taskStatuses` (new, in `internal/tools/tasklist.go`) is validated against.
- An unknown status is a validation error, never an empty list: a typo'd
  `status=redy` must not read as "nothing ready".
- An integration test parses `pg_get_constraintdef` for the tasks status CHECK and
  asserts set-equality, so the Go list cannot drift from the database's.
- Single value, like the board's `?status=`.

**L6 — Rows are compact: `id, title, status, priority, assignee_type`, plus
`subproject` and `parent_id` ONLY when set.** Each field was kept or cut against L15:
- **No body, ever.** Capture and promotion put raw message text there. It is the
  single largest token cost, and since Q3 it is also the thing whose absence makes
  listing `local_only` projects acceptable (L3). `task_context` is the audited
  per-task read.
- **No `plan_order`.** Row position already encodes the order the server applied.
- **No `created_at`.** "What's in my queue" does not ask how old a task is, and
  `task_context` carries it.
- **Kept:**
  - `priority` says why a row sits where it does;
  - `assignee_type` separates "mine" from "Claude's";
  - `status` is needed whenever the default set is mixed.
- **`subproject` and `parent_id` are omitted, not `null`, when NULL.** Absence is
  unambiguous, and most rows have neither. `parent_id` is kept when set because a
  child task reads wrongly without its parent.
- **Titles are returned whole.** A cut title can change meaning.

**L7 — Ordering has ONE spelling, shared with `task_get_next`.**
- The `ORDER BY` fragment becomes an unexported const in `getnext.go`, used by both
  handlers.
- A structural test fails on a second copy of the literal.
- An integration test proves the first row of
  `task_list(status=ready, assignee_type=claude)` equals `task_get_next`'s answer,
  pass after pass.

**L8 — The response carries counts by status over the FULL filtered set,
independent of `limit`.**
- `counts` is keyed by status, `total` is their sum, and `truncated` is reported
  honestly via `limit+1`.
- The WHERE fragment is built ONCE and used by both the rows query and the counts
  query.
- Echoed back:
  - `project`: the resolved slug;
  - `filter`: only the keys that apply. `status` is always present (default
    `"in_play"`), `limit` is always present, and `assignee_type`/`subproject`
    appear only when given.

**L9 — `limit`: default 25, max 200, clamped rather than refused; negative refused.**
This is the `mail_search` shape. The default is 25 under L15: the counts carry the
full picture, so rows beyond the first screenful are tokens a conversational answer
will not read. The applied value is echoed.

**L10 — An unknown slug is an error that names `project_list`.** The text is
"project %q not found; call project_list for valid slugs". It does not enumerate
the slugs. This is the only way `task_list` refuses a project.

**L11 — `project_list` is a compact DIRECTORY.**
- No arguments. Every project, `ORDER BY slug`, none hidden: a project with nothing
  in play is still a slug a repo may memorise.
- Row: `slug, name, in_play`, plus `client` only when set.
- **No `local_only` key.** Its only purpose was to stop a session memorising a slug
  `task_list` would refuse. After Q3 nothing is refused, so under L15 it is noise.
- `in_play` is the `total` `task_list(project)` would report under its default,
  computed from the SAME predicate const. Parity is proven by an integration test.

**L12 — Policy: both tools fall through the static allow-list.**
- They are not `humanOnly` and not `snapshotGated`, exactly like `mail_search`.
- `internal/policy` is not edited.
- Nothing in either handler branches on who is calling, or on any property of the
  project beyond its id.

**L13 — The user-scope install: server name `ops`, the READ-ONLY binary
`ops-mcp-read`, built from `main`.**
- *Why a binary rather than `go run`.* A user-scope `go run` compiles whatever
  branch is checked out in the switchboard repo when a session starts, so a
  half-built ticket branch would serve every project's session.
  `go install ./cmd/ops-mcp-read` run on `main` makes the installed version
  deliberate. The runbook says to re-run it after any merge touching
  `cmd/ops-mcp-read`, `internal/mcpserver` or `internal/tools`.
- *Why the name `ops`.* Claude Code resolves a same-name server local → project →
  user. Inside this repo, `.mcp.json`'s `ops` (the live checkout) shadows the
  user-scope one; every other repo gets the installed binary. Verified in step 5,
  not assumed.
- *Env.* `DATABASE_URL`, and `OPS_WORKER_ID=manual:salvo` (required; fact 4).
- **Amended after the Codex and go-reviewer passes (2026-09-10).** This decision
  first said "`OPS_TOKEN_KEY` is deliberately omitted, so no mail sender is wired".
  That premise is false (fact 4, amended): the server inherits the shell's
  `OPS_TOKEN_KEY`, and the full allowlist it would serve includes
  `approve_delivery`/`send_delivery`, which `manual:salvo` may call, plus
  `create_task`, `task_claim`, `mark_done_local`, `record_decision` and
  `draft_delivery`. Every repo's session reads untrusted content, so every one of
  those was one prompt injection away.
- **The read-only binary `cmd/ops-mcp-read`.**
  - `internal/mcpserver` gains `Profile` (`full` | `read`) and
    `NewWithProfile`. The read profile lists and accepts exactly `project_list`,
    `task_list` and `task_get_next` — each writes nothing but its audit row;
    `task_context` is out because fetched by the claim holder it flips claimed →
    in_progress. Its entries are taken FROM `agentTools`, so no schema is spelled
    twice, and only `ProfileFull` keeps the allowlist whole.
  - `cmd/ops-mcp-read` builds the read profile over the same executor pipeline and
    wires NO mail sender and NO calendar booker — its `main` imports no connector
    and calls no `tools.Set*` seam, so the seams stay nil (the connector code is
    linked via `internal/tools`, never wired) — so no inherited secret can arm one.
  - `cmd/ops-mcp` is unchanged in behaviour (full, senders wired); both share
    `mcpserver.Server.Serve`. Worker consoles and `.mcp.json` are untouched.
  - *Why a binary, not a setting on ops-mcp.* An `OPS_MCP_PROFILE` variable was
    tried first; Codex's re-review showed it fails open: every existing launcher
    sets nothing, so unset had to mean `full`, and an install that lost the variable
    would silently restore the write surface. A separate binary has no such state.
  This supersedes the "reduced tool set" non-goal and follow-up.
- *`.mcp.json` is unchanged.*

**L14 — The worker rule "never choose your own work" stays a PROMPT rule.**
`task_list` makes it easy for a worker to `task_claim` an id out of order. Nothing
enforced the rule before, and nothing does after. The tool description says
"read-only; does not claim; a worker takes work only via task_get_next".

**L15 — "No junk and wasted tokens" is the acceptance test for the response shape.**
- The output is compact JSON (`marshalResult`, no indentation).
- A key appears in a row only when it carries information (L6, L11).
- The default set excludes finished work (L4).
- The default limit is one screenful (L9).
- Counts replace scrolling (L8).
- A criterion pins the exact key sets (criterion 7), so a later "just add one field"
  is a deliberate test edit, not drift.

## Acceptance criteria

### Registration, validation and shape (unit, `go test ./...`)

1. `tools.Register` registers `task_list` and `project_list` (both added to
   `allToolNames` in `internal/tools/tools_unit_test.go`).
2. `task_list` is added to `TestValidate_RejectsMissingRequiredArgs`. `project_list`
   is NOT, and that test's header comment is amended to name `project_list` as the
   one tool whose empty args are legal, and why (fact 11).
3. `internal/tools/tasklist_test.go` `TestValidateTaskList` table, calling the
   validator directly:
   - `{}` → error "missing project";
   - `{"project":"  "}` → error;
   - `{"project":"x","status":"redy"}` → error naming the value;
   - every one of the twelve CHECK statuses → ok, including `closed` and
     `delivered` (explicit status still works, L4);
   - `{"project":"x","assignee_type":"robot"}` → error;
   - `human`/`claude` → ok;
   - `{"project":"x","limit":-1}` → error;
   - `limit` 0 and 10,000 → ok (clamped at handle time, L9);
   - `{"project":"x","worker_id":"mcp-injected"}` → ok (fact 9).
4. `TestValidateProjectList`:
   - `{}`, `{"worker_id":"manual:salvo"}` and an empty body → ok;
   - a non-object (`[]`) → error.
5. **`TestTaskList_OrderingSpelledOnce`** (structural, no db): across non-test `.go`
   files in `internal/tools`, the literal `t.plan_order ASC NULLS LAST` occurs
   exactly once, and both `getnext.go` and `tasklist.go` reference the const that
   holds it (L7).
6. **`TestTaskList_InPlayPredicateSpelledOnce`** (structural): the literal
   `NOT IN ('closed','delivered')` occurs exactly once in non-test `internal/tools`
   sources, and both `task_list` and `project_list` reference the const that holds
   it (L4/L11). The test's comment says why `internal/dashboard/board.go:72` and
   `internal/promote` are outside the scan: they are different views that
   deliberately do not share it.
7. **`TestQueueReadOutput_KeySetsAreCompact`** (L3/L6/L11/L15): marshal the row,
   filter and project-row types directly and assert the EXACT JSON key sets.
   - **Task row:**
     - with NULL subproject/parent it has exactly
       `{id,title,status,priority,assignee_type}`;
     - with both set, those plus `{subproject,parent_id}`;
     - no `body`, `plan_order` or `created_at` key can appear. Since Q3 this
       assertion is the load-bearing half of L3.
   - **Filter:** the default is exactly `{status:"in_play",limit:25}`.
   - **Project row:**
     - with a NULL client it is exactly `{slug,name,in_play}`;
     - with a client, those plus `{client}`;
     - no `local_only` or `ai_locality` key can appear.

### Behaviour against real rows (integration, `make integration`)

All in the new `internal/tools/tasklist_integration_test.go`, reusing
`lifecycle_integration_test.go`'s helpers (`newToolsPool`, `cleanupToolsData`,
`seedProject`, `newExecutor`, `callOK`) and `getnext_ordering_integration_test.go`'s
`insertTask`. Slugs sit under the existing `itest-mcp-tools-` prefix, inside the
cleanup and the `go test -p 1` pact. Every call goes through `executor.Execute`.

8. **`TestTaskList_Integration_OrderingMatchesGetNext`** — seed the priority,
   plan_order and created_at mix `TestGetNext_Integration_OrderingAndFilters` uses,
   then:
   - assert `task_list` returns ids in priority DESC → plan_order ASC NULLS LAST →
     created_at ASC → id ASC order;
   - then, repeatedly, assert the first row of
     `task_list(status=ready, assignee_type=claude)` equals `task_get_next(client)`,
     claiming it between rounds, until both are empty.
9. **`TestTaskList_Integration_DefaultShowsOnlyWorkInPlay`** — one task in each of
   the twelve statuses:
   - with no `status`, ten rows come back, and `closed` and `delivered` are absent
     from both `tasks` and `counts`;
   - `holding` and `done_locally` ARE present (L4a);
   - `status=delivered` returns exactly the delivered task;
   - `status=closed` returns exactly the closed task.
10. **`TestTaskList_Integration_ProjectFilterIsolates`** — the filter control.
    - Seed project A and sibling B under the SAME client, plus project C under
      another client, each with ready tasks.
    - `task_list(A)` returns only A's ids, and `counts`/`total` count only A's.
    - Comment: **MUTATION THAT MUST TURN THIS RED: removing the project clause from
      the shared WHERE fragment.** The same-client sibling is what makes the control
      bite against a `p.client` substitution.
11. **`TestTaskList_Integration_CountsIgnoreLimitAndShareTheFilter`**
    - Seed 5 ready + 3 blocked in A plus noise in B.
    - `limit=2`: 2 rows, `truncated=true`, `counts={"blocked":3,"ready":5}`,
      `total=8`.
    - `limit=200`: `truncated=false`, and `total == len(tasks)`.
    - With `assignee_type` and `subproject`, the counts narrow exactly as the rows
      do.
    - Then seed 30 ready tasks in a fresh project: with no `limit`, exactly 25 rows
      come back, with `truncated=true`, `total=30` and `filter.limit=25` (L9).
    - Mutation that must turn it red: giving the counts query its own WHERE without
      the project clause.
12. **`TestTaskList_Integration_ListsLocalOnlyProjectsForEveryActor`** [Q3].
    - Seed project L with `ai_locality='local_only'` and project Y with
      `ai_locality='any'`. Give each the same task shape: two ready, one blocked,
      and one with a non-empty `body`.
    - Call `task_list` on both under each of the nine actor shapes in the repo:
      `dashboard:salvo`, `opsctl:salvo`, `mcp:manual:salvo`, `mcp:acme`,
      `mcp:acme.main`, `mcp:worker:acme`, `drafts:gpt`, `worker:acme`,
      `ticketstatus:jira`.
    - Every call succeeds.
    - L's response equals Y's in shape: same counts and same row keys, differing
      only in ids and slug.
    - No row in either response has a `body` key, and the seeded body text appears
      nowhere in the raw response bytes.
    - The test's comment states: **`task_list` no longer reads
      `projects.ai_locality`** (L3). A re-added refusal, keyed on the column or on
      the actor, turns this test red.
13. **`TestTaskList_Integration_UnknownProjectIsAnError`** — an unknown slug returns
    an error containing both the slug and `project_list`, not `{"tasks":[]}`.
14. **`TestTaskList_Integration_StatusVocabularyMatchesTheCheck`**
    - Read `pg_get_constraintdef` of the CHECK on `tasks.status`, extract the quoted
      values, and assert set-equality with `taskStatuses`.
    - Assert exactly one CHECK on `tasks` mentions `status`, so a vacuous parse
      cannot pass.
15. **`TestTaskList_Integration_ReadsWriteOnlyTheAuditRow`**
    - Record the fixture tasks' `status`/`updated_at` and the `task_events` and
      `task_claims` counts.
    - Call `task_list` and `project_list`. All of those are unchanged afterwards.
    - `audit_events` gained rows for both tools, completed `ok`.
    - The unknown-slug call (criterion 13) leaves its audit row in `error`. This is
      invariant 3 for reads.
16. **`TestProjectList_Integration_DirectoryAndCounts`**
    - Seed:
      - A (`any`, with a client): 3 in play + 1 closed + 1 delivered;
      - E (`any`, NULL client): zero tasks;
      - L (`local_only`, with a client): 2 ready.
    - All three are listed, ordered by slug. E is NOT hidden, and L is listed like
      any other project.
    - `A.in_play == 3 == task_list(A).total`.
    - `E.in_play == 0`, and E has no `client` key.
    - `L.in_play == 2 == task_list(L).total`.
    - No row carries a `local_only` key.

### Policy (unit)

17. `internal/policy/matrix_tasklist_test.go`
    **`TestMatrix_QueueReadToolsFallThroughForEveryActorShape`**:
    - for both tools, under the nine actor shapes of criterion 12,
      `policy.NewMatrix(recordingLoader, NewStatic("task_list","project_list"))`
      returns `allow`, and the loader is never called;
    - `policy.Decide` with a worker actor returns allow rule `matrix-human`, never
      `human_only`.

### MCP surface (unit)

18. `internal/mcpserver/schemas.go` gains both tools, and `adapter_test.go`'s
    `wantAgentTools` gains both, so `TestListTools_ExactlyAgentAllowlist` stays
    exact.
19. New `internal/mcpserver/queue_tools_test.go`
    **`TestListTools_IncludesQueueReadTools`**. Both `InputSchema`s are valid JSON,
    and neither mentions `worker_id`.
    - **`task_list` schema:** `project` (the ONLY required field), `status` (enum
      of the twelve), `assignee_type` (enum `human|claude`), `subproject` and `limit`
      (integer).
    - **`project_list` schema:** no `required` and no properties.
    - **Descriptions** (asserted case-insensitively):
      - `task_list` says it is read-only, that it does not claim, that closed AND
        delivered tasks are hidden unless requested by `status`, and that `project`
        is the caller's slug (mentioning `project_list`);
      - `project_list` says it lists slugs to confirm before memorising one.
20. **`TestCallTool_TaskListForwardsWithMCPActor`** — the adapter forwards
    `task_list` with `Actor = "mcp:"+testWorkerID`, an injected `worker_id`, and the
    caller's `project` unaltered. The adapter adds no other field (L2, L2a).

### Install and runbook

21. `docs/runbooks/ops-mcp-user-scope.md` (new) contains, verbatim:
    - the `go install` line;
    - the `claude mcp add --scope user` line with both `-e` flags, registering
      `ops-mcp-read`;
    - the precedence note (L13);
    - why omitting `OPS_TOKEN_KEY` is not a boundary (inheritance) and what the read
      binary is, and the re-install rule;
    - the verification of steps 4-5;
    - the memorise-a-slug usage line;
    - one sentence stating that `task_list` hides closed and delivered by default,
      unlike the board.
22. `internal/mcpserver/runbook_test.go` **`TestRunbook_DocumentsUserScopeInstall`**
    requires the runbook to mention `--scope user`, `OPS_WORKER_ID=manual:salvo`,
    `go install ./cmd/ops-mcp-read`, `OPS_TOKEN_KEY`, `ops-mcp-read`,
    `project_list`, `task_list` and `delivered`.

### Added after review (2026-09-10)

23. **The read profile** (`internal/mcpserver/profile_test.go`):
    `NewWithProfile(…, ProfileRead)` lists exactly `project_list`, `task_get_next`,
    `task_list`, each byte-identical to its full-profile entry, and refuses every
    other agent-facing and spine-facing tool at the MCP layer without reaching the
    executor. A `task_list` call is forwarded with `Actor = "mcp:"+workerID`.
24. **No door to full:** `NewWithProfile` with any profile other than
    `ProfileFull` (e.g. `Profile("READ")`) serves the three-tool read slice.
25. **The read binary arms nothing** (`cmd/ops-mcp-read/main_structure_test.go`):
    its `main.go` imports no `internal/connector/*` package, calls no `tools.Set*`
    seam, never calls `mcpserver.New`, and calls `NewWithProfile(…, ProfileRead)`
    exactly once.
26. **One snapshot** (`TestTaskList_ReadsInOneReadOnlySnapshot`): the resolve, the
    page and the counts run on ONE `pool.BeginTx` with `pgx.RepeatableRead` and
    `pgx.ReadOnly`, and `taskList` makes no other `pool.*` call. Sharing the WHERE
    (L8) stops rows and counts drifting in predicate; this stops them drifting in
    time — two pool statements could straddle a claim or a close.

## Data model changes

**None.** No migration and no new table. The queue stays a filter over the one
`tasks` table.

Read-only columns:
- `tasks`: `id, project_id, title, status, priority, assignee_type, subproject,
  parent_id, plan_order, created_at`. `plan_order` and `created_at` are read for
  ORDER BY only and never returned.
- `projects`: `id, slug, name, client`.

`projects.ai_locality` is NOT read (L3).

No index. `tasks_status_project_idx (status, project_id)` (0001:134) is sufficient
at this scale.

## API / MCP tool changes

Both are registered in `internal/tools/createtask.go`'s `Register` list, so
`Executor.Execute` is the only route (invariant 3). Both are listed in
`internal/mcpserver/schemas.go` `agentTools`. Neither is added to `policy.humanOnly`
or `snapshotGated`.

**`task_list`** — `internal/tools/tasklist.go`

Request:
```json
{"project": "saka", "status": "ready", "assignee_type": "claude",
 "subproject": "main", "limit": 25}
```
Only `project` is required. `worker_id` arrives injected over MCP and is ignored.

Handler, in order:
1. Resolve the slug: `SELECT id, slug FROM projects WHERE slug = $1`.
2. No row → "project %q not found; call project_list for valid slugs". This is the
   ONLY refusal.
3. Build the WHERE fragment once: `t.project_id = $1`, plus either `t.status = $n`
   or the in-play const, plus the optional `t.assignee_type = $n` and
   `t.subproject = $n`.
4. Rows: `SELECT … WHERE <fragment> ORDER BY <queue order const> LIMIT <limit+1>`.
5. Counts: `SELECT t.status, count(*) … WHERE <fragment> GROUP BY t.status`.

Response (compact JSON on the wire; indented here for reading):
```json
{
  "project": "saka",
  "filter": {"status": "in_play", "limit": 25},
  "counts": {"blocked": 2, "holding": 1, "ready": 12},
  "total": 15,
  "truncated": false,
  "tasks": [
    {"id": 412, "title": "…", "status": "ready", "priority": 5, "assignee_type": "claude"},
    {"id": 418, "title": "…", "status": "blocked", "priority": 3, "assignee_type": "human",
     "subproject": "main", "parent_id": 401}
  ]
}
```
`tasks` is `[]`, never `null`, when empty.

**`project_list`** — same file. It takes no args.

Query: `SELECT p.slug, p.name, p.client, count(t.id) FILTER (WHERE <in-play const>)
FROM projects p LEFT JOIN tasks t ON t.project_id = p.id GROUP BY p.id ORDER BY p.slug`.

Response:
```json
{"projects": [
  {"slug": "collaboratory", "name": "…", "client": "…", "in_play": 34},
  {"slug": "personal", "name": "Personal", "in_play": 7},
  {"slug": "saka", "name": "…", "client": "…", "in_play": 1}
]}
```

**`getnext.go`**: its `ORDER BY` literal moves into the shared const, and its
behaviour is unchanged.

**CLI:** none (fact 10).

**Dashboard / HTTP:** none. The board's default is untouched.

## MQTT topics

None.

## Files likely to touch

- `internal/tools/tasklist.go` (new): args, validators, `taskStatuses`, the in-play
  predicate const, the output types, both handlers
- `internal/tools/getnext.go`: extract the `ORDER BY` into the shared const
- `internal/tools/createtask.go`: two `Register` entries, with a comment pointing
  at L3/L12
- `internal/mcpserver/schemas.go`: two `agentTools` entries
- `internal/tools/tasklist_test.go` (new)
- `internal/tools/tools_unit_test.go`: `allToolNames`, the validation list and its
  comment
- `internal/tools/tasklist_integration_test.go` (new)
- `internal/policy/matrix_tasklist_test.go` (new)
- `internal/mcpserver/queue_tools_test.go` (new)
- `internal/mcpserver/adapter_test.go`: `wantAgentTools`
- `internal/mcpserver/runbook_test.go` (new)
- Added after review (L13 amended): `internal/mcpserver/adapter.go` (`Profile`,
  `NewWithProfile`, a per-instance allowlist), `internal/mcpserver/serve.go` (new,
  the shared stdio loop), `internal/mcpserver/profile_test.go` (new),
  `cmd/ops-mcp-read/main.go` + `main_structure_test.go` (new), and
  `cmd/ops-mcp/main.go` (serve loop moved to `Serve`; behaviour unchanged)
- `docs/runbooks/ops-mcp-user-scope.md` (new)
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`, at deliver:
  - the two tools, and the `in_play` default that differs from the board;
  - `task_list` lists `local_only` projects by owner decision (2026-09-10), titles
    only, and does not read `ai_locality`. Name the residual (Future work);
  - **six** projects are `local_only` in production. The migrations create two, and
    0016's default caught the four created by hand since. "Measure the table, not
    the migration";
  - the user-scope install and its re-install rule;
  - "cross-client privacy is not a goal (2026-09-10); the MCP surface is unscoped
    by client";
  - the worker actor shape correction (`mcp:{client}`, not `mcp:worker:{client}`).

Not touched: `internal/policy/*.go` (non-test), `cmd/opsctl`,
`cmd/opsworker`, `internal/worker`, `internal/dashboard`, `.mcp.json`, `migrations/`.

## In scope / Out of scope

**In scope:** the two tools, the shared ordering and in-play consts, the schemas,
the tests above, the runbook, and the one-time user-scope install on the
workstation.

**Out of scope:**
- **Client scoping of any identity.** Answered no [Q1].
- **Changing any project's `ai_locality`.** Whether foundry, saka, town-ai and
  homelab SHOULD be `local_only` is an operator question about the classify, drafts
  and triage lanes. `task_list` no longer depends on the answer [Q3].
- **A locality clause on `task_context` / `task_get_next`.** This is a pre-existing
  gap and its own ticket (Future work).
- **Enforcing "never choose your own work"** (L14).
- **Changing the board's default**, or sharing the in-play const with
  `internal/dashboard` or `internal/promote`. The divergence from the board is
  intentional (L4).
- **Multi-status filters, title search, `worker_type`, bodies, and timestamps in
  rows.**
- **Build-order step 4's wrapper and step 10's board/exports.** No change to either.

## Invariants that apply

1. **Raw-first:** not engaged. Nothing is captured.
2. **One funnel:** satisfied by construction. No table is added: a queue is a filter
   over the ONE `tasks` table, and `project_list` is a read of `projects`, not a
   second registry.
3. **Everything through the executor, reads included.**
   - Both handlers are unexported closures reachable only via `Register` →
     `Executor.Execute`: validate → policy (static fallthrough) → audit start →
     handler → audit complete.
   - Criterion 15 proves the audit row for an ok call and a refused one.
   - Every predicate is a bound parameter, and the WHERE fragment is assembled from
     fixed strings, never caller text.
   - opsctl reaches both through the same executor path, and the MCP adapter stays
     SQL-free.
4. **Nothing external without a delivery row:** nothing outbound. The user-scope
   install is `ops-mcp-read`, which lists no send tool and wires no sender whatever
   the inherited environment holds, so installing it in every repo puts nothing in
   front of a client (L13, criteria 23-25).
5. **Own-message loop closure:** not engaged.
6. **Stealth attribution:** nothing client-visible is produced.
7. **Orchestrator purity:** untouched. There is no rule, no `task_events` write
   (criterion 15) and no NOTIFY, and the policy test runs with zero I/O
   (criterion 17).

**The locality boundary (SWT-21/SWT-30)** is not a numbered invariant.
- **The owner decided, on 2026-09-10, that `task_list` lists `local_only` projects
  [Q3].** This is recorded as a decision, not an oversight.
- The ground: the tool returns titles, statuses and priorities, never bodies. That
  is pinned by criteria 7 and 12, and the body is the content the locality rule
  exists to keep off hosted models.
- The rule itself is unchanged everywhere it is enforced today: classify, drafts
  and triage.
- The residual (titles derived from private mail; `task_context` bodies) is named
  in Future work, not hidden.

## Sibling patterns to copy

- **Read-only agent tool:** `internal/tools/mail.go` (not humanOnly, default/max
  limit, `limit+1` truncation, `marshalResult`), `internal/mcpserver/mail_tools_test.go`,
  and `internal/policy/mcp_actor_test.go:108`.
- **Queue ordering and its fixture:** `internal/tools/getnext.go` and
  `getnext_ordering_integration_test.go` (`insertTask`, explicit `created_at`
  offsets).
- **A filter control that dies when the clause is dropped:**
  `internal/ticketstatus/store_integration_test.go`'s "MUTATION THAT MUST TURN THIS
  RED" comments (criteria 10-11 copy that shape).
- **Enumerating actor shapes:** the SWT-19 lesson, with
  `internal/policy/mcp_actor_test.go`'s case table as the shape.
- **"One spelling" structural guard:** `internal/textmatch/callsites_test.go` and
  `internal/connector/upworkcrm/keyspelling_test.go`.
- **Runbook prose guard:** `internal/ticketstatus`' `TestRunbook_DocumentsTheReconciler`.
- **`FOR UPDATE SKIP LOCKED`:** deliberately NOT used. Nothing is claimed.

## Verification protocol

Do not commit before step 3 passes.

**0. Measure the live world first** (read-only; record the output in the delivery
summary; no counts become test literals):

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
psql -h 192.168.50.49 -U ops -d ops -c "
SELECT p.slug, p.ai_locality, t.status, count(t.id)
  FROM projects p LEFT JOIN tasks t ON t.project_id = p.id
 GROUP BY 1,2,3 ORDER BY 1,3;"
```

Record the list of `local_only` projects as found. As of 2026-09-10 it was bulk,
homelab, personal, foundry, saka and town-ai. It is informational only: nothing in
this ticket branches on it. Also note the `delivered` and `holding` counts per
project, which are what L4 hides and what L4a keeps.

**1. `go test ./...`** covers criteria 1-7 and 17-22.

**2. `make integration`** covers criteria 8-16 on the compose db (:5433,
`go test -p 1`). Then perform the two named mutations by hand (criteria 10 and 11),
see each go red, and revert.

**3. opsctl smoke against production, read-only:**

```bash
cd ~/projects/personal/switchboard
alias opsctl='DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl'
opsctl call --tool project_list --args '{}'
opsctl call --tool task_list --args '{"project":"collaboratory"}'
opsctl call --tool task_list --args '{"project":"collaboratory","status":"delivered"}'
opsctl call --tool task_list --args '{"project":"collaboratory","status":"ready","assignee_type":"claude"}'
opsctl call --tool task_list --args '{"project":"saka"}'         # local_only: lists normally
opsctl call --tool task_list --args '{"project":"personal"}'     # local_only: lists normally, titles only
opsctl call --tool task_list --args '{"project":"nope"}'         # refused; names project_list
```

Check all of these:
- the default collaboratory call's `counts` equals step 0's collaboratory rows
  minus `closed` and `delivered`;
- `project_list`'s `in_play` for collaboratory equals that `total`;
- the `status=delivered` call returns step 0's delivered count;
- the saka and personal calls list their in-play tasks, and their counts match
  step 0;
- no row in any response carries a body or timestamp key, and no `project_list`
  row carries `local_only`;
- the ready/claude first id equals
  `opsctl call --tool task_get_next --args '{"client":"<collaboratory's client>"}'`
  when no same-client project outranks it (otherwise say so and skip);
- the audit trail
  (`SELECT actor, tool, status FROM audit_events WHERE tool IN ('task_list','project_list') ORDER BY id DESC LIMIT 10;`)
  shows `opsctl:…` rows, all `ok` except the single unknown-slug call, which is in
  `error`.

**4. The "usable alone" install**, on `main` after merge:

```bash
cd ~/projects/personal/switchboard && git switch main && go install ./cmd/ops-mcp-read
claude mcp add --scope user ops \
  -e DATABASE_URL='${OPS_DATABASE_URL}' -e OPS_WORKER_ID=manual:salvo \
  -- "$(go env GOPATH)/bin/ops-mcp-read"
claude mcp get ops
```

The registered command MUST be `ops-mcp-read`, never `ops-mcp` (L13 amended: the
full binary at user scope is the write surface in every repo).

Whether Claude Code expands `${OPS_DATABASE_URL}` in a USER-scope entry is to be
ESTABLISHED here. A failure is loud: `DATABASE_URL is not set`, or a ping error in
`/mcp`. In that case, re-add with the literal DSN, which has the same exposure as
`~/.pgpass` and `~/.bashrc`. Record which form worked in the runbook and in
INSTITUTIONAL_KNOWLEDGE.

**5. Prove it from outside the repo, and prove the precedence:**
- `cd ~/projects/personal/kube && claude`. `/mcp` shows `ops` connected, with
  exactly three tools: `project_list`, `task_get_next`, `task_list`.
- "list the switchboard projects" → `project_list`.
- "remember this repo's switchboard project is homelab" → saved to memory.
  (homelab is `local_only`; since Q3 that makes no difference.)
- In a NEW session in the same repo, "what's in my queue" → a
  `task_list(project=homelab)` call whose counts match step 0 minus
  closed/delivered.
- In `~/projects/personal/switchboard`, `/mcp` shows ONE `ops`, and it is the
  project-scope `go run` entry.
- The audit rows from these sessions carry actor `mcp:manual:salvo`.

**6. Worker consoles:** no action. opsworker rebuilds ops-mcp at start (fact 13).
Nothing is deployed and there is no kube handoff.

## Open questions

None remain. All three were answered on 2026-09-10; see
`docs/tickets/task-list-mcp_OPEN_QUESTIONS.md`.
- Q1: worker scoping → none.
- Q2: the default hides closed and delivered.
- Q3: list `local_only` projects anyway.

L4a (`holding` stays in) was decided here rather than asked: it has one sensible
answer under the "no junk" rule.

## Future work (not this ticket)

- **The locality residual, recorded by Q3:**
  - `task_context` still returns any task's BODY by id, whatever its project's
    `ai_locality`. That is pre-existing, and one call away from any MCP session.
  - A promoted `personal` task's TITLE may be derived from private mail, and
    `task_list(project="personal")` now returns it to a hosted model.
  - If either matters, the fix is a locality clause on `task_context` (bodies), or
    a title policy at promotion time. It is not a refusal in `task_list`.
- **Review the four client projects that are `local_only` by default** (foundry,
  saka, town-ai, homelab). 0016's fail-closed default made them `local_only` when
  they were created by hand. That affects the classify, drafts and triage lanes,
  not this tool.
- **A claim guard**, so a worker can only `task_claim` what `task_get_next` last
  returned to it (L14).
- **A multi-status filter or a `worker_type` field**, if conversational use shows a
  real need. Each must earn its tokens (L15).
