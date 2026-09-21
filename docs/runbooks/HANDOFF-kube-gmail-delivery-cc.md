# Handoff to the kube session — gmail-delivery-cc (SWT-69)

An email reply drafted through switchboard can now carry Cc recipients. A session (or the drafts
worker on a redo) passes `cc` to `draft_delivery`; the dashboard shows it beside From and To and
lets Salvador change or clear it before he approves; the sent message carries a `Cc:` header.
Switchboard never adds a Cc by itself. Spec: `docs/tickets/gmail-delivery-cc_SPEC.md`.

## 1. Migration `0038_delivery_cc.sql` FIRST

`migrations/0038_delivery_cc.sql` adds `deliveries.cc TEXT[] NOT NULL DEFAULT '{}'` and two
CHECKs (`deliveries_cc_gmail_check`, `deliveries_cc_shape_check`). Additive, idempotent, no
backfill, no rewrite. **Merging is not applying, and this one must land before the roll:** the new
binaries select `deliveries.cc` on the delivery paths (the dashboard's deliveries page, approve,
reject, send, the drafts worker's redo read), so a new image on a pre-0038 schema fails those
pages and sends with "column cc does not exist". The old images are unaffected by the new column.

The switchboard session applies 0038 to production itself before handing this over, and says so
in the message that accompanies this file; verify with

```sql
SELECT version FROM schema_migrations ORDER BY 1 DESC LIMIT 1;   -- 0038
```

## 2. Then the image

Image tag and digest are in the message that accompanies this file (built from `main` after the
merge). No env var, port, volume or secret changes. Roll the same tag to all 11 workloads as
usual, keeping the pins (classify-promote `--lane personal`; pipelined `PIPELINE_STAGES=…`;
`MS_OAUTH_CLIENT_ID` on connector-google). Behaviour changes only in `deployment/dashboard`
(the delivery card and its edit form) and wherever `send_delivery` runs.

A dashboard deliveries page that was open before the roll refuses its Approve once with "changed
since it was shown to you; reload and review it again": the approval hash now covers the Cc.
Reloading the page fixes it.

## 3. Then the user-scope MCP binary, on both machines (switchboard session does this)

Sessions in other repos draft through the user-scope binary, which carries the tool schema, so
`cc` exists there only after `go install ./cmd/ops-mcp-user` on `main` — on this workstation and
on 192.168.50.30 — and only in sessions opened after it. Not a cluster step; listed so the order
is in one place.

## 4. Post-roll check

- `/deliveries` renders; a gmail row with a Cc shows `Cc: …` under `To:`; a drafted gmail row's
  edit form has a `cc` box.
- No send is needed to verify: `SELECT id, cc FROM deliveries ORDER BY id DESC LIMIT 5;` runs.

## 5. Rollback

Roll the workloads back to the previous tag. Leave 0038 in place: the old binaries never read the
column, and a row that already carries a Cc would simply be sent without it by an old binary —
so if a Cc'd draft is waiting for approval, roll forward again rather than sending it from the
old image.
