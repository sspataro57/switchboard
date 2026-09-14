> Jira: SWT-51

# board-done-button — a Done verb on the board row, through the existing `task_close`

No open questions arose: every point was settled from CLAUDE.md, the SWT-31 SPEC
and the code. The one real choice (how "human-assigned only" is enforced) is
recorded under "Decisions made unilaterally" with its rationale.

## Source

Ad-hoc, from Salvador, 2026-09-14, verbatim:

> yes build the Done button. also do we have an inprogress status?

Not a build-order step. It is the second verb on the board after SWT-31's
Dismiss. Dismiss means "this task should never have existed". There is still no
board verb for "this task is finished", so today a finished human task stays on
the board until someone runs `opsctl call task_close` or a Claude session calls
the MCP `task_close`.

## Goal

Add a per-row **Done** control to `/tasks`. It POSTs to `/tasks/{id}/close` and
closes the task through the EXISTING `task_close` tool as `dashboard:{user}`,
using the executor, policy and audit path that `/tasks/{id}/dismiss` uses. It
writes no dismissal label and changes nothing in `task_close`.

**Usable alone means:** with nothing else deployed, Salvador opens `/tasks`,
clicks *Done* on a finished human task (optionally typing a note), and the flash
reads `task_close ok`. The row leaves the default board and shows under
`?status=closed`. `/tasks/{id}` shows a `status_changed` event whose reason
carries his note. `audit_events` has a `task_close` row with his
`dashboard:` actor and the task id. `task_dismissals` has no row for the task.
Nothing outbound happens, and no worker, connector or orchestrator rule changes
behaviour. Once `orchestratord` drains the event, dependents unblock as they do
for any close.

## Decisions made unilaterally (with rationale)

- **D1 — No new tool and no new arg. The optional note folds into
  `task_close`'s required `reason`.** `closeArgs` is `{task_id, reason}`, and
  `validateClose` rejects an empty reason (`internal/tools/close.go`). The
  handler writes the reason as prose: `done on the board`, or
  `done on the board: <note>` when the trimmed note is non-empty. This matches
  how `dismissTask` writes `dismissed (<code>): <note>`. The reason is prose for
  the human trail and nothing queries it (the SWT-31 D2 rule). The actor in
  `audit_events` is what tells a board close apart from other closes.

- **D2 — The button renders only on rows with `assignee_type = 'human'`. The
  template is what enforces this, not the executor.**
  *Why not claude rows:* `task_close` does not handle worker-held tasks safely.
  `closeTransition` refuses `claimed`, `in_progress` and `needs_feedback`, but
  it ACCEPTS `pr_open`, `awaiting_ci` and `awaiting_merge` (see the
  INSTITUTIONAL_KNOWLEDGE.md SWT-37 note). A worker's claim is still live in
  those statuses:
  - Only `mark_done_local` and `task_release` ever write
    `task_claims.released_at` (`internal/tools/donelocal.go:57,116`).
    `taskPRTransition` never touches `task_claims`.
  - R6's facts load expired claims only for `claimed`, `in_progress` and
    `needs_feedback` (`internal/orchestrator/facts.go:112`), so claim expiry
    never sweeps that claim.

  Closing such a row from the board would close work out from under a live
  CI/PR loop and leave its claim unreleased forever.
  *Why not a hard gate:*
  - (a) Enforcing assignee inside the executor path needs a new narrowing arg
    on `task_close` (a `require_assignee_type`, the SWT-38 pin shape). That
    changes `task_close`'s contract, which the owner's brief puts out of scope.
  - (b) A dashboard-side `SELECT assignee_type` before calling the executor
    would be SQL outside the executor with an unaudited refusal. That is the
    side door invariant 3 names, and SWT-31 criterion 3 says the board handler
    "performs no SQL of its own for the action".
  - (c) The dashboard actor is a human who already holds `task_close` over every
    task through `opsctl` and this repo's full `ops` MCP. The risk here is a
    mis-click on a worker's row, and a missing button prevents that.

  *Cost:* a hand-crafted `POST /tasks/{id}/close` on a claude task in
  `pr_open`/`awaiting_*` closes it, exactly as `opsctl call task_close` does
  today. The hard gate is listed under Future work. Nothing rewrites
  `tasks.assignee_type` after creation (no `UPDATE … assignee_type` anywhere
  under `internal/`), so the rendered condition cannot go stale between render
  and click.

- **D3 — Within human rows, Done renders for EVERY status (SWT-31 D6,
  unchanged).** There is no status conditional in the template. A human task can
  be `claimed`/`in_progress`/`needs_feedback` when a manual `/task N` session
  holds it: `claimTask` lets a human MCP identity claim a human task
  (`claim.go:69`). Done on such a row gets `task_close`'s own refusal as the
  flash: `task N is in_progress; refusing to close active work`. The way to
  finish that work is `mark_done_local` from the session. One spelling of the
  status list, in `closeTransition`. A visible error is better than a hidden
  button that drifts from it.

- **D4 — A separate form in the same cell, after Dismiss.** Its own four hidden
  filter inputs, its own `note` text input and `<button>Done</button>`, with no
  `<select>` and no `onchange`. Two forms, not one form with two submit buttons:
  the Dismiss `reason_code` select must never ride along with a Done submit, and
  a Done must never be sent to `/dismiss`.

- **D5 — One spelling of the filter rebuild.** Move the four-key `url.Values`
  loop from `dismissTaskAction` into one unexported helper in `board.go` (for
  example `boardBack(r) url.Values`), called by both handlers. A second
  copy-pasted loop is how a fifth filter ends up surviving one verb's redirect
  but not the other's. `TestBoardHandler_RebuildsFiltersRatherThanEchoingRawQuery`
  stays green: `board.go` still names the four keys and `url.Values`, and never
  `RawQuery`.

- **D6 — `Deliver #N` and `Answer feedback #M on task #N` rows get Done, like
  every human row.** R3 and R1 both create their tasks with
  `assignee_type: "human"` (`internal/orchestrator/rules.go:180,258`), so these
  rows qualify. The consequences are those of `task_close` today, and of
  SWT-31's Dismiss on the same rows. This ticket neither widens nor narrows them:
  - Closing a Deliver task does not mark its parent delivered, and
    `drafts.DeliverTasks` stops drafting for it: its query keeps only
    `t.status IN ('ready','holding')` (`internal/drafts/store.go:91`).
  - Closing an unanswered feedback task hides the reminder. The
    `feedback_requests` row stays `open` and the parent stays `needs_feedback`,
    which R6 exempts. `opsctl answer-feedback` still works afterwards: R2's own
    `task_close` of the answer task is an idempotent no-op.

  The Future work section names the dedicated verbs.

## Acceptance criteria

### Template (`internal/dashboard/templates/tasks.html`)

1. Each row's last cell contains, after the existing Dismiss form, a
   `<form class="inline" method="post" action="/tasks/{{.ID}}/close">` holding:
   the four hidden filter inputs (`project`, `status`, `assignee_type`,
   `subproject`, valued from `$.Filters`, exactly as the Dismiss form does), a
   `<input type="text" name="note" placeholder="note (optional)">`, and
   `<button>Done</button>`.
2. The Done form, and only the Done form, sits inside
   `{{if eq .AssigneeType "human"}} … {{end}}`. The Dismiss form stays
   unconditional and still renders on every row (SWT-31 D6).
3. `tasks.html` contains no status conditional anywhere: the substring
   `eq .Status` does not occur. (This generalizes SWT-31's two banned literals to
   both verbs.)
4. `tasks.html` still contains exactly one `onchange`, the project filter's
   (SWT-26, SWT-31 criterion 16). The Done form adds none.

### Route and handler (`internal/dashboard/server.go`, `internal/dashboard/board.go`)

5. `server.go` registers
   `mux.Handle("POST /tasks/{id}/close", s.auth.Require(http.HandlerFunc(s.closeTaskAction)))`
   on the auth-required mux, beside the dismiss route.
6. `closeTaskAction` works as follows:
   - It parses the form. A non-numeric or non-positive `{id}` is a 400 and
     reaches no executor.
   - It builds args with `json.Marshal` of exactly `{"task_id": id, "reason": reason}`.
   - `reason` is `done on the board`, or `done on the board: ` + `strings.TrimSpace(note)`
     when the trimmed note is non-empty.
   - It makes exactly ONE `s.executeTask(w, r, "task_close", args, id, back)`
     call, with `back` from D5's helper.
   - It does not use `s.pool` (no SQL of its own: invariant 3).
7. The args are JSON-safe. A note of `x","task_id":999,"y":"` yields a call whose
   `task_id` is the path id and whose reason contains the note verbatim. String
   concatenation into JSON would fail this.
8. The redirect is `303` to `/tasks?` + the rebuilt filters + `flash`
   (`task_close ok` or the executor's error text), never an echo of
   `r.URL.RawQuery`.

### Executor, policy, audit (no code change; asserted)

9. Policy: `task_close` by `dashboard:{user}` goes through `policy.Matrix.Check`.
   Its `mcpHumanOnly` branch calls `Decide`, which does not deny (the actor is
   not on the MCP transport), and falls through to the static allow-list:
   **allow / `static-default`**. This is already pinned for `dashboard:salvo` by
   `TestMatrix_MCPHumanOnly_ThroughCheck` (`internal/policy/matrix_mcpverbs_test.go`,
   `mcpVerbsCorpus` row 1) against the real registry. This ticket changes no line
   of `internal/policy`. `task_close` does not move to `humanOnly`, and
   `mcpHumanOnly` stays exactly `[task_close task_mark_delivered]`
   (`mcpverbs_internal_test.go`).
10. A board Done writes `audit_events` rows with `tool='task_close'`,
    `actor='dashboard:{user}'`, `task_id` = the task (non-NULL, via
    `executeTask`'s `Call.TaskID`) and final `status='ok'`. The matching
    `policy_decisions` row reads `allow` / `static-default`.

### Behaviour (all existing `task_close` behaviour, proven through the route)

11. A `ready` human task becomes `closed`, `closed_at` is set,
    `closed_from_status='ready'`, and ONE `status_changed` event is written whose
    payload keys are exactly `[from reason to]`: `from=ready`, `to=closed`, and
    `reason` containing the note.
12. Done never writes `task_dismissals`: the count for the task is 0 before and
    after, including after a Done with a note.
13. Refusals, as a table over a human task in `claimed`, `in_progress` and
    `needs_feedback`, each seeded with an unreleased `task_claims` row the way
    production has one. The flash contains `refusing to close active work`, the
    status is unchanged, no `status_changed` event is added, and the claim row
    is untouched.
14. Idempotence: Done on an already-`closed` task (a stale page or double submit)
    flashes `task_close ok`, adds no `status_changed` event, and leaves
    `closed_at` and `closed_from_status` unchanged.
15. The closed task is gone from `/tasks?project=<slug>` and present under
    `/tasks?project=<slug>&status=closed`, with no change to `boardQuery`
    (`TestBoardQuery_ClosedStaysHiddenByDefault` stays green).
16. Dependents: the close's `status_changed {to: "closed"}` event is exactly the
    input `Evaluate` routes to `ruleUnblockDependents`
    (`internal/orchestrator/rules.go:124-128`). The dashboard code touches
    neither `task_dependencies` nor any dependent row. The integration test
    seeds a `blocked` dependent and asserts it is still `blocked` and untouched
    after the POST: unblocking is the orchestrator's job on its next drain.
    That rule is already covered by the orchestrator suites.
17. Inquiry outcome: take a task promoted by the inquiry lane (a
    `classify_promotions` row whose `action` is NOT `attached`, i.e. `task` or
    `review` per migration 0021's CHECK, over an `ai_runs.worker_type =
    'classify_inquiry'` extraction). A board Done moves `promote.InquiryOutcomes`
    by exactly `TruePositive +1`, with `FalsePositive`, `MisClick` and `Excluded`
    unchanged, measured as a before/after delta (the table is global). An
    `attached` promotion counts `Excluded` whatever the task's status
    (`outcomes.go`), so seeding one would prove nothing. The pure half,
    `InquiryOutcome("closed", nil) == "true_positive"`, is already pinned by
    `TestInquiryOutcome`.
18. Rendering: on one board page carrying a human task and a claude task, the
    human row has `action="/tasks/<id>/close"` and the claude row does not. Both
    rows still have `action="/tasks/<id>/dismiss"`.
19. The surface is unchanged: no tool is registered (`allToolNames` in
    `internal/tools/tools_unit_test.go` unchanged), `closeArgs` and
    `validateClose` are unchanged, `internal/mcpserver/schemas.go` is unchanged,
    and there is no migration (the ledger guard in
    `internal/classify/structure_test.go` is untouched).

## Data model changes

None. No migration. `tasks`, `task_events`, `task_claims`, `task_dismissals`,
`audit_events` and `policy_decisions` are written or read exactly as
`task_close` and the executor already do.

## API / MCP tool changes

- **Tools: none.** `task_close {task_id, reason}` is reused as is. It is
  registered in `tools.Register` (`internal/tools/createtask.go`); the executor
  path is validate (`validateClose`) → policy (`policy.Matrix`:
  `mcpHumanOnly` → `Decide` → static fallback, criterion 9) → audit start →
  `closeTask` (`closeTransition` under `SELECT … FOR UPDATE`) → audit complete.
- **New dashboard route:** `POST /tasks/{id}/close` (auth-required). Form
  fields: `note` (optional) and the four filter keys. Response: `303` to the
  filtered board with `flash`. No GET route and no JSON API.

## MQTT topics

None. Nothing is published or subscribed, and there is no LWT. (The close writes
a `task_events` row whose NOTIFY wakes the orchestrator's drain; that is
Postgres, not MQTT, and unchanged.)

## Does switchboard have an in-progress status? (the owner's second question)

**Yes, since migration 0001.** `tasks.status` has a CHECK over
`holding, ready, claimed, in_progress, needs_feedback, pr_open, awaiting_ci,
awaiting_merge, done_locally, delivered, closed, blocked`
(`migrations/0001_initial.sql:123-126`). The board already has an `in_progress`
column (`boardStatusOrder` in `board.go`).

**How a task gets there. There is exactly one writer:** `task_context`
(`internal/tools/taskcontext.go:69-95`). When the caller's `worker_id` holds the
task's ACTIVE claim and the status is `claimed` or `needs_feedback`, fetching the
context flips it to `in_progress` under the row lock. By design there is no
separate `task_start` tool. The claim comes first, from `task_claim`
(`claim.go`): `ready` → `claimed`, plus a `task_claims` row with
`expires_at = now() + ClaimTTL` (2h). In practice that is a worker console's
loop, or a manual `/task N` session on this repo's full `ops` MCP. (The
user-scope `ops-mcp-user` profile has neither `task_claim` nor `task_context`.)
Nothing on the board sets it. Per the owner's count on 2026-09-14, all 8 open
production tasks are human-assigned and `ready`.

**How a task leaves `in_progress`:**
- `mark_done_local`: `done_locally`, releases the claim, emits `done_local`.
- `request_feedback`: `needs_feedback`.
- `task_release`: `ready`, releases the claim.
- `task_pr_transition`: `pr_open` and on; the claim is NOT released.
- R6 claim expiry: after 2h without release, `task_release` puts it back to
  `ready` with reason `claim expired (orchestrator sweep)`.

**Would a board "Start" (→ `in_progress`) be the same pattern? Not cheaply, so it
is OUT OF SCOPE.** `claimed` and `in_progress` mean a worker holds a claim row,
and every verb and rule around them assumes that row exists. The two possible
shapes both break something:

1. **Start as a bare status write** (`ready` → `in_progress`, no claim). This
   produces an in-progress task with no `task_claims` row. That is the shape
   `validateReopen`'s comment calls one "nothing else in the spine can produce".
   It is also a dead end:
   - `task_close` and `task_dismiss` refuse it (active work), so the new Done
     button would refuse the task Salvador just started.
   - `mark_done_local` and `task_release` both require `activeClaim(task,
     worker_id)` and fail.

   The only way out would be psql.
2. **Start as a real claim** (`task_claim` + the `task_context` flip as
   `dashboard:{user}`):
   - R6 sweeps expired `claimed`/`in_progress` claims, so a task started on the
     board silently goes back to `ready` two hours later.
   - Finishing it needs `mark_done_local`, which emits `done_local`. R3 then
     creates a human `Deliver #N` task for every project whose delivery mode is
     not `console`, so each board-started task would spawn a Deliver task.
   - Calling `task_context`, a read, for its side effect from the dashboard
     abuses the read.

A Start verb would first need three decisions: the dashboard's claim identity, a
lease or R6 exemption for human claims (R6 already exempts `needs_feedback` for
the parked case), and how it finishes (Done accepting `in_progress` for its own
holder, or a done path that skips R3 for human work). That is a ticket of its
own (Future work).

## Files likely to touch

- `internal/dashboard/templates/tasks.html`: the Done form (criteria 1-4).
- `internal/dashboard/board.go`: `closeTaskAction`; D5's shared filter-rebuild
  helper, which `dismissTaskAction` also calls.
- `internal/dashboard/server.go`: the route registration (criterion 5). No
  change to `executeTask`, which already sets `Call.TaskID`.
- `internal/dashboard/board_structure_test.go`: extended (see Tests).
- `internal/dashboard/board_close_test.go` (new, unit, package `dashboard`): the
  handler tests with `captureExec`.
- `internal/dashboard/board_close_integration_test.go` (new, `//go:build
  integration`): the route end to end.

Deliberately NOT touched: `internal/tools/close.go`, `internal/policy/*`,
`internal/mcpserver/*`, `internal/orchestrator/*`, `internal/promote/*`,
`migrations/`, `templates/task.html` (the detail page), and the `/funnel`
templates and handlers.

## Tests to write

### Unit / structure (`go test ./...`, no db)

Extend `internal/dashboard/board_structure_test.go`:
- **Done form present:** `tasks.html` contains `action="/tasks/{{.ID}}/close"`,
  a `<button>Done</button>` and, inside that form, `name="note"`.
- **Human-only conditional:** `{{if eq .AssigneeType "human"}}` occurs exactly
  once. The text between it and its closing `{{end}}` contains `/close` and does
  NOT contain `/dismiss`, so the Dismiss form did not get swept into the
  conditional.
- **No status conditional:** `eq .Status` occurs nowhere in `tasks.html`. This
  generalizes the existing banned-literal loop, which stays in place.
- **One onchange:** the existing count assertion is unchanged. Re-run it, do not
  duplicate it.
- **Route registered:** a regexp over `server.go` for
  `mux\.Handle\("POST /tasks/\{id\}/close",\s*s\.auth\.Require\(`, copying the
  `deliveries_structure_test.go` reject-route check.
- **No SQL in the handler:** slice `closeTaskAction`'s body with `go/ast`, as
  `board_reopen_structure_test.go` slices `boardQuery`. Assert it contains
  `"task_close"` and `executeTask`, and does not contain `s.pool`,
  `"task_dismiss"` or `RawQuery`.

New `internal/dashboard/board_close_test.go`, package `dashboard`, reusing
`captureExec` from `deliveries_structure_test.go`
(`s := &Server{ex: ex, auth: auth}`, `req.SetPathValue("id", …)`):
- **Args shape:** a table over the note `""`, `"   "`, `"shipped in 1.4"` and
  the injection note of criterion 7. Assert exactly one `executor.Call`,
  `Tool == "task_close"`, `Actor` starting `dashboard:`, `*TaskID` equal to the
  path id, args unmarshalling to exactly the keys `{task_id, reason}`, and the
  reason exactly `done on the board` / `done on the board: shipped in 1.4`.
- **Bad id:** `abc`, `0` and `-3` each give a 400 and ZERO calls.
- **Redirect:** `303`, `Location` path `/tasks`, and the four posted filter
  values plus `flash=task_close ok` round-trip. An extra posted key (for example
  `next=//evil`) does not appear in the `Location`.

### Integration (`make integration`, own database)

New `internal/dashboard/board_close_integration_test.go`, joining the dashboard
suite's pact (`dashGuard`, `dashPool`, `newDashServer`, `get`, `snippet` from
`dashboard_integration_test.go`). It uses its own prefix `itest-done-%` and its
own cleanup, in FK order:
`task_claims` → `task_dismissals` → `classify_promotions` → `task_events` →
`task_dependencies` → `policy_decisions` (by `audit_event_id` of the
task's audit rows) → `audit_events` (by `task_id`) → `tasks` → `ai_extractions`
→ `ai_runs` → `normalized_messages` → `normalized_threads` →
`raw_source_items` → `projects` → `source_accounts`.
`audit_events.task_id` has no cascade, and this route fills it. That is the
SWT-31/SWT-37 landmine.

Cover criteria 10-18:
- `TestBoardDone_Integration_ClosesAuditsAndWritesNoLabel`: criteria 10, 11,
  12, 15.
- `TestBoardDone_Integration_RefusesActiveWork`: criterion 13, as a table over
  the three statuses, each with a seeded unreleased claim.
- `TestBoardDone_Integration_AlreadyClosedIsANoOp`: criterion 14.
- `TestBoardDone_Integration_DependentsAreTheOrchestratorsJob`: criterion 16.
- `TestBoardDone_Integration_InquiryTruePositive`: criterion 17. It imports
  `internal/promote` for `InquiryOutcomes` and seeds the promotion as
  `board_dismiss_integration_test.go`'s `seedDismiss` does, with
  `ai_runs.worker_type='classify_inquiry'` and an action other than `attached`.
- `TestBoardDone_Integration_OnlyHumanRowsRenderDone`: criterion 18.

**Mutations that must turn a test red** (check each before review):
- Handler calls `task_dismiss` → criteria 11/12 go red (and validation fails on
  the missing `reason_code`).
- `executeTo` instead of `executeTask` → criterion 10 (NULL `audit_events.task_id`).
- Drop the template conditional → criterion 18 and the structure test.
- Build args by `fmt.Sprintf` → criterion 7.
- Add `task_close` to `humanOnly` → no dashboard test goes red (the dashboard is
  human). `TestDecide_TaskClose_StaysCallableByTheOrchestrator` and
  `TestMatrix_MCPHumanOnly_ThroughCheck` must, which is why criterion 9 cites them.

## In scope / Out of scope

**In scope:** the Done form on the board row (human rows only), the
`POST /tasks/{id}/close` route and handler, D5's shared filter-rebuild helper,
the structure/unit/integration tests above, and this SPEC's answer on
`in_progress`.

**Out of scope, named because each is a tempting bundle:**
- **A board "Start" (→ `in_progress`) verb.** See the section above: it needs a
  claim identity, an R6 decision and a finish path.
- **Any change to `task_close`:** its args, its refusal set, its
  dependent-unblocking (orchestrator R5 on the event), or a
  `require_assignee_type` narrowing arg (D2's hard gate).
- **Done on `/tasks/{id}`.** Same reason SWT-31 kept Dismiss off it: a redirect
  target decision this ticket does not make.
- **Anything on `/funnel`.** That page is a window, not a control, and registers
  no POST route (`funnel_test.go` pins that).
- **Other board verbs:** Delivered (`task_mark_delivered`), Answer feedback
  (`answer_feedback`), Reopen (`task_reopen`), priority (`task_set_priority`),
  edit title.
- **Releasing or reaping claims left by pr-phase closes** (the D2 finding). That
  is pre-existing, reachable today through `opsctl`/MCP, and not made reachable
  from the board by this ticket.
- **CSRF tokens on dashboard POST forms.** This is pre-existing for every
  dashboard verb, and the dashboard is port-forward-only with no Ingress.

## Invariants that apply

1. **Raw-first:** not exercised. Nothing is ingested or normalized, and no code
   here reads `raw_source_items`.
2. **One funnel:** no new table. Done is a status transition on the one `tasks`
   table, and the closed task stays a row reachable through the `?status=closed`
   FILTER. Specifically, Done writes NO `task_dismissals` row
   (criterion 12): a finished task is not a labelled negative, and
   `InquiryOutcome` counts it as a true positive precisely because the
   dismissal row is absent.
3. **Everything through the executor:** the handler makes exactly one
   `s.ex.Execute` call (via `executeTask`), with actor `dashboard:{user}` and
   `Call.TaskID` set: validate → policy → audit start → `closeTask` → audit
   complete. It runs no SQL, performs no pre-check of its own and makes no
   direct `tasks` UPDATE (structure test plus criterion 10). The human-only rule
   is a rendering choice, not a second gate outside the executor (D2).
4. **Nothing external without a delivery row:** nothing outbound, no
   `deliveries` row created, read or mutated, and no adapter imported.
   `closeTransition`'s live-send fence still applies: a task with a `sending`,
   unsettled delivery inside `sendAttemptLease` refuses with
   `refusing to close active work`, and that surfaces as the flash.
5. **Own-message loop closure:** untouched. No normalizer or capture path
   changes.
6. **Stealth attribution:** nothing client-visible is produced. The reason is
   Salvador's own note plus a fixed prefix, stored and never sent.
7. **Orchestrator purity:** the orchestrator is not modified. It sees an
   ordinary `status_changed → closed` and runs `ruleUnblockDependents`, as it
   does for every close. The executor audits the close. No rule learns that the
   close came from the board.

## Sibling patterns to copy

- **The verb itself:** `dismissTaskAction` in `internal/dashboard/board.go`,
  including the parse, `json.Marshal`, filter rebuild and `executeTask`. Done is
  the same function with a different tool and args.
- **The per-row form:** the existing Dismiss form in `tasks.html` (hidden
  filters, `form.inline`, plain POST → 303 → flash; no HTMX, SWT-31 D8).
- **Handler unit tests without a db:** `captureExec` and `&Server{ex: ex, auth:
  auth}` in `internal/dashboard/deliveries_structure_test.go`.
- **Route-registration regexp:** the reject-route check in the same file.
- **Function-body slicing:** `internal/dashboard/board_reopen_structure_test.go`.
- **Integration harness, cleanup pact and `bdDismiss`-style POST helper:**
  `internal/dashboard/board_dismiss_integration_test.go`.
- **InquiryOutcomes delta measurement:** `internal/promote/inquiry_integration_test.go`
  (around lines 1380-1452).
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used. The close takes one
  `SELECT … FOR UPDATE` row lock inside `closeTransition`. That lock is also
  what makes a race with a worker's `task_claim` safe:
  - The claim is `SELECT … WHERE status='ready' FOR UPDATE SKIP LOCKED`
    (`claim.go:55`).
  - If the close wins, the claim finds no ready row and fails fast.
  - If the claim wins, the close sees `claimed` and refuses.

## Verification protocol

Run in this order; do not commit before step 4 passes.

1. `go test ./...`: the structure tests (criteria 1-5, 7 and the handler
   slice), the handler unit tests (6-8) and the unchanged policy suites
   (criterion 9: `TestMatrix_MCPHumanOnly_ThroughCheck`,
   `TestDecide_TaskClose_StaysCallableByTheOrchestrator`, `mcpverbs_internal_test`).
2. **Integration in a branch-owned database.** The compose Postgres is shared by
   every worktree (the INSTITUTIONAL_KNOWLEDGE.md landmine of 2026-09-12):
   ```
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_boarddone"
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_boarddone?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boarddone?sslmode=disable' \
     go test -tags integration -p 1 -count=1 ./internal/dashboard/ ./internal/policy/ ./internal/tools/
   ```
   Then run the full `go test -tags integration -p 1 ./...` against the same
   URL. The suite must be rerunnable: run it twice.
3. Run each mutation from "Tests to write" once and watch it go red.
4. **Manual smoke against the local db** (no migration is involved, so no
   pg-main step): `DATABASE_URL=<ops_boarddone url> go run ./cmd/dashboard`
   (:8085, dev-login). Seed one human `ready` task, one human `in_progress` task
   with a claim, and one claude `ready` task in a throwaway project. Then check:
   - The claude row shows Dismiss but no Done; the human rows show both.
   - Done with the note `smoke`: the flash is `task_close ok`, the row is gone,
     and it shows under `?status=closed`. `/tasks/{id}` shows the
     `status_changed` event with reason `done on the board: smoke`.
   - Done on the `in_progress` row: the flash reads
     `… refusing to close active work`, and the row stays.
   - Run
     `psql <url> -c "SELECT tool, actor, status, task_id FROM audit_events WHERE tool='task_close' ORDER BY id DESC LIMIT 2"`
     and
     `psql <url> -c "SELECT count(*) FROM task_dismissals WHERE task_id=<id>"`
     → a `dashboard:salvo` row with the task id, and `0`.
5. **Production** is a new dashboard image only (no migration, no other
   workload). Build and push the image here, and hand the manifest tag bump to
   the kube session (`kube/switchboard/dashboard.yaml`; that session owns kube
   manifests). After the roll: `kubectl -n ops port-forward svc/dashboard 8085:80`,
   click Done on one task Salvador confirms is actually finished, and repeat step
   4's two psql checks against `psql -h 192.168.50.49 -U ops -d ops`. Record the
   task id in the delivery summary. A mis-click is undone with
   `opsctl call task_reopen '{"task_id":N,"reason":"board Done mis-click"}'`:
   a plain reopen goes to `ready` and surfaces the task (SWT-45 J8).

## Future work (not this ticket)

- **Hard assignee gate:** an optional `require_assignee_type` on `task_close`,
  checked under `closeTransition`'s row lock (SWT-38's pin shape). The dashboard
  would send `"human"`, turning D2's rendering rule into an audited refusal.
  Only worth it if a board verb ever needs to act on claude rows.
- **Claims stranded by pr-phase closes:** `task_close` on
  `pr_open`/`awaiting_*` leaves the claim unreleased, and R6 never loads it.
  Either release open claims in `closeTransition` or refuse those statuses. This
  needs the R9-R11 lifecycle analysis.
- **A board Start verb:** the three decisions in the in-progress section.
- **Board Answer and Delivered verbs**, so that closing a `Answer feedback` or
  `Deliver #N` row stops being the only board action on them (D6).
- **Done on `/tasks/{id}`**, once its redirect target is decided (shared with
  SWT-31's deferred Dismiss there).
