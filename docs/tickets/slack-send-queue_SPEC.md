> Jira: SWT-76

# slack-send-queue — the leaf ACCEPTS a send while the browser is busy and clicks it in the next gap, instead of refusing it into `failed`

**Evidence status.** Every code fact below was read in this worktree (branch `main`, at `08b0ea5`) or in the
sibling leaf repo `/home/salvo/projects/personal/slackconnector`, and is cited file:line in whichever repo
it lives. This spec session ran no SQL, no `kubectl`, and drove no browser; Verification Step 0 turns every
remaining production assumption into a read-only pre-check with a stated gate. **A gate that fails means
stop and re-spec, not adapt the code quietly.**

## Source

Ad-hoc, from Salvador, 2026-09-22, verbatim:

> so with the bridge sweep more frequent it should accept outgoing messages and q them to send in between
> sweeps not just 400s

swb task **#499**.

## Goal

Make an approved `slack_reply` survive a busy browser: the leaf **accepts** the send, queues it ahead of the
next export, and clicks it as soon as the browser frees; switchboard records the delivery as
dispatched-awaiting-confirmation (the `sending` row it already has) instead of driving it to `failed` and
back through a human re-approval — and the existing export-side confirmation by body prefix closes the loop
exactly as it does for a synchronous send today.

**Usable alone means:** with migration 0042 applied, the leaf rolled on the mini and one switchboard image
rolled, Salvador approves a Slack reply from `/deliveries` **while a rotation export is running** and:

- the dashboard flashes `delivery N queued on the bridge (job …)` instead of `slack send rejected (503)`;
- the row stays `sending`, now labelled **queued on the bridge** with its job id, and is NOT re-approvable;
- inside the next gap between passes the mini's log shows `bridge request … /send … 202` followed by the
  click, and the message is in Slack;
- the next rotation export stamps `sent_external_id` + `confirmed_at` by body prefix, the row reads `sent`,
  and the work task advances to `delivered` (D7);
- nothing was sent twice, and no human re-approved anything.

Nothing else about Slack changes: ingestion, normalization, thread keys, the watch loop, the policy tier and
the `prefill_delivery` assisted path are untouched.

## What exists (code-read, with file and line)

### switchboard's half

- **`send_delivery` routes `slack_reply` to `sendSlackReply`** (`internal/tools/delivery.go:1339-1341`),
  which is a two-phase shape: phase 1 (tx) verifies `approved`, `approval_source='switchboard'`,
  `send_enabled`, then commits `status='sending', send_attempted_at=now(), send_settled_at=NULL` and reads
  the timestamp back (`:2035-2048`). Every phase-2 write is fenced on `status='sending' AND
  send_attempted_at=$n` (`:2069-2070`).
- **The seam is `SlackSender`** — `Send(ctx, targetURL, text string) error`, one method, returning nothing
  on success because "a browser click reserves no external id" (`delivery.go:68-78`; the same statement at
  `http_bridge.go:242-244`). Implemented by `*slackweb.HTTPBridge` (`http_bridge.go:245`) and by
  `*slackweb.CommandBridge` (`bridge.go:93`, the one-shot node CLI transport). Wired at
  `cmd/dashboard/main.go:76` and `cmd/opsctl/main.go:240`, both through
  `slackweb.NewDeliveryBridgeFromEnv()`.
- **Three outcomes today** (`delivery.go:2072-2117`):
  1. `*slackweb.SendRejectedError` — DEFINITE, the click provably never happened: `status='failed'`,
     `send_settled_at=now()`, error recorded. A `failed` row **is re-approvable** — `approve_delivery`
     accepts "drafted (or failed without a sent id)" (`delivery.go:1102`) — so this path always costs a
     human a second approval.
  2. anything else — AMBIGUOUS: the row stays `sending`, `send_settled_at=now()`, nothing retries it.
  3. success — `status='sent'`, `sent_at`, and a `delivery_sent` task event (`:2099-2117`).
- **503 is currently in bucket 1.** SWT-75 D7 mapped it there on purpose (`http_bridge.go:259-267`): the
  leaf's 503 is `QueueFullError` thrown inside `JobQueue.run` **before the job function is called**
  (`job-queue.ts:148-165`), so it is provably pre-click, like a 4xx. That mapping is CORRECT and stays.
  What it costs is exactly Salvador's complaint: the message is dropped on the floor and a human has to
  re-approve it. The right fix is upstream — stop producing the 503 for a send that could simply wait.
- **`sending` means two things and the columns tell them apart** (IK, "Slack send promotion"):
  `send_attempted_at IS NOT NULL AND send_settled_at IS NULL` is IN FLIGHT; settled is ambiguous.
  `mark_delivery_failed` REFUSES an unsettled attempt younger than `sendAttemptLease = 15 * time.Minute`
  (`delivery.go:2122-2126`, `:2195-2205`) — "that refusal is what stops a human reopening a live call for a
  second send".
- **`mark_delivery_failed` is `slack_reply`-only** and its whole safety argument is that Slack wedges at
  `sending`, where `delivery_sent` never fired and orchestrator R8 never ran (`delivery.go:2157-2183`).
- **Confirmation is post hoc, by body prefix** — `slackweb.PGSink.confirmDelivery`
  (`internal/connector/slackweb/sink.go:375-488`): exact `target_ref`, `textmatch.NormalizedPrefix` over
  120 characters, `status IN ('sending','sent')`, `sent_external_id IS NULL AND confirmed_at IS NULL`, a
  2-minute-skew time floor on `send_attempted_at` (`:419-427`), multi-match REFUSAL (`:454-460`). It
  promotes a `sending` row to `sent` and stamps `sent_external_id`/`confirmed_at`/`sent_at` (`:465-472`).
- **…and it emits `delivery_confirmed`, for which there is NO orchestrator rule** (`sink.go:479-486`;
  `grep delivery_confirmed internal/orchestrator` is empty). **R8 keys on `delivery_sent` alone**
  (`internal/orchestrator/rules.go:114`, `:271-309`): mark the work task `delivered`, close R3's Deliver
  task, record the `delivery_lifecycle` dedup key. Today that is harmless because the happy path emits
  `delivery_sent` at the click. **It stops being harmless the moment a send has no click result** — which
  is every queued send. The same latent gap already exists for ambiguous rows confirmed later. D7 closes it.
  gmail's sink states the rule out loud and refuses to promote for exactly this reason
  (`internal/connector/google/sink.go:732-738`), and SWT-71 put the transition in the tool that owns it
  (`finishConfirmedSend`, `delivery.go:746-826`).
- **`ReconcileUnconfirmed`** flags — never retries — a Slack delivery after
  `DefaultUnconfirmedFlagPasses = 3` (`reconcile.go:19`) `slack_web`-phase runs that started after
  `COALESCE(sent_at, send_attempted_at, updated_at)` **and** whose `stats->'read'` contains the
  conversation (`reconcile.go:66-74`, `:116-125`). SWT-75 D6 deliberately excluded `slack_web_watch` runs.
  The flag is written into `deliveries.error` and is a **fire-once marker**: IK's rule is that every path
  starting a NEW attempt must clear that column to re-arm the alarm.
- **The executor's audit row stores status and error only** — `store.Complete(ctx, id, "ok", "")`
  (`internal/executor/executor.go:92-104`). No result payload. So "this send was queued" has to be recorded
  on the delivery row and in a task event; the audit row proves the call happened, not what it returned.
- **Dashboard**: `listDeliveries` (`internal/dashboard/server.go:240-288`) selects a fixed column list;
  `templates/deliveries.html:116-129` renders, for a `sending` `slack_reply` row, exactly two buttons —
  "It's in Slack" (`mark_delivery_sent`) and "Not in Slack" (`mark_delivery_failed`). Its execute contexts
  are 60 s (`server.go:449`, `:465`); `opsctl`'s are 30 s (`cmd/opsctl/main.go:208` and siblings).
- **`deliveries.policy_result`** is `JSONB NOT NULL DEFAULT '{}'` (`migrations/0001_initial.sql:199`) and
  holds the policy matrix's verdict. `task_events.event_type` is free TEXT, no CHECK (`:145`).

### the leaf's half (`/home/salvo/projects/personal/slackconnector`)

- **One queue owns the one browser.** `JobQueue` is non-preemptive priority: interactive ahead of any
  waiting sweep, a sweep only starts when nothing interactive is waiting, nothing preempts a running job
  (`src/browser/job-queue.ts:13-22`, `:143-146`, `:184-197`).
- **`/send` is an INTERACTIVE job** — `queue.run('interactive', 'send', …)` doing `prepareSend` then
  `sendReply` (`src/cli/bridge-server.ts:190-194`). So **the ordering Salvador wants already exists**: an
  interactive job jumps every queued sweep, and `pump()` drains `this.interactive` before `this.sweeps`
  (`job-queue.ts:189`). A send that is merely *admitted* runs in the first gap, before the next export.
- **What actually 503s a send today, verified against `run`'s admission logic** (`job-queue.ts:143-165`):
  a send is admitted immediately when nothing is running and nothing interactive waits; otherwise it is
  refused iff `tooDeep` (6 interactive waiters, `bridge-server.ts:74-78`) **or** `tooLong` —
  `estimatedWaitMs('interactive') > maxInteractiveWaitMs` (60 s, `bridge-server.ts:87-91`). So:
  - during a **targeted watch pass** (per-job estimate `targetCount * 20 s`, `bridge-server.ts:99-105`) the
    remaining estimate is under 60 s, so a send **waits and succeeds** — no 503;
  - during a **rotation export** (`estimatedSweepMs = 540_000`, `bridge-server.ts:82`) the remainder is
    minutes, so the send is **503'd**;
  - once any running job blows its estimate, `overrunFloorMs = 120_000` (`bridge-server.ts:85`,
    `job-queue.ts:122`) puts it over 60 s and the send is **503'd**.
  **The answer to the brief's question:** today `/send` waits behind a short running job and 503s behind a
  long one. It never preempts. Nothing about it is durable.
- **The 503 is produced by `handleBridgeRequest`'s catch**, with `retry_after_seconds` in the body and a
  `retry-after` header (`src/switchboard/http-bridge.ts:287-305`). Every successful route hard-codes
  `status: 200` (`:249-251`, `:275-284`).
- **`/send` is double-gated before it reaches the queue**: `writesEnabled` (`http-bridge.ts:265-267`) and
  `unattendedSend` (`:279-281`). Those stay 403s and are unaffected by anything here.
- **The leaf's own MCP tools do NOT use `/send`.** `slack_send_reply` and friends forward through
  `/op` → `forwardableOperations` → `queue.run('interactive', operation, …)`
  (`http-bridge.ts:33-47`, `bridge-server.ts:196-201`). Their 60-second `maxInteractiveWaitMs` is a
  deliberate product choice for a human waiting at a tool and must not move.
- **`/status` is browser-free by contract** ("Must never touch the browser — it is what a watchdog asks
  first", `http-bridge.ts:27`), and returns `queue.stats` plus `msSinceBrowserOk`
  (`bridge-server.ts:175-182`).
- **The process dies readily, by design.** `monitorAbandonedExport` exits on an `/export` caller
  disconnect (`src/switchboard/export-lifecycle.ts:15-29`, wired `bridge-server.ts:309-315`);
  `monitorStaleBrowserJob` restarts Chrome and exits after 240 s without browser progress
  (`bridge-server.ts:266-294`); SIGTERM closes the server and the adapter (`:346-352`). **Nothing in the
  leaf is persistent.** A queued job dies with the process.
- **Unknown request keys are ignored.** `parseDraftBody` reads `target_url` and `text` by `Reflect.get`
  and ignores everything else (`http-bridge.ts:88-100`) — so a new request field is safe against an old
  leaf, and an old leaf simply never answers 202.

## Decisions

### D1 — The leaf ACCEPTS: 200 when it can click now, **202 + a job id** when it queues

`/send` gains one branch. If the queue would admit the job immediately, nothing changes: the job runs, the
leaf answers **200** `{drafted:false, sent:true}` and switchboard emits `delivery_sent` exactly as today. If
it would not, the leaf enqueues the job, answers **202** `{queued:true, job_id, queued_at,
estimated_wait_ms, expires_in_ms}` **without awaiting it**, and clicks in the first gap.

The admission test must be **the queue's own** `canStartNow` expression (`job-queue.ts:143-146`), exposed as
`JobQueue.admits(priority)` and used by `run` itself — not re-derived in `bridge-server.ts`. Two spellings
of "can this start now" drifting apart is how a send gets enqueued that could have run, or awaited that
could not. Node is single-threaded and `run` evaluates admission synchronously before its first `await`, so
`admits()` then `run()` in the same tick cannot interleave.

**Rejected: hold the HTTP connection and answer 200 when the click eventually lands.** Three reasons, any
one sufficient. (a) The dashboard's executor context is 60 s (`server.go:449`) and `opsctl`'s is 30 s
(`cmd/opsctl/main.go:208`); a wait behind a 12-minute rotation cannot fit in either, and raising them makes
a human stare at a spinner for a quarter of an hour. (b) A client that disconnects mid-`/export` already
kills the bridge (`export-lifecycle.ts:15-29`); making `/send` long-lived invites the same class of
coupling on the write path. (c) A blown client deadline on a held connection is precisely the AMBIGUOUS
outcome (`delivery.go:2086-2097`) — we would have traded a definite refusal for an unknown, which is worse
than both the status quo and the fix.

### D2 — No per-job status endpoint and no callback: **confirmation by export is the receipt for both paths**

The leaf gets no `/job/{id}`, and never calls switchboard.

1. **The happy path needs nothing new.** `/send` has never returned a message id — "a browser click
   reserves no external id" (`http_bridge.go:242-244`) — so `sent_external_id` is stamped by the export
   matcher for a synchronous send too (`sink.go:465-472`). A queued send is confirmed by the identical
   mechanism, on the identical evidence. The 202 path does not weaken the proof; it removes a *hint*
   (`sent:true`) that was never the proof.
2. **The only answers a status endpoint could give are `not clicked` and `unknown`, and it gives the wrong
   one when it matters.** Job state would live in the same memory as the queue, and the dominant way a
   queued send is lost is the process dying — stale-job kill, abandoned-export kill, launchd restart. After
   that restart the endpoint answers `unknown`, which is the state we are already in. It buys the minority
   of cases (a TTL drop in a surviving process, made rare by D3) at the price of a poller, a persistence
   question, and a second definition of "sent".
3. **Invariant 4 means it can never shorten the path to a RESEND, only to a human**, and
   `ReconcileUnconfirmed` already is that path. Making a human arrive faster at a row that must be
   eyeballed in Slack is worth much less than it looks.

What the leaf owes instead is **legibility**: every enqueue, drop, and shutdown-loss logs the job id at
`warn`/`error`, and `/status` gains `send_queue {waiting, oldest_waiting_ms}` (browser-free, D9). A
leaf-produced send receipt — the durable fix IK has wanted since SWT-12 ("the durable fix is a
leaf-produced receipt; it needs the `/send` route first") — is Future work and is a different ticket,
because it changes what "sent" MEANS, not when it happens.

### D3 — The wait bound travels in the REQUEST (`max_queue_ms`), and the leaf refuses up front what it would drop

The caller, not the leaf, owns how long a send may sit queued, because the caller owns the 15-minute lease
that protects the row (D4). So `/send` gains an optional `max_queue_ms` and the leaf:

- **refuses immediately** — today's 503 + `Retry-After`, a DEFINITE pre-click refusal that lands the row in
  `failed`, re-approvable now — when `queue.estimatedWaitMs('interactive') > max_queue_ms`, or when the
  send queue is at depth (D9);
- **enqueues** otherwise, stamping `enqueuedAt`;
- **drops at DEQUEUE time**, before touching the browser, if `now - enqueuedAt > max_queue_ms`, logging
  `Queued send expired before the browser was free; dropped without clicking` with the job id.

This is `budget_ms`' shape on `/export` (SWT-75 D5: the bound that stops work cleanly travels in the
request). The up-front refusal is what keeps the drop rare: the leaf only accepts what its own estimate says
it can serve, and the TTL is the backstop for when that estimate was wrong — an overrunning export.

**switchboard sends `sendQueueMaxWait`, derived from the lease, never a free env value.**
`sendQueueMaxWait + sendQueueClickAllowance <= sendAttemptLease` must hold or the protection in D4 is
fiction. Ship `sendQueueMaxWait = 10 * time.Minute`, `sendQueueClickAllowance = 2 * time.Minute`,
`sendAttemptLease = 15 * time.Minute` (unchanged), with the inequality asserted by a plain unit test. An
override (`SLACK_SEND_QUEUE_MAX_WAIT`) is **clamped** to the bound and logs when it clamps — the
`positiveEnv` discipline at `export_request.go:69-82`, where an unparseable or out-of-range value falls back
rather than producing a value the leaf or the lease would reject.

**Named residual, not a defect:** a send enqueued at the start of a rotation whose budget exceeds
`max_queue_ms` is refused up front (503 → `failed` → re-approve), i.e. today's behaviour, not a regression.
The operational remedy is **two env values on the watcher, no code and no image roll** — SWT-75 D3 already
recommends `SLACK_ROTATION_INTERVAL=10m` with `SLACK_WEB_EXPORT_BUDGET_MS=300000`, which puts every
rotation inside the 10-minute acceptance window and makes refusals disappear. Verification Step 6 measures
the refusal rate and Salvador decides; this SPEC changes no watcher default.

### D4 — A queued send is a `sending` row whose attempt has **not settled**; the 15-minute lease is the "never re-approvable while the job may run" guard. **No new status.**

The brief's instruction, and the right answer: reuse. On a 202 the row is already exactly where it needs to
be — phase 1 committed `status='sending', send_attempted_at=now(), send_settled_at=NULL`
(`delivery.go:2042-2048`). The 202 branch **leaves status and `send_settled_at` alone**. Consequences,
all of them already implemented:

| requirement | what already provides it |
|---|---|
| not re-approvable | `approve_delivery` takes only `drafted`/`failed` (`delivery.go:1102`); `sending` is neither |
| nothing retries it | no code path sends a `sending` row; `send_delivery` requires `approved` (`:1993`) |
| a human cannot declare it failed while the click may land | `mark_delivery_failed` refuses an unsettled attempt younger than `sendAttemptLease` (`:2195-2205`) |
| a human CAN record it if they see it in Slack | `mark_delivery_sent` — safe by construction, and it emits `delivery_sent` so R8 advances |
| automatic closure when it lands | `confirmDelivery`'s body-prefix match (`sink.go:375-488`) |
| an alarm when it does not | `ReconcileUnconfirmed`, 3 rotation passes (`reconcile.go:116-125`) |

The asymmetry in rows 3 and 4 is deliberate and must be reflected in the dashboard (D5): **recording a send
that happened is always safe; declaring that one did not happen while a click may still be pending is
not.**

`send_settled_at` therefore keeps one meaning — "this attempt is over, in switchboard's hands again" — and
a queued attempt is honestly still open. A 202 whose leaf omitted `job_id` still takes this branch: the
protection is the unsettled attempt plus the lease, not the id. The missing id is logged as a leaf defect.

### D5 — Two nullable columns, not `policy_result`: `send_queued_at`, `send_queue_job_id` (migration 0042) — and `error=NULL` to re-arm

`policy_result` is the policy matrix's verdict (`0001_initial.sql:199`). Storing transport state there
forks the vocabulary and gives the dashboard and the tests a jsonb path to agree on by hand. Two nullable
columns are what a `SELECT` can drop and a mutation test can catch — IK's *test the column, not the
fixture*. No index (0012's argument, `migrations/0012_slack_send_attempts.sql:59`: rows are located by id
or by a bounded scan).

The 202 write, fenced identically to the other phase-2 writes (`delivery.go:2069-2070`):

```sql
UPDATE deliveries
   SET send_queued_at=now(), send_queue_job_id=$2, error=NULL, updated_at=now()
 WHERE id=$1 AND status='sending' AND send_attempted_at=$3
```

**`error=NULL` is not tidiness — it is the fire-once re-arm.** `ReconcileUnconfirmed`'s marker lives in
`deliveries.error` and it skips any row already carrying it (`reconcile.go:43`, `:73`, `:137`). IK's rule:
*a fire-once marker stored in mutable state needs a re-arm on every path that creates a new attempt.* A
queued send IS a new attempt. `send_delivery`'s success path already does this (`delivery.go:2100`);
`mark_delivery_sent` once did not, and the alarm went permanently silent for exactly the delivery it had
already caught. The queued path must not repeat it.

A `log` task event is written in the same breath — `{kind:"delivery_queued", delivery_id, job_id,
queued_at, max_queue_ms}` — never `delivery_sent` (nothing left) and never `delivery_failed` (it did not
fail, and there is no orchestrator rule for it). `task_events.event_type` is free text
(`0001_initial.sql:145`); `log` with a `kind` is the shape SWT-71 used (`delivery.go:811-813`).

`send_delivery` **returns success**: `{"delivery_id":N,"status":"sending","queued":true,"job_id":"…",
"queued_at":"…"}`. The executor's audit row records the call `ok` and stores no payload
(`executor.go:101`), which is why the queue fact lives on the row and in the event.

Dashboard (`server.go:240-288` + `templates/deliveries.html:116-129`): a `sending` `slack_reply` row with
`send_queued_at IS NOT NULL AND confirmed_at IS NULL` renders **"queued on the bridge"** with the job id and
the elapsed time; **"Not in Slack" is not rendered while the lease holds** (the verb would refuse anyway —
better to not offer it than to offer a button that errors); "It's in Slack" stays. After the lease both
buttons render as today.

### D6 — **Nothing automatically fails a queued send.** The horizon is the lease and the reconciler.

Stated explicitly because the brief asks for the horizon "after which it becomes `failed`": there is none,
and there must not be.

`failed` is re-approvable (`delivery.go:1102`), so an automatic timeout-to-`failed` is an automatic path to
a **double post** whenever the click did land and the export simply has not covered that conversation yet —
and SWT-39 established that coverage is partial: a conversation can go unread for days
(IK, "slackweb `status='ok'` … are not coverage"). The comment at `delivery.go:2108-2110` says it in the
code: *"do NOT re-approve, because approve_delivery accepts a failed row and a resend would double-post"*.

So the horizon is two-staged and both stages already exist:

1. **T + `sendAttemptLease` (15 min):** `mark_delivery_failed` becomes permitted. A human who has looked in
   Slack resolves it. The leaf can no longer click by then (D3's arithmetic: enqueue + 10 min TTL + ~30 s
   click < 15 min).
2. **3 `slack_web` rotation passes that READ the conversation:** `ReconcileUnconfirmed` appends its note
   and emits `delivery_unconfirmed`, naming `mark_delivery_sent` / `mark_delivery_failed`. Unchanged.

**The `slack_web_watch` phase exclusion stays** (SWT-75 D6). A per-minute targeted pass would satisfy the
3-pass threshold in three minutes and turn a healthy queued send into an alarm.

**Named residual:** for a queued row the reconciler's floor is `send_attempted_at` (the enqueue instant),
while the message appears at the click, up to `max_queue_ms` later. A rotation pass falling in that window
counts as "could have observed" when it could not, so a queued row that waited across a pass boundary can
be flagged after 2 real passes instead of 3. It FLAGS, never acts, and the only way to do better is to know
the click instant — which is D2's rejected status endpoint. Recorded, not fixed.

### D7 — The confirmation path must emit `delivery_sent` when it PROMOTES a `sending` row, or the task never leaves `done_locally`

**This is the defect the queue would otherwise create, and it already exists in a rarer form.**

`confirmDelivery` promotes `sending → sent` and emits only `delivery_confirmed` (`sink.go:465-486`). R8
keys on `delivery_sent` (`rules.go:114`, `:271-309`). Today the promotion path is rare (a crashed sender, an
ambiguous 500) because the happy path emits `delivery_sent` at the click. **Every queued send takes the
promotion path**, so without this the feature ships a work task stuck at `done_locally` with its Deliver
task open forever, silently — gmail's sink names exactly this hazard as its reason not to promote at all
(`google/sink.go:732-738`).

The fix, following SWT-71's four rules (IK, "A gmail send that died mid-flight"):

- The candidate SELECT reads `status` alongside `id, task_id, body`, and the promotion `UPDATE` is guarded
  `AND status=$n` with that value — **validate the value that LANDS**, never a branch that can disagree
  with the row it writes.
- Promotion **and** its events go in ONE transaction. They are two unfenced `pool.Exec` calls today
  (`sink.go:465`, `:482`), so a crash between them already loses the event; a queued send makes that event
  load-bearing.
- **Lock order: delivery, then task, and the task status is read WITHOUT a row lock.** The sinks and
  `mark_delivery_*` lock delivery → task; `refuseClosedTask` locks task → delivery and takes `FOR SHARE`
  precisely so the cycle stays open (`delivery.go:705-711`). Adding a `FOR SHARE OF t` here would close it.
- `status` was `sending` → emit `delivery_confirmed` **and** `delivery_sent {recovered:true}`.
  `status` was already `sent` → emit `delivery_confirmed` only, exactly as today.
- **Task closed since** → emit `delivery_confirmed` and a `log` `{kind:"delivery_finished"}`, **never**
  `delivery_sent`: R8 would "succeed" through `task_mark_delivered`'s closed no-op, record its
  `delivery_lifecycle` key against the task id, and mute a later real delivery after a reopen (the SWT-28
  calendar trap, restated at `delivery.go:769-771`).

Emitting a task event from a connector sink is established practice here (`sink.go:482`, `jira/sink.go:276`,
`google/sink.go:756`) and touches no orchestrator import (invariant 7).

### D8 — Ordering and starvation: the leaf's existing priority does the work; pin it

A queued send is an `interactive` job (`bridge-server.ts:190-194`). `pump()` drains `this.interactive`
before `this.sweeps` (`job-queue.ts:189`), and a sweep's `canStartNow` requires `this.interactive.length
=== 0` (`:143-146`). Therefore, with no new scheduling code:

- **a queued send runs BEFORE the next export job**, targeted or rotation;
- **the watcher cannot starve sends**: it never preempts, and a sweep will not start while a send waits;
- **sends cannot starve the watcher**: the interactive queue drains as soon as the running job ends, so a
  send costs the watcher at most one targeted pass, and a pass that finds the browser busy already skips
  cleanly (SWT-75 criterion 14).

None of that is new, all of it is now load-bearing, and none of it is currently pinned by a test that names
a send. Criteria 18-20 pin it in `tests/unit/job-queue.test.ts`.

### D9 — A bounded send queue, and what happens when the bridge dies

- **Bound:** `maxSendWaiting`, default **4** (`SLACK_CONNECTOR_SEND_QUEUE_DEPTH`). At or above it,
  today's 503 + `Retry-After` — definite, pre-click, `failed`, re-approvable. Four is generous: a fifth
  approved Slack reply waiting on one browser means something upstream is wrong and a refusal is the honest
  answer. The MCP/interactive depth (`maxInteractiveDepth = 6`, `bridge-server.ts:74-78`) is a separate
  knob and does not move.
- **Death:** the queue is in memory and dies with the process. **That is correct, and persistence would be
  actively wrong** — a replayed job after a crash cannot know whether the click landed before it, which is
  the one thing invariant 4 forbids guessing. So: **no persistence, no automatic resend, ever.**
- **A lost job surfaces as an unconfirmed delivery**, which is precisely the state the row is already in:
  `sending`, unsettled, `send_queued_at` set. It resolves through the lease or the reconciler (D6).
- **The leaf's duty is to say what it lost.** On SIGTERM/SIGINT (`bridge-server.ts:346-352`) and on the
  recovery exits (`:235-264`, `:309-315`), every waiting send job logs at `error` with its job id and
  waiting time before the process goes. `/status` gains `send_queue {waiting, oldest_waiting_ms}`, computed
  from queue state only — `/status` must never touch the browser (`http-bridge.ts:27`).

### D10 — The version gate is the 202 itself; the CLI transport can never queue

Nothing negotiates. An old leaf ignores `max_queue_ms` (`http-bridge.ts:88-100` reads two keys by name) and
answers 200 or 503 exactly as today, so **today's behaviour remains correct with no flag and no probe** —
`coverage.mode: "targeted"`'s role in SWT-75, one layer down.

`post` (`http_bridge.go:86-123`) starts returning the status alongside the body and treats 200 **and 202**
as success. **`Export` and `Draft` must REFUSE a 202** with a typed error: a 202 there would mean the leaf
queued browser work whose result those paths require, and letting it through would ingest an empty export
or report a draft that was never typed. Only `Send` interprets it.

`CommandBridge.Send` (`bridge.go:93`) spawns a one-shot node process; it has no queue and structurally
cannot answer 202, so it returns "sent, not queued" and is unchanged. The seam's new outcome type must make
that expressible without a naked zero value being misread — see the API section.

### D11 — What does NOT change

`Ingest`, `Normalize`, thread keys, direction, `raw_source_items`, the watch loop and its two cadences, the
`slack_reply` policy tier and its matrix row, the kill switch and rate limit (`send_delivery` stays
`sendShaped`, `policy/matrix.go:39`), `prefill_delivery` and the `/draft` route, `mark_delivery_sent`'s
semantics, `sendAttemptLease`, `DefaultUnconfirmedFlagPasses`, `SchemaVersion`, the 503→`SendRejectedError`
mapping from SWT-75 D7, the leaf's `maxInteractiveWaitMs` for MCP tools, `/export`'s contract, and every
other connector.

### D12 — Cross-repo order: leaf first, and it is backward compatible in both directions

1. Leaf L1-L4 ship and are deployed on the mini (`rsync` + `npm run build` + `launchctl kickstart -k
   gui/501/com.salvadorspataro.slack-bridge-server`; the memory *Mac mini operating pattern* says run that
   from a session ON the machine — the "slack" tmux session can implement this half in its own ticket). An
   old switchboard sends no `max_queue_ms`; the leaf's default (`SLACK_CONNECTOR_SEND_QUEUE_TTL_MS`,
   600 s) applies and the 202 is simply never produced for it — see below.
2. switchboard rolls (migration 0042 first, then the image). Until it rolls, a 202 would be a `bridgeStatusError`
   to the old binary and therefore AMBIGUOUS, wedging the row in `sending`. **So the leaf must only answer
   202 when the request carried `max_queue_ms`** — the field is the caller's declaration that it
   understands the queue. That one rule makes step 1 safe on its own and removes any deploy ordering
   constraint.

## Data model changes

**Migration `0042_slack_send_queue.sql`** (0041 is `slack_watch`; forward-only, numbered, never edited after
apply).

```sql
-- A queued send is a 'sending' row whose attempt is still open (send_settled_at
-- NULL) and whose click has not happened yet: the leaf accepted it and will
-- click when the browser frees. Two columns rather than a jsonb path in
-- policy_result, which is the policy matrix's verdict and not transport state.
-- No index: rows are located by id or by the dashboard's bounded scan (0012).
ALTER TABLE deliveries
  ADD COLUMN send_queued_at    TIMESTAMPTZ,
  ADD COLUMN send_queue_job_id TEXT;
```

No backfill: no row has ever been queued. No status value is added — `deliveries.status`' CHECK
(`0001_initial.sql:197`) is untouched.

Tables read or written otherwise are exactly today's: `deliveries`, `task_events`, `audit_events` +
`policy_decisions` (through the executor), `normalized_messages`/`_threads` and `sync_runs` (unchanged in
the connector).

## API / MCP tool changes

**No new tool, and no MCP surface change.** `send_delivery` is the same executor tool with the same args and
the same policy row; only its RESULT gains two keys.

| tool | result today | result on a queued send |
|---|---|---|
| `send_delivery` (slack_reply) | `{delivery_id, status:"sent"}` | `{delivery_id, status:"sending", queued:true, job_id, queued_at}` |

Invariant 3 is unchanged and unbypassed: validate → policy check → audit start → handler → audit complete,
with the queue branch entirely inside `sendSlackReply`, which is reached only from `sendDelivery`
(`delivery.go:1339-1341`). The audit row records the call `ok`; the queue fact is on the delivery row and in
the `log` task event, because `store.Complete` stores no payload (`executor.go:101`).

**Go seam.** `SlackSender` becomes:

```go
// SendOutcome distinguishes a click that HAPPENED from a leaf ACCEPTANCE.
// Queued=false is a completed send (the leaf answered sent:true); Queued=true
// means the click is still pending on the leaf's browser queue and the row
// must stay in 'sending' with its attempt unsettled.
type SendOutcome struct {
    Queued    bool
    JobID     string
    QueuedAt  time.Time
    ExpiresIn time.Duration
}

type SlackSender interface {
    Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (SendOutcome, error)
}
```

`maxQueue` is passed explicitly rather than read from the environment inside the bridge, so the one place
that owns the lease arithmetic (D3) is the one place that computes it. `CommandBridge.Send` ignores it and
returns `SendOutcome{}`. Callers branch on `Queued`, never on a zero value.

**Leaf HTTP contract** (`POST /send`), additive and optional in both directions:

```jsonc
// request  (target_url, text unchanged)
{ "target_url": "https://app.slack.com/client/T…/D…", "text": "…",
  "max_queue_ms": 600000 }                       // NEW, optional; its presence is what permits a 202

// 200 — unchanged: the click happened
{ "drafted": false, "sent": true }

// 202 — NEW: accepted, not yet clicked
{ "queued": true, "job_id": "…", "queued_at": "2026-09-22T…Z",
  "estimated_wait_ms": 213000, "expires_in_ms": 600000 }

// 503 — unchanged: definite pre-click refusal (queue full, or wait > max_queue_ms)
{ "error": "busy", "retry_after_seconds": 240 }     // + Retry-After header
```

`/status` gains `send_queue: { waiting, oldest_waiting_ms }`. `/export`, `/draft`, `/discard-draft` and
`/op` are untouched and must never answer 202.

## MQTT topics

None added, changed, or published. This ticket touches no connector pass and no worker.

## Files likely to touch

**switchboard**

- `migrations/0042_slack_send_queue.sql` (new).
- `internal/connector/slackweb/http_bridge.go` — `post` returns the status; 200/202 both success; `Export`
  and `Draft` refuse 202; `Send` gains `maxQueue`, parses the 202 body, returns `SendOutcome`. The
  503/429/4xx/dial classification is untouched.
- `internal/connector/slackweb/bridge.go` — `CommandBridge.Send`'s signature only.
- `internal/tools/delivery.go` — `SlackSender`/`SendOutcome`; `sendQueueMaxWait`,
  `sendQueueClickAllowance` and the clamped `SLACK_SEND_QUEUE_MAX_WAIT`; `sendSlackReply`'s queued branch
  (the fenced UPDATE, `error=NULL`, the `log` event, the result).
- `internal/connector/slackweb/sink.go` — D7: `status` into the candidate read, the guarded promotion, the
  single transaction, `delivery_sent` / `log` on promotion.
- `internal/dashboard/server.go` + `internal/dashboard/templates/deliveries.html` — `send_queued_at` /
  `send_queue_job_id` into `listDeliveries`' SELECT and `deliveryRow`; the "queued on the bridge" label;
  suppress "Not in Slack" while the lease holds.
- Tests (new/extended): `internal/connector/slackweb/http_bridge_send_queue_test.go`,
  `internal/tools/delivery_slack_queue_integration_test.go`,
  `internal/tools/delivery_slack_lease_test.go`,
  `internal/connector/slackweb/confirm_promotes_integration_test.go`,
  `internal/connector/slackweb/reconcile_queued_integration_test.go`,
  `internal/dashboard/deliveries_queued_integration_test.go`.
- Docs: `docs/runbooks/slack-web-connector.md`, `.claude/INSTITUTIONAL_KNOWLEDGE.md`.

**the leaf** (`/home/salvo/projects/personal/slackconnector`, separate commit, deployed first)

- `src/browser/job-queue.ts` — L1: `admits(priority)` exposing the existing `canStartNow` expression (used
  by `run` itself); `maxSendWaiting`; a `waiting(priority)` view for the shutdown log.
- `src/cli/bridge-server.ts` — L2: `/send`'s accept-or-queue branch, `enqueuedAt`, the dequeue-time TTL
  drop, the enqueue/drop/lost log lines, `send_queue` in `/status`, the shutdown drain-log.
- `src/switchboard/http-bridge.ts` — L3: `parseSendBody` accepting optional `max_queue_ms` (positive
  integer, the `optionalPositiveInteger` shape at `:118-124`); the router answering **202** when the send
  result carries `queued:true`.
- `src/config.ts` — L4: `SLACK_CONNECTOR_SEND_QUEUE_TTL_MS` (default 600000, used only when the request
  omits `max_queue_ms`), `SLACK_CONNECTOR_SEND_QUEUE_DEPTH` (default 4).
- `tests/unit/{job-queue,http-bridge}.test.ts`; `docs/HANDOFF.md` and `CLAUDE.md` there.

**Not this repo:** no manifest change is needed — no new workload, no new port, no new env on any
Deployment (the switchboard override is optional and unset). If Salvador takes D3's rotation-budget
recommendation, that is two env values on `deployment/connector-slackweb-watch` and belongs to the kube
session (IK: *kube manifests belong to the kube session*).

## In scope / Out of scope

**In scope:** the leaf's accept-and-queue `/send` with its bound, depth, TTL drop and logs; the 202 on the
Go client and the `SendOutcome` seam; `send_delivery`'s queued branch, the two columns and migration 0042;
D7's `delivery_sent` on promotion; the dashboard label and button suppression; the runbook and IK entries.

**Out of scope, each named because it is a tempting bundle:**

- **A leaf-produced send receipt / job-status endpoint / callback** (D2). It changes what "sent" means and
  deserves its own ticket.
- **Persisting the leaf's queue across restarts.** Actively refused (D9), not deferred.
- **`prefill_delivery` and `/draft`.** The assisted tier is a human standing at a composer; a synchronous
  refusal and a retry are the right ergonomics there, and its 30-second `opsctl` deadline is documented
  behaviour (IK, Slack Web connector "Accepted risks").
- **Anything in SWT-75's watcher**: intervals, budgets, the phase split, `ReconcileUnconfirmed`'s phase
  filter, `/healthz`, `slack_watch`. D3's rotation-budget recommendation is an env change for Salvador,
  proposed in verification, not made here.
- **Promoting `slack_reply` off the approve tier**, raising `sendAttemptLease`, or any other policy-matrix
  row.
- **Preempting a running sweep**, a since-cursor on reads, or per-workspace parallelism (a second Chrome).
  A running job runs to completion; this ticket buys nothing by changing that.
- **A compensating lifecycle transition for a delivery failed after R8 fired** (IK; SWT-20's deferred
  work). D7 deliberately adds an event; it walks none back.
- **The `upwork_chat` and gmail send paths.** Untouched.

## Invariants that apply

1. **Raw-first** — no connector pass changes. Nothing new writes `raw_source_items`; D7 edits only the
   post-normalize confirmation step, which runs after the raw write (`sink.go:340-371`).
2. **One funnel** — no new table and no new status. The queue is state on the existing `deliveries` row;
   `slack_watch`-style configuration is not involved. Nothing task-like is created.
3. **Everything through the executor** — the queued branch lives inside `sendSlackReply`, reachable only
   from `send_delivery`'s handler, which the executor runs validate → policy → audit start → handler →
   audit complete (`executor.go:60-104`). No new tool, no new HTTP verb on the dashboard, no side door. The
   audit row records the call; the payload-less `Complete` is why the queue fact is also written to the row
   and to a task event.
4. **Nothing external without a delivery row, idempotent sends** — the queued job exists ONLY because an
   approved `deliveries` row passed the matrix and phase 1 committed `sending` with `send_attempted_at`
   BEFORE anything left the process. **Never a double send:** nothing retries a `sending` row; the leaf
   holds at most one job per call and never replays after a restart (D9); `sent_external_id` is stamped
   once by the matcher under `WHERE sent_external_id IS NULL` (`sink.go:465-472`); a lost job becomes an
   unconfirmed delivery a human resolves, never an automatic `failed` (D6) and therefore never an automatic
   re-approval. The two definite refusals (queue full, wait over bound) keep today's `failed` path, which
   is safe precisely because they are pre-click (`job-queue.ts:148-165`).
5. **Own-message loop closure** — strengthened, not weakened. A queued send's only receipt IS loop closure:
   `Normalize` → `upsertMessage` → `confirmDelivery` stamps it from our own message re-entering ingestion,
   and D7 makes that stamp finally advance the task instead of stopping at an event nothing reads. The
   matcher's rules are untouched: canonical `target_ref`, `textmatch.NormalizedPrefix` (IK: four matchers,
   one spelling, enforced by `internal/textmatch/callsites_test.go`), the 2-minute skew floor, multi-match
   refusal. Our sends still attach to their delivery row and are never re-triaged.
6. **Stealth attribution** — the body is unchanged and still passes `google.ScrubAIAttribution` at dispatch
   (`delivery.go:2055`). Nothing new is written to a client-visible surface.
7. **Orchestrator purity** — `internal/orchestrator` is neither imported nor modified. D7 writes a
   `task_events` row from a connector sink, which is what makes R8 fire; R8 itself is unchanged and stays a
   pure function of (event, task, policy). No LLM anywhere in this ticket.

## Acceptance criteria

### Part 1 — the leaf accepts instead of refusing

1. With a job RUNNING and the interactive queue empty, `POST /send` with `max_queue_ms` returns **202**
   with `queued:true` and a non-empty `job_id`, **before** the running job finishes, and the click happens
   after it (unit, fake clock + fake adapter).
2. With the browser IDLE, `POST /send` returns **200** `{drafted:false, sent:true}` and the response is not
   sent until the click completed — byte-identical to today.
3. `POST /send` **without** `max_queue_ms` never returns 202: it waits or 503s exactly as today (D12's
   deploy-order rule). Removing that condition turns the test red.
4. 503 with `Retry-After` when `estimatedWaitMs('interactive') > max_queue_ms`, and when the send queue is
   at `maxSendWaiting`. Both are thrown before any browser call (assert the fake adapter was never
   touched).
5. A job whose wait exceeded `max_queue_ms` by the time the queue reaches it is DROPPED without touching
   the browser and logs its job id; the queue proceeds to the next job.
6. `writesEnabled=false` → 403, `unattendedSend=false` → 403, on the queued path as on the direct one: the
   gates are evaluated before any enqueue (`http-bridge.ts:265-281`).
7. `/export`, `/draft`, `/discard-draft` and `/op` never return 202 under any queue state.
8. `/status` reports `send_queue {waiting, oldest_waiting_ms}` and makes zero adapter calls.
9. On SIGTERM and on the stale-job/abandoned-export exits, every waiting send logs at `error` with its job
   id and waiting time before the process exits; no waiting job is run during shutdown.

### Part 2 — the Go client

10. `post` returns the HTTP status; 200 and 202 are success, everything else is `bridgeStatusError` as
    today. `Export` and `Draft` return a typed refusal on 202 and ingest/report nothing.
11. `Send` on 202 returns `SendOutcome{Queued:true, JobID, QueuedAt, ExpiresIn}` and a nil error; on 200
    with `{sent:true}` it returns `SendOutcome{Queued:false}` and nil; `checkSendResult` is not applied to a
    202 body.
12. A 202 whose body omits `job_id` still returns `Queued:true` (with an empty `JobID`) — the protection is
    the unsettled attempt, not the id.
13. The SWT-75 classification is intact: 503, 429 and 4xx → `SendRejectedError`; a dial failure →
    `SendRejectedError`; 500 and a mid-response EOF stay ambiguous. Reverting any of it turns a test red.
14. `sendQueueMaxWait + sendQueueClickAllowance <= sendAttemptLease` is asserted by a unit test; an
    `SLACK_SEND_QUEUE_MAX_WAIT` above the bound is clamped and logged, and an unparseable or non-positive
    value falls back to the default.

### Part 3 — the delivery row

15. `send_delivery` on a 202 returns success with `{status:"sending", queued:true, job_id}`, and the row has
    `status='sending'`, `send_settled_at IS NULL`, `send_queued_at` set, `send_queue_job_id` set, `error`
    **NULL**. Integration, asserted against Postgres. (Mutating the UPDATE to drop `error=NULL` turns the
    re-arm test red.)
16. That UPDATE is fenced on `status='sending' AND send_attempted_at=$n`: a row resolved by another actor
    between dispatch and return is not overwritten, and the call reports it.
17. A `log` task event `{kind:"delivery_queued", …}` is written, and **no** `delivery_sent` and **no**
    `delivery_failed` event exists for that delivery at that point.
18. `approve_delivery` and `send_delivery` both REFUSE the queued row while it is `sending`
    (existing behaviour; pinned here because it is now the guard).
19. `mark_delivery_failed` refuses the queued row for `sendAttemptLease` from `send_attempted_at`, naming
    the in-flight attempt, and is permitted after it. `mark_delivery_sent` is permitted throughout and
    emits `delivery_sent`.
20. The executor wrote `audit_events` start+complete rows with `status='ok'` and a `policy_decisions` row
    for the queued call — a queued send is an allowed, audited call, not an error.

### Part 4 — ordering and the two cadences (leaf unit tests, fake clock)

21. A send enqueued while a sweep runs executes BEFORE a sweep queued after it, and before a sweep
    submitted while the send waits. Reordering `pump()`'s `interactive ?? sweeps` turns it red.
22. A sweep submitted while a send waits is refused (`maxSweepDepth: 0`) rather than started ahead of it;
    removing `this.interactive.length === 0` from the sweep's `canStartNow` turns it red.
23. `admits()` and `run()`'s admission agree by construction — a test that drives both across
    {idle, running-sweep, running-interactive, waiting-interactive} and asserts they never disagree. Two
    hand-written copies of the predicate turn it red.

### Part 5 — the loop closes, and the task moves (D7)

24. A `sending` `slack_reply` row that was queued, whose own message then arrives in an export, is
    promoted to `sent` with `sent_external_id` and `confirmed_at`, and emits **both** `delivery_confirmed`
    and `delivery_sent`. Integration.
25. An already-`sent` row confirmed by the same matcher emits `delivery_confirmed` ONLY — no second
    `delivery_sent`, no duplicate R8. A `--all` replay emits nothing new (the `sent_external_id IS NULL`
    guard plus the RowsAffected check).
26. When the task is CLOSED, the promotion emits `delivery_confirmed` plus a `log`
    `{kind:"delivery_finished"}` and **never** `delivery_sent`. Mutating it to emit `delivery_sent` turns a
    test red, and the test's comment names the SWT-28 `delivery_lifecycle` dedup trap.
27. Promotion and its events commit in one transaction: a forced failure on the event insert leaves the row
    UNPROMOTED. The transaction locks the delivery before reading the task and takes no row lock on
    `tasks` — a structure/comment test names `delivery.go:705-711`'s lock-order argument.
28. End-to-end integration: queued send → confirmation → R8 → the work task reads `delivered` and its
    Deliver task is closed. Dropping the `delivery_sent` emission leaves the task at `done_locally` and
    turns this red.
29. `ReconcileUnconfirmed` still ignores `slack_web_watch` runs and still flags after 3 `slack_web` runs
    that read the conversation; a queued row is an ordinary candidate. Ten watch runs flag nothing.

### Part 6 — the dashboard

30. A `sending` `slack_reply` row with `send_queued_at` set renders "queued on the bridge" with the job id,
    does NOT render "Not in Slack" while the lease holds, and does render it after. A `sending` row with
    `send_queued_at` NULL renders exactly as today.
31. `listDeliveries` renders correctly when both new columns are NULL for every row, and the page's
    existing single-query failure mode is not widened.
32. The flash after a queued send says it was queued and names the job id, not an error.

### Part 7 — docs

33. `docs/runbooks/slack-web-connector.md` gains a "What happens when the browser is busy" section: the
    three outcomes (200 / 202 / 503), what a queued row looks like in the dashboard and in SQL, the two
    horizons (lease, reconciler), what a bridge restart means for a queued send, and the explicit rule that
    **nothing ever resends**.
34. The leaf's `npm run check` (lint, typecheck, full unit suite) is green, and `docs/HANDOFF.md` +
    `CLAUDE.md` there record `/send`'s 202, `max_queue_ms`, the TTL drop and the shutdown log.

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| Answer 200-after-waiting instead of 202 when the queue is busy | criterion 1 |
| Answer 202 when the request omitted `max_queue_ms` | criterion 3 |
| Enqueue instead of 503 when the estimate exceeds `max_queue_ms` | criterion 4 |
| Click a job whose TTL has expired | criterion 5 |
| Check `writesEnabled`/`unattendedSend` after the enqueue | criterion 6 |
| Return 202 from `/export` | criteria 7, 10 |
| Treat a 202 as `bridgeStatusError` in `post` | criterion 11 |
| Run `checkSendResult` on a 202 body | criterion 11 |
| Revert the 503 → `SendRejectedError` mapping | criterion 13 |
| Raise `sendQueueMaxWait` above `sendAttemptLease` | criterion 14 |
| Set `send_settled_at=now()` on the queued branch | criteria 15, 19 |
| Drop `error=NULL` from the queued UPDATE | criterion 15 |
| Drop the `send_attempted_at` fence from the queued UPDATE | criterion 16 |
| Emit `delivery_sent` at enqueue time | criteria 17, 28 |
| Swap `pump()`'s `interactive ?? sweeps` order | criterion 21 |
| Drop `this.interactive.length === 0` from the sweep's admission | criterion 22 |
| Re-derive the admission predicate in `bridge-server.ts` | criterion 23 |
| Emit only `delivery_confirmed` on a `sending → sent` promotion | criteria 24, 28 |
| Emit `delivery_sent` on an already-`sent` confirmation | criterion 25 |
| Emit `delivery_sent` when the task is closed | criterion 26 |
| Split the promotion and its events into two `pool.Exec` calls | criterion 27 |
| Add `FOR SHARE OF t` to the promotion's task read | criterion 27 |
| Count `slack_web_watch` runs in `ReconcileUnconfirmed` | criterion 29 |
| Render "Not in Slack" on a queued row inside the lease | criterion 30 |
| Drop `send_queued_at` from `listDeliveries`' SELECT | criteria 30, 31 |

## Sibling patterns to copy

- **A send that already left, finished from its confirmation:** `internal/tools/delivery.go:746-826`
  (`finishConfirmedSend`) and SWT-71's four rules in IK. D7 is the same problem on the Slack side, where
  the sink rather than the tool owns the promotion — read both before writing either.
- **The three-outcome send shape and its fences:** `internal/tools/delivery.go:2035-2117`. The queued
  branch is a fourth outcome in the same style; copy the fence constants rather than re-spelling them.
- **Post-hoc body matching:** `internal/connector/slackweb/sink.go:375-488` and the IK entry "Exact text
  comparison across a provider round trip". `internal/textmatch` is the ONE spelling and
  `internal/textmatch/callsites_test.go` enforces it mechanically — do not add a second.
- **Env knob discipline (clamp, never a zero the far side rejects):**
  `internal/connector/slackweb/export_request.go:69-82` (`positiveEnv`).
- **Request-carried bounds on leaf work:** `ExportRequest.BudgetMS` and SWT-75 D5
  (`docs/tickets/slack-watch-sweep_SPEC.md`), the direct precedent for `max_queue_ms`.
- **Leaf queue semantics and its tests:** `src/browser/job-queue.ts` with
  `tests/unit/job-queue.test.ts`; the router's status mapping with `tests/unit/http-bridge.test.ts`.
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used — nothing is claimed here. **rag-svc HTMX:** the
  dashboard change is a label and a conditional button in an existing template, no new interaction.

## Verification protocol

Run in order. Do not commit before step 4 passes. Capture the exit status of every `go test` separately
from any pipe (IK: *gate commits on test exit status*; a bare `set -e` is not honoured in the Bash tool).

**0. Read-only pre-checks.** SQL inside `BEGIN READ ONLY; … ROLLBACK;` against `psql -h 192.168.50.49 -U
ops -d ops`; the leaf's `/status` and the mini's log are reads. NOT run by the spec session. Paste the
results into the delivery summary and assert none of them as a frozen literal (IK: production counts are
not frozen literals).

- **0a. How often a send actually meets a busy browser — the gate on the whole ticket.**

  ```sql
  SELECT status, count(*), min(created_at), max(updated_at),
         count(*) FILTER (WHERE error ILIKE '%503%')   AS busy_refusals,
         count(*) FILTER (WHERE error ILIKE '%unconfirmed after%') AS flagged
    FROM deliveries WHERE channel='slack_reply' GROUP BY 1 ORDER BY 1;
  ```

  **Gate:** if there are no `slack_reply` rows at all, the end-to-end smoke in step 5 has to create one by
  hand; the ticket proceeds either way, but say so in the summary rather than claiming a production
  baseline that does not exist.
- **0b. The R8 gap D7 claims, on real rows.** Deliveries confirmed by the matcher whose task never advanced:

  ```sql
  SELECT d.id, d.status, d.confirmed_at, t.id AS task_id, t.status AS task_status
    FROM deliveries d JOIN tasks t ON t.id = d.task_id
   WHERE d.channel='slack_reply' AND d.confirmed_at IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM task_events e
                      WHERE e.task_id = t.id AND e.event_type='delivery_sent'
                        AND (e.payload->>'delivery_id')::bigint = d.id)
   ORDER BY d.id;
  ```

  **Gate:** any row here is a task D7 will unstick and a fact for the delivery summary. Zero rows is also
  fine — the gap is structural (`rules.go:114` vs `sink.go:482`), not empirical — but say which you found.
- **0c. The browser's real duty cycle**, which is what decides whether D3's rotation-budget recommendation
  is needed: over the last 24 h of the mini's bridge log, the count and duration of `export` vs
  `export:targeted` jobs, plus `curl -s localhost:8787/status` taken a few times. **Gate:** record the
  fraction of the day a rotation is running; if it exceeds `max_queue_ms`, recommend
  `SLACK_ROTATION_INTERVAL=10m` + `SLACK_WEB_EXPORT_BUDGET_MS=300000` to Salvador in the summary.
- **0d. Confirm the leaf version on the mini** (`git -C ~/projects/personal/slackconnector log -1`, and
  that the running bridge is the built dist), so step 5 is not run against a stale process.

**1. Leaf half** (in the mini's "slack" tmux session, or locally then deployed):
`npm run check` — lint, typecheck, full unit suite, all green. Then `rsync` + `npm run build` +
`launchctl kickstart -k gui/501/com.salvadorspataro.slack-bridge-server`, and confirm `/healthz` and
`/status` answer.

**2. Migration:** `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate` — 0042 applies; `\d
deliveries` shows both columns nullable.

**3. Unit + structure:** `go test ./internal/... ./cmd/...` (capture `rc`). Includes the textmatch callsite
scan, the lock-key collision scan and the send-guard structure test, all of which must stay green.

**4. Integration:** `go test -tags=integration ./internal/tools/... ./internal/connector/slackweb/...
./internal/dashboard/...` against the dockerized local Postgres (capture `rc`). Then run the mutation table
above, one at a time, confirming each named test goes red and reverting.

**5. Manual smoke — the "usable alone" check.** Requires the mini and an Avviato conversation.

   a. Start a rotation export by hand (or wait for one) so the browser is busy for minutes:
      `curl -s -XPOST -H "Authorization: Bearer $TOK" localhost:8787/status` shows `running: "sweep"`.
   b. Draft and approve a `slack_reply` to a real conversation, then
      `opsctl call --tool send_delivery --args '{"delivery_id":N}'`.
   c. **Expect:** exit 0, a result carrying `"queued":true` and a job id; the mini's log shows
      `/send … 202`; `psql` shows `status='sending'`, `send_settled_at IS NULL`, `send_queued_at` set,
      `error` NULL; `/deliveries` shows "queued on the bridge" with no "Not in Slack" button.
   d. `mark_delivery_failed` on it REFUSES, naming the in-flight attempt.
   e. When the rotation ends, the message appears in Slack within seconds (before the next targeted pass),
      and the mini's log shows the click.
   f. After the next rotation export: `sent_external_id` and `confirmed_at` are set, `status='sent'`, and
      the work task reads `delivered` with its Deliver task closed.
   g. **Negative check, same session:** with the browser busy, fire five sends so the queue fills; the
      fifth returns 503 and its row is `failed` and re-approvable — today's behaviour, deliberately
      preserved.

**6. 24-hour soak, before calling it done.** Re-run 0a and 0c. Expect: busy refusals down to the overflow
cases only, zero duplicate messages in any watched conversation (eyeball, plus
`SELECT sent_external_id, count(*) FROM deliveries WHERE channel='slack_reply' GROUP BY 1 HAVING count(*)>1`
returning nothing), and the mini's self-kill rate no worse than SWT-75's baseline.

## Verification record (2026-09-22, delivery)

- **0a:** prod has **zero `slack_reply` deliveries** — none ever drafted through switchboard. So there is no
  production baseline of busy refusals; the 503s Salvador has met come from the leaf's OTHER caller, the
  `slack-web` MCP tools (`slack_send_reply`, an interactive job bounded by `maxInteractiveWaitMs` 60 s),
  which this ticket's `/send` contract does not cover (D9 leaves the MCP depth alone). Flagged to
  Salvador in the delivery note; the end-to-end smoke (step 5) needs a target conversation of his choosing.
- **0b:** zero confirmed `slack_reply` rows whose task lacks `delivery_sent` — the R8 gap is structural,
  not (yet) empirical on prod. **Dupes:** none.
- **0c/0d:** not measured from here (the mini's log is the slackconnector session's; it reported the leaf
  live at `0d078c6`, 20:02Z, `/status` answering with `send_queue`). The rotation-budget recommendation
  (D3) is left for the 24-hour soak.
- **1. Leaf half:** shipped and deployed by the slackconnector session (its report: `npm run check` green,
  every spec mutation red except criterion 22's dead-code clause, which it explains).
- **2. Migration:** applied to the scratch db (`ops_sendqueue`, 42 migrations); prod at delivery time.
- **3. Unit + structure:** `go vet ./...` and `go test ./...` green; `-race` green on slackweb, tools and
  dashboard.
- **4. Integration:** `go test -tags integration -p 1 ./internal/tools/ ./internal/connector/slackweb/
  ./internal/dashboard/` green **twice** on `ops_sendqueue` (rerunnable cleanup held), including D7 end to
  end through the real orchestrator engine (work task `delivered`, Deliver task `closed`) and the forced
  event-insert failure leaving the row unpromoted. Mutations not run individually (each maps to a test
  that was red first).
- **5. Manual smoke:** deferred to after the roll, against a conversation Salvador names — there is no
  `slack_reply` row on prod to reuse and the send is client-visible.
- **Test-side amendments (dated in the files):** `send_test.go`, `send_503_test.go` and
  `delivery_slack_integration_test.go`'s fake re-signatured for `Send(…, maxQueue) (SendOutcome, error)`;
  assertions unchanged. Test-author's deviation accepted: `SendOutcome` lives in `slackweb` (import cycle
  otherwise). Criterion 14's contradiction with rollback lever 1 resolved by `SLACK_SEND_QUEUE_MAX_WAIT=off`.

## Rollback

Three independent levers, smallest first, none requiring a revert of the other repo:

1. **Stop producing 202s** — unset `max_queue_ms` on the switchboard side (`SLACK_SEND_QUEUE_MAX_WAIT=off`
   means "never queue"; `0` and any non-positive value fall back to the default, criterion 14 — the
   test-author caught the contradiction between the two, resolved 2026-09-22 by giving "never" its own
   spelling). The leaf then waits or 503s exactly as today. No roll, no restart.
2. **Roll back the leaf** — `git revert` + build + `launchctl kickstart`. An unchanged switchboard sends
   `max_queue_ms`, the old leaf ignores it, and everything is today's behaviour.
3. **Roll back the switchboard image.** Migration 0042 stays (forward-only, additive, nullable; the old
   binary never selects the columns). Rows already queued are ordinary `sending` rows an old binary handles
   correctly — the lease and the reconciler are unchanged, and D7's already-emitted `delivery_sent` events
   are already processed.

**Not reversible by any of the above, and therefore the thing to be sure about before merging:** D7's
`delivery_sent` on promotion is a real lifecycle transition. Once it fires, R8 has marked tasks delivered
and closed Deliver tasks. That is the correct outcome and is the same transition the happy path has always
made, but it is the one change here that moves task state.

## Open questions

None. The two calls the brief flagged as arguable — whether the leaf needs a job-status surface (D2) and
what the horizon is before a queued row may be failed (D6) — are decided above with the evidence for each,
and both resolve toward reusing machinery that already exists rather than adding a surface.

**Decisions made unilaterally, each with its rationale in the D above:** D2 (no status endpoint), D3
(the bound travels in the request and is derived from the lease, not an independent env value), D5 (two
columns rather than `policy_result`), D6 (nothing automatically fails a queued send), D7 (the promotion
emits `delivery_sent` — the one scope expansion in this ticket, taken because without it the feature
silently strands every task it touches), D9 (bounded, non-persistent queue), D12 (`max_queue_ms`'s presence
is the version gate, which removes the deploy ordering constraint).

## Future work

- **A leaf-produced send receipt.** IK has wanted it since SWT-12: a click that returns something
  correlatable would make confirmation-by-body-prefix a belt rather than the only proof, and would let a
  lost queued job be reported as definitely-not-sent. It changes what "sent" means; separate ticket.
- **`prefill_delivery` over the same acceptance path**, if the assisted tier ever becomes unattended.
- **Counting watch passes toward `ReconcileUnconfirmed` with a per-phase threshold**, which would shorten
  the alarm horizon from ~90 minutes to minutes without the false-flag risk SWT-75 D6 avoided.
- **A yielding rotation** — checking between conversation reads whether interactive work waits — which
  would remove D3's up-front refusal entirely. It is a preemption change to the leaf's export loop and
  wants its own risk budget.
- **Surfacing queued/unconfirmed Slack deliveries on `/funnel`**, so a stuck one is visible without
  reading `/deliveries?status=sending`.
