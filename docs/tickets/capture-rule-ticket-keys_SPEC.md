> Jira: PENDING-SYNC

# capture-rule-ticket-keys — rule 10 keys every Treetop ticket by its prefix, so all OPS/WEB/API mentions collapse onto three tasks

## Source

Ad-hoc, from Salvador, verbatim:

> ops tickets from foundry are ending on collaboratory. the rule matchins ops
> need to quality which jira are they comming from

and, after the investigation below:

> do option 1 and write the ticket for 2

Option 1 shipped the same day as data (rule 59, see "Already shipped"). **This
ticket is option 2**: the defect the investigation found underneath the symptom.

## What the investigation established

Verified against the live `ops` db on 2026-09-11. Do **not** copy these counts
into a test as literals — the corpus is live (SWT-19's rule: "a literal cries
wolf every day a message arrives"). They are here to size the work.

### Rule 10 keys by prefix, not by ticket

```
10  prio 90  on  collaboratory  body_regex  (WEB|API|OPS)-[0-9]+
    -> jira key url https://treetopllc.jira.com/browse/{key}
    note: Treetop ticket key in ANY channel ...
```

It has no `key_regex`, so `externalKey` reuses the pattern — and
`extractKey` returns the **first capture group** when there is one
(`internal/capture/rules.go`). The group is `(WEB|API|OPS)`, so the key is the
bare prefix. Every mention of every ticket dedups onto one of three
`external_refs`:

```
task 56  closed  API — Re: [treetopllc/gonoble] Api 4308 ...   key API  247 messages logged
task 57  READY   WEB — [treetopllc/gonoble] Web 10365 ...      key WEB  404 messages logged
task 60  closed  OPS — Jira                                    key OPS   44 messages logged
```

Each links to `https://treetopllc.jira.com/browse/OPS` (or `/WEB`, `/API`),
which is not a ticket. Task 60 is where the reported symptom lived: 25 Foundry
GitHub CI mails had been logged onto it live.

### Why Foundry mail hit it

Foundry's Jira never reaches the rule. Every Foundry misroute was a GitHub CI
notification whose branch name carries a Foundry ticket key:

```
[Foundry-Underwriting/foundry-rave-infra] Run failed: terraform-ci - ticket/OPS-21 (b0dde15)
```

The pattern cannot tell Treetop's `OPS-21` from Foundry's. Rule 59 now claims
those (see below); this ticket does not need to.

### Where rule 10's traffic comes from

| source | messages | carry the text `treetopllc` |
|---|---|---|
| slack_web `T0360B84U` (Treetop) | 1869 | — |
| slack_web `T0HPR78RX` (Collaboratory) | 337 | — |
| gmail (`sspataro@gmail.com`) | 849 | 746 |
| jira connector, account 1400 (`treetopllc.jira.com`) | 180 | 35 |

654 distinct ticket keys across 3235 messages. Qualifying rule 10 by the text
`treetopllc` would drop 2127 of them, most of them genuine Treetop traffic (the
Slack Jira app, Jira mail whose body omits the host) — so **text-qualifying the
pattern is not the fix**.

### Rule 10 also shadows the site-qualified rules

Rules 3–5 (`thread_key_prefix jira:treetopllc.jira.com:{WEB,API,OPS}-`, priority
50, key `[A-Z]+-[0-9]+$`) are correct: per-ticket keys, site-qualified by the
thread key. But rule 10 at priority 90 claims the same jira-connector messages
first (180 of them), so first-match-wins hands them to the prefix key.

### Capture is live

Live decisions in the 7 days to 2026-09-11 for google, jira, slack_web and
upwork_crm. Whatever rule 10 becomes acts on the next connector pass.

### Tooling

- `capture_rule_add` and `capture_rule_set_enabled` are executor tools
  (`internal/tools/createtask.go`, allowed in `internal/policy/matrix.go`).
- `opsctl capture-rules` exposes `add`, `list`, `run`, `report` only.
  `capture_rule_set_enabled` is reachable today only through
  `opsctl call --tool capture_rule_set_enabled --args '<json>'` (args per
  `parseCaptureRuleSetEnabled`: the rule id and `enabled`).
- There is **no tool that changes a rule's pattern, key_regex or priority.**
  A raw `UPDATE capture_rules` would be a side door (invariant 3).

## Goal

Treetop ticket mentions resolve to the ticket they name — one dedup key per
ticket (`WEB-10442`), never a prefix — without flooding the board when the
change goes live, with the site-qualified rules claiming the jira-connector
traffic they were written for, and with the three bucket tasks resolved.

## Open questions (owner decides before implementation)

**Q1 — Fix path.**
- **(a) Disable and replace, data only.** Disable rule 10 with
  `capture_rule_set_enabled`; add a replacement through `capture_rule_add` with
  an explicit `--key-regex '\b((?:WEB|API|OPS)-[0-9]+)\b'`. No code. The
  replacement gets a new rule id, so per-rule report history splits across two
  ids; record the lineage in both notes.
- **(b) Edit in place.** Add a `capture_rule_update` executor tool (pattern,
  key_regex, url_template, priority, note; audited like `add`) and
  `opsctl capture-rules update|enable|disable`. Rule 10 keeps its id.

Recommendation: **(a)** for the fix, plus thin `opsctl capture-rules
enable|disable` subcommands over the existing tool (no new tool, no migration).
Editing rules in place is worth having, but it is not what this bug needs.

**Q2 — What a mention does once keys are per ticket.** Today the prefix key
means a mention of a ticket with no task *appends* to a bucket. With per-ticket
keys, a mention of a ticket that has no task *creates* one — up to one per
distinct ticket mentioned (654 so far), from Slack chatter as much as from work.
Options:
- **(i) Attribution only.** The replacement has no `external_system`: mentions
  attribute to collaboratory, never create or log. Tasks come only from rules
  3–5 (the jira connector). Loses the link from GitHub/Slack mentions to the
  ticket's task.
- **(ii) Create per ticket.** Accept a task per mentioned ticket. Needs a
  deliberate `--since` for the first live pass and a read of the shadow report.
- **(iii) Log only onto an existing ticket task.** A new rule field, e.g.
  `append_only`: the rule may append to a task whose `external_refs` key
  already exists, and never creates one. Code in `internal/capture` plus a
  forward-only migration. Keeps the mention linkage without the flood.

Recommendation: **(iii)** if the mention linkage is wanted, else **(i)**.
(ii) makes the board a mirror of Slack.

**Q3 — The bucket tasks.** Task 57 is still `ready`. Proposal: dismiss it
through `task_dismiss` with a reason naming this ticket (it is labelled data,
per SWT-31). Leave 56 and 60 closed and their logs as history. Leave the three
`external_refs` rows (keys `OPS`/`WEB`/`API`) in place: nothing produces those
keys after the fix, and deleting them is a separate decision.

## Decisions made unilaterally (with rationale)

- **The replacement ranks BELOW rules 3–5** (priority 40, say, not 90). The
  site-qualified thread-key rules must claim the jira-connector traffic, and a
  mention rule that outranks them repeats the shadowing described above.
- **Rule 59 must still outrank the Treetop mention rule.** It sits at 95; the
  replacement must stay below it.
- **No text qualification of the pattern.** Established above: it drops ~2100
  genuine Treetop messages.

## Acceptance criteria

- The Treetop mention rule's derived key matches `^(WEB|API|OPS)-[0-9]+$` for
  every message it matches; no message yields a bare prefix. Proven with the
  engine's own Go `regexp` over the exported corpus (recipe below), not a
  Postgres regex — Postgres ARE reads `\b` as a backspace.
- After a shadow `--since 0 --all` pass, jira-connector messages from account
  1400 are decided by rules 3–5, not by the mention rule.
- The Foundry GitHub mail rule 59 claimed still resolves to rule 59.
- The Q2 policy holds in the shadow report: under (i) or (iii), zero
  `tasks_created` attributable to the mention rule.
- No new `task_log` lands on tasks 56, 57 or 60 after the change goes live.
- Task 57 resolved per Q3.

## Files likely to touch

- Q1 (a) + enable/disable: `cmd/opsctl/main.go` (subcommands over
  `capture_rule_set_enabled`), `docs/runbooks/capture-rules.md`.
- Q2 (iii): `internal/capture/rules.go`, `internal/capture/rules_store.go`,
  a new `migrations/` file, `internal/tools/createtask.go`
  (`capture_rule_add` accepts the field), `cmd/opsctl/main.go` (`--append-only`).
- Tests beside their siblings: `internal/capture/rules_test.go`,
  `internal/capture/rules_integration_test.go`.

## Invariants that apply

- **Everything through the executor** — no raw `UPDATE capture_rules`.
- **Shadow first** — the runbook's go-live rule; capture is live, so a rule
  that creates tasks acts the moment it is added.
- **One task per external ticket** — dedup on `external_refs`; the fix restores
  exactly this.
- **A regex that does not match yields no key** — attribution survives.

## Verification protocol

1. `echo "CAPTURE_RULES_MODE=${CAPTURE_RULES_MODE:-<unset, so shadow>}"` — must
   be unset before any `run`.
2. Export subject + body of every message the mention rule matched, plus the
   `Foundry-Underwriting/` thread-key messages, as JSONL; run the candidate
   pattern and key_regex through Go `regexp` (the rule-59 check below is the
   template).
3. Shadow pass: `opsctl capture-rules run --since 0 --all`, then
   `opsctl capture-rules report --since 168h`.
4. Queries: derived-key distribution for the mention rule; rules 3–5 match
   counts on account 1400; rule 59's claims; new `task_log` rows on 56/57/60.

## Already shipped (option 1, data only, 2026-09-11)

```
59  prio 95  on  foundry  body_regex  \[Foundry-Underwriting/[^\]]+\][^\n]*\bOPS-[0-9]+
    -> attribution only (no external_system, so no task)
```

Added with `opsctl capture-rules add` (executor path). Checked with Go `regexp`
against 3345 messages before adding: 26/26 misrouted Foundry mails matched, 0 of
the 110 other Foundry CI mails, 0 of 3209 non-Foundry rule-10 mails. A shadow
`--since 0 --all` pass afterwards (53,296 considered, 0 tasks created) routed
the 26 to rule 59 and left the 110 unmatched for triage; rule 59 claimed nothing
outside Foundry mail.

It is attribution only on purpose: capture is live for gmail, and a
task-creating rule would act immediately. Giving Foundry OPS tickets their own
tasks (`--external-system jira --url-template
https://foundryunderwriting.atlassian.net/browse/{key} --key-regex
'\b(OPS-[0-9]+)\b'`) is a follow-up decision once its decisions have been read —
and it would be a replacement rule (disable 59, add), for the same reason as Q1.

## Out of scope

- A general source qualifier on rules ("this rule applies only to messages from
  source X"). It would have been the textbook answer to "qualify which Jira",
  but every misroute turned out to be GitHub mail, which rule 59 separates by
  repo. Future work if a second Jira ever sends overlapping keys through the
  same channel.
- Re-keying the historical decisions and logs already on 56/57/60.
