# Reproduction — revived-task-not-in-incoming (SWT-80, swb #553)

## Status
Confirmed. Three variants fail; the open-task control passes.

## Trigger
Setup (mirrors production #381 / WEB-10362, collaboratory, human task, project not gated):

1. The project has two rules. Rule 71's shape: `thread_key_contains` `jira:{site}:RVA-`, key_regex `[A-Z]+-[0-9]+$`,
   priority 92, `revive=true`. Rule 3's shape: non-reviving `thread_key_prefix`, priority 50. As in production, the
   task was created unsurfaced by rule 3 while rule 71 was off. Then the rules swap: rule 3 off, rule 71 on.
2. A first jira comment creates the task in a live `capture.EvaluateRules` pass. `external_refs` is written by
   `link_external_ref`, and the task is set to `assignee_type='human'`.
3. The task is closed:
   - **A**: `ticketstatus.Run` sees a stored `done` issue snapshot and closes the task with `task_close` as
     `ticketstatus:jira`.
   - **B / C**: `task_dismiss` (`dashboard:` actor, `not_actionable`) on the executor.
4. A new inbound `channel='jira'` comment from "Katie Evans" is added to the ticket's connector thread, with an
   ingest time after the close. Then a live `capture.EvaluateRules` pass runs.

| Variant | Close | Rule matching the comment | Comes back? (works) | Lands in INCOMING? |
|---|---|---|---|---|
| A (production #381) | reconciler `task_close` | revive=true (rule 71 shape) | yes: `ready`, Revived=1, surfaced_by = comment | **NO: FAIL** |
| B (SWT-36 + revive rule) | `task_dismiss` | revive=true | yes: `ready`, dismissal `reopened_by_message_id` = comment | **NO: FAIL** |
| C (pure SWT-36) | `task_dismiss` | non-reviving | yes: `ready`, dismissal `reopened_by_message_id` = comment | **NO: FAIL** |
| Control | none (task open, `reviewed_at` stamped) | revive=true | stays `ready` | yes: PASS |

## Observed behavior
In A, B and C, after the pass:
- `tasks.activity_by_message_id` still names the first (creating) message, not the new comment.
- `tasks.activity_at` stays earlier than `tasks.reviewed_at`. The close stamped `reviewed_at`, and in A `reviewed_at`
  equals `closed_at`. So `activity_at > reviewed_at` is false, and the task sits in QUEUE rather than INCOMING.

Sample from variant A: `activity_by=15 activity_at=09:45:58.627 reviewed_at=09:45:58.638 surfaced_by=16`. The comment
is message 16.

This matches production #381 exactly: status `ready`, `surfaced_by_message_id=459664`,
`activity_by_message_id=403477`, `activity_at` 2026-09-22 15:45 < `reviewed_at` 2026-09-22 22:45 (the reconciler close).

## Expected behavior
A task revived (SWT-45) or reopened from a dismissal (SWT-36) by a new inbound message lands in INCOMING. That means
`activity_by_message_id` = that message and `activity_at > reviewed_at`, the same result as the control, where the
comment arrives on an open task.

## Reproduction location
`internal/capture/swt80_revived_incoming_integration_test.go`, tests `TestRegression_SWT80_*`.

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_revincoming"   # once
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_revincoming?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_revincoming?sslmode=disable' \
  go test -tags integration -p 1 -count=1 -run TestRegression_SWT80_ ./internal/capture/ -v
```

The suite can be rerun (it cleans up by slug/account/actor before and after) but deletes `capture_decisions`
wholesale, so use the private database.

Production query (read-only), its logic: `task_reopen` audits by
`capture:%` / `promote:%`, each followed within 5s by a `status_changed` event from `closed`, joined to the current
`tasks` activity columns.

## Environment
- HEAD `736ef6075cd53ee50076d770fbf03780a880edbb` (branch fix-revived-task-incoming)
- Local: compose pgvector pg17 on :5433, private db `ops_revincoming`, migrations through 0043
- No env vars besides `DATABASE_URL`. No LLM, no network (`ticketstatus.Run` with an empty `Config`, snapshot stored
  as a raw issue row)

## Production count (read-only, last 14 days)
- 42 capture/promote reopens took effect (closed → open). 39 of them came before the SWT-72 activity mark existed
  (migration 0039 applied 2026-09-22 15:12Z; first `task_mark_activity` 15:20:39Z). Those could not have landed in
  INCOMING and are not this bug.
- **Since SWT-72 went live: 3 reopens, and all 3 landed outside INCOMING.** In each audit trail the order is
  `task_append_log` → `task_mark_activity` (ok) → `task_reopen` (ok), and the mark did not change the columns:
  - **#452**, `capture:google`, dismissal reopen, 2026-09-22 16:50:39Z, message 404077: `activity_at` NULL. The
    reconciler re-closed it at 17:00.
  - **#155**, `promote:inquiry`, dismissal reopen, 2026-09-22 18:03:53Z, message 405014: `activity_at` NULL. Later
    re-dismissed (dismissal 144 is open).
  - **#381**, `capture:jira`, revive, 2026-09-23 13:30:04Z, message 459664: `activity_at` = 403477 (2026-09-22 15:45).
    Still `ready`, and not needs-review now.
- Capture only: 2 (#452, #381). With the promote path: 3.

## Notes
- Variant A needed rule 71 to be off while the task was created. When a reviving rule creates the task, it surfaces
  the task, and the reconciler then holds it open instead of closing it (SWT-45 J11). Production #381 was created
  before rule 71 existed.
- #155 shows the same audit order on the promote (`promote:inquiry`) reopen path. This reproduction exercises only
  the capture path, and the promote path is not covered by a failing test here.
- The email copy (rule 75, `capture:google`, non-reviving, notifier sender) is not exercised. By design it never
  revives a reconciler-closed task.
