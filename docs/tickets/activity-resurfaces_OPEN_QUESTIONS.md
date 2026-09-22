> Jira: SWT-72

# activity-resurfaces — open questions (ANSWERED 2026-09-22, folded into the SPEC)

Both answered by the owner the same day. The SPEC is DECIDED; this file is the record of what was
asked and what the answer changed.

---

## OQ-1 — Requeue on a `holding` task: leave the status alone, or lift it to `ready`?

**The situation.** The inquiry lane creates `holding` tasks (`createVerdictTask` with
`d.Status = "holding"`), and after the owner decision of the same day (SPEC D11) EVERY ask is one.
When such a task surfaces into INCOMING and you Requeue it, does it go back to holding, or forward
to the queue?

**A.** *Leave the status alone.* Requeue is purely "I looked at this": `reviewed_at`, optional
priority, nothing else. No tool in the repo lifts `holding → ready`
(`internal/tools/revive_test.go:95` refuses exactly that for a capture pass), so nothing new enters
the status machine. Cost: the button says "Requeue" and a holding row goes back to holding.

**B.** *Requeue lifts `holding → ready`, and is a no-op on every other status.* One transition inside
the row lock with its own `status_changed` event; the review lane finally has an exit that is not
Dismiss or Done. Cost: a status transition shipped inside a board ticket.

**Answer: B.** (Owner, 2026-09-22.)

*Folded in as SPEC D6 and criteria 20, 21, 43.* It is safe because
`internal/orchestrator/rules.go:124-129` fires on `status_changed` only for
`to ∈ {delivered, closed}`, so `holding → ready` runs no rule; the payload spelling is
`internal/tools/dependency.go:130,176`'s. It is also now the main use of the verb, because D11 makes
every ask a `holding` task.

---

## OQ-2 — A comment on a `claude` task that a worker is running: does it leave IN FLIGHT?

**The situation.** Capture attaches messages to whatever task holds the ticket's `external_refs`
row, and some are worker tasks in `claimed` / `in_progress`. SWT-59 I4 says INCOMING outranks every
light except green, so a marked worker task would move out of IN FLIGHT into INCOMING while the
worker keeps running it. SWT-59's PR-review fact carries an `assignee_type = 'human'` clause, so a
human-only clause has a precedent.

**A.** *Surface it like any other task.* Every inbound comment reaches him, including the ones that
change work a worker is mid-way through. Cost: IN FLIGHT stops being a complete list of what the
consoles are doing.

**B.** *Mark `claude` tasks but section only HUMAN ones into INCOMING.* The fact is still recorded
for `task_context`; the board only moves rows he owns. Cost: a comment that invalidates in-flight
work is invisible until the worker parks.

**Answer: A.** (Owner, 2026-09-22.)

*Folded in as SPEC D7.* The row keeps its yellow light and its session tag. If the yellow rows turn
out to annoy, the narrowing is one `AND t.assignee_type = 'human'` in the `needs_review` expression.

---

Nothing else was asked. Say "questions answered" only if something above needs changing again.
