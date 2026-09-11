> Jira: SWT-40

# inquiry-promote — open questions

**Both questions answered 2026-09-11 and folded into the SPEC, which is FINAL.** Kept as the
record of what was asked and decided. Also answered the same day, recorded in the SPEC as owner
decisions O1-O6 and not asked here: rules first, LLM after; the handsonconnect mailbox is
collaboratory or reengine; default to collaboratory; LHH tickets are checked in Jira before a task
exists; only capture stays on cron.

---

## Q1 — The Spanish channel in the Avviato Slack: is it Collaboratory work?

**ANSWERED 2026-09-11 — A, and further: it is nobody's project.** Salvador, verbatim, after seeing
the #a-millon chatter (HOC3/HOC4/LlamaSite deploys, Salesforce incidents, LHH ticket links):

> "those are HOC/LLamasite not my projects. not even ReEngine"

and, on the LHH links there:

> "the LHH tickets need a trip to jira to see if they are assigned to me. if they are then
> reengine if not ignore"

Folded into the SPEC as owner decisions O4/O5 and A-D5:
- **Destination:** #a-millon (`C1C1TSLJH`) goes to `bulk`, ignored.
- **Priority 99.** That is below rules 1-2 (the LHH rules, 100), so an LHH link there still reaches
  rule 1 and, once Part D ships, its Jira assignee check: the ticket decides, not the channel. It is
  above everything else that can match Slack (rule 10 at 90, rule 59 at 95, the catch-all at 1), so
  everything else in the channel is ignored.
- **The rule:** `thread_key_prefix` `slack:T0360B84U:C1C1TSLJH` → bulk, attribution only, in exact
  case (the prefix match is case-sensitive).
- **Routing measured before the change:** 89 collaboratory `attributed` via rule 9, and 4 reengine
  `task` via rule 1 (tasks 72, 75, 93, 94, all closed as not assigned).

(Original options, kept for the record: A = file it as ignored in `bulk`; B = it is Collaboratory,
leave it. Recommended was A.)

---

## Q2 — New question-tasks: Holding column first, or straight to Ready?

**ANSWERED 2026-09-11 — A.** Salvador, verbatim: "A is ok".

Folded into the SPEC as owner decision O7 (C-D8, C-D12, C8, C14):
- **`inquiryCreateStatus = "holding"`:** promoted question-tasks land in the Holding column
  (promotion action `review`). Salvador works the real ones there and dismisses the wrong ones.
- **The signal is `classify promote --outcomes`,** the dismissal-based readout. A "not actionable"
  or "wrong kind" dismissal counts as "that wasn't a real ask"; no labelling is ever asked for.
- **After about two weeks,** when the readout satisfies him, one line flips new ones to Ready
  (`inquiryCreateStatus = "ready"`, plus its pinned test). Tasks already created stay where they are.

(Original options, kept for the record: A = Holding for about two weeks, then Ready; B = Ready from
day one. Recommended was A.)

---

No questions remain open. The SPEC is ready for `test-author`.
