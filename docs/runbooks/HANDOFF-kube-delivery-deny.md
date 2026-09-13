# Handoff to the kube session: SWT-43 delivery-deny

The switchboard session builds and pushes the image. The kube session owns the manifests in `kube/switchboard`. This file lists what changes and, above all, the order.

## Order: apply migration 0028 BEFORE rolling the dashboard or the drafts worker

`migrations/0028_delivery_rejection.sql` widens `deliveries_status_check` to admit `rejected` and adds two columns, `rejection_note` and `redraft_requested_at`. The new code reads both columns unconditionally:

- **A new dashboard on a database without 0028 returns 500 on `/deliveries`.** `listDeliveries` selects `d.rejection_note` and `d.redraft_requested_at`, and Postgres refuses the whole query ("column does not exist"). The page is the approve/send surface, so this blocks every delivery.
- **A new drafts worker on a database without 0028 fails in `DeliverTasks`.** Its queue query reads `redraft_requested_at` (the Redo unblock) and `rejection_note` (the redraft prompt). `cmd/drafts run` exits with `list deliver tasks: select deliver tasks: ...` and drafts nothing.

The other direction is safe. The migration is additive, so old binaries keep working on a migrated database:

- an old dashboard shows a `rejected` row's status with no buttons;
- an old drafts worker treats a rejected row as blocking, which is Deny's meaning;
- nothing old writes `rejected`.

So apply 0028 first, then roll the images in either order.

## Pre-flight on pg-main (merging is not applying)

```
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"   # expect 0027
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT conname FROM pg_constraint WHERE conrelid='deliveries'::regclass AND contype='c'"   # must list deliveries_status_check
psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT status, channel, count(*) FROM deliveries GROUP BY 1,2 ORDER BY 1,2"   # record which rows get the new buttons
```

Apply 0028 with a `migrate` Job on the new image, or run `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate` against prod. Then re-run the first query and expect `0028`.

Numbering note: SWT-40 Part D carries its own `0029_capture_ticket_gate.sql` on the `ticket-inquiry-promote` branch, which is not on main. Whichever branch merges second renumbers (SPEC criterion 3). Compare prod `schema_migrations` against `ls migrations/` before pushing the tag.

## Image and workloads

**Image:** `192.168.50.20:5000/switchboard:<tag>`. The tag is filled in when SWT-43 merges.

| workload | change |
|---|---|
| `deployment/dashboard` (`kube/switchboard/dashboard.yaml`) | image bump only. New Deny/Redo form on `/deliveries`, `POST /deliveries/{id}/reject`. |
| the drafts worker (`cmd/drafts run`), wherever it is scheduled | image bump only. It re-drafts after a Redo. |
| every other workload | image bump when convenient; nothing in them changed. |

There are no new env vars, no new workloads and no new ports.

## Smoke

1. `kubectl -n ops port-forward svc/dashboard 8085:80`, then open `/deliveries`. It renders with no 500, and a drafted row shows a note box with **Deny** and **Redo** next to Approve.
2. `/deliveries?status=rejected` renders (empty is fine).
3. Do not Deny a real client draft just to test. The first real Deny or Redo is Salvador's.
