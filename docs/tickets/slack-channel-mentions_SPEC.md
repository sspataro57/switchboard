> Jira: SWT-79

# SPEC: slack-channel-mentions (ad-hoc, follows SWT-78)

## Source

Salvador, 2026-09-23, verbatim:

> "yes, #a-millon is the general forum, send it through qwen"
> "channels is only when they mention me"

Asked "When someone @-mentions you in a channel (including #a-millon), what should happen?" he answered
**"qwen decides"** (not a direct task).

> "a-million is generally not collaboratory. we only respond to mentions on those channels"

Context: SWT-78 (live in 0.7.47) decides a person's Slack DM or group DM in capture, as a task on its
conversation's task, and that message never reaches qwen. Every other Slack message that capture leaves
`attributed` to an `ai_inquiry` project still goes to qwen3:8b (`inquiry-v3`, which breaks ties toward
recall). That includes all channel chatter in collaboratory. #a-millon goes nowhere: rule 63 sends it to
`bulk`.

No open questions came up that CLAUDE.md, the code and his answers above could not settle. Decisions
taken without asking are listed under "Decisions made unilaterally", each with its reason. Two of them
change behaviour he may want to overrule: D5 (unmentioned thread replies) and D10 (backfill). Pre-checks
0d and 0f put numbers on both before any commit.

## Goal

A Slack **channel** message reaches the inquiry lane only when its text @-mentions Salvador. A channel
mention the lane admits counts as addressed to him in promotion. #a-millon becomes its own armed project,
`a-millon`, on that mention-gated lane.

**Usable alone:** after the deploy, the rule swap and arming, an @-mention of Salvador in any Slack channel
(collaboratory's channels or #a-millon) gets a qwen verdict. If the verdict is an ask, it becomes a
`ready` human task in that channel's project, within one rotation (up to about 30 min) plus one pipeline
pass. Unmentioned channel chatter gets no qwen call and no task. DMs behave exactly as in 0.7.47.

## What exists today (verified by reading, worktree at `aefbede`)

**Capture**
- `internal/capture/rules_store.go:891` `decideMessage` makes one decision per message.
  - Rules 8/9 (the workspace catch-alls) have no `external_system`, so the exit at `:912-927` leaves the
    message `attributed` ("attribution only").
  - Two more exits also leave a message `attributed`: a keyed rule that derived no key (`:928-938`) and a
    github key that is not a PR (`:950-958`).
  - SWT-78 runs `directConversationTask(directFacts(pm, winner))` at all three exits (`:924`, `:935`, `:954`).
  - `decideMessage` has four external callers: `EvaluateRules` (`rules_store.go:440`), `DryRunRules`
    (`dryrun.go:174`), `ExplainMessage` (`explain.go:76`, behind `task_match`) and the SWT-78 backfill
    (`direct_backfill.go:177`).
  - It is also called recursively from `prFallThrough` (`rules_store.go:1253`).
- `internal/capture/direct.go:40` is the pure DM predicate. `direct_store.go:36` `directFacts` builds its
  input:
  - `slack` is `pm.channel == slackweb.Channel`;
  - `dm` is `slackweb.IsDirectMessageKey`;
  - `groupDM` is raw `conversation.type = 'group_dm'`, which `pendingMessageCols` reads at
    `rules_store.go:838-843`.
- `insertDecision` (`rules_store.go:1536-1557`) writes `capture_decisions`. Migration 0034 is the
  precedent for a capture-recorded fact that the lanes only read: the `resurface` column with the
  `capture_decisions_resurface_is_task_log` CHECK.

**Slack helpers**
- `internal/connector/slackweb/normalize.go` has the key helpers: `channelThreadKey` (`:107`),
  `IsRootedThreadKey` (`:121`), `ConversationThreadKey` (`:140`) and `IsDirectMessageKey` (`:165`).
- `NormalizeMessage` stores `message.Text` verbatim as `body_text` (`:90`). Mentions arrive as display
  names ("@Salvador", "@Salvador Spataro", "@SalvadorSpataro"), not `<@U…>`. The raw item carries only
  `own_user_id`, never his display name (`types.go:23,130`).
- `slackweb/dmkey_test.go:148` allows exactly ONE declaration matching
  `func\s+(Is\w*(?:Direct|DM|Dm)\w*|\w*DirectMessage\w*)\s*\(`.

**Inquiry inboxes**
- `internal/replyfold/replyfold.go:127-130` `InquiryEligibleLatestSQL` is the one admission predicate for
  both inquiry inboxes: `latest.action='attributed'`, OR the live `task_log`/resurface branch.
- `internal/classify/store.go:141-154` `inboxWhereInquiry` and `internal/promote/inquiry.go:401-425`
  `inquiryInbox` each spell their own `latest` LATERAL. Today it selects `cd.action, cd.project_id` only
  (`store.go:143-146`, `inquiry.go:413-415`).
- The classify inbox is SQL-`LIMIT`ed on rows read (`store.go:253-254`). A message skipped in Go stays in
  the inbox and starves the rows behind it (the SWT-40 "Limit bounds acted rows" landmine). Promote's
  `runInquiry` bounds rows acted on (`inquiry.go:292-295`).

**Promote gate**
- `promote.InquiryGate` (`inquiry.go:157-202`) checks rethreaded, kind, stale, pending, answered,
  `not_addressed`, then `claude_task`.
- `addressed()` (`inquiry.go:204-217`) is C-D3: gmail, OR a 1:1 DM, OR a `thread`-scoped key he posted on
  before the ask. "A top-level channel message is NOT addressed even after he spoke there … the accepted
  recall cost." So a top-level `@Salvador` in a channel is gated `not_addressed` today.
- `InquiryCandidate` (`inquiry.go:139-151`) has no text field. Promote reads no `body_text` today.

**#a-millon routing**
- Rule 63 is `thread_key_prefix` `slack:T0360B84U:C1C1TSLJH` → `bulk`, attribution only, priority 99
  (`docs/runbooks/capture-rules.md` "#a-millon → bulk"; inquiry-promote SPEC A-D5).
- Evaluation order is `priority DESC, id ASC` (`internal/capture/rules.go:189-194`). Two enabled rules at
  99 on the same prefix therefore resolve to the LOWER id.
- `capture_rules` is UNIQUE on `(project_id, criteria_type, pattern)`, and no tool edits a rule. The same
  prefix CAN be re-added for a different project.
- The rule tools:
  - `capture_rule_add` (`internal/tools/capturerules.go:254-293`, `opsctl capture-rules add --priority`,
    `cmd/opsctl/main.go:459-498`);
  - `capture_rule_set_enabled` (`opsctl call --tool capture_rule_set_enabled`, `main.go:407`);
  - `opsctl capture-rules try` (`main.go:544`).

  All three are humanOnly and audited.

**Creating projects**
- No tool creates a project (`project_list` is the only project verb). Migrations 0016 (`personal`) and
  0018 (`bulk`) are the repo's precedent for creating a project row: a reviewed, forward-only INSERT with
  explicit values and `ON CONFLICT (slug) DO NOTHING`.
- 0016's landmine: `ai_locality` defaults to `local_only`.
- `inquiry_promote_after` (0031) is NULL = off, armed by a hand UPDATE (C-D2; `inquiry.go:302-313`).

**Other**
- The migration ledger at `internal/classify/structure_test.go:1245` must learn every new number. Its note
  must sit ABOVE the "34 is" line (see the 0042 note at `:1197`). The latest migration is 0042.
- The inquiry prompt (`internal/classify/inquiry.go:81-120`) is NOT changed by this ticket.

## Decisions made unilaterally (with rationale)

**D1: The gate lives in CAPTURE, as a recorded fact on the decision row.** Capture already makes a
deterministic, forever decision at exactly the exits that make a message inquiry-eligible. It already
knows DM vs group DM vs channel (SWT-78's facts) and already holds `body_text`. The IK rule from SWT-78
applies directly: an owner-declared class is decided in capture, before any model. The alternatives are
each worse:
- A Go-side skip in `classify.Run` leaves unmentioned rows in an inbox that is SQL-`LIMIT`ed on rows read,
  which is the SWT-40 starvation landmine.
- A "skipped" extraction row written without a model call would be a fake verdict. It pollutes
  `--outcomes`, eval strata and the SWT-68 dismissal readout.
- A normalized column written by the slackweb normalizer would need a normalize-only backfill. The inbox
  SQL would then need its own DM vs channel test to keep notifier DMs unchanged, which is a second
  spelling of the slack key (banned). And capture's DM facts would sit in one package and the mention in
  another.
- A mention regex in SQL is a second spelling, and Postgres regex is not Go's (the SWT-70 lesson).

**D2: The column is `capture_decisions.channel_unmentioned BOOLEAN NOT NULL DEFAULT false`, with CHECK
`(NOT channel_unmentioned OR action = 'attributed')`.** Polarity and default follow 0034's reasoning:
- Every writer that does not name the column records `false`, meaning "eligible, as today". Those writers
  are the gate stage's rows, the route stage's rows, pre-deploy rows and an old binary.
- So the migration changes no current behaviour on its own, and a rollback of the images is safe.
- The fact is recorded on EVERY `attributed` decision capture writes, whatever the project, through ONE
  post-decision step. See "Capture" under API / tool changes.

**D3: One predicate, `slackweb.MentionsOwner(text string) bool`, in a new
`internal/connector/slackweb/mention.go`.**
- **The name** stays outside `dmkey_test.go:148`'s regex, so that test stays unchanged and green.
- **The owner's names are a Go constant** in that file. They are not an env var or a column. SWT-30 D2's
  reason applies: a typo cannot widen it unreviewed. The raw item carries no display name.
- **The forms**, case-insensitive: `@` + `salvador`, optionally followed directly by `spataro`.
  - A bare "@Salvador" followed by a space already covers "@Salvador Spataro".
  - "@SalvadorSpataro" needs the explicit joined form, because after "Salvador" comes a letter.
- **Left boundary:** the `@` is at the start of the text, or the character before it is NOT a Unicode
  letter or digit and NOT one of `. _ % + -` (the email local-part characters). So `x@salvador.com` and
  `a.b@Salvador.org` are not mentions.
- **Right boundary:** after the name comes end of text, or a character that is NOT a Unicode letter or
  digit and NOT `_`. And it is NOT a `.` immediately followed by a letter or digit, which would be a
  domain. So "@Salvadora", "@Salvador_bot" and "@salvador.com" are not mentions. "@Salvador.",
  "@Salvador," "(@Salvador)", "@Salvador's" and "@Salvador<NBSP>Spataro" are.
- **`@here`, `@channel` and `@everyone` are not a mention of him** ("only when they mention me").
- **`<@U…>` is not recognised.** The pre-check below confirms the leaf stores none, and his user ids are
  per workspace, raw-only facts. It goes to Future work if 0b finds any.
- **Implementation:** plain Go, not one regex. RE2 has no lookahead and the domain rule needs one.

**D4: Promotion: a Slack channel mention is addressed.** `addressed()` gains the clause
`c.Channel == slackweb.Channel && c.Mentioned`, placed before the thread rule. The existing
`thread && prior post` clause is KEPT.
- **Why a mention must count as addressed:** without it, every top-level mention (conversation scope) and
  every mention in a thread he has not posted in is gated `not_addressed`. The feature would do nothing,
  in #a-millon above all.
- **Why the old clause stays:** after D1, an unmentioned channel message reaches promote only through
  paths this ticket leaves alone:
  - the SWT-53 resurface branch, a rule-matched `task_log` onto a closed task;
  - gate or route rows;
  - pre-deploy rows inside the 72h window.

  Requirement 1 says rule-matched outcomes are unchanged. For every channel message admitted through the
  attributed branch, "addressed iff mentioned" holds in practice, because D1 lets in only mentioned ones.
- **Where the mention comes from:** promote's inbox now selects `nm.body_text`, and the scan sets
  `InquiryCandidate.Mentioned = slackweb.MentionsOwner(body)`.
  - The gate stays pure; `Mentioned` is an input.
  - The body is used only for this predicate. It never enters a title, a body or a log. C-D9's "every
    value is a copy of a stored field" still holds for the task.
  - Why not read capture's column instead: the column is `false` for every legacy, gate or route row, so
    it cannot express "mentioned". Recording a second, positive column would duplicate what the one
    function computes from the same text.

**D5: An unmentioned reply in a channel thread he is already in no longer reaches qwen.** This is
his literal rule ("channels is only when they mention me"; "we only respond to mentions on those channels").
It is a real recall cost: Slack thread replies to him often carry no @. Pre-check 0d measures how many
promoted channel tasks in the last 30 days came from unmentioned messages. If that number is material,
show it to Salvador before commit. The alternative, "mentioned OR a thread he posted in before", is one
extra clause in the capture predicate (see Future work). Not re-asked, per the coordinator's instruction.

**D6: The #a-millon project is created by migration 0043, following the 0016/0018 precedent.** No tool
creates projects, and a raw psql INSERT on prod is unreviewed. Values, each named explicitly:

| column | value | why |
|---|---|---|
| `name` | `#a-millon (Avviato general)` | display only |
| `slug` | `a-millon` | the channel's name; aliases are data (A-D3), so nothing is generated from it |
| `client` | NULL | load-bearing (0016): `task_get_next`'s `p.client = $1` keeps worker consoles off it. Inquiry tasks are `human` anyway |
| `execution` / `delivery` | `manual` / `dashboard` | defaults, stated so they are reviewed |
| `ai_locality` | `'any'`, **explicit** | Avviato Slack work chatter. It was collaboratory (`any`) until O4 moved it to bulk on 2026-09-12. The default `local_only` would silently make the drafts lane skip this project's tasks. The inquiry lane is pinned `ClassRestricted` either way (`classify.routedClass`), so locality never changes where qwen runs |
| `ai_classify` | false | the personal lane is mail |
| `ai_inquiry` | true | the workload flag, the 0024 precedent. Inert until a rule attributes something here |
| `inquiry_promote_after` | **NULL** | armed by hand at go-live (C-D2; see D7). This also keeps every test DB free of an armed project the fixtures did not create |
| `notifier_senders` | `'{}'` | read only on DM/resurface/comm paths, none of which an attribution-only channel rule reaches. Pre-check 0h lists a-millon's senders in case a bot mentions him |
| `ticket_assignee_gate` / `ticket_delivered_statuses` | defaults | no ticket rules point here |

The migration does not touch `bulk`. It stays `ai_inquiry = false` with its 24 sender rules.

**D7: Arming is `UPDATE projects SET inquiry_promote_after = now() WHERE slug = 'a-millon'`, the
documented C-D2 lever.** It is run once, after every workload is on the new image and BEFORE the rule
swap. A verdict recorded before the cutover never promotes, because the cutover is forward-only on the
verdict clock. So arming after the swap would silently drop the swap window's mentions.

**D8: The rule swap uses the existing audited tools, in an order with no gap.**
1. `opsctl capture-rules add --project a-millon --type thread_key_prefix --pattern slack:T0360B84U:C1C1TSLJH --priority 99 --note "SWT-79: #a-millon is the general forum; its own project, mention-gated inquiry lane (was rule 63 → bulk)"`
   - No `--external-system`.
   - The same priority and shape as rule 63. It still outranks rules 10 (90) and 59 (95) and stays below
     LHH rule 1 (100), so the runbook's "why 99" still holds: WEB/API/OPS keys in the channel never create
     collaboratory tasks, and LHH links still reach rule 1 and the Part D gate.
   - Rule 63 still wins while it is enabled, because it has the lower id at equal priority. The add
     changes nothing yet.
2. `opsctl call --tool capture_rule_set_enabled --args '{"rule_id":63,"enabled":false}'`. From the next
   pass on, the new rule wins.

Disabling first would leave a window in which #a-millon falls through to the workspace catch-all, which
attributes Avviato to collaboratory, an armed project. Between steps 1 and 2 a decision records
`ambiguous=true` (two rules, two projects). That is harmless and short.

**D9: No new counter in capture's printed stats line.** SWT-74 recorded that the line is printed in six
places. The record is the column plus a reason suffix. Verification uses SQL.

**D10: Backfill recommendation: no code and no re-point pass.** Details under "Backfill".

**D11: Notifier and blank-sender DMs are unchanged.** They are not channels, so they are not flagged, and
they keep going to qwen as in 0.7.47 (requirement 4). A notifier sender in a CHANNEL who mentions him is
still admitted, and qwen decides. The prompt already calls bot notices "false".

**D12: The inquiry prompt and `inquiry-v3` are unchanged.** The model is still not told the recipient's
name (C-D3's reason: an identity in the prompt means a version bump and a re-eval). See Future work.

## Acceptance criteria

**The mention predicate (unit, `internal/connector/slackweb/mention_test.go`)**

1. `slackweb.MentionsOwner` is a table test. TRUE cases:
   - `@Salvador can you check`, `hey @Salvador Spataro`, `@SalvadorSpataro`, `(@Salvador)`;
   - `cc @salvador,`, `thanks @Salvador.`, `@Salvador's PR`, `@Salvador` + NBSP + `Spataro`;
   - text that is only `@Salvador`, and a mention on a later line of a multi-line CRLF body.

   FALSE cases:
   - `@Salvadora`, `@Salvador_bot`, `@SalvadorSpataroX`;
   - `x@salvador.com`, `a.b@Salvador.org`, `@salvador.com` at the start of the text;
   - `Salvador can you` (no @), `@here`, `@channel`, `@everyone`, `<@U0182G5UH8V>`, the empty string.

   Each case names its reason in the failure message.
2. `TestDirectMessageRuleHasOneExportedSpelling` (`dmkey_test.go`) passes UNCHANGED. Exactly one DM
   helper, and the new file declares no `Is*Direct*|*DM*` function.

**The capture fact (`internal/capture`)**

3. A pure predicate beside `direct.go` (e.g. `internal/capture/mention.go`, `channelUnmentioned(in)
   (bool, string)`, reason-bearing, first cause wins, no I/O tokens) is table-tested:
   - `attributed` + slack + not DM + not group DM + not mentioned → true.
   - Each of these → false, with its own reason: not attributed, not slack, 1:1 DM, group DM
     (`group_dm` raw type), mentioned.
4. The fact is applied EXACTLY ONCE per decided message, after the rule decision. All four external
   callers of `decideMessage` get it; `prFallThrough`'s recursive call does not apply it a second time,
   so the reason suffix appears once.
   - A unit or integration test decides a Slack channel message through a pr_review fall-through, or
     otherwise asserts the suffix count is 1.
   - When the fact is true, the reason gains the suffix
     `; a channel message that does not mention Salvador: no inquiry verdict (SWT-79)`. Otherwise the
     reason is unchanged.
5. `insertDecision` writes `channel_unmentioned`. Integration test with a live `EvaluateRules` over REAL
   rows (a real rule, project and raw item with `conversation.type`), so the column is what is tested and
   not a fixture:
   - a `public_channel` message with no mention → `attributed`, `channel_unmentioned = true`;
   - the same message text with `@Salvador` → `attributed`, `false`;
   - a person's 1:1 DM → SWT-78 `task` (unchanged), `false`;
   - a notifier (`Jira`) DM with no key → `attributed`, `false`;
   - a raw `group_dm` → SWT-78 path, `false`;
   - a gmail message attributed by a sender rule → `false`;
   - a Slack channel message on a keyed rule that derived no key → `attributed`, `true` (every attributed
     exit, the SWT-78 codex lesson);
   - a Slack channel message on a jira-keyed rule → `task`/`task_log` as today, `false`.

   Mutation: drop the raw-type column from `pendingMessageCols` → the group-DM case goes red.
6. The DB CHECK refuses `channel_unmentioned = true` with `action <> 'attributed'`. An integration probe
   inserts one and expects the constraint name in the error.
7. Shadow mode records the same fact and creates nothing. `DryRunRules` and `ExplainMessage`
   (`task_match`) show the suffix for an unmentioned channel message.

**The inboxes (`internal/replyfold`, `internal/classify`, `internal/promote`)**

8. `InquiryEligibleLatestSQL` becomes
   `((latest.action = 'attributed' AND NOT latest.channel_unmentioned) OR (live.action = 'task_log' AND live.resurface AND EXISTS (…closed…)))`.
   Still one parenthesised expression with no `mode`. `inquiryeligible_test.go`'s fragment list is
   amended deliberately; its first fragment pinned the old text. The resurface branch is byte-identical.
9. Both inboxes' `latest` LATERALs select `cd.channel_unmentioned`. A missing select is a SQL error, not a
   silent pass. The criterion-11 one-spelling scan passes: neither inbox spells the admission itself.
10. Integration, on the real SQL:
    - After a live capture pass, the unmentioned channel message from criterion 5 is ABSENT from
      `inboxWhereInquiry`, and with a `needs_reply=true` verdict fixture it is absent from promote's
      `inquiryInbox`.
    - The mentioned one is PRESENT in both.
    - A newer shadow row decides, in either direction (any-mode latest).
    - A resurfaced channel `task_log` onto a closed task is still admitted (SWT-53 unchanged).
    - The SWT-78 positive control in `internal/promote/direct_inbox_internal_integration_test.go:157-158`
      ("can someone confirm the date?" in a public channel) must gain an `@Salvador`. Otherwise it now
      correctly fails its POSITIVE CONTROL. Amending it is expected and not a regression.

**Promotion (`internal/promote`)**

11. `InquiryCandidate` gains `Mentioned bool`. `addressed()` returns true for a Slack non-DM candidate
    with `Mentioned`. Table test:
    - conversation-scope channel + Mentioned → passes `not_addressed`;
    - conversation-scope channel, not Mentioned → `not_addressed` (unchanged);
    - thread + prior post, not Mentioned → addressed (unchanged);
    - a jira or upwork candidate with Mentioned → unchanged (the clause is Slack-only);
    - DM and gmail → unchanged.
12. `inquiryInbox` selects `nm.body_text` and sets `Mentioned` through `slackweb.MentionsOwner` only.
    Integration: in an armed project, a top-level channel verdict (`ask_kind=question`,
    `needs_reply=true`, body `@Salvador can you confirm the date?`) creates ONE `ready` human task.
    Without the mention and with a legacy `false` capture row, it is gated `not_addressed`.
    Mutation: select a constant instead of `nm.body_text` → the promote test goes red. The body never
    appears in the task title, body or any log line (assert on the created task).

**Data and configuration**

13. Migration `0043_slack_channel_mentions.sql`:
    - adds the D2 column and its named CHECK;
    - inserts the D6 row with every column in D6's table named explicitly, `ON CONFLICT (slug) DO NOTHING`;
    - does not UPDATE `bulk` or any other project.

    It applies twice cleanly on a scratch DB. A guard `TestMigration0043_*` pins the file: column,
    default, CHECK, slug, `ai_locality='any'`, `ai_inquiry=true`, `inquiry_promote_after` absent or NULL,
    `client` NULL. `internal/classify/structure_test.go`'s ledger learns 43, with its note ABOVE the
    "34 is" line.
14. `bulk` is unchanged by everything this ticket ships (`ai_inquiry=false`, rules untouched), asserted in
    the migration guard.

**Production acceptance (after deploy; see Verification)**

15. `opsctl capture-rules list` shows the new a-millon rule at 99, and rule 63 disabled. The `audit_events`
    rows for both tool calls exist.
16. For 24h after go-live:
    - every Slack channel `attributed` decision written since the roll has
      `channel_unmentioned = NOT MentionsOwner(body)`, checked by the Go measurement in V5;
    - no `classify_inquiry` verdict exists on a message whose latest decision is flagged;
    - every #a-millon inbound message's latest decision names project `a-millon`, except LHH-keyed ones
      (rule 1).
17. Docs:
    - `docs/runbooks/capture-rules.md`: "#a-millon → bulk" is rewritten as "#a-millon → a-millon
      (SWT-79)", covering why 99 still holds, the D8 swap order and rule 63's disabled state. It gains a
      short "Channel mentions" section: the column, how to read it, the D5 consequence.
    - `docs/runbooks/local-classifier.md:672`'s `not_addressed` row adds "and not a Slack channel message
      that mentions him".
    - IK gains an entry under the SWT-78 one.
    - `docs/runbooks/HANDOFF-kube-slack-channel-mentions.md` is written.

## Data model changes

Migration **0043** (`migrations/0043_slack_channel_mentions.sql`), forward-only, with a header comment in
the 0034 style (what, why, default reasoning, "apply BEFORE any image built from this branch"):

```
ALTER TABLE capture_decisions ADD COLUMN channel_unmentioned BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE capture_decisions ADD CONSTRAINT capture_decisions_channel_unmentioned_is_attributed
  CHECK (NOT channel_unmentioned OR action = 'attributed');
INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify, ai_inquiry,
                      notifier_senders)
VALUES ('#a-millon (Avviato general)', 'a-millon', NULL, 'manual', 'dashboard', 'any', false, true, '{}')
ON CONFLICT (slug) DO NOTHING;
```

(Illustrative. The implementer writes the file, and the guard pins the facts in criterion 13.)
- `ADD COLUMN … DEFAULT false` is a metadata-only change on PG ≥ 11 (prod is pg17), so there is no table
  rewrite of `capture_decisions`.
- No index: the column is read only on the one latest row per message, which the LATERAL already reaches
  by `message_id`.
- No new table and no new action value.
- `inquiry_promote_after` is set by hand (D7). The rule swap is data through tools (D8).

## API / MCP tool changes

**No new tool and no changed tool schema.** The existing executor paths used:
- `capture_rule_add` and `capture_rule_set_enabled`: humanOnly, off MCP, via opsctl. Validate → policy →
  audit → handler → audit, unchanged.
- The inquiry promote path: `create_task`, `task_set_source_thread` and so on, as `promote:inquiry`,
  unchanged. Only `addressed()`'s input grows.
- `task_match` (ExplainMessage) shows the new reason suffix for free, because it runs the same
  `decideMessage`.

**Capture:** one post-decision step, in `rules_store.go` beside `decideMessage`.
- Recommended shape: rename today's body to an unexported inner function that `prFallThrough` keeps
  calling, and make `decideMessage` = inner + apply the fact. Then all four external callers get it once
  and the recursion never double-applies it.
- `ruleDecision` gains `channelUnmentioned bool`. It is a WRITTEN field, like `resurface`, not a
  carried-only one.
- The facts come from the same spellings `directFacts` uses: `pm.channel == slackweb.Channel`,
  `slackweb.IsDirectMessageKey(pm.msg.ThreadKey)` and `pm.rawConvType == "group_dm"`. Add
  `slackweb.MentionsOwner(pm.msg.BodyText)`. The subject is the channel name for Slack and is never
  searched.

## MQTT topics

None changed. `ops/pipeline/captured` still wakes `inquiry`. A pass that admits fewer messages is not a
contract change.

## Files likely to touch

- `migrations/0043_slack_channel_mentions.sql` (new)
- `internal/connector/slackweb/mention.go` (new), `mention_test.go` (new)
- `internal/capture/mention.go` (new, pure) and its `_test.go`
- `internal/capture/rules_store.go`: the `ruleDecision` field, the `decideMessage` wrapper,
  `insertDecision`'s column list
- the migration guard: in `internal/capture`, beside `TestMigration0040_CaptureCommTasksShape`'s file
- `internal/capture/*_integration_test.go`: new suite, criterion 5
- `internal/replyfold/replyfold.go` (`InquiryEligibleLatestSQL`), `internal/replyfold/inquiryeligible_test.go`
  (fragment amendment)
- `internal/classify/store.go:143-146` (the `latest` select), `internal/classify/structure_test.go` (ledger 43)
- `internal/promote/inquiry.go`: `InquiryCandidate.Mentioned`, `addressed()`, the `inquiryInbox` select and
  scan. Plus `inquiry_internal_test.go` / `inquiry_integration_test.go` and
  `direct_inbox_internal_integration_test.go` (positive-control body)
- `docs/runbooks/capture-rules.md`, `docs/runbooks/local-classifier.md`,
  `docs/runbooks/HANDOFF-kube-slack-channel-mentions.md` (new), `.claude/INSTITUTIONAL_KNOWLEDGE.md`

Not touched: `internal/classify/inquiry.go` (prompt), `internal/capture/direct*.go` (SWT-78),
`internal/capture/gate.go`, `internal/capture/route.go`, the slackweb normalizer, and every connector main.

## In scope

- The mention predicate and the capture fact on every capture-written `attributed` decision.
- The admission change in the shared inquiry predicate.
- `addressed()` counting a Slack channel mention.
- Migration 0043: the column, the CHECK and the `a-millon` project.
- The prod data steps: arm, add the rule, disable 63.
- Runbooks, IK and the kube handoff.

## Out of scope

- DMs and group DMs (SWT-78, unchanged).
- Rule-matched outcomes:
  - ticket keys → `task`/`task_log`;
  - the SWT-74 comm path;
  - SWT-54 `pr_review`;
  - the Part D gate stage's `attributed` resolutions. A gate-dropped, jira-keyed channel message still
    reaches qwen, as today.
  - the SWT-53 resurface branch;
  - route rows.
- Making mentions direct tasks. He said "qwen decides".
- Changing the inquiry prompt or its version (D12).
- `slack_watch` for #a-millon (latency). It is his data call through `opsctl slack-watch add`, and the
  rotation already reads the channel.
- The leaf's `C…` group-DM typing on targeted reads (the SWT-78 known gap). Such a message is now treated
  as a channel and needs a mention. `slack_watch` holds only 1:1 DMs, so it does not occur today.
- Any change to `bulk`, its sender rules or older #a-millon history. History keeps `bulk`, the A-D6
  precedent: no reader acts on it.
- Adjacent tempting bundles:
  - build-order step 6 (triage) and step 10 (board);
  - the conversation-scope `answered` gate in busy channels;
  - SWT-50 stuck-claim recovery.

## Invariants that apply

1. **Raw-first.** No connector or normalizer change. The mention is computed from `body_text`, which
   the normalizer derives from `raw_source_items`. Re-normalizing or re-running capture reproduces it.
   Capture reads raw only through the existing `pendingMessageCols` column (`conversation.type`).
2. **One funnel.** No task-like table. Mentions become rows in `tasks` through the existing promoter. The
   new project is a `projects` row, and #a-millon's queue is a filter (`project = a-millon`).
3. **Everything through the executor.**
   - Rule changes go through `capture_rule_add` / `capture_rule_set_enabled` (humanOnly, audited). Task
     creation goes through the promoter's executor calls as `promote:inquiry`.
   - Capture writing its own `capture_decisions` row is its existing log, not a tool, which is unchanged.
   - The project row is created by a reviewed migration, the 0016/0018 path. Arming is the documented
     C-D2 hand UPDATE.
4. **Nothing external without a delivery row.** Nothing outbound is added. Replies to a mention go through
   `send_slack_reply` (SWT-77) as before.
5. **Own-message loop closure.** Capture decides inbound only (`rules_store.go:754`). His own "@Salvador"
   is outbound and never decided. The replied-since fold is unchanged.
6. **Stealth attribution.** Nothing client-visible is produced.
7. **Orchestrator purity.** The orchestrator is untouched.
   - Both predicates are pure: `MentionsOwner`, and capture's `channelUnmentioned` with inputs as values.
   - `InquiryGate` stays pure; `Mentioned` is an input.
   - Every capture decision is a written row whose reason names the fact. A gated verdict stays
     countable by reason, as today.

## Sibling patterns to copy

- **The pure predicate plus an I/O half, reason-bearing, first cause wins:** `internal/capture/direct.go`
  / `direct_store.go` (SWT-78), `resurface.go`, `comm.go`.
- **A capture-recorded fact read only by the lanes:** migration 0034 plus `insertDecision` plus
  `replyfold.InquiryEligibleLatestSQL`.
- **A project row by migration:** `migrations/0018_bulk_project_and_classify_flag.sql`,
  `0016_provider_locality.sql` (explicit values, `ON CONFLICT DO NOTHING`, statement-order comments).
- **One slackweb spelling plus a table test that states each reason:** `normalize.go`
  `IsDirectMessageKey` / `dmkey_test.go`.
- **The integration test that proves "skips qwen" on the real SQL:**
  `internal/promote/direct_inbox_internal_integration_test.go`.
- **A kube handoff:** `docs/runbooks/HANDOFF-kube-slack-dm-tasks.md`.
- Queue claims and HTMX: not applicable.

## Verification protocol

### Before code: read-only prod pre-checks

Use `psql -h 192.168.50.49 -U ops -d ops -X`, SELECT only. Record the results in the deliver summary.

- **0a. Rules on the channel and the workspace catch-alls:**
  `SELECT r.id, p.slug, r.criteria_type, r.pattern, r.priority, r.enabled, r.external_system FROM capture_rules r JOIN projects p ON p.id = r.project_id WHERE r.pattern LIKE 'slack:T0360B84U:C1C1TSLJH%' OR r.criteria_type = 'source_slack_workspace' ORDER BY r.priority DESC, r.id;`
  Expect rule 63 → bulk at 99, and rules 8/9 at the bottom. If another rule at 99 or higher has a lower id
  than the new rule will get and matches this prefix, stop.
- **0b. The storage form of mentions:**
  `SELECT count(*) FILTER (WHERE body_text LIKE '%<@U%') AS raw_ids, count(*) FILTER (WHERE body_text ~* '@salvador') AS names FROM normalized_messages WHERE channel = 'slack' AND direction = 'inbound' AND sent_at > now() - interval '30 days';`
  Expect `raw_ids` = 0. If it is not zero, stop: D3's display-name premise is wrong.
- **0c. Another Salvador:**
  `SELECT DISTINCT sender FROM normalized_messages WHERE channel = 'slack' AND sender ILIKE '%salvador%';`
  Only his own names are expected. If another person named Salvador exists in either workspace, a bare
  "@Salvador" is ambiguous. Stop and raise it before implementing.
- **0d. What the gate removes (D5's cost).** Slack channel inquiry verdicts in the last 30 days, excluding
  DMs by raw type:
  ```
  SELECT (nm.body_text ~* '@salvador') AS mentions_approx, e.fields->>'needs_reply' AS needs_reply,
         EXISTS (SELECT 1 FROM classify_promotions cp WHERE cp.normalized_message_id = nm.id) AS promoted,
         count(*)
    FROM ai_extractions e
    JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_inquiry' AND r.status = 'ok'
    JOIN normalized_messages nm ON nm.raw_source_item_id = e.raw_source_item_id
    JOIN raw_source_items ri ON ri.id = nm.raw_source_item_id
   WHERE e.fields->>'channel' = 'slack' AND r.created_at > now() - interval '30 days'
     AND COALESCE(ri.raw_json->'conversation'->>'type','') NOT IN ('dm','group_dm')
   GROUP BY 1,2,3 ORDER BY 1,2,3;
  ```
  - The `mentions_approx = false, promoted = true` row is D5's lost recall, the tasks that would no longer
    exist. Show Salvador that number if it is more than a handful.
  - The false total is the qwen calls saved.
  - The ops-only regex is approximate; V5 below is authoritative.
- **0e. #a-millon mentions (the backfill decision):**
  `SELECT m.id, m.sent_at, m.sender, left(m.body_text, 120) FROM normalized_messages m JOIN normalized_threads t ON t.id = m.thread_id WHERE t.thread_key LIKE 'slack:T0360B84U:C1C1TSLJH%' AND m.direction = 'inbound' AND m.body_text ~* '@salvador' AND m.sent_at > now() - interval '30 days' ORDER BY m.sent_at;`
- **0f. The free catch-up.** Collaboratory channel verdicts that are `needs_reply=true`, unpromoted and
  inside 72h. Those that mention him will promote on the first `inquiry_promote` pass after the roll:
  `SELECT nm.id, nm.sent_at, e.fields->>'ask_kind', e.fields->>'thread_scope', left(nm.body_text, 100) FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_inquiry' AND r.status = 'ok' JOIN normalized_messages nm ON nm.raw_source_item_id = e.raw_source_item_id WHERE e.fields->>'needs_reply' = 'true' AND e.fields->>'channel' = 'slack' AND nm.sent_at > now() - interval '72 hours' AND NOT EXISTS (SELECT 1 FROM classify_promotions cp WHERE cp.normalized_message_id = nm.id) ORDER BY nm.sent_at;`
- **0g. Projects:**
  `SELECT id, slug, client, ai_locality, ai_classify, ai_inquiry, inquiry_promote_after, notifier_senders FROM projects WHERE ai_inquiry OR slug IN ('bulk','collaboratory','a-millon');`
  Expect no `a-millon` row, `bulk` unarmed, and only collaboratory armed.
- **0h. #a-millon senders, 30 days:**
  `SELECT m.sender, count(*) FROM normalized_messages m JOIN normalized_threads t ON t.id = m.thread_id WHERE t.thread_key LIKE 'slack:T0360B84U:C1C1TSLJH%' AND m.direction = 'inbound' AND m.sent_at > now() - interval '30 days' GROUP BY 1 ORDER BY 2 DESC;`
  Look for bots. Any bot that mentions him goes to Future work, not into this ticket.

### Before commit

- **V1:** `go test ./...` is green, apart from the known `TestAttributionTrend_*` 20:00-24:00 EDT flake;
  run with `TZ=UTC` if needed.
- **V2:** `make integration` against a branch-owned database. The compose Postgres is shared, so create
  it first: `CREATE DATABASE ops_chanmention`, then
  `make migrate LOCAL_DB_URL=…/ops_chanmention`. Run `go test -tags integration -p 1 ./...` with
  `DATABASE_URL` pointed at it.
- **V3:** record the mutations:
  - drop the column from `pendingMessageCols` → the group-DM case goes red (criterion 5);
  - select a constant for `nm.body_text` → the promote mention test goes red (12);
  - remove `AND NOT latest.channel_unmentioned` → the inbox-absence tests go red (10);
  - move the fact application into `prFallThrough`'s path → the suffix-count test goes red (4).
- **V4:** apply 0043 twice on the scratch DB (idempotent), then check `\d capture_decisions` and the
  project row.
- **V5: measure the predicate in Go over production bodies, never Postgres regex (the SWT-70 rule).**
  1. Export 30 days of inbound Slack channel bodies with ids, read-only
     (`\copy (SELECT …) TO '<scratchpad>/slack_bodies.csv' CSV`).
  2. Run a throwaway Go test or program calling `slackweb.MentionsOwner` over them.
  3. Report true/false counts, list every true, and eyeball 30 random falses that contain "salvador" in
     any case.

  Expect about 21 trues (Salvador's 30-day figure). Every miss or false positive is explained before
  commit.
- **V6:** run `/ticket-review slack-channel-mentions` (go-reviewer). This touches capture's admission
  logic, so an optional codex adversarial pass is warranted.

### Deploy (the kube session owns manifests; see memory "Kube manifests belong to the kube session")

1. **Migration 0043 FIRST**, applied by the switchboard session from here (the 0042 handoff precedent).
   New binaries write and select the column. On a DB without it, every connector's capture pass fails and
   both inquiry inboxes error.
2. **Roll ONE tag to all 13 workloads in ONE apply**, as SWT-78's handoff did (5 Deployments including
   `connector-slackweb-watch`, and 8 CronJobs). Reinstall `opsctl` (and the .30 copy of `ops-mcp-user` if
   `task_match` explanations matter there).
   - An old capture binary on the new DB records `channel_unmentioned = false`. That is today's
     behaviour, not a regression, until step 4. After step 4 it would send all of #a-millon's chatter
     to qwen.
   - SWT-76's rule applies: `SELECT count(*) FROM deliveries WHERE status='sending' AND send_queued_at IS NOT NULL AND send_settled_at IS NULL`
     must be 0 before the watcher pod is replaced.
3. **Arm:** `UPDATE projects SET inquiry_promote_after = now() WHERE slug = 'a-millon';` (D7), and
   re-read 0g.
4. **Rule swap (D8):** first `opsctl capture-rules try --project a-millon --type thread_key_prefix --pattern slack:T0360B84U:C1C1TSLJH --priority 99 --since 72h --show wins`
   (a dry run: expect every a-millon message to be won by rule 63 still, since it has the lower id). Then
   `add`, then disable 63. Check `opsctl capture-rules list`.

### Usable-alone smoke (after go-live)

- **S1 (immediate):** `opsctl task-match --message <id>` (or the MCP `task_match`) on one unmentioned
  collaboratory channel message from today shows the SWT-79 suffix. On one of 0e/0f's mention ids it
  shows none.
- **S2 (within about 30 min, one rotation):**
  - new #a-millon messages have latest decisions naming `a-millon`, with `channel_unmentioned` true on
    chatter;
  - `SELECT count(*) FROM capture_decisions cd JOIN ai_extractions e ON e.raw_source_item_id = cd.raw_source_item_id JOIN ai_runs r ON r.id = e.ai_run_id AND r.worker_type = 'classify_inquiry' WHERE cd.channel_unmentioned AND r.created_at > cd.created_at;`
    = 0 (no qwen verdict after a flag).
- **S3:** 0f's mentioned verdicts, if any, become `ready` human tasks in collaboratory on the first
  `inquiry_promote` pass. That is the live proof of D4. If 0f is empty, the first real channel mention
  (about 0.7/day) is the proof: a verdict, then a task if it is an ask. Watch `pipelined`'s
  `inquiry_promote` log: `gated=map[… not_addressed:…]` should not count it.
- **S4 (24h):** criterion 16's checks, including V5 re-run over the day's bodies against the recorded
  column.

## Amendments after review (2026-09-23)

- **Edits (codex, two rounds; go-reviewer race):** each capture pass recomputes `channel_unmentioned` from the
  CURRENT text for every Slack message ingested in the last hour (its latest `attributed` decision of the
  pass's mode), in both directions, with no decision-timestamp comparison. 0043 adds
  `raw_source_items_ingested_at_idx`. Tests: `…EditAddingTheMentionClearsTheGate`,
  `…EditRemovingTheMentionShutsTheGate`, `…EditRacingTheDecisionIsStillRechecked`.
- **Gate/route rows (codex):** accepted residual, not fixed here — 0 Slack gate/route rows in 60 days; default
  false means an extra qwen call at worst, and promote still requires a mention to call a channel addressed.
- **Accepted residual (codex round 4, medium):** the Slack normalizer does not take capture's lock, so an edit
  normalized DURING a capture pass can leave a stale fact until the next pass (minutes: the recheck covers the
  last hour). Self-healing; closing it fully means serializing normalization with capture — not this ticket.
- **URL paths (go-reviewer):** a `/` before the `@` is not a mention (`medium.com/@salvador/post`).
- **V5 (real-body check) not run:** the harness denied exporting production message bodies to disk; the
  50-case table built from production's observed forms stands in.

## Rollback (mildest first)

1. **#a-millon only:** `UPDATE projects SET ai_inquiry = false, inquiry_promote_after = NULL WHERE slug = 'a-millon';`
   It stops classifying and promoting there, and attribution stays.
2. **Undo the rule swap:** re-enable rule 63 (`capture_rule_set_enabled` true). At equal priority its
   lower id wins at once. Then disable the new rule. Both steps are audited. Old decisions keep
   `a-millon`, and no reader acts on them.
3. **Code:** roll all 13 workloads back to 0.7.47 together. **Do 1 and 2 first:** with the old binary,
   #a-millon on an armed project would send every message to qwen. The 0043 column and project row stay
   (forward-only) and are inert to the old binary: its writes default the column to false, which means
   pre-ticket behaviour.
4. Tasks already created stay. They are ordinary human tasks; dismiss them on the board if unwanted.

## Backfill (D10)

- **Collaboratory channel mentions** that qwen flagged but promote gated `not_addressed` inside the 72h
  fence promote on their own at the first pass after deploy (0f lists them). Nothing to do.
- **Channel mentions qwen said no to** are not re-asked. There is no re-classify path, and a new
  extraction would need one.
- **#a-millon mentions** before go-live have a live `attributed → bulk` decision forever. The only lever is
  the shadow re-point (`opsctl capture-rules run --since 72h --all`). It rewrites the latest decision for
  EVERY inbound message in the window to rescue what 0e suggests is at most a couple of messages, and it
  must run from the new `opsctl` or it floods qwen. **Recommendation: do not run it.** Put 0e's list inside
  72h into the deliver summary so Salvador can `swb add` any he still owes, and treat history as settled.

## Future work (not this ticket)

- **Thread participation:** admit "mentioned OR a thread he posted in before", if 0d shows D5 costs real
  asks. It is one extra input to the capture predicate: prior outbound on the thread, the fact
  `replyfold.PriorParticipationCol` computes.
- **`<@U…>` mentions** from his per-workspace `own_user_id`, if the leaf ever emits raw ids.
- **Tell qwen who the recipient is,** so "@Salvador …" is not read as "addressed to someone else". This
  needs an `inquiry-v4` bump and a re-eval.
- **The conversation-scope `answered` gate in busy channels:** any later post by him in #a-millon blocks
  a top-level mention.
- **The flag on gate and route rows** for Slack channel messages.
- **A capture stats counter** for flagged messages (six print sites).
- **`slack_watch` for #a-millon** if 30-minute latency matters.
