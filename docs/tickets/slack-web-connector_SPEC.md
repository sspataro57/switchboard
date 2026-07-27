> Jira: SWT-13

# slack-web-connector — authenticated Slack Web ingestion and assisted replies

## Source

User request, 2026-07-24:

> Build a local Slack Web connector through an already authenticated dedicated Chromium profile, then connect it to the sibling Switchboard project so Slack messages enter the canonical funnel and replies use Switchboard's governed delivery path.

## Goal

Connect the existing TypeScript/Playwright Slack Web leaf adapter to Switchboard with a one-shot Go poller that lands observations raw-first, deterministically normalizes them into canonical messages and threads, and supports human-approved assisted Slack replies through `deliveries`. Usable alone means a configured run imports allowlisted Slack messages into the local `ops` database, triage can see inbound messages, and an approved `slack_reply` delivery can be drafted into Slack without sending it.

## Acceptance criteria

1. The TypeScript project exposes a versioned, machine-readable bridge command with two operations:
   - `export` returns only policy-allowed workspaces, conversations, channel messages, thread roots, and replies as JSON on stdout;
   - `draft` reads `{target_url,text}` from stdin, invokes the existing draft-only adapter, and returns `{drafted:true,sent:false}`.
   Logs go to stderr and neither operation reads cookies, browser storage, or Slack tokens.
2. Exported messages carry stable workspace/conversation/message identifiers, canonical URLs when exposed, author display name and member ID, timestamp, text, reply count, and a thread-root identifier. Export deduplicates channel roots repeated by thread reads. Rows without enough stable identity to determine direction are omitted and counted per conversation instead of being guessed.
3. Switchboard has `internal/connector/slackweb` plus `cmd/connectors/slackweb`. The command invokes the bridge without a shell, uses `DATABASE_URL` for the `ops` sink, supports `--normalize-only` and `--all`, records `sync_runs`, and exits nonzero with contextual errors.
4. Raw-first ingestion is observable: one synthetic `source_accounts` row per Slack workspace (`provider='slack_web'`), deterministic `conversation:{conversation_id}` and `message:{conversation_id}:{message_id}` external IDs, canonical JSON content hashes, insert/update/unchanged behavior, and `normalized_at=NULL` before normalization. Reprocessing works with the browser unavailable.
5. Normalization deterministically upserts:
   - `normalized_threads.thread_key = slack:{workspace_id}:{conversation_id}` for channel-level messages or `slack:{workspace_id}:{conversation_id}:{root_message_id}` for Slack threads;
   - `normalized_messages` with channel `slack`, stable external message ID, direction, timestamp, sender, and body.
   Raw rows are stamped normalized only after the canonical write succeeds.
6. Own-message loop closure fails closed: Switchboard export requires an explicit own Slack member ID per workspace; messages by that ID normalize as outbound and never enter triage. Messages with a different stable member ID normalize inbound; identity-ambiguous rows are skipped and counted. No display-name guessing is allowed.
7. Ingestion is idempotent: an unchanged rerun creates no duplicate raw, thread, or message rows; changed observations update raw JSON/reset `normalized_at` and update the one canonical row; virtualized duplicate observations collapse by stable message identity.
8. Migration `0009_slack_web_connector.sql` extends—not edits—the delivery channel constraint with `slack_reply`. `target_ref` is a canonical `https://app.slack.com/client/{workspace}/{conversation}[/{message}]` URL. No new task-like or outbound-message table is introduced.
9. `draft_delivery` accepts `channel='slack_reply'` only with `target_ref`. The MCP schema exposes that value. AI-attribution scrubbing still runs before the row is written.
10. A new spine-facing `prefill_delivery` executor tool is human-only. It requires an approved `slack_reply` delivery, calls the TypeScript bridge's `draft` operation, never clicks Send, leaves the delivery approved, and is audited through the normal executor pipeline. It is not MCP-listed.
11. `send_delivery` remains denied for `slack_reply` with policy rule `channel_assisted`; `mark_delivery_sent` accepts approved `slack_reply` rows after the human sends manually. The global kill switch and hourly limit apply to the mark-sent transition.
12. When a later outbound Slack observation matches an unconfirmed sent `slack_reply` delivery by exact destination plus a whitespace-normalized 120-character body prefix, normalization fills `sent_external_id`, sets `confirmed_at`, and emits `delivery_confirmed`. It never creates new triage work.
13. Unit tests cover bridge JSON validation, pure normalization, hash/idempotency decisions, URL/thread-key parsing, policy decisions, input validation, duplicate removal, and attribution scrubbing. Integration tests cover raw-first storage, normalization reruns, executor audit rows, assisted lifecycle, and loop closure without a real Slack account.
14. `npm run check`, `go test ./...`, `go vet ./...`, and the relevant local-Postgres integration tests pass. An optional authenticated smoke verifies export and draft-only behavior; it must not claim a message was sent.

## Data model changes

Forward-only migration `migrations/0009_slack_web_connector.sql`:

- replace `deliveries_channel_check` with the existing values plus `slack_reply`;
- no new tables;
- reuse `source_accounts`, `sync_runs`, `raw_source_items`, `normalized_threads`, `normalized_messages`, `deliveries`, `approvals`, `task_events`, `policy_decisions`, and `audit_events`.

Runtime conventions:

- `source_accounts.provider = 'slack_web'`;
- `source_accounts.account_email = '{workspace_id}@slack-web.local'` (synthetic, following the Upwork CRM precedent);
- `source_accounts.domain_default = canonical workspace URL`;
- `source_accounts.scopes = allowed conversation IDs observed for that workspace`;
- raw external IDs: `conversation:{conversation_id}` and `message:{conversation_id}:{message_id}`;
- normalized message channel: `slack`;
- delivery channel: `slack_reply`;
- delivery target: canonical Slack client channel or message URL.

## API / MCP tool changes

TypeScript local bridge:

- `node dist/cli/switchboard-bridge.js export`
  - stdout: `{schema_version:1, workspaces:[...]}`;
  - requires the existing CDP/security configuration plus `SLACK_CONNECTOR_OWN_USER_IDS`, a JSON object keyed by workspace ID.
- `node dist/cli/switchboard-bridge.js draft`
  - stdin: `{target_url:string,text:string}`;
  - stdout: `{drafted:true,target_url:string,text:string,sent:false}`;
  - requires `SLACK_CONNECTOR_ENABLE_WRITES=true`; never sends.

Switchboard executor:

- `draft_delivery` adds `slack_reply` to its channel enum and requires `target_ref`.
- `prefill_delivery` (spine-facing only): `{delivery_id}` → `{delivery_id,drafted:true,sent:false}`.
- `mark_delivery_sent` accepts the assisted channels `upwork_chat` and `slack_reply`.
- `send_delivery` stays denied for `slack_reply`.

Switchboard command configuration:

- `SLACK_WEB_NODE` (default `node`);
- `SLACK_WEB_BRIDGE_SCRIPT` (required outside normalize-only mode; absolute path to `dist/cli/switchboard-bridge.js`);
- all existing Slack connector environment controls, including allowlists and CDP localhost enforcement;
- `SLACK_CONNECTOR_OWN_USER_IDS` required by export.

## MQTT topics

None. The poller is a one-shot connector scheduled externally like Gmail, Jira, and Upwork CRM.

## Files likely to touch

Slack TypeScript project:

- `src/config.ts`, `src/domain.ts`;
- `src/slack/message-parser.ts`, `src/slack/selectors.ts`, `src/slack/slack-web-adapter.ts`;
- `src/cli/switchboard-bridge.ts`;
- unit/fixture tests and `README.md`.

Switchboard:

- `migrations/0009_slack_web_connector.sql`;
- `internal/connector/slackweb/{bridge.go,ingest.go,normalize.go,sink.go}` and tests;
- `cmd/connectors/slackweb/main.go`;
- `internal/tools/delivery.go`, `internal/tools/createtask.go` and tests;
- `internal/policy/matrix.go` and tests;
- `internal/mcpserver/schemas.go` and adapter tests;
- command wiring for the bridge where `prefill_delivery` is invoked;
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` and both project READMEs.

## In scope

- Allowlisted public/private channels and threads visible in the dedicated Slack browser profile.
- Raw-first polling and deterministic normalization.
- Explicit own-user-ID direction mapping and outbound loop closure.
- Switchboard delivery drafting, human approval, Slack composer prefill, manual send confirmation.

## Out of scope

- Slack login, MFA, SSO, CAPTCHA, or security-key automation.
- Official Slack API, bot token, custom Slack app, cookie/token extraction.
- Automatic Slack send from Switchboard; the initial tier is assisted.
- DMs unless the existing Slack connector DM policy is explicitly enabled.
- Automatic Slack-person to existing-person merges or automatic project routing.
- New triage behavior; triage remains shadow mode.
- Deploying the browser profile into Kubernetes.

## Invariants that apply

1. **Raw-first:** the Go poller completes raw writes before calling normalize; normalized rows always reference raw rows.
2. **One funnel:** Slack uses canonical threads/messages and the existing `tasks` funnel; no parallel inbox table.
3. **Executor path:** `prefill_delivery` and every delivery mutation run validate → policy → audit start → handler → audit complete. Ingestion remains a trusted connector with `sync_runs` as its audit trail.
4. **Delivery rows:** no Slack composer mutation occurs without an approved `deliveries` row; no Switchboard path clicks Send.
5. **Own-message loop closure:** explicit member-ID direction detection plus destination/body matching confirms our messages and keeps them out of triage.
6. **Stealth attribution:** bodies are scrubbed on delivery creation and again at the leaf adapter boundary.
7. **Orchestrator purity:** no Slack imports or browser calls enter `internal/orchestrator`; delivery lifecycle continues from existing events.

## Sibling patterns to copy

- Raw/hash/cursor lifecycle: `internal/connector/upworkcrm/ingest.go` and `sink.go`.
- Thread/message normalization and loop closure: `internal/connector/jira/normalize.go` and `sink.go`.
- Delivery adapter seam and pre-network gating: `internal/tools/delivery.go`.
- Pure policy decisions: `internal/policy/matrix.go`.
- One-shot connector command: `cmd/connectors/jira/main.go`.

## Verification protocol

1. Slack project: `npm run check && npm audit --omit=dev`.
2. Switchboard: `go vet ./... && go test ./...`.
3. Local Postgres: `make db-up && make migrate && make integration` with the Slack integration suite included in the existing cleanup pact.
4. Database fixture smoke: run the Go connector against a deterministic export source, inspect raw/normalized counts, rerun unchanged, and confirm no duplicates.
5. Optional authenticated smoke: start the dedicated Chromium profile, set workspace/channel allowlists and own-user IDs, run `switchboard-bridge export`, run `cmd/connectors/slackweb`, then prepare an approved fixture delivery and call `prefill_delivery`. Inspect the Slack composer and remove the draft manually. Never click Send during the smoke.

## Decisions made unilaterally

- Initial Switchboard Slack delivery tier is assisted, matching the recommendation accepted by “go ahead”: Switchboard may prefill only; a human sends and marks sent.
- The bridge is a subprocess JSON protocol, not HTTP. This keeps CDP local, avoids a new listening port, and lets the Go spine own database writes.
- Own-user IDs are explicit configuration rather than inferred display names. Missing identity configuration blocks export.
- No open questions remain for the initial slice.

## Delivery verification — 2026-07-24

- `npm run check`: 13 files / 41 tests passed; lint, TypeScript 7 typecheck, and build passed.
- `go test ./...` and `go vet ./...`: passed.
- `make integration`: migrations 0001–0009 and every serialized integration package passed, including Slack raw-first ingestion, assisted delivery, and loop closure.
- Authenticated browser: both configured workspaces exported five recent `general` messages with zero identity skips; Avviato also completed a Go raw-first ingest/normalize smoke.
- Assisted smoke: an approved Switchboard `slack_reply` populated the Avviato `random` composer with the exact body and returned `sent:false`; the delivery remained approved with no sent timestamp/external ID. The exact smoke draft and test database rows were removed afterward. No message was sent.
- Required Opus adversarial review was attempted with the repository-prescribed command but the Claude CLI refused it because the account had reached its monthly spend limit. A local invariant/security review was completed; independent cross-model review remains the one deferred verification item.
