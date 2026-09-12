> Jira: SWT-39

# Reproduction — slackweb-collab-export-stale

## Status
Confirmed, but as a **delay**, not permanent loss. The report said "never lands". Until
2026-09-11 14:00Z that was literally true. Run **34684** (14:00Z) then inserted the
message as raw **77849**, **17h17m** after it was sent and after **34** `ok` runs of 542
had missed it. The reproduction therefore asserts delay: `ok` export runs that happened
while a message already in Slack stayed out of raw. This form still fails on the
incident and on earlier episodes. See Notes for the scope question this raises.

## Trigger
No action is needed: the production state already has it. The Collaboratory/LlamaSite
workspace `T0HPR78RX` (`source_accounts.id = 542`) is exported by CronJob
`ops/connector-slackweb` (`*/30`, image `switchboard:0.7.7`) through the Mac-mini
bridge. asunda45 posted this in DM `D0AUD86LKGA`: "Hey Salvador, it is now ready for
review and merge. I have also validated the changes with Jose, just to be safe.
github.com/treetopllc/gonoble/pull/3872". Source: Screenshot_20260911_094927.png,
"4:49 PM". The raw row since gives the exact time, Slack ts `2026-09-10T20:49:06.665Z`.
The export ran every 30 minutes, reported `ok` each time, and did not pick the message
up until 14:00Z the next day.

## Observed behavior
All read-only against prod, 2026-09-11 ~14:10Z.

**The reported message.** Raw 77849 is `message:D0AUD86LKGA:p1789073346665869`. Sent
2026-09-10 20:49:06Z; first normalized 2026-09-11 14:06:07Z, in run 34684. It went
through **34 `ok` runs** of 542 (21:30Z through 13:30Z) that started ≥30 min after it was
sent, without being seen. Run 34684 also inserted three other late D0AUD86LKGA
messages: raw 77846–77848, sent 19:33–19:36Z, 36 missed runs each.

**The freeze.** It starts at run **33807** (2026-09-10 23:00:01Z), the first run after
the last pre-freeze insert run 33779 (22:30Z). It ends at run 34656 (13:30Z): **30
consecutive `ok` runs**, all `raw_inserted=0` and `conversations_seen=7`.
`messages_seen` was 166–168 and `raw_updated` 25–27:
- 33807–34069 (23:00–03:30Z): 166 / 27 / 146 (msgs / updated / unchanged)
- 34097–34301 (04:00–07:30Z): 168 / 27 / 148
- 34329 (08:00Z) 167 / 25 / 149; 34359 (08:30Z) 168 / 25 / 150
- 34387–34475 and 34568–34656: 166 / 27 / 146; 34510–34540: 166 / 26 / 147

Run **34684** broke it: 8 conversations / 365 messages / `raw_inserted=4` /
`raw_updated=25`.

**The same rows rewritten each run.** As of 34656, every 542 row whose `ingested_at`
fell in the freeze window was last touched by 34656 itself. `ingested_at` is bumped on
update. Those rows are exactly 27: 22 messages plus the conversation row in
`C03MYC6VAJ1`, and 3 messages plus the conversation row in `C045FRJTYNN`. Their Slack
timestamps run 2022-07-01 to 2022-12-02. That fits one fixed set being rewritten each
run, but it is not proven, because only the last touch per row is stored. No
D0AUD86LKGA row, not even `conversation:D0AUD86LKGA` (last touched 2026-09-10 17:06Z),
was touched during the freeze.

**Not only this instance.** Delay for Slack messages since 2026-09-08, same measure:

| account | messages | missed 0 runs | 1–2 | ≥3 | worst |
|---|---|---|---|---|---|
| 539 Avviato | 46 | 29 | 17 | 0 | 1 run (max delay 14h03m) |
| 542 Collaboratory | 57 | 24 | 12 | **21** | **39 runs** (max delay 27h46m) |

The 542 messages that waited through ≥3 runs:
- `D0AUD86LKGA`: 17. 13 were sent 2026-09-08 16:30–18:01Z and first seen 2026-09-09
  20:16Z, 10 missed runs each, in wide run 32234. The other 4 are this incident.
- `C03J2KTN1PD`: 2, worst raw 77722 with 39 missed runs, first seen 2026-09-10 20:06Z.
- `D023E7XSSGG`: 2, 3 missed runs.

Avviato's 14h worst delay coexists with ≤1 missed run. That fits a message sent while
runs were sparse or not `ok`, not a skipped pass. It is noted, not investigated.

**Zero-insert streaks of 542, ≥6 `ok` runs, since 2026-09-08:** 31637–32142 (8 runs),
32366–32575 (8), 32636–33362 (25), 33807–34656 (30).

**Latest job logs.** Both workspaces:
- `connector-slackweb-29818890` (13:30Z): ingest `conversations_seen=23
  messages_seen=1391 raw_inserted=0 raw_updated=29`.
- `connector-slackweb-29818920` (14:00Z): ingest `conversations_seen=24
  messages_seen=1590 raw_inserted=4 raw_updated=26`.

Neither has an error or per-conversation lines.

**Contrast, Avviato (539).** Same runs, 16 conversations, `messages_seen` 1206–1226 and
`raw_updated` 0–537, both varying run to run.

**No DB copy of the Slack message existed before 34684.** Searching all
`raw_source_items` for the text returned 0 rows. The only `normalized_messages` hit,
158555, is a GitHub notification email from `ananthsekar007`, sent 2026-09-10
20:49:00Z, on the same PR #3872 ("This is ready for review and merge"). It is a
different message; it only corroborates the time.

## Expected behavior
An `ok` export of 542 should include every message already in the conversations it
covers. A message sent at 20:49Z should reach `raw_source_items` in the next run or
two, as it does for Avviato (worst: 1 missed run). It should not wait through 34 `ok`
runs while those runs report `raw_inserted=0`.

## Reproduction location
`docs/bugs/slackweb-collab-export-stale_repro.sql` is read-only SQL. It runs in a
`READ ONLY` transaction and rolls back.

```
psql -h 192.168.50.49 -U ops -d ops -v ON_ERROR_STOP=1 \
     -f docs/bugs/slackweb-collab-export-stale_repro.sql
# optional: -v since='<timestamptz>' (default 2026-09-08 00:00Z)  -v max_missed=3
```

Measure: `missed_runs(message)` is the number of `status='ok'` `sync_runs` of the same
account that started ≥30 min after `normalized_messages.sent_at` and finished before
`normalized_messages.created_at`. `created_at` is the stable first-seen time; sink
upserts never rewrite it. `raw_source_items.ingested_at` is deliberately not used,
because it moves on every update. Assertion: every 542 Slack message with `sent_at >=
:since` has `missed_runs < :max_missed`.

Current results:
- **Default window** (`since=2026-09-08`): **exit 3**, `REPRO FAILS (bug present): 21 of
  57 slack messages for account 542 since 2026-09-08 00:00:00+00 were invisible to >= 3
  ok export runs (worst: raw 77722 message:C03J2KTN1PD:p1788998471987859 missed 39 ok
  runs)`. Section 4 prints raw 77849: 34 missed runs, landed in run 34684.
- **`since='2026-09-10 17:00Z'`**: exit 3, 4 of 17 delayed (the incident: 77846–77849).
- **`since='2026-09-10 21:00Z'`**: exit 0, `REPRO PASSES`. That window holds only the
  2 byeluri messages, which landed on time. This confirms the pass branch works.
- **`since='2026-09-11 14:00Z'`**: exit 3 `INCONCLUSIVE`, no messages in the window yet.

After a fix, run with `-v since=<deploy time>` once 542 has new messages. The default
window contains the incident, so the defaults keep failing on history.

Surface: SQL on `sync_runs` + `raw_source_items` + `normalized_messages`. The upstream
surface is the leaf's export output, `SwitchboardExport` in
`slackconnector/src/switchboard/export.ts`:
`workspaces[].conversations[]{id, name, type, url, skipped_message_count, messages[]}`.
Observing it directly means running the bridge, which drives the Slack browser, so it
was not done. It is the surface the diagnoser would need to see which conversations the
leaf lists for `T0HPR78RX` on a frozen run, and what `messages[]` it returns for them.

## Environment
- switchboard worktree `bug-slackweb-collab-export-stale` @
  `4cf29c13d74c57316ef6751c26f7f1376731ac2f`. The deployed connector is image
  `192.168.50.20:5000/switchboard:0.7.7`, not this revision.
- Leaf: the local clone `~/projects/personal/slackconnector` is @ `c938526e`
  (2026-09-09, "Recycle the Slack tab before the renderer stalls"). The revision
  actually running on the Mac mini was not checked, since SSH was out of scope.
- CronJob `ops/connector-slackweb`, `*/30`, not suspended; env `CAPTURE_RULES_MODE`,
  `DATABASE_URL`, `SLACK_WEB_BRIDGE_URL`, `SLACK_WEB_BRIDGE_TOKEN`.
- `source_accounts`: 539 = `t0360b84u@slack-web.local` (Avviato), 542 =
  `t0hpr78rx@slack-web.local` (Collaboratory/LlamaSite).

## Notes
Observations only, nothing about cause.
- **Scope question for Salvador / the diagnoser.** The report is one message that
  "never landed"; it landed 17h late. The same delay shows up in 542 on 2026-09-08 → 09
  (13 D0AUD86LKGA messages, ~27h) and in C03J2KTN1PD (39 missed runs). Is SWT-39 the
  single freeze window 33807–34656, or 542's recurring delay in general? The artifact
  measures the general form; section 2 isolates the freeze.
- Both late batches in D0AUD86LKGA arrived on a run with more conversations than usual:
  32234 (34 conversations, 1892 inserted) and 34684 (8 conversations, 365 messages).
  Between such runs, 542 alternated between 6 conversations / ~47 messages and 7
  conversations / 75–168 messages.
- The message was already missed by 4 `ok` runs **before** the freeze (21:30–22:30Z).
  In that period 542 still inserted: 33779 took byeluri rows 77765/77766. So "message
  missing" and "stats frozen" begin at different times.
- Corrections to the receipt: the zero-insert streak began at **23:00Z on 2026-09-10**,
  not "at least 11:00Z". The receipt's "raw 77683 last ingested 17:36:36Z" is its last
  write time. Its first-seen (normalize) time is 17:36:41Z. `ingested_at` is not an
  insert time.
- The run-at-a-time measure treats `ok` runs only. A message sent during an erroring
  run counts as not missed.
