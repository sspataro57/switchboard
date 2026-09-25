# Handoff to the kube session: every page wears the board's look, and the task page has a Back link (SWT-92)

Salvador, 2026-09-25: "this screen needs a back link https://switchboard.sspataro.com/tasks/680 and the looks and
feel on all the screens should match the board style and branding". The seven secondary pages (task, deliveries,
briefs, plans, plan, sources, funnel) now load `/static/swb-1.css`: the board's palette, fonts, chrome bar and sign.
`/tasks/{id}` has a Back button that returns to the board view it was opened from. The board and `/kiosk` are
unchanged.

Image: `192.168.50.20:5000/switchboard:0.7.57`
(`sha256:ca41ee4476f200ccfccb1bd8675b88ef42e2b3eba91a7298ce02067246784090`), built from `main` at `cf82e4b`.
It includes everything in 0.7.56 (SWT-91).

## 0. Already done

Nothing. There is no migration and no config change.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Only `deployment/dashboard` changes behaviour; the others carry the tag for consistency. Keep the pins, and do the
SWT-76 check (bridge `send_queue.waiting == 0`) before replacing the watcher pod. No env var, port or probe change.
`/static/` stays open (no auth), as before.

## 2. Check

- `curl -sI https://<host>/static/swb-1.css` returns 200 with `Cache-Control: public, max-age=31536000, immutable`,
  through BOTH hosts.
- In the browser: open a task from the board with a project filter. The task page is dark with the yellow sign,
  and ← BOARD returns to the same filtered board.
- `/deliveries`, `/funnel`, `/sources`, `/plans` and `/briefs` render dark, with the same sign.

## 3. Rollback

Roll all 13 back to 0.7.56.
