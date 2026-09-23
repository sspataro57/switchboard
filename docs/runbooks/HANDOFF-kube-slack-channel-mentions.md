# Handoff to the kube session — Slack channel messages reach qwen only on a mention (SWT-79)

Salvador, 2026-09-23: "channels is only when they mention me" / "#a-millon is the general forum, send it
through qwen" / "a-million is generally not collaboratory. we only respond to mentions on those channels".
A Slack channel message now reaches the qwen inquiry lane only when it @-mentions him; #a-millon moves from
`bulk` to its own `a-millon` project. Spec: `docs/tickets/slack-channel-mentions_SPEC.md`.

Image: `192.168.50.20:5000/switchboard:0.7.48`
(`sha256:28843fd2613a4903c7c2aaebbf34f99325a443af1a4ee9253c98926a78a8dfa2`), built from `main` at `d308ba0`.
It includes everything in 0.7.47.

## 0. Already done

- **Migration 0043 is applied to production** (switchboard session, 2026-09-23 13:13Z): the
  `capture_decisions.channel_unmentioned` column with its CHECK, `raw_source_items_ingested_at_idx`, and the
  `a-millon` project row (not armed). The running 0.7.47 binaries ignore all of it; nothing changed yet.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Same rule as SWT-78: capture decisions are permanent and every connector's capture pass decides every pending
message, so all 13 move together (5 Deployments including `connector-slackweb-watch`, and 8 CronJobs). Keep the
pins. SWT-76's check before replacing the watcher pod: bridge `send_queue.waiting == 0` (POST `/status`).
No env var, no manifest change beyond the tag.

## 2. After the roll — done by the switchboard session (message switchboard-06 when rolled)

Arm the a-millon project, then swap #a-millon's capture rule from `bulk` to `a-millon` (add first, then disable
rule 63, so the channel never falls through to collaboratory). Then the smoke checks.

## 3. Rollback

Mildest first: disarm a-millon (`inquiry_promote_after = NULL`), re-enable rule 63 and disable the new rule,
then roll all 13 back to 0.7.47 together. 0043 stays (forward-only; old binaries write the default, which is
today's behaviour).
