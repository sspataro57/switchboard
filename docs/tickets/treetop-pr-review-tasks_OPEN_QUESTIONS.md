# treetop-pr-review-tasks: open questions

Two questions, both about what lands on your board. Everything else in the SPEC is decided, with
the reasons written there.

## OQ-1. Bot PRs (Dependabot "Bump sanitize-html …")

A bot opens these PRs; nobody asked you to review them. Should they become review tasks?

- **(a) No task.** The rule lists `*[bot]` as an excluded author, so every GitHub bot is skipped.
  **Recommended:** these are routine version bumps, and you would dismiss them.
- **(b) Task.** Same as a colleague's PR.

This is data on the rule, so it can change later without code. Changing it means disabling the rule
and adding a new one with a slightly different pattern, because rules cannot be edited.

Answer: **(b) Task.** On 2026-09-14 the owner chose "Yes, task them too", which is NOT the
recommended default. Bot PRs, Dependabot included, become review tasks like a colleague's. So the
seeded rule carries NO `*[bot]` exclusion. The exclude list holds only his own second login(s),
if pre-check 0b finds any.

## OQ-2. A PR is merged or closed on GitHub

GitHub mails "Merged #N into main." or "Closed #N." Should that notice close the review task?

- **(a) Close it**, as Done, with the reason "PR #N merged on GitHub". **Recommended:** a review of
  merged or closed work is no longer work, and `task_reopen` undoes it.
- **(b) Leave it open.** The notice only lands in the task's log, so you learn it merged when you
  open the task.

Answer: **(a) Close it.** On 2026-09-14 the owner chose "Close the task (Recommended)". A merged or
closed notice closes the review task as done, with the reason "PR #N merged on GitHub" (or closed),
and the task shows green until midnight.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
