> Jira: SWT-34

# qa-delivered-drop — open questions

One question. It does **not** block implementation, tests or delivery: the answer
is one element in a hand-run `UPDATE`, no code change. Ship with `TT-In QA` alone
and add the second name if the answer is (a).

---

**Q1 — does `TT-In Review` belong in collaboratory's `ticket_delivered_statuses`
alongside `TT-In QA`?**

Two readings, and they demand opposite behaviour for the 2 tasks currently sitting
in it:

- **(a) "waiting on their reviewer" → delivered.** Seed
  `ARRAY['TT-In QA','TT-In Review']`; those 2 tasks drop with
  `drop_reason='ticket_delivered'` on the next pass, and come back if the ticket
  moves to `TT-Reopened` or `TT-Work In Progress`.
- **(b) "review comments are on me" → mine.** Seed `ARRAY['TT-In QA']` only; the 2
  tasks stay on the board, which is today's behaviour.

Not guessed in the SPEC (E11): a wrong (a) makes a task silently vanish while the
ball is in your court, which is the one failure this ticket must not create.

**Answer:**

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.

**ANSWERED (Salvador, 2026-09-10): NO — `TT-In Review` stays OUT of the set.**
His words: *"in review means ci is running on it it's not ready"*. So the status
does not mean "handed back for someone else to check"; it means the work is
still in flight and has not been delivered at all. Reading (b) was the right
one, the seeded set stays `ARRAY['TT-In QA']`, and the 2 In Review tasks
correctly remain on the board. No code change was needed — the array was seeded
with QA alone precisely because guessing here was the one failure this ticket
could not afford.
