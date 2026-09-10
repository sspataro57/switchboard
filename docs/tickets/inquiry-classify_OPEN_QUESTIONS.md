> Jira: SWT-33

# inquiry-classify — open questions

**ALL THREE ANSWERED 2026-09-10 and folded into the SPEC, which is no longer provisional.**
Kept as the record of what was asked, what was decided, and why — the answers carry
constraints (a by-channel breakdown, an eval refusal, a labelling protocol) that the SPEC
now enforces as criteria 30, 31 and 32.

---

## Q1 — Does `collaboratory`'s Slack traffic carry `direction='outbound'` rows at all?

**Why it was blocking.** The whole "is it still open" fold (SPEC criteria 12, 17) keys on a
later `normalized_messages` row with `direction='outbound'` on the same thread. Slack
direction FAILS CLOSED per workspace: `slackweb` refuses to normalize a message whose
workspace has no `SLACK_CONNECTOR_OWN_USER_IDS` entry rather than guess
(`normalize.go:63-65, 77-80`). INSTITUTIONAL_KNOWLEDGE (SWT-12 section) records that
`T0HPR78RX` (Collaboratory/LlamaSite) has **no** entry, and that `connector-slackweb` was
SUSPENDED — yet 708 Collaboratory Slack messages are attributed and 184 arrived in the last
7 days, so something had changed and the file had not caught up.

**ANSWERED 2026-09-10 — A, MEASURED, not assumed.** Run per workspace against the live db:

| workspace | inbound | outbound | latest outbound |
|---|---|---|---|
| `T0360B84U` (Avviato) | 24,352 | 19,012 | 2026-09-04 |
| `T0HPR78RX` (Collaboratory/LlamaSite) | 2,155 | **2,393** | 2026-09-09 |

Collaboratory carries MORE outbound than inbound, nine days fresh. The fold's discriminating
column is emphatically not a production constant.

**Rationale and consequences folded into the SPEC:**

- The fold ships as specified, with the integration test that seeds the outbound row in
  Postgres and proves it bites by MUTATING it (criterion 17) — the measurement says the
  column has values, the mutation test says the query reads them.
- The per-workspace counts go in the runbook with their date (criterion 12), and neither the
  runbook nor any test may freeze a production count as a literal (SWT-19's rule: that
  corpus is live).
- **The INSTITUTIONAL_KNOWLEDGE line is STALE and this ticket corrects it in place, dated**
  (criterion 29). Not a deletion: the sentence was true when written, and the record of when
  it stopped being true is the useful artefact. Left standing it would make the next reader
  re-derive this exact doubt — which is what happened here, and cost a measurement.

---

## Q2 — First cut: every channel of an armed project, or Slack only?

**ANSWERED 2026-09-10 — A, all channels.** One knob (`ai_inquiry`), no `--channel` flag
anyone has to remember, and the labelled set is drawn from the population the lane actually
sees.

**The stated cost — "a bad number will not say which message shape broke" — is bought back,
not accepted.** Every count the inquiry lane prints (classified, flagged, open,
answered-in-thread, spoke-in-conversation-since, skipped) is broken down BY CHANNEL, in
`classify.Summarize` so the CLI and `/funnel` cannot disagree — SPEC criterion 30. That
recovers the diagnostic the flag would have given, without the flag: a bad recall number
still says whether it was Jira comments, marketing-shaped client email or Slack one-liners
that broke it.

Recorded so it is not re-litigated: `channel` is read from the STORED
`ai_extractions.fields`, never re-joined from `normalized_messages` (SWT-22 criterion 20's
rule — what was classified is what should be shown). The single deliberate exception is the
replied-since fold, which is a statement about the world NOW and must join.

---

## Q3 — Labelled-set size, and whether it blocks the merge

**ANSWERED 2026-09-10 — B, ship a 40-line starter set, guard at 40, with one hard
condition.**

Rationale: the lane is shadow-only and creates nothing, so the first real measurement is
Salvador reading flagged output on `/funnel`. Labelling against real flagged output is both
faster and less biased than labelling cold, and a merge-blocking 120 would trade an
afternoon for a number that is not the number anyone will act on first.

**THE CONDITION, enforced in the artifacts rather than trusted (SPEC criterion 31):** at
n < 120 **no precision or recall number may be presented as a result**. `classify eval`
REFUSES to print a bare ratio below the threshold — it prints counts plus an explicit
marker (`INDICATIVE ONLY — n=40 < 120, this is not a measurement`), one exported constant,
one spelling, and the same marker goes in the runbook's score-table row and in any Jira
comment. Unit tests assert (a) no `\d\.\d\d`-shaped number below the threshold, (b)
byte-identical existing output at/above it, so the two measured lanes cannot regress.
A guard beats a convention: this repo shipped a 25-29x cost error by quoting the 0.25 s warm
benchmark out of the context that produced it, and a convention would not have stopped it.

**The dated commitment** to 120 stratified (`uniform >= 80` + `enriched >= 40`) goes in the
runbook, and the structure test's minimum-40 comment names it, so raising the minimum is a
scheduled edit rather than a forgotten one.

**The labelling protocol, added on the same instruction (SPEC criterion 32) — it names who
labels and against what.** The judgement "does Salvador need to answer this" is HIS and
cannot be delegated to an agent or inferred from a heuristic. The starter 40 come from
`collaboratory` messages recent enough to confirm from memory in seconds (~14 days, mixed
across slack/gmail/jira in roughly the population's proportions), labelled with the thread
context in front of him. The remaining 80 accumulate DURING the shadow period from real
flagged output on `/funnel` **plus a uniform sample of unflagged messages from the same
window** — the uniform half is not optional, because a set built only from flagged output
can measure precision and can never measure recall.

Carried forward unchanged: the `subject_sha256` drift guard is weak on Slack, because the
slackweb normalizer sets a message's `subject` to the conversation NAME — every message in a
channel shares a hash. Do not add a second hash spelling to fix it (SPEC criterion 25).

---

Answered by the coordinator on 2026-09-10. No questions remain open; the SPEC is ready for
`test-author`.
