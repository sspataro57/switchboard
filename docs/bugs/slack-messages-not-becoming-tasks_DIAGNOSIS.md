# Diagnosis — slack-messages-not-becoming-tasks (Jira SWT-78, swb #521)

Status: **both causes confirmed** against production (read-only SELECTs, 2026-09-23). The repro still
fails the same way (`FAIL: 25 of 48 … (17 of them in DMs)`).

## Root cause

The system has no DM path. A Slack DM from a person is handled exactly like channel chatter:

1. Capture rules 8 (Collaboratory `T0HPR78RX`) and 9 (Avviato `T0360B84U`) are `source_slack_workspace`
   rules with no `external_system`. So `capture.decideMessage` ends at
   `internal/capture/rules_store.go:887-894` with `action=attributed` and the note "no external_system, so
   attribution only". The comment there says "Turning arbitrary chatter into tasks is triage's job"
   (SWT-17, b6038ca4, 2026-08-28).
2. `attributed` makes the message eligible for the inquiry lane (`replyfold.InquiryEligibleLatestSQL`,
   `internal/replyfold/replyfold.go`). The pipelined `inquiry` stage then sends it to qwen3:8b
   (`classify.Run`, inbox `inboxWhereInquiry`, `internal/classify/store.go:141-154`).
3. The inquiry prompt is tuned for precision: "needs_reply = false, which is most of this conversation …
   thanks, acknowledgement, agreement, or social conversation … When you are genuinely torn, answer false"
   (`internal/classify/inquiry.go:89-103`). **Cause #1:** the promoter only reads verdicts where
   `e.fields->>'needs_reply' = 'true'` (`internal/promote/inquiry.go:420`). A `false` verdict is never
   selected, and nothing else reads it. 16 of the 17 DMs stopped here.
4. **Cause #2:** a `true` verdict still goes through the pure gate `promote.InquiryGate`. Its `answered`
   clause (`internal/promote/inquiry.go:182-183`) refuses any verdict whose thread has a later outbound
   message. For a DM, the "thread" is the whole conversation (`thread_scope=conversation`), so **any**
   later message from Salvador anywhere in the DM counts as answering it. A gated verdict writes no row.
   It sits in the inbox until the 72h fence and leaves no trace. 403664 stopped here.

## Evidence

### Cause #1: needs_reply=false ends processing
- `internal/capture/rules_store.go:729`: `pendingMessages` reads `m.direction = 'inbound'` only.
  Capture never decides one of Salvador's own messages (invariant 5 holds).
- `rules_store.go:887-894`: rules 8/9 have `external_system` NULL (confirmed in prod `capture_rules`),
  so the result is `attributed`, with no task and no external ref.
- `internal/pipeline/contract.go:124-131`: `captured` wakes `gate`, `route` and `inquiry`. Gate only
  acts on `held` rows and route only on `unmatched` ones, so for a rule-attributed DM **only `inquiry`
  acts**. Then `inquiry_classified` wakes `inquiry_promote`.
- `internal/classify/store.go:141-154` (`inboxWhereInquiry`): latest decision `attributed` + `p.ai_inquiry`
  (collaboratory, project 4, is armed) + no `classify_inquiry` extraction yet. Nothing in this path
  checks whether the conversation is a DM or a channel.
- `internal/promote/inquiry.go:420`: `WHERE e.fields->>'needs_reply' = 'true'`. The 16 `fyi` verdicts
  (extractions 12754, 12774-12787, 12793, 12799) are never read.
- In the gate, the only DM-specific logic is `addressed()` (`inquiry.go:208-217`). It *helps* a DM pass
  `not_addressed`, but it runs after the `answered` clause and only on `needs_reply=true` rows.
- Not a capture gap: all 48 messages have raw → normalized → live decision (REPRO). The watcher / rotation
  coverage issues in the REPRO are real, but they did not cause these 17.

### Cause #2: 403664 (Katie, D04F7LXRB8B, needs_reply=true, scheduling)
- `normalized_messages` 403664: `sent_at` 15:56:12.421Z, first seen (`created_at`) 16:15:14.151Z.
  Salvador's reply 403665 (outbound, same thread 5019): `sent_at` 15:56:50.005Z, first seen 16:15:14.162Z.
  **The ask and its answer arrived in the same rotation export, 11 ms apart.** The inquiry verdict (ai_run
  at 16:15:25Z) was recorded after both were stored.
- The replyfold columns for extraction 12764, recomputed now: `replied_since = t`, `prior_post = t`.
  So `InquiryGate` returns `answered` on every pass. No `classify_promotions` row, no audit row.
- The post-restart log says `gated="map[answered:11 …]"`. The inbox query reproduces exactly **11**
  unpromoted `needs_reply=true` verdicts inside the 72h fence. **All 11 are DMs and all 11 have
  `replied_since=t`**: D04F7LXRB8B ×10 (388052 … 403664) and D0AUD86LKGA ×1 (388524). That count is these
  verdicts.
- `InquiryGrace = 0` (`inquiry.go:64`, 343f531, 2026-09-18) comes with a comment that `answered` "almost
  never fires now, because nothing waits long enough for a reply to land first" (`inquiry.go:54-57`). That
  assumption is false for Slack. Rotation ingests in batches of up to 30 minutes, and an ask plus Salvador's
  quick reply land in the **same** export. Every verdict is therefore recorded *after* the reply is
  already stored.
- One could argue the gate was right about 403664: he answered 38 s later. Salvador's decision
  ("all DMs to me are actionable") overrides that for DMs.

### How DMs, channels, bots and own messages are identified in the stored data
| fact | where | reliable? |
|---|---|---|
| 1:1 DM | conversation segment starts `D` (`slack:{ws}:D…[:{root}]`). The one reader is `slackweb.IsDirectMessageKey` (`internal/connector/slackweb/normalize.go:146`) | yes. Raw `conversation.type='dm'` agrees for all 63 D-threads |
| group DM (mpdm) | **only** in raw: `raw_source_items.raw_json->'conversation'->>'type' = 'group_dm'`. Ids are `G…` (13 threads) **or `C…` (40 threads)**; current Slack gives new mpdms C ids | **no by id prefix.** Also, the leaf types by id prefix on explicit-URL / targeted paths (`slackconnector src/switchboard/export.ts:177-181`, `slack-web-adapter.ts:1500`), so a watched C-mpdm would be stored as `public_channel`. `IsDirectMessageKey` excludes group DMs by design (C-D3). The repro's `^[DG]` rule misclassifies C-mpdms as channels (none were active on 09-22, so its counts stand) |
| channel | raw `conversation.type = 'public_channel'` (no `private_channel` rows exist in prod) | `C…` alone is ambiguous with C-mpdms |
| app/bot sender (Jira) | `normalized_messages.sender = 'Jira'` in D01EJRX6P45 / D023E7XSSGG. `author_id` is an ordinary `U…` (U0182G5UH8V, UAWRGBVNK), so **author id cannot tell a bot**. Project 4's `notifier_senders` contains `Jira` (equality match: `capture.notifierSender`, `internal/capture/resurface.go:78`) | yes for Jira, via the per-project notifier list. All 220 Jira-DM messages in 30 days are rule-75 `task_log` today |
| Salvador's own message | `normalized_messages.direction = 'outbound'`, set at normalize from `author_id == workspace.own_user_id` (`normalize.go:78-80`, fails closed on a missing id). Capture skips outbound (`rules_store.go:729`). Self-DM DSA806DHA is all outbound | yes |

## How "attach to the conversation's open task" works today
- **The mechanism 402504/402505 → #464 used** (classify_promotions 90/91, 13:15Z on 09-22, empty reason):
  promote's `threadTask` (`internal/promote/store.go:393-450`) finds the oldest task with
  `source_thread_id = message.thread_id AND project_id = <attributed project> AND status NOT IN
  ('closed','delivered')`. `Decide` rule 1 returned `attached`. `act` then called `task_append_log` and
  `task_mark_activity` through the executor as `promote:inquiry` (`store.go:194-226`). That was the code
  before SWT-72 D11. D11 (4604437, committed 15:11Z the same day) replaced rule 1 on the inquiry lane:
  an open thread task now produces a **new** task with `related_task` (407656 → #501, reason "thread's
  open task 464; created its own task").
- `source_thread_id` is set by `task_set_source_thread` (promote `setProvenance`, capture
  `setRuleProvenance`). For a DM it is the conversation-level thread (unrooted key). Rooted DM threads are
  rare (11 of 2,927 DM messages in 60 days) but get their own `normalized_threads` row.
- Capture's own attach is by `external_refs` (`taskForExternalRef`, UNIQUE `(system, external_key)`). It
  **cannot** express "closed → new task": one key means one task forever (IK "Capture rules contract").
  Also, a `task_log` onto a closed task resurfaces into the inquiry lane, which means qwen again. So
  "just add a capture rule with `external_system='slack'`" is the wrong tool for this.
- **What "open" should mean:** `status NOT IN ('closed','delivered')`, the ONE spelling (`promote.open`,
  `threadTask`, `task_list`'s `in_play`). Dismissed = closed.
- **Which open tasks count as the conversation's task.** Prod has **ticket tasks whose `source_thread_id`
  is a DM**: #452 `WEB-10469 — Jose Garcia` (jira ref) on DSAV4HJ2F, and #60 `OPS — Jira` on D01EJRX6P45.
  They were created by rule 75 from a DM message. A bare `source_thread_id` lookup would pile José's
  chatter onto a Jira ticket task. Recommended: human tasks only (C-D13: never log untrusted text onto
  a claude task, since its log feeds a worker prompt) and **no `external_refs` row** (a ticket task
  belongs to its ticket). See Open questions.

## Why the reproduction fails
16 DMs have live `attributed` (rule 8/9) → qwen `needs_reply=false` → excluded by
`promote/inquiry.go:420`. Outcome: `classifier_said_no_reply(fyi)`. 403664 has `attributed` → qwen
`true` → gated `answered` (`inquiry.go:182`) because 403665 was stored with it. That writes nothing, so
the outcome is `classifier_said_reply_but_no_promotion_row`. Both paths exist only because a DM takes the
classifier route at all.

## Invariant implicated
None violated: every write that happened went through the executor, capture skips outbound, the
orchestrator is not involved. **The fix must preserve** 1, 2, 3, 5 and 7. It also must not create a
**second** qwen bypass that reads the model's output (see Fix scope, "why capture").

## Proposed fix scope

**Where: capture, not classify or promote.** Capture is deterministic, runs before every stage, and
makes one live decision per message, forever. If a DM's live decision is `task` / `task_log` (with
`resurface=false`, onto an open task), `replyfold.InquiryEligibleLatestSQL` already excludes it from
**both** inquiry inboxes. That keeps qwen away from DMs by construction, with no change to classify SQL
or promote SQL. The alternatives break rules the repo already has:
- skipping DMs inside `classify.Run` leaves them in its inbox forever: the SWT-40 "Limit bounds acted
  rows" starvation;
- detecting DMs in classify/promote SQL would be a second spelling of the slack key (banned; the
  DM rule must come from `slackweb`);
- an `external_refs`-keyed capture rule cannot make "closed → new task" work.

- [ ] **A. Pure predicate, new file `internal/capture/direct.go`** (the `comm.go` / `resurface.go`
  shape, no I/O tokens, reason-bearing). `directConversationTask(input)` returns (applies, why). Inputs:
  channel is slack; the conversation is a DM or group DM (below); sender not blank (fail closed, reason
  "no sender identity"); sender not on the rule project's notifier list (`notifierSender`, which keeps
  Jira and any future app out). Its caller runs it **only** at the attribution-only exit,
  `rules_store.go:887-894` (`winner.extSystem == ""`). Every rule-matched outcome keeps its current
  behaviour: rule 75 task_log/comm, rule 63 bulk, gate, pr_review.
- [ ] **B. DM detection, one spelling.** 1:1 is `slackweb.IsDirectMessageKey`. Group DMs need the raw
  `conversation.type = 'group_dm'`: add it to `pendingMessageCols` from `ri.raw_json` (capture already
  reads raw_json in `prreview_store.go`) **or** add a `slackweb` helper over the raw observation. Note that
  `slackweb/dmkey_test.go:148` allows **exactly one** function whose name matches `Is*Direct*|*DM*`, so
  pick a name outside that pattern or amend the test deliberately. This is a DB column, so the
  "test the column, not the fixture" rule applies.
- [ ] **C. Store half in `decideMessage`**, attribution-only branch. Look up the conversation's open
  task: `tasks.source_thread_id = pm.threadID AND project_id = winner.projectID AND status NOT IN
  ('closed','delivered') AND assignee_type = 'human' AND NOT EXISTS (external_refs for the task)`, oldest
  first (`threadTask`'s ordering). Found → `action=task_log`, `task_id` set. Not found → `action=task`.
  `external_system` / `external_key` stay NULL (no live CHECK forbids that; confirmed on
  `\d capture_decisions`). `resurface` stays false. Reason text worded by mode ("would …" in shadow).
- [ ] **D. Act half in `EvaluateRules`**, as a new case alongside `actionTask` / `actionTaskLog`. The
  existing cases dereference `*decision.extSystem` / `*decision.extKey` (`rules_store.go:469-609`, e.g. 469, 482, 528)
  and **would nil-panic**, so gate them on a `decision.direct` flag. Executor calls as `capture:{connector}`,
  claim → act → record order (`recordDecisionTask` before provenance):
  - create: `create_task` (rule's project, `assignee_type=human`, `status=ready`, title
    `{sender}: {first line}` via `textmatch.NormalizedPrefix`, body = ids-only key lines like
    `ruleTaskBody`), `recordDecisionTask`, `task_set_source_thread` (`setRuleProvenance`),
    `task_mark_activity` (so it lands in INCOMING, swb #491's rule). **No `external_refs` row.**
  - attach: `task_append_log` (`appendRuleLog`) then `task_mark_activity` on the target (SWT-72 D3). The
    log/mark helpers take `system, key` only for text, so give them a DM wording.
- [ ] **E. Resurface corner (decide in/out explicitly).** A person's DM that rule 75 logs onto a
  **closed** ticket task is recorded `resurface=true` and goes to the inquiry lane, i.e. to qwen (406195 →
  #497 on 09-22). To keep "DMs skip qwen" complete, that case should also take the DM conversation-task
  path instead of resurfacing. The change is in the `resurfaces()` inputs (a `direct` disqualifier) plus
  the DM act. It is small, but it touches the SWT-53 contract.
- [ ] **F. Dry run and explain.** `DryRunRules`' `simulatedRefs` needs a per-conversation simulated
  task, so that a second DM in the window reads `task_log` in a dry run the way the live pass would decide
  it. `ExplainMessage` / `task_match` get it for free (same `decideMessage`).
- [ ] **G. Regression tests** (test-author converts the repro):
  1. Unit, pure: DM + person → applies. Notifier (`Jira`) → not. Blank sender → not (named reason).
     `public_channel` C id → not. `G…` and C-id `group_dm` → applies. Outbound is never an input.
  2. Integration, live `EvaluateRules`: first DM → one `task` decision + a task with `source_thread_id`,
     activity mark, no external_ref. **A second DM in the same conversation → `task_log` onto the same
     task, still one task.** Open task closed (plain close **and** dismissed) → next DM → a **new** task.
     Open task is a claude task, or a ticket task (external_ref) → not attached to (new task).
  3. **Own messages never create tasks:** an outbound DM message gets no decision and no task (and the
     self-DM shape, outbound only).
  4. **Jira-app DM unchanged:** with a WEB key it stays rule-75 `task_log` / no comm (notifier). Without
     a key it stays `attributed` with no task.
  5. **Channels still classified:** a `public_channel` message stays `attributed` and **is** in
     `inboxWhereInquiry`. A DM message decided by the new branch is **absent** from both
     `inboxWhereInquiry` and `promote.inquiryInbox` (assert on the real SQL: that is the "skips qwen"
     proof).
  6. Shadow mode writes the reason and creates nothing.
  7. Group DM read from the raw column (integration, not a hand-set struct field). Mutation: drop the
     column from the SELECT → the group-DM test goes red.
  Mutations to record: drop the notifier clause → Jira test red; drop the status filter → closed test
  red; drop `assignee_type`/ref filters → claude/ticket tests red.
- [ ] **H. Deploy note, not code.** Any connector main's capture pass decides every pending message,
  not only its own channel. An old image (e.g. `connector-google-watch`, or the watcher still pinned on
  0.7.44) can decide a Slack DM `attributed` first, and that live claim is forever. Roll one tag to
  every capture writer in one apply (the 0034 precedent) and reinstall opsctl. **No migration.**

**Backfill (2026-09-22's 17 DMs).** The fixed path cannot re-decide them. Each already has a live
`attributed` row, and the live claim is one row per message forever (`insertDecision` ON CONFLICT on
`capture_decisions_live_uniq`). `capture-rules run --all` skips them for that reason, and a new
decision mode would need a migration plus a fifth partial unique index. Recommended: a one-off
`opsctl capture-rules direct-backfill --message <ids> [--dry-run]`. It loads the named messages
through `pendingMessageCols` / `scanPendingMessage` and runs the **same** predicate (A-B) and the
**same** act helper (D) through the executor, oldest first, with no capture_decisions write. The
per-message trace lives in `task_events` (log lines naming the message ids) and `audit_events`.
Applied as of now, both conversations have no open task (#155/#497/#452 on DSAV4HJ2F and #464/#501 on
D04F7LXRB8B are all closed; #155 was dismissed `handled_elsewhere` and #464 `duplicate` at 22:28Z on
09-22). Result: **2 new tasks**, José (DSAV4HJ2F, 14 messages: 403016, 404991-405422) and Katie
(D04F7LXRB8B, 3: 403664, 409610, 420472). The fallback with no new code is the same calls by hand via
`opsctl call` (create_task, task_set_source_thread, task_append_log ×N, task_mark_activity), which is
still audited. With either route, the repro's PASS rule has to learn a "backfilled" outcome (a task
log line naming the message), because the live decision row stays `attributed`.

## Out of scope for this fix
- Channel C1C1TSLJH (#a-millon) → rule 63 bulk, never classified (8 REPRO FAILs). Salvador's question
  3 on whether that is the "general forum" is still open.
- The inquiry lane's quality on channels (precision prompt; conversation-scope `answered` in busy
  channels).
- Comm tasks (SWT-74) are per message, not per conversation. A DM that names a WEB key on an open ticket
  task still makes its own comm task.
- Rotation coverage: D08L7HCA8NP / D022YQ4KNNT never read, `messages_skipped_identity`, bridge-busy
  skips (REPRO "Capture side").
- The leaf's prefix-based `conversationTypeForId` (sibling repo): a C-id group DM added to `slack_watch`
  would be typed `public_channel`.
- 2026-09-21 (31 DM fails), 2026-09-17 and 09-23 until deploy were affected the same way. Salvador only
  asked for 09-22 (see Open questions).

## Open questions
1. **Dismissed conversation task, then a new DM.** Salvador's rule is "closed means a new task". SWT-36
   (his earlier decision) reopens a *dismissed* task on new activity. Which one wins for DMs?
   Recommendation: new task (his words this time); #155 was re-dismissed 4× under SWT-36.
2. **Which open tasks count as "the conversation's".** Recommendation: human, no external_refs (so
   not ticket #452-style tasks). Should a comm task (SWT-74, human, no ref) on the DM absorb later DMs?
   The recommended predicate says yes.
3. **Group DMs:** does "group DM" include named group conversations the leaf types `group_dm` (e.g.
   C0BPR9FUCLE "ASU-Collab Dev Discussions")? Could they be private channels? Not verifiable from the
   stored data.
4. **Rooted threads inside a DM:** separate task per thread (lookup by thread id, as specified), or
   folded into the conversation task (needs a slackweb conversation-key helper plus a thread lookup)?
   11 messages in 60 days.
5. **Fix item E** (resurface onto a closed ticket task from a DM): in this fix or later?
6. **Backfill scope and form:** only 09-22 (as asked), or also 09-21 and 09-23 up to the deploy?
   Should the 14 José banter messages ("Jajajaja", "Si") go into one task (they would, under one task
   per conversation)?
7. Bots other than Jira: only `Jira` sent DMs in the last 14 days, so the notifier list covers today.
   A future app DM (Slackbot, calendar) from a sender not on the list would become a task.

## Risk assessment
- `decideMessage` / `EvaluateRules` are shared by every connector main, `opsctl capture-rules
  run|try`, `task_match` and `DryRunRules`. A nil deref on `extSystem` in the new case (D) would stall
  capture for every connector, so test the live pass explicitly.
- Volume: human DM traffic is about 20-60 inbound messages/day. With one task per conversation that is a
  handful of tasks/day, plus activity marks (Requeue taps; SWT-72 residual).
- Readers that treat `task` / `task_log` as "has an external ref" (capture report, board `from_message`,
  `task_match` sources): check each one with a NULL-ref live task row.
- The inquiry lane loses all DM input, so its `--outcomes` precision readout and eval strata shift.
  That is expected.
- Deploy order (H): a stale capture binary silently reverts a DM to the qwen path forever for that
  message.

## Landmine matched
New. Recorded in `.claude/INSTITUTIONAL_KNOWLEDGE.md` as "A Slack DM is not a channel, and the
conversation id does not tell you which is which (SWT-78)". It covers C-id group DMs and the
path-dependent raw type, conversation-scope `answered` in a DM, and the same-export ask+reply that
defeats `InquiryGrace=0`'s premise. Related to the existing "The classify lane's only gate is a boolean
the same model authored (SWT-68)": the same shape, this time dropping instead of creating.

## Decisions on the open questions (2026-09-23, session sbc, under Salvador's "keep going until it's fixed and backfilled")

Taken from his stated rules; each is his to overrule.
1. Dismissed conversation task + a new DM → a NEW task ("otherwise it creates one"). "Open" = status NOT IN ('closed','delivered').
2. The conversation's task = a HUMAN task with NO external_refs row whose source thread is in that DM conversation; a comm task counts.
3. Group DMs are included, detected from the raw `conversation.type='group_dm'` (he said "all DMs to me").
4. Rooted threads inside a DM fold into the conversation's task ("one task per conversation"): the open-task lookup matches any thread of the same conversation, via ONE slackweb conversation-key helper.
5. Item E is IN: a person's DM that would resurface onto a closed ticket task takes the DM conversation-task path instead, so no DM reaches qwen.
6. Backfill: 2026-09-22 (asked) plus 2026-09-23 up to the deploy (same loss); 09-21 and earlier are offered, not done.
7. Bots: the per-project notifier list (Jira) is the exclusion; no change.
