# demo-mode: open questions (RESOLVED 2026-09-29)

All answered by Salvador on 2026-09-29 and folded into `demo-mode_SPEC.md` (SWT-99).

## Q1: free text on visible projects that names hidden things — ANSWERED (b)

Answer: a per-task hide override. Migration 0048 adds `tasks.demo_hidden boolean NOT NULL DEFAULT
false`. `@demo.task` requires that neither the task nor any ancestor is flagged (SPEC D9). The
column is set only by psql, from the runbook's audit query; the term list is runbook data. SPEC
criteria 28–31 and mutations M13–M16 cover it.

## Q2: Claude on screen versus invariant 6 — ANSWERED (a)

Answer: show it as it is. Salvador: "they know it's ai driven, it is the point." Nothing is
relabelled. The SPEC's invariant 6 section records that the demo intentionally shows the AI
workers, and that invariant 6 governs client-facing artifacts.

## Q3: sharing the repo (NOTE, out of scope for this ticket)

If Esteban asks to see the code: docs, `.claude/INSTITUTIONAL_KNOWLEDGE.md`, `docs/tickets/`, test
fixtures, commit messages and the whole git history name Foundry, Lyle, Grady, Upwork and your
personal accounts. Demo mode does nothing about that. The options are:

- **(a) A sanitised export repo.** Take a fresh history of the current tree, drop `docs/`,
  `.claude/` and fixtures that carry names, then scrub and grep again. This is a separate ticket.
- **(b) Do not share.**

This is still open as a note. Nothing in SWT-99 depends on it.
