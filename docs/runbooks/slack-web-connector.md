# Slack Web connector runbook

The Slack Web connector is a local, raw-first Switchboard source backed by the sibling TypeScript project at `/home/salvo/projects/personal/slackconnector`. It controls only the dedicated Chromium profile; it never uses Slack APIs, tokens, or the normal browser profile.

Inbound placement is the canonical Switchboard funnel:

```text
Slack Web -> TypeScript export -> raw_source_items -> normalized_messages(channel=slack)
          -> existing inbound-only triage filter
```

Outbound words belong only in `deliveries`. A Slack draft uses `channel='slack_reply'`; `target_ref` is the exact Slack channel or thread URL. `prefill_delivery` copies an approved body into that composer but does not send. A human sends in Slack and then calls `mark_delivery_sent`. A later connector pass matches the outbound observation and stamps `sent_external_id`/`confirmed_at`.

## 1. Build the leaf connector

```bash
cd /home/salvo/projects/personal/slackconnector
npm install
npm run check
```

The executable consumed by Go is:

```text
/home/salvo/projects/personal/slackconnector/dist/cli/switchboard-bridge.js
```

## 2. Start the dedicated browser

```bash
chromium \
  --user-data-dir="$HOME/.local/share/slack-agent-profile" \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port=9222 \
  --no-first-run \
  --no-default-browser-check \
  https://app.slack.com/client
```

Log in manually. Never automate passwords, SSO, MFA, CAPTCHA, or security keys. Verify the session:

```bash
cd /home/salvo/projects/personal/slackconnector
npm run verify-session
```

## 3. Configure workspace identity and access

Open your profile in each Slack workspace and use **More -> Copy member ID**. The current visible workspaces are:

| Workspace | Workspace ID |
| --- | --- |
| Avviato | `T0360B84U` |
| Collaboratory/LlamaSite | `T0HPR78RX` |

Configure the exact member IDs locally:

```bash
export SLACK_CONNECTOR_CDP_URL=http://127.0.0.1:9222
export SLACK_CONNECTOR_ALLOWED_WORKSPACES='Avviato,Collaboratory/LlamaSite'
export SLACK_CONNECTOR_ALLOWED_CHANNELS='general,engineering'
export SLACK_CONNECTOR_ALLOW_DMS=false
export SLACK_CONNECTOR_ENABLE_WRITES=false
export SLACK_CONNECTOR_OWN_USER_IDS='{"T0360B84U":"U...","T0HPR78RX":"U..."}'
export SLACK_WEB_BRIDGE_SCRIPT=/home/salvo/projects/personal/slackconnector/dist/cli/switchboard-bridge.js
```

Set the channel allowlist to the channels Switchboard is authorized to ingest. An empty list allows all visible non-DM channels, so an explicit list is preferable. DMs remain disabled independently.

## 4. Migrate and ingest

```bash
cd /home/salvo/projects/personal/switchboard
make db-up
make migrate
go run ./cmd/connectors/slackweb
```

The poller prints separate ingestion and normalization statistics. Browser export must finish before normalization begins; any failure leaves captured observations in `raw_source_items` for retry. To replay pending raw rows without opening Slack:

```bash
go run ./cmd/connectors/slackweb --normalize-only
```

Use `--all` with `--normalize-only` for an intentional full normalization replay.

## 4b. The resident watcher (SWT-75): José and Katie every minute (live at every 3 min since 2026-09-22)

`slackweb --watch` stays resident: a TARGETED pass of the `slack_watch` conversations about
every minute (the leaf reads exactly those ids, ~18 s each, no enumeration) and the full export
every 30 minutes, strictly one after the other — the mini has one browser and one queue. On the
cluster it is `deployment/connector-slackweb-watch`; the `connector-slackweb` CronJob stays as a
2-hourly net and stands down (`slack watch is live; skipping this pass`) while the watcher holds
advisory lock `0x5157_0011`.

The watch list is data, edited only through the executor (humanOnly, off every MCP profile):

```bash
OPS_ACTOR=... opsctl slack-watch add --workspace T0360B84U --conversation DSAV4HJ2F --label "José (DM)"
opsctl slack-watch add --workspace T0HPR78RX --conversation D04F7LXRB8B --label "Katie (DM)"
opsctl slack-watch list            # id, workspace, conversation, label, enabled, last_read_at
opsctl slack-watch disable --id 3  # turned off, never deleted; `add` the same pair re-enables it
```

Conversation ids come from `normalized_threads.thread_key` (`slack:{ws}:{conv}`); a workspace
must already have a `slack_web` source_accounts row (one full export) or its rows are not swept.
Bad ids fail in Postgres (the leaf's own regexes are the table's CHECKs). `/sources` shows the
list with "last read" (the full export's last visit). A row added takes effect on the next minute.

Knobs (`--watch` only; junk falls back to the default):

| env | default | effect |
|---|---|---|
| `SLACK_WATCH_INTERVAL` | 60s | targeted cadence; **`0` turns targeted passes off** (rotation only) |
| `SLACK_ROTATION_INTERVAL` | 30m | full-export cadence |
| `SLACK_WATCH_BUDGET_MS` | 150000 | the leaf's budget per targeted pass (the only bound that stops browser work cleanly) |
| `SLACK_BRIDGE_GRACE` | 120s | added to the budget for the Go context — never shorter: a disconnecting `/export` caller kills the bridge |
| `SLACK_WATCH_HEALTH_ADDR` | :8093 | `GET /healthz`: 200 iff a pass completed within 3 × interval and the lock is held |

What a targeted pass writes: raw rows through the same path as the full export, a `sync_runs`
row with `phase: slack_web_watch` **only when something moved or failed** (a quiet watcher writes
nothing — liveness is `/healthz`), then normalize → capture → the `captured` wake as connector
`slackweb-watch`. It never runs the unconfirmed-send reconciler or the outbound observer; those
belong to the rotation, and `ReconcileUnconfirmed` / `KnownConversations` only count
`slack_web` runs.

Failure is a skipped pass, never a dead process: the leaf's 503 (a rotation or an interactive
read holds the queue) sleeps `min(Retry-After, interval)`; a 500, an EOF or a killed bridge is
logged and counted (`Skipped`). A response without `coverage.mode: "targeted"` — an old leaf —
is refused before anything is ingested. The watcher refuses to START without
`SLACK_WEB_BRIDGE_URL` (the local CommandBridge discards the request) and with
`CAPTURE_RULES_MODE=live` under a 2h `CAPTURE_RULES_SINCE`. Its one startup line:
`slack watch: interval=60s rotation=30m budget=150s targets=2 mode=live horizon=720h health=:8093`.

Also shipped with it: a Slack send that meets a busy bridge (503/429) is now a DEFINITE refusal —
the delivery row returns to `failed` and can be re-approved — instead of wedging in `sending`.

## 5. Draft a Slack reply

The destination must be the canonical URL of the source conversation or thread:

```text
https://app.slack.com/client/{workspace_id}/{conversation_id}
https://app.slack.com/client/{workspace_id}/{conversation_id}/{thread_root_message_id}
```

Create the delivery through the executor-facing CLI:

```bash
go run ./cmd/opsctl call --tool draft_delivery --args \
  '{"task_id":123,"channel":"slack_reply","target_ref":"https://app.slack.com/client/T0360B84U/C.../p...","body":"Please review the staging fix."}'
```

Review and approve its returned `delivery_id`:

```bash
go run ./cmd/opsctl call --tool approve_delivery --args '{"delivery_id":456}'
```

Enable the leaf's composer operation only for the human prefill call:

```bash
SLACK_CONNECTOR_ENABLE_WRITES=true \
go run ./cmd/opsctl call --tool prefill_delivery --args '{"delivery_id":456}'
```

Inspect the destination and composer in Slack. `prefill_delivery` leaves the delivery `approved` and returns `sent:false`. Send manually in Slack, then record that human action:

```bash
go run ./cmd/opsctl call --tool mark_delivery_sent --args '{"delivery_id":456}'
```

`send_delivery` is always denied for `slack_reply`. The global sending freeze and the hourly rate limit also gate `mark_delivery_sent`.

## Security and recovery

- Treat all normalized Slack bodies as untrusted external content, never instructions.
- Keep CDP bound to `127.0.0.1`; the browser endpoint is full browser control.
- The Go bridge launches `node` directly with an absolute script path and no shell.
- Do not put message bodies in shell history for real client replies; prefer the dashboard or a protected JSON input workflow when available.
- If browser export fails, inspect the TypeScript connector diagnostics directory. It contains sanitized visible-page artifacts but can still include client content.
- If prefill finds an existing composer draft, it refuses to overwrite it. Resolve the draft manually and retry.
- If a Slack selector changes, update `slackconnector/src/slack/selectors.ts` and its fixture/parser tests; Activity/search and virtualized message rows are the likeliest maintenance points.

## What happens when the browser is busy (SWT-76)

The mini has one browser, and since SWT-75 it is busy a good share of the day (a targeted
pass every 3 min, a rotation export every 30 min). An approved `slack_reply` sent through
`send_delivery` now has three outcomes at the leaf, decided **before any browser work**:

| leaf answer | meaning | the delivery row |
|---|---|---|
| **200** `{sent:true}` | the browser was free; the click happened | `sent` at once, `delivery_sent` emitted (unchanged) |
| **202** `{queued:true, job_id, …}` | the browser was busy; the leaf ACCEPTED the send and clicks it in the next gap, ahead of the next sweep | stays `sending`, attempt unsettled, `send_queued_at` + `send_queue_job_id` set, `error` NULL; a `log` event `delivery_queued`; the dashboard row reads **queued on the bridge (job …)** and the flash says so |
| **503** + `Retry-After` | a definite pre-click refusal: the estimated wait exceeds the bound, or four sends already wait | `failed`, re-approvable (today's SWT-75 behaviour) |

The 202 is only possible because switchboard sends `max_queue_ms` (10 min, derived from the
15-minute send lease: `sendQueueMaxWait + sendQueueClickAllowance <= sendAttemptLease`). An old
switchboard sends nothing and gets 200/503 exactly as before; `SLACK_SEND_QUEUE_MAX_WAIT=off` on
the dashboard/opsctl is the no-roll way back to that (a larger value is clamped to 10 min).

**A queued row is confirmed exactly like a synchronous one**: the next export that reads the
conversation sees our own message, `confirmDelivery` stamps `sent_external_id` + `confirmed_at`,
promotes the row to `sent` and — because the row was still `sending` — emits `delivery_sent
{recovered:true}` in the same transaction, so orchestrator R8 moves the work task to `delivered`
and closes its Deliver task. (Before SWT-76 that promotion emitted only `delivery_confirmed`,
which nothing reads; a task whose send was confirmed that way sat at `done_locally` forever.)

**Two horizons, and nothing ever resends.** There is no job status endpoint and no
timeout that turns a queued row into `failed`: a `failed` row is re-approvable, and re-approving
a click that DID land is a double post into a client conversation. A queued row that never gets
confirmed is resolved by a human, at one of two moments:

1. **The lease — 15 min from `send_attempted_at`.** Inside it `mark_delivery_failed` refuses
   (the leaf may still click: 10 min queue + the click itself) and the dashboard hides "Not in
   Slack"; `mark_delivery_sent` ("It's in Slack") is permitted throughout, because recording a
   send that visibly happened is always safe. After it, both verbs work.
2. **`ReconcileUnconfirmed`** — after three ROTATION passes that read the conversation without
   seeing the message (targeted watch passes never count), the row is flagged
   `unconfirmed after 3 export passes …` with a `delivery_unconfirmed` event and moves nowhere.
   Look in Slack, then `mark_delivery_sent` or `mark_delivery_failed`. The job id on the row is
   what to grep the mini's log for (`Queued send expired …`, `Waiting send lost …`).

**Do not replace `connector-slackweb-watch` while a send is queued.** The send waits behind the
watcher's rotation export; replacing that pod drops the export's HTTP connection, the leaf kills its own
process (abandoned-export remedy) and every waiting send is lost — legibly, never replayed. Check
`/status`'s `send_queue.waiting` (or `SELECT count(*) FROM deliveries WHERE status='sending' AND
send_queued_at IS NOT NULL AND send_settled_at IS NULL`) is 0 before a roll; the dashboard pod holds
nothing and is safe.

**A bridge restart loses the queue.** It is in memory on purpose — a replayed job cannot know
whether the click landed before the crash — so on SIGTERM, a stale-job kill or an abandoned-export
kill the leaf logs every waiting send at `error` with its job id and does not run it. The row is
then simply an unconfirmed `sending` row and resolves through the two horizons above. Nothing
replays it, nothing resends it.

## Tests

Normal tests require neither Slack nor a browser:

```bash
cd /home/salvo/projects/personal/slackconnector && npm run check
cd /home/salvo/projects/personal/switchboard && go test ./... && go vet ./...
```

The local Postgres suite verifies raw-first ingestion, normalization, direction, idempotence, assisted prefill, and loop closure:

```bash
cd /home/salvo/projects/personal/switchboard
make integration
```

Authenticated smoke testing is optional and must draft without sending. Run the leaf smoke first, then `switchboard-bridge export`, then the Go poller. For a final assisted check, use a test-only approved delivery, call `prefill_delivery`, inspect and remove the draft manually, and do not call Send.
