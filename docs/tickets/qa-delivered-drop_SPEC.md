> Jira: SWT-34

# qa-delivered-drop — a third `warranted` clause: a ticket sitting in a configured "delivered" status is not my turn, so its task drops

## Source

Ad-hoc, from Salvador, verbatim:

> jira tickets on collaboratory in qa drop as taks (qa means i delivered) so drop
> unless reopened or a fresh question on them

Not a build-order step. It is maintenance on the reconciler SWT-32 shipped on
2026-09-09 (`internal/ticketstatus`, `docs/tickets/jira-status-sync_SPEC.md`,
`docs/runbooks/ticket-status-sync.md`). **This ticket EXTENDS that reconciler and
re-litigates none of its decisions** — in particular D2 stands untouched; see
"The design tension" below, which exists precisely so a future reader does not
"fix" the apparent contradiction.

Verified against the live `ops` db on 2026-09-10 — the collaboratory tickets that
currently have tasks, by `statusCategory` and status name:

```
done          TT-Verified            11 refs   (tasks already closed by the reconciler)
done          TT-Closed               3        (closed)
indeterminate TT-In QA                8        (tasks all still OPEN on the board)  <-- the ask
indeterminate TT-In Review            2        (open)
indeterminate TT-Work In Progress     1        (open)
new           TT-New                  1        (open)
new           TT-Reopened             1        (open)
```

Do **not** copy those counts into a test as literals. The corpus is live (the
recorded rule from SWT-19: "a literal cries wolf every day a message arrives").
They are here to size the work and to name the exact status string that gets
seeded.

## Goal

Add a third, per-project **configured** clause to `ticketstatus`'s existing
`warranted` predicate — "the ticket's status name is in this project's
delivered-statuses set" — so that a ticket Salvador has handed back (Treetop's
`TT-In QA`) drops its task off the board, and a ticket that leaves that set
(`TT-In QA` → `TT-Reopened`, or → `TT-Work In Progress`) brings the task back
through the reopen path SWT-32 already built.

**Usable alone means:** with nothing new deployed, one `UPDATE projects SET
ticket_delivered_statuses = ARRAY['TT-In QA'] WHERE slug='collaboratory'` followed
by one hand-run `opsctl ticket-status sync` takes the 8 `TT-In QA` tasks off
`/tasks` (the board hides `closed` by default), records
`drop_reason='ticket_delivered'` for each, and leaves an `audit_events` +
`task_events` trail. Moving one of those tickets out of QA in Treetop's Jira and
running the connector + the pass puts its task back in the status it held. Every
other project — reengine, saka, foundry, town-ai, homelab, personal — behaves
byte-identically to today, because their column is `'{}'` and the clause is inert.
Reverting is the same one `UPDATE` back to `'{}'`, and the next pass restores the
tasks.

## The design tension, faced explicitly (read this before "fixing" D2)

SWT-32's **D2** says the status discriminator is
`fields.status.statusCategory.key`, **never a list of status names**, because
names are per-project workflow configuration and a hardcoded name list "would pass
every fixture and then silently stop closing tasks the day a client renames a
column". That decision is **correct and is not being overturned.**

It is correct for the question it answers: *is this ticket finished?* Jira itself
owns that fact and exposes it as a three-value structure every custom status maps
into.

Salvador's ask is a **different question**: *is the ball in my court?*
`statusCategory` cannot express it, and the live data proves it rather than
arguing it — `TT-In QA` and `TT-Work In Progress` are BOTH `indeterminate`, yet
one means "I delivered, they are checking" and the other means "I am mid-build".
No function of `statusCategory` can separate them. There is no Jira-level
structure for "not my turn"; it exists only in a particular team's column layout.

So the honest resolution is **not** to weaken D2 but to add a **second predicate
of a different kind**:

| | SWT-32 D2 (`ticket_done`) | this ticket (`ticket_delivered`) |
|---|---|---|
| question | is the ticket finished? | is the ball in my court? |
| authority | Jira's own structure | Salvador's knowledge of one team's workflow |
| discriminator | `statusCategory.key` | `status.name` |
| where it lives | **code** (three keys, fixed) | **a `projects` column** (per project, hand-armed) |
| default | always on | `'{}'` — inert until armed |

The rule that follows, and the one to state whenever this comes up again:

> **A workflow-shaped fact belongs in configuration, never in code.** D2 bans a
> status-name list *in the binary*. It does not ban knowing a client's workflow —
> it bans hard-wiring one. A `TEXT[]` on `projects` is the same shape as
> `ai_locality` (0016), `ai_classify` (0018), `classify_promote_after` (0021) and
> `ticket_assignee_gate` (0023): a typed column, fail-closed default, armed by a
> hand-run `UPDATE` recorded in the runbook. When Treetop renames the column, the
> failure is a task that stays on the board and an operator who edits one row —
> not a silent stop that needs a code change and a deploy.

And the mechanical consequence: **criterion 8's structural ban gets STRONGER, not
weaker.** `internal/ticketstatus` and the jira readers still must not contain a
status-name literal — and this ticket adds the three real Treetop names to the
banned list, so the very strings being seeded are the ones the compiler-adjacent
guard refuses. The names live in the database or they do not exist.

The one thing that genuinely changes: `jira.Facts.StatusName`'s comment says
"DIAGNOSTIC ONLY; nothing branches on it", and `ticket_status_syncs.status_name`
says the same in migration 0023. After this ticket **something does branch on it**
— through configuration. Both comments must be corrected in the same diff. A
comment that states the opposite of its code is a recorded defect class in this
repo ("Also from this ticket: a comment can be a defect"), and this is exactly how
one gets created.

## What the investigation established

1. **The name is already parsed and already stored.**
   `internal/connector/jira/facts.go` `IssueFacts` reads `fields.status.name` into
   `Facts.StatusName` today, and `internal/ticketstatus/store.go` already copies it
   into `Observation.StatusName`. **No new fetch, no new raw write, no new parse.**
   This ticket's whole reading half is "stop ignoring a value already in hand".
2. **`StatusName` is only ever populated for a readable status.** `IssueFacts`
   sets `StatusKnown`, `StatusCategory` and `StatusName` together, and only when
   the category is one of the three navigable keys — so Jira's fourth key
   (`undefined`) still yields `StatusKnown=false` and the ref is `unreadable`
   before the new clause is ever consulted (SWT-32 go-reviewer F1). The delivered
   predicate can therefore never fire on a status the reader could not classify.
3. **`warranted` is spelled in exactly one place**: `ticketstatus.Decide`
   (`internal/ticketstatus/decide.go:92`), a pure function with a structural purity
   guard (`TestDecideGo_IsPure` bans `context`, `pgx`, `os.Getenv`, `net/http`, the
   jira client and the executor from that file).
4. **The reopen path is already general.** `Decide`'s warranted branch reopens on
   `state.LastAction == "closed"` alone — it does not consult *which* fact closed
   it. So a third drop cause gets its return path for free the moment it is a term
   in the same predicate.
5. **`drop_reason` is CHECK-constrained to two values** (migration 0023:41,
   `CHECK (drop_reason IN ('ticket_done','not_assigned'))`). A third value needs a
   forward-only migration. `ls migrations/` currently tops out at
   `0023_ticket_status_sync.sql`, so **this ticket is 0025**.
6. **The migration ledger is a living registry.** `internal/classify/structure_test.go:1054`
   reads `if n > 17 && n != 18 && n != 19 && n != 20 && n != 21 && n != 22 && n != 23`.
   It must learn 25. Rewrite the guard to the new truth; never delete it.
7. **`projects` fixtures that omit a new column get its default**, and here the
   default is the INERT side. Unlike 0016's `ai_locality` trap (where the default
   made 23 suites start *skipping*), `'{}'` means "behave exactly as before", so no
   existing suite changes behaviour. The two `ticketstatus` integration fixtures
   name `ticket_assignee_gate` explicitly (`store_integration_test.go:295,298`) and
   will name this column explicitly too.
8. **The `refused_active` dedup key does not currently include the name.**
   `loadCandidates` selects `s.status_category, s.assignee_account_id` into `State`
   and `Decide` compares those two to decide whether an observation is "unchanged"
   (`decide.go:109-110`). After this ticket the drop-triggering fact can change
   (`TT-In Review` → `TT-In QA`, both `indeterminate`, same assignee) without either
   stored field moving — so a claimed task would silently not get its second log
   line. E6 widens the key.
9. **A later notification about a dropped ticket still lands on the closed task.**
   `internal/capture/rules_store.go` `appendRuleLog` calls `task_append_log` for any
   message whose external key already has a task, with no status filter. So a client
   question on a QA ticket IS recorded on the closed task as a `log` task_event —
   it is just not surfaced anywhere. That is the honest gap E9 defers and the
   runbook must state.
10. **No new advisory lock.** This ticket runs inside `ticketstatus.Run`, which
    already holds one, spelled exactly once in `store.go`. This SPEC deliberately
    prints no `0x5157…` literal anywhere, and neither may any new test file — the
    repo-wide collision scan in `internal/classify/structure_test.go` reads a
    restated key as a duplicate.

## Decisions made unilaterally (with rationale)

Numbered **E1…** rather than continuing SWT-32's `D1…D21`, so that a reader with
both documents open never has to guess which ticket a `D` belongs to.

**E1 — the delivered set is `projects.ticket_delivered_statuses TEXT[] NOT NULL
DEFAULT '{}'`: a typed column, per project, hand-armed.** Not a `policies` jsonb
key (0016 and 0018 both record at length why an untyped predicate over jsonb is
the thing this repo keeps paying for, and 0023 restates it for
`ticket_assignee_gate`). Not a new table — it is one attribute of a project, and a
`project_delivered_statuses` side table would be a second vocabulary for a
one-column fact. `TEXT[]` because `source_accounts.scopes` (0001:16) is the repo's
existing "a small set of labels on a row" shape, it needs no join, and `NOT NULL
DEFAULT '{}'` removes 0018's recorded nullable-column trap (`AND p.ai_classify`
silently excluding every row nobody set) before it can happen. Empty is
today's behaviour exactly — the arming convention of `classify_promote_after` and
`ticket_assignee_gate`, verbatim.

**E2 — matching is EXACT on a normalized form, never substring or prefix.**
The fold: lowercase, and collapse every run of Unicode whitespace to one space,
trimming the ends — `strings.ToLower(strings.Join(strings.Fields(s), " "))`.
Rationale on both halves:
- *Why normalize at all.* These are human-typed labels, on both sides: Jira
  serialises whatever an admin typed into the workflow column, and the configured
  entry is pasted into a `psql` `UPDATE` by hand. A trailing space or an NBSP
  copied out of a browser is the realistic typo, and it would make an armed set
  match nothing — an armed feature that looks identical to an unarmed one.
  `strings.Fields` splits on `unicode.IsSpace`, which is what makes the NBSP case
  work; that is the same reasoning `internal/textmatch` records.
- *Why NOT substring.* `strings.Contains(name, "QA")` would match `TT-In QA`,
  `TT-QA Blocked` and `TT-Needs QA Rework` — two of which mean the ball IS in his
  court. A partial predicate over a human label is the magic-literal defect
  wearing a third costume; the configured set is a set of whole names, and adding
  a name is one array element.

**E3 — the fold lives in Go, in ONE function, and NEVER in SQL.** The candidate
query SELECTs the array as-is; the comparison happens in
`ticketstatus.IsDeliveredStatus`. This is the recorded textmatch rule applied to a
second kind of label: "Do NOT re-spell it in SQL — Postgres's POSIX `\s` does not
cover the unicode spaces Go's `strings.Fields` does, so an NBSP alone makes the two
disagree, silently." A `WHERE lower(btrim(...)) = ANY(p.ticket_delivered_statuses)`
would be that second spelling, and it would also move the predicate out of the
pure `Decide` where invariant 7 wants it. Enforced structurally (criterion 12).

**E4 — a configured entry that normalizes to the empty string is IGNORED.**
`ARRAY['']`, `ARRAY[' ']` or a stray trailing comma must not become "matches every
status whose name we could not read". That is the "discriminating column is a
constant in production" landmine pre-empted: an empty entry silently matching
everything would drop a client's whole board with no error. Both sides are guarded
— an empty *observed* name is never a member either (E5).

**E5 — an empty observed status NAME is "not delivered", not an evidence gap.**
Reaching `Decide` at all means `StatusKnown` is true, so the CATEGORY was
readable; only the name is missing. Counting it `unreadable` would suppress the
**reopen** direction too, stranding a task the pass had closed. "Not a member"
keeps the task on the board (the fail-safe direction, same as D12's) and keeps the
return path alive. In practice Jira always sends a name alongside a category; this
is the defensive reading, not an expected path.

**E6 — the drop-triggering NAME joins the "unchanged observation" key.**
`State` gains `StatusName` (from the `ticket_status_syncs.status_name` column that
already exists), `loadCandidates` selects it, and `Decide`'s `refused_active`
sameness check compares it. Without this, a claimed task whose ticket moves
between two configured delivered statuses — or between a non-delivered and a
delivered status of the same category and assignee — gets no second log line, and
the log then names a status the ticket left (fact 8). It also closes the same gap
for a plain rename inside one category, which was latent and harmless before this
ticket and is not after it.

**E7 — precedence: `ticket_done` > `ticket_delivered` > `not_assigned`, spelled as
ONE ordered list.** `warranted` keys on the conjunction alone (D15 unchanged); the
ordered list only decides which `drop_reason` is RECORDED. Rationale:
- `ticket_done` keeps the top slot it holds today. D15 already made status
  precede assignment, and the strongest statement about a ticket is that it is
  finished. A status mapped into the `done` category while *named* `TT-In QA` (an
  admin's choice we do not control) is `ticket_done`, not `ticket_delivered`.
- `ticket_delivered` sits **directly under it and above `not_assigned`**, because
  it is the same kind of fact — a statement about the ticket's own lifecycle — and
  a weaker version of the one above it ("finished from my side, awaiting theirs").
  A QA ticket that is also assigned to a QA engineer is dropped *because he
  delivered it*; `not_assigned` there would be an artifact of the handoff, and the
  counter Salvador reads to judge whether a capture rule is too broad would be
  wrong.
- Inserting it between the two leaves today's `ticket_done` > `not_assigned`
  relation byte-identical, so no currently-recorded reason changes meaning.
- Nothing in production can exercise the new pairs yet: no project has both the
  gate armed (`reengine`) and a delivered set (`collaboratory`). Precedence is
  therefore pinned by the decision table and by the migration's comment, not by
  observed behaviour — say so rather than implying it is tested in the field.
- ONE ordered list, because two independent `if` chains is how the recorded
  reason and the counter drift apart.

**E8 — this is a CLAUSE in `warranted`, not a second pass and not a second
mechanism.** Everything Salvador asked for on the return path — "unless reopened"
— falls out of that single choice: a ticket that leaves the delivered set makes
`warranted` true, and `Decide`'s existing branch reopens it to
`closed_from_status` (fact 4). Close, reopen, dismissal suppression (D4), the
active-work refusal (D6/criterion 24), the state row, the idempotence property and
the audit trail are all inherited unchanged. A separate "QA sweep" would need its
own state, its own reopen authority and its own interaction with `task_dismissals`
— three chances to disagree with the reconciler about the same task. The
migration's comment says this out loud.

**E9 — "or a fresh question on them" is DEFERRED to a follow-up ticket,
`qa-question-resurface`, and this SPEC records the hook it must use.** Reasons:
- the inquiry-classify lane (the local qwen classifier that decides whether an
  inbound message is an inquiry needing a reply) is being specced concurrently and
  ships **shadow-only on day one** — it creates nothing. Blocking the QA drop on
  it would deliver neither;
- a question is **not** a `warranted` fact. The ticket's status does not change
  when a client asks something, so `warranted` stays false and the very next
  reconciler pass would re-close the task within minutes. Bolting the resurface
  onto `warranted` is the wrong mechanism, and would be an actively harmful
  15-minute flap;
- the two halves are independently valuable: dropping 8 stale tasks is worth
  shipping today, and the resurface is worth designing once its input exists.

**The hook the follow-up will use, precisely** (so it is not re-derived from
scratch):
- **Trigger**: an inquiry-lane verdict of "needs a reply" on an **inbound**
  `normalized_messages` row whose thread is the conversation of a ticket with a
  `ticket_status_syncs` row where `last_action='closed'` and
  `drop_reason='ticket_delivered'`, with the message arriving **after**
  `ticket_status_syncs.acted_at`. The verdict carries the exact thread, so the
  join is on stored ids, not on a heuristic.
- **Path**: `task_append_log` (quoting the question and naming the
  `normalized_messages.id`) **then** `task_reopen` to `closed_from_status` — both
  through the executor, both as their own actor, in that order, so a failed reopen
  never leaves an unexplained open task.
- **Its own recorded action, which is the load-bearing part**: a new
  `ticket_status_syncs.last_action` value (`resurfaced_question`, a further
  migration and a further CHECK swap) **plus** a clause in `Decide` that refuses to
  re-close a ref in that state until the ticket's observed status actually changes
  or a human closes/dismisses it. Without both, `ticketstatus.Run` re-closes the
  resurfaced task on its next tick and the feature is invisible.
- **Dismissals**: it must respect `task_dismissals` exactly as D4 does — a
  dismissed task never resurfaces; one suppression log line, once.
- **Dedup**: one question resurfaces once. Key on the `normalized_messages.id`, in
  the shape of `task_events_outbound_observed_uniq` (migration 0013) — and if that
  partial-index shape is reused, the `ON CONFLICT` must restate the predicate or it
  raises at runtime.

**E10 — the honest gap in the meantime goes in the RUNBOOK, not in a future
reader's debugging session.** While a task is dropped as delivered, a client
question on that ticket is *recorded* (capture appends a `log` task_event to the
closed task — fact 9) and *not surfaced anywhere*. Salvador must read that
sentence when he arms the column, not discover it. The runbook carries the
sentence and the one-line psql that lists log events landing on
`ticket_delivered`-dropped tasks since their close, as the manual stand-in until
the follow-up ships.

**E11 — seed `TT-In QA` only. `TT-In Review` is NOT guessed into the set.**
It has two readings — "waiting on their reviewer" (delivered) and "review comments
are on me" (mine) — and the two demand opposite behaviour for its 2 tasks. Guessing
is how a task silently disappears while the ball is in his court, which is the one
failure this ticket must not create. It goes to `qa-delivered-drop_OPEN_QUESTIONS.md`
as a single question; the answer is one array element, no code change, so it does
not block implementation or delivery.

**E12 — `report` shows the armed set AND whether the row's current status is in
it.** An armed set that matches nothing looks exactly like an unarmed one, and E2's
whole reason for normalizing is that a mis-typed entry is the likely failure. Two
new columns on `opsctl ticket-status report` — the project's configured set, and
`delivered=yes|no` for the row's last-observed status name — turn a silent typo
into a visible one. Same argument as "an alarm whose fire-once marker is never
cleared goes permanently silent", applied to configuration.

## Acceptance criteria

### Data model

1. `migrations/0025_ticket_delivered_statuses.sql` is the ONLY migration this
   ticket adds, and it adds
   `ALTER TABLE projects ADD COLUMN ticket_delivered_statuses TEXT[] NOT NULL
   DEFAULT '{}'`. A guard test asserts exactly one `migrations/0025_*.sql`, with
   `0023_ticket_status_sync.sql` present as the control.
2. The same migration performs **no** `UPDATE` arming any project (E1 — arming is
   an operator act in the runbook, exactly as `classify_promote_after` and
   `ticket_assignee_gate` are), creates **no index** on the new column, contains
   **no status-name literal** (`TT-In QA` must not appear in it), and contains no
   `DROP COLUMN` / `DROP TABLE` / down section.
3. The same migration widens `ticket_status_syncs.drop_reason` to
   `('ticket_done','ticket_delivered','not_assigned')` by the 0009 precedent —
   `ALTER TABLE ... DROP CONSTRAINT ...; ALTER TABLE ... ADD CONSTRAINT ... CHECK (...)`
   — safe only because the migrate runner executes each file in one transaction,
   and the migration says so in a comment. The dropped constraint is named
   explicitly (`ticket_status_syncs_drop_reason_check`, Postgres's generated name
   for 0023's inline CHECK), and the ADD re-uses that exact name.
4. **The swap is self-verifying.** The migration ends with a `DO $$ … $$` block
   that raises unless `pg_constraint` holds **exactly one** CHECK on
   `ticket_status_syncs` mentioning `drop_reason`. Without it, a `DROP CONSTRAINT
   IF EXISTS` against a name Postgres did not generate is a silent no-op, and the
   first real `ticket_delivered` insert fails months later at runtime, every tick,
   on the same ref.
5. An integration test against the real migrated schema INSERTs a
   `ticket_status_syncs` row with `drop_reason='ticket_delivered'` and expects
   success, and one with `drop_reason='bogus'` and expects failure. Mutation that
   must turn it red: removing the ADD CONSTRAINT from 0025. (A structural scan of
   the SQL text is not enough — the constraint that matters is the one in the
   database.)
6. `internal/classify/structure_test.go`'s ledger accepts 24 and nothing above it;
   the test still fails for any unowned migration number.
7. `internal/ticketstatus/structure_test.go`'s existing 0023 guard is UNCHANGED
   (it asserts 0023's own shape, including its two-value `drop_reason` CHECK — an
   already-applied migration is never edited).
8. **`TestAdvisoryLockKey_IsThisPackagesAlone` keeps asserting `"0023"`** and gains
   a one-line comment saying why: the convention is the migration number of the
   ticket that CREATED the lock, not the newest migration to touch the package.
   "Aligning" it to 0025 would change a live lock key and read as a fresh
   collision to the repo-wide scan. No file in this diff — source or test — spells
   an advisory-lock literal.

### The fold and the membership test

9. `internal/ticketstatus/deliveredstatus.go` declares two pure functions and no
   others: `NormalizeStatusName(s string) string` and
   `IsDeliveredStatus(name string, configured []string) bool`. Zero I/O; the file
   imports only `strings`.
10. Unit table for `NormalizeStatusName`: `"TT-In QA"` → `"tt-in qa"`;
    `"  TT-In QA "` → same; `"TT-In QA"` (NBSP) → same; `"tt-in  qa"` → same;
    `""` → `""`; `"   "` → `""`.
11. Unit table for `IsDeliveredStatus`: member with differing case/spacing on
    either side → true; a name not in the set → false; **`"TT-QA"` against a set
    containing `"TT-In QA"` → false, and `"TT-In QA Blocked"` → false** (E2: exact,
    never substring or prefix); an empty configured set → false; a set containing
    only `""` or `"  "` → false for every input including `""` (E4); an empty
    observed name against a non-empty set → false (E5).
12. **Structural: the fold is never re-spelled in SQL.** A scan over `internal/`
    fails any file that mentions `ticket_delivered_statuses` in the same string
    literal as `= ANY`, `@>`, `<@`, `&&`, `unnest(`, `lower(`, `upper(` or
    `btrim(`. The column may only ever appear in a SELECT list (E3). The test also
    asserts the column IS selected somewhere, so the ban is not scanning a repo
    that never reads it.

### The predicate

13. `Observation` gains `DeliveredStatuses []string` — a VALUE, supplied by the
    driver from the `projects` column. `TestDecideGo_IsPure` stays green: no
    `context`, no `pgx`, no `os.Getenv`, no `net/http`, no jira client, no executor
    in `decide.go`.
14. `warranted` is computed in ONE place as
    `statusCategory != 'done' AND NOT deliveredStatus AND (gate off OR assignee == own)`,
    where `deliveredStatus = IsDeliveredStatus(StatusName, DeliveredStatuses)`.
15. `drop_reason` is chosen by ONE ordered list (E7): `ticket_done`, then
    `ticket_delivered`, then `not_assigned`. A unit test drives the two new
    precedence pairs explicitly — a `done`-category ticket whose name is in the
    delivered set records `ticket_done`; a delivered-status ticket assigned to
    someone else under an armed gate records `ticket_delivered`.
16. The decision table grows from 18 rows to **36**: the cross-product
    `{done, indeterminate, new} × {mine, other, unassigned} × {gate on, gate off} ×
    {name in set, name not in set}`, each row naming the expected `warranted` and
    `drop_reason`. With an EMPTY delivered set, all 18 original rows must produce
    byte-identical results to today — that half is the "inert by default" proof and
    is asserted as such, not left implicit.
17. **Every SWT-32 behaviour is preserved for the new drop cause, and each is a
    test**: not-warranted + restorable status → `close` recording
    `closed_from_status`; not-warranted + `claimed|in_progress|needs_feedback` →
    `refused_active` with exactly one log line; not-warranted + already `closed` +
    no state row → `none` (never claim a close it did not make); warranted +
    `closed` + `last_action='closed'` + no dismissal → `reopen` to
    `closed_from_status`; warranted + `closed` + a `task_dismissals` row →
    `suppressed_dismissed`, once.
18. **Flapping through the new fact is symmetric**: `TT-Work In Progress` →
    `TT-In QA` → `TT-Reopened` → `TT-In QA` over one state row is a unit test —
    close, reopen, close, with the restored status preserved each time.
19. **A convergent re-observation that changes only the reason writes no executor
    call.** A task already closed with `drop_reason='ticket_delivered'` whose
    ticket then moves to `TT-Verified` (category `done`) updates the state row's
    `drop_reason` to `ticket_done`, preserves `closed_from_status`, counts
    `converged`, and makes zero executor calls.
20. **E6**: `State` gains `StatusName`, `loadCandidates` selects
    `s.status_name`, and `Decide`'s `refused_active` sameness check compares it. A
    unit test drives a claimed task whose ticket moves between two configured
    delivered statuses of the same category and assignee and asserts a SECOND log
    line is emitted; the same test with an unchanged name asserts none.
21. **E5**: a status with a readable category and an empty name, under a non-empty
    configured set, is `warranted` (not `unreadable`) — one unit row, with the
    reason in a comment.

### The driver, the counters and the report

22. `loadCandidates` selects `p.ticket_delivered_statuses` and the driver copies it
    into `Observation.DeliveredStatuses`. No other query in the repo reads the
    column.
23. **The column-fed regression test lives in the INTEGRATION suite** (institutional
    landmine 6; the sibling is
    `TestTicketStatus_TheAssigneeGateComesFromTheProjectsColumn`). Both directions,
    against real rows:
    - a project with `ticket_delivered_statuses = ARRAY['ITS-QA-NAME']` closes the
      task whose ticket carries that status — **mutation that must turn this red:
      dropping `p.ticket_delivered_statuses` from the candidate SELECT and passing
      a literal `nil`/`[]string{}`**;
    - a project with `'{}'` leaves an identically-statused ticket's task on the
      board — **mutation that must turn this red: passing a literal non-empty set,
      i.e. arming it globally**.
    A unit test cannot catch either, by construction: it is the thing supplying the
    value.
24. A second integration case proves the fold end to end **through Postgres**: the
    configured entry is stored with a trailing space and different case
    (`'  its-qa-name '`) and the task still closes. This is what makes E2's
    normalization a property of the system rather than of a Go table test.
25. `Stats` gains `ClosedTicketDelivered`, printed as `closed_ticket_delivered` in
    BOTH counter lines — `cmd/opsctl/main.go:678` and `cmd/connectors/jira/main.go:141`
    (which prints JSON) — unconditionally, zeros included.
    `TestTicketStatus_CountersCoverTheWholeVocabulary` learns the new field, and
    keeps failing if a `Stats` field exists that no counter line prints.
26. `count()` routes `Action=="closed"` with `DropReason=="ticket_delivered"` to the
    new counter; the existing `else` branch must no longer swallow it (today
    anything that is not `not_assigned` counts as `ticket_done` — a switch on the
    reason, not an if/else, so a fourth value is a compile-time-visible gap rather
    than a silently miscounted one).
27. `opsctl ticket-status report` prints the project's configured set and a
    `delivered=yes|no` for each row's last-observed status name (E12), computed
    with the same `IsDeliveredStatus` the decision uses — never a second
    comparison.
28. `--dry-run` is unchanged in shape and needs no new field: its line already
    carries `action`, `warranted`, `drop_reason` and `status=%s/%s`. A dry run over
    an armed project prints `drop_reason=ticket_delivered` for the QA rows and
    performs no writes and no fetches.
29. **Idempotence survives**: a second pass over an unchanged armed world performs
    zero executor calls and adds no `audit_events` rows for actor
    `ticketstatus:jira`.

### Guards on the tension

30. `TestTicketStatus_HasNoNameListNoEmailNoDisplayName` gains the three real
    Treetop names — `"TT-In QA"`, `"TT-In Review"`, `"TT-Verified"` — to its banned
    `statusNames` list, alongside the existing generic six. The names being seeded
    are exactly the ones the guard refuses in `internal/ticketstatus` and in
    `internal/connector/jira`'s `facts.go` / `rawid.go` / `lookup.go`. Test files
    are out of scope of that scan (it lists non-test sources only), so fixtures may
    use them freely.
31. **The two stale comments are corrected in this diff**:
    `internal/connector/jira/facts.go`'s `StatusName` field comment and migration
    0023's `status_name` comment both say "DIAGNOSTIC ONLY; nothing branches on it".
    Replace with the accurate statement — nothing in CODE branches on it; a
    per-project configured set may. 0023 is already applied and must NOT be edited,
    so its correction is a comment in **0025** that names 0023's line and supersedes
    it. A test asserts 0025 mentions `status_name`.
32. A prose guard on the runbook (`TestRunbook_DocumentsTheReconciler`'s shape)
    requires `docs/runbooks/ticket-status-sync.md` to mention
    `ticket_delivered_statuses`, `ticket_delivered`, the exact arming `UPDATE`, that
    an empty array is today's behaviour, the "not my turn ≠ finished" distinction
    versus `statusCategory`, and the **deferred resurface gap** (E10) with its
    stand-in query.

## Data model changes

**`migrations/0025_ticket_delivered_statuses.sql`** — the only migration this
ticket adds.

```sql
-- 0025 ticket delivered statuses (qa-delivered-drop).
--
-- SWT-32's D2 stands: the "is this ticket FINISHED" discriminator is
-- fields.status.statusCategory.key, and no status-name list lives in the binary.
-- This column answers a DIFFERENT question — "is the ball in my court" — which
-- statusCategory provably cannot express: on Treetop, TT-In QA and TT-Work In
-- Progress are both `indeterminate`, yet one means delivered and one means
-- mid-build. That fact is one team's workflow layout, so it belongs in
-- CONFIGURATION, never in code: a typed column, per project, fail-closed default,
-- armed by a hand-run UPDATE recorded in the runbook — the ai_locality (0016),
-- ai_classify (0018), classify_promote_after (0021) and ticket_assignee_gate
-- (0023) precedent.
--
-- EMPTY ARRAY = today's behaviour EXACTLY. No backfill, no arming here; a
-- migration that armed collaboratory would drop eight tasks off a live board as
-- a deploy side effect.
ALTER TABLE projects
  ADD COLUMN ticket_delivered_statuses TEXT[] NOT NULL DEFAULT '{}';

-- Membership is decided in Go (ticketstatus.IsDeliveredStatus): lowercase +
-- unicode-whitespace collapse, then EXACT equality. It is deliberately NOT
-- spelled in SQL — Postgres's POSIX \s does not cover the unicode spaces Go's
-- strings.Fields does, so an NBSP alone would make the two disagree silently.
-- That is internal/textmatch's recorded rule, applied to a second kind of label.
-- NO INDEX: `projects` holds tens of rows and every reader reaches it by primary
-- key; an index nothing uses is a permanent claim some query needs it.

-- drop_reason gains a third value. Drop/add of the CHECK is safe only because
-- the migrate runner executes each file in ONE transaction (0009's recorded
-- precedent for deliveries_channel_check). Precedence, decided here and pinned
-- by the decision table rather than by production (no project today has both a
-- gate and a delivered set): ticket_done > ticket_delivered > not_assigned —
-- two statements about the ticket's own lifecycle, strongest first, then the
-- orthogonal assignment fact, which keeps SWT-32's existing done > not_assigned
-- ordering byte-identical.
ALTER TABLE ticket_status_syncs
  DROP CONSTRAINT ticket_status_syncs_drop_reason_check;
ALTER TABLE ticket_status_syncs
  ADD CONSTRAINT ticket_status_syncs_drop_reason_check
  CHECK (drop_reason IN ('ticket_done','ticket_delivered','not_assigned'));

-- SUPERSEDES a comment in 0023, which is applied and must not be edited: 0023
-- says status_name is "DIAGNOSTIC only (D2) — nothing branches on it". Nothing
-- in CODE branches on it, and that is still true. From this migration on, a
-- per-project CONFIGURED set of names may. Read the two together.

-- Self-check: a DROP CONSTRAINT against a name Postgres did not generate is a
-- SILENT no-op, and the first real ticket_delivered insert would then fail
-- months later at runtime, every tick, on the same ref.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n
    FROM pg_constraint
   WHERE conrelid = 'ticket_status_syncs'::regclass
     AND contype = 'c'
     AND pg_get_constraintdef(oid) LIKE '%drop_reason%';
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly 1 drop_reason CHECK on ticket_status_syncs, found %', n;
  END IF;
END $$;
```

- **No new table.** The drop mechanism, the state row, the reopen authority and
  the board lane are all SWT-32's, unchanged (E8).
- **No change to `ticket_status_syncs`' columns**, its unique index, or its FKs.
- `tasks`, `task_events`, `external_refs`, `capture_rules`, `capture_decisions`,
  `task_dismissals`, `deliveries`, `raw_source_items` and every normalized table
  are untouched — no columns, and this ticket writes no new rows to any of them
  beyond the closes/reopens the executor already performs.

New **data** (not schema), created by an operator, recorded in the runbook:
```sql
UPDATE projects SET ticket_delivered_statuses = ARRAY['TT-In QA'] WHERE slug = 'collaboratory';
```
and its exact inverse, `SET ticket_delivered_statuses = '{}'`.

## API / MCP tool changes

**None.** No tool is added, removed or re-signed.

The drops and restores continue to go through the executor path (invariant 3) at
exactly the hook SWT-32 built: `internal/ticketstatus/store.go`'s `act()` calls
`task_close`, `task_reopen` and `task_append_log` as actor `ticketstatus:jira`
with `executor.Call.TaskID` set, and
`TestTicketStatus_NeverWritesToolActionTablesDirectly` continues to ban `UPDATE
tasks` / `INSERT INTO tasks` / `INSERT INTO task_events` from the package. Nothing
new appears on the MCP surface; nothing is added to `policy.humanOnly`; the policy
matrix is unchanged.

**CLI, modified:** `opsctl ticket-status sync` gains one counter in its printed
line; `opsctl ticket-status report` gains two columns. No new subcommand, no new
flag.

**Provider surface:** none. This ticket makes **no HTTP call of any kind** — the
status name it reads was already stored by the existing poller.

**No dashboard route, no HTTP surface, no JSON API.**

## MQTT topics

None. Nothing published, nothing subscribed, no LWT.

Note for the reviewer, inherited verbatim from SWT-32 and still accurate: a close
and a reopen each write a `status_changed` task_event, which NOTIFYs the
orchestrator's drain. `Evaluate`'s `status_changed` case runs
`ruleUnblockDependents` for `to == "closed"` (and `"delivered"`) and returns nil
otherwise. No orchestrator change, no new rule.

## Concurrency

Unchanged. The work runs inside `ticketstatus.Run`, under the advisory lock that
package already takes on a dedicated connection with an explicit unlock. **No new
lock, and no advisory-lock literal is written anywhere in this diff** — the
repo-wide collision scan reads a restated key as a duplicate, and criterion 8
keeps the existing key's assertion pinned to its own ticket's number.

## Files likely to touch

- `migrations/0025_ticket_delivered_statuses.sql` (new)
- `internal/ticketstatus/deliveredstatus.go` (new) — `NormalizeStatusName`,
  `IsDeliveredStatus`; imports `strings` and nothing else
- `internal/ticketstatus/deliveredstatus_test.go` (new)
- `internal/ticketstatus/decide.go` — `Observation.DeliveredStatuses`,
  `State.StatusName`, the third `warranted` term, the ordered `drop_reason` list,
  the widened `refused_active` sameness check
- `internal/ticketstatus/decide_test.go` — the 36-row table, the precedence pairs,
  the flap, E5's row, E6's second-log case
- `internal/ticketstatus/store.go` — `candidate` gains the set; `loadCandidates`
  selects `p.ticket_delivered_statuses` and `s.status_name`; `Stats` gains
  `ClosedTicketDelivered`; `count()` becomes a switch on `DropReason`
- `internal/ticketstatus/store_integration_test.go` — the column-fed regression
  (both directions), the through-Postgres fold case, the CHECK-value case, the
  fixture projects naming the new column explicitly
- `internal/ticketstatus/structure_test.go` — the 0025 guard, the widened
  `statusNames` ban, the "never in SQL" scan, the lock-key comment
- `internal/connector/jira/facts.go` — comment only, on `Facts.StatusName`
- `cmd/opsctl/main.go` — the counter line (:678) and `runTicketStatusReport` (:699)
- `cmd/connectors/jira/main.go` — the JSON counter line (:141)
- `internal/classify/structure_test.go` — the migration ledger (:1054)
- `docs/runbooks/ticket-status-sync.md` — the delivered-set section, the arming
  UPDATE, the reversal, and E10's gap + stand-in query

## In scope / Out of scope

**In scope:** migration 0025 (the `projects` column and the `drop_reason` CHECK
swap); the pure fold and membership test; the third `warranted` clause and the
ordered `drop_reason`; the widened `refused_active` key; the new counter in both
mains and the two report columns; the two corrected comments; the runbook section;
the one-time arming of `collaboratory` with `TT-In QA` and the hand-run pass.

**Out of scope — named because they are the tempting bundles:**

- **The resurface-on-question half** (E9). It is `qa-question-resurface`, it
  depends on the inquiry-classify lane leaving shadow mode, and it needs its own
  `last_action` value plus a re-close suppression or it flaps every 15 minutes.
  Do not fold it in here "since we're touching `warranted` anyway" — a question is
  not a `warranted` fact.
- **Anything in the inquiry-classify lane itself** — the classifier, its prompt,
  its verdict storage, its promotion path. That SPEC is being written in parallel
  (`docs/tickets/inquiry-classify_*.md`); this ticket reads none of it and must not
  create a dependency on it.
- **Seeding `TT-In Review`** (E11), or any status name beyond `TT-In QA`, until the
  open question is answered. Adding one is one array element and no code.
- **Arming any other project's delivered set** — reengine, saka, foundry, town-ai,
  homelab. One `UPDATE` each, an operator act, after watching collaboratory.
- **Arming `collaboratory`'s `ticket_assignee_gate`** (already SWT-32's "Future
  work"). A different fact, a different column, still not asked for.
- **Overturning, softening or "unifying" D2.** The `statusCategory` discriminator
  stays in code and stays authoritative for `ticket_done`; no code path may fall
  back to a name for that question.
- **Making the fold configurable** (per-project case sensitivity, regex entries,
  glob entries). E2 chose exact-on-normalized deliberately; a regex column is an
  untyped predicate by another name.
- **A dashboard control** for arming the set, or surfacing `ticket_delivered` on
  `/tasks` / `/funnel`. The board hides `closed`; that IS the drop.
- **A GitHub equivalent** (`external_refs.system='github'`) — SWT-32's Future work,
  still.
- **Commenting on, transitioning or re-assigning the Jira ticket.** This pass
  reads. Outbound Jira words stay behind `deliveries` and the policy matrix.
- **Touching `capture`, `promote`, `classify`, `triage`, the orchestrator, the
  lookup half, `jira_lookup` accounts, or any connector's normalize path.**

## Invariants that apply

1. **Raw-first — satisfied by construction, and this ticket adds no write.** The
   status NAME comes from `fields.status.name` in the **already stored**
   `raw_source_items` row (`external_id = jira.IssueRawID(key)`), parsed by
   `jira.IssueFacts` from bytes the poller wrote. No new provider call exists, so
   there is no new raw write to place: D19's write-then-read-back property is
   inherited unchanged, and every close this ticket performs stays reproducible
   from `raw_source_items` alone with the network unplugged. Concretely: the new
   clause reads a value already present in `Observation`, built from
   `loadSnapshots`' stored row — never from an HTTP response.
2. **One funnel — no new table, and no second board.** The delivered set is a
   COLUMN on the existing `projects` row (E1); a `project_delivered_statuses` side
   table was rejected for exactly this reason. A drop is still
   `tasks.status='closed'` on the ONE tasks table, and the lane is still a board
   FILTER (`?status=closed`). `ticket_status_syncs` gains no column and stays state,
   not a task list.
3. **Everything through the executor — no new door.** No tool is added, and the
   only writes are the `task_close` / `task_reopen` / `task_append_log` calls
   SWT-32 already makes as `ticketstatus:jira` with `Call.TaskID` set, each through
   validate → policy → audit start → handler → audit complete. The structural ban
   on `UPDATE tasks` / `INSERT INTO tasks` / `INSERT INTO task_events` inside
   `internal/ticketstatus` must stay green; the package's only direct write remains
   its own `ticket_status_syncs` row. **Note the invariant-3 shape of the new
   column too:** it is read by the driver and passed as a value into a pure
   decision — it is not a predicate hidden in SQL that bypasses the audited path
   (criterion 12).
4. **Nothing external without a delivery row — nothing outbound exists here.**
   Zero HTTP calls, zero `deliveries` rows created, read or mutated, no send
   adapter imported. A dropped task keeps any `deliveries` row it has (D10
   unchanged): delivery state is never inferred from task state — the "failing a
   delivery R8 already processed" landmine. Worth stating for THIS ticket
   specifically: a QA ticket is precisely the kind that may have an open `drafted`
   or `approved` `jira_comment` row, and closing its task must not touch it.
5. **Own-message loop closure — untouched, and now load-bearing for the
   follow-up.** The pass reads no messages; capture's `direction='inbound'` filter
   and jira's `matchByBodyPrefix` / `confirmDelivery` path are unmodified and
   cannot be widened from here. The named consequence stands and becomes the
   material E9's follow-up will consume: a later notification about a dropped
   ticket still resolves through `external_refs` and appends a `log` task_event to
   the CLOSED task (`capture/rules_store.go` `appendRuleLog`, no status filter). A
   log on a closed task is a record, not a resurrection — and until
   `qa-question-resurface` ships, it is also the only trace of the client question
   E10 warns about.
6. **Stealth attribution — nothing client-visible is produced.** The close and
   reopen reasons are stored prose composed from the ticket key, its category, its
   name and the assignee comparison; the new reason quotes the configured status
   name back verbatim from the database. No model authors anything, and nothing
   this pass writes ever leaves switchboard.
7. **Orchestrator purity — no rule added, none modified, and the new predicate is
   itself pure.** The orchestrator sees a close and a reopen exactly as it does
   today. `Decide` stays a function of (observation, state) with zero I/O — the
   delivered set arrives as a `[]string` VALUE, the fold is a pure function in a
   file that imports only `strings`, and `TestDecideGo_IsPure` must stay green. The
   36-row decision table is the unit-testability invariant 7 exists for: the whole
   rule set is provable with no database, no network and no model. Every action
   still writes an audit row through the executor, and every no-op still writes the
   state row, so "why did nothing happen to this task" stays answerable from the
   database.

## Sibling patterns to copy

- **A typed, hand-armed `projects` column with a fail-closed default and no
  index**: `projects.ai_locality` (`migrations/0016_provider_locality.sql` — read
  its comments for the "default belongs on the recoverable side" argument),
  `projects.ai_classify` (0018), `projects.classify_promote_after` (0021) and
  `projects.ticket_assignee_gate` (0023). 0025's comment block should read like
  theirs.
- **A CHECK swap in a forward-only migration**:
  `migrations/0009_slack_web_connector.sql:6-8` (`deliveries_channel_check`) — the
  drop/add pair, safe because the runner wraps each file in one transaction.
- **The fold rule and why it never goes into SQL**: `internal/textmatch`
  (`NormalizedPrefix`) and its `callsites_test.go`; plus
  `internal/connector/upworkcrm/threadkey.go` + `keyspelling_test.go` for the
  structural "this string is never built or picked apart in SQL" guard shape that
  criterion 12 copies.
- **Column-fed predicate proof in the integration suite**:
  `internal/ticketstatus/store_integration_test.go`
  `TestTicketStatus_TheAssigneeGateComesFromTheProjectsColumn` — criterion 23 is
  the same test for a second column, including its two-direction structure and its
  "MUTATION THAT MUST TURN THIS RED" comments. The original of the class is
  `internal/drafts`' locality regression (institutional landmine 6).
- **Pure decision + all queries in the driver**:
  `internal/ticketstatus/decide.go` vs `store.go`, and
  `internal/promote/promote.go` `Decide` vs `internal/promote/store.go` `Run`.
- **The ordered-reason list rather than nested ifs**: `internal/policy`'s rule
  ordering inside `Matrix` — first matching rule wins, spelled once.
- **Multi-match / evidence-gap refusal**: unchanged from SWT-32, which took it
  from `internal/connector/slackweb/sink.go` `confirmDelivery` and
  `upworkcrm/sink.go` `confirmUpworkDelivery`.
- **`FOR UPDATE SKIP LOCKED`**: deliberately NOT used, for SWT-32's recorded
  reason — a single-instance pass over tens of rows serialized by an advisory lock
  is not a work queue.

## Verification protocol

Run in this order; do not commit before step 6 passes.

**0. Blocking pre-check — re-measure the live status distribution, and get the
EXACT status string.** Record the output in the delivery summary; do not freeze
any count as a test literal.

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"

psql -h 192.168.50.49 -U ops -d ops -c "
SELECT p.slug,
       ri.raw_json #>> '{fields,status,statusCategory,key}' AS category,
       ri.raw_json #>> '{fields,status,name}'               AS name,
       t.status AS task_status, count(*)
  FROM external_refs er
  JOIN tasks t    ON t.id = er.task_id
  JOIN projects p ON p.id = t.project_id
  LEFT JOIN raw_source_items ri ON ri.external_id = 'issue:' || er.external_key
 WHERE er.system = 'jira'
 GROUP BY 1,2,3,4 ORDER BY 1,2,3;"
```

(That `||` is an operator's ad-hoc query; SWT-32 criterion 34 forbids it in code.)

Confirm the QA row's `name` is byte-for-byte `TT-In QA` and note whether it
carries any leading/trailing whitespace:

```bash
psql -h 192.168.50.49 -U ops -d ops -c "
SELECT DISTINCT '['||(raw_json #>> '{fields,status,name}')||']' AS bracketed,
       length(raw_json #>> '{fields,status,name}') AS len
  FROM raw_source_items WHERE external_id LIKE 'issue:%'
 ORDER BY 1;"
```

If the bracketed form differs from `[TT-In QA]`, seed the string this query
returns — E2's fold makes case and whitespace irrelevant, but nothing else is
forgiven.

**1. `go test ./...`** — the fold and membership tables (10-11), the 36-row
decision table and the inert-by-default half (16), the precedence pairs (15), the
flap (18), the convergent reason change (19), E6's second log (20), E5's row (21),
the structural scans (12, 30), the migration guards (1-4, 7, 8) and the ledger (6).

**2. `make integration`** — `db-up` + `migrate` (applies 0025 to the compose db on
:5433) + `go test -tags integration ./...`. Covers the CHECK's real values (5), the
column-fed regression in both directions (23), the through-Postgres fold (24), the
counters (25-26) and idempotence (29). The suite is rerunnable: clean up in FK
order under a test-owned slug prefix, and stay in the existing mutual-cleanup pact
(`go test -p 1`).

**3. Confirm the local db really migrated:**
`psql "postgres://ops:ops@localhost:5433/ops?sslmode=disable" -tAc "SELECT max(version) FROM schema_migrations"`
→ `0025`.

**4. Apply the migration to pg-main FIRST** (merging a migration is not applying
it — the five-migrations-behind incident):

```bash
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0023
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0025
```

**5. Prove INERTNESS before arming anything.** This is the step that makes "no
other project changes behaviour" a measured fact rather than a claim.

```bash
cd ~/projects/personal/switchboard
alias opsctl='DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl'

psql -h 192.168.50.49 -U ops -d ops -c \
  "SELECT slug, ticket_delivered_statuses FROM projects ORDER BY slug;"   # all '{}'
opsctl ticket-status sync --dry-run
opsctl ticket-status sync
```

`closed_ticket_delivered` must be **0** and the other counters must match the
pre-change run. Nothing may move.

**6. The "usable alone" smoke — arm collaboratory and drop the QA tasks.**

```bash
psql -h 192.168.50.49 -U ops -d ops -c \
  "UPDATE projects SET ticket_delivered_statuses = ARRAY['TT-In QA'] WHERE slug = 'collaboratory';"

opsctl ticket-status sync --dry-run     # read the plan line by line FIRST
opsctl ticket-status sync
opsctl ticket-status report
```

Check all six:
- the dry-run plan and the live counters agree, and `closed_ticket_delivered`
  equals the `TT-In QA` open-task count from step 0 — no more, no fewer;
- every task the pass closed has a `TT-In QA` ticket:
  `SELECT task_id, status_category, status_name, drop_reason, last_action, closed_from_status
     FROM ticket_status_syncs WHERE drop_reason='ticket_delivered' ORDER BY task_id;`
- **no other project moved**: `closed_not_assigned`, `reopened`,
  `refused_active`, `unreadable`, `ambiguous`, `unpolled` are unchanged from step
  5, and the reengine / saka / foundry / town-ai task counts are identical;
- the `TT-In Review`, `TT-Work In Progress`, `TT-New` and `TT-Reopened` tasks are
  **still on the board** (E11 — `TT-In Review` in particular must NOT have dropped);
- those closed tasks are gone from `/tasks?project=collaboratory` and present under
  `?status=closed` (`kubectl -n ops port-forward svc/dashboard 8085:80`);
- the audit trail exists —
  `SELECT actor, tool, task_id, created_at FROM audit_events WHERE actor='ticketstatus:jira' ORDER BY id DESC LIMIT 20;`
  — and a second `opsctl ticket-status sync` immediately after adds no rows to it.

**7. The return path, on real data.** Pick one ticket the pass just closed and move
it out of QA in Treetop's Jira (to `TT-Reopened` or `TT-Work In Progress`). The
polled half refreshes on `updated >= "-Nm"`, so the connector must run first or the
stored snapshot is stale:

```bash
DATABASE_URL="$OPS_DATABASE_URL" OPS_TOKEN_KEY=... go run ./cmd/connectors/jira
opsctl ticket-status sync
```

The task must be back on `/tasks?project=collaboratory` **in the status it held
before**, with a `status_changed {from: closed, to: ...}` event on `/tasks/{id}`.
Move it back to `TT-In QA`, re-run both, and it drops again — the flap, on
production data.

**8. Reversibility, and the dismissal interaction.**

```bash
psql -h 192.168.50.49 -U ops -d ops -c \
  "UPDATE projects SET ticket_delivered_statuses = '{}' WHERE slug = 'collaboratory';"
opsctl ticket-status sync
```

Every task dropped in step 6 comes BACK (they become warranted, `last_action` is
`closed`, so the reopen path fires) — except any that Salvador dismissed in the
meantime, which stay closed with exactly one suppression log line (D4). Confirm
both. Then re-arm and re-run step 6 to leave the board in the intended state.

**9. Recurring path.** The `*/15` behaviour only changes when the connector image
is rebuilt and `connector-jira`'s tag re-pinned — the kube session owns
`~/projects/personal/kube/switchboard/`. Until then the pinned image runs code
without the new clause and the drops are hand-run only. That is expected, not a
failure. Hand the migration-applied fact and the new tag to that session together;
no manifest change beyond the tag is needed (no new env var).

## Open questions

**ONE**, in `docs/tickets/qa-delivered-drop_OPEN_QUESTIONS.md`: whether
`TT-In Review` belongs in the seeded set. It does **not** block implementation,
tests or delivery — the answer is one array element in a hand-run `UPDATE`. Ship
with `TT-In QA` alone; add the second name if and when he says so.

Everything else was resolved in-document: the D2 tension (see "The design
tension"), the column shape (E1), exact-vs-substring matching and the fold (E2/E3),
the empty-entry and empty-name edges (E4/E5), the dedup key (E6), precedence (E7),
clause-not-pass (E8), and the deferred resurface with its named hook (E9/E10).

## Future work (not this ticket)

- **`qa-question-resurface`** — E9's half, once the inquiry-classify lane leaves
  shadow mode. The hook is specified above; the new `last_action` value and its
  re-close suppression are the load-bearing parts, and it needs its own migration
  for the CHECK.
- **Seeding other projects' delivered sets** once collaboratory has run for a
  while — and, more interestingly, reading `drop_reason='ticket_delivered'` counts
  per capture rule to see whether a rule is mostly surfacing work that was already
  handed back.
- **`/funnel` counters** for the three drop reasons ("N tasks dropped this week,
  split by reason"), which SWT-32 already listed and which this ticket makes more
  useful by adding a third bar.
- **A dashboard editor for the delivered set**, if hand-`UPDATE`ing it ever becomes
  frequent. It would be a `humanOnly` executor tool, not a raw SQL door.
- **A "stale QA" brief** — a QA ticket that has sat in the delivered set for N days
  with no movement is a nudge candidate, which is a delivery question (invariant 4)
  and therefore a real ticket, not a widening of this one.
