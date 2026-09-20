> Jira: SWT-68 · swb task #421

# Diagnosis — receipts-become-tasks

## Root cause

The verdict is wrong at the leaf, and the spine has no second gate that could catch it.
`internal/classify/prompt.go`'s `VerdictSchema` (lines 39-53) offers the model five `kind`
values — `payment_due | deadline | appointment | action_required | informational` — **none of
which names a receipt, a charge confirmation, an autopay notice or a refund**, so the only
money-shaped value available for a PayPal "You paid $24.99 to Valve Corp." is `payment_due`,
whose very name asserts that a payment *is* due. The `SystemPrompt` (lines 74-115) lists
"receipts and confirmations for something already done" under `actionable = false`, but two
stronger clauses pull the other way: the `actionable = true` list opens with "*a payment or
bill that is due, or a balance that must be paid*" (every one of these 19 mails names a payment
and an amount) and the explicit tie-break "*RECALL IS THE OBJECTIVE … When you are genuinely
torn, answer true*". qwen3:8b under `classify-v1` resolved that conflict toward `true` in 19 of
19 cases. The consuming side then has exactly one content gate: `internal/promote/store.go:294`
selects only `e.fields->>'actionable' = 'true'`, and `promote.Decide`
(`internal/promote/promote.go:145-159`) branches on `Kind` alone. So **the whole of the
deterministic spine's defence against a wrong verdict is one boolean authored by the same model
that chose the wrong kind** — and when that boolean is wrong, `Decide` returns
`{Action:"task", Status:"ready"}` with nothing left to stop it. `Decide` "consumes only Kind"
is not the defect; the defect is that the field it *would* have consumed, `actionable`, was
already spent as the inbox filter and there is no third signal (the schema deliberately has no
`confidence` field — prompt.go:23-29 records that qwen3:8b returns 0.95 for everything).

## Evidence

### Where the verdict is produced

- `internal/classify/prompt.go:39-53` — `VerdictSchema`. `kind` is a closed enum of five;
  there is no receipt-like member. `additionalProperties:false`, so the model cannot invent one.
- `internal/classify/prompt.go:23-29` — the comment recording why there is **no** `confidence`
  field: "*qwen3:8b returns exactly 0.95 for everything it flags — 27 true positives and 17
  false positives, identical … do not fake a threshold*". This is why per-field confidence is
  not available to the promoter: it does not exist, on purpose.
- `internal/classify/prompt.go:79-96` — the two competing clauses, verbatim:

  ```
  actionable = true when the message requires the recipient to DO something, and
  there is a consequence for not doing it:
    - a payment or bill that is due, or a balance that must be paid
  ...
  actionable = false for pure information, even when it sounds urgent:
    - "your statement is available", "your statement is ready"
    - balance notices, low-balance alerts, "your available balance"
    - "your card was used", transaction and deposit notifications
    - receipts and confirmations for something already done
    - marketing, offers, newsletters, community announcements, event invitations

  RECALL IS THE OBJECTIVE. A missed payment or fine notice costs a late fee; a
  false alarm costs one second to dismiss. When you are genuinely torn, answer
  true.
  ```

  The receipt clause is **one bullet**; the recall tie-break is a standalone paragraph with an
  explicit instruction for exactly the ambiguity these mails create. The prompt gives the model
  **no way to say "money moved, nothing to do" other than `actionable:false` +
  `kind:informational`** — a two-field answer where the `kind` half is a poor fit for a mail
  that is entirely about a payment.
- `internal/classify/prompt.go:61-67` — the near-miss clause was tuned and measured against
  **Bank of America** (883 messages). PayPal was never in that measurement.
- `internal/classify/lane.go:48-54, 83-90` — the personal lane uses `ActionabilityContract`
  (`DecisionKey:"actionable"`, `CategoryKey:"kind"`), the same `Contract` value as the residue
  lane. The enum is therefore shared by two lanes.
- `internal/classify/classify.go:652-660` — `renderMessage` caps the body at 4,000 chars.
  **Verified read-only that truncation is NOT a factor:** for runs 12431, 12464, 12539, 12554,
  12577 and 12613 the stored `ai_runs.input->>'user_prompt'` is 2,494-3,158 chars, none contains
  `(truncated)`, and the decisive phrases were present — run 12554 (task 412) contains
  "nothing you need to do" and the model still answered `actionable:true, kind:payment_due`
  with the reason "*…and there is nothing the recipient needs to do*". Provider `ollama`,
  model `qwen3:8b`, `prompt_version` `classify-v1`, `status ok`. **The model was shown the
  evidence and decided against it.**

### Where the verdict is consumed

- `internal/promote/store.go:278-297` — `inbox`. `WHERE e.fields->>'actionable' = 'true'` is
  line 294. `actionable` **is** read, and it is the only content predicate in the query; the
  rest is lane, project, cutover and claim bookkeeping.
- `internal/promote/promote.go:28-31` — `var whitelist = {"payment_due", "deadline"}`.
- `internal/promote/promote.go:145-159` — `Decide`. Rule order: attach-to-open → attach+reopen
  → inquiry-lane branch → `whitelist[v.Kind]` → `ready` task → else `holding` review.
- `docs/tickets/classify-promotion_SPEC.md` D2 (lines 57-63) records why the whitelist is a Go
  constant and not configuration; criteria 2, 7 and 8 (lines 95-136) record that the
  `actionable` filter lives in the inbox and that non-whitelisted kinds go to a `holding`
  "human-review lane, never a live task". Nothing in the SPEC contemplates a verdict that is
  `actionable` **and** whitelisted **and** wrong; the design treats `actionable` as trustworthy.
- git blame: the receipt clause and the recall tie-break are original to `abc5466`
  (2026-08-28, SWT-22); the whitelist and the `actionable` inbox filter are both `dea97df`
  (2026-09-09, SWT-30). **Neither is a recent change and neither is a regression** — the two
  halves were written a fortnight apart and each is correct in isolation.

### What the board actually does with the two outcomes

- `internal/dashboard/board.go:385-386`:
  `from_message = t.id IN (SELECT cp.task_id FROM classify_promotions cp WHERE cp.action IN ('task','review'))`.
- `internal/dashboard/sections.go:31-39, 62-67` — an `Incoming != ""` row goes to the board's
  **FIRST** section, "arrivals — incoming", whatever its status, unless closed or green.
- Therefore **a `holding` review row is on the board, in the same section, above everything
  else**. Demoting receipts from `ready` to `holding` moves them zero pixels. Confirmed by the
  data: of the 17 `action_required`/`appointment`/`informational` rows the promoter put in the
  review lane, **16 were closed and 14 carry a first dismissal of `not_actionable`** — Salvador
  dismisses the review lane too.

### Measured pipeline precision (read-only, 2026-09-19)

Every post-cutover `classify_promotions` row, by kind, action, and first dismissal code:

| kind | action | n | first dismissal |
|---|---|---:|---|
| payment_due | task | 25 | not_actionable |
| payment_due | task | 6 | (none — closed with Done) |
| payment_due | task | 3 | handled_elsewhere |
| deadline | task | 3 | not_actionable |
| action_required | review | 14 | not_actionable |
| action_required | review | 3 | (none/open) |
| appointment | review | 1 | not_actionable |
| informational | review | 1 | not_actionable |
| informational | review | 1 | wrong_kind |

**37 auto-created `ready` tasks; 28 first-dismissed `not_actionable`; 3 `handled_elsewhere`.**
Scored by the repo's own existing fold (`promote.InquiryOutcome`: `handled_elsewhere` and
closed-without-dismissal are true positives), pipeline precision is **9/37 = 0.24**. If the six
Done-closed receipts are counted as the false positives the REPRO says they are, it is
**3/37 = 0.08**. The lane's committed eval baseline is 0.94 recall / **0.50 precision**
(`docs/runbooks/local-classifier.md:457-460`, 280 labels, 2026-08-31) — so the promoter was
wired on top of a classifier already measured at coin-flip precision, and auto-creates a live
task from every positive.

## Why the reproduction fails

`go test ./internal/promote -run TestReceiptsBecomeTasks` is red (confirmed by running it) because
each production verdict it replays carries a whitelisted `Kind`, and `Decide` reaches
`promote.go:155` with `existing == nil` and returns `{Action:"task", Status:"ready"}`. The test
is a faithful slice of production: the rows it replays passed `store.go:294` (they are all
`actionable=true`), had no open task on their thread, and their project (`personal`, id 6) had
`ai_classify=true` and a cutover of 2026-09-09 that their verdict clock cleared. The test
cannot go green by fixing `Decide` alone while `Kind` stays `payment_due`, which is precisely
the shape of the bug: the input to the pure function is already wrong.

**Why 19 landed at once.** `source_accounts.id=14418` (`sspataro57@msn.com`) was created
**2026-09-19** — the day of the report. Its historical mail was ingested and classified that
day, and the cutover is compared against the **verdict clock** (`r.created_at >=
p.classify_promote_after`, store.go:293; SPEC Q2 chose this deliberately), not the message's
`sent_at`. So an entire backfill became promotable in one pass. Verified: task 214 was promoted
from a message **134 days old** (sent 2026-05-05, verdict and task 2026-09-16). The
`--max-age` fence that exists for exactly this — "*the 72h fence is what keeps historical asks
from becoming tasks*", `cmd/classify/main.go:175-179` — is **refused on any lane but inquiry**.
This is a contributing factor, not the root cause, and it is separable.

## Invariant implicated

**None of the seven numbered invariants is violated.** Invariant 7 holds — `Decide` is pure and
every decision writes its `classify_promotions` row; invariants 4 and 5 are untouched (promoted
tasks are `assignee_type='human'`, `priority=0`, and `personal` has `client IS NULL` so no
worker can claim them — SPEC D6).

What *is* violated is `CLAUDE.md`'s core principle — **"agency at the leaves, determinism at the
spine"** — and build-order step 6's rule, "*below threshold → human-review lane, never a live
task*". There is no threshold here to be below: the lane has one boolean, no confidence, and the
spine promotes on it directly. The fix should restore a gate, not patch `Decide`'s output.

Note also that the SPEC's containment argument ("non-whitelisted kinds land in a human-review
lane") is **no longer true in effect**: SWT-59 put `action IN ('task','review')` rows into the
board's first section. See "Landmine" below.

## Proposed fix scope

Recommendation: **(c) both, staged — (a) first, because it is the only half that satisfies the
stated expectation with no schema change, and (b) only as a deliberate backstop.**

Reasoning, stated plainly:

- **(b) alone cannot work as usually written.** "Require `actionable=true` AND a kind
  whitelist" is **already the production behaviour** (store.go:294 + promote.go:155) — it is an
  inert predicate, this repo's most-repeated landmine, and it would turn the repro green only if
  the repro fed it a verdict production never produces. All 19 rows satisfy both conjuncts.
- **(b) as a whitelist narrowing does not meet the expectation either.** The REPRO's expected
  behaviour is "not `ready`, **not on the board**". A non-whitelisted kind still creates a
  `holding` row, and `from_message` puts it in "arrivals — incoming" (board.go:385, sections.go:63).
  Creating *nothing* needs a new `classify_promotions.action` value, and the live CHECK is
  `action IN ('task','review','attached')` — a **migration 0038** with `cp.action IN (...)`
  updated in `board.go:386` in the same diff, or the new value silently drops out of the
  incoming section.
- **(a) satisfies the expectation for free.** If the verdict says `actionable:false`, the
  message never enters `inbox` (store.go:294), no promotion row is written, nothing reaches the
  board, and no code, schema or test outside `internal/classify` changes. The gate already
  exists and already works; the leaf lied to it.
- But **(a) is a prompt edit on a small local model with 0.50 measured precision**, so it is a
  probabilistic improvement, not a guarantee. Hence a backstop is still warranted — for a hard
  guarantee it must be a deterministic skip, not a kind re-route.

Fix tasks:

- [ ] **A1. Label first, then change.** Add the 19 as `label:"not"` and task 397's message
      (`normalized_messages.id=347452`, extraction 12290, `kind=payment_due`, a genuine
      bill-due) as `label:"actionable"` to `docs/evals/personal-actionability.jsonl`. Verified:
      **zero overlap today** — the file's ids run 11866-120458 and all 19 are ≥ 333709, so the
      failure mode is entirely absent from the baseline. Format is `{message_id, label,
      subject_sha256}` (eval.go:33-48); `note` must come from the closed vocabulary and
      `classify.OwnerBlanketNote` ("owner-blanket") is the right marker for "receipts are not
      actionable" — a blanket instruction, not 19 individual judgements. 280 → 300 labels keeps
      the file above `EvalResultThreshold` (120), so ratios still print.
- [ ] **A2. `internal/classify/prompt.go` `SystemPrompt`** — tighten `payment_due` to *a payment
      the recipient must make by hand, by a date that has not passed*, and add explicit
      `actionable=false` bullets for: a payment already made or authorized ("you paid", "you
      authorized", "payment received", "total … paid"); a scheduled or automatic charge the
      sender will collect ("we'll automatically charge", "nothing you need to do", autopay
      reminders); and a refund or money arriving. Add a **carve-out to the recall tie-break** for
      this family, because the tie-break is what the model is following. Do **not** trim the
      existing near-miss clause (prompt.go:61-67 records that its removal was measured).
- [ ] **A3. Bump `PromptVersion` `classify-v1` → `classify-v2`** (prompt.go:8). Mandatory: it
      is the only thing that lets a v1 and a v2 verdict be told apart later.
- [ ] **A4. Re-run the eval and record a new runbook row.** `DATABASE_URL="$OPS_DATABASE_URL"
      go run ./cmd/classify eval --lane personal` (note the IK landmine: `cmd/classify` reads
      `DATABASE_URL`, not `OPS_DATABASE_URL`). **Cost: 300 labels × 7.2 s ≈ 36 min** on the
      workstation GPU, **≈ 21 min** on the z4 (192.168.50.55, measured 4.1 s median). Acceptance:
      the four repro messages and the 19 flip to `not`; **recall must not fall below the 0.94
      baseline** — the 2026-09-07 thinking A/B is the warning (precision 0.50 → 0.67 bought at
      recall 0.94 → 0.57; a prompt that reads as "be more careful" can buy the same bad trade).
- [ ] **B1 (backstop, owner decision required). A deterministic receipt skip in the promoter.**
      The only shape that gives a hard guarantee: a `classify_promotions` row with a new
      `action='skipped'` and no task. Needs **migration 0038** (rewrite the CHECK; forward-only),
      `board.go:386`, and a pure predicate in `internal/promote` (not in `internal/classify` —
      `structure_test.go` keeps that package provider-free and this must stay pure). Note the
      trade-off honestly: a sender/phrase pre-filter is "rules in a costume" — the exact thing
      `prompt.go:55-60` rejects — with unbounded maintenance, and it would be tuned against one
      sender family (18 of the 19 are `service@paypal.com`).
- [ ] **B2 (cheap, independent, recommended). Make the personal lane's dismissals readable.**
      `promote.InquiryOutcomes` (outcomes.go:92-131) hard-codes
      `r.worker_type = 'classify_inquiry'`, and `cmd/classify/main.go:182` refuses `--outcomes`
      on any lane but inquiry. **The personal lane's 28 `not_actionable` labels are written by
      the dashboard and read by nothing.** Parameterising the worker_type and dropping the lane
      refusal turns the precision table in this document into a standing command.
- [ ] **C1. Regression test (test-author converts the repro).** `receipts_repro_test.go` must be
      rewritten: with fix (a) the correct assertion is at the classify layer (a verdict for these
      bodies is `actionable:false`), which is a model-dependent eval assertion, not a unit test.
      The model-free half that survives is the **positive control** — task 397's verdict still
      produces a `ready` task — plus, if B1 ships, a pure table over the skip predicate.

## Out of scope for this fix

- **Message age at promotion.** The cutover is the verdict clock, so onboarding a mailbox makes
  its whole backfill promotable at once; task 214 was promoted 134 days after its message was
  sent. `--max-age` exists (`cmd/classify/main.go:175-179`) and is inquiry-only. A stale but
  *correct* `payment_due` from May is still a bad board row. Separate ticket.
- **The six Done-closed false positives.** The board's Done button writes no `task_dismissals`
  row, so precision measured from dismissals reads 13/19 on the receipt set and undercounts by a
  third. A "this should not have existed" signal that is only available through one of two close
  buttons is a labelling gap, not this bug.
- **The residue lane.** Its prompt has the same recall tie-break and the same five-value enum,
  and its baseline is 0.59/0.28 over 874 labels. Untouched here on purpose (see Risk).
- **`promote.laneOfWorkerType`** (outcomes.go:148-158) does not know `classify_residue` and
  returns `unknown (worker_type …)`. Harmless today; noticed while tracing.

## Open questions

1. **Task 358 — Bank of America, "You scheduled a $25 payment to [a family member]. Please make sure
   there are sufficient funds"** — receipt or obligation? It is the one row in the 19 where a
   human act is implied, and it is one of the six Salvador closed with **Done** rather than
   dismissing. The prompt's new autopay bullet depends on the answer. Owner's call.
2. **Task 214 — a Mistral invoice PDF with no amount-due, no pay link and no date** — is an
   invoice *document* actionable? Separately: should a 134-day-old message promote at all?
3. **Are the six Done-closed rows false positives?** It changes the measured precision from 0.24
   to 0.08 and decides whether they can be labelled `not`.
4. **Does the owner accept a shared-enum change?** Adding a `receipt` kind touches the residue
   lane's schema through `ActionabilityContract` (lane.go:48) and makes its 874-label baseline
   stale (**re-run ≈ 2.9 h** at the measured 11.9 s median). The prompt-only route (A2) avoids
   this entirely. I recommend the prompt-only route first; I did not decide this.
5. **Why the model chose `true`** is attributed above to the recall tie-break and the missing
   receipt kind. That is a hypothesis about model behaviour: it is consistent with all 19 stored
   reasons and with the absent enum member, but it is only *provable* by an A/B eval (A4). I did
   not guess further.

## Risk assessment

- **Recall is the lane's stated objective and this fix trades against it.** The runbook's
  2026-09-07 row is the precedent: every precision gain measured on qwen3:8b so far cost recall,
  once catastrophically. A tightened `payment_due` could suppress a real bill. Positive control:
  extraction 12290 / message 347452 (task 397, Citi "Your payment due date is approaching") must
  stay `actionable:true, payment_due`.
- **The enum is shared and pinned.** `internal/classify/structure_test.go:279` asserts the exact
  five-value enum and fails on length first; `internal/classify/lane.go:48` makes
  `LanePersonal.Contract == LaneResidue.Contract` a structural fact asserted by `lane_test.go`.
  Any schema change reaches the residue lane and both its prompt's `Fields` block and its
  baseline.
- **Tests that pin today's behaviour** (each will need a deliberate edit, not a delete):
  `internal/promote/promote_test.go:72-110` (`classifyKinds` table — "exactly payment_due and
  deadline auto-create"), `promote_test.go:167-181` and `:282` (attach-beats-whitelist, both
  using `kind=payment_due` as the strongest possible whitelisted input),
  `internal/promote/store_integration_test.go:413-443` and `:597-613` (the inbox fixture,
  including the mutation note "drop `fields->>'actionable'='true'`" — that mutation is the proof
  the gate is not inert), `:831` (a non-whitelisted flag still writes a promotion row),
  `internal/classify/structure_test.go:279` (enum), `internal/classify/worker_test.go:595`
  (`Request.System == classify.SystemPrompt`), `internal/classify/lane_test.go:245-250`.
- **Other readers of the things being changed.** `kind`: `classify.go:225/436` (parse + store),
  `classify/summary.go:182` (the report's group-by — no hardcoded kind list, verified),
  `classify_promotions.kind` (`TEXT NOT NULL`, **no CHECK** — a new value stores fine),
  `promote.Verdict.Kind` → `Decide`. `actionable`: `store.go:294` and `summary.go:181` only.
  `Decide` has exactly two production callers, `promote/store.go:154` (personal) and
  `promote/inquiry.go:352` (inquiry) — and the inquiry lane returns before the whitelist
  (promote.go:152-154), so a whitelist change cannot reach it. `internal/triage` has its own
  unrelated `kind`; it is a different contract and is not affected.
- **No armed backlog, but the safety is the cutover, not a guard.** Verified read-only: **121**
  unpromoted personal-lane verdicts carry a whitelisted kind with `actionable=true` (120
  `payment_due`, 1 `deadline`), and **0 of them satisfy the full `inbox` predicate today** —
  every one is pre-cutover. They are inert *only* while `classify_promote_after` stays at
  2026-09-09 13:03:59Z; moving that timestamp backwards would promote all 121 at once. Their
  shape is the same failure, wider: 49 × Sallie Mae "Tuition Due? We can help!" (marketing),
  13 × "Your credit card statement is available" — the exact phrase the prompt names as **not**
  actionable — 7 × Citi autopay reminders. Anyone re-running classify over the MSN backfill
  under a new prompt version should confirm the cutover before the run.
- **Nothing outbound is in this path.** No delivery row, no send, no client-visible surface.

## Landmine matched

**One new, recorded in `.claude/INSTITUTIONAL_KNOWLEDGE.md`: "A later ticket can void an
earlier one's containment argument without touching its code."** SWT-30's criterion 8 argued
that a non-whitelisted verdict is safe because it lands in a `holding` "human-review lane, never
a live task" — a claim about *visibility*. SWT-59 then made `board.go:386` treat
`cp.action IN ('task','review')` as `from_message` and `sections.go:63` put every such row in
the board's FIRST section. The promoter's code never changed and its tests stayed green, but its
review lane is now the loudest place on the board — which is why "narrow the whitelist" is not a
fix here. Measured: 14 of 17 review-lane rows were dismissed `not_actionable`.

**One existing family matched, not new:** "a guard whose predicate is already true in
production". Requiring `actionable=true AND kind ∈ whitelist` inside `Decide` would be exactly
the SWT-18 / SWT-21 inert-predicate mistake — it reads like a new gate, it would make a
hand-written test pass, and it changes nothing for any row production has ever produced.

---

## What was verified vs inferred

**Verified by reading code:** prompt.go's schema, prompt text and the no-confidence rationale;
lane.go's shared contract; promote.go's whitelist and `Decide`; store.go's `inbox` including the
`actionable` filter; cmd/classify/main.go's `--outcomes` and `--max-age` lane refusals;
outcomes.go's hard-coded `classify_inquiry`; board.go's `from_message` and sections.go's
incoming-first rule; migration 0021's `action` CHECK; the pinning tests named above; SPEC D2 and
criteria 2/7/8; the runbook's eval baseline table.

**Verified by executing (read-only DB / `go test`):** the repro test is red for the stated
reason; the six stored prompts are untruncated and contain the decisive phrases; provider/model/
prompt_version of the runs; the two quoted stored reasons; the full promotion-outcome table by
kind and dismissal code; 121 unpromoted whitelisted verdicts and 0 currently promotable; zero
overlap between the eval label file and the 19; `source_accounts` 14418 created 2026-09-19;
message age at promotion (task 214 = 134 days); the live `classify_promotions_action_check`;
task 397's verdict row. The production database was **only read** — no writes, no advisory lock,
no classify or promote run.

**Inferred, not proven:** that the recall tie-break and the missing receipt kind are *why*
qwen3:8b answered `true` (consistent with all 19 stored reasons and with the enum, but only an
A/B eval proves it); that a tightened prompt will fix the class rather than a sample (that is
what A4 measures); whether task 358 is a receipt (open question 1).
