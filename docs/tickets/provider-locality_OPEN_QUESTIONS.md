> Jira: SWT-21 — open questions for `provider-locality_SPEC.md`

Four. Each changes a named acceptance criterion, so the SPEC is provisional until
they are answered.

**ANSWERED 2026-08-28** — Salvador asked for sensible defaults with the shaky ones
flagged. Q1 and Q4 I am confident in. **Q2 carries a deviation I proposed rather
than chose from the options offered, and it is the one to review.** Q3's shape is
right and its threshold is a guess.

---

## Q1 — `projects.ai_locality` default for NEW rows (criterion 9)

`DEFAULT 'local_only'` OR `DEFAULT 'any'`?

`local_only`: fail closed. A project created later without thinking about this
column has its messages skipped by triage and its Deliver tasks skipped by drafts
until someone sets it — visible in `triage report`'s skipped lane (criterion 18),
but visible only if read. Cost is a work stall on a real client project, which is
reversible with one UPDATE.

`any`: preserves today's behaviour exactly for everything except `personal`, and
the migration names existing rows explicitly either way. Cost is that a future
private project (a second personal account, a client under NDA) leaks until
someone remembers the column — the failure this ticket exists to prevent, one
project later.

Note the asymmetry that makes this a real choice rather than a slogan: the
fail-closed direction here does not protect the `personal` project (0016 sets it
explicitly); it only protects projects nobody has thought about yet, at the cost
of stalling projects nobody has thought about yet.

**Answer: `local_only`. Confidence: HIGH.**

The two failures are not comparable in kind. A leak is irreversible — the content
has left the building and no UPDATE brings it back. A stall is one UPDATE, and it
shows up in the skipped lane. When one direction is recoverable and the other is
not, the default belongs on the recoverable side. That is this repo's consistent
position: SWT-19 chose refusal over a guess on exactly this reasoning — refusing
is reversible, a wrong-room send is not.

The cost is also near zero right now. Triage is shadow-only and the draft worker
is undeployed, so a stalled new project stalls nothing that is running — and the
stall is loud the moment anyone reads the report, whereas a leak is silent
forever.

---

## Q2 — Where the `personal` project row and its capture rules are created (criteria 10, 11)

Migration 0016 seeds both OR migration seeds the project only and the rules go in
by `opsctl capture-rules add` per the runbook?

Precedent cuts both ways. `0015_capture_rules.sql` deliberately did NOT seed
`capture_rules` ("routing is configuration with an enabled flag and an audit
trail; seeding it would put production routing into every test database and make a
rule edit a new migration"). But `personal`'s rules are not routing preferences —
they are the boundary's input, and a rule that is missing means mail goes to
OpenAI. Seeding them makes them present in every test db, which makes criteria 11
and 12 testable without fixtures; it also makes them un-editable except by a new
migration, and makes `capture_rule_add`'s audit row absent for exactly the rules
that matter most.

Sub-question if the answer is "runbook": the sender list itself. This SPEC does
not invent addresses. It needs the actual `sender` patterns — Bank of America, the
mortgage servicer, CareCredit, the HSA, the medical biller — either from you or
from a measurement step (top senders in the unmatched corpus, reviewed by hand
before any rule is added). Which?

**Answer: migration seeds the PROJECT only; rules go in by `opsctl` from a
measured sender list. PLUS a deviation — see below. Confidence: MEDIUM on the
deviation, HIGH on the seeding.**

Seeding: follow SWT-17's precedent unchanged. Its reasoning holds — routing is
configuration with an enabled flag and an audit trail, and seeding it puts
production routing into every test database while making a rule edit a new
migration. The counter-argument ("these rules are the boundary's input, a missing
rule means mail goes to OpenAI") is real, and the deviation below removes it
rather than answering it.

Sender list: from MEASUREMENT, reviewed by hand. Not invented, and not from
memory. The unmatched corpus has already been characterised (2026-08-28) — the
top senders are known and the buckets are financial ~1,510, job alerts ~1,857,
newsletters ~1,557, dev/infra ~814, brand marketing the largest share. A rule set
built from that, eyeballed before it is added, is honest; a list I write from
imagination is not.

**THE DEVIATION, and the thing to review: an UNMATCHED message should be
RESTRICTED, not general.**

The SPEC maps unseen → restricted, unmatched → general, project → per
`ai_locality`. That mapping is what makes rule completeness load-bearing: a
personal message no rule happens to match is `unmatched`, therefore general,
therefore hosted. The boundary would then be only as good as the sender list,
which is exactly the fragility Q2 is worried about.

Treating unmatched as restricted removes the dependency entirely. And it is not
a costly choice on this corpus, for two measured reasons:

1. The unmatched pile IS the personal-heavy residue — that is what SWT-17 made it
   (33,427 of 49,420 messages now route deterministically, and what remains is
   dominated by brand marketing, financial mail, job alerts and newsletters).
   Sending it to a hosted model is the thing we are trying to stop, not a
   capability we are giving up.
2. The local model is not a downgrade. The spike (`local-classifier-spike.md`)
   measured qwen3:8b at 0.90 recall against 0.80 for the best hosted model, and
   the hosted models missed 3 of 5 HOA violation notices that it caught.

The honest cost: triage's whole inbox becomes local-only, so triage cannot run at
all until a local adapter exists. Today that costs nothing — triage last ran
2026-07-12 and is shadow-only — but it does mean this ticket gates triage on the
classifier ticket rather than the other way round. **That is the consequence
worth a decision, and it is why this is flagged rather than assumed.**

---

## Q3 — An unclassified error on the restricted lane: skip or error? (criteria 16, 18)

Skip it with reason `unclassified_error` (never exits non-zero, message retried
next pass) OR treat it as a normal per-message error (counted, contributes to the
consecutive-error abort at `maxConsecutiveErrors`, run exits non-zero)?

Timeouts and connection failures are settled — they skip; you said so and the
4B-at-low-priority makes them normal operation. This is about the residue: a local
adapter returning HTTP 400 on a malformed schema, or JSON that will not parse.
Skipping keeps the CronJob green and the operator un-desensitised, at the cost of a
genuinely broken local adapter looking like a busy box forever (visible only as a
growing `unclassified_error` count in the report). Erroring makes a broken adapter
loud, at the cost of a non-zero exit on the one lane whose failures are supposed
to be routine — the alarm-fatigue path you flagged.

Neither answer leaks. This is purely about which failure you want to be loud.

**Answer: SKIP per message, with a distinct reason — and raise on the PATTERN,
not the instance. Confidence: HIGH on the shape, LOW on the number.**

This repo has already solved this shape once. The reconcilers count completed
passes rather than wall time precisely so that "the poller did not run" and "the
poller found nothing" cannot share an alarm. The same separation applies: one
message failing to classify is not news, and a run exiting non-zero over it is
the alarm-fatigue path. An adapter that is genuinely broken is news, and it has a
different signature — it fails on everything, not on one thing.

So: `unclassified_error` never fails a run, and the pass raises ONCE when such
errors exceed a proportion of its attempts. That makes a broken adapter loud
without making a malformed message loud.

**The number is a guess and should be treated as one**: start at "more than half
the attempts in a pass, with at least 20 attempts" — the floor stops a two-message
pass from crying wolf. It wants tuning against a real failure, and the SPEC should
say so rather than presenting it as derived.

---

## Q4 — Do drafts, planimport and `task_get_next` ship in this ticket? (criteria 22, 23, 24)

All four surfaces now OR triage only, with the rest as a follow-up?

Including them is small (one column in a query, one explicit constant, one SQL
predicate) and makes the seam total: every hosted-model call site in the repo
declares a class. Against it: criteria 22 and 24 are both armed-but-inert today
(`personal` has no tasks; `personal.client` is NULL, so `getNext`'s existing
`p.client = $1` already excludes it), and this repo has an explicit landmine entry
about inert mechanisms that still look alive and about predicates whose
discriminating column is constant in production. Shipping them means shipping two
guards whose tests can only prove their own fixtures, and writing the honesty
labels that keep the next reader from trusting them.

Triage-only would leave `drafts.Run` and `planimport.Propose` taking a
`provider.Client`, which weakens criterion 21's scan to one package.

**Answer: SPLIT — ship the interface change everywhere, do NOT ship the
`task_get_next` predicate. Confidence: HIGH.**

The two halves are not the same kind of thing, and the SPEC's own honesty labels
are what make that visible.

Ship the interface change to `drafts.Run` and `planimport.Propose`: every
hosted-model call site in the repo then declares a class, and the COMPILER
enforces it. A new worker cannot forget. That is not an inert mechanism — it is a
type, checked at build time, and leaving two of three call sites unguarded is the
`callsites_test` problem from SWT-18, where the scan covered three of four
matchers and the fourth silently rotted for five weeks.

Do NOT ship criterion 24's `getNext` SQL predicate. It is inert today by the
SPEC's own admission — `personal.client` is NULL, so the existing `p.client = $1`
already excludes those tasks — which makes it a predicate whose discriminating
column is constant in production. This repo has paid for that shape three times
(SWT-18's time floor, SWT-19's room segment, and the model's constant confidence
in this week's spike). Shipping it means shipping a guard whose test can only
prove its own fixture. It belongs with the ticket that gives `personal` tasks at
all, where it will actually discriminate something.

---

Answer by editing the entries. Say "questions answered" and I'll fold them into
the SPEC.
