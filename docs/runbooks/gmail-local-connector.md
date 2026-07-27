# Gmail + Calendar local connector runbook

The local Google connector is the sibling Go project at `/home/salvo/projects/personal/gmailconnector`. It owns the Google Desktop OAuth client configuration and the five account refresh tokens. Switchboard calls its narrow one-shot bridge for Gmail and Calendar ingestion plus approved Gmail sends; it never reads those token files.

## Data flow

```text
Gmail API
   -> gmail-switchboard export (exact full-format Gmail JSON)
   -> raw_source_items
   -> existing Gmail Normalize
   -> normalized_threads / normalized_messages
   -> existing inbound-only triage

approved delivery
   -> send_delivery reserves sent_external_id + builds MIME
   -> gmail-switchboard send-raw
   -> Gmail API
   -> next export confirms loop closure by RFC Message-ID

Calendar API
   -> gmail-switchboard calendar-export (exact Calendar event JSON)
   -> raw_source_items
   -> existing Calendar Normalize
   -> normalized_events

Gmail Watch -> Cloud Pub/Sub pull subscription        [NOT SHIPPED - see §6]
   -> local gmail-watch StreamingPull
   -> cmd/connectors/google --account <notified-email>
   -> the same raw-first Gmail + Calendar path above

10-minute local reconciliation
   -> every account through the same command
   -> recovers dropped Gmail notices and advances Calendar sync tokens
```

Bridge mode and the legacy direct mode both ingest Gmail plus each account's primary Calendar. Bridge mode keeps OAuth tokens in the sibling connector; direct mode uses Switchboard's encrypted database tokens when `GMAIL_CONNECTOR_BRIDGE` is unset.

## 1. Build and authenticate the leaf connector

```bash
cd /home/salvo/projects/personal/gmailconnector
make check
./bin/gmailctl list
./bin/gmailctl doctor
```

Follow the sibling `README.md` for enabling both APIs, Google Auth Platform consent configuration, and the one-time browser login for each account. The bridge refuses to list a configured account without a local refresh token.

The local OAuth grants must include `calendar.events` and `calendar.events.freebusy`. Tokens created before Calendar support do not silently gain those grants: rerun the same `gmailctl auth <alias> <email>` command for every alias, then confirm both Gmail identity and Calendar access with `gmailctl doctor`.

For event-driven incoming mail, also follow the sibling README's Pub/Sub setup. The pull subscriber uses local Application Default Credentials; the per-mailbox OAuth tokens continue to be used only for Gmail and Calendar API calls. The `sspataro.com` organization enforces Domain Restricted Sharing, so the Gmail system publisher needs Google's documented narrow organization-policy exception before topic IAM can be completed.

## 2. Configure Switchboard

```bash
export GMAIL_CONNECTOR_CONFIG=/home/salvo/.config/gmail-connector/config.json
export GMAIL_CONNECTOR_BRIDGE=/home/salvo/projects/personal/gmailconnector/bin/gmail-switchboard
```

The bridge path must be absolute and name a regular file. Switchboard invokes it directly with no shell. Stdout is versioned JSON; stderr is bounded diagnostic text.

For delivery sending, the leaf config must have:

```json
{
  "enable_writes": true,
  "send_mode": "approval"
}
```

The leaf's MCP approval mode does not add a second approval to Switchboard. `send-raw` is a trusted subprocess operation reached only through Switchboard's existing approved `deliveries`/`send_delivery` path. Do not expose `gmail-switchboard` itself as an MCP server or general agent tool.

## 3. Inspect accounts and narrow no-write exports

```bash
"$GMAIL_CONNECTOR_BRIDGE" accounts
```

```bash
printf '%s\n' '{"account":"work-main","after_unix":1784937600,"max_messages":25}' \
  | "$GMAIL_CONNECTOR_BRIDGE" export > /tmp/gmail-bridge-export.json
```

Verify `schema_version` is `1`, the account alias/email are correct, and `messages` contain exact Gmail objects. The bridge fails instead of truncating beyond `max_messages`.

Use a bounded Calendar window for the initial no-write check:

```bash
printf '%s\n' '{"account":"work-main","time_min":"2026-07-01T00:00:00Z","time_max":"2026-10-01T00:00:00Z","max_events":100}' \
  | "$GMAIL_CONNECTOR_BRIDGE" calendar-export > /tmp/calendar-bridge-export.json
```

Verify `events` contain exact Calendar API objects and `next_sync_token` is non-empty. `calendar-export` is read-only. Switchboard stores the terminal token and supplies it on later calls; if Google expires it, the leaf re-reads the bounded fallback window and returns `reset:true`.

## 4. Ingest and normalize

```bash
cd /home/salvo/projects/personal/switchboard
make db-up
make migrate
go run ./cmd/connectors/google --backfill 168h
```

In bridge mode, the command:

1. discovers each local alias/email;
2. ensures a token-free `provider='google'` source account row (existing rows and encrypted tokens are preserved);
3. computes the normal cursor overlap/backfill window;
4. captures every exact Gmail message in `raw_source_items` and commits the Gmail cursor only after a complete export;
5. exports exact primary-Calendar events using the stored sync token or initial 30-day-past/90-day-future window;
6. captures every event as `calendar:<event-id>` and commits `calendar_sync_token` only after the complete export is stored;
7. runs the existing Gmail and Calendar normalization plus Gmail loop-closure logic.

Use either an alias or email to limit a diagnostic run:

```bash
go run ./cmd/connectors/google --account work-main --backfill 24h
```

Replay captured raw rows without invoking the leaf:

```bash
go run ./cmd/connectors/google --normalize-only
```

New bridge-discovered accounts are deliberately `send_enabled=false`. After
verifying the exact account rows, enable only the mailboxes Switchboard may use:

```sql
SELECT id, account_email, send_enabled
FROM source_accounts
WHERE provider = 'google'
ORDER BY account_email;

UPDATE source_accounts
SET send_enabled = true
WHERE provider = 'google'
  AND account_email IN ('you@company.example', 'sales@company.example');
```

Leave personal or receive-only accounts disabled unless outbound Switchboard
delivery from them is intentional.

## 5. Approved delivery sending

Start the dashboard with `GMAIL_CONNECTOR_BRIDGE` in its environment, or use `opsctl` with the same variable. The adapter is wired behind the existing `tools.GmailSender` seam.

The normal workflow is unchanged:

1. `draft_delivery` creates a Gmail delivery for an existing normalized thread. From is resolved from that thread, never caller-chosen.
2. A human approves the delivery through the dashboard/`approve_delivery`.
3. `send_delivery` commits `sending` plus its self-chosen RFC Message-ID before invoking the bridge.
4. The bridge posts the already-built raw MIME through the matching local account.
5. A later ingestion pass matches the RFC Message-ID and confirms loop closure.

Do not test `send-raw` directly. For a send smoke test, use an intentionally approved delivery to a controlled recipient.

**Which failures release the delivery and which hold it.** The reservation
(`sent_external_id`, committed before the network call) is what stops a resend,
so the split matters:

- **Released — re-approvable, safe to retry.** Failures that provably happened
  before the leaf could reach Gmail: the bridge not configured, an absolute-path
  or missing/non-executable binary, a request rejected or too large to send, or
  the leaf answering with an explicit, self-consistent `sent:false`. These come
  back as a definite rejection and the delivery returns to `failed` with a NULL
  external id.
- **Held — needs a human.** Anything where the leaf actually ran: a non-zero
  exit, a timeout, a killed process, an unparseable or version-skewed response,
  a `sent:false` that nonetheless names a message id, or a response omitting
  `sent` entirely. The message may have gone out, so switchboard never releases
  it. **Inspect Gmail Sent before any manual recovery.**

Operators should also watch three new fields in `sync_runs.stats`:
`calendar_resets` (a sync-token reset was applied as a replacement),
`calendar_superseded` (how many observations that reset marked gone — their
normalized events were cancelled), and `accounts_busy` (a pass skipped an
account because another run held its lock; a value that never returns to zero
means a stuck lock, not healthy contention).

## 6. Run the local incoming-mail watcher — NOT PART OF THIS TICKET

> **Status: not shipped.** The Pub/Sub watcher below is described in neither
> `gmail-local-bridge_SPEC.md` nor `gmail-calendar-local-bridge_SPEC.md`, and no
> Switchboard code implements it — the delivered connector is one-shot, driven
> externally. `gmail-watch` lives in the sibling repo and is usable on its own;
> this section documents that leaf capability, not a Switchboard feature.
> Wiring it into Switchboard needs its own ticket, and the Domain Restricted
> Sharing blocker below is unresolved.

Build the one-shot Switchboard command once:

```bash
cd /home/salvo/projects/personal/switchboard
mkdir -p bin
go build -o bin/switchboard-google ./cmd/connectors/google
```

Then start the watcher from an environment containing `DATABASE_URL`, `GMAIL_CONNECTOR_CONFIG`, and `GMAIL_CONNECTOR_BRIDGE`:

```bash
/home/salvo/projects/personal/gmailconnector/bin/gmail-watch run \
  --exec /home/salvo/projects/personal/switchboard/bin/switchboard-google \
  --reconcile 10m
```

The watcher renews Gmail mailbox watches daily. A notification runs `switchboard-google --account <email>` immediately; a ten-minute pass runs every account to recover rare dropped notifications and poll Calendar incrementally. The Pub/Sub connection is outbound-only, so no local port, public webhook, or hosted Switchboard instance is required. If the ingestion command fails, the notice is nacked and retried. Ingestion remains idempotent and raw-first.

## Security and recovery

- Google refresh tokens remain only in the sibling connector's mode-`0600` files.
- Pub/Sub notifications contain the account email and Gmail history ID only, never message content.
- The Postgres `refresh_token_encrypted` column remains nullable for bridge-created accounts.
- Gmail bodies are raw external observations and stay subject to Switchboard's untrusted-content handling.
- Calendar titles, descriptions, locations, links, and attendees are also untrusted external observations.
- No new Switchboard MCP tool or executor side door is introduced.
- The bridge output and stderr are bounded; a too-large export fails before cursor advancement.
- A Calendar sync-token reset is performed inside the leaf as one complete replacement export; partial reset results never advance Switchboard's cursor.
- `send-raw` does not rebuild or alter Switchboard MIME, preserving attribution scrubbing, threading headers, and the reserved Message-ID.
- If an account alias/email changes, update the leaf config intentionally. Export identity mismatches fail closed.

## Validation

```bash
cd /home/salvo/projects/personal/gmailconnector && make check
cd /home/salvo/projects/personal/switchboard && go vet ./... && go test ./...
```

The Switchboard unit suite uses a runner seam and fake sinks; it never launches the leaf binary or calls Google. Live verification should stop after account discovery, narrow Gmail and Calendar exports, ingestion, and normalization unless a send has been explicitly approved. Calendar event creation is a local MCP action with its own exact approval gate and is not exposed through this Switchboard observation bridge.
