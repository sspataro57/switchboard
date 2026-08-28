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
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
DATABASE_URL="$OPS_DATABASE_URL" opsctl capture-rules run --since 168h
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
DATABASE_URL="$OPS_DATABASE_URL" opsctl capture-rules report --since 168h
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
