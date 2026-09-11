> Jira: SWT-36

# dismiss-reopen-on-activity — SPEC

## Source

Ad-hoc, Salvador, verbatim, 2026-09-10:

> "tasks popup rigfully but somebody not salvador would work on them I need a wait to dismisss but they should reopen if ingested again"

**Owner decision, recorded here and not to be raised again.** Asked "When a new message arrives,
which dismissed tasks should come back to your queue?", the options were "Only 'handled
elsewhere'" (recommended) and "Every dismissed task". He answered **"Every dismissed task"**.
A dismissal with ANY reason code (`not_actionable`, `wrong_kind`, `duplicate`,
`handled_elsewhere`) reopens on new inbound activity.

The dismiss half already exists (SWT-31, `task_dismiss`). This ticket is the reopen half.

## Goal

When a new inbound message is ingested for a dismissed task (on its thread via classify
promotion, or on its external ref via capture rules), bring the task back to its pre-dismissal
status. The reopen goes through `task_reopen` on the executor. The dismissal label is kept, and
the dismissal is marked as reopened.

**Usable alone:** after deploy, dismiss a task on the board. The next inbound message routed to
it puts it back on the board at the status it held before, with these visible:
- a "reopened after dismissal (reason)" marker on the board row;
- a `status_changed` log entry naming the reason code, the message and the sender.

A message that was already in switchboard before the dismissal does nothing but append its log
line.

## What happens today (verified 2026-09-10)

- **The promote path** (`internal/promote`):
  - `Decide` (`promote.go:99`) attaches only to an OPEN task.
  - `threadTask` (`store.go:302`) finds the open task by `source_thread_id` plus project. If
    there is none, it falls back to the oldest task of any status.
  - A dismissed task therefore falls through Q3. A NEW task is created, and the promotion reason
    names the closed id (`decisionReason`, `store.go:339`). The result is a duplicate of what was
    dismissed, and the thread history splits.
- **The capture path** (`internal/capture/rules_store.go`):
  - `taskForExternalRef` (:657) has no status filter.
  - `decideMessage` returns `task_log`, and `appendRuleLog` (:817) appends to the closed task.
  - The follow-up is logged silently and the task stays closed.
- **The reconciler** (`internal/ticketstatus/decide.go:167`):
  - Any `task_dismissals` row gives `suppressed_dismissed`. `loadCandidates` (`store.go:494`) is
    `EXISTS (SELECT 1 FROM task_dismissals d WHERE d.task_id = t.id)`.
  - This is status sync, not message ingest.
- **`task_reopen`** (`close.go:146`):
  - It is spine-facing, not humanOnly and not MCP-listed. It targets `openStatuses` (`close.go:32`)
    and defaults to `ready`.
  - It does not touch `task_dismissals`, so the row survives. `reopen_integration_test.go:326`
    pins that.
- **`task_dismiss`** (`close.go:215`):
  - It discards `closeTransition`'s `from` return value.
  - It uses `ON CONFLICT (task_id) DO NOTHING` against the TOTAL unique index
    `task_dismissals_task_uniq` (0022). The first label on a task survives forever.
- **The outbound observer** (`internal/capture/observe.go`) writes only `outbound_observed`
  events. It never changes status.

## Decisions

**D1: what "ingested again" means.**
- It is a NEW `normalized_messages` row with `direction='inbound'` that one of the two EXISTING
  attach paths routes to the dismissed task:
  - promote: same `source_thread_id`, same project, exactly `threadTask`'s scoping;
  - capture: same `external_refs (system, external_key)`.
- No new matching path is invented. Examples of what is excluded: capture thread matching for
  attribution-only rules, and triage (shadow).
- Every inbound sender counts, notification bots included. The owner's word is "ingested", and
  the prod measurement (Verification §4) shows the rate before go-live.

**D2: the clock is the INGEST time.**
- The rule: reopen iff `normalized_messages.created_at > task_dismissals.created_at`, strictly.
- It is not the send time (`sent_at`), for four reasons:
  - **One clock.** Both columns are Postgres `now()`, so no provider clock skew is involved and
    the IK two-minute allowance does not apply.
  - **Stable under reprocessing (invariant 1).** All four sinks upsert
    `ON CONFLICT (raw_source_item_id) DO UPDATE` without touching `created_at`
    (`upworkcrm/sink.go:256`, `jira/sink.go:231`, `google/sink.go:271`, `slackweb/sink.go:161`).
    A re-normalize keeps both the id and the instant. Google's cross-account Message-ID dedup
    creates no second row.
  - **It covers the lag case.** A message sent at 09:55, dismissed at 10:00 and ingested at 10:10
    (the `*/15` cron gap) was unseen when he dismissed. `sent_at` would swallow it silently, which
    is exactly the failure this ticket fixes.
  - **Its cost is bounded.** A connector that resumes after a long suspension (slackweb is
    suspended) ingests backlog stamped "now". That backlog can reopen tasks for messages sent
    before the dismissal, but it is bounded by capture's 720h live horizon on `sent_at`.
    Over-reopening costs one click. A miss costs a lost message.

**D3: scope. A task is "dismissed" iff `status='closed'` AND it has an OPEN dismissal row
(`reopened_at IS NULL`, D6).**
- **A reconciler-closed task that a human labelled afterwards counts.** SWT-31 criterion 14
  permits labelling an already-closed task. The row is a human judgement, and the owner said
  every dismissal.
- **Unchanged (they keep today's behaviour):**
  - **Plain `task_close`** (orchestrator R1/feedback rules, hand-run): it is lifecycle, not a
    judgement that the task should not be on the board. A human who wants reopen-on-activity uses
    Dismiss.
  - **R8's Deliver-task close:** R8 already fired (`delivery_lifecycle` dedup, IK "before allowing
    a status transition backwards, check which orchestrator rules the forward transition already
    fired"). Reopening the Deliver task would put it back in the draft worker's inbox and invite a
    second draft of a reply already sent.
  - **`delivered` tasks** are not closed. **Reconciler-only closes** already have their own,
    ticket-driven return path (SWT-32 D3).
- In practice the promote path attaches to open tasks (unchanged), reopens dismissed tasks (new),
  and falls through Q3 as today for every other closed or delivered task.

**D4: mechanism. `task_reopen` gains a dismissal guard. The time rule lives in the handler, under
the row lock.**
- New optional args are `dismissal_id` and `message_id` (API section).
- The callers:
  - `promote.Decide` stays pure and decides the ROUTING: "this message belongs to dismissed task
    T; attach and request a reopen against dismissal D".
  - Capture's `decideMessage` carries the same fact from its lookup.
- The handler decides whether the dismissal is overtaken, under the `tasks` row lock that
  `closeTransition` already takes. It reads both instants and the message direction from COLUMNS.
  Callers supply ids only, so they cannot assert a time or a direction.
- Why the handler and not the pure `Decide`:
  - The dismissal can change between the pass's read and its call (a human reopens and
    re-dismisses).
  - A second spelling of the comparison in Go would be one more place for the rule to disagree
    with itself.
- The actors stay the same: `promote:classify` and `capture:{connector}`. Invariant 7 is
  untouched: the orchestrator is not involved.

**D5: the restore status is the pre-dismissal status, falling back to `ready`.**
- `task_dismiss` records `closeTransition`'s `from` in the new `task_dismissals.closed_from_status`.
  It is NULL when the task was already closed at dismiss time (criterion 14), and NULL on every
  pre-0026 row (no backfill: the only source is `task_events` jsonb, which SWT-31 criterion 20
  and SWT-32 D3 forbid mining).
- The handler restores it iff it is in `openStatuses`, else `ready`. This is SWT-32 D6's rule,
  the one spelling in `close.go`.
- **Why not always `ready`:** a promote review-lane task (`holding`) would go live on an inbound
  email. That would widen autonomy by message, bypassing the whitelist SWT-30 made a Go constant.
- **Why not always `holding`:** a ready task would be hidden in the review lane.
- **Blocked is re-checked (amended after the Codex re-review, 2026-09-10).** A task dismissed
  while `blocked` may have had its dependencies satisfied while it was closed: their completion
  events could not unblock a closed task (R5 only unblocks BLOCKED dependents), and
  closed → blocked fires no R5, so a verbatim restore would strand it. The guarded reopen
  restores `blocked` only while a dependency is still unmet (`depUnsatisfiedPredicate`, the
  tools package's one spelling), else `ready`. Pinned by
  `TestDismissalReopen_BlockedRestoreRechecksDependencies`. (A human's plain `task_reopen`
  with an explicit `status` is unchanged: the caller chose it.)

**D6: dismissal rows are kept. A reopen stamps them, and one task may hold several rows over
time.**
- 0026 adds `reopened_at`, `reopened_by` and `reopened_by_message_id`.
- It replaces the TOTAL unique index with a PARTIAL one: at most one OPEN dismissal per task.
- The reason is that under the total index, re-dismissing a task after an activity reopen would
  be a silent label loss (`DO NOTHING`). The task would then be plain-closed and never reopen
  again, breaking the owner's rule on the second dismissal.
- Every successful reopen of a task with an open dismissal stamps it:
  - the guarded reopen stamps `reopened_by_message_id`;
  - the plain reopen (a human undoing a mis-click) leaves it NULL;
  - the reconciler never reaches this, because D4 suppresses it first.
- **How labelled-data consumers read this.** Each row is ONE judgement, true as of its
  `created_at`.

  | Row state | Meaning |
  |---|---|
  | `reopened_by_message_id IS NOT NULL` | Overtaken by later activity. NOT evidence the label was wrong: `handled_elsewhere` was true when given. |
  | `reopened_at IS NOT NULL AND reopened_by_message_id IS NULL` | A human undid it. This is the mis-click signal, the only reopen that hints the label itself was wrong. |
  | Several rows on one task | Repeated judgement. Count rows, not tasks. |

**D7: the promotion and decision rows keep their existing actions.**
- A promote reopen records `classify_promotions.action='attached'`, with `task_id` set to the
  dismissed task. The reason says `thread's task N was dismissed (reason_code); attached, reopen
  requested against dismissal D`.
- Capture records `action='task_log'`, with a matching reason clause.
- The typed OUTCOME lives in exactly one place, `task_dismissals.reopened_by_message_id`, and
  joins to either row by message id.
- Rejected alternative: a new `'reopened'` action. It would widen two CHECKs and spell the fact
  three times, and `promote.CountersByLane` / `/funnel` would need a fourth counter.

**D8: in promote, a message that predates the dismissal attaches as log only. No new task.**
- Today's Q3 fall-through would create a duplicate of the task he just dismissed.
- `Decide` routes every message on a dismissed thread to the dismissed task. The handler's
  `message_predates_dismissal` answer leaves it closed, with the log line appended.

**D9: the reconciler's D4 reads OPEN dismissals.**
- `loadCandidates` gains `AND d.reopened_at IS NULL`.
- After an activity reopen the task is ordinary. If its ticket is Done, delivered or assigned
  away, the reconciler closes it again in the same jira tick. Capture runs first (SWT-32 D8), so
  the task ends closed with a log line explaining it. That is intended: the ticket's current
  state outranks a comment.
- That close is the reconciler's own, so it reopens the task later if the ticket warrants it
  again.
- A human re-dismissal re-arms D4.

**D10: order per message.** The existing log append runs first, then `task_reopen`.
- A crash between the two leaves exactly today's behaviour: the line is logged and the task stays
  closed.
- The claim (the live `capture_decisions` row, or the `classify_promotions` row) is spent, as with
  every existing post-claim failure. The next inbound message still reopens the task.

**D11: shadow and dry-run.** Capture's shadow mode calls no executor tool, so nothing is reopened
(`EvaluateRules` returns before the action switch when mode != live). The shadow reason may say
"would request reopen". Promote `--dry-run` prints the reopen request and writes nothing. Promote
is live only for projects with `classify_promote_after` set, so check which ones in the prod
measurement.

**D12: the dashboard gets a minimal marker.**
- The board row shows `reopened after dismissal (<reason_code>)` when the task is not closed and
  its NEWEST dismissal row has `reopened_by_message_id IS NOT NULL`.
- It is a separate read in `listTasks`, NOT in `boardQuery`: the CSV/JSON exports share
  `boardQuery` and pin their header.
- The detail page needs nothing: the `status_changed` event already renders its reason.

**D13: outbound never reopens, in three layers.**
1. Capture's `pendingMessages` is `direction = 'inbound'` (rules_store.go:439).
2. Promote's inbox inner-joins the latest `capture_decisions` row, and an outbound message can
   never have one (IK SWT-21 (7): absent-because-impossible, here working for us).
3. The handler refuses a `message_id` whose direction is not `inbound` with an ERROR, not a skip.
   This is invariant 5 at the verb, gated on a column rather than on the caller.

The observer is untouched and calls no reopen.

**D14: 0026 stamps inconsistent rows.**
- Open dismissal rows whose task is NOT closed get `reopened_at=now()`,
  `reopened_by='migration:0026'`. These are tasks reopened by psql UPDATE or by SWT-32's reopen
  before this ticket.
- Without the stamp, "an open dismissal implies a closed task" is false, and re-dismissing such a
  task conflicts on the partial index and loses the label.
- The count is measured first (Verification §4, query b).

### Races and idempotence

- **Dismiss and reopen are serialized by one lock.** Both run `SELECT … FROM tasks … FOR UPDATE`
  first (`closeTransition`), then touch `task_dismissals`. They take the same lock in the same
  order, so they cannot deadlock.
- **A message lands while Salvador is dismissing:**
  - the pass saw the task open, so it appends a log line and does not reopen. The message
    predates the dismissal in any case;
  - or the pass saw the dismissal. If the dismissal committed after the message's `created_at`,
    the handler skips.
- **A stale dismissal id** (reopened and re-dismissed between the read and the call) gets
  `skipped: dismissal_not_open`. The new dismissal is judged by the next message, not this one.
- **Replays:**
  - one live `capture_decisions` row and one `classify_promotions` row per message, forever;
  - a second guarded call for the same dismissal gets `not_closed` or `dismissal_not_open`;
  - a re-dismissal after a reopen is newer than the message, so `message_predates_dismissal`.
  - So one message id can reopen a task at most once, and never reopens a dismissal made after
    it was ingested.

## Acceptance criteria

**The verb (`internal/tools/close.go`)**

1. `validateReopen` accepts optional `dismissal_id` and `message_id` (both > 0). It refuses:
   - `message_id` without `dismissal_id`;
   - `dismissal_id` without `message_id`;
   - `status` together with `dismissal_id` (the dismissal decides the target, D5).

   Each refusal names the offending field.
2. A guarded call, inside ONE transaction that first locks the `tasks` row, in this order:
   - (a) If the message does not exist, or its `direction <> 'inbound'`: ERROR, nothing written.
   - (b) If the task is not `closed`: success `{reopened:false, skipped:"not_closed"}`.
   - (c) If there is no row with that id for that task with `reopened_at IS NULL`:
     `{reopened:false, skipped:"dismissal_not_open"}`.
   - (d) If `message.created_at <= dismissal.created_at`:
     `{reopened:false, skipped:"message_predates_dismissal"}`.
   - (e) Otherwise it transitions via `closeTransition` to `closed_from_status`, if that value is
     in `openStatuses`, else `ready`. It stamps the dismissal with
     `reopened_at=now()`, `reopened_by=<actor>`, `reopened_by_message_id=<message_id>`, and
     returns `{task_id, status, reopened:true, dismissal_id}`.
3. The `status_changed` reason written in 2(e) contains:
   - the literal `reopened after dismissal`;
   - the reason code, the dismissal time and `dismissed_by`;
   - the message id, sender and ingest time;
   - the caller's `reason`.
4. An unguarded `task_reopen` that transitions a task stamps any open dismissal of that task
   (`reopened_by_message_id` NULL). Its existing behaviour is otherwise byte-identical (SWT-32
   criteria 36-37), and `reopen_integration_test.go:326` still sees exactly one row.
5. `task_dismiss` writes `closed_from_status` = `closeTransition`'s `from` when it transitioned,
   and NULL when the task was already closed.
6. `task_dismiss`'s insert is
   `ON CONFLICT (task_id) WHERE reopened_at IS NULL DO NOTHING`, with the predicate RESTATED.
   After an activity reopen, a second dismiss inserts a SECOND row. A replay of that second
   dismiss adds nothing.
7. `task_reopen` stays off `internal/mcpserver/schemas.go` and out of `humanOnly`.

**Promote (`internal/promote`)**

8. `ExistingTask` gains `DismissalID int64` (plus the reason code, for prose). `Decision` gains
   `ReopenDismissalID int64`. `Decide` rules in order:
   - an open task gives attach;
   - else a task with `DismissalID != 0` gives `{Action:"attached", TaskID, ReopenDismissalID}`,
     whatever the kind (whitelisted or not);
   - else the existing whitelist/holding rules.
9. `promote_test.go` (pure, offline, criterion 6's imports) covers four cases:
   - open beats dismissed;
   - dismissed plus a whitelisted kind attaches and never creates a task;
   - a non-dismissed closed or delivered task keeps the Q3 new-task behaviour;
   - nil existing is unchanged.
10. `threadTask` gains a middle lookup: after "open" and before "finished", it looks for the
    oldest `status='closed'` task on the thread and project with an open dismissal (inner join,
    `reopened_at IS NULL`).
11. On a reopen decision, `Run`:
    - claims (reason per D7);
    - calls `task_append_log` (existing text);
    - records `task_id` (amended at review: BEFORE the reopen, the create path's "record before
      provenance" rule — the claim is already spent, so a failed reopen must not also lose the
      task pointer);
    - calls `task_reopen` with `{task_id, dismissal_id, message_id, reason}` as `promote:classify`.

    `Stats` gains `Reopened` (counted from `reopened:true`). Dry-run prints the request and
    writes nothing.
12. A message that predates the dismissal (D8) yields `action='attached'` on the dismissed task,
    one log line, the task still closed, and NO new task.

**Capture (`internal/capture/rules_store.go`)**

13. `taskForExternalRef` also returns the task's open dismissal id: present iff
    `tasks.status='closed'` and a row with `reopened_at IS NULL` exists. `ruleDecision` carries it.
    The `task_log` reason gains `task N was dismissed (reason_code); reopen requested against
    dismissal D`.
14. In live mode, for a `task_log` decision carrying a dismissal id, `appendRuleLog` runs (as
    today), then `task_reopen` guarded, as the configured `capture:{connector}` actor. A failed
    call fails the pass (linkRuleRef's policy). `RulesStats` gains `Reopened`, which is zero in
    shadow, always.
15. A closed task WITHOUT an open dismissal is unchanged: a log line, and it stays closed.

**Reconciler, dashboard, invariant 5**

16. `loadCandidates` treats a task as dismissed only with an open dismissal. A task that was
    activity-reopened, then closed by the pass, gets `reopened` when its ticket is warranted
    again, not `suppressed_dismissed`. An open dismissal still suppresses (SWT-32 criteria 26 and
    28 unchanged).
17. The board row shows the D12 marker. It is absent for a human plain reopen, and absent once the
    task is re-dismissed. The CSV/JSON export header and columns are unchanged.
18. An outbound message on a dismissed task's thread or ref never reopens it:
    - it is absent from capture's pending set;
    - it is absent from promote's inbox;
    - `ObserveOutbound` over the thread writes `outbound_observed` only;
    - a guarded `task_reopen` naming it errors (2a).

**Migration and structure**

19. `migrations/0026_dismissal_reopen.sql` matches the Data model section. `TestMigration0026_*`
    pins it:
    - the four columns;
    - both CHECKs;
    - the old index dropped;
    - the partial unique `(task_id) WHERE reopened_at IS NULL`;
    - the D14 backfill;
    - no other index.

    The ledger in `internal/classify/structure_test.go:1162` admits 26. The 0022 guard is left
    as is (it scopes to its own file) and gains an AMENDED comment pointing at 0026.
20. The direct-write bans gain `task_dismissals`:
    - `internal/capture/rules_structure_test.go:78-79`;
    - `internal/promote/structure_test.go:220-221`.

    Neither package writes the table. Only the executor verb does.

**Integration: "test the column, not the fixture"** (compose db, `itest-` prefix, `-p 1`).
Each criterion below names the mutation that must turn it red.

21. **Clock (D2).**
    - Two messages on one dismissed task: `sent_at` before the dismissal with `created_at` after,
      which must reopen; and `sent_at` after with `created_at` before, which must skip.
    - Mutation: switch the handler to `sent_at` (or to `COALESCE(sent_at, created_at)`). Both go
      red.
22. **Restore (D5).**
    - Dismiss a `holding` task, then an activity reopen must restore `holding`.
    - Mutation: drop `closed_from_status` from dismiss's INSERT or the handler's SELECT. It
      restores `ready`, and the test goes red.
23. **Open predicate.**
    - Promote lookup: a task dismissed, activity-reopened, then `task_close`d is plain-closed.
      The next verdict creates a new task (Q3).
    - Mutation: drop `reopened_at IS NULL` from `threadTask`, and it reopens instead. Same test
      for `taskForExternalRef`, and for criterion 16 in `loadCandidates`.
24. **Partial index.**
    - Criterion 6's second dismiss inserts a second row.
    - Mutation: remove the restated predicate. A runtime ON CONFLICT error fails the test.
25. **Outbound (2a).**
    - A guarded call with an outbound `message_id` errors and changes nothing.
    - Mutation: remove the direction check. The task reopens, and the test goes red.
26. **Every fixture is shaped like production.**
    - It seeds BOTH an inbound and an outbound message on the thread.
    - It dismisses via `task_dismiss` through the executor, never by INSERT, so
      `closed_from_status` and `created_at` come from the real code path.

## Data model changes

Migration **0026** (`migrations/0026_dismissal_reopen.sql`). 0025 is the current highest.

```sql
ALTER TABLE task_dismissals
  ADD COLUMN closed_from_status     TEXT,         -- D5; NULL = was already closed / pre-0026
  ADD COLUMN reopened_at            TIMESTAMPTZ,  -- D6; NULL = the dismissal is OPEN
  ADD COLUMN reopened_by            TEXT,         -- executor actor of the reopen
  ADD COLUMN reopened_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL,
  ADD CONSTRAINT task_dismissals_reopen_pair CHECK ((reopened_at IS NULL) = (reopened_by IS NULL)),
  ADD CONSTRAINT task_dismissals_reopen_msg  CHECK (reopened_by_message_id IS NULL OR reopened_at IS NOT NULL);

-- D14: an open dismissal must imply a closed task.
UPDATE task_dismissals d SET reopened_at = now(), reopened_by = 'migration:0026'
  FROM tasks t WHERE t.id = d.task_id AND t.status <> 'closed' AND d.reopened_at IS NULL;

DROP INDEX task_dismissals_task_uniq;
CREATE UNIQUE INDEX task_dismissals_open_uniq ON task_dismissals (task_id) WHERE reopened_at IS NULL;
```

- **No CHECK on `closed_from_status`.** It follows 0023's precedent, and the handler's
  fallback to `ready` is the guard. `openStatuses` stays the one spelling.
- **`ON DELETE SET NULL`, not CASCADE,** on the message FK: deleting a message must not delete a
  human label. Production never deletes `normalized_messages` (0015's recorded fact). Only test
  cleanup does.
- **The file needs the IK partial-index landmine comment.** Every `ON CONFLICT` against
  `task_dismissals` must restate `WHERE reopened_at IS NULL`.
- **Migrate runs the file in one transaction,** so the drop, the backfill and the create happen
  atomically.
- **No new tables and no task-like rows** (invariant 2). `task_dismissals` remains a log.

## API / MCP tool changes

**`task_reopen` (modified).** It stays spine-facing, not humanOnly and not MCP-listed.

```
args:   {task_id, reason, status?, dismissal_id?, message_id?}
        dismissal_id and message_id: both or neither; status forbidden with them
result: {task_id, status, reopened, skipped?: not_closed|dismissal_not_open|message_predates_dismissal,
         dismissal_id?}
```

**`task_dismiss` (modified, same surface).** It writes `closed_from_status` and uses the partial
`ON CONFLICT`. Its result shape is unchanged.

**The executor path (invariant 3).** Both are called as `executor.Call{Tool:"task_reopen", …}`,
which runs validate → policy (static fallthrough: not humanOnly, not sendShaped) → audit start →
`reopenTask` → audit complete. The callers are `promote.Run` (new helper beside
`appendVerdictLog`) and `capture.EvaluateRules` (new helper beside `appendRuleLog`). Neither
package gains SQL against `tasks`, `task_events` or `task_dismissals`.

## MQTT topics

None.

## Files likely to touch

- `migrations/0026_dismissal_reopen.sql` (new)
- `internal/tools/close.go`: `reopenArgs`, `validateReopen`, `reopenTask`, `dismissTask`
- `internal/tools/reopen_integration_test.go` (extend) or a new
  `internal/tools/dismissal_reopen_integration_test.go`
- `internal/tools/dismiss_structure_test.go` (AMENDED comment), plus a new
  `internal/tools/dismissal_reopen_structure_test.go` (the 0026 guard)
- `internal/classify/structure_test.go:1162` (the ledger)
- `internal/promote/promote.go`: `ExistingTask`, `Decision`, `Decide`
- `internal/promote/store.go`: `threadTask`, `decisionReason`, `Run`, the new reopen helper,
  `Stats`
- `internal/promote/promote_test.go`, `internal/promote/store_integration_test.go`,
  `internal/promote/structure_test.go`
- `internal/capture/rules_store.go`: `taskForExternalRef`, `ruleDecision`, `decideMessage`,
  `EvaluateRules`, the new reopen helper, `RulesStats`
- `internal/capture/rules_integration_test.go` (or a new `rules_reopen_integration_test.go`),
  `internal/capture/rules_structure_test.go`, `internal/capture/observe_integration_test.go`
  (criterion 18)
- `internal/ticketstatus/store.go:494`; the comment at `decide.go:29`;
  `internal/ticketstatus/store_integration_test.go`
- `internal/dashboard/board.go` (`listTasks`, `taskRow`), `templates/tasks.html`, and a new
  `internal/dashboard/board_reopen_integration_test.go`
- Wherever `RulesStats` and `promote.Stats` are printed: `cmd/opsctl/main.go`,
  `cmd/classify/main.go`, and the connector mains
- Docs:
  - `docs/runbooks/capture-rules.md` and `docs/runbooks/local-classifier.md`: one paragraph each;
  - `docs/runbooks/ticket-status-sync.md`: D4 now means an OPEN dismissal;
  - `.claude/INSTITUTIONAL_KNOWLEDGE.md`: an entry covering the partial index, the clock choice
    and D3's scope.

## In scope / Out of scope

**In scope:** everything above.

**Out of scope:**
- **A board "Reopen" button.** Salvador's ask is automatic reopening. The manual remedy remains
  `opsctl call task_reopen`.
- **Reopening on new activity for plain-closed, R8-closed, reconciler-closed or delivered tasks**
  (D3).
- **New matching paths.** That includes capture thread-matching for attribution-only rules, and
  triage-live attach (step 6, still shadow).
- **The inquiry lane (SWT-33).** It writes no tasks.
- **Narrowing D1**, e.g. excluding notification bots. That is a follow-up if the measurement
  shows churn.
- **A dismissals panel on `/funnel`,** and the precision report over `task_dismissals` (SWT-31
  future work).

## Invariants that apply

1. **Raw-first:**
   - Reprocessing must not re-trigger a reopen. Handled by D2's ingest-time clock, which is
     stable because the sink upserts never touch `created_at`, plus the per-message claims.
   - The dismissal row is kept (D6), so labelled data survives any replay.
2. **One funnel:**
   - A message on a dismissed thread returns to the SAME task instead of spawning a duplicate
     (D8), which is invariant 2's intent.
   - `task_dismissals` stays a log. No table is added.
3. **Everything through the executor:**
   - The reopen is `task_reopen` on the executor, with an audit row per call.
   - Capture and promote gain no direct writes. The structure tests extend their bans to
     `task_dismissals` (criterion 20).
   - The dashboard marker is a read.
4. **Nothing external without a delivery row:** nothing here sends. A reopened Deliver task (only
   if it was dismissed) re-enters the draft worker's inbox. Drafts are `drafted` rows gated by
   approval, so no send is reachable without a delivery row that passes policy.
5. **Own-message loop closure:** D13's three layers. The handler's direction check is the
   structural backstop, and criterion 18 tests each layer.
6. **Stealth attribution:** there is no client-visible output. Log lines are internal.
7. **Orchestrator purity:**
   - The orchestrator is untouched.
   - `promote.Decide` stays pure and offline (criterion 9 plus the existing import guard).
   - The reconciler's `Decide` is unchanged. Only its input query changes.
   - `closed → X` emits `status_changed`. The orchestrator acts only on `to ∈ {delivered,closed}`
     (`rules.go:124-129`), and that is harmless.

## Sibling patterns to copy

- **Guarded transition inside the row lock:** `closeTransition` and `dismissTask` (`close.go:45`,
  `:215`). Use Exec + RowsAffected, not QueryRow + ErrNoRows, for the stamp (the go-reviewer note
  at `close.go:233`).
- **Partial-index ON CONFLICT with the restated predicate:** `capture.insertDecision`
  (`rules_store.go:689-698`).
- **A pure decision taking the dismissal as an input:** `ticketstatus.Observation.Dismissed`
  (`decide.go:29`) and SWT-30's `ExistingTask.Status`.
- **A post-claim executor call that fails the pass:** `linkRuleRef` and `setRuleProvenance`
  (`rules_store.go:766-812`).
- **A migration guard:** `TestMigration0022_*` in `dismiss_structure_test.go`.
- **Integration setup:** `reopen_integration_test.go`'s suite helpers (`s.task`, `s.call`,
  `roHumanActor`, FK-ordered cleanup).
- There are no queue claims and no HTMX verbs here. The jobagent and rag-scv siblings are not
  needed.

## Verification protocol

1. **Unit tests:** `go test ./...` passes. The structure tests (0026 guard, ledger, direct-write
   bans) and the pure `Decide` / `validateReopen` tests run with no db.
2. **Integration tests:**
   `make db-up && make migrate && DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable go test -tags integration -p 1 ./internal/tools/... ./internal/promote/... ./internal/capture/... ./internal/ticketstatus/... ./internal/dashboard/...`,
   then the full `make integration`.
   - Never against 192.168.50.49.
   - Fixtures use the `itest-` prefix and join the mutual-cleanup pact. `task_dismissals` rows
     cascade with tasks.
3. **Mutation checks (criteria 21-25):** apply each named mutation by hand, confirm red, revert.
   Record each result in the ticket's deliver notes.
4. **Prod read-only measurement.** The main thread runs this with
   `psql -h 192.168.50.49 -U ops -d ops`, before implementation and again at deliver. There are
   no writes; wrap it in `BEGIN READ ONLY; … ROLLBACK;`.

   ```sql
   -- (a) dismissals by reason and task status
   SELECT d.reason_code, t.status, count(*) FROM task_dismissals d JOIN tasks t ON t.id=d.task_id
    GROUP BY 1,2 ORDER BY 1,2;
   -- (b) D14's backfill set: open dismissals on a non-closed task
   SELECT d.task_id, t.status, d.created_at FROM task_dismissals d JOIN tasks t ON t.id=d.task_id
    WHERE t.status <> 'closed';
   -- (c) capture path: inbound messages logged onto a dismissed task AFTER the dismissal
   SELECT d.task_id, d.reason_code, count(*) AS msgs, min(m.created_at), max(m.created_at)
     FROM task_dismissals d
     JOIN capture_decisions cd ON cd.task_id=d.task_id AND cd.mode='live' AND cd.action='task_log'
     JOIN normalized_messages m ON m.id=cd.message_id AND m.direction='inbound' AND m.created_at > d.created_at
    GROUP BY 1,2 ORDER BY msgs DESC;
   -- (d) promote path (upper bound: thread-level, any lane): inbound on the task's thread after dismissal
   SELECT d.task_id, d.reason_code, count(*) AS msgs
     FROM task_dismissals d JOIN tasks t ON t.id=d.task_id
     JOIN normalized_messages m ON m.thread_id=t.source_thread_id AND m.direction='inbound' AND m.created_at > d.created_at
    GROUP BY 1,2 ORDER BY msgs DESC;
   -- (e) D2's lag case, as data: sent before the dismissal, ingested after
   SELECT count(*) FROM task_dismissals d JOIN capture_decisions cd ON cd.task_id=d.task_id AND cd.mode='live'
     JOIN normalized_messages m ON m.id=cd.message_id
    WHERE m.direction='inbound' AND m.sent_at < d.created_at AND m.created_at > d.created_at;
   -- (f) senders of post-dismissal inbound (the bot-churn check for D1)
   SELECT m.sender, count(*) FROM task_dismissals d JOIN capture_decisions cd ON cd.task_id=d.task_id AND cd.mode='live'
     JOIN normalized_messages m ON m.id=cd.message_id AND m.direction='inbound' AND m.created_at > d.created_at
    GROUP BY 1 ORDER BY 2 DESC LIMIT 20;
   -- (g) where promote is live, and today's Q3 duplicates past a closed task
   SELECT slug, classify_promote_after FROM projects WHERE classify_promote_after IS NOT NULL;
   SELECT count(*) FROM classify_promotions WHERE reason LIKE '%created a new task (Q3%';
   ```

   - The distinct task count of (c) ∪ (d) is the number of tasks that would have come back had
     this shipped earlier. Report it with the total from (a).
   - If (f) is dominated by notification senders, record that. A narrowing is a follow-up, not a
     change to this SPEC.
   - Do not freeze these counts into tests (IK: the corpus is live).
   - **Baseline, measured before implementation (2026-09-10, prod, read-only):**
     - (a) 17 dismissals, all on `closed` tasks: not_actionable 14, handled_elsewhere 2,
       wrong_kind 1.
     - (b) 0 rows, so migration 0026's backfill stamps nothing.
     - (c) 0, (d) 0, (e) 0: no dismissed task has post-dismissal inbound on either path, so
       nothing reopens at ship time.
     - (f) no senders to judge yet.
     - (g) promote is live for `personal` only (since 2026-09-09 13:03 UTC), with 0 Q3
       duplicates so far.
     - `schema_migrations` max is 0025, as step 5 requires.
5. **Deploy order — a coordinated CUTOVER, not migrate-then-roll (Codex review, 2026-09-10):**
   - **Why neither order is safe on its own.** Pre-SWT-36 code dismisses with
     `ON CONFLICT (task_id) DO NOTHING`; after 0026 that target cannot infer the now-PARTIAL
     index, so every old writer's dismiss fails at runtime. New code writes
     `closed_from_status` and restates `WHERE reopened_at IS NULL`, so it fails before 0026.
     The errors are loud (no row is corrupted), but the dismiss button breaks during skew.
   - **The old `task_dismiss` writers** are: the dashboard deployment; the installed
     user-scope `ops-mcp-user` binary (SWT-37); any installed `opsctl`; any running
     `ops-mcp` session. Worker consoles cannot dismiss (humanOnly).
   - **The sequence:**
     1. Run `SELECT max(version) FROM schema_migrations`; it must be 25.
     2. Stop the old writers: scale the dashboard to 0 (kube session); close open Claude Code
        sessions that load `ops`.
     3. Apply 0026.
     4. Deploy the new dashboard image and the CronJob images carrying this code (the tag bump
        is the kube session's; the image build happens here), and re-install
        `go install ./cmd/ops-mcp-user` and `./cmd/opsctl` from `main`.
     5. Scale the dashboard back up; open a new session.
     6. Re-run Verification §4 query (b); expect 0 rows. A row means an old `task_reopen` ran in
        the window and left an open dismissal on an open task — stamp it by hand (`reopened_at`,
        `reopened_by='cutover'`).
   - Do not dismiss anything during the window. (A zero-downtime alternative — keep the total
     index in 0026 and drop it in a later 0027 — was weighed at review and rejected: with 17
     dismissals in production the drain costs a minute.)
   - The IK drift landmine ("apply the migration BEFORE any image carrying the code") still
     holds for step 3 vs step 4; this ticket adds the drain in step 2 because the migration also
     breaks the OLD code.
6. **Live smoke (after deploy):**
   - Pick a dismissed task from (c) whose ticket or thread is active. Wait for, or ask for, one
     inbound message.
   - Confirm all of these:
     - `SELECT status FROM tasks WHERE id=N` gives the pre-dismissal status;
     - `SELECT reopened_at, reopened_by, reopened_by_message_id FROM task_dismissals WHERE task_id=N`
       shows the stamp;
     - `/tasks` shows the marker;
     - `/tasks/N` shows the `status_changed` reason with the reason code.
   - Then dismiss it again and confirm a second `task_dismissals` row exists.

## Open questions

None arose. The two calls that could have been owner questions were:
- the clock (D2);
- whether bot notifications count (D1).

Both are decided above with rationale, and the prod measurement (§4 e/f) checks each with data
before go-live.

## Future work

- A board Reopen button (the human mis-click remedy, today `opsctl call task_reopen`).
- Narrowing D1 by sender class, if §4(f) shows notification churn.
- A dismissal-precision report reading D6's semantics: rows, not tasks; message-reopened vs
  human-reopened.
- Capture thread-matching for attribution-only rules, so a dismissed task with no external ref
  can also come back.
