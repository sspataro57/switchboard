> Jira: PENDING-SYNC

> Follow-on: `gmail-calendar-local-bridge_SPEC.md` adds read-only Calendar observations without changing this Gmail milestone's original acceptance record.

# Gmail local connector bridge

## Source

User request: build the local multi-account Gmail MCP connector in Go and make it work as Switchboard's email connector, following the sibling Slack connector's narrow subprocess bridge pattern.

## Goal

Allow Switchboard to ingest exact Gmail API observations and execute its already-approved Gmail deliveries through the sibling local `gmailconnector` binary, without storing a second copy of Google OAuth tokens in Postgres. Usable alone means `cmd/connectors/google` can run Gmail ingestion through the local bridge and existing normalization, while `opsctl`/dashboard can use the same bridge behind the existing `send_delivery` executor path.

## Acceptance criteria

1. A command bridge accepts only an absolute regular-file binary path, invokes it directly without a shell, bounds stdin/stdout/stderr, validates `schema_version`, and supports `accounts`, per-account `export`, and `send-raw` operations.

   **Amended after adversarial review (2026-07-26) — send failure classification.** Failures that provably happened before the leaf could reach Gmail (bad input, marshal failure, oversized request, a binary that never started, or the leaf answering `sent:false`) are `SendRejectedError`, so `send_delivery` releases the reservation and the delivery can be re-approved. Everything else — the process ran and then timed out, died, or answered unintelligibly — stays untyped, keeping the reservation because the message may have gone out. Previously every failure was untyped, so a missing bridge binary wedged the delivery into a state needing manual SQL.

   **Amended — export truncation and per-account serialization.** The cursor advances to the newest `internalDate` returned, so a silently truncated export would skip permanently past unstored messages: an export landing exactly on the message cap is now refused, and the leaf's reported `max_internal_date_ms` is cross-checked against what was actually stored. Each account's whole Gmail+Calendar pass holds a Postgres advisory lock (`0x51570007`, account id), and cursor writes are field-scoped via `jsonb_set` so Gmail and Calendar can never clobber each other inside the one `sync_cursor` blob.
2. Bridge-mode ingestion discovers the connector's configured account emails, ensures canonical `provider='google'` `source_accounts` rows without copying OAuth tokens, exports from the existing Gmail cursor/backfill window, writes every exact Gmail message to `raw_source_items` before normalization, and advances the cursor only after a complete successful export.
3. Re-ingesting unchanged bridge observations is idempotent through the existing content-hash path. Existing Gmail `Normalize` handles bridge-captured raw JSON without a parallel canonical model.
4. `cmd/connectors/google` selects bridge mode when `GMAIL_CONNECTOR_BRIDGE` is set. Existing direct Google OAuth Gmail+Calendar behavior remains the fallback and bridge mode does not pretend to provide Calendar.
5. The bridge send adapter implements the existing `tools.GmailSender` seam. It is wired only into trusted `opsctl` and dashboard processes when configured, so all Switchboard sends still originate from approved/idempotent `deliveries` through `send_delivery`.
6. Unit tests cover bridge protocol validation, account discovery/export ingestion, raw-first ordering, cursor overlap/backfill calculation, idempotence, and raw-send request/response mapping without live Google or a real subprocess.
7. Setup/runbook documentation identifies the sibling binary, environment variables, direct-vs-bridge mode, OAuth token ownership, and a no-send smoke sequence.

## Data model changes

None. Reuse `source_accounts`, `sync_runs`, `raw_source_items`, `normalized_threads`, `normalized_messages`, and `deliveries`. `refresh_token_encrypted` is already nullable, so bridge-owned OAuth accounts need no migration.

## API / MCP tool changes

No new Switchboard MCP tools. Existing `draft_delivery` and spine-facing `send_delivery` remain the only outbound path. The local subprocess JSON contract is version 1:

- `accounts` -> `{schema_version, accounts:[{alias,email}]}`
- `export` <- `{account,after_unix,max_messages}`; -> `{schema_version,account,messages,max_internal_date_ms}`
- `send-raw` <- `{from_email,raw,thread_id}`; -> `{schema_version,message_id,thread_id,sent}`

## MQTT topics

None.

## Files likely to touch

- `internal/connector/google/bridge.go`
- `internal/connector/google/bridge_test.go`
- `internal/connector/google/bridge_ingest.go`
- `internal/connector/google/bridge_ingest_test.go`
- `internal/connector/google/sink.go`
- `internal/connector/google/send.go`
- `cmd/connectors/google/main.go`
- `cmd/opsctl/main.go`
- `cmd/dashboard/main.go`
- `docs/runbooks/gmail-local-connector.md`

## In scope

- Local subprocess ingestion and send adapters.
- Existing Gmail raw/normalize/delivery integration.
- Direct Google connector fallback.

## Out of scope

- Calendar access through the local Gmail bridge.
- New database tables, MCP tools, delivery channels, policy rules, or automatic approval.
- Changes to Gmail normalization semantics or triage behavior.
- Moving already-encrypted database OAuth tokens automatically.

## Invariants that apply

1. **Raw-first:** bridge exports are written with the existing `InsertRaw`/`UpdateRaw` path before `Normalize` is called.
2. **One funnel:** bridge messages normalize into the existing Gmail message/thread rows; no alternate task or message store.
3. **Everything through the executor:** no new agent-facing Switchboard tool; outbound bridge invocation is reachable through the existing executor-registered `send_delivery` handler only.
4. **Nothing external without a delivery row:** `BridgeSender` is wired only as the existing `tools.GmailSender` dependency. The handler's pre-network `sending` and `sent_external_id` reservation remain unchanged.
5. **Own-message loop closure:** bridge exports exact Gmail API JSON, preserving RFC Message-ID matching in existing normalization.
6. **Stealth attribution:** the existing `send_delivery` MIME builder and attribution scrubber run before the bridge receives raw MIME.
7. **Orchestrator purity:** no orchestrator changes or provider imports.

## Sibling patterns to copy

- `internal/connector/slackweb/bridge.go` for bounded shell-free subprocess execution.
- `internal/connector/slackweb/ingest.go` for versioned exports and raw-first observation writes.
- `internal/connector/google/ingest.go` for Gmail cursor/backfill/overlap and content hashing.
- `internal/connector/google/send.go` plus `internal/tools/delivery.go` for the only outbound seam.

## Verification protocol

1. `go test ./...`
2. `go vet ./...`
3. Build `/home/salvo/projects/personal/gmailconnector/bin/gmail-switchboard`.
4. Run `gmail-switchboard accounts`, then an `export` request with a narrow recent timestamp; inspect that output contains schema version 1 and exact Gmail message JSON.
5. Run `cmd/connectors/google` against a local migrated database with `GMAIL_CONNECTOR_BRIDGE` set and verify `raw_source_items` precede populated `normalized_at`/`normalized_messages`.
6. Do not invoke `send-raw` in the smoke check. Delivery sending requires a separately approved test delivery.

No open questions arose; the existing Slack bridge and Gmail delivery seams determine the integration shape.
