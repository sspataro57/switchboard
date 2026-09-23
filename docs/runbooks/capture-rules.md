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

## Done vs Dismiss on the board (SWT-51)

The board has two verbs per row, and they answer different questions:

- **Dismiss** means "this task should never have existed". Its reasons are
  `not_actionable`, `wrong_kind`, `duplicate` and `handled_elsewhere`. It writes
  a `task_dismissals` label.
- **Done** means "this was real work and it is finished". It calls the existing
  `task_close` as `dashboard:{user}`, with the reason `done on the board` or
  `done on the board: <note>`, and writes NO label. A promoted inquiry closed
  this way counts as `true_positive`.

Done renders only on `assignee_type='human'` rows. The template enforces this,
not the executor. `task_close` still accepts `pr_open`/`awaiting_*` on worker
tasks, so a hand-built POST, which has the same power as `opsctl call
task_close`, could strand a worker's claim. On `claimed`, `in_progress` or
`needs_feedback`, `task_close` refuses, and the refusal shows as the board
flash.

**Never press Done on an "Answer feedback #M" task.** Closing it records no
answer: the asking task stays in `needs_feedback`, nothing times it out, and
the worker never resumes. Answer it with `opsctl answer-feedback` instead.
That still works after an accidental Done, because the feedback request stays
`open`.

Undo a mis-clicked Done with `task_reopen`.

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

- `--revive` needs `--external-system jira` (the tool refuses any other system:
  Part D's hold keys on jira, so a reviving github/slack rule would bypass a gated
  project's assignee check; the CHECK asks only for some system, and capture
  treats a non-jira reviving rule as inert) and an explicit `--key-regex` that
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

**His own Jira COMMENTS never revive (J17); his edits can.** Jira can mail him
about his own changes ("Anonymous (JIRA)"). Capture skips a revive, or a
creation, when the ticket's connector thread `jira:{site_host}:{KEY}` holds an
OUTBOUND message (his comment) sent between 10 minutes before and 2 minutes
after the email. The decision reason says `own_action`: on a closed task the
email is only logged (`revive skipped`); for a ticket with no task it is
`attributed` (`no task created`).

- **Comments only.** His field and status edits leave no outbound message (the
  connector stores no changelog), so their Anonymous notices revive or create.
  On prod that is 182 of the 208 Anonymous Treetop emails. The only protection
  is turning off Jira's "Notify me about my own changes", and Verification 0c
  gates J2's seeding on it.
- **A named actor is someone else.** An email whose From names another person,
  `"Katie Evans (JIRA)" <…>`, is decided at once even inside the window around
  his comment (reason `actor named`): no skip, no wait. Only "Anonymous (JIRA)",
  or a From with no "(JIRA)" shape, goes through the window.
- **Deferred, then BLIND.** If connector-jira has not yet polled past the email
  (no `ok` run started after it, or the key's comment rows not yet normalized),
  the message is left undecided: a `capture rules: message N deferred` log line,
  `"deferred"` on the counter line, and a retry every pass. That lasts up to 30
  minutes from ingest. Then the pass decides as if clear (fail-open): the reason
  says the guard ran BLIND, and `"blind"` on the counter line counts it.
- Keys no provider='jira' account's scopes cover (lookup-only LHH) are not
  guarded: nothing stores his comments there.

**`--limit` reads the oldest first.** A hand-run `opsctl capture-rules run
--limit N` takes the N OLDEST pending messages (by sent_at). A deferred message
stays pending, so for up to 30 minutes deferred rows can use up the whole limit,
and the run decides nothing new. Raise N, or wait for the next jira tick.
`--limit` is for the narrow smoke only; no CronJob sets it.

**Live horizon floor.** A live pass refuses a horizon under 2h
(`--since` or `CAPTURE_RULES_SINCE`) with an error. A deferred message writes no
decision row, so a shorter window could let it age out undecided. A value that
doesn't parse, or isn't positive, still falls back to 720h without a word.

**Reengine's addressed rule (J4). NOT seeded:** pre-check 0d returned 0 rows on
prod (2026-09-12). Seed it only if a later 0d confirms the subject shapes. Use the literal wording 0d returned, and a priority above rules
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

## PR review rules (SWT-54)

A `--pr-review` rule turns GitHub PR notification mail into ONE human `ready` review
task per PR he did not author: "Review PR #N — {repo}: {title}". The flag is stored in
`capture_rules.pr_review`, the exclude list in `.exclude_pr_authors` (migration 0035).
`capture-rules list` prints both at the end of the key line. No GitHub token or
webhook is involved; stored mail is the only input.

**What a match does:**
- **Only GitHub's own mail counts (origin check, 2026-09-14).** A pr_review rule
  acts on a mail only when its TOPMOST `Authentication-Results` header is Gmail's
  (`mx.google.com`) and shows `dkim=pass` for `github.com` (`header.d=github.com` or
  `header.i=…@github.com`). Prod has this on 121 of 121 PR-thread mails. Anything
  else, forged GitHub-shaped mail included, falls through to the next rule exactly
  like his own PR's mail. The reason begins `rule R skipped: untrusted GitHub mail
  for PR {key} (…)`, and `pr_author_skipped` does not count it. Authorship evidence
  is read only from trusted mails. A mail is also untrusted when any of
  `Message-ID`, `References`, `In-Reply-To` or an X-GitHub-* header appears more
  than once: a genuine GitHub mail relayed with prepended copies still passes DKIM,
  but the first copy is the attacker's. The reason names the header (`header
  References appears more than once`). We rely on Gmail's dkim=pass plus
  single-instance headers, and do not re-verify signatures.
  - **The PR is bound to SIGNED headers.** GitHub's DKIM does not sign
    `Message-ID` or any X-GitHub-* header (prod `h=` lists, 2026-09-14). So the
    mail's `List-ID` must be `… <{repo}.{owner}.github.com>` for the thread key's
    repo, and its decoded `Subject` must end `(PR #N)` for the thread key's N. A
    mismatch reads `List-ID binding failed: …` or `Subject binding failed: …`.
    Prod: 123 of 123 Treetop PR-thread mails bind.
  - Residuals:
    - an unmodified relay of someone's own GitHub mail is harmless, because its
      root names their repo;
    - a relayer of GENUINE treetopllc PR mail can still spoof the unsigned
      X-GitHub-Reason/Sender/Recipient, suppressing at most one review task.
  - A `"` anywhere in a dkim result makes the mail untrusted.
  - Outside the model: a delivery path where Gmail does not prepend its own
    `Authentication-Results` (e.g. intra-Workspace mail).
  - Accepted: an insider who receives genuine treetopllc PR mail could relay it with
    an ADDED X-GitHub-* header; at most one review task suppressed.
  - `MAIL_SOURCE=bridge` or `gmail_api` stores no RFC822: EVERY PR mail becomes
    untrusted and the rule stops creating tasks.
  - Do not add a github-keyed rule without `--pr-review` on the same threads: it
    keys the same PR and, on an untrusted fall-through, could reopen a dismissed
    review task.
- **The key is canonical.** The key_regex captures the thread root's path,
  `treetopllc/{repo}/pull/{N}`. Capture stores it as the connector's
  `treetopllc/{repo}#{N}` (`github.ParsePRRef` / `github.PRKey`, the ONE spelling).
  The ref's URL is `github.PRURL`. Issue and commit threads derive no key, so they are
  attribution only. This canonicalization applies to EVERY github-keyed rule, not
  only pr_review ones.
- **No task yet (the create branch):** capture reads authorship from the STORED raw
  headers of the PR's mail (`X-GitHub-Reason`, `-Sender`, `-Recipient` in
  `raw_source_items.raw_json`), never from a normalized column:
  - `own`: any mail says reason `author`, or the opening mail's sender is its
    recipient;
  - `excluded`: the opening mail's sender matches an `--exclude-pr-author` entry;
  - `other`: a named sender, or reason `review_requested` with no name;
  - `undetermined`: none of the above. This one creates the task anyway (fail-open).

  `own` and `excluded` FALL THROUGH: the message is re-decided without the rule, so
  his own PR's mail gets exactly today's decision (rule 10's bucket log, rule 6/7
  attribution, or unmatched). The reason begins `rule R skipped: PR {key} authored
  by him (…)`, or `… excluded author ({entry}): …` for an excluded login, and
  `"pr_author_skipped"` counts it.
- **A task exists:** every later mail logs onto it, closed or not. A dismissed task
  is reopened by SWT-36's guarded reopen on ordinary mail. It is NEVER reopened by
  a PR state notice (merged, closed or reopened): that notice is logged, nothing
  else changes (owner decision 2026-09-14).
- **Merge, close or reopen notice.** GitHub's "Merged #N into {branch}.", "Closed
  #N." or "Reopened #N." is checked on the FIRST non-empty body line only; merge
  prose in a PR description never counts. "Reopened #N." only logs (as the first
  mail seen, it creates the task like any mail).
  - On an open task: the notice is logged, then `task_close` runs as
    `capture:{connector}` with the reason "PR #N merged on GitHub (message M)". It
    writes no dismissal label, so it counts as Done, and `"pr_closed"` counts it.
  - On active work (claimed, in_progress, needs_feedback): the close is refused with
    "refusing to close active work". That is a skip, not a pass failure. The pass
    logs it and appends it to the decision's reason, and the task keeps its status.
  - As the FIRST mail seen for a PR: `attributed`, reason "PR already merged/closed;
    no review task". A review of merged work is not work.
  - Shadow closes nothing; its reason says `would close`.

**Refusals** (the tool and 0035's CHECKs):
- `--pr-review` needs `--external-system github` and a `--key-regex`, and is refused
  with `--revive`. The CHECK is fail-closed on a NULL system.
- `--exclude-pr-author` needs `--pr-review`. Each entry is a GitHub login, `*` plus a
  login suffix, or `*[bot]`.
- `--url-template` is refused on any github rule, because the canonical key contains
  `#`.

**Prove a rule before adding it: `opsctl capture-rules try`.** It takes `add`'s flags
plus `--since` (default 720h, `0` = all) and `--show all|wins`. `wins` prints only the
messages the candidate matches: won, skipped (his own PR), or lost to a higher rule.
- It loads the enabled rules plus the candidate (as rule `max(id)+1`), in memory.
- It decides every inbound message in the window with the pass's own decide step,
  gate- and route-resolved ones included.
- It prints each message's CURRENT decision beside the proposed one, a per-PR rollup
  (verdict and evidence, title, would create/log/close, last mail, notice seen), and
  the D9 backfill payloads.

It writes nothing: no decision row, no executor call, no lock. It validates the
candidate with `capture_rule_add`'s own validator first. It REFUSES two things, by
name:
- `--revive`/`--addressed`: try does not simulate activity rules;
- a candidate whose (project, type, pattern) an enabled stored rule already has.
  That candidate could never win, so the run would print an empty backfill. Use the
  output saved from the run BEFORE the add.

**The Treetop rule.** Seed it only after the image carrying it runs on every capture
workload (`docs/runbooks/HANDOFF-kube-treetop-pr-review-tasks.md`). Run `try` with
the same flags first:

```bash
opsctl capture-rules add --project collaboratory --type thread_key_contains --pattern '<treetopllc/' \
  --external-system github --key-regex '<(treetopllc/[A-Za-z0-9._-]+/pull/[0-9]+)@github\.com>$' \
  --priority 91 --pr-review \
  --note "treetop-pr-review-tasks: one review task per treetopllc PR he did not author"
```

- **Priority 91:**
  - above rule 10 (90), so a colleague's `WEB-1234 …` PR becomes a review task
    instead of a bucket log;
  - below J2 (92), rule 59 (95), rule 63 (99), and rules 1/2 (100), so a Treetop PR
    naming an LHH key stays reengine work.
- **Empty exclude list (OQ-1 = b):** bot PRs, Dependabot included, get tasks like a
  colleague's. His only login on prod is `sspataro57` (pre-check 0b). To exclude a
  login later, disable the rule and re-add it with a slightly different pattern
  (F8).
- **Every treetopllc repo attributes to collaboratory (D7).** Issue and CI mail from
  repos other than www/gonoble moves from `unmatched` to collaboratory, attribution
  only.

**Backfill (D9), once.** Only NEW messages act, so PRs already waiting for review get
no task from the rule. The payloads come from the `try` run made BEFORE the add, saved
to a file (`… --show wins | tee ~/swt54-try-pre-add.txt`). A `try` after the add refuses. For
each PR in the saved "D9 backfill payloads" section, check it is still open and still
has no task, then run its three `opsctl call` lines in order:
1. `create_task`;
2. `link_external_ref`, putting the returned id where `TASK_ID` stands;
3. `task_set_source_thread`, with the same id.

The section lists PRs whose verdict is other or undetermined, with no notice seen,
mail in the last 30 days, and no task yet.

**Residuals, recorded:**
- **Two receiving accounts (0f).** The thread-side authorship read covers the pending
  message's own account. Only the opening lookup, by exact Message-ID, crosses
  accounts.
- **`author` has never been observed on prod (0a).** Row 1 is an unexercised path
  until his first Treetop PR draws a comment.
- **A review task can occasionally appear for HIS OWN PR.** Undetermined is
  fail-open by design; prod had 2 undetermined PRs of 55 in 90 days, both someone
  else's. It happens when his first mail on his own PR says reason `mention`, or
  when the `author` mail went to the other account. **Close it as Done**
  (`task_close`). A Dismiss does not stick: SWT-36 reopens it on the PR's next trusted mail that is not a merged, closed or reopened notice.
- **The mixed-version barrier is procedural.** Seed the rule only after
  `kubectl -n ops get cronjob,deploy -o wide` shows the new image on every capture
  workload.
- **A transient `link_external_ref` failure after `create_task`** can give one PR a
  second task on a later notification (the general capture partial-write weakness,
  SWT-50).

**Rollback.** `opsctl call --tool capture_rule_set_enabled --args
'{"rule_id":<id>,"enabled":false}'`. PR mail returns to rules 10/6/7 on the next new
message. Created review tasks stay; dismiss by hand. Disable the rule BEFORE rolling
any image back past SWT-54.

## Closed-task chats resurface (chat-on-closed-task)

SWT-53, `docs/tickets/chat-on-closed-task_SPEC.md`, migration 0034.

**The gap it closes.** Rule 10 (`(WEB|API|OPS)-[0-9]+`) keys by PREFIX, so a
human message naming a Treetop ticket is a `task_log` onto one of the bucket
tasks 56/57/60. When that task is CLOSED, the log line lands where nobody looks,
and the inquiry lane read only `attributed` messages, so the message
disappeared. Reopening the bucket would resurrect a catch-all (SWT-45 J1), so
nothing reopens it.

**The rule (CC3).** On every `task_log`, capture decides `resurface` with a
pure function (`internal/capture/resurface.go`) and records it on the decision
row, `capture_decisions.resurface`. It is true only when ALL of these hold:

- the linked task is `closed` at log time (a `ready`, `in_progress` or
  `delivered` task already shows the log line);
- the winning rule is not activity: a revive, own_action or blind outcome stays
  SWT-45's ("Activity rules" above);
- the task has no open dismissal: SWT-36's guarded reopen owns it;
- the message is not the Jira connector's own copy (channel `jira`, SWT-45 J3);
- the message is not a GitHub PR state notice (merged, closed or reopened) on a
  `--pr-review` rule's PR (SWT-54 D4). A state notice is logged and nothing else
  changes, so the notice that closed a review task never brings it back, even
  with an empty notifier list. Its reason says `PR state notice`;
- the sender is not on the winner project's notifier list;
- the sender is not blank. A message with an empty or whitespace sender has no
  identity, so it fails closed: it is logged silently and its reason says `the
  message has no sender identity`.

The action stays `task_log` and the log line is still appended, exactly as
before. Capture creates nothing else and makes no new executor call. Shadow
records the same value. The inquiry lanes only READ the fact: classify's inquiry
inbox and `classify promote --lane inquiry` admit a message whose latest LIVE
decision is a `task_log` with `resurface=true` onto a task that is STILL
closed, exactly like an `attributed` message. A shadow row never adds a message
through this path, and a newer shadow row of any other action never removes
one. The one accepted exception (owner decision 2026-09-14, the re-point
contract): a NEWER shadow `attributed` row takes precedence, so the message is
admitted as an attributed message under that row's project, with no
`logged_on_closed_task` line, or dropped if that project is not armed. If it passes the model and the gate, it becomes a Holding task on the
message's own conversation, with a body line `logged_on_closed_task: N`
(`docs/runbooks/local-classifier.md`, "Inquiry promotion"). The closed task
stays closed. Each decision's reason says why it did or did not resurface.

**The notifier list (CC4).** `projects.notifier_senders` (TEXT[], default
`'{}'`) holds bot and notification identities per project: a Slack display name
(`Jira`) or a mail ADDRESS (`jira@treetopllc.jira.com`). A sender matches when
an entry, trimmed and case-folded, is EQUAL to the whole stored sender or to the
address parsed from a mail From. It is never a substring match. The `sender`
capture criterion IS a substring match, and that difference is the point: an
entry `Jira` must not swallow a human called `Jiraiya`. A display name such as
`"Katie Evans (JIRA)" <jira@treetopllc.jira.com>` matches only through its
address. Empty entries match nothing. Only capture's `loadRules` reads the
column.

Blank senders: no entry can equal an empty sender, so a blank sender has its
own disqualifier and fails closed (above): it never resurfaces. Slack web used
to omit the author on grouped Jira-app continuation messages, so 0a found 741
such rows in 30 days. Those were decided before 0034 and are never re-decided,
so they never resurface either. Since the connector fix at 17:00Z on
2026-09-14, no new inbound Slack message has had an empty sender (prod,
read-only: 7 new messages, all named).

Read it:

```sql
SELECT slug, notifier_senders FROM projects WHERE notifier_senders <> '{}';
```

Seed it (hand-run, the `inquiry_promote_after` precedent; values from the
SPEC's Verification 0b). **Owner decision 2026-09-14: keep GitHub notification
mail silent**, so `notifications@github.com` stays in, although it also carries
human PR comments. This is the same statement as the handoff's seed step:

```sql
UPDATE projects SET notifier_senders = ARRAY['Jira','jira@treetopllc.jira.com',
  'notifications@github.com','noreply@github.com','no-reply@github.com','no-reply@builds.circleci.com']
 WHERE slug = 'collaboratory';
```

Disarm it: `UPDATE projects SET notifier_senders = '{}' WHERE slug =
'collaboratory';`. That is NOT a rollback. An empty list only WIDENS
resurfacing: bot messages then resurface too. The rollback is reverting the
pipelined image, after which the lanes stop reading `resurface` and the flag is
inert data.

What each decision recorded, last 24h:

```sql
SELECT cd.id, cd.created_at, nm.channel, nm.sender, cd.task_id, cd.resurface, cd.reason
  FROM capture_decisions cd JOIN normalized_messages nm ON nm.id = cd.message_id
 WHERE cd.action = 'task_log' AND cd.created_at > now() - interval '24 hours'
 ORDER BY cd.id DESC;
```

**Deploy order (CC10).** Apply 0034 BEFORE any image built from this branch
runs. A new capture binary selects `p.notifier_senders` and writes `resurface`
on every pass, so on a db without 0034 every connector's capture pass fails.
Then seed the list, BEFORE rolling the images, so the first new pass already
excludes the Jira app. Then roll ONE image tag to every capture writer and
every inquiry reader in ONE apply
(`docs/runbooks/HANDOFF-kube-chat-on-closed-task.md`). An old capture binary on
a 0034 db records `resurface=false`, and a live decision is never re-decided,
so messages captured by an old writer during the roll window are logged but
never resurfaced (the recorded residual). Rebuild opsctl from main before any
hand-run capture pass.

**Counters.** Every `capture_rules:` line prints `"resurfaced"`, zeros
included, in both modes. The `capture_gate:` line prints it as 0 always.

**The gate residual (CC8).** The pipelined gate stage and the route stage
never write `resurface` (default false). A gate `task_log` onto a closed task on
a gated project (reengine) only logs, as before. Gated projects are not
inquiry-armed today, so nothing is lost. A gate-path resurface is future work.

**No backfill (CC9).** Decisions written before 0034 carry `resurface=false`,
and there is no tool to pick up those misses. A shadow re-point
("Re-pointing already-decided messages" above) does not help here, because
the resurface path reads only live decisions.

## Slack DMs are always tasks (SWT-78)

A person's Slack DM or group DM to Salvador never goes to the inquiry lane (qwen). At capture's
attribution-only exit it is decided onto its CONVERSATION's task: `task_log` onto the open one (a human
task with no external ref whose source thread is any thread of that DM), else `task` — one task per
conversation until he closes or dismisses it; the next DM then opens a new one. Channels are unchanged
(attribution → inquiry lane). The Jira app's DMs are unchanged (notifier list). His own messages are never
decided (inbound only). The decision row reads `DM task: …` in its reason and carries no external key.

Messages decided `attributed` before this shipped cannot be re-decided (the live claim is forever).
Backfill them by id:

```
opsctl capture-rules direct-backfill --message 403016,403664 --dry-run   # what it would do, writes nothing
opsctl capture-rules direct-backfill --message 403016,403664             # oldest first; idempotent
```

It runs the same decision and executor calls, writes no capture_decisions row, and logs
`capture: DM backfill (SWT-78) …` on the conversation task for every message (a second run finds that line
and skips). Non-DMs, app DMs and outbound ids are reported as skipped.
If a run dies between `create_task` and its log line, the next run finds the task by its body (the DM
marker plus the `message_id:` line) and finishes it instead of creating a second one.

**Rolling it out (or rolling back).** A live capture decision is forever, and ANY connector's capture pass
decides every pending message. So every capture writer — `connector-google-watch`,
`connector-slackweb-watch`, the one-shot connector CronJobs, `pipelined` if it runs capture, and `opsctl` —
must run the same image, in ONE apply. A DM an old binary decides during the roll stays `attributed` and
goes to qwen. After the roll, sweep the window and backfill whatever it claimed:

```
docs/bugs/slack-messages-not-becoming-tasks_repro.sh $(date +%F)          # FAIL lines with "| dm |" are the DMs to backfill
opsctl capture-rules direct-backfill --message <those ids> --dry-run
opsctl capture-rules direct-backfill --message <those ids>
```

**Known gap (sibling repo):** group DMs are recognised by the raw `conversation.type='group_dm'`. The Slack
leaf types a conversation by its id prefix on targeted/explicit-URL reads (`slackconnector`
`export.ts conversationTypeForId`), so a `C…`-id group DM read that way is stored `public_channel` and
stays on the inquiry lane. Today `slack_watch` holds only 1:1 DMs, so no such read happens; do not add a
`C…` group DM to `slack_watch` until the leaf types it from Slack's own metadata.

## Comm rules (SWT-74, comms-inbox)

A rule armed with `comm_task` turns a PERSON's message it files onto an OPEN task into its own
task in INCOMING — a "comm" to answer, route or dismiss — instead of a log line that surfaces the
ticket task. Everything else about the rule is unchanged: the ticket task still gets the full log
line, plus an ids-only pointer (`capture: comm #N created from this message (message M)`).

```bash
# see what arming WOULD do, before any row exists (shadow, writes nothing)
opsctl capture-rules try --project collaboratory --type body_regex --pattern '\b(?:WEB|API|OPS)-[0-9]+\b' \
  --external-system jira --key-regex '\b((?:WEB|API|OPS)-[0-9]+)\b' --comm-task --show all
# arm a NEW rule
opsctl capture-rules add … --external-system jira --key-regex '…' --comm-task
# arm an EXISTING rule (rules cannot be edited through the tool): one UPDATE, recorded in the ticket
psql … -c "UPDATE capture_rules SET comm_task = true WHERE id = 75"
# disarm — the mildest rollback, no deploy
psql … -c "UPDATE capture_rules SET comm_task = false WHERE id = 75"
```

What does NOT become a comm even on an armed rule (the decision's `reason` names the cause): a
closed target (SWT-45/SWT-53 own it), a blank sender, a sender on the project's `notifier_senders`
(the Jira bot's Slack echoes, GitHub notifications), and `Anonymous (JIRA)` (his own edits). A first
message about a NEW key creates the ticket task, never a comm. `comm_task` is refused with
`pr_review` and without an `external_system`.

Why not arm the Jira connector's own thread rules AND a body-regex rule on the same keys: both file
the same comment (J3's two copies) and you would get two comms per comment. Arm one side per key
shape; pre-check 0a in the SPEC measured which. A rule on a project whose `ticket_assignee_gate`
is on creates NO comms for its jira matches (the gate stage has no comm path).

Routing a comm: on the board, `actions → Attach` with the target task id (the comm closes; the
target gets an ids-only pointer, is NOT resurfaced, and never sees the note). From a session:
`swb match <id>` proposes the task by capture's own rules; `swb attach <id> <target>` routes it.
`opsctl task-match --message N | --task N | --text "…"` is the same read from the shell; a pasted
line can only match subject/body rules and says so.

Counters: `"comm_tasks":N` on every capture line. Watch it for a day after arming ONE rule.
