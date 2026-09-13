> Jira: SWT-45

# jira-activity-revive — Jira activity on a ticket revives its closed task (or creates one), and the reconciler stops re-closing what activity surfaced

## Source

Ad-hoc, from Salvador, verbatim, 2026-09-12:

> "there are emails landing with a subject like this one Katie Evans mentioned you on API-4104 that
> should revive the task on that ticket if closed or create a new one"

Earlier:

> "api-4103 is closes but there is a comment there for me. It was not captured as task"

**Owner decisions (2026-09-12). These are settled, not re-argued here.**

1. **Any activity.** ANY Jira activity on a ticket revives its closed task, or creates a task if
   the ticket has none. Activity means mentions, comments, assignments and status updates,
   including the ticket-closed notification, not only mail addressed to him. The anti-bounce must
   be designed explicitly: a "ticket closed" email must not ping-pong against the reconciler.
2. **Hand-closed tasks.** A task Salvador closed by hand is revived by activity that arrives AFTER
   the close (SWT-36's ingested-after-close idea). Tasks in `delivered` status stay out.
3. **Gated projects (as refined later the same day; this replaces the first version).** On gated
   projects (reengine / LHH), ONLY activity addressed to him overrides the assignee check: an
   "X mentioned you on K" or "X assigned K to you" notification, or the connector-side equivalent
   if it can be identified deterministically. All other activity on a gated-project ticket follows
   the SWT-40 Part D gate: a task only if the ticket is assigned to him and warranted. That keeps
   his 09-11 rule: "the LHH tickets need a trip to jira to see if they are assigned to me. if they
   are then reengine if not ignore". On non-gated projects (collaboratory) decision 1 is
   unchanged. Prod fact: reengine has `ai_classify=false` and `ticket_assignee_gate=true`, and in
   the last 30 days rules 1 and 2 matched only LHH keys.

This is not a build-order step. It is the follow-up that `docs/runbooks/ticket-status-sync.md`
names as the gap `qa-question-resurface` (SWT-34 E9), widened by decision 1 from "a fresh
question" to "any Jira activity".

## Goal

A Treetop Jira notification email (and, on reengine, a Jira email addressed to him) keys to the
ticket it names. If that ticket's task is closed, the email brings it back. If the ticket has no
task, the email creates one. The ticket-status reconciler then refrains from re-closing a task that
activity surfaced until the ticket's status, status name or assignee changes, or a human closes or
dismisses the task.

**Usable alone means:** after migration 0030, the new image and one `opsctl capture-rules add`
for the Treetop notification rule:
- the next "X mentioned you on API-n" / "(API-n) …" email about a ticket whose task is closed
  reopens that task to the status it held (else `ready`);
- an email about a ticket with no task creates one;
- the jira tick's reconciler pass logs one line on such a task ("ticket is done, activity surfaced
  it, leaving it open") and then leaves it alone;
- a hand close afterwards sticks until newer activity arrives.

The anti-bounce also has a deterministic prod smoke that needs no inbound mail: reopen task 85
(API-4103, the ticket he reported) by hand, and watch the reconciler hold it open instead of
re-closing it within 15 minutes (Verification step 7). Every project without a reviving rule
behaves byte-identically to today.

## What the investigation established

### Prod facts (read-only, supplied with the ticket; do NOT freeze as test literals)

- Treetop Jira mail arrives on `salvador@handsonconnect.org` (source account 1009) from
  `jira@treetopllc.jira.com`, with display names such as `Katie Evans (JIRA)`. Subjects come in
  three shapes: `X mentioned you on K`, `X assigned K to you` and `(K) title`.
- Since 07-29: 32 mention emails over 20 tickets, 25 assign emails over 25 tickets, and 284 update
  emails over 158 tickets. That is roughly 7-8 emails a day.
- Gmail groups the mail per ticket in threads keyed
  `gmail:salvador@handsonconnect.org:<…@treetopllc.jira.com>`. **The ticket key appears only in
  the subject**, never in the thread key.
- A comment arrives twice: once via the Jira connector (account 1400, thread
  `jira:treetopllc.jira.com:API-4104`) and once as email.
- Rule 10 is `(WEB|API|OPS)-[0-9]+`, priority 90, collaboratory, `external_system=jira`, no key
  regex.
  - Every notification email is therefore `task_log`ged onto the prefix catch-all tasks 56/57/60
    (refs keyed `API`, `WEB`, `OPS`).
  - All three tasks are closed except 57 (`ready`, per capture-rule-ticket-keys). Task 56 carries
    256 logs.
  - Rules 3–5 match only `jira:` thread keys, so emails only ever reach rule 10.
- Per-ticket tasks exist for some tickets:
  - API-4103: ref 31 → task 85, closed by the reconciler (`ticket_delivered`/done).
  - API-4104: ref 32 → task 86. Katie's 09-11 comment reached task 86 through rule 4 as a
    `task_log` and left it closed.
- Tasks 92 (WEB-10355, now TT-Reopened), 99 and 77 were closed by hand on 09-12.
- Since capture went live, 19 inbound logs have landed on closed, undismissed tasks. 6 mentioned
  and 17 assigned tickets have no task at all.

### Code facts (verified in this worktree)

- **F1: why rule 10 keys by prefix.** Rule 10 has no `key_regex`, so `externalKey` reuses the
  pattern. `extractKey` returns the FIRST capture group, which is `(WEB|API|OPS)`
  (`internal/capture/rules.go` `extractKey`).
- **F2: where the key text comes from.** `keyText` for `body_regex` and `sender` rules is
  `subject + "\n" + body` (`rules.go` `keyText`/`matchText`). A key regex anchored at text start
  with `^[^\n]*?` therefore reads the SUBJECT LINE ONLY, because Go's `^` without `(?m)` is start
  of text. **No new criterion type is needed** to key on the subject: the existing `sender`
  criterion plus a subject-anchored `key_regex` does it, as data.
- **F3: capture reopens only for an open dismissal.** `taskForExternalRef` LEFT JOINs an OPEN
  `task_dismissals` row and nothing else (`rules_store.go:688-712`). A closed task with no open
  dismissal gets a silent `appendRuleLog`.
- **F4: the reconciler reopens only its own closes.** It reopens only when
  `last_action='closed'` and the ticket is warranted again (`internal/ticketstatus/decide.go`).
  It CLOSES any restorable (open) task whose ticket is not warranted, whoever reopened it. Two
  consequences:
  - A comment is not a status change, so it reopens nothing.
  - A human's plain `task_reopen` of a done ticket's task is re-closed by the next pass.

  The second is a pre-existing bounce of the same class this ticket must prevent.
- **F5: one writer closes tasks.** `closeTransition` is the only non-test writer of
  `tasks.status='closed'` (`internal/tools/close.go:105`). Every other `UPDATE tasks SET status`
  in `internal/` targets blocked, ready, claimed, in_progress, needs_feedback, done_locally,
  delivered or PR statuses. But integration fixtures across many suites INSERT closed tasks
  directly, so a CHECK tying `status='closed'` to a new column would break them en masse.
- **F6: no close instant, no general pre-close status.** `tasks` records neither when it was
  closed nor what status it was closed from. The only pre-close status stores are:
  - `task_dismissals.closed_from_status`, for dismissals only (0026);
  - `ticket_status_syncs.closed_from_status`, for reconciler closes only (0023).

  `tasks.updated_at` is `NOT NULL DEFAULT now()` (0001:132). `closeTransition` stamps it at the
  close and nothing ever lowers it, so on a closed task **`updated_at >= the last close
  instant`**.
- **F7: an argument hidden from the MCP schema is still reachable.** The MCP adapter passes
  arguments through. `injectWorkerID` rewrites raw JSON and nothing filters it against the schema
  (`internal/mcpserver/adapter.go:241`). A worker could therefore set any `create_task` argument,
  schema or not. **Surfacing must not be a `create_task` argument.**
- **F8: rules cannot be edited, and the same pattern cannot be re-added.** `capture_rules` is
  `UNIQUE (project_id, criteria_type, pattern)` (0015:58), and no tool changes a rule's pattern,
  key_regex, priority or flags. A "disable and re-add" of an existing rule collides on the unique
  key unless the pattern changes. Consequence: **the new rule's key_regex must be right the first
  time** (see the blocking pre-check), and existing rules 2 and 3–5 cannot gain a flag without
  a new tool.
- **F9: the connector sees whole projects, email follows his involvement.** The Jira poller's
  JQL is `project IN (…) AND updated >= …` (`internal/connector/jira/ingest.go:104`), so account
  1400 ingests every issue and comment in WEB/API/OPS, not only tickets he is involved in. Issue
  updates re-upsert the same `issue:{KEY}` raw row, so capture decides an issue ONCE. After
  creation, the connector's only new messages are comments. Status changes and assignments reach
  switchboard only as email. Jira's notification email exists only when Jira's notification scheme
  considers him involved (watcher, assignee, reporter, mentioned).
- **F10: Part D.** SWT-40 Part D (unmerged, `wt/inqpromote`, migration 0029) holds a jira-keyed
  match on a gated project (`decideMessage`: `system == "jira" && winner.gateOn` → `held`). Its
  pipelined `gate` stage then resolves the hold with `ticketstatus.Warranted`. Rule 2 is the
  Avviato Jira mail sender rule (`jira@avviato.atlassian.net`, Part D D-D6). Part D extracts
  `ticketstatus.Warranted` and `EnsureSnapshots` and adds `p.ticket_assignee_gate` to capture's
  `loadRules`.
- **F11: the capture wake-up is unaffected.** `pipeline.AnnounceCaptured` keys only on
  `RulesStats.Considered` (`internal/pipeline/contract.go:202`). New counters do not change when
  the pipeline wakes.
- **F12: SWT-36's ingest clock.** Reopen iff `normalized_messages.created_at >
  task_dismissals.created_at`, strictly, compared in SQL in the handler under the row lock. Both
  sides are Postgres `now()` and both survive re-normalization. This ticket reuses that clock
  against a close instead of a dismissal.

## Decisions made unilaterally (with rationale)

Numbered **J1…** so they never collide with SWT-32's D, SWT-34's E or SWT-40's D-D.

**J1: Activity is a per-rule property: two new `capture_rules` columns, `revive` and
`addressed`.**
- `revive`: this rule's matches are Jira activity (decision 1).
- `addressed`: this rule's matches are activity addressed to Salvador (decision 3). It
  implies `revive`.
- **What they do together:** `overrides = revive AND (NOT gate OR addressed)`.
  - On a gate-off project, `revive` alone overrides.
  - On a gated project only `addressed` does. A `revive`-only rule there still goes through
    Part D, so an operator cannot bypass the gate by forgetting which flag means what.
- **Why data and not code.** "Which mail is activity" and "which mail is addressed to me" depend
  on Jira's email templates, which is SWT-34's "a workflow-shaped fact belongs in
  configuration". Jira's subject wording lives in a rule's pattern, never in a Go literal.
- **Why not a new criterion type (`subject_regex`).** F2: `sender` plus a subject-anchored
  `key_regex` already keys on the subject.
  - The addressed reengine rule selects on the subject shape with `body_regex` anchored the same
    way (`\A[^\n]*…`).
  - A Slack message has no subject, so its text starts with `\n` and the anchor cannot match.
- **Validation (tool and CHECK both):**
  - `revive` requires `external_system` AND a non-empty `key_regex`. Rule 10 has no
    `key_regex`, so it can never be flagged. This makes the prefix-bucket landmine (F1)
    impossible to combine with reviving: tasks 56/57/60, with their hundreds of logs, can never
    be revived.
  - `addressed` requires `revive`.

**J2: The Treetop notification rule is data, added through the executor.**

```
opsctl capture-rules add --project collaboratory --type sender --pattern jira@treetopllc.jira.com \
  --external-system jira --key-regex '^[^\n]*?\b((?:WEB|API|OPS)-[0-9]+)\b' \
  --url-template 'https://treetopllc.jira.com/browse/{key}' --priority 92 --revive \
  --note "SWT-45: Treetop Jira notification mail keys to the ticket in its SUBJECT; revives/creates"
```

- **Priority 92** puts it above rule 10 (90), so Jira mail leaves the prefix buckets, and below
  rule 59 (95, Foundry GitHub mail, no overlap).
- **The sender qualifies the instance.** That answers capture-rule-ticket-keys' "which Jira"
  for this traffic without text-qualifying rule 10.
- **The prefix set is the connector's (WEB/API/OPS).** A key outside it has no snapshot, and the
  reconciler could never judge it. A subject with no key (a digest) derives no key and becomes
  attribution only, never a bucket log.
- **Blocking pre-check (Verification 0b).** It measures the distinct prefixes in account 1009's
  Jira subjects. Any other prefix stops the seeding for a decision, because F8 makes the
  key_regex hard to change later.

**J3: The connector side is NOT flagged; the notification email is the only Treetop trigger.**
Rules 3–5 stay exactly as they are. Reasons:
- **F9 (the connector reads whole projects).** A reviving connector rule would turn a comment
  on ANY WEB/API/OPS ticket into a task. Surfacing (J6) would also stop the reconciler closing
  the done ones.
- **The email already covers his tickets.** For every ticket he is involved in, the email carries
  everything the connector copy would, plus the status changes and assignments the connector never
  produces as messages.
- **Dedup (item e) becomes structural.** Only one of the two copies can revive, so no
  cross-channel matching is needed, and none must ever be attempted: it would be a post-hoc
  content matcher, a recorded landmine class.
- **The cost, accepted.** A comment Jira does not email him about (he is not a watcher) does not
  revive anything. Per Jira, he is not involved.

**J4: Reengine's addressed rule is data, seeded only if the measurement confirms the shapes.**

```
opsctl capture-rules add --project reengine --type body_regex \
  --pattern '\A[^\n]*(?:\bmentioned you on LHH-[0-9]+|\bassigned LHH-[0-9]+ to you)' \
  --external-system jira --key-regex '\A[^\n]*?\b(LHH-[0-9]+)\b' \
  --url-template 'https://avviato.atlassian.net/browse/{key}' --priority <max(rule1,rule2)+1> \
  --revive --addressed --note "SWT-45 decision 3: LHH mail addressed to him overrides the gate"
```

- The literal wording is confirmed against prod subjects first (Verification 0d).
- It must outrank rules 1 and 2 and stay below 95.
- **The connector-side equivalent is not needed today.** No gated project is connector-polled:
  reengine's Jira is lookup-only. Recorded under Future work.
- Rules 1 and 2 are untouched. Everything they match keeps following Part D, as decision 3
  requires.

**J5: `tasks` gains the close record and the surfacing record, written only by executor
handlers.**
- **`closed_at`, `closed_from_status`.** `closeTransition` writes both on → closed and NULLs
  both on → open. It is the ONE writer (F5).
- **`surfaced_at`, `surfaced_by_message_id`.** These record the last time something other than
  the reconciler put this task on the board: an activity revive, a creation by an overriding
  rule, or a human's plain reopen. The message id is NULL for a human.
- **No CHECK ties `closed_at` to status** (F5: fixtures insert closed tasks directly).
- **No backfill.** A pre-0030 or old-binary close has `closed_at` NULL, and the revive guard
  falls back to `updated_at` (F6). The fallback is spelled ONCE, in the handler's SQL.
  - What it reads: the task's last STAMPED write. On a closed task only `closeTransition` stamps
    it (`task_append_log` and `task_mark_surfaced` do not), so for any close made through the
    executor it IS the close instant (criterion 46 proves it through a capture pass).
  - When it mis-fires, both ways, and only when something outside the executor wrote the row:
    a hand-run UPDATE of `updated_at` after the close moves the guard LATER (a message ingested
    in between does not revive); a close written without stamping it (hand SQL
    `SET status='closed'`, a fixture INSERT with an old `updated_at`) leaves the guard BEFORE the
    real close (a message ingested in between revives although it predates the close).
- **No `closed_from_status` backfill either:** NULL restores `ready`, with the dependency
  re-derivation. That follows SWT-36 D5's precedent of never mining `task_events` jsonb.

**J6: The revive is a third form of `task_reopen`: `{task_id, message_id, revive: true, reason}`.
It always surfaces.** Handler `reviveGuarded`, in one transaction under the tasks row lock, in
this order:
- **(a)** The message exists and is INBOUND, else an ERROR (invariant 5 at the verb, SWT-36 D13).
- **(b)** The task is `closed`, else skip `not_closed`. That covers `delivered` (decision 2) and
  every duplicate copy that arrives after the first revive.
- **(c)** Read `closed_at`, `updated_at`, `closed_from_status` and the task's OPEN dismissal, if
  any. The handler finds the dismissal itself under the lock, so there is no stale-id skip.
- **(d)** Guard instant = `GREATEST(COALESCE(t.closed_at, t.updated_at), d.created_at)`. That is
  the latest human-or-spine judgement that put the task down. Require `m.created_at > guard`
  strictly, one Postgres clock (F12), else skip `message_predates_close`.
- **(e)** Target = `closed_from_status` if it is in `openStatuses`, else `ready`, with SWT-36's
  dependency re-derivation for ready/blocked. That logic is extracted into ONE helper that both
  guarded forms call.
- **(f)** `closeTransition` to the target.
- **(g)** Stamp the open dismissal if there is one (`reopened_at/_by/_by_message_id`,
  RowsAffected == 1).
- **(h)** Set `surfaced_at = now()` and `surfaced_by_message_id`.

Validation:
- `revive` requires `message_id > 0`.
- It forbids `dismissal_id` (the handler finds the dismissal itself) and `status` (the record
  decides).
- A `message_id` without `revive` and without `dismissal_id` is still refused, so SWT-36's
  both-or-neither guard stays exactly as pinned.

Rejected alternatives:
- A `surface` bit on the SWT-36 form. That would mean two ways to say one thing.
- Reviving without surfacing. The reconciler would re-close it on the next jira tick: one flip
  per message, which is the bounce itself.

**J7: `task_mark_surfaced {task_id, message_id, reason}` is a new spine tool for creations.**
- It is registered on the executor and deliberately absent from `internal/mcpserver/schemas.go`
  (the `task_reopen` / `task_set_source_thread` shape). It is not humanOnly (capture calls it)
  and has no policy rule, so it falls through to static-default.
- Handler:
  - lock the task;
  - refuse a non-inbound message with an ERROR;
  - skip a closed task;
  - no-op if `surfaced_by_message_id` already equals this message;
  - else write `surfaced_at = now()` and the message id.
- Why a separate tool: F7. As a `create_task` argument it would be settable by any MCP caller.
- The crash window (created, not yet surfaced) degrades to today's behaviour: the reconciler may
  close the new task in the same tick, and the next overriding email revives and surfaces it.

**J8: A human's plain `task_reopen` surfaces too.**
- The unguarded form, when `policy.HumanActor(actor)` holds (ONE definition, SWT-20), sets
  `surfaced_at` with a NULL message.
- It fixes F4's pre-existing bounce, where a hand reopen of a done ticket's task was re-closed
  within 15 minutes. It is the enabler for J15's hand revives.
- The reconciler's own reopen (`ticketstatus:jira`) and every other non-human actor never
  surface.

**J9: Capture's live action, for winner W and key K.** The `capture_decisions.action` stays
`task` / `task_log`, SWT-36 D7's precedent. The typed outcome lives in `tasks.surfaced_*` and
`task_dismissals.reopened_by_message_id`.

| situation | action |
|---|---|
| no ref | create + link + provenance; then `task_mark_surfaced` iff `overrides(W)` |
| ref, task not closed (open, delivered, active) | log only. **Activity on an open task does not surface** (J10 explains why) |
| ref, task closed, `overrides(W)` | log, THEN revive (J6). The revive handles an open dismissal itself |
| ref, task closed, open dismissal, NOT `overrides(W)` | log, then SWT-36's guarded reopen, **unchanged** |
| ref, task closed, no open dismissal, NOT `overrides(W)` | log only (today) |
| gated project, jira, NOT `W.addressed` (once Part D is merged) | `held` → Part D, **unchanged** |

- Log first, then act (SWT-36 D10). A crash between the two leaves today's state.
- `taskForExternalRef` additionally returns `t.status`.
- `loadRules` selects `r.revive`, `r.addressed` and `p.ticket_assignee_gate`. That last spelling
  is identical to Part D's line, so the merge is mechanical.
- Shadow calls nothing. The reason says "would revive".

**J10: Activity on an OPEN task only logs. That makes the ticket-closed email end one of two ways,
and neither loops.**
- **Why an open task's activity must not surface.** Every Jira close sends an email. If activity
  on an open task surfaced it, the close email would usually land before the jira tick and
  surface the task. The reconciler would then never close a Treetop task for a done ticket again.
- **The two outcomes, depending on arrival order:**
  - **The email arrives before the reconciler sees the close.** It logs, the reconciler closes,
    and the task stays closed.
  - **The email arrives after the close.** It is activity after the close, so it revives (decision
    1). The reconciler holds the task open (J11). It stays until he closes it by hand, which
    sticks, or until the ticket changes.
- **The cost of decision 1, stated so it is read before arming.** Jira Cloud batches
  notification mail. So closing a Treetop ticket will often leave its task back on the board,
  revived by the close email, until one click closes it for good.

**J11: The reconciler gains `last_action='resurfaced'` and one Decide clause.**
- **New inputs.**
  - `Observation.SurfacedAt` and `SurfacedByMessageID` are VALUES from `tasks.surfaced_*`.
  - `State.SurfacedSeen` comes from a new `ticket_status_syncs.surfaced_seen_at`: the
    surfacing instant this pass last observed while the task was open.
- **The clause:**
  - `newSurfacing = SurfacedAt set AND (state == nil OR SurfacedAt != state.SurfacedSeen)`
    (exact instant equality; a timestamptz round trip through pgx is exact).
  - Not warranted, restorable, and `newSurfacing` → `resurfaced`, `Act = true`: ONE log line
    that names the drop fact and the message, and says how the hold ends. The current facts are
    recorded as the baseline.
  - Not warranted, restorable, `last_action = 'resurfaced'`, and `(status_category, status_name,
    assignee)` equal to the recorded baseline → `resurfaced`, `Act = false` (converged).
  - Otherwise SWT-32's rules, unchanged. A facts change closes.
  - `Decision.RecordSeen` is true for (warranted and open) and for (not warranted and
    restorable), so a surfacing observed while the ticket was still warranted is consumed, and a
    later fact change closes normally. Active work records nothing (unchanged).
  - A closed task (hand close, dismissal) is never claimed. It is `none` or converged exactly as
    today.
- **Inert by default.** `SurfacedAt` zero leaves every existing decision byte-identical.
  Asserted, not assumed.
- **`drop_reason`** is recorded on `resurfaced` rows as the fact being held off. 0030's comment
  supersedes 0023's "NULL unless dropped".
- **Supersedes SWT-36 D9 for overriding rules only.** A revive by an overriding rule is held.
  Everything else (non-overriding rules, promote) stays "ordinary to the reconciler". The runbook
  paragraph stating D9's same-tick re-close is corrected.
- **Subsumes SWT-34 E9 (`qa-question-resurface`) for jira-keyed tasks.** The trigger is decision
  1's deterministic activity instead of an inquiry verdict. `resurfaced` plus the Decide clause
  are E9's "own recorded action plus a re-close suppression", and dismissals keep outranking.
  The name is `resurfaced`, not `resurfaced_question`.

**J12: The no-loop property, stated as the thing the tests prove.**
- **Revive side.** Every closed → open transition this ticket adds consumes an inbound message
  ingested after the close, plus a live claim that is spent once per message.
- **Reconciler side.** Every re-close of a surfaced task consumes a ticket fact change observed
  after the surfacing was recorded.
- **Neither side generates the other's input.** The reconciler sends nothing to Jira, and a
  revive changes no ticket fact.
- **So:** with no new external event, the system reaches a fixed point after at most one
  reconciler pass. Each flip costs a real Jira event, so no pass-driven ping-pong exists.
- **Duplicate copies of one event.** Connector plus email, or batched mail: the first revives,
  the rest find the task open and log (J6 b), so one event gives one revive.

**J13: Dedup residual, accepted.**
- The case: he closes the task in the minutes between the connector copy and the email copy of
  one comment, and the email copy is ingested after his close.
- The result: the email revives once more.
- Why it is accepted: it is bounded to one extra revive per event. The alternative is a
  cross-channel content matcher.

**J14: Delivered stays out** (decision 2). A `delivered` task is not closed: the capture row
logs, and the handler would answer `not_closed`. A task closed FROM `delivered` restores to
`delivered`, through the one SWT-36 D5 restore rule. Activity does not pull delivered work back
into the ready lane.

**J15: Backfill (item g). Recommendation: no code backfill. A dry-run listing plus existing
verbs.**
- **Why not a code backfill:**
  - Pre-0030 close instants are not recorded (F6). A revive backfill would have to mine
    `task_events` jsonb (refused by SWT-36 D5's precedent) or call the verb without its guard,
    which is a side door.
  - The 23 task-less tickets' messages have spent their live claims
    (`capture_decisions_live_uniq`) and `--all` is refused in live. A retro-create would need a
    new claim mode, another CHECK widening that collides with 0029's.
  - Roughly 42 items do not justify either.
- **Instead:**
  - The runbook carries two read-only listing queries (Verification 0f).
  - He revives the ones he wants with the plain human `task_reopen`, which now surfaces (J8) and
    therefore sticks.
  - A task-less ticket gets its task from its next notification email, automatically.
  - A blanket "revive them all" is one shell loop over the listing's ids.
- A bounded one-shot is recorded under Future work.

**J16: Slack and GitHub mentions count (owner, 2026-09-12: "yes, any mention").** A Slack or
GitHub message that names a ticket key is Jira activity: it revives the ticket's closed task or
creates one. Only NEW messages act; there is no backfill (`jira-activity-revive_OPEN_QUESTIONS.md`
Q1).
- **Delivered by the mention successor rule, which is DEFERRED to capture-rule-ticket-keys.**
  That rule replaces rule 10 with a whole-key `key_regex` and `--revive`. This ticket adds no
  such rule. It only makes one legal (J1) and jira-only (J18); capture needs no further code,
  because it treats every reviving jira-keyed rule alike.
- **The load check's adjustments, which that ticket must carry:**
  - a GitHub rule: GitHub mail naming a key is today outranked by rules 6/7, so the successor
    alone never sees it;
  - exclude the Slack Jira-app status DMs, 61% of mention traffic;
  - exclude CircleCI mail;
  - a Foundry guard for the OPS-21 false positive;
  - the measured load: about 46 new tasks in the first week, about 31 of which would never
    self-close.

**J17: The own-action guard, a backstop for his own Jira COMMENTS (not his edits).** Invariant
5's one new exposure (below), made concrete on prod. With Jira's "Notify me about my own changes"
on, Jira mails him "Anonymous (JIRA)" notifications about his own changes. That mail is
inbound, so an activity rule would revive (or create) a task for his own action.
- **What the evidence supports, and what it does not (review round 2, prod read-only).**
  - Prod holds 208 "Anonymous (JIRA)" Treetop emails.
  - 26 of them correlate with an outbound message of his (a comment the connector stored) on the
    same ticket's `jira:{site_host}:{KEY}` thread, with sent_at in [email − 10m, email + 2m].
  - The other 182 are generic "[JIRA] (KEY) <summary>" notices of field or status edits, with no
    comment. 146 of them have no outbound message on the thread even within [−60m, +10m].
  - The connector stores no changelog (0 of 2,431 issue raw rows carry one), so nothing
    identifies an edit's actor.
  - J2 matches all Treetop Jira mail, so **his own edits would revive or create.** The protection
    against them is the owner turning Jira's "Notify me about my own changes" off. Verification
    0c gates J2's seeding on that.
  - The guard below is a backstop for his own comments: for mail already queued, and for the
    setting coming back.
- **The guard.** For an activity match that would revive a closed task or create one, capture
  looks for an OUTBOUND message on the ticket's connector thread with sent_at in
  [sent − `OwnActionLead`, sent + `OwnActionLag`] (10m / 2m, named constants in
  `internal/capture/ownaction.go`, the one spelling of the window; the SQL binds its two values).
  Structural, in the spine, no model. The window itself uses no sender literal; the only
  literals are the named-actor exemption's template words below, and they only exempt.
  - Found, on a closed task: log only, reason `own_action … revive skipped`. Neither the revive
    nor SWT-36's dismissal reopen runs.
  - Found, no task: action `attributed`, reason `own_action … no task created`. First match has
    already won, so no other rule can create from the message.
- **The named-actor exemption (review round 2).** Jira's notification From is
  `"<Name> (JIRA)" <jira@site>`.
  - **Evidence (prod, read-only).** Email 158474, "[JIRA] Katie Evans mentioned you on
    WEB-10355", from `"Katie Evans (JIRA)" <jira@treetopllc.jira.com>`, was sent
    2026-09-10 20:27:23Z. His outbound comment on WEB-10355 was at 20:18:45Z, inside the window,
    so the window alone would suppress exactly the mention the owner wants revived.
  - Across all history the window fires on 26 "Anonymous (JIRA)" emails (all his own comments)
    and on 1 named-other email (this one). No "(JIRA)"-shaped mail has ever carried his own
    name as the actor. Other Atlassian Cloud sites use `<Name> <jira@site>` with no "(JIRA)":
    prod message 31058, `Salvador Spataro <jira@foundryunderwriting.atlassian.net>`
    (2026-07-23), names him that way, and Foundry has no poller.
  - 182 other Anonymous emails are not his comments, so the placeholder cannot identify HIS
    actions. A named actor other than him, though, positively identifies someone else's.
  - **The rule.** The From display name may have the shape `<Name> (JIRA)`: quoted or not, and
    only the last "(JIRA)" is the template's, so "Katie (QA) Evans" survives whole. If Name is
    neither of these, the verdict is `named_actor`:
    - Jira's anonymous placeholder: `jiraAnonymousActor`, "Anonymous", a named constant in
      `ownaction.go` commented as Jira's template wording;
    - the stored sender of the outbound message the window found (his Jira display name, as the
      connector stores it; case and whitespace ignored).

    `named_actor` proceeds immediately: no window skip, no freshness reads, no wait. The reason
    reads `own-action guard: actor named ("Katie Evans", not his)`.
  - Anonymous, or no "(JIRA)" shape (another source, `Jira <…>`, a bare address): the window
    check as before.
  - The name check only EXEMPTS. Nothing is ever suppressed because a sender says "Anonymous".
  - **Order:**
    1. no poller → not applicable;
    2. a named actor who is not him → proceed now;
    3. his outbound message in the window → `own_action`;
    4. freshness → clear, deferred, or BLIND.
  - **Known residuals.**
    - An Anonymous-actor email about SOMEONE ELSE's action inside the window around his own
      comment is still suppressed.
    - A "(JIRA)"-shaped email naming him while his comment is not yet stored would proceed.
      Never observed in that shape on prod (0 of 341 "(JIRA)" emails name him).
    - The exemption knows only Treetop's `"<Name> (JIRA)"` shape. A site that renders actors as
      `<Name> <jira@site>` (Atlassian Cloud, e.g. Foundry) falls to the window check, so if such
      a site is ever polled under a reviving rule, the WEB-10355 mis-suppression returns there.
      Extend the parse before seeding a revive rule for it.
- **Whose threads.** A key's pollers are the provider='jira' accounts whose `scopes` claim its
  prefix (`ticketstatus.KeyPrefix`, RouteLookup's rule). Their thread keys use the connector's
  spelling, `jira:` + `jira.SiteHost(domain_default)` + `:` + KEY. A key no poller covers
  (lookup-only reengine/LHH) is "not applicable": nothing stores his comments there, and the
  reason says so.
- **The race.** Capture can see the email before connector-jira has polled the comment. The
  thread is known synced when EVERY poller of the key has an `ok` `sync_runs` row that STARTED
  after sent + `OwnActionLag` + `OwnActionSyncMargin` (2m for search-index lag and clock skew),
  and holds no raw row of the key (`issue:K`, `comment:K:*`) awaiting normalization. Until then
  the message is DEFERRED: no `capture_decisions` row (the live claim is unspent), a log line,
  `RulesStats.Deferred`, and a retry on every pass. connector-jira's own capture runs after its
  ingest and normalize, so the next jira tick normally settles it.
- **Bounded.** The deferral lasts at most `OwnActionMaxWait` (30m) from the message's ingest
  (`normalized_messages.created_at`, the database clock). After that the pass decides as if clear,
  and the reason and the log say the guard ran BLIND. `RulesStats.Blind` counts those decisions.
  Every capture counter line prints `"deferred"` and `"blind"`, zeros included; the gate line
  prints them as constant 0.
- **Why BLIND fails open.**
  - With the owner's "Notify me about my own changes" turned off, his own-change emails stop.
  - So a blind decision acts on real activity.
  - Failing closed would drop genuine mentions during a Jira outage, or whenever connector-jira
    is behind.
  - BLIND is counted (`"blind"`) and logged, and the decision reason says it.
- **The live horizon floor.** A deferral writes no decision row, so a message is decided only
  while it is inside the live horizon.
  - The horizon is on sent_at, but the deferral is measured from ingest.
  - `RulesConfig.normalize` refuses a LIVE horizon below `MinLiveRulesHorizon` = **2h**, and the
    error states the floor.
  - Why 2h: sent→ingest lag plus a */15 tick, `OwnActionMaxWait` (30m) and one more */15 pass
    add up to about 45m. 2h leaves 75m of margin.
  - Shadow is not floored.
  - `CAPTURE_RULES_SINCE`'s silent fallback on garbage or non-positive values is unchanged. A
    positive value under 2h now errors every live pass; the kube handoff warns. Prod leaves it
    unset, so 720h.
  - `Limit` stays smoke-only: the pending query reads oldest first, so deferred rows can use it
    up for 30m (runbook note).
- **An existing freshness signal, reused.** Part D's snapshot `VerifiedAt` describes the ISSUE,
  and an unchanged refetch does not move `ingested_at`, so neither proves the COMMENTS were
  read. `sync_runs` is the record that the poller's JQL (`updated >= cursor − 1h`) ran after the
  comment. No new table, column or lock: deferral is the pending query's existing "no decision
  row for this mode" filter.
- **Cost, accepted.** Someone else's Anonymous activity on the same ticket inside that window
  around his own comment does not revive either: he has just acted on the ticket. A NAMED actor
  is exempt (above). The owner turning the Jira setting off is the primary fix, and the only
  one for his edits. The guard is the backstop for his comments: mail already queued, and the
  setting coming back.

**J18: `revive` requires `external_system='jira'`.** Part D's hold keys on `system == "jira"`,
so a reviving github/slack/gmail rule on a gated project would create and surface past the
assignee check. `capture_rule_add` refuses it, naming the field. 0030's CHECK asks only for some
system (unchanged: 0030 is not re-shaped for this), so capture also computes
`activity := system == "jira" && overrides(...)`, and a non-jira reviving rule stored any other
way is inert.

## Traces: the scenarios the design must survive

| # | scenario | outcome |
|---|---|---|
| S1 | Comment on API-4103; task 85 reconciler-closed, ticket done; email ingested after the close | revive → `ready` (`closed_from_status` NULL pre-0030); surfaced; next jira tick: `resurfaced`, one log; later ticks: converged |
| S2 | Ticket closed in Jira; close email ingested BEFORE the jira tick | log on the open task; reconciler closes it; stays closed |
| S3 | Same, email ingested AFTER the reconciler's close | revive (surfaced) → held open → stays until his hand close (sticks) or a facts change |
| S4 | Connector copy and email copy of one comment | only the email rule revives (J3); even if both could, the second finds the task open |
| S5 | First email about a done ticket with no task | create + `task_mark_surfaced`; the same-tick reconciler sees a new surfacing → `resurfaced`, not closed (reverses SWT-32 D8's same-tick close for overriding rules) |
| S6 | Hand-closed task 99; email ingested after the close | revive (guard = `closed_at`). Ingested before the close → `message_predates_close`, log only |
| S7 | `delivered` task | log only |
| S8 | Reengine: "X mentioned you on LHH-n", ticket unassigned or done | addressed → not held → create/revive + surface → reconciler `resurfaced` (`not_assigned`/`ticket_done` held off) |
| S9 | Reengine: status-change email (rule 2, not addressed) | `held` → Part D gate → warranted ? task/task_log : attributed. **Unchanged** |
| S10 | Surfaced task; ticket moves TT-Closed → TT-Verified | facts change → reconciler closes; a later email ingested after that close revives again. One flip per Jira event |
| S11 | Surfaced task dismissed | closed + open dismissal; reconciler `none`; the next overriding email revives and stamps the dismissal (J6 g) |
| S12 | He reopens task 85 by hand (plain `task_reopen`) | surfaced (J8); reconciler holds it; a hand close later sticks |
| S13 | "Anonymous (JIRA)" email about his own comment; connector already polled | outbound message in the window → `own_action`: logged, no revive; no task created if none exists (J17) |
| S14 | Same email, captured before connector-jira polls the comment | deferred (no decision) until the jira tick; then S13. If the thread never syncs: decided after 30m, reason "BLIND", counted in `"blind"` |
| S15 | "Katie Evans (JIRA)" mention 8.6m after his own comment on the ticket (WEB-10355) | named actor, not him → revive/create now, no wait, reason `actor named` (J17) |
| S16 | "Anonymous (JIRA)" notice of his own field/status EDIT (no comment) | NOT covered by the guard: revives/creates. Prevented only by the Jira setting; 0c gates J2 on it (J17) |

## Coordination with neighbouring tickets

**capture-rule-ticket-keys (untracked draft, another session):**
- **Q1 (fix path).** Answered for the Jira-originated subset: data only, a new rule through
  `capture_rule_add` (J2). F8 adds a fact its Q1(a) missed. The `UNIQUE (project_id,
  criteria_type, pattern)` constraint means a "disable and replace" must change the pattern
  text, and no existing rule can gain the new flags.
- **Q2 (what a mention does).** Superseded by decision 1 **for Jira-originated traffic**: Jira
  notification mail creates and revives, never append-only. For Slack and GitHub mentions (rule
  10's residue after J2 claims the Jira mail) the owner answered **yes** (J16): its replacement
  for rule 10 carries `--revive` (option ii), with J16's load-check adjustments.
  
  Its "replacement ranks BELOW rules 3–5" decision is untouched: this ticket changes no rule
  priority except by adding J2 at 92.
- **Q3 (the bucket tasks).** Not answered. What this ticket does guarantee: after J2, Jira mail
  stops landing on 56/57/60, and J1's CHECK makes it impossible for them to revive. Task 57 and
  the three prefix refs remain that ticket's call.

**SWT-40 Part D (unmerged, migration 0029):**
- **Whichever merges second** adds `&& !winner.addressed` to the `held` condition in
  `decideMessage`, with a column-fed test (an addressed rule on a gated project is not held; a
  non-addressed one still is).
- Part D's gate path is otherwise UNCHANGED. Gate-path creations and task_logs never surface,
  because a warranted-only task needs no hold, and gate-path task_logs do not revive (decision
  3: non-addressed activity follows the gate).
- Both tickets add `p.ticket_assignee_gate` to `loadRules`. Keep one line.
- Part D reshapes `ticketstatus` (`Warranted`, `EnsureSnapshots`). If it merges first, J11's
  clause is written on top of `Decide`'s call to `Warranted`, and `Warranted` itself is
  untouched. The surfacing hold is a Decide-level fact, never a `Warranted` input, because the
  gate must keep asking only "is this ticket his and open".
- Part D's D-D6 path for assignment mail ("rule 2 holds and resolves `task`") changes for mail
  the J4 rule claims: it now creates at capture time without a lookup. The runbook says so.

**Migration numbering.** 0030 assumes 0028 (SWT-43) and 0029 (SWT-40 Part D) land first.
**Whichever of the three merges later than a sibling it collides with renumbers to the next free
number.** It also updates the migration ledger (`internal/classify/structure_test.go:1169` and
the ledger check in `internal/ticketstatus/delivered_structure_test.go`) and every guard test
that names the file. A number already on `main` is never reused, and an applied migration is
never edited.

## Acceptance criteria

### Data model (migration)

1. `migrations/0030_jira_activity_revive.sql` is this ticket's only migration. A guard test
   asserts exactly one `0030_*.sql`, and the ledger accepts 30 and still fails any unowned
   number.
2. It adds `capture_rules.revive` and `capture_rules.addressed` (`BOOLEAN NOT NULL DEFAULT false`)
   with the CHECKs `capture_rules_revive_needs_key` (`NOT revive OR (external_system IS NOT NULL
   AND key_regex IS NOT NULL)`) and `capture_rules_addressed_implies_revive`. An integration test
   INSERTs each violation and expects failure. Mutation: drop either CHECK → red.
3. It adds `tasks.closed_at`, `tasks.closed_from_status`, `tasks.surfaced_at` and
   `tasks.surfaced_by_message_id` (FK `normalized_messages(id) ON DELETE SET NULL`). It has no
   CHECK tying them to `status`, no index and no backfill `UPDATE tasks`. The comment names F5
   (fixtures) and F6 (the `updated_at` fallback).
4. It adds `ticket_status_syncs.surfaced_seen_at TIMESTAMPTZ`, and swaps
   `ticket_status_syncs_last_action_check` to add `'resurfaced'` by the 0009/0025 drop/add
   precedent. It ends with a `DO $$` self-check raising unless exactly one CHECK on the table
   mentions `last_action`.
5. Integration: `last_action='resurfaced'` inserts, `'bogus'` fails. Mutation: remove the ADD →
   red.
6. The migration arms nothing (no `UPDATE capture_rules`, no `INSERT`), carries no status-name
   literal and no `DROP COLUMN`/`DROP TABLE`, and supersedes 0023's `drop_reason` comment in a
   comment of its own. `internal/ticketstatus/structure_test.go`'s 0023 vocabulary pin (:563) is
   UNCHANGED, and a new pin asserts 0030's six-value set.
7. No advisory-lock literal appears anywhere in the diff. No lock is added.

### Tools: close record, revive, surfacing

8. `closeTransition` writes `closed_at = now()` and `closed_from_status = <from>` on → closed,
   and NULLs both on → open. It is still the one status writer, and an already-closed task is a
   no-op that keeps them. Integration: close, assert, reopen, assert. Mutation: drop `closed_at`
   from the UPDATE → criterion 11's guard test goes red.
9. `validateReopen` table test:
   - `revive` without `message_id` → error;
   - `revive` with `dismissal_id` → error;
   - `revive` with `status` → error;
   - `message_id` alone → error (SWT-36's `TestValidateReopen_DismissalGuardIsBothOrNeither`
     passes unmodified);
   - the three accepted shapes → ok.
10. `reviveGuarded` follows J6's order, one integration test per branch:
    - an outbound message → ERROR and no write;
    - a task not closed (incl. `delivered`) → `not_closed`;
    - the message ingested at or before the guard → `message_predates_close` (strict);
    - an ingested-after message → reopen to `closed_from_status`, else `ready`, with dependency
      re-derivation;
    - `surfaced_at` and `surfaced_by_message_id` are set in the same transaction.
11. **The guard is ingest time against the close.**
    - A message with `sent_at` after the close but `created_at` before it → skip.
    - `sent_at` before the close but `created_at` after it → revive.
    - With `closed_at` NULL the guard is `updated_at` (a fixture closed task with no `closed_at`
      revives only for a message ingested after its `updated_at`).
    - Mutation: replace the COALESCE with `closed_at` alone → the NULL case goes red.
12. **An open dismissal is handled inside the revive.** The guard is the later of the close and
    the dismissal. A message between the two → skip. A message after both → reopen and stamp the
    dismissal with `reopened_by_message_id`.
13. **One message revives a task at most once.** A second call with the same message →
    `not_closed`.
14. The restore-target logic (open-status membership + `depUnsatisfiedPredicate` re-derivation)
    lives in ONE helper called by both guarded forms. A structural test fails a second spelling.
    The SWT-36 integration suite (`dismissal_reopen_*`) passes unmodified.
15. `task_mark_surfaced`:
    - validates `task_id`, `message_id` and `reason`;
    - a non-inbound message → ERROR;
    - a closed task → skip;
    - the same message twice → the second call is a no-op (`surfaced_at` unchanged);
    - it is registered, absent from BOTH MCP profiles' schemas (structural, the
      `TestTaskReopen_StaysOffTheMCPSchemas` shape), and absent from `policy.humanOnly`;
    - its policy decision for `capture:google` is allow/static-default (matrix test).
16. A plain `task_reopen` surfaces (message NULL) for exactly the human actor shapes
    `dashboard:x`, `opsctl:x`, `manual:salvo` and `mcp:manual:salvo`. It does NOT surface for
    `ticketstatus:jira`, `capture:google`, `capture:gate`, `promote:classify`, `drafts:gpt` or
    `mcp:acme`, all enumerated per the IK actor-prefix rule. It keys on `policy.HumanActor`, never
    a re-spelled prefix.
17. `capture_rule_add` accepts `revive` and `addressed`, validates J1's two rules before the
    INSERT, and names the field in the error. `opsctl capture-rules add` gains `--revive` and
    `--addressed`. `capture-rules list` prints `revive` / `addressed` on the key line. A prose
    guard pins the flags in the runbook.

### Capture

18. `storedRule` gains `revive`, `addressed` and `gateOn`, and `loadRules` selects `r.revive`,
    `r.addressed` and `p.ticket_assignee_gate`. **Column-fed integration test, both directions:**
    - a rule seeded `revive=true` through the column revives a closed task (mutation: literal
      `false` in the SELECT → red);
    - a `revive=false` rule only logs (mutation: literal `true` → red).
19. `taskForExternalRef` returns the task status (column-fed; mutation: a literal `'ready'` →
    the closed-task test goes red).
20. `overrides(revive, addressed, gateOn)` is a pure function with a full truth table (8 rows).
    A `revive`-only rule on a gated project does NOT override.
21. Live, ref + closed + overriding rule → `task_append_log` THEN `task_reopen` (revive form).
    If the reopen fails, the log line is still present. The decision reason says "revive
    requested", and `RulesStats.Revived` counts reopened:true answers.
22. Live, no ref + overriding rule → create, link, provenance, THEN `task_mark_surfaced`, in that
    order, all as `capture:{connector}`. `RulesStats.SurfacedCreated` counts it. A
    non-overriding rule creates exactly as today, with no surfacing.
23. Live, a task not closed → log only, no reopen call and no surfacing (J10), for `ready`,
    `delivered` and `in_progress` fixtures.
24. A non-overriding rule on a closed task with no open dismissal → log only
    (`TestCaptureReopen_Integration_APlainClosedTaskOnlyLogs` unchanged). With an open dismissal
    → SWT-36's call, unchanged (every `rules_reopen_integration_test.go` case passes unmodified).
25. Shadow makes no executor calls. The reason text says "would revive".
26. **S4 through real rows.** A `jira:` thread message (rule-4-shaped, non-reviving) and a
    `gmail:` Jira-mail message (J2-shaped) for one key on a closed task, evaluated in both orders.
    Result: exactly one `status_changed` closed → open, two `log` events, and one `task_reopen`
    audit row with reopened:true.
27. Invariant 5: an outbound Jira-mail-shaped message never revives (pending filter), and
    `ObserveOutbound` never calls a reopen (existing test unchanged).
28. Every capture counter line prints `"revived"` and `"surfaced_created"`, zeros included:
    `cmd/connectors/{jira,slackweb,upworkcrm}/main.go`, `cmd/connectors/google/main.go` and
    `watch.go`, and `cmd/opsctl/main.go`'s `capture-rules run`. A structural scan fails a main
    that prints the counters without them.

### Reconciler

29. `Observation` gains `SurfacedAt time.Time` and `SurfacedByMessageID int64`; `State` gains
    `SurfacedSeen time.Time`; `Decision` gains `RecordSeen bool`. `TestDecideGo_IsPure` stays
    green and additionally bans `time.Now` in `decide.go`.
30. Decision-table rows (unit):
    - new surfacing + not warranted + restorable → `resurfaced`, Act;
    - seen == surfaced + `last_action='resurfaced'` + same facts → `resurfaced`, !Act;
    - status_category, status_name or assignee changed → `closed`, one row each;
    - warranted + open → `none`, RecordSeen;
    - a surfacing consumed while warranted, then not warranted → `closed`;
    - closed task + `resurfaced` state → `none`, never a claimed close;
    - active work → unchanged, !RecordSeen;
    - dismissed paths unchanged.
31. **Inert by default:** with `SurfacedAt` zero, every pre-existing `decide_test.go` case yields
    a byte-identical Decision. This is asserted as a loop over the existing table, not left
    implicit.
32. **The no-loop property (J12), unit.**
    - A driver-state simulator applies Decide's writes (state row, RecordSeen) for 50 passes over
      a fixed ticket with no new message → exactly ONE Act across all passes.
    - With one fact change injected at pass 20 → exactly two.
    - With a revive injected after that close → exactly three.
33. `loadCandidates` selects `t.surfaced_at`, `t.surfaced_by_message_id` and
    `s.surfaced_seen_at`. **Column-fed integration test:**
    - a done ticket's task with `surfaced_at` set stays open after `Run` and carries one log
      line (mutation: select NULL for `t.surfaced_at` → red);
    - the same fixture with `surfaced_at` NULL closes;
    - a second `Run` makes zero executor calls.
34. `upsertState` writes `surfaced_seen_at` only when RecordSeen, else preserves it
    (`COALESCE(EXCLUDED…, existing)`). `drop_reason` is stored on `resurfaced` rows. A timestamptz
    round trip is proven exact (the integration test compares `Equal` after a re-read).
35. `Stats.Resurfaced` is printed as `resurfaced` in `cmd/connectors/jira/main.go`'s JSON line
    and in `opsctl ticket-status sync`. `TestTicketStatus_CountersCoverTheWholeVocabulary` learns
    it. `count()`'s switch routes Act `resurfaced` to it and converged ones to `Converged`.
36. `decisionReason` for `resurfaced` names the ticket, the drop fact, the surfacing (the
    message id, or "reopened by hand") and how the hold ends. `--dry-run` prints
    `action=resurfaced` with no new field.

### End to end and docs

37. **S2 and S3 through real rows** (integration): a capture pass plus `ticketstatus.Run`, in
    both orders, over a ticket whose stored snapshot moves to done. Final states as in the trace
    table, then 5 further `Run`s make zero executor calls.
38. **S5 through real rows:** creation by an overriding rule on a done ticket survives the
    same-tick `Run`.
39. **S12 through real rows:** a plain human `task_reopen` of a reconciler-closed done-ticket
    task survives the next `Run`. A hand `task_close` afterwards survives the one after.
40. Runbooks, each with a prose guard in the existing `TestRunbook_*` shape:
    - `docs/runbooks/capture-rules.md` gains "Activity rules (SWT-45)": the two flags, J1's
      overrides table, the J2/J4 commands, the F8 warning and J10's cost.
    - `docs/runbooks/ticket-status-sync.md` replaces "The gap, until `qa-question-resurface`
      ships" and its stand-in query with "Surfaced by activity": `resurfaced`, how a hold ends,
      J8, J10.
    - The same runbook's SWT-36 D9 paragraph ("this pass closes it in the same jira tick") gains
      the overriding-rule exception.
41. `.claude/INSTITUTIONAL_KNOWLEDGE.md` gains an SWT-45 entry covering F7 (schema absence is not
    a boundary for args), F8 (rules cannot be re-added with the same pattern), the J10 cost and
    the `closed_at`/`updated_at` fallback.

### Review fixes (J17, J18)

42. **The own-action window, unit.** `ownActionWindow` is inclusive at both edges: exactly
    `OwnActionLead` before and `OwnActionLag` after are in, one nanosecond beyond either is out.
    `ownActionSyncedPast` is strictly after the far edge. `decideOwnAction`'s table:
    no poller → not applicable; found → own_action even on a stale thread; fresh → clear;
    stale inside the bound → deferred; stale at or past the bound → blind.
43. **The own-action guard, integration, through a real capture pass** (column-fed: pollers from
    `source_accounts.scopes`/`domain_default`, freshness from `sync_runs` and `raw_source_items`):
    - an outbound message on the ticket's thread inside the window → no reopen, no `task_reopen`
      call, one log, reason `own_action` + `revive skipped`;
    - an outbound message 11 minutes before the email plus an INBOUND one inside the window →
      revived. Mutations: drop the direction predicate or the sent_at window → red;
    - a poller run that started inside the window only → deferred (no decision row, twice); after
      `OwnActionMaxWait` from ingest → revived, reason says BLIND. Mutation: drop
      `started_at > $2` → red;
    - a fresh run but a `comment:K:*` raw row unnormalized → deferred; normalized → revived.
      Mutation: drop that clause → red.
44. **The creation half.** His own comment on a ticket with no task → `attributed`, `no task
    created`; an untouched ticket beside it is created and surfaced.
45. **J18.** `capture_rule_add` refuses `revive` with `external_system` github or slack, naming
    `jira`; github without `revive` stays legal. `opsctl capture-rules add --revive` says
    "needs --external-system jira". Capture's `activity` requires `system == "jira"`, proven
    through a real pass: a github rule with revive+addressed stored by direct SQL on a GATED
    project neither revives its closed github-linked task nor surfaces a new one. Mutation: drop
    `system == "jira" &&` → red.
46. **The `updated_at` fallback through capture.** A task with `closed_at` NULL and `updated_at`
    T: a message ingested before T is logged and not revived, and `updated_at` is unchanged by
    that log; a message ingested after T revives. Mutation: `COALESCE(closed_at, updated_at)` →
    `closed_at` → red.

### Review fixes, round 2 (J17)

47. **The named-actor From parse, unit.** `jiraNotificationActor` handles:
    - quoted and unquoted `<Name> (JIRA) <addr>`, and the bare display name;
    - the placeholder, which it returns and `namedJiraActor` excludes;
    - no shape: bare "Jira", a bare address, `"(JIRA)"` alone;
    - names containing parentheses (only the last "(JIRA)" is stripped), and escaped quotes.

    `decideOwnAction`: a named other with his comment in the window → `named_actor`, even on a
    stale thread past the bound; named as him with his comment in the window → `own_action`.
48. **The named-actor exemption, integration** (a real pass):
    - (a) WEB-10355's shape: his comment 8m38s before a `"Katie Evans (JIRA)"` email, and no poller
      run at all. In ONE pass the closed task revives, and a task-less ticket is created and
      surfaced. `deferred=0`, `blind=0`, reason `actor named`, no `own_action`.
    - (b) An `"Anonymous (JIRA)"` email inside the window is still skipped (`own_action`). So is a
      named actor equal to his stored name.
    - Criteria 43/44's window tests send Anonymous email, so the exemption cannot mask their
      predicates.
    - Mutation: drop the exemption → (a) red.
49. **The live-horizon floor.** `normalize` refuses a live horizon under 2h, and the error names
    `2h0m0s`. Exactly 2h passes, as do unset (720h) and a 10m SHADOW horizon. Through
    `RulesHorizon`, `CAPTURE_RULES_SINCE=30m` errors a live pass, while "720" and "-5h" still fall
    back to 720h.
50. **Visibility.** `RulesStats.Blind` counts blind decisions: the stale-thread test sees
    `blind=1`, and 0 while deferred. Every capture counter printer prints `"deferred"` and
    `"blind"`, zeros included (the structural scan of criterion 28). That covers
    `opsctl capture-rules run` and the gate line (constant 0).
51. **Docs.** `docs/runbooks/capture-rules.md`:
    - the guard is comments-only;
    - the named-actor exemption;
    - `--limit` reads the oldest N first, so deferred rows can use it up;
    - the floor.

    `HANDOFF-kube-jira-activity-revive.md` warns that a `CAPTURE_RULES_SINCE` under 2h errors
    every live pass, and states the new 0c gate. The IK entry carries the edits landmine.

## Data model changes

`migrations/0030_jira_activity_revive.sql` (number subject to the renumbering rule):

```sql
-- 0030 jira-activity-revive (SWT-45, docs/tickets/jira-activity-revive_SPEC.md).
--
-- Deploy order: apply BEFORE any image built with this file runs. New code selects
-- capture_rules.revive/.addressed on every capture pass and writes tasks.closed_at on
-- every close, so a new image on a db without 0030 fails both. Old images are
-- unaffected by 0030 (docs/runbooks/HANDOFF-kube-jira-activity-revive.md).
--
-- (1) Activity is a RULE property (J1). revive: this rule's matches are Jira activity
-- (owner decision 1). addressed: they are addressed to Salvador (decision 3) and so
-- override a gated project's assignee check. overrides = revive AND (NOT gate OR
-- addressed), decided in Go (capture), never here. revive needs an explicit key_regex:
-- a key derived from a pattern's first group is how rule 10 keys by PREFIX, and a
-- reviving prefix rule would resurrect a catch-all task on every mention. The CHECK
-- asks only for SOME external_system; capture_rule_add refuses any but 'jira' (Part
-- D's hold keys on jira, so another system would bypass the gate), and capture treats
-- a non-jira reviving rule as inert.
-- Rules are armed by capture_rule_add (the executor), never by a migration.
ALTER TABLE capture_rules
  ADD COLUMN revive    BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN addressed BOOLEAN NOT NULL DEFAULT false,
  ADD CONSTRAINT capture_rules_revive_needs_key
    CHECK (NOT revive OR (external_system IS NOT NULL AND key_regex IS NOT NULL)),
  ADD CONSTRAINT capture_rules_addressed_implies_revive
    CHECK (NOT addressed OR revive);

-- (2) The close record and the surfacing record (J5). closed_at / closed_from_status
-- are written ONLY by internal/tools closeTransition (the one writer of
-- status='closed') and NULLed on reopen. No CHECK ties them to status: integration
-- fixtures INSERT closed tasks directly. No backfill: a NULL closed_at (pre-0030, or a
-- close by an old binary during rollout) makes the revive guard fall back to
-- updated_at, the task's last stamped write. On a closed task only closeTransition
-- stamps it (logs and surfacing do not), so it is the close instant for any close made
-- through the executor. It is LATER if a hand-run UPDATE touched updated_at after the
-- close (then a message ingested in between does not revive), and EARLIER if the task
-- reached 'closed' by a write that did not stamp it (hand SQL, a fixture INSERT; then a
-- message ingested in between DOES revive). surfaced_* = the last time something other than the reconciler
-- put this task on the board (activity revive, overriding-rule creation, a human's
-- plain reopen); message NULL = a human. The reconciler reads it; only executor
-- handlers write it. No index: read by primary key only.
ALTER TABLE tasks
  ADD COLUMN closed_at              TIMESTAMPTZ,
  ADD COLUMN closed_from_status     TEXT,
  ADD COLUMN surfaced_at            TIMESTAMPTZ,
  ADD COLUMN surfaced_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL;

-- (3) The reconciler's hold (J11). 'resurfaced' = not warranted, but activity (or a
-- human) surfaced the task after this pass last saw it; held open until status,
-- status name or assignee changes, or a human closes/dismisses it.
-- surfaced_seen_at = the tasks.surfaced_at value this pass last observed while the
-- task was open. SUPERSEDES 0023's "drop_reason NULL unless dropped": a resurfaced
-- row records the drop fact it is holding off. Drop/add is safe only because migrate
-- runs each file in one transaction (0009, 0025).
ALTER TABLE ticket_status_syncs ADD COLUMN surfaced_seen_at TIMESTAMPTZ;
ALTER TABLE ticket_status_syncs DROP CONSTRAINT ticket_status_syncs_last_action_check;
ALTER TABLE ticket_status_syncs ADD CONSTRAINT ticket_status_syncs_last_action_check
  CHECK (last_action IN ('none','closed','reopened','refused_active','suppressed_dismissed','resurfaced'));

-- Self-check (0025's): a DROP of a name Postgres did not generate would fail loudly
-- here rather than leave two CHECKs, one of which refuses 'resurfaced' at runtime.
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM pg_constraint
   WHERE conrelid = 'ticket_status_syncs'::regclass AND contype = 'c'
     AND pg_get_constraintdef(oid) LIKE '%last_action%';
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly 1 last_action CHECK on ticket_status_syncs, found %', n;
  END IF;
END $$;
```

- **No new table** (invariant 2). There is no task-like row: surfacing is two columns on the one
  `tasks` table, and the hold is a `last_action` value on the reconciler's existing state row.
- `capture_decisions`, `external_refs`, `task_events`, `task_dismissals`, `deliveries`,
  `raw_source_items` and the normalized tables get no schema change.
- **Old binaries are unaffected by the schema.** They never read the new columns, never write
  `'resurfaced'`, and their `capture_rules` INSERT gets the `false` defaults. Their only effect
  during rollout is closes with `closed_at` NULL, which the `updated_at` fallback covers.

New **data**, created by the operator through the executor (Verification step 5): the J2 rule
only. J4 is not seeded (0d: 0 shapes). Nothing is disabled.

## API / MCP tool changes

All through the executor: validate → policy → audit start → handler → audit complete.

| tool | change | surface | policy |
|---|---|---|---|
| `task_reopen` | + revive form `{task_id, message_id, revive:true, reason}` → `{task_id, reopened, status, skipped?, dismissal_id?}`; plain form by a human actor now also surfaces | spine, off MCP (unchanged) | static fallthrough (unchanged) |
| `task_mark_surfaced` (new) | `{task_id, message_id, reason}` → `{task_id, surfaced, skipped?}` | spine, off MCP | static-default; NOT humanOnly (capture calls it) |
| `capture_rule_add` | + `revive`, `addressed` (bool, optional) | off MCP (unchanged) | humanOnly (unchanged) |
| `task_close` / `task_dismiss` | handler writes `closed_at` / `closed_from_status` via `closeTransition` | unchanged | unchanged |

- **Where each call hooks in.**
  - Capture's live switch (`internal/capture/rules_store.go` `EvaluateRules`, the
    `actionTask` / `actionTaskLog` cases) calls `create_task`, `link_external_ref`,
    `task_set_source_thread`, `task_mark_surfaced`, `task_append_log` and `task_reopen` as
    `capture:{connector}` with `Call.TaskID` set.
  - The reconciler (`internal/ticketstatus/store.go` `act`) calls `task_append_log` for a
    `resurfaced` Act, as `ticketstatus:jira`.
- **CLI:**
  - `opsctl capture-rules add --revive --addressed`;
  - `capture-rules list` shows the flags;
  - new counters on `capture-rules run` and `ticket-status sync`.
- No dashboard route. No HTTP surface. **No provider call of any kind.**

## MQTT topics

None added. `ops/pipeline/captured` is unchanged: `AnnounceCaptured` keys on `Considered` only
(F11). A revive or a hold writes `status_changed` / `log` task_events, and the orchestrator's
`status_changed` case fires only `ruleUnblockDependents` on to-closed. No orchestrator change.

## Concurrency

- **Capture and the revive.** Capture passes are serialized by capture's existing advisory lock
  (both copies of one event are decided under it). The revive decides under the tasks row lock,
  the same lock and the same order as `task_dismiss` and SWT-36's guarded reopen, so a dismissal
  and a revive of one task serialize.
- **The reconciler** holds its own existing lock and reads `tasks.surfaced_at` without locking.
  - A revive committed after `loadCandidates` read the row is simply seen next pass.
  - A revive racing the reconciler's close: the close's `closeTransition` and the revive's lock
    serialize. If the close wins, the revive's message must still be ingested after the new
    `closed_at`, and a message already ingested is not, so it skips. That is the conservative
    side, one missed revive for an in-flight race, recorded.
- **The own-action deferral (J17)** adds no lock and no state. A deferred message has no
  `capture_decisions` row, so the next pass (any connector's, under capture's existing lock)
  re-reads it. Its guard reads (`sync_runs`, `raw_source_items`, the jira thread) take no lock:
  a poller run committing mid-pass is simply seen next pass.
- **No new lock**, and no lock literal in the diff.

## Files likely to touch

- `migrations/0030_jira_activity_revive.sql` (new)
- `internal/tools/close.go`: `closeTransition` columns; `reopenArgs.Revive`; `validateReopen`;
  `reviveGuarded`; the extracted restore-target helper; human-actor surfacing in `reopenTask`
- `internal/tools/surfaced.go` (new): `task_mark_surfaced`. Registered in
  `internal/tools/createtask.go` `Register`
- `internal/tools/capturerules.go`: `revive` / `addressed` args, validation, INSERT
- `internal/tools/revive_test.go`, `revive_integration_test.go`, `surfaced_integration_test.go`,
  `surfaced_structure_test.go` (new); `reopen_integration_test.go` (human-actor surfacing)
- `internal/policy/matrix_surfaced_test.go` (new, `matrix_reopen_test.go` shape)
- `internal/capture/rules_store.go`: `storedRule`, `loadRules`, `refTask.status`,
  `decideMessage`, the live switch, `reviveRuleTask`, `markRuleSurfaced`, `RulesStats`
- `internal/capture/revive.go` (new, pure): `overrides`
- `internal/capture/ownaction.go` (new, pure: J17's window, freshness instant, the From parse
  and named-actor exemption, verdict) with `ownaction_test.go` and
  `ownaction_integration_test.go`; `rules_store.go` `ownActionFacts` / `ownActionGuard`,
  `RulesStats.Deferred` / `.Blind`, `MinLiveRulesHorizon` in `RulesConfig.normalize`; every
  capture counter printer, `cmd/opsctl/gate.go` included
- `internal/ticketstatus/routing.go` (`KeyPrefix`, shared with RouteLookup);
  `internal/connector/jira/rawid.go` (`CommentRawIDPrefix`, used by `ingestIssue`)
- `docs/runbooks/HANDOFF-kube-jira-activity-revive.md` (new)
- `internal/capture/revive_test.go`, `rules_revive_integration_test.go` (new);
  `rules_structure_test.go` (counter-line scan)
- `internal/ticketstatus/decide.go`, `store.go` (`candidate`, `loadCandidates`, `upsertState`,
  `count`, `decisionReason`, `Stats`)
- `internal/ticketstatus/decide_test.go`, `store_integration_test.go`, `structure_test.go`
  (0030 pin, `time.Now` ban)
- `cmd/opsctl/main.go`: add flags (:381), list (:438), `capture-rules run` counters (:542),
  `ticket-status sync` counters (:676)
- `cmd/connectors/jira/main.go` (:127, :146), `cmd/connectors/google/main.go`, `watch.go`,
  `cmd/connectors/slackweb/main.go`, `cmd/connectors/upworkcrm/main.go`: counter lines
- `internal/classify/structure_test.go` (:1169 ledger),
  `internal/ticketstatus/delivered_structure_test.go` (ledger mention)
- `docs/runbooks/capture-rules.md`, `docs/runbooks/ticket-status-sync.md`
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`
- If Part D is merged first: `internal/capture/rules_store.go`'s `held` condition, plus its
  test in `internal/capture/gate_integration_test.go`

## In scope / Out of scope

**In scope:**
- migration 0030;
- the revive form and `task_mark_surfaced`, closed_at/closed_from_status, human-reopen
  surfacing;
- the two rule flags end to end (tool, CLI, list);
- capture's live switch;
- the reconciler's `resurfaced` hold;
- counters, runbooks, IK, the kube handoff;
- the own-action guard (J17) and the revive ⇒ jira validation (J18);
- seeding J2 only (J4 is not seeded: 0d found 0 shapes on prod);
- the Part D `!addressed` clause, whichever merges second.

**Out of scope, named because they are the tempting bundles:**
- **Rule 10's successor and the bucket tasks 56/57/60** (capture-rule-ticket-keys, Q1 for the
  non-Jira residue, Q2 per the open question, Q3). This ticket only guarantees that buckets never
  revive.
- **Flagging rules 1, 2 or 3–5.** F8 makes that a new tool, and J3/J4 make it unnecessary. A
  `capture_rule_update` tool is capture-rule-ticket-keys' Q1(b).
- **Connector-side "addressed" detection** (a Jira comment ADF mention of his accountId). No
  gated project is connector-polled.
- **Any code backfill** of the 19 logs or the 23 task-less tickets (J15).
- **SWT-40 Part D itself**, its gate stage, the inquiry lane, promote. Promote's SWT-36 path is
  unchanged: promote tasks never surface.
- **A dashboard marker** for surfaced tasks, or a `/funnel` counter. The `status_changed` reason
  and the reconciler's log line already show on `/tasks/{id}`.
- **Changing the reconciler's warranted predicate** (`Warranted`), D2, or the delivered set.
- **Anything outbound:** no Jira comment, transition or assignment.
- **The build-order neighbours:** the triage go-live (step 6) and the plan import/board (step 10).

## Invariants that apply

1. **Raw-first.** Nothing new is ingested. Every revive and creation is decided from
   `normalized_messages` rows produced from `raw_source_items` by the existing sinks, and its
   inputs (`created_at`, `direction`, subject, sender) are re-derivable by re-normalization. The
   reconciler still decides from the STORED snapshot (D19). `surfaced_at` is an action record, not
   an observation, and is written only through the executor.
2. **One funnel.** No new table. Revived and created work is the same row in the one `tasks`
   table: a revive is a status transition, and creation is `create_task`. Surfacing is two
   columns, and the hold is a `last_action` value on existing state. The board's lanes stay
   filters.
3. **Everything through the executor.**
   - Capture reaches tasks, refs, events and dismissals only via `create_task`,
     `link_external_ref`, `task_set_source_thread`, `task_mark_surfaced`, `task_append_log` and
     `task_reopen`.
   - The reconciler only via `task_close`, `task_reopen` and `task_append_log`.
   - The revive's reads and writes (both instants, the direction, the dismissal stamp, the
     surfacing) all happen inside the handler under the row lock. Callers supply ids only.
   - `TestCaptureRules_NeverWritesToolActionTablesDirectly` and
     `TestTicketStatus_NeverWritesToolActionTablesDirectly` stay green.
   - `task_mark_surfaced` and `task_reopen` stay off both MCP profiles (structural), because F7
     means schema absence is the only gate on args.
4. **Nothing external without a delivery row.** There is no send, no HTTP and no `deliveries`
   read or write. A revived task keeps any delivery row it has. A revive never implies a reply.
5. **Own-message loop closure.**
   - Three layers keep our own messages out: capture's `direction='inbound'` filter, the revive
     handler's and `task_mark_surfaced`'s inbound ERROR, and `ObserveOutbound` untouched.
   - **The one new exposure:** Jira can email him about HIS OWN change (a per-user notification
     setting). That mail is `inbound` (From `jira@treetopllc.jira.com`, display name
     "Anonymous (JIRA)") and would revive a task on our own comment.
   - **J17's own-action guard narrows it for his COMMENTS, in the spine:** our own comment is an
     OUTBOUND message on the ticket's thread, so capture skips the revive or creation when one
     sits in the window, and defers while the thread is not yet known synced. It does NOT close
     it for his field and status edits: the connector stores no changelog, so nothing names an
     edit's actor.
   - What closes it is the Jira setting. Verification 0c is BLOCKING for seeding J2: the owner
     turns the setting off, and an uncorrelated count of Anonymous Treetop mail stays 0 for at
     least a working day.
6. **Stealth attribution.** Nothing client-visible is produced. Log lines and reasons are
   composed from stored ids, keys and status names. No model authors anything.
7. **Orchestrator purity.**
   - No orchestrator rule is added or modified.
   - The reconciler's new clause is pure: `SurfacedAt` and `SurfacedSeen` arrive as VALUES, and
     `decide.go` gains a `time.Now` ban.
   - Capture's `overrides` is a pure truth table.
   - Every action writes an audit row through the executor, and every no-op writes the state
     row, so "why is this task still open" is answerable from the database.

## Sibling patterns to copy

- **The guarded reopen:** `internal/tools/close.go` `reopenGuarded` (SWT-36), with the same lock,
  the same (a)…(e) shape and the same skip vocabulary, plus its integration suite
  `internal/tools/dismissal_reopen_integration_test.go`.
- **Capture's log-then-act:** `rules_store.go` `appendRuleLog` + `reopenRuleTask`, and
  `internal/capture/rules_reopen_integration_test.go`.
- **A spine tool off MCP:** `task_set_source_thread` (`internal/tools/provenance.go`) and
  `TestTaskReopen_StaysOffTheMCPSchemas`.
- **A new reconciler clause with an inert-by-default proof and a CHECK swap:**
  `docs/tickets/qa-delivered-drop_SPEC.md` (SWT-34), migration 0025 including its `DO $$`
  self-check, and `TestTicketStatus_TheAssigneeGateComesFromTheProjectsColumn` for the column-fed
  test.
- **Human-actor keying:** `policy.HumanActor` as used by `draft_delivery` (SWT-20). Never
  re-spell the prefixes.
- **The rule regex check before seeding:** capture-rule-ticket-keys' rule-59 recipe (Go `regexp`
  over an exported corpus, never Postgres ARE, where `\b` is a backspace).
- `FOR UPDATE SKIP LOCKED` is deliberately NOT used: nothing here is a work queue.

## Verification protocol

Run in order. Do not commit before step 3 passes. Do not seed before step 0 passes.

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
alias opsctl='DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl'
P='psql -h 192.168.50.49 -U ops -d ops'
```

**0. Blocking prod pre-checks.** All read-only. Record the outputs in the delivery summary, never
as test literals.

- **0a. State.**
  - `echo "CAPTURE_RULES_MODE=${CAPTURE_RULES_MODE:-<unset, so shadow>}"` must be unset for
    every `opsctl` run below.
  - `opsctl capture-rules list`: rules 1, 2, 3–5, 10 and 59 as this SPEC describes them. Record
    rule 1's and rule 2's priorities (for J4's `--priority`).
  - `$P -tAc "SELECT max(version) FROM schema_migrations"`.
- **0b. Treetop key prefixes.** Expect only API, OPS and WEB, plus NULL rows (digests). Any
  other prefix stops the seeding for a decision (J2, F8):
  ```sql
  SELECT split_part(substring(m.subject from '[A-Z][A-Z0-9]+-[0-9]+'), '-', 1) AS prefix, count(*)
    FROM normalized_messages m
   WHERE m.direction = 'inbound' AND m.sender ILIKE '%jira@treetopllc.jira.com%'
   GROUP BY 1 ORDER BY 2 DESC;
  ```
- **0c. Own-action mail (invariant 5, BLOCKING for seeding J2).**
  - **The sender shape.** Jira sends his own changes as "Anonymous (JIRA)", not under his name.
    Prod stores the From QUOTED, `"Anonymous (JIRA)" <jira@treetopllc.jira.com>`, so the predicate
    is `ILIKE '%anonymous (jira)%'`; `'anonymous%'` never matches the leading quote.
  - **Why this gate.** The guard covers only his comments (J17). His field and status edits are
    covered only by the Jira setting.
  1. The owner turns off Jira's "Notify me about my own changes" and records the moment
     (`:setting_off_at`).
  2. After at least a full working day, this UNCORRELATED count must be 0:
     ```sql
     SELECT count(*), min(m.sent_at), max(m.sent_at)
       FROM normalized_messages m
      WHERE m.direction = 'inbound' AND m.sender ILIKE '%jira@treetopllc.jira.com%'
        AND m.sender ILIKE '%anonymous (jira)%'
        AND m.created_at > :setting_off_at;
     ```
     - **0 new → seed J2.**
     - **Still arriving → STOP seeding.** Those are not his own changes (likely automation, such
       as GitHub-driven transitions). List them (same WHERE; `m.id, m.sent_at, m.subject`) and
       bring them to the owner. He decides an exclusion in the rule's DATA (sender or pattern),
       not in Go.
  3. **Informational only:** the comment correlation. On 2026-09-12 it found 26 rows, of 208
     Anonymous Treetop emails, every one inside J17's window. It no longer gates seeding:
  ```sql
  SELECT m.id, m.sent_at, m.sender, m.subject, o.id AS his_msg, o.sent_at AS his_sent
    FROM normalized_messages m
    JOIN normalized_threads nt
      ON nt.thread_key = 'jira:treetopllc.jira.com:' || substring(m.subject from '(?:WEB|API|OPS)-[0-9]+')
    JOIN normalized_messages o
      ON o.thread_id = nt.id AND o.direction = 'outbound'
     AND o.sent_at BETWEEN m.sent_at - interval '10 minutes' AND m.sent_at + interval '2 minutes'
   WHERE m.direction = 'inbound' AND m.sender ILIKE '%jira@treetopllc.jira.com%'
     AND m.created_at > :setting_off_at
   ORDER BY m.sent_at DESC;
  ```
  The display-name check stays as a second net (expect zero rows; `'%anonymous (jira)%'`, not
  `'anonymous%'`, for the quoted From):
  ```sql
  SELECT m.sender, count(*) FROM normalized_messages m
   WHERE m.direction = 'inbound' AND (m.sender ILIKE '%spataro%' OR m.sender ILIKE '%anonymous (jira)%')
     AND (m.sender ILIKE '%jira@treetopllc.jira.com%' OR m.sender ILIKE '%jira@avviato.atlassian.net%')
     AND m.created_at > :setting_off_at
   GROUP BY 1;
  ```
- **0d. Avviato addressed shapes.** These decide whether J4 is seeded and with which literal
  wording. No rows: J4 is not seeded; record it. **Recorded 2026-09-12: 0 rows on prod, so J4 is
  NOT seeded by this ticket.**
  ```sql
  SELECT regexp_replace(m.subject, 'LHH-[0-9]+', 'LHH-n', 'g') AS shape, count(*)
    FROM normalized_messages m
   WHERE m.direction = 'inbound' AND m.sender ILIKE '%jira@avviato.atlassian.net%'
     AND (m.subject ILIKE '%mentioned you on%' OR m.subject ILIKE '%to you%')
   GROUP BY 1 ORDER BY 2 DESC LIMIT 20;
  ```
- **0e. Offline Go-regexp evaluation.** This is the shadow for a live system.
  - Export JSONL (id, source, sender, subject, body) of:
    - every inbound message from either Jira sender;
    - every message rule 10 matched in the last 30 days;
    - a 500-message Slack control sample.
  - Run a scratch program in the session scratchpad (plain `regexp`, no repo import) applying
    J2's sender test and key regex, J4's pattern and key regex, and rule 59's pattern.
  - Pass conditions:
    - every J2 match derives `^(WEB|API|OPS)-[0-9]+$` or empty, and every empty one has no key
      in its subject;
    - no J2 match is also a rule-59 match;
    - J4 matches only `jira@avviato.atlassian.net` senders;
    - zero Slack messages match J2 or J4.
- **0f. Baselines and the J15 listings.**
  - Closed, undismissed tasks with capture logs after their last close. This is operator SQL,
    reading `task_events` ad hoc; code never mines it:
    ```sql
    WITH lc AS (SELECT task_id, max(created_at) AS closed_at FROM task_events
                 WHERE event_type = 'status_changed' AND payload->>'to' = 'closed' GROUP BY task_id)
    SELECT t.id, t.title, count(*) AS logs_after_close, max(te.created_at) AS last_log
      FROM tasks t JOIN lc ON lc.task_id = t.id
      JOIN task_events te ON te.task_id = t.id AND te.event_type = 'log'
           AND te.created_at > lc.closed_at AND te.payload->>'message' LIKE 'capture:%'
     WHERE t.status = 'closed'
       AND NOT EXISTS (SELECT 1 FROM task_dismissals d WHERE d.task_id = t.id AND d.reopened_at IS NULL)
     GROUP BY 1, 2 ORDER BY last_log DESC;
    ```
  - Task-less Treetop ticket keys in Jira mail since 07-29:
    ```sql
    SELECT k, count(*), max(created_at) AS last_seen, max(subject) AS a_subject
      FROM (SELECT m.created_at, m.subject, substring(m.subject from '(?:WEB|API|OPS)-[0-9]+') AS k
              FROM normalized_messages m
             WHERE m.direction = 'inbound' AND m.sender ILIKE '%jira@treetopllc.jira.com%'
               AND m.created_at >= '2026-07-29') x
     WHERE k IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM external_refs r WHERE r.system = 'jira' AND r.external_key = x.k)
     GROUP BY k ORDER BY last_seen DESC;
    ```
  - Record `opsctl ticket-status sync --dry-run` as the pre-change baseline.

**1. `go test ./...`** covers criteria 6–7, 9, 14–17 (unit parts), 20, 28–32, 35–36 and the
structural and prose guards.

**2. `make integration`** (compose db :5433, `go test -p 1 -tags integration ./...`) covers
criteria 2–5, 8, 10–13, 15, 16, 18–19, 21–27, 33–34 and 37–39. Keep to the mutual-cleanup pact:
- delete test-owned `audit_events` and `policy_decisions` by `task_id` before task cleanup (IK
  SWT-37 landmine);
- delete tasks BEFORE `normalized_threads` and `normalized_messages` (`surfaced_by_message_id` is
  SET NULL, so the order is for `source_thread_id`);
- run each mutation named in a criterion once by hand and record that it goes red.

**3. Local migration check.**
`psql "postgres://ops:ops@localhost:5433/ops?sslmode=disable" -tAc "SELECT max(version) FROM schema_migrations"`
→ `0030`.

**4. Rollout** (`docs/runbooks/HANDOFF-kube-jira-activity-revive.md`). Merging a migration is not
applying it. In this order:
- a. **0030 FIRST.** Confirm prod is at the sibling tickets' numbers (0028/0029), then apply 0030
  with the kube one-shot `migrate` Job (or `DATABASE_URL="$OPS_DATABASE_URL" go run
  ./cmd/tools/migrate`) and re-read `max(version)`. New code selects `capture_rules.revive` on
  every capture pass and writes `tasks.closed_at` on every close, so a new image on a db
  without 0030 fails both.
- b. Build the image. Hand the kube session ONE tag for **every connector CronJob and
  `pipelined` together**. Reason: a new google-image capture that revives while an old
  jira-image reconciler ignores surfacing re-closes that task, a one-flip-per-message bounce
  that lasts until the images match.
- c. Then orchestratord and the dashboard: they call `task_close`, and until rolled their closes
  carry `closed_at` NULL, which the fallback covers.
- d. `go install ./cmd/ops-mcp-user` here AND on 192.168.50.30 (this diff touches
  `internal/tools`), then open new sessions.
- e. **Inertness before seeding.** `opsctl ticket-status sync --dry-run` must match 0f's
  baseline line for line: no task is surfaced yet, so nothing may differ.

**5. Seed J2 only**, and only when all three hold: every workload in step 4 runs the new image
(an old capture binary ignores the flags and would take J2 as a plain creating rule); the owner
has turned off Jira's "Notify me about my own changes"; and 0c's correlation query returns 0 new
rows. Use the exact command in J2 (executor path, humanOnly as `opsctl:$USER`). Then
`opsctl capture-rules list` shows it at 92 with `revive`.

**6. J4 is NOT seeded.** 0d returned 0 Avviato addressed shapes on prod (2026-09-12). Seed it in a
later change only if a new 0d shows the shapes, with the literal wording 0d returns and the
priority from 0a.

**7. Deterministic anti-bounce smoke (J8/J11, no inbound mail needed), on API-4103's task 85.**
```bash
$P -c "SELECT id, status, closed_at, surfaced_at FROM tasks WHERE id = 85;"
opsctl call --tool task_reopen --args '{"task_id":85,"reason":"SWT-45 smoke: comment on API-4103 was missed"}'
$P -c "SELECT status, surfaced_at, surfaced_by_message_id FROM tasks WHERE id = 85;"   # surfaced, message NULL
opsctl ticket-status sync --dry-run | grep API-4103     # action=resurfaced
opsctl ticket-status sync                                # resurfaced=1: ONE log line on task 85
opsctl ticket-status sync                                # resurfaced=0, zero new audit rows for ticketstatus:jira
```
Leave task 85 open: it is the item he reported. Optionally prove that a hand close sticks: run
`task_close`, then `sync`, and the task stays closed.

**8. Natural-traffic smoke.** Expect about 7 Treetop Jira emails a day.
- After the first google tick that shows `"revived"` or `"surfaced_created"` above zero, per
  affected task:
  ```sql
  SELECT t.id, t.status, t.surfaced_at, t.surfaced_by_message_id, m.subject, m.created_at
    FROM tasks t JOIN normalized_messages m ON m.id = t.surfaced_by_message_id
   WHERE t.surfaced_at > now() - interval '1 day';
  ```
- Check four things:
  - the subject's key equals the task's external ref;
  - the jira tick after it prints `resurfaced` for done tickets only;
  - a second tick adds nothing;
  - no new log lands on tasks 56, 57 or 60 from a `jira@treetopllc.jira.com` message
    (`capture_decisions` for those tasks since the seed, joined to sender).

**9. Backfill (J15).** Re-run 0f's two listings and hand them to Salvador. He revives what he
wants with `opsctl call --tool task_reopen --args '{"task_id":N,"reason":"SWT-45 backfill"}'`
(sticky, per J8). Task-less tickets get their tasks from their next email.

**10. Rollback.**
- `opsctl call --tool capture_rule_set_enabled --args '{"rule_id":<J2>,"enabled":false}'`, and
  the same for J4. Jira mail returns to rule 10's behaviour.
- Tasks already surfaced stay held until closed by hand, and a hand close sticks.
- The schema stays (forward-only). Every new column is inert when nothing writes it.

## Open questions

None. The one question (`docs/tickets/jira-activity-revive_OPEN_QUESTIONS.md`: does a Slack or
GitHub mention of a ticket key count as "Jira activity"?) was answered "yes, any mention" and is
folded in as J16, delivered by capture-rule-ticket-keys. Everything else was resolved above
(J1–J18).

## Future work (not this ticket)

- **`capture_rule_update`**, an executor tool (humanOnly, off MCP) for pattern, key_regex,
  priority and flags. F8 makes every rule change a new pattern spelling today. Shared with
  capture-rule-ticket-keys' Q1(b).
- **Connector-side "addressed" detection** for a gated project whose Jira is ever connector-polled
  (an ADF mention of the polling accountId, or `assignee` changed to him), deterministic from the
  stored raw row.
- **Gate-path revive**: Part D reviving a hand-closed task when the ticket is warranted, for
  non-addressed activity. Decision 3's refinement routes that traffic through the gate unchanged,
  so it is not asked for yet.
- **A bounded one-shot backfill** (`--since`, `--limit`, `--dry-run`) under its own claim mode,
  if the J15 hand path proves tedious.
- **A board marker** "revived by activity (message N)" beside SWT-36's dismissal marker, and a
  `/funnel` count of held tasks.
- **Surfacing telemetry**: how often a close email revives (J10's cost measured), to judge
  whether decision 1 should exempt the close notification later.
