# Slack Web connector runbook

The Slack Web connector is a raw-first Switchboard source backed by the sibling
TypeScript project — developed at `/home/salvo/projects/personal/slackconnector`
(`github.com/sspataro57/slackconnector`, private) and deployed to the Mac mini at
`~/slackconnector`. It controls only the dedicated Chromium profile; it never uses
Slack APIs, tokens, or the normal browser profile.

It is the one connector that is not self-contained in the cluster: see
[Where each part runs](#0-where-each-part-runs) before changing anything.

Inbound placement is the canonical Switchboard funnel:

```text
Slack Web -> TypeScript export -> raw_source_items -> normalized_messages(channel=slack)
          -> existing inbound-only triage filter
```

Outbound words belong only in `deliveries`. A Slack draft uses `channel='slack_reply'`; `target_ref` is the exact Slack channel or thread URL. `prefill_delivery` copies an approved body into that composer but does not send. A human sends in Slack and then calls `mark_delivery_sent`. A later connector pass matches the outbound observation and stamps `sent_external_id`/`confirmed_at`.

## 0. Where each part runs

This connector is split across two hosts, and that split is deliberate:

| Part | Host | Why |
| --- | --- | --- |
| Dedicated Chromium with the logged-in Slack session | **Mac mini** | irreducibly host-bound; needs a manually authenticated profile |
| Bridge server (`/export`, `/draft` over HTTP) | **Mac mini** | must reach that browser over loopback CDP |
| `cmd/connectors/slackweb` poller | **cluster** (`ops` namespace) | Switchboard belongs in the cluster |

The poller cannot exec the bridge: `NewCommandBridge` stats the script on its own
host and the browser is on the mini, so it calls the bridge server over HTTP
instead. `SLACK_WEB_BRIDGE_URL` selects that transport; without it the local
command bridge still runs, which is what a same-machine development setup uses.

Do **not** point the connector at the mini's CDP port across the LAN. It rejects
non-loopback CDP hosts by design, and CDP is total control of that browser
profile.

## 1. Build the leaf connector

On the mini (`salvadorspataro@192.168.50.130`), where the checkout lives at
`~/slackconnector`:

```bash
cd ~/slackconnector
npm install
npm run check
```

Two executables matter:

```text
~/slackconnector/dist/cli/bridge-server.js       # HTTP bridge, what the cluster calls
~/slackconnector/dist/cli/switchboard-bridge.js  # one-shot CLI, same-machine runs only
```

## 2. Browser and bridge are launchd agents

Both run as `KeepAlive` LaunchAgents on the mini; plists are checked into
`slackconnector/deploy/launchd/`. Nothing needs starting by hand:

```bash
launchctl print gui/501/com.salvadorspataro.slack-agent-browser | grep state
launchctl print gui/501/com.salvadorspataro.slack-bridge-server  | grep state
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/healthz   # expect 200
```

**Chrome ≥136 refuses `--remote-debugging-port` on the default user-data-dir**, so
the profile must be a dedicated one. The failure is silent: Chrome looks healthy
and the port simply never opens.

Log in manually in that browser. Never automate passwords, SSO, MFA, CAPTCHA, or
security keys. Verify the session:

```bash
cd ~/slackconnector && npm run verify-session
```

**After deploying new connector code, restart the bridge** — it is long-running
and otherwise keeps executing the old `dist`:

```bash
launchctl kickstart -k gui/501/com.salvadorspataro.slack-bridge-server
```

## 3. Configure workspace identity and access

Open your profile in each Slack workspace and use **More -> Copy member ID**. The current visible workspaces are:

| Workspace | Workspace ID |
| --- | --- |
| Avviato | `T0360B84U` |
| Collaboratory/LlamaSite | `T0HPR78RX` |

Both are already configured in the bridge server's plist on the mini:

```text
T0360B84U -> USAMSAM0D          (Avviato)
T0HPR78RX -> U01UQLD7WMB        (Collaboratory/LlamaSite)
```

The Collaboratory one is not derivable from message content — there are no
messages of his in that workspace. It came from the `member_profile_pane` after
user-button -> Profile. Getting a member ID wrong makes his own messages look
inbound and re-triages them, so read it, do not guess it.

The connector env lives in
`slackconnector/deploy/launchd/com.salvadorspataro.slack-bridge-server.plist`:

```text
SLACK_CONNECTOR_CDP_URL=http://127.0.0.1:9222
SLACK_CONNECTOR_ALLOWED_WORKSPACES=Avviato,Collaboratory/LlamaSite
SLACK_CONNECTOR_ALLOWED_CHANNELS=            # empty: all visible non-DM channels
SLACK_CONNECTOR_ALLOW_DMS=true               # DMs are most of the real traffic
SLACK_CONNECTOR_ENABLE_WRITES=false          # ingestion path stays read-only
SLACK_CONNECTOR_OWN_USER_IDS={...}
SLACK_CONNECTOR_BRIDGE_TOKEN_FILE=~/.config/slack-web-connector/bridge-token
SLACK_CONNECTOR_BRIDGE_HOST=0.0.0.0          # reachable from the cluster
```

The bridge refuses to start without a token of at least 32 characters. Because it
binds beyond loopback, that token is the only thing between the LAN and Slack
content — scope it with a firewall rule if the LAN is not trusted.

## 4. Ingest

The poller runs as a cluster CronJob, not by hand:

```bash
kubectl -n ops get cronjob connector-slackweb          # 0 */2 * * *
kubectl -n ops create job slackweb-now --from=cronjob/connector-slackweb
kubectl -n ops logs job/slackweb-now
```

Its manifest is `slackconnector/deploy/k8s/connector-slackweb-cronjob.yaml`. It
needs `DATABASE_URL` (secret `switchboard-db`), `SLACK_WEB_BRIDGE_URL`
(`http://192.168.50.130:8787`), and `SLACK_WEB_BRIDGE_TOKEN` (secret
`switchboard-slack-bridge`).

**A full export is ~9 minutes of real browser work.** The bridge refuses a second
concurrent export with a 500, so overlapping runs fail rather than queue — that is
why the schedule is every two hours rather than every 30 minutes. Do not shorten
it without measuring.

The poller prints separate ingestion and normalization statistics. Browser export must finish before normalization begins; any failure leaves captured observations in `raw_source_items` for retry. To replay pending raw rows without opening Slack:

```bash
kubectl -n ops create job slackweb-renorm --from=cronjob/connector-slackweb
# then override the command with --normalize-only, or run the binary locally
# against DATABASE_URL; no browser or bridge is involved.
```

Use `--all` with `--normalize-only` for an intentional full normalization replay.

Expect a small `skipped_message_count`: the export drops any message lacking a
stable id, timestamp, and author id rather than guess its direction. Currently
about 3 of ~1550 per run — the leading messages of a group whose header sits above
the loaded history. Do **not** close that gap by inheriting the author from the
message below; a new group head means a new author, so that would file his own
messages as inbound.

A conversation or thread that enumerates but will not read is skipped and logged
at `warn` by the bridge rather than failing the whole export. Grep the bridge log
for `unreadable` to see what a given run excluded — a partial export otherwise
looks identical to a complete one.

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
- The HTTP bridge authorizes before revealing whether a route exists, exposes no
  send route at any setting, and never logs request bodies — they carry Slack
  content and draft text. `/healthz` is unauthenticated and says nothing about Slack.
- The same-machine command bridge launches `node` directly with an absolute script
  path and no shell.
- A Claude session on Salvador's workstation can read *and post* to Slack through
  the user-scope `slack-web` MCP server, which bypasses this runbook's approval
  flow entirely: it talks to `dist/index.js` on the mini with
  `SLACK_CONNECTOR_UNATTENDED_SEND=true`. Switchboard's `deliveries` gate governs
  Switchboard's own outbound, not that path.
- Do not put message bodies in shell history for real client replies; prefer the dashboard or a protected JSON input workflow when available.
- If browser export fails, inspect the TypeScript connector diagnostics directory. It contains sanitized visible-page artifacts but can still include client content.
- If prefill finds an existing composer draft, it refuses to overwrite it. Resolve the draft manually and retry.
- If a Slack selector changes, update `slackconnector/src/slack/selectors.ts` and its fixture/parser tests; Activity/search and virtualized message rows are the likeliest maintenance points.

## Tests

Normal tests require neither Slack nor a browser:

```bash
ssh salvadorspataro@192.168.50.130 'cd ~/slackconnector && npm run check'
cd /home/salvo/projects/personal/switchboard && go test ./... && go vet ./...
```

The local Postgres suite verifies raw-first ingestion, normalization, direction, idempotence, assisted prefill, and loop closure:

```bash
cd /home/salvo/projects/personal/switchboard
make integration
```

Authenticated smoke testing is optional and must draft without sending. Run the leaf smoke first, then `switchboard-bridge export`, then the Go poller. For a final assisted check, use a test-only approved delivery, call `prefill_delivery`, inspect and remove the draft manually, and do not call Send.
