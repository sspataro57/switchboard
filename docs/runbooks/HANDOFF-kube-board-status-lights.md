# Handoff to the kube session — board-status-lights (SWT-52)

The switchboard session built and tested this ticket; the kube session owns the
manifests. The ORDER below is the whole point: do not reorder it.

## 1. Apply migration 0033 FIRST

`migrations/0033_task_working_state.sql` adds `tasks.working_state` and
`tasks.working_state_at` (nullable, no default, no backfill, no index) with the
named CHECKs `tasks_working_state_check` and `tasks_working_state_pair`.

- Run the one-shot migrate Job against the `ops` db on pg-main.
- Confirm: `SELECT max(version) FROM schema_migrations` → `0033`.

Why first: the new image reads and writes the two columns on paths that run all
the time. On a db without 0033, a new image fails:

- **every `/tasks` render.** `boardLightFacts` selects `working_state` and
  `working_state_at` for the lights, so the board itself errors, with or without
  a filter or auto-refresh;
- **every close.** `closeTransition` clears the columns on Done, Dismiss,
  `task_close`, the Jira reconciler and the orchestrator;
- **every reopen.** The same `closeTransition` UPDATE clears them when
  `task_reopen` moves a task out of `closed` (plain, guarded, revive);
- **every claim.** `task_claim`'s `ready → claimed` UPDATE clears them.

Old images on a 0033 db are fine: the columns are nullable, and an old binary
never names them. An old binary's close leaves the marker on the closed row. The
light ignores it there, and the next reopen or claim, run by a new image, clears
it. A rollback after the roll therefore needs no cleanup.

## 2. Then roll ONE image tag to every workload

Build from `main` after the merge, push to `192.168.50.20:5000`, and bump the same
tag on every workload that closes, reopens or claims:

- the dashboard. It carries the lights, the legend and auto-refresh, and its
  Done, Dismiss and Reopen buttons close and reopen;
- orchestratord, which closes;
- the Jira ticket-status reconciler CronJob, which closes and reopens;
- capture. It never closes a task; it reopens them through `task_reopen`
  (guarded and revive). Since the reopen now clears the marker (SPEC D9
  amendment, 2026-09-14), its reopen writes the two columns too, so it needs the
  new image like the rest;
- pipelined.

**Rollout barrier: §3 waits until EVERY workload above runs the new tag.**
Sessions get `task_signal` only when the switchboard session installs
`ops-mcp-user` and the skill (§3), which happens after the roll. So no marker can
exist while any workload still runs an old binary. A later rollback is covered:
an old binary's close leaves a marker, and the next reopen or claim clears it.

No new env var, no new route, no new port. Auto-refresh is opt-in per page
(`/tasks?refresh=on`), a full-page reload every 5 s, and stores nothing.

## 3. Then, on the workstation AND on 192.168.50.30 (the switchboard session does this)

Only after §2 is complete on EVERY workload (the rollout barrier). On `main`:

```bash
cd ~/projects/personal/switchboard && git switch main
go install ./cmd/ops-mcp-user
go install ./cmd/opsctl
make install-skill
```

Then open NEW Claude Code sessions: `/mcp` shows `ops` with fourteen tools, and the
`swb-status` skill is listed. The skill is COPIED, never symlinked
(`docs/runbooks/ops-mcp-user-scope.md`, "The swb-status skill").

## 4. Smoke on the real board (through the port-forward)

- `/tasks?refresh=on`: the indicator reads "auto-refresh on (every 5 s, last
  refreshed HH:MM:SS)" and the time advances.
- The human `ready` tasks show one blue per project, grey otherwise. Claude
  `ready` tasks show at most one blue per client (SPEC D2 amendment).
- Leave the tab open for 10 minutes: `SELECT count(*) FROM pg_stat_activity WHERE
  datname='ops'` shows no connection growth.
- Record `SELECT status, count(*) FROM tasks GROUP BY 1` in the delivery summary,
  including the `delivered` count (delivered tasks stay on the board until closed;
  SPEC D5).
