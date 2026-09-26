# Handoff to the kube session: the task page carries the board's verbs (SWT-95)

Salvador, 2026-09-26: "task views like /tasks/707 should have the same controls for dismissing or marking the task
done as the board". `/tasks/{id}` gets Done, Dismiss, Requeue and Attach, posting to the board's existing endpoints.

Image: `192.168.50.20:5000/switchboard:0.7.59`
(`sha256:cf6cadbebd0a36a7807a41819d8103f5b782b43174683ca5fadc8a20e0afc074`), built from `main` at `4020cf9`.
It includes everything in 0.7.58.

## 0. Already done

Nothing. There is no migration of ours. 0047 (the job-agent session's) is already on prod.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Only `deployment/dashboard` changes behaviour. Keep the pins, and do the SWT-76 check before replacing the watcher pod.
No env var, port or probe change.

## 2. Check

- `GET /tasks/<an open human task>` (logged in) shows an actions bar with DONE, DISMISS and ATTACH.
- Leave the click-through to the switchboard session.

## 3. Rollback

Roll all 13 back to 0.7.58.
