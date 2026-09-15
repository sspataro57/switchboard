# signal-session-name — open questions

Only one decision is open. Everything else in
`docs/tickets/signal-session-name_SPEC.md` is decided (S1–S14). This one
changes only criterion 36.

## Q1 — Can a session in any repo read the text capture copied from your private mail?

**Background.** You asked for a way for a session to pull a task in full, and
the SPEC adds it: `task_context` in every repo's session, read-only.

Some tasks are made by capture from incoming mail. When capture makes or
updates such a task, it copies the start of the message into it. That is the
subject plus up to about 400 characters of the body, placed in the task's
description and in its log lines. `task_context` returns both.

Today only the switchboard repo's own session can read those. Sessions in your
other repos see task titles only. When you added attachment reading for those
sessions (SWT-42), full mail bodies were kept away from them on purpose.

Six projects are marked private for AI purposes (`local_only`): bulk, homelab,
personal, foundry, saka and town-ai. Most of them got that mark only because
every new project starts with it.

**(a) No filter.** Every session can read every task in full, private
projects included.
- It works the same in every repo, and it is the simplest.
- The cost: up to 400 characters of each captured message reach whichever
  session reads the task. That includes mail from the `personal` project.

**(b) Hide the mail text for private projects.** For a task in a `local_only`
project, other repos' sessions get everything except the description and the
log-line text, which read "withheld".
- It protects the personal mail.
- The cost: sessions in the foundry, homelab and town-ai repos could not read
  their own tasks' descriptions either. Fixing that means marking those projects
  not-private, which also lets their mail go to the hosted classifier.

Answer: **(a) Show everything.** On 2026-09-15 the owner chose "Show everything (Recommended)":
every session reads every task in full, private projects included. No locality filter on
task_context; criterion 36 (a) pins it with a test.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
