> Jira: SWT-23

# residue-classifier — open questions

One question. Everything else the repo settled, and those calls are recorded
under "Decisions made unilaterally" in the SPEC — argue them there, not here.

The SPEC is written for **answer A** and is provisional until this is closed.

---

## Q1 — where the deterministically-claimed bulk lands, and whether it costs a migration

**The forced move.** `capture_decisions.action` has exactly four values —
`unmatched | attributed | task | task_log` (`migrations/0015_capture_rules.sql:112-113`).
There is no "ignore" verb. So claiming a LinkedIn digest deterministically means
**attributing it to a project**, and every attributed message leaves triage's
inbox and becomes a candidate for the SWT-22 classify lane, whose filter is
`p.ai_locality = 'local_only'` (`internal/classify/store.go:65`). Left alone, the
bulk rules would push ~5,700 marketing messages into the *personal* lane — the
one whose prompt and 280 labels are tuned on bank and HOA mail, at 7.2 s each.

So the bulk project must be excluded from that lane, and there are exactly two
ways to do it.

**A — `bulk` is `ai_locality='local_only'`, and migration 0018 adds
`projects.ai_classify BOOLEAN NOT NULL DEFAULT false`.** The personal lane's
filter becomes `p.ai_locality='local_only' AND p.ai_classify`; the UPDATE in 0018
sets `ai_classify=true` on `personal` so SWT-22's behaviour is byte-identical.
Costs: one forward-only migration; one new per-project concept ("mail attributed
here gets an actionability verdict"); an edit to a filter SWT-22 pinned with an
integration test; the replacement of `TestMigration0017_IsTheOnlyOneThisTicketAdds`
(`internal/classify/structure_test.go:741`); and every project-creating test
fixture now defaults to `ai_classify=false` — the same trap 0016 sprang on 23
fixtures with `ai_locality`, so a suite that exercises the personal lane will
start *skipping* rather than failing if it forgets the column.

**B — `bulk` is `ai_locality='any'`, and nothing else changes.** The SWT-22
filter already excludes it for free, there is no migration beyond the `INSERT`
that creates the project, and criteria 8/25/27 of the SPEC shrink. Cost: it
declares LinkedIn/Indeed/Nextdoor/Amazon marketing mail **hosted-model-eligible**.
Nothing would actually send it anywhere today — triage's inbox excludes
attributed messages, drafts works from tasks and no task exists — which is
precisely the objection: an inert downgrade is this repo's most-repeated landmine
shape, and `migrations/0016_provider_locality.sql:30-34` chose the fail-closed
side on exactly this trade ("a leak is irreversible, a stall is one UPDATE").
The counter-argument is equally real: these are bulk commercial senders, matched
by exact domain, and calling them restricted personal content is a fiction the
boundary does not need.

**Which is it — A (local_only + the `ai_classify` column) or B (`any`, no
migration)?**

Answer: **A** (2026-08-31, decided in-session under the autonomous-run mandate;
Salvador can overrule — flipping later is one UPDATE plus dropping a column
default, whereas B's loosening is the irreversible direction).

The repo already settled this trade, in this exact shape:
`migrations/0016_provider_locality.sql:30-34` chose fail-closed over convenient
("a leak is irreversible, a stall is one UPDATE"), and SWT-21's whole design is
that locality is enforced, not configured. B is an inert downgrade — the
most-repeated landmine shape in this codebase — and "bulk commercial senders
are not restricted content" is an argument for *changing the boundary
deliberately later*, not for defaulting through it. The fixture-churn cost of A
is real and is exactly what the 0016 precedent already taught the suite to
absorb.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
