# Handoff to the kube session — the task board is pushed, not polled (SWT-89)

Salvador, 2026-09-25: "I don't like the task board polling it's frustrating. let's make it streaming push the
changes to it". With refresh=on (the kiosk and the installed app), /tasks now holds a Server-Sent Events stream at
`GET /tasks/stream` and swaps changed regions in place within about 2 s of a change, with no page reload. SPEC:
`docs/tickets/board-streaming_SPEC.md`.

Image: `192.168.50.20:5000/switchboard:0.7.55`
(`sha256:97f58dcbc7c1e8bcffe1f8e9153172d287d271cc586592ea61d2febea86fb941`), built from `main` at `9112185`.
It includes everything in 0.7.54.

## 0. Already done

**Migration 0044 is applied to production** (switchboard session, 2026-09-25 17:26Z): the `board_changed_notify()`
trigger function and two triggers on each of `tasks`, `task_dismissals`, `classify_promotions`, `external_refs`.
Harmless under the running 0.7.54: nobody LISTENs yet.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Only `deployment/dashboard` changes behaviour; the others carry the tag for consistency. Keep the pins, and do the
SWT-76 check (bridge `send_queue.waiting == 0`) before replacing the watcher pod. No env var, no port, no probe
change. The dashboard now holds ONE extra Postgres connection (application_name `switchboard-board-live`). Do not
add an http `WriteTimeout` anywhere in front of it.

## 2. Check

- `kubectl -n ops logs deploy/dashboard` shows no `board live: not listening` loop.
- On pg-main: `SELECT count(*) FROM pg_stat_activity WHERE application_name='switchboard-board-live'` is 1.
- Through BOTH hosts, with a dashboard session cookie: `curl -N -b <cookie> https://<host>/tasks/stream` prints
  `retry: 5000` at once and a `: ping` within 20 s. **If the pings arrive in a lump or only when curl exits**, add
  `nginx.ingress.kubernetes.io/proxy-buffering: "off"` to both dashboard Ingresses (a contingency, not expected:
  the handler sends `X-Accel-Buffering: no`).
- The tablet's `/kiosk`: a change (e.g. an opsctl `task_set_priority`) shows within about 2 s, with no reload and
  the shell still full-screen.

## 3. Rollback

Roll all 13 back to 0.7.54. 0044 can stay: its triggers only NOTIFY, and nothing listens.
