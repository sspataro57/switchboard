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
