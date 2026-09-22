> Jira: SWT-74

# comms-inbox — a person's comm becomes its own INCOMING task, and Claude gets a matcher tool to route it

**STATUS: DECIDED.** No open questions arose; every choice is under "Decisions" with its rationale,
and the ones taken without asking are flagged **(unilateral)**. Builds directly on SWT-72
(`activity-resurfaces`, shipped 0.7.39, main at `6fa8f02`) and does not re-open any of its decisions.

**Evidence status.** Every code fact below was read in this worktree at `6fa8f02`, with file and line.
This spec session ran NO SQL. Every production assumption is a read-only pre-check in Verification
Step 0 with a stated gate; a gate that fails means stop and re-spec, not adapt the code quietly (the
SWT-54 / SWT-59 / SWT-72 precedent).

## Source

Ad-hoc, from Salvador, 2026-09-22 (swb task #479), verbatim, in the order he said it:

> I want to see comms as incoming coms can come from slack,mail or jira then I'll route to the tasks
> or dismiss. I would like a help for claude to match a particulat line to a task like a tool to
> access/excercise the matcher rules

> you don't need a message contruct they can still be task. It's just that the tast is just answer
> questions or update other tasks.

> yes, spec it after #477

## The gap this closes

SWT-72 fixed the *silence*: a rule-filed comment now moves its ticket task into INCOMING. It did not
change *what the row is*. Today, when capture's `actionTaskLog` branch files a person's message onto
an existing open task (`internal/capture/rules_store.go:499-519`):

- Katie's Jira comment on WEB-10470 → a log line on task #73 **and task #73 jumps to INCOMING**;
- José's direct mail quoting WEB-10469 (rule 75, `body_regex`) → a log line on task #452 **and task
  #452 jumps to INCOMING**.

One tap (Requeue) reviews it, and the person's words live only as a line inside someone else's row.
Salvador wants the comm itself to be the row: "incoming coms can come from slack, mail or jira then
I'll route to the tasks or dismiss", and the task's job is small — "answer questions or update other
tasks". SWT-72 D11 already does exactly this for an inquiry-lane ask (`internal/promote/promote.go`,
`Decision.RelatedTaskID`); this ticket extends it to the rule-filed comms D12 says the inquiry lane
can never see.

Meanwhile the same branch carries pure NOISE that must keep filing silently: Jira field/status-edit
notices (IK SWT-45: 182 of 208 `Anonymous (JIRA)` Treetop mails), his own GitHub PR-review mail
(SWT-72 Step 0a measured 25 in 14 days), Upwork system messages, CI mail. A comm task for each of
those is a worse board than today's.

## Goal

Three halves, all deterministic, no model anywhere:

- **The comm is the row.** A `task_log` attach by a rule that is ARMED for comms creates its OWN
  human task in INCOMING — sender in the title, the message's words in the body, `related_task: N`
  pointing at the task the rule would have filed it onto — instead of surfacing that task.
- **Noise keeps filing silently.** Un-armed rules behave exactly as they do today (SWT-72's mark on
  the target). An armed rule still refuses a blank sender, a `projects.notifier_senders` sender, and
  Jira's `Anonymous (JIRA)` placeholder (his own edits).
- **Routing is one verb and one read.** `task_match` runs capture's OWN matcher over a message, a
  task or a pasted line and proposes the task it belongs to, with the rule that matched and why;
  `task_attach` routes the comm onto that task through the executor and closes it.

**Usable alone means:** with migration 0040 applied, one image rolled and ONE rule armed
(`opsctl capture-rules add … --comm-task`, or `UPDATE capture_rules SET comm_task = true WHERE id =
75`) — José's next mail about WEB-10469 appears as its OWN row at the top of INCOMING, remark
`new email`, `from José Garcia <jose.g@avviato.com>`; task #452 stays in QUEUE with the log line plus
a one-line pointer; from any Claude Code session `swb match <id>` answers "task 452, rule 75,
body_regex, jira WEB-10469" and `swb attach <id> 452` puts the pointer on 452 and closes the comm.
Nothing else on the board changes, because a comm task surfaces through SWT-72's own columns.

## What exists (code-read, with file and line)

- **The one attach branch.** `internal/capture/rules_store.go:499-556`: `appendRuleLog` (`:1529`),
  then SWT-72's `markRuleActivity` (`:1665`) unless `decision.prClose`, then exactly one of
  `closeRuleTask` / `reviveRuleTask` / `reopenRuleTask`. Every rule kind and every connector funnels
  through it — `decideMessage`'s `found` branch (`:905-984`).
- **The create branch beside it.** `:459-498`: `createRuleTask` → `recordDecisionTask` →
  `linkRuleRef` → `setRuleProvenance` → (optionally) `markRuleSurfaced`. Ordering discipline stated
  in the comments: record the id on the decision BEFORE the follow-ups, because the live claim is
  spent and a later failure must not lose the pointer to what was created.
- **The claim.** `capture_decisions_live_uniq` (partial, `WHERE mode='live'`); `insertDecision`
  (`:1368`) restates the predicate in its `ON CONFLICT`. One live decision per message, forever — a
  replay creates nothing.
- **The per-rule flags, all COLUMNS loaded with the rules.** `storedRule` (`:238-267`): `revive`,
  `addressed`, `gateOn`, `prReview`, `excludePRAuthors`, `notifiers`
  (`projects.notifier_senders`); `loadRules`'s SELECT is at `:598-603`.
  `internal/capture/resurface_structure_test.go:276-300` pins `notifier_senders` to ONE reader.
- **The pure "is this a person's new words" rule already exists, for CLOSED tasks.**
  `internal/capture/resurface.go:46-64` `resurfaces()`: not closed / activity / dismissed /
  connector copy / PR state notice / `notifierSender` / `blankSender`, each returning its own reason
  fragment. `notifierSender` (`:78`) is equality against the whole sender or its parsed address;
  `blankSender` (`:68`) fails closed.
- **The pure Jira-From parse.** `internal/capture/ownaction.go:103-139`: `jiraNotificationActor`,
  `namedJiraActor`, `sameJiraActor`, and the two template literals `(JIRA)` / `Anonymous`. IK SWT-45:
  `Anonymous (JIRA)` is Jira's placeholder for HIS OWN changes, and 182 of 208 such Treetop mails are
  field/status-edit notices the own-action guard cannot attribute.
- **The evaluator.** `internal/capture/rules.go` `Evaluate` (`:168`), `externalKey` (`:275`),
  `Match` (`:137`) — pure, no I/O, structure-scanned. `decideMessage`
  (`rules_store.go:776`) is the impure decision: it reads `taskForExternalRef` (`:1317`), the
  own-action facts and the PR-mail trust, and WRITES NOTHING. `pendingMessages` (`:651`) is the one
  spelling of the message projection capture evaluates.
- **SWT-72's tools.** `internal/tools/activity.go` `task_mark_activity` — spine-facing, off both MCP
  profiles, errors on a non-inbound message (invariant 5), skips a closed task, same-message no-op,
  writes `activity_at` / `activity_by_message_id` and nothing else.
  `internal/tools/requeue.go` `task_requeue` — humanOnly, both profiles.
- **SWT-72's board, which this ticket reuses verbatim.** `internal/dashboard/board.go:425-431`
  computes `needs_review`, `activity_channel`, `activity_sender`, `activity_stamp` through a PK
  `LEFT JOIN normalized_messages nm ON nm.id = t.activity_by_message_id`; `board.go:243-255` sets
  `Incoming = incomingKind(f.FromMessage, f.PRReview, f.NeedsReview)` and overrides the remark with
  `activityRemark(f.ActivityChannel)`; `sections.go:101-158` ranks incoming by light → needs-review →
  kind → `ActivityStamp` DESC → id DESC.
- **The close transition.** `internal/tools/close.go:53-141` `closeTransition` — the ONE writer of
  `status='closed'`: row lock, active-work refusal (`activeWorkRefusal`, `:40`), the in-flight-send
  fence, `reviewed_at = now()` on the close only (SWT-72 D8), one `status_changed` event. Runs inside
  the caller's transaction.
- **The log verb.** `internal/tools/appendlog.go:47-69` writes a `log` `task_events` row; the user
  MCP profile pins it to HUMAN tasks (SWT-38 C4) because a claude task's log feeds a worker prompt.
- **`tasks.source_thread_id` is deliberately not unique** (`migrations/0019_delivery_provenance.sql:3-10`)
  and has no index; `external_refs` is `UNIQUE (system, external_key)` and `taskForExternalRef` takes
  `ORDER BY r.created_at DESC, r.id DESC LIMIT 1`.
- **Per-rule flag precedent, schema side.** `migrations/0035_capture_rules_pr_review.sql`: two
  columns, two CHECKs, fail-closed on a NULL `external_system`, "rules are armed by
  `capture_rule_add` (the executor), never by a migration".
- **The counter line, five copies.** `cmd/connectors/{google,jira,slackweb,upworkcrm}/main.go` and
  `cmd/opsctl/main.go:723` each print `RulesStats` field by field.

## Decisions

### D1 — The split is a per-rule COLUMN, `capture_rules.comm_task`, not a classifier and not a per-project switch (unilateral)

A comm task must be created for a person's message and not for a notice. The candidate
discriminators, and why this one wins:

- **A model verdict** (a classify lane) — refused by construction: build-order step 6's precedent
  demands shadow mode plus a measured precision, the local lane's measured precision is 0.50 (IK
  SWT-68), and "half the board is noise" is the failure this ticket exists to prevent. The spine
  stays deterministic.
- **A body-shape test** ("does this mail carry comment text?") — refused. Jira's comment mail and its
  field-edit mail come from the SAME sender with the same template family; telling them apart is
  prose parsing, and the IK records three costumes of the same bite (an inert time floor, a constant
  discriminator, a guard whose column no query selected). `classify.StripQuotedHistory` (SWT-70) cuts
  quoted REPLY history and says nothing about a notification template, so it does not apply.
- **A per-project column** (`projects.comm_tasks`, the `ai_classify` / `ai_inquiry` shape) — refused:
  a project is exactly the wrong grain. Treetop's traffic is one project and holds both José's asks
  (rule 75) and his own GitHub PR mail; arming the project arms both.
- **A per-rule column** — chosen. A capture rule already names a traffic SHAPE ("mail whose body
  carries a `WEB-NNNNN` key", "GitHub PR notification mail", "the Jira connector's own copies"), which
  is precisely the grain at which "person's comm" versus "notice" is decidable. It is data: arming and
  disarming are `capture_rule_add` / one UPDATE, reversible in a second, with no image roll. It
  defaults FALSE, so the rollout is opt-in rule by rule with a measurement behind each (Step 0a), and
  a wrong call costs one Dismiss tap, never a flood.

```
capture_rules.comm_task BOOLEAN NOT NULL DEFAULT false
```

**"Everything on" is one statement** (`UPDATE capture_rules SET comm_task = true WHERE external_system
IS NOT NULL AND NOT pr_review`) if he decides the per-rule grain is fussier than he wants. Nothing in
the code assumes a small armed set.

**Schema-level refusals, not Go predicates** (0040, the 0035 shape):
`CHECK (NOT comm_task OR (external_system IS NOT NULL AND NOT pr_review))`.
`external_system IS NOT NULL` because a keyless rule can never reach the `task_log` branch
(`decideMessage:798-812`) — an armed keyless rule would be an inert flag. `NOT pr_review` because
GitHub notification mail is a notice stream, has its own review tasks (SWT-54), and his own PR mail
is 25 of the 14-day sample. Both are also refused by `validateCaptureRuleAdd` with a named error, the
way `pr_review` + `revive` already is (`internal/tools/capturerules.go:165-181`).

### D2 — The per-message disqualifiers are a pure function in `internal/capture/comm.go`, modelled line for line on `resurfaces()`

```go
type commInput struct {
	armed       bool   // the winning rule's capture_rules.comm_task
	status      string // the linked task's tasks.status, from taskForExternalRef
	blankSender bool   // blankSender(sender)            — resurface.go's spelling
	notifier    bool   // notifierSender(sender, winner.notifiers) — resurface.go's spelling
	ownJiraEdit bool   // anonymousJiraActor(sender)     — ownaction.go's spelling
}
func commTask(in commInput) (bool, string)
```

False, first cause wins, each with its own reason fragment for `capture_decisions.reason`:

1. `!armed` — "the matched rule is not a comm rule";
2. `status == "closed"` — "the task is closed; SWT-45's revive and SWT-53's resurface own it";
3. `blankSender` — fails closed, `notifierSender` is an equality and can never match `""` (CC4b's
   argument, verbatim);
4. `notifier` — the project's notifier list;
5. `ownJiraEdit` — Jira's `Anonymous (JIRA)` placeholder, i.e. HIS OWN change (SWT-72 D10's named
   residual, closed here for armed rules).

`anonymousJiraActor` lives in `ownaction.go` beside its siblings and is the existing parse, not a new
literal:

```go
func anonymousJiraActor(from string) bool {
	name, ok := jiraNotificationActor(from)
	return ok && sameJiraActor(name, jiraAnonymousActor)
}
```

It bites only on a `(JIRA)`-shaped From, so José's `José Garcia <jose.g@avviato.com>` and every Slack
display name pass it untouched.

**Three clauses `resurfaces()` has that this deliberately does NOT have, each because it would be
inert** (the IK's constant-discriminator landmine):

- `dismissed` — `taskForExternalRef` (`:1325`) joins `task_dismissals` under `AND t.status='closed'`,
  so an OPEN task never carries one. Clause 2 already excluded every closed task.
- `connectorCopy` (`channel == jira`) — J3's structural dedup (the connector polls WHOLE projects; the
  notification mail is scoped to the tickets he is involved in) is enforced by NOT ARMING the
  connector rules. A Go clause for it would be false on every armed rule that exists, which is a
  predicate that goes green on a hand-written fixture and changes nothing.
- `prNotice` / `prClose` — impossible under the 0040 CHECK, which is a data-level guarantee rather
  than a Go one.

### D3 — An armed comm REPLACES SWT-72's mark on the target; the target keeps its log line and gains an ids-only pointer

One message, one row on the board. In the `actionTaskLog` branch, live mode:

| step | call | why in this order |
|---|---|---|
| 1 | `appendRuleLog` (unchanged, byte-identical text) | the words belong in the ticket's log, where a session working it reads them; a crash after it leaves exactly today's behaviour |
| 2 | `createCommTask` → `create_task` | the comm |
| 3 | `recordDecisionCommTask` → `capture_decisions.comm_task_id` | the claim is spent; a later failure must not lose the pointer to what was created (`recordDecisionTask`'s argument, verbatim) |
| 4 | `setRuleProvenance(commID)` → `task_set_source_thread` | without it `draft_delivery` refuses the task, and "answer the question" is the point |
| 5 | `markRuleActivity(commID)` → `task_mark_activity` | this, and only this, is what puts the comm in INCOMING |
| 6 | `appendCommPointer(targetID)` → `task_append_log`, ids only | last, D11's ordering: a crash before it leaves a complete, visible comm task |

and `markRuleActivity` on the TARGET is skipped. Every call fails the pass on error (the
`linkRuleRef` policy); the decision row records how far it got.

**Why the target is not also surfaced:** D11's sentence, unchanged — "the new task is the thing to
look at, and surfacing the old one too would double the rows". Arming rule 75 therefore MOVES José's
mail from "task 452 jumps to INCOMING" to "a new row in INCOMING, 452 stays in QUEUE"; that swap is
the whole ticket.

**Why the full log line stays on the target** (unlike D11, which logged ids only): `appendRuleLog`
already ships, its text is pinned by capture's suites, and the ticket task's log is where a worker
session reconstructs the ticket's history. The added cost is that the words exist twice (the target's
log and the comm's body) — accepted, and cheaper than losing them from either place.

**The pointer text**, ids only, no title, no sender, no message text — which is what makes it safe on
a claude task (the C-D13 worry):

```
capture: comm #<new id> created from this message (message <M>)
```

### D4 — What the comm task is (unilateral)

Through `create_task` on the executor, as `capture:{connector}` — the same actor, validator and audit
path `createRuleTask` uses:

| field | value | source |
|---|---|---|
| `project` | `winner.rule.Project` | the RULE's project, `ruleCreateTaskArgs`'s spelling, even if the target task's project differs |
| `subproject` | `winner.subproject` | same |
| `assignee_type` | `human` | it is his to answer; no worker console may claim it |
| `status` | `ready` (create_task's default) | SWT-72's correction to D6: `holding` would need a Requeue to become workable, and INCOMING shows it either way |
| `priority` | 0 | `ruleCreateTaskArgs`'s value; `task_set_priority` reorders it |
| `title` | `commTaskTitle` = `{sender}: {subject else first line}`, cut with `textmatch.NormalizedPrefix(…, rulesTitleLen)` | the `inquiryTitle` shape (C-D9), with `ruleTaskTitle`'s fallbacks (project name → slug → key) so it is never empty |
| `body` | `ruleTaskBody` + one line | below |

**The body is `ruleTaskBody`'s, plus `related_task: N` as the LAST key/value line**, before the blank
line and the preview. `ruleTaskBody` gains a `relatedTaskID int64` parameter emitted only when
non-zero, so every existing body stays byte-identical — CC6's "one line, only when set" precedent.
The line goes with the other keys rather than at the very end because what follows the blank line is
free text, and a `key: value` after a 400-character preview is unreadable. The key name is D11's
(`related_task`), one vocabulary for both paths.

**No `external_refs` row.** `link_external_ref` is NOT called for a comm task, and this is
load-bearing rather than an omission: the key already belongs to the ticket task, `external_refs` is
`UNIQUE (system, external_key)`, and `taskForExternalRef` takes the NEWEST ref for a key — a second
ref row would silently hijack every future attach for that ticket onto the comm task. A structure
test bans the call from the comm path.

**No `parent_id`, no `task_dependencies`** — D11's reasons, verbatim: `parent_id` carries plan
ordering and lifecycle meaning, and nothing here is blocked.

### D5 — The comm task reaches INCOMING through SWT-72's own columns, so the board is UNTOUCHED

`task_mark_activity(comm_task_id, message_id)` sets `activity_at` / `activity_by_message_id` on the
NEW task. Then, with no dashboard change at all:

- `needs_review` is true → `incomingKind(false, false, true)` = `activity` → `boardSectionOf` puts it
  in **arrivals — incoming**;
- the remark is `activityRemark(nm.channel)` → `new comment` (jira) / `new email` (gmail) /
  `new slack` (slack) / `new message` — exactly the words he asked for;
- the title cell carries the muted `from {{.ActivityFrom}}` span (`nm.sender`);
- ordering is SWT-72 D5's: needs-review rows lead, newest `activity_at` first;
- **Requeue**, **Done** and **Dismiss** already clear it (`reviewed_at`, or `closeTransition`'s stamp).

**Precedent that this is the honest meaning, not a trick:** `markRuleSurfaced` (`:1693`) is already
called on a task capture has *just created* (SWT-45 J7). A created-by-this-message task has activity
by this message.

**No new incoming kind for "comm" versus "activity on a ticket task"** (unilateral): the two look
identical on purpose — both are "someone said something, look". Distinguishing them needs a new
stored fact and changes nothing he does. Future work if the mix ever confuses him. The one cosmetic
cost, accepted: a comm row shows the sender twice (in the title he can read after review, and in the
muted span that disappears after it).

### D6 — `task_match`: capture's OWN decision, run read-only, for a message, a task or a pasted line

`{message_id | task_id | text}` (exactly one) `+ {project?, limit?}` → ranked proposals. Read-only:
it writes nothing but its audit row (`task_list`'s shape — not humanOnly, not snapshotGated).

**It must not be a second matcher.** The tool calls one new exported entry point in capture, which
runs `decideMessage` itself in `RulesModeShadow` — the function that decides every live attach —
and returns a value struct:

```go
// internal/capture/explain.go
type Explanation struct {
	MessageID int64
	Action    string // unmatched | attributed | task | task_log | held
	Project   string
	RuleID    int64
	RuleKind  string
	System    string
	Key       string
	TaskID    int64  // the task the rule would file onto (action task_log)
	Reason    string // decideMessage's own reason string
	Deferred  bool
}
func ExplainMessage(ctx, pool, messageID int64) (Explanation, error)
func ExplainText(ctx, pool, msg Message) (Explanation, error)
```

- `ExplainMessage` loads the rules with `loadRules`, builds the `pendingMessage` with the SAME column
  projection `pendingMessages` uses (factored into one `const pendingMessageCols` + one scan helper,
  pinned by a structure test) and calls `decideMessage(…, RulesModeShadow, …, nil)`. It is
  direction-blind and horizon-blind — the caller is asking a question, not running a pass — and
  reports the direction so a session can see that an outbound message is not a comm.
- `ExplainText` is the "match a particular line" case: it evaluates `capture.Evaluate` over a
  caller-built `Message` and resolves the derived key through `taskForExternalRef`. It cannot run the
  own-action guard or the PR-trust check (both need a stored message), so its `Explanation.Reason`
  says so and the tool marks the proposal `partial: true`.
- `{task_id}` resolves to `tasks.activity_by_message_id` — which this very ticket sets on every comm
  task. A task with none is refused by name ("task N carries no activity message; pass message_id or
  text"), never guessed from the thread.

**Two proposal sources, both evidence, ranked in this order:**

| source | query | rank |
|---|---|---|
| `rule_ref` | the `Explanation`'s `TaskID` (the rule's own external-ref resolution) | 0 |
| `source_thread` | open tasks whose `source_thread_id` is the message's thread, oldest first (`threadTask`'s shape, NOT project-scoped: a thread is a conversation and the human decides) | 1 |

**No recency source** (unilateral). "The 10 most recent open tasks in the project" is not evidence,
it is a list that looks like matches; `task_list(project=…)` already returns it and is on both
profiles. Future work if the two evidence sources prove too thin.

Each proposal carries `{task_id, title, status, assignee_type, project, source, rule_id, rule_kind,
external_system, external_key, why}`, where `why` is capture's own reason string. Titles only, never
bodies (`task_list`'s rule: rows in a model context cost tokens; `task_context` is the per-task read).
`limit` defaults to 5, max 20. `matched:false` with an empty list still returns the reason ("no
enabled rule matched"), so a session can say why it has nothing.

**Profiles: BOTH** (`agentTools` + `userProfileTools`). A worker console asking "which task does this
line belong to" cannot act on the answer — `task_attach` is humanOnly — so this does not let it
choose its own work.

### D7 — `task_attach`: route the comm onto a task and close it, in one audited transaction

`{task_id, target_task_id, note?}` → `{task_id, target_task_id, attached, closed, skipped?}`.
humanOnly (interactive `mcp:manual:salvo`, `dashboard:`, `opsctl:` pass; every worker console and the
orchestrator are refused), both MCP profiles, beside `task_requeue`.

One transaction, locking the LOWER task id first and then the higher (two concurrent attaches in
opposite directions must not deadlock; `lockTask` re-taken inside `closeTransition` is a no-op):

1. refuse `task_id == target_task_id` (validation);
2. refuse a CLOSED target by name — "task_reopen is the verb for that": routing live work onto a
   closed task is a mistake, and nothing would ever read it;
3. **dedup**: the source already carries an `attached` event naming this exact target →
   `{attached:false, skipped:"already_attached"}`, a success. Idempotent by the (source, target) pair,
   which also makes a second, DIFFERENT target legal — he can route one comm onto two tasks, and the
   second call finds the source already closed and skips only the close;
4. on the TARGET: one `log` `task_events` row, **ids only** — `attached: task #<source> (message
   <M>)`, the message omitted when the source has none;
5. on the SOURCE: `closeTransition(tx, source, "closed", "routed to task #<target>: <note>")` — the
   ONE status writer, so the active-work refusal, the in-flight-send fence, `closed_at`,
   `closed_from_status`, the session-state clear and SWT-72's `reviewed_at` stamp all come for free.
   An already-closed source is `closeTransition`'s idempotent no-op;
6. one `attached` `task_events` row on the SOURCE, payload `{target_task_id, note}`.

**The note never reaches the target** (unilateral). It rides the source's close reason and the
source's own event. Rationale: `task_append_log` is pinned to HUMAN tasks on the user profile
(SWT-38 C4) precisely because a claude task's log feeds a worker prompt, and a `note` is words a
session may have composed from untrusted input. Ids-only on the target makes the verb
unconditionally safe on a claude task, with no assignee branch — and an assignee branch would be one
more predicate that is false for almost every row (the inert-predicate landmine).

**The target is NOT surfaced into INCOMING** (no `activity_at` write): he just looked at the comm and
decided where it belongs; re-raising the destination is the double-row D11 refused. It also keeps
`task_mark_activity`'s contract intact — that tool takes an inbound MESSAGE, and an attach is a human
act.

**Deliberately general:** nothing in the verb is specific to comm tasks. "Route this task onto that
one and close it" is the same act for a duplicate or a mis-filed capture. The blast radius equals the
board's existing Done button, it is audited, and `task_reopen` undoes it.

**Not a `task_dismissals` row.** A routed comm was CORRECT — it just belongs with other work. The
four reason codes are training labels for the inquiry lane (IK SWT-68), and writing `handled_elsewhere`
here would mint mislabelled data. A close with a reason is the honest record.

### D8 — The first message about a NEW key does not get a comm task

When no task exists for the key, `decideMessage` takes the `actionTask` branch and the person's
message ALREADY becomes a task — the ticket task. A second, comm task would duplicate it. So the comm
path lives strictly in the `found` / `task_log` branch. The consequence, accepted and named: a
capture-created ticket task still lands in QUEUE rather than INCOMING, exactly as today. Surfacing
those is SWT-45's `surface` machinery and is Future work.

### D9 — Orchestrator purity (invariant 7)

No orchestrator rule reads `comm_task`, `comm_task_id` or the new event type. `create_task`,
`task_append_log`, `task_set_source_thread` and `task_mark_activity` are called exactly as capture
calls them today. The one new event type, `attached`, falls into `Evaluate`'s nil default
(`internal/orchestrator/rules.go:124-129` fires only on `status_changed` with
`to ∈ {delivered, closed}`), and `task_attach`'s close goes through `closeTransition`, whose
`status_changed {to:"closed"}` is the event R1/R2/R8 already handle for every close in the system.
A structure test asserts `internal/orchestrator` mentions none of the new names.

### D10 — Accepted residuals, measured before the roll

- **A named actor's Jira field-edit notice still becomes a comm task.** `Katie Evans (JIRA)` sends
  both comments and "changed the Status" notices, and nothing deterministic separates them. Cost: one
  Dismiss tap each. Step 0c measures the share; Step 0a's gate stops the roll if an armed rule's daily
  volume is too high. The narrowing, if it is ever needed, is RULE DATA (a tighter `body_regex` on a
  sibling rule armed for comms) or Future work's body-shape test — never a widening of this code.
- **Two copies of one Jira comment.** The connector copy (channel `jira`) and the notification mail
  can both hit the `task_log` branch for one comment. Only the armed rule creates a comm, so the dedup
  is structural (J3's argument). Step 0b measures how often both exist so the arming choice is made on
  data, not on the assumption.
- **The words exist twice** for an armed rule (the target's log line and the comm's body) — D3.

## Acceptance criteria

### Part 1 — schema

1. **Migration `0040_capture_comm_tasks.sql`** adds `capture_rules.comm_task BOOLEAN NOT NULL DEFAULT
   false` with `CHECK (NOT comm_task OR (external_system IS NOT NULL AND NOT pr_review))`, and
   `capture_decisions.comm_task_id BIGINT REFERENCES tasks(id)` with
   `CHECK (comm_task_id IS NULL OR action = 'task_log')`. No index, no backfill, no other table
   touched, no rule armed by the migration (0035's sentence). Its header states the rollout barrier
   (Verification Step 5).

### Part 2 — the pure split

2. `internal/capture/comm.go` declares `commInput` and `commTask(commInput) (bool, string)` with D2's
   five clauses in order, each returning a distinct reason fragment. The file is pure: a structure
   test bans `pool`, `Query(`, `Exec(`, `os.Getenv`, `time.Now` and every provider import from it
   (`resurface_structure_test.go:40-60`'s shape).
3. `anonymousJiraActor` lives in `ownaction.go`, is built from `jiraNotificationActor` and
   `sameJiraActor`, and introduces NO new string literal. Unit table: `"Anonymous (JIRA)"` and
   `"\"Anonymous (JIRA)\" <jira@x>"` true; `"Katie Evans (JIRA)"`, `"José Garcia <jose.g@avviato.com>"`,
   `"Katie"`, `""` false.
4. `commTask` unit table over `{armed, not armed} × {open, closed} × {plain, blank, notifier,
   anonymous-jira sender}`, asserting the exact first-cause reason in each row.
5. `commTask`'s inputs contain no `dismissed`, `connectorCopy`, `prNotice` or `prClose` field
   (structure test), each documented in the file with D2's inert-predicate argument.

### Part 3 — capture's create path

6. `loadRules` selects `r.comm_task` into `storedRule.commTask`; a structure test pins the SELECT
   token and asserts `comm_task` is named by NO other non-test Go file than `rules_store.go`
   (`notifier_senders`' criterion 10 shape).
7. `decideMessage`'s `found` branch sets `d.comm` from `commTask(…)` with `armed = winner.commTask`,
   and appends the reason fragment to `capture_decisions.reason` — `"comm task requested"` in live
   mode, `"would create a comm task"` otherwise (`requested` / `would` is decideMessage's existing
   pattern). `d.comm` is carried on `ruleDecision`, never written as a column.
8. `EvaluateRules`, LIVE mode only, in D3's table order: `appendRuleLog`, then — when `decision.comm`
   — `createCommTask`, `recordDecisionCommTask`, `setRuleProvenance(commID)`,
   `markRuleActivity(commID)`, `appendCommPointer(targetID, commID)`; and `markRuleActivity` on the
   TARGET is NOT called. When `decision.comm` is false the branch is byte-unchanged from SWT-72.
   Every error fails the pass.
9. `RulesStats.CommTasks` counts created comm tasks; the counter line in
   `cmd/connectors/{google,jira,slackweb,upworkcrm}/main.go` and `cmd/opsctl/main.go` gains
   `"comm_tasks":%d`. `pipeline.CapturedWake`'s counts map is unchanged (it carries five keys by
   design).
10. **Shadow mode creates nothing**: a shadow pass over an armed rule writes the decision with the
    "would create a comm task" reason, `comm_task_id` NULL, and calls no tool.
11. **A replay creates nothing**: re-running a live pass over the same message finds the live claim
    taken (`insertDecision` returns `inserted=false`) and does not reach the branch. Asserted on
    counts of `tasks`, `task_events` and `audit_events`.
12. `createCommTask`'s args come from `commTaskArgs(pm, winner, system, key, relatedTaskID)`, a pure
    function, and the comm path calls neither `link_external_ref` nor `task_mark_surfaced`
    (structure test).
13. `ruleTaskBody` gains `relatedTaskID int64`; with 0 the output is BYTE-IDENTICAL to today (the
    existing body test passes unchanged), and with N it carries `related_task: N` as the last
    key/value line, before the blank line and the preview.
14. `commTaskTitle` is `{sender}: {subject else first line}` through `textmatch.NormalizedPrefix` at
    `rulesTitleLen`, falling back to the project name, then the slug, then the key, and is never
    empty and never a dangling separator (`ruleTaskTitle`'s criterion 4).
15. The pointer log on the target is EXACTLY `capture: comm #<new> created from this message (message
    <M>)` and contains no subject, sender, preview or body text (structure test over the function
    source, `internal/promote`'s criterion 39 shape).
16. **Closed targets are untouched**: an armed rule whose target task is closed creates no comm task
    and still revives (SWT-45) or reopens (SWT-36) as today.
17. **Un-armed rules are untouched**: the José case with `comm_task=false` behaves exactly as SWT-72
    ships — log line plus `task_mark_activity` on the target.
18. `validateCaptureRuleAdd` refuses `comm_task` with `pr_review` and `comm_task` without an
    `external_system`, each by name, both ways (the `pr_review`+`revive` test's shape); `--comm-task`
    is an `opsctl capture-rules add` flag and rides `capture_rule_add`'s INSERT.
19. `DryRunRules`' proposed string names the comm proposal, and `dryRunRuleFlags` prints the flag, so
    `opsctl capture-rules try --comm-task --show all` shows what arming would do before anything is
    armed.

### Part 4 — `task_match`

20. `internal/capture/explain.go` exports `Explanation`, `ExplainMessage` and `ExplainText`; both
    run over `decideMessage` / `Evaluate` and write NOTHING (structure test: no `INSERT`, no `UPDATE`,
    no `ex.Execute` in the file).
21. `ExplainMessage` and `pendingMessages` share ONE column projection and ONE scan helper (structure
    test); `ExplainMessage` applies no direction filter and no horizon.
22. `internal/tools/match.go` registers `task_match` in `createtask.go`'s table with
    `validateMatch` / `matchTask`. Validation requires EXACTLY one of `message_id`, `task_id`, `text`
    (zero or two is an error naming all three), `limit` within 1..20, and a non-empty `text`.
23. The handler returns D6's proposal shape, `rule_ref` before `source_thread`, deduped by task id
    (a task that is both keeps the `rule_ref` source), capped at `limit` (default 5), titles only and
    never a body. `project`, when given, filters proposals by project slug.
24. `{task_id}` resolves through `tasks.activity_by_message_id`; a task with none is refused by name,
    and nothing is inferred from `source_thread_id`.
25. `{text}` marks its proposals `partial:true` and says in the reason that the own-action and
    PR-trust checks did not run.
26. Policy: `Decide` allows `task_match` for `mcp:manual:salvo`, `mcp:treetop`, `dashboard:salvo`,
    `opsctl:salvo` and `capture:google` alike — in neither `humanOnly` nor `mcpHumanOnly` nor
    `snapshotGated` (`matrix_tasklist_test.go`'s shape).
27. MCP: in `agentTools` and `userProfileTools`; `ProfileFull` and `ProfileUser` list it,
    `ProfileRead` does not; the tool counts in `serve_test.go` / `profile_test.go` are updated.
28. `opsctl task-match --message N | --task N | --text "…" [--project slug] [--limit N]` parses to
    the executor call and prints its JSON (the `create-task` path), and the usage lines in
    `cmd/opsctl/main.go`'s header comment and its two `usage:` strings name it.

### Part 5 — `task_attach`

29. `internal/tools/attach.go` registers `task_attach`; validation refuses `task_id <= 0`,
    `target_task_id <= 0`, the two being equal, and a `note` over 500 characters.
30. The handler is one transaction in D7's order, locking the lower id first; a missing task is
    "task N not found" (`lockTask`'s spelling) and a closed TARGET is refused by name, naming
    `task_reopen`.
31. The target's log event is ids-only and carries neither the note nor any text from the source task
    (structure test over the function source plus an integration assertion on the payload).
32. The source is closed through `closeTransition` — a structure test asserts `attach.go` contains no
    `UPDATE tasks SET status` of its own — so an active-work source errors with `activeWorkRefusal`
    and the whole call rolls back, and `reviewed_at`, `closed_at`, `closed_from_status` and the
    session-state clear all land.
33. Idempotence: the same (source, target) twice returns `{attached:false, skipped:"already_attached"}`
    with no second log event and no second `attached` event; a second, DIFFERENT target writes its
    pointer, records a second `attached` event and reports `closed:false`.
34. **The target is not surfaced**: `tasks.activity_at` and `reviewed_at` of the target are unchanged
    by an attach (integration, with a mutation).
35. Policy: `task_attach` is in `humanOnly`. Allowed for `mcp:manual:salvo`, `dashboard:salvo`,
    `opsctl:salvo`; denied with rule `human_only` for `mcp:treetop`, `worker:treetop`,
    `capture:google`, `drafts:gpt` and `orchestrator` — the six-actor-shape test the IK demands.
36. MCP: in `agentTools` and `userProfileTools`, listed by `ProfileFull` and `ProfileUser` and not by
    `ProfileRead`; `internal/mcpserver/serve.go`'s Instructions and `skills/swb-status/SKILL.md`'s
    trigger table gain `swb match <id>` and `swb attach <id> <target>` rows (the existing phrase
    tests' shape).

### Part 6 — the dashboard

37. `POST /tasks/{id}/attach` → `attachTaskAction` → ONE `executeTask` call with `boardBack(r)` and no
    SQL of its own, registered behind `auth.Require` beside dismiss, close and requeue.
38. `templates/tasks.html` gains exactly one addition: an Attach form in the per-row `actions` popup
    with a numeric `target_task_id` input and an optional note, rendered for EVERY row (a comm still
    needs routing after it has been reviewed, so gating it on `NeedsReview` would hide it exactly
    when he comes back to it). Dismiss, Done and Requeue stay byte-identical
    (`TestTasksTemplate_VerbFormsByteUnchanged` untouched); the template still contains no occurrence
    of the word `incoming`, no HTMX and exactly one `<script`.
39. `board.go`, `lights.go`, `sections.go`, `display.go`, `boardQuery`, `TaskExportRow` and both
    exports are BYTE-UNCHANGED: a comm task reaches INCOMING through SWT-72's columns alone
    (structure test asserting the four files' relevant function bodies are untouched is not required;
    the existing SWT-59/SWT-72 structure tests must pass unamended, which is the criterion).

### Part 7 — orchestrator and purity

40. `internal/orchestrator/rules_test.go`'s "must fire nothing" table gains an `attached` row; a
    structure test asserts `internal/orchestrator` mentions neither `comm_task`, `comm_task_id`,
    `task_attach` nor `task_match`.
41. `internal/ticketstatus`'s source mentions none of the four names either (its SWT-72 neighbour
    test), and a jira-keyed ticket that is Done still closes its ticket task after a pass that created
    a comm task from its mail (`surfaced_at` untouched).

### Part 8 — integration (`//go:build integration`, isolated db only)

42. **`TestComm_Integration_ArmedRuleCreatesItsOwnIncomingRow`** — seed a project, a jira-keyed open
    `ready` human task, and a rule armed `comm_task`; run a real capture pass over an inbound gmail
    message from `José Garcia <jose.g@avviato.com>` carrying the key. Assert: one
    `capture_decisions` row, `action='task_log'`, `task_id` = the ticket task, `comm_task_id` = the
    new task; the new task is `ready`, human, in the rule's project, its title starts with the
    sender, its body's last key line is `related_task: <ticket>`, its `source_thread_id` is set and
    its `activity_at` is set; the TICKET task has the full `appendRuleLog` line, the ids-only
    pointer, and `activity_at` still NULL. Then `GET /tasks?project=…`: the comm row is in
    `section-incoming` with remark `new email` and the sender in the title cell, and the ticket task
    is in QUEUE.
43. **Column-fed** (IK: *test the column, not the fixture*): in the same test,
    `UPDATE capture_rules SET comm_task = false` and re-run the pass over a second message — no comm
    task, and the ticket task's `activity_at` moves instead (SWT-72's behaviour). Replacing
    `r.comm_task` in `loadRules` with a literal `false` turns criterion 42 red; the fixture supplies
    neither value.
44. **The noise cases, one pass, three messages** on the armed rule: `Anonymous (JIRA)`, a sender on
    `projects.notifier_senders`, and a blank sender. None creates a comm task; each decision's reason
    names its own cause; each target keeps SWT-72's mark.
45. **Slack and Jira too** (channel-blind): the same armed-rule fixture driven by a `sender` rule on a
    Slack message and by a jira-channel message produces comm tasks whose remarks render `new slack`
    and `new comment`. Neither `commTask` nor the create path contains a channel literal.
46. **`task_match` end to end**: over the message of criterion 42, `{message_id}` returns the ticket
    task first with `source:"rule_ref"`, `rule_id`, `rule_kind:"body_regex"`, `external_key` and a
    `why` equal to capture's reason; `{task_id: <comm>}` returns the same; `{text: "<the WEB key
    line>"}` returns it with `partial:true`; an unmatched text returns `matched:false` with a reason.
    As `mcp:treetop` it succeeds (criterion 26).
47. **`task_attach` end to end**: attach the comm to the ticket task → the ticket gains exactly one
    ids-only `log` event, its `activity_at` and `reviewed_at` are unchanged, the comm is `closed`
    with `closed_from_status='ready'`, `reviewed_at` set, one `attached` event and one
    `status_changed`; the board no longer shows the comm anywhere. A second identical call is
    `already_attached`. A second call with a different target writes the second pointer and reports
    `closed:false`. As `mcp:treetop` it is denied `human_only` with a `policy_decisions` row and no
    write. Against a closed target it errors by name.
48. **Existing suites stay green unchanged** — every `internal/capture` suite (`rules_*`,
    `resurface_*`, `revive_*`, `prreview_*`, `dryrun_*`), `internal/dashboard`'s board suites,
    `internal/promote`, `internal/ticketstatus`, `internal/orchestrator`. Any suite asserting an exact
    audit-tool sequence for an ARMED rule gains the new calls by name; no un-armed fixture changes.

## Data model changes

Migration **0040** only (criterion 1): one column on `capture_rules`, one on `capture_decisions`, two
CHECKs. `tasks`, `task_events`, `external_refs`, `classify_promotions`, `task_dismissals` and
`normalized_*` are read and written exactly as today. **No new table** — a comm is a row in the one
`tasks` table (invariant 2), which is Salvador's own framing ("you don't need a message construct they
can still be task"). No new task status, no new `capture_decisions.action` value (a comm decision IS a
`task_log`, and inventing an action value would break every latest-decision reader, including
`replyfold.InquiryEligibleLatestSQL`).

## API / MCP tool changes

| Tool | Profile | Policy | Args → result |
|---|---|---|---|
| `task_match` (new) | full + user | allow / static default (read-only, `task_list`'s shape) | `{message_id? \| task_id? \| text?, project?, limit?}` → `{input, matched, reason, proposals[{task_id,title,status,assignee_type,project,source,rule_id,rule_kind,external_system,external_key,partial,why}]}` |
| `task_attach` (new) | full + user | `humanOnly` | `{task_id, target_task_id, note?}` → `{task_id, target_task_id, attached, closed, skipped?}` |

Both go through `executor.Execute` (validate → policy → audit start → handler → audit complete). No
existing tool's schema changes. `create_task`, `task_append_log`, `task_set_source_thread` and
`task_mark_activity` are called by the capture path exactly as they are called today. One new
dashboard route, `POST /tasks/{id}/attach`, one `executeTask` call. One new opsctl subcommand,
`task-match`.

## MQTT topics

None. No worker contract, heartbeat, command topic or pipeline wake payload changes.

## Files likely to touch

- `migrations/0040_capture_comm_tasks.sql` (new)
- `internal/capture/comm.go` (new), `internal/capture/explain.go` (new),
  `internal/capture/ownaction.go` (`anonymousJiraActor`),
  `internal/capture/rules_store.go` (`storedRule.commTask`, `loadRules`, `ruleDecision.comm`,
  `decideMessage`'s `found` branch, the `actionTaskLog` branch, `createCommTask`, `commTaskArgs`,
  `commTaskTitle`, `ruleTaskBody`'s parameter, `recordDecisionCommTask`, `appendCommPointer`,
  `RulesStats.CommTasks`, the shared message projection), `internal/capture/dryrun.go`
- `internal/tools/match.go` (new), `internal/tools/attach.go` (new),
  `internal/tools/createtask.go` (registration table)
- `internal/tools/capturerules.go` (`CommTask` arg + the two refusals)
- `internal/policy/matrix.go` (`humanOnly` += `task_attach`)
- `internal/mcpserver/schemas.go`, `adapter.go` (`userProfileTools` + the profile doc), `serve.go`
- `skills/swb-status/SKILL.md`
- `internal/dashboard/server.go`, `board.go` (`attachTaskAction` only), `templates/tasks.html`
- `cmd/opsctl/main.go` (the `task-match` subcommand, `--comm-task`, the counter line),
  `cmd/connectors/{google,jira,slackweb,upworkcrm}/main.go` (the counter line)
- `docs/runbooks/capture-rules.md` (a "comm rules" section: what arming does, J3's dedup argument, how
  to disarm)
- Tests: new `internal/capture/comm_test.go`, `comm_structure_test.go`,
  `rules_comm_integration_test.go`, `explain_test.go`; `internal/tools/match_test.go` +
  `match_integration_test.go`, `attach_test.go` + `attach_integration_test.go`,
  `internal/policy/matrix_attach_test.go`, `internal/mcpserver/{match,attach}_test.go`,
  `internal/dashboard/board_attach_integration_test.go`,
  `internal/orchestrator/comm_structure_test.go`; amended by name:
  `internal/capture/rules_structure_test.go`, `resurface_structure_test.go`, `rules_title_test.go`,
  `internal/mcpserver/{profile,serve,runbook,skill}_test.go`,
  `internal/orchestrator/rules_test.go`, `internal/tools/capturerules_test.go`
- Docs: a dated amendment line in `docs/tickets/activity-resurfaces_SPEC.md` (D3/D7: an armed rule's
  attach marks the COMM task, not the target); a new IK section (Verification Step 6); at deliver time
  `docs/runbooks/HANDOFF-kube-comms-inbox.md`

**Deliberately NOT touched:** `internal/dashboard/{board.go's read,lights.go,sections.go,display.go}`
beyond the one new action handler, `internal/promote/*`, `internal/classify/*`,
`internal/replyfold/*`, `internal/ticketstatus/*`, `internal/orchestrator/*`, every connector sink,
`boardQuery`, `export.go`, `internal/capture/{gate,route,prreview,revive,resurface}.go`.

## In scope / Out of scope

**In scope:** the two columns and their CHECKs, the pure `commTask` split with its Jira-anonymous
clause, capture's comm-task create path with its provenance/activity/pointer calls, `task_match` over
capture's own decision, `task_attach` through `closeTransition`, the two MCP registrations and the
policy row, the dashboard attach form and route, the opsctl subcommand and the flag, the counters, the
runbook section, the tests and the kube handoff.

**Out of scope, each named because it is a tempting bundle:**

- **Changing what SWT-72 does for UN-ARMED rules.** The default is false everywhere; a db with 0040
  applied and nothing armed behaves byte-identically to 0.7.39.
- **The inquiry lane.** D11's ask path, `InquiryGate`, `GateAnswered`, `promote.Decide` and the
  `classify_promotions` claim are untouched. D12's boundary stands: a message a rule files never
  reaches the lane.
- **Closed tasks.** SWT-45's revive and SWT-53's resurface keep them; a closed target is clause 2 of
  `commTask`.
- **Surfacing capture-CREATED ticket tasks into INCOMING** (D8).
- **A body-shape test that tells a Jira comment from a field-edit notice** (D10).
- **A recency source in `task_match`**, a proposal PICKER or prefill in the dashboard's attach form,
  and any ranking score beyond the two-source order (D6).
- **A distinct board kind, tally, count or colour for comm rows** (D5).
- **Backfill.** Nothing re-decides history; Salvador hand-created #481/#482 for yesterday's stale
  items and that stays the answer for anything before the roll.
- **Suppressing his own GitHub PR-review mail** (the 0035 CHECK keeps it from being armed at all; a
  narrower notifier list is rule/project data).

## Invariants that apply

1. **Raw-first** — nothing is ingested. Every path runs on an already-normalized message that already
   has its `raw_source_items` row; the comm task's body copies stored columns and nothing else.
2. **One funnel** — a comm is a row in the one `tasks` table with SWT-72's two columns set. No new
   table, no new status, no "messages" construct (his words). INCOMING stays a render-time filter.
3. **Everything through the executor** — capture creates the comm with `create_task`, records its
   thread with `task_set_source_thread`, surfaces it with `task_mark_activity` and points at it with
   `task_append_log`, all as `capture:{connector}`; `task_match` and `task_attach` are executor tools;
   the dashboard's Attach is one `executeTask`. The ONLY direct SQL capture adds is
   `recordDecisionCommTask`, on `capture_decisions`, which is this package's own log (the
   `recordDecisionTask` precedent). Structure tests keep the comm path off `tasks`.
4. **Nothing external without a delivery row** — nothing is sent and no `deliveries` row is read or
   written. The comm task's `source_thread_id` is what LATER lets `draft_delivery` bind a reply to
   the right thread; the reply itself is still a delivery through the policy matrix.
5. **Own-message loop closure** — capture's inbox is `direction='inbound'` (`pendingMessages:667`),
   and `task_mark_activity` ERRORS on a non-inbound `message_id`, so one of our own sends re-entering
   can never become a comm task. `task_match` reports a message's direction rather than filtering it,
   and writes nothing.
6. **Stealth attribution** — nothing client-visible is produced. Titles, bodies, pointers and log
   lines are stored data or ids; the only generated strings are fixed labels.
7. **Orchestrator purity** — D9: no rule reads the new columns, the new `attached` event hits
   `Evaluate`'s default, and the close is the same `status_changed` every close already emits.
   `commTask`, `Evaluate` and `Explanation`'s builders are pure and structure-scanned.

## Sibling patterns to copy

- **The pure disqualifier set with per-clause reasons:** `internal/capture/resurface.go` and
  `resurface_test.go` / `resurface_structure_test.go` (including the one-reader scan for the project
  column).
- **A per-rule flag end to end:** SWT-54 — `migrations/0035_capture_rules_pr_review.sql`,
  `storedRule.prReview`, `loadRules`'s SELECT, `validateCaptureRuleAdd`'s refusals,
  `dryRunRuleFlags`.
- **Create-plus-provenance-plus-mark in one branch, and its ordering discipline:**
  `rules_store.go:459-498` (`createRuleTask` → `recordDecisionTask` → `linkRuleRef` →
  `setRuleProvenance` → `markRuleSurfaced`) and `internal/promote/store.go:225-249` (`act`'s create
  branch with D11's pointer last).
- **The ids-only pointer log:** `internal/promote/store.go:593-615` `appendRelatedPointer`.
- **A humanOnly verb on both MCP profiles:** `task_requeue` (`internal/tools/requeue.go`,
  `internal/policy/matrix.go:102-106`, `internal/mcpserver/schemas.go:195-209`, `adapter.go:68-73`),
  with the six-actor-shape policy test.
- **A read-only tool on both profiles:** `task_list` (`internal/tools/tasklist.go`,
  `internal/policy/matrix_tasklist_test.go`).
- **The close inside a caller's transaction:** `internal/tools/close.go` `closeTransition`, called by
  `closeTask` and `dismissTask`.
- **A dashboard verb:** `requeueTaskAction` (`internal/dashboard/board.go:898-930`) and its route.
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used; nothing is claimed. **rag-svc HTMX:** not
  used; the board has none (pinned).

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| `loadRules` selects a literal `false` instead of `r.comm_task` | criteria 42, 43 |
| Drop clause 2 (closed task) from `commTask` | criterion 16 |
| Drop the `ownJiraEdit` clause | criterion 44 |
| Drop the `notifier` or `blankSender` clause | criterion 44 |
| Key the comm path on the channel or the rule kind | criterion 45 |
| Keep `markRuleActivity` on the TARGET when a comm task is created | criterion 42 (the ticket task's `activity_at`) |
| Drop `markRuleActivity` on the COMM task | criterion 42 (the row is in QUEUE, not INCOMING) |
| Drop `setRuleProvenance` on the comm task | criterion 42 (`source_thread_id` NULL) |
| Call `link_external_ref` for the comm task | criterion 12, and criterion 42's second pass files onto the comm instead of the ticket |
| The pointer log carries the subject, sender or preview | criterion 15 |
| `ruleTaskBody` emits `related_task: 0` | criterion 13 (the byte-identical body) |
| Create a comm task on the `actionTask` branch | criterion 42's counts (two tasks for one first message) |
| Allow `comm_task` with `pr_review` (drop the CHECK, drop the validator refusal) | criteria 1, 18 |
| `task_match` writes anything | criterion 20 |
| `task_match` infers the message from `source_thread_id` when `activity_by_message_id` is NULL | criterion 24 |
| `task_attach` writes `UPDATE tasks SET status='closed'` itself | criterion 32 |
| `task_attach` puts the note or the source's title on the target | criterion 31 |
| `task_attach` marks activity on the target | criterion 34 |
| Drop the (source,target) dedup | criterion 33 |
| Remove `task_attach` from `humanOnly` | criteria 35, 47 |
| Lock the two tasks in call order instead of id order | no test (documented); reviewed by reading |

## Verification protocol

Run in order. Do not commit before step 4 passes. Capture the exit status of every `go test`
separately from any pipe (IK: *gate commits on test exit status*).

**0. Read-only prod pre-checks** (`psql -h 192.168.50.49 -U ops -d ops`, inside
`BEGIN READ ONLY; … ROLLBACK;`). NOT run by the spec session. Paste the results into the delivery
summary; assert none of them as a frozen literal in any test (IK: *do not assert production counts as
frozen literals*).

- **0a. Which rules to arm, and what each would cost per day** (the gate):

  ```sql
  SELECT cd.matched_rule_id, r.criteria_type, p.slug, nm.channel,
         count(*) FILTER (WHERE t.status <> 'closed')                       AS onto_open,
         count(*) FILTER (WHERE t.status <> 'closed'
                            AND nm.sender ILIKE '%anonymous (jira)%')        AS own_edits,
         count(*) FILTER (WHERE t.status <> 'closed'
                            AND coalesce(btrim(nm.sender),'') = '')          AS blank,
         round(count(*) FILTER (WHERE t.status <> 'closed') / 14.0, 1)       AS per_day
    FROM capture_decisions cd
    JOIN normalized_messages nm ON nm.id = cd.message_id
    JOIN tasks t                ON t.id  = cd.task_id
    JOIN capture_rules r        ON r.id  = cd.matched_rule_id
    JOIN projects p             ON p.id  = cd.project_id
   WHERE cd.mode='live' AND cd.action='task_log'
     AND cd.created_at > now() - interval '14 days'
   GROUP BY 1,2,3,4 ORDER BY 5 DESC;
  ```

  **Gate:** arm only rules whose `onto_open - own_edits - blank` is under ~10/day. A rule over that
  goes on the list for a narrower sibling rule first, not into this roll. Rule 75 (José's
  `WEB-NNNNN` `body_regex`) is the known candidate and the one the "usable alone" claim rests on;
  record the Slack rule ids the query names, since nobody has read them yet.
- **0b. The two-copy risk** (D10, J3): for each candidate rule, how many of its messages have a
  sibling `task_log` decision on the SAME target task from a `channel='jira'` message within ±10
  minutes. A large overlap means arming both sides would double every comment — arm one.
- **0c. The sender shapes he will read:** distinct `nm.sender` behind 0a's open-task rows for the
  candidate rules, with counts and the `(JIRA)`-shape share. Expected `"<Name> (JIRA)"`,
  `"Anonymous (JIRA)"`, real people. Not a gate; it is the D10 residual's size.
- **0d. Notifier lists as they stand:** `SELECT slug, notifier_senders FROM projects WHERE
  notifier_senders <> '{}'` — which projects already suppress bot senders, and whether the candidate
  rules' projects are among them.
- **0e. Claude targets:** how many of the candidate rules' target tasks are `assignee_type='claude'`
  — how often the ids-only pointer lands on a worker task.
- **0f. `EXPLAIN (ANALYZE, BUFFERS)`** of `task_match`'s `source_thread` query
  (`WHERE source_thread_id = $1 AND status <> 'closed'`). Expected: a seq scan of `tasks` in today's
  order (SWT-72 Step 0e measured 453 rows / 55 buffers for the board's own scan). Not a gate; it
  decides whether an index is Future work.

- **Measured 2026-09-22 14:00 EDT (prod, read-only, 14 days, the build session):**
  - **0a — gate passed for every rule.** Attaches onto OPEN tasks per day: rule 10 (`body_regex`
    collaboratory) gmail 2.9 / slack 1.2 / jira 0.1; rule 57 (Mario Cruz's Upwork room, saka) 2.8;
    rule 3 (jira WEB-) 1.0; rule 75 (José's key regex) slack 0.9 / gmail 0.4; rule 72 (jira API-) 0.7;
    rule 71 (jira WEB-) 0.6; rule 74 slack 0.4 / gmail 0.1. Own edits (`Anonymous (JIRA)`): 2, all on
    rule 10 gmail; blank senders: 0. Nothing near 10/day.
  - **0b — the two-copy risk is real for rule 75 on Slack:** 37 of its 48 Slack attaches have a jira
    sibling on the same task within ±10 min (the Jira bot echoing comments into Slack, sender `Jira`);
    rule 10 slack 42/253, gmail 23/554; rule 74 slack 7/13, gmail 5/9. The `Jira` sender is already
    in collaboratory's notifier list, so D2's notifier check removes those before a comm is made;
    still, arm the JIRA-side rules (3, 71, 72) and rule 75 first, and watch 74/10 before arming them.
  - **0c — senders behind the candidates:** rules 3/71/72 — José Garcia 15, Katie Evans 17 (real
    comments, ~1/day each); rule 75 — `Jira` 12 (notifier), Katie Evans (JIRA) 4, José Garcia (JIRA)
    1, José direct 1; rule 10 gmail — Salvador's own GitHub notifications 25 (notifier), `Jira` 14
    (notifier), Katie (JIRA) 6, Lyle 3, Anonymous 2, misc bots 3; rule 57 — Mario Cruz 39 (real,
    Upwork). The D10 residual after the notifier/anonymous checks is a handful of bot mails on rule 10.
  - **0d —** only `collaboratory` has a notifier list
    (`Jira, jira@treetopllc.jira.com, notifications@github.com, noreply@github.com, no-reply@github.com,
    no-reply@builds.circleci.com`). saka/foundry have none and need none (their attaches are people).
  - **0e —** all 155 open targets are `human`; the ids-only pointer never lands on a worker task today.
  - **0f —** `Seq Scan on tasks` 461 rows / 55 buffers, 0.36 ms. No index needed.

**1. Unit:** `go test ./...`. The SWT-48 `TestAttributionTrend_*` flake (20:00–24:00 EDT) is
pre-existing; re-run with `TZ=UTC` if it fires.

**2. Integration, on an ISOLATED database** (never prod, never the shared compose `ops`):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_comms"
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_comms?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_comms?sslmode=disable' \
  go test -tags integration -p 1 -count=1 ./internal/capture/ ./internal/tools/ ./internal/dashboard/ \
                                        ./internal/promote/ ./internal/ticketstatus/ ./cmd/opsctl/
```

Run the capture and dashboard packages twice (rerunnable cleanup), then
`go test -tags integration -p 1 ./...` once against the same URL.

**3. Mutations:** every row of the table above goes red, then is reverted.

**4. Local smoke, in a real browser** (IK: verify UI work in Chrome/Playwright, `channel="chrome"`;
`--screenshot` alone has hidden two layout landmines).

- `DATABASE_URL=<ops_comms url> go run ./cmd/dashboard` (:8085, `/dev/login?user=salvo`).
- Seed through the executor and a real capture pass, never raw SQL: an armed `body_regex` rule, a
  ticket task, one gmail message from a person, one Slack message, one `Anonymous (JIRA)` message.
- At 1000×700: INCOMING leads the left pane and holds the two comm rows (`new email` / `new slack`
  plus their senders); the ticket task is in QUEUE with two log lines; the Anonymous message produced
  no row. `actions` → Attach with the ticket task's id → the flash shows once, the comm leaves the
  board, the ticket task gains the pointer and does NOT move to INCOMING; a second attach of the same
  pair is a clean no-op.
- View source: one `<script`, no `<details … open`, no occurrence of `incoming`.
- From a Claude Code session against `ops-mcp-user` built from this branch: `swb match <id>` then
  `swb attach <id> <target>`.
- `opsctl capture-rules try --comm-task …` on the same db shows the comm proposals.
- Drop the database afterwards.

**5. Deploy — migration FIRST, then ONE image tag, then arm ONE rule and watch it.**

1. Apply 0040 (kube one-shot migrate Job). **Barrier:** every capture pass built from this branch
   selects `capture_rules.comm_task`, so a new image on a pre-0040 db fails capture for every
   connector (the 0034/0035 precedent). Old images never name the columns and are unaffected.
2. Roll one tag to every capture writer (the connector CronJobs), the pipelined workload and the
   dashboard, in one apply. A mixed fleet is harmless: an old binary cannot read the flag, so it
   simply keeps SWT-72's behaviour.
3. `go install ./cmd/ops-mcp-user` and `./cmd/opsctl` here **and on 192.168.50.30** (record it pending
   if still offline; IK: *second workstation .30 install*), then `make install-skill`. A stale
   `ops-mcp-user` does not list the two tools at all and silently drops unknown args (IK: *nothing
   rejects unknown tool args*). Restart open sessions.
4. **Arm exactly one rule** — 0a's winner, expected to be 75:
   `opsctl call --tool capture_rule_set_enabled …` does not set it, so use
   `UPDATE capture_rules SET comm_task = true WHERE id = 75;` (one row, recorded in the delivery
   summary), or add a fresh rule with `--comm-task`. Watch one day: the comm tasks created, the
   dismissals, and the `comm_tasks` counter in the connector logs. Arm the next rule only after that.
5. The kube session owns `kube/switchboard/*.yaml`: hand over
   `docs/runbooks/HANDOFF-kube-comms-inbox.md`. This session never edits manifests.
6. **Post-roll smoke:** `/tasks?refresh=on` on the tablet after the next capture tick — the armed
   rule's next message is its own INCOMING row and its ticket task stayed in QUEUE.

**6. IK entry** (`## A person's comm is its own task, and the matcher is a tool`): the per-rule flag
and why the grain is the rule and not the project or a classifier; `commTask`'s five clauses and the
three clauses deliberately absent because they would be inert; that an armed rule MOVES the SWT-72
mark from the target to the comm; that the board needed no change because a comm reaches INCOMING
through `activity_at`; the `external_refs` hijack that `link_external_ref` would have caused; the
ids-only pointer and the note that never reaches the target; `task_match` running `decideMessage`
itself rather than a second matcher; and 0a/0b's measured numbers with the rules actually armed.

**7. Rollback:** `UPDATE capture_rules SET comm_task = false` — one statement, no deploy, and the
system is back to SWT-72's behaviour with the comm tasks already created still on the board. Rolling
the images back is the heavier option; the columns stay (forward-only) and old binaries ignore them.

## Future work (not this ticket)

- **A body-shape test for Jira notification mail** (comment versus field/status edit), measured
  against the corpus in Go — never with Postgres regex (IK SWT-70) — so a named actor's edit notice
  stops making a comm task.
- **Proposal prefill in the dashboard's attach form**: the board calls the same pure matcher and
  offers the top proposal as a button, so routing on the tablet is one tap.
- **A recency source in `task_match`**, and a `task_match` mode that explains why a rule did NOT
  match (the closest losing rule).
- **Surfacing capture-CREATED ticket tasks into INCOMING** (D8), with SWT-45's `surface` machinery.
- **An index on `tasks.source_thread_id`** if 0f says the scan matters.
- **A label on Attach and Dismiss for comm tasks** — "this comm needed no routing" is training data
  nothing records today, and the same gap SWT-72 left on Requeue.
- **Multi-target attach in one call**, if routing one comm onto three tasks becomes common.
