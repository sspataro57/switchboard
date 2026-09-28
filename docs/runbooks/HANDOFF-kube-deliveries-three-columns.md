# Handoff to the kube session: the deliveries page in three columns (SWT-96)

Salvador, 2026-09-28: on /deliveries "the actual email is to the right and I need to scroll". The page now has three
columns: Task (with channel, status and when), Goes to, and Draft (with its commands under it).

Image: `192.168.50.20:5000/switchboard:0.7.60`
(`sha256:99c4e3af10fd5cdabfb6bf9639d7ea696c9a70b7ab70dc785f8f0c654dc4dddd`), built from `main` at `b221d73`.
It includes everything in 0.7.59.

## 0. Already done

Nothing. There is no migration.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Only `deployment/dashboard` changes behaviour (one template). Keep the pins, and do the SWT-76 check before replacing
the watcher pod. No env var, port or probe change.

## 2. Check

- `GET /deliveries` (logged in) shows three column headers: TASK, GOES TO, DRAFT, and no horizontal scroll.
- Leave the click-through to the switchboard session.

## 3. Rollback

Roll all 13 back to 0.7.59.
