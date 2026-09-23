> Jira: SWT-80

# revived-task-not-in-incoming

swb task: #553 (priority high)

## Report (verbatim, Salvador, 2026-09-23)

> so there is comment in jira from katie and the task didn't reopen

## Evidence gathered before the reproduction (production, read-only)

- Ticket WEB-10362, switchboard task **#381** (human, collaboratory).
- 2026-09-22 22:45:08Z: the ticketstatus reconciler closed #381 ("ticket_delivered; status TT-In QA").
- 2026-09-23 13:16:23Z: Katie's Jira comment.
- 13:16:47Z: Jira's email copy (normalized message 459483, rule 75) was logged onto #381 without a revive
  ("not resurfaced: the sender is on the project's notifier list" — the email copy never revives, by design).
- 13:30:04Z: the Jira connector's copy (message 459664, rule 71, an activity rule) revived #381 closed → ready
  ("revived by activity: message 459664 from Katie Evans"), surfaced = true. The connector-jira CronJob runs
  every 15 minutes, hence the 14-minute gap.
- But `tasks.activity_at` for #381 is still 2026-09-22 15:45 (message 403477): the revive did not put the task
  in INCOMING, so on the board it sat in the queue, not where a new comment is surfaced.
- Suspected (not yet verified by reproduction): capture's task_log branch calls the activity mark BEFORE the
  revive/reopen, and the mark skips a closed target.
