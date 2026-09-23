# Reproduction — imap-idle-missed-arrivals (SWT-81)

## Status
**Confirmed, but on a different mailbox than the one reported.** While the watcher
has been live, 11 INBOX arrivals got **no IDLE wake** and waited for a reconcile sweep:
10 on `sspataro@gmail.com` and 1 on `developer@sspataro.com`.
**`salvador@handsonconnect.org` does not reproduce:** 0 misses in 9 arrivals since the
watcher went live. Every handsonconnect delay in the receipt (median 179 s, worst 582 s)
comes from the pre-watcher `*/10` CronJob era that the 24h window still included. The
receipt's example (received 13:34:21Z, "stored ~13:44Z") was first stored at 13:35:01Z (40 s).
The 13:44 time is `raw_source_items.ingested_at` being bumped by a `\Seen` flag change.
Salvador asked to focus on handsonconnect, so he should confirm that the gmail misses are
the bug he means before diagnosis starts.

## Trigger
No input sequence has been pinned. This is production observation: a message is delivered
to INBOX while `connector-google-watch` is running, and for that mailbox no single-account
(wake) pass starts within 90 s of the server's `internaldate`. The message is stored only by
the next 10-minute sweep, or by the next pod's startup sweep.

## Observed behavior
Window: 2026-09-22 20:56Z (watcher first rolled out, 0.7.43) → 2026-09-23 14:10Z. INBOX only.
Classification is by the `sync_runs` `imap` row that first stored each message. Single-account
rows are wakes. They match the pod's `watch: wake <account>` log lines one-to-one; this was
checked against the only surviving pod log, 13:14Z–14:10Z. Rows forming a contiguous
all-account group are sweeps.

| account | n | idle-wake | reconcile, but a wake also fired (race) | **reconcile, no wake** | startup | idle % | median | worst |
|---|---|---|---|---|---|---|---|---|
| developer@sspataro.com | 5 | 4 | 0 | **1** | 0 | 80 | 12 s | 33 s |
| salvador@handsonconnect.org | 9 | 8 | 1 | **0** | 0 | 89 | 23 s | 64 s |
| sspataro57@msn.com | 1 | 1 | 0 | **0** | 0 | 100 | 5 s | 5 s |
| sspataro@gmail.com | 55 | 42 | 2 | **10** | 1 | 78 | 41 s | 804 s |

Every arrival with no wake:

| account | uid | received (UTC) | stored | delay | stored by | watcher pod | previous wake for this mailbox |
|---|---|---|---|---|---|---|---|
| developer | 9447 | 09-23 02:47:04 | 02:47:37 | 33 s | sweep 51494 | 0.7.46 | 00:25:32 |
| gmail | 824722 | 09-23 01:15:16 | 01:17:33 | 138 s | sweep 51373 | 0.7.46 | 00:02:12 |
| gmail | 824726 | 09-23 03:16:43 | 03:17:35 | 53 s | sweep 51537 | 0.7.46 | 02:26:29 |
| gmail | 824730 | 09-23 06:16:25 | 06:17:36 | 71 s | sweep 51782 | 0.7.46 | 05:37:40 |
| gmail | 824731 | 09-23 06:26:17 | 06:27:37 | 81 s | sweep 51792 | 0.7.46 | 05:37:40 |
| gmail | 824732 | 09-23 06:26:29 | 06:27:37 | 69 s | sweep 51792 | 0.7.46 | 05:37:40 |
| gmail | 824752 | 09-23 11:53:07 | 11:59:42 | 396 s | sweep 52258 | 0.7.47 | 11:48:19 |
| gmail | 824753 | 09-23 12:03:55 | 12:09:43 | 349 s | sweep 52279 | 0.7.47 | 11:48:19 |
| gmail | 824754 | 09-23 12:13:06 | 12:19:42 | 397 s | sweep 52290 | 0.7.47 | 11:48:19 |
| gmail | 824757 | 09-23 13:01:12 | 13:14:35 | 804 s* | 0.7.48 startup sweep 52361 | 0.7.47 | 11:48:19 |
| gmail | 824756 | 09-23 13:04:13 | 13:14:35 | 623 s | 0.7.48 startup sweep 52361 | 0.7.47 | 13:03:16 (the late pass, see above) |

\* UID 824757 is higher than 824756, which was received later. It got its INBOX UID after
13:04:13, so 804 s overstates its delay.

Also seen:
- **Race (IDLE did fire, the sweep got there first):** handsonconnect 22625 (13:34:21,
  sweep 19 s later, wake +40 s found nothing), gmail 824729 (+18 s) and gmail 824743 (+36 s).
- **Startup (not counted):** gmail 824750 arrived 11:38:41, 4 s after the 0.7.47 ReplicaSet
  was created.
- **Late single-account pass:** gmail 824755 (13:00:52) was stored by run 52347, which started
  13:03:16, 144 s after receipt, under 0.7.47. That pod's logs are gone, so it can't be
  confirmed as a wake.

**Pattern (observations only):**
1. **By pod lifetime.** All 11 misses fall under the 0.7.46 pod (00:26Z–11:38Z: 6 misses, 24 arrivals
   caught by a wake) and the 0.7.47 pod (11:38Z–13:14Z: 5 misses, 1 clean wake at 11:48:19). The
   0.7.43/44/45 pods (20:55Z–00:26Z, ~3.5 h, 21 arrivals) had 0 misses. The 0.7.48 pod
   (13:14Z–, ~1 h, 9 arrivals as of 14:12Z) has had 0 misses so far.
2. **By mailbox.** 10 of 11 misses are on `sspataro@gmail.com`, which is also the busiest mailbox.
3. **In stretches.** Under 0.7.47, gmail produced no clean wake for 75 minutes after 11:48:19,
   and every arrival in that span (11:53, 12:03, 12:13, 13:01, 13:04) waited for a sweep. Under
   0.7.46 the misses came in stretches (01:15; 03:16; 06:16 + 06:26 + 06:26), and IDLE then
   worked again for the next arrival without a restart (02:01, 03:30, 07:01).
4. **Quiet time alone doesn't separate hits from misses.** Misses came 5–73 min after the
   previous wake for the mailbox. Hits also came after 52–70 min of quiet (09:00, 10:10).
5. **Not arrivals during a pass.** No missed message arrived while any `imap` pass (any
   account) was running. A second message right after a wake pass was caught by the next wake
   every time (00:01:44, 10:42:51, 10:45:02). Messages arriving 4–12 s apart were caught
   together by one wake (11:10:01/05, 14:06:16/27), except 06:26:17/29, which were both missed.
6. **Not message size.** Missed sizes are 39–489 KB; caught sizes are 6–373 KB. Not direction
   either: all misses are inbound.
7. **IDLE never reported a failure.** There are zero `sync_runs` rows with
   `stats->>'phase'='imap_idle'` ever, so no mailbox's IDLE failed as far as the watcher knows.
8. **Wake latency** (receipt → wake pass start): gmail +4 to +45 s, usually 20–30 s;
   handsonconnect/developer +8 to +27 s, one at +60 s.

## Expected behavior
Every INBOX arrival while the watcher is running (outside the first seconds after a pod
start) triggers a wake for its mailbox and is stored within about a minute. The reconcile
sweep catches nothing that IDLE should have caught.

## Reproduction location
`docs/bugs/imap-idle-missed-arrivals_repro.sh`. It is read-only and runs SELECT only against
the production ops db (`~/.pgpass`).

```bash
cd /home/salvo/projects/personal/wt/idlemiss
docs/bugs/imap-idle-missed-arrivals_repro.sh                  # exit 1 now: "REPRO FAILS: 11 ..."
ACCOUNT=salvador@handsonconnect.org docs/bugs/imap-idle-missed-arrivals_repro.sh   # exit 0 (not reproduced)
SINCE=2026-09-23T13:15:00Z docs/bugs/imap-idle-missed-arrivals_repro.sh           # exit 0 (0.7.48 pod so far)
```

The script prints the per-message classification and a per-account summary. It exits 1 if any
INBOX arrival in the window is `reconcile(NO wake)`, 0 if none are, and 2 on a psql error.
`SINCE`, `UNTIL` and `ACCOUNT` (a LIKE pattern) narrow the window. Re-running it later tests
whether the current pod ever misses.

**Why there is no Go test (yet):** the repo has no IMAP fake that speaks IDLE on the wire.
`internal/connector/google/fake_imap_test.go` and `cmd/connectors/google/{idle,watch}_test.go`
fake `MailSource`/`idleDeps` at the interface level, so their `Idle` channel fires whenever
the test says. `imapauth_test.go`'s TCP fake handles only LOGIN/SELECT. The production pattern
is "IDLE for one mailbox is silent for a stretch, without reporting an error", not "a message
during a wake pass". A failing Go test would have to guess the server- or connection-level
mechanism first, which is the diagnoser's job.

## Environment
- `git rev-parse HEAD` at reproduction time: 357dfb3a9cb423145be2769a981e7e4d6eebdccb (branch `fix-imap-idle-missed`, later rebased onto main)
- Watcher: `deployment/connector-google-watch`, running image 0.7.48 since 2026-09-23T13:14Z.
  Rollouts: 0.7.43 at 09-22T20:55:09Z, 0.7.44 at 20:58:45Z, 0.7.45 at 21:32:07Z, 0.7.46 at
  09-23T00:26:39Z, 0.7.47 at 11:38:37Z, 0.7.48 at 13:14:02Z. Startup line:
  `watch: mode=live horizon=720h reconcile=10m idle_refresh=25m pass_timeout=10m accounts=4`.
  `MAIL_IDLE_REFRESH` / `MAIL_RECONCILE_INTERVAL` are not set in the Deployment (defaults).
- `connector-google` CronJob runs `0 */2 * * *` (a sweep at even hours :00; the script labels it `cron`).
- Accounts: 1003 sspataro@gmail.com, 1004 developer@sspataro.com,
  1009 salvador@handsonconnect.org (app_password), 14418 sspataro57@msn.com (xoauth2).
- ops db at schema_migrations 0043.

## Notes
- **What limits going further.**
  (a) Logs from the 0.7.46/0.7.47 pods, the only ones that missed, are only in Loki
  (`infra/loki`). Grafana at `grafana.home.arpa` needs auth, and I did not port-forward,
  because my brief allowed only `kubectl get`/`logs`. Reading
  `{namespace="ops", app="connector-google-watch"}` for 00:26Z–13:14Z would show whether those
  pods logged anything about IDLE between wakes.
  (b) [Resolved by SWT-81's logging, 2026-09-23: `watch: idle …` lines and `imap: idle … ended before we stopped it`.] The watcher logged no IDLE session start, re-issue (`idle_refresh`) or reconnect, and
  writes no `sync_runs` row for them. So "time since the last idle_refresh / session start"
  can't be measured from anything that exists today.
  (c) The current pod hasn't missed yet. The next production miss will be the first one with
  a full log.
  (d) The raw envelope doesn't store Gmail labels (`X-GM-LABELS`), so the label/thread
  dimension couldn't be checked.
- `normalized_messages.created_at` is the first-stored time. `raw_source_items.ingested_at` is
  bumped on flag changes (confirmed on handsonconnect 22625 and 22624), so it is useless for
  arrival latency.
- Heuristic limits of the script: a wake that ends less than 5 s before a sweep starts, on the
  sweep's first account, would be misread as part of the sweep. None was found in this window
  (all groups were 1, 4 or 4+1 rows, apart from one sweep split by a 3.4 s gap, which the
  5 s threshold now handles).

## Outcome (2026-09-23)

Salvador confirmed the investigation continues on the observed misses ("log only schedulle a review in 1
week"): IDLE lifecycle logging shipped; review on/after 2026-09-30 (swb #561) with this script plus the new lines.
