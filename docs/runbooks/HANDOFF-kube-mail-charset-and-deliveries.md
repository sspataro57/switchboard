# Handoff to the kube session: mail charset decoding + deliveries layout (SWT-98, swb 768)

**Supersedes `HANDOFF-kube-deliveries-three-columns.md` (0.7.60), which was never applied: roll 0.7.61 instead.**

Two changes:
- **SWT-98:** the google connector decodes mail subjects and bodies by their declared charset. Outlook's
  windows-1252 em dash was stored as a control character, and big5's dash as "¡X".
- **swb 754/768:** /deliveries has every fact about a row in column 1, the draft in column 2 and the actions in
  column 3. Salvador: "the actual email is to the right and I need to scroll".

Image: `192.168.50.20:5000/switchboard:0.7.61`
(`sha256:b0361ea1b74ed3da2d4a1e333e96a742456fe05acb933b38ba6b1bfe77e5ec13`), built from `main` at `f9096d7`.
It includes everything in 0.7.59 and 0.7.60.

## 0. Already done

Nothing. There is no migration. The re-normalize of stored mail is run by the switchboard session, not by you.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Behaviour changes in `deployment/dashboard` (template) and in every workload that runs the google connector:
`deployment/connector-google-watch` and the google CronJobs. Keep the pins, and do the SWT-76 check before replacing the
watcher pod. No env var, port or probe change.

## 2. Check

- `GET /deliveries` (logged in) shows three column headers: DELIVERY, DRAFT, ACTIONS, and no horizontal scroll.
- `kubectl -n ops logs deploy/connector-google-watch --since=5m` shows normal passes, with no normalize error.
- Tell the switchboard session when the roll is done: it re-runs the mail re-normalize once, to pick up anything
  ingested by 0.7.59 in the meantime.

## 3. Rollback

Roll all 13 back to 0.7.59. Rows already re-normalized keep their corrected text; 0.7.59 only affects new mail.
