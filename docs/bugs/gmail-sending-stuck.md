> Jira: SWT-71

# gmail-sending-stuck

swb task #462. Noticed by the switchboard session on 2026-09-21 while verifying SWT-69; Salvador:
"check delivery #45 stuck in sending", then "fix it now".

## Observed (ops db, 2026-09-21)

- `deliveries.id = 45`: gmail, task 382 (closed), `status = 'sending'` since 2026-09-18 19:47Z,
  `sent_external_id` set, `send_attempted_at` set, `sent_at` NULL, **`confirmed_at` set at 19:50Z**.
- The message's own copy is in `normalized_messages` (outbound, same Message-ID) and a
  `delivery_confirmed` task event exists: the email left.
- `audit_events`: `approve_delivery ok`, `send_delivery started` at 19:47:36 with no completion, then
  a second `send_delivery` four seconds later refused "already carries sent_external_id; never
  resend (invariant 4)". The dashboard was being rolled that afternoon; the first call's process
  died between the network call and the finalize.
- No `delivery_sent` event, so R8 never ran for it.

## Cause

Two correct decisions with a gap between them. `sendDelivery` reserves the Message-ID and commits
`sending` before the network call (invariant 4), so a process death after the call leaves
`sending`. The gmail sink's `confirmDelivery` stamps `confirmed_at` and deliberately promotes
nothing ("Flipping 'sending' to 'sent' here would emit no delivery_sent event, so the
orchestrator's R8 never fires… the lifecycle transition belongs to the path that owns it").
Nothing owned that transition afterwards: `mark_delivery_sent` is the assisted tier's verb
(upwork/slack only) and `send_delivery` refused by invariant 4.

## Fix

`finishConfirmedSend` (internal/tools/delivery.go), called first in `sendDelivery`'s transaction.
A gmail row that is `sending`, carries a `sent_external_id`, is confirmed AND whose composed
message — by that Message-ID — is in the ingested mailbox becomes `sent`, with `sent_at` from the
confirmation. No transport call. An unconfirmed `sending` row still gets the invariant-4 refusal:
nothing proves it left, and it is never sent twice. The dashboard shows "Finish: it was sent" on a
row that meets the same proof (it posts to the same send route).

Decisions taken in review (adversarial pass, 2026-09-21), each pinned by a test:

- **Only the strong proof finishes a row.** `confirmed_at` has two producers: the exact Message-ID
  match, and the sink's body-prefix belt, which matches any message of the same mailbox that opens
  with the same 120 characters. Finishing on the belt could record "sent" for a message that never
  left, terminally. So the finish also requires the own copy in `normalized_messages`.
- **Phase 2 never overwrites a finished row.** A finish can now land while the original send is
  still in flight. All three phase-2 writes are conditional on `status='sending'`, and the rejection
  branch clears the Message-ID only while the row is unconfirmed. Otherwise an in-flight failure
  would write `failed` over a delivery R8 had processed, or clear the id and re-open a resend.
- **On a task closed since, a log event is written instead of `delivery_sent`.** The finish runs
  ahead of the closed-task guard (closing the task after "reply sent" is exactly what happened
  here), but R8 would change nothing on a closed task and would still record its
  `delivery_lifecycle` dedup key, muting a later real delivery if the task were reopened — SWT-28's
  calendar trap. On an open task it emits `delivery_sent` with `recovered: true`, so R8 advances
  the work.
- **The proof is scoped to `channel='gmail'`** (second review): "one normalized row per Message-ID" is a
  gmail-only partial UNIQUE index, so without the predicate the proof rests on nothing the schema
  enforces — and the planner cannot use that index (34 ms seq scan vs 0.06 ms, measured on production).
- **A definite rejection on a still-`sending` row is always recorded**, even if the body-prefix belt
  stamped `confirmed_at` mid-send; only the Message-ID is kept in that case. gmail has no reconciler,
  so a skipped write would have been a silent wedge.
- A recovered row's `sent_at` is the ingested copy's own instant (the true send time); the
  `delivery_sent` payload carries the APPROVED Cc, since the died send's record of what went on the
  wire is lost with it.
- **The event is written inside the transaction.** A finished row without its event could never be
  finished again.

Tests: `internal/tools/delivery_sending_recover_integration_test.go` (the proof ladder, the closed
task, the in-flight race, each proven under mutation) and
`internal/dashboard/deliveries_finish_integration_test.go` (the button's two column-fed conditions).

Policy is inherited: `send_delivery` is human-only and freeze-gated, so a stuck row cannot be
finished while sending is frozen. Safe direction; noted.

This bug went straight to a fix on the owner's "fix it now"; there is no separate REPRO or
DIAGNOSIS file — this file carries both.

Not done here: an automatic finish when the confirmation arrives. The orchestrator could execute
`send_delivery` on a `delivery_confirmed` event for a `sending` row; that is a rule change with a
policy question (who may trigger a send verb unattended) and is left as future work. Until then a
stuck row needs one click.
