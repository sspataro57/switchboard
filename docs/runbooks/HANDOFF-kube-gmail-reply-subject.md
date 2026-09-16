# Handoff to the kube session — gmail-reply-subject (SWT-61)

A gmail reply drafted with no subject reached a client as a standalone, subject-less email
(delivery #36). Every gmail delivery now carries a subject, resolved server-side at draft time and
gated at draft, edit, approve and send. Bug docs:
`docs/bugs/gmail-reply-empty-subject-off-thread{,_REPRO,_DIAGNOSIS}.md`.

Image: `192.168.50.20:5000/switchboard:0.7.27`
(`sha256:292e42f21aa3c54e4e3ee08c04b978fc032c68cc997c48d7be782f6758954f4a`), built from `main`
at `7a9d84b`.

## 1. No migration

Nothing in `migrations/` changed (newest is still 0036). No env var, route, port or manifest change
beyond the image tag. The `CHECK` that would make a subject-less gmail row unrepresentable at the
table was deliberately NOT added: prod row #36 is a sent row that would violate it, so it needs a
backfill decision first.

## 2. Roll the new image tag

Only `deployment/dashboard` changes behaviour in the cluster: the deliveries page renders each
row's own subject with a `(no subject)` marker on gmail rows, and `approve_delivery` refuses a
gmail row whose subject is empty. Roll the same tag to all 11 workloads in one apply as usual,
keeping the pins (classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`).

```bash
kubectl -n ops get cronjob,deploy -o wide   # every image on the new tag
```

## 3. Half of this fix is NOT in the image — no action for you, but do not expect it here

The Dockerfile builds `cmd/connectors/...`, `migrate`, `dashboard`, `google-auth`, `classify`,
`orchestratord` and `pipelined`. It does **not** build `ops-mcp`, `ops-mcp-user` or `opsctl`, which
are `go install` binaries on the workstation. The draft-time fill and the edit refusal reach MCP
callers only through those binaries, not through this roll. Already reinstalled from `7a9d84b` on
2026-09-16 09:14; they take effect in a NEW Claude Code session.

## 4. Smoke (switchboard session)

`GET /deliveries` returns 200, a gmail row with a subject shows it as text next to the `Thread:`
line, and a subject-less non-gmail row carries no `(no subject)` marker.

Nothing is queued that the new approve gate could block: as of 2026-09-15 prod had zero gmail
deliveries in `drafted`, `approved` or `sending` with an empty subject — #36 is the only one that
ever existed and it is long sent.

## Rollback

Roll back to `0.7.26`. Nothing is stored and no schema changed: instant and lossless. Subjects
filled while 0.7.27 was live stay filled — they are ordinary column values, and a rolled-back
dashboard renders them normally.
