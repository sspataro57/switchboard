# Diagnosis — sana-email-not-captured (SWT-58)

## Root cause

The routing tier was never armed on prod. By design, an unarmed account is filtered out of `route_apply` in SQL, before any counter can see it. `capture.routeInbox` (`internal/capture/route.go:232-265`) selects only messages whose receiving account has `source_accounts.route_after IS NOT NULL` (`internal/capture/route.go:244`). On prod, account 1009 (`salvador@handsonconnect.org`), the only account with route candidates, has `route_after` NULL. So message 291568 never enters the pass:
- `DecideRoute` never runs for it, so no `Unrouted` reason is ever incremented (`internal/capture/route.go:198-201`);
- no `mode='route'` row is written.

Its latest decision stays `live/unmatched`, which fails `replyfold.InquiryEligibleLatestSQL` (`latest.action = 'attributed'`, `internal/replyfold/replyfold.go:127-130`). The inquiry classifier's inbox (`internal/classify/store.go:141-154`) therefore never offers it to qwen, and `inquiry_promote` has no verdict to act on.

NULL is deliberately "shadow" (SPEC B-D7, migration 0032). Arming is a hand-run `UPDATE` after an eval gate, and it was never done. The eval's label file, `docs/evals/route-from-rules.jsonl`, does not exist, so the go-live's step 5, V6 "B" (V5 reads → eval → arm → V6.5 backfill), never happened.

The gap is invisible because of the design above: route_apply's log line (`cmd/pipelined/route.go:88-96`) has no counter for "routable, but the account is not armed". It prints `written=0` with every reason at 0, the same line as an empty inbox. That is a real observability defect. It contradicts the comment directly above that log line ("'found nothing' and 'never ran' are different lines") and the explicit sentence the inquiry promoter prints for its own unarmed state (`internal/promote/inquiry.go:267-278`, C-D2).

**Verdict:** a go-live (operations) gap, the arming never done, plus a code gap, a missing counter. It is not a logic bug in the route, inquiry or promote code.

## Evidence

- `internal/capture/route.go:244`: `AND sa.route_after IS NOT NULL` in `routeInbox`. The doc comment at `:226-227` reads "the receiving account is ARMED (route_after set; B7, NULL writes nothing)". `RouteFacts.ArmedAt`, at `:80-82`, says "The driver never calls DecideRoute for an unarmed account."
- `internal/capture/route.go:44-60`: there are only four unrouted reasons, `pending_verdict`, `no_default`, `verdict_before_arming` and `candidate_revoked`. None of them can describe an unarmed account, because that account never reaches `applyRoute`.
- `cmd/pipelined/route.go:88-96`: the log line emits only `ByStep` and `Unrouted`, which is the observed `written=0 … candidate_revoked=0`.
- `migrations/0032_route_tier.sql:19-21,58`: "`route_after` is the per-account arming column (B-D7): NULL = routing off (shadow: verdicts only). It is set by a hand-run UPDATE after the shadow reads and the eval gate, never by code and never by this migration."
- `internal/capture/route_structure_test.go:178-200`: fails the build if any non-test Go file assigns `route_after`. Arming is deliberately human-only.
- **Runbooks.** Each says arming is a manual human step after the eval, never a deploy step:
  - `docs/tickets/inquiry-promote_SPEC.md:655-671` (B-D7);
  - `:709-717` (B7);
  - `:1235-1237` (V6 step 5: "enable `route` + `route_apply` in shadow (not armed) → the V5 reads → arm `route_after` → one `classify run --lane route --since 720h` backfill → confirm the 115");
  - `docs/runbooks/HANDOFF-kube-inquiry-promote.md` step 5 (line 92);
  - `docs/runbooks/local-classifier.md:880-906`;
  - `docs/runbooks/pipeline.md:104`.
- **Prerequisite missing.** `docs/evals/` holds `inquiry-needs-reply.jsonl`, `personal-actionability.jsonl` and `residue-actionability.jsonl`, but no `route-from-rules.jsonl`. The HANDOFF says: "With no label file there is no eval, and with no eval there is no arming."
- **git history:**
  - `f93b9fe` (2026-09-13) introduced Part B and the `routeInbox` filter together. `internal/capture/route.go` has no other commit.
  - Every later route-related commit concerns something else: `2b34875`/`d1f0b3b` (candidate `--provider`), `3c80a6b` (the handoff image tag), and `370b211` (only a SPEC mention).
  - No commit, runbook entry or IK line records arming or a scheduled go-live. The arming step fell off after the shadow deploy.
- **Prod, read-only (2026-09-15 18:2xZ):**
  - `source_accounts` 1009: `route_after` NULL, 2 candidates (collaboratory, the default; reengine). No other account has candidates, and none is armed.
  - `capture_decisions` by mode: live 5913, shadow 384950, **route 0**.
  - `classify_route` ai_runs: 21 ok, from 2026-09-13 05:43 to 2026-09-15 17:40. The `route` stage runs in shadow as designed.
  - Thread 159886:
    - 158690, inbound 09-10 22:06: live unmatched, then three shadow unmatched rows;
    - 201924, outbound 09-12 15:57: no decision;
    - 291568, inbound 09-15 15:33: live unmatched (390838);
    - verdict `ai_extractions` 11630 (15:41:08): project 4, grounded true.
  - Capture rule 60 (`(?i)\A[^\n]*\b(?:ce)?collaboratory\b`, the project name in the subject line) and rule 62 (sender `cecollaboratory.com`) cannot match the client's university-domain mail with this subject. `unmatched` is the correct rules outcome, and the route tier is the intended bridge.
- **Hypothesis verified locally** (`ops_sanabug`, with a temporary BEFORE INSERT trigger that set the fixture account's `route_after`, dropped afterwards; the repro file itself was unchanged):
  - `route_after = now() - 3h`, before the verdict: `route_apply written=1 model=1` → `inquiry processed=1` → `inquiry_promote review=1`. The repro **PASSES** and creates a Holding task.
  - `route_after = now()`, after the verdict: `written=0 … verdict_before_arming=1`, and it FAILS. This is B-D7's forward-only rule, and it bears on the arming plan below.
  - `route_after` NULL (the repro as committed): every counter is 0, and it FAILS.

## Why the reproduction fails

The fixture copies prod: account `route_after` NULL. `routeInbox`'s `sa.route_after IS NOT NULL` returns zero rows, so `route_apply` writes nothing and counts nothing. The message's latest decision stays `unmatched`, so `inboxWhereInquiry` never selects it and the fake model gets 0 calls. `inquiryInbox` has no `classify_inquiry` verdict to read, so no promotion or task follows. Arming the fixture account before the verdict makes all three stages pass unchanged.

## Invariant implicated

None is violated. The arming gate is working as designed: it is B-D7's fail-closed shadow. The fix must NOT weaken it:
- no default arming;
- no code path that sets `route_after`, which `route_structure_test.go` forbids;
- no applying of unarmed verdicts.

The code change is observability only.

## Proposed fix scope

**1. Code: make "not armed" visible in route_apply** (small, no behaviour change).
- [ ] `internal/capture/route.go`: add a count, separate from the inbox, of messages that match `routeInbox`'s predicates except that the account is unarmed (`sa.route_after IS NULL`, candidates exist, a live `unmatched` decision exists, the latest decision is `unmatched`, inbound, inside `cfg.Since`).
  - Expose it on `RouteStats` as a field (for example `Unarmed int`) or as `Unrouted["account_unarmed"]` with a new `RouteReasonUnarmed` constant.
  - Keep `routeInbox` itself unchanged. Folding unarmed rows into it would let them take up the 500-row `LIMIT` and would hand `DecideRoute` a zero `ArmedAt`.
  - It must never add to `Written`, or the stage loop re-runs at once (pipelined's `processed` semantics, `cmd/pipelined/route.go:77-79`).
  - Optionally split the count by "has a classify_route verdict". "Verdicts waiting on arming" is the number that would have flagged this bug.
- [ ] `cmd/pipelined/route.go:90-96`: log the new counter unconditionally, zeros included.
  - When it is > 0, add an Info sentence in the C-D2 style (`internal/promote/inquiry.go:275-276`), for example: "route_apply: N live-unmatched messages on candidate accounts with route_after NULL are in shadow and will not route (arm by hand — docs/runbooks/local-classifier.md "Routing lane")".
  - Spell the SQL `route_after IS NULL`, never `route_after = …`. The structure test's arm regex (`route_structure_test.go:187`) would flag `= null`.
- [ ] `docs/runbooks/pipeline.md:101` (the `route_apply` table row) and `:104`: name the new counter.

**2. Prod data change: arm account 1009.** The owner must approve this. It is the only thing that gets Sana's message on the board.

Pre-arming checks:
- the B-D7 HARD PRECONDITION: every capture binary is on the Part B image or later (tag ≥ 0.7.16). Check with `kubectl -n ops get cronjob,deploy -o wide`. DB evidence is consistent but is not proof: the last `shadow` capture row was 2026-09-13 01:31:51Z, before the candidates were seeded at 05:42, and there have been 0 shadow rows since.
- an owner decision on skipping the route eval (the label file was never produced). See the open questions.

```sql
BEGIN;
UPDATE source_accounts
   SET route_after = now()
 WHERE id = 1009
   AND provider = 'google'
   AND account_email = 'salvador@handsonconnect.org'
   AND route_after IS NULL;
-- expect: UPDATE 1
COMMIT;
```

Use `now()`, not a backdated instant (see "Backfill" for why and what follows).

**3. Runbook / tracking.**
- [ ] Record in IK (done, see "Landmine matched") and in the SWT-40 Part B go-live notes that step 5 (V5 reads → eval → arm → V6.5) was never completed. Track the remaining V6.5 backfill and the route eval as an open Jira item, so the next shadow-until-armed feature does not drop its arming step the same way.

**4. Regression tests** (test-author):
- [ ] `internal/capture/route_integration_test.go`, **unarmed is counted**:
  - an account with candidates and `route_after` NULL, plus a live-unmatched message with an ok classify_route verdict;
  - the pass gives `Written == 0`, unarmed count == 1, and no route row;
  - the same message on an armed control account gives unarmed 0.
  - Mutation: delete the count query (or its `IS NULL` clause) and the test must go red. Per "test the column, not the fixture", the test drives the real `route_after` column.
- [ ] `internal/capture/route_integration_test.go`, **the count never feeds Written**: a pass with only unarmed rows returns `Written == 0` and the stage returns processed 0.
- [ ] Convert `cmd/pipelined/sana_repro_integration_test.go` into three cases:
  - (a) NULL `route_after`: route_apply reports the unarmed count as 1 and nothing else happens (pins the visibility);
  - (b) `route_after` set before the verdict: route row → classify_inquiry verdict → one `holding` task;
  - (c) `route_after = now()` after the shadow verdict. This is the real prod path. The route stage re-classifies the message, which needs a route-shaped fake reply beside the inquiry one. Then route_apply routes it by step `model` and the chain reaches a holding task.
- [ ] Add one assertion to (b) or (c): an outbound in the thread BEFORE the ask does not gate it `answered`. replyfold's own tests may already cover `RepliedSinceCol` strictly-after, so check before duplicating.

## Backfill plan (d)

**Will inquiry and promote handle 291568 once routed? Yes, traced clause by clause.**

- **Route re-classification after arming at now().** Verdict 11630 (15:41) is before `route_after`. `DecideRoute` would return `verdict_before_arming` (`route.go:474-475`), confirmed by the second local run above. But once the account is armed, `inboxWhereRoute`'s `r.created_at >= sa.route_after` clause (`internal/classify/store.go:209-213`) puts the message back in the route classify inbox:
  - the `route` stage covers 168h and 25 per pass, and is woken by `captured` and the 5-min sweep, so it writes a fresh verdict on its next pass;
  - `route_classified` → `route_apply` routes it by `model` (grounded collaboratory) or `default` (collaboratory, O3). The earlier thread message 158690 has no rules attribution (only unmatched rows), so the `thread` step does not fire, and a neighbour's route row never counts.
  - No hand backfill is needed for 291568.
- **Inquiry classify.** It is woken by `routed` or the sweep. `inquiryStageSince` = 72h (`cmd/pipelined/inquiry.go:40`) on `sent_at`, and 291568 was sent 2026-09-15 15:33:15Z, so it stays eligible until **2026-09-18 ~15:33Z**. Collaboratory has `ai_inquiry` true. The latest decision would be the route row (`attributed`, project 4).
- **inquiry_promote gate** (`internal/promote/inquiry.go:132-174`):
  - rethreaded: stored thread = current thread;
  - kind: depends on qwen's `ask_kind`. The body asks explicit questions ("Could you provide the allowed values…"). This is unproven, see the open questions;
  - stale: under 72h;
  - pending: more than 1h past `sent_at`;
  - **answered: no.** `RepliedSinceCol` = `lo.last_outbound > t.sent_at` (`replyfold.go:159`). The thread's only outbound is 09-12 15:57, BEFORE 09-15 15:33, so this is false;
  - not_addressed: gmail is always addressed;
  - claude_task: no task on thread 159886.
  - The inbox also requires the verdict at or after `inquiry_promote_after` (2026-09-13 04:03), which a fresh verdict meets.
- **Result:** one `holding` task in collaboratory, action `review`.

**Deadline.** Arm before about **2026-09-18 14:30Z**. That leaves room for a route pass, a route_apply pass, an inquiry pass and the promote sweep, against the 72h fence at 15:33Z. After that, 291568 is routed but never promoted (C-D6; `--max-age` is refused on live passes). It would have to be hand-filed.

**Order on prod after the UPDATE:**
1. Wait at most ~15 min. Confirm a `mode='route'` row for 291568, a `classify_inquiry` extraction on raw item 291568's, and a `classify_promotions` row with a task: `/tasks?project=collaboratory&status=holding`.
2. Only THEN run the V6.5 one-shot backfill: `classify run --lane route --since 720h`. It holds the shared GPU lock `0x5157_0022` for its whole run (~62 messages × ~10 s). While it runs, the inquiry stage waits (`docs/runbooks/pipeline.md:108`). Running it first would delay Sana's inquiry verdict by that much.
   - The backfill covers the 8 verdicted messages older than 168h and the 54 unverdicted live-unmatched messages inside 720h. Those would otherwise sit as `pending_verdict` or `verdict_before_arming`, because of the 168h-vs-720h window mismatch recorded in SPEC Future work, `inquiry-promote_SPEC.md:1308-1312`.

**Other stranded client mail.** All of it is older than 72h, so by design it is routed or attributed but never promoted. SPEC line 1239: "Historical asks older than 72h never become tasks. Salvador hand-files the Rochester thread if it is still open."
- Thread 109296, a courses-endpoint issue (two other contacts at the same university client): 142095, 142118, 142222 and 158584, 09-08 to 09-10.
- Thread 159886: 158690 (the same contact, 09-10). Her 09-12 thread was answered by the 201924 outbound anyway.

There is no safe automatic backfill onto the board for these: widening the fence is refused on live passes, on purpose. The safe path is to read `classify promote --lane inquiry --dry-run --max-age 720h` after the route backfill (dry-run only, writes nothing), then hand-file whatever is still open.

## Affected count (e)

Prod, read-only, 2026-09-15 18:21Z. Criteria: inbound, has a live `unmatched` decision, latest decision `unmatched`, no `mode='route'` row, on a candidate account. Account 1009 is the only one.

| | count | sent within 72h | within 168h | within 720h |
|---|---|---|---|---|
| **with a classify_route verdict, never routed** | **21** | 6 | 13 | 21 |
| no verdict yet (outside the route stage's 168h) | 69 | 0 | 0 | 54 |

Of the 21, 10 are grounded and 11 ungrounded or with no choice:
- **grounded collaboratory (6):** 142095, 142118, 142222, 158584, 158690 and 291568. All are human mail from one university client.
- **grounded reengine (4):** 158362, 262278 and 269152 ("[CircleCI] … treetopllc / gonoble"), and 271184 (New Relic).
- **ungrounded or no choice (11):** these would fall to the collaboratory default. They are GitHub advisories, Dependabot, New Relic, Atlassian marketing, a wage-advance ad, and 149997 (a reengine choice, ungrounded).

Of the 6 within 72h, only **291568** is a human client ask. 241621 and 294300 would default into collaboratory, and the inquiry model should drop them (not needs_reply). 262278, 269152 and 271184 would attribute to reengine, which has `ai_inquiry` false: attribution only, no task.

## Out of scope for this fix

- Board ordering, "emails or slacks should be high priority": that is ticket `board-incoming-first`.
- The 168h route-stage window vs the 720h route_apply window: SPEC Future work, already recorded.
- GPU-lock starvation of `inquiry` during a route backlog drain: SPEC Future work.
- A capture rule for this client's university domain (→ collaboratory). It would bypass routing for this client entirely, but it is a rules-tier decision for the owner, not this bug's fix.
- Producing `docs/evals/route-from-rules.jsonl` and running the route eval, if the owner wants the gate honoured before arming.

## Open questions

1. **Arm without the eval?** B-D7 makes the rules-tier eval the go-live gate, and it was never run: there is no label file. Arming now skips it. The shadow verdicts already show one kind of misroute the eval exists to catch: the model routes "[CircleCI] Workflow failed: treetopllc / gonoble" mail to **reengine** as grounded (158362, 262278, 269152), while capture rule 7 says "gonoble is Collaboratory API". A route is "one per message, forever" (`capture_decisions_route_uniq`). These would be wrong attributions, although with no task, since reengine is not inquiry-armed. The owner decides: arm now, or run the eval first. Arming now is the only way to meet the 72h deadline for 291568. The alternative is to arm now and hand-file 291568 separately, never mind.
2. **Image precondition.** Is every capture workload (connector CronJobs, pipelined, any hand-run opsctl) on the Part B image or later? The DB evidence is consistent (0 shadow rows since 2026-09-13 01:31Z), but it is not proof. It needs `kubectl -n ops get cronjob,deploy -o wide`, which this diagnosis did not run: read-only psql was the only prod access in scope.
3. **qwen's verdict on 291568.** The canned fake verdict passes the gate. Whether qwen3:8b returns `needs_reply: true` with an allowed `ask_kind` on the real body was not tested: no live model was in scope. If it answers `fyi` or `needs_reply: false`, the message is correctly routed and classified but gated `kind` or never in the promote inbox, and must be hand-filed.
4. **Where the counter lives.** A separate `Unarmed int` field or an `Unrouted` map key: either is fine. The map key reuses the log loop's shape, but "unrouted" also means "was decided", which an unarmed message never is. This is the implementer's call; the tests above pin the behaviour, not the name.

## Risk assessment

- **The code change is additive and read-only.** It adds one more COUNT query under the capture lock per pass; the prod candidate set is 90 messages, so the cost is negligible. The only risk is feeding the count into `Written`, which would hot-loop the stage. The second test pins that.
- **Arming prod** changes attribution for up to ~75 in-window messages on 1009 (21 verdicted + 54 unverdicted inside 720h) as they get fresh verdicts or the backfill.
  - The `thread` step applies immediately, with no verdict needed, to any in-window message whose thread has exactly one rules-attributed candidate project.
  - Ungrounded junk falls to the collaboratory default: O3 by design.
  - Every route row takes the message out of the residue and triage inboxes (B6).
  - Routes are permanent per message.
  - Anything younger than 72h that the inquiry model flags lands in Holding (review), never Ready (O7), so a false positive costs one dismissal.
- **Disarming** (`route_after` back to NULL by hand) stops new routes; rows already written stay.
- **Shared code:** `routeInbox` and `DecideRoute` are untouched by the proposed change. The inquiry and promote inboxes already follow route rows (B6, `route_integration_test.go` fixtures).

## Landmine matched

It partly matches a known landmine: IK "The routing tier", "Arming is `source_accounts.route_after`, set by hand". The **silence** is NEW: route_apply's log cannot tell "unarmed with verdicts waiting" from "nothing to do". I added a landmine entry to `.claude/INSTITUTIONAL_KNOWLEDGE.md` under "The routing tier (SWT-40 Part B, inquiry-promote)". It says a shadow-until-armed gate must be counted by its stage, and that Part B's go-live (eval → arm → V6.5) was never completed as of 2026-09-15.
