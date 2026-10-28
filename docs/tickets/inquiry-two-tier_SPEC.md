> Jira: SWT-110

# inquiry-two-tier — GPT second opinion on qwen's "no reply needed", and alert mail on armed projects reopens instead of resurfacing

swb task 2668. Ad-hoc, not a build-order step.

**STATUS: READY for `test-author`.** No open questions. The owner decisions (A) and (B) below are not
reopened. Every remaining call is under "Decisions made unilaterally", each with its rationale. Two
verification gates (V0, VB0) are real preconditions: if either comes back other than expected, stop
before arming and decide.

## Source

Owner decisions, Salvador, 2026-10-28 (relayed by the coordinator, verbatim in substance):

> **(A)** The inquiry lane becomes two-tier. qwen3:8b (local ollama, today's tier) classifies first,
> unchanged. When qwen returns `needs_reply=false` on a message whose OWN class (classOf over the
> target and its thread neighbours, SWT-21) is ClassGeneral (project `ai_locality='any'`: today
> collaboratory and a-millon), a second pass asks GPT through the existing openai adapter with the SAME
> InquirySystemPrompt and InquiryContract. GPT's verdict is final for the inquiry promoter.
> `needs_reply=true` from qwen goes straight to promotion with no GPT call. Restricted messages
> (llamasite-sec is local_only) never go hosted, and qwen's verdict stands for them. Tier 1 keeps
> today's pin.
>
> **(B)** In a project whose inquiry lane is ARMED (`ai_inquiry AND inquiry_promote_after IS NOT NULL`),
> a `task_log` onto a CLOSED task from a matched rule that is NOT a comm rule (`capture_rules.comm_task`
> false) and is keyed (`external_system` set) reopens the task into INCOMING, exactly as the unarmed
> path does (swb 650 / SWT-88: the revive form of `task_reopen` plus the activity mark), instead of
> resurfacing into the inquiry lane. Comm rules (e.g. rule 91 Rochester sender, 89 José) keep the
> inquiry path. Every other `resurfaces()` clause still holds. Also consider rule-10-style
> constant-key bucket rules.

Owner input, same day: **"gpt on the home lab is via codexproxy".** codex-proxy is an
OpenAI-compatible endpoint at `http://192.168.50.48` (k8s ns `codex-proxy`, LoadBalancer :80). It
needs a bearer token: `GET /v1/models` without one returns 401 `invalid_api_key`. It forwards to
hosted OpenAI.

**Motivating failure.** On 2026-10-27 qwen judged a testing punch list from Rochester client Cherith
Reynolds-Clark (normalized message 1623435, extraction 18689) "informational", so no task was made. A
30-day scan of collaboratory found 15 resurfaced client mails that qwen dropped. 14 of them were
correct drops (thanks, FYI, OOO). One was this miss.

**Motivating residual for (B)** (IK "`eks` project", swb 2709). Rules 124-129 (AWS alarms, budgets,
collab-alerts) moved on 2026-10-28 from the unarmed `eks` project to the armed `collaboratory`. A
re-fired alarm on a CLOSED task now resurfaces into the inquiry model, which will never judge an alarm
"needs reply". Before the move, it reopened.

## Goal

Two independent changes, each usable alone:

- **Part A (two-tier inquiry).** For messages in an armed project, a qwen `needs_reply=false` verdict on
  ClassGeneral content gets a stored GPT verdict, through codex-proxy, under a new worker_type. The
  inquiry promoter acts on that verdict. A restricted message is never offered to the hosted client.
  **Usable alone:** once `projects.inquiry_hosted_after` is set for collaboratory, a client mail like
  1623435 becomes a ready human task in collaboratory within one sweep of qwen's false verdict.
  `classify report --lane inquiry` shows what tier 2 did, and `classify eval --lane inquiry --two-tier`
  scores the composite against the committed labels.
- **Part B (alert reopen).** On an armed project, a non-comm keyed rule's match onto a closed task
  reopens the task into INCOMING. A constant-key bucket rule is excluded by a pure structural
  predicate. **Usable alone:** the next re-fired AWS alarm on a closed collaboratory alarm task puts
  that task back in INCOMING, and its decision row's reason names swb 2668.

They share a ticket, not code. B ships no migration and touches only `internal/capture`. A can land
second.

## Decisions made unilaterally

**D1 — Tier 2 is its own pass, not a fallback inside tier 1.** The new pipelined stage
`inquiry_hosted` is woken by `inquiry_classified` and by the sweep. Its pass function is
`classify.RunInquiryHosted`. Why it is not a call made inline after qwen's false verdict:

- An inline call needs a retry path anyway, because the extraction is written first and GPT can fail.
- It would hold the GPU lock `0x5157_0022` across a network call.
- It would put tier 2 behind the route-backlog starvation already recorded in
  `HANDOFF-kube-inquiry-promote.md`.

As its own stage, tier 2 has its own lock, heartbeat and retry by sweep, and switching it off is
config (`PIPELINE_STAGES`).

**D2 — Tier 2 goes through a Router, routed on the message's own class.** The router is
`provider.NewRouter(hosted, nil, 0)`: the hosted client is GENERAL and there is no local client.
`RunInquiryHosted` takes a `*provider.Router`, like every worker entry point
(`TestWorkerEntryPoints_TakeARouter` gains the row). For each message it calls
`router.Route(ctx, classOf(m))`: the unpinned fold, never `routedClass`.

- A ClassGeneral message gets the hosted client.
- A ClassRestricted one gets `(nil, DecideSkip, no_local_provider)`.

So "restricted content never goes hosted" is the existing `Decide` table, not new discipline. Tier 1's
`routedClass` pin and cmd/classify's `NewRouter(nil, …)` are untouched.

**D3 — A distinct worker_type, `classify_inquiry_hosted`, with no new Lane.**

- Every tier-1 reader stays byte-identical: classify's inbox `NOT EXISTS`, `Summarize`, the report,
  `/funnel`, eval checkpoints, `InquiryOutcomes`.
- Tier 2's inbox gets its own `NOT EXISTS`.
- The promoter's choice is explicit SQL plus a pure function, not an accident of OR-ing two
  `needs_reply` rows under one worker_type.

It is not a Lane, so the "four lanes, three contracts" guards in `lane_test.go` do not change. Tier 2
reuses `LaneInquiry`'s `System`, `Contract`, `PromptVersion` and `renderUser`. Exported as
`classify.InquiryHostedWorkerType`.

**D4 — Every tier-2 decision is recorded, including a refusal.** Per message, tier 2 writes one of three
things:

| outcome | ai_runs.status | ai_extractions row | hosted call |
|---|---|---|---|
| GPT answered and parsed | `ok` | yes: `inquiryFields(m, iv)` plus `tier1_extraction_id` | 1 |
| class fold says restricted | `refused` | yes: `{refused:"restricted", route_reason, tier1_extraction_id, normalized_message_id, thread identity}` | **0** |
| transport error / non-2xx / parse failure | `skipped` (ErrUnavailable) or `error` | **none**, so it retries next pass | 1 attempted |

Why a refused message gets an extraction, unlike a tier-1 boundary skip: the promoter must be able to
tell "qwen's false stands, because the content is restricted" from "GPT has not answered yet".
`internal/promote` cannot import `internal/provider`, so it cannot fold the class itself. A refusal
re-folds nothing next pass, so without a row it would sit in the tier-2 inbox until the deadline and
then hit the D6 fallback. `ai_runs.status` has no CHECK (0001), so `refused` needs no migration. No
other reader keys on this worker_type.

**D5 — A per-project cutover: `projects.inquiry_hosted_after TIMESTAMPTZ` (NULL = tier 2 off),
migration 0050.** This follows SWT-33 D3 / SWT-40 C-D2's one-column-per-question rule.

- It is forward-only on the tier-1 verdict clock (`r.created_at >= inquiry_hosted_after`), so arming
  never escalates the 72h backlog.
- It is the only signal the promoter has that tier 2 is meant to exist for a row. Without it, "tier 2
  not deployed" and "tier 2 pending" look the same.
- It is the kill switch: `UPDATE projects SET inquiry_hosted_after = NULL` reverts the promoter to
  tier-1-only at once.
- Arming is by UPDATE after the smoke, never in the migration (0021/0028's precedent: this changes task
  creation, unlike 0024).

**D6 — GPT unavailable: fail toward RECALL after a short deadline.** If an armed tier-1 false verdict
has no terminal tier-2 row (`ok` or `refused`):

- **Inside `InquiryHostedDeadline = 30 min`** of the tier-1 verdict's `ai_runs.created_at`, the promoter
  gates it as `hosted_pending`. That is 6 sweeps of retries.
- **After the deadline**, it promotes on the FALLBACK source:
  - The title is `inquiryTitle(sender, subject, ask)`, which already falls back to the subject when qwen
    left `ask` empty.
  - The body line is `verdict_tier: fallback (hosted tier unavailable; fail toward recall)`.
  - The `kind` gate is SKIPPED, because qwen's `ask_kind` (often `fyi`) is the judgement GPT was asked
    to overrule.
  - Every other gate (rethreaded, stale, pending, answered, not_addressed, claude_task) still applies.

Rationale:

- SWT-78 reversed the tie-break to recall ("when unsure, answer true", `inquiry-v3`), on the measured
  fact that a dropped verdict is read by nothing ("Qwen dropped it" is visible only by joining
  `ai_extractions` against `classify_promotions`).
- A GPT outage that silently kept qwen's false would repeat the 1623435 failure invisibly, for exactly
  the population this ticket exists for.

Accepted cost: an outage longer than 30 min promotes every qwen-false general message that passes the
other gates. At ~34 collaboratory inbound/day that is tens of tasks per day of outage. They are
recognisable by the body line, and disarming (D5) stops them.

30 min rather than 2h: `InquiryGrace` is 0 since 2026-09-18, because Salvador wants asks on the board at
once. An hour's tier-2 wait would undo that for exactly the asks qwen missed.

**D7 — Lane-scoped env names, not `OPENAI_*`.** The variables are `INQUIRY_HOSTED_URL`,
`INQUIRY_HOSTED_KEY` and `INQUIRY_HOSTED_MODEL`. This departs from cmd/triage, cmd/drafts and
cmd/planimport's `OPENAI_BASE_URL`/`OPENAI_API_KEY` on purpose. pipelined is one process running every
stage, and a generic `OPENAI_API_KEY` in its env is exactly the name a later stage would pick up. A
lane-scoped name lets the structural guard pin the one file that may read it.

All three are REQUIRED once the stage is enabled:

- An empty URL is refused. `NewOpenAI`'s default of `https://api.openai.com/v1` must never receive a
  codex-proxy token.
- An empty key is refused.
- With any one missing, every pass returns an error naming the missing variable and writes nothing.
  D6's fallback then applies, so the failure is loud and not lossy.

**D8 — Part B's bucket guard is structural and pure.**

- `bucketKey(rule)` is true when the rule's EFFECTIVE key regex parses (`regexp/syntax`) to a FINITE
  language, i.e. it contains no unbounded repetition (`*`, `+`, `{n,}`).
- The effective key regex is `key_regex` when set, otherwise the pattern of a `body_regex` rule.
- A thread-keyed rule (no `key_regex`, any other criteria type) keys by `thread_key` and is never a
  bucket.
- Examples of buckets: `CCW` and `\b(WEB|API|OPS)\b` (rule 10's prefix shape, K3). Examples that are
  not: `LHH-[0-9]+`, `\A[^\n]*`, and an alarm-name capture.
- On an armed project a bucket rule keeps today's resurface path (K3: a reopen would resurrect a
  catch-all).

It is code, not a data check, because `capture_rule_add` can add a bucket rule tomorrow, and swb 650's
"never add one without a key_regex" is a convention. **Not** applied to the unarmed path: there, a
bucket match today reopens (the recorded swb 650 residual). Resurfacing would be read by nothing, so
neither option is right, and fixing it is Future work. The DE-FACTO constant key, a regex that could
vary but in practice always yields one value (rule 129's subscription-confirmation subject onto #2640),
is not structurally detectable. VB0 measures it.

**D9 — Rule 129's shape (if VB0 confirms it) reopens.** A new SNS subscription confirmation on closed
#2640 reopening into INCOMING is the desired outcome: each one needs a click to confirm. Rule 132's
per-threshold spend keys reopening monthly is likewise intended (IK swb 2661: "the same threshold next
month resurfaces it"). VB0 is the stop gate if the measured set contains anything else.

## Acceptance criteria

### Part A — locality boundary and codex-proxy (land first within A)

1. `provider.Descriptor` gains `Hosted bool`. `LocalityOf` returns `LocalityRemote` when `Hosted` is
   true, checked BEFORE any host parsing. A declaration can only narrow: no field makes anything local.
   Unit: `LocalityOf(Descriptor{Endpoint:"http://127.0.0.1:1", Hosted:true}) == LocalityRemote`.
2. `provider.NewHostedOpenAI(apiKey, baseURL string) (*OpenAI, error)` refuses an empty `baseURL` and an
   empty `apiKey` with distinct errors. It never defaults to `api.openai.com`. Its `Describe()` returns
   `Hosted: true`. `NewOpenAI` is unchanged (triage/drafts/planimport byte-identical).
3. `provider.LocalityOf` treats the hosts in an unexported compiled-in set `knownHostedRelayHosts =
   {"192.168.50.48"}` as `LocalityRemote` whatever the adapter. The comment names codex-proxy, its
   namespace and this ticket. Unit:
   - `LocalityOf(Descriptor{Endpoint:"http://192.168.50.48"})` and `…:11434` are `LocalityRemote`;
   - `AvailabilityOf(provider.NewOllama("http://192.168.50.48", "qwen3:8b")) == AvailNotLocal`;
   - a `Router(nil, thatOllama)` routes `ClassRestricted` to `(nil, DecideSkip,
     local_endpoint_not_private)` and the fake server records **zero** requests, probe included.

   This is the guard that codex-proxy cannot be configured as `OPS_LOCAL_PROVIDER_URL` in ANY binary.
4. Router unit: `NewRouter(nil, hostedClient)`, i.e. a hosted client wrongly placed as LOCAL, routes
   `ClassRestricted` to a skip with zero `Complete` calls. Positive control: the same client as GENERAL
   serves `ClassGeneral`.
5. The tier-2 builder (`cmd/pipelined/inquiry_hosted.go`, and `cmd/classify/hosted.go` for the CLI)
   refuses to build when `INQUIRY_HOSTED_URL`'s host equals `OPS_LOCAL_PROVIDER_URL`'s host. Unit test on
   the pure builder function.

### Part A — tier 2 (`classify.RunInquiryHosted`)

6. `classify.InquiryHostedWorkerType == "classify_inquiry_hosted"`. `classify.HostedAdvisoryLockKey ==
   0x5157_0050`, distinct from every key under `internal/` (the test lists them, including
   `internal/lockkeys`).
7. **Tier-2 inbox**, integration-tested with one fixture per clause and each mutation red:
   - an ok `classify_inquiry` extraction with `needs_reply = false`;
   - on an inbound message whose admission passes `replyfold.InquiryEligibleLatestSQL` /
     `InquiryLiveDecisionJoinSQL` / `InquiryProjectIDSQL`, the ONE spelling, spliced and not
     re-spelled;
   - `p.ai_inquiry` AND `p.inquiry_hosted_after IS NOT NULL` AND the tier-1 `r.created_at >=
     p.inquiry_hosted_after`;
   - `nm.sent_at >= now() - $since`;
   - NO `classify_inquiry_hosted` extraction whose run status is `ok` or `refused`;
   - NO `classify_promotions` row for the message (a fallback promotion stops the retries);
   - oldest first, limit-bounded.

   Controls: a `needs_reply=true` tier-1 row is NOT returned; an unarmed (`inquiry_hosted_after` NULL)
   project's row is not; a tier-1 verdict recorded before `inquiry_hosted_after` is not. The
   cutover-column mutation is the SWT-21 rule: seed through the column, then replace the column with a
   literal `now() - interval '1 year'` in the SELECT, and the before-cutover control goes red.
8. **Restricted never goes hosted, through the real column** (integration, hosted client faked with a
   request counter). Same pass, three messages, all armed and qwen-false:
   - (a) general project, gmail thread with no other inbound: 1 hosted call, an `ok` extraction;
   - (b) a thread with one inbound neighbour attributed to a `local_only` project: **0** calls, a
     `refused` extraction;
   - (c) a thread with one inbound neighbour that has no capture decision (`AttrUnseen`): **0** calls,
     `refused`.

   Mutations: route on `routedClass` instead of `classOf`, so (a) becomes refused; drop the neighbour
   fold from `classOf`, so (b) and (c) are sent. Both go red. The project's `ai_locality` is set by
   UPDATE in the test, never by a Go literal.
9. **Same prompt, same contract.** Unit: for one `PendingMessage`, tier 2's request has `System ==
   InquirySystemPrompt`, `SchemaName`/`Schema == InquiryContract`'s, and `User == renderUser(LaneInquiry,
   m)`, byte-equal to the request tier 1 sent. `Model` is `INQUIRY_HOSTED_MODEL`.
10. **What is recorded.**
    - `ai_runs`: `worker_type='classify_inquiry_hosted'`, `provider` = the client's `Describe().Name`,
      `model` = the model the response named (else the configured one), and `input` carrying
      `prompt_version`, `tier: 2` and `tier1_extraction_id`.
    - `ok` extraction fields: exactly `inquiryFields(m, iv)`'s keys plus `tier1_extraction_id`, on the
      same `raw_source_item_id` as tier 1.
    - `refused` extraction: the D4 keys and no model fields.
    - An error writes a run row and no extraction, and the message is in the next pass's inbox
      (integration: fake server 503 → retry → 200).
11. `RunInquiryHosted` takes the hosted lock (`HostedAdvisoryLockKey`), never `0x5157_0022`. Two
    concurrent passes make one hosted call per message (integration).
12. Tier 1 is untouched. `classify.Run` on `LaneInquiry` is byte-identical: the existing
    `routedClass` pin tests and the zero-hosted-calls test pass unmodified, and cmd/classify's
    `buildRouter` still constructs `provider.NewRouter(nil,` (the labels_test guard is AMENDED only to
    also assert `hosted.go` never hands its client to `buildRouter`).
13. **The eval loader's attribution is fixed.** The inquiry branch of `MessagesByID` sets `Attribution =
    AttrProject` only when the latest decision names a project, and `AttrUnmatched`/`AttrUnseen`
    otherwise (today it is `AttrProject` unconditionally, which is harmless only while the pin holds).
    Integration: a labelled message with no decision folds `ClassRestricted`.

### Part A — the promoter's final verdict

14. **Pure `promote.InquiryFinal(t1NeedsReply bool, armed bool, t2 *Tier2Row, t1At, now time.Time)
    (src FinalSource, pending bool)`** with a table test. The armed side is `inquiry_hosted_after IS NOT
    NULL AND t1At >= inquiry_hosted_after`, computed in SQL and passed in.

    | tier 1 | armed | tier-2 row | result |
    |---|---|---|---|
    | true | any | any | **tier 1** (tier 2 is never asked; a stray row is ignored) |
    | false | no | any | no promotion (today) |
    | false | yes | `ok`, needs_reply true | **tier 2** (GPT's fields) |
    | false | yes | `ok`, needs_reply false | no promotion |
    | false | yes | `refused` | no promotion (qwen stands; restricted) |
    | false | yes | none, `now - t1At < InquiryHostedDeadline` | pending → gate `hosted_pending` |
    | false | yes | none, past the deadline | **fallback** (D6) |
15. **`inquiryInbox` selects exactly the rows the table can promote or pend.** The tier-1 rows are those
    with `needs_reply='true'`, OR `needs_reply='false'` AND armed AND (no terminal tier-2 row, OR a
    tier-2 `ok` row with `needs_reply='true'`). The tier-2 row comes from `LEFT JOIN LATERAL` over
    `classify_inquiry_hosted` extractions with run status `ok`/`refused`, newest by `e2.id`. Every
    existing C2 clause is unchanged. Integration: one fixture per table row, plus the mutation
    "drop `r.created_at >= p.inquiry_hosted_after`", where a pre-cutover qwen-false row pends and the
    test goes red.
16. **The verdict promoted carries its source.**
    - `classify_promotions.ai_extraction_id` is the tier-2 extraction for a tier-2 promotion, and tier
      1's for a tier-1 or fallback one.
    - `kind` is the ask_kind of the source's fields.
    - `reason` names the tier.
    - The task body gains one line, `verdict_tier: 1` / `verdict_tier: 2 (<model>)` /
      `verdict_tier: fallback (hosted tier unavailable; fail toward recall)`, asserted exact-text
      (C8's tests amended, not deleted).
17. `GateHostedPending = "hosted_pending"` joins `InquiryGateReasons()`, placed after `rethreaded` and
    before `kind`. The reasons test is rewritten to the new order. A fallback source skips the `kind`
    gate ONLY. Unit: a fallback with qwen `ask_kind=fyi` passes kind, and the same fallback with
    `RepliedSince` is still `answered`.
18. **The motivating case, end to end** (integration, pipelined stages injected):
    - a gmail message on an armed general project gets tier-1 `needs_reply=false, ask_kind=fyi`;
    - the `inquiry_hosted` pass with the fake returning `needs_reply=true, ask_kind=request`;
    - an `inquiry_promote` pass;
    - result: one `ready` human task, actor `promote:inquiry`, `verdict_tier: 2`, a promotion row naming
      the tier-2 extraction.

    Control: the same message on a project with `inquiry_hosted_after` NULL produces no task.
19. **Fallback, end to end.** The fake returns 503 forever, and the fixture's tier-1 `ai_runs.created_at`
    is set 31 min in the past through SQL. One promote pass creates a task with the fallback body line.
    At 29 min it is gated `hosted_pending`, writing no row and calling no tool. After the fallback
    promotion, the tier-2 inbox no longer returns the message.
20. `InquiryOutcomes` and `CountersByLane` count `classify_inquiry_hosted` promotions under the inquiry
    lane, and `--outcomes` prints the tier split (1 / 2 / fallback) as counts. The existing outcome
    tests pass with zero tier-2 rows, byte-identical. The FIRST-dismissal mutation test still bites.

### Part A — pipeline, CLI, report, eval

21. `internal/pipeline` gains `EventInquiryHostedClassified = "inquiry_hosted_classified"` and
    `StageInquiryHosted = "inquiry_hosted"`.
    - `Subscribers(inquiry_classified)` = {inquiry_promote, inquiry_hosted}.
    - `Subscribers(inquiry_hosted_classified)` = {inquiry_promote}.
    - The downstream map gives `StageInquiryHosted → EventInquiryHostedClassified`.
    - The contract tables in `contract_test.go`/`downstream_test.go` are extended.
    - The stage heartbeats on `ops/workers/pipeline.inquiry_hosted/status`, retained, with LWT `dead`.
    - `inquiryHostedStageSince >= promote.InquiryMaxAge` (test, C-D6's pattern).
22. `TestPipelined_EveryRouterIsLocalOnly` is REWRITTEN, not deleted, to its new truth:
    - every `provider.NewRouter` in cmd/pipelined has `nil` general EXCEPT exactly one, in
      `inquiry_hosted.go`, whose general is the `NewHostedOpenAI` value and whose local is `nil`;
    - `NewOpenAI(` and `api.openai.com` stay banned in every file;
    - `INQUIRY_HOSTED_` appears only in `inquiry_hosted.go`;
    - `route.go`'s guard is unchanged;
    - the positive control (≥1 router found) stays.
23. CLI: `classify hosted --since <dur> [--limit N]` runs one tier-2 pass (hand smoke).
    `classify report --lane inquiry` appends a tier-2 block (escalated, gpt needs_reply, gpt not,
    refused, errors, pending, by channel) ONLY when the window has tier-2 runs, so with none the report
    is byte-identical (the golden test).
24. `classify eval --lane inquiry --two-tier`:
    - Tier 1 is scored on the local router, the refusal unchanged.
    - Each labelled message with a false tier-1 answer AND `classOf(m) == ClassGeneral` (criterion 13's
      fixed loader) is sent to the hosted router.
    - A hosted error scores as `needs_reply` (D6's production behaviour) and is counted.
    - It prints the tier-1 block byte-identical to `--lane inquiry`'s, then a composite block (caught /
      flagged / escalated / refused / hosted errors).
    - Below `EvalResultThreshold` it prints COUNTS and `EvalIndicativeMarker`, never a ratio.
    - It refuses `--checkpoint`: the binding does not cover the hosted model.
    - It writes NO `ai_runs`/`ai_extractions` (re-asserted).
    - It refuses without the `INQUIRY_HOSTED_*` env.
    - Unit: a labelled restricted message makes zero hosted calls.
25. Migration `0050_inquiry_hosted_tier.sql` is exactly `ALTER TABLE projects ADD COLUMN
    inquiry_hosted_after TIMESTAMPTZ;`, with a comment stating "NULL = off", "no default, no backfill"
    and the arming UPDATE as a comment only. The classify migration ledger learns 0050, and a per-file
    guard pins the single statement and the absence of any UPDATE/INSERT.

### Part B — alert reopen on armed projects

26. In `decideMessage`'s found-ref branch, the swb 650 condition becomes:
    ```
    resurface && !winner.gateOn &&
      (!winner.inquiryArmed || (!winner.commTask && !bucketKey(winner.rule)))
    ```
    The SWT-78 item-E DM check stays BEFORE it, unchanged. On the armed branch the reason reads `logged
    onto closed task N; rule R on P is not a comm rule, so a reopen into INCOMING instead of
    resurfacing (swb 2668): <requested>`. The unarmed reason is unchanged byte for byte.
27. `bucketKey(r *Rule) bool` is pure and lives in `internal/capture/rules.go` beside `externalKey`,
    whose comment points at it. Table test: `CCW` true, `\b(WEB|API|OPS)\b` true, `(?i)alarm` true,
    `LHH-[0-9]+` false, `\A[^\n]*` false, `ALARM: "([^"]+)"` false, an uncompilable regex false (no key
    is ever derived), a `sender` rule without `key_regex` false (thread key), and a `sender` rule with
    `key_regex` `x{2,}` false.
28. Integration, with the project ARMED through the columns (`ai_inquiry=true, inquiry_promote_after =
    now()`) and each case on a closed task:
    - (a) per-thread keyed non-comm rule (lyleThread / rule-127 shape), human sender: task `ready`,
      marked by the message (`activity_by_message_id`), `resurface=false`, reason names swb 2668,
      `Revived=1, Resurfaced=0`;
    - (b) the same rule after `UPDATE capture_rules SET comm_task = true`: `resurface=true`, task stays
      closed, no `task_reopen` audit row;
    - (c) bucket rule (the suite's existing `bucket` helper): `resurface=true`, stays closed;
      `TestCaptureResurface_Integration_AHumanOnAClosedTaskResurfaces` is this control and passes
      UNMODIFIED;
    - (d) notifier sender on (a)'s rule: only logs;
    - (e) open dismissal: SWT-36's path, unchanged;
    - (f) gated project: unchanged.

    Mutations: select `false` for `r.comm_task` in `loadRules`, so (b) reopens; select `true` for the
    armed expression, so the unarmed swb 650 tests go red; drop `!bucketKey(...)`, so (c) reopens. All
    red.
29. Shadow mode records the same `resurface` value and reason wording ("would revive") and calls
    nothing. The existing shadow test is extended with case (a). `opsctl capture-rules try`'s
    simulation agrees with live on (a)/(b) (its dry-run shares `decideMessage`).
30. Every swb 650 test and every other `resurface_integration_test.go` test passes unmodified, except any
    that used an armed project with a non-comm, non-bucket keyed rule on a closed task. test-author lists
    those, and each is rewritten to its new truth with a comment naming swb 2668, never deleted.

### Docs

31. IK:
    - the "Closed-task chats resurface" section's swb 650 paragraph gains a dated **swb 2668** bullet
      (armed + non-comm + non-bucket keyed reopens; comm and bucket keep resurfacing; the unarmed bucket
      residual stands);
    - the `eks` section's "Residual: … resurfaces into the inquiry model" line gets a dated correction in
      place, not deleted;
    - a new **"Inquiry tier 2 (codex-proxy)"** entry covers: the worker_type, D2's routing, D4's
      refused row, D5's column and kill switch, D6's deadline and fallback, the codex-proxy LocalityOf
      landmine and its three guards, D7's env names, and the measured refusal share from V5.
32. `docs/runbooks/local-classifier.md` gains a "Tier 2" section (arming order, kill switch, reading the
    report block, the eval command, the fallback line). `docs/runbooks/HANDOFF-kube-inquiry-two-tier.md`
    lists the deploy rows below.

## Data model changes

`migrations/0050_inquiry_hosted_tier.sql` (next free, forward-only):

```sql
-- 0050 inquiry tier 2 (swb 2668): the per-project cutover for the hosted second opinion.
-- NULL = off. No default, no backfill. Arm after the smoke:
--   UPDATE projects SET inquiry_hosted_after = now() WHERE slug IN ('collaboratory', ...);
ALTER TABLE projects ADD COLUMN inquiry_hosted_after TIMESTAMPTZ;
```

There are no new tables. Tier-2 verdicts are `ai_runs` + `ai_extractions` rows under
`worker_type='classify_inquiry_hosted'`, and `ai_runs.status` gains the VALUE `refused` (there is no
CHECK to alter). Part B has no migration: it reads `capture_rules.comm_task` (0040), `key_regex`,
`pattern`, and `projects.ai_inquiry`/`inquiry_promote_after`, which `loadRules` already selects.

**Landmine (0034/0045 precedent):** new promote and classify binaries SELECT `p.inquiry_hosted_after`.
Apply 0050 BEFORE the image roll, or every inquiry pass fails.

## API / MCP tool changes

None. No executor tool is added or changed.

- **Tier 2 makes no tool call.** It writes `ai_runs`/`ai_extractions` only, like every classify lane.
  `classify.Store` still has no task-write method.
- **The promoter** keeps its existing executor path as `promote:inquiry`: `create_task` →
  `recordTask` → `task_set_source_thread`, or attach/guarded reopen. Every call is still validate →
  policy → audit → handler (invariant 3). Only the verdict source changes.
- **Part B's reopen** is the existing `reviveRuleTask` (`task_reopen` with `revive: true`) followed by
  the existing activity mark, as capture's actor through the executor. That is exactly the swb 650 path,
  with no new call site shape.

## MQTT topics

| topic | retained | QoS | payload | publisher | subscriber |
|---|---|---|---|---|---|
| `ops/pipeline/inquiry_classified` | no | 1 | `pipeline.Wake` | inquiry (unchanged) | inquiry_promote, **inquiry_hosted (new)** |
| `ops/pipeline/inquiry_hosted_classified` (new) | no | 1 | `pipeline.Wake` | inquiry_hosted, after ≥1 terminal row | inquiry_promote |
| `ops/workers/pipeline.inquiry_hosted/status` (new) | **yes** | 1 | `fleet.Status`, LWT `{"state":"dead"}` | inquiry_hosted | fleetd mirror, mosquitto_sub |

Wake-ups only, never work (E-D1).

## Files likely to touch

Part A:
- `migrations/0050_inquiry_hosted_tier.sql`
- `internal/provider/locality.go` (`Descriptor.Hosted`, `knownHostedRelayHosts`), `openai.go`
  (`NewHostedOpenAI`), `locality_test.go`, `router_test.go`, `callsites_test.go` (the entry-point row
  for `RunInquiryHosted`)
- new `internal/classify/inquiry_hosted.go` (`RunInquiryHosted`, `InquiryHostedWorkerType`,
  `HostedAdvisoryLockKey`, D4's recording), plus `_test.go` and `_integration_test.go`
- `internal/classify/store.go` (the tier-2 inbox `inboxWhereInquiryHosted`, criterion 13's loader fix,
  a `TryHostedLock`)
- `internal/classify/summary.go`, `report.go` (the tier-2 block), `eval.go` (`--two-tier`),
  `structure_test.go` (ledger 0050, the URL/confidence scans cover the new file automatically)
- `internal/promote/inquiry.go` (`InquiryFinal`, `InquiryHostedDeadline`, `GateHostedPending`, the
  inbox, the body line), `outcomes.go`, `store.go`/`summary.go` (worker_type → lane)
- `internal/pipeline/contract.go`, `downstream.go`, and their tests
- new `cmd/pipelined/inquiry_hosted.go`; `cmd/pipelined/main.go` (stage registry);
  `route_stages_test.go` (criterion 22)
- new `cmd/classify/hosted.go`; `cmd/classify/main.go` (`hosted` subcommand, `--two-tier`);
  `labels_test.go` (amended)
- `docs/runbooks/local-classifier.md`, new `docs/runbooks/HANDOFF-kube-inquiry-two-tier.md`

Part B:
- `internal/capture/rules_store.go` (the condition at the swb 650 block, about line 1234; the reason)
- `internal/capture/rules.go` (`bucketKey`), `rules_test.go`
- `internal/capture/swb650_reply_reopens_integration_test.go` (armed cases), and
  `resurface_integration_test.go` (only the rewrites criterion 30 names)

Both: `.claude/INSTITUTIONAL_KNOWLEDGE.md`.

## In scope / Out of scope

**In scope:** Parts A and B as above: the provider-boundary hardening for codex-proxy, tier 2 as a
stage and a CLI, the promoter's final-verdict rule and fallback, the composite eval, migration 0050,
the bucket guard, docs and the kube handoff document.

**Out of scope (do not bundle):**
- Editing kube manifests or creating the token secret. The kube session owns them (memory: "Kube
  manifests belong to the kube session"). This ticket ships the image and the handoff.
- Changing qwen's prompt, `InquiryPromptVersion`, the tier-1 pin, or tier-1 thresholds. A
  `--two-tier` eval delta is information for a later prompt ticket.
- A GPT pass over qwen's TRUE verdicts (precision). Owner decision (A) is one-directional.
- Narrowing `classOf`'s neighbour fold to the six-message transcript. It would raise tier 2's reach on
  Slack conversation keys, but it changes SWT-21's fold. See Future work.
- The unarmed-project bucket residual (D8).
- Disabling, re-keying or re-pointing any capture rule. VB0 may conclude one needs it: that is a data
  change via `capture_rule_set_enabled` / `capture-rules add`, as its own swb task.
- Raising the inquiry labelled set to 120. `/funnel` tier-2 rendering (the report block is enough to
  verify).
- The O7-style status of promoted tasks (`inquiryCreateStatus` stays `"ready"`).
- Triage/drafts/planimport moving to codex-proxy. Their `OPENAI_*` wiring is untouched.

## Invariants that apply

1. **Raw-first.** Tier 2 reads `normalized_messages`/`normalized_threads` and the stored tier-1
   extraction only. The classify structure scan (no `raw_source_items` outside B-D9's constant, no
   `raw_json`, no `net/http`/`mime`) covers `inquiry_hosted.go` automatically. Part B decides from the
   rows `loadRules`/`pendingMessages` already read.
2. **One funnel.** No new table and no task-like row. Tier-2 verdicts are `ai_extractions`. Every task
   still comes from the one promoter (inquiry lane) or capture (Part B) into the one `tasks` table.
   Fallback tasks are ordinary inquiry tasks distinguished by a body line, not a second store.
3. **Everything through the executor.** Tier 2 calls no tools (criterion 10's "no task write" holds
   structurally). The promoter's calls are unchanged executor calls with audit start/complete. Part B's
   reopen is `task_reopen` (revive form) through the executor, audited as capture's actor. Criterion
   28(a) asserts the audit row.
4. **Nothing external without a delivery row.** A hosted model call is not an outbound communication:
   nothing reaches a client surface. No `deliveries` row, no target construction.
5. **Own-message loop closure.** Both inboxes stay `direction='inbound'`, and our own sends are never
   classified as an ask. The transcript keeps outbound context (SWT-33 D2), which is what lets GPT see
   "already answered".
6. **Stealth attribution.** Not relevant. Nothing client-visible is produced.
7. **Orchestrator purity.** The orchestrator is untouched. The deciders are pure and table-tested:
   `promote.InquiryFinal`, `promote.InquiryGate`, `capture.bucketKey`, `capture.resurfaces`,
   `provider.LocalityOf`/`Decide`. Every task write leaves audit rows.

**SWT-21 locality, concretely:**

- The hosted client is reachable only as a router's GENERAL client, routed on the unpinned `classOf`
  fold (target + every inbound thread neighbour, `MostRestrictive`, unseen/unmatched = restricted).
- Restricted content gets a recorded refusal and zero calls (criterion 8, column-fed).
- The codex-proxy LAN address cannot make it "local" (criteria 1-5).
- Tier 1's pin and nil-general router are untouched (criterion 12).
- The eval loader's unconditional `AttrProject` is fixed before anything hosted reads it (criterion
  13).
- A routed message (SWT-40 B) reaches ClassGeneral only through its human-written
  `source_account_projects` authorisation (B-D1), which is the existing rule.

## Sibling patterns to copy

- Lane-scoped hosted builder with refusals: `cmd/triage/main.go:61-140`. The env shape differs (D7).
- Stage wiring, lock-held mapping, heartbeat: `cmd/pipelined/inquiry.go`, `route.go`.
- Inbox with a per-worker_type `NOT EXISTS` and the cutover clause: `internal/classify/store.go`
  `inboxWhereRoute` (its `route_after` verdict-clock clause is D5's exact shape).
- Pure gate plus table test, reasons in order: `promote.InquiryGate` / `InquiryGateReasons`.
- Column-fed mutation tests: `swb650_reply_reopens_integration_test.go`'s header ("MUTATIONS: …").
- A guard rewritten to a new truth: `TestMigration0018_IsTheOnlyOneThisTicketAdds` in
  `internal/classify/structure_test.go`.
- Fake OpenAI server with a request counter: `internal/provider/openai_test.go`.
- Queue claims: not applicable. Tier 2 uses an advisory lock, `PGStore.TryLock`'s explicit-unlock
  shape.

## Verification protocol

**Preconditions (stop and decide if either surprises):**

- **V0, codex-proxy speaks strict json_schema.** With the token, run one `classify hosted --since 72h
  --limit 1` against a scratch armed fixture, or a curl of `/v1/chat/completions` with
  `response_format.json_schema.strict=true` and `InquiryVerdictSchema`. Expect a parseable five-field
  verdict.
  - `GET /v1/models` lists the model id that goes into `INQUIRY_HOSTED_MODEL`.
  - If the proxy rejects `response_format`, the adapter needs work first. Stop.
- **VB0, the armed-project rule census.** Read-only, on prod:
  ```sql
  SELECT r.id, p.slug, r.criteria_type, r.pattern, r.key_regex, r.external_system, r.comm_task,
         r.priority, r.note
    FROM capture_rules r JOIN projects p ON p.id = r.project_id
   WHERE r.enabled AND r.external_system IS NOT NULL AND NOT r.comm_task
     AND p.ai_inquiry AND p.inquiry_promote_after IS NOT NULL
   ORDER BY p.slug, r.priority DESC;
  ```
  Then, per rule, the distinct-key spread over 90 days:
  ```sql
  SELECT cd.matched_rule_id, count(DISTINCT cd.external_key) AS keys, count(DISTINCT nm.thread_id) AS threads,
         count(*) AS msgs
    FROM capture_decisions cd JOIN normalized_messages nm ON nm.id = cd.message_id
   WHERE cd.mode='live' AND cd.action IN ('task','task_log') AND cd.created_at > now() - interval '90 days'
     AND cd.matched_rule_id = ANY(<ids above>)
   GROUP BY 1 ORDER BY keys;
  ```
  - Run `bucketKey` over each listed rule (a throwaway test reading the export).
  - Expect rule 10 (if still enabled) to be a bucket, and rule 129 to be `keys=1`, which D9 accepts.
  - Any OTHER rule with `keys` ≪ `threads` is a de-facto catch-all: stop and decide (re-key or comm)
    before deploying B.
  - Also confirm `no-reply@sns.amazonaws.com` and the budgets sender are NOT in collaboratory's
    `notifier_senders`. If they are, `resurfaces()` already refuses them and B is a no-op for alerts:
    record that and decide.
  - Record the result in IK (criterion 31).

**Before commit:**

1. `go test ./...` covers the pure tables (`InquiryFinal`, `InquiryGate`, `bucketKey`, `LocalityOf`,
   `Router`) and the structural guards (criteria 12, 22, 25, callsites).
2. `make integration` (`-p 1`). The new suites clean their fixtures in FK order under a test marker.
3. `/ticket-review inquiry-two-tier`, with the codex adversarial pass. This diff crosses the locality
   boundary and the promotion path.

**After deploy (the kube session applies the handoff):**

4. `psql … -tAc "SELECT max(version) FROM schema_migrations"` shows ≥ 50 before the image rolls.
5. **The tier-2 smoke, before arming.**
   - `UPDATE projects SET inquiry_hosted_after = now() - interval '72 hours' WHERE slug='collaboratory'`
     inside a transaction you ROLL BACK after reading `classify hosted --since 72h --limit 10`'s stats
     line (hand-run, `DATABASE_URL` + `INQUIRY_HOSTED_*` exported).
   - Better: run it against the 30-day scan's 15 messages via `--two-tier` eval-style ids.
   - Record ok/refused/error counts BY CHANNEL. A channel at ~100% refused is tier 2 inert there (the
     conversation-key neighbour fold). Write that beside the number in the IK, not a fix.
6. **Composite eval.** Run `go run ./cmd/classify eval --lane inquiry --two-tier`. Record counts (n<120:
   counts and marker only) as a dated runbook row. Also check that 1623435 is escalated and flagged
   when labelled; if it is not in the set, check it by hand with `classify hosted` on a rolled-back
   arming.
7. **Arm:** `UPDATE projects SET inquiry_hosted_after = now() WHERE slug IN ('collaboratory','a-millon')
   AND ai_locality='any' AND ai_inquiry AND inquiry_promote_after IS NOT NULL;`, then
   `mosquitto_sub -t 'ops/workers/+/status' -v | grep pipeline.inquiry_hosted` shows `idle`/`working`.
8. Within a day:
   ```sql
   SELECT r.status, e.fields->>'needs_reply', e.fields->>'refused', count(*)
     FROM ai_runs r LEFT JOIN ai_extractions e ON e.ai_run_id = r.id
    WHERE r.worker_type='classify_inquiry_hosted' AND r.created_at > now() - interval '1 day'
    GROUP BY 1,2,3;
   ```
   Also check `classify promote --lane inquiry --outcomes` for the tier split, and that any task with
   `verdict_tier: fallback` is explained by an error run.
9. **Part B, live:** after the next re-fired alarm or budget mail on a closed collaboratory task:
   ```sql
   SELECT cd.message_id, cd.task_id, cd.resurface, cd.reason, t.status, t.activity_by_message_id
     FROM capture_decisions cd JOIN tasks t ON t.id = cd.task_id
    WHERE cd.mode='live' AND cd.reason LIKE '%swb 2668%' ORDER BY cd.id DESC LIMIT 10;
   ```
   Expect `resurface=false` and a non-closed task marked by the message, visible in INCOMING on the
   board (check it in Chrome: "verify live yourself"). Before one arrives, `opsctl capture-rules try` on
   a recent alarm message against a closed task shows the same reason.

## Deploy handoff (for `docs/runbooks/HANDOFF-kube-inquiry-two-tier.md`; the kube session applies it)

**Order:** 0050 → image roll (ONE tag to every workload, keeping existing pins: classify-promote
`--lane personal`, pipelined's current `PIPELINE_STAGES`, `MS_OAUTH_CLIENT_ID` on connector-google) →
smoke (V5/V6) → enable the stage → arm (V7). Never arm before the stage is running, or D6's fallback
fires after 30 minutes.

| workload | change |
|---|---|
| **pipelined** (Deployment, ns `ops`) | image bump; `PIPELINE_STAGES` += `,inquiry_hosted` (`gate,route,route_apply,inquiry,inquiry_promote,calendar_push,inquiry_hosted`, or whatever is current plus `inquiry_hosted`); add env `INQUIRY_HOSTED_URL=http://192.168.50.48/v1`, `INQUIRY_HOSTED_MODEL=<id from V0's GET /v1/models>`, `INQUIRY_HOSTED_KEY` from a new Secret in ns `ops` (e.g. `codex-proxy-token`, key `token`), whose value is the codex-proxy bearer token from ns `codex-proxy`. Never put it in a ConfigMap. `OPS_LOCAL_PROVIDER_URL=http://192.168.50.55:11434` and `OPS_LOCAL_MODEL=qwen3:8b` unchanged |
| connector-* CronJobs | image bump only (Part B ships in capture, which runs in every connector main: every capture writer must roll to the same tag, SWT-78's rule) |
| classify-personal / classify-residue / classify-promote | image bump only; no hosted env (they never run tier 2) |
| NetworkPolicy (if ns `ops` has egress policies) | allow pipelined → `192.168.50.48:80` |

**Rollback:** `UPDATE projects SET inquiry_hosted_after = NULL` (instant, no deploy), then drop
`inquiry_hosted` from `PIPELINE_STAGES`. Part B has no flag: rolling the image back restores
resurfacing. 0050 is additive and stays.

## Future work (not this ticket)

- Narrow `classOf`'s neighbour fold to the messages whose bodies actually travel in the prompt (the
  6-message transcript plus the target). Today `neighbours()` folds EVERY inbound message of a
  conversation-level Slack key, so one undecided message anywhere in a channel restricts every ask in
  it. V5's per-channel refusal share is the measurement that justifies or kills this.
- The unarmed-project bucket residual (D8): a bucket match on an unarmed project reopens a catch-all.
- A hosted second opinion on qwen's TRUE verdicts (precision), if `--outcomes` shows false positives
  rising.
- Exclude fallback promotions from `InquiryOutcomes`' precision readout, or report them on their own
  line, if outages make them a material share.
- An `ai_runs.status` CHECK now that the value set (`ok`, `error`, `skipped`, `refused`) is stable.

## VB0 result (run 2026-10-28 by the coordinating session)

Enabled, non-comm, keyed rules on armed projects: 10, all collaboratory — 64 (github PRs, 78 keys),
71/72/73 (Jira WEB/API/OPS thread keys, 59/27/1 keys), 70, 124, 126, 127, 129, 132 (0-1 live keys so far:
new since 2026-10-27). No rule's key is a de-facto constant beyond the expected #2640 (129) and per-threshold
(132) shapes (D9). `no-reply@sns.amazonaws.com` and the budgets sender are NOT on collaboratory's
`notifier_senders` (`Jira, jira@treetopllc.jira.com, notifications@github.com, noreply@github.com,
no-reply@github.com, no-reply@builds.circleci.com`), so Part B reaches alert mail. GitHub PR mail (64) stays
log-only through the notifier clause; Jira 71-73 through the connector-copy clause. VB0: pass.
