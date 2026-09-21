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

`finishConfirmedSend` (internal/tools/delivery.go), called first in `sendDelivery`'s transaction:
a gmail row that is `sending`, carries a `sent_external_id` AND is confirmed becomes `sent` with
`sent_at` from the confirmation; the caller emits `delivery_sent` with `recovered: true`, so R8
fires. No transport call. It runs before the closed-task guard, because closing the task after
"reply sent" is exactly what happened here. An unconfirmed `sending` row still gets the
invariant-4 refusal: nothing proves it left, and it is never sent twice. The dashboard shows
"Finish: it was sent" on such a row (it posts to the same send route).

Regression test: `internal/tools/delivery_sending_recover_integration_test.go`.

Not done here: an automatic finish when the confirmation arrives. The orchestrator could execute
`send_delivery` on a `delivery_confirmed` event for a `sending` row; that is a rule change with a
policy question (who may trigger a send verb unattended) and is left as future work. Until then a
stuck row needs one click.
