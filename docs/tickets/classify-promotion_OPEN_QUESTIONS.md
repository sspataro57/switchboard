> Jira: SWT-30

# classify-promotion — open questions

Three. Each changes code shape, not wording. The SPEC is written against the
assumption named in each entry; answering flips the assumption, not the ticket.

---

## Q1 — What IS the human-review lane: `holding` tasks, or promotion rows with an approve verb?

**(a) A `tasks` row with `status='holding'` in project `personal`.** The review
lane is `/tasks?project=personal&status=holding` — a filter, which is what
invariant 2 says a queue is, and `board.go`'s status order already renders
`holding` as the first column. Cost: `create_task` grows an optional `status`
(`ready|holding`), which is the parameter 06-gpt-triage's SPEC reserved for
exactly this; "approve" then means dragging the task to ready, which needs a
verb the dashboard does not have yet (`/tasks` today has no POST route at all),
so in practice approving is a psql UPDATE until someone builds it. Every review
decision leaves an `audit_events` row because it went through the executor.

**(b) A promotion row with `action='review'` and NO task, plus a review surface
with approve/dismiss.** Nothing enters `tasks` until a human says so, which is
the most literal reading of "never a live task". Cost: two new humanOnly
executor tools (`approve_classify_promotion` / `dismiss_classify_promotion`), a
new dashboard page with POST routes (and `/funnel` is documented as "a WINDOW,
NOT A CONTROL", so it would need its own page or an explicit renegotiation of
that criterion), and a review decision writes a promotion row but no
`audit_events` row — the one invariant-7 gap in the design.

SPEC assumes (a).

**Answer:** (a) — holding tasks (Salvador, 2026-09-09). The review lane is
`/tasks?project=personal&status=holding`; `create_task` gains the optional
`status` of exactly `ready|holding`.

---

## Q2 — Which clock does the cutover compare against: the verdict's, or also the message's?

**(a) `ai_runs.created_at >= classify_promote_after` only.** "Forward-only" means
"verdicts recorded after the cutover". Simplest, one clause. Verified safe today
in one respect: `classify eval` writes no `ai_runs`/`ai_extractions` rows at all
(it calls `lane.Complete` directly and scores in memory), so an eval run cannot
inject fresh-timestamped verdicts over old labelled mail. The residual exposure
is a personal-lane `run` that classifies a never-before-classified OLD message —
its verdict is recorded today, so a 2024 bill lands on the board as a live task.

**(b) `ai_runs.created_at >= cutover AND nm.sent_at >= cutover`.** Strictly
forward-only on both clocks and fail-closed: nothing older than the cutover can
ever appear, no matter when it happened to be classified. Cost: a genuinely
actionable old message that the lane reaches late is silently never promoted,
and the only trace is its absence.

SPEC assumes (a).

**Answer:** (a) — verdict clock only (Salvador, 2026-09-09). Forward-only means
"verdicts recorded after the cutover"; the late-classified-old-message exposure
is accepted and stays documented in the runbook.

---

## Q3 — Attach-before-create when the existing task on that thread is `closed` or `delivered`

The attach lookup is `tasks.source_thread_id = nm.thread_id`. A thread can
already carry a task that is finished — a `payment_due` task closed last month,
then a second notice arrives on the same thread.

**(a) Attach anyway.** One thread, one task, forever; the follow-up shows as a
`log` event on the closed task's detail page. Simple, and the same rule capture
uses for a ticket that already has a task. Cost: a real new obligation lands
inside a closed task and never appears on the board — invisible unless someone
opens it.

**(b) Attach only to a task that is not `closed`/`delivered`; otherwise create a
new task.** A thread yields at most one OPEN task, and a re-raised obligation is
visible. Cost: the promoter's attach key stops being "the thread" and becomes
"the thread's open task", so a thread can accumulate N tasks over time, and the
`Decide` function takes the existing task's STATUS as an input (fine — it is
still pure).

SPEC originally assumed (a); the answer flips it.

**Answer:** (b) — attach only to a task that is not `closed`/`delivered`;
otherwise create a new task (Salvador, 2026-09-09). A thread yields at most one
OPEN task; a re-raised obligation is visible on the board. `Decide` takes the
existing task's status as an input and stays pure. SPEC criterion 9 updated
accordingly.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
