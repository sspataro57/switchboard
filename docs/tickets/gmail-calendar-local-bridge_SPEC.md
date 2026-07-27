> Jira: SWT-15

# Calendar observations through the local Google connector bridge

## Source

User request: extend the local multi-account Gmail connector's OAuth approval to Google Calendar so meeting requests can be scheduled, and make the same connector serve as Switchboard's email/Calendar connector.

## Goal

Allow Switchboard bridge mode to ingest exact primary-calendar event observations for every connector-owned account through the same local OAuth tokens, while preserving the existing Google raw-first normalization and sync-token recipe. Usable alone means `cmd/connectors/google` with `GMAIL_CONNECTOR_BRIDGE` set ingests both Gmail and Calendar without requiring encrypted Google refresh tokens in Postgres.

## Acceptance criteria

1. The versioned, shell-free, bounded command bridge adds `calendar-export`, binding one account alias and a finite event limit. Initial/full requests carry a bounded time window; incremental requests carry the saved Calendar sync token.
2. `calendar-export` drains all Calendar API pages and returns exact event JSON plus the terminal `next_sync_token`. An expired sync token is handled by re-windowing once and explicitly reports that reset; no partial successful export is returned. **The reported reset is acted on, not merely logged:** observations absent from the replacement snapshot are superseded and their normalized events cancelled, in one transaction, BEFORE the new token is saved — a failed replacement leaves the old cursor so the next pass retries.
3. For each discovered account, bridge-mode ingestion runs Calendar after Gmail, writes every exact event to `raw_source_items` as `calendar:<event-id>`, and advances `calendar_sync_token` only after the complete export is durably stored.
4. A raw persistence or bridge failure marks the Calendar sync run failed and leaves the prior Calendar cursor unchanged. Gmail and Calendar cursor fields preserve each other on every save.
5. Re-ingesting unchanged Calendar observations is idempotent through the existing content-hash path. Existing Calendar normalization consumes bridge-captured raw JSON; no second canonical event model is introduced.
6. Direct database-token Gmail+Calendar mode remains unchanged **except for one deliberate, recorded fix**: cancelled-event tombstones (`{"id":...,"status":"cancelled"}` with no start/end, which incremental sync returns in both modes) previously made `Normalize` hard-error and abort the whole phase. They now normalize to a `cancelled` row with a NULL interval. Availability already filters NULL spans, so free/busy is unaffected. This is a latent-bug fix that direct mode inherits. The local MCP may create events under its exact local approval gate, but this Switchboard bridge extension is observation-only and exposes no new agent-facing Switchboard tool or outbound adapter.
7. Unit tests cover protocol validation, exact raw preservation, window/token requests, sync-token reset, raw-first/cursor ordering, partial-failure cursor safety, idempotence, and existing normalization. The PostgreSQL bridge integration test covers Calendar raw/cursor/idempotence alongside token/policy preservation.
8. The runbook identifies the combined Gmail+Calendar mode, reauthorization requirement for expanded scopes, and a no-write Calendar smoke check.

## Data model changes

**Amended after adversarial review (2026-07-26):** migration
`0010_calendar_reset.sql` adds `raw_source_items.superseded_at TIMESTAMPTZ`
plus a partial index. A sync-token reset is a REPLACEMENT — an event deleted
while the token was expired is simply absent from the snapshot, so upserting
only what came back and saving the fresh token stranded it as permanently
active with no delta able to repair it. Raw-first forbids deleting the
observation, so absence is recorded: the raw row is stamped superseded and
every normalize pass skips it, `--all` included.

Otherwise reuse `source_accounts.sync_cursor.calendar_sync_token`, `sync_runs`, `raw_source_items`, and `normalized_events`. Connector-owned OAuth refresh tokens remain local and are not copied into `source_accounts.refresh_token_encrypted`.

## API / MCP tool changes

No new Switchboard MCP tool. The local subprocess JSON contract remains schema version 1 and adds:

- `calendar-export` request: `{account,sync_token,time_min,time_max,max_events}`; the bounded window is always present as the HTTP 410 fallback
- `calendar-export` response: `{schema_version,account,events,next_sync_token,reset}`

The leaf sends exactly one Calendar API query mode: `syncToken` for an increment, or `timeMin`/`timeMax` for an initial/reset window. The subprocess request retains the bounded window alongside an incremental token solely as the HTTP 410 fallback. On HTTP 410 the local leaf retries that window without the token and returns `reset:true`.

Local `calendar_prepare_event` / `calendar_create_event` are part of the sibling MCP connector, not Switchboard tools. Calendar invites through Switchboard remain future delivery-channel work and must eventually use `deliveries(channel='calendar')` plus the executor and policy matrix.

## MQTT topics

None.

## Files likely to touch

- `internal/connector/google/bridge.go`
- `internal/connector/google/bridge_test.go`
- `internal/connector/google/bridge_ingest.go`
- `internal/connector/google/bridge_ingest_test.go`
- `internal/connector/google/bridge_pg_integration_test.go`
- `cmd/connectors/google/main.go`
- `docs/runbooks/gmail-local-connector.md`
- sibling `gmailconnector/internal/calendarapi/client.go`
- sibling `gmailconnector/internal/switchboard/bridge.go`
- sibling `gmailconnector/cmd/gmail-switchboard/main.go`

## In scope

- Read-only Calendar observation export through the existing local subprocess.
- Existing Google Calendar sync-token/window/reset semantics.
- Existing raw, cursor, and normalization stores.
- Setup and reauthorization documentation.

## Out of scope

- A Switchboard Calendar outbound adapter, Calendar delivery handler, policy change, or agent-facing booking tool.
- Secondary/shared calendar selection; each alias maps to its primary calendar.
- A second OAuth client or copying local tokens into Postgres.
- Schema migrations or new canonical tables.

## Invariants that apply

1. **Raw-first:** every exact bridge event is inserted or hash-updated before Calendar normalization; cursor movement is the complete-export commit marker.
2. **One funnel:** bridge events feed the existing `normalized_events` path and canonical task funnel.
3. **Everything through the executor:** no new Switchboard tool or handler is added.
4. **Nothing external without a delivery row:** the Switchboard bridge is Calendar-read-only; local MCP event creation is outside the Switchboard spine and separately exact-approved.
5. **Own-message loop closure:** event identity/status/attendees remain exact in raw JSON for the existing normalizer and future delivery matching.
6. **Stealth attribution:** no outbound Switchboard Calendar action is added.
7. **Orchestrator purity:** no orchestrator package changes or provider imports.

## Sibling patterns to copy

- `internal/connector/google/calendar.go` and `IngestCalendar` in `internal/connector/google/ingest.go` for paging, initial windows, sync-token increments, and HTTP 410 re-windowing.
- `internal/connector/google/bridge_ingest.go` for raw-first bridge account phases.
- `internal/connector/slackweb/bridge.go` for bounded shell-free subprocess invocation.
- Existing `NormalizeCalendarEvent` and `Normalize` for canonical event mapping.

## Verification protocol

1. In the sibling connector, run `go test -race ./...`, `go vet ./...`, and build `bin/gmail-switchboard`.
2. In Switchboard, run `go test -count=1 ./...`, `go vet ./...`, and the tagged PostgreSQL Google bridge integration test.
3. Authenticate one alias with the expanded scopes, run `gmail-switchboard calendar-export` over a narrow window, and verify schema version 1, exact event JSON, and a terminal sync token. Do not create an event in this smoke check.
4. Run `cmd/connectors/google` in bridge mode and verify `calendar:*` raw rows normalize through existing `normalized_events`, then rerun and verify idempotence.

No open questions arose. The existing direct Calendar sync contract fixes the window/token/reset behavior, and the policy invariants require this bridge increment to remain observation-only.
