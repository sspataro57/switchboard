# Runbook — orchestratord (SWT-41)

orchestratord is the spine loop from build step 05. It LISTENs on `task_events`, drains every
event past `orchestrator_cursor`, evaluates the pure rules (R1–R11), and applies their actions
through the executor as actor `orchestrator`. Its only broker write is `resume` on
`ops/workers/{id}/cmd`. It never calls an LLM, wires no sender, and holds no `OPS_TOKEN_KEY`.

It runs as one Deployment, `orchestratord`, in namespace `ops`. Advisory lock `0x5157_0005` keeps
it single-instance. See `docs/runbooks/HANDOFF-kube-orchestrator-deploy.md` for the manifest.

## Reading its health

Health is judged from outside the process, because a process that isn't running can't report on
itself.

- **`/funnel` → Orchestrator section.** It shows the verdict, the backlog (events past the
  cursor), the oldest unprocessed event, the cursor, and the head.
  - `ok` — the lock is held and nothing unprocessed is older than 5 minutes. With no new events
    the cursor legitimately stands still; that's `ok`, not stale.
  - `not_running` — nobody holds the orchestrator lock: the pod is scaled to 0, crash-looping, or
    was never deployed.
  - `stalled` — the lock is held, but an unprocessed event is older than 5 minutes (five ticks).
    The loop is alive but not draining. Check `kubectl -n ops logs deploy/orchestratord` for
    `drain failed`.
- **`/tasks`** shows one red line whenever the verdict isn't `ok`. The board never breaks on a
  failing health query: that failure shows only on `/funnel`, inline.
- **`GET /healthz` on `:8091`** is the Kubernetes liveness probe, not for humans. It returns 200
  only if a loop iteration finished within 3 ticks AND the lock connection answers. Three consecutive 503s (about 90 seconds) restart the pod.

**"another orchestratord holds the advisory lock; exiting"** right after a rollout is expected:
the old pod still held the lock. It clears within one restart. `strategy: Recreate` makes it rare.

A **lost lock connection** (for example a CNPG switchover) makes the process exit non-zero before
its next tick or drain — the lock is checked before every one, and again before every event
inside a drain. Kubernetes restarts it, and it takes the lock again. That's by design: an engine that
kept draining without the lock could double-apply.

## `orchestrator_cursor_advance` — when to use it, and when not

Moving the cursor forward is how a human decides to throw away lifecycle events. It is:

- `humanOnly` and off MCP;
- a compare-and-set on the value you expect;
- refused while any orchestratord holds the lock;
- audited, with a histogram of what it skipped.

```bash
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool orchestrator_cursor_advance \
  --args '{"expect_last_event_id":<current cursor>,"reason":"<why>"}'
```

**Use it only** for the first switch-on of a never-run engine, or after a deliberate decision to
discard a known-bad backlog.

**Never** use it as a routine restart step. After downtime, catching up is the default and
correct behaviour: the engine drains what it missed, and the rules' dedup facts make replays
no-ops. To stop the engine, scale to 0. The cursor stays put, and a restart catches up.

## Cutover (first switch-on, 2026-09-11)

The owner decided on 2026-09-11 to start from now: don't replay the backlog since July.

### P0 — the July leftovers are closed, and before the cursor

Re-run right before the advance. Both must hold:

```sql
-- none of #4–#6, #8–#20 is open
SELECT id, status FROM tasks WHERE id IN (4,5,6,8,9,10,11,12,13,14,15,16,17,18,19,20) AND status <> 'closed';
-- their closing events: the highest id must be <= the advance's reported "to"
SELECT max(id) FROM task_events
 WHERE task_id IN (4,5,6,8,9,10,11,12,13,14,15,16,17,18,19,20) AND event_type = 'status_changed';
```

### Pre-cutover checks (prod, read-only, run 2026-09-11 ~15:40Z)

| Check | Query intent | Result |
|---|---|---|
| P1 | cursor, head, histogram past cursor | cursor **75** (2026-07-12 01:01Z), head **989**; past cursor: log 796, status_changed 84, delivery_confirmed 1, delivery_sent 1, priority_changed 1 |
| P2 | expired unreleased claims on claimed/in_progress (R6 would release on tick 1) | **0** |
| P3 | `delivery_sent` past the cursor (R8 effect start-from-now forgoes) | 1: event 76, task #21 "calendar-booking smoke", already `closed`. **No by-hand R8 needed.** |
| P4 | `blocked` tasks whose deps are all satisfied (R5 unblocks forgone) | **0**; 0 blocked tasks in total |
| P5 | smoke project for the usable-alone smoke | `smoke` (delivery `dashboard`) |

```sql
BEGIN READ ONLY;
SELECT last_event_id, updated_at FROM orchestrator_cursor;                       -- P1
SELECT max(id) FROM task_events;
SELECT event_type, count(*) FROM task_events
 WHERE id > (SELECT last_event_id FROM orchestrator_cursor) GROUP BY 1;
SELECT c.task_id, c.worker_id, c.expires_at, t.status FROM task_claims c          -- P2
  JOIN tasks t ON t.id = c.task_id
 WHERE c.released_at IS NULL AND c.expires_at < now() AND t.status IN ('claimed','in_progress');
SELECT e.id, e.task_id, t.status FROM task_events e JOIN tasks t ON t.id = e.task_id  -- P3
 WHERE e.event_type = 'delivery_sent' AND e.id > (SELECT last_event_id FROM orchestrator_cursor);
SELECT t.id FROM tasks t WHERE t.status = 'blocked' AND NOT EXISTS (               -- P4
  SELECT 1 FROM task_dependencies d JOIN tasks dep ON dep.id = d.depends_on_task_id
   WHERE d.task_id = t.id AND dep.status NOT IN ('done_locally','delivered','closed'));
SELECT slug, delivery FROM projects WHERE slug IN ('smoke','switchboard');        -- P5
ROLLBACK;
```

### Closes before the advance (owner-approved Q1/Q2, done 2026-09-11 15:39Z)

The July smoke leftovers (#4, #5, #6, #8) and the July plan follow-ups (#9–#20) were closed
through `opsctl call --tool task_close`, audited as `opsctl:salvo`. Their `status_changed` events
fall before the new cursor, so the engine never evaluates them. The morning brief stays off
(`ORCH_BRIEF_PROJECT` unset) — see "Turning on the morning brief" below.

### Sequence

1. Merge SWT-41 to `main`; build and push the image carrying `/usr/local/bin/orchestratord`.
2. Re-run P1 and note the head.
3. Advance the cursor (from merged `main`):
   ```bash
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool orchestrator_cursor_advance \
     --args '{"expect_last_event_id":75,"reason":"SWT-41 start from now"}'
   ```
   Its `skipped_by_type` must match P1's histogram plus whatever arrived since. Paste the output
   below.
4. The kube session applies the Deployment. Only after step 3: a Deployment applied first would
   replay the backlog, and the advance refuses while an engine runs.
5. Check:
   - `kubectl -n ops logs deploy/orchestratord` shows `orchestratord running`;
   - `/funnel` shows Orchestrator `ok` with backlog 0;
   - `/tasks` has no red line.
6. Run the smoke in SPEC V5 (the `smoke` project, worker id `swt41-smoke`).

Cursor-advance output: _(pasted at cutover)_

## Turning on the morning brief

Off for the first deploy (owner, 2026-09-11). To turn it on, add to the Deployment's env:

- `ORCH_BRIEF_PROJECT=<slug>` — the project the daily "Morning brief YYYY-MM-DD" task is created in.
- `ORCH_BRIEF_HOUR=7` — optional; default 7.
- `TZ=America/New_York` — required in practice: the container clock is UTC, so without it hour 7
  is 03:00 Eastern. Distroless `static` ships tzdata.

Nothing closes old briefs; they pile up unless closed.

## Rollback

`kubectl -n ops scale deploy/orchestratord --replicas=0`. Nothing on the database side needs
undoing. The cursor stays where the engine left it, and a later start catches up.
