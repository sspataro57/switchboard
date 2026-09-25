> Jira: SWT-91

# stale-copy-reopens

Reported 2026-09-25 by the collaboratory-www-ec session (Salvador asked it to send), verbatim:

> Bug report from collaboratory-www-ec (Salvador asked me to send this): switchboard reopens
> already-dismissed collaboratory tasks in a same-second burst every ~30 minutes, with no new
> activity on the source Jira ticket.
>
> Evidence (project collaboratory, 2026-09-25):
> - Burst 1 at 18:13:07 UTC reopened 7 tasks: #663 (WEB-10442), #662 (WEB-10446), #458 (WEB-10445),
>   #452 (WEB-10469), #377 (WEB-10444), #91 (API-4340), #70 (WEB-10360).
> - Burst 2 at 18:43:23-24 UTC reopened #663, #458, #452, #377, #91, #70.
> - Earlier the same tasks also came back individually: #70 at 17:33, 17:41, 17:45 and 18:19; #91 at
>   17:45 and 17:46; #64 (WEB-10375) several times.
> - All were dismissed with reason handled_elsewhere or duplicate between bursts.
> - Jira shows nothing new on these tickets in the windows. [...]
>
> Expected: a dismissed task stays closed unless a genuinely new message, one sent after the
> dismissal, arrives on that thread. Repeats of already-seen notification copies shouldn't revive it.

## Reproduction (prod data, read-only)

- The bursts are Slack DM copies of Jira-app notifications (sender `Jira`), one burst per
  workspace: 18:13 = Avviato (T0360B84U, account 539), 18:43 = Collaboratory (T0HPR78RX, account 542).
- Slack 582535 "Katie transitioned WEB-10444 → TT-Verified": sent 17:15:27, ingested 18:13:01.
  Task 377 was dismissed at 17:30:35. Its twin 583650 was ingested at 18:43:05. No raw row is
  ingested twice; every copy has its own Slack ts.
- The 18:19 (gmail) and 18:30 (jira) reopens of #70 were José's genuinely new comment: correct.

## Diagnosis

`task_reopen`'s guarded and revive forms (`internal/tools/close.go`) compare ingest time only, by
design (dismiss-reopen SPEC D2: "over-reopening costs one click"). A late copy of an event from
before the dismissal therefore counts as new activity.

Ingest lag, inbound messages, last 3 days (seconds): gmail p50 45 / p95 812; jira p50 606 / p95 850
(max ~870); slack p50 1805, p95 in the months (backfill).

## Fix (Salvador's choices, 2026-09-25: "Both", then "Only bot copies")

- Capture flags a message whose sender is on `projects.notifier_senders` with `notifier_copy`.
- The verb, for a flagged message only: a Slack copy never reopens (`slack_notifier_copy`); any
  other copy sent more than 20 min before the put-down skips (`message_sent_before_dismissal` /
  `message_sent_before_close`). A NULL sent_at keeps the ingest clock alone.
- A person's message keeps D2 exactly.
- Tests: `internal/tools/stale_copy_reopen_integration_test.go`,
  `internal/capture/swt91_stale_copy_integration_test.go`.
