# Runbook — capture rules (SWT-17)

Deterministic project assignment for normalized messages. A priority-ordered rule
engine runs as a post-normalize pass in each connector, records every evaluation
in `capture_decisions`, and — in live mode only — creates one task per external
ticket through the executor.

## Capture runs BEFORE triage, and the order is not cosmetic

Triage's inbox is `action='unmatched'`. A message that capture routed
deterministically is never re-triaged; a message capture could not place falls
through to triage as the gap it is meant to fill.

Run them the other way round and the gap closes over: triage picks up messages
capture would have routed, spends model calls on them, and may create a task that
capture then creates again from its own decision. **Capture before triage, in
every main and in every schedule.** Shadow and live mode alike — shadow still
writes the decisions triage reads.

## Modes

**Shadow is the default and shadow is real.** It evaluates everything and writes
every decision; it creates nothing. That is the whole safety property: you can
seed rules, run for days, and read the report before a single task exists.

Live mode creates one task per external ticket and appends later notifications
about the same ticket as task log events rather than as new tasks.

Do not go live on a rule until its shadow decisions look right in the report. The
whole point of the SWT-6 shadow-first pattern is that a wrong rule in live mode
manufactures tasks faster than anyone reads them.

## Seeding the fixture rules

**`opsctl` is not in the container image** — it builds the connectors, `migrate`,
`dashboard` and `google-auth` only. Run it from a checkout, which is where an
operator is anyway:

```bash
cd ~/projects/personal/switchboard
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
alias opsctl='DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl'
```

Every `opsctl` command below assumes that.

The nine fixture rules are in the SPEC's "Fixture rules (the acceptance data)"
table. Add them with:

```bash
opsctl capture-rules add --project <slug> --type <criteria_type> --pattern <pattern> \
  [--external-system jira|github|upwork_crm|slack|gmail] \
  [--key-regex <re>] [--url-template <tmpl with {key}>] \
  [--subproject <slug>] [--priority <n>] [--note <why>]
opsctl capture-rules list
```

`--type` is one of `body_regex`, `sender`, `thread_key_prefix`,
`thread_key_contains`, `source_slack_workspace`, `person`.

`--external-system` empty means **attribution only** — the message gets a project
and no task. Supply it (with `--url-template`) when the rule should create one
task per external ticket.

`--key-regex` extracts the dedup key; empty reuses `--pattern` for `body_regex`
and the thread key otherwise.

### Two things about those rules that will otherwise waste your afternoon

**The Slack workspace id is case-sensitive in the data and is NOT in the rules.**
`slackweb/sink.go` writes `strings.ToLower(workspace.ID) + "@slack-web.local"`
into `source_accounts.account_email`, while the fixture patterns spell the
workspace as `T0360B84U`. A rule that compares those two with `=` matches
**nothing, forever**, for the majority of the Slack corpus — and matching nothing
is indistinguishable from "no messages qualified". The engine compares
case-insensitively for exactly this reason. If you add a workspace rule by hand,
do not "fix" it into an exact comparison.

**The two `thread_key_contains` repo rules DO fire, by a mechanism that is not
obvious.** `treetopllc/collaboratory-www` and `treetopllc/gonoble` match against
`normalized_threads.thread_key`, and no connector emits a repo path directly —
the github connector writes no threads at all, Slack keys are ids, Jira keys are
issue keys. They match through **GitHub notification emails**: GitHub sets a
Message-ID carrying the repo path, and the gmail thread key is
`gmail:{account}:{message-id}`, so the path lands inside the key.

```
gmail:sspataro@gmail.com:<treetopllc/collaboratory-www/pull/3179@github.com>
```

Measured 2026-08-28: 182 threads match `collaboratory-www`, 11 match `gonoble`.

Worth knowing because it is **fragile in a way the rule does not show**: it
depends on GitHub's Message-ID format, which is outside our control. If those two
rules ever stop matching, suspect GitHub changed its Message-ID before suspecting
the engine.

## Running a pass

Capture runs inside the connector mains automatically, after
`capture.ObserveOutbound`. To run one by hand:

```bash
opsctl capture-rules run --since 168h
```

**Shadow is the default and there is no `--mode` flag.** Acting requires `--live`.

One caveat, because the obvious reading is wrong: `--live` can only turn acting
ON. The mode is read from `CAPTURE_RULES_MODE` FIRST, so if that is set to `live`
in your environment, omitting `--live` does not give you shadow. Check the env
before assuming a bare `run` is safe:

```bash
echo "CAPTURE_RULES_MODE=${CAPTURE_RULES_MODE:-<unset, so shadow>}"
```

The pass takes an advisory lock, so two runs cannot overlap; a second one exits
rather than queueing.

`--since` and `--limit` are the anti-flood controls. The first `--live` run after
a long shadow period is the dangerous one — it sees every message inside the
window at once. Set `--since` deliberately.

`--all` re-evaluates messages that already carry a live decision row. It is
**shadow-only and refused in live mode**, because re-running an acted decision is
how you get a second task for a ticket that already has one.

## Reading the report

```bash
opsctl capture-rules report --since 168h
```

What to look for, in order:

1. **Rules that matched nothing.** Either the rule is wrong or its traffic has not
   arrived. Check against the two landmines above before assuming the engine.
2. **Ambiguous decisions** — more than one rule matched. First-match-wins resolves
   them deterministically, but a rule set that is routinely ambiguous is a rule
   set someone will misread later.
3. **The unmatched pile.** This is triage's inbox, so it should be the messages
   you genuinely cannot route by pattern — not a symptom of a rule that silently
   stopped matching.

## Reading the residue (SWT-23)

The census answers "what rule should exist next" at the granularity a rule is
written at — the sender DOMAIN — with cumulative coverage, and the channel /
no-address split that separates work from noise:

```bash
DATABASE_URL=... go run ./cmd/opsctl capture-rules report --since 0
DATABASE_URL=... go run ./cmd/opsctl capture-rules report --domain sspataro.com
```

`--domain` is the Phase-0 investigation and it is a BLOCKER: no rule may be
written for a domain until its detail has been run and read. It is what turned
`sspataro.com` (518) from a suspicion into `test@sspataro.com` session
notifications (bulk), and `upwork.com` (106) into "Invitation to Interview"
(work — refused).

**The claim gate.** A domain is claimed only when the census shows >= 100
residue messages AND a hand-checked sample of at least 20 of its messages
contains ZERO actionable ones AND it is not on the refusal list. The gate rows
join `docs/evals/residue-actionability.jsonl` as stratum `domain_gate`, so the
gate is auditable and the reading is done once.

**Seeded 2026-08-31** — 24 attribution-only rules, one per gated domain, all
`--project bulk --type sender --priority 5`:

```bash
go run ./cmd/opsctl capture-rules add --project bulk --type sender --pattern linkedin.com --priority 5   --note "SWT-23 bulk: gated 2026-08-31, 20-sample zero actionable"
# ... identically for: jobalert.indeed.com match.indeed.com sspataro.com medium.com
#     mailer.humblebundle.com ss/rs/is.email.nextdoor.com ziprecruiter.com
#     discover/explore.pinterest.com amazon.com nytimes.com nl.mail.washingtonpost.com
#     fastweb.com mail.instagram.com glassdoor.com notifications.monster.com
#     motorola-mail.com emails.cinemark.com ezcontacts.com statuspage.io
#     email.informeddelivery.usps.com
```

**Attribution only, no task in any mode**: every rule above has no
external_system, and `opsctl capture-rules list` prints exactly
`-> attribution only (no external_system, so no task)` for each. That is what
makes them safe to seed while capture is still in shadow — their live behaviour
is identical to their shadow behaviour, and the engine cannot create a task on
them, live or not.

**Measured before/after (2026-08-31).** Residue before the rules: **14,743**
unmatched messages. After seeding and one `--since 0 --all` shadow pass:
**8,703** — 6,040 messages (41%) claimed to `bulk`. Every one of the 24 rules
matched (linkedin.com 707 down to ezcontacts.com 101); a rule that had matched
zero messages would have been disabled and recorded here, not left in place —
the go-live checklist's own rule, applied to this ticket's additions. Tasks
and external_refs counts were unchanged by the pass (20 / 0).

`sspataro.com` (518) was investigated, then CLAIMED — `--domain` showed it is
`test@sspataro.com` machine notifications; the investigation, not the count, is
what earned the rule. It is listed here, above the refusals, so the two do not
read as one list.

**The refusal list** — domains deliberately NOT claimed, with reasons:

- `github.com` (455) — unrouted WORK, not noise: predominantly
  Foundry-Underwriting CI failures plus OSS threads. A "noise" rule over them
  would take work out of triage's inbox and give it nothing.
- `upwork.com` (106) — platform mail including "Invitation to Interview";
  actionable work.
- `google.com` (239) — mixed: security alerts (the most actionable mail in
  the corpus) and marketing under one domain.
- `browardschools.com` (293), `rocketmoney.com` (174) — school deadlines and
  financial notices; exactly what the classifier exists to catch.
- `facebookmail.com` (156) — FAILED the gate: its 20-sample contained a
  new-device login security alert. One actionable in twenty refuses the
  domain.

## Go-live checklist

- [ ] Rules seeded and `opsctl capture-rules list` shows them enabled.
- [ ] At least several days of shadow decisions, and the report read.
- [ ] Every rule you intend to rely on has matched at least once in shadow. A rule
      that has never matched is not "ready" — it is untested.
- [ ] The capture-before-triage ordering holds in every main you deploy.
- [ ] `projects.client_person_id` is gone (migration 0015) and nothing references
      it — the drafts and triage stores resolve the project from decisions now.
- [ ] `--since` set deliberately for the first `--live` run.
- [ ] **Migration 0015 applied to production BEFORE the image ships.** Stricter
      than usual because 0015 DROPS a column: the new code cannot run against the
      old schema and the old code cannot run against the new one. Institutional
      knowledge records a five-migrations-behind incident that came from exactly
      this gap.
      `psql "$OPS_DATABASE_URL" -tAc "SELECT max(version) FROM schema_migrations"`
      must say `0015` before the tag bump.
- [ ] **Going live is a CROSS-REPO handoff.** `CAPTURE_RULES_MODE` lives in the
      CronJob manifests in the kube repo, not here. Flipping it is the kube
      session's change, and it must happen AFTER capture has run in shadow long
      enough to read — and BEFORE triage goes live, per the ordering above.

## Rollback

Drop `--live` — shadow is the default, so rolling back is running the pass without
that flag. Decisions keep being written and nothing new is created. Tasks already created stay — they are real tasks about real tickets, and
deleting them is a separate decision.

There is no schema rollback: 0015 drops a column, and forward-only means
forward-only. Restoring `projects.client_person_id` would be a new migration plus
a backfill from `capture_decisions`, which is a worse position than fixing the
rules.

## Task titles for thread-keyed rules (SWT-31)

A rule whose derived external key is the message's thread key (any
`thread_key_prefix`/`thread_key_contains` rule with no `key_regex` — the Upwork
client rules) no longer puts the raw key in the title. The label is the message
**sender**, falling back to the project name, then the slug, then the key.

Before: `upwork_crm:e2ef9b65-…:room:room_6f162de2… — Hi Salvador,`
After: `Mario Cruz — Hi Salvador,`

Jira-keyed and `body_regex` titles are unchanged, byte for byte. The identity
dropped from the title still lives in the task body (`thread_key:`, `sender:`,
`message_id:`) and in `external_refs`. The five tasks created live before this
shipped were corrected by a one-off psql UPDATE by explicit id (recorded in the
SWT-31 delivery summary) — capture's live claim for those messages is spent, so
no code path can ever re-title them.

## Dismissals are labelled data (SWT-31)

Dismissing a task from the board (`task_dismiss`, human-only) closes it and
writes ONE typed row to `task_dismissals` (reason_code enum + optional note +
who). **`task_dismissals` is the labelled-data store; `task_events` is NOT.**
The status_changed event's prose reason is for humans reading a task's history —
never GROUP BY it, never parse it in a report. The two sanctioned label queries
(dismissals joined back to the classify verdict, and per-capture-rule dismissal
counts) are pinned verbatim in the SWT-31 SPEC and its integration tests.

**A dismissed ticket comes back on new inbound activity (SWT-36).** When a live
pass logs a new INBOUND message onto a task that is `closed` with an OPEN
dismissal (`task_dismissals.reopened_at IS NULL`), it appends the log line as
always and THEN calls `task_reopen` guarded with `{dismissal_id, message_id}`
as `capture:{connector}`. The handler reopens — to the status the task was
dismissed from, else `ready` — only if the message was INGESTED after the
dismissal (`normalized_messages.created_at > task_dismissals.created_at`; the
send time is never read), and stamps the dismissal row
(`reopened_at/_by/_by_message_id`) instead of deleting it. The decision row
stays `task_log`; its reason gains `task N was dismissed (code); reopen
requested against dismissal D`, and the printed stats gain `"reopened"`. Every
reason code reopens (owner's decision). Plain `task_close`d tasks never do.
Shadow reopens nothing. To keep a task down for good after it came back,
dismiss it again: that writes a second, open row.

## Project-name rules (SWT-40 Part A)

A message whose SUBJECT LINE names a project is attributed to it by a data rule,
priority 3. That is below every source-specific rule (tickets 90/50, workspaces
10, bulk senders 5) and above the T0360B84U catch-all (1), so a stronger signal
always wins and marketing mail naming a project stays in bulk.

- **Pattern shape:** `(?i)\A[^\n]*\b<alias>\b`. `body_regex` matches `Subject +
  "\n" + BodyText`, and `\A[^\n]*` pins the match to the first line: the subject
  (for Slack, the channel name). A body mention is left to the routing tier
  (Part B).
- **Aliases are data, never generated from `projects.name`** (`personal`,
  `foundry`, `bulk` are ordinary words). An alias goes in only after a Go-regexp
  export shows zero unexplained out-of-project subject matches. Postgres reads
  `\b` as a backspace, so test in Go, never in SQL.
- **Live since 2026-09-12:**

  | rule | project | criteria | pattern |
  |---|---|---|---|
  | 60 | collaboratory | body_regex | `(?i)\A[^\n]*\b(?:ce)?collaboratory\b` (`\b` does not fire inside "cecollaboratory") |
  | 61 | reengine | body_regex | `(?i)\A[^\n]*\bre-?engine\b` |
  | 62 | collaboratory | sender | `cecollaboratory.com` |

- **Pre-add export, 2026-09-12:** 26,062 inbound messages over 180 days.
  - The collaboratory alias matched 504, with 0 out-of-project matches. All 7
    Rochester "Questions About Collaboratory Activities Integration" messages
    matched, and 40 unmatched mails were newly attributed.
  - The reengine alias matched 0.
  - `cecollaboratory.com`: 13 inbound messages all-time, all unmatched; nothing
    moved out of another project.

## Re-pointing already-decided messages

A new rule never re-decides a message that already has a LIVE decision. The live
pass skips it, and `--all` is refused in live mode. To make a new ATTRIBUTION-ONLY
rule apply to history, run a shadow pass:

```bash
echo "CAPTURE_RULES_MODE=${CAPTURE_RULES_MODE:-<unset, so shadow>}"   # must be unset
opsctl capture-rules run --since 4320h --all
```

It writes a newer shadow row per inbound message in the window. Every
latest-decision reader follows the newest row in any mode, so attribution moves.
A shadow row never creates or logs a task, so this does NOT work for
task-creating rules.

**The 2026-09-12 pass** (`--since 4320h`, 180 days, 26,062 messages considered):

| group | before | after |
|---|---|---|
| collaboratory-named subjects | 40 unmatched + 60 undecided + 92 attributed + 309 task_log + 3 task | 190 attributed + 311 task_log + 3 task, all collaboratory |
| #a-millon inbound, last 180 days | 428 collaboratory (rule 9) + 10 reengine LHH | 428 bulk + 10 reengine LHH (rule 1 unchanged) |

Tasks 72/75/93/94 (the a-millon LHH tasks) were unchanged. Use a window at least
as wide as the history the new rule must cover: 720h missed 15 older unmatched
mails.

## #a-millon → bulk (SWT-40 O4)

`#a-millon` (`C1C1TSLJH` in workspace `T0360B84U`) carries HOC3/HOC4/LlamaSite
deploys, Salesforce incidents and LHH ticket links. Salvador, 2026-09-11: "those
are HOC/LLamasite not my projects. not even ReEngine".

- **Rule 63:** `thread_key_prefix` `slack:T0360B84U:C1C1TSLJH` → `bulk`,
  attribution only, **priority 99**.
- **Why below 100:** an LHH link in a-millon still reaches rule 1 (priority 100)
  and, with Part D, the Jira assignee gate. The ticket decides, not the channel.
- **Why 99 rather than just above 1:** it must also outrank rule 10 (Treetop
  keys, 90) and rule 59 (95). Otherwise a WEB/API/OPS mention in a HOC channel
  would become a collaboratory task.
- **Case-sensitive prefix:** `thread_key_prefix` is a case-SENSITIVE
  `strings.HasPrefix`, and stored keys keep `T0360B84U`. One prefix covers the
  unthreaded key and every `…:C1C1TSLJH:{root}` thread key. Checked 2026-09-12:
  8,559 messages match the prefix, the same count as messages whose raw
  conversation id is `C1C1TSLJH`. No longer conversation id shares the prefix.

## Activity rules (SWT-45)

Two flags on a rule turn its matches into Jira activity. Set them with
`opsctl capture-rules add --revive [--addressed]`; they are stored in
`capture_rules.revive` and `.addressed` (migration 0030). `capture-rules list`
prints them at the end of the key line.

- **`--revive`**: a match is Jira activity (owner decision 1: any activity —
  mentions, comments, assignments, status changes, the close notification).
  - The ticket's task is CLOSED: capture logs the message, then calls the revive
    form of `task_reopen`. The handler reopens it to the status it held (else
    `ready`) only if the message was ingested after the close, and after any open
    dismissal. It then SURFACES the task, so the reconciler holds it instead of
    re-closing it (`docs/runbooks/ticket-status-sync.md`, "Surfaced by
    activity").
  - The ticket has no task: capture creates one, then surfaces it with
    `task_mark_surfaced`.
  - The task is OPEN (ready, delivered, in progress...): capture only logs.
- **`--addressed`**: the match is addressed to him ("X mentioned you on K", "X
  assigned K to you"), which overrides a gated project's assignee check
  (decision 3). It implies activity; pass `--revive` too.

Whether a match overrides is one truth table, `overrides = revive AND (NOT gate
OR addressed)`:

| project gate | `--revive` | `--addressed` | what a match does |
|---|---|---|---|
| off | no | no | today's behaviour: create, or log |
| off | yes | either | revive a closed task, or create; surface it |
| on | no | no | `held` for the SWT-40 Part D gate, unchanged |
| on | yes | no | `held` for the gate, unchanged; the gate's own log never revives |
| on | yes | yes | NOT held: revive or create and surface at capture time, with no Jira lookup |

The tool and migration 0030's CHECKs refuse two combinations:

- `--revive` needs `--external-system` and an explicit `--key-regex` that
  captures the WHOLE ticket key. Without a `key_regex` the key is the pattern's
  first group. That is how rule 10 (`(WEB|API|OPS)-[0-9]+`) keys by PREFIX, and a
  reviving prefix rule would resurrect the catch-all tasks 56/57/60 on every
  mention. Rule 10 can never carry the flag.
- `--addressed` needs `--revive`.

**Rules cannot be edited, and the same pattern cannot be re-added.**
`capture_rules` is UNIQUE on `(project_id, criteria_type, pattern)`, and no tool
changes a rule's pattern, key_regex, priority or flags. A "disable and re-add"
collides on that unique key unless the pattern text changes. So a key regex must
be right the first time: run it in Go over an exported corpus first, because
Postgres reads `\b` as a backspace. The same fact means existing rules (1, 2,
3–5) cannot gain the flags.

**The Treetop notification rule (J2).** Seed it only after the SPEC's blocking
pre-checks 0a–0e pass, including the zero-row own-action check:

```bash
opsctl capture-rules add --project collaboratory --type sender --pattern jira@treetopllc.jira.com \
  --external-system jira --key-regex '^[^\n]*?\b((?:WEB|API|OPS)-[0-9]+)\b' \
  --url-template 'https://treetopllc.jira.com/browse/{key}' --priority 92 --revive \
  --note "SWT-45: Treetop Jira notification mail keys to the ticket in its SUBJECT; revives/creates"
```

- **Priority 92** outranks rule 10 (90), so Jira mail leaves the prefix
  buckets, and stays below rule 59 (95).
- **The key comes from the SUBJECT line only.** The key text is subject + "\n" +
  body, and Go's `^` without `(?m)` is start of text, so `^[^\n]*?` never reads
  the body. A digest with no key in its subject derives no key and is
  attribution only.
- **The connector side (rules 3–5) stays non-reviving.** The Jira poller reads
  whole projects, so a reviving connector rule would turn a comment on ANY
  WEB/API/OPS ticket into a task. The email copy covers every ticket Jira
  considers him involved in, and it is the only copy that revives, so a comment
  that arrives twice revives once.

**Reengine's addressed rule (J4).** Seed it only if pre-check 0d confirmed the
subject shapes. Use the literal wording 0d returned, and a priority above rules
1 and 2 and below 95:

```bash
opsctl capture-rules add --project reengine --type body_regex \
  --pattern '\A[^\n]*(?:\bmentioned you on LHH-[0-9]+|\bassigned LHH-[0-9]+ to you)' \
  --external-system jira --key-regex '\A[^\n]*?\b(LHH-[0-9]+)\b' \
  --url-template 'https://avviato.atlassian.net/browse/{key}' --priority <max(rule1,rule2)+1> \
  --revive --addressed --note "SWT-45 decision 3: LHH mail addressed to him overrides the gate"
```

Rules 1 and 2 are untouched, and everything they match keeps following the
gate. Part D's D-D6 path changes only for mail this rule claims: an "assigned
LHH-n to you" email now creates its task at capture time instead of being held.

**Mentions count too (Salvador, 2026-09-12).** A Slack or GitHub message that
names a ticket key is Jira activity as well. When capture-rule-ticket-keys
replaces rule 10, its successor carries `--revive` with a whole-key key regex
(`\b((?:WEB|API|OPS)-[0-9]+)\b`). Only NEW messages act. A message already
decided keeps its one live decision, and `--all` is refused in live mode. There
is no backfill, so historically mentioned keys do not become tasks
retroactively.

**The cost, read before arming (J10).** Every Jira close sends a notification
email. If the close email arrives before the reconciler sees the close, it only
logs and the task closes normally. If it arrives after the reconciler's close,
it is activity after the close: it revives the task and the reconciler holds it
open. Jira Cloud batches notification mail, so closing a Treetop ticket will
often leave its task back on the board until one hand close, which sticks.

**Counters.** Every `capture_rules:` line prints `"revived"` (closed tasks
revived) and `"surfaced_created"` (tasks an activity rule created and
surfaced), zeros included. The `capture_gate:` line prints both as 0 always,
because the gate never revives or surfaces.

**Rollback.** `opsctl call --tool capture_rule_set_enabled --args
'{"rule_id":<id>,"enabled":false}'` for each activity rule. Tasks already
surfaced stay held until closed by hand, and a hand close sticks.
