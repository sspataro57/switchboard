> Jira: SWT-73

# imap-idle-watch — mail arrives in seconds: the resident IMAP IDLE watcher becomes the live mail path

**STATUS: ANSWERED** (Salvador, 2026-09-22: "move it to every 2 hours to catch misses"). OQ-1 =
**B-reduced**: when the Deployment goes live, `connector-google`'s CronJob is NOT suspended; its
schedule moves from `*/10 * * * *` to `0 */2 * * *` — a safety net that catches a wedged or
crash-looping watcher within two hours, not a co-worker. D3 and the rollout are written for that.

**Evidence status.** Every code and doc fact below was read in this worktree (branch
`ticket-comms-inbox`, at `6fa8f02`) and is cited file:line. Cluster facts come from the handoff
runbooks in this repo and from the kube session's ground truth as relayed by the coordinator; this
spec session ran no `kubectl` and no SQL. Verification Step 0 turns every remaining production
assumption into a read-only pre-check with a stated gate. A gate that fails means stop and re-spec,
not adapt the code quietly.

## Source

Ad-hoc, from Salvador, 2026-09-22, verbatim:

> also spec I don't like rolling imap per 10 minutes. imap ingestow should subscribe so they get
> emails instanly

swb task **#486**.

## Goal

Make the resident `--watch` mode — IMAP IDLE per mailbox plus a reconcile sweep, already written and
already in the image — the **live** mail path, deployed as one `Deployment` in `ops`, so an inbound
email is ingested, normalized and run through the capture rules within seconds of arriving instead of
up to ten minutes later; and give it the three things a resident process needs and does not yet have:
a singleton lock, a bounded pass, and a health signal judged from outside the process.

**Usable alone means:** after one image roll and one manifest change (kube session), sending a mail
to `sspataro@gmail.com` from a phone puts a `raw_source_items` row, a `normalized_messages` row and
its `capture_decisions` row in the ops db within seconds, `kubectl -n ops logs deploy/connector-google-watch`
shows `watch: wake <address> normalized=1` followed by the `capture_rules:` counter line, and — because
capture's announce already wakes pipelined (IK "the pipeline wake-ups") — a client ask on that mail
becomes its own INCOMING task about a minute later instead of at the next `:20`. `/funnel` keeps
showing the google accounts fresh; `curl :8092/healthz` answers `ok`; killing the pod brings it back
with one catch-up pass.

## What exists (code-read, with file and line)

### The resident mode is already written, and is NOT what production runs

- **The flag and the two drivers.** `cmd/connectors/google/main.go:46` defines `--watch`; `:50-56`
  branches into `watchMain` **before** `MAIL_SOURCE` is ever consulted, so **watch mode is IMAP-only
  by construction** — setting `MAIL_SOURCE=imap` on the Deployment is harmless but is NOT what selects
  the path (contrast `:125` in the one-shot path). `watchMain` (`:63-77`) owns its own pool and an
  un-timed context, deliberately: "the one-shot path bounds itself to ten minutes, which must never
  bound a resident process" (`:63-64`, against `:84`'s `context.WithTimeout(…, 10*time.Minute)`).
- **The loop.** `cmd/connectors/google/watch.go:66-113`: one immediate pass, one goroutine per account
  holding IDLE on INBOX (`:88-95`), a `MAIL_RECONCILE_INTERVAL` ticker (default 10m, `:44`), and a
  select over `wake` / `ticker.C` / `ctx.Done()`.
- **The pass is the whole funnel.** `watchPass` (`:115-171`) = `runIMAPIngest` → `google.Normalize` →
  `capture.ObserveOutbound` → `capture.EvaluateRules` through an executor built once (`:80`,
  `newExecutor`, `main.go:265-270`) → `pipeline.AnnounceCaptured`. Byte-for-byte the same sequence as
  the one-shot pass (`main.go:193-227`), minus the calendar phase.
- **IDLE itself.** `internal/connector/google/imap.go:808-866`: `Idle` selects the folder, attaches an
  updates channel, runs `conn.Idle` in a goroutine and signals **once** on the first
  `*client.MailboxUpdate`. "The signal is a WAKE-UP, never a payload" (`:810-812`) — the caller re-runs
  a bounded UID fetch, so a missed or duplicated notification costs a round trip, never a message.
  `idleOnce` (`watch.go:219-292`) bounds each IDLE with `MAIL_IDLE_REFRESH` (25m, `:42`, under RFC
  2177's 29m) and re-issues.
- **Backoff and visibility.** `watchAccount` (`:173-216`): per-account goroutine, exponential backoff
  with real jitter (`:191-196`), floor 5s, ceiling 5m, and on every failure a `sync_runs` row with
  phase `imap_idle`, status `error` (`:200-202`). One mailbox's failure cannot stop the others.
- **Tests: there are none.** `grep` for `runWatch|idleOnce|watchPass` finds exactly two Go files, both
  non-test: `cmd/connectors/google/{main,watch}.go`. `internal/connector/google/imap_watch.go` — the
  file `docs/tickets/imap-mail-connector_SPEC.md:450` said would hold the loop, with an
  `imap_watch_test.go` beside it — **does not exist**; the loop was written in `cmd/` instead and no
  test came with it. `cmd/connectors/google` has five test files (`mailsource_test.go`,
  `calendarsource*_test.go`, `pipedream_calsource*_test.go`, `msoauth_integration_test.go`) and none of
  them touches watch mode. Treat SWT-11's "it works and is tested" as **half true**: `Idle` and the
  ingest path below it are covered; the resident loop is not.

### Why it is not deployed (the honest answer)

Two records, and they say different things — neither says "never":

1. **Shelved at SWT-11 delivery, 2026-07-31, as a deferred runtime-shape decision.**
   `docs/runbooks/HANDOFF-kube-swt11.md:66-73`, under "Deliberately NOT in this handoff": *"Watch mode
   (`--watch`) is not deployed. It works and is tested, but it would be switchboard's first
   long-running connector and wants its own decision about runtime shape. The `*/10` CronJob does the
   same work on a schedule."* It then lists what a later deploy would need: `MAIL_IDLE_REFRESH`,
   `MAIL_RECONCILE_INTERVAL`, and the per-account advisory lock that keeps it from racing a CronJob.
   **That deferred decision is this ticket.** Note the premise has since expired: switchboard's first
   long-running workload shipped the same day (`deployment/dashboard`, IK "Deploy"), and `orchestratord`
   (SWT-41) and `pipelined` (SWT-40 Part E) followed — so the runtime shape, health probe, `Recreate`
   strategy and image discipline all have in-repo precedent now.
2. **A factual correction, 2026-09-18, by the kube session.**
   `docs/runbooks/HANDOFF-kube-microsoft-oauth-mail.md:46-52`: an earlier draft of that file named a
   "watch Deployment"; *"There is no such workload. Namespace `ops` runs eleven: deployments dashboard,
   orchestratord and pipelined, and cronjobs classify-{personal,promote,residue} and
   connector-{gcal,google,jira,slackweb,upworkcrm}. The `--watch` mode (`idleOnce`, IMAP IDLE) exists in
   the binary but is not deployed, so the locking it needs is dormant code, not a live path. If it is
   ever deployed, it needs this variable too."* That is a correction of a **factual claim about the
   cluster**, plus a standing warning that the watch-mode locking has never run in anger. It is not a
   prohibition, and it names the condition for deploying (`MS_OAUTH_CLIENT_ID` on the workload).

Two other documents already assume the watcher exists and are, today, **wrong or premature** —
this ticket makes them true, and they are listed so the handoff can be checked against them:
`docs/runbooks/calendar-availability.md:197` ("production mail runs in the watch loop, which has no
calendar phase" — today prod mail runs in the CronJob, whose inline calendar phase is a no-op, see
below) and `docs/runbooks/imap-mail-connector.md:91-93,136-138` (describes `--watch` as if resident).

No evidence anywhere that watch was blocked by the MSN mailbox, by the calendar phase, or by the
10-minute deadline: SWT-66 shipped `MS_OAUTH_CLIENT_ID` guidance for "**both** workloads that resolve a
credential: the connector CronJob and the watch Deployment" (`docs/runbooks/imap-mail-connector.md:236-238`),
i.e. it was written to be watch-compatible.

### What production runs today

- CronJob `connector-google`, `*/10`, one-shot `google` with `MAIL_SOURCE=imap` and
  `MS_OAUTH_CLIENT_ID` pinned. Siblings: `connector-jira` `*/15`, `connector-slackweb` `*/30`,
  `connector-upworkcrm` `*/5`, `connector-gcal` 11:00/17:00 (`--calendar-only`, `CAL_SOURCE=pipedream`;
  `docs/runbooks/calendar-availability.md:149-160`, cut back from `*/20` — memory: "Calendar sync waits
  for Oct 1"), `classify-promote` hourly at `:20` (`--lane personal`), `classify-personal` `*/30`.
  Deployments: `dashboard`, `orchestratord`, `pipelined`
  (`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`,
  `docs/runbooks/HANDOFF-kube-inquiry-quoted-history.md:20`). **Step 0g re-confirms all of this before
  anything is changed.**
- **Four mailboxes.** Three `provider='google'` app-password rows (`salvador@handsonconnect.org`,
  `sspataro@gmail.com`, `developer@sspataro.com` — `docs/runbooks/imap-mail-connector.md:8-15`) and one
  `xoauth2` row (`sspataro57@msn.com`, SWT-66). All four are what `google.ListIMAPAccounts` returns
  (`cmd/connectors/google/mailsource.go:73`).
- **The CronJob's calendar duty is a no-op.** `calendarPhaseRuns` is true for the imap source
  (`cmd/connectors/google/calendarsource.go:14-21`), but `runCalendarIngest` selects accounts by
  CREDENTIAL (`:38-53`) and IK records that all three google rows have a NULL
  `refresh_token_encrypted` and empty scopes (CLAUDE.md build-order step 7). So the pass prints
  `calendar: no google accounts with OAuth calendar credentials` and writes no `sync_runs` row.
  Calendar ingestion belongs to `connector-gcal` over Pipedream. **Suspending `connector-google` costs
  the calendar nothing**, today.
- **The per-account advisory lock is the anti-race.** `runIMAPIngest`
  (`cmd/connectors/google/mailsource.go:86-123`) takes `sink.LockAccount` **before** opening the source
  and releases it only after `IngestIMAP` returns and the connection is closed; `idleOnce`
  (`watch.go:236-263`) takes the same lock around the credential mint and releases it before going
  idle. A second pass counts `accounts_busy` and skips.
- **Capture mode is env-driven and fails safe.** `capture.RulesMode()`
  (`internal/capture/rules_store.go:165-176`): anything that is not exactly `live` is SHADOW.

### The latency chain after ingest (already event-driven, except one lane)

`pipeline.AnnounceCaptured` publishes `ops/pipeline/captured` iff the pass committed at least one
decision, and the static subscriber table routes it (`internal/pipeline/contract_test.go:219`):
`captured` → `gate`, `inquiry`, `route`; `inquiry` → `inquiry_promote`; `route` → `route_apply`;
`routed`/`gated` → `inquiry`. `pipelined` runs all five in prod. So **from an IDLE wake to an ask
becoming its own task is already push all the way**, bounded by the local model's ~7.2 s/message
median (IK, residue lane) — no change needed in this ticket. SWT-72's activity mark rides inside
`capture.EvaluateRules` (`rules_store.go` `actionTaskLog`), so it becomes near-instant for free.

**Not woken: the personal lane.** `cmd/pipelined/main.go:63-69` implements five stages and none of them
is the personal actionability lane; `classify-personal` (`*/30`) and `classify-promote` (`:20`) stay on
cron. Out of scope here (see "Out of scope"), stated so nobody reads "instant mail" as "instant
receipts triage".

### The pieces a resident process needs and does not have

- **No singleton lock.** `docs/tickets/imap-mail-connector_SPEC.md:720-721` (decision 16) said watch
  mode should take one, and suggested `0x51570010`; `grep 0x5157` finds **no such constant anywhere**.
  Today two watchers would both IDLE every mailbox and merely contend on the per-account locks.
- **No pass bound.** `watchMain` runs on `context.Background()` (`main.go:66`), and `client.DialTLS`
  (`internal/connector/google/imap.go:339`) sets no dial or read deadline. A wedged fetch stalls the
  loop forever with nothing to notice — the CronJob's 10-minute context is exactly what covers this
  today.
- **No health signal from outside.** IK, SWT-41 landmine: *"A daemon a ticket depends on must be in the
  `Dockerfile` build line …, have a manifest, and have a health signal judged from OUTSIDE the
  process, or the ticket is not delivered."* The binary IS in the build line already
  (`Dockerfile:16-17`, `./cmd/connectors/...`), so no Dockerfile change; the other two are this ticket.
- **A named config landmine.** `docs/runbooks/HANDOFF-kube-jira-activity-revive.md:60`: *"A google
  `--watch` loop, if one is ever deployed, only logs that error and prints a zero counter line on every
  wake, so it looks alive while capturing nothing"* — a positive `CAPTURE_RULES_SINCE` below
  `MinLiveRulesHorizon` (2h). Add `CAPTURE_RULES_MODE` unset (= shadow) to the same family of silent
  no-ops.
- **`imap_idle` is an error-only phase.** `watch.go:200-202` writes `imap_idle` rows on failure only,
  and `/funnel` groups by `stats->>'phase'` with `last_ok = max(finished_at) FILTER (status IN
  ('ok','partial'))` (`internal/dashboard/funnel.go:213-228`). A phase with only error rows displays
  **`never`, forever** — the same cosmetic trap `imap_refetch` already has
  (`docs/runbooks/imap-mail-connector.md:195-199`). Note `funnel.go:34-39` already writes its 3h
  staleness constant in terms of "the resident mail watch loop".

## Decisions

### D1 — Deploy the existing loop. No new binary, no new mode, no rewrite

`google --watch` is the mechanism Salvador asked for, it is in the image, and every behavioural line
of it (wake-not-payload, sweep-anyway, never-exit-on-transient) was argued in SWT-11 and still reads
correctly. This ticket adds an operational skin: singleton lock, pass bound, health endpoint, startup
validation, account-set refresh. `watchPass`'s body, `runIMAPIngest`, `Normalize`, `ObserveOutbound`,
`EvaluateRules` and `AnnounceCaptured` are **not** touched.

### D2 — Deployment `connector-google-watch`: 1 replica, `strategy: Recreate`, no Service

Same shape as `orchestratord` and `pipelined` (IK "Deploy"; `docs/runbooks/HANDOFF-kube-inquiry-promote.md:16`).
`Recreate` and not `RollingUpdate`: during a rolling update two pods would hold four extra IMAP
connections and, worse, both would be resolving the MSN credential — see D5. No Service and no Ingress:
`/healthz` is for the kubelet, not for a human (port-forward when a human wants it).
`terminationGracePeriodSeconds: 120`, so a pass in flight gets to finish or be cancelled cleanly
(SIGTERM already cancels the context, `watch.go:67`).

### D3 — The CronJob keeps running, every 2 hours, as the safety net (OQ-1 = B-reduced)

Salvador, 2026-09-22: "move it to every 2 hours to catch misses." With the Deployment live,
`connector-google`'s tick does byte-identical work to the in-process reconcile sweep, on the same
accounts, taking the same per-account locks — so at `*/10` every tick would duplicate the sweep or
count `accounts_busy`. At `0 */2 * * *` it is a net, not a co-worker: a wedged or crash-looping
watcher costs at most two hours of latency and never a loss of ingestion, which is the failure mode
cron is genuinely good at. Its calendar phase is a no-op (see "What exists").

What this costs, and what makes it safe: two processes can hold the same per-account lock in turn,
so an every-2h tick that lands during a watch pass counts `accounts_busy` and does nothing — fine.
The MSN refresh token is redeemed by two processes; D5's analysis (one connection per lock
acquisition) is what makes that safe, so criterion 21's overlap test and criterion 8/22 are
LOAD-BEARING, not regression guards. And the two workloads must carry the SAME env (the parity
table in the rollout): drift in `CAPTURE_RULES_MODE`, `CAPTURE_RULES_SINCE` or
`MAIL_MAX_MESSAGE_BYTES` would give two behaviours on one mailbox depending on which process won
the lock. The rollout's parity check is therefore a gate, not advice.

Rollback of the whole ticket is: scale the Deployment to 0 and put the CronJob back on `*/10` —
one patch each, no image.

**Not proposed either way:** narrowing `connector-google` to `--calendar-only`. `connector-gcal` already
owns the calendar phase with the Pipedream transport and its own schedule; a second calendar workload
would race it for the same `calendar_sync_token` cursor key — exactly what
`calendarsource.go:14-21` exists to prevent.

### D4 — A process singleton lock, `lockkeys.MailWatch`, with standby rather than crash

New constant in the import-free `internal/lockkeys` (beside `Orchestrator`, `lockkeys.go:11`), value
**`0x5157_0010`** — SWT-11 decision 16's suggestion, free today (`grep 0x5157` across the repo finds
0005 orchestrator, 0006 triage, 0007 google per-account namespace, 0015 capture, 0021 promote, 0022
classify, 0023 ticketstatus, 0028 calendar booking; the repo-wide collision scan is
`internal/classify/structure_test.go:579-613`, and it walks `internal/` only — which is why the constant
lives there and not in `cmd/`).

Acquired at startup on a dedicated pooled connection, held for the process lifetime, `Alive`-checked on
every reconcile tick — the `internal/orchestrator/engine.go:215-265` shape, re-spelled locally in
`cmd/connectors/google` (a connector must not import `internal/orchestrator`).

**Two deliberate differences from orchestratord.** (1) Failing to GET the lock is **standby, not exit**:
log once, answer `/healthz` 503 `standby: another watcher holds the lock`, retry every 15 s. A node
drain can leave the old pod terminating while the new one starts, and a crash-loop there would be
self-inflicted. (2) LOSING it (a CNPG switchover kills the connection silently) exits non-zero, so the
kubelet restarts the pod — same as orchestratord, for the same reason.

The singleton lock is about watcher-vs-watcher. It does **not** stop a CronJob or a hand-run one-shot
pass; the per-account locks do that, and they already work (`mailsource.go:87-103`).

### D5 — The MSN mailbox: the per-account lock makes side-by-side SAFE, but only by an invariant, so aim for one process

Microsoft rotates the refresh token on every redemption and switchboard stores the new one
(`internal/connector/google/credential.go:29-33, 60-71, 89-96`). Two processes redeeming the same stored
token is how the loser gets `invalid_grant` — an alarm that fires on ordinary traffic.

**What the lock actually covers.** `pg_advisory_lock` is database-wide, so the per-account lock
serializes across PROCESSES, not just goroutines:

- the ingest pass holds it from before `OpenIMAPSource` until after `src.Close()`
  (`mailsource.go:92-123`), so every mint that pass makes — the eager one and any re-mint through the
  `AccessToken` closure — happens under the lock;
- `idleOnce` holds it across `OpenIMAPSource` only (`watch.go:236-263`) and releases before going idle,
  which is correct **because that cycle makes exactly one connection** (`src.Idle` consumes the eagerly
  minted token), a property the code states in a comment at `:257-263` and nothing enforces.

So: mint-versus-mint is serialized, and the stale token each process holds in its closure is discarded
when its source is closed and re-read from the db on the next cycle. **Side-by-side operation is
genuinely safe today** — and it is safe by a comment, not by a test. Two consequences:

1. **A test pins the invariant** (criterion 8): a watch cycle must open at most one IMAP connection per
   account per lock acquisition. If someone later fetches inside `idleOnce`, the rotation race comes
   back intermittently, and its symptom is a false revoked-consent alarm on the one mailbox that cannot
   be re-consented from a script.
2. **The design still aims at exactly one process per account** (D3 = A). "Safe but only by an
   invariant" is the reason the recommendation is to suspend the CronJob rather than to rely on the
   lock.

### D6 — Every pass is bounded: `MAIL_PASS_TIMEOUT`, default 10m

`watchPass` runs under `context.WithTimeout(ctx, MAIL_PASS_TIMEOUT)`. 10 minutes is not a new number: it
is the one-shot path's own bound (`main.go:84`), proven in prod on these mailboxes every ten minutes for
two months. A pass that exceeds it is cancelled, logged with its `why` string, counted, and the loop
continues — cancellation is not an exit (`watch.go:36`, rule 3). The cursor discipline makes a cancelled
pass safe: it advances only after a complete folder pass (`docs/runbooks/imap-mail-connector.md:126-128`),
so a truncated pass re-fetches rather than skips.

Unbounded-by-design today; this is the single change that most reduces the risk of moving off cron,
because cron's per-run process death is what covers a wedge in production right now.

### D7 — Health: `GET /healthz` on `:8092`, judged on completed passes and the lock; NOT on IDLE

`MAIL_WATCH_HEALTH_ADDR`, default `:8092` (hooksd owns `:8090`, orchestratord `:8091` —
`cmd/orchestratord/structure_test.go:197-203`). The handler is `newHealthHandler`'s twin
(`cmd/orchestratord/main_test.go:14-17`): **200 `ok` iff a `watchPass` COMPLETED within 3 x
`MAIL_RECONCILE_INTERVAL` AND the singleton lock is `Alive`; else 503 with a one-line reason and
nothing else.** `livenessProbe` on it, `initialDelaySeconds` generous enough for the initial catch-up
pass (60s), `failureThreshold: 3`.

**Deliberately not in the health verdict:**

- **IDLE state.** One mailbox in backoff must not restart the pod: a restart cannot fix
  `invalid_grant`, and restarting would thrash the three healthy mailboxes. A failing IDLE is visible
  as `sync_runs` error rows (D9) and, if the account's ingest is failing too, as the `imap` phase on
  `/funnel`. Failure isolation is the goal (`watch.go:173-216`), and health that keys on any account
  contradicts it.
- **Mail volume.** A quiet mailbox is not a sick one. The reconcile sweep completing is the liveness
  fact; whether it found anything is not.

No MQTT heartbeat and no `ops/workers/…` topic: `docs/tickets/imap-mail-connector_SPEC.md:380-384`
already decided that fleet topics are the worker contract and this connector is not a worker. That
stands.

### D8 — Startup refuses a configuration that would make every pass a silent no-op

One resolution at startup, before the first pass, of exactly what the pass will use, and one log line:
`watch: mode=live horizon=720h reconcile=10m idle_refresh=25m pass_timeout=10m accounts=4 health=:8092`.
The process **exits non-zero** when `capture.RulesConfig{…}.normalize()` errors (the sub-2h live horizon
of the `CAPTURE_RULES_SINCE` landmine) — under cron that is a failed run an operator sees; in a resident
loop it is a pod that logs an error on every wake and looks alive while capturing nothing. It does
**not** refuse `mode=shadow`: shadow is a legitimate configuration, and `RulesMode`'s fail-safe
(`rules_store.go:165-176`) must not be inverted. It prints it, and the handoff's env-parity check
(criterion 24) is what catches the manifest that forgot `CAPTURE_RULES_MODE=live`.

### D9 — A recovery run row, so `imap_idle` cannot read `never` forever

Keep the failure row exactly as it is (`watch.go:200-202`). Add ONE row: on the first successful cycle
**after** a failure — the existing `backoff = backoffMin` reset point (`:214`) — write an `imap_idle`
`sync_runs` row with status `ok`. Consequences: an account that never fails has no `imap_idle` rows at
all and no phantom phase on `/funnel`; an account that fails and recovers shows a fresh `last_ok`; one
that fails and stays broken shows a stale-or-never `imap_idle`, which is the truth. No row per wake and
no row per refresh — at 25m refresh that would be ~230 rows/day for nothing.

### D10 — Watch the accounts table, not a snapshot of it

`runWatch` lists accounts once (`watch.go:88-95`), so a mailbox onboarded later never gets an IDLE
goroutine until the pod restarts — it would be ingested only by the sweep, silently at 10-minute
latency, which is precisely the complaint this ticket answers. On every reconcile tick, re-list and
start goroutines for accounts not already watched. Existing goroutines are never stopped except by
`ctx` (an account whose row disappears fails, backs off and logs — simpler than a supervision protocol,
and a deleted `source_accounts` row is not a thing that happens quietly).

### D11 — What does NOT change

`--full`, `--overlap`, `--all`, `--normalize-only`, `--calendar-only` and `--account` stay one-shot-only
(`watchMain` takes backfill and account only, `main.go:65-77`): a Deployment cannot be asked for a full
rescan, and backfill/replay/`opsctl mail refetch` keep their own path. INBOX gets IDLE; **Sent keeps the
sweep** (`watch.go:175-177`) — so own-message loop closure (invariant 5) runs at the same <=10-minute
latency as today, not worse. Folder discovery, the size cap, the UID cursors, the UIDVALIDITY rule and
`MAIL_FOLDERS` are untouched.

### D12 — Connection cost, stated in numbers

Steady state per account: **1** long-lived IDLE connection (INBOX) plus **1** transient connection per
pass (`OpenIMAPSource` per account, `mailsource.go:105`, closed at `:122`). Four mailboxes = 4 resident
+ up to 4 transient = **at most 8 concurrent**, versus up to 4 transient today. Google documents a limit
of **15 simultaneous IMAP connections per account**; we use at most 2 per Google account, and Gmail's
other relevant bound — IDLE dropped near 29 minutes — is already respected by `MAIL_IDLE_REFRESH=25m`
(`watch.go:38-42`, RFC 2177). Microsoft publishes no comparable per-mailbox IMAP connection number for
consumer Outlook.com; one IDLE plus one transient is far inside anything observed. Step 0c records what
each provider actually shows.

## Acceptance criteria

### Part 1 — singleton and lifecycle

1. `internal/lockkeys` gains `MailWatch int64 = 0x5157_0010` with a doc comment naming this workload;
   the repo-wide collision scan (`internal/classify/structure_test.go`) stays green and a duplicate key
   turns it red.
2. `runWatch` acquires the lock before the initial pass. Held elsewhere: it logs once, serves
   `/healthz` 503 with reason `standby`, retries every 15 s, and acquires it when the holder goes away
   — without restarting the process and without running any pass meanwhile (no `sync_runs` row, no
   `raw_source_items` write).
3. The lock is `Alive`-checked on every reconcile tick; loss makes `runWatch` return a non-nil error and
   the process exit non-zero (the `os.Exit` stays in `main`, `cmd/orchestratord/structure_test.go:16-19`
   shape).
4. SIGTERM/SIGINT still shuts down cleanly (existing `signal.NotifyContext`, `watch.go:67`): no pass is
   started after cancellation, the lock connection is released, exit code 0.

### Part 2 — bounded pass and health

5. `MAIL_PASS_TIMEOUT` (default 10m, `envDuration`'s defensive parse, `watch.go:51-63`) bounds every
   `watchPass`. A pass that exceeds it is cancelled and logged with its `why`, the loop continues, and
   the next wake or tick runs a fresh pass. A unit test drives a pass that blocks and asserts the loop
   makes progress afterwards.
6. `GET /healthz` on `MAIL_WATCH_HEALTH_ADDR` (default `:8092`): 200 `ok` iff a pass completed within
   3 x reconcile AND the lock is `Alive`; else 503 with exactly one line naming which condition failed.
   Table test over {fresh, stale, never-completed} x {lock alive, lock dead, standby}, pure, no db
   (the `cmd/orchestratord/main_test.go` shape).
7. Health ignores IDLE: with every account's IDLE goroutine in maximum backoff and the sweep still
   completing, `/healthz` is 200. Structure test: the health handler's inputs are the pass clock and
   the lock handle only.

### Part 3 — the MSN invariant and failure isolation

8. **One connection per lock acquisition.** A test over `idleOnce` with a fake `MailSource` asserts
   exactly one `OpenIMAPSource`/connect per cycle and that the per-account lock is released before the
   IDLE begins and after the mint. Adding a second connect inside `idleOnce` turns it red (D5).
9. One account failing to authenticate (fake `invalid_grant`) leaves the other three IDLE-ing, writes
   its `imap_idle` error `sync_runs` row, backs off with jitter, and never fails the loop or
   `/healthz`.
10. D9's recovery row: failure then success writes exactly one `ok` `imap_idle` row at the transition;
    a run of successes with no preceding failure writes none; a run of failures writes one error row
    each (existing behaviour, unchanged).

### Part 4 — configuration

11. Startup resolves the capture config once and exits non-zero with a message naming
    `CAPTURE_RULES_SINCE` and `MinLiveRulesHorizon` when a live horizon is below the floor; it does NOT
    refuse `mode=shadow`.
12. The startup line prints mode, horizon, reconcile, idle refresh, pass timeout, account count and
    health address. A pass that would capture nothing is legible from the first ten lines of
    `kubectl logs`.
13. `MAIL_SOURCE` is documented in `main.go`'s header as irrelevant in watch mode (watch is IMAP-only,
    `main.go:50-56`), and a test asserts `watchMain` never calls `selectMailSource`.

### Part 5 — account set

14. On each reconcile tick the account list is re-read; an account added between ticks gets an IDLE
    goroutine within one tick, without a restart and without a second goroutine for accounts already
    watched (integration: insert a second `app_password` row mid-run).

### Part 6 — nothing else moves

15. `watchPass`'s body is byte-unchanged apart from the context it receives: the same five calls in the
    same order, the same counter lines (`printCaptureRules`, `main.go:253-260`), the same
    `AnnounceCaptured`.
16. The connector still arms **no** `tools.Set*` seam (structure test over `cmd/connectors/google`, the
    `cmd/orchestratord/structure_test.go:89` shape): the watcher cannot send anything (invariant 4).
17. The capture announce client id stays `switchboard-capture-google-{random}` (IK: a shared id kicks
    the other client off the broker, and under OQ-1 = B the CronJob and the watcher announce
    concurrently). Test: two announces from one process use different ids.
18. No migration. No new table, column, MCP tool, dashboard route or policy rule.
19. `Dockerfile` is unchanged (`./cmd/connectors/...` already builds `google`,
    `Dockerfile:16-17`), and a test asserts it.
20. `sync_runs` phases written by the watcher are exactly `imap` (per account, per pass, by
    `IngestIMAP`) and `imap_idle` (D9) — no new phase string.

### Part 7 — overlap with the one-shot path (load-bearing under OQ-1 = B, a guard under A)

21. **Integration:** a watcher holding an account's lock and a one-shot `runIMAPIngest` on the same
    account do not both fetch: the loser counts `accounts_busy`, no duplicate `raw_source_items` row
    appears beyond the content-hash upsert, and the folder cursor ends at the higher of the two
    positions, never lower.
22. **Integration (xoauth2):** with a stubbed Microsoft token endpoint that rotates on every redemption,
    a watcher cycle and a one-shot pass interleaved on the same account both succeed and the stored
    refresh token equals the last one issued. Removing the lock from either call site turns it red.

### Part 8 — docs and handoff

23. `docs/runbooks/imap-mail-connector.md` gains a "Running it resident" section: the env table
    (`MAIL_PASS_TIMEOUT`, `MAIL_WATCH_HEALTH_ADDR` added), the singleton lock, what `/healthz` means and
    what it deliberately ignores, the `imap_idle` phase semantics, and the fact that `--full` and the
    other one-shot flags are not available in watch mode. Line 197 of
    `docs/runbooks/calendar-availability.md` is corrected to say which workload owns calendar.
24. `docs/runbooks/HANDOFF-kube-imap-idle-watch.md` (new) carries: the image tag and digest, the
    Deployment spec in full (command, args, env, probe, strategy, grace period), an **env-parity table**
    between `cronjob/connector-google` and the new Deployment with the exact `kubectl` command that
    prints both, the rollout order, the rollback, and the OQ-1 answer's consequence for
    `connector-google`'s `suspend`.
25. An IK section (Verification Step 6) records: why watch was shelved and what changed, the singleton
    key, D5's "safe by an invariant" analysis, the `CAPTURE_RULES_MODE`/`CAPTURE_RULES_SINCE` parity
    landmine, and the `imap_idle` funnel semantics.

## Data model changes

**None.** No migration. Should a later revision need one (a watcher lease or heartbeat column — not in
this design; the lock is a session lock and health is an HTTP probe), it takes the **next free
number**: `migrations/` currently ends at `0039`, and `0040` may be taken by the comms-inbox ticket
being specced in parallel.

Tables read/written are exactly today's: `source_accounts` (read), `raw_source_items`,
`normalized_messages/_threads`, `sync_runs`, `capture_decisions`, `tasks`/`task_events`/`external_refs`
(through the executor only), `deliveries` (confirmation stamps inside `Normalize`, unchanged).

## API / MCP tool changes

**None.** No tool is added, removed or re-scoped; no profile list changes. The only new network surface
is `GET /healthz` on the pod, for the kubelet, with no Service and no Ingress (D7).

## MQTT topics

No topic is added or changed. The watcher publishes exactly what every connector main already
publishes: `ops/pipeline/captured`, QoS 1, **never retained** (IK landmine), from a per-process client
id `switchboard-capture-google-{random}`, iff the pass committed a decision. `MQTT_BROKER` must be on
the Deployment: unset, `AnnounceCaptured` skips with one log line and the inquiry/gate/route stages fall
back to pipelined's 5-minute sweep — mail is still instant, task creation is not. No `ops/workers/…`
topic, no LWT: this process is not a worker (D7).

## Files likely to touch

- `cmd/connectors/google/watch.go` — the lock, the bounded pass, the account refresh, the recovery row,
  the startup line and validation.
- `cmd/connectors/google/health.go` (new) — `newHealthHandler` + the listener, twin of
  `cmd/orchestratord`'s.
- `cmd/connectors/google/singleton.go` (new) — `TryMailWatchLock` / `LockHandle`, the
  `internal/orchestrator/engine.go:215-265` shape re-spelled locally.
- `cmd/connectors/google/main.go` — the header env block (`MAIL_PASS_TIMEOUT`,
  `MAIL_WATCH_HEALTH_ADDR`, the `MAIL_SOURCE`-is-irrelevant note); `watchMain` wiring.
- `internal/lockkeys/lockkeys.go` — `MailWatch`.
- Tests (new): `cmd/connectors/google/watch_test.go` (loop, pass timeout, account refresh, recovery
  row — the loop has **no** tests today), `health_test.go`, `singleton_integration_test.go`,
  `watch_overlap_integration_test.go`, `structure_test.go` (no `tools.Set*`, no `selectMailSource` in
  watch mode, Dockerfile line).
- Docs: `docs/runbooks/imap-mail-connector.md`, `docs/runbooks/calendar-availability.md` (one line),
  `docs/runbooks/HANDOFF-kube-imap-idle-watch.md` (new), `.claude/INSTITUTIONAL_KNOWLEDGE.md`.

**Deliberately NOT touched:** `internal/connector/google/*` (including `imap.go`'s `Idle`,
`credential.go`, `imap_ingest.go`), `internal/capture/*`, `internal/pipeline/*`, `internal/dashboard/*`,
every other connector main, `Dockerfile`, `migrations/`.

**Not this repo:** `~/projects/personal/kube/switchboard/*.yaml`. The kube session owns manifests; this
session writes the handoff (IK: *kube manifests belong to the kube session*).

## In scope / Out of scope

**In scope:** deploying the existing `--watch` loop as the live mail path, with a singleton lock, a
bounded pass, `/healthz`, startup config validation, account-set refresh, the `imap_idle` recovery row,
their tests, the runbook and the kube handoff.

**Out of scope, each named because it is a tempting bundle:**

- **Making the personal classify lane push.** `classify-personal` (`*/30`) and `classify-promote`
  (`:20`) are not pipelined stages (`cmd/pipelined/main.go:63-69`). Turning them into stages is a real
  ticket (a lock shared with the CronJobs, a GPU budget, a shadow period) and is Future work.
- **IDLE on the Sent folder.** SWT-11 decided INBOX only, on connection count and failure surface, and
  Sent latency only delays delivery confirmation, which no rule waits on.
- **Touching the ingest internals** — batch size, the size cap, `MAIL_MAX_MESSAGE_BYTES`'s pending 25
  MiB roll (`HANDOFF-kube-mail-refetch.md`), folder discovery, the refetch tool.
- **Gmail push over Pub/Sub** (`docs/runbooks/gmail-local-connector.md:27-30`, never shipped). IMAP
  IDLE is the answer for four mailboxes across two providers with no cloud project.
- **Resident modes for the other connectors** (jira, slackweb, upworkcrm). Different providers,
  different polls, no IDLE.
- **A fleet/MQTT presence for the watcher**, a `/funnel` widget for IDLE state, or an alert when a
  mailbox stops listening. D7 and D9 keep it to `sync_runs` plus the probe.
- **Un-suspending or re-scheduling any other CronJob**, and the calendar's Pipedream decision (memory:
  wait for Oct 1).

## Invariants that apply

1. **Raw-first** — unchanged and load-bearing. The IDLE notification is a WAKE-UP; the write path is
   `runIMAPIngest` → `IngestIMAP` → `upsertRaw` into `raw_source_items` (raw RFC822 + content hash)
   BEFORE `Normalize`. Nothing in this ticket reads the notification's contents; a duplicate wake costs
   one no-op pass and a lost one costs latency until the sweep. **Where the raw write happens:**
   `internal/connector/google/imap_ingest.go`, reached only through `cmd/connectors/google/mailsource.go:121`.
2. **One funnel** — no new table and no new row kind. The watcher produces exactly the rows the CronJob
   produces today.
3. **Everything through the executor** — `capture.EvaluateRules` is called with the executor built at
   `watch.go:80` (`newExecutor`, `main.go:265-270`: registry → `tools.Register` → `policy.NewMatrix` →
   `executor.New`), so every task, log and external_ref write is validate → policy → audit. No new
   handler and no side door; the health endpoint performs no work and touches no tool.
4. **Nothing external without a delivery row** — the watcher arms no sender seam (criterion 16): with
   `tools.SetGmailSender` never called, this process cannot send even if a delivery row asked it to.
5. **Own-message loop closure** — Sent stays in the reconcile sweep (D11), so our own sends keep
   re-entering and stamping `sent_external_id` inside `Normalize`/`upsertMessage` at the same latency as
   today. A ticket that moved Sent out of the sweep would break this; this one does not. `ObserveOutbound`
   keeps running after `Normalize` in the same order (`watch.go:143-152`), so a switchboard send is never
   relabelled as sent by hand.
6. **Stealth attribution** — nothing client-visible is produced. No prose, no send.
7. **Orchestrator purity** — `internal/orchestrator` is not imported, not modified and not consulted;
   the singleton lock is a local re-spelling of its idiom, keyed from the import-free `internal/lockkeys`
   exactly so no dependency edge is created (`lockkeys.go:1-6`).

## Sibling patterns to copy

- **Resident process shape, health handler, lock-checked tick, `os.Exit` only in main:**
  `cmd/orchestratord/main.go` + `main_test.go:14-17` + `structure_test.go:186-203`, and
  `internal/orchestrator/engine.go:215-265` for `LockHandle` / `TryAdvisoryLock` (note its
  `sync.Mutex`: the loop and `/healthz` both call `Alive`, and pgx conns are not concurrency-safe).
- **Loop discipline under a broker/DB that may vanish:** `internal/pipeline/stage.go` (`StageLoop`:
  coalescing wake, sweep fallback, `PassTimeout`, lost-lock retry) and `cmd/pipelined/main.go`'s
  shutdown ordering. Read it before writing the pass-timeout code — it is the same problem, solved
  once.
- **Per-account locking and per-account error rows:** `cmd/connectors/google/mailsource.go:86-135` and
  its calendar twin `calendarsource.go:55-109` (one failing account recorded, never aborting the rest,
  the pass failing only if none succeeded).
- **Env knob discipline:** `watch.go:51-63` `envDuration` (an unparseable value falls back, never
  produces a zero interval).
- **Handoff document shape:** `docs/runbooks/HANDOFF-kube-inquiry-promote.md` (a new Deployment with its
  full env table) and `HANDOFF-kube-activity-resurfaces.md` (the "keep the pins" parity list).
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used — nothing is claimed here. **rag-svc HTMX:** not
  used — no dashboard change.

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| Drop the singleton lock acquisition | criterion 2 |
| Exit instead of standby when the lock is held | criterion 2 |
| Skip the per-tick `Alive` check | criterion 3 |
| Remove the pass timeout | criterion 5 |
| Make `/healthz` 200 whenever the process is up | criterion 6 |
| Make `/healthz` fail when any account is in backoff | criteria 7, 9 |
| Open a second IMAP connection inside `idleOnce` | criterion 8 (D5) |
| Release the per-account lock before the mint | criteria 8, 22 |
| Return an error instead of backing off on one account's auth failure | criterion 9 |
| Write an `imap_idle` ok row on every cycle | criterion 10 |
| Accept a sub-2h live `CAPTURE_RULES_SINCE` | criterion 11 |
| Refuse to start in shadow mode | criterion 11 |
| List accounts once at startup | criterion 14 |
| Reorder or drop a call in `watchPass` | criterion 15 |
| Call `tools.SetGmailSender` in this main | criterion 16 |
| Use a fixed capture announce client id | criterion 17 |
| Drop the per-account lock from `runIMAPIngest` | criteria 21, 22 |

## Verification protocol

Run in order. Do not commit before step 4 passes. Capture the exit status of every `go test` separately
from any pipe (IK: *gate commits on test exit status*).

**0. Read-only pre-checks.** SQL inside `BEGIN READ ONLY; … ROLLBACK;` against
`psql -h 192.168.50.49 -U ops -d ops`; `kubectl` reads only. NOT run by the spec session. Paste results
into the delivery summary; assert none of them as a frozen literal in any test (IK: production counts
are not frozen literals).

- **0a. The latency this ticket removes** — per account, over 7 days, inbound imap mail:

  ```sql
  SELECT a.account_email,
         count(*),
         percentile_disc(0.5) WITHIN GROUP (ORDER BY r.ingested_at - m.sent_at) AS p50,
         percentile_disc(0.9) WITHIN GROUP (ORDER BY r.ingested_at - m.sent_at) AS p90,
         max(r.ingested_at - m.sent_at)                                         AS worst
    FROM raw_source_items r
    JOIN normalized_messages m ON m.raw_source_item_id = r.id
    JOIN source_accounts a     ON a.id = r.source_account_id
   WHERE r.raw_json->>'source' = 'imap' AND m.direction = 'inbound'
     AND r.ingested_at > now() - interval '7 days'
   GROUP BY 1 ORDER BY 1;
  ```

  **Gate:** p50 should be roughly half the `*/10` period and p90 near it. A p90 far above 10 min means
  passes are failing or over-running, and THAT is the bug to fix first — a watcher would inherit it.
  Record the numbers; they are the before-picture for step 6.
- **0b. Run health and phases per account, 7 days:** `sync_runs` grouped by
  `account_email, stats->>'phase', status` with `max(finished_at)`. Expect phases `imap` (and
  `calendar`/`imap_refetch` where the runbook explains them) and **zero `imap_idle` rows** — proof the
  resident loop has never run in prod.
- **0c. The MSN mailbox:** `SELECT account_email, auth_type, send_enabled, scopes,
  (refresh_token_encrypted IS NOT NULL) FROM source_accounts WHERE provider='google';` plus that
  account's `sync_runs.error` rows for 14 days (count of `invalid_grant`). **Gate:** a mailbox already
  failing to authenticate must be re-consented (`google-auth add-microsoft`) BEFORE the roll — do not
  debug a new workload against a broken credential. Record the redemption cadence: one mint per pass
  today, i.e. ~144/day at `*/10`.
- **0d. Connection count:** derive it from the code (D12) and corroborate — Google account → Security →
  recent activity, and `kubectl -n ops logs job/<latest connector-google job> | grep -c 'imap dial'` if
  the line is present. Record what each provider shows before the change.
- **0e. Does pipelined already react to `captured`?**
  `kubectl -n ops logs deploy/pipelined --since=6h | grep '"wake"'` — the daemon logs every wake
  (`cmd/pipelined/main.go:232-239`). Compare each wake's timestamp with the `connector-google` job that
  produced it. **Gate:** if wakes are absent, `MQTT_BROKER` is missing on the CronJob and the inquiry
  lane is running on its 5-minute sweep — fix that first, or "instant tasks" will not follow instant
  mail.
- **0f. Broker sanity:** `mosquitto_sub -h 192.168.50.45 -t 'ops/pipeline/#' -v` during one
  `connector-google` tick; confirm the `captured` payload and that nothing is retained.
- **0g. The cluster, before touching it:** `kubectl -n ops get cronjob,deploy -o wide` and, for
  `connector-google`, the full env list
  (`kubectl -n ops get cronjob connector-google -o jsonpath='{range …env[*]}{.name}={.value}{"\n"}{end}'`).
  This is the source of the env-parity table in criterion 24 — especially `CAPTURE_RULES_MODE`,
  `CAPTURE_RULES_SINCE`, `MQTT_BROKER`, `MS_OAUTH_CLIENT_ID`, `MAIL_MAX_MESSAGE_BYTES`, `OPS_TOKEN_KEY`,
  `DATABASE_URL`.

**1. Unit:** `go test ./...`. (The SWT-48 `TestAttributionTrend_*` flake between 20:00 and 24:00 EDT is
pre-existing; re-run with `TZ=UTC` if it fires.)

**2. Integration, on an ISOLATED database** (never prod, never the shared compose `ops`):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_idlewatch"
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_idlewatch?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_idlewatch?sslmode=disable' \
  go test -tags integration -p 1 -count=1 ./cmd/connectors/google/ ./internal/connector/google/
```

Run twice (rerunnable cleanup), then `go test -tags integration -p 1 ./...` once against the same URL.

**3. Mutations:** every row of the table above goes red, then is reverted.

**4. Local smoke against a REAL mailbox** (the "usable alone" gate; this is the step SWT-11's
verification 4 described and nobody has run since):

- `MAIL_SOURCE=imap DATABASE_URL=<isolated> OPS_TOKEN_KEY=… MS_OAUTH_CLIENT_ID=… CAPTURE_RULES_MODE=live
  go run ./cmd/connectors/google --watch` on the workstation, against the real mailboxes. Confirm the
  startup line (D8) names four accounts and the right mode.
- `curl -sS localhost:8092/healthz` → `ok`.
- Send a mail from a phone to `sspataro@gmail.com`. Expect `watch: wake sspataro@gmail.com
  normalized=1` **within seconds**, then the `capture_rules:` line. Confirm in the db: one
  `raw_source_items` row, one `normalized_messages` row, its `capture_decisions` row.
- **IMAP hygiene:** the message is still UNREAD in Gmail's UI afterwards (every fetch is `BODY.PEEK`).
- **Singleton:** start a second `--watch` in another terminal — it must log standby, answer 503 on its
  own health port, ingest nothing, and take over within 15 s of the first being killed.
- **Overlap:** while the watcher runs, run a one-shot `google` pass — it must report `accounts_busy`
  rather than duplicate work, and the MSN account must keep authenticating afterwards (D5).
- **Wedge:** `MAIL_PASS_TIMEOUT=1s` for one run — passes are cancelled and logged, the loop survives,
  `/healthz` goes 503 once no pass has completed for 3 x reconcile.
- `Ctrl-C` shuts down clean (exit 0, lock released).
- Drop the database afterwards.

**5. Deploy — no migration; ONE image tag; the manifests belong to the kube session.**

1. Build and push `192.168.50.20:5000/switchboard:<tag>`; record the digest and the main commit.
2. Roll that tag to every workload as usual (keeping the pins: classify-promote `--lane personal`,
   pipelined `PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`, `MS_OAUTH_CLIENT_ID` on
   connector-google). Nothing behaves differently yet: the new code runs only under `--watch`.
3. Hand over `docs/runbooks/HANDOFF-kube-imap-idle-watch.md`. The kube session creates
   `deployment/connector-google-watch` (D2) with the env-parity table's variables, then — **and only
   after** `/healthz` is 200 and one wake has been observed in its logs — changes
   `cronjob/connector-google`'s schedule from `*/10 * * * *` to `0 */2 * * *` (OQ-1 = B-reduced;
   never suspended).
   **Order matters:** the Deployment first, verified, then the CronJob off. Never the reverse; a gap
   with neither running is a mail outage nobody would notice for ten minutes.
4. **Post-roll smoke:** repeat 0a's query over the following day — p50 should collapse to seconds for
   INBOX mail while Sent stays at sweep latency; `/funnel` shows the google accounts fresh on phase
   `imap`; `kubectl -n ops logs deploy/connector-google-watch --since=1h | grep -c 'watch: wake'` is
   non-zero on a mailbox that received mail; `pipelined` logs a `captured` wake within a second of each
   one.

**6. IK entry** (`## The IMAP watcher is the live mail path`): why watch was shelved in 2026-07 and what
changed; the 2026-09-18 correction and what it did and did not say; `lockkeys.MailWatch`; D5's "safe by
an invariant, so aim for one process per account" analysis with the credential.go citation; the
`CAPTURE_RULES_MODE`/`CAPTURE_RULES_SINCE` parity landmine (a resident loop turns a failed run into a
healthy-looking no-op); `/healthz` semantics and what it deliberately ignores; the `imap_idle` phase and
its recovery row.

**7. Rollback:** `kubectl -n ops patch cronjob connector-google -p '{"spec":{"schedule":"*/10 * * * *"}}'`
and scale `deployment/connector-google-watch` to 0. Mail ingestion returns to `*/10` within ten minutes,
with no data loss: the cursors, the raw rows and the locks are the same in both modes. Nothing about
the schema or the db changes, so there is nothing to un-apply.

## Future work (not this ticket)

- **Push the personal classify lane** (a `personal` pipelined stage woken by `captured`), so receipts
  and actionability follow mail instead of waiting for `:20`. Needs a lock-sharing plan with
  `classify-personal`/`classify-promote` and a GPU budget.
- **IDLE on Sent**, if delivery confirmation ever becomes something a rule waits on.
- **An operator signal when a mailbox stops listening** — today it is `sync_runs` rows plus logs; a
  `/funnel` IDLE column or an `ops/…` notification would be the next step (and see SWT-72's own
  Future work on notification).
- **A dial/read deadline on the IMAP connection** (`imap.go:339` uses `client.DialTLS` with no
  timeout). D6 bounds the pass, which is enough; bounding the socket is the narrower fix.
- **Retire or further reduce the CronJob** once the watcher has run unattended for a few weeks
  (Salvador chose the 2-hour net deliberately; revisit with data, not by default).
