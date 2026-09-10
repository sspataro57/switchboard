> Jira: SWT-33

# inquiry-classify — the inquiry lane (does this message need a reply from Salvador?)

**STATUS: DELIVERED 2026-09-10.** All three open questions were answered 2026-09-10 and
are folded in below; `docs/tickets/inquiry-classify_OPEN_QUESTIONS.md` keeps each answer
with its date and rationale. Where the build departed from this SPEC, or learned what it
did not know, is recorded in "Implementation notes" near the end — read those before
trusting a number above them (e.g. the ~10 s planning figure: the measured one is 4.3–4.5 s).

## Source

Ad-hoc, not a build-order step. Salvador, verbatim:

> "we should be able to pass messages to qwen and make it dicern if they are inquieres
> that need answer"

Follow-up requirement, same session, verbatim:

> "we need to capture the exact thread so the reply goes to that same thread when done."

Context that produced it: "is slack creating tasks for collaboratory" — the answer being
that Slack reaches `collaboratory` through *attribution-only* capture rules (rules 8 and 9,
`source_slack_workspace`, no `external_system`), so 708 Slack messages are attributed and
0 tasks exist from them. Only rule 10 (`body_regex` on `WEB|API|OPS` ticket keys, priority
90) creates anything from Slack.

## Goal

Add a THIRD local classify lane — `inquiry`, `worker_type='classify_inquiry'` — that reads
one inbound message plus its prior thread context and answers *does this contain an inquiry
that needs a reply from Salvador*, recording the message's exact `normalized_threads` id and
thread key on every verdict so a later ticket can aim the reply at that same thread. It
runs in SHADOW: `ai_runs` + `ai_extractions` rows only, no tasks, no deliveries, no
outbound anything.

**Usable alone** = one pass of `classify run --lane inquiry --since 168h` over
`collaboratory`'s recent attributed messages produces verdicts a human reads in
`classify report --lane inquiry` and on `/funnel`, each naming the asker, the ask, the
thread it belongs to and whether that thread has been replied in since, **broken down by
channel** — plus one `classify eval --lane inquiry` run against a committed labelled set.
Nothing else in the system changes behaviour: no task appears, no delivery row is created,
the personal and residue lanes' output stays byte-identical, and the promoter's inbox is
unchanged.

## Decisions made unilaterally

Recorded here rather than buried, because each is a fork a reviewer will want the argument
for. (The three that were *asked* rather than assumed are in OPEN_QUESTIONS; their answers
are folded in below and marked **[Q1]**, **[Q2]**, **[Q3]**.)

**D1 — A new output contract, not the actionability schema with a new prompt.**
`VerdictSchema`'s `kind` enum is `payment_due | deadline | appointment | action_required |
informational` — the vocabulary of a bill, not of a conversation — and `actionable` scored
against inquiry labels would make two different questions' recall/precision falsely
comparable. So `LaneInquiry` carries its own schema. This breaks two guards that must be
REWRITTEN, not deleted (the repo's standing rule for a guard that becomes wrong):
`TestLane_ThereAreExactlyTwoAndTheyAreDeclaredInGo` and
`TestLane_DoesNotForkTheOutputContract` (`internal/classify/lane_test.go`). Their new
truth: **three lanes, two contracts** — `LanePersonal.Contract == LaneResidue.Contract` is
the assertion that preserves what "one contract, both lanes" was protecting (SWT-23's
0.94 / 0.50 comparability), and it is a *stronger* guard than the field-list scan it
replaces.

**D2 — The model answers "is this an inquiry to me", the spine answers "is it still open".**
See "The hardest problem" below. Consequence: the prompt shows PRIOR context only, never
messages that arrived after the target, so a label stays valid forever and a re-run is a
re-run.

**D3 — Its own project column, `projects.ai_inquiry` (migration 0024).** Not `ai_classify`:
that flag means "mail attributed here gets an actionability verdict from the personal lane"
(0018's own words), and `personal` is the only project carrying it — reusing it would drag
personal mail into a lane that asks a client-conversation question. Typed column, not a
`policies` jsonb key: 0016 (`ai_locality`), 0018 (`ai_classify`) and 0023
(`ticket_assignee_gate`) are three consecutive precedents and the recorded argument against
jsonb predicates.

**D4 — The migration arms `collaboratory`.** Unlike 0021's `classify_promote_after` (which
was left NULL because it EXITS shadow), this lane creates nothing, so arming it is
reversible with one UPDATE and is what makes the ticket verifiable. Default is `false`
(fail-closed, 0018's polarity and reasoning).

**D5 [Q2] — All channels of an armed project; no `--channel` flag.** Confirmed 2026-09-10.
`collaboratory` is ~34 inbound messages/day (184 slack + 35 gmail + 17 jira over 7 days)
= ~6 GPU-minutes/day at the measured ~10 s/verdict. A Jira comment asking a question and a
client email asking a question are the same question as a Slack DM asking one, and a flag
nobody remembers is worse than the mixed population it avoids. **The stated cost is bought
back rather than accepted:** every count the report and `/funnel` print is broken down BY
CHANNEL (criterion 30), so a bad number still says which message shape broke — which is
the whole diagnostic the flag would have given.

**D6 — `--since` is REQUIRED on this lane**, with the arithmetic in the refusal message
(SWT-23's shape). Two reasons: an inquiry has a shelf life of days, so a verdict on a
six-month-old message is GPU spent on nothing; and the armed project's historical corpus is
unbounded from the code's point of view.

**D7 [Q3] — A 40-line starter labelled set, with a hard anti-misquotation guard.** Confirmed
2026-09-10: the lane is shadow-only and creates nothing, so the first real measurement is
Salvador reading flagged output on `/funnel`, and labelling against real flagged output is
faster and less biased than labelling cold. **The condition is enforced in code, not by
convention**: below 120 labels the eval may not print a ratio at all (criterion 31). This
repo has a recorded history of a number quoted out of the context that produced it — the
0.25 s warm benchmark that turned 29.5 GPU-hours into "60 minutes" — and a guard beats a
convention every time.

## The hardest problem: "answered five minutes later is not an open inquiry"

A per-message verdict with no thread awareness flags every question ever asked, including
every one Salvador already answered. That destroys the lane on day one. Four options were
weighed:

| option | why not / why |
|---|---|
| classify at THREAD level over a window | **Rejected.** `internal/connector/slackweb/normalize.go:71-76` keys an unthreaded Slack message on `slack:{ws}:{conv}` — the whole channel or DM. Measured 2026-09-10: 113 thread-exact keys (`…:{thread_root}`) vs 83 conversation-level keys, and **one conversation-level key holds 9,704 messages**. "Thread level" would mean classifying a 9,704-message channel as one unit, with no message id for the report to point at and no stable eval unit. |
| per-message + SQL suppression when a later outbound exists on the thread | **Adopted, but at READ time, not inbox time.** See below. |
| a staleness window alone | Kept as `--since`, which bounds the pass. It is not an answer to "already replied". |
| two-stage (detect, then a second model pass over the thread tail) | **Future work.** Doubles GPU and adds a second prompt nothing has measured. Named so nobody re-invents it silently. |

**The adopted split — agency at the leaves, determinism at the spine:**

1. **Inference (the leaf).** The model sees the target message preceded by its PRIOR thread
   context (up to `inquiryContextMax` = 6 messages, oldest→newest, each tagged `me:` /
   `them:` from `normalized_messages.direction`, each truncated) and answers `needs_reply`
   for the target *in that context*. Prior only — never a message with a later
   `sent_at`/`id`. This is what lets the model see "Salvador already answered this two
   messages up" while keeping the verdict a stable, re-scoreable property of the message.
2. **"Still open" (the spine).** A deterministic fold, in SQL, at READ time
   (`classify.Summarize`, which owns every fold both the CLI report and `/funnel` render —
   SWT-29's rule): a flagged verdict whose thread carries an OUTBOUND message with
   `sent_at` greater than the classified message's is rendered as replied-since. It never
   deletes or edits the verdict row.

**Why read-time and not inbox-time** (i.e. why not simply refuse to classify a message that
has already been replied past): the fold becomes tunable without re-running the GPU, a
mis-tuned rule loses no data, and the verdict record stays complete — SWT-31's
"dismissals are labelled data" instinct applied one lane earlier. The cost is GPU on
messages that turn out to be answered, which at 34 messages/day is minutes.

**The scope caveat, stated rather than papered over.** The fold's grouping key is
`normalized_messages.thread_id`, and what that id MEANS differs:

- **`thread`** — gmail threads, jira issues (`jira:{host}:{KEY}`), and Slack messages with a
  thread root. A later outbound here is genuinely *a reply in this thread*.
- **`conversation`** — a Slack channel or DM with no thread root. A later outbound here
  only means *Salvador has spoken in this conversation since*. In a DM that is nearly a
  reply; in a busy channel it is weak, and collapsing it into "answered" would hide real
  open inquiries.

So the two are **counted and rendered separately** (`answered in thread` /
`spoke in conversation since` / `open`) and the scope is stored on the verdict. Three
counters, one claim each — the four-link-states idiom from SWT-25.

**The inertness risk — RAISED, THEN MEASURED AWAY [Q1].** The fold rests entirely on
`direction='outbound'` existing for the armed project's channels, and Slack direction FAILS
CLOSED per workspace on `SLACK_CONNECTOR_OWN_USER_IDS` (`normalize.go:63-65`). If the column
were empty for Slack, the fold would be a predicate whose discriminating column is a
constant in production — this repo's most repeated defect. **Measured against the live db
2026-09-10, per workspace:**

| workspace | inbound | outbound | latest outbound |
|---|---|---|---|
| `T0360B84U` (Avviato) | 24,352 | 19,012 | 2026-09-04 |
| `T0HPR78RX` (Collaboratory/LlamaSite) | 2,155 | **2,393** | 2026-09-09 |

Collaboratory carries **more outbound than inbound**, nine days fresh. The column is
emphatically not a production constant and the fold ships as specified.

**`.claude/INSTITUTIONAL_KNOWLEDGE.md` is STALE on this point and this ticket fixes it.**
Its SWT-12 section says `T0HPR78RX` has no `OWN_USER_IDS` entry (and that
`connector-slackweb` is suspended). That line is what produced the doubt above, and left
standing it will produce it again in the next session that reads this fold. Criterion 29
requires a dated correction line in that entry — not a deletion: the sentence was true when
written, and the record of when it stopped being true is the useful artefact.

## "Capture the exact thread so the reply goes to that same thread"

The aiming mechanism already exists and this ticket's job is to **not lose what is already
captured**:

- `slackweb` already appends `message.ThreadRootID` to BOTH the `thread_key` and the
  delivery `targetRef` (`normalize.go:71-76`), so a threaded message's
  `normalized_threads.thread_key` is thread-exact: `slack:{ws}:{conv}:{root}`.
- `draft_delivery` already refuses a `slack_reply` whose `target_ref` it cannot parse
  (`slackweb.ParseTargetURL`) and stores `Target.CanonicalURL()`, never the caller's
  spelling.
- **The sanctioned carrier from a verdict to a task is `tasks.source_thread_id`**, written
  only by the `task_set_source_thread` spine tool (SWT-20) — which SWT-30's promoter
  already calls on every task it creates. **Do not invent a second provenance store, and do
  not reach for `external_refs`**: SWT-20 rejected it for exactly this (agent-facing free
  text, a mutable join key, and `UNIQUE (system, external_key)` allows one task per
  conversation forever).

So this ticket records, on every verdict: `thread_id` (the `normalized_threads` row id),
`thread_key` verbatim as stored at classify time, `thread_scope`, and the message's own
`external_message_id`. A future drafting/promotion ticket then has everything it needs and
re-derives nothing.

**The honest limit.** A question asked as a plain, unthreaded channel message has NO thread
to reply into yet. `thread_scope='conversation'` is exactly that statement, and the verdict
also carries `external_message_id` (`slack:{ws}:{conv}:{msg}` — whose last segment is the
same `p…` token `ParseTargetURL` accepts as a message id) so a later ticket can root a NEW
thread AT the asking message rather than guessing a target. A message with no `thread_id` at
all records `thread_scope='none'`. **Nothing in this ticket builds a target URL or a
delivery row**; it records the facts that make aiming possible.

**One spelling of the key parse.** Whether a Slack thread key is rooted is decided by ONE
exported helper in `internal/connector/slackweb` (beside `channelThreadKey`, which builds
it). No SQL anywhere may `split_part`/`LIKE` a slack thread key, and `internal/classify`
must not re-spell the rule — SWT-19's landmine, generalised.

## Acceptance criteria

Each is testable; the test kind is named where it is not obvious.

**The lane**

1. `classify.LaneInquiry` exists with `Name: "inquiry"`, `WorkerType: "classify_inquiry"`,
   `PromptVersion: "inquiry-v1"`, `LabelsPath: docs/evals/inquiry-needs-reply.jsonl`.
   `classify run|report|eval --lane inquiry` resolves it; `LaneByName` refuses an unknown
   name naming all THREE valid spellings.
2. `Lane` carries a `Contract` (schema name, schema, the JSON key of its decision boolean,
   the JSON key of its grouped category, and the positive label token used by the eval
   fixture). `LanePersonal.Contract == LaneResidue.Contract` — asserted directly. The
   actionability `VerdictSchema` and `SchemaName` are unchanged, byte for byte, and
   `TestSchema_MatchesTheOutputContract` still passes untouched.
3. The two guards named in D1 are rewritten to "exactly three lanes / exactly two
   contracts", with the argument in the test's own comment. Deleting either is a failure.
4. `Config.Lane`'s zero value is still REFUSED, before any I/O (existing test unchanged),
   and the refusal message names all three lanes.

**The prompt and the output contract**

5. `InquiryVerdictSchema`: `type: object`, `additionalProperties: false`, all five fields
   required — `needs_reply` (boolean), `ask_kind` (string, enum
   `question | request | decision | scheduling | fyi`), `asker` (string), `ask` (string),
   `reason` (string). No sixth field, no `confidence` anywhere (the package-wide scan in
   `structure_test.go` covers the new file automatically — do not add an exemption), and
   no `url`/`uri`/`href`/`link*` property, enum value or `format` (the URL scan is extended
   to the new schema; today it walks `classify.VerdictSchema` only).
6. **No `link_index` on this contract**, and the numbered-candidate block in `renderUser` is
   NOT rendered for a contract without it. Reason: `normalized_messages.links` is written by
   the google normalizer only, so on a slack/jira-dominated project the field would be null
   on every row — a stored constant, which is the landmine this repo keeps paying for. A
   test asserts that an inquiry-lane request carrying a message WITH links renders no
   numbered list and that the schema has no link field.
7. `InquirySystemPrompt` is ONE prompt for all senders (the existing per-sender-branch and
   sender-literal scans apply to the new file: no email addresses, no client names, no
   `switch sender`). It states: what counts as an inquiry needing a reply from the
   recipient (a direct question, a request for a decision, a request for information or
   work, a scheduling ask, an explicit @-mention asking for something); what does NOT (an
   FYI, a status update, a bot/CI notification, a question already answered in the context
   shown, a question addressed to someone else in the conversation, thanks/acknowledgement);
   that the transcript above the message is prior context and an inquiry already answered
   there is `needs_reply: false`; and the objective. It is bilingual on the same terms as
   the other two prompts.
8. **The objective is stated and is precision-leaning, unlike the other two lanes** — and
   the prompt says why in one line: a missed bill is a late fee (recall), but a false
   "someone is waiting on you" in a client channel is a false alarm on a surface Salvador
   is expected to trust, and this lane's population is mostly chatter. A test asserts the
   objective sentence is present; the eval records both numbers regardless.

**The inbox and the boundary**

9. The inquiry inbox is: `nm.direction='inbound'` AND the LATEST `capture_decisions` row is
   `action='attributed'` AND the joined project has `ai_inquiry` AND no `ai_extractions` row
   exists for the message's `raw_source_item_id` under an `ai_runs.worker_type =
   'classify_inquiry'`. Integration test (Postgres produces every value):
   an `ai_locality='any'` armed project's messages ARE returned; an `ai_inquiry=false`
   project's are not; a `task`/`task_log` latest decision is not; an outbound message is
   not. Mutating the SELECT to drop `p.ai_inquiry` must go red.
10. The filter deliberately carries **no `ai_locality` clause**, and the reason is in the
    comment above it: this lane's containment is `cmd/classify`'s `buildRouter` passing
    `general = nil` — there is nothing to fall back TO — plus criterion 11, not the project
    column.
11. **The routed class is pinned to `provider.ClassRestricted` for this lane**, regardless
    of the message's own class. Without this the ticket is a NO-OP: `collaboratory` is
    `ai_locality='any'`, so `ClassOf` returns `ClassGeneral`, and `Router.Route` with a nil
    general client returns `(nil, DecideSkip, no_general_provider)` for **every message**.
    The pin is not a downgrade of anything — it is the refusal to widen, and it is required
    twice over, because the prompt carries thread-NEIGHBOUR bodies as well as the target's.
    Tests: (a) unit — an `AttrProject`/not-local-only fixture is classified by the local
    client, with the control that the same fixture through the personal lane's class fold
    would skip; (b) unit — the hosted client records ZERO `Complete` calls for the inquiry
    lane, with the local-client control proving the fixture CAN be classified;
    (c) `classReasonOf` files an inquiry-lane skip under `lane_local_only`, never
    `thread_context`. The honesty label in the package comment gains this THIRD reason,
    stated in prose (the existing comment-text scan will require it), including the
    neighbour-bodies sentence.
12. **The fold's input is measured, recorded, and pinned by a mutation test.** The per-workspace
    inbound/outbound counts of the table above go in the runbook with their date; the
    integration test for criterion 17 seeds the outbound row in Postgres and proves the fold
    bites by mutating it. Neither the runbook nor any test may assert a production count as
    a frozen literal — that corpus is live and a literal cries wolf every day a message
    arrives (SWT-19's recorded rule).

**Thread capture**

13. Every verdict records `thread_id` (0 / absent only when the message has none),
    `thread_key` verbatim, `thread_scope` (`thread | conversation | none`) and the message's
    `external_message_id`, alongside `needs_reply`, `ask_kind`, `asker`, `ask`, `reason`,
    `sender`, `subject`, `channel`, `project_id`, `project_slug`,
    `normalized_message_id` and `context_messages` (how many prior messages the model was
    shown — without it, "no context existed" and "context was not loaded" are the same
    row).
14. `thread_scope` for a slack thread key is decided by ONE exported helper in
    `internal/connector/slackweb`; non-slack channels are `thread`; no `split_part`, `LIKE`
    or `||` over a slack thread key appears in any SQL in the repo, and
    `internal/classify` contains no second spelling of the rule (structural test, in the
    shape of `upworkcrm/keyspelling_test.go`).
15. Integration test: a threaded slack fixture (`slack:T…:C…:p…`) records
    `thread_scope='thread'`; an unthreaded one (`slack:T…:C…`) records `conversation`; a
    jira fixture records `thread`; a message with `thread_id` NULL records `none`.

**Thread context in the prompt**

16. The rendered user prompt for the inquiry lane is: up to `inquiryContextMax` prior
    messages of the SAME `thread_id` — strictly `(sent_at, id)` less than the target's,
    oldest→newest, both directions, each tagged from `direction` and truncated — then the
    target message. A unit test with a fixture containing a LATER message asserts that
    message never appears in the request. The context loader is separate from
    `PGStore.neighbours`, whose `direction='inbound'` filter exists for capture-decision
    reasons (invariant 5) and must not be reused here; both carry a comment naming the
    other.

**The "still open" fold**

17. `classify.Summarize` computes, for the inquiry lane only: `Open`,
    `AnsweredInThread` (scope `thread` with a later outbound on the thread) and
    `SpokeInConversationSince` (scope `conversation`, same predicate), and marks each
    rendered flag with its state. `Flagged = Open + AnsweredInThread +
    SpokeInConversationSince`. Integration test seeds the outbound row in Postgres and
    proves the fold bites by MUTATING it (drop the row / flip its direction / move its
    `sent_at` before the target's): each mutation moves a verdict back to `Open`.
18. The fold is READ-ONLY: no verdict row is updated or deleted, and a second `report` over
    the same window prints the same counts (no state).
19. `Summarize` keeps its current signature (`…, workerType string`) and resolves the lane
    from it, so `ReportForWorker`'s pinned signature
    (`internal/classify/summary_structure_test.go:39`) and the byte-identical golden report
    both stay green. An unrecognised worker_type keeps today's behaviour (the actionability
    fold) — the golden test uses `itest-swt29-golden`.

**Rendering**

20. `classify report --lane inquiry` prints, per flagged verdict: time, message id,
    `ask_kind`, sender, subject, the `ask` line, the asker, the thread scope and the
    open/answered state; and a header line `open: N  answered in thread: N  spoke in
    conversation since: N`. The personal and residue reports are BYTE-IDENTICAL to today
    (the existing characterization test is the guard).
21. `/funnel` renders three lane blocks (the loop gains `classify.LaneInquiry`; the
    worker_type literals stay out of `funnel.go` — the existing test bans them), and the
    inquiry block shows the three counters and the thread scope per flag. The dashboard
    restates no fold: every number comes from `classify.Summarize`.

**Eval**

22. `docs/evals/inquiry-needs-reply.jsonl` exists, carries NO message content (ids, label,
    `subject_sha256`, optional `stratum`, optional `note` — the existing key whitelist gains
    the third file as a table row), uses the label vocabulary `needs_reply | not`, and holds
    **at least 40 hand-checked labels** [Q3]. The structure-test table's minimum for this
    file is 40, with a comment carrying the dated commitment to 120 stratified
    (`uniform >= 80`, `enriched >= 40`) and naming criterion 31 as what makes shipping at 40
    safe. Strata are OPTIONAL on this file at n<120 and REQUIRED once the minimum is raised.
23. `classify eval --lane inquiry` scores the lane's decision field (`needs_reply`), not
    `actionable`; `loadLabels` validates against the lane's positive token so a personal
    file cannot be scored as an inquiry file and vice versa; `--labels` still defaults to
    the lane's own fixture.
24. **`classify eval` still writes NO `ai_runs` / `ai_extractions` rows** — re-asserted for
    the new lane. This is the only reason an eval over a labelled set cannot inject
    fresh-timestamped verdicts into a promoter's inbox, and any future change to eval
    persistence must say so.
25. **The label's `subject_sha256` is weak on Slack, and the runbook says so**: the slackweb
    normalizer sets a message's `subject` to the CONVERSATION NAME, so every message in a
    channel shares a subject hash and the drift detector can only catch an id that has moved
    to a different channel. Do not invent a second hash spelling to fix it
    (`classify.SubjectHash` is the one spelling); record the limitation.

**Shadow, data model, docs**

26. Shadow is structural and unchanged: `classify.Store` gains no task-write method; nothing
    in `internal/classify` writes `tasks`, `task_events`, `deliveries`, `external_refs` or
    `classify_promotions`. Integration test: after an inquiry pass, `SELECT count(*) FROM
    tasks` is unchanged and the promoter's inbox (`internal/promote`) returns ZERO rows for
    the inquiry verdicts (it keys on `worker_type='classify'` by name — the assertion is
    that this remains true, not that new code enforces it).
27. Migration `0024_project_ai_inquiry.sql`: `ALTER TABLE projects ADD COLUMN ai_inquiry
    BOOLEAN NOT NULL DEFAULT false;` then `UPDATE projects SET ai_inquiry = true WHERE slug
    = 'collaboratory';` — in that order, forward-only, no down, no index, no
    `INSERT INTO capture_rules`. The migration ledger in
    `internal/classify/structure_test.go` learns 0024 (rewrite the guard, never delete it),
    and a per-file guard pins the two statements and their order.
28. `docs/runbooks/local-classifier.md` gains an **Inquiry lane** section: the commands, the
    `--since` requirement with its arithmetic, what `thread_scope` means and why
    `conversation` is a weaker claim, the measured per-workspace outbound counts with their
    date (criterion 12), the base rate, the by-channel breakdown, the eval numbers as a
    dated table row **carrying the n<120 marker**, the dated commitment to the 120-label
    set, the labelling protocol of criterion 32, and the Slack subject-hash limitation. The
    runbook's opening sentence learns there are three lanes (the existing "opens on two
    lanes" test is rewritten to three).
29. `.claude/INSTITUTIONAL_KNOWLEDGE.md` gains a short **Inquiry lane** entry: the three
    lanes and their worker_types, `ai_inquiry` vs `ai_classify` vs `ai_locality`, the pinned
    restricted class and WHY (`ClassGeneral` + nil general = skip everything), the
    thread-scope split with the 113/83 measurement dated, and the shared advisory lock's
    consequence for cadence. **In the same change, the stale SWT-12 line gets a dated
    correction** — "`T0HPR78RX` has no `OWN_USER_IDS` entry" was true when written and is
    false as of 2026-09-10 (inbound 2,155 / outbound 2,393, latest outbound 2026-09-09);
    correct it in place with the date, do not delete it, and say what the correction is load
    bearing FOR (the inquiry lane's replied-since fold). A structural test asserts the entry
    names both `T0HPR78RX` and a 2026-09-10 date.

**Added by the 2026-09-10 answers**

30. **[Q2] Every classify-lane count the inquiry lane prints is broken down BY CHANNEL** —
    classified, flagged, open, answered-in-thread, spoke-in-conversation-since, and skipped
    — in `classify.Summarize` (the fold, so the CLI and `/funnel` cannot disagree), in
    `classify report --lane inquiry`, and in the `/funnel` inquiry block. `channel` comes
    from the STORED `ai_extractions.fields` (criterion 13), never a re-join to
    `normalized_messages` for a second copy of what was classified — the exception is the
    replied-since fold itself, which is deliberately a statement about the world NOW and
    must join. Integration test: a fixture with slack + gmail + jira verdicts renders three
    channel rows whose counts sum to the lane totals. This is what buys back the diagnostic
    the rejected `--channel` flag would have given: a bad number says which message shape
    broke.
31. **[Q3] The eval REFUSES to print a bare ratio below the 120-label threshold.** With
    fewer than `classify.EvalResultThreshold` (= 120) scored labels, `Eval` prints COUNTS
    (`caught 7 of 9 labelled needs_reply; 7 of 12 flagged were labelled needs_reply`) and a
    marker line, and prints **no** `0.78`-shaped number anywhere in its output. The marker
    is one exported constant with one spelling, worded so it cannot be quoted out of
    context — e.g. `INDICATIVE ONLY — n=40 < 120, this is not a measurement`. Tests:
    (a) unit at n below the threshold — the output contains the marker and matches no
    `\d\.\d\d` ratio pattern; (b) unit at n at/above the threshold — the existing ratio
    output is byte-identical to today for the personal lane, so this guard cannot regress
    the two measured lanes; (c) the same marker string appears in the runbook's score-table
    row, asserted by the runbook test. Rationale: this repo shipped a 25-29x cost error by
    quoting a number out of the context that produced it; a convention would not have
    stopped it.
32. **[Q3] The labelling protocol is written down, in the runbook, and it names who
    labels.** The judgement "does Salvador need to answer this" is HIS and cannot be
    delegated to an agent or inferred from a heuristic. The protocol: the starter 40 are
    drawn from `collaboratory` messages recent enough that he can confirm from memory in
    seconds (the last ~14 days, mixed across slack/gmail/jira in roughly the population's
    proportions), labelled `needs_reply | not` with the thread context in front of him; the
    remaining 80 are accumulated DURING the shadow period from real flagged output on
    `/funnel` and from a uniform sample of unflagged messages of the same window — the
    uniform half is not optional, because a set built only from flagged output can measure
    precision and can never measure recall. Every label carries `subject_sha256`, and the
    file never carries message content.

## Data model changes

One migration: **`migrations/0024_project_ai_inquiry.sql`**.

```
projects.ai_inquiry  BOOLEAN NOT NULL DEFAULT false
```

Meaning: *"inbound messages attributed to this project get an inquiry verdict from the
local inquiry lane."* A WORKLOAD flag, like `ai_classify` and unlike `ai_locality` — which
remains the boundary and is deliberately absent from this lane's filter (criterion 10).
Fail-closed default, 0018's polarity and its recorded reasoning ("a stall is one UPDATE, a
leak is irreversible"). Then `UPDATE projects SET ai_inquiry = true WHERE slug =
'collaboratory'` (D4). No index: `projects` holds tens of rows and every read reaches it by
primary key (0016/0018/0023's recorded argument).

**No new tables.** Verdicts are `ai_runs` + `ai_extractions` rows, exactly as the other two
lanes. Vocabulary used verbatim throughout: `ai_runs`, `ai_extractions`,
`normalized_messages`, `normalized_threads`, `capture_decisions`, `projects`, `tasks`.

## API / MCP tool changes

**None.** No executor tool is added, modified or exposed. The lane is a queue-shaped worker
that reads normalized rows and writes two bookkeeping tables; it makes no tool calls at all,
so invariant 3 has nothing to gate here.

Where a future promotion WOULD hook in, stated so nobody improvises it: `internal/promote`,
which reads stored verdicts and calls `create_task` / `task_append_log` /
`task_set_source_thread` **through the full executor stack** as `promote:classify`
(`cmd/classify/main.go:80-88`). That is where `tasks.source_thread_id` gets the thread id
this ticket records. **Out of scope here** — see below.

CLI surface (not MCP, not the executor):

- `classify run --lane inquiry --since <dur> [--limit N]`
- `classify report --lane inquiry [--since <dur>]`
- `classify eval --lane inquiry [--labels <file>] [--checkpoint <file>]`

## MQTT topics

None. This ticket publishes and subscribes to nothing.

## Scheduling and cost

- Measured cost: ~10 s/verdict on the z4 at the 90 W cap (production figure; the 7.2 s
  workstation median and the 0.25 s warm benchmark are NOT this box). At ~34 messages/day
  for `collaboratory`, ~6 GPU-minutes/day. Thread context raises the prompt size, so
  **re-measure the median during verification and record it** rather than quoting 10 s.
- **`internal/classify.AdvisoryLockKey` (`0x5157_0022`) is shared by all lanes**, and
  `runCmd` treats losing it as an ERROR (exit 1, by classify's policy — a solo pass that
  silently no-ops looks like an empty inbox). Consequence: **two lanes must never be
  scheduled at the same minute.** Either one CronJob runs them sequentially in one command
  (`classify run --lane personal && classify run --lane inquiry --since 168h` — each
  invocation takes and releases the lock) or the schedules are staggered by more than the
  longest pass. Do not give the inquiry lane its own key: serialising the GPU is the
  correct behaviour, since ollama on the z4 answers one model at a time.
- Suggested cadence for the shadow period: twice daily with `--since 24h` (overlap is free —
  the extraction `NOT EXISTS` dedups). Manifests live in the sibling **kube** repo, not
  here; this ticket ships the binary path and the runbook, and the CronJob is a handoff.

## Files likely to touch

New:

- `migrations/0024_project_ai_inquiry.sql`
- `internal/classify/inquiry.go` — `InquirySystemPrompt`, `InquiryPromptVersion`,
  `InquiryVerdictSchema`, the inquiry `Contract`
- `docs/evals/inquiry-needs-reply.jsonl`
- tests: `internal/classify/inquiry_test.go`, `internal/classify/inquiry_integration_test.go`

Modified:

- `internal/classify/lane.go` — `LaneInquiry`, the `Contract` field, `LaneByName`,
  `validate`, `LaneByWorkerType` (for criterion 19)
- `internal/classify/classify.go` — contract-aware decode and `fields` assembly, the pinned
  routed class, `classReasonOf`'s `lane_local_only`, `renderUser`'s contract-conditional
  link block and the thread-context block, the honesty label's third reason
- `internal/classify/store.go` — `inboxWhereInquiry`, the `MessagesByID` branch, the
  thread-context loader, `external_message_id`/`thread_key` in `inboxSelect`
- `internal/classify/summary.go` — contract-aware fold, the three-state open/answered fold,
  the by-channel breakdown (criterion 30)
- `internal/classify/report.go` — the inquiry rendering incl. by-channel rows (the other two
  byte-identical)
- `internal/classify/eval.go` — score the lane's decision field; the lane's label token; the
  sub-threshold refusal and its marker constant (criterion 31)
- `cmd/classify/main.go` — `--lane inquiry`, the `--since` refusal, `loadLabels` vocabulary
- `internal/connector/slackweb/normalize.go` (or a new leaf file in that package) — the one
  exported thread-scope helper
- `internal/dashboard/funnel.go` + `internal/dashboard/templates/funnel.html` — the third
  lane block, its counters and the by-channel rows
- `internal/classify/structure_test.go` — migration ledger → 0024; the URL/confidence scans
  extended to the new schema; the labelled-set table gains a third row (min 40); the runbook
  tests learn the third lane, the marker and the IK correction
- `internal/classify/lane_test.go` — the two rewritten guards (D1)
- `internal/dashboard/funnel_test.go` — three lanes
- `docs/runbooks/local-classifier.md`, `.claude/INSTITUTIONAL_KNOWLEDGE.md`

## In scope / Out of scope

**In scope:** the third lane end to end — prompt, schema, inbox filter, pinned routing,
thread context, thread capture on the verdict, the read-time open/answered fold, the
by-channel breakdown, the CLI report, the `/funnel` block, the eval harness path with its
sub-threshold refusal, a 40-line committed labelled set, one migration, the runbook (incl.
the labelling protocol) and the IK entry plus its dated SWT-12 correction.

**Out of scope — named because they are exactly what a session would bundle:**

1. **Promotion of inquiry verdicts to tasks.** `internal/promote` is hard-scoped to
   `worker_type='classify'` and to `projects.classify_promote_after`; extending it needs its
   own ticket, because `collaboratory` has a non-NULL `client` — unlike `personal` — so a
   promoted task WOULD appear in a worker queue (`task_get_next` filters `p.client = $1`) and
   could be claimed by a Claude console. That is a lifecycle decision, not a flag.
2. **Drafting or sending a reply.** No `deliveries` row, no `draft_delivery`, no target URL
   construction. This ticket records the thread so a later one can aim; it aims nothing.
3. **Making the capture rules create tasks from Slack.** Rules 8/9 stay attribution-only;
   this lane does not touch `capture_rules` (the migration guard forbids inserting one).
4. **Widening `ai_inquiry` to other projects.** One armed project for the shadow period;
   widening is an UPDATE plus a fresh measurement, not a code change.
5. **A second-pass / re-ask model over flagged verdicts**, and any re-classification of a
   message whose thread moved on. Future work.
6. **Anything about the residue or personal lanes' prompts, thresholds or scores.** Their
   output must be byte-identical after this ticket.
7. **Raising the labelled set to 120.** That happens during the shadow period, by the
   protocol of criterion 32, in whatever ticket does it — not here.

## Invariants that apply

1. **Raw-first.** Nothing here ingests. The lane reads `normalized_messages` /
   `normalized_threads` only; the existing structural scan bans `raw_source_items`, `mime`,
   `net/http` and `net/url` in `internal/classify` and covers the new files automatically.
   Concretely: the thread context is loaded from normalized rows, never re-decoded from raw.
2. **One funnel.** No new table and no task-like row. Verdicts are `ai_extractions` rows
   under `worker_type='classify_inquiry'`; the review surface is a filter over an existing
   fold (`/funnel`), not a new store. `classify.Store` still has no task-write method.
3. **Everything through the executor.** The lane calls no tools, so there is no side door to
   audit — and the criterion that keeps it that way is 26. Where a future promotion hooks
   in is named above (`internal/promote`, full executor stack).
4. **Nothing external without a delivery row.** This lane produces no outbound artefact of
   any kind, and the no-fetch scan means it cannot even dereference a URL. The thread
   capture is *provenance*, not a target: the SPEC forbids constructing a `target_ref` here.
5. **Own-message loop closure.** Two concrete demands. (a) The inbox filters
   `direction='inbound'`, so our own sends re-entering through ingestion are never
   classified as someone else's inquiry. (b) The open/answered fold uses
   `direction='outbound'` as its evidence — invariant 5's own marker — and criterion 12
   ties it to the 2026-09-10 measurement rather than an assumption, because
   "absent-because-impossible" and "absent-because-pending" look identical (the SWT-21
   landmine, seventh instance). The measurement came back 2,393 outbound rows for the
   Collaboratory workspace, so the fold discriminates.
6. **Stealth attribution.** Nothing client-visible is produced. `asker`, `ask` and `reason`
   are model text stored internally only; if a later ticket promotes them into a task title,
   the register rule attaches there, not here.
7. **Orchestrator purity.** The orchestrator is untouched: no rule, no event type, no
   provider import. The lane's determinism lives in the SQL fold, which is a pure function
   of stored rows and is integration-tested against Postgres.

## Sibling patterns to copy

- **Inbox filter**: `internal/classify/store.go` `inboxWhere` / `inboxWhereResidue` — the
  lateral latest-decision join, the `NOT EXISTS` keyed on `worker_type`, and the
  comment-per-clause style that says what each clause actually excludes.
- **Lane values and their reasons**: `internal/classify/lane.go`.
- **Class + project in ONE read**: `internal/drafts/store.go` (SWT-21's recorded preference —
  the class and the project cannot disagree if they are one query).
- **`ai_extractions` → `normalized_messages` join**: `internal/promote/store.go:216-226`
  (`nm.raw_source_item_id = e.raw_source_item_id`).
- **Typed column migrations**: `migrations/0016_provider_locality.sql`,
  `0018_bulk_project_and_classify_flag.sql`, `0023_ticket_status_sync.sql`.
- **A key parsed in Go, never in SQL**: `internal/connector/upworkcrm/threadkey.go` +
  `keyspelling_test.go`.
- **A guard rewritten to a new truth**: `TestMigration0018_IsTheOnlyOneThisTicketAdds` and
  its ledger comment in `internal/classify/structure_test.go`.
- **Report/dashboard seam**: `classify.Summarize` owns every fold; the dashboard restates
  none (`internal/dashboard/funnel_test.go` enforces it).
- **A number that must not be quoted out of context**: the residue lane's `--since` refusal
  message (`internal/classify/classify.go:207-213`) carries its own arithmetic for exactly
  the reason criterion 31 exists.
- **Queue claims**: not applicable — this worker claims nothing; it uses the shared advisory
  lock (`internal/capture/rules_store.go`'s explicit-unlock shape, already in
  `PGStore.TryLock`).

## Verification protocol

Before commit, in this order:

1. `go test ./...` — unit, including the rewritten lane guards, the sub-threshold eval
   refusal, and the structural scans.
2. `make integration` (`db-up` + `migrate` + `go test -tags integration ./...`,
   serialized `-p 1` — the cross-pollution pact). The new integration suite must clean up
   its own fixtures in FK order under a test-owned marker.
3. **Confirm the migration is applied where it matters**:
   `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"`
   against `ls migrations/`. Merging a migration is not applying it.
4. **Re-measure the fold's inputs and record them** (criterion 12). The 2026-09-10 numbers
   are in the SPEC; confirm they have not inverted, per channel this time:
   ```sql
   SELECT nm.channel, nm.direction, count(*), max(nm.sent_at)
     FROM normalized_messages nm
     JOIN LATERAL (SELECT cd.project_id FROM capture_decisions cd
                    WHERE cd.message_id = nm.id ORDER BY cd.id DESC LIMIT 1) l ON true
     JOIN projects p ON p.id = l.project_id AND p.slug = 'collaboratory'
    WHERE nm.sent_at >= now() - interval '30 days'
    GROUP BY 1,2 ORDER BY 1,2;
   ```
   Record per channel in the runbook. A channel with zero `outbound` rows has an INERT fold
   FOR THAT CHANNEL and the runbook must say so beside the number.
5. **The shadow pass**:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify run --lane inquiry --since 168h --limit 50`
   (note: `cmd/classify` reads `DATABASE_URL`, not `OPS_DATABASE_URL`). Expect a JSON stats
   line with `processed > 0` and `skipped` NOT dominated by `no_general_provider` — that
   reason appearing at all means criterion 11 is not in effect and the lane is a no-op.
6. `go run ./cmd/classify report --lane inquiry --since 168h` — read the flagged lines by
   hand, and read the BY-CHANNEL block. This is the human judgement the ticket exists for:
   are the `ask` lines things Salvador would actually answer, is the open/answered split
   believable against what he remembers replying to, and does one channel account for the
   noise?
7. `psql` spot-check of what was recorded, on three verdicts of different scope:
   ```sql
   SELECT e.fields->>'channel', e.fields->>'thread_scope', e.fields->>'thread_id',
          e.fields->>'thread_key', e.fields->>'external_message_id',
          e.fields->>'needs_reply', e.fields->>'ask'
     FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
    WHERE r.worker_type = 'classify_inquiry' ORDER BY r.created_at DESC LIMIT 20;
   ```
   Every row must carry a thread id where the message has one, and a thread-exact key
   wherever the source message was threaded.
8. **Nothing else moved**: `SELECT count(*) FROM tasks;` and `SELECT count(*) FROM
   deliveries;` before and after the pass are equal; `classify report` (personal) and
   `classify report --lane residue` print what they printed before.
9. `kubectl -n ops port-forward svc/dashboard 8085:80` → `/funnel` shows three lane blocks,
   and the inquiry counters and by-channel rows match step 6's report for the same window.
10. `go run ./cmd/classify eval --lane inquiry` — at n=40 the output must carry the
    INDICATIVE-ONLY marker and contain **no** ratio. Record counts (not percentages), median
    latency and the base rate as a dated row in the runbook table, with the marker in the
    row.
11. `/ticket-review inquiry-classify` (go-reviewer) before commit — this diff touches a
    provider boundary and a fold that decides what a human sees, so the adversarial pass is
    worth it.

## Implementation notes (2026-09-10)

Where the build departed from, or learned something the SPEC did not know:

1. **Criterion 31's refusal is enforced inside `Eval`, on EVERY lane, on the
   SCORED n.** Two earlier cuts scoped it (inquiry lane only, then inquiry plus
   a caller-set `LabelsOverride` flag); the Codex adversarial re-review showed
   any non-CLI caller of the exported function could leave the flag unset. The
   measured lanes' own fixtures (280 and 874; the personal file's
   structure-test minimum was raised from 100 to
   `classify.EvalResultThreshold`) sit above the threshold, so their published
   output is unchanged — pinned byte for byte at n=120. The SWT-22/23
   small-fixture characterization tests (strata, strata-less golden,
   checkpoint resume) were REWRITTEN to the count-only form; the strata
   semantics they defend are unchanged, asserted on counts instead of ratios.
2. **Two test-author guards were amended, not deleted.** (a)
   `TestInquiryFilter_HasNoLocalityClauseAndTheReasonIsInTheComment` ran "no
   ai_locality clause" and "the comment explains the absent ai_locality clause"
   over one window, which contradict each other; the clause is now judged on the
   SQL literal and the reason on the doc comment. (b) SWT-29's
   `TestClassifySummary_OwnsTheQueriesAndTheFolds` banned any
   `normalized_messages` in summary.go, while criterion 30 requires the
   replied-since join; it is now carved out BY NAME — the constants
   `repliedSinceSQL` (joins) and `repliedSinceCol` (column) — held to an
   ALLOWLIST of identifiers (thread_id, direction, sent_at, id), with the table
   name, its aliases and any third SELECT banned everywhere else in the file.
3. **The replied-since fold is set-based** (latest outbound `sent_at` per thread,
   one scan per report). A correlated EXISTS measured 21.5 s over 1,806 verdicts
   on prod — `normalized_messages` has NO index on `thread_id`. An index
   migration is a candidate follow-up, not bundled here.
4. **Measured cost: 4.5 s median / 5.3 s p90 per verdict** (first shadow pass,
   50 verdicts, full six-message context) — not the planning figure of ~10 s.
5. **Jira never reaches this inbox for collaboratory.** 14-day inquiry inbox on
   2026-09-10: slack 247, gmail 7, jira 0 — collaboratory's jira messages carry
   `task`/`task_log` latest decisions (capture rule 10), which criterion 9
   excludes by design.
6. **Verification step 4's query cannot see outbound**: it counts ATTRIBUTED
   messages, and capture never decides an outbound one. Measured on the
   collaboratory THREADS instead (30 days): slack 793 outbound / 1,681, jira 25 /
   105, gmail 1 / 447 — the fold is near-inert for gmail on this project.
7. **Migration numbering**: 0024 was applied to prod after 0025 (SWT-34 merged
   first); the runner keys on version, so order is irrelevant.
8. **Observed, out of scope**: a capture rule attributes an Avviato channel
   (`T0360B84U:C1C1TSLJH`) to collaboratory.
9. **Timestamp ties FAIL CLOSED in the fold** — a strictly later `sent_at`, as
   criterion 17 wrote it. Codex's first round asked for a `(sent_at, id)`
   tie-break; its second showed `normalized_messages.id` is a BIGSERIAL
   insertion key, so a backfilled older message can carry a higher id and a
   tie-break would hide a real open inquiry. A same-instant reply therefore
   reads `open` (pinned by two integration tests). **Accepted residual:** the
   transcript loader keeps criterion 16's explicit `(sent_at, id)` bound, so a
   same-instant message ingested earlier can appear as "prior" context — and
   the transcript decides whether a message is flagged at all, so a
   same-instant lower-id outbound could push the model to `needs_reply=false`,
   and an unflagged verdict never reaches the fold. Measured 2026-09-10
   (read-only, prod): 47,098 of 47,957 Slack `sent_at` values carry sub-second
   precision, and only 8 Slack thread/instant groups mix inbound and outbound —
   all in Avviato (`T0360B84U`), unrooted, dated 2020-01 to 2021-10, none in
   `T0HPR78RX` or any `--since` window. Accepted on that measurement, not on
   "ties are rare".
10. **Eval checkpoints are bound to their evaluation** (Codex rounds 3–4):
    every line carries worker_type / prompt version / a fingerprint of the
    system prompt + schema / configured model / think / max tokens / context
    size, checked at load before any request; unkeyed legacy lines are refused.
    Not bound, by decision: the label-set fingerprint (verdicts do not depend
    on labels). Not yet bound, a follow-up: the USER-prompt template
    (`renderInquiryUser` / `renderMessage`) and the provider URL / model digest
    — so any template change MUST bump `PromptVersion`, or a resume would mix
    renderings.
11. **Out of scope, reported by Salvador 2026-09-10:** HOC and LlamaSite work
    is not his. The `a-millon` channel (hotfix announcements, team chatter) is
    attributed to collaboratory and produced several of the first pass's false
    flags; its attribution is a capture-rule follow-up.
12. **The label record is closed** (Codex adversarial review, rounds 6 and 8):
    `Eval` refuses a repeated `message_id` before any I/O (a duplicated set
    would otherwise cross the ratio threshold with no new judgement), and
    `loadLabels` plus the structure test refuse any key outside
    `message_id | label | subject_sha256 | stratum | note` and any `note`
    outside `classify.LabelNoteAllowed` — criterion 22's "optional note" is a
    closed vocabulary, not free text, because the file is committed. Every
    other allowed field is constrained too (round 9): `subject_sha256` must be
    exactly 16 lowercase hex (`classify.SubjectHash`'s shape), `stratum` must
    be in `classify.LabelStratumAllowed`, and the personal lane refuses any
    stratum. A repeated key is refused (round 10: `encoding/json` keeps only
    the last value, so an earlier one could carry text past every check) by
    `classify.DuplicateLabelKey`, which compares keys decoded, in the loader
    and the structure test alike. Blanket-labelled rows carry
    `classify.OwnerBlanketNote`, and `Eval` reports them on
    their own line (with the uniform-stratum share on a stratified set).

## Future work (not this ticket)

- Raise the labelled set to 120 stratified (80 uniform + 40 enriched) during the shadow
  period by the protocol of criterion 32, then raise the structure-test minimum in the same
  change — at which point criterion 31's refusal stops firing and the first real
  recall/precision numbers may be published.
- Promotion of inquiry verdicts into `tasks` for `collaboratory`, with `task_set_source_thread`
  carrying the recorded `thread_id`, an `ask_kind` whitelist, a cutover column of its own,
  and the client-queue question answered.
- A drafting path that turns an open inquiry into a `slack_reply` / `jira_comment` /
  `gmail` delivery aimed at the recorded thread — including what to do for
  `thread_scope='conversation'` (root a new thread at the asking message).
- A second-pass model over flagged verdicts to trade precision back, as the personal lane's
  prompt comment already proposes; never a fabricated confidence threshold.
- Re-classification when a thread moves on (today a verdict is written once and the fold is
  what keeps it honest).
- Feeding dismissals (SWT-31 `task_dismissals`) back as labelled data for this lane once
  promotion exists.
