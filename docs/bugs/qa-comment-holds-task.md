> Jira: SWT-82

# qa-comment-holds-task

## Report (verbatim)

Salvador, 2026-09-24: "Jahnvi slacks are not comming in as tasks", then, after the diagnosis:
"make QA comments reopen the task".

## What happened

Jahnvi Seth (QA) commented on API-4323 (2026-09-23 21:17Z) and API-4324 (22:16Z). Both tickets sat
in `TT-In QA`, collaboratory's delivered status, assigned to him.

| task | event | at |
|---|---|---|
| #592 | log: jira message 477115 from Jahnvi Seth (rule 72, reviving) | 21:30:06 |
| #592 | closed by ticketstatus: `ticket_delivered`, TT-In QA | 21:30:08 |
| #598 | log: jira message 480181 from Jahnvi Seth (rule 72) | 22:30:10 |
| #598 | closed by ticketstatus | 22:30:13 |

`surfaced_at` stayed NULL on both, so the reconciler closed them. #592 had been created a
moment earlier by rule 75 from the Jira notification email (not an activity rule), so creation
did not surface it either. The Jira bot's Slack DMs about the comments are notifier copies and
are logged only.

## Cause

SWT-45 J10: activity on an OPEN task only logs. Only a comment arriving AFTER the close revives
the task (J9), and the reconciler then holds it (J11). The outcome depended on arrival order:
a comment two seconds before the reconciler's close is lost, and the same comment after the close
brings the task back.

J10 exists because every Jira close sends an email. If that email surfaced an open task, the
reconciler would never close a done ticket's task.

## Fix

`commentHolds` (internal/capture/revive.go) makes one exception to J10. It applies to a
PERSON'S COMMENT as copied by the Jira connector: channel `jira`, external id `…:comment:{id}`,
inbound (his own comments are outbound and never decided), a named sender, not on the project's
notifier list, matched by an activity rule, on a task that is neither closed nor actively worked. Such a comment surfaces
the task through `task_mark_surfaced`, after the log and the activity mark. The reconciler holds
it as it holds a revived task until he closes it or the ticket's facts change (for example, the
ticket goes Done).

These still follow J10 and only log: notification emails (the close email among them), the
ticket-description copy (`…:issue:{KEY}`, re-emitted on every update), notifiers, and blank
senders.

Review decisions (go-reviewer, 2026-09-24):
- **Active work is excluded** (claimed, in_progress, needs_feedback). The holder already reads the
  comment in the log. The reconciler also never consumes a surfacing while work is active, so it
  would linger and hold the task later, when the ticket reaches a delivered status.
- **No J17 own-action guard on this path.** His own Jira comments are normalized as outbound
  whenever the account's `own_account_id` is set, and capture never decides outbound messages.
  Verified 2026-09-24: both Jira `source_accounts` rows (7, 1400) have it set.
- **Bot commenters:** every inbound `:comment:` sender in the last 60 days is a person (Katie
  Evans, José Garcia, and four others). A named bot would hold a task unless it is added to the
  project's `notifier_senders`.
- **A comm-armed rule** still marks activity on the comm task, and the target is surfaced (held).
  `surfaced_at` is read only by the reconciler, never by the board, so the rows are not doubled.

A new counter, `surfaced_open`, is printed on every capture_rules line (live only).

Cost: a person's comment on a ticket that is already Done, landing before the reconciler's close,
now holds the task until one hand close. That matches what the same comment does after the close
today (J9).

## Test

`internal/capture/commentholds_test.go` (the truth table) and
`internal/capture/swt82_comment_holds_integration_test.go`: the held task, the three J10 controls,
and shadow mode. Mutations checked red: commentHolds forced false; the apply call dropped.

## Already-closed tasks

#592 and #598 stay closed. The fix acts on new comments only, and her two comments were QA passes
(the feature works). A human's plain reopen (task_reopen) would bring them back and hold them (J8).
