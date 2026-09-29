> Jira: SWT-99

# demo-mode — a read filter on the web dashboard for a client demo

## Source

Ad-hoc, Salvador 2026-09-29 (swb task 841): Esteban (Avviato) asked for a live demo of
switchboard. The dashboard also holds Foundry and other Upwork clients, personal banking/medical/HOA
mail and other income, none of which may be disclosed. Build a demo mode for the web dashboard.
Decisions already made by Salvador (not reopened here):

1. It is switched on and off **only in the database**. There is no UI control and no indicator, and
   nothing on any page shows that a demo mode exists or that anything is hidden.
2. Visibility is **by project, as an allowlist**. Visible: `collaboratory`, `a-millon`, `reengine`,
   `switchboard`, `homelab`. Everything else is hidden (`foundry`, `ahs`, `saka`, `town-ai`,
   `personal`, `bulk`, `smoke`), and so is any project added later (fail closed). `personal`
   (banking, medical, HOA mail) is hidden with no exceptions (confirmed 2026-09-29).
3. **Every dashboard surface is filtered.** No page is hidden outright.
4. Web dashboard only. MCP, consoles, orchestrator, connectors and swb-push are unchanged, and
   ingestion and all real behaviour continue while demo mode is on.
5. Sharing the repo is a separate question, recorded in OPEN_QUESTIONS (Q3), and is out of scope here.
6. **Q1 answered (b), 2026-09-29:** a per-task hide override, `tasks.demo_hidden`, for tasks in
   visible projects whose text names hidden things. It is set by psql from a runbook audit query.
7. **Q2 answered (a), 2026-09-29:** the demo shows the Claude workers as they are ("they know it's
   ai driven, it is the point"). Nothing is relabelled.

## Goal

When the `ops_flags` row `demo_mode` is on, every dashboard response, including the SSE-driven
board swaps, the exports and the POST verbs, behaves as if only these existed: the allowlisted
projects' tasks not marked `demo_hidden` (nor under a marked ancestor), the allowlisted source
accounts, and their rows. The setting is read on every request, so no restart is needed.

**Usable alone:** Salvador runs the runbook's audit and marks the hits `demo_hidden`, then runs
one `UPDATE ops_flags …` in psql. He opens the dashboard on the tablet or a shared screen, and
every page shows only Avviato, switchboard and homelab work. A second `UPDATE` restores the full
view. With demo off, `demo_hidden` has no effect anywhere, and nothing else in the system notices
either change.

## Storage decision: `ops_flags` for the switch and allowlists, a column for per-task overrides

The switch is `ops_flags(name='demo_mode', value jsonb)`:

```json
{"on": true,
 "projects": ["collaboratory", "a-millon", "reengine", "switchboard", "homelab"],
 "source_accounts": ["<account_email>", "..."]}
```

Why the flag holds the project and account allowlists, and not a new `projects.demo_visible`
column:

- **One row flips everything at once.** The switch and both allowlists change in a single
  statement. There is no state where the switch is on while a list in another table is stale.
- **Fail-closed by absence.** A project created later is not in the list, so it is hidden, and
  nobody has to remember a column default. A mistyped or renamed slug hides that project, which
  errs toward privacy.
- **The kill switch is unaffected.** The only reader of `ops_flags` today is the kill switch, and
  `internal/policy/pgloader.go` and `internal/tools/delivery.go` read it **by name**
  (`name='sending_frozen'`). A second row does not change what they do. The line in
  classify-promotion_SPEC ("ops_flags stays … the global send kill switch") was about promotion
  eligibility and does not rule out another named flag.

The **per-task override** is a column, `tasks.demo_hidden` (migration 0048, Q1(b)). It is per row
and lasts across demos, so it belongs on the row it hides, not in a list of ids inside the flag. It
can only ever hide: its default is `false`, and it never makes a hidden-project task visible.

**Decode rules (D1). Every ambiguous case fails closed:**

| Row state | Scope |
|---|---|
| no `demo_mode` row | off (the full dashboard, exactly as today) |
| value decodes and `"on": false` | off |
| value decodes and `"on": true` | on, with the lists as given; a missing list means empty |
| `on` missing, not a bool, or value fails to decode | **on, with empty lists** (nothing visible) |
| the flag read errors | **503**, no body data (never "off") |

Account entries are compared case-insensitively (`lower()` on both sides). Slugs are compared
exactly.

## Acceptance criteria

Unit tests have no DB. Integration tests are build tag `integration` and use a branch-owned
database per the IK compose landmine (`ops_demo_mode`). Each integration test deletes the
`demo_mode` row at start and in `t.Cleanup`, and resets `demo_hidden` on its own fixture tasks.

**Scope and flag**

1. `decodeDemoScope(raw []byte, present bool)` implements the D1 table exactly. There is one
   table-driven unit test per row, including `{"on":"yes"}`, `{"on":true,"projects":5}`, `null`,
   and `{}`, all of which decode to on with empty lists.
2. The scope is loaded **once per request** by a wrapper around the authenticated routes in
   `Handler()`, and handlers read it from the request context. A handler that finds no scope in
   its context treats demo as **on with empty lists**. A unit test calls a handler without the
   wrapper and gets no rows.
3. Integration test: request `/tasks` with the row absent and see the hidden sentinel task. Insert
   the row with `on:true`; the **next** request, on the same `Server` with no restart, does not
   show it. Set `on:false`; the next request shows it again.
4. `/healthz`, `/static/*` and `/dev/login` never read the flag.

**The one predicate**

5. `internal/dashboard/demo.go` is the only place visibility is spelled. `demoSQL(sc, q, args)`
   expands markers into bound fragments. Each marker is invalid SQL if left unexpanded, so a
   forgotten expansion fails loudly.
   - `@demo.project(p)`: `(NOT $on OR p.slug = ANY($projects))`.
   - `@demo.task(t)` requires all three of:
     - the task's project passes `@demo.project`;
     - `t.title NOT LIKE 'Morning brief %'`. That LIKE pattern is R7's dedup key and must come
       from a single const shared with `listBriefs`;
     - **neither `t` nor any ancestor on its `parent_id` chain has `demo_hidden`** (D9). This is
       one correlated `WITH RECURSIVE` walk up `parent_id`, using `UNION` (not `UNION ALL`) so a
       corrupt cycle terminates.
   - `@demo.delivery(d)`: `d.channel <> 'upwork_chat'` AND the delivery's task passes
     `@demo.task`.
   - `@demo.account(a)`: `lower(a.account_email) = ANY($accounts)`.
   - `@demo.message(nm)`: `nm.channel <> 'upwork'` AND the message's raw item belongs to an
     account that passes `@demo.account`.
   - `@demo.ref(er)`: `er.system <> 'upwork_crm'`.

   Every fragment is wrapped in `(NOT $on OR …)`, so demo-off SQL runs the **same text** with
   `$on=false`, and `demo_hidden` has no effect when demo is off. There is no code path that skips
   the predicate. The Go-side twin `sc.accountVisible(email)` lives in the same file, for
   addresses that arrive in Go (`ResolveGmailRoute`).
6. Unit test: `demoSQL` numbers its binds after the caller's (`$4…` when three args are already
   bound). It binds each scope value once per statement, even when a statement has several
   markers. It returns an error on an unknown marker.
7. **Structure test** (`demo_structure_test.go`, precedent: `boardSQLTables` in `live_test.go`).
   Parse every non-test `.go` file in `internal/dashboard`. Every string literal that
   `FROM`/`JOIN`s a guarded table (`tasks`, `deliveries`, `plan_imports`, `projects`,
   `source_accounts`, `raw_source_items`, `normalized_messages`, `slack_watch`, `sync_runs`,
   `task_events`, `feedback_requests`, `external_refs`, `task_dependencies`, `task_dismissals`,
   `classify_promotions`, `capture_decisions`, `ai_extractions`) must either contain an
   `@demo.` marker or sit in a function listed in `demoExempt` (func name to one-line reason).
   - The test also fails on a `demoExempt` entry whose function no longer exists.
   - CONTROL: the scan must find at least 15 guarded literals. Otherwise it is not reading the
     code.
   - The only allowed exemptions are the task-page sub-reads keyed on an id that already passed
     `@demo.task` (`task_events`, `feedback_requests`), `reopenMarkers` (its ids come from
     `boardRows`), and `resolveSourceSQL`/`threadSQL` if the message marker is applied at the
     outer SELECT instead.
8. **Seam guard.** In the same test, every call from a dashboard file to another package that
   passes `s.pool` (`orchestrator.Health`, `capture.AttributionTrend`, `classify.Summarize`,
   `promote.CountersByLane`, `availability.CalendarSyncStates`, `tools.ResolveGmailRoute`) must
   be named in `demoSeams` (callee to reason). A new pool-taking seam that is not listed fails the
   test.

**Board, stream and kiosk**

9. With demo on, a hidden project's task (open, ready, in flight, needs-input, done today,
   `?status=closed`) is absent from:
   - `/tasks`, in every section, tally, the `counts` footer, and every `data-live` region;
   - `/tasks?project=<hidden slug>`, which renders exactly like
     `/tasks?project=<nonexistent slug>`;
   - the project `<select>`;
   - `/kiosk`'s framed URL render.
10. `boardQuery` and **both** `boardLightFacts` statements carry `@demo.task`. Queue-head
    candidates are therefore picked among visible ready tasks. This amends SWT-52 D2 **in demo
    mode only**; with demo off, the candidate set is unchanged. The facts' `LEFT JOIN
    normalized_messages` carries `@demo.message`, so an activity sender from a hidden account
    renders blank.
11. With demo on, the board never renders `OrchAlert` (its backlog, cursor and head are global
    event volume). `/funnel` shows the orchestrator verdict and running state only (see 24).
12. `/tasks/stream` is unchanged: it carries no data and runs no SQL. A broadcast caused by a
    hidden row re-fetches a filtered render identical to the one on screen. Flipping the flag
    takes effect on the next change broadcast or the 60 s tick. `ops_flags` goes in
    `boardTickOnlyTables` **only if** a board function reads it. The wrapper design means none
    does, and the existing coverage test is the arbiter.
    - Setting `demo_hidden` is a `tasks` UPDATE, so 0044's trigger fires and an open demo board
      drops the row within one coalesced broadcast.
    - `boolean` has an equality operator, so `TestBoardLive_Integration_NoColumnTypeWithoutEquality`
      stays green.
13. `/export/tasks.csv` and `/export/tasks.json` omit hidden tasks, because they share
    `boardRows`. The header and column set are unchanged; `demo_hidden` is **not** an export
    column.

**Task page**

14. `/tasks/{id}` for a hidden task returns the **same status and body** as `/tasks/999999999`.
    The integration test compares the two bodies byte for byte.
15. On a visible task:
    - Parent, children and dependencies omit hidden tasks.
    - `ParentLink` (the `#N` link) is set only when the parent passed `@demo.task`.
    - Deliveries pass `@demo.delivery`, so there are no `upwork_chat` rows.
    - Refs pass `@demo.ref`.
    - The source-message section and its thread siblings pass `@demo.message`. A message from a
      hidden account costs the page that section and nothing else.
    - No `/tasks/<hidden id>"` string appears anywhere in the body.
16. **Event and log rule (D2).** Events, logs and feedback on a **visible** task render unchanged.
    They are text written about that task, trusted exactly as far as its own title and body.
    Structured references to other tasks (parent, children, dependencies, attach) are filtered by
    15. Free text on a visible task that names hidden things is handled by **data, not code**: the
    runbook audit (see Runbook) finds such tasks, and `demo_hidden` removes them whole (28–31).
    The Attach form is a bare numeric input with no candidate list, so there are no candidate
    titles to filter.

**Verbs: refused exactly like not-found (D3)**

17. Every mutating route runs a **pre-executor visibility check**:
    `s.visibleTask(ctx, sc, id)`, `s.visibleDelivery`, or `s.visiblePlan`, each one `SELECT
    EXISTS(…)` carrying the marker.
    - It covers `/tasks/{id}/dismiss|close|requeue|attach` (both the source **and**
      `target_task_id`), `/deliveries/{id}/edit|approve|reject|send|mark-sent|mark-failed`, and
      `/plans/{id}/approve|reject`.
    - A false result, whether the id is hidden or nonexistent, redirects with the dashboard's own
      flash (`task #N not found`, `delivery #N not found`, `plan import #N not found`) to the same
      page the verb would have returned to. It **does not call the executor**, so no audit row is
      written.
    - The integration test posts each verb for a hidden id and for a nonexistent id. The
      `Location` headers must match byte for byte, and a recording fake `Exec` must see zero
      calls.
    - Non-numeric delivery and plan ids become a 400, as board verbs already do.
18. With demo **off**, every verb on an existing id reaches the executor unchanged. Existing verb
    tests stay green. A nonexistent id now gets the dashboard flash instead of the tool's error.
    This change is deliberate and applies in both modes, so the two modes never diverge.
19. `POST /flags/sending-frozen` is untouched. It names no row.

**Deliveries, plans, briefs, sources, funnel**

20. Demo on:
    - `/deliveries` (every `?status=`) passes `@demo.delivery` **in SQL before `LIMIT 100`**. With
      150 hidden rows newer than 3 visible ones, the page still shows the 3.
    - A visible gmail row whose resolved `From` fails `sc.accountVisible` renders `From` as
      `(unresolved)`.
21. `/plans` and `/plans/{id}` pass `@demo.project`. A hidden plan's detail page returns the same
    404 as a nonexistent id.
22. `/briefs` passes `@demo.task`, whose brief clause hides every `Morning brief %` task in demo
    mode, so the page renders its normal empty state. A brief's body summarises every project by
    slug, so the same clause hides brief tasks on the board, the task page and the exports too.
    R7 is off in production (orchestrator-deploy O3). This is the backstop for when it is turned
    back on.
23. `/sources`:
    - Account rows, the per-channel table and the headline totals pass `@demo.account` /
      `@demo.message`, and the totals are sums of **visible** rows only.
    - The Slack-watch panel passes a row only when its workspace account
      (`lower(workspace_id)||'@slack-web.local'`, the join `slackWatchRows` already uses) passes
      `@demo.account`.
    - The `upwork_crm` account, personal mailboxes and any account not listed never render.
24. `/funnel`:
    - Connector health and the intake trend (the per-account columns, raw totals and message
      counts) are restricted to `@demo.account` / `@demo.message`. The account column list comes
      from the same filtered set.
    - The capture attribution, classify shadow summary (which includes the **personal** lane and
      raw senders and subjects) and classify promotion loaders **do not run**, and their
      sections, headings and help text do not render.
    - The static help text naming `upwork`/`upworkcrm` and "personal mail" is inside the same
      non-demo branch.
    - The orchestrator section renders verdict and running state, without backlog, cursor or head
      numbers.
    - No count on the page includes a row from a hidden account.

**The leak crawl (the end-to-end guard)**

25. The integration fixture holds one visible project, one hidden project and one hidden source
    account.
    - Every hidden row carries the sentinel `ZZHIDDEN`: the project slug, task title and body,
      subproject, sender, subject, account email, capture rule name, Slack-watch label, plan
      source path and delivery body.
    - There is one `upwork_chat` delivery on a **visible** task.
    - In the **visible** project, there is a `demo_hidden` task with a `ZZHIDDEN` title, plus a
      child and a grandchild of it, neither of which is flagged. Each has a `ZZHIDDEN` title,
      one delivery and one event.

    The crawl requests, with demo on:
    - `/tasks` and every `?status=` value;
    - `/tasks?project=<hidden>`;
    - `/kiosk`;
    - `/tasks/<every fixture id>`;
    - `/deliveries` and every `?status=`;
    - `/plans` and `/plans/<both ids>`;
    - `/briefs`, `/sources`, `/funnel?days=90`;
    - both exports;
    - every POST verb on hidden ids, following the redirect.

    No body or `Location` contains `ZZHIDDEN`, `upwork` (case-insensitive), `/tasks/<hidden id>"`,
    or `/plans/<hidden id>"`.
26. **CONTROL, same fixture, demo off:** the crawl **does** find `ZZHIDDEN` on `/tasks`,
    `/deliveries`, `/sources`, `/funnel` and `/plans`, including the `demo_hidden` task and its
    descendants on `/tasks`. Otherwise 25 is vacuous (see memory: test the column, not the
    fixture).
27. No page renders the words "demo", "hidden" or "filtered", or a hidden-row count. The crawl
    asserts the first two case-insensitively on every demo-on body, allowing only the known
    static occurrences that exist in the demo-off render too.

**The per-task override (`tasks.demo_hidden`, Q1(b))**

28. Migration `0048_tasks_demo_hidden.sql`:
    - Forward-only: `ALTER TABLE tasks ADD COLUMN demo_hidden boolean NOT NULL DEFAULT false;`
      with no index (the walk is by `parent_id`, and `tasks` is in the low thousands).
    - Its header names this SPEC and states that only psql writes the column.
    - It is registered in **both** migration ledgers, the way 0047 was. Add `&& n != 48` plus a
      comment line in `internal/classify/structure_test.go` (~line 1256), and `&& v != 48` plus
      an "AMENDED — not deleted" comment in `internal/tools/signal_structure_test.go` (~line 104).
      Both ledger tests stay green, and both go red if either registration is removed.
29. With demo on, a `demo_hidden` task in a **visible** project is absent everywhere that criteria
    9, 13, 14, 15, 20 and 22 list for a hidden-project task. So are all its descendants:
    - the board, with every section, tally and count;
    - the exports;
    - the task page, which returns the not-found 404;
    - a visible parent's children list, and a visible task's dependencies;
    - `/deliveries`, for their deliveries;
    - the crawl (25).

    A child created **after** the flag was set is hidden without being flagged itself (D9).
30. With demo on, every verb in 17 on a `demo_hidden` task or any of its descendants, or with one
    as the attach target, is refused exactly like a nonexistent id. This covers a delivery whose
    task is hidden this way. The `Location` headers match, and the executor sees zero calls.
31. **The dashboard never writes `demo_hidden`.**
    - A structure test scans every non-test `.go` file in the repo. `demo_hidden` may appear only
      in `internal/dashboard/demo.go`, and there only inside the `@demo.task` fragment.
    - No `UPDATE`/`INSERT` literal anywhere names it, and no tool, MCP schema, form field or
      handler takes it.
    - The runbook's psql is its only writer.
    - With demo **off**, a `demo_hidden` task renders and acts exactly as it does today (covered by
      the 26 control, and by one verb test that reaches the executor).

**Mutations: each must turn a named test red, and the implementer runs each once**

| # | Mutation | Must fail |
|---|---|---|
| M1 | delete `@demo.task` from `boardQuery` | 7, 9, 25 |
| M2 | loader returns off regardless of the row | 3, 25 |
| M3 | decode error → off instead of on | 1 |
| M4 | drop the pre-check in `closeTaskAction` | 17 |
| M5 | drop the attach **target** check | 17 |
| M6 | drop `d.channel <> 'upwork_chat'` | 20, 25 |
| M7 | drop the `Morning brief` clause | 22, 25 |
| M8 | run the capture-attribution loader in demo | 24, 25 |
| M9 | remove `@demo.account` from `slackWatchRows` | 7, 23, 25 |
| M10 | set `ParentLink` from the raw `parent_id` | 15, 25 |
| M11 | add a new `SELECT … FROM tasks` func with no marker | 7 |
| M12 | filter `/deliveries` in Go after `LIMIT` | 20 |
| M13 | drop the `demo_hidden` clause from `@demo.task` | 25, 29, 30 |
| M14 | check only `t.demo_hidden`, not ancestors | 25, 29 (the child and grandchild) |
| M15 | add `UPDATE tasks SET demo_hidden = …` in a dashboard handler | 31 |
| M16 | delete `&& n != 48` from the classify ledger | the classify migrations-ledger test |

## Data model changes

- **Migration 0048** (`migrations/0048_tasks_demo_hidden.sql`): `tasks.demo_hidden boolean NOT
  NULL DEFAULT false`.
  - Forward-only, and registered in both migration ledgers (criterion 28).
  - Written only by psql, per the runbook.
  - It extends `tasks`, the one funnel table. It is not a side table and not a synonym.
- **One new `ops_flags` row by name, `demo_mode`.** The dashboard never writes it, so there is no
  verb and no seed. The dashboard only SELECTs it.

## API / MCP tool changes

- No MCP tool, executor tool or policy entry is added or changed. No tool reads or writes
  `demo_hidden`, and `task_context`/`task_list` do not expose it.
- The dashboard's routes are unchanged. Visibility depends on the scope, and the verbs gain the
  pre-executor check from 17.
- The executor path for a **visible** row is byte-identical: `executeTask` / `executeTo` → the
  executor's validate → policy → audit → handler.

## MQTT topics

None.

## Runbook: `docs/runbooks/demo-mode.md` (new, short)

The **term list is data**. It lives in the runbook as a psql variable and is edited freely, and
no Go code carries it. Start with:
`foundry|upwork|lyle|grady|ahs|saka|town-ai|bank|medical|hoa|personal`. Add names as they
come up.

```sql
\set terms 'foundry|upwork|lyle|grady|ahs|saka|town-ai|bank|medical|hoa|personal'
\set visible '{collaboratory,a-millon,reengine,switchboard,homelab}'

-- 1. Audit: visible-project tasks whose own text names a term. Review before step 2.
SELECT t.id, p.slug, t.status, left(t.title, 80) AS title,
       (t.title ~* :'terms') AS in_title, (COALESCE(t.body,'') ~* :'terms') AS in_body,
       EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.payload::text ~* :'terms') AS in_events,
       EXISTS (SELECT 1 FROM feedback_requests f WHERE f.task_id = t.id
                 AND (f.question ~* :'terms' OR COALESCE(f.answer,'') ~* :'terms')) AS in_feedback
  FROM tasks t JOIN projects p ON p.id = t.project_id
 WHERE p.slug = ANY(:'visible'::text[]) AND NOT t.demo_hidden
   AND (t.title ~* :'terms' OR COALESCE(t.body,'') ~* :'terms'
        OR EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.payload::text ~* :'terms')
        OR EXISTS (SELECT 1 FROM feedback_requests f WHERE f.task_id = t.id
                     AND (f.question ~* :'terms' OR COALESCE(f.answer,'') ~* :'terms')))
 ORDER BY p.slug, t.id;

-- 2. Hide the hits (same WHERE). Descendants follow automatically (D9).
UPDATE tasks t SET demo_hidden = true
  FROM projects p
 WHERE p.id = t.project_id AND p.slug = ANY(:'visible'::text[]) AND NOT t.demo_hidden
   AND (t.title ~* :'terms' OR COALESCE(t.body,'') ~* :'terms'
        OR EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.payload::text ~* :'terms')
        OR EXISTS (SELECT 1 FROM feedback_requests f WHERE f.task_id = t.id
                     AND (f.question ~* :'terms' OR COALESCE(f.answer,'') ~* :'terms')));

-- Hand-pick instead:   UPDATE tasks SET demo_hidden = true  WHERE id IN (...);
-- Unhide one:          UPDATE tasks SET demo_hidden = false WHERE id = N;
```

The runbook also carries:
- the on/off SQL from the Verification protocol, step 4;
- the account-choosing query from step 5;
- a caveat that the audit is only as good as the term list. Before switching on, skim the demo
  board with demo on and mark anything the list missed.

## Files likely to touch

- `migrations/0048_tasks_demo_hidden.sql` (new).
- `internal/classify/structure_test.go` and `internal/tools/signal_structure_test.go`: the 0048
  ledger registrations.
- `internal/dashboard/demo.go` (new):
  - `demoScope`, `decodeDemoScope`, the per-request loader and context wrapper;
  - `demoSQL` and its markers, including the `demo_hidden` ancestor walk, and
    `sc.accountVisible`;
  - `visibleTask`, `visibleDelivery` and `visiblePlan`;
  - `demoExempt` and `demoSeams`.
- `internal/dashboard/server.go`:
  - `Handler()`: wrap the authenticated routes.
  - `listDeliveries`: add `@demo.delivery`, and apply `accountVisible` to the From.
  - `action`, `approveAction`, `actionEdit`, `actionReject`: the pre-check.
- `internal/dashboard/board.go`:
  - `boardQuery` and `boardLightFacts` (both statements plus the `nm` join);
  - `listTasks` (the project list and `OrchAlert`);
  - `showTask` (parent, children, dependencies, `ParentLink`, deliveries, refs, the
    `needsReviewSQL` read);
  - `listBriefs` (the shared brief const), `listPlans`, `showPlan`;
  - the four task verbs and `planAction`.
- `internal/dashboard/sourcemessage.go`: `resolveSourceSQL` and `threadSQL` / `loadSourceMessage`.
- `internal/dashboard/sources.go`, `internal/dashboard/slackwatch.go`.
- `internal/dashboard/funnel.go`: the per-section demo gates and the account-scoped SQL.
- `internal/dashboard/templates/funnel.html`: the non-demo branch for the omitted sections and
  their help text, and the orchestrator numbers.
- No change expected to `kiosk.go`, `export.go`, `live.go` (unless 12's coverage test demands it),
  `cmd/dashboard/main.go` or any template other than `funnel.html`.
- Tests:
  - `internal/dashboard/demo_test.go` (criteria 1, 2, 6);
  - `demo_structure_test.go` (7, 8, 31);
  - `demo_integration_test.go` (3, 9–30, with the crawl as a table of URLs).
- `docs/runbooks/demo-mode.md` (new): the section above.

## In scope / Out of scope

**In scope:** everything above: the web dashboard process, migration 0048 and the runbook.

**Out of scope:**
- MCP tools (both profiles), consoles and worker wrappers, the orchestrator, connectors, swb-push,
  and `opsctl`. Hidden rows keep flowing, sending and being worked. There is no `opsctl demo`
  verb and no `demo_hidden` verb: psql is the switch, per decision 1.
- A NOTIFY trigger on `ops_flags`. The 60 s tick is fast enough for a switch flipped before a
  demo.
- Redacting free text by keyword in code. `demo_hidden` removes whole tasks instead.
- Relabelling Claude lanes (Q2 answered: no).
- Repo sanitisation (Q3, a note only).
- The dashboard's own lack of CSRF tokens (pre-existing, SWT-31 note).
- Any other build-order step. Step 10's board is shipped; this does not reopen its layout.

## Invariants that apply

1. **Raw-first:** no writes. Demo mode never touches `raw_source_items` or normalization, and
   reprocessing is unaffected.
2. **One funnel:** demo is one more **filter** on the one `tasks` table, like queues. `demo_hidden`
   is a column on that table, not a side table or a copy. The allowlist lives beside the flag, not
   in a parallel project list.
3. **Everything through the executor:** the refusal happens **before** the executor, in the
   dashboard, through a shared `visible*` check. It does not go inside the executor. The
   executor, MCP and orchestrator must keep acting on hidden rows (decision 4), and a
   presentation scope has no place in policy. A refused POST never becomes a tool call, the same
   as `approveAction`'s missing-hash refusal. A visible row's path is unchanged. There is no new
   side door: the dashboard still runs no mutating SQL. `demo_hidden` and the `demo_mode` row are
   presentation data written by the operator in psql, never by a tool or the dashboard (31). Like
   the flag, they change no task state that any rule or worker reads.
4. **Nothing external without a delivery row:** no outbound path changes. The dashboard can no
   longer send a **hidden** delivery in demo mode. Auto-tier sends and other paths are unaffected.
5. **Own-message loop closure:** not touched.
6. **Stealth attribution:** the demo **intentionally shows the AI workers**: `assignee_type=claude`,
   the claude lanes, worker types and session names are rendered as they are (Q2(a), Salvador
   2026-09-29: "they know it's ai driven, it is the point"). Invariant 6 governs client-facing
   **artifacts**, meaning commits, PR text, Jira comments, email and Slack, and those are
   untouched. The demo is a disclosure Salvador chose, not a byline leaking into client work. No
   relabelling.
7. **Orchestrator is pure:** not touched. R7's title is reused only as a read-side key, through a
   shared const.

## Sibling patterns to copy

- The FROM/JOIN literal scan: `boardSQLTables` in `internal/dashboard/live_test.go`. The exemption
  map with one-line reasons follows `boardTickOnlyTables` in `live.go`.
- A structure scan with an allow-list: `internal/capture/gate_structure_test.go`.
- A pre-executor refusal that writes no audit row: `approveAction` in `server.go` (missing
  `content_hash`).
- Reading `ops_flags` by name with an absent row meaning the default: the `sending_frozen` read at
  `server.go:315`.
- Migration-ledger registration: 0047's entries in `internal/classify/structure_test.go` and
  `internal/tools/signal_structure_test.go`.

## Verification protocol

1. `go test ./internal/dashboard/...` (unit and structure), then the full `go test ./...`, which
   includes both migration ledgers.
2. Integration in a branch database:
   `psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_demo_mode'`,
   `make migrate LOCAL_DB_URL=…/ops_demo_mode` (applies 0048), then
   `DATABASE_URL=…/ops_demo_mode go test -tags integration ./internal/dashboard/...`. Capture the
   exit status; do not pipe it into the commit (memory: gate commits on test exit status).
3. Run M1–M16 once each and record in the ticket which test went red.
4. Manual smoke (local dashboard against `ops_demo_mode`, `/dev/login`):
   ```sql
   INSERT INTO ops_flags (name, value) VALUES ('demo_mode',
     '{"on":true,"projects":["collaboratory","a-millon","reengine","switchboard","homelab"],"source_accounts":[]}')
   ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
   -- off:
   UPDATE ops_flags SET value = jsonb_set(value, '{on}', 'false'), updated_at = now() WHERE name = 'demo_mode';
   ```
   With a board tab open and streaming:
   - Flip on and confirm hidden rows leave within 60 s with no reload.
   - Set `demo_hidden` on a visible task and confirm it and its children leave within one
     broadcast.
   - Open a hidden task id and compare it against a nonexistent id.
5. **Before a real demo (runbook):**
   - Production needs 0048 applied (kube handoff) before the new image, because `@demo.task`
     reads the column on every render, demo on or off.
   - `SELECT slug FROM projects ORDER BY slug` confirms the five slugs are spelled as listed.
   - `SELECT id, provider, account_email FROM source_accounts ORDER BY provider` is the list to
     choose `source_accounts` from. List an account **only if all of its traffic is demo-safe**:
     never `upwork_crm`, never a personal mailbox, and never a mailbox that mixes personal with
     work mail (its counts cannot be split per message). Leaving an account out only costs
     /sources and /funnel rows, and the source-message section of tasks it fed.
   - Run the runbook audit, review it, and set `demo_hidden` on the hits.

## Decisions made unilaterally

- **D1:** storage and decode as above; unparseable means on and empty.
- **D2:** the event/log rule, text on a visible task is shown (criterion 16). Tasks whose text
  names hidden things are removed whole via `demo_hidden`.
- **D3:** refusal before the executor, and the not-found flash in both modes (17, 18).
- **D4:** Morning briefs are hidden as tasks in demo mode rather than having their body parsed
  (22). One predicate, with no format coupling to `renderBrief`.
- **D5:** queue heads are recomputed among visible tasks in demo mode (10).
- **D6:** messages are visible by **account**. `normalized_messages` has no project, so "visible
  projects' messages" is realised as messages from allowlisted accounts, one rule for /sources,
  /funnel, board senders and the task page.
- **D7:** the funnel's capture/classify/promotion sections are omitted in demo mode. Their SQL is in
  other packages, and they aggregate across every account, the personal lane included. The
  alternative, threading the scope through `capture`, `classify` and `promote`, widens a
  dashboard ticket into three packages.
- **D8:** the account allowlist is explicit emails, not "accounts that fed a visible project". A
  derived rule would expose a personal mailbox the day it fed one visible task.
- **D9: `demo_hidden` is inherited down the `parent_id` tree, evaluated at read time.**
  - A task is hidden if it or **any ancestor** is flagged.
  - Why: a child's title and body usually restate its parent's subject, and a child created later
    (`create_child_task`, a plan import) would otherwise appear on its own.
  - Why read time, not write time: setting the flag on descendants in the runbook UPDATE would miss
    those later children, which fails open.
  - The rule only hides, never unhides, and it does not propagate **up**: a flagged child leaves
    its parent visible.
  - Dependencies are not inherited. A visible task's dependency list simply omits the hidden one
    (15).
  - Cost: one correlated recursive walk per row over shallow trees in a table of a few thousand
    rows. Postgres does not promise to short-circuit `NOT $on OR …`, so assume the walk can run
    with demo off too. That is acceptable at this size. If `pg_stat_statements` ever shows it, add
    a partial index `ON tasks (id) WHERE demo_hidden`.

Open questions: all answered 2026-09-29 and folded in above (see `demo-mode_OPEN_QUESTIONS.md`);
Q3 remains an out-of-scope note.

## Future work

- `opsctl demo on|off` if psql proves clumsy.
- A NOTIFY on `ops_flags` for an instant flip.
- A sanitised demo **database** (a pg_dump filtered to the allowlist) as a stronger alternative to
  a read filter, if demos become regular.
- Repo sharing (Q3): a sanitised export repo is its own ticket if the question comes up.
