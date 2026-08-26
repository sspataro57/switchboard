# Handoff → kube session: deploy SWT-18 (Upwork delivery matcher)

Written by the switchboard session, 2026-08-26. Everything in this repo is done,
merged and pushed; what remains is cluster state, which is yours.

**Image is built and pushed — do not rebuild.**

```
192.168.50.20:5000/switchboard:0.4.3
digest sha256:f5e80a4243a3b8757a9cf6990d980ee1b2b9c14ee81c0836413bf9db366dab5f
built from main bb6b0f7 (clean tree)
```

All eight entrypoints verified to start in that image (`upworkcrm`, `jira`,
`slackweb`, `google`, `github`, `dashboard`, `google-auth`, `migrate` — each
fails on its own missing config rather than on a bad binary), and `/migrations`
ships 0001–0014.

## This is a low-urgency bump. Read the "why it can wait" section before scheduling.

## Preconditions (already true — stated so you can skip verifying)

- **No migration.** pg-main is at `0014` and main is at `0014`; the fix is code
  only. Verified today:
  `psql -h 192.168.50.49 -U ops -d ops -tAc "SELECT max(version) FROM schema_migrations"` → `0014`.
  So the IK "merging a migration is not applying it" trap does not apply to this
  one — nothing to apply.
- Ingestion is healthy on the current image: 70,589 raw items, **zero**
  unnormalized, across nine source accounts.
- Current cluster state, read today:

```
CronJob connector-google      */10   0.4.1   (unsuspended)
CronJob connector-jira        */15   0.4.1
CronJob connector-slackweb    0 */2  0.4.1
CronJob connector-upworkcrm   */15   0.4.1
Deployment dashboard                 0.4.2
```

  For the record, since it is not obvious from the tags: `0.4.1` is `d69cc3b`
  and `0.4.2` is `526233b`. There is no code in `0.4.3` that is missing from the
  cluster other than SWT-18 itself.

## What needs doing

### 1. Bump `connector-upworkcrm` to 0.4.3

This is the only workload that carries the fix. One line in
`kube/switchboard/connectors.yaml`, then apply.

### 2. Optionally bump the other four to 0.4.3 as well

They gain nothing functional — SWT-18 touches
`internal/connector/upworkcrm/sink.go` and nothing else — but leaving four
workloads on `0.4.1`, one on `0.4.2` and one on `0.4.3` makes "which code is
running" a three-way question. Your call; uniformity is the only argument for it,
and it is a real one given how the last drift went.

## Why it can wait (and why you should not treat it as urgent)

The bug is real and shipped, but it has never been reachable for harm, and I
measured that rather than assuming it:

- Entire production `deliveries` table: **1 row**, a `jira_comment` from the
  2026-07-12 smoke test, already carrying `sent_external_id`.
- `upwork_chat` deliveries with `status='sent'` and `sent_external_id IS NULL`:
  **0**. There has never been an `upwork_chat` delivery in production.
- The matcher itself HAS executed roughly 1,141 times (outbound upwork messages
  do carry a non-empty `external_message_id`, so it never short-circuits). It
  found no candidates every time, because the delivery side is empty.

It becomes reachable the moment the draft worker and the Upwork assisted tier go
live — which is also when its failure is most expensive, because a silently
unclaimable delivery is invisible by construction. So: deploy it before that
happens, not before anything else.

## What the fix changes, in one paragraph

`confirmUpworkDelivery` is the assisted-tier post-hoc matcher: it binds an
observed outbound Upwork message to the `deliveries` row that produced it. It
compared `left(body,120)` **raw** (so any whitespace the browser round trip
altered made the match fail permanently) and scoped to
`target_ref LIKE 'upwork_crm:{client}:%'` — **every room that client has** — with
recency breaking ties, so a message from one room could claim a delivery sent in
another. It now compares `textmatch.NormalizedPrefix` on both sides in Go and
matches the exact room (`target_ref = thread_key`). No time bound was added, on
purpose: nothing writes `send_attempted_at` for this channel, so the sibling
clause would have been always-true. Full reasoning in
`docs/bugs/upwork-matcher-hardening_DIAGNOSIS.md`.

## Deliberately NOT in this handoff

- **No manifest changes beyond the image tag.** Schedules, suspend flags, env,
  secrets and volumes are all correct as they stand.
- **No new workload.** Orchestrator, triage, drafts, fleetd and hooksd remain
  undeployed, unchanged by this ticket.
- **The dashboard Ingress stays commented out.** OIDC is still unconfigured, and
  the dev-login stub hands a session to anyone who reaches `/dev/login`.
- **No migrate Job.** There is nothing to migrate.

## How to tell it worked

There is no positive signal available in production, and that is expected — with
zero `upwork_chat` deliveries the matcher has nothing to bind. So the check is
that nothing regressed:

```bash
# the run completes and normalizes
kubectl -n ops get jobs | grep upworkcrm | tail -3
kubectl -n ops logs job/connector-upworkcrm-<id> | tail -20

# and the funnel keeps moving (should stay 0)
psql -h 192.168.50.49 -U ops -d ops -tAc \
  "SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL"
```

A real end-to-end confirmation is only possible once an `upwork_chat` delivery
exists. When one does, the row should end up with both `sent_external_id` and
`confirmed_at` set after the next connector run, and a `delivery_confirmed`
event on its task.

## Rollback

Set the tag back to `0.4.1` and apply. There is no schema change and no data
migration, so rollback is exactly that one line — nothing to undo on the
database side.

## One caveat, stated rather than hidden

A `go-reviewer` pass on this diff was requested and has not returned a verdict
yet (the first attempt died on an auth error before reading anything). The code
is merged on the strength of: reproduction proved red before the fix and green
after on the same fixtures, plus the full unit and integration suites green,
22/22 packages. If the review comes back with a finding that changes the code,
you will get a `0.4.4` and this handoff will be superseded — nothing you deploy
now becomes wrong, it would just be one tag behind again.
