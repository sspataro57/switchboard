# Handoff to the kube session: /deliveries lands back on the row (SWT-111, swb 2898)

Salvador, 2026-10-30: clicking Approve on /deliveries reloads the page at the top and he has to scroll back down. Every
row verb now redirects to `/deliveries?flash=...#d<id>`, and the flash sticks to the top of the window.

Image: `192.168.50.20:5000/switchboard:0.7.72`
(`sha256:64291f70150521f6e0fc171c90d06e700e989271fc4fc5c82c67de7b59564d81`), built from `main` at `1164d3e`.
It includes everything in 0.7.71, plus two commits already on main that never got their own roll: `e307588` (swb-push:
a session's own tasks never nudge it) and `fe7f2a5` (task_context returns source_thread_id).

## 0. Already done

Nothing. There is no migration.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Only `deployment/dashboard` changes for this ticket. Keep the pins, and do the SWT-76 check before replacing the watcher
pod. No env var, port or probe change.

## 2. Check

- `kubectl get deploy -n ops` shows every workload on 0.7.72 and available.
- `GET /healthz` on the dashboard answers 200.
- Leave the click-through to the switchboard session: approving a row is a real action.

## 3. Rollback

Roll all 13 back to 0.7.71.
