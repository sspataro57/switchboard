# Reproduction — slack-messages-not-becoming-tasks (SWT-78, swb #521)

**Did qwen drop them? Yes, for the DMs: 16 of the 17 failing DM messages from 2026-09-22 carry a
`qwen3:8b` (ollama, `classify_inquiry`) verdict of `needs_reply=false, ask_kind=fyi`. The 17th was
judged `needs_reply=true`, and the promote stage then recorded nothing for it.** Every one of those
messages was captured, normalized, given a live capture decision and classified. None was lost before
the classifier. In both cases, messages stop at the inquiry classifier (qwen's verdict) or at the promote
stage right after it.

## Status
Confirmed (query-based, against production, read-only).

## Trigger
An inbound Slack message in a DM (a watched one or not) whose live capture decision is `attributed`
(rules 8 and 9, `source_slack_workspace` → collaboratory; the decision reason is "attribution only").
That sends it to the inquiry lane:
- if qwen answers `needs_reply=false`, nothing is created or attached;
- if qwen answers `needs_reply=true`, the message may still get no `classify_promotions` row.

DM messages that capture files onto a ticket (rule 75, Jira key in the body) become attachments. So do
messages that qwen marks `needs_reply=true` and that get promoted.

## Observed behavior — 2026-09-22 EDT (48 inbound Slack messages)

| kind | outcome | n | sample message ids |
|---|---|---|---|
| DM, human | qwen `needs_reply=false` (`fyi`) → nothing | **16** | 403016, 404991, 404992, 404996 … (José 14, Katie 2: 409610, 420472) |
| DM, human | qwen `needs_reply=true` (`scheduling`), **no promotion row, no task** | **1** | 403664 (Katie, 11:56) |
| DM, human | qwen true → promote attached | 3 | 402504, 402505 → #464; 405014 → dismissed #155 (reopen requested) |
| DM, human | task | 2 | 406195 → #497, 407656 → #501 |
| DM, Jira app (D01EJRX6P45, D023E7XSSGG) | capture rule 75 task_log | 14 | 402937, 402496 … |
| channel | rule 63 → bulk (C1C1TSLJH), never classified | 8 | 399982, 403037 … (group "buenos días" greetings) |
| channel | qwen `needs_reply=false` (C03J2KTN1PD) | 3 | 404066, 412536, 430268 |
| channel | task (rule 75) | 1 | 418307 → #516 |

Human DMs: **17 of 22 failed** (José Garcia DSAV4HJ2F: 16 in, 2 outcomes; Katie D04F7LXRB8B: 6 in, 3 outcomes).
Every qwen verdict in the window is `ollama/qwen3:8b`. There is 1 `ai_extractions` row per message. No
per-message confidence field and no human-review lane is recorded. `ai_extractions.fields` keys:
`needs_reply, ask_kind, ask, asker, reason, thread_scope, …`. The qwen reason strings on the dropped
DMs are short, e.g. "a social message with no direct question, request, or actionable item" (404991),
and for 403016 "an automated notification or status update".

For 403664 (`needs_reply=true`), no `classify_promotions` row exists and no `audit_events` row names it.
The `pipelined` log for that time (16:15Z) is gone: the pod was replaced at 00:26Z on 09-23. The
post-restart log shows, on every `inquiry_promote` pass, `gated="map[answered:11 …]"`. That count is
logged only; there is no per-message DB record, so I cannot tie 403664 to it from the data.

**The pipeline treats DMs and channels the same.** DMs and channel C03J2KTN1PD go through the same capture
rules: rule 8 for all of Collaboratory, rule 9 for all of Avviato. Then the same `classify_inquiry` lane on
the same model. Over 14 days: DMs 291 extractions (`thread_scope=conversation`), channels 58
(`conversation` 33 / `thread` 25). The data shows no DM-specific path. Channel C1C1TSLJH alone is diverted
by rule 63 (bulk) and never classified.

**It is not one bad day** (same script, other days): 2026-09-21 had 32/44 FAIL (31 in DMs); 2026-09-17
had 32/45 FAIL (27 in DMs). Over 2026-09-09..22, most failing human DM messages are qwen `needs_reply=false`.
Most of the rest are qwen `needs_reply=true` with no task (1–10 per day).

### Capture side (the other direction)
- Every inbound message sent in the window has raw → normalized → a live `capture_decisions` row
  (0 without). Nothing is stuck unnormalized: 0 of 1,166 raw rows ingested in the window.
- Rotation (`phase=slack_web`) ran 44 of 48 half-hours per workspace. All runs are `partial`, and all had
  `budget_exhausted=true`. The watcher log shows rotations skipped as `bridge busy` at 21:28Z and 23:04Z;
  the earlier pod's log is gone. The targeted watch
  (`slack_web_watch`) was live from 13:58 EDT: 77 + 35 ok, and 1 error per workspace at 01:38Z on 09-23
  ("Slack renderer stopped answering").
- Per-conversation reads: every DM ranked 1–8 in either workspace was read 11–44 times. Never read all day:
  Collaboratory **D08L7HCA8NP** (rank 9) and **D022YQ4KNNT** (rank 10). Avviato C05BN1HULFQ was read once.
  The DM rank order matches the last captured message dates: those two last showed activity 2026-05-06
  and 2026-03-17. So **no evidence of uncaptured DM activity**, but the data cannot rule it out for those two.
- `messages_skipped_identity` is non-zero on every rotation, 6–71 per run; summed over the day it is
  539: 521, 542: 1,555. That is cumulative across runs, not distinct messages. Those messages are stored
  nowhere, so which ones they are cannot be determined from the DB. Unknown whether any were human DMs
  from 09-22.
- DM DSA806DHA (Avviato rank 1) is Salvador's self-DM ("Salvadoryou", 3 outbound on 09-22). It is not a gap.

## Expected behavior
Salvador (2026-09-23): "basically all DMs to me are actionable just messages on the general forum need
decision". Every inbound DM or group-DM message becomes a task or an attachment to a task. A classifier
"no reply needed" verdict is not an acceptable DM outcome. Channel messages need at least a recorded
decision.

## Reproduction location
- `docs/bugs/slack-messages-not-becoming-tasks_repro.sh [YYYY-MM-DD]` (default 2026-09-22, EDT day).
  It runs `docs/bugs/slack-messages-not-becoming-tasks_repro.sql` with psql against
  `192.168.50.49/ops`, SELECT only.
- Prints PASS/FAIL per message (ids, senders and conversation ids; no bodies), stage counts,
  qwen verdict breakdown, the coverage gaps and the run bookkeeping. **Exits 1** when there is any FAIL.
  Today: `FAIL: 25 of 48 inbound Slack messages on 2026-09-22 have no task/attachment outcome (17 of them in DMs)`.
- FAIL rule: DM (`D…`/`G…`) with no task/attachment; channel with neither a task/attachment nor a
  classifier verdict. The 8 channel FAILs are rule 63 → bulk (C1C1TSLJH). If a bulk attribution counts
  as the "decision" a general channel needs, those 8 are not part of this bug. The DM count (17) stands
  either way.

## Environment
- HEAD `5c1a0d953a41c041482a5fd34b3ce500b7dd6153`; prod `schema_migrations` max 0042.
- Watcher `connector-slackweb-watch` on image 0.7.44 (pod since 2026-09-22 20:58:45Z). `pipelined` pod since
  2026-09-23 00:26Z; earlier logs of both are gone.
- `slack_watch`: DSAV4HJ2F (José, Avviato) and D04F7LXRB8B (Katie, Collaboratory), both enabled.
- `projects.inquiry_promote_after`: armed only for collaboratory (4), since 2026-09-13. Every failing DM
  message is attributed to project 4.
- Slack accounts: 539 = Avviato T0360B84U, 542 = Collaboratory T0HPR78RX.

## Notes
- Observations only; no source was read.
- A message counts as "attached" when capture wrote `task_log` with a task, or promote wrote `attached`.
  Several of those targets (#155, #464, #501, #497) were closed on 09-22.
- Still needed from Salvador, to confirm this repro covers what he saw:
  1. One or two Slack messages from 09-22 he expected as tasks (conversation + rough time). Needed
     to check whether they are among the 17 above, or were never captured: skipped-identity, or a DM
     outside the captured set such as D08L7HCA8NP / D022YQ4KNNT.
  2. Whether José's DM banter ("Jajajaja", "Si", 13:59–14:08) counts under "all DMs are actionable".
     The FAIL rule currently says yes.
  3. Whether the Avviato general channel C1C1TSLJH (rule 63 → bulk) is the "general forum" that should
     get a decision rather than bulk attribution.
