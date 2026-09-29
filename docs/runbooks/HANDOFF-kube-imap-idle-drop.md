# Handoff to the kube session: reopen IMAP IDLE when the server ends it (swb 556, SWT-81)

When Gmail closed an IDLE connection, the watcher sat on the dead session for up to 25 minutes, so INBOX mail waited
for the 10-minute reconcile. Grady's mail of 29 Sep arrived 8 minutes late this way. Now the watcher reopens at once,
after one catch-up pass.

Image: `192.168.50.20:5000/switchboard:0.7.62`
(`sha256:9063cd002c2ef3ece5559518b8b5eba5e7ff35edfc1a80bb6ccb2332ef6b1a64`), built from `main` at `c55b50c`.
It includes everything in 0.7.61.

## 0. Already done

Nothing. There is no migration.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Behaviour changes only in the google connector: `deployment/connector-google-watch` and the google CronJob. Keep the
pins, and do the SWT-76 check before replacing the watcher pod. No env var, port or probe change.

## 2. Check

- `kubectl -n ops logs deploy/connector-google-watch --since=10m` shows `watch: idle <acct> open` for all 4 accounts.
- Leave the drop verification to the switchboard session: it looks for
  `closed without news` followed within about 5 s by `open` for the same account.

## 3. Rollback

Roll all 13 back to 0.7.61.
