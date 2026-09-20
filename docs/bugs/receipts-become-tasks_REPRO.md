> Jira: SWT-68 · swb task #421

# Reproduction — receipts-become-tasks

> **Superseded (2026-09-19).** The failing test named below, `internal/promote/receipts_repro_test.go`, was removed when the fix shipped: the diagnosis put the fault in the VERDICT, and a real bill (task 397) reaches `promote.Decide` with the same `kind`, so "payment_due creates no task" cannot be asserted at that layer. The guards that replaced it are `internal/classify/prompt_money_test.go`, `internal/promote/receipts_regression_test.go` and the 20 rows added to `docs/evals/personal-actionability.jsonl`.


## Status

**Confirmed.** The failure reproduces deterministically, offline, with no model call:
`go test ./internal/promote -run TestReceiptsBecomeTasks` is RED, and it is red for the
reported reason — a stored verdict for a PayPal "you paid / you authorized" receipt turns
into `action=task, status=ready`, the exact row that appeared on the board.

---

## Trigger

1. Inbound mail is ingested for project `personal` (`projects.id=6`, `ai_classify=true`,
   `classify_promote_after = 2026-09-09 13:03:59Z`). 17 of the 19 rows came from the MSN
   mailbox `sspataro57@msn.com` (`source_accounts.id=14418`) filed there by capture rule 76;
   **2 (tasks 214, 358) came from `sspataro@gmail.com` and predate the MSN onboarding** — the
   trigger is not MSN-specific.
2. The personal classify lane (`ai_runs.worker_type='classify'`, qwen3:8b) records a verdict in
   `ai_extractions.fields`. For all 19 rows the verdict is `actionable: true` with
   `kind: payment_due` (18) or `kind: deadline` (1).
3. The promoter (`internal/promote`, actor `promote:classify`) reads the stored verdict and calls
   `promote.Decide(verdict, existingTask)`. With no open task on the thread and one of those two
   kinds, the decision is `{Action:"task", Status:"ready"}`.
4. A `ready` task titled with the dollar amount lands in the board's incoming section.

Minimal offline trigger: `promote.Decide(promote.Verdict{Kind: "payment_due"}, nil)`.

---

## Observed behavior

19 `personal` tasks whose title starts with `$`, all created by the promoter (each has a
`classify_promotions` row with `action='task'`, `reason` NULL), all now `closed`.

### The reproduction set (read read-only from the ops db, 2026-09-19)

Verdict columns are `ai_extractions.fields`; every row has `actionable=true` and
`classify_promotions.action='task'`, `reason=NULL`, so those three are not repeated per row.

| task | title | mailbox | sender | subject | sent | verdict kind | nm id | ext id | run id | closed by |
|---|---|---|---|---|---|---|---|---|---|---|
| 214 | `$10.00 due on May 05, 2026` | gmail | Mistral AI SAS \<no-reply@mistral.ai\> | Your Invoice from Mistral AI SAS #MSTRL-API-831431-001 | 2026-05-05 | payment_due | 17988 | 11929 | 12033 | dismissal 51 `not_actionable` (09-17) |
| 358 | `$25.00 on 9/19/26` | gmail | "Bank of America" \<onlinebanking@ealerts.bankofamerica.com\> | Reminder: Your recurring payment to [a family member] will occur tomorrow | 2026-09-18 | payment_due | 333709 | 12186 | 12290 | `task_close` "done on the board" |
| 398 | `$68.38 USD - Aug 25, 2026` | msn | PayPal \<service@paypal.com\> | eBay Commerce Inc.: $68.38 USD | 2026-08-25 | payment_due | 348955 | 12327 | 12431 | `task_close` "done on the board" |
| 399 | `$45.00 USD - Aug 26, 2026` | msn | PayPal \<service@paypal.com\> | Payment Escrow Inc.: $45.00 USD | 2026-08-26 | payment_due | 348963 | 12335 | 12439 | `task_close` "done on the board" |
| 400 | `$50.55 USD due today` | msn | PayPal \<service@paypal.com\> | See your new PayPal Pay in 4 plan | 2026-08-30 | payment_due | 348986 | 12358 | 12462 | dismissal 119 `not_actionable` |
| 401 | `$202.21 USD - Aug 30, 2026` | msn | PayPal \<service@paypal.com\> | eBay Commerce Inc.: $202.21 USD | 2026-08-30 | payment_due | 348987 | 12359 | 12463 | dismissal 120 `not_actionable` |
| 402 | `$50.55 USD on September 15, 2026` | msn | PayPal \<service@paypal.com\> | Your PayPal Pay in 4 payment went through | 2026-08-30 | **deadline** | 348988 | 12360 | 12464 | dismissal 121 `not_actionable` |
| 403 | `$9.99 USD - Sep 2, 2026` | msn | PayPal \<service@paypal.com\> | Microsoft Corporatio...: $9.99 USD | 2026-09-02 | payment_due | 349003 | 12375 | 12479 | dismissal 122 `not_actionable` |
| 405 | `$1.00 USD - Sep 6, 2026` | msn | PayPal \<service@paypal.com\> | Roku, Inc.: $1.00 USD | 2026-09-07 | payment_due | 349038 | 12410 | 12514 | dismissal 123 `not_actionable` |
| 406 | `$1.00 USD - Sep 6, 2026` | msn | PayPal \<service@paypal.com\> | Roku, Inc.: $1.00 USD | 2026-09-07 | payment_due | 349039 | 12411 | 12515 | dismissal 124 `not_actionable` |
| 407 | `$21.49 USD - Sep 6, 2026` | msn | PayPal \<service@paypal.com\> | Roku, Inc.: $21.49 USD | 2026-09-07 | payment_due | 349040 | 12412 | 12516 | dismissal 118 `not_actionable` |
| 408 | `$19.99 USD - Sep 7, 2026` | msn | PayPal \<service@paypal.com\> | Payment Escrow Inc.: $19.99 USD | 2026-09-08 | payment_due | 349048 | 12420 | 12524 | dismissal 126 `not_actionable` |
| 409 | `$45.00 USD - Sep 9, 2026` | msn | PayPal \<service@paypal.com\> | Payment Escrow Inc.: $45.00 USD | 2026-09-09 | payment_due | 349055 | 12427 | 12531 | `task_close` "done on the board" |
| 410 | `$24.99 USD - Sep 10, 2026` | msn | PayPal \<service@paypal.com\> | Valve Corp.: $24.99 USD | 2026-09-10 | payment_due | 349063 | 12435 | 12539 | `task_close` "done on the board" |
| 411 | `$29.99 USD - Sep 10, 2026` | msn | PayPal \<service@paypal.com\> | Valve Corp.: $29.99 USD | 2026-09-10 | payment_due | 349065 | 12437 | 12541 | `task_close` "done on the board" |
| 412 | `$50.55 USD on September 15, 2026` | msn | PayPal \<service@paypal.com\> | Your PayPal Pay in 4 payment is coming up | 2026-09-12 | payment_due | 349078 | 12450 | 12554 | dismissal 130 `not_actionable` |
| 413 | `$68.38 USD by September 15, 2026` | msn | PayPal \<service@paypal.com\> | Your refund from eBay Commerce Inc. is on the way | 2026-09-13 | payment_due | 349088 | 12460 | 12564 | dismissal 131 `not_actionable` |
| 414 | `$45.00 USD - Sep 15, 2026` | msn | PayPal \<service@paypal.com\> | Payment Escrow Inc.: $45.00 USD | 2026-09-15 | payment_due | 349101 | 12473 | 12577 | dismissal 132 `not_actionable` |
| 419 | `$60.96 USD` | msn | PayPal \<service@paypal.com\> | Cinemark USA: $60.96 USD | 2026-09-19 | payment_due | 351664 | 12509 | 12613 | dismissal 125 `not_actionable` |

Closure split: **13 dismissed** (`dashboard:salvo`, `not_actionable`, none reopened) and
**6 closed** with `task_close` / `status_changed {"reason":"done on the board"}` (358, 398, 399,
409, 410, 411) — i.e. the board's Done button, which leaves no dismissal row and therefore no
label for a precision readout.

*Correction to the report's count:* 14 `dashboard:salvo` + `not_actionable` dismissals exist on
2026-09-19, but **two of them are not `$…` tasks** (417 "Acceso a cuenta de Microsoft no
autorizado", 404 "Security alert for sspataro@gmail.com"). Of the 19, twelve were dismissed on
09-19 and one (214) on 09-17.

### Classification by eye

Body excerpts kept to the few words that separate a receipt from a bill that is due.

| class | count | tasks | what the body says |
|---|---|---|---|
| Pure receipt / charge confirmation (money already moved, nothing to do) | **14** | 398, 399, 401, 402, 403, 405, 406, 407, 408, 409, 410, 411, 414, 419 | "You paid $24.99 USD to Valve Corp. … Total $24.99 USD **Paid**"; "You **authorized** $60.96 USD to Cinemark USA … Transaction date Sep 19, 2026"; 402: "We received your Pay in 4 payment … **charged** to the Bank Account ending in x-#### on August 30, 2026" |
| Autopay / scheduled-payment notice (will be charged automatically) | **3** | 400, 412, 358 | 412: "Your next payment of $50.55 USD will be charged on September 15, 2026. **There's nothing you need to do.** We'll automatically charge your Bank Account…"; 400: Pay in 4 plan schedule, "$50.55 USD due today / …September 15 / …October 1 / …October 17"; 358 (BoA): "You **scheduled** a $25 payment to [a family member]. Please make sure there are sufficient funds in the account" |
| Refund notification (money coming in) | **1** | 413 | "Your refund is on the way! Available in your bank by September 15, 2026" |
| Invoice/statement document with no payment instruction | **1** | 214 | "Invoice from Mistral AI SAS $10.00 issued on May 05, 2026 … Download invoice for details" — no amount-due, no pay link, no date by which to pay |
| **Bill genuinely due, needing manual payment by Salvador** | **0** | — | none of the 19 |

**The open point in the report is answered: none of the 19 was a bill he had to pay by hand.**

- `$50.55 USD due today` (task 400) is the PayPal **Pay in 4** plan schedule. Its sibling task
  402 — same day, same plan — is PayPal confirming "We received your Pay in 4 payment … charged
  … on August 30, 2026", i.e. the "due today" instalment was auto-collected the same day. Task
  412 then states for the next instalment: "There's nothing you need to do."
- The only row where a reasonable person could argue is **358** (Bank of America: a recurring
  payment is scheduled; the one human act implied is keeping the balance topped up). It is also
  one of the six he closed with Done rather than dismissing. Worth a decision from the owner, not
  an assumption.
- **Contrast row, deliberately outside the set** (its title does not start with `$`): task **397**
  "Payment due", from `"Citi Alerts" <alerts@info6.citi.com>`, subject "Your payment due date is
  approaching", stored verdict `kind=payment_due`. That IS a genuine bill-due notice, and he
  dismissed it `handled_elsewhere` — a different reason code from the 13 `not_actionable` ones.
  The human's own labelling separates the two classes; the promotion did not.

---

## Expected behavior

A payment receipt, charge confirmation, autopay notice or refund notification that requires
nothing from Salvador does not become a task in `personal` — not `ready`, not on the board.
(Owner's words, 2026-09-19: "receipts are not actionable".) A genuine bill due for manual payment
— task 397's shape — should still produce one.

---

## Reproduction location

### 1. Failing Go test (primary artifact — offline, no model, no db)

`/home/salvo/projects/personal/switchboard/internal/promote/receipts_repro_test.go`

```
cd /home/salvo/projects/personal/switchboard
go test ./internal/promote -run TestReceiptsBecomeTasks -v
```

RED today. It feeds four production verdicts (tasks 419, 410, 402, 412 — extraction ids 12509,
12435, 12360, 12450) into `promote.Decide`, the promoter's pure decision function, with
`existing = nil` (every one of these mails was first on its thread), and asserts no task is
created. Current output, per case:

```
receipt became a task
  message PayPal <service@paypal.com> / Valve Corp.: $24.99 USD
  body:   You paid $24.99 USD to Valve Corp. ... Total $24.99 USD Paid
  stored verdict: kind="payment_due" actionable=true (ai_extractions 12435)
  Decide -> action="task" status="ready"
  want:      no task (production created tasks.id 410, dismissed not_actionable)
```

A second test, `TestReceiptsBecomeTasks_AllNineteenKinds`, pins the population rather than a
sample: both stored kinds present in the 19 (`payment_due` ×18, `deadline` ×1) return a ready
task, so the whole set reproduces, not just the four quoted.

The rest of `./internal/promote` stays green — only these two tests fail.

### 2. The data, re-derivable read-only

`/home/salvo/projects/personal/switchboard/docs/bugs/receipts-become-tasks_repro.sql`

```
cd /home/salvo/projects/personal/switchboard
psql "$OPS_DATABASE_URL" -f docs/bugs/receipts-become-tasks_repro.sql
```

Three SELECTs, no writes: the table above, the body excerpts, and the close events for the six
that were not dismissed.

### Not used, and why

No scratch database and no `classify promote --dry-run` run was needed: `promote.Decide` is an
exported pure function (`func Decide(v Verdict, existing *ExistingTask) Decision`), so the
promotion decision is reachable directly with the stored verdict as input. A dry-run against
production would print nothing for these rows anyway — `classify_promotions.normalized_message_id`
is uniquely claimed, so all 19 are already out of the promoter's inbox. No non-dry-run classify or
promote was executed anywhere; the production db was only ever read.

---

## Environment

- `git rev-parse HEAD` = `7f01a51baa3e8fce49267786cb73584cfbc57dc7` (branch `main`, clean apart
  from this bug's own files)
- go1.25.0 linux/amd64
- Production ops db read READ-ONLY via `$OPS_DATABASE_URL`; nothing written, no advisory lock taken
- Data prerequisites for the row states quoted: `projects.slug='personal'` → id 6,
  `ai_locality='local_only'`, `ai_classify=true`, `classify_promote_after='2026-09-09 13:03:59Z'`;
  `source_accounts` 14418 (`sspataro57@msn.com`) and 1003 (`sspataro@gmail.com`); capture rule 76
- The Go test needs no environment at all — no `DATABASE_URL`, no broker, no provider

---

## Notes

Observations only.

- **The model's own verdict calls these actionable.** Two receipts, quoted verbatim from
  `ai_extractions.fields` (link fields elided):
  - task 419, extraction 12509: `{"kind": "payment_due", "actionable": true, "title": "$60.96 USD",
    "reason": "The message indicates a payment of $60.96 USD was authorized to Cinemark USA on Sep 19,
    2026, and the recipient is expected to confirm or process the payment.", "sender":
    "\"service@paypal.com\" <service@paypal.com>", "subject": "Cinemark USA: $60.96 USD",
    "project_id": 6, "project_slug": "personal", "normalized_message_id": 351664}`
  - task 412, extraction 12450: `{"kind": "payment_due", "actionable": true, "title": "$50.55 USD on
    September 15, 2026", "reason": "The message states that a payment of $50.55 USD will be
    automatically charged on September 15, 2026, and there is nothing the recipient needs to do.",
    "subject": "Your PayPal Pay in 4 payment is coming up", "project_id": 6, "project_slug":
    "personal", "normalized_message_id": 349078}`

  (`link_url`, `link_text`, `link_index`, `link_index_rejected` and `link_candidates` elided — long
  Outlook safelink wrappers.)

  The second is notable without being interpreted here: the stored reason says in plain words that
  there is nothing to do, and the same row carries `actionable: "true"` and a whitelisted kind. So
  both halves — the verdict and the promotion rule — reproduce the symptom on their own inputs, and
  which one is "the" fault is the diagnoser's call, not established here.
- `actionable` is a JSON **boolean** `true` in every one of the 19 rows (`->>'actionable'` renders it
  as the text `true`, which is what the table above and the test's message show).
- `promote.Decide`'s documented contract says it "consumes only Kind"; nothing else in the verdict
  reaches the decision. Recorded as an interface fact, not as a cause.
- 18 of the 19 came from PayPal (`service@paypal.com`) on the same mailbox; the remaining one is
  Bank of America. The lane sees a high-volume, uniform sender family.
- Six of the 19 were closed with the board's Done button, so `task_dismissals` under-counts the
  false positives by a third. Any precision measurement built on dismissals alone will read 13/19.
- One duplicate pair exists inside the set: tasks 405 and 406 are two distinct Roku $1.00
  authorisations (different order ids), not a double-promotion — `classify_promotions` has one row
  per `normalized_message_id`, and there are two messages.

## Stopping point

Reproduction exists and fails as described. No cause investigated, no fix proposed —
`bug-diagnoser` next.
