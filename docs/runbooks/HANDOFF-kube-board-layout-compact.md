# Handoff to the kube session — board-layout-compact (SWT-57)

The `/tasks` board's layout: sections from the lights (blocked, in flight, queue, holding,
done, other), a project-only first line with an Advanced filter popup, per-row Dismiss/Done
behind an `actions` popup, a short `updated` stamp, and the legend at the bottom.
SPEC: `docs/tickets/board-layout-compact_SPEC.md` (Verification step 5).

## 1. No migration

Nothing in `migrations/` changed. No schema, env var, route, port or manifest change beyond
the image tag.

## 2. Roll the new image tag

Image: `192.168.50.20:5000/switchboard:0.7.24` (the digest rides in the handoff message).

Only `deployment/dashboard` changes behaviour. Roll the same tag to all workloads in one
apply as usual (so no binary lags; nothing else changes behaviour), keeping the existing pins
(classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`). No ordering constraint.

```bash
kubectl -n ops get cronjob,deploy -o wide   # every image on the new tag
```

## 3. Smoke (switchboard session)

- `GET /tasks` returns 200, and the page carries `<div class="topbar">`, `id="section-…"`
  headers, and the legend after the last table.
- On the tablet, `/tasks?refresh=on`: BLOCKED and IN FLIGHT are above the fold; a row's
  `actions` popup opens and the board does not reload while it is open.

## Rollback

Roll `deployment/dashboard` (and the rest) back to the previous tag. Nothing is stored and no
schema changed, so rollback is instant and lossless; the URL keys are unchanged, so bookmarks
stay valid.
