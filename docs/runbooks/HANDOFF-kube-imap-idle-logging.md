# Handoff to the kube session — IMAP IDLE session logging (SWT-81)

Salvador, 2026-09-23: "log only, schedule a review in 1 week". Some INBOX arrivals get no IDLE wake and wait
for the 10-minute reconcile. This image only adds log lines so the cause can be found; no behaviour change.
Bug docs: `docs/bugs/imap-idle-missed-arrivals*`. Review on/after 2026-09-30 (swb #561).

Image: `192.168.50.20:5000/switchboard:0.7.50`
(`sha256:ea540efd8506569b08eee4b576e08dbfa5ba53bb5eef295eeaafafa2ea963985`), built from `main` at `8981a48`.
It includes everything in 0.7.49.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Keep the pins, and the usual SWT-76 check (bridge `send_queue.waiting == 0`) before the watcher pod. The only
workload whose behaviour this touches is `connector-google-watch` (log lines).

## 3. Check

`kubectl -n ops logs deploy/connector-google-watch` shows `watch: idle <account> open` for each mail account
within a minute of start, then `refresh after …` about every 25 minutes or `fired after …` on new mail.
**Keep this pod's logs available for the week** (Loki retention ≥ 7 days), because the review reads them.

## 4. Rollback

Roll all 13 back to 0.7.49 together.
