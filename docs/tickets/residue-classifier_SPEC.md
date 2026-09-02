> Jira: SWT-23
> Jira description is SPLIT: this SPEC exceeds Jira's 32,767-character
> description cap. The issue carries the front sections up to an h2 boundary
> that fits; the remainder goes in as one or more comments on the issue. A
> re-sync must split at h2 boundaries again, not truncate. This file is the
> authoritative copy.

# residue-classifier — what the unmatched pile is for, what a rule can claim, and what a model should ever see

**Status: FINAL** (Q1 answered **A** on 2026-08-31 — `bulk` is `local_only` and
migration 0018 adds `projects.ai_classify`, fail-closed; decided in-session
under the autonomous-run mandate on 0016's own precedent, Salvador may
overrule. See `docs/tickets/residue-classifier_OPEN_QUESTIONS.md` for the
recorded reasoning.)

## Source

Not a build-order step. Ad-hoc, the second half of the "both" answer to SWT-22's
Q1 (`docs/tickets/local-classifier_OPEN_QUESTIONS.md`), from the SWT-23 issue:

> SWT-22 classifies messages attributed to `personal`; this ticket runs over the
> `unmatched` residue that is triage's actual inbox. THE WIRING IS NOT THE WORK —
> SWT-22 delivered the ollama adapter, local lane, boundary proof and eval
> harness (and SWT-25 since added link candidates); running a local model over
> the residue is a filter change and a config change. THE WORK IS THE PROMPT, and
> it is a real question: triage's existing prompt asks client-work questions; the
> residue is LinkedIn, Indeed, GitHub notifications, Medium, Humble Bundle,
> Nextdoor, brand marketing. This ticket owns: (a) what the residue is actually
> FOR — plausibly a keep/discard signal or a small number of categories that earn
> a queue, decided before any prompt; (b) its own labelled set drawn from the
> residue (SWT-22's personal set does not transfer, nor do the spike's numbers);
> (c) whether the residue is worth ~60 min GPU per pass at all, or whether more
> capture rules are the cheaper answer for most of it — "most of pile A should be
> claimed deterministically and the classifier should see what is left" is an
> answer this ticket must be allowed to reach.

Predecessors: `docs/tickets/local-classifier_SPEC.md` (SWT-22, delivered),
`docs/tickets/link-preservation_SPEC.md` (SWT-25, delivered),
`docs/tickets/provider-locality_SPEC.md` (SWT-21, delivered),
`docs/tickets/capture-rules_SPEC.md` (SWT-17, shadow).

## The three answers this SPEC gives, up front

Because they are the ticket, and everything below is their consequence.

**(c) first, because it settles the other two. The residue is NOT a ~60-minute
pass. It is a ~29-hour pass, and the estimate that said otherwise was a warm
micro-benchmark of a ten-word prompt, not a measurement of this classifier.**
`docs/runbooks/local-classifier.md:176-179` records the classifier's own measured
median latency through the real prompt path: **6.3 s** (SWT-22, 2026-08-30) and
**7.2 s** (SWT-25, 2026-08-31, with candidate lists). 14,737 × 7.2 s = 106,106 s
= **29.5 GPU-hours**; at the 6.3 s baseline, 25.8 h. The 60-minute figure is
14,737 × 0.25 s — the SWT-22 warm-call benchmark, which measured a subject line
with `num_predict` in the low hundreds, not a 4,000-character body plus a
numbered link list producing a `title` and a `reason`. The two numbers differ by
**25-29×** and only one of them was measured on this workload.

**(c), continued: capture rules are worth writing and they do NOT make the
classifier unnecessary.** The supplied domain census says the top ~27 sender
domains cover 6,412 of 14,737 (43%), and of those, six domains — `github.com`
(455), `sspataro.com` (518), `google.com` (239), `upwork.com` (106),
`browardschools.com` (293), `rocketmoney.com` (174) — are work-shaped or
mixed-actionability and must NOT be claimed as noise. What is safely claimable
from the supplied list is ≈5,700 messages, **≈39%**. **61% of the residue is a
long tail no rule reaches**, which is the structural difference from the
`personal` pile and the reason "most of pile A should be claimed
deterministically" cannot be delivered as written. What makes the classifier
affordable is not the rules — it is running **forward-only**, over new arrivals
rather than a five-year backlog. A 2021 LinkedIn notification has no action left
in it; paying 7 s of GPU to be told so is the definition of the wrong question.

**(a) What the residue is FOR: the same question as SWT-22 — actionability —
asked with a different prompt, not a new taxonomy.** `VerdictSchema`
(`internal/classify/prompt.go:39-53`) is reused **verbatim**: `actionable`,
`kind` (the five-member enum), `title`, `reason`, `link_index`. Reasons, in
order of weight:

- **A category earns a queue only if something filters on it, and nothing does.**
  CLAUDE.md invariant 2: "queues are filters, not tables". `classify` creates no
  tasks and will not in this ticket, so a six-member residue taxonomy would be
  six labels with no consumer, no test that can fail, and no way to be wrong.
- **Reusing the contract is the only way to get a comparable number.** The
  residue's recall/precision can be read directly against the personal lane's
  0.94 / 0.50 (`docs/runbooks/local-classifier.md:179`). A different schema makes
  the two lanes two experiments.
- **The deterministic half of "what category is this" is already better answered
  by SQL.** Which sender, which channel, which domain, whether the sender even
  has an address — all of it is a `GROUP BY`, and this ticket ships it as one
  (criteria 1-4). A model asked to sort mail into `job_alert | social |
  newsletter | retail` would be re-deriving, unreliably and at 7 s each, a fact
  the From header states exactly.
- **Keep/discard IS actionability, with the polarity flipped.** `actionable:
  false` is the discard signal. There is nothing a separate boolean would add.

**(b) The labelled set is drawn fresh from the residue, STRATIFIED, and the
strata are recorded in the file** — because a uniform sample of a pile whose
actionable base rate is a couple of percent yields four or five positives, and a
recall computed on four positives is a coin flip printed to two decimal places.
Criteria 17-20.

## Premises, verified against this repo before writing

Line references are against `ticket-residue-classifier` at the time of writing.
**Every production count below came from the main session's measurements of
2026-08-31 and is quoted, not re-derived** — this session had no psql. Re-measure
at verification time; the corpus is live and a frozen literal cries wolf every
day a message arrives (SWT-19's lesson). Criteria 1-4 exist precisely so that
re-measuring is a command rather than an afternoon.

1. **The residue is defined by one clause and it already has a reader.**
   `internal/triage/store.go:63-70` — latest `capture_decisions` row with
   `action='unmatched'`. `internal/capture/rules_store.go` filters
   `direction='inbound'` (that line is invariant 5), so an outbound message can
   never carry a decision of any kind, in any mode.
2. **`unmatched` has no project, structurally.** `migrations/0015_capture_rules.sql:126-127`,
   `capture_decisions_unmatched_has_no_project`, `CHECK ((action = 'unmatched') =
   (project_id IS NULL))`. Consequence for this ticket: the residue inbox
   **cannot** join `projects`, and every query the SWT-22 lane uses joins it
   (`internal/classify/store.go:62`, `:107`).
3. **So SWT-22's inbox and SWT-22's eval loader both return exactly zero residue
   messages today, and neither errors.** `inboxWhere` (`store.go:56-69`) has
   `JOIN projects p ON p.id = latest.project_id` plus `latest.action =
   'attributed'`; `MessagesByID` (`store.go:97-112`) has the same join plus
   `p.ai_locality = 'local_only'`. A residue label file handed to
   `classify eval` today prints `n=0 scored` and `recall n/a`. That is a silent
   empty set, not a failure — criterion 16 makes it loud.
4. **The boundary already covers the residue and needs no change.**
   `provider.ClassOf` (`internal/provider/locality.go:195-203`): anything that is
   not `AttrProject` is `ClassRestricted`. `AttrUnmatched` is therefore restricted
   **by the same clause SWT-21 chose deliberately** so that rule completeness is
   never load-bearing for a security property (locality.go:186-194). The residue
   lane inherits this with zero new code; the fold over inbound neighbours
   (`store.go:167-199`, `MostRestrictive`) can only make it more restrictive,
   never less.
5. **A rule is written at domain granularity; the existing report is written at
   full-`From` granularity.** `capture.KindSender` is a case-insensitive
   **substring** of the raw From header (`internal/capture/rules.go:205-206`),
   so `linkedin.com` is a legitimate one-line rule — but
   `reportUnmatchedSenders` (`internal/capture/rulesreport.go:257-286`) groups by
   `COALESCE(NULLIF(m.sender,''),'(none)')`, the whole `Name <addr>` string, and
   caps at `reportListLimit = 20` (rulesreport.go:24) with no coverage
   percentage. `LinkedIn <messages-noreply@…>` and `LinkedIn Job Alerts
   <jobalerts-noreply@…>` are two rows. That is why the supplied census had to be
   hand-written SQL, and why criteria 1-4 put it in the report.
6. **A sender with no `@` is not gmail, and the connector code says which
   connector it is.** google writes the raw From header
   (`internal/connector/google/rfc822.go:134`, `normalize.go:121`), which always
   carries an address. `internal/connector/slackweb/normalize.go:88` writes
   `Sender: message.Author` — a **display name**. `internal/connector/upworkcrm/normalize.go:148`
   writes the CRM's `sender` column, also a name. So the supplied bare-name rows
   (gil vazquez 271, mario cruz 243, lyle deitch 168, erica rapa 147, pau garcia
   132 — 961 messages across five names) are **Slack or Upwork, i.e. WORK
   conversations sitting unmatched**. This is a code-established fact, not a
   hypothesis; criterion 3 measures which of the two and how many.
7. **No non-test code creates a project.** `migrations/0016_provider_locality.sql:66-68`
   creates `personal` in a migration and explicitly refuses to seed capture rules
   there ("routing is configuration with an enabled flag and an audit trail").
   Both halves of that precedent bind this ticket: the bulk project is a
   migration, the bulk rules are `opsctl capture-rules add`.
8. **`capture_decisions.action` has exactly four values** —
   `unmatched | attributed | task | task_log` (0015:112-113). There is no
   "ignore" verb. Claiming a message deterministically means **attributing it to
   a project**, and nothing else.
9. **An attribution-only rule can never create a task, in any mode.**
   `external_system` NULL means "attribution only, no task" — enforced in the
   engine's driver and printed as such by `opsctl capture-rules list`
   (`cmd/opsctl/main.go:456-457`). Every rule this ticket adds is
   attribution-only.
10. **The loop is already shared and must stay shared.** `classifyAll`
    (`internal/classify/classify.go:177`) is called by both `Run` (`:167-173`)
    and `Eval` (`eval.go:120-143`), and `renderUser` (`classify.go:445-469`) by
    both — SWT-25 premise 6 names that sharing as what makes an eval a real delta
    measurement rather than a different experiment. A second worker package would
    fork it.
11. **`worker_type` is what keeps two workers off each other's inbox.**
    `inboxWhere`'s `NOT EXISTS` keys on `worker_type='classify'`
    (store.go:66-69), triage's on `'triage'` (`triage/store.go:59-62`).
12. **Migration state.** `migrations/` runs 0001…0017, highest
    `0017_normalized_message_links.sql`. This ticket's is **0018**.
    `internal/classify/structure_test.go:741`
    `TestMigration0017_IsTheOnlyOneThisTicketAdds` fails on any file above 0017 —
    correct for SWT-25, wrong for this one, and it must be REPLACED, not deleted
    (criterion 25; the pattern is SWT-25's own criterion 26).
13. **The labels-file guard is hard-coded to one path and one key set.**
    `structure_test.go:589-651` reads `docs/evals/personal-actionability.jsonl`
    and rejects any key outside `{message_id, label, subject_sha256, note}`. A
    second labelled set needs that test generalised, not copied.
14. **`normalized_messages.links` is populated for the whole google corpus.**
    SWT-25's `--normalize-only --all` backfill ran; upwork/jira/slackweb rows
    keep `[]`. The residue is mostly HTML marketing mail, so it is where the
    candidate list is most often non-empty — and where the drop lists
    (`internal/connector/google/links.go`) do the most work.
15. **Nothing is deployed.** Institutional knowledge: orchestrator, triage,
    drafts, fleetd, hooksd are not in the cluster; `classify` has only ever run
    by hand from the workstation, against ollama as a systemd **user** service.

## Goal

Decide what the unmatched residue is for, shrink it deterministically where a
rule can honestly claim it, and give what remains a forward-only local
actionability verdict on the same contract as the personal lane — with a
stratified labelled set that makes the number honest and a census that makes the
next rule obvious.

**Usable alone.** After this ticket, with no cluster change and no new hardware:

1. `opsctl capture-rules report` answers "what rule should exist next" at the
   granularity a rule is actually written at — sender **domain**, with cumulative
   coverage — instead of at full-`From` granularity capped at 20 rows. That is
   useful on its own even if nothing else in this ticket shipped.
2. The three suspicious segments are named rather than suspected: how many
   residue messages have no email address at all and on which channel, what
   `sspataro.com` actually is, what `upwork.com` actually is. Each is a number in
   a report, reproducible on demand.
3. Triage's inbox is measurably smaller by a recorded percentage, and every
   message removed from it was removed by a rule that cannot create a task in any
   mode.
4. `classify run --lane residue --since 720h` produces shadow verdicts on real
   residue mail, and `classify report --lane residue` reads them, with the same
   skip-vs-verdict distinction SWT-22 established.
5. `classify eval --lane residue` prints a recall, a precision computed on the
   **uniform stratum only**, and the measured actionable base rate of the
   residue — the three numbers that decide whether this lane is worth running at
   all.

## Acceptance criteria

Numbered and testable. 1-4 and 21-25 are unit or structural; 5-16 split between
unit and integration as marked; 17-20 are data plus their structural guards.

### The census — because a rule you cannot measure is a rule you cannot defend

1. `internal/capture/rulesreport.go` gains **`reportUnmatchedSenderDomains`**,
   registered in the `Report` section list (rulesreport.go:66-73) **before**
   `reportUnmatchedSenders`, which stays. It prints, for the unmatched pile only:
   sender domain, message count, share of the residue, and **cumulative share**,
   top 40. The cumulative column is the point — "the top 20 cover 43%" is the one
   number that decides whether rules or a classifier is the cheaper answer, and
   today it is not printable.
2. **The domain is parsed in GO, never in SQL.** The query returns whole sender
   strings; the fold uses `net/mail.ParseAddress` with a lower-cased host, and
   falls back to the raw string only when parsing fails. A unit table test covers
   `Name <a@b.com>`, `a@b.com`, `"Last, First" <a@b.com>`, an RFC-2047 display
   name, and a bare display name. **Failure message must name the reason**:
   `split_part(sender,'@',2)` on `Name <a@b.com>` yields `b.com>` — a domain that
   matches no rule, silently — and this repo's standing rule is that a format Go
   owns is never taken apart in SQL (`internal/capture/rulesreport.go:288-295`
   says it for thread keys; the same applies here).
3. **`reportUnmatchedChannels`**: the residue broken down by
   `normalized_messages.channel`, and within that, the count of senders carrying
   **no `@` at all**. That single line is the work-vs-noise discriminator
   established in premise 6: a bare display name means slack or upwork, never
   gmail. Its failure message carries the connector line references, so the next
   reader does not re-derive it.
4. **`reportUnmatchedDomainDetail --domain <d>`** (an `opsctl capture-rules
   report` flag): for one domain, the top 20 **full** sender addresses with
   counts and the newest 10 subjects. This is the tool that answers
   "what IS `sspataro.com`" — 518 inbound messages from Salvador's own domain,
   where `developer@sspataro.com` is an ingested account and the direction rule
   marks a message outbound only when From is one of the five own account
   addresses. Anything left inbound is a *different* local part, and the SPEC
   deliberately does not guess which; it ships the query.
   Same for `upwork.com` (106) and for `github.com` (455).

   **These three are Phase-0 blockers**: the rule set of criterion 5 may not be
   written until this report has been run and its output recorded in
   `docs/runbooks/capture-rules.md`.

### Phase 1 — the deterministic claim

5. **Every rule this ticket adds is `--external-system`-less (attribution only)
   and points at ONE project.** A structural check at review time plus the
   runbook's recorded `opsctl capture-rules list` output: no rule added by this
   ticket has an `external_system`, so none of them can create a task in shadow
   or in live mode (premise 9). This is the property that makes it safe to add a
   dozen routing rules while capture is still in shadow and before its go-live
   checklist has been worked.
6. **The claim gate, applied per domain before its rule is written**: a domain is
   claimed only when (a) the census shows ≥ 100 residue messages, AND (b) a
   hand-checked sample of ≥ 20 of its messages contains **zero** actionable ones,
   AND (c) it is not on the refusal list of criterion 7. The sample rows join the
   labelled set of criterion 17 (`stratum: "domain_gate"`), so the gate and the
   eval are the same work done once.
7. **The refusal list is part of the deliverable and is written into the
   runbook.** These domains are NOT claimed by this ticket, each with its
   recorded reason:
   - `github.com` (455) — work. Two `thread_key_contains` rules already claim
     `collaboratory-www` and `gonoble` through GitHub's Message-ID
     (`docs/runbooks/capture-rules.md:79-96`); what is left unmatched is
     notifications for *other* repos, and sweeping them into a noise project
     would take them out of triage's inbox without giving them a task.
   - `sspataro.com` (518), `upwork.com` (106) — unknown until criterion 4 runs.
   - `google.com` (239) — mixed: security alerts are the most actionable mail in
     the corpus and marketing is the least, under one domain.
   - `browardschools.com` (293), `rocketmoney.com` (174) — school deadlines and
     financial notices; both are exactly what the classifier exists to catch.
     `rocketmoney` is a candidate for a **`personal`** rule (SWT-22's lane), not
     a bulk one, and this ticket may add it there.
   - Any domain whose 20-message sample contains one actionable message.
8. **The bulk project.** Under answer A (see the open question) the rules point
   at a new project, slug **`bulk`**, `ai_locality='local_only'`,
   `ai_classify=false`, created by migration 0018 (data model, below).
   `ai_locality` is `local_only` because 0016's reasoning applies unchanged: a
   rule that is one character too wide would otherwise downgrade real personal
   mail to hosted-eligible, and "a leak is irreversible, a stall is one UPDATE"
   (0016:30-34).
9. **Coverage is recorded as a measured before/after, not as a plan.** The
   runbook records: residue size before, residue size after the rules are seeded
   and one shadow pass has run, the per-rule match counts from
   `opsctl capture-rules report`, and the resulting percentage. A rule that
   matched **zero** messages is disabled and recorded, not left in place — the
   capture runbook's own go-live rule ("a rule that has never matched is not
   ready — it is untested", capture-rules.md:150-151).

### Phase 2 — the residue lane

10. **`internal/classify/lane.go` introduces `Lane`, and there are exactly two.**
    ```go
    type Lane struct {
        Name          string // "personal" | "residue"
        WorkerType    string // ai_runs.worker_type
        System        string // the system prompt
        PromptVersion string
        LabelsPath    string // default eval fixture
    }
    var LanePersonal = Lane{Name: "personal", WorkerType: "classify", System: SystemPrompt, PromptVersion: PromptVersion, ...}
    var LaneResidue  = Lane{Name: "residue",  WorkerType: "classify_residue", System: ResidueSystemPrompt, PromptVersion: ResiduePromptVersion, ...}
    ```
    `Config` gains `Lane Lane`; the zero value is refused rather than defaulted,
    so a caller that forgets it gets an error instead of the personal prompt over
    the residue.
    **`VerdictSchema` and `SchemaName` are NOT lane-scoped** — one contract, both
    lanes (the argument is in "The three answers", (a)).
    `TestSchema_MatchesTheOutputContract` (structure_test.go:200) is untouched,
    and that is the assertion that this ticket did not fork the contract.
11. **`worker_type='classify_residue'` is its own value, and the reason is a
    real defect it prevents, not tidiness.** The two lanes' `NOT EXISTS` clauses
    key on `worker_type`, so sharing one value would mean: a residue message
    classified today, then claimed by a capture rule tomorrow into a
    `local_only`, `ai_classify=true` project, would be **permanently invisible to
    the personal lane** — already "classified", by a different prompt, with a
    verdict nobody would ever look for. An integration test seeds exactly that
    sequence and asserts the message is pending for the personal lane after the
    residue lane has classified it.
12. **The residue inbox filter** (`inboxWhereResidue`, beside the existing
    `inboxWhere`, both under the ONE shared `inboxSelect`):
    ```
      FROM normalized_messages nm
      JOIN LATERAL (SELECT cd.action, cd.project_id
                      FROM capture_decisions cd
                     WHERE cd.message_id = nm.id
                     ORDER BY cd.id DESC LIMIT 1) latest ON true
      LEFT JOIN projects p ON p.id = latest.project_id
     WHERE nm.direction = 'inbound'
       AND latest.action = 'unmatched'
       AND NOT EXISTS (... worker_type = 'classify_residue' ...)
    ```
    `LEFT JOIN` and NULL-safe projections in `inboxSelect`
    (`COALESCE(p.id,0)`, `COALESCE(p.slug,'')`,
    `COALESCE(p.ai_locality = 'local_only', false)`), because premise 2 says an
    unmatched row has no project to join to. **`inboxSelect` stays ONE constant**
    — SWT-25 criterion 15's property, that `run` and `eval` cannot read different
    columns, is preserved by construction.
13. **The three-state discipline restated for this lane**, integration-tested
    with Postgres producing the values, all four shapes in one suite:
    - no decision row (unseen) → **not ours**; the engine has not looked, and
      treating unseen as unmatched hands every fresh message to the model before
      the rules run (capture-rules.md:8-18);
    - latest `unmatched` → **ours**;
    - latest `attributed` → **never ours** (either lane's business, not this
      filter's);
    - latest `task` / `task_log` → **never ours**.
    **Mutation that must turn it red:** drop `AND latest.action = 'unmatched'`
    and the `attributed` fixture must be returned. If it stays green the test is
    proving its own fixture.
14. **`Attribution` is set to `attrUnmatched` for this lane**, not to
    `attrProject` as `scanMessages` does today (store.go:139). Consequences,
    both asserted: `classOf` yields `ClassRestricted` through the
    `state != AttrProject` branch (premise 4), and `classReasonOf`
    (classify.go:383-395) files skips under `unmatched` rather than
    `project_local_only`, so a residue skip and a personal skip are
    distinguishable in the report.
15. **The honesty label is re-stated, not inherited.** The package comment
    (classify.go:1-28) says every message this worker sees is `ClassRestricted`
    by construction *because the inbox filter selects local_only projects*. That
    sentence becomes false for the residue lane while the conclusion stays true,
    and a comment that states the opposite of its code is this repo's named
    landmine. The comment must say: the personal lane is restricted because of
    the project's `ai_locality`; the residue lane is restricted because
    `ClassOf` maps every non-`AttrProject` state to restricted
    (locality.go:195-198), which is SWT-21's deliberate choice so that rule
    completeness is never load-bearing for containment. Two reasons, one
    outcome, and neither is the class fold acting as a guard.
    A structural test (extending `TestPackage_CarriesTheHonestyLabel`,
    structure_test.go:489) asserts both sentences are present.
16. **`--since` is REQUIRED for the residue lane and an unbounded pass is
    refused**, with the arithmetic in the refusal message: "14,737 messages ×
    7.2 s measured median = ~29 GPU-hours; pass --since, or pass --since 87600h
    deliberately if a full historical sweep is what you want". Not a silent
    default: a default would be a 29-hour job started by a typo. `--limit` is
    unaffected. A unit test asserts the refusal and that it names both the count
    and the latency.
    The personal lane keeps today's behaviour (`--since` optional) — its
    population is 1,600 and bounded.
17. **`MessagesByID` for the residue lane loads by id with NO action and NO
    project predicate**, and `Eval` prints how many scored labels are **no longer
    unmatched** ("a rule has claimed them since"). Rationale, in the doc comment:
    the whole point of Phase 1 is to move messages out of the residue, so a
    loader that required `unmatched` would make the labelled set silently shrink
    every time a rule is added — which is label drift by another mechanism, and
    the existing drift report (eval.go:95-105) exists because a score computed
    over a set that quietly lost rows is a score nobody can reproduce. Loading
    without the predicate is safe: `Eval` already refuses any lane but the local
    one (eval.go:55-60), so a message that has since become client work is still
    only ever sent to the local model.
    An integration test: label a message, attribute it by rule, re-run — the
    message is still scored and the "no longer unmatched" line names it.

### The labelled set

18. **`docs/evals/residue-actionability.jsonl`**, same record shape as the
    personal file plus ONE key:
    `{"message_id": N, "label": "actionable"|"not", "subject_sha256": "<16 hex>",
    "stratum": "uniform"|"enriched"|"domain_gate", "note": "<optional>"}`.
    No message content: not a subject, not a body, not a sender. The file is
    committed and the whole reason this classifier is local is that this mail
    must not be copied around.
19. **The strata, their sizes, and what each one is FOR** — recorded in the
    runbook and enforced by the structural test:
    - **`uniform` ≥ 200**: a uniform random sample of the residue *after* the
      Phase-1 rules. It is the only stratum from which a **base rate** and an
      honest **precision** can be computed.
    - **`enriched` ≥ 100**: a targeted draw for actionable-shaped mail (subject
      sweeps for due/overdue/payment/appointment/verify/deadline, plus every
      message from a sender the census flags as mixed). It is the **recall
      denominator**; without it a uniform 200 at a ~2% base rate yields four
      positives and a recall that is noise.
    - **`domain_gate`**: the ≥20-per-domain samples of criterion 6. Recorded so
      the gate is auditable and so the same reading is not done twice.
    Total ≥ 300, hand-checked. **Regex-generated labels are refused**, with the
    reason in the runbook: the SWT-22 spike's first eval scored every model
    0.10-0.27 recall because the *fixture* was wrong and the models were right.
20. **`Eval` prints the strata apart, and says what each number is worth.**
    ```
    recall     (all labels, n=…)          — actionable-shaped mail is over-represented
    precision  (uniform stratum only, n=…) — the only precision that describes production
    base rate  (uniform stratum, k of n)   — how much of the residue is actionable at all
    ```
    Plus, unchanged, every false negative by message id. A unit test asserts that
    a label set carrying strata produces the three-line breakdown and that a set
    carrying none (the personal file) produces today's output byte-for-byte —
    backwards compatibility is what keeps this from being a fork of the harness.
    **The base rate line is the number that decides the lane's future**: if the
    residue is 0.5% actionable after the rules, the honest conclusion is that the
    lane should run daily on new mail and never on history, and the runbook must
    say so with the measured figure rather than as a hedge.

### The prompt — the actual work

21. **`ResidueSystemPrompt` in `internal/classify/lane.go`, one prompt for every
    sender**, and the existing structural guard
    `TestPackage_HasNoPerSenderBranch` (structure_test.go:435) covers it with no
    change: per-sender prompts are rules in a costume — unbounded maintenance,
    untestable in aggregate, unattributable when they misfire.
22. **What it must NOT say, asserted by test**: it must not open "You classify
    one personal (non-work) email" (`prompt.go:74`). The residue is *unclassified
    by definition* — premise 6 establishes that ~961 of its messages are Slack or
    Upwork work conversations — and telling the model the mail is personal is
    telling it something this SPEC has just measured to be false. It must also
    not mention a project, a client, an open-task list or `attach_to_task_id`:
    that is triage's prompt (`internal/triage/prompt.go:19-60`) and asking those
    questions of a Nextdoor digest is the mistake this ticket exists to avoid.
23. **What it must say, each clause justified by the census**, and each asserted
    by a structural test in the shape of
    `TestPrompt_StatesTheObjectiveAndTheAttachmentCeiling` (structure_test.go:373):
    - **The objective, unchanged from the personal lane**: recall first, a missed
      payment or fine notice is a late fee, a false alarm costs a second to
      dismiss, and when genuinely torn answer true.
    - **Actionable**: a payment/bill due or a balance to pay; a fine, violation or
      compliance deadline; an appointment to confirm, reschedule or attend; a
      document, form or signature to return; **an account-security action the
      recipient must take (verify a sign-in, reset a password, confirm a
      device)** — new for this lane, because `google.com` (239) is on the refusal
      list precisely for carrying these; **a named human asking for something**,
      as distinct from a template.
    - **Not actionable, and this is the half the residue needs**, named
      explicitly because the census says these ARE the residue: job alerts and
      recommended-jobs digests (indeed 680, ziprecruiter 320, fastweb 157,
      glassdoor 142, monster 109); professional- and social-network notifications
      and digests (linkedin 695, nextdoor 726, pinterest 296, facebookmail 156,
      instagram 144); repository and service notifications with no request
      addressed to the recipient (github 455, statuspage 231); newsletters,
      publications and reading digests (medium 382, nytimes 192, washingtonpost
      177); retail marketing, offers, sales and wishlist mail (amazon 407,
      humblebundle 356, motorola 122, cinemark 119, ezcontacts 101); shipping and
      delivery *previews* with nothing to do (usps informeddelivery 146).
    - **The trap clause, stated as its own paragraph**: marketing mail is written
      to look urgent. "Ends tonight", "final notice", "act now", "your account
      needs attention", a countdown — none of these is a consequence. Actionable
      means the recipient loses something real by not acting, not that the sender
      wants a click. **This is the residue's equivalent of the personal lane's
      near-miss clause** (`prompt.go:63-67`, whose removal was measured to flip
      "your statement is available" on 883 messages), and it must not be trimmed
      for brevity.
    - **Bilingual**, same as the personal lane: English and Spanish treated
      identically, originals never translated.
    - **The link paragraph, verbatim from the personal prompt**
      (`prompt.go:103-107`): the numbered list is the complete set; answer with
      the number a person would open to act; `null` when none of them is that or
      no list is shown; never invent a number. One spelling — a structural test
      asserts the same sentences appear in both prompts, so a future edit to one
      cannot silently change the link contract of the other.
24. **`ResiduePromptVersion = "residue-v1"`**, stamped into every
    `ai_runs.input` through the existing `runInput` (classify.go:473-484) by
    reading `cfg.Lane.PromptVersion`. Without it, two runs that disagree are
    indistinguishable from a model that drifted.

### Docs, guards and the statements this change makes false

25. **`TestMigration0017_IsTheOnlyOneThisTicketAdds` is REPLACED, not deleted**
    (structure_test.go:741), by a test pinning 0018 as the highest and requiring
    `migrations/0018_*.sql` to add `projects.ai_classify` and insert the `bulk`
    project. A guard that becomes wrong and gets deleted is how the next ticket
    adds a migration nobody notices — and the runner keys on
    `schema_migrations.version` with no checksum, so a stray file is skipped
    silently.
26. **`TestLabelsFile_IsIdsAndLabelsOnly` is generalised to a table over both
    files** (structure_test.go:589): shared assertions (no content keys, unique
    positive ids, valid label, 16-hex subject hash, no unexpected keys), per-file
    minimum counts, and `stratum` permitted **only** in the residue file where it
    is **required** on every line and must be one of the three values.
27. **The statements this change makes false are fixed in the SAME change**:
    - `internal/classify/classify.go:1-28` — the honesty label (criterion 15).
    - `internal/classify/store.go:21-55` — `inboxWhere`'s comment says
      `p.ai_locality` is "THE DISCRIMINATOR, and the only column here with two
      values in production". With `ai_classify` added it is one of two.
    - `docs/tickets/local-classifier_SPEC.md` criterion 11 — amended in place
      with a dated pointer to SWT-23, exactly as SWT-25 amended criterion 18.
      The SWT-22 Jira description is re-synced afterwards (it carries the SPEC).
    - `docs/runbooks/local-classifier.md:1-6` — "reads personal mail" becomes two
      lanes.
28. **`docs/runbooks/local-classifier.md` gains a Residue lane section**: the two
    lanes and what each inbox is; that `--since` is required and why (the
    arithmetic); the measured base rate, recall and precision with their date;
    what the strata mean and why precision is quoted from the uniform one only;
    and the standing rule that a residue verdict cannot become a task while the
    message has no project (Future work). `TestRunbook_LocalClassifier`
    (structure_test.go:659) gains assertions for `--lane residue`, `classify_residue`
    and the required-`--since` rule.
29. **`docs/runbooks/capture-rules.md` gains a "Reading the residue" section**:
    the domain census command, the claim gate of criterion 6, the refusal list of
    criterion 7 with reasons, the exact `opsctl capture-rules add` lines for
    every rule this ticket seeded, and the recorded before/after coverage. A
    structural test asserts the refusal list names `github.com` and states that
    an attribution-only rule creates no task in any mode.
30. **`.claude/INSTITUTIONAL_KNOWLEDGE.md`** gains a short **Residue lane
    (SWT-23)** entry: the two lanes and their `worker_type` values and why they
    are different; that the residue inbox cannot join `projects` (0015's CHECK);
    that a bare-name sender means slack/upwork, never gmail, with the connector
    line references; the measured 7.2 s/message figure and that the 0.25 s
    warm-benchmark must never be used to size a pass again; and the
    `ai_classify` flag's meaning.

## Data model changes

**One migration: `migrations/0018_bulk_project_and_classify_flag.sql`.** Under
answer A of the open question; under answer B this section is deleted and
criteria 8, 25 and 27 shrink accordingly.

```sql
-- STATEMENT ORDER IS LOAD-BEARING (0016's lesson, restated).
--   1. ALTER  — fail-closed default
--   2. UPDATE — preserve SWT-22's lane exactly
--   3. INSERT — the bulk project, explicitly excluded
ALTER TABLE projects
  ADD COLUMN ai_classify BOOLEAN NOT NULL DEFAULT false;

UPDATE projects SET ai_classify = true WHERE slug = 'personal';

INSERT INTO projects (name, slug, client, execution, delivery, ai_locality, ai_classify)
VALUES ('Bulk mail', 'bulk', NULL, 'manual', 'dashboard', 'local_only', false)
ON CONFLICT (slug) DO NOTHING;
```

- **What `ai_classify` means, and it must be spelled out in the migration
  comment**: "mail attributed to this project gets an actionability verdict from
  the personal lane". It is a workload flag, not a boundary flag.
  `ai_locality` remains the boundary. The personal lane's filter therefore keeps
  **both** clauses — `p.ai_locality = 'local_only' AND p.ai_classify` — because
  they answer different questions, and collapsing them would make the boundary
  depend on a workload decision.
- **`DEFAULT false` is the fail-closed side.** Not classifying is a stall
  (recoverable with one UPDATE, and visible as an empty lane); classifying by
  accident costs GPU-hours and pollutes a report. Same asymmetry 0016 used for
  `ai_locality`, opposite polarity, same reasoning.
- **`client` is NULL on `bulk`**, and it is load-bearing for the same reason it
  is on `personal` (0016:57-62): `task_get_next`'s `p.client = $1` is what
  excludes these projects from every worker queue, and giving the row a client
  name would silently make that guard necessary after all.
- **No index.** Every read reaches `projects` by primary key already, and the
  table holds tens of rows (0016:70-76 says this at length; do not re-add one).
- **No new table.** Verdicts stay `ai_runs` + `ai_extractions` discriminated by
  `worker_type`; the residue queue is a filter, not a table (invariant 2).
- **No `capture_rules` / `capture_decisions` change.** The rules this ticket adds
  are rows, inserted through the existing `capture_rule_add` executor tool.
- **Consequence to check before merging**: test fixtures that `INSERT INTO
  projects` without naming `ai_classify` now get `false`. 0016 hit exactly this
  with `ai_locality` and 23 fixtures. Any suite that exercises the personal
  classify lane must set `ai_classify = true` or it will start **skipping**
  rather than failing — passing while exercising nothing.

## API / MCP tool changes

**None.** No executor tool, no MCP tool, no new agent-facing capability.

The capture rules are added through the **existing** `capture_rule_add` executor
tool via `opsctl capture-rules add` (`cmd/opsctl/main.go:350-396`) — that tool is
`humanOnly` and deliberately not MCP-listed, because an agent must not be able to
redirect the funnel. This ticket adds no second path to it and inserts no
`capture_rules` row by hand or by migration (premise 7).

The classifier still creates nothing, so invariant 3 has no surface here.
Recorded for the ticket that takes it live, and sharper than SWT-22's version
because the residue makes it concrete: **a flagged residue message has no
project**, so `create_task` has nothing to file it under. Going live for this
lane therefore requires either a capture rule (which removes the message from the
residue) or a deliberate inbox project — a design decision, not an increment.
Nothing here may pre-empt it.

Internal Go surface added (not tools):

```go
// internal/classify
type Lane struct{ Name, WorkerType, System, PromptVersion, LabelsPath string }
var LanePersonal, LaneResidue Lane
const ResiduePromptVersion = "residue-v1"
const ResidueSystemPrompt   = `...`

// internal/capture (report sections, called from Report)
// reportUnmatchedSenderDomains, reportUnmatchedChannels, reportUnmatchedDomainDetail
func senderDomain(sender string) string   // net/mail parse, pure, unit-tested
```

## MQTT topics

None. No heartbeat, command topic or LWT is touched. `classify` and the capture
pass are one-shot CLI runs, not fleet workers.

## Files likely to touch

New:
- `migrations/0018_bulk_project_and_classify_flag.sql`
- `internal/classify/lane.go` (`Lane`, the two lanes, `ResidueSystemPrompt`)
- `internal/classify/lane_test.go` (criteria 10, 16, 21-24)
- `internal/classify/residue_integration_test.go` (criteria 11, 13, 17)
- `internal/capture/senderdomain.go` + `senderdomain_test.go` (criterion 2)
- `docs/evals/residue-actionability.jsonl`

Modified:
- `internal/classify/store.go` (`inboxWhereResidue`; NULL-safe `inboxSelect`;
  lane-aware `PendingMessages` / `MessagesByID`; `Attribution` per lane)
- `internal/classify/classify.go` (`Config.Lane`; `classifyAll` reads
  `cfg.Lane.System` / `.WorkerType` / `.PromptVersion`; the honesty label)
- `internal/classify/report.go` (lane-scoped `worker_type` in both queries)
- `internal/classify/eval.go` (lane; strata; the "no longer unmatched" line)
- `internal/classify/prompt.go` (`SystemPrompt` referenced by `LanePersonal`;
  the "four/five fields" comment untouched — the contract does not change)
- `internal/classify/structure_test.go` (criteria 25, 26, 27, 28)
- `internal/classify/store_integration_test.go` (a `local_only` +
  `ai_classify=false` control that must be excluded from the personal lane)
- `internal/capture/rulesreport.go` (three new sections)
- `internal/capture/rules_integration_test.go` (census sections over seeded rows)
- `cmd/classify/main.go` (`--lane`, the required `--since` refusal, labels
  default per lane)
- `cmd/opsctl/main.go` (`capture-rules report --domain`)
- `docs/runbooks/local-classifier.md`, `docs/runbooks/capture-rules.md`
- `docs/tickets/local-classifier_SPEC.md` (criterion 11 amendment)
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`

Deliberately NOT touched: `internal/triage/*` (its prompt, its filter and its
inbox are unchanged — this ticket does not make triage better at the residue, it
gives the residue a different reader), `internal/provider/*` (the adapter,
`ClassOf` and the Router already cover unmatched — premise 4), `internal/drafts/*`,
`internal/orchestrator/*`, `internal/capture/rules.go` (the pure evaluator; only
the report file changes), `internal/connector/*`,
`docs/evals/personal-actionability.jsonl`.

*Amended 2026-09-02 (measured-run robustness, found the hard way): the 874-label
eval run added three pieces of supporting surface not listed above —
`Config.EvalCheckpoint` + `cmd/classify --checkpoint` (each verdict appends to
a progress file, a rerun resumes past finished ids and refuses a model change,
the file is removed on success), a batch-sized eval context deadline in
`cmd/classify` (the original constant 2h ceiling killed two full runs with an
error wearing a provider costume), and one bounded per-message retry. They are
verification-infrastructure for step 12 at 874 labels, not features; contract
pinned by `internal/classify/evalresume_test.go`.*

## In scope / Out of scope

**In scope**
- The domain-level residue census, its three sections, and the Go-side sender
  parse.
- The measured investigation of `sspataro.com`, `upwork.com`, `github.com` and
  the address-less senders, and the recorded answer.
- Attribution-only capture rules for the domains that clear the gate, and the
  `bulk` project they point at.
- `Lane`, the residue inbox filter, the residue prompt, `worker_type='classify_residue'`,
  the required `--since`.
- The stratified labelled set, the strata-aware eval output, and the recorded
  base rate / recall / precision.
- Migration 0018 and the two replaced guards.
- Runbook and institutional-knowledge entries.

**Out of scope — including what it is tempting to bundle**
- **Taking `classify` live, on either lane.** Still shadow. Flagged →
  `create_task` through the executor is a later ticket, and the residue makes it
  harder, not easier (no project — see API section).
- **Taking capture live.** `CAPTURE_RULES_MODE` stays shadow and the flip lives
  in the kube repo's CronJob manifests. Every rule this ticket adds is
  attribution-only, so its live behaviour is identical to its shadow behaviour —
  which is exactly why it is safe to add them before that decision.
- **Routing the WORK segment.** The ~961 Slack/Upwork messages, the `github.com`
  notifications and whatever `sspataro.com` turns out to be are **measured and
  named, not claimed**. Attributing them to a client project would remove them
  from triage's inbox without giving them a task — a routing decision with live
  consequences that this ticket cannot verify. It is the obvious follow-up and it
  belongs to whoever owns triage's go-live.
- **Improving triage.** Its client-work prompt over the residue is the wrong
  question (SWT-22 premise 4, and SWT-22's own verification step 9 tells the
  operator to expect poor verdicts). This ticket does not fix it and does not
  delete it: triage's future is genuinely-unrouted CLIENT mail.
- **A second model or a second pass.** Still SWT-22's named future dial; still
  needs the full labelled set first. Nothing here loads a second model.
- **A confidence field.** qwen3:8b returns a constant 0.95
  (`prompt.go:23-29`); the answer has not changed for a different inbox.
- **A new taxonomy / per-category queues.** Argued and refused in "The three
  answers", (a). If it is ever right, it is right because a consumer exists.
- **A dashboard lane** for classifier verdicts. Still future work.
- **Deploying anything.** No CronJob, no image, no manifest — the kube repo is a
  different session's territory. This runs on the workstation.
- **A full historical sweep.** The code permits it (`--since 87600h`); this
  ticket does not run one and its verification does not depend on one.
- **Attachment / PDF extraction.** Unchanged from SWT-22.

## Invariants that apply

1. **Raw-first.** No connector changes and no new decoder. The residue lane reads
   `normalized_messages` rows that were raw-first when ingested and `links` that
   SWT-25's normalizer extracted from raw. The concrete demand on this diff:
   `internal/classify` still reads no `raw_source_items` and decodes no MIME —
   `TestClassifyPackage_FetchesNothingAndDecodesNoMIME` (structure_test.go:808)
   must still pass unchanged, and the census parses only the `sender` column that
   is already normalized.
2. **One funnel.** No new task-like table and no new decision table. The residue
   queue is `latest.action='unmatched'` — a filter (criterion 12). The bulk
   "category" is a `projects` row reached through the existing
   `capture_decisions`, not a `noise_messages` table, and the verdicts are
   `ai_runs` + `ai_extractions` discriminated by `worker_type`. Shadow stays
   structural: `classify.Store` gains no write method, so
   `TestShadow_StoreHasNoTaskWriteMethod` (worker_test.go:616) still holds.
3. **Everything through the executor.** Nothing here is executed by an agent and
   no handler is invoked outside `Executor.Execute`. The one thing this ticket
   *writes* to production configuration — the capture rules — goes through the
   existing `capture_rule_add` tool, which is `humanOnly` and off the MCP
   surface. **No `INSERT INTO capture_rules` in a migration, in a script, or by
   hand** (premise 7): a rule inserted around the tool has no audit row and no
   pattern validation, and the tool is the only place the regex is compiled
   before it is stored.
4. **Nothing external without a delivery row.** Nothing sends. SWT-21's sharper
   reading carries over: a POST to a model is not a `deliveries` channel, its
   gate is the Router, and this ticket's demand is that the residue lane never
   reaches a hosted client. That is not a new guarantee to build — premise 4 shows
   `ClassOf` already restricts every unmatched message — but it IS a guarantee to
   pin: a unit test builds a Router with a nil local slot, runs a residue pass,
   and asserts the general fake recorded **zero** `Complete` calls, with a control
   proving the same fixture CAN be classified when the local lane is present.
5. **Own-message loop closure.** The residue inbox is inbound-only, and the
   `direction='inbound'` clause is **structurally redundant** for the same reason
   SWT-22 recorded (store.go:26-31): capture only ever decides inbound messages,
   so an outbound message can never carry an `unmatched` decision either. It is
   written anyway so a reader need not know that to trust the query, and it must
   **never** be described as the thing that keeps our own sends out. The
   neighbour fold (store.go:167-199) stays inbound-only for the stronger reason:
   an outbound message's absent decision is absent-because-impossible, and
   folding it would restrict every thread we have ever replied on, permanently.
6. **Stealth attribution.** Nothing client-visible is written. `title` and
   `reason` are internal, and no residue verdict reaches a delivery.
7. **Orchestrator purity.** Untouched; it learns nothing about lanes. The new
   report sections are deterministic SQL plus a pure Go fold with no model in
   them, and `internal/capture/rules.go` — the file the purity scan guards — is
   not modified at all.

Landmine classes this ticket walks past, and what it does about each:

- **A predicate whose input comes from a column, tested with a fixture** (the
  7th-instance landmine, twice inside SWT-21). Two here: the residue inbox's
  `latest.action` and the personal inbox's new `ai_classify`. Both regressions go
  in the **integration** suite with Postgres producing the value, and criteria 13
  and 27 name the mutation that must turn each red.
- **A predicate whose discriminating column is a constant in production.**
  `ai_classify` will be `true` on exactly one project and `false` on all others
  on day one — which is a real discriminator only because `bulk` exists. The test
  that matters is the one seeding a `local_only, ai_classify=false` project and
  asserting it is excluded; without `bulk` in production that clause would be
  untested and inert.
- **A comment that states the opposite of its code.** Criterion 15 and criterion
  27 fix three, and the honesty label is the one a reviewer should read first.
- **An alarm that cannot tell "did not run" from "found nothing."** SWT-22's
  criterion 17 separation is inherited unchanged and must keep working per lane:
  a skipped residue pass writes no extraction and the messages stay pending.
- **A number quoted out of the context that produced it.** The 60-minute estimate
  is this ticket's own instance, and criterion 30 puts the correction in
  institutional knowledge so it is not made a third time.

## Sibling patterns to copy

- **The lane split:** do NOT invent a second worker package. `classifyAll`
  (`classify.go:177`) and `renderUser` (`:445`) are shared by `Run` and `Eval`
  today; the lane is a parameter threaded through `Config`, exactly as `Model`
  and `Limit` are.
- **Two SQL variants under one projection:** `internal/classify/store.go`'s
  `inboxSelect` / `inboxWhere` split is already the shape — add a second WHERE,
  never a second SELECT list (SWT-25 criterion 15's property).
- **Report sections:** `internal/capture/rulesreport.go` — the `latestDecisions`
  CTE with its `DISTINCT ON … ORDER BY id DESC` comment (why shadow re-runs must
  not triple-count), `topCounts` for deterministic ordering, and
  `threadKeyPrefix` (`:288-345`) as the precedent for folding a format in Go
  rather than in SQL.
- **Migration shape:** `migrations/0016_provider_locality.sql` — the numbered,
  order-is-load-bearing comment block, the fail-closed default, the preserving
  UPDATE, and `ON CONFLICT (slug) DO NOTHING`. Copy its structure literally.
- **Replacing a guard that became wrong:** `TestMigration0017_IsTheOnlyOneThisTicketAdds`
  (structure_test.go:734-741) is itself the replacement of SWT-22's
  `TestNoMigrationWasAdded`, and its comment explains the rule. Do the same again.
- **Structural scans with a control:** `internal/provider/callsites_test.go` and
  `internal/capture/rules_structure_test.go` `TestRulesGo_IsPure` — a scan for a
  token that cannot appear is worse than no scan.
- **Integration-suite hygiene:** `make integration` runs `-p 1` because the triage
  and connector suites cross-pollute on one compose db. The new residue suite
  joins that mutual-cleanup pact: clean its own rows first, in FK order, scoped by
  a test-owned slug.
- **Labelled-set discipline:** `internal/classify/eval.go:76-105` (drift printed
  and excluded before anything is classified) and `SubjectHash` (`:33-36`, the one
  spelling, over `textmatch.NormalizedPrefix`). Do not re-spell either.

## Verification protocol

Every command is meant to be run, not reasoned about.

```bash
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
export OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434
export OPS_LOCAL_MODEL=qwen3:8b
alias opsctl='DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl'
```

Remember `cmd/classify` reads **`DATABASE_URL`**, not `OPS_DATABASE_URL`
(institutional knowledge, bit 2026-08-30).

1. **`go test ./...`** — the lane table, the required-`--since` refusal, the
   prompt scans (criteria 21-23), the sender-domain parse table, the
   zero-hosted-calls residue test with its control, the strata-aware eval output
   and its backwards-compatibility case, and the replaced guards. No db, no
   network, no model.
2. **`make db-up && make migrate && make integration`** — criteria 11, 13, 17 and
   the personal-lane exclusion control.
   **Mutations that must turn them red, run them:** drop
   `AND latest.action = 'unmatched'` from `inboxWhereResidue` (the `attributed`
   fixture appears); drop `AND p.ai_classify` from `inboxWhere` (the
   `ai_classify=false` fixture appears). If either stays green you tested a
   fixture.
3. **Re-measure the residue before touching it** — do not trust this SPEC's
   literals:
   ```bash
   psql "$OPS_DATABASE_URL" -tAF' | ' -c "
   WITH latest AS (
     SELECT DISTINCT ON (message_id) message_id, action, project_id
       FROM capture_decisions ORDER BY message_id, id DESC)
   SELECT l.action, COALESCE(p.slug,'(none)'), COALESCE(p.ai_locality,'-'), count(*)
     FROM latest l LEFT JOIN projects p ON p.id = l.project_id
    GROUP BY 1,2,3 ORDER BY 4 DESC;"
   ```
   `DISTINCT ON … ORDER BY id DESC` is not decoration: shadow writes a decision
   per pass, so a plain count multiplies by the number of passes.
4. **The census, and the three investigations** (criteria 1-4). This is Phase 0
   and it gates everything after it:
   ```bash
   opsctl capture-rules report --since 0
   opsctl capture-rules report --domain sspataro.com
   opsctl capture-rules report --domain upwork.com
   opsctl capture-rules report --domain github.com
   ```
   Record in `docs/runbooks/capture-rules.md`: the top-40 domain table with
   cumulative coverage, the channel breakdown, the address-less count and its
   channel, and one sentence each on what `sspataro.com` and `upwork.com`
   actually are. **If the address-less segment is large and Slack-shaped, say so
   in the commit summary** — it is a finding about the funnel, not about this
   classifier.
5. **Seed the rules, then measure the delta.** Residue size before, `opsctl
   capture-rules add` per domain that cleared the gate, one shadow pass
   (`opsctl capture-rules run --since 0 --all`), residue size after. Record both
   numbers and the percentage. Confirm `opsctl capture-rules list` shows every
   new rule as `-> attribution only (no external_system, so no task)`, and
   confirm nothing was created:
   `SELECT count(*) FROM tasks;` and `FROM external_refs;` unchanged.
6. **Migration state, before and after.**
   `psql "$OPS_DATABASE_URL" -tAc "SELECT max(version) FROM schema_migrations"`
   reads `0017` before and `0018` after `go run ./cmd/tools/migrate`;
   `ls migrations/` shows exactly one new file. Merging a migration is not
   applying it.
7. **The personal lane is unchanged, and this is the check that proves it.**
   Run `go run ./cmd/classify run --limit 5` (personal, the default lane) and
   confirm it still selects `personal` messages only and that a `bulk`-attributed
   message never appears:
   ```bash
   psql "$OPS_DATABASE_URL" -tAF' | ' -c "
     SELECT r.worker_type, e.fields->>'project_slug', count(*)
       FROM ai_extractions e JOIN ai_runs r ON r.id = e.ai_run_id
      WHERE r.worker_type LIKE 'classify%' GROUP BY 1,2;"
   ```
   Every `classify` row is `personal`; every `classify_residue` row has an empty
   slug.
8. **The headline smoke — the residue lane runs.**
   ```bash
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify run --lane residue --since 720h --limit 20
   DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify report --lane residue --since 24h
   ```
   Read twenty verdicts by eye. The check is not the score — it is whether the
   *questions* are right: a LinkedIn digest must be `informational`, and if the
   model is flagging marketing urgency the trap clause of criterion 23 needs
   work before the eval is drawn.
9. **The refusal.** `go run ./cmd/classify run --lane residue` with no `--since`
   exits non-zero, sends nothing, and prints the count and the 7.2 s figure.
10. **The negative smoke still refuses, on the new lane.**
    `OPS_LOCAL_PROVIDER_URL=https://api.openai.com go run ./cmd/classify run
    --lane residue --since 24h --limit 5` with `OPENAI_API_KEY` exported →
    startup refusal, `avail_reason='local_endpoint_not_private'`, one
    `status='skipped'` row, zero extractions, zero hosted calls. A wider inbox
    must not weaken SWT-21's thesis.
11. **The unreachable smoke, and the difference it draws.** Stop ollama
    (`systemctl --user stop ollama`) or point at `http://127.0.0.1:1`; run the
    residue pass; confirm exit code **0**, `avail_reason='local_unreachable'`, no
    extraction, and the same message ids still pending. Restart and confirm they
    are processed and leave the inbox.
12. **Draw and score the labelled set.** Draw per criterion 19 (uniform ≥200,
    enriched ≥100, plus the domain-gate rows), hand-check every line, then:
    ```bash
    DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify eval --lane residue \
      --labels docs/evals/residue-actionability.jsonl
    ```
    Expect the three-line breakdown, `label drift: 0 excluded`, and every false
    negative by id. **Record n per stratum, base rate, recall, precision, median
    latency and the date** in `docs/runbooks/local-classifier.md`. Then confirm
    the personal eval is byte-for-byte unaffected:
    `go run ./cmd/classify eval --labels docs/evals/personal-actionability.jsonl`
    still prints today's shape and a comparable number.
13. **Re-derive the cost honestly and write it down.** Take the median latency
    the eval just printed, multiply by the residue size from step 5's "after"
    figure, and record both the full-sweep hours and the steady-state cost:
    ```bash
    psql "$OPS_DATABASE_URL" -tAc "
    WITH latest AS (
      SELECT DISTINCT ON (message_id) message_id, action FROM capture_decisions
       ORDER BY message_id, id DESC)
    SELECT count(*) FILTER (WHERE m.sent_at >= now() - interval '30 days') / 30.0
      FROM latest l JOIN normalized_messages m ON m.id = l.message_id
     WHERE l.action = 'unmatched';"
    ```
    That daily arrival rate × the median is the number that decides whether this
    lane runs on a schedule. **If it contradicts this SPEC's estimate, the SPEC is
    what is wrong.**
14. **`go test ./...` once more after the doc edits** — criteria 26-30's
    structural tests read the files on disk.

## Decisions made unilaterally (argue if wrong)

- **One prompt contract for both lanes** (`VerdictSchema` reused verbatim),
  rather than a residue taxonomy. Argued in "The three answers", (a). The
  strongest version of the counter-argument, recorded so it can be picked up
  later: if the residue's real value is "should I still be receiving this", the
  right output is an unsubscribe/discard queue — but that queue has no consumer
  and would be a table, which invariant 2 refuses.
- **A separate `worker_type`, not a shared one.** Criterion 11 names the concrete
  defect a shared value causes.
- **Forward-only by default, with `--since` REQUIRED and no default value.** A
  default is a 29-hour job one typo away; a refusal is one flag away from either
  behaviour. The full sweep stays possible (`--since 87600h`) and this ticket
  does not run it.
- **The bulk rules are attribution-only, and the work segment is not claimed.**
  Attributing Slack/GitHub/`sspataro.com` traffic to a client project would take
  it out of triage's inbox and give it nothing — a routing decision with live
  consequences this ticket cannot verify. Measured and handed on instead.
- **ONE bulk project, not four category projects.** `job-alerts`, `social`,
  `newsletters`, `retail` would be four slugs nothing filters on, and the
  category is recoverable from `capture_decisions.matched_rule_id` and the census
  whenever it is wanted.
- **The claim gate is a measured zero-actionable sample of ≥20 per domain**, not
  judgement. It is the same reading the labelled set needs anyway, so it costs
  nothing extra and leaves an audit trail for why a domain was called noise.
- **The labelled set is stratified and the strata are IN the file.** Precision
  computed over an enriched sample is not production precision, and it will be
  quoted as if it were. Putting the stratum in the record is the only way the
  eval can refuse to make that mistake for the reader.
- **The census lives in `internal/capture/rulesreport.go`, not in a new
  `residue` package.** It is the "what rule should exist next" report and it is
  useful to capture with or without this classifier.
- **`ai_classify` is a boolean column, not a key in `projects.policies`.** 0016
  set the precedent by adding `ai_locality` as a real column with a CHECK rather
  than a jsonb key; an untyped predicate over jsonb is exactly the thing this
  repo keeps paying for.
- **The residue prompt keeps the recall-first objective.** It is tempting to
  argue that in a pile of marketing mail precision matters more, because false
  positives are the whole population. It is still wrong: a false positive costs a
  second in a report nobody is paged by, and this lane is shadow. Revisit when
  something consumes it.

## What changes if the census contradicts this SPEC

Written now, so the answer is not improvised at implementation time:

- **If the address-less / Slack segment is more than ~15% of the residue**, then
  the residue is not primarily a personal-mail problem and the ticket's centre of
  gravity moves to routing. The classifier lane still ships (it is cheap once the
  lane exists), but the runbook must say plainly that the largest single thing
  wrong with the residue is unrouted work, and the follow-up ticket is triage's,
  not this one's.
- **If `sspataro.com` turns out to be a contact form or a client alias**, its
  rule points at the relevant project, not at `bulk`, and it counts as work
  claimed rather than noise claimed. If it turns out to be cron/mailer output
  from Salvador's own host, it is `bulk` and the runbook says so.
- **If the uniform stratum's base rate is below ~0.5%**, say so and act on it:
  the lane runs forward-only on new mail, never on history, and the honest
  description in the runbook is "a daily filter", not "a classifier over the
  residue".
- **If the domain gate rejects most candidates** (i.e. the "noise" domains turn
  out to carry actionable mail), then rules are not the cheaper answer for
  anything, Phase 1 shrinks to whatever passed, and the whole value of the ticket
  is the lane plus the census. That is an acceptable outcome and must be recorded
  as the measured result, not hidden by adding rules that failed their gate.

## Phase-0 addendum — the census investigations, run 2026-08-31 (ad-hoc SQL; the report tool of criteria 1-4 re-derives these)

- **`sspataro.com` (518)** = `test@sspataro.com` (503) — the notify-idle /
  session-notification sender ("[BLOCKED] Claude: …", "[FYI] Claude done: …") —
  plus `openproject@sspataro.com` (14, the retired OpenProject). This is the
  pre-committed "cron/mailer output from Salvador's own host" branch: it goes
  to **bulk**, and its rule clears the gate trivially (machine notifications,
  zero actionable in any sample).
- **`upwork.com` (106)** = platform notifications from `donotreply@` /
  `email.upwork.com`, including "Invitation to Interview" — actionable work
  mail. **Stays refused**, confirmed measured rather than suspected.
- **`github.com` (455)** = predominantly `Foundry-Underwriting/Foundry-RAVE`
  CI-failure notifications plus OSS threads — **unrouted WORK** for a project
  no capture rule names. Stays refused; recorded as a funnel finding.
- **Address-less senders: 1,287, and every one is `channel='upwork'`** — not
  Slack. They are Upwork CRM conversations (prospect/unrouted clients) sitting
  unmatched. 8.7% of the residue — under the 15% pivot threshold, so the
  ticket's centre of gravity stays as written; the finding goes in the runbook
  and the commit summary.
- Residue by channel: gmail 13,454, upwork 1,287, nothing else.

## Future work (not this ticket)

- **Routing the work segment**: Slack workspaces/channels and GitHub repos to
  projects, which is triage-go-live territory and needs client-routing decisions.
- **An inbox project for flagged residue**, so a residue verdict can become a
  task through `create_task`. Today it cannot: no project, nothing to file under.
- **The second-pass precision mechanism** (SWT-22's named dial) — more valuable
  on this lane than on the personal one, because the residue's false-positive
  volume is larger. Still needs the full labelled set first.
- **A sender-context column**, editable through `opsctl` and rendered into the
  one prompt — the residue's version is "this sender emits both marketing and
  account notices", which is exactly `google.com`.
- **Promoting rules from the classifier's own output**: a domain whose last N
  verdicts are unanimously `informational` is a rule candidate, with the evidence
  already recorded. That is the honest way this lane shrinks its own workload
  over time, and it needs the eval numbers first.
- **A dashboard lane** for both classify lanes and their skipped counts.
- **Running `classify` on a schedule**, once the deployment question (SWT-22's
  Q2) has an answer that survives the workstation being off.
