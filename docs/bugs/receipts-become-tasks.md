> Jira: SWT-68

# receipts-become-tasks

swb task #421. Reported by Salvador, 2026-09-19, in the switchboard session.

## The report, verbatim

After the MSN mailbox (sspataro57@msn.com) was onboarded and filed under `personal` (capture rule 76),
the board's incoming section filled with tasks titled like `$60.96 USD`, `$1.00 USD - Sep 6, 2026`,
`$50.55 USD due today`. Salvador dismissed them from the dashboard, then said:

> I removed those

and, asked whether to fix the cause:

> yes open a task and fix it. receipts are not actionable

## Observed (from the ops db at the time of the report)

- 19 `personal` tasks whose title starts with `$`, all now `closed`.
- 14 `task_dismissals` rows by `dashboard:salvo`, reason `not_actionable`, in the two hours before the report.
- The tasks were created by the personal classify lane + promoter from inbound mail in the MSN mailbox.

## Expected

A payment receipt, charge confirmation or billing notification that needs no action from Salvador
never becomes a task. Open point to establish from the 19: whether any of them was a bill genuinely
due (something he must pay by hand) rather than a receipt — his statement covers receipts.

## Reproduction set

`task_dismissals` by `dashboard:salvo` on 2026-09-19 for project `personal`, plus the other closed
`personal` tasks titled `$…`; their source messages through `classify_promotions`
(`normalized_message_id`, `ai_extraction_id`).
