# Handoff to the kube session: dashboard demo mode (SWT-99, swb 841)

A database-only switch that filters the web dashboard for a client demo. It is OFF by default: no `demo_mode`
row exists, and nothing turns it on. Salvador asks the switchboard session to turn it on when he's ready.

Image: `192.168.50.20:5000/switchboard:0.7.63`
(`sha256:a6ff7ce4d782aba88f503d035d9f1f83fe88f1dad857b8d29009af8402dc1a52`), built from `main` at `d1a795c`.
It includes everything in 0.7.62.

## 1. Migration FIRST: 0048, before any 0.7.63 pod runs

`migrations/0048_tasks_demo_hidden.sql` is `ALTER TABLE tasks ADD COLUMN demo_hidden boolean NOT NULL DEFAULT false`.
It is instant on PG11+, with no table rewrite. The dashboard reads this column on EVERY render, even with demo mode off.
If the image lands first:
- `/tasks` answers 500, and the error names `demo_hidden`;
- every task page answers 404;
- every board verb answers 503.

Apply it with the usual one-shot `migrate` Job on the new image, or from a workstation:
`DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations`.
Check: `SELECT max(version) FROM schema_migrations` returns 48. Old images (0.7.62) are unaffected by 0048.

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Only `deployment/dashboard` changes behaviour. Keep the pins, and do the SWT-76 check before replacing the watcher
pod. No env var, port or probe change.

## 3. Check

- Logged in, `GET /tasks` renders the full board (demo mode is off), and `/tasks/<any id>` opens.
- `SELECT count(*) FROM ops_flags WHERE name = 'demo_mode'` returns 0. Do not create it.

## 4. Rollback

Roll all 13 back to 0.7.62. Leave 0048 in place: forward-only, and harmless to old images.
