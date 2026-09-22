# Handoff to the kube session — Slack sends queue behind a busy browser (SWT-76)

Salvador's ask: "with the bridge sweep more frequent it should accept outgoing messages and q them to
send in between sweeps not just 400s". An approved Slack reply that meets a running rotation export on
the mini is now ACCEPTED by the bridge (HTTP 202 + job id) and clicked in the next gap, instead of
refused into `failed` for a second approval. Spec: `docs/tickets/slack-send-queue_SPEC.md`; runbook:
`docs/runbooks/slack-web-connector.md`, "What happens when the browser is busy".

Image tag and digest are in the message that accompanies this file (built from `main` after the merge).

## 0. Already done outside the cluster

- The leaf on the Mac mini is live (slackconnector main `0d078c6`, 2026-09-22 20:02Z). It answers 202
  only to a request carrying `max_queue_ms`, which the current image (0.7.43) never sends — nothing
  changed for prod yet.

## 1. Migration FIRST — additive, safe in either order but apply before the roll

`migrations/0042_slack_send_queue.sql`: two NULLABLE columns on `deliveries` (`send_queued_at`,
`send_queue_job_id`), no default, no backfill, no index, no status change. The switchboard session
applies it from here and confirms in the message. The new dashboard selects the columns on every
`/deliveries` render and the new send path writes them on a 202, so the image must not precede it; an
old image never touches them.

## 2. Roll ONE tag to every workload, in one apply

No env var and no manifest change. Behaviour changes only in the dashboard's send path
(`send_delivery` for `slack_reply` now sends `max_queue_ms=600000` and records a 202) and in the
connectors' Slack confirmation path (a `sending` row confirmed by the export now emits `delivery_sent`,
so its work task advances — the D7 fix). Keep the pins. Check no Send is in flight before replacing the
dashboard pod.

Optional, later, and Salvador's call (SPEC D3): if rotation exports on the mini regularly run longer
than 10 minutes, `SLACK_ROTATION_INTERVAL=10m` + `SLACK_WEB_EXPORT_BUDGET_MS=300000` on
`deployment/connector-slackweb-watch` keep every rotation inside the acceptance window. Not part of
this roll.

## 3. Post-roll check

- `/deliveries` renders (both new columns NULL on every existing row).
- The next Slack reply approved while the watcher's rotation is running: the flash reads
  `delivery N queued on the bridge (job …)`, the row shows "queued on the bridge" with only "It's in
  Slack" offered for 15 minutes, the message appears in Slack when the rotation ends, and the next
  rotation export promotes the row to `sent` and the task to `delivered`.

## 4. Rollback

Mildest first: `SLACK_SEND_QUEUE_MAX_WAIT=off` on the dashboard (and opsctl users) — `max_queue_ms` is
omitted and the leaf 503s exactly as before, no roll. Then the image back to 0.7.43: 0042 stays
(forward-only, additive, nullable; the old binary never selects the columns), queued rows are ordinary
`sending` rows the old binary handles through the lease and the reconciler.
