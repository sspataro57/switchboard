# Handoff to the kube session — task-detail-source-message (SWT-65)

Clicking a task on the board showed the promoter's extract card and nothing else: "there is just
not enough info there". `/tasks/{id}` now renders the message the task came from — in full,
verbatim — with the rest of its thread folded into `<details>` and, for gmail, an attachment
manifest. Spec: `docs/tickets/task-detail-source-message_SPEC.md`.

Image: `192.168.50.20:5000/switchboard:0.7.28`
(`sha256:44d04cd9c58254580bf1132b2bf3c1a74236eeccef2f5f018b2c94adefb09010`), built from `main`
at `0e74352`.

**This image also carries 0.7.27's fix (SWT-61, gmail reply subjects), which has not been rolled
yet.** If you roll 0.7.28 you get both; 0.7.27 can be skipped. Its handoff
(`HANDOFF-kube-gmail-reply-subject.md`) still describes what to check after the roll.

## 1. No migration, no env var, no manifest change beyond the tag

Nothing in `migrations/` changed (newest is still 0036). Every column this page reads has existed
since 0021. No route is added, no port, no config. A pre-change binary on the same schema is
equally fine, which is what makes the rollback in §4 free.

The page performs **no write**. That is asserted, not just claimed: the integration suite traces
the statements a render issues and fails if any of them is an insert, update or delete.

## 2. Roll the tag

Only `deployment/dashboard` changes behaviour in the cluster. Roll the same tag to all 11
workloads in one apply as usual, keeping the pins (classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`).

Nothing else in the image changed for the other workloads: the three constants that moved in
`internal/tools` (`MailThreadMaxMessages`, `MailThreadBodyCap`, `LatestInboundOrder`) are the same
numbers under exported names, and `mail_read_thread` and the gmail reply route behave identically.

## 3. Post-roll check

`kubectl -n ops port-forward svc/dashboard 8085:80`, then on the tablet:

- **`/tasks/225`** — must now name the merchant and the card, not just the extract.
- **`/tasks/220`** — must show BOTH of Mike's questions (the card carried only one).
- **Any hand-made task** — must look exactly as it did before: no heading, no empty block. This is
  the majority case (33 of 47 open tasks resolve to no message) and it is pinned by a byte
  comparison against a golden captured from the pre-change binary.

On a mail-derived page, check: the tracking URL is plain text and NOT tappable, the thread
`<details>` are closed, and the attachment table lists names, types and sizes only — the page
serves no attachment bytes and adds no download route. Reading an attachment is still
`mail_read_attachment` over MCP.

## 4. Rollback

Roll `deployment/dashboard` back to `0.7.27` (or `0.7.26` to also drop SWT-61). Nothing is written
and no schema changed, so rollback is instant and lossless. URLs are unchanged and no data needs
repair.

## 5. Two things to know, neither blocking

**Privacy — read this before adding an Ingress.** The SWT-21 locality gate that governs the MCP
mail tools is deliberately NOT applied to this page: it is a boundary on where text may *travel*
(to a hosted model), not a rule about who may read, and applying it here would blank out `personal`
and every `local_only` project — precisely the tasks that prompted the request. What keeps anyone
else out is **network reach**: the dashboard has no Ingress and is reached by port-forward. Do not
read `auth.Require` as the barrier — with `OIDC_ISSUER` unset, `/dev/login?user=<anything>` mints a
session. That is pre-existing and true with or without this page, but this page now renders full
client mail, so anyone exposing the service must configure OIDC first.

**Cost on gmail-sourced pages.** Rendering one decodes and MIME-walks the whole stored message to
list its attachments — the same work `mail_list_attachments` does, but on a page load rather than a
deliberate tool call. Fine today (largest stored body is 33,465 characters), and worth remembering
once `MAIL_MAX_MESSAGE_BYTES` goes to 25 MiB per `HANDOFF-kube-mail-refetch.md`: a 25 MiB capture
means a ~25 MiB decode per manual view. There is no auto-refresh on this page.
