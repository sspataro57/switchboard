# Handoff to the kube session — gmail-sending-stuck (SWT-71)

A gmail delivery whose send process died after the network call stayed `sending` forever, even
after the message's own copy came back through the mailbox sync (delivery 45, stuck since
2026-09-18, when the dashboard was rolled mid-send). `send_delivery` can now FINISH such a row —
no email is sent — and the dashboard shows a "Finish: it was sent" button on it. Bug record:
`docs/bugs/gmail-sending-stuck.md`.

Image: **`192.168.50.20:5000/switchboard:0.7.37`**
(`sha256:c5a52510e87bdefd9127d707f94f09d833d20db13cccf11444f8306c8186fd33`), built from `main`
at `aad37c0`. It supersedes 0.7.36 and carries nothing else new.

## 1. No migration, no env var, no manifest change beyond the tag

Newest migration is still 0038 (applied).

## 2. Roll the tag

Behaviour changes only in **`deployment/dashboard`** (it runs `send_delivery` and renders the
button). Roll the same tag to all 11 workloads as usual, keeping the pins (classify-promote
`--lane personal`; pipelined `PIPELINE_STAGES=…`; `MS_OAUTH_CLIENT_ID` on connector-google).

**Please do not roll the dashboard while a Send is in progress.** That is how delivery 45 got stuck.
It is recoverable now, but only when the sent copy comes back through the sync.

## 3. Post-roll check

`/deliveries?status=sending` renders. The switchboard session finishes delivery 45 itself through
the dashboard route and verifies it; nothing for the kube session to do there.

## 4. Rollback

Set the tag back to 0.7.36. A row finished under 0.7.37 stays `sent`.
