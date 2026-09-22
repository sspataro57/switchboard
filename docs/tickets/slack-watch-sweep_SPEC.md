> Jira: SWT-75

# slack-watch-sweep — two cadences on one browser: a per-minute targeted sweep of the conversations that matter, the rotation behind it

**Evidence status.** Every code fact below was read in this worktree (branch `ticket-comms-inbox`, at
`040f490`) or in the sibling leaf repo `/home/salvo/projects/personal/slackconnector`, and is cited
file:line in whichever repo it lives. Cluster and Mac-mini facts come from the runbooks and handoffs in
both repos; this spec session ran no `kubectl`, no SQL, and touched no browser. Verification Step 0
turns every remaining production assumption into a read-only pre-check with a stated gate. **A gate that
fails means stop and re-spec, not adapt the code quietly.**

## Source

Ad-hoc, from Salvador, 2026-09-22, verbatim:

> slack from jose and katie are critical. messages from yersterday went to black hole. Sweep frequecy
> is too low for them. I'll want to look for them quicker than the 15 min

> I want to contanstly sweep jose an katie like very minute ... thew full export can then be half hour
> or so

> build the slack sweep first

swb task **#478**.

## Goal

Give the Slack connector a **watch list** — conversations held as rows in the ops db, not names in code
— and a resident loop that sweeps exactly those conversations about once a minute while the existing
whole-of-Slack rotation continues behind it, so a message from a watched person reaches
`raw_source_items` → `normalized_messages` → `capture_decisions` → the inquiry lane in **1–3 minutes**
instead of the 11–45 minutes measured today; and do it without a second process fighting the Mac mini's
single browser.

**Usable alone means:** with migration 0041 applied, the leaf rolled on the mini, one image rolled, the
`connector-slackweb-watch` Deployment created and **two rows seeded** —

```
opsctl call --tool slack_watch_add --args '{"workspace_id":"T…","conversation_id":"D…","label":"José"}'
opsctl call --tool slack_watch_add --args '{"workspace_id":"T…","conversation_id":"D…","label":"Katie"}'
```

— Salvador sends himself a message in one of those DMs from his phone and, inside ~2 minutes, sees:
`kubectl -n ops logs deploy/connector-slackweb-watch` printing
`slack watch: pass=targeted conversations=2 raw_inserted=1 normalized=1` followed by the
`capture_rules:` counter line; the row present in `raw_source_items`/`normalized_messages`; and — because
capture's announce already wakes pipelined and `promote.InquiryGrace` is `0`
(`internal/promote/inquiry.go:64`) — the ask on the board about a minute after that. `curl :8093/healthz`
answers `ok`. `/sources` lists the two watched conversations and when each was last read. Nothing else
about Slack ingestion changes: the rotation still covers all 51 conversations, the delivery matcher and
the reconciler behave exactly as today.

## What exists (code-read, with file and line)

### switchboard's half

- **One shot, one shape.** `cmd/connectors/slackweb/main.go:42-136` is the whole connector: a 30-minute
  context (`:46`, deliberately — "a full bridge export legitimately runs ~12m"), `CheckPartialStatus`,
  `Ingest`, `Normalize`, `ReconcileUnconfirmed`, `capture.ObserveOutbound`, an executor built at
  `:103-106`, `capture.EvaluateRules`, `pipeline.AnnounceCaptured` (`:134`).
- **The export request already carries a conversation list, and it is a ROTATION HINT, not a cursor.**
  `ExportRequest{Known, BudgetMS, MaxConversations}` (`internal/connector/slackweb/export_request.go:20-24`);
  `KnownConversation{ID, LastSeenTS, Name}` (`:33-37`), where `LastSeenTS` is explicitly *when
  switchboard last saw the leaf READ it*, not the newest message's ts (`:26-32`). `BuildExportRequest`
  (`:96-120`) drops any row failing the leaf's id regexes (`:88-90`) with a warning
  (`ingest.go:130-133`). Defaults: `DefaultExportBudgetMS = 900000` (15 min) and
  `DefaultExportMaxConversations = 60`, whose comment records the cost that governs this whole ticket —
  **"(15–19 s each)"** (`export_request.go:55-63`).
- **`KnownConversations` derives `LastReadAt` from `sync_runs`** — the start of the latest `ok`/`partial`
  run of the last 30 days whose `stats->'read'` or `stats->'unreadable'` lists the conversation
  (`sink.go:30-79`). Every run row is stamped `{"phase":"slack_web"}` at `StartRun` (`sink.go:124-133`,
  the literal is at `:128`).
- **`partial` is not a failure and almost certainly not a bug.** `Ingest` marks a workspace run `partial`
  whenever `partialCoverage()` holds — any `deferred`, any `unreadable`, or `budget_exhausted`
  (`types.go:83-86`, applied at `ingest.go:104-108`). With ~51 conversations in scope and a 15-minute
  budget, `deferred` is non-empty on every run, so **every run is honestly `partial`**. Both consumers
  already treat it as success: the reconciler counts `status IN ('ok','partial')` (`reconcile.go:121`)
  and `/funnel`'s `last_ok` filters the same pair. **Step 0c confirms this from `stats->'coverage'`
  before anything is changed; if the cause is instead `unreadable`, that is a different bug and this
  ticket stops.**
- **The reconciler counts PASSES, and only passes that read the conversation.**
  `ReconcileUnconfirmed` flags a Slack delivery unconfirmed after `DefaultUnconfirmedFlagPasses = 3`
  (`reconcile.go:19`) runs that started after the click **and** whose `stats->'read'` contains the target
  conversation (`:116-125`). That threshold was chosen against a 30-minute cadence. A per-minute sweep of
  a watched conversation would satisfy it in three minutes — the "one upworkcrm invocation writes TWO
  sync_runs rows" landmine (IK) in a new costume. D6 closes it.
- **`HTTPBridge` has no client timeout by design** ("an export drives a real browser and takes minutes …
  the caller's context is the deadline that matters", `http_bridge.go:59-61`), reads a 64 MiB cap
  (`:103`, `bridge.go:15`), and turns any non-200 into `bridgeStatusError` carrying the status
  (`:107-115`, `:193-201`).
- **`Send` misclassifies 503 — the standing ACTION from the project memory.** `:220-232` maps 4xx and a
  failed dial to `SendRejectedError` (definite, retryable) and leaves everything else ambiguous, which
  parks the delivery in `sending` for a human. The leaf's 503 is thrown by the queue **before any browser
  work** (`job-queue.ts:144-152` → `http-bridge.ts:267-273`), so it is exactly as definite as a 4xx.
  Today that is rare; this ticket makes the browser busy most of every minute, so it becomes constant.
  **In scope** (D7).
- **`CommandBridge.Export` IGNORES the request entirely** — "The request is ignored: the CLI transport has
  no body" (`bridge.go:42-58`). A targeted pass over that transport would silently run a **full** export
  every minute. `newSource` (`main.go:144-153`) falls back to it whenever `SLACK_WEB_BRIDGE_URL` is
  unset. D8 refuses to start the watcher in that configuration.
- **Capture's lock-miss is a skip, not an error** (`internal/capture/rules_store.go:386-394`), so a
  per-minute capture pass cannot fail a CronJob — it can only make one skip. `CaptureClientID` is
  `"switchboard-capture-" + connector` (`internal/pipeline/contract_test.go:196`), and IK records that a
  shared MQTT client id kicks the other holder off the broker (IK "Client ids").
- **`/sources`** is `internal/dashboard/sources.go:64` (`listSources`), routed at
  `internal/dashboard/server.go:100`; connector health lives on `/funnel` since SWT-62.
- **The humanOnly/off-MCP precedent for configuration tools** is `capture_rule_add` /
  `capture_rule_set_enabled` (`internal/tools/capturerules.go:3-19`): "off the MCP list keeps a worker
  from seeing the tool, humanOnly keeps a … actor from calling it".

### the leaf's half (`/home/salvo/projects/personal/slackconnector`)

- **`/export` takes `known`, `budget_ms`, `max_conversations` and NOTHING else.** `parseExportBody`
  (`src/switchboard/http-bridge.ts:126-170`) validates those three and **silently ignores unknown keys** —
  which is why the rollout order in D12 is load-bearing: an old leaf handed a `targets` field would
  quietly run a full export, every minute.
- **There is no conversation filter today.** `exportSlackForSwitchboard`
  (`src/switchboard/export.ts:329-585`) always calls `listWorkspaces()` (`:358`) and, per workspace,
  `listChannels({includePrivate:true})` (`:396`), then `planReadPhases` (`:228-275`) splits the queue into
  discovery → head → rotation (`:181-226`). `known` only reorders that queue; it does **not** restrict
  it, and it does **not** carry a since-cursor — every read is a full `readChannel` with
  `limit: channelLimit` (200 by default, `src/cli/bridge-server.ts:112-116`).
- **But reading a conversation by URL without enumerating it is already implemented.** `planReadPhases`
  synthesises `https://app.slack.com/client/{workspaceId}/{entry.id}` for known-but-unenumerated entries
  (`export.ts:258-272`). The targeted mode of D1 is that path, with enumeration skipped.
- **Coverage is reported back per workspace** — `enumerated` / `read` / `unreadable` / `deferred` /
  `coverage{…}` (`export.ts:110-125`, `:563-582`), which switchboard stores in `sync_runs.stats`.
- **One queue owns the browser, and an export is a `sweep`.** `bridge-server.ts:87-152` runs `/export` as
  `queue.run('sweep', 'export', …)` with `maxSweepDepth: 0` (`:73`), so **a second concurrent export is
  503 + Retry-After**, not a wait (`job-queue.ts:124-153`, `http-bridge.ts:267-273`). Interactive work
  (the `slack-web` MCP tools, `/draft`, `/send`) jumps a *queued* sweep but never preempts a *running*
  one (`job-queue.ts:13-22`, `:170-183`).
- **The busy estimate is per-PRIORITY, not per-job.** `estimatedWaitMs` charges a running sweep
  `estimatedSweepMs = 540_000` (`bridge-server.ts:76`) minus elapsed, with an `overrunFloorMs` of 120 s
  (`job-queue.ts:103-122`). So a 45-second targeted export would tell an interactive caller "come back in
  ~9 minutes" and, since that exceeds `maxInteractiveWaitMs` (60 s, `bridge-server.ts:80-84`), turn it
  away. **Uncorrected, this ticket breaks Salvador's own Slack MCP tools for most of every minute.** L3
  fixes it.
- **A disconnecting `/export` caller KILLS the bridge.** `monitorAbandonedExport`
  (`src/switchboard/export-lifecycle.ts:15-29`, wired at `bridge-server.ts:281-293`) exits the process so
  launchd starts one with no surviving browser work — deliberate, and explicitly not to be replaced by a
  promise timeout (`docs/HANDOFF.md:171-176`). Consequence for this ticket: **a short client-side
  deadline on a per-minute pass is a per-minute bridge suicide.** D5 bounds passes from the leaf side
  (`budget_ms`) and keeps the Go context generously above it.
- **Navigation is what wedges the renderer.** Every `page.goto` reloads the whole Slack client and leaks
  the previous document (587 MB → 1.2 GB over seven reads, `docs/HANDOFF.md:7-46`); the tab is recycled
  every `SLACK_CONNECTOR_TAB_RECYCLE_AFTER_RELOADS` (default 2) reloads, and a job with no browser
  progress for 240 s restarts Chrome and terminates the bridge (`bridge-server.ts:244-272`). A watch list
  of 2–4 conversations per minute roughly doubles the daily navigation count. D10 states the mitigation
  and the rollout gate.
- **Known live facts:** Katie's DM is `D04F7LXRB8B`; workspaces are Avviato `T0360B84U` and
  Collaboratory/LlamaSite `T0HPR78RX` (`docs/HANDOFF.md:386-390`). **Nothing in either repo records a
  Slack conversation for José** — the only José on record is `jose.g@avviato.com` over mail
  (`docs/tickets/comms-inbox_SPEC.md:34,65,202`). Step 0a finds both ids from production or the ticket
  stops and asks.

### the latency chain after ingest (already push, and unchanged by this ticket)

`capture.EvaluateRules` → `pipeline.AnnounceCaptured` (`main.go:134`) publishes `ops/pipeline/captured`
iff the pass committed a decision; pipelined routes `captured` → `gate`, `inquiry`, `route`, and
`promote.InquiryGrace = 0` since 2026-09-18 (`internal/promote/inquiry.go:45-64`, pinned by
`inquiry_test.go:81-84`). So the end-to-end budget for a watched DM is:

| leg | cost |
|---|---|
| wait for the next pass | 0 – `SLACK_WATCH_INTERVAL` (60 s) |
| leaf: `listWorkspaces` + 2–4 reads at 15–19 s | 40 – 90 s (Step 0d measures it) |
| switchboard: normalize + capture + announce | seconds |
| pipelined: inquiry stage + local model (~7.2 s/message median, IK) | ~10 – 60 s |

**≈ 1–3 minutes, p50 ~1.5 min.** Not "under a minute" — 15–19 s per conversation read is the floor and
this SPEC does not pretend otherwise. Salvador asked for "quicker than the 15 min"; this is 10–20x.

## Decisions

### D1 — The leaf gains a TARGETED export mode; switchboard never re-implements reading

One new optional request field, `targets: {workspaceId: [conversationId…]}`. When present the leaf skips
`listChannels` entirely, reads exactly the named conversations by synthesised URL (the
`export.ts:258-272` path), emits only workspaces that have targets, and reports `coverage.mode:
"targeted"`. Everything else — `own_user_id` direction resolution, thread expansion, `skipped_message_count`,
the coverage block, the response shape — is byte-identical to a full export, so **switchboard's ingest,
normalize, direction, thread-key and loop-closure code are untouched**. Reading Slack stays entirely in
the leaf (IK: direction FAILS CLOSED and is never inferred switchboard-side).

Rejected: driving `/op readChannel` (interactive priority, already allowlisted at
`http-bridge.ts:33-47`). It returns the MCP shape, not the export shape, and has no `own_user_id`, so
switchboard would need a second normalizer and a second direction rule. Two spellings of direction is
how own messages get re-triaged (invariant 5).

### D2 — The watch list is a table, `slack_watch`, keyed by conversation

Migration **0041**. Not a column on `source_accounts` (no per-row label or enabled flag, and
`EnsureAccount` rewrites that row every pass, `sink.go:103-122`); not `projects.slack_watch_conversations`
(a project is the wrong grain — the comms-inbox SPEC refused the same shape at
`docs/tickets/comms-inbox_SPEC.md:143-147`, and a workspace is not a project); not derived from a
`people` flag (no Slack identities exist in `person_identities` to derive from, and the leaf needs
conversation ids either way — see Future work).

The id CHECKs are the point: `BuildExportRequest` silently DROPS a malformed id (`export_request.go:101-104`),
so the constraint that makes a dropped watch row impossible belongs in the database, where the write
fails loudly instead.

### D3 — Runtime shape: a resident Deployment, `connector-slackweb-watch`, owning BOTH cadences

Recommended over the alternatives, for one reason above all: **the Mac mini has one browser and one
queue, so switchboard must present it with one caller.** Two schedulers (a `* * * * *` CronJob plus the
existing `*/30` one) would spend most of their day 503-ing each other, and a `* * * * *` CronJob is 1,440
pod starts a day for a 45-second job.

- (a) *second CronJob at `* * * * *`, `concurrencyPolicy: Forbid`* — rejected: pod-start cost, no way to
  coordinate with the full export, and `Forbid` only serialises the CronJob against itself.
- (b) **resident Deployment** — chosen. Precedent shipped in this repo: `dashboard`, `orchestratord`,
  `pipelined`, and the mail watcher specced in `docs/tickets/imap-idle-watch_SPEC.md` (SWT-73), whose
  singleton/bounded-pass/healthz skin this copies verbatim.
- (c) *the leaf polls and pushes* — rejected: it would make the mini a writer to the ops db and move
  scheduling policy out of switchboard. The leaf answers questions; it does not own the funnel.

One loop, two pass kinds, strictly sequential — never concurrent, because concurrency here means 503:

| pass | every | request | what runs after ingest |
|---|---|---|---|
| **targeted** | `SLACK_WATCH_INTERVAL` (60 s) | `targets` from `slack_watch WHERE enabled`, `budget_ms` = `SLACK_WATCH_BUDGET_MS` (150 s), `max_conversations` = len(targets) | Normalize → EvaluateRules → AnnounceCaptured |
| **rotation** | `SLACK_ROTATION_INTERVAL` (30 m) | today's `knownExportRequest` — `known` + `budget_ms` + `max_conversations`, unchanged | the full `main.go:71-134` sequence, including `ReconcileUnconfirmed` and `ObserveOutbound` |

The rotation pass is byte-for-byte today's export; its 12–15 minutes are also 12–15 minutes in which the
targeted pass gets 503 and skips. That is **not** a black hole — the watched DMs are almost certainly in
the leaf's `recentHeadSize = 10` head (`export.ts:181-226`), which is read in the first third of each
workspace's budget — so worst-case latency during a rotation is minutes, not hours. If Step 0d shows
enumeration is cheap, the honest tuning move is smaller, more frequent rotations
(`SLACK_ROTATION_INTERVAL=10m`, `SLACK_WEB_EXPORT_BUDGET_MS=300000`): the leaf's least-recently-visited
rotation (`export.ts:188-208`) already achieves full coverage across successive budgeted runs, which is
exactly what SWT-39 built it for. **Both are the same code path and two env values — no image roll to
change your mind.** Defaults ship at Salvador's stated numbers (a minute, and half an hour).

### D4 — The CronJob stays as a net, and stands down while the watcher is alive

`cronjob/connector-slackweb` keeps running (SWT-73's answered OQ-1: a net, never a co-worker) but moves
to `0 */2 * * *`, and the **one-shot path gains a standing-down check**: at startup it takes
`lockkeys.SlackWatch` with `pg_try_advisory_lock`, releases it immediately, and — if it was already held
— logs `slack watch is live; skipping this pass` and exits **0**.

This is what makes the net a net. Without it, a 2-hourly tick lands on an idle browser, holds it for 15
minutes doing work the watcher already does, and blocks the watch list for those 15 minutes twice a day.
With it, the CronJob does work exactly when the watcher is not running — which is the only time it is
wanted. Rollback of the entire ticket is: scale the Deployment to 0, and the next CronJob tick resumes
today's behaviour with no other change.

### D5 — A pass is bounded by the LEAF's budget, never by cutting the HTTP connection

`monitorAbandonedExport` terminates the bridge when an `/export` caller disconnects
(`export-lifecycle.ts:15-29`). So:

- the leaf-side bound is `budget_ms` on the request — the only bound that stops browser work cleanly;
- the Go context for a targeted pass is `SLACK_WATCH_BUDGET_MS + SLACK_BRIDGE_GRACE` (default 120 s), so
  **the client deadline can only fire after the leaf has already given up**;
- a Go context that fires anyway is logged loudly as `bridge disconnect risk` and counted, because it
  means the leaf overran its own budget and the bridge has probably just restarted.

The existing 30-minute one-shot context (`main.go:46`) is unchanged and keeps covering the rotation.

### D6 — Watch runs are a SEPARATE PHASE, invisible to every existing `sync_runs` consumer

`StartRun` takes the phase; a targeted pass writes `{"phase":"slack_web_watch"}`, a rotation pass keeps
`{"phase":"slack_web"}` (`sink.go:128` today). Both existing readers filter to `slack_web`:

- `ReconcileUnconfirmed`'s pass count (`reconcile.go:116-125`) — **load-bearing**: without it, a delivery
  into a watched conversation is flagged "unconfirmed after 3 export passes" three minutes after the
  send, an alarm meaning something entirely different from what it says;
- `KnownConversations`' `runs` CTE (`sink.go:31-48`) — so the rotation's ordering is byte-identical to
  today, and so that CTE does not grow to scan a per-minute row set over 30 days.

**Volume discipline** (imap-idle-watch D9's rule): a targeted pass writes a `sync_runs` row **only** when
it inserted or updated raw rows, when it failed, or on the first success after a failure. A quiet
watcher writes nothing — 1,440 rows/day/workspace for nothing is worse than the 230/day that decision
already refused. A phase with no rows at all shows nothing on `/funnel`, which is correct: liveness is
`/healthz`'s job (D9), not a funnel column's.

### D7 — This ticket fixes `HTTPBridge.Send`'s 503 classification

`Send` (`http_bridge.go:206-235`) treats 503 as ambiguous and wedges the delivery row in `sending`. The
leaf's 503 comes from `QueueFullError`, thrown inside `JobQueue.run` **before the job function is
called** (`job-queue.ts:144-152`), and converted at `http-bridge.ts:267-273`; no other path in the leaf
returns 503. It is therefore provably pre-click, like the 4xx cases and the failed dial. 429 is included
for the same reason should it ever be added.

Not optional housekeeping: with a watch pass occupying the browser most of every minute, **every approved
Slack send would otherwise wedge**, and each wedge needs a human plus `mark_delivery_failed`, which is
itself lease-guarded for 15 minutes (IK, Slack send promotion).

### D8 — The watcher refuses configurations that would make it a full export every minute

Exit non-zero at startup when `SLACK_WEB_BRIDGE_URL` is unset (`CommandBridge` discards the request —
`bridge.go:42-58`). Skip targeted passes, logging once, when the leaf's response does not report
`coverage.mode == "targeted"` for every workspace it returned: an old leaf ignores unknown request keys
(`http-bridge.ts:126-170`) and would answer a 1-minute targeted request with a 15-minute full export.
Rotation passes continue in that state, so a version skew degrades to today's behaviour rather than to a
browser meltdown. Startup also resolves and prints the capture config once — the
`CAPTURE_RULES_SINCE`-below-`MinLiveRulesHorizon` landmine (IK, HANDOFF-kube-jira-activity-revive) turns a
failed CronJob run into a resident loop that looks alive while capturing nothing.

### D9 — Singleton lock, health endpoint, and the client id

- `lockkeys.SlackWatch int64 = 0x5157_0011` — free today (`grep 0x5157`: 0005 orchestrator, 0006 triage,
  0007 google per-account, 0015 capture, 0021 promote, 0022 classify, 0023 ticketstatus, 0028 calendar
  booking; `0x5157_0010` is reserved by SWT-73's `MailWatch`). It lives in the import-free
  `internal/lockkeys` (`lockkeys.go:1-11`) because the repo-wide collision scan
  (`internal/classify/structure_test.go`) walks `internal/` only — and because D4's CronJob check needs
  the same constant.
- Held elsewhere ⇒ **standby, not exit**: log once, `/healthz` 503 `standby`, retry every 15 s, run
  nothing. Lost (a CNPG switchover kills the connection silently) ⇒ exit non-zero and let the kubelet
  restart. Straight from `internal/orchestrator/engine.go:215-265`, re-spelled locally — a connector must
  not import the orchestrator (invariant 7).
- `GET /healthz` on `SLACK_WATCH_HEALTH_ADDR`, default **`:8093`** (hooksd `:8090`, orchestratord
  `:8091`, the mail watcher `:8092` under SWT-73): 200 iff a pass **completed** within
  `3 × SLACK_WATCH_INTERVAL` **and** the lock is `Alive`. Deliberately **not** in the verdict: whether the
  bridge is reachable, whether the pass found messages, and whether any single conversation was readable.
  A wedged mini must not crash-loop a pod in the cluster — restarting the pod cannot fix Chrome, and
  `sync_runs` error rows plus the mini's own launchd recovery already own that failure.
- The capture announce uses connector string **`slackweb-watch`**, so the client id is
  `switchboard-capture-slackweb-watch` and can never collide with the CronJob's
  `switchboard-capture-slackweb` (IK: a shared id kicks the other off the broker;
  `internal/pipeline/contract_test.go:196`).

### D10 — Accepting, and bounding, the extra browser load

2–4 conversations per minute ≈ 3,000–5,700 extra `page.goto` navigations a day against today's ~1,700 —
and navigation is the documented cause of the renderer wedges (`docs/HANDOFF.md:7-46`). Mitigations
already exist on the leaf (tab recycling every 2 reloads, the 240 s stale-job kill plus launchd restart),
and the watcher is written to survive all of them: **any bridge error — 503, 500, EOF, a killed
process — is a skipped pass, never a failed process** (`watch.go` rule, criterion 14). The rollout gate
(Step 6) is 24 hours of the mini's self-kill rate against the pre-change baseline from Step 0e; if it
worsens materially, the remedy is one env change (`SLACK_WATCH_INTERVAL=180s`) or `=0` to disable
targeted passes entirely, with no image roll and no rollback of anything else.

### D11 — What does NOT change

`Ingest`'s raw write (`ingest.go:159-185`), `Normalize`, `confirmDelivery`, thread keys, the
`slack_reply` policy tier, `prefill_delivery`, `SchemaVersion` (the response shape gains one optional
field, which old switchboard binaries ignore), `DefaultUnconfirmedFlagPasses`, the `partial` semantics,
`/funnel`, and every other connector. No task, delivery or orchestrator behaviour is touched.

### D12 — Cross-repo order: leaf first, and it is backward compatible in both directions

1. Leaf changes L1–L3 ship and are deployed on the mini (`rsync` + `npm run build` + **`launchctl
   kickstart -k gui/501/com.salvadorspataro.slack-bridge-server`** — `docs/HANDOFF.md:291-302`; the memory
   "Mac mini operating pattern" says run that from a session ON the machine, not by remote-driving it).
   An old switchboard sends no `targets`, so behaviour is unchanged.
2. switchboard rolls, the Deployment is created, rows are seeded. A leaf that somehow predates step 1 is
   caught by D8's `coverage.mode` check and the watch simply stays off.

## Data model changes

**Migration `0041_slack_watch.sql`** (0040 is claimed by the comms-inbox ticket being specced in
parallel; forward-only, numbered, never edited after apply — IK).

```sql
CREATE TABLE slack_watch (
  id              BIGSERIAL PRIMARY KEY,
  workspace_id    TEXT NOT NULL CHECK (workspace_id ~ '^T[A-Z0-9]{5,}$'),
  conversation_id TEXT NOT NULL CHECK (conversation_id ~ '^[CDG][A-Z0-9]{5,}$'),
  label           TEXT NOT NULL DEFAULT '',
  enabled         BOOLEAN NOT NULL DEFAULT true,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, conversation_id)
);
```

The two CHECKs are the leaf's own rules (`export_request.go:88-90` ≡ `http-bridge.ts:140,146`), restated
where a bad value cannot be entered rather than silently dropped. **No rows are seeded by the
migration** — production ids are not frozen literals (IK), and which conversations matter is Salvador's
call, made with `opsctl` after Step 0a.

Tables read/written otherwise are exactly today's: `source_accounts`, `raw_source_items`,
`normalized_messages`/`_threads`, `sync_runs`, `deliveries` (confirmation stamps inside `Normalize`,
unchanged), `capture_decisions`, and `tasks`/`task_events`/`external_refs` **through the executor only**.

## API / MCP tool changes

Three executor tools, registered in a new `internal/tools/slackwatch.go`, all **`policy.humanOnly` and
NOT MCP-listed** — the `capture_rule_add` shape and for the same reason (`internal/tools/capturerules.go:3-19`):
an agent must not be able to point the browser at a conversation of its choosing, and browser time is a
scarce shared resource.

| tool | args | result |
|---|---|---|
| `slack_watch_add` | `{workspace_id, conversation_id, label?}` | `{id, workspace_id, conversation_id, label, enabled}`; upsert on the unique key, re-enabling a disabled row |
| `slack_watch_set_enabled` | `{id, enabled}` | `{id, enabled}`; idempotent no-op success |
| `slack_watch_list` | `{enabled?}` | `{rows:[{id, workspace_id, conversation_id, label, enabled, last_read_at}]}` |

Every one goes validate → policy check → audit start → handler → audit complete, like every other tool
(invariant 3). `opsctl slack-watch add|list|disable` wraps them, mirroring `opsctl capture-rules`.

**Leaf HTTP contract** (`POST /export`), additive and optional in both directions:

```jsonc
// request
{ "targets": { "T0360B84U": ["D04F7LXRB8B", "D…"] },   // NEW; mutually exclusive with "known"
  "budget_ms": 150000, "max_conversations": 2 }
// response, per workspace (unchanged except the last line)
{ "id": "T0360B84U", …, "read": ["D04F7LXRB8B"], "deferred": [], "unreadable": [],
  "coverage": { "enumerated_count": 2, …, "mode": "targeted" } }   // NEW: "targeted" | "full"
```

`SchemaVersion` stays **1**: the response gains one optional field and Go's `Coverage` struct
(`types.go:66-75`) gains `Mode string \`json:"mode,omitempty"\``. Bumping it would force a lockstep
deploy of two repos across two machines for an additive field — exactly the coupling the version check
(`http_bridge.go:137-139`) exists to avoid.

## MQTT topics

No topic added or changed. The watcher publishes exactly what every connector main publishes today:
`ops/pipeline/captured`, QoS 1, **never retained** (IK landmine), iff the pass committed a capture
decision — from client id `switchboard-capture-slackweb-watch` (D9). `MQTT_BROKER` must be set on the
Deployment: unset, `AnnounceCaptured` skips with one log line and pipelined falls back to its 5-minute
sweep, so the message is ingested in a minute but the task is not created for up to five. No
`ops/workers/…` topic and no LWT — a connector is not a worker
(`docs/tickets/imap-mail-connector_SPEC.md:380-384`).

## Files likely to touch

**switchboard**

- `migrations/0041_slack_watch.sql` (new).
- `internal/connector/slackweb/watch.go` (new) — the loop: pass scheduling, the two pass kinds, backoff,
  bridge-busy handling, the counter line.
- `internal/connector/slackweb/export_request.go` — `Targets`, `BuildTargetedRequest`, the `mode` check.
- `internal/connector/slackweb/types.go` — `Coverage.Mode`; phase constants.
- `internal/connector/slackweb/ingest.go` — the request and phase become inputs; the conditional run row.
- `internal/connector/slackweb/sink.go` — `StartRun(phase)`, `WatchTargets`, the phase filter in
  `KnownConversations`.
- `internal/connector/slackweb/reconcile.go` — the phase filter in the pass count (D6).
- `internal/connector/slackweb/http_bridge.go` — 503/429 on `/send` (D7); a typed `ErrBridgeBusy` with
  `Retry-After` for `/export`.
- `cmd/connectors/slackweb/main.go` — `--watch`, `watchMain`, the D4 stand-down check, D8's refusals.
- `cmd/connectors/slackweb/{health.go,singleton.go}` (new) — twins of `cmd/orchestratord`'s.
- `internal/lockkeys/lockkeys.go` — `SlackWatch`.
- `internal/tools/slackwatch.go` (new) + registration + the `policy.humanOnly` list.
- `cmd/opsctl` — `slack-watch` subcommand.
- `internal/dashboard/sources.go` — the read-only watch panel.
- Tests (new): `internal/connector/slackweb/{watch_test.go,targeted_request_test.go,watch_integration_test.go,
  phase_integration_test.go}`, `internal/tools/slackwatch_integration_test.go`,
  `cmd/connectors/slackweb/{health_test.go,singleton_integration_test.go,structure_test.go}`,
  `internal/dashboard/sources_slackwatch_integration_test.go`, plus a `send_test.go` case for D7.
- Docs: `docs/runbooks/slack-web-connector.md`, `docs/runbooks/HANDOFF-kube-slack-watch-sweep.md` (new),
  `.claude/INSTITUTIONAL_KNOWLEDGE.md`.

**the leaf** (`/home/salvo/projects/personal/slackconnector`, separate commit, deployed first)

- `src/switchboard/export.ts` — `targets` in the options and request types; skip `listChannels` and
  synthesise the queue in targeted mode; weight only workspaces with targets
  (`planWorkspaceWeights:173-179`); `coverage.mode`.
- `src/switchboard/http-bridge.ts` — `parseExportBody` validates `targets` with the existing regexes and
  rejects `targets` + `known` together.
- `src/cli/bridge-server.ts` — pass `targets` through; supply the per-job estimate (L3).
- `src/browser/job-queue.ts` — `run(priority, label, job, estimatedMs?)` used by `estimatedWaitMs`.
- `tests/unit/{switchboard-export,http-bridge,job-queue}.test.ts`.
- `docs/HANDOFF.md` and `CLAUDE.md` — the new field, the new cadence, and the fact that the browser is
  now busy most of every minute.

**Not this repo:** `~/projects/personal/kube/switchboard/*.yaml`. The kube session owns manifests; this
session writes the handoff (IK: *kube manifests belong to the kube session*), and a **new resident
workload needs Salvador's go-ahead** before it is created.

## In scope / Out of scope

**In scope:** the `slack_watch` table and its three executor tools; the leaf's targeted export mode and
per-job queue estimate; the resident watch loop with its singleton lock, `/healthz`, phase separation and
refusals; the D4 CronJob stand-down; the D7 `Send` 503 fix; the `/sources` panel; the runbook, the kube
handoff and the IK entry.

**Out of scope, each named because it is a tempting bundle:**

- **Any change to how Slack messages are normalized, threaded, classified or promoted.** This ticket
  changes only WHEN bytes arrive. The inquiry lane, `StripQuotedHistory`, the board sections and the
  comms-inbox ticket's `comm_task` column are all elsewhere.
- **Making the rotation smarter** — a real since-cursor (`oldest_ts`) on `readChannel`, a smaller
  `channel_limit` for targeted reads, per-workspace parallelism (Salvador's two-Chrome idea,
  `docs/HANDOFF.md:400-403`). All are Future work; this ticket buys latency with a filter, not with a new
  read algorithm.
- **The `imap-idle-watch` Deployment (SWT-73)** and the `comms-inbox` migration 0040 (SWT-74). Both are in
  flight on adjacent branches; this ticket takes 0041 and port `:8093` to avoid colliding with them and
  touches neither.
- **Katie's Jira-mirror mail and José's `jose.g@avviato.com` mail.** Those arrive through the jira and
  google connectors and are governed by capture rules; "Katie" and "José" here mean **Slack conversations
  only**. Making mail from the same people faster is SWT-73's ticket.
- **Any notification/alerting when a watched conversation goes unread**, any `/funnel` widget for watch
  state, any MQTT presence for the watcher.
- **Un-suspending, re-scheduling or narrowing any other CronJob**, and the calendar's Pipedream decision
  (memory: wait for Oct 1).
- **Promoting `slack_reply` off the assisted tier**, or anything else in the policy matrix.

## Invariants that apply

1. **Raw-first** — unchanged and load-bearing. A targeted pass writes through the SAME
   `upsertObservation` → `InsertRaw`/`UpdateRaw` path (`ingest.go:159-185`), raw provider JSON plus
   `sha256` content hash, BEFORE `Normalize`. **There is exactly one raw writer**, and a structure test
   pins it: no new function may insert into `raw_source_items` for `provider='slack_web'`. The targeted
   mode changes WHICH conversations the leaf reads, never what is stored or in what order.
2. **One funnel** — `slack_watch` is CONFIGURATION, the `capture_rules` shape, not a task-like table: it
   holds no work, no status, no assignee. Everything actionable it causes still becomes a row in `tasks`
   via `capture.EvaluateRules` through the executor. No new queue and no second Slack inbox.
3. **Everything through the executor** — the three `slack_watch_*` tools are registered on the executor
   (validate → policy → audit start → handler → audit complete) and are `humanOnly` and off the MCP list.
   The watch loop itself reads `slack_watch` directly (a read of its own configuration, like
   `loadRules` in capture) but writes tasks/logs/external_refs only through the executor built at
   `main.go:103-106`. `/healthz` performs no work and touches no tool.
4. **Nothing external without a delivery row** — the watcher arms **no** `tools.Set*` sender seam; a
   structure test asserts it (the existing `internal/connector/slackweb/sender_seam_test.go` shape), so
   this process cannot send even if a delivery row asked it to. D7 touches only how a REFUSAL is
   classified: a 503 means the click provably did not happen, so the row returns to `failed` (re-approvable)
   instead of wedging in `sending` — it never causes a send, and `sent_external_id` idempotency is
   untouched.
5. **Own-message loop closure** — a watched conversation is a conversation Salvador also replies in, so
   the watcher's normalize pass must keep stamping deliveries. It does: `Normalize` →
   `upsertMessage` → `confirmDelivery` (`sink.go:229-382`) runs identically on a targeted pass, and
   `capture.ObserveOutbound` runs on rotation passes. **D6's phase filter is what protects this
   invariant's alarm**: without it, per-minute passes would make `ReconcileUnconfirmed` flag a healthy
   send three minutes after the click.
6. **Stealth attribution** — nothing client-visible is produced. No prose, no send, no draft.
7. **Orchestrator purity** — `internal/orchestrator` is neither imported nor modified; the singleton lock
   is a local re-spelling of its idiom keyed from the import-free `internal/lockkeys` (`lockkeys.go:1-6`)
   exactly so no dependency edge is created. No LLM is called anywhere in this ticket.

## Acceptance criteria

### Part 1 — the watch list as data

1. Migration 0041 creates `slack_watch` with the two id CHECKs and the unique key; an `INSERT` of
   `workspace_id='t0360b84u'` or `conversation_id='XYZ'` is REFUSED by Postgres (integration, asserted
   against the database, not a Go validator).
2. `slack_watch_add` / `slack_watch_set_enabled` / `slack_watch_list` execute through the executor and
   write `audit_events` start+complete rows; `slack_watch_add` on an existing pair updates the label and
   re-enables rather than erroring (idempotent — memory: *owner works the board concurrently*).
3. All three are absent from every MCP tool list (`ProfileUser` and `ProfileRead`) and are refused for a
   `mcp:` actor AND for a bare `drafts:gpt`-shaped actor — the six-actor-shape test of IK's "an
   actor-prefix check is a transport label" entry, not just the `mcp:` one.
4. `opsctl slack-watch add|list|disable` drives exactly those tools and prints the row.

### Part 2 — the targeted request and its guard

5. `BuildTargetedRequest` produces `{targets, budget_ms, max_conversations}` with **no** `known` key and
   omits every zero field (the leaf 500s on `budget_ms: 0` — `export_request.go:17-19`,
   `http-bridge.ts:118-124`).
6. A disabled row, and a row for a workspace with no `source_accounts` entry, are excluded from
   `targets`; an empty enabled set means **no targeted pass is issued at all** (no HTTP call, one log
   line at most per interval).
7. A response whose workspace coverage reports `mode != "targeted"` — including a leaf that omits `mode`
   entirely — is REFUSED: nothing is ingested from it, one `sync_runs` error row is written, targeted
   passes stand down until the next successful probe, and rotation passes continue. Mutating the fake
   leaf to answer `"full"` turns the test red (D8).
8. The watcher exits non-zero at startup when `SLACK_WEB_BRIDGE_URL` is unset, naming `CommandBridge`
   (`bridge.go:42-58`) as the reason.

### Part 3 — the loop

9. One targeted pass per `SLACK_WATCH_INTERVAL`, one rotation per `SLACK_ROTATION_INTERVAL`, **never
   overlapping**: a table-driven test over a fake clock asserts the pass sequence and that no second pass
   starts while one is in flight.
10. A rotation pass runs the full `main.go:71-134` sequence in the same order; a targeted pass runs
    Normalize → EvaluateRules → AnnounceCaptured and does **not** run `ReconcileUnconfirmed`. Reordering
    or dropping a call turns a structure test red.
11. The pass Go context is `budget + SLACK_BRIDGE_GRACE` and is never shorter than the `budget_ms` the
    request carries; a test pins the inequality and its comment names `monitorAbandonedExport` (D5).
12. `AnnounceCaptured` is called with connector `slackweb-watch`, so the client id differs from the
    CronJob's; a test asserts the two ids are distinct (IK: a shared id kicks the other off the broker).
13. Startup prints one line — `slack watch: interval=60s rotation=30m budget=150s targets=2 mode=live
    horizon=720h health=:8093` — and exits non-zero when the capture config would make every pass a
    silent no-op (a live `CAPTURE_RULES_SINCE` under `MinLiveRulesHorizon`); it does **not** refuse
    `mode=shadow`.

### Part 4 — failure is a skipped pass, never a dead process

14. Each of a 503 with `Retry-After`, a 500, an EOF mid-response, a dial failure and an unparseable body
    from `/export` leaves the loop running: the pass is counted and skipped, the next tick runs, and
    `/healthz` is unaffected until the staleness window elapses. A 503 additionally sleeps until
    `min(Retry-After, interval)`.
15. A conversation the leaf reports `unreadable` does not fail the pass: what WAS read is ingested
    raw-first and the run is `partial` (`ingest.go:104-108`, unchanged).
16. The watch list is re-read from the database on every pass — a row added between passes is swept on
    the next one without a restart (integration; the SWT-73 D10 "watch the table, not a snapshot" rule).

### Part 5 — the phases, and the alarms that must not move

17. A targeted pass writes `sync_runs` with `stats->>'phase' = 'slack_web_watch'`; a rotation pass writes
    `'slack_web'`. **Integration, against Postgres.**
18. `ReconcileUnconfirmed` ignores `slack_web_watch` runs: with an unconfirmed `slack_reply` delivery and
    **ten** watch runs whose `stats->'read'` contains its conversation, nothing is flagged; three
    `slack_web` runs flag it exactly as today. Dropping the phase filter from the SQL turns it red — and
    the test asserts the filter's effect on rows Postgres produced, never on a fixture-supplied field
    (IK: *test the column, not the fixture*).
19. `KnownConversations` ignores `slack_web_watch` runs, so the rotation's `last_seen_ts` ordering is
    byte-identical to today's for the same run set.
20. A targeted pass that inserted or updated nothing and did not fail writes **no** `sync_runs` row; one
    that ingested writes one; a failure writes one error row; the first success after a failure writes one
    `ok` row and subsequent successes write none (D6).

### Part 6 — the singleton, the CronJob and health

21. The watcher takes `lockkeys.SlackWatch = 0x5157_0011` before its first pass; held elsewhere it logs
    once, serves `/healthz` 503 `standby`, retries every 15 s, and runs no pass and writes no row
    meanwhile. Losing the lock returns a non-nil error and the process exits non-zero. The repo-wide
    key-collision scan stays green and a duplicate turns it red.
22. **The one-shot path stands down while the watcher lives**: with the lock held, `connector-slackweb`
    logs `slack watch is live; skipping this pass`, exits **0**, opens no bridge connection and writes no
    `sync_runs` row. With the lock free it behaves exactly as today. (Integration, both directions.)
23. `GET /healthz` on `:8093`: 200 iff a pass completed within `3 × SLACK_WATCH_INTERVAL` **and** the lock
    is alive; else 503 with one line naming which condition failed. Table test over {fresh, stale,
    never} × {alive, dead, standby}, pure, no db.
24. Health ignores the bridge: with every `/export` call answering 503 and the loop still cycling,
    `/healthz` is 200 until the pass-completion window lapses — a wedged mini does not crash-loop a pod
    (D9).
25. SIGTERM shuts down cleanly: no pass starts after cancellation, the lock connection is released, exit
    code 0.

### Part 7 — the send fix and the sender seam

26. `HTTPBridge.Send` maps 503 and 429 to `SendRejectedError{Status:…}`; 500 and a mid-response EOF stay
    ambiguous. The 503 test's comment cites `job-queue.ts:144-152` for why it is pre-click. Reverting the
    mapping turns it red.
27. The watcher arms no `tools.Set*` seam (the `sender_seam_test.go` shape) — invariant 4.

### Part 8 — dashboard, docs, handoff

28. `/sources` lists each `slack_watch` row (label, workspace, conversation, enabled, last read) and
    renders correctly with an EMPTY table; the page's existing failure mode (one query error 500s the
    whole page — `internal/dashboard/funnel_test.go:262`) is not widened by the new query.
29. `docs/runbooks/slack-web-connector.md` gains a "Running it resident" section (env table, the two
    cadences, the watch list and how to edit it, the phase semantics, what `/healthz` means and ignores,
    the leaf version requirement) and its stale one-box instructions are corrected to the cluster/mini
    split.
30. `docs/runbooks/HANDOFF-kube-slack-watch-sweep.md` (new): image tag and digest, the full Deployment
    spec, an **env-parity table** between `cronjob/connector-slackweb` and the Deployment with the
    `kubectl` command that prints both, the migration-before-image rule, the rollout order (leaf → image →
    migration → Deployment → CronJob reschedule), the rollback, and the seeding commands.
31. The leaf repo's `npm run check` (lint, typecheck, full unit suite) is green, and `docs/HANDOFF.md` +
    `CLAUDE.md` there record the new field and the new duty cycle.

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| Drop the `workspace_id`/`conversation_id` CHECK from 0041 | criterion 1 |
| MCP-list any `slack_watch_*` tool | criterion 3 |
| Send `known` instead of `targets` on a watch pass | criterion 5 |
| Accept a response without `coverage.mode` | criterion 7 |
| Allow `--watch` without `SLACK_WEB_BRIDGE_URL` | criterion 8 |
| Start a pass while one is in flight | criterion 9 |
| Run `ReconcileUnconfirmed` on a targeted pass | criterion 10 |
| Make the Go context shorter than `budget_ms` | criterion 11 |
| Reuse the CronJob's capture announce connector string | criterion 12 |
| Return an error instead of skipping on a 503 from `/export` | criterion 14 |
| List the watch rows once at startup | criterion 16 |
| Write phase `slack_web` on a targeted pass | criteria 17, 18, 19 |
| Drop the phase filter from `ReconcileUnconfirmed`'s pass count | criterion 18 |
| Replace `KnownConversations`' phase filter with a literal true | criterion 19 |
| Write a `sync_runs` row on every targeted pass | criterion 20 |
| Exit instead of standby when the singleton lock is held | criterion 21 |
| Remove the one-shot stand-down check | criterion 22 |
| Make `/healthz` fail when the bridge answers 503 | criterion 24 |
| Revert `Send`'s 503 mapping to ambiguous | criterion 26 |
| Call a `tools.Set*` seam in the watch main | criterion 27 |

## Sibling patterns to copy

- **Resident process, singleton lock, health handler, `os.Exit` only in main:** `cmd/orchestratord/main.go`
  + `main_test.go:14-17` + `structure_test.go:186-203`, and `internal/orchestrator/engine.go:215-265` for
  `LockHandle`/`TryAdvisoryLock` (note its `sync.Mutex` — the loop and `/healthz` both call `Alive` and a
  pgx conn is not concurrency-safe). `docs/tickets/imap-idle-watch_SPEC.md` D4/D6/D7/D9/D10 is the same
  skin on the same problem, written two days ago — copy its decisions rather than re-deriving them.
- **Loop discipline under a broker or DB that may vanish:** `internal/pipeline/stage.go` (`StageLoop`:
  coalescing wake, sweep fallback, `PassTimeout`, lost-lock retry) and `cmd/pipelined/main.go`'s shutdown
  ordering.
- **Configuration tools that must stay off the agent surface:** `internal/tools/capturerules.go` — the
  `humanOnly` + not-MCP-listed pair, its validator, and `opsctl capture-rules`' CLI shape.
- **Per-account error rows that never abort the pass:** `cmd/connectors/google/mailsource.go:86-135`.
- **Env knob discipline:** `internal/connector/slackweb/export_request.go:69-82` (`positiveEnv`: an
  unparseable or non-positive value falls back, never produces a zero the leaf would reject).
- **Handoff shape:** `docs/runbooks/HANDOFF-kube-inquiry-promote.md` (new Deployment + full env table) and
  `HANDOFF-kube-chat-on-closed-task.md` (migration-before-image gate).
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used — nothing is claimed. **rag-svc HTMX:** the
  `/sources` panel is a table in the existing template, no new interaction.

## Verification protocol

Run in order. Do not commit before step 4 passes. Capture the exit status of every `go test` separately
from any pipe (IK: *gate commits on test exit status*).

**0. Read-only pre-checks.** SQL inside `BEGIN READ ONLY; … ROLLBACK;` against
`psql -h 192.168.50.49 -U ops -d ops`; `kubectl` and the leaf's `/status` are reads. NOT run by the spec
session. Paste the results into the delivery summary and assert none of them as a frozen literal (IK:
production counts are not frozen literals).

- **0a. Who and where — the gate on the whole ticket.** Find the Slack conversations for José and Katie:

  ```sql
  SELECT t.thread_key, count(*) AS messages, max(m.sent_at) AS latest,
         count(*) FILTER (WHERE m.direction='inbound') AS inbound
    FROM normalized_threads t JOIN normalized_messages m ON m.thread_id = t.id
   WHERE t.channel = 'slack' AND m.sent_at > now() - interval '60 days'
     AND (m.sender ILIKE '%katie%' OR m.sender ILIKE '%jos%')
   GROUP BY 1 ORDER BY latest DESC;
  ```

  `thread_key` is `slack:{ws}:{conv}[:{root}]` (IK, Slack Web connector), so the workspace and
  conversation ids fall out of it. Corroborate against `docs/HANDOFF.md:386-390` (Katie = `D04F7LXRB8B`).
  **Gate:** if no Slack conversation exists for José, STOP and ask — the ticket seeds what exists and
  Salvador names the rest; do not substitute his email traffic, which is a different connector and
  explicitly out of scope.
- **0b. The latency this ticket removes** — the before-picture, over 7 days:

  ```sql
  SELECT date_trunc('day', r.ingested_at) AS day, count(*),
         percentile_disc(0.5) WITHIN GROUP (ORDER BY r.ingested_at - m.sent_at) AS p50,
         percentile_disc(0.9) WITHIN GROUP (ORDER BY r.ingested_at - m.sent_at) AS p90,
         max(r.ingested_at - m.sent_at) AS worst
    FROM raw_source_items r
    JOIN normalized_messages m ON m.raw_source_item_id = r.id
    JOIN source_accounts a ON a.id = r.source_account_id AND a.provider = 'slack_web'
   WHERE m.direction = 'inbound' AND r.ingested_at > now() - interval '7 days'
   GROUP BY 1 ORDER BY 1;
  ```

  Then the same restricted to the two watch conversations. **Gate:** record it; step 6 compares.
- **0c. Why every run reads `partial`** — the claim in D-notes above, proven or refuted:

  ```sql
  SELECT a.account_email, r.started_at, r.status,
         r.stats->'coverage'->>'enumerated_count' AS enumerated,
         r.stats->'coverage'->>'read_count'       AS read,
         r.stats->'coverage'->>'deferred_count'   AS deferred,
         r.stats->'coverage'->>'unreadable_count' AS unreadable,
         r.stats->'coverage'->>'budget_exhausted' AS budget_exhausted,
         r.stats->'coverage'->>'elapsed_ms'       AS elapsed_ms
    FROM sync_runs r JOIN source_accounts a ON a.id = r.source_account_id
   WHERE a.provider='slack_web' AND r.started_at > now() - interval '3 days'
   ORDER BY r.started_at DESC LIMIT 40;
  ```

  **Gate:** if `deferred_count > 0` and/or `budget_exhausted` on every run, `partial` is HONEST and this
  ticket changes nothing about it (record that in the delivery summary and the IK entry). If instead
  `unreadable_count > 0` dominates, that is a separate bug — raise it and re-scope before writing code.
- **0d. The real per-conversation and per-enumeration cost.** From the same rows: `elapsed_ms /
  read_count` is the per-read cost including enumeration; on the mini,
  `ssh … 'grep "Export coverage" ~/Library/Logs/…'` gives the per-workspace lines
  (`bridge-server.ts:139-150`). Estimate enumeration separately as `elapsed_ms − read_count × per_read`.
  **Gate:** if a read is materially above 19 s, `SLACK_WATCH_INTERVAL`'s default rises to match — a pass
  that cannot finish inside its interval is a loop that never idles. If enumeration turns out cheap
  (< 30 s), record it: that is what makes D3's smaller-rotation tuning worthwhile.
- **0e. The mini's current health baseline** (the D10 comparison): bridge restarts, stale-job kills and
  tab recycles per day for the last 7 days from `~/Library/Logs/` and the launchd logs, plus `POST
  /status` once to confirm the queue is idle and `msSinceBrowserOk` is fresh.
- **0f. Does `known` actually change what the leaf reads today?** For the last 10 runs compare
  `stats->'enumerated'` sources (`sidebar` / `dms` / `known`) against `stats->'read'`. **Gate:** at least
  one `source: "known"` id must appear in `read`, or the `known` mechanism is not working in production
  and D1 is building on a path that has never run.
- **Measured 2026-09-22 (prod, read-only, the build session):**
  - **0a — passed.** José: DM `slack:T0360B84U:DSAV4HJ2F` (363 messages in 60 d, latest 14:23Z today)
    and channel `T0360B84U:C1C1TSLJH` (12); Katie: DM `slack:T0HPR78RX:D04F7LXRB8B` (283, latest
    13:03Z today) and `T0HPR78RX:C0BST0C6RV3` (6). Both people, two different workspaces. Seed the
    two DMs; the channels are his to add.
  - **0b — recorded, not meaningful as written:** p50 lag reads in hundreds of days because every full
    export re-ingests old history (`raw_updated`/`raw_unchanged` dominate). Step 6 compares on the two
    watch conversations with `sent_at > roll time` only.
  - **0c — `partial` is HONEST.** Every run in 3 days: `budget_exhausted: true`, deferred 14–24 of
    38/45 per workspace, `unreadable_count: 0`. Nothing to fix; D6 stands.
  - **0d — per read ≈ 18 s** (433,682 ms / 24 reads, 467,978 / 24), enumeration folded in. A 2-DM
    targeted pass fits a 60 s interval with room; `SLACK_WATCH_INTERVAL` default 60 s holds.
  - **0e — not run from here** (mini logs are read on the mini, per the operating pattern); the leaf
    deploy runbook records the baseline when it runs there.
  - **0f — passed.** `enumerated[].source = "known"` ids (`C015AK8MCPN`, `C03J2KTN1PD`, `C0HPR7B2M`,
    `C0HPS0V7G`, `D023E7XSSGG`) appear in `read` on the latest run: the `known` path runs in production.
  - **0g:** `cronjob/connector-slackweb` is `*/30 * * * *`, `concurrencyPolicy: Forbid`,
    `SLACK_WEB_BRIDGE_URL=http://192.168.50.130:8787`, `CAPTURE_RULES_MODE=live`, `MQTT_BROKER` set.
    The cluster says `*/30`; the leaf's `SWITCHBOARD_BRIDGE_HANDOFF.txt` is stale.
- **0g. The cluster, before touching it:** `kubectl -n ops get cronjob,deploy -o wide`, and
  `connector-slackweb`'s full env list and schedule (the leaf's `CLAUDE.md:36-44` says `*/30`,
  `SWITCHBOARD_BRIDGE_HANDOFF.txt:47-54` says `0 */2 * * *` — **they disagree; the cluster decides**).
  This is the source of the env-parity table in criterion 30, especially `SLACK_WEB_BRIDGE_URL`,
  `SLACK_WEB_BRIDGE_TOKEN`, `CAPTURE_RULES_MODE`, `CAPTURE_RULES_SINCE`, `MQTT_BROKER`, `OPS_TOKEN_KEY`,
  `DATABASE_URL`, `SLACK_WEB_EXPORT_BUDGET_MS`.
- **0h. Broker sanity:** `mosquitto_sub -h 192.168.50.45 -t 'ops/pipeline/#' -v` during one
  `connector-slackweb` tick; confirm the `captured` payload and that nothing is retained.

**1. Leaf first.** In `/home/salvo/projects/personal/slackconnector`: `npm run check` (lint, typecheck,
all unit tests) green, with new cases for targeted mode, the `targets`+`known` rejection, the id
validation, `coverage.mode`, and the per-job queue estimate.

**2. Unit:** `go test ./...`. (The SWT-48 `TestAttributionTrend_*` flake between 20:00 and 24:00 EDT is
pre-existing; re-run with `TZ=UTC` if it fires.)

**3. Integration, on an ISOLATED database** (never prod, never the shared compose `ops` — IK landmine:
the compose db is shared by every worktree and capture suites delete wholesale):

```
psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_slackwatch"
make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable'
DATABASE_URL='postgres://ops:ops@localhost:5433/ops_slackwatch?sslmode=disable' \
  go test -tags integration -p 1 -count=1 ./internal/connector/slackweb/ ./internal/tools/ \
    ./cmd/connectors/slackweb/ ./internal/dashboard/
```

Run twice (rerunnable cleanup, FK order, test-owned slugs), then `go test -tags integration -p 1 ./...`
once against the same URL.

**4. Mutations:** every row of the table above goes red, then is reverted.

**5. Smoke against the REAL leaf — the "usable alone" gate.** With the leaf deployed on the mini and an
isolated database:

- `curl -sS http://192.168.50.130:8787/healthz` → ok; `POST /status` → idle.
- One hand-made targeted request with `curl` naming ONE conversation: it must return in well under a
  minute, carry `coverage.mode: "targeted"`, `enumerated_count == 1`, and the conversation's recent
  messages. **Time it** — this is the number D3's defaults are set from.
- `DATABASE_URL=<isolated> SLACK_WEB_BRIDGE_URL=… SLACK_WEB_BRIDGE_TOKEN=… CAPTURE_RULES_MODE=live
  go run ./cmd/connectors/slackweb --watch` on the workstation. Confirm the startup line, then
  `curl -sS localhost:8093/healthz` → `ok`.
- Send yourself a message in a watched DM from the phone. Expect the pass line and the `capture_rules:`
  line within ~2 minutes, and in the db: one `raw_source_items` row, one `normalized_messages` row, its
  `capture_decisions` row.
- **Interactive coexistence (L3):** while a targeted pass is running, call a `slack-web` MCP tool from
  this session. It must succeed after a short wait, or be refused with a Retry-After of **tens of
  seconds** — not ~9 minutes. This is the check that this ticket has not broken Salvador's own Slack
  tools.
- **Singleton:** start a second `--watch` — it logs standby, answers 503 on its own port, ingests
  nothing, and takes over within 15 s of the first being killed.
- **Stand-down:** while the watcher runs, run a plain one-shot `go run ./cmd/connectors/slackweb` — it
  must print the skip line and exit 0 without touching the bridge.
- **Busy:** run a rotation and a targeted pass deliberately overlapping (start the one-shot with the lock
  released) — the loser gets 503, skips, and the loop survives.
- `Ctrl-C` exits 0 with the lock released. Drop the database afterwards.

**6. Deploy — leaf first, then migration, then image, then workload; manifests belong to the kube
session.**

1. Leaf: rsync + `npm run build` + `launchctl kickstart -k gui/501/com.salvadorspataro.slack-bridge-server`
   on the mini (a session run ON the machine). Verify with the `curl` from step 5.
2. **Apply migration 0041 to prod BEFORE any image built from main runs** (the SWT-53 precedent:
   `docs/runbooks/HANDOFF-kube-chat-on-closed-task.md:3`). Nothing in the old image reads the table, so
   this direction is always safe; the reverse is not.
3. Build and push `192.168.50.20:5000/switchboard:<tag>`; record digest and commit; roll it to every
   workload, keeping the pins. Nothing behaves differently yet — the new code runs only under `--watch`.
4. Hand over `docs/runbooks/HANDOFF-kube-slack-watch-sweep.md`. The kube session creates
   `deployment/connector-slackweb-watch` (1 replica, `strategy: Recreate`, no Service, no Ingress,
   `terminationGracePeriodSeconds: 120`, liveness on `:8093`) and then — **only after** `/healthz` is 200
   and one targeted pass is in its logs — reschedules `cronjob/connector-slackweb` to `0 */2 * * *`.
   Order matters: workload first, verified, then the schedule. Never the reverse.
5. Seed the two rows with `opsctl slack-watch add`.
6. **Post-roll, 24 hours:** re-run 0b for the watched conversations (p50 must collapse to minutes);
   re-run 0e and compare the mini's self-kill/tab-recycle rate against the baseline (**the D10 gate**);
   confirm `pipelined` logs a `captured` wake within a second of each ingesting pass; confirm no
   `delivery_unconfirmed` event was written for a healthy send.

**7. IK entry** (`## The Slack watch sweep`): the two cadences and why one process owns both; the
`slack_web_watch` phase and the two readers that filter it out (with the reconciler's three-minutes-instead-of-90
trap spelled out); `monitorAbandonedExport` — a short client deadline on `/export` kills the bridge; the
leaf queue's per-priority estimate and why a 45-second sweep used to quote nine minutes; `Send`'s 503
now being definite, and why; `coverage.mode` as the only proof a leaf honoured `targets`; the D4
stand-down; `lockkeys.SlackWatch`; and the verdict of pre-check 0c on `partial`.

**8. Rollback:** `kubectl -n ops scale deploy/connector-slackweb-watch --replicas=0` and put the CronJob
back on its pre-change schedule. Slack ingestion returns to exactly today's behaviour within one tick —
the cursors, the raw rows, the rotation ordering and the delivery matcher are unchanged in both modes,
and `slack_watch` simply stops being read. The migration stays (forward-only); nothing needs un-applying.
Milder rollbacks, in order of preference: `SLACK_WATCH_INTERVAL=180s` (less browser load),
`SLACK_WATCH_INTERVAL=0` (targeted passes off, rotation only), `slack_watch_set_enabled false` per row.

## Open questions

**None.** Every fork was resolvable from the code in the two repos plus Salvador's own words, and each is
recorded above as a decision with its rationale. The two that came closest — whether the CronJob survives
(D4) and how the full export is scheduled against the targeted one (D3) — are settled by the answered
precedent in `docs/tickets/imap-idle-watch_SPEC.md` (a net, never a co-worker) and by making both
cadences env-tunable on one code path, so neither needs a decision before implementation.

**Decisions made unilaterally, worth a second look at review:**

- **The watch list is keyed by conversation, not by person.** Smaller and honest (the leaf needs
  conversation ids anyway), at the cost of a manual row when a watched person appears in a new channel.
- **The `/sources` panel is read-only.** Editing is `opsctl`, like capture rules. A dashboard form is
  Future work.
- **`partial` is left alone.** The pre-check is expected to show it is the correct status, not a defect.

## Future work (not this ticket)

- **A real incremental read.** `readChannel` has no `oldest_ts`, so every visit re-reads up to 200
  messages. A since-cursor (the `known[].last_seen_ts` field renamed to what it pretends to be) would cut
  a 15–19 s read to a few seconds and make a 30-second cadence realistic.
- **A person-grained watch list**, once Slack identities exist in `person_identities`: "watch everything
  this person posts in" resolved to conversation ids at pass time.
- **Two Chromes, two profiles, two queues** — Salvador's own idea (`slackconnector/docs/HANDOFF.md:400-403`).
  Real parallelism, no workspace switching (the navigation most implicated in the wedges), at the cost of a
  second manual login.
- **An operator signal when a watched conversation stops being readable** — today it is `sync_runs` error
  rows plus the logs.
- **Retiring or further reducing the CronJob** once the watcher has run unattended for a few weeks.
  Revisit with data, not by default.
