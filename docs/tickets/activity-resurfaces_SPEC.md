> Jira: SWT-72

# activity-resurfaces — activity on an open task surfaces it into INCOMING, an ask always becomes its own task, and Requeue is the third review verb

**STATUS: DECIDED.** The two open questions were answered by the owner on 2026-09-22 (OQ-1: **B**,
Requeue lifts `holding → ready`; OQ-2: **A**, claude tasks surface too) and are folded in below;
`docs/tickets/activity-resurfaces_OPEN_QUESTIONS.md` keeps the record. Every other choice is recorded
under "Decisions" with its rationale.

**Evidence status.** Every code fact below was read in this worktree (branch
`ticket-activity-resurfaces`, at `1a8b287`), with file and line. The production facts under "The
trigger" were measured by the sessions that raised the ticket; this spec session ran no SQL.
Verification Step 0 turns every remaining production assumption into a read-only pre-check with a
stated gate. A gate that fails means stop and re-spec, not adapt the code quietly (the SWT-54/SWT-59
precedent).

## Source

Ad-hoc, from Salvador, 2026-09-22, verbatim, in the order he said it:

> when there is a comment we need to move to incoming to review even if the review is to put it back
> on the q. So we need to do that and allow for claude to requeue on low priority

> there are also emails from jose (direct emails) suffering same problem.

> slack from jose and katie are critical. messages from yersterday went to black hole.

> and those messages to create it's own tasks even is they require action on other tasks that would
> be for claude to figure out

> that was the intent int the incomning. fast incoming event to tell people I'll check

## The trigger (measured in the ops db, 2026-09-21/22)

Four shapes of inbound activity landed on OPEN tasks and changed **nothing visible** on the board:

1. **Jira notification mail.** Six comments by Katie (12:52–13:20Z) each attached as a `task_log`
   capture onto the open task of that ticket — #73, #91, #366, #372, #378, #381, all `ready`, human,
   priority 0, sitting in QUEUE.
2. **Direct client mail, no Jira in the path.** José Garcia writes from `jose.g@avviato.com`;
   capture rule 75 (`body_regex` on a `WEB-NNNNN` key) files the mail onto the task already linked to
   that ticket. Message 393396 (2026-09-21 22:31Z, *"PR #3247 — WEB-10469 required External ID locks
   out…"*) → `task_log` on open task 452 (`ready`, human): a silent log line. Two more the same day
   (389781, 389783, 18:17Z) landed on task 458, which was CLOSED, so SWT-53's chat-on-closed-task
   path took them to the inquiry lane instead — different machinery, working as designed.
3. **The inquiry promoter's attach.** Four asks from José — *"Listo para arrancar WEB-10469"*,
   *"Viste lo del resumen…"*, *"Pa cuando tienes planeado el release?"*, *"Epa tas vivo?"* — and two
   from Katie this morning were recorded `attached` and appended as log lines onto open tasks #452
   and #464. **Nobody saw them.** These are client asks that needed a reply, not notes about a
   ticket.
4. The same rule set will do it again on every channel: a `sender` rule, a `body_regex` rule and a
   `thread_key_prefix` rule all converge on the same `task_log` branch, over gmail, Slack, Jira
   notification mail and Upwork.

A log line on an open task is silent: `task_append_log` writes a `task_events` row and nothing else.

## Goal

Two halves, both answering "messages went to a black hole":

- **Resurface.** Inbound activity that a RULE files onto an existing open task (shape 1, 2 and 4)
  surfaces that task into the board's INCOMING section until a human or a Claude session reviews it.
  Review has a third outcome beside Dismiss and Done: **Requeue** — leave the task open, lift a
  `holding` row to `ready`, optionally set the priority, clear the review flag.
- **Never pile an ask onto a task.** An inquiry-lane verdict that needs a reply (shape 3)
  **always creates its own task**, whatever else is open on that thread, with a light pointer both
  ways. INCOMING is the fast "I'll check" surface; an ask has to be a row on it, not a line inside
  someone else's row.

**Usable alone means:** with migration 0039 applied and one image rolled — the next Katie comment or
José email moves its task from QUEUE to INCOMING, remark `new comment` / `new email`, the sender in
the title cell; `actions` → Requeue (priority: unchanged) drops it back to QUEUE, and a `holding`
row to READY; `swb requeue 452` does the same from any Claude session. And the next José Slack ask
appears as **its own** INCOMING row within a pipeline tick, with `related_task: 452` in its body and
a one-line pointer on 452 saying the ask exists.

## What exists (code-read, with file and line)

- **Capture's attach path.** `internal/capture/rules_store.go:498-539`: the `actionTaskLog` branch
  calls `appendRuleLog` (`:1512`, one `task_append_log` through the executor as
  `capture:{connector}`), then exactly one of three mutually exclusive branches — `closeRuleTask`
  (SWT-54 D5, a PR merge/close notice), `reviveRuleTask` (SWT-45, closed task) or `reopenRuleTask`
  (SWT-36, dismissed task). Both reopen branches act only on a CLOSED task. The decision is made in
  `decideMessage` (`:888`) whenever `taskForExternalRef` finds a task for the key — whatever the rule
  kind or the channel, which is why shapes 1, 2 and 4 arrive through one branch.
- **The promoter.** `internal/promote/promote.go:145-159` — `Decide`, pure: rule 1 an OPEN task on
  the thread → `attached`; rule 2 a DISMISSED one → `attached` + a guarded reopen; then the lane's
  create. `internal/promote/store.go:191-212` — `act` carries it out: `appendVerdictLog` (`:520`),
  `recordTask`, and for a dismissal `reopenDismissed`. `threadTask` (`:372`) finds the thread's
  oldest open task, else its oldest dismissed one, else the oldest finished one, **project-scoped**.
- **The inquiry gate.** `internal/promote/inquiry.go:117-198`: `InquiryGate` returns the FIRST
  failing reason of `rethreaded, kind, stale, pending, answered, not_addressed`, then C-D13's
  `claude_task` — which exists solely because an attach would log untrusted text onto a claude task
  and "creating a second task instead would break Q3".
- **`tasks.source_thread_id` is deliberately NOT unique** (`migrations/0019_delivery_provenance.sql:3-10`:
  "allows many tasks per conversation, which external_refs cannot").
- **Capture decides before the inquiry lane can see a message.**
  `internal/replyfold/replyfold.go:127-130`: a message is inquiry-eligible only when its latest
  decision is `attributed`, or when its latest LIVE decision is a `task_log` that capture recorded as
  resurfacing onto a still-closed task (SWT-53).
- **`task_mark_surfaced`** (`internal/tools/surfaced.go`) is SWT-45 J7: spine-facing, off both MCP
  profiles, not humanOnly, writes no event, and writes `surfaced_at` / `surfaced_by_message_id` under
  the row lock with three rules — non-inbound message = ERROR, closed task = skip, same message twice
  = no-op.
- **The Jira reconciler reads that column.** `internal/ticketstatus/decide.go:133`:
  `newSurfacing := !obs.SurfacedAt.IsZero() && (state == nil || !obs.SurfacedAt.Equal(state.SurfacedSeen))`.
  Not warranted + restorable + new surfacing → `Action: "resurfaced"`, a HOLD that ends only on a
  change of status category, status name or assignee, or a hand close (`:135-151`).
- **The board.** `internal/dashboard/sections.go` (`incomingKind`, `boardSectionOf`,
  `boardSections`), `lights.go`, `board.go` (`boardLightFacts`, two statements; `listTasks`),
  `display.go` (`remarkFor`, `boardTallies`, `boardPanels`).
  `board_incoming_structure_test.go` pins `incomingKind`'s signature, the first statement's facts and
  a ban on `normalized_messages` anywhere `boardLightFacts` reaches.
- **The verbs.** `dismissTaskAction` / `closeTaskAction` (`board.go:817`, `:845`), one `executeTask`
  each, redirect via `boardBack`. `TestTasksTemplate_VerbFormsByteUnchanged` matches each form by its
  own action URL, so a third form does not disturb it.
- **Priority is 0..3** (`internal/tools/priority.go:23-39`). There is no value below 0.
  `task_set_priority` is humanOnly (`internal/policy/matrix.go:101`) and on both MCP profiles.
- **`task_events` has no `event_type` CHECK** (`migrations/0001_initial.sql:142`);
  `internal/orchestrator/rules.go:124-129` fires on `status_changed` **only** for
  `to ∈ {delivered, closed}`, and unknown types fall into `Evaluate`'s nil default (pinned for
  `working_state_changed` by `rules_test.go:737-747`).

## Decisions

### D1 — NEW columns. `surfaced_at` is NOT reused; the brief's recommendation is overturned

The brief recommended reusing `tasks.surfaced_at` / `surfaced_by_message_id` plus one new
`reviewed_at`. `decide.go` says that breaks the Jira reconciler on exactly this population:

- the trigger's tasks are jira-keyed, i.e. the reconciler's own candidates;
- while the ticket still warrants its task a surfacing is CONSUMED every pass
  (`decide.go:179-183`, `RecordSeen`), so nothing changes — but once the ticket is Done, Delivered or
  assigned away, a new surfacing returns `resurfaced` and **holds the task open forever**, until a
  facts change or a hand close;
- Jira mails on every close (IK SWT-45 J10), that mail is captured, matches the same rule, and lands
  as a `task_log` on the still-open task. So reuse means **every closed Treetop ticket leaves its
  task stuck on the board**. SWT-45 saw this and chose not to surface open tasks for that reason (IK:
  "Activity on an OPEN task only logs; if it surfaced, no done ticket's task would ever close").

Migration 0039 therefore adds a parallel, board-facing pair plus the review stamp, and `surfaced_at`
keeps exactly its SWT-45 meaning:

```
tasks.activity_at             TIMESTAMPTZ   -- last inbound activity a rule filed onto this open task
tasks.activity_by_message_id  BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL
tasks.reviewed_at             TIMESTAMPTZ   -- last human/Claude review of that activity
```

**"Needs review"** ⇔ `activity_at IS NOT NULL AND (reviewed_at IS NULL OR activity_at > reviewed_at)`,
on a task the board is not already excluding (closed or green).

**The seed-value question dissolves.** The brief asked what a NULL `reviewed_at` does to rows whose
`surfaced_at` is already set (SWT-45 revives) and whether to backfill. With a NEW `activity_at`,
every existing row is NULL: **no existing task appears in INCOMING at rollout, no backfill** — the
behaviour the backfill was meant to buy, for free.

**No `activity_kind` column.** An earlier draft carried one (`comment` | `ask`) to word the remark.
D11 removes the promoter's ask-attach entirely, so `ask` would never be written — a discriminator
that is constant in production, which the IK names as an inert-predicate landmine. The remark is
derived from the surfacing message's CHANNEL instead (D5), which is real data and needs no column.

### D2 — `reviewed_at` rather than clearing `activity_at`

Clearing is one column fewer but destructive (the last activity's provenance is gone) and it widens
the concurrency window: an activity landing between the render and the click would be erased with no
trace. A monotone `reviewed_at` keeps the history, and a later activity re-surfaces without a reset.
(IK: *owner works the board concurrently* — re-read before writing, idempotent verbs.)

### D3 — Who writes it: a sibling spine tool, `task_mark_activity`, on the rule-driven attach paths

`{task_id, message_id, reason}` → `{task_id, marked, skipped?}`. Modelled line for line on
`task_mark_surfaced`, under the tasks row lock:

- a missing or non-inbound `message_id` is an **ERROR** (invariant 5 at the verb);
- a **closed** task is a skip (`task_closed`) — which is what keeps closed-task work out of this
  ticket for free: the SWT-36 reopen and SWT-45 revive branches run AFTER the mark, so the task is
  still closed when the mark is attempted and the mark self-excludes;
- the **same message twice** is a no-op (`already_marked_by_message`), so `activity_at` never moves
  on a replay;
- otherwise `activity_at = now()`, `activity_by_message_id = the message`.
- It touches nothing else — not `updated_at` (which would move the board's `updated` cell and the
  SWT-45 revive guard's fallback), not `surfaced_at`, status, priority or any claim — and it writes
  **no `task_events` row**: the caller appended the log line one statement earlier, and a second
  event would be noise on the orchestrator's feed.

**Callers, and the actor each uses:**

| Path | Call site | Actor | In practice |
|---|---|---|---|
| capture `task_log` attach — ANY channel, ANY rule kind | `internal/capture/rules_store.go`, the `actionTaskLog` branch, right after `appendRuleLog` and before the prClose/revive/reopen branch | `capture:{connector}` (`cfg.Actor`) | shapes 1, 2, 4: the whole point of the ticket |
| promoter attach | `internal/promote/store.go`, `act`'s `"attached"` branch, after `recordTask` and before `reopenDismissed` | `promote:{lane}` (`actorFor(v.Lane)`) | **personal lane only** after D11; the inquiry lane's remaining attach targets a dismissed (closed) task, so the mark is a skip |

**Does the promoter still need the hook at all?** Yes, but narrowly. After D11 no ASK is ever
attached, so the inquiry lane's only `attached` decision is rule 2's dismissed-task reopen, whose
target is closed — a skip by construction. What remains is the **personal** lane, which is not an ask
lane: it attaches a follow-up actionability verdict onto the thread's open task, exactly the silent
log line this ticket is about. One lane-agnostic call site, no `kind` argument, no lane branch.

**One exclusion, deliberate:** capture skips the mark when `decision.prClose` is set (SWT-54's
merged/closed PR notice). The next call closes the task; surfacing a row in order to close it one
statement later is noise, and when the close is refused the task is active work someone holds.

**Why a second tool and not an argument on `task_mark_surfaced`:** one tool writing two column pairs
by argument makes the reconciler's contract conditional on an argument, and F7 says an argument is
not a boundary. `task_mark_activity` is spine-facing exactly like its sibling — **off both MCP
profiles**, not humanOnly (capture and promote call it), no policy rule of its own, falling through
to the static allow-list (`internal/policy/matrix_surfaced_test.go`'s shape).

### D4 — The Jira reconciler is untouched, and a test proves it

`internal/ticketstatus` never learns the new columns (structure test: the package's source mentions
neither `activity_at` nor `reviewed_at`), and `Observation.SurfacedAt` still comes from `surfaced_at`
alone. Concretely: a capture `task_log` onto an open task whose ticket is Done still closes that task
on the next reconciler tick (criterion 24).

### D5 — The board: a third incoming kind, `activity`; unreviewed activity leads the section; the remark comes from the channel and the sender from a PK join

- `sections.go` gains `const incomingActivity = "activity"`, and `incomingKind` becomes
  `incomingKind(fromMessage, prReview, activity bool) string` (message > pr_review > activity > "").
  A deliberate signature amendment to `board_incoming_structure_test.go` (SWT-59 criterion 2).
- `boardSectionOf` is byte-unchanged: any row with a non-empty `Incoming` goes to `incoming` unless
  it is closed or green.
- **Within-section order** (amending SWT-59 I5, which the brief asked me to argue):
  1. light rank (unchanged — a red row still leads);
  2. **needs review before reviewed**, whatever the kind;
  3. kind rank: message 0, pr_review 1, activity 2;
  4. among two needs-review rows, the later `activity_at` first; otherwise `id DESC` (SWT-59's key).

  **The argument.** `activity` last in the kind list and nothing else would bury the newest fact,
  because a brand-new ask is an old task id away from a comment on #73. `activity` first would invert
  his own order ("emails or slacks… same thing with prs") on a quiet day. Key 2 settles both: the
  unreviewed thing — the only thing in this section with a clock on it, and the only thing Requeue
  can clear — leads, and among the unreviewed his message-before-PR order still holds. With no
  needs-review row anywhere the order is byte-identical to SWT-59's (criterion 15).
- **The "why" the row shows.** One PK LEFT JOIN, `normalized_messages nm ON nm.id =
  t.activity_by_message_id`, yields two display facts:
  - the **Remarks cell** shows `new comment` (jira), `new email` (gmail), `new slack` (slack),
    `new message` (anything else, including upwork) — a pure helper over `nm.channel`;
  - the **title cell** gains one muted span, `from {{.ActivityFrom}}`, exactly where
    `reopened after dismissal` already sits, carrying `nm.sender`.
  - That answers the coordinator's question about the two channels with one field: Jira's From
    carries the actor in the common case (`"Katie Evans (JIRA)"` — IK SWT-45's named-actor
    exemption), and a direct mail carries the person (`José Garcia <jose.g@avviato.com>`). The ugly
    case is `"Anonymous (JIRA)"`, Jira's placeholder for **his own** changes — and seeing that word
    is exactly how he recognises a self-inflicted row and Requeues it in one tap (D10).
  - **No body text, no first line:** the departures row has no width for it, the title already names
    the ticket, and previews already live in the task's log. **Not a count** of unreviewed items: it
    needs a second source of rows and a second read for a number that changes nothing he does.
- **This NARROWS SWT-59's `normalized_messages` ban rather than deleting it.** The ban exists because
  `normalized_messages` has no `thread_id` index (a scan every 5 s). A PK equality on `nm.id` is not
  that query. The amended structure test permits the token ONLY inside
  `LEFT JOIN normalized_messages nm ON nm.id = t.activity_by_message_id` and keeps banning
  `source_thread_id`, `surfaced_by_message_id`, `thread_id` and `'attached'`.
- **Still at most two statements.** Lights, tallies, panes, auto-refresh, `boardQuery` and both
  exports are untouched.

### D6 — The verb: `task_requeue {task_id, priority?, note?}`, humanOnly, both MCP profiles, and it lifts `holding → ready` (OQ-1 = **B**)

- **Effect**, one transaction under `lockTask`:
  1. refuse a **closed** task by name (`task_reopen` is the verb for that);
  2. `reviewed_at = now()` — unconditional, so a double-tap is a clean no-op;
  3. **if the status is `holding`, set it to `ready`** and write one `status_changed`
     `{from:"holding", to:"ready", rule:"requeue", reason}` event, the `internal/tools/dependency.go:130,176`
     spelling. Every other open status is left alone; this is the only transition the verb can make.
  4. if `priority` is given and differs, apply it through the **shared** helper `applyPriority`
     factored out of `task_set_priority`, which writes `priority`, `updated_at` and the existing
     `priority_changed {from,to,reason}` event — one spelling of the priority write;
  5. one `task_events` row, type `reviewed`, payload
     `{note, from_status, to_status, priority_from, priority_to, priority_changed, had_unreviewed_activity}`.
- **Why B is safe.** `Evaluate` fires on `status_changed` only for `to ∈ {delivered, closed}`
  (`rules.go:124-129`), so a `holding → ready` event runs no rule; the transition is the same one
  `unblock_task` already makes into `ready`; and it gives the inquiry review lane the exit it has
  never had — which matters now that D11 makes every ask a `holding` task of its own.
- **Priority range is 0..3**, `checkPriorityRange`'s existing check. Nothing is below 0, so "low
  priority" **is** 0 and the verb does not invent −1. Omitted = unchanged (a dropped argument must
  never demote a task — `task_set_priority`'s pointer rule). Raising is allowed; a second, narrower
  scale would be new vocabulary for no gain.
- **Policy: `humanOnly`**, beside `task_set_priority` and `task_signal`. That is exactly the split
  asked for: an interactive session is `mcp:manual:salvo`, which `policy.HumanActor` passes; a worker
  console is `mcp:{client}`, refused — workers never choose their own work, and this verb can lift a
  status and raise a priority. `dashboard:` and `opsctl:` pass.
- **MCP:** in `agentTools` (`schemas.go`) and in `userProfileTools`, so both `.mcp.json`'s server and
  the user-scope `ops-mcp-user` list it. No pin.
- **Dashboard:** `POST /tasks/{id}/requeue` → `requeueTaskAction` → one `executeTask` with
  `boardBack(r)`, no SQL of its own. The form lives in the per-row `actions` popup under
  `{{if .NeedsReview}}` — exactly where there is something to clear. Its priority `<select>` leads
  with `priority: unchanged` (empty value, omitted from the args) then `0 normal … 3 urgent`; a
  default of 0 would silently demote an elevated task on a careless tap.
- **Words:** one Instructions line in `internal/mcpserver/serve.go` and one row in
  `skills/swb-status/SKILL.md`'s trigger table — `swb requeue <id> [priority]`.

### D7 — Everything surfaces: channel-blind, rule-blind, assignee-blind (OQ-2 = **A**)

Shape 2 proves this cannot key on the channel or the rule: the hook sits in the single
`actionTaskLog` branch every rule kind and every connector funnels through. Per OQ-2 = A it is also
blind to `assignee_type`: a comment on a `claude` task in flight surfaces it into INCOMING (it keeps
its yellow light and its session tag). The trigger for this whole ticket was activity going unseen,
and a worker task jumping into INCOMING is a truthful "this needs your eye". If the yellow rows
annoy, the narrowing is one `AND t.assignee_type = 'human'` in the `needs_review` expression.

### D8 — Dismiss and Done need no change, because `closeTransition` stamps `reviewed_at`

A closed task leaves INCOMING through `boardSectionOf`'s existing guard, so neither verb needs a new
argument. But a later SWT-36 reopen or SWT-45 revive would bring the row back with STALE unreviewed
activity and a stale remark. One line prevents it: `closeTransition`'s **close** UPDATE (the one
writer of `status='closed'`, `internal/tools/close.go`) also sets `reviewed_at = now()`; the
**reopen** UPDATE leaves it alone, so old activity stays reviewed and only new activity resurfaces.
No orchestrator rule learns anything: R1/R2/R8 keep calling `task_close`.

### D9 — Orchestrator purity (invariant 7)

No rule reads `activity_at` or `reviewed_at`. `task_mark_activity` emits no event at all;
`task_requeue`'s `reviewed` event falls into `Evaluate`'s default branch (like SWT-52's
`working_state_changed`), and its `status_changed {to:"ready"}` hits the `to ∈ {delivered, closed}`
guard and returns nil. Criterion 23 adds both rows to the existing "must fire nothing" table.

### D10 — Accepted residual, measured before the roll: his OWN Jira edits will surface tasks

Jira mails him about his own changes when the setting is on. Capture's own-action guard
(`internal/capture/ownaction.go`) covers his own COMMENTS, and only on the revive/create paths; IK
records that **182 of 208 Anonymous Treetop mails are field/status-edit notices the guard cannot
attribute**. Those arrive as plain `task_log` attaches and will surface their task with
`new comment · from Anonymous (JIRA)`. Cost: one Requeue tap each. Step 0a MEASURES the daily volume
and the Anonymous share before the roll, with a stop gate. Mitigations are deliberately Future work
(key the mark on the own-action window, or a per-project notifier-sender suppression in SWT-53's
equality spelling) — a suppression built on a sender `ILIKE` would drop 182 mails that are not
necessarily his (the IK's standing warning).

### D11 — OWNER DECISION, 2026-09-22: an inquiry ask ALWAYS becomes its own task

> "and those messages to create it's own tasks even is they require action on other tasks that would
> be for claude to figure out" … "that was the intent int the incomning. fast incoming event to tell
> people I'll check"

Evidence: José's four asks and Katie's two were `attached` onto #452 / #464 and went unseen. An ask
needs a reply from *him*; a reply is a delivery on a new thread of work, and the relationship to an
existing ticket task is for the session working it to figure out.

**The change, minimal and pure.** `promote.Decide` keeps rules 1–4 byte-identical for the **personal**
lane. For `LaneInquiry` the OPEN-task rule is replaced:

```
inquiry, existing open  -> create (inquiryCreateStatus, action "review"), RelatedTaskID = existing.ID
inquiry, dismissed      -> attached + reopen   (unchanged, rule 2)
inquiry, nothing        -> create              (unchanged)
```

`Decision` gains `RelatedTaskID int64`, set on the inquiry create only. Rule 2 is deliberately NOT
touched: a dismissed task coming back is VISIBLE (SWT-36's reopen, the board's
`reopened after dismissal` marker) and carries the labelled `reopened_by_message_id` — it is not a
silent pile-on.

**The link, both ways, lightest honest form:**

- **On the new task:** one more line in the C-D9 body contract, LAST, after `verdict`:
  `related_task: N` — or `(none)`, the contract's own convention for an empty value. A named,
  one-line amendment to the pinned body text.
- **On the old task:** ONE `task_append_log` through the executor as `promote:inquiry`, text
  `promote: ask #<new id> created from this thread (message M)`. **Ids only, no message text** —
  which is why it is safe on a claude task, whose log feeds a worker prompt (the very worry behind
  C-D13). It is NOT an activity mark: the new task is the thing to look at, and surfacing the old one
  too would double the rows. An explicit exclusion, with a test.
- **Not `parent_id`.** It carries plan ordering and lifecycle meaning (R-rules, the board's tree,
  plan import); an ask is not a subtask of a ticket. Not a `task_dependencies` row either: nothing is
  blocked.
- `decisionReason` gains a part so `classify_promotions.reason` records it:
  `thread's open task N; created its own task (owner decision 2026-09-22: an ask is always its own task)`.
- **Which task the pointer names:** `threadTask`'s existing result, unchanged — the thread's OLDEST
  open task in the project, i.e. precisely the task the ask would have been piled onto today. No
  second query, no new ordering.

**C-D13 (`claude_task`) narrows instead of disappearing.** Its whole justification was the attach and
the reopen ("creating a second task instead would break Q3"). Q3 no longer applies to asks, so gating
an ask because the thread's task belongs to a worker would keep the black hole open for exactly the
messages this ticket is about. The gate becomes: refuse only when the thread task is not a human's
**and** `Decide` would attach to it (rule 2's dismissed path) — spelled as one call to the pure
`Decide`, so "would attach" has one spelling. Every other C3 reason is untouched.

**The replied-since fold STAYS.** `GateAnswered` (`replyfold.RepliedSinceCol`) is what keeps an ask
he answered in Slack within minutes from becoming a task at all, and `GateStale`, `GatePending`,
`GateRethreaded`, `GateKind` and `GateNotAddressed` are unchanged. This ticket widens what a passing
verdict BECOMES; it does not widen what passes.

**Consequence accepted:** a thread can now hold several open tasks. `tasks.source_thread_id` was
built for that (`0019`: "allows many tasks per conversation"), nothing has a unique index on it, and
`threadTask` already takes the oldest. Q3's "a thread yields at most one open task" now reads "…for
the personal lane; an inquiry ask is always its own task". Both SPECs get a dated amendment line.

### D12 — Boundary: when a message is BOTH a rule-filed attach and an ask, capture wins — by construction

Capture decides first, and one live decision per message is forever
(`capture_decisions_live_uniq`). The inquiry lane only ever sees messages whose latest decision is
`attributed`, or a `task_log` that capture recorded as resurfacing onto a **still-closed** task
(`replyfold.InquiryEligibleLatestSQL`, `replyfold.go:127-130`). So a message a RULE files onto an
existing OPEN task never reaches the inquiry lane at all, and cannot produce both outcomes. No code
in this ticket changes that, and none needs to.

**What that means in practice, stated plainly so nobody re-litigates it from the board:** a client
ask that happens to quote a `WEB-NNNNN` key and is therefore claimed by rule 75 SURFACES that
ticket's task (with the sender's name) rather than becoming its own task. That is one tap on
INCOMING instead of a black hole, which is the improvement this ticket ships. If it turns out José's
asks are routinely swallowed by a key-matching rule, the fix is in the RULES (narrow rule 75's
pattern, or stop filing his DMs onto ticket tasks) or in SWT-53's resurface rule — not here. Step 0f
measures how often it happens so the question can be answered with data.

## Acceptance criteria

### Part 1 — schema

1. **Migration `0039_task_activity_review.sql`** adds to `tasks`: `activity_at TIMESTAMPTZ`,
   `activity_by_message_id BIGINT REFERENCES normalized_messages(id) ON DELETE SET NULL`,
   `reviewed_at TIMESTAMPTZ`. No index (read by primary key and per displayed row), no CHECK, no
   backfill, no other table touched. Its header states the rollout barrier (Verification Step 5).

### Part 2 — `task_mark_activity` (`internal/tools/activity.go`, new)

2. Registered in `internal/tools/createtask.go`'s table with `validateMarkActivity` / `markActivity`;
   every call goes through the executor (invariant 3).
3. Validation refuses `task_id <= 0`, `message_id <= 0` and an empty `reason`.
4. Handler, under the row lock: a missing or non-inbound message is an ERROR naming invariant 5; a
   closed task returns `{marked:false, skipped:"task_closed"}`; an `activity_by_message_id` already
   equal to the message returns `{marked:false, skipped:"already_marked_by_message"}`; otherwise it
   sets both columns and returns `{marked:true}`.
5. It writes no `task_events` row and never mentions `surfaced_at`, `surfaced_by_message_id`,
   `reviewed_at`, `status`, `priority` or `updated_at` (structure test over the file).
6. Absent from `internal/mcpserver/schemas.go` and from every profile list — the
   `TestTaskMarkSurfaced_StaysOffBothMCPProfiles` scan, extended to both tool names.
7. Policy: `Decide` returns allow/static-default for `capture:google`, `promote:classify`,
   `dashboard:salvo` and `mcp:treetop` alike, and the tool is in neither `humanOnly` nor
   `mcpHumanOnly` (the `matrix_surfaced_test.go` shape).

### Part 3 — the attach paths

8. **Capture.** In `EvaluateRules`' `actionTaskLog` branch, live mode only: after `appendRuleLog`
   succeeds and before the prClose/revive/reopen branch, `markRuleActivity` calls
   `task_mark_activity {task_id, message_id: pm.msg.ID, reason:"capture: …"}` as `cfg.Actor` —
   unless `decision.prClose` is set. An error fails the pass (the `appendRuleLog` policy).
   `RulesStats` gains `Activity int`, counted from `marked:true` and printed by the existing counter
   line.
9. **Promoter.** In `act`'s `"attached"` branch, after `recordTask` and before `reopenDismissed`,
   `markVerdictActivity` calls the same tool as `actorFor(v.Lane)`. `Stats` gains `Activity int`.
   Dry runs call nothing.
10. **Blind to channel and rule** (the José case): an integration test drives a `body_regex`
    gmail-sourced rule and a `sender` Slack-sourced rule onto the same task and asserts both set
    `activity_at`. Neither new function contains a channel or rule-kind literal.
11. **Closed targets untouched:** a capture attach onto a closed task (SWT-45 revive and SWT-36
    dismissal shapes) leaves `activity_at` NULL, and the revive/reopen still happens.
12. **A PR merge/close notice does not surface** (`decision.prClose`): `activity_at` stays NULL and
    the task is closed as today.

### Part 4 — the ask always becomes its own task (D11)

13. **`Decide` is pure and its personal-lane table is byte-unchanged.** For `LaneInquiry` with an
    OPEN thread task it returns `{Action: "review", Status: inquiryCreateStatus, RelatedTaskID: existing.ID}`;
    with a dismissed one, rule 2 unchanged; with none, the create unchanged. Unit table over
    `{lane} × {no task, open task, dismissed task, finished task} × {whitelisted kind, other}`.
14. **`act` carries the link.** On a create with `RelatedTaskID != 0`: the new task's body ends with
    `related_task: N`; one `task_append_log` lands on task N as `promote:inquiry` with the exact text
    `promote: ask #<new> created from this thread (message <M>)` and **no message text**; the log is
    NOT an activity mark (`tasks.activity_at` of task N stays as it was); `classify_promotions` for
    the message has `action='review'`, `task_id` = the NEW task, and a reason naming N. `Stats` gains
    `Related int`.
15. **Every inquiry body carries the key**, `(none)` when there is no related task — the C-D9 body
    contract amended by exactly one line, at the end (the pinned-text test amended by name).
16. **C-D13 narrows:** an inquiry verdict on a thread whose open task is a CLAUDE task is NOT gated
    and creates its own human task; a verdict whose only thread task is a DISMISSED claude task is
    still gated `claude_task`. `InquiryGate` stays pure and keeps C3's reason order.
17. **The other gates are unchanged**, `GateAnswered` first among them: a verdict whose thread has an
    outbound reply since the ask is still gated and creates nothing (integration, with a real
    outbound message on the thread).
18. **No duplicate storm:** two asks on the same thread in one pass create TWO tasks, each with its
    own promotion row and its own pointer log; re-running the pass creates none (the claim is one row
    per message, forever).

### Part 5 — `task_requeue` (`internal/tools/requeue.go`, new)

19. Registered like every other tool; args `{task_id, priority?, note?}`; validation refuses
    `task_id <= 0` and a `priority` outside 0..3 via the existing `checkPriorityRange`.
20. Handler, one transaction under `lockTask`, in D6's order: refuses `closed` by name; always sets
    `reviewed_at = now()`; lifts `holding → ready` with one `status_changed {from,to,rule:"requeue",reason}`
    event and leaves every other status alone; applies the priority through `applyPriority`, the
    helper now shared with `task_set_priority` (whose behaviour and tests stay byte-unchanged,
    same-value no-op and `priority_changed` event included); writes one `reviewed` event. Result:
    `{task_id, status, reviewed:true, priority:{from,to,changed}}`.
21. Idempotence: a second call changes nothing but `reviewed_at` and the second `reviewed` event —
    no second `status_changed`, and no `priority_changed` when `priority` is omitted.
22. Policy: in `humanOnly`. Allowed for `mcp:manual:salvo`, `dashboard:salvo`, `opsctl:salvo`; denied
    with rule `human_only` for `mcp:treetop`, `worker:treetop`, `capture:google`, `drafts:gpt` and
    `orchestrator` (the six-actor-shape test the IK demands).
23. MCP: in `agentTools` and `userProfileTools`; `ProfileUser` and `ProfileFull` list it,
    `ProfileRead` does not; the schema's `minimum`/`maximum` and level names equal
    `tools.PriorityMin` / `PriorityMax` / `PriorityLevels` (the `TestTaskSetPrioritySchema` shape);
    the tool counts in `serve_test.go` / `profile_test.go` are updated;
    `internal/mcpserver/serve.go`'s Instructions and `skills/swb-status/SKILL.md` carry a
    `swb requeue <id>` line (the existing phrase tests).

### Part 6 — close, reopen, orchestrator

24. `closeTransition` sets `reviewed_at = now()` in the CLOSE update only; the reopen update leaves
    it. Integration: activity → Done → a SWT-36 reopen by a message that marks no activity → the
    reopened row is NOT in INCOMING; a reopen followed by NEW activity is.
25. `internal/orchestrator/rules_test.go`'s "must fire nothing" table gains a `reviewed` row and a
    `status_changed {from:"holding", to:"ready"}` row; a structure test asserts `internal/orchestrator`
    mentions neither `activity_at` nor `reviewed_at` nor `task_requeue`.
26. **J11 non-regression (integration).** A jira-keyed OPEN task whose ticket is Done: run a capture
    pass that attaches a comment (so `activity_at` moves), then the reconciler — `surfaced_at` is
    still NULL, the decision is `closed`, not `resurfaced`, and the task is closed. Mutating
    `task_mark_activity` to write `surfaced_at` turns this red.

### Part 7 — the board

27. `lightFacts` gains four **display-only** fields, documented as such and never read by `lightFor`
    (the `TestLightFacts_DisplayOnlyFieldsNeverFeedTheLight` shape): `NeedsReview bool`,
    `ActivityChannel string`, `ActivitySender string`, `ActivityStamp string`.
28. `boardLightFacts`' FIRST statement adds, inside the `f` select and the outer list, with
    `LEFT JOIN normalized_messages nm ON nm.id = t.activity_by_message_id`:

    ```sql
    COALESCE(t.activity_at IS NOT NULL
             AND (t.reviewed_at IS NULL OR t.activity_at > t.reviewed_at), false) AS needs_review,
    COALESCE(nm.channel, '')                                                      AS activity_channel,
    COALESCE(nm.sender, '')                                                       AS activity_sender,
    COALESCE(to_char(t.activity_at AT TIME ZONE $2,
                     'YYYY-MM-DD HH24:MI:SS.US'), '')                             AS activity_stamp
    ```

    Still at most two statements; the second byte-unchanged; `boardQuery`, `TaskExportRow` and both
    exports byte-unchanged; no Go clock involved.
29. `sections.go`: `const incomingActivity = "activity"`;
    `incomingKind(fromMessage, prReview, activity bool) string`; `incomingRank` gains `activity: 2`;
    `boardSectionOf` unchanged; the incoming comparator is D5's four keys. Both functions stay pure
    (no `s.pool`, `Query(`, `Exec(`, `time.`).
30. `taskRow` gains `NeedsReview bool`, `ActivityFrom string`, `ActivityStamp string` — board-only,
    never export columns. `listTasks` sets them plus
    `Incoming = incomingKind(f.FromMessage, f.PRReview, f.NeedsReview)` and, when `NeedsReview`,
    overrides `Remark = activityRemark(f.ActivityChannel)`.
31. `display.go` gains `func activityRemark(channel string) string` → `new comment` (jira) |
    `new email` (gmail) | `new slack` (slack) | `new message` (anything else or empty).
    `remarkFor` is byte-unchanged.
32. `templates/tasks.html`: exactly two additions — the muted `from {{.ActivityFrom}}` span in the
    title cell (inside `{{if .NeedsReview}}`, only when non-empty) and the Requeue form in the
    `actions` popup under `{{if .NeedsReview}}`. Dismiss and Done stay byte-identical
    (`TestTasksTemplate_VerbFormsByteUnchanged` untouched); the template still contains no occurrence
    of the word `incoming`, no HTMX and exactly one `<script`.
33. `server.go` registers `POST /tasks/{id}/requeue` behind `auth.Require`, beside dismiss and close.

### Part 8 — unit tests (no db)

34. `incomingKind`: all eight combinations, message > pr_review > activity.
35. `boardSections` ordering: a needs-review grey `activity` row precedes a reviewed grey `message`
    row; a red row still precedes both; two needs-review rows sort by `ActivityStamp` DESC, falling
    back to `id DESC` on equal or empty stamps; the partition holds (every id exactly once, fed in id
    order and reversed); **with no needs-review row anywhere the section strings are byte-identical to
    SWT-59's mixed-board golden.**
36. `activityRemark`'s table including the empty and unknown channel.
37. `Decide`'s table (criterion 13) and `InquiryGate`'s reason order (criterion 16), both with no I/O.

### Part 9 — structure tests

38. The amended `board_incoming_structure_test.go`: the new `incomingKind` signature; the four new
    tokens present in the first statement and absent from the second; `normalized_messages` permitted
    ONLY as the PK join (regex-pinned, exactly one occurrence) while `source_thread_id`,
    `surfaced_by_message_id`, `thread_id` and `'attached'` stay banned; `lightFor`'s body mentions
    none of the four new fields.
39. `internal/ticketstatus`'s source mentions neither `activity_at` nor `reviewed_at`
    (`revive_structure_test.go`'s neighbour); `internal/promote`'s pointer-log text contains no
    verdict field beyond the two ids (a source scan for `v.Title` / `v.Sender` in that function).

### Part 10 — integration (`//go:build integration`, isolated db only)

40. **`TestActivity_Integration_SurfacesAnOpenTaskIntoIncoming`** — seed a project and a jira-keyed
    open `ready` human task in QUEUE, then run a real capture pass over an inbound message the rule
    routes to it. Assert `capture_decisions.action='task_log'`, one `task_append_log` audit row, one
    `task_mark_activity` audit row, `tasks.activity_at` set and `surfaced_at` still NULL; then
    `GET /tasks?project=…`: the row is in `section-incoming`, remark `new comment`, the title cell
    carries the sender, QUEUE no longer holds it. Then `POST /tasks/{id}/requeue` with `priority=""`
    → back in QUEUE, priority unchanged, one `reviewed` event, an `audit_events` row for
    `task_requeue` as `dashboard:…`.
41. **Column-fed (the "test the column, not the fixture" rule).** In the same test, after the first
    render: `UPDATE tasks SET reviewed_at = now()` → re-render → the row is in QUEUE; then
    `UPDATE tasks SET activity_at = now()` → re-render → back in INCOMING. Replacing the
    `needs_review` expression with `false` turns criterion 40 red; dropping the `nm` join turns the
    sender and remark assertions red. The fixture supplies neither.
42. **The ask half, end to end** (`internal/promote`): an armed inquiry project, a thread with an
    OPEN human task, a needs-reply verdict → a NEW `holding` task exists, its body ends
    `related_task: <old>`, the OLD task has exactly one new `log` event with the pointer text and an
    unchanged `activity_at`, and the board shows the new task in INCOMING (SWT-59's `from_message`
    fact, no change needed). Repeat with the old task assigned `claude`: still created, not gated.
43. **Requeue on a `holding` task** moves it to `ready` (status in the db, one `status_changed`
    event) and the board row moves from HOLDING to QUEUE; on a closed task the verb errors by name;
    as `mcp:treetop` it is denied `human_only` with a `policy_decisions` row.
44. **Existing suites stay green unchanged** — `board_layout_integration_test.go`,
    `board_incoming_integration_test.go`, `board_lights_…`, `board_session_…`, `board_refresh_…`
    (the tracer's statement count), `board_close_…`, `board_dismiss_…`, `board_reopen_…`,
    `dashboard_integration_test.go`, and every `internal/capture` suite. `internal/promote`'s
    inquiry suites get the named amendments of criteria 13–16 and nothing else; any suite asserting
    an exact audit-tool sequence gains `task_mark_activity` by name.

## Data model changes

Migration **0039** only (criterion 1). `capture_decisions`, `classify_promotions`, `task_dismissals`,
`ticket_status_syncs` and `external_refs` are read and written exactly as today. No new table
(invariant 2: INCOMING stays a filter over the one `tasks` table), and D11 adds rows to `tasks`, not
a "second task-like table".

## API / MCP tool changes

| Tool | Profile | Policy | Args → result |
|---|---|---|---|
| `task_mark_activity` (new) | **none** (spine only) | allow / static-default | `{task_id, message_id, reason}` → `{task_id, marked, skipped?}` |
| `task_requeue` (new) | full + user | `humanOnly` | `{task_id, priority?, note?}` → `{task_id, status, reviewed, priority:{from,to,changed}}` |

Both go through `executor.Execute` (validate → policy → audit start → handler → audit complete). No
existing tool's schema changes; `create_task` and `task_append_log` are called by D11 exactly as they
are called today. One new dashboard route, `POST /tasks/{id}/requeue`, one `executeTask` call.

## MQTT topics

None. No worker contract, heartbeat or command topic is touched.

## Files likely to touch

- `migrations/0039_task_activity_review.sql` (new)
- `internal/tools/activity.go` (new), `internal/tools/requeue.go` (new),
  `internal/tools/createtask.go` (registration table), `internal/tools/priority.go` (`applyPriority`
  extracted), `internal/tools/close.go` (`closeTransition`'s close update)
- `internal/policy/matrix.go` (`humanOnly` += `task_requeue`)
- `internal/capture/rules_store.go` (`markRuleActivity`, the `actionTaskLog` call, `RulesStats.Activity`)
- `internal/promote/promote.go` (`Decision.RelatedTaskID`, `Decide`'s inquiry branch),
  `internal/promote/store.go` (`act`'s create branch, the pointer log, `markVerdictActivity`,
  `decisionReason`, `Stats.Activity` / `Stats.Related`), `internal/promote/inquiry.go` (C-D13's
  narrowing, the body's `related_task` line)
- `internal/mcpserver/schemas.go`, `adapter.go` (`userProfileTools` + the profile doc), `serve.go`
- `skills/swb-status/SKILL.md`
- `internal/dashboard/board.go`, `lights.go`, `sections.go`, `display.go`, `server.go`,
  `templates/tasks.html`
- Tests: new `internal/tools/activity_test.go` + `_integration_test.go`,
  `internal/tools/requeue_test.go` + `_integration_test.go`,
  `internal/policy/matrix_requeue_test.go`, `internal/capture/rules_activity_integration_test.go`,
  `internal/promote/ownask_test.go` + `ownask_integration_test.go`,
  `internal/ticketstatus/activity_noregression_integration_test.go`,
  `internal/dashboard/sections_activity_test.go`, `board_activity_integration_test.go`;
  amended by name: `internal/dashboard/board_incoming_structure_test.go`, `sections_incoming_test.go`,
  `internal/promote/{promote_test,inquiry_test,inquiry_integration_test}.go`,
  `internal/orchestrator/rules_test.go`, `internal/mcpserver/{profile,serve,runbook,skill}_test.go`
- Docs: dated amendment lines in `docs/tickets/board-incoming-first_SPEC.md` (I5, criterion 5) and
  `docs/tickets/inquiry-promote_SPEC.md` (Q3 / C-D8 / C-D13); a new IK section (Verification Step 6);
  at deliver time `docs/runbooks/HANDOFF-kube-activity-resurfaces.md`

**Deliberately NOT touched:** `internal/ticketstatus/*`, `internal/orchestrator/*`,
`internal/classify/*`, `internal/replyfold/*`, every connector sink, `boardQuery`, `export.go`, every
other template.

## In scope / Out of scope

**In scope:** the three columns, the two tools, the capture and personal-lane attach hooks, the
close-time stamp, D11's create-instead-of-attach with its two-way pointer and C-D13 narrowing, the
board's third incoming kind with its order and remark, the Requeue verb and button, the tests, the
docs and the kube handoff.

**Out of scope, each named because it is a tempting bundle:**

- **Closed tasks.** Revive is SWT-45's; chat-on-closed-task is SWT-53's. A closed target is a skip
  here, by construction.
- **Widening what passes the inquiry gate.** `GateAnswered` and the other five stay exactly as they
  are (D11).
- **Changing capture's rules** so fewer client asks are claimed by a key match (D12). Rule data, and
  it needs Step 0f's numbers.
- **Suppressing his own Jira edits** (D10).
- **`parent_id` / `task_dependencies` between an ask and a ticket task**, and any second query to
  pick a "better" related task than `threadTask`'s.
- Surfacing SWT-45 revives into INCOMING; a per-row channel tag; message bodies on the board; a count
  of unreviewed items; an INCOMING count in the page title; email or MQTT notification.

## Invariants that apply

1. **Raw-first** — nothing is ingested. Every hook runs on an already-normalized message that already
   has a `raw_source_items` row; `task_mark_activity` reads `normalized_messages` by id only.
2. **One funnel** — no new table and no new status. "Needs review" is two columns on `tasks` plus a
   render-time filter; INCOMING stays a section. D11's asks are rows in the one `tasks` table.
3. **Everything through the executor** — every new write is an executor tool (validate → policy →
   audit). Capture and promote call them exactly as they call `task_append_log` and
   `task_mark_surfaced`; D11's pointer log is a `task_append_log` call, not SQL; the dashboard's
   Requeue is one `executeTask` with the task id on the call.
4. **Nothing external without a delivery row** — nothing is sent and no `deliveries` row is read or
   written. An ask becoming its own task creates work, not a message.
5. **Own-message loop closure** — `task_mark_activity` ERRORS on a non-inbound `message_id`, the guard
   `task_mark_surfaced` and `task_reopen` already carry, so one of our own sends re-entering through
   ingestion can never surface a task. The inquiry inbox stays inbound-only, and `GateAnswered` still
   reads our own outbound reply as the reason not to create anything.
6. **Stealth attribution** — nothing client-visible is produced. The remark, the sender and the
   pointer line are stored data or ids, never generated prose.
7. **Orchestrator purity** — D9: no rule reads the columns; `task_mark_activity` emits no event; the
   `reviewed` event hits `Evaluate`'s default and the `holding → ready` `status_changed` hits its
   `to ∈ {delivered, closed}` guard. `Decide` and `InquiryGate` stay pure; `boardSectionOf`,
   `incomingKind` and `activityRemark` are pure and structure-scanned.

## Sibling patterns to copy

- **The spine surfacing tool, verbatim shape:** `internal/tools/surfaced.go` and its tests
  (`surfaced_test.go`, `surfaced_integration_test.go`), including the off-both-profiles scan.
- **The attach-path hook and its ordering discipline:** `rules_store.go`'s `appendRuleLog` →
  `reviveRuleTask` sequence (log first, then the guarded call; a crash between them degrades to
  today's behaviour) and `promote/store.go`'s `act`.
- **A create plus a provenance call in one branch:** `act`'s existing `createVerdictTask` →
  `recordTask` → `setProvenance` chain — D11's pointer log is the fourth link, ordered last for the
  same reason (the claim is spent; a later failure must not lose the pointer to what was created).
- **The priority write:** `internal/tools/priority.go` `setPriority` — pointer argument, idempotent
  no-op, `priority_changed` event. **The status write:** `internal/tools/dependency.go:126-134`.
- **The humanOnly verb on both MCP profiles:** `task_set_priority` and `task_signal`
  (`matrix.go:96-118`, `adapter.go:68-93`, `schemas.go`), with the six-actor-shape policy test.
- **A display-only fact fed by the first statement:** SWT-57's `QueueRank` / `UpdatedStamp` and
  SWT-59's `FromMessage` / `PRReview`.
- **Fixtures and cleanup:** `internal/promote/reopen_integration_test.go` (message → verdict →
  promotion chain, FK-ordered cleanup); `layoutSections` / `lyRender` in
  `board_layout_integration_test.go`.
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used; nothing is claimed. **rag-svc HTMX:** not
  used; the board has none (pinned).

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| `task_mark_activity` writes `surfaced_at` instead of `activity_at` | criterion 26 (the reconciler holds instead of closing) |
| Drop the closed-task skip in `task_mark_activity` | criteria 4, 11 |
| Drop the non-inbound error | criterion 4 (invariant 5) |
| Remove the `prClose` exclusion in capture | criterion 12 |
| Key the capture hook on the channel or the rule kind | criterion 10 (the José case) |
| `Decide` returns `attached` for an inquiry verdict with an open thread task | criteria 13, 42 |
| The pointer log carries the message's title or sender text | criteria 14, 39 |
| The pointer log also marks activity on the old task | criterion 14 |
| Drop `related_task` from the body | criterion 15 |
| Keep C-D13 gating an OPEN claude thread task | criterion 16 |
| Loosen `GateAnswered` | criterion 17 |
| Replace the `needs_review` expression with `false` | criteria 40, 41 |
| Drop the `nm` PK join | criterion 40 (sender and remark) |
| Read `normalized_messages` by `thread_id` | criterion 38 |
| `lightFor` reads `NeedsReview` | criterion 38 |
| Sort incoming without the needs-review key, or by `id DESC` instead of the stamp | criterion 35 |
| `task_requeue` accepts a closed task / defaults a missing priority to 0 | criteria 21, 43 |
| `task_requeue` lifts a status other than `holding` | criterion 20 |
| Remove `task_requeue` from `humanOnly` | criteria 22, 43 |
| `closeTransition` stamps `reviewed_at` on the REOPEN update too | criterion 24 |

## Verification protocol

Run in order. Do not commit before step 4 passes. Capture the exit status of every `go test`
separately from any pipe (IK: *gate commits on test exit status*).

**0. Read-only prod pre-checks** (`psql -h 192.168.50.49 -U ops -d ops`, inside
`BEGIN READ ONLY; … ROLLBACK;`). NOT run by the spec session. Paste the results into the delivery
summary; assert none of them as a frozen literal in any test.

- **0a. What INCOMING gains, and the Anonymous share** (D10's gate):

  ```sql
  SELECT date_trunc('day', cd.created_at)::date AS d,
         (nm.sender ILIKE '%anonymous (jira)%')  AS own_edit_shape,
         t.status, count(*)
    FROM capture_decisions cd
    JOIN normalized_messages nm ON nm.id = cd.message_id
    JOIN tasks t               ON t.id  = cd.task_id
   WHERE cd.mode = 'live' AND cd.action = 'task_log'
     AND cd.created_at > now() - interval '14 days'
   GROUP BY 1,2,3 ORDER BY 1,2,3;
  ```

  **Gate:** if attaches onto NOT-closed tasks average more than ~25/day, or the `own_edit_shape`
  share of those is over half, STOP and decide the D10 suppression first. Otherwise record the daily
  figure — it is how many Requeue taps a day he is signing up for.
  **Measured 2026-09-22 (14 days, prod):** attaches onto NOT-closed tasks per day — 79 (Sep 9, the
  live seed), 6, 6, 4, 2, 1, 32 (Sep 18), 1, 10, 22 (Sep 22 to 10:30) — typical days under 10, busy
  days 20–30; `Anonymous (JIRA)` share 4 of ~160 (2 on Sep 9, 2 on Sep 21). **Gate passed.** Senders
  behind them: Jira mirrors 39, Mario Cruz 38 (Upwork), Salvador's own GitHub notifications 25, Katie
  17 (+13 via Jira), José 9 (+4 direct, +3 via Jira), Lyle 3. The 25 self-GitHub rows are PR-review
  mail on his own PRs — a candidate for D10's suppression list, not this ticket.
- **0b. What D11 changes:** `classify_promotions` rows with `action='attached'` in 30 days, split by
  lane actor (`audit_events.actor` or the verdict's lane) and by the target task's status. Each
  inquiry-lane open-task row is a task that will now exist instead. Expected order of magnitude:
  José's four plus Katie's two per busy day.
  **Measured:** 30 days of `attached`: 12, all inquiry lane (7 onto closed tasks, 5 onto ready). So
  D11 creates about 5 tasks a month that were being buried — plus the ones the replied-since fold
  correctly drops.
- **0c. The sender shapes he will read:** distinct `nm.sender` behind 0a's not-closed rows, with
  counts. Expected `"<Name> (JIRA)"`, `"Anonymous (JIRA)"`, and real people (José). An empty sender
  renders no muted span — acceptable; record the count.
- **0d. The J11 population at risk under the rejected design** (why D1 went this way): open tasks
  with a jira ref whose ticket is currently not warranted. Not a gate; a number for the IK entry.
- **0e. `EXPLAIN (ANALYZE, BUFFERS)`** of the new first statement with a realistic id array. Expected:
  the `nm` join is an index scan on `normalized_messages_pkey`, no sequential scan of
  `normalized_messages`, total time of today's order (prod 2026-09-15: 0.60 ms). Record both timings.
- **0f. D12's frequency:** over 30 days, inbound Slack/gmail messages from the inquiry-armed
  project's counterparties whose latest LIVE capture decision is `task_log` onto an OPEN task — the
  asks a key-matching rule swallows before the inquiry lane can see them. Not a gate; it decides
  whether a rule change is the next ticket.

**1. Unit:** `go test ./...`. The SWT-48 `TestAttributionTrend_*` flake (20:00–24:00 EDT) is
pre-existing; re-run with `TZ=UTC` if it fires.

**2. Integration, on an ISOLATED database** (never prod, never the shared compose `ops`):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_activity"
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_activity?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_activity?sslmode=disable' \
  go test -tags integration -p 1 -count=1 ./internal/tools/ ./internal/capture/ ./internal/promote/ \
                                        ./internal/ticketstatus/ ./internal/dashboard/
```

Run the dashboard and promote packages twice (rerunnable cleanup), then
`go test -tags integration -p 1 ./...` once against the same URL.

**3. Mutations:** every row of the table above goes red, then is reverted.

**4. Local smoke, in a real browser** (IK: verify UI work in Chrome/Playwright, `channel="chrome"`;
`--screenshot` alone has hidden two layout landmines).

- `DATABASE_URL=<ops_activity url> go run ./cmd/dashboard` (:8085, `/dev/login?user=salvo`).
- Seed through the executor (`opsctl call --tool task_mark_activity …`), never raw SQL: a jira-keyed
  `ready` human task with activity from `Katie Evans (JIRA)`, a second with
  `José Garcia <jose.g@avviato.com>` on a gmail message, a third on a Slack message, one `holding`
  inquiry task with activity, one SWT-59 promoter task with none, one PR review task, ~15 plain ready
  tasks.
- At 1000×700: INCOMING leads the left pane; the needs-review rows lead the section with
  `new comment` / `new email` / `new slack` and their senders; the SWT-59 promoter row follows; QUEUE
  no longer holds them. `actions` → Requeue with `priority: unchanged` → the flash shows once, the
  row drops to QUEUE, the `holding` one lands in **QUEUE as ready**, `refresh=on` survives, and a
  second tap is a clean no-op. Then Requeue with `3 urgent` and check the ▲ mark.
- View source: one `<script`, no `<details … open`, no occurrence of `incoming`.
- From a Claude Code session against `ops-mcp-user` built from this branch: `swb requeue <id>`.
- Drop the database afterwards.

**5. Deploy — migration FIRST, then ONE image tag to every workload that runs this code.**

1. Apply 0039 (kube one-shot migrate Job). **Barrier:** the new dashboard selects the three columns
   on every `/tasks` render and the new capture/promote binaries call a tool that writes them, so a
   new image on a pre-0039 db fails every render and every capture pass (the 0030/0033/0034
   precedent). Old images are unaffected by 0039.
2. Roll one tag to every capture writer (the connector CronJobs), the classify/promote workloads,
   `orchestratord` and the dashboard, in one apply. A mixed fleet is harmless for the marking (an old
   binary marks nothing) but NOT for D11: an old promote binary still attaches asks, so do not leave
   the promote workload behind.
3. `go install ./cmd/ops-mcp-user` and `./cmd/opsctl` here **and on 192.168.50.30** (currently
   offline — record it pending and re-run; IK: *second workstation .30 install*), then
   `make install-skill`. A stale `ops-mcp-user` does not list `task_requeue` at all and silently
   drops unknown args (IK: *nothing rejects unknown tool args*). Restart open sessions.
4. The kube session owns `kube/switchboard/*.yaml`: hand over
   `docs/runbooks/HANDOFF-kube-activity-resurfaces.md`. This session never edits manifests.
5. **Post-roll smoke:** `/tasks?refresh=on` on the tablet after the next capture and promote ticks —
   INCOMING shows the rows Step 0a predicted plus any new ask as its own row, and `pg_stat_activity`
   shows no statement shape beyond the widened first statement.

**6. IK entry** (`## Activity resurfaces an open task, and an ask is always its own task`): why
`surfaced_at` was NOT reused (D1, with 0d's number); the two tools and their actors; the closed-task
skip that keeps SWT-45/SWT-53 out; D11 with his words, the two-way pointer, C-D13's narrowing and the
"a thread may now hold several open tasks" consequence; D12's boundary (capture decides first, by
`InquiryEligibleLatestSQL`); the four board fields and the narrowed `normalized_messages` ban;
`humanOnly` = interactive sessions, not workers; and D10's residual with its measured share.

**7. Rollback:** roll the images back. The columns stay (forward-only); an old binary ignores them, so
the board stops showing activity rows and the promoter attaches asks again. Nothing is lost.

## Future work (not this ticket)

- **Suppress his own Jira edits** (D10): key the mark on capture's own-action window, or a
  per-project notifier-sender list in SWT-53's equality spelling. Needs 0a.
- **Narrow the rules that swallow asks** (D12), on 0f's numbers.
- **Mark activity on the SWT-45 revive and SWT-36 reopen paths**, so a resurrected task explains
  itself in INCOMING instead of appearing in QUEUE.
- **A count of unreviewed items** since `reviewed_at`, and the message's first line on the task detail
  page rather than the board.
- **Notification** (email or the `ops/…` MQTT tree) when something surfaces while he is away.
- **A label on Requeue** — "this activity did not need me" is training data nothing records today.
- **An explicit ask↔ticket link object** if the body line and the pointer log prove too weak for the
  sessions working the asks.
