# Handoff to the kube session — signal-session-name (SWT-56)

Every `working` / `needs_input` signal now names its Claude session, the board shows the
name beside the light, and the user-scope MCP profile gains a READ-ONLY `task_context`.
SPEC: `docs/tickets/signal-session-name_SPEC.md` (Verification step 6).

## 1. Apply migration 0036 FIRST

One-shot migrate Job with the new image, as for 0033–0035. It is one statement:

```sql
ALTER TABLE tasks ADD COLUMN working_session TEXT;
```

Nullable, no default, no index, no CHECK, no backfill. Confirm:

```bash
psql "$OPS_DATABASE_URL" -tAc "SELECT max(version) FROM schema_migrations"   # 0036
```

**Why first.** Any binary built with this change fails on a db without 0036: every
`/tasks` render (`boardLightFacts` selects the column), every close, reopen, claim and
signal (they write it), and every `task_context` (it selects it). That includes worker
consoles' `ops-mcp`, which would break the claim loop. Old binaries on a 0036 db are fine:
they never name the column.

## 2. Then roll ONE image tag to every workload, together

The same 11 workloads as 0.7.22, in one apply, keeping the existing pins
(classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`). Every workload that
closes, reopens or claims must carry the new binary: dashboard, orchestratord, the Jira
reconciler (connector-jira), the capture writers and pipelined.

```bash
kubectl -n ops get cronjob,deploy -o wide   # every image on the new tag
```

No new env var, no new route, no new port, no manifest change beyond the tag.

## 3. Switchboard session, after step 2 (not kube)

On `main`, on this workstation AND on 192.168.50.30:

```bash
go install ./cmd/ops-mcp-user && go install ./cmd/opsctl && make install-skill
```

Also reinstall `ops-mcp` / `opsworker` wherever worker consoles run from installed
binaries. Then RESTART every open Claude Code session: a session keeps its old
`ops-mcp-user` process until it restarts, so until then its signals carry no name and it
has no `task_context`. `/mcp` in a new session shows fifteen tools.

## 4. Smoke (switchboard session)

- BEFORE any new signal, record the pre-0036 markers, which render `session unknown`:
  `SELECT id, working_state, working_state_at FROM tasks WHERE working_state IS NOT NULL`.
- One real session signals and its name shows on `/tasks?refresh=on`; it reads its task
  with `task_context`.
- `SELECT payload FROM task_events WHERE event_type='working_state_changed' ORDER BY id DESC
  LIMIT 3` shows the five keys `from, from_session, session, to, worker_id`.

## Rollback

- Roll the workloads back to the previous tag (0.7.22). Old binaries never name
  `working_session`, so nothing errors (that is why there is no CHECK); their clears leave a
  dangling name under a NULL state, which nothing shows.
- On both machines, reinstall `ops-mcp-user`, `opsctl` and the skill from the previous `main`
  commit and restart open sessions.
- Rolling forward again: list `SELECT id FROM tasks WHERE working_state IS NOT NULL` and
  `clear` each whose session is unclear with the NEW opsctl
  (`opsctl call --tool task_signal --args '{"task_id":N,"state":"clear"}'`).
- Schema is forward-only: never drop or edit 0036 in place.
