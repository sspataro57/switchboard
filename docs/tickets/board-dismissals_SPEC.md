> Jira: SWT-31

# board-dismissals — readable Upwork task titles, and dismissal as labelled data

**STATUS: ANSWERED** (Salvador, 2026-09-09; see
`docs/tickets/board-dismissals_OPEN_QUESTIONS.md`). Q1 = **message sender**,
falling back to project name when the source stored no sender. Criteria 2-4
below carry the answer; nothing else in this document changed.

## Source

Ad-hoc, from Salvador, verbatim:

> fix the upwork task titles. I want to be able to dismiss tasks right from the
> board and record the decision so we can improve the clasifier. A dismisal
> reason recording would help I think

Not a build-order step. It is maintenance on two things that shipped in the last
two days and are now LIVE: capture rules went live 2026-09-09 (40 tasks created,
including 2 upwork tasks on `saka`), and SWT-30 gave the classify lane a
promotion path into `tasks`. Both funnels now write to a board that has no verb
on it.

## Goal

Make capture-created Upwork tasks legible on the board, and give the board its
first verb — a dismissal that goes through the executor, closes the task, and
records a typed, machine-readable label joinable back to the classifier verdict
or capture rule that produced the task.

**Usable alone means:** with nothing else deployed, Salvador opens
`/tasks?project=saka`, reads a row that says who wrote what instead of a
128-character thread key, clicks *Dismiss* with a reason, the row disappears from
the board, and `SELECT ... FROM task_dismissals JOIN classify_promotions ...`
returns the dismissal alongside the stored verdict that caused it. Nothing
outbound happens at any point, and no worker, connector or orchestrator rule
changes behaviour.

## Decisions made unilaterally (with rationale)

- **D1 — the title branch keys on "the external key IS the thread key", not on
  `external_system = 'upwork_crm'`.** `capture.externalKey` (rules.go) returns
  the `thread_key` **verbatim** whenever a non-`body_regex` rule carries no
  `key_regex` — that is the entire cause of the defect, and it is not
  upwork-specific (a future slack/gmail `thread_key_prefix` rule has it too).
  Branching on the provider name would also put the literal `upwork_crm:` (or a
  provider constant) into `internal/capture`, and the one spelling of that key
  lives in `internal/connector/upworkcrm/threadkey.go` — the SWT-19 rule. The
  discriminator is a comparison of two values already in hand:
  `key == msg.ThreadKey && key != ""`. It discriminates on real data today
  (Jira keys are `WEB-123`, never a thread key), which is what keeps it out of
  the "predicate whose discriminating value is a constant in production" family.
- **D2 — the label is a typed table, `task_dismissals`, not a
  `task_events.payload` key.** `capture_decisions` (0015) and
  `classify_promotions` (0021) are the precedent, and both migrations state the
  reason: an untyped predicate over jsonb is the thing this repo keeps paying
  for. CLAUDE.md's own line — "Log every dashboard correction as labeled data" —
  wants something a future precision ticket can `GROUP BY`. The `status_changed`
  event still carries a *prose* reason, as it does for every other close;
  **nothing may query that payload for labels** (criterion 20).
- **D3 — a new `task_dismiss` tool, not extra args on `task_close`.** Reuse was
  the preference and it is honoured where it matters: **the refusal is not
  re-encoded** — `closeTask` and `dismissTask` call ONE shared, unexported
  transition helper in `internal/tools/close.go`. What forces a second verb is
  the policy gate: a dismissal is a human judgement used as training data, so it
  must be `policy.humanOnly`, and `task_close` cannot be — the orchestrator
  calls it as actor `orchestrator` (`internal/orchestrator/engine.go:17`) from
  R1, R8 and the feedback rules. Gating `task_close` on a human actor would
  break the orchestrator; leaving `task_close` open and letting it write labels
  would let any automated caller mint training data with no gate at all.
- **D4 — the reason enum is a CHECK constraint with Salvador's four values**
  (`not_actionable | wrong_kind | duplicate | handled_elsewhere`) plus an
  optional free-text `note`. A free-text `reason_code` is a column nothing can
  `GROUP BY` — the `kind`-enum argument from `internal/classify`'s schema,
  verbatim. Cost, stated: widening the set later is one more migration.
- **D5 — the five existing production titles are corrected by a documented
  one-off psql UPDATE** (Verification protocol, step 6). There is no
  title-editing tool in the repo (verified: no `UPDATE tasks SET title`
  anywhere outside `create_task`'s INSERT) and this ticket does not add one for
  five rows. Capture's live claim for those messages is spent — the partial
  unique index means no pass will ever revisit them — so nothing in code can
  ever re-title them either.
- **D6 — the dismiss control renders on every row the board renders**, and the
  refusal surfaces as the existing flash. The alternative (render only for
  `holding`/`ready`) restates `closeTask`'s status list in a template, where it
  would drift silently. One spelling of the refusal, in the handler; a visible
  error beats a hidden affordance.
- **D7 — no denormalised `ai_extraction_id` on the dismissal row.** The join key
  is `task_id`. A task can carry a `capture_decisions` row AND several
  `classify_promotions` rows (an `attached` promotion points at an existing
  task), so a single denormalised pointer would have to pick one arbitrarily and
  would then drift from the log it copied.
- **D8 — no HTMX, no partial swap.** The board has no HTMX today (plain GET
  filter form, POST → 303 → flash, as `/plans` and `/deliveries` do). Dismissal
  follows that shape; "the row disappears" is the redirect plus the board's
  existing `t.status <> 'closed'` default.

## Acceptance criteria

### Part 1 — titles

1. `ruleTaskTitle` renders `{key} — {head}` byte-identically to today for every
   task whose derived external key is NOT the message's thread key (Jira
   `body_regex` rules, any rule with a `key_regex`), including the 120-rune
   `textmatch.NormalizedPrefix` truncation and the subject → first-body-line
   fallback. A characterization unit test pins at least: a Jira-shaped key with
   a subject, a Jira-shaped key with no subject (body first line), and an
   over-length title that truncates.
2. When the derived external key equals the message's non-empty `thread_key`,
   the title is `{label} — {head}`, where `label` is the message's **sender**
   (Q1 answer) — the one fact the board row does not already show; the project
   is its own column. Unit test: a `thread_key_prefix` rule on
   `upwork_crm:{client}:room:{room}` with sender `Mario Cruz`, an empty
   subject and a body starting `Hi Salvador,` yields `Mario Cruz — Hi
   Salvador,`; the SAME rule with an empty sender yields `Saka — Hi Salvador,`
   (the project-name fallback).
3. Both label inputs come from COLUMNS. The sender is
   `pendingMessages`' existing `m.sender` (already selected); the FALLBACK is
   `projects.name`, so `loadRules`' SELECT gains `p.name` and `storedRule`
   gains the field. An **integration** test proves each column feeds the title
   — mutating either SELECT to a literal must turn it red (institutional
   landmine 6: "for any predicate whose input comes from a column, the
   regression test belongs in the integration suite").
4. Fallbacks, in order: sender → project name → project slug → the key
   itself. The title is never empty (`create_task` rejects an empty title —
   `validateCreateTask`), and the head-less case still yields a title.
5. `internal/capture` contains no `upwork_crm:` literal, no `ParseThreadKey`
   import and no key parsing of any kind; the branch is the equality of D1.
   Asserted in `internal/capture/rules_structure_test.go`.
6. `ruleTaskBody` is unchanged — `thread_key`, `sender`, `message_id` and the
   rule id stay in the body, so the identity dropped from the title is still
   one click away on `/tasks/{id}`, and `external_refs` still carries the key.

### Part 2 — dismissal

7. Migration `0022_task_dismissals.sql` creates `task_dismissals`
   (`id`, `task_id` FK → `tasks` **ON DELETE CASCADE**, `reason_code` CHECK in
   the four values, `note TEXT`, `dismissed_by TEXT NOT NULL`, `created_at`),
   with a **total** `UNIQUE (task_id)` and no other index. No status, no
   assignee, no claim — it is a log, not a second tasks table (invariant 2).
8. `task_dismiss` is registered in `tools.Register`; `validateDismiss` rejects
   `{}`, a missing/zero `task_id`, an empty `reason_code`, and any
   `reason_code` outside the enum (by name, both ways — the `create_task`
   `status` precedent).
9. `task_dismiss` is in `policy.humanOnly`. A unit test enumerates EVERY actor
   shape that exists in this repo — allowed: `dashboard:`, `opsctl:`,
   `manual:`, `mcp:manual:`; denied with rule `human_only`: `worker:x`,
   `mcp:worker:x`, `drafts:gpt`, `orchestrator`, `capture:slackweb`,
   `promote:classify`. (Institutional landmine: "enumerate the actor shapes
   that exist in the repo — one of them is usually the hole".)
10. `task_dismiss` is absent from `internal/mcpserver/schemas.go` and is added
    to `spineTools` in `internal/mcpserver/adapter_test.go` — asserted
    deliberately, not by omission (the SWT-20 precedent).
11. The refusal is shared, not restated: `closeTask` and `dismissTask` call one
    unexported helper, and `task_dismiss` refuses `claimed`, `in_progress` and
    `needs_feedback` with the same message `task %d is %s; refusing to close
    active work`. Test: dismissing an `in_progress` task fails AND leaves no
    `task_dismissals` row.
12. One transaction: the `tasks.status` update, the `status_changed` event and
    the `task_dismissals` insert commit together or not at all. The event's
    `reason` is prose composed from the code and the note (e.g.
    `dismissed (not_actionable): duplicate of the Tuesday thread`) — the same
    `{from,to,reason}` payload shape every other close writes, with no new key.
13. Idempotent: a second `task_dismiss` on the same task returns success, adds
    no second `task_dismissals` row (`ON CONFLICT (task_id) DO NOTHING`), and
    writes no second `status_changed` event.
14. Dismissing a task that is ALREADY `closed` but has no dismissal row records
    the label and emits no `status_changed` event. (Reachable by a stale page,
    a double submit, or a task the orchestrator closed. The label is a human's
    judgement about a task that should not have existed; refusing would lose it,
    and the row makes no claim about the transition.)
15. Route `POST /tasks/{id}/dismiss` on the auth-required mux, executing
    `task_dismiss` with actor `dashboard:{session user}` **and
    `executor.Call.TaskID` set**, so `audit_events.task_id` is non-NULL for the
    call (today's `executeTo` sets no task id; extend it or add a sibling).
16. `reason_code` comes from a `<select>` and `note` from a text input inside a
    per-row `<form method="post">`. The select must NOT carry `onchange` —
    `internal/dashboard/board_structure_test.go` asserts *exactly one* onchange
    in `tasks.html` (the project filter), and a second one turns it red.
17. The redirect returns to `/tasks` **with the current filters preserved**,
    rebuilt from the four known keys (`project`, `status`, `assignee_type`,
    `subproject`) via `url.Values` — never by echoing `r.URL.RawQuery`
    (the `safeNext` lesson in `internal/dashboard/auth.go`) — plus the existing
    `flash`. A refusal renders as the flash text on the same filtered board.
18. A dismissed task is gone from the default board with **no change to
    `boardQuery`**: `t.status <> 'closed'` already hides it. Integration test
    asserts the id is present before and absent after, and present again under
    `?status=closed`.
19. Both label joins work, proven by an integration test that seeds one
    promoted task (`classify_promotions` → `ai_extractions`) and one
    capture-rule task (`capture_decisions` → `capture_rules`), dismisses both
    through the real handler, and runs the two queries in "Labelled-data
    contract" below verbatim.
20. Nothing reads a jsonb payload for a label: a structural test asserts no
    `payload->>'reason'`, `payload->>'reason_code'` or equivalent appears in
    `internal/` in connection with dismissals.
21. The migration ledger guard in `internal/classify/structure_test.go` (the
    `n > 17 && n != 18 …` list) learns 22, and nothing above 0022 exists.
22. This ticket takes NO advisory lock and adds no key. **Do not write the
    string `0x5157_0022` anywhere** — `TestAdvisoryLockKey_HasNoCollisionInTheRepo`
    walks `internal/` for that literal and fails any file outside
    `internal/classify/` that contains it. Say "migration 0022" in prose.
23. `docs/runbooks/capture-rules.md` records the new title shape (with a
    before/after example) and the one-off correction statement; a runbook line
    also states that `task_dismissals` is the labelled-data store and
    `task_events` is not.

## Data model changes

**`migrations/0022_task_dismissals.sql`** (the only migration this ticket adds):

```sql
CREATE TABLE task_dismissals (
  id           BIGSERIAL PRIMARY KEY,
  task_id      BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  reason_code  TEXT NOT NULL CHECK (reason_code IN
                 ('not_actionable','wrong_kind','duplicate','handled_elsewhere')),
  note         TEXT,
  dismissed_by TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX task_dismissals_task_uniq ON task_dismissals (task_id);
```

- **ON DELETE CASCADE** for `capture_decisions`' recorded reason: a dismissal
  without its task means nothing, and integration suites clear fixtures by
  deleting `tasks` — without the cascade they fail inside cleanup, which reads
  like the cross-pollution pact breaking rather than like a new FK.
- **The unique index is TOTAL**, so `ON CONFLICT (task_id) DO NOTHING` needs no
  predicate restated (unlike `capture_decisions_live_uniq` and
  `task_events_outbound_observed_uniq`, both partial, whose callers must repeat
  the WHERE clause or hit a runtime error). The index IS criterion 13.
- **No other index, deliberately.** The join targets (`classify_promotions`,
  `capture_decisions`) hold tens to hundreds of rows for this purpose and every
  reader reaches `tasks` by primary key. An index nothing uses is a permanent
  claim that some query needs it, which the next reader has to disprove (0018's
  recorded argument).
- `dismissed_by` duplicates the executor actor that is already in
  `audit_events`. Deliberate: the label is training data and must be readable
  with one typed query, not by parsing `audit_events.args`.

No other schema change. `tasks`, `task_events`, `capture_rules`,
`capture_decisions`, `classify_promotions`, `ai_extractions` are untouched.

## API / MCP tool changes

**New: `task_dismiss`** — `{task_id: int, reason_code: string, note?: string}`
→ `{task_id, status: "closed", dismissed: true|false}` (`dismissed:false` when
the row already existed, i.e. the idempotent replay).

Executor path (invariant 3), hooked in exactly where its siblings are:

- registered in `tools.Register`'s table (`internal/tools/createtask.go`),
  beside `task_close`;
- `Validate` = `validateDismiss` (`internal/tools/close.go`);
- policy: added to `humanOnly` in `internal/policy/matrix.go`. Not `sendShaped`
  and not `snapshotGated` — nothing leaves the system, so neither the kill
  switch nor the rate limit has any claim on it (the `mark_delivery_failed`
  argument, verbatim);
- audit: the dashboard passes `executor.Call.TaskID`, so audit start/complete
  rows carry the task;
- **NOT** in `internal/mcpserver/schemas.go`. An agent that could dismiss tasks
  could clear its own queue.

**Modified: none.** `task_close` keeps its exact signature and behaviour; only
its transaction body moves into a shared helper both verbs call.

**New dashboard route:** `POST /tasks/{id}/dismiss` (auth-required), form fields
`reason_code` (select) and `note` (text). No GET route, no JSON API.

## MQTT topics

None. Nothing published, nothing subscribed, no LWT.

Note for the reviewer: a dismissal writes a `status_changed` task_event, which
NOTIFYs the orchestrator's drain. That is deliberate and unchanged — the
orchestrator's `status_changed → closed` branch runs `ruleUnblockDependents`
(`internal/orchestrator/rules.go:124-129`), so dismissing a blocker unblocks its
dependents exactly as `task_close` already does. A dismissal is a real close;
the orchestrator neither knows nor needs to know it was one.

## Files likely to touch

Part 1:
- `internal/capture/rules_store.go` — `storedRule` (+`projectName`), `loadRules`
  SELECT, `ruleTaskTitle` signature and branch, `createRuleTask` call site.
- `internal/capture/rules_structure_test.go` — criterion 5's scan.
- `internal/capture/rules_test.go` (or a new `rules_title_test.go`) — the
  characterization table.
- `internal/capture/rules_integration_test.go` — criterion 3's column proof.
- `docs/runbooks/capture-rules.md` — title shape + the correction statement.

Part 2:
- `migrations/0022_task_dismissals.sql`
- `internal/tools/close.go` — the shared transition helper, `dismissArgs`,
  `validateDismiss`, `dismissTask`.
- `internal/tools/createtask.go` — the `Register` table.
- `internal/tools/tools_unit_test.go` — `allToolNames` + `toolsUnderTest`.
- `internal/policy/matrix.go` — `humanOnly`.
- `internal/policy/matrix_dismiss_test.go` (new) — criterion 9's actor table.
- `internal/mcpserver/adapter_test.go` — `spineTools`.
- `internal/dashboard/server.go` — the route; `executeTo` gains a task id and a
  query-safe `back`.
- `internal/dashboard/board.go` — the dismiss handler + filter rebuild.
- `internal/dashboard/templates/tasks.html` — the per-row form.
- `internal/dashboard/dashboard_integration_test.go` (or a new
  `board_dismiss_integration_test.go` joining the same cleanup pact) —
  criteria 18 and 19.
- `internal/classify/structure_test.go` — the migration ledger.

## Labelled-data contract

The two queries a future precision ticket runs, and which criterion 19 pins.
They are plain typed SQL: no jsonb predicate on `task_events`, no LIKE, no
payload grepping.

Classifier side — "every verdict a human dismissed as not actionable, with what
the model said":

```sql
SELECT d.reason_code, d.note, d.dismissed_by, d.created_at,
       cp.normalized_message_id, cp.kind AS promoted_kind, cp.action,
       e.fields->>'kind'   AS verdict_kind,
       e.fields->>'title'  AS verdict_title,
       e.fields->>'reason' AS verdict_reason
  FROM task_dismissals d
  JOIN classify_promotions cp ON cp.task_id = d.task_id
  JOIN ai_extractions e       ON e.id = cp.ai_extraction_id
 WHERE d.reason_code = 'not_actionable'
 ORDER BY d.created_at;
```

Capture side — "which rules produce tasks that get thrown away":

```sql
SELECT r.id AS rule_id, r.criteria_type, r.pattern, p.slug AS project,
       d.reason_code, count(*)
  FROM task_dismissals d
  JOIN capture_decisions cd ON cd.task_id = d.task_id AND cd.mode = 'live'
  JOIN capture_rules r      ON r.id = cd.matched_rule_id
  JOIN projects p           ON p.id = r.project_id
 GROUP BY 1,2,3,4,5
 ORDER BY 6 DESC;
```

Two facts the reader must know, and which belong in the code comment above the
table as well as here:

- **`classify_promotions.task_id` is not unique.** An `attached` promotion
  points at a task another message created, so one dismissal can join to several
  promotion rows. That is correct — dismissing the task rejects every verdict
  attached to it — and any counting query must group rather than assume 1:1.
- **A dismissal joins zero rows on both sides for a hand-created task** (opsctl,
  plan import). `task_dismissals` is complete on its own; the joins are
  provenance, and a LEFT JOIN is the honest shape for a mixed report.

**Re-classification after dismissal is already impossible, and this ticket does
not rebuild it:** `classify_promotions_message_uniq` is a TOTAL unique index on
`normalized_message_id` (0021), so a message that has been promoted once can
never be promoted again — dismissed or not. Nothing here weakens or duplicates
that.

## In scope / Out of scope

**In scope:** the title branch and its label column; migration 0022;
`task_dismiss` (validate + policy + handler + shared refusal); the board's POST
route, form and redirect; the two documented label queries; the runbook lines;
the one-off production title correction.

**Out of scope — named because they are the tempting bundles:**

- **Feeding `task_dismissals` back into `classify eval`.** The whole point of
  the typed row is that a later ticket can. Not this one — and note the standing
  fact from SWT-30: `classify eval` writes NO `ai_runs`/`ai_extractions` rows,
  and if it ever gains a store write the promoter's cutover gains an eval-shaped
  hole. Any ticket that turns dismissals into eval labels must re-check that.
- **A dismiss verb on `/tasks/{id}`.** The board is the surface Salvador asked
  for; the detail page needs a redirect-target decision this ticket does not
  make.
- **Any other board verb** (re-open, re-prioritise, re-assign, edit title). A
  title-editing tool in particular is explicitly refused here (D5).
- **A dismissals panel on `/funnel`.** That page is a WINDOW, NOT A CONTROL and
  registers no POST route; adding a counter there is a separate, read-only
  change.
- **Re-titling by re-running capture.** `--all` is shadow-only and a live replay
  would double-append; the live claim for those five messages is spent forever.
- **Widening or promoting the reason enum, or per-project reason sets.**
- **Touching `promote`, `classify`, the orchestrator, or any connector's
  ingest/normalize path.**

## Invariants that apply

1. **Raw-first** — not exercised: nothing here ingests. The title is composed
   from `normalized_messages` columns already loaded by `pendingMessages`; no
   code in this ticket reads `raw_source_items` or decodes anything.
2. **One funnel** — `task_dismissals` must not become a second tasks table. It
   carries no status, no assignee, no claim and nothing ever "works" a row; the
   dismissed items remain rows in `tasks` with `status='closed'`, and the board
   lane stays a FILTER (`?status=closed`), never a table. The migration comment
   says this out loud, as 0015 and 0021 do.
3. **Everything through the executor** — concretely, for THIS ticket: the
   dashboard handler makes exactly one `s.ex.Execute` call and performs **no
   SQL of its own** for the action; `task_dismissals` is written **inside the
   `task_dismiss` handler**, in the same transaction as the status change; and
   the tool is reachable only through `tools.Register`. A direct INSERT from
   `internal/dashboard` would be the side door this invariant names.
4. **Nothing external without a delivery row** — nothing outbound exists here.
   No `deliveries` row is created, read or mutated, and no adapter is imported.
   A dismissed task with an open `deliveries` row keeps it; closing a task does
   not touch delivery state (and must not — see the "failing a delivery R8
   already processed" landmine for why delivery state is not to be inferred
   from task state).
5. **Own-message loop closure** — untouched. Capture's `direction='inbound'`
   filter (the line that IS invariant 5) is not modified; the title change is
   downstream of it and cannot widen the inbox.
6. **Stealth attribution** — nothing client-visible is produced. The title is
   composed only from stored data (project name + subject/first body line): no
   model, no generated prose, and the note is Salvador's own words, stored, never
   sent.
7. **Orchestrator purity** — the orchestrator is not modified and no rule is
   added. It sees the dismissal as an ordinary `status_changed → closed` event
   and runs `ruleUnblockDependents` as it already does for `task_close`; the
   dismissal reason never reaches a rule, so rules stay pure functions of
   (event, task, policy). Every dismissal writes audit rows through the
   executor.

## Sibling patterns to copy

- **The shared-transition + typed-log shape**: `internal/tools/close.go`
  `closeTask` for the transaction and the refusal; `internal/promote/store.go`
  `claim`/`recordTask` for the "typed row + `ON CONFLICT DO NOTHING`" idiom.
- **Dashboard POST verbs**: `internal/dashboard/server.go` `action(tool)` /
  `actionEdit` / `executeTo` — actor `"dashboard:" + s.auth.User(r)`, 60s
  timeout, 303 redirect with a flash. `planAction` is the closest analogue (a
  path-id tool, redirecting to a page other than `/deliveries`).
- **Per-row action forms**: `internal/dashboard/templates/deliveries.html`
  (`form.inline`, buttons inside a table cell, a textarea for edited text).
- **humanOnly policy + the actor table**:
  `internal/policy/matrix_capturerules_test.go` — copy its actor enumeration
  wholesale, and add `orchestrator` and `promote:` to it.
- **Spine-tool absence from MCP**: `internal/mcpserver/adapter_test.go`
  `spineTools`, and the `task_set_source_thread` comment as the model for why
  the absence is asserted.
- **Column-fed predicate proof**: `internal/drafts`' locality regression
  (institutional landmine 6) — the integration test must go red when the column
  is dropped from the SELECT.
- **Queue claims / `FOR UPDATE SKIP LOCKED`**: not used here. The dismissal
  locks a single row with `SELECT ... FOR UPDATE`, exactly as `closeTask` does.

## Verification protocol

Run in this order; do not commit before step 5 passes.

1. `go test ./...` — unit: title characterization + the upwork case, the policy
   actor table, tool registration/validation, the migration ledger, the
   structural scans, and `board_structure_test`'s one-onchange assertion.
2. `make integration` — `db-up` + `migrate` (applies 0022 to the compose db on
   :5433) + `go test -tags integration ./...`. Covers criterion 3's column
   proof, 11-14's transaction/idempotency behaviour, 18's board visibility and
   19's two label queries. The suite must be **rerunnable**: clean up in FK
   order (`task_dismissals` cascades, but delete `task_events` before `tasks`)
   under a test-owned slug prefix, and join the existing mutual-cleanup pact.
3. Confirm the local db was actually migrated:
   `psql "postgres://ops:ops@localhost:5433/ops?sslmode=disable" -tAc "SELECT max(version) FROM schema_migrations"`
   → `0022`.
4. **Manual smoke, real board, real ops db.** Apply the migration to pg-main
   FIRST (merging a migration is not applying it):
   ```
   psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0021
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate
   psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0022
   ```
   Then run the dashboard locally against the ops db (`cmd/dashboard`, :8085,
   dev-login) or `kubectl -n ops port-forward svc/dashboard 8085:80` **after**
   the image is rebuilt. Open `/tasks?project=saka`, dismiss one of the two live
   upwork tasks with `duplicate` + a note, and check all four:
   - the flash says `task_dismiss ok` and the row is gone from the board;
   - it reappears under `/tasks?status=closed`;
   - clicking Dismiss again on the closed row is a success with no second row;
   - `psql -h 192.168.50.49 -U ops -d ops -c "SELECT * FROM task_dismissals ORDER BY id DESC LIMIT 3"`
     shows one row with `dismissed_by = 'dashboard:salvo'`.
5. Run both queries from "Labelled-data contract" against the real db and
   confirm the dismissed capture task joins its rule (and, once a personal
   verdict has been dismissed, that the classifier query returns its stored
   verdict fields).
6. **Correct the five existing production titles** (D5). Look first, then update
   by explicit id — never a blind pattern UPDATE:
   ```sql
   SELECT t.id, p.slug, p.name, t.title
     FROM tasks t JOIN projects p ON p.id = t.project_id
    WHERE t.title LIKE 'upwork%'
    ORDER BY t.id;

   BEGIN;
   -- Sender-first (Q1), via the task's live capture decision → message;
   -- project name is the fallback exactly as the code's chain reads.
   UPDATE tasks t
      SET title = COALESCE(NULLIF(nm.sender,''), p.name)
                  || ' — ' || split_part(t.title, ' — ', 2),
          updated_at = now()
     FROM projects p, capture_decisions cd
     JOIN normalized_messages nm ON nm.id = cd.message_id
    WHERE p.id = t.project_id
      AND cd.task_id = t.id AND cd.mode = 'live'
      AND t.id IN (<the ids from the SELECT>);
   -- verify the new titles, then:
   COMMIT;
   ```
   This is a human one-off over five rows, not a code path: no tool writes
   `tasks.title` and none is being added. Record the ids and the before/after in
   the delivery summary.
7. Verify the capture title fix on live data only AFTER the connector image is
   rebuilt and the CronJob tag re-pinned (the kube session owns
   `~/projects/personal/kube/switchboard/`). Until then the pinned image keeps
   producing the old titles — that is expected, not a failure. First new upwork
   task after the roll must read `{Project name} — {first line}`.

## Future work (not this ticket)

- `classify eval --labels-from-dismissals`: score the local classifier against
  `task_dismissals` instead of, or alongside, the committed JSONL fixture. Needs
  the SWT-30 note about eval persistence re-checked first.
- A precision report per capture rule (`opsctl capture-rules report
  --dismissals`) built on the second query above; a rule whose tasks are all
  dismissed is a rule to disable.
- Dismiss from `/tasks/{id}`, and a dismissal counter on `/funnel`.
- Re-open (`task_reopen`) as the compensating verb for a mis-click; today the
  remedy is a psql UPDATE, deliberately unbuilt until something needs it.
