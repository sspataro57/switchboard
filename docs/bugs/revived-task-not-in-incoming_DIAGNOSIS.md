# Diagnosis — revived-task-not-in-incoming (SWT-80, swb #553)

## Root cause
This is a call-order bug that SWT-72's spec chose on purpose. The activity mark (`task_mark_activity`) runs
**before** the reopen or revive in both attach paths:

- capture: `EvaluateRules`, `case actionTaskLog`, `internal/capture/rules_store.go:578-585` (mark) runs before
  `:635-661` (prClose / `reviveRuleTask` / `reopenRuleTask`);
- promote: `act`, `case "attached"`, `internal/promote/store.go:204-210` (mark) runs before `:216-223`
  (`reopenDismissed`).

The tool skips a closed task (`internal/tools/activity.go:92-95`, `skipActivityClosed`). So at mark time the
target is still closed, and the tool answers `{marked:false, skipped:"task_closed"}` with audit status `ok`.
Then the reopen or revive makes the task open again (`ready`/`closed_from_status`). Nothing marks it after that.
The close already set `reviewed_at` (`internal/tools/close.go:125-129`, SWT-72 D8), and the reopen leaves that
column alone. So `activity_at` is either NULL or older than `reviewed_at`, the board's
`needs_review = activity_at > reviewed_at` (`internal/dashboard/board.go:426`) is false, and the task shows in
QUEUE instead of INCOMING. SWT-72 designed it this way: SPEC D3 says "the SWT-36 reopen and SWT-45 revive
branches run AFTER the mark, so … the mark self-excludes" (`docs/tickets/activity-resurfaces_SPEC.md:180-182`).
Criterion 11 (`:458-459`) and `TestCaptureActivity_Integration_ClosedTargetsAreUntouched`
(`internal/capture/rules_activity_integration_test.go:234-283`) pin that outcome. The spec expected SWT-45's
`surfaced_at` or SWT-36's "reopened after dismissal" marker to make a revived task visible. But INCOMING
(SWT-59/72) deliberately does not read `surfaced_by_message_id` (IK "INCOMING is the first section"), so
neither of those puts the task in INCOMING.

## Evidence
- `internal/capture/rules_store.go:564-585`: `appendRuleLog`, then `markRuleActivity` guarded by
  `!decision.prClose && !decision.comm`. The comment at `:571-573` states the ordering assumption.
- `internal/capture/rules_store.go:635-667`: `prClose` → `closeRuleTask` / `decision.revive` → `reviveRuleTask`
  / `decision.dismissalID != 0` → `reopenRuleTask`. Each returns `reopened bool`. Only `stats` reads that value,
  and nothing marks the task after it.
- `internal/promote/store.go:194-223`: `appendVerdictLog` → `recordTask` → `markVerdictActivity` →
  `reopenDismissed`. The comment at `:201-203` says so directly: "the tool skips a closed target, which is what the
  inquiry lane's remaining attach (a dismissed task) always is". `Decide`
  (`internal/promote/promote.go:152-166`) gives only two `attached` shapes: an open task (mark, no reopen) or a
  dismissed one (`ReopenDismissalID`, closed at mark time, so the mark is always skipped).
- `internal/tools/activity.go:92-95`: `if status == "closed" { result["skipped"] = "task_closed"; return nil }`.
  The doc comment at `:21-23` states the same assumption.
- `internal/tools/close.go:125-129`: a close sets `reviewed_at = now()`, and a reopen does not change it. So after
  any close, only a NEW mark can put the task back in INCOMING.
- git blame: every mark call site and the skip come from `4604437` (2026-09-22 11:11, SWT-72
  activity-resurfaces). `5f9a9e9` (SWT-74, same day) only added `&& !decision.comm` to the guard. SWT-72 was
  written this way on purpose, so this is not a regression from a later commit.
- Production audit (read-only): 3 of 3 effective capture/promote reopens since SWT-72 went live follow the same
  order: `task_append_log` ok → `task_mark_activity` ok (a skip) → `task_reopen` ok:
  #452 (audits 5148-5150, capture:google), #155 (5215-5217, promote:inquiry), #381 (6233-6235, capture:jira,
  `revive:true`).
- The repro runs RED on 3 variants and GREEN on the control. In every RED variant `activity_by` is the creating
  message and `activity_at` is earlier than `reviewed_at`.

## Why the reproduction fails
- A (reconciler close, then revive): the close sets `reviewed_at` = `closed_at`. The comment's pass calls the mark
  while the task is closed, so it is skipped. `reviveRuleTask` then sets the task to `ready` with `surfaced_by` =
  the comment. `activity_by` stays on message 15, and `activity_at` (09:45:58.627) is earlier than `reviewed_at`
  (.638).
- B / C (dismiss, then reopen through the reviving rule or through a plain rule's `dismissalID`): the same skip,
  then `task_reopen` succeeds. The columns are unchanged.
- Control: the task is open, so the mark writes `activity_at = now()`, which is later than the hand-set
  `reviewed_at`. PASS.

## Invariant implicated
None of the seven is broken: every write went through the executor and was audited, and the orchestrator is
untouched. The fix must keep invariant 3: the new mark goes through `task_mark_activity` on the executor, not a
column write inside `task_reopen`. The broken rule is SWT-72's own contract ("activity on a task puts it in
INCOMING"), and one of SWT-72's criteria excluded this case explicitly.

## Should `task_mark_activity` keep skipping closed tasks? Yes.
Leave the tool unchanged. The skip is correct and other code depends on it:
- **SWT-53 resurface**: a `task_log` onto a closed task with no reopen (a non-activity rule, no open dismissal) must
  leave the closed task alone. The chat becomes its own inquiry item.
- **A non-reopen onto a closed task**: rule 75's notifier email copy on a reconciler-closed task (#381's 22:50
  and 13:16 marks, audits 5705 and 6201), a revive the handler refuses (`message_predates_close`), a reopen
  refused as `message_predates_dismissal`, and the own-action guard's non-revive.
- **D8 staleness**: if the tool marked a closed task, a later HUMAN reopen (dashboard/`task_reopen`, no message)
  would bring the row back as "needs review" with activity nobody asked about. That is exactly what D8's
  `reviewed_at`-on-close exists to prevent.

So the fix goes in the callers: **mark after the reopen or revive, on the same message**. Then "marked" means the
task was open when the tool checked.

## Proposed fix scope
- [ ] **capture** `internal/capture/rules_store.go`, `EvaluateRules` `case actionTaskLog`: move the target's
  `markRuleActivity` block (`:578-585`, with its `!decision.prClose && !decision.comm` guard) to AFTER the
  `prClose / revive / dismissalID` chain (after `:667`). One call site, no new branch. Resulting behaviour:
  - open target: revive or reopen answers `not_closed`, or is not called at all, then the mark sets activity.
    Same as today.
  - closed and reopened or revived: the mark sets activity. This is the fix.
  - closed and not reopened (resurface, notifier copy, a refused revive, own action): the tool skips.
    Same as today.
  - `prClose` and `comm`: still excluded by the unchanged guard.

  A crash between the reopen and the mark leaves today's behaviour (reopened, in QUEUE), which is the same
  degrade-to-today rule the log-first ordering follows. Update the comments at `:571-573` and `:627-634` to say
  "log → reopen/revive → mark".
- [ ] **capture gate**: nothing to change (see Out of scope).
- [ ] **promote** `internal/promote/store.go`, `act` `case "attached"`: move `markVerdictActivity` (`:204-210`)
  below the `if d.ReopenDismissalID != 0 { reopenDismissed … }` block. Keep one call site with no lane branch, so
  `TestPromote_TheActivityMarkIsLaneAgnostic` stays green. Rewrite the comment at `:201-203`: the inquiry lane's
  dismissed-task attach is now marked when the reopen succeeds.
- [ ] **tool doc**: rewrite the comment at `internal/tools/activity.go:21-23`. The closed skip stays, but it now
  exists for tasks that were NOT reopened, and callers mark AFTER any reopen. Handler code does not change.
- [ ] **Spec and IK amendments**: `docs/tickets/activity-resurfaces_SPEC.md` D3 bullet (`:180-182`), D3 table
  (`:195-196`) and criterion 11 (`:458-459`) get an "Amended SWT-80" note. IK SWT-72 bullets "SKIPS a closed
  task (so SWT-45 revive and SWT-36 reopen, which run after it…)" and "the hook … (after `appendRuleLog`,
  before revive/reopen)" need the same amendment. (The diagnoser adds the landmine line; the fix updates the
  prose.)
- [ ] **Tests** (test-author converts the repro):
  1. The three `TestRegression_SWT80_*` variants go GREEN: `activity_by_message_id` = the comment, and
     `activity_at > reviewed_at`.
  2. **Amend** `TestCaptureActivity_Integration_ClosedTargetsAreUntouched`. It currently pins the bug
     (`activity_at` nil, `Activity == 0` on a revive or dismissal reopen). Replace it with: revive → activity =
     the reviving message, `Activity == 1`, `Revived == 1`, and `surfaced_at` still set (SWT-45 unchanged).
     Dismissal reopen → activity = the message, `Reopened == 1`. Rename it (e.g.
     `…ReopenedTargetsAreMarkedAfterTheReopen`).
  3. **Negative cases, closed and NOT reopened, with columns read back from `tasks`** (the "test the column"
     rule). Each needs `activity_at` / `activity_by_message_id` unchanged, the task still closed, and a
     `task_mark_activity` audit row answering `task_closed` (proof the call was made and skipped, not left out):
     - (a) SWT-53 resurface: a non-reviving rule's `task_log` onto a closed task with no open dismissal;
       `capture_decisions.resurface = true`.
     - (b) a notifier-sender email copy (rule 75's shape) on a reconciler-closed task: no revive.
     - (c) a revive the handler refuses: message ingested before `closed_at`, answer `message_predates_close`.
     - (d) the own-action guard (`ownActionSkip`, `rules_store.go:1116`): his own Jira comment on a closed task
       with no open dismissal does not revive and does not mark.
     - (e) a dismissal reopen refused as `message_predates_dismissal` (capture and promote).
  4. **Audit order**: on a revive or reopen, `task_append_log` → `task_reopen` → `task_mark_activity`, all as
     `capture:{connector}`. Amend `TheLogComesFirst` to allow the reopen between the two, or add a sibling test.
     `TestCaptureActivity_ThePRCloseExclusionIsSpelledOnce` must still find `prClose` next to the moved call. If
     the new placement moves the call more than 400 characters from the guard, adjust the test window.
  5. **promote**: an integration test for the inquiry lane's rule 2. A dismissed thread task, then a new
     inquiry message: `task_reopen` succeeds and the task lands in INCOMING with `activity_by` = that message
     (the #155 shape). Plus a negative: a refused reopen leaves `activity_at` unchanged. Existing
     `ownask_integration_test.go:169` (the old task gets no mark from the D11 pointer) must stay green.
  6. The tool-level criterion 24 test (`internal/tools/requeue_integration_test.go:274`, a direct `task_reopen`
     with no mark stays out of INCOMING) stays green unchanged. A plain human or reconciler reopen still does not
     mark.

## Backfill (production, via the executor)
Current state (read-only, 2026-09-23):

| task | status | activity_by | reviewed_at | action |
|---|---|---|---|---|
| #381 | `ready` | 403477 (09-22 15:45) | 09-22 22:45 (reconciler close) | **backfill** |
| #452 | `closed` (reconciler re-closed 09-22 17:00, audit 5155) | NULL | 17:00 | none: a closed task is skipped and would never show |
| #155 | `closed` (re-dismissed `handled_elsewhere`, dismissal 144 open, 09-22 22:28) | NULL | 22:28 | none: a human reviewed it after the reopen and closed it again |

No other capture or promote reopen has happened since SWT-72 went live (the audit query returns exactly these
three).

#381 is backfilled with one executor call. `task_mark_activity` is not humanOnly and is off MCP, so the
route is `opsctl call` (actor `opsctl:salvo`, audited, allowed by the static list):

```
# re-read first (owner works the board concurrently): skip if status='closed' or reviewed_at > '2026-09-23 13:30:04Z'
DATABASE_URL=<prod> opsctl call --tool task_mark_activity \
  --args '{"task_id":381,"message_id":459664,"reason":"SWT-80 backfill: revived 2026-09-23 13:30Z by message 459664; the mark ran before the revive"}'
```

Expected result: `{marked:true}`, with `activity_by_message_id = 459664` and `activity_at = now()`, later than
`reviewed_at`, so #381 appears in INCOMING with "new comment" from Katie Evans. The backfill runs only when
Salvador authorizes it, and it does not depend on the code fix shipping.

## Out of scope for this fix
- **The capture gate** (`internal/capture/gate.go:450-465`, `capture:gate`, `ticket_assignee_gate` projects,
  currently only `reengine`) never calls `task_mark_activity`, for an open attach or a reopen. That is a separate
  SWT-72 coverage gap: gated projects' attaches never reach INCOMING. The gate made no calls at all since
  2026-09-22 15:12Z. File it separately if wanted.
- **A reconciler reopen** (`internal/ticketstatus/store.go:363-366`, no message) does not mark, correctly: that
  is not inbound activity.
- **Making `task_reopen` itself set the activity columns** is the alternative. It would be atomic and have one
  site, but it widens the reopen handler's write set, mixes SWT-36/45 semantics with board columns, and
  `TestActivityFile_TouchesOnlyTheActivityColumns` / the one-writer shape would need to move. Rejected in favour
  of the caller order.
- **Filtering own or bot activity** ("Anonymous (JIRA)", the Jira Slack bot) out of the mark: SWT-72 residual,
  future work.

## Open questions
- **Noise increase (for Salvador, not a blocker)**: over the last 14 days there were 42 effective capture/promote
  reopens, about 3 a day. With the fix, every one lands in INCOMING. About 10 of the 42 come from bot or own
  senders: `Jira` Slack bot 5, `Anonymous (JIRA)` (his own edit, #452's shape) 3, `Jira <jira@…>` mail 1, a
  GitHub notification 1. The open-task hook already surfaces these senders, so this matches SWT-72's
  sender-blind design, but those rows will now also appear on reopen. I have not verified whether any J10
  close-mail revive still happens (rule 75's notifier copy should not revive). The counts above are the evidence.
- None about the cause: the code, the repro and all three production audit trails agree.

## Risk assessment
- The change is limited to the order of calls in two branches. Open-target behaviour is unchanged: the mark still
  runs once with the same arguments, only later in the branch.
- `comm` (SWT-74): comm tasks exist only for OPEN targets (`commTask` rejects closed), so moving the target's
  mark cannot double-mark. The `!decision.comm` guard stays.
- `prClose`: still excluded. Without the guard, a close refused as active work would now be marked, so the guard
  must move with the call.
- Reconciler J11 hold: it reads `surfaced_at` only, and SWT-72 structure tests keep `internal/ticketstatus` and
  `internal/orchestrator` from reading the activity columns. Unaffected.
- Tests that change: `ClosedTargetsAreUntouched` (it inverts), possibly `TheLogComesFirst`'s exact sequence and
  the prClose-window structure test. SPEC criterion 11 is amended.

## Landmine matched
New, added to INSTITUTIONAL_KNOWLEDGE.md under SWT-72: **"a skip-closed mark placed BEFORE a status-changing
call in the same branch is a silent no-op for exactly the case that changes status"**. The `ok` audit status on
a skip hides it. Order a stamp that depends on status AFTER the call that sets the status.

## Review amendments (2026-09-23)

- **Accepted residual (codex, high):** reopen/revive and the activity mark are separate executor calls, so a
  crash or a failed mark between them leaves the task reopened but in QUEUE, and the spent live claim means no
  retry. This is the same shape as every capture/promote action sequence (create → record → provenance → mark),
  and the previous order had the mirror window (marked-and-skipped, then reopened). A failed mark returns an
  error and fails the pass loudly. Making it atomic needs a combined executor tool — not this fix.
