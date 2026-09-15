# Handoff to the kube session — board-incoming-first (SWT-59)

The `/tasks` board gains a first section, INCOMING, above BLOCKED: tasks the promoter created
from an email or Slack message, and PR review tasks. SPEC:
`docs/tickets/board-incoming-first_SPEC.md` (Verification step 5).

## 1. No migration

Nothing in `migrations/` changed (newest is still 0036). No env var, route, port or manifest
change beyond the image tag.

## 2. Roll the new image tag

Only `deployment/dashboard` changes behaviour. Roll the same tag to all 11 workloads in one apply
as usual, keeping the pins (classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`).

```bash
kubectl -n ops get cronjob,deploy -o wide   # every image on the new tag
```

## 3. Smoke (switchboard session)

`GET /tasks` returns 200 and the first `<h2` is `id="section-incoming"` whenever an open
promoter or PR review task exists (4 on 2026-09-15: 2 messages, 2 PRs).

## Rollback

Roll back to the previous tag. Nothing is stored and no schema changed: instant and lossless.
