> Jira: PENDING-SYNC

# capture-rules — open questions

**Questions 1-4 are answered and CLOSED** (Salvador, 2026-08-20); the SPEC has
folded them in. Answers are recorded under each below — do not re-open them.

**Question 5 is NEW and NON-BLOCKING.** It surfaced while implementing Q4's
answer: the drafts rework hit exactly the "reachable only via a person" case,
and the SPEC resolves it by REMOVING a capability rather than inventing a
lookup. Implementation can proceed on the SPEC's default; answer 5 when
convenient.

---

## 1. Treetop Jira **OPS** project (148 threads) — which project?

WEB and API are assigned to Collaboratory. OPS was never placed. Threads are
`jira:treetopllc.jira.com:OPS-*`.

**Add `thread_key_prefix jira:treetopllc.jira.com:OPS-` → collaboratory at
priority 50** (same shape as WEB/API — one more row, tickets become Collaboratory
tasks) **OR leave OPS unruled** (it falls to `unmatched`, shows up in the shadow
report's unmatched leaderboard, and stays out of the funnel until you have
looked at what those 148 threads actually are).

A third option exists if OPS is neither: its own `projects` row. Say so and the
rule becomes `→ <new slug>`.

**Answer:** Collaboratory, same as WEB and API (Salvador, 2026-08-20: "OPS is
there too"). Add `thread_key_prefix jira:treetopllc.jira.com:OPS-` → collaboratory
at priority 50. Folded into the SPEC's fixture rule set — this question is closed.

---

## 2. Treetop Slack workspace `T0360B84U` — 40,452 messages, 59% of the corpus

This is where the ReEngine discussion actually happens, and it is unassigned. It
cannot mirror the `source_slack_workspace T0HPR78RX → collaboratory` rule: that
workspace maps cleanly to one project, this one spans both and probably neither.
Note the LHH signal already arrives from it via `body_regex LHH-[0-9]+` at
priority 100, so ReEngine ticket capture works whether or not this question is
answered.

**Leave the workspace unruled** — only the specific high-priority content rules
(LHH keys, repo names, Jira thread keys) pull messages out of it, everything else
is `unmatched` and invisible to the funnel — **OR add a low-priority
(priority 1) catch-all `source_slack_workspace T0360B84U → <project>` with
`external_system` NULL**, so every otherwise-unmatched Treetop message is at
least attributed to a project without creating any task.

The second only makes sense if you name the project it should default to. If the
honest answer is "neither, it's a mixed workspace", that IS the first option.

**Answer:** Option two, defaulting to **collaboratory** (Salvador, 2026-08-20).
Add a priority-1 catch-all `source_slack_workspace T0360B84U` → collaboratory with
`external_system` NULL: every otherwise-unmatched Treetop message is attributed,
none becomes a task. The priority-100 `LHH-[0-9]+` rule still pulls ReEngine
tickets out of the same workspace first — the catch-all only ever sees what the
specific rules did not claim. CLOSED.

---

## 3. Unmatched messages: fall through to triage later, or stay unassigned?

Today an unmatched message gets `capture_decisions.action='unmatched'` and
nothing else. Two futures, and they imply different code in THIS ticket:

**(a) Unassigned is terminal for now.** `unmatched` is a report line and a signal
that a rule is missing. Triage (when it goes live) keeps its own project lookup
and simply cannot create a task for a message with no project. Nothing extra to
build; criterion 17 stands as written.

**(b) Unmatched is triage's inbox.** The decision row becomes the handoff: triage
live consumes `action='unmatched'` messages specifically, and matched ones are
already routed deterministically. That means `capture_decisions` gets read by a
worker looking for work — which brushes against invariant 2 and needs to be
designed deliberately rather than emerging.

The SPEC assumes **(a)**. Confirm or switch.

**Answer:** **(b) — unmatched is triage's inbox** (Salvador, 2026-08-20). Triage
live consumes `action='unmatched'` messages specifically; deterministically
matched messages are already routed and must not be re-triaged. This changes
acceptance criterion 17 and needs the `capture_decisions`-as-handoff read
designed deliberately against invariant 2 (a worker reading a non-task table
looking for work) rather than left to emerge. CLOSED.

---

## 4. `projects.client_person_id` — drop now or later?

The premise "0 rows, nothing to lose" is true of the data and false of the code.
`internal/drafts/store.go` reads it in three load-bearing places:
channel selection (`clientPerson != nil && hasUpworkIdentity(...)` → `upwork_chat`,
line 76), gmail thread resolution (lines 87-102) and the upwork client UUID
(lines 114-117), plus the client display name in `DeliverTasks`.

**(a) Keep the column, narrow its meaning** (what the SPEC does): add the
`person` criteria type so project selection is expressible as a rule row, make
triage prefer the capture decision, and leave `client_person_id` as "the client's
person for delivery targeting" until step 8 reworks draft targeting. One
migration, no drafts changes, nothing can disagree about project assignment.

**(b) Drop it in 0015.** Move project selection wholly into `capture_rules
(criteria_type='person')` and rework `internal/drafts/store.go` to resolve the
delivery target from the task's project + thread instead of a person id.
Cleaner end state, one mechanism, but it drags step-8 delivery targeting into
this ticket and the draft worker has no live coverage to catch a regression
(`cmd/drafts` is not deployed).

The SPEC assumes **(a)**.

**Answer:** **(b) — drop it in 0015** (Salvador, 2026-08-20). Project selection
moves wholly into `capture_rules (criteria_type='person')`, and
`internal/drafts/store.go` is reworked to resolve the delivery target from the
task's project + thread instead of a person id. The cost was stated and accepted:
this pulls step-8 delivery targeting into this ticket, and `cmd/drafts` has no
live coverage, so the rework needs test coverage written for it rather than
relying on a deployment to catch a regression. CLOSED.

---

## 5. NEW — the first-contact Upwork delivery target dies with the column

Implementing Q4(b) hit the case worth bouncing back rather than resolving
quietly. Four of the five read sites rework cleanly from the task's project +
thread (SPEC §9 sites A-D). The fifth does not.

`internal/drafts/store.go:130-133` has a no-thread fallback: when a client has an
upwork identity but NO ingested conversation, it synthesizes
`TargetRef = "upwork_crm:" + clientUUID + ":upwork"` so a draft can still be
aimed at that client. The uuid comes from `person_identities` via
`client_person_id`. With the column gone there is no honest way to reach it — a
thread would give it up (`clientIDFromThreadKey`, `upworkcrm/sink.go:327`), but
this branch exists precisely because there is no thread.

**The SPEC's default is (a): let it die.** The rework drops the fallback; a
Deliver task with no resolvable upwork thread yields `Channel=""` and the worker
skips it, exactly as for any other unresolvable task. Rationale: the policy
matrix already restricts Upwork to "existing threads only, ≤2 touches", so
drafting into a conversation that does not exist was outside policy anyway; and
a Deliver task only exists after work was completed for that client, which is
close to impossible for a client we have never exchanged a message with. The
capability is out-of-policy and effectively unreachable — but it is a capability,
and removing it is a choice made inside your answer, not by it.

**(b) Preserve it via `external_refs`.** Keep a client-level upwork target by
writing an `external_refs (system='upwork_crm', external_key=<client uuid>)` row
— which a capture rule can produce, since upwork thread_keys carry the uuid — and
have the drafts store fall back to that ref when no thread resolves. Costs an
extra resolution path in `internal/drafts` and a rule whose only job is to mint
client-level refs; buys back first-contact drafting on a channel the policy
matrix does not currently allow it on.

Nothing blocks on this: (a) is implemented, and (b) is additive later if you want
it back.

**Answer:** **(a) — let it die** (Salvador, 2026-08-20): "switchboard would never
talk to a person with no chat history; literally chat history is prerequisite for
the upwork person to exist."

Verified against the live corpus: 26 `person_identities` rows with
`provider='upwork_crm'`, and **0** of them lack a matching
`upwork_crm:{uuid}:%` thread — the fallback branch has never been reachable.

One nuance recorded so the reasoning is not overstated: people are created by
`upworkcrm/sink.go:185-196` from a CLIENT record (`nc.ClientID`,
`nc.DisplayName`), not from a message, so a CRM client with zero communications
WOULD by construction yield a person with no thread. It is 26/26 today because
every ingested client happens to have history, not because the code forbids the
other case. That does not change the answer: the policy matrix restricts Upwork
to "existing threads only, ≤2 touches", so the branch could only ever fire in
violation of it. Removing it removes an out-of-policy path, not a working
capability. Option (b) stays additive if first-contact drafting is ever wanted,
and would need a policy-matrix change alongside it. CLOSED.

---

**All five questions answered and closed, 2026-08-20.** Two changed scope (Q3:
criterion 17 + a designed handoff; Q4: drops a column and reworks draft
targeting). Q5 confirmed the SPEC's default, with sharper reasoning and an
empirical check recorded above.
