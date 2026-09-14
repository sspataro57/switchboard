> Jira: SWT-53

# chat-on-closed-task — a human message that capture logs onto a CLOSED task resurfaces through the inquiry lane; notifier traffic keeps logging silently

**Status: decided (2026-09-14).** Q1 is answered with its recommended default (`chat-on-closed-task_OPEN_QUESTIONS.md`): an
Avviato DM that names a WEB/API/OPS key follows rule 10's attribution and resurfaces in collaboratory.
**Migration number:** this ticket owns **0034**. SWT-54 (treetop-pr-review-tasks), specced in parallel,
takes 0035.

## Source

Ad-hoc, from Salvador, 2026-09-14: "open the first ticket". It is the first of two gaps found while
investigating his report "incoming chats on slack are not entering as tasks". The second gap, GitHub PR
tasks, is a separate ticket (Out of scope).

The gap as reported:
- A captured message (Slack chat or mail) that mentions a Jira key is attributed by capture rule 10
  (`body_regex` `(WEB|API|OPS)-[0-9]+`, collaboratory) to that key's existing task and logged there as
  `task_log`. This happens **even when that task is closed**.
- A plain-closed task is never reopened by activity. Only an OPEN dismissal reopens (SWT-36), and SWT-45
  revive applies only to rules with `revive=true`.
- So the message disappears: there is no board row and no inquiry check. The inquiry lane (SWT-40 Part C)
  reads only messages whose latest decision is `attributed`.

Prod evidence supplied with the ticket (read-only, last 7 days), inbound Slack `task_log` onto CLOSED
tasks:
- 42 are from the "Jira" Slack app. These are bot notifications, and logging them is fine.
- 4 are from people:
  - asunda45, 09-08, DM `D0AUD86LKGA` → task 57;
  - Jose Garcia, 09-09 and 09-10, Avviato DM `DSAV4HJ2F` → tasks 57 and 56;
  - byeluri, 09-10, DM `D0B6FV6HFSR` → task 60 "OPS — Jira".

Goal from the owner: a human message that lands on a closed task must not disappear; it should
resurface. Bot and notification traffic (the Jira Slack app, GitHub/Jira notification mail) keeps
logging silently.

## Goal

When capture logs an inbound message onto a task that is already closed, and the message is not a
notifier's and not SWT-45/SWT-36 territory, capture records that fact on its decision row
(`capture_decisions.resurface`). Both inquiry-lane inboxes then accept that message exactly like an
`attributed` one. The model judges needs-reply, Part C's deterministic gate applies unchanged, and a
passing ask becomes a Holding task **on the message's own conversation**. The closed ticket task stays
closed.

**Usable alone means:** once 0034 is applied, the new images are rolled and the collaboratory notifier
list is seeded by one UPDATE, the following holds.
- The next human Slack DM or mail that names a WEB/API/OPS key and lands on a closed task still gets its
  log line on that task, as today.
- It also appears in collaboratory's Holding column as `{asker}: {ask}`, with a body line naming the
  closed task. That happens once the 1h grace has passed, if the model says needs-reply and he has not
  answered.
- A Jira-app DM about a closed ticket logs exactly as today and produces nothing else.
- Every project that is not inquiry-armed behaves byte-identically to today.

## What the investigation established

### Code facts (verified in this worktree, main 62046e5)

- **K1: where the log happens.**
  - `decideMessage` (`internal/capture/rules_store.go:664`) turns a ref hit into `action=task_log`
    whatever the task's status. `taskForExternalRef` (`:1025`) already returns `t.status` (SWT-45
    criterion 19) and the open dismissal.
  - The live pass then calls `appendRuleLog` (`:1208`). It revives only when `decision.revive` is set,
    and reopens only when `decision.dismissalID != 0`.
  - A plain-closed task gets the log and nothing else: `TestCaptureReopen_Integration_APlainClosedTaskOnlyLogs`.
- **K2: why the inquiry lane never sees it.** Both inquiry inboxes require the latest decision to be
  `attributed`:
  - `classify.inboxWhereInquiry` (`internal/classify/store.go:133`) says `task`/`task_log` "already
    produced a task";
  - `promote.inquiryInbox` (`internal/promote/inquiry.go:355`) joins `latest ON latest.action =
    'attributed'`.

  That premise is false when the task was already closed.
- **K3: rule 10 keys by PREFIX.** It has no `key_regex`, so `externalKey` reuses the pattern and
  `extractKey` returns the first group, `WEB`/`API`/`OPS` (`internal/capture/rules.go:275-300`, SWT-45
  F1). Every rule-10 match therefore logs onto one of the three bucket tasks (56/57/60, refs keyed `API`,
  `WEB`, `OPS`), never onto a per-ticket task.
  - Reopening the logged-onto task would resurrect a catch-all. Task 56 carries 256 logs.
  - SWT-45 J1 made rule 10 unrevivable by CHECK for this reason.
- **K4: the rule-10 false-positive shape.** The pattern is case-sensitive and has no word boundary. Text
  like "Jira tickets" alone CANNOT match, because it needs the literal `OPS-` followed by a digit.
  Anything containing that substring does match: a genuine `OPS-123`, `DEVOPS-2`, or a URL fragment.
  - Task 60's title "OPS — Jira" is not evidence of a false match. `ruleTaskTitle` is `{key} — {subject}`,
    and a Slack message's subject is the conversation name, so task 60 was created from a message in the
    Jira-app DM (conversation "Jira") with key `OPS`.
  - byeluri's message contains an `OPS-<digit>` substring somewhere. Verification 0c identifies it.
- **K5: the inquiry lane already carries keyless notifier traffic.** The Treetop workspace catch-all
  attributes keyless Slack messages (Jira-app DMs included) to collaboratory as `attributed`. So they
  already reach `classify_inquiry` and `InquiryGate` today. The prompt (`InquirySystemPrompt`) tells the
  model that "an automated notification … a ticket transition, a bot message" is `needs_reply=false`.
  This ticket only adds the keyed siblings, plus a deterministic notifier exclusion on top.
- **K6: what is stored about a sender.**
  - `normalized_messages.sender` is Slack's display name (`message.Author`, `slackweb/normalize.go:88`)
    or gmail's raw From header. The Slack author id (`SenderID`) is NOT stored: `slackweb/sink.go:241`
    omits it, so it exists only in `raw_json`.
  - `people`/`person_identities` are not linked to messages.
  - So the only deterministic, normalized identity of a notifier is the `sender` column.
  - `capture/senderdomain.go` is the one Go-side From parser (`net/mail`). The repo rule is that a
    format Go owns is never taken apart in SQL.
- **K7: Part C's gate and Decide are reusable as they are.**
  - `InquiryGate` (rethreaded, kind, stale 72h, pending 1h, answered, not_addressed, claude_task) is a
    pure function of stored facts.
  - `Decide` attaches to the thread's open task, reopens a dismissed thread task (SWT-36), or creates
    `inquiryCreateStatus` ("holding").
  - `threadTask` keys on `tasks.source_thread_id` and the project, never on `external_refs`, so the
    closed ref task is not the thread's task unless it was created from that conversation. If it was,
    it is "finished", and Q3's fall-through creates a new task.
- **K8: the wake-up exists.** Capture publishes `ops/pipeline/captured` when a pass considered at least
  one message (`internal/pipeline/contract.go`, SWT-45 F11). The pipelined `inquiry` stage is woken by
  `captured`. No MQTT change is needed.
- **K9: arming.**
  - Only collaboratory has `ai_inquiry` (IK "Inquiry lane (SWT-33)"), and `inquiry_promote_after` (0031)
    arms promotion per project.
  - Rule 10 attributes to collaboratory whatever the workspace, so an Avviato DM naming a Treetop-shaped
    key is attributed to collaboratory. Keyless Avviato DMs are attributed elsewhere and are not
    inquiry-checked (Q1).
- **K10: three writers of `capture_decisions`.** `insertDecision` (live/shadow, `rules_store.go:1076`),
  the gate stage (`gate.go:589`) and the route stage (`route.go:402`). Only the first can produce this
  ticket's case at a project that is inquiry-armed today (below: gate-path residual).
- **K11: the last migration is 0033** (SWT-52). The ledger lives in `internal/classify/structure_test.go:1200`.

## Decisions made unilaterally (with rationale)

Numbered **CC1…** so they never collide with SWT-45's J, SWT-40's C-D/D-D/B-D, or SWT-36's D.

**CC1: Option (a), through the inquiry lane. (b) and (c) are rejected.**

| option | verdict | why |
|---|---|---|
| (a) human message → inquiry lane → Holding task if needs-reply | **chosen** | A closed ticket task plus a new chat is a *conversation* question ("is someone waiting on me?"), which Part C already answers with a model verdict and a pure gate: answered, grace, stale, addressed, claude_task. It adds no new executor path, no new tool and no new autonomy (O7 Holding). Dismissals label it for free (C-D12). |
| (b) SWT-45 revive, humans revive and bots don't | rejected | Rule 10 keys by prefix (K3), so a revive resurrects bucket tasks. J1's CHECK forbids it, and F8 means rule 10 cannot gain the flag anyway. A revive also surfaces against the reconciler for *any* human word ("ok thanks"). Mention-revive is J16's, deferred to capture-rule-ticket-keys with its load check (about 46 new tasks a week). |
| (c) always reopen on a human message | rejected | It has every fault of (b) and no reply judgement. Capture's reopen is not a human actor, so it does not surface (J8), and the reconciler re-closes a done ticket's task within 15 minutes: the SWT-45 bounce, per message. |

**CC2: the resolution is a Holding task on the message's conversation, never a reopen of the closed task.**
- The owner offered both ("the task reopens or a holding task is created").
- Every case that reaches this path today is a bucket task (K3), or a thread-keyed task whose rule is not
  an activity rule. Reopening a bucket is wrong by construction.
- The conversation is the unit Part C already reasons about (Q3: one open task per thread). A second ask
  in the same DM attaches to the Holding task, and a dismissed thread task follows SWT-36. All of that is
  `Decide`, unchanged.
- The closed task keeps its log line (today's behaviour). The new task's body names it, so the human sees
  the link.

**CC3: capture decides "resurface" deterministically and RECORDS it. The lanes only read it.**
- A new `capture_decisions.resurface BOOLEAN NOT NULL DEFAULT false` is written by `insertDecision` from
  a pure function, `resurfaces(in resurfaceInput) (bool, string)` in a new `internal/capture/resurface.go`
  (the `revive.go`/`overrides` precedent). It is true iff all of these hold:
  - the action is `task_log`, and the linked task's status (from `taskForExternalRef`) is `closed`;
  - the winner is not an activity match (`activity`, as computed in `decideMessage`). Revive, own_action
    and blind outcomes stay SWT-45's;
  - the task has no open dismissal (SWT-36's guarded reopen owns it);
  - it is not the Jira connector's own copy (`pm.channel == jira.Channel`, computed in `rules_store.go`
    and passed in as a bool so the pure file imports no connector). This is SWT-45 J3: the poller reads
    whole projects, and the email copy is the only Treetop trigger;
  - the sender is not on the winner project's notifier list (CC4);
  - the sender is not blank (CC4b, review fix 2026-09-14): a message with no sender identity fails closed.
- **Why record it rather than recompute it in SQL:**
  - Status-at-log-time is a fact that only capture sees. A later close of an OPEN task must not make
    already-seen logs look like "logged onto a closed task" (a clock comparison would need SWT-45's
    `closed_at`/`updated_at` fallback, which the IK says is spelled once).
  - The From parse must stay in Go (K6).
  - The reason text says why, per message. This is the SWT-36 D7 / SWT-45 J9 precedent: the action stays
    `task_log`, and the typed outcome is a column.
- Shadow records the same flag; the decision is mode-free. Shadow still calls nothing.

**CC4: "notifier vs human" is data: `projects.notifier_senders TEXT[] NOT NULL DEFAULT '{}'`.**
- It is per project, hand-set, and shaped like `ticket_delivered_statuses` (0025): a workflow-shaped fact
  lives in configuration, `DEFAULT '{}'` is today's behaviour, and NOT NULL because of 0018's
  nullable trap.
- It is read ONLY by capture's `loadRules` (joined with the rules, like `ticket_assignee_gate`), into
  `storedRule.notifiers`.
- **Match rule** (`notifierSender(sender, list)`, pure). An entry matches when, case-folded and trimmed,
  it EQUALS either:
  - the whole stored sender, which is Slack's display name, e.g. `Jira`; or
  - the address parsed from the sender, e.g. `jira@treetopllc.jira.com` inside
    `"Katie Evans (JIRA)" <jira@treetopllc.jira.com>`.

  The address is parsed by a new `senderAddress` helper beside `senderDomain` (one `net/mail` spelling;
  `senderDomain` is refactored onto it with its tests unchanged).
- **Equality, never substring.** The `sender` capture criterion is a substring match. Reusing that here
  would let an entry `Jira` swallow a human whose name merely contains it, and a swallowed human message
  is the exact failure this ticket fixes.
- **Not a rule flag.** Rule 10 matches humans and bots alike, so a per-rule flag cannot separate them.
  F8 also means no existing rule can gain a flag.
- **Not a Go literal.** A literal "Jira" would be a per-sender rule in code.
- **Not the Slack author id.** It is not a normalized column (K6). Reading `raw_json` from capture would
  be a second raw reader. See Future work.
- The seed values are data chosen from Verification 0b, not frozen here.
- **Owner decision 2026-09-14: keep GitHub notification mail silent.** `notifications@github.com` stays in
  the proposed collaboratory seed, although it also carries human PR comments (173 in 30 days onto buckets
  56/57). GitHub PR tasks are SWT-54's. The runbook and the handoff carry the identical seed statement.

**CC4b: a blank sender FAILS CLOSED (review fix, 2026-09-14).**
- `notifierSender` is an equality match, so no list entry can ever equal an empty sender. Without its own
  disqualifier, a message with no sender identity would resurface into the inquiry model by default.
- `resurfaces()` therefore has a sixth disqualifier: `blankSender`, true when the stored sender is empty or
  whitespace (`blankSender(sender)`, pure, in `resurface.go`). Its reason is distinct: `not resurfaced:
  the message has no sender identity (blank sender), so it is logged silently`. The log line is still
  appended.
- Risk was low, fixed anyway. Since the Slack connector fix at 17:00Z on 2026-09-14, 0 new inbound Slack
  messages have had an empty sender (prod, read-only: 7 new messages, all named). The 741 historical
  blank-sender rows in 0a were decided before 0034, carry `resurface=false`, and are never re-decided.

**CC5: both inquiry inboxes widen by ONE shared predicate.**
- `internal/replyfold` gains an exported SQL constant, `InquiryEligibleLatestSQL`. replyfold is already
  the stdlib-only SQL leaf that classify and promote share for exactly these inboxes (`JoinSQL`), and a
  constant needs no import (C6 holds). The constant is:

  ```
  (latest.action = 'attributed'
   OR (live.action = 'task_log' AND live.resurface
       AND EXISTS (SELECT 1 FROM tasks lt WHERE lt.id = live.task_id AND lt.status = 'closed')))
  ```

- `latest` is each inbox's own LATERAL, unchanged since before this ticket (`cd.action, cd.project_id`,
  newest row in ANY mode, C2). `live` is a second shared constant, `InquiryLiveDecisionJoinSQL`: a
  `LEFT JOIN LATERAL` over `capture_decisions` restricted to `mode = 'live'`, `ORDER BY id DESC LIMIT 1`
  (CC5b). The project join is `InquiryProjectIDSQL` (`latest.project_id` for an attributed admission,
  else `live.project_id`), and promote's `LoggedOnTaskID` is `InquiryLoggedOnTaskSQL`.
- **"Still closed" is re-read at both stages.** If something reopened the task in the meantime (a human,
  a revive, SWT-36), the log line is back on the board and the message leaves both inboxes.
- `inboxWhereInquiry` keeps `p.ai_inquiry` and its deliberate absence of an `ai_locality` clause.
  `promote.inquiryInbox` keeps `ai_inquiry`, `inquiry_promote_after`, the verdict clock, the 72h fence and
  the `classify_promotions` NOT EXISTS.

**CC5b: the resurface branch reads only LIVE decisions (review fix, 2026-09-14).**
- The hole: as first written, the predicate read the resurface fact from the latest decision in ANY mode.
  A shadow pass (or a `--all` re-point) that writes `task_log` + `resurface=true` would make both inboxes
  admit the message, and promote could then create a live Holding task from a what-if row. A regular
  shadow pass is enough: it re-decides every message in its window that has no shadow row yet, and a
  message whose live decision was `task` now finds its ref and becomes a `task_log`, which resurfaces if
  the task has since closed. Symmetrically, a newer shadow row would have REMOVED a live resurfaced
  message.
- The fix: the resurface branch reads only the message's latest LIVE decision (`InquiryLiveDecisionJoinSQL`,
  alias `live`), and a resurfaced message takes its project and `LoggedOnTaskID` from that row. A shadow
  row never adds a message through the resurface branch, and a newer shadow row of any action other than
  `attributed` never removes one. **Accepted corner (owner-session decision 2026-09-14):** a NEWER shadow
  `attributed` row takes precedence under the Part A re-point contract. The message is then admitted by
  the attributed branch under the shadow row's project, with `LoggedOnTaskID = 0` and no
  `logged_on_closed_task` line, or dropped if that project is not armed (T19, pinned by
  `TestPromoteInquiryResurface_Integration_ANewerShadowAttributedRowTakesPrecedence`). An EXISTS over the
  live row alone would not do: the
  project join would still read `latest.project_id`, which a newer shadow `unmatched` row sets to NULL.
- **Finding, NOT changed: the existing attributed branch has the analogous exposure.** It admits a latest
  `attributed` row in any mode, shadow included. A shadow pass that writes a newer `attributed` over a live
  `unmatched` (after a rule change, or a `--all` re-point) makes the message eligible, and promote can
  create a Holding task from it. That is the documented re-point contract, not an accident: the runbook's
  "Re-pointing already-decided messages" says every latest-decision reader follows the newest row in any
  mode, so attribution moves (Part A), and C2 pins the any-mode read for gate and route rows. It is
  intended and out of scope here. The attributed branch is byte-identical, and
  `TestPromoteInquiryInbox_LatestDecisionHasNoModePredicate` passes unmodified.

**CC6: promote changes only what it copies.**
- `Verdict` gains `LoggedOnTaskID int64`, which is `latest.task_id` when the latest action is
  `task_log`, and 0 otherwise.
- `inquiryBody` appends one line, `logged_on_closed_task: N`, LAST and only when non-zero. Every existing
  body is byte-identical, and C8's exact-text tests pass unmodified.
- `decisionReason` gains one part: `capture logged the message onto closed task N; resurfaced
  (chat-on-closed-task)`.
- `InquiryGate`, `Decide`, `threadTask`, `inquiryCreateStatus` and the actor are untouched.

**CC7: the counter.**
- `RulesStats.Resurfaced` counts decisions written with `resurface=true`, in both modes (the `Deferred`/
  `Blind` precedent: a recorded fact, not an action).
- Every `capture_rules:` counter line prints `"resurfaced"`, zeros included. The `capture_gate:` line in
  `cmd/opsctl/gate.go` prints it as constant 0.
- SWT-45 criterion 28's structural scan is extended to require it.

**CC8: the gate and route writers stay `resurface=false`. This is a recorded residual.**
- The gate path's `task_log` onto a closed task only logs, as SWT-45's Part D section left it (a gate-path
  revive is also future work).
- Gated projects (reengine) are not inquiry-armed, so today nothing is lost.
- Route rows are `attributed` by CHECK and already reach the lane.

**CC9: no backfill.** The four human messages were decided before 0034, so their rows carry
`resurface=false`, and they are past the 72h fence anyway. A shadow `--all` re-point cannot re-decide
them into the resurface branch: that branch reads only live decisions (CC5b). There is no backfill tool.

**CC10: deploy order is a landmine and is written into the handoff.**
- New capture binaries select `p.notifier_senders` and write `resurface`, so **0034 must be applied
  BEFORE any image built from this branch**. Otherwise every capture pass fails and stalls every
  connector. This is the 0029/0030 precedent.
- Seed the notifier list after 0034 and BEFORE rolling the images. Old binaries ignore the column, so the
  first new pass already excludes the Jira app.
- **The mixed-version window loses resurfacing (review fix, 2026-09-14).** An old capture binary on a 0034
  db writes the default `resurface=false`. The live decision is unique per message and never re-decided,
  and a shadow re-point cannot recover it (CC5b). So roll ONE tag to ALL capture writers (every connector
  CronJob, google's watch loop) and the inquiry readers (pipelined, classify-*) in ONE apply, right after
  0034 and the seed. Any hand-run `opsctl` capture pass must be rebuilt from main first.
- **Recorded residual:** messages captured by an old writer during the minutes-long roll window are logged
  but never resurfaced.

## Traces

| # | scenario | outcome |
|---|---|---|
| T1 | asunda45 DMs "can you check WEB-10355?"; rule 10 → bucket 57, closed | log on 57; `resurface=true`; the inquiry stage classifies; after 1h, if needs-reply and unanswered → Holding task on the DM thread, body `logged_on_closed_task: 57`; 57 stays closed |
| T2 | the Jira Slack app DMs "OPS-123 moved to Done"; `Jira` ∈ collaboratory notifiers | log on 60; `resurface=false`, reason names the notifier list; nothing else |
| T3 | same app, list not yet seeded | `resurface=true`; the model sees a bot message (K5: prompt says false); at worst a Holding task, dismissed `not_actionable` → a labelled false positive. CC10 seeds first to avoid it |
| T4 | the human replies in the DM before the grace ends | gate `answered` (or `pending` then `answered`); nothing |
| T5 | second ask in the same DM while the T1 Holding task is open | `Decide` attaches (log on the Holding task); no second task |
| T6 | message logged onto an OPEN task, the task closed later | `resurface=false` (status was not closed at log time); never resurfaces |
| T7 | resurfaced, then a human reopens task 57 before classification | leaves both inboxes (task not closed); the log is on an open task |
| T8 | Treetop Jira mail on a closed per-ticket task, rule J2 (`revive`) | SWT-45 path unchanged (revive / own_action); `resurface=false` |
| T9 | Jira connector comment (rules 3–5) on a closed task | `resurface=false` (J3); unchanged |
| T10 | closed task with an open dismissal | SWT-36 guarded reopen unchanged; `resurface=false` |
| T11 | `delivered` task | log only (J14 precedent); `resurface=false` |
| T12 | Upwork client chat on a closed room task (upwork project not inquiry-armed) | `resurface=true` is recorded; neither inbox admits it (`ai_inquiry` false); byte-identical board. Arming later makes it work with no code change |
| T13 | Jose Garcia, Avviato DM naming `API-…`, rule 10 → collaboratory bucket 56 closed | per Q1's default: resurfaces under collaboratory like T1 |
| T14 | the thread's existing task is `assignee_type=claude` | gate `claude_task` (C-D13) unchanged |
| T15 | his own Slack message naming a key | outbound (`AuthorID == OwnUserID`), never captured; unchanged |
| T16 | a Slack message with an empty or whitespace sender naming a key, onto a closed bucket | log on the bucket; `resurface=false`, reason `no sender identity` (CC4b, fail closed) |
| T17 | live `task_log` `resurface=false`, then a newer shadow `task_log` `resurface=true` | neither inbox admits it; no Holding task (CC5b) |
| T18 | live `task_log` `resurface=true` onto a closed task, then a newer shadow row of any action EXCEPT `attributed` (e.g. `unmatched`, `task_log` `resurface=false`) | both inboxes still admit it, under the live row's project; the body names the closed task (CC5b) |
| T19 | live `task_log` `resurface=true` onto a closed task, then a newer shadow `attributed` row | ACCEPTED (owner-session decision 2026-09-14, Part A re-point contract): the shadow row takes precedence. Admitted by the attributed branch under the shadow row's project, `LoggedOnTaskID = 0`, no `logged_on_closed_task` line; dropped if that project is not armed |

## Acceptance criteria

### Data model

1. `migrations/0034_chat_on_closed_task.sql` is this ticket's only migration. A guard test asserts
   exactly one `0034_*.sql`. The ledger (`internal/classify/structure_test.go:1200`) accepts 34 with the
   ownership note "34 is chat-on-closed-task", and still fails any unowned number.
2. It adds `projects.notifier_senders TEXT[] NOT NULL DEFAULT '{}'` and
   `capture_decisions.resurface BOOLEAN NOT NULL DEFAULT false`, with the CHECK
   `capture_decisions_resurface_is_task_log (NOT resurface OR action = 'task_log')`. It has no index, no
   backfill, no `UPDATE projects` and no `UPDATE capture_decisions`. The comment names CC3, CC4 and CC10.
   Integration: inserting `resurface=true` with action `attributed` fails. Mutation: drop the CHECK →
   red.

### Capture

3. `resurfaces` is pure: `resurface.go` is scanned for I/O tokens by the `rules_structure_test.go` shape.
   Table test over `{status: closed|ready|delivered|in_progress} × activity × dismissed × connectorCopy
   × notifier × blankSender`: true only for closed ∧ ¬activity ∧ ¬dismissed ∧ ¬connectorCopy ∧ ¬notifier ∧
   ¬blankSender. Each false row returns a distinct reason fragment. CC4b adds an integration case: an empty
   and a whitespace sender with human-looking bodies on a closed bucket write `resurface=false` with the
   `no sender identity` reason, beside a named control that resurfaces. Mutation: drop the `blankSender`
   case → red.
4. `notifierSender` table test:
   - `Jira` matches `Jira`, ` jira `, `JIRA`;
   - `Jira` does NOT match `Jiraiya Tanaka` or `"Katie Evans (JIRA)" <jira@x>`;
   - `jira@treetopllc.jira.com` matches `"Katie Evans (JIRA)" <jira@treetopllc.jira.com>` and
     `JIRA@TreetopLLC.jira.com`;
   - an empty or whitespace entry matches nothing, and an empty list matches nothing.

   `senderAddress` has its own table, and `senderdomain_test.go` passes unmodified.
5. **Column-fed, both directions** (the "test the column" rule). There are two integration cases:
   - A project seeded `notifier_senders = '{Jira}'` through the column, a rule-10-shaped rule, a closed
     task with a ref, and an inbound slack message from `Jira` → `task_log`, `resurface=false`, and the
     reason names the list. Mutation: select `'{}'::text[]` instead of `p.notifier_senders` in
     `loadRules` → red.
   - The same fixture with sender `asunda45` → `resurface=true`. Mutation: a literal `false` for
     `d.resurface` in `insertDecision` → red.
6. `resurface=false` for each of these, with the log line still appended:
   - a `ready`, an `in_progress` and a `delivered` task;
   - a closed task with an open dismissal (SWT-36's reopen still called; `rules_reopen_integration_test.go`
     unmodified);
   - a reviving rule on a closed task (every `rules_revive_integration_test.go` and
     `ownaction_integration_test.go` case unmodified);
   - a `jira`-channel message (rule-4-shaped).
7. Shadow writes the same `resurface` value and makes no executor call.
   `TestCaptureReopen_Integration_APlainClosedTaskOnlyLogs` passes unmodified: `resurface` is a column,
   not an action.
8. The gate path writes `resurface=false` (default) for a gate `task_log` on a closed task (CC8), asserted
   in `gate_scope_integration_test.go`'s shape.
9. `RulesStats.Resurfaced` is counted in both modes. Every capture counter line prints `"resurfaced"`:
   `cmd/connectors/{jira,slackweb,upworkcrm,google}/main.go`, `cmd/connectors/google/watch.go`, and
   `cmd/opsctl/main.go`'s `capture-rules run`. `cmd/opsctl/gate.go` prints it as constant 0. The
   structural counter scan fails a main without it.
10. `projects.notifier_senders` is read by no non-test Go file except `internal/capture/rules_store.go`,
    excluding migrations and docs. A structure scan enforces this (the SWT-34 criterion 22 precedent).

### The lanes

11. `replyfold.InquiryEligibleLatestSQL` is the ONE spelling of the widened predicate. A structure scan
    fails any literal in `internal/classify` or `internal/promote` that names `latest.resurface` outside
    it. `replyfold` still imports only stdlib and slackweb (C6).
12. **Classify inbox, integration.**
    - `inboxWhereInquiry` admits a `task_log` + `resurface=true` message whose task is still closed, in an
      `ai_inquiry` project.
    - It excludes, one fixture per clause:
      - `resurface=false`;
      - `resurface=true` whose task has since been reopened;
      - the same in a non-`ai_inquiry` project;
      - a `task_log` onto an open task.
    - Mutation: drop the `EXISTS … status = 'closed'` → the reopened fixture goes red.
    - The existing `inquiry_structure_test.go` pins (`p.ai_inquiry`, no `ai_locality`, the comment) hold.
13. **Promote inbox, integration.** The same four clauses on `inquiryInbox`, plus the existing C2 cases
    unchanged. `TestPromoteInquiryInbox_LatestDecisionHasNoModePredicate` passes unmodified (the LATERAL
    that picks the latest row has no mode predicate). CC5b adds two cases to each inbox: a live `task_log`
    with `resurface=false` plus a newer shadow `task_log` with `resurface=true` admits nothing (beside a
    live resurfaced control); and the reverse, a live `task_log` with `resurface=true` plus a newer shadow
    row of another action (`unmatched`, and `task_log` `resurface=false`) is still admitted, under the
    live row's project. Mutations: drop `lcd.mode = 'live'` → the first goes red; join projects on
    `latest.project_id` → the reverse goes red.
14. **Promotion, integration through real rows.** Capture pass (rule-10-shaped rule, closed task, human
    Slack DM) → a seeded `classify_inquiry` verdict (the existing promote fixture shape) → `promote.Run`
    with `LaneInquiry` after the grace. The result:
    - a `holding` task, promotion action `review`, on the message's thread (`source_thread_id` set),
      whose body ends `logged_on_closed_task: <id>\n`;
    - the closed task is still `closed`, with no `task_reopen` audit row for it and exactly one new `log`
      event (capture's);
    - run twice → no second task.
15. A second resurfaced message on the same DM attaches to the Holding task (T5). A thread whose task is
    `assignee_type=claude` is gated `claude_task` (T14). `answered` and `pending` fixtures gate exactly as
    for an `attributed` message.
16. C1 holds: `classify promote --lane personal` is byte-identical. The existing inquiry integration and
    exact-text suites (C3, C7, C8, C9, C10) pass unmodified.

### Docs

17. `docs/runbooks/capture-rules.md` gains "Closed-task chats resurface (chat-on-closed-task)":
    - the CC3 rule, the notifier list and its equality match;
    - the seed/read/disarm SQL;
    - the CC10 order;
    - the gate residual.

    `docs/runbooks/local-classifier.md` "Inquiry promotion", "What promotes", gains the `task_log` +
    `resurface` clause and the body line. Each has a prose guard in the existing `TestRunbook_*` shape.
18. `docs/runbooks/HANDOFF-kube-chat-on-closed-task.md` states the order: 0034 migrate Job → seed UPDATE
    → one image tag to every connector CronJob and pipelined → `go install ./cmd/opsctl` wherever it runs
    hand passes.
19. `.claude/INSTITUTIONAL_KNOWLEDGE.md` gains a chat-on-closed-task entry covering:
    - `resurface` is capture's recorded fact, and the lanes only read it;
    - notifier matching is EQUALITY, never substring;
    - the 0034-before-image landmine;
    - the gate residual;
    - rule 10's prefix keying (K3/K4) is why a reopen is wrong.

## Data model changes

Migration **0034** (`migrations/0034_chat_on_closed_task.sql`), additive:
- `projects.notifier_senders TEXT[] NOT NULL DEFAULT '{}'` holds exact-match notifier identities: a Slack
  display name, or a mail address. It is hand-set per project.
- `capture_decisions.resurface BOOLEAN NOT NULL DEFAULT false` is the resurface fact for the lanes, with
  `CHECK (NOT resurface OR action = 'task_log')`.

This adds no table, changes no `tasks` column and adds no new status. The resurfaced message becomes a
row in `tasks` through promote's existing create path (invariant 2).

## API / MCP tool changes

None. Specifically:
- Capture makes **no new executor call**. `resurface` is a column on capture's own append-only log
  (direct SQL, as every `capture_decisions` write).
- Promote's calls are its existing inquiry-lane calls, as `promote:inquiry`, through the executor:
  `create_task`, `task_set_source_thread`, `task_append_log`, and the SWT-36 guarded `task_reopen` for a
  dismissed thread task.
- No tool reopens the closed ref task.
- `notifier_senders` is set by a hand-run UPDATE, the precedent of `inquiry_promote_after`,
  `ticket_delivered_statuses` and `route_after`. A tool for it is Future work.

## MQTT topics

None changed. Capture's existing non-retained QoS 1 `ops/pipeline/captured` wakes the `inquiry` stage,
and `inquiry_classified` wakes `inquiry_promote` (K8).

## Files likely to touch

- `migrations/0034_chat_on_closed_task.sql` (new)
- `internal/capture/resurface.go` (new, pure: `resurfaces`, `notifierSender`) + `resurface_test.go`
- `internal/capture/senderdomain.go`: `senderAddress`, with `senderDomain` refactored onto it
- `internal/capture/rules_store.go`:
  - `storedRule.notifiers`, and `loadRules` selecting `p.notifier_senders`;
  - `ruleDecision.resurface`, set in `decideMessage` on the `found` branch;
  - `insertDecision` writing the column;
  - `RulesStats.Resurfaced`
- `internal/capture/rules_structure_test.go`, `gate_structure_test.go`: purity, and the counter scan
- `internal/capture/resurface_integration_test.go` (new): criteria 5–8
- `internal/replyfold/replyfold.go`: `InquiryEligibleLatestSQL`
- `internal/classify/store.go`: `inboxWhereInquiry` (LATERAL columns + the predicate)
- `internal/promote/inquiry.go`: `inquiryInbox`, `inquiryBody`
- `internal/promote/promote.go`: `Verdict.LoggedOnTaskID`
- `internal/promote/store.go`: `decisionReason`
- `internal/classify/inquiry_integration_test.go`, `internal/promote/inquiry_integration_test.go` (or a
  new `inquiry_resurface_integration_test.go` in each)
- `internal/classify/structure_test.go`: ledger 34
- `cmd/connectors/{jira,slackweb,upworkcrm,google}/main.go`, `cmd/connectors/google/watch.go`,
  `cmd/opsctl/main.go`, `cmd/opsctl/gate.go`: the counter
- `docs/runbooks/capture-rules.md`, `docs/runbooks/local-classifier.md`,
  `docs/runbooks/HANDOFF-kube-chat-on-closed-task.md` (new), `.claude/INSTITUTIONAL_KNOWLEDGE.md`

## In scope / Out of scope

**In scope:** the recorded `resurface` fact and the notifier list (capture, live and shadow); widening
both inquiry inboxes by one shared predicate; the body line and reason part; the counter; docs and the
handoff.

**Out of scope:**
- **GitHub PR tasks.** This is the second gap from the same investigation, a separate ticket.
- **Rule 10's replacement or key-regex fixes** (prefix buckets, missing `\b`, case, tenant). These belong
  to capture-rule-ticket-keys, including J16's mention-revive with its load check. This ticket makes a
  false key match mostly harmless (the message resurfaces on its own conversation) but does not fix it.
- Any change to SWT-45 revive, the own-action guard, the reconciler, or SWT-36's reopen.
- The gate-path resurface (CC8) and arming more projects for inquiry (`ai_inquiry`,
  `inquiry_promote_after` on upwork/reengine): the owner's per-project decisions.
- The O7 flip, and any prompt, model or eval change to the inquiry lane.
- A backfill of pre-0034 decisions (CC9).
- A dashboard change, and an MCP or opsctl tool for `notifier_senders`.

## Invariants that apply

1. **Raw-first.** No connector or normalizer change. The notifier check reads the normalized `sender`
   column only, never `raw_json`. The Slack author id stays raw-only (K6, Future work). Reprocessing is
   intact: a shadow re-point recomputes `resurface` from stored rows.
2. **One funnel.** No new task-like table. The resurfaced ask becomes a row in `tasks` (Holding, human)
   through promote's existing path. `notifier_senders` is configuration, and `resurface` is a column on
   capture's decision log.
3. **Everything through the executor.** Capture adds no executor call and no side door: its only new
   write is its own `capture_decisions` column. Every task mutation is promote's existing
   create/attach/reopen-dismissed, each validate → policy → audit → handler → audit, as `promote:inquiry`.
   Nothing new is exposed on MCP.
4. **Nothing external without a delivery row.** Nothing is sent. The outcome is a Holding task, and a
   reply would go through the existing delivery tools on a later ticket.
5. **Own-message loop closure.** Capture decides inbound messages only (`pendingMessages`' `direction`
   predicate). Slack own messages are outbound by `AuthorID == OwnUserID` (T15). His own reply after the
   ask gates `answered`. His own Jira-change notification mail either wins the J2 `revive` rule (SWT-45
   path, `resurface=false`) or carries the Jira sender address, which Verification 0b seeds onto the
   notifier list.
6. **Stealth attribution.** Nothing client-visible. The title and body are copied from verdict fields
   and columns (C-D9).
7. **Orchestrator purity.** `resurfaces` and `notifierSender` are pure and table-tested. `InquiryGate` and
   `Decide` are untouched. The orchestrator is not touched at all. Every act is audited by the executor,
   and every capture decision records its reason.

## Sibling patterns to copy

- **Per-project configuration column:** `migrations/0025_ticket_delivered_statuses.sql` (shape, polarity,
  comment, no arming UPDATE) and its "read in one place" scan.
- **Pure flag function beside the driver:** `internal/capture/revive.go` (`overrides`) and its truth-table
  test `revive_test.go`.
- **Column-fed capture tests:** `rules_revive_integration_test.go` (SWT-45 criterion 18's both-direction
  mutation).
- **Go-side From parsing:** `internal/capture/senderdomain.go`.
- **Shared inbox SQL leaf:** `internal/replyfold` (`JoinSQL`, `RepliedSinceCol`) used by
  `classify/store.go` and `promote/inquiry.go`.
- **Counter lines and their scan:** SWT-45 criterion 28 (`"revived"`, `"surfaced_created"`).
- **Queue claims:** none new. Promote's existing `classify_promotions` claim-before-act is reused as is.

## Verification protocol

**0. Blocking pre-checks (read-only prod, `psql "$OPS_DATABASE_URL"`). Record the outputs in the
delivery summary.**

- **0a. Size the gap by sender.** Human senders become resurfacing candidates; notifier senders feed 0b.

  ```sql
  SELECT nm.channel, nm.sender, count(*) AS n, array_agg(DISTINCT cd.task_id) AS tasks
    FROM normalized_messages nm
    JOIN LATERAL (SELECT * FROM capture_decisions c WHERE c.message_id = nm.id
                   ORDER BY c.id DESC LIMIT 1) cd ON true
    JOIN tasks t ON t.id = cd.task_id
   WHERE nm.direction = 'inbound' AND cd.action = 'task_log' AND t.status = 'closed'
     AND cd.created_at > COALESCE(t.closed_at, t.updated_at)
     AND cd.created_at > now() - interval '30 days'
   GROUP BY 1, 2 ORDER BY n DESC;
  ```

  The `COALESCE` is analysis only, an approximation of "logged after the close". The code never spells it.
- **0b. Notifier identities.** Choose the seed from this, then confirm that no human's stored sender
  equals the chosen string:

  ```sql
  SELECT channel, sender, count(*) FROM normalized_messages
   WHERE direction = 'inbound' AND (sender ILIKE '%jira%' OR sender ILIKE '%github%')
   GROUP BY 1, 2 ORDER BY 3 DESC LIMIT 40;
  ```

  Mail entries are the ADDRESS inside the From, e.g. `jira@treetopllc.jira.com`,
  `notifications@github.com`. Slack entries are the exact display name, e.g. `Jira`.
- **0c. byeluri's key match (K4).** Fetch the 09-10 message on the `D0B6FV6HFSR` thread
  (`thread_key LIKE 'slack:%:D0B6FV6HFSR%'`). Run `(WEB|API|OPS)-[0-9]+` over `subject + "\n" + body_text`
  **in Go**, never SQL (Postgres reads `\b`/classes differently; IK F8). Record the matched substring. A
  false positive is noted for capture-rule-ticket-keys, not fixed here.
- **0d. The model on keyless notifier traffic (K5).**

  ```sql
  SELECT e.fields->>'sender', e.fields->>'needs_reply', count(*)
    FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
   WHERE r.worker_type = 'classify_inquiry' AND e.fields->>'sender' ILIKE '%jira%'
   GROUP BY 1, 2;
  ```

  Any `needs_reply=true` rows raise the importance of seeding before the roll (CC10).
- **0e. Arming and stages.** Check `SELECT slug, ai_inquiry, inquiry_promote_after FROM projects WHERE
  ai_inquiry;`. Also check that pipelined's `PIPELINE_STAGES` includes `inquiry` and `inquiry_promote`
  (kube repo, read-only).
- **0f. Rule state.** Run `SELECT id, project_id, criteria_type, pattern, key_regex, priority, revive,
  enabled FROM capture_rules WHERE id = 10 OR revive;`, and check the statuses of tasks 56, 57 and 60.

**1. Local.** Run `go build ./... && go vet ./... && go test ./...`, then the integration suite against
the local db with 0034 applied (`make integration`). Capture the exit status and commit separately (never
pipe tests into a commit).

**2. Mutations** (each must go red, then be reverted):
- `'{}'::text[]` for `p.notifier_senders` (criterion 5);
- literal `false` for `resurface` in `insertDecision` (5);
- drop the `status = 'closed'` EXISTS (12);
- substring instead of equality in `notifierSender` (4);
- drop the CHECK (2);
- drop `"resurfaced"` from one main (9);
- drop the `blankSender` case from `resurfaces`, and a literal `false` for `blankSender` in `decideMessage`
  (CC4b);
- drop `lcd.mode = 'live'` from `InquiryLiveDecisionJoinSQL`, and join projects on `latest.project_id`
  (CC5b).

**3. Rollout (handoff; the kube session applies the manifests).** Apply 0034 → seed with
`UPDATE projects SET notifier_senders = ARRAY[...] WHERE slug = 'collaboratory';` using 0b's values (owner
decision 2026-09-14: `notifications@github.com` stays in) → roll ONE image tag to every capture writer and
every inquiry reader in ONE apply (CC10: an old writer's decisions stay `resurface=false` for good) →
reinstall opsctl before any hand-run capture pass.

**4. Smoke (read-only, within 24h of the roll):**

```sql
SELECT cd.id, cd.created_at, nm.channel, nm.sender, cd.task_id, cd.resurface, cd.reason
  FROM capture_decisions cd JOIN normalized_messages nm ON nm.id = cd.message_id
 WHERE cd.action = 'task_log' AND cd.created_at > now() - interval '24 hours'
 ORDER BY cd.id DESC;
```

- Every row from a notifier-list sender has `resurface=false` with the reason naming the list.
- A human row onto a closed task has `resurface=true`.
- Connector logs show `"resurfaced":N` on every `capture_rules:` line.
- For a `resurface=true` message, `classify promote --lane inquiry --dry-run` lists it after its grace,
  either as a would-create (`status=holding`) or gated with a reason.

**5. Usable-alone check.** After the next real human DM naming a key on a closed task, the collaboratory
Holding column shows the `{asker}: {ask}` task with `logged_on_closed_task: N`, and task N is still
closed.

**6. Withdrawn by CC5b.** The original step (a shadow re-point, `opsctl capture-rules run --since 72h
--all`, to pick up recent misses) is not valid with this predicate: the resurface branch reads only live
decisions, so a shadow re-point cannot add a resurfaced message. There is no history
demonstration; step 5 is the usable-alone check.

**Rollback.** Revert the pipelined image; the lanes stop reading `resurface`. Capture's flag is then inert
data. `UPDATE projects SET notifier_senders = '{}'` only widens resurfacing, so it is not a rollback. The
schema stays (forward-only).

## Open questions

One: `chat-on-closed-task_OPEN_QUESTIONS.md` Q1, on Avviato messages that rule 10 attributes to
collaboratory. The SPEC assumes its recommended default.

## Future work

- A gate-path resurface for gated projects, if reengine is ever inquiry-armed (CC8).
- Storing the Slack author id as a normalized column (a raw-first re-normalize) so notifier identity can
  key on a stable id instead of a display name.
- An executor tool plus `opsctl` verb for `notifier_senders`, and `capture-rules list` printing each
  project's list.
- An `--outcomes` split for resurfaced vs attributed promotions, to read this path's precision alone.
- capture-rule-ticket-keys: replacing rule 10, which removes the prefix buckets and the false key matches.
