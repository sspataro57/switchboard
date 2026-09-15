> Jira: SWT-59

# board-incoming-first — an INCOMING section above blocked: open tasks raised by an email or Slack message, and PRs waiting for his review

**STATUS: DECIDED.** No open questions. Every choice below is recorded with its rationale under
"Decisions made unilaterally". The owner said "build it once the spec is ready".

**Evidence status.** Every code fact below was read in this worktree (main 8028d3c). **The prod
read-only queries were NOT run by the spec session**: it had no shell. Verification Step 0 turns
each prod assumption into a pre-check with a stated gate. The main thread runs it before merge and
pastes the results into the delivery summary. A gate that fails means stop and re-spec, not adapt
the code silently. That is the SWT-54 precedent.

## Source

Ad-hoc, from Salvador, 2026-09-15, verbatim:

> Emails or slacks should be high pritority. move to the top before the blocked. same thing with
> prs waiting for review

A layout amendment to the SWT-57 board (board-layout-compact). Related but separate: bug
`sana-email-not-captured` (a client email never captured). This ticket does not touch capture.

## Goal

`/tasks` renders a new first section, **incoming**, above **blocked**. It holds every open task
that is either:
- (a) created from an inbound email or Slack message by the promoter; or
- (b) a PR review task (SWT-54): a human task carrying a `system='github'` external ref.

Each row keeps its own light. The partition holds: every task is in exactly one section.

The change is dashboard-only:
- no migration, no tool, no policy change, no route and no template change;
- `boardQuery`, `TaskExportRow` and the exports are byte-unchanged;
- `boardLightFacts` still issues at most two statements.

**Usable alone means:** with only the new dashboard image rolled, Salvador opens
`/tasks?refresh=on` on the tablet at about 1000px and sees:
- `INCOMING (n)` directly under the Board line, before `BLOCKED`;
- a Slack question he was flagged on (a Holding inquiry task) at the top of it, red if a session
  is waiting on him, and the Treetop "Review PR #N — …" rows below the messages;
- a Jira ticket's task still in QUEUE, not in incoming.

He taps `actions`, then Done, on an incoming row, and it moves to DONE.

## What exists (code-read)

- **Promoter provenance.** `internal/promote` creates tasks through the executor and records each
  one in `classify_promotions`:
  - the row carries `normalized_message_id`, `action ∈ {task, review, attached}` and `task_id`
    (0021);
  - `claim` inserts the row with `task_id` NULL, and `recordTask` fills it after `create_task`
    (`store.go:459-485`);
  - `createVerdictTask` always creates `assignee_type: "human"`, priority 0 (`store.go:489-497`);
  - `action='task'` is a `ready` create and `'review'` is a `holding` create (the inquiry lane's
    `inquiryCreateStatus`, and personal-lane review);
  - `'attached'` means the message was attached to an EXISTING task (a log, or SWT-36's reopen);
    it created nothing.
- **What the promoter's lanes read.** The personal lane reads mail. The inquiry lane reads a
  project's inbound client conversation (gmail and Slack; only `collaboratory` is armed, IK
  "Inquiry lane"). SWT-53's resurfaced ask becomes a Holding task created by the same promoter.
- **The crash artifact.** A promotion row with `task_id IS NULL` is a real, visible state (IK
  "Classify promotion": "a crash artifact — visible, inert").
- **Capture creates one task per external TICKET, never per message:**
  - `createRuleTask` → `linkRuleRef` → `setRuleProvenance` (`rules_store.go:1395-1507`);
  - its tasks are keyed in `external_refs` (jira, github, upwork_crm);
  - they also carry `source_thread_id`: the gmail thread of a Jira notification mail, a PR's
    notification thread, or an Upwork room.
- **`tasks.source_thread_id`** (0019) is written only by `task_set_source_thread`. Both capture and
  the promoter call it.
- **`tasks.surfaced_by_message_id`** (0030) is **SWT-45**'s Jira-activity revive, not SWT-53. It is
  written by `task_mark_surfaced` / the revive, and it concerns Jira tickets only (revive is
  jira-only, `capturerules.go`).
- **PR review tasks (SWT-54).** Rule 64 creates human `ready` tasks, `external_refs(system='github',
  key owner/repo#N)`. D9's hand backfill makes the same three executor calls via `opsctl`, so a
  backfilled task has NO `capture_decisions` row. A merged or closed notice closes the task through
  `task_close` (D5), so "not closed" is "still waiting for review".
- **`external_refs`** has `UNIQUE (task_id, system, external_key)` (0007), an index leading with
  `task_id`, plus `(system, external_key)`. `link_external_ref` is also agent-facing (full
  profile), so a worker can link its OWN PR to a claude task.
- **`normalized_messages` has no index on `thread_id`** (`replyfold.JoinSQL`'s comment: the
  correlated shape measured 21.5 s on prod). Only `(raw_source_item_id)` and the gmail
  `(external_message_id)` partial index exist.
- **The board (SWT-57).**
  - `sectionFor(status, light)` maps a row to one of six sections, and `boardSections` groups and
    sorts the rows (`sections.go`).
  - `boardLightFacts`' first statement reads row facts plus the render time, over
    `t.id = ANY($1) OR t.status = 'ready'`. The second reads the queue candidates (`board.go:324`).
  - `lightFacts.QueueRank` / `UpdatedStamp` are display-only facts that `lightFor` never reads.

## Decisions made unilaterally (with rationale)

### I1 — "came from an email or Slack message" = a task the promoter CREATED from a message

**The fact.** `from_message` is true iff a `classify_promotions` row names the task with
`action IN ('task','review')`.

**Candidates rejected:**
- **`tasks.source_thread_id IS NOT NULL`.** Capture sets it on every ticket task too: a Jira
  task raised by a Jira notification mail, and a PR task. So it would put every Jira ticket in
  incoming. Narrowing it to non-Jira needs:
  - the thread's channel, which lives on `normalized_messages` (no `thread_id` index: a scan
    every 5 s on the shared pg-main); or
  - a SQL parse of the thread-key prefix, which is a second spelling of the connector key formats
    (the SWT-13/18/19 landmine class).
- **`tasks.surfaced_by_message_id`.** That is SWT-45's Jira-activity revive: a Jira ticket's email,
  exactly what the owner does not mean.
- **`action='attached'`.** The message landed on a task that already existed, possibly a ticket
  task; it created nothing. A task CREATED by the promoter already qualifies through its own
  `task` / `review` row, so excluding `attached` loses nothing.

**Jira-ticket tasks do not count**, even when a Jira notification mail created them. They carry no
promotion row, so they are excluded by construction. The owner means a person's message waiting on
him, and a Jira ticket is a ticket.

**Upwork is out, by his words ("emails or slacks").** Capture's Upwork conversation tasks
(rules 55–58) are keyed `upwork_crm`, with no promotion row, so they stay where their light puts
them. A promoter task from an Upwork message (only possible if an Upwork-carrying project is ever
inquiry-armed) WOULD count. That is the same "person's message" class, and Step 0a records the
channel mix. Adding capture's Upwork tasks later is a one-clause change (Future work).

**NULL safety.** `task_id` can be NULL (the crash artifact), and `x IN (subquery containing NULL)`
is NULL, not false, for a non-matching row. The subquery filters `cp.task_id IS NOT NULL` AND the
expression is wrapped in `COALESCE(…, false)`. The scan targets are `*bool` and are dereferenced,
so a NULL would otherwise break every render.

### I2 — "PR waiting for review" = a human task with a github ref

**The fact.** `pr_review` is `t.assignee_type = 'human' AND t.id IN (SELECT er.task_id FROM
external_refs er WHERE er.system = 'github')`.

**Why each clause:**
- **github ref.** Rule 64 and D9's backfill both write one, and nothing else in production writes
  a github ref onto a human task (Step 0c verifies).
- **human.** A worker's own PR, linked with the agent-facing `link_external_ref`, sits on a
  `claude` task. That is not a PR waiting for Salvador's review.
- **No `capture_decisions` join.** Backfilled review tasks have no decision row, so a provenance
  join through capture would drop them.
- **No PR-key shape test in SQL.** `github.PRKey` is the one spelling, and all github keys are
  canonical (SWT-54).

**"Not closed" suffices** for "waiting". A merged or closed notice closes the task (SWT-54 D5), and
a Done closes it.

### I3 — The section: key `incoming`, title `incoming`, first

- `boardSectionOrder` gains `{Key: "incoming", Title: "incoming"}` as its FIRST entry, above
  `blocked`. So the header is `<h2 id="section-incoming">incoming (n)</h2>` and CSS uppercases it
  to `INCOMING (n)`.
- The template is byte-unchanged: it already ranges over `.Sections`.
- "incoming" says what the rows have in common (they arrived from outside), where "high
  priority" would collide with `tasks.priority` (I6).

### I4 — Incoming outranks every light except green; the row keeps its light

**The rule.** A row is `incoming` iff its incoming kind is set AND `status <> 'closed'` AND its
light class is not `done`. Otherwise it goes to `sectionFor(status, light)`, unchanged.

**Consequences:**
- A red incoming row (a session's `needs_input`) is in incoming, not blocked, and stays red. The
  owner said "before the blocked".
- The same holds for yellow, the stale ring, blue and grey (including status `blocked`).
- A finished row (`closed` today, `done_locally`, `delivered`) stays in **done**. A dismissed
  closed row (shown only under `?status=closed`) stays in **other**. "Incoming" is work waiting, and
  a finished task is not waiting.

**This amends SWT-57 L1 ("sections come from the light") in exactly one place.** One provenance
fact outranks every light but green, and the light itself is untouched (`lightFor` never reads the
new facts; structure test). The partition holds by construction: a single pure function,
`boardSectionOf(row)`, returns exactly one key per row. `sectionFor` keeps its signature and its
table, so every SWT-57 test of it passes unchanged.

**The queue's blue head may sit in incoming.** An incoming `ready` task can still be the first of
its human lane. It shows blue ("next in queue (slug)") in incoming, and QUEUE then starts with a
grey row. This is truthful: the task IS next, it is just shown higher. SWT-57's "blue head first in
QUEUE" now reads "…unless the head is incoming".

### I5 — Order within incoming: attention, then messages before PRs, then newest first

The order has three keys:
1. Light rank, as L2: input, stale, working, next, done, none. A red row leads.
2. Kind: `message` before `pr_review`, the owner's own order ("emails or slacks… same thing with
   prs").
3. `id` DESC: the newest task first.

**Why not the inbound message's own time:**
- **Cost.** For promoter tasks it is one PK join (`classify_promotions.normalized_message_id`). For
  PR tasks, "the latest PR mail" needs a `normalized_messages` read by `thread_id`, and there is no
  index for that.
- **Mixed clocks.** Mixing provider `sent_at` with task `created_at` would compare two clocks.
- **Close enough.** A task id is created within one pipeline tick of its message, so `id` DESC is
  arrival order.

**Why newest first.** It is the inbox convention, and it keeps a fresh ask visible without
scrolling, while older rows stay in the same section. Flipping to oldest first is one comparator
line.

The status tiebreak of L2 is deliberately NOT used here. It would put `holding` inquiry rows ahead
of newer `ready` ones for a reason that means nothing in this section.

**Facts come from the existing first statement:** two boolean expressions, two uncorrelated
`IN (subquery)` clauses (Postgres hashes them: one small scan of `classify_promotions`, one index
scan of `external_refs` by `system`). No third statement is issued. D15's count, the IK load table
and the refresh tracer stay unchanged.

### I6 — Board-only; `tasks.priority` is not touched

"High priority" is honoured as placement, not as a priority write:
- **`priority` is ROUTING.** It drives `tools.TaskQueueOrder`, and with it `task_get_next`,
  `task_list` and the blue heads.
- **No spine writer exists.** Both creators hard-code priority 0 (`createRuleTask`,
  `createVerdictTask`), and `task_set_priority` is `humanOnly`, so no spine caller may write it
  (IK SWT-38). Changing that is a policy change and a capture/promote change, which is out of
  "dashboard-only".
- **A write would outlive its reason.** It would need an un-bump once he replied, whereas a
  read-time section is reversible with an image roll.

### I7 — Answered messages are not folded out: the task's own status is the signal

A promoter task stays in incoming until it is closed, dismissed or finished, whether or not he has
replied on its thread since. Reasons:
- **The fold is not cheap.** `replyfold.RepliedSinceCol` rides on `replyfold.JoinSQL`, a
  set-based scan of every outbound message, anchored on an `ai_extractions` alias. On the board it
  would be a full `normalized_messages` scan every 5 s per visible tab (no `thread_id` index). A
  per-task variant would be a second spelling of the fold, which the IK forbids.
- **It is near-inert where it matters.** IK "Inquiry lane": 1 outbound among 447 collaboratory
  gmail thread messages. His replies do not land on the same thread, so the fold would say
  "unanswered" for nearly every email anyway.
- **The gate already ran at creation.** The inquiry promoter skips an answered message before
  creating anything. After that, an open task means open work; Done or Dismiss (one tap in
  `actions`) removes it.

### I8 — Closed and finished tasks are excluded

They are excluded by I4's rule. They stay in done or other, exactly as SWT-57 places them.

## Acceptance criteria

### Part 1 — pure Go (`internal/dashboard/sections.go`, `lights.go`)

1. **`boardSectionOrder`** has SEVEN `{Key, Title}` pairs:
   `incoming/incoming, blocked/blocked, in_flight/in flight, queue/queue, holding/holding,
   done/done, other/other`.
2. **`sections.go` declares:**
   - `const incomingMessage = "message"` and `const incomingPRReview = "pr_review"`;
   - `func incomingKind(fromMessage, prReview bool) string`, which returns `incomingMessage` when
     `fromMessage` (it wins if both are set), else `incomingPRReview` when `prReview`, else `""`;
   - `func boardSectionOf(r taskRow) string`: `"incoming"` iff `r.Incoming != "" && r.Status !=
     "closed" && r.Light.Class != "done"`, else `sectionFor(r.Status, r.Light)`.

   `sectionFor`'s signature and body are unchanged. The file's import ban and the body bans of
   `TestSections_PureNoIONoClock` extend to both new functions.
3. **`boardSections`** calls `boardSectionOf` (never `sectionFor` directly) and sorts `incoming`
   by I5: light rank, then kind (message 0, pr_review 1), then id DESC. The queue and every other
   section keep L2 exactly.
4. **The facts.**
   - `lightFacts` gains `FromMessage, PRReview bool`, documented display-only.
   - `lightFor`'s body mentions neither, and `lightFor(st, f)` is identical with them flipped.
   - `taskRow` gains `Incoming string`, board-only: never an export column.

### Part 2 — the read (`internal/dashboard/board.go`)

5. **`boardLightFacts`' FIRST statement adds, and scans, two columns:**

   ```sql
   COALESCE(t.id IN (SELECT cp.task_id FROM classify_promotions cp
                      WHERE cp.task_id IS NOT NULL AND cp.action IN ('task','review')), false) AS from_message,
   COALESCE(t.assignee_type = 'human'
            AND t.id IN (SELECT er.task_id FROM external_refs er WHERE er.system = 'github'), false) AS pr_review
   ```

   These go into the inner `f` select and the outer select list, and are scanned as `*bool` like
   `closed_today`. Requirements:
   - it still issues at most two statements;
   - the second statement is byte-unchanged;
   - `boardLightFacts` (and what it reaches) never mentions `source_thread_id`,
     `surfaced_by_message_id`, `normalized_messages` or `'attached'`, per I1 and I7 (structure
     test);
   - `TestBoardLightFacts_IsASeparateRead`, `…FirstStatementSelectsTheSession`,
     `…FirstStatementFormatsTheUpdatedStamp`, `TestBoard_NoGoClockFeedsVisibilityOrALight` and
     `TestBoardRefresh_Integration_NoExtraQueries` pass unchanged.
6. **`listTasks`** sets `tr.Incoming = incomingKind(f.FromMessage, f.PRReview)`. Nothing else in
   the handler changes. `boardQuery`, `TaskExportRow`, `boardKeys`, `boardBack`,
   `boardRefreshURLs`, `boardAdvanced` and `tasks.html` are byte-unchanged.

### Part 3 — unit tests (`sections_incoming_test.go`, new; no db)

7. **`boardSectionOf` named cases** (each with a CONTROL on `lightFor`'s class):

   | status + facts | Incoming | light | section |
   |---|---|---|---|
   | `ready` | message | none | incoming |
   | `ready` + QueueHead | pr_review | next | incoming (stays blue) |
   | `holding` + `needs_input` | message | input | incoming (stays red) |
   | `blocked` | message | none | incoming |
   | `ready` + fresh `working` | pr_review | working | incoming |
   | `ready` + stale `working` | message | stale | incoming |
   | `closed` today | message | done | done |
   | `closed` + open dismissal | message | none | other |
   | `done_locally` / `delivered` | pr_review | done | done |
   | `ready` | "" | none | queue |
8. **Agreement.** Over every status in `boardStatusOrder` plus `some_future_status`, crossed with
   `factCombos`:
   - with `Incoming == ""`, `boardSectionOf == sectionFor` everywhere;
   - with `Incoming = message`, `boardSectionOf` is `incoming` iff status ≠ closed and class ≠ done,
     and otherwise equals `sectionFor`;
   - the light is identical in both cases.
9. **`incomingKind`** truth table: 4 rows, message winning when both are set.
10. **`boardSections` with incoming rows:**
    - `incoming` comes first;
    - a red pr_review row precedes a grey message row;
    - among grey rows, messages precede PRs;
    - within one kind, id DESC;
    - the partition holds (the multiset of ids is equal, no id twice) over a mixed board fed in id
      order and reversed;
    - an incoming `closed`/done row is in `done`;
    - a board with no incoming rows yields exactly SWT-57's sections (the existing mixed-board
      string, unchanged).

### Part 4 — structure (`board_incoming_structure_test.go`, new)

11. **Purity.** `boardSectionOf` and `incomingKind` bodies contain none of `s.pool`, `Query(`,
    `Exec(` or `time.`, and `boardSections`' body contains `boardSectionOf(`.
12. **The first statement.** It (the slice before the second `s.pool.Query`) contains
    `classify_promotions`, `cp.task_id IS NOT NULL`, `('task','review')`, `external_refs`,
    `er.system = 'github'` and `t.assignee_type = 'human'`, each inside a `COALESCE(`. The
    second statement has none of them. The criterion-5 bans hold across `ps.reach("boardLightFacts")`.
13. **Display-only.** `lightFor`'s body mentions neither `FromMessage` nor `PRReview`, and both
    fields' comments say "display" (the `TestLightFacts_DisplayOnlyFieldsNeverFeedTheLight` shape).
14. **Template.** `tasks.html` contains no `incoming` and no `.Incoming`: the section comes from
    data, and the template stays status- and kind-blind.

### Part 5 — integration (`board_incoming_integration_test.go`, new; `//go:build integration`)

The harness reuses:
- `dashGuard`, `dashPool`, `newDashServer`, `get`, `snippet`;
- `bdInsID`;
- `lightsExecutor`, `lsSignal`, `boardLight`, `onBoard`, `lsDayStart`;
- `layoutSections` / `lyRender` from `board_layout_integration_test.go`.

Its own project slug is `itest-incoming-proj`, with a message chain: `source_accounts` (provider
`itest-incoming`), `raw_source_items`, `normalized_threads` (`itest-incoming:` keys),
`normalized_messages` (channel `gmail` / `slack`), `ai_runs` (model `itest-incoming`),
`ai_extractions` and `classify_promotions`. This is the shape of
`internal/promote/reopen_integration_test.go` `message`/`verdict`, seeded directly, not through
the promoter.

The cleanup is FK-ordered and rerunnable:
1. `classify_promotions` and `external_refs` by task or message;
2. `policy_decisions` / `audit_events` by `task_id` (the SWT-37 landmine);
3. `task_dismissals`, `task_claims`, `task_events`, `tasks`;
4. the message chain;
5. the project.

It runs ONLY on an isolated DB.

15. **`TestBoardIncoming_Integration_SectionAboveBlocked`.** Seed:

    | Row | Seed | Expected |
    |---|---|---|
    | J1 | human `ready`, priority 1, created FIRST; `external_refs(jira, ITINC-1)` + `source_thread_id` = a gmail thread (the Jira-notification shape) | queue, first (blue) |
    | M1 | human `ready`; promotion `task` (gmail message) | incoming |
    | M2 | human `holding`; promotion `review` (slack message); `lsSignal needs_input` | incoming, FIRST, red |
    | M3 | human `blocked`, created after M1; promotion `task` | incoming, grey, before M1 |
    | P1 | human `ready`; `external_refs(github, itest-incoming/repo#1)` | incoming, after M1 |
    | P2 | claude `in_progress`; `external_refs(github, itest-incoming/repo#2)` | in flight |
    | A1 | human `ready`; promotion `attached` only | queue |
    | S1 | human `ready`; `source_thread_id` set, no promotion, no ref | queue |
    | M4 | human `closed`, `closed_at = lsDayStart + 1 hour`; promotion `task` | done |
    | C0 | a promotion row with `task_id` NULL on its own message (crash artifact) | nothing breaks |

    `GET /tasks?project=itest-incoming-proj` asserts:
    - HTTP 200, even with C0 present;
    - exact sections `incoming(4)=[M2 M3 M1 P1] | in_flight(1)=[P2] | queue(3)=[J1 …] |
      done(1)=[M4]`: the queue holds exactly {J1, A1, S1} with J1 first, and there is no
      `section-blocked` (M3 wins it) and no `section-holding` (M2 wins it);
    - `section-incoming` precedes every other `<h2`;
    - each seeded id's light span occurs exactly once;
    - lights are M2 `input`, M3 and M1 and P1 `none`, J1 `next`, P2 `working`, M4 `done`, which
      proves incoming kept each row's light.
16. **Column-fed (the "test the column" rule).** In the same test, after the first render:
    - `DELETE FROM classify_promotions WHERE task_id = M1` and re-render: M1 is in `queue`;
    - `DELETE FROM external_refs WHERE task_id = P1` and re-render: P1 is in `queue`.

    Only Postgres supplies the facts, so replacing either SELECT column with `false` turns
    criterion 15 red.
17. **A status filter keeps the grouping (L3).** `?status=ready` gives `incoming=[M1 P1]` and
    `queue` = {J1, A1, S1}. `?status=closed` gives `done=[M4]`.
18. **Existing suites stay green unchanged.** Their seeded rows carry no promotion row and no
    github ref, so incoming never renders for them:
    - `board_layout_integration_test.go` (its exact section strings);
    - `board_lights_…`, `board_session_…`, `board_refresh_…` (the tracer count);
    - `board_close_…`, `board_dismiss_…`, `board_reopen_…` (whose rows carry `source_thread_id`,
      which proves I1's rejection holds);
    - `dashboard_integration_test.go`.

### Existing tests: exactly one deliberate amendment

- **`TestBoardSectionOrder_SixPairsInOrder`** (`sections_test.go`) is renamed
  `TestBoardSectionOrder_SevenPairsInOrder`, and its `want` gains the leading
  `incoming/incoming, `. A comment names this ticket.

Everything else passes UNCHANGED, and the reviewer checks it:
- in `sections_test.go`, `sectionKeys` (six) and the tests that use it stay valid because
  `sectionFor` never returns `incoming`; `TestBoardSections_MixedBoard` / `…Partition` feed no
  incoming row;
- every template test;
- every board integration test.

## Data model changes

None. No migration, and the `internal/classify/structure_test.go` ledger is untouched. Two
existing tables are newly read by the board, `classify_promotions` and `external_refs`; both
are read-only here.

## API / MCP tool changes

None. No tool, pin, schema, policy or route changes. `GET /tasks` renders one more section.
Dismiss and Done are unchanged (`executeTask`, byte-identical forms).

## MQTT topics

None.

## Files likely to touch

- `internal/dashboard/sections.go`: `boardSectionOrder` (+incoming), `incomingMessage` /
  `incomingPRReview`, `incomingKind`, `boardSectionOf`, the `boardSections` sort, the file-header
  comment (the L1 amendment).
- `internal/dashboard/lights.go`: `lightFacts` +`FromMessage`, +`PRReview` (display-only).
- `internal/dashboard/board.go`: `taskRow.Incoming`; the first statement and scan of
  `boardLightFacts`, plus its doc comment; one line in `listTasks`.
- Tests:
  - `internal/dashboard/sections_test.go` (the one amendment);
  - new: `sections_incoming_test.go`, `board_incoming_structure_test.go`,
    `board_incoming_integration_test.go`.
- Docs:
  - `docs/tickets/board-layout-compact_SPEC.md`: a dated amendment line under L1 and L2 pointing
    here;
  - `.claude/INSTITUTIONAL_KNOWLEDGE.md`: a short "Board incoming section" paragraph in the SWT-57
    entry (I1's predicate and why `source_thread_id` is not it; I4's rule; the NULL-safe `IN`), and
    the SWT-52 load-table row for `boardLightFacts`, noting the two hashed subqueries in statement 1;
  - at deliver time, `docs/runbooks/HANDOFF-kube-board-incoming-first.md`.

**Deliberately NOT touched:**
- `templates/tasks.html` and every other template;
- `boardQuery`, `export.go`, `server.go`;
- `internal/promote`, `internal/capture`, `internal/tools`, `internal/policy`, `internal/mcpserver`,
  `internal/replyfold`;
- `migrations/`.

## In scope / Out of scope

**In scope:** the incoming section, its two facts, its order, the tests, the IK and SWT-57 SPEC
amendments, and the kube handoff.

**Out of scope, named because each is a tempting bundle:**
- **Capture (bug `sana-email-not-captured`).** Incoming shows messages that BECAME tasks. A client
  email on a project that is not inquiry-armed (only collaboratory is) creates no task, so it
  cannot appear here until capture or promote make one. That is the bug's and the arming's
  business.
- Writing `tasks.priority`, promoting incoming work in `task_get_next`, or changing
  `inquiryCreateStatus`.
- A replied-since fold on the board (I7), and an index on `normalized_messages(thread_id)`.
- Capture's Upwork conversation tasks in incoming (I1; Future work).
- A per-row kind tag ("email", "slack", "PR"), a per-section collapse, and incoming on `/tasks/{id}`
  or `/funnel`.
- The GitHub connector, R9–R11, SWT-54's rules, and SWT-45 surfacing.

## Invariants that apply

1. **Raw-first:** not exercised; nothing is ingested.
2. **One funnel:** no table and no status are added. Incoming is a filter over the one `tasks`
   table, computed at render time from provenance rows that already exist.
3. **Everything through the executor:** the new code performs no write. It adds two read-only
   expressions to an existing statement. The dashboard's only actions stay Dismiss and Done, each
   one `executeTask` call.
4. **Nothing external without a delivery row:** nothing is sent, and no delivery is read.
5. **Own-message loop closure:** untouched. Provenance comes from the promoter's claims, whose inbox
   is inbound-only. An outbound message can never have a promotion row.
6. **Stealth attribution:** nothing client-visible. The dashboard is port-forward only.
7. **Orchestrator purity:** the orchestrator is untouched. `boardSectionOf` and `incomingKind` are
   pure (structure-scanned). The facts are SQL on the DB, and no Go clock is involved.

## Sibling patterns to copy

- **A display-only fact on `lightFacts`, fed by the first statement:** SWT-57's `QueueRank` /
  `UpdatedStamp` and `TestLightFacts_DisplayOnlyFieldsNeverFeedTheLight`.
- **The pure section function plus agreement table:** `sectionFor`, `TestSectionFor_AgreesWithLightForEverywhere`,
  `factCombos`.
- **Section-slicing integration asserts:** `layoutSections` / `lyRender` / `lyAllowed`
  (`board_layout_integration_test.go`).
- **The message → verdict → promotion fixture chain and its FK-ordered cleanup:**
  `internal/promote/reopen_integration_test.go` (`prrCleanup`, `message`, `verdict`).
- **Real signals:** `lightsExecutor` / `lsSignal`.
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used; nothing is claimed.
- **rag-svc HTMX:** not used; the board has no HTMX (pinned).

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| `boardSections` calls `sectionFor` instead of `boardSectionOf` | criteria 10, 15 (M-rows in queue/blocked/holding) |
| Drop the `Light.Class != "done"` / `Status != "closed"` guard | criteria 7, 8, 15 (M4 in incoming) |
| Add `'attached'` to the action list | criterion 15 (A1 in incoming) |
| Remove both `cp.task_id IS NOT NULL` and the `COALESCE` | criterion 15 (C0 makes the render fail) |
| Drop `t.assignee_type = 'human'` | criterion 15 (P2 in incoming) |
| Use `t.source_thread_id IS NOT NULL` as the message fact | criteria 12, 15 (J1, S1 in incoming) |
| Replace `from_message` (or `pr_review`) in the SELECT with `false` | criteria 15, 16 |
| Incoming sorted by id ASC | criteria 10, 15 (M1 before M3) |
| PRs before messages | criteria 10, 15 (P1 before M1) |
| Put incoming after blocked in `boardSectionOrder` | criteria 1, 15 |
| `lightFor` reads `FromMessage` | criterion 13 |
| Move the facts into a third statement | criterion 5's count, `TestBoardRefresh_Integration_NoExtraQueries` |

## Verification protocol

Run in this order. Do not commit before step 4 passes.

**0. Read-only prod pre-checks** (`psql -h 192.168.50.49 -U ops -d ops`, inside
`BEGIN READ ONLY; … ROLLBACK;`). NOT run by the spec session. Paste the results into the delivery
summary. Do not assert them as frozen literals in any test: the corpus is live.

- **0a. The promoter's tasks.**

  ```sql
  SELECT cp.action, nm.channel, t.status, t.assignee_type, count(*)
    FROM classify_promotions cp JOIN tasks t ON t.id = cp.task_id
    JOIN normalized_messages nm ON nm.id = cp.normalized_message_id
   GROUP BY 1,2,3,4 ORDER BY 1,2,3,4;
  ```

  **Gate:** `assignee_type` is only `human`; otherwise STOP (it contradicts
  `createVerdictTask`). Record the channels. Expected: gmail and slack. Any other channel means I1's
  "promoter ⇒ email or Slack" reading needs a channel clause: STOP and re-spec.
- **0b. The size of incoming** on the default board (open = not `closed`/`delivered`/`done_locally`):
  counts of rows matching `from_message` and `pr_review`, using criterion 5's two expressions. If
  there are more than about 25, say so in the delivery summary, because the section will push
  BLOCKED below the fold on the tablet. That is not a stop; the owner decides from the count.
- **0c. The github refs.**

  ```sql
  SELECT t.assignee_type, t.status, count(*)
    FROM external_refs er JOIN tasks t ON t.id = er.task_id
   WHERE er.system = 'github' GROUP BY 1,2;
  ```

  Expected: human, from rule 64 and D9. A human row that is not a review task means STOP.
- **0d. Why `source_thread_id` is not the predicate.** Open tasks with `source_thread_id` set and
  no `task`/`review` promotion, grouped by their `external_refs.system` (NULL included). Expected:
  jira, github and upwork_crm dominate. Record the NULL group, if any, with ids, and look at
  their titles: a person's message there would be a missed incoming row (Future work, not a stop
  unless it is the majority).
- **0e.** `SELECT count(*) FROM classify_promotions WHERE task_id IS NULL`: the NULL-safety
  surface. Record it.
- **0f.** `EXPLAIN (ANALYZE, BUFFERS)` of the new first statement with a realistic id array (the
  default board's ids), read-only. Expected: hashed SubPlans, no scan of `normalized_messages`, and
  total time of the same order as today's statement. Record both timings.

**1. Unit:** `go test ./...`, capturing the exit status separately from any pipe. The SWT-48
`TestAttributionTrend_*` flake (20:00–24:00 EDT) is pre-existing; if it fires, re-run with
`TZ=UTC`.

**2. Integration, on an ISOLATED database** (never prod, never the shared compose `ops`):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_boardincoming"
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_boardincoming?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardincoming?sslmode=disable' \
  go test -tags integration -p 1 -count=1 ./internal/dashboard/
```

Run it twice (rerunnable), then run `go test -tags integration -p 1 ./...` once against the same
URL.

**3. Mutations:** every row of the table above goes red and is reverted.

**4. Local smoke at 1000px.**
- Run `DATABASE_URL=<ops_boardincoming url> go run ./cmd/dashboard` (:8085,
  `/dev/login?user=salvo`).
- Seed a throwaway project with SQL in the criterion-15 shape:
  - two promoter tasks (one Holding, via a `review` promotion on a slack message, then
    `opsctl call --tool task_signal --args '{"task_id":N,"state":"needs_input","session":"shell"}'`;
    one `ready`, a gmail `task` promotion);
  - one human task with a github ref;
  - one jira-keyed ready task;
  - one status-`blocked` task;
  - about 15 plain ready tasks.
- In Chrome DevTools' device toolbar at 1000×700, open `/tasks?project=<slug>&refresh=on` and
  check:
  - the first line is the nav with the filter, the second is Board with auto-refresh, and then
    `INCOMING (3)` appears, fully visible, above `BLOCKED (1)`;
  - the red Holding row leads incoming with its red light and session tag, then the gmail task,
    then the PR row;
  - the jira task is in QUEUE;
  - Done via `actions` on an incoming row: the flash shows once, the row moves to DONE, and
    `refresh=on` survives;
  - `?status=ready` still shows INCOMING and QUEUE;
  - view source: one `<script`, and no `<details … open`.
- Drop the DB afterwards.

**5. Deploy: image only, no migration.**
- Build and push `192.168.50.20:5000/switchboard:<tag>` here.
- Hand the tag bump to the kube session in `docs/runbooks/HANDOFF-kube-board-incoming-first.md`.
  That session owns `kube/switchboard/dashboard.yaml`; this session never edits manifests.
- Only `deployment/dashboard` needs the tag. There is no ordering constraint, no env var and no
  schema dependency: the columns read exist since 0007/0021.
- **Post-roll smoke:** `kubectl -n ops port-forward svc/dashboard 8085:80`, then `/tasks?refresh=on`
  on the tablet. Check that INCOMING lists the rows Step 0b predicted, and that
  `pg_stat_activity` shows no new statement shape beyond the widened first statement.

**6. Rollback:** roll `deployment/dashboard` back to the previous tag. Nothing is stored and no
schema changed, so rollback is instant and lossless; URLs are unchanged.

## Future work (not this ticket)

- **Capture's Upwork conversation tasks** in incoming: one more `OR` clause (`er.system =
  'upwork_crm'` on a human task), if he wants Upwork chats there too.
- **A replied-since dimming** once `normalized_messages(thread_id)` is indexed, and only through a
  `replyfold` spelling.
- **A per-row kind tag** (email / slack / PR), and an "arrived" time from the promoted message's
  `sent_at`.
- **Incoming count in the page title** or the nav, for a glance from another tab.
