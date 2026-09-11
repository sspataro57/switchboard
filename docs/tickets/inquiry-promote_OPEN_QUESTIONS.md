> Jira: SWT-40

# inquiry-promote — open questions

Two decisions, one each. Everything else in the SPEC is decided. Already answered on 2026-09-11 and
not asked again here: rules first, LLM after; the handsonconnect mailbox is collaboratory or
reengine; default to collaboratory.

---

## Q1 — The Spanish channel in the Avviato Slack: is it Collaboratory work?

The channel is `C1C1TSLJH`, where jack, rafael and esteban talk to each other. Today the Avviato
workspace's catch-all rule files everything in it under Collaboratory. That is where the
"@esteban vamos a revisar" noise in the question-check came from.

- **A — No, it isn't Collaboratory. File it as ignored** (project `bulk`). One rule, reversible by
  disabling it. Cost: if someone in that channel ever asks you something, it won't become a task.
- **B — Yes, it's Collaboratory work. Leave it as is.** The new "is it addressed to you" check
  (DMs, and threads you're already in) already drops people talking to each other, so the noise
  mostly goes away without a rule.

**Recommended: A**, unless those three work on Collaboratory with you. If they do, pick B.

---

## Q2 — New question-tasks: Holding column first, or straight to Ready?

- **A — Holding for about two weeks, then Ready.** They show up in the Holding column, the "check
  me" pile. You work the real ones from there. Dismissing a wrong one with "not actionable" is
  automatically recorded as "that wasn't a real ask". You never get asked to label anything. When
  the dismissal count (the `--outcomes` readout) looks low to you, one line switches new ones to
  Ready.
- **B — Ready from day one.** Same dismiss button, same automatic record, but wrong ones sit in
  your Ready column until you dismiss them.

**Recommended: A.** Either way no worker console can pick these up; they are yours only. There is
no "approve" button to move a Holding task to Ready; you just work it where it is and close it.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into the SPEC.
