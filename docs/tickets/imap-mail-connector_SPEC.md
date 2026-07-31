> Jira: SWT-11

# imap-mail-connector — IMAP ingest + SMTP send + read-only mail tools + MCP delivery approval

Open questions ANSWERED 2026-07-26 (both option A) — see the Open questions section.
Ready for `test-author`.

## Source

Ad-hoc work, not a numbered build-order step. It **supersedes the ingest half of
build-order step 7** and **completes the send half of step 8**:

> 7. Google OAuth (one project, Desktop-app client, loopback flow, publish to
>    In Production to avoid 7-day token expiry; test users = the 5 accounts;
>    readonly scopes only). Gmail + Calendar pollers (5–15 min). Message-ID
>    dedup across accounts. Availability service (free/busy merge +
>    propose_slots — deterministic, no LLM).

> 8. Draft worker + deliveries + dashboard approve/edit/send + Gmail send
>    adapter (threading headers, From inherited from thread — never
>    model-chosen) + Upwork assisted tier.

Salvador's decision, 2026-07-26 (recorded, not relitigated): **Google OAuth is
abandoned as the mail path.** Of the mailboxes involved one is personal Gmail and
several are Workspace orgs he does not administer. An Internal OAuth app covers only
his own `sspataro.com` org; External + restricted Gmail scopes
(`gmail.readonly`/`gmail.send`) requires Google verification + a CASA assessment, and
third-party Workspace admins can block the client id regardless. He will not register
the app. IMAP with per-account app passwords was tested live and works, including
against the org he does not control:

- `salvador@handsonconnect.org` — LOGIN OK, 10,628 messages in INBOX, 16 folders
- `sspataro@gmail.com` — LOGIN OK, 106,930 messages in INBOX, 29 folders
- `developer@sspataro.com` — app password ALREADY EXISTS in the cluster
  (`upwork/upwork-api-connector-secrets` keys `IMAP_USER`/`IMAP_PASS`, and
  `upwork/upwork-crm-secrets` keys `imap-user`/`imap-pass`/`imap-host`/`imap-port`/
  `imap-folder`). **Reuse it; do not mint a new one.** The upwork CRM connector
  already reads that mailbox with a `"New job:"` subject filter — a second IMAP
  connection is fine (Gmail allows ~15 concurrent per account).

## Goal

Replace the Google-API mail path with IMAP ingest and SMTP send inside the existing
`internal/connector/google` package — same `provider='google'` accounts, same
raw-first→normalize→dedup→loop-closure machinery — and expose read-only mail search
over `normalized_messages` plus human-gated delivery approval/send to the interactive
MCP session.

**Usable alone means:** after one operator pass (`google-auth add-app-password` ×3,
`UPDATE source_accounts SET send_enabled=true` on the sending account), a single
long-running `connector-google --watch` Deployment in the `ops` namespace fills the
ops db with the last 90 days of INBOX + Sent for all three mailboxes and keeps it
live within seconds via IMAP IDLE; the interactive session can `mail_search` /
`mail_read_thread` what was ingested; and a drafted reply can be approved and sent
end-to-end over SMTP, with the Sent-folder copy closing the loop back onto the
delivery row. No Google Cloud project, no OAuth consent screen, no verification.

## Named consequences (state these, do not treat as omissions)

1. **Calendar goes dark.** IMAP cannot carry calendar data. `IngestCalendar`,
   `NormalizeCalendarEvent`, `internal/availability`, and the `propose_slots`
   executor tool all remain in the tree and keep working against whatever
   `normalized_events` rows exist, but under `MAIL_SOURCE=imap` nothing new is
   ingested, so `propose_slots` answers from stale/empty free-busy data. Availability
   and calendar deliveries stay out of service until a separate CalDAV/ICS ticket.
   `AVAIL_*` env and the tool are untouched by this ticket.
2. **`gmail.go` / `calendar.go` / `oauth.go` / `bridge*.go` stay.** They are the
   calendar path and the regression baseline. This ticket adds a third source; it
   deletes nothing.
3. **The first backfill floods shadow triage.** Three mailboxes × 90 days of inbound
   lands in `triage.PendingMessages` (inbound-only, channel-agnostic). Run
   `triage run` with `--since`/`--limit` after the first backfill or it will burn
   thousands of gpt-5-mini calls in one pass. Operational note for the runbook, not
   code in this ticket.
4. **Host-local credential files are transitional.**
   `/home/salvo/sspataro@gmail.com.imap.txt` and
   `/home/salvo/salvadorhandsonconnectmap.txt` (chmod 600) exist today; this ticket
   moves the app passwords into `source_accounts` encrypted with `OPS_TOKEN_KEY`.
   The runbook's last step is deleting the files.

## Amendments after implementation (2026-07-31, Salvador's calls)

Recorded rather than left to drift, in the SWT-16 style.

1. **Migration renumbered 0011 -> 0014.** 0011 went to slack-send-promotion and
   main is at 0013 by the time this ticket resumed. No test pinned the number.

2. **Oversize messages keep their text, and list what was dropped.** Criterion 7
   said "captured as headers-only". That threw away the BODY of every large
   message, not just its attachments — so a client mail reading "see the attached
   spec, confirm by Friday" with a 3 MB PDF reached triage as a subject and
   nothing else. The ask itself was the thing being discarded.

   IMAP fetches parts individually, so the size cap no longer has to be judged
   against the whole message: `IMAPClientSource.Fetch` pulls BODYSTRUCTURE first,
   then fetches the headers and the text parts in full (a text body is a few KB
   even in a 12 MB message) and skips only the binary parts. The dropped parts
   are recorded as a `parts` manifest in the raw envelope — part id, filename,
   content type, size — and rendered into `body_text` as a trailing
   `[Attachments not stored: ...]` line, because `normalized_messages` has no
   attachment column and a manifest nothing downstream can read is not context.

   `truncated` now means "attachments omitted", not "everything but headers".
   Attachment BYTES remain out of scope, as originally specced; this is metadata
   plus text preservation. The existing tests hold unchanged: a genuinely
   bodyless capture still normalizes to an empty body.

3. **The real IMAP client is `emersion/go-imap` v1.2.1, in-repo.** Considered
   splitting ingestion into a sibling service (as slackconnector/gmailconnector
   are) and rejected for now: `MailSource` is already the frontier, only the
   fetch is new, and the same library is proven against Gmail in this cluster by
   job-agent. If it later needs to be a service, the migration is to implement
   `MailSource` as a client of it and touch nothing else. Recorded so the
   2026-07-26 "not the sibling repo" decision is not silently re-litigated.
   Note the Slack leaf's wedging is a Playwright/browser-automation artifact, not
   evidence about service boundaries.

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass offline. The RFC822 normalizer, the
   IMAP ingest phase (against a fake `MailSource`), the thread-key derivation, the
   size-cap/truncation rule, and the SMTP error classifier are unit-tested with zero
   network and zero Postgres.
2. **Regression:** every existing test in `internal/connector/google` passes
   unmodified — `normalize_test.go`, `poller_test.go`, `fake_google_test.go`,
   `send_test.go`, `bridge_test.go`, `bridge_ingest_test.go`,
   `bridge_calendar_test.go`, `integration_test.go`, `loopclosure_integration_test.go`,
   `oauth_integration_test.go`, `bridge_pg_integration_test.go`. Plus a new unit test
   pinning source selection: `MAIL_SOURCE` unset ⇒ bridge when
   `GMAIL_CONNECTOR_BRIDGE` is set, else the direct Gmail-API path — byte-identical
   behaviour to today.
3. Migration `migrations/0011_imap_mail.sql` applies cleanly on a fresh db and on the
   current schema, and is a no-op on second apply. It adds to `source_accounts`:
   `auth_type TEXT NOT NULL DEFAULT 'oauth'` (CHECK in `('oauth','app_password')`),
   `app_password_encrypted BYTEA`, `imap_host TEXT`, `imap_port INT`,
   `smtp_host TEXT`, `smtp_port INT`. No new tables. No new extension (the `ops` role
   cannot `CREATE EXTENSION`; pgcrypto is already present).
4. `google-auth add-app-password <email> [--imap-host H] [--imap-port P]
   [--smtp-host H] [--smtp-port P] [--no-availability]` reads the password from
   **stdin** (never argv, never env — argv leaks through `ps`), verifies it by
   performing a real IMAP LOGIN + LIST against the target host, and upserts one
   `provider='google'` row with `auth_type='app_password'`,
   `app_password_encrypted = pgp_sym_encrypt($pw, $OPS_TOKEN_KEY)`,
   `send_enabled=false`, `scopes='{}'`. It never touches
   `refresh_token_encrypted`. `google-auth list` shows `auth_type` per row. The
   password appears in no log line, no error string, and no `sync_runs.stats`.
5. **Raw-first (invariant 1).** For every fetched message the connector writes
   `raw_source_items` BEFORE any parsing, with
   `external_id = imap:{folder}:{uidvalidity}:{uid}` and
   `raw_json = {"source":"imap","folder":...,"uidvalidity":N,"uid":M,
   "internaldate":"RFC3339","flags":[...],"size":N,"truncated":bool,
   "rfc822_b64":"<base64 of the bytes as received>"}`. Base64, not a JSON string:
   RFC822 is 8-bit-clean and would not survive UTF-8 validation. `content_hash` is
   the existing `chash.ContentHash` over that object. Normalization is a separate
   phase reading only `raw_json`; `--normalize-only --all` rebuilds every normalized
   row with no IMAP connection constructed.
6. **Never mark mail read.** Every fetch uses `BODY.PEEK[...]`; no `\Seen` flag is
   ever set, no message is moved, deleted, or labelled. An integration/live test
   asserts flags are unchanged after a pass. (This connector has no write verbs
   against the mailbox at all except the SMTP submission of criterion 12.)
7. **Bounded initial ingest.** First pass per folder uses IMAP
   `SEARCH SINCE <date>` with the window from `--backfill` (default
   `google.DefaultBackfill`, 2160h/90d); `--full` re-runs the window. Messages larger
   than `MAIL_MAX_MESSAGE_BYTES` (default 1 MiB) are captured as headers-only with
   `"truncated": true` in the raw envelope, and are still normalized (headers give
   Message-ID, direction, subject, threading; `body_text` is empty). A 106,930-message
   mailbox must never produce a full-mailbox fetch.
8. **Per-folder cursors, advanced only after a complete pass.**
   `source_accounts.sync_cursor` gains
   `"imap_folders": {"<folder>": {"uidvalidity": N, "uid_next": M}}` alongside the
   existing `gmail_internal_date_ms` / `calendar_sync_token` keys (both preserved on
   every save). Incremental passes issue `UID FETCH {uid_next}:*`. The cursor is
   written only after every message in the pass is durably in `raw_source_items`; any
   error leaves it unchanged, marks the `sync_runs` row `error`, and (one-shot mode)
   exits non-zero. **A UIDVALIDITY change forces a resync of that folder** — the
   stored `uid_next` is discarded and the SINCE window is re-run; the new
   `external_id`s differ (uidvalidity is in the key), so old raw rows are kept as
   history and normalize-time Message-ID dedup collapses the duplicates.
9. **Folders: INBOX + Sent only.** Sent is discovered from the RFC 6154
   `\Sent` SPECIAL-USE attribute in `LIST`, falling back to `MAIL_FOLDERS` (explicit
   comma-separated override) and then to `[Gmail]/Sent Mail`. Spam, Trash, All Mail,
   Drafts and every other folder are excluded. Sent matters because it is the
   own-message loop-closure surface (invariant 5).
10. **Normalization.** The `Normalize` dispatch in `normalize.go` gains an
    `imap:` prefix branch (the existing `gmail:` and `calendar:` branches are
    untouched — this is the raw-discriminator). `NormalizeRFC822(raw, accountEmail,
    ownEmails)` is pure and produces the same `NormalizedMessage` struct as
    `NormalizeGmailMessage`:
    - `Channel = "gmail"` (the existing vocabulary for e-mail — see Decisions),
    - `ExternalMessageID` = the RFC 5322 `Message-ID` header verbatim, brackets kept;
      fallback `imap:{folder}:{uidvalidity}:{uid}`,
    - `SentAt` = `Date` header, falling back to the IMAP INTERNALDATE,
    - `Direction` = **outbound iff the From address is in the own-email set**
      (unchanged rule, reused verbatim — so Sent-folder copies are outbound and can
      never be re-triaged),
    - `Subject`, `Sender` from headers (RFC 2047 encoded-words decoded),
    - `BodyText` = first `text/plain` leaf (quoted-printable/base64 decoded, charset
      best-effort to UTF-8, unknown charset ⇒ raw bytes rather than a hard error);
      if the message is HTML-only, a tag-stripped/entity-unescaped fallback; capped
      at 256 KiB,
    - `ThreadKey = "gmail:{account_email}:{root}"` where root = `References[0]`, else
      `In-Reply-To`, else the message's own `Message-ID`, else the `external_id`.
      The three-segment `gmail:{email}:{x}` shape is mandatory:
      `tools.splitGmailThreadKey` (delivery.go:218) parses it and `draft_delivery` /
      `send_delivery` resolve From from segment 2.
11. **Cross-account and cross-folder dedup still holds.** The INBOX copy and the Sent
    copy of the same message, and the same message in two mailboxes, yield N raw rows
    and exactly ONE `normalized_messages` row, enforced by the existing partial unique
    index `normalized_messages_gmail_msgid_idx` (migration 0005) plus the SELECT-first
    belt in `PGSink.upsertMessage`; losers are counted `dedup_skipped` and still
    stamped `normalized_at`. No change to `sink.go`'s dedup code is permitted.
12. **SMTP transport behind the existing seam.** A new
    `google.SMTPSender` implements `tools.GmailSender`
    (`Send(ctx, fromUserID, rawMIME, threadID) (string, error)`) — **no new tool, no
    new policy rule, no signature change**. It resolves the account row by
    `fromUserID`, decrypts `app_password_encrypted`, dials
    `smtp_host:smtp_port` (default `smtp.gmail.com:587`, STARTTLS; 465 = implicit TLS
    when configured), authenticates PLAIN over TLS only, parses the envelope
    recipient from the built MIME's `To` header, and submits the bytes unchanged.
    `threadID` is ignored (IMAP/SMTP has no Gmail thread id; threading is carried by
    the `In-Reply-To`/`References` headers `BuildOutboundMIME` already writes). It
    returns the reserved `<sb-…>` Message-ID (`send_delivery` discards the return
    value today — delivery.go:483).
13. **Send router.** `google.MailSender` implements `tools.GmailSender` and dispatches
    per from-account `auth_type`: `app_password` ⇒ `SMTPSender`, `oauth` ⇒ the
    existing `AccountSender`/`BridgeSender`. `cmd/dashboard`, `cmd/opsctl` and
    `cmd/ops-mcp` wire it. `send_delivery`'s guards are unchanged: `approved` only
    (delivery.go:392), `send_enabled` required, `sent_external_id` reserved and
    committed BEFORE the network call, never resent while present.
14. **Error classification preserves invariant 4.** An SMTP error carrying a protocol
    response code (`textproto.Error`, any 4xx/5xx — the server spoke and refused) is
    wrapped in the existing `google.SendRejectedError`, so `send_delivery` clears the
    reserved Message-ID and the `failed→approved` retry path stays reachable. An I/O
    error with no response code (dial failure, timeout, reset mid-DATA) stays untyped
    ⇒ the row goes `failed` with `sent_external_id` KEPT, blocking any automatic
    resend. Unit-tested against an in-process fake SMTP listener for: 550 at RCPT,
    5xx at end-of-DATA, connection dropped mid-DATA, happy path.
15. **Loop closure (invariant 5) has two matchers.** Primary: the Sent-folder copy
    re-enters ingestion, is `direction='outbound'`, and `PGSink.upsertMessage` →
    `confirmDelivery` matches `external_message_id` against
    `deliveries.sent_external_id` (existing code, sink.go:265-294 — unchanged). Belt,
    because a submission service MAY rewrite `Message-ID`: a post-hoc matcher over
    outbound gmail-channel messages with `confirmed_at IS NULL` deliveries, scoped to
    the same `from_account_id`, matching on 120-char whitespace-normalized body prefix
    — the exact idiom already shipped for `upwork_chat`. It sets `confirmed_at` and
    emits `delivery_confirmed` with the OBSERVED Message-ID in the event payload;
    it never overwrites `sent_external_id` (that is the idempotency token). An
    integration test covers both paths. **No new task is ever created from an own
    send.**
16. **Read-only mail tools, served from `normalized_messages` — never live IMAP.**
    Two tools registered in `tools.Register` (executor path: validate → policy →
    audit start → handler → audit complete) and MCP-listed in
    `mcpserver.agentTools`:
    - `mail_search {query?, from?, thread_key?, since?, until?, direction?, limit?}`
      → `{messages:[{message_id, thread_id, thread_key, subject, sender, sent_at,
      direction, snippet}], truncated}`. At least one of `query`/`from`/`thread_key`
      is required; `query` is a case-insensitive substring over subject + sender +
      body_text; `channel='gmail'` only; ordered `sent_at DESC`; `limit` default 20,
      hard max 50; `snippet` capped at 300 chars.
    - `mail_read_thread {thread_id | thread_key, limit?}` → `{thread_key, subject,
      messages:[{message_id, sender, sent_at, direction, body_text}]}`, ordered
      `sent_at ASC`, max 50 messages, each body capped at 8 KiB.
    Both are read-only (they write nothing but their audit row) and fall through the
    static allow-list in the policy matrix (not `humanOnly`, not `snapshotGated`).
    **Documented limitation, stated in the tool descriptions:** agents see only what
    has been ingested — searching beyond the backfill window is an ingestion decision,
    not an MCP one. Gmail mutations (mark read, archive, label) are OUT (see Out of
    scope).
17. **`approve_delivery` and `send_delivery` become MCP-listed** with schemas
    `{"delivery_id": integer}`. They stay in `policy.humanOnly`; the actor gate is the
    only thing standing between an autonomous worker and a send. **[OQ1 ANSWERED —
    option A, 2026-07-26.** `policy.humanActor()` strips one optional leading `mcp:`
    transport prefix before checking the `dashboard:`/`opsctl:`/`manual:` set, so
    `mcp:manual:salvo` passes and `mcp:worker:X` still fails. The audit row keeps the
    full unmodified actor, so an MCP-triggered send stays distinguishable from an
    opsctl one — that distinction is the reason A was chosen over changing the
    adapter.**]** Required regardless of the answer: a unit test proving a
    `worker:*`-identity MCP call to `approve_delivery`/`send_delivery` is DENIED with
    rule `human_only`, and an audited deny row is written. `draft_delivery` remains
    the agent verb; there is no compose-and-send tool and no path that sends without a
    separate prior approve call.
18. **Watch mode.** `connector-google --watch` runs as a long-lived process:
    per account an IMAP IDLE connection on INBOX, re-issued at most every
    `MAIL_IDLE_REFRESH` (default 25m, RFC 2177 caps at 29m); every notification
    triggers a bounded UID fetch pass for that folder (the notification is a
    wake-up, never trusted as the payload — same discipline as the orchestrator's
    NOTIFY); plus a full reconcile pass over every account × folder every
    `MAIL_RECONCILE_INTERVAL` (default 10m), which is exactly the one-shot code path.
    Connection loss ⇒ exponential backoff with jitter capped at 5m, an `error`
    `sync_runs` row, and a reconnect — the process does not exit on a transient IMAP
    failure. SIGINT/SIGTERM cancels the context and shuts down cleanly. Without
    `--watch` the binary is one-shot exactly as today (backfill and manual replay use
    this mode).
19. **Zero tasks, zero deliveries created by ingestion.** No run of the connector,
    `google-auth`, `mail_search`, or `mail_read_thread` changes `count(*)` of `tasks`
    or `deliveries`. `sync_runs` gets one row per account per pass (phase `imap`)
    with per-folder counters in `stats`.
20. `make integration` green, serialized (`-p 1`), joining the mutual-cleanup pact
    (INSTITUTIONAL_KNOWLEDGE.md, "integration suites cross-pollute"): the new suites
    scope every assertion to their own `provider='google', account_email LIKE
    'itest-imap-%'` corpus and clean up in FK order before and after.

## Data model changes

Migration `migrations/0011_imap_mail.sql` (forward-only, numbered; no new extension):

```sql
ALTER TABLE source_accounts
  ADD COLUMN auth_type              TEXT NOT NULL DEFAULT 'oauth',
  ADD COLUMN app_password_encrypted BYTEA,
  ADD COLUMN imap_host              TEXT,
  ADD COLUMN imap_port              INT,
  ADD COLUMN smtp_host              TEXT,
  ADD COLUMN smtp_port              INT;

ALTER TABLE source_accounts
  ADD CONSTRAINT source_accounts_auth_type_check
  CHECK (auth_type IN ('oauth','app_password'));
```

No new tables. Vocabulary is unchanged: `source_accounts`, `sync_runs`,
`raw_source_items`, `normalized_messages`, `normalized_threads`, `deliveries`,
`task_events`.

Runtime shapes (written by code, not by migration):

- `sync_cursor`:
  `{"gmail_internal_date_ms": N, "calendar_sync_token": "...",
    "imap_folders": {"INBOX": {"uidvalidity": 12, "uid_next": 88231},
                     "[Gmail]/Sent Mail": {"uidvalidity": 9, "uid_next": 4410}}}`
  — the existing keys are preserved on every save (the `Cursor` struct round-trips).
- `raw_source_items.external_id`: `imap:{folder}:{uidvalidity}:{uid}`.
  The prefix is the raw discriminator: `gmail:` = Gmail-API JSON, `calendar:` =
  Calendar JSON, `imap:` = RFC822 envelope. Both shapes may coexist for the same
  account; the normalizer dispatches on the prefix.
- `raw_source_items.raw_json`: the envelope object of criterion 5.
- `normalized_messages.channel`: stays `'gmail'`.
- `deliveries`: no schema change; `channel='gmail'`, `thread_id` set, `target_ref`
  NULL, `sent_external_id` = the reserved `<sb-{id}-{nanos}@{domain}>`.
- Encryption: SQL-side pgcrypto, same idiom as the refresh token —
  `pgp_sym_encrypt($1, $OPS_TOKEN_KEY)` / `pgp_sym_decrypt(...)`. The key never
  reaches the db, the password never reaches a log.

## API / MCP tool changes

Everything goes through `internal/tools.Register` → `executor.Execute`
(validate → policy check → audit start → handler → audit complete). Invariant 3 has
no exceptions here; there is no side door, and no `raw_sql`/`raw_api`/live-IMAP tool
is exposed.

**New, agent-facing (registered + MCP-listed):**

| tool | args | result | policy |
|---|---|---|---|
| `mail_search` | `{query?, from?, thread_key?, since?, until?, direction?, limit?}` | `{messages:[…], truncated}` | static allow (read-only) |
| `mail_read_thread` | `{thread_id?, thread_key?, limit?}` | `{thread_key, subject, messages:[…]}` | static allow (read-only) |

**Newly MCP-listed, already registered and already `humanOnly`:**

| tool | args | result | policy |
|---|---|---|---|
| `approve_delivery` | `{delivery_id}` | `{delivery_id, status:"approved"}` | `human_only`, then matrix |
| `send_delivery` | `{delivery_id}` | `{delivery_id, status:"sent", sent_external_id}` | `human_only` + `kill_switch` + `rate_limit` + channel tier |

**Honest caveat to record in the SPEC, the runbook, and the tool descriptions:** in an
interactive session the model calls as the human. "The human read the draft" is
convention, not enforcement. What IS enforced: (a) an autonomous worker identity is
denied by `human_only`; (b) approve and send are two separate calls, so a single
model turn cannot compose-and-send; (c) the harness permission prompt sits in front of
each MCP call. Draft-only was rejected by Salvador as useless; compose-and-send in one
call is forbidden and is not being built.

**Unchanged:** `draft_delivery` (still THE route for client-visible words),
`update_delivery`, `mark_delivery_sent`, `prefill_delivery`, `set_sending_frozen`,
`task_mark_delivered`, the whole `policy.Decide` core except the actor-prefix question,
and the `tools.GmailSender` interface signature.

**No new outbound channel, no new `deliveries.channel` value, no matrix rule.**

## MQTT topics

**None.** Fleet topics (`ops/workers/{id}/status|cmd`) are the worker contract; this
connector is not a worker and publishes nothing. Liveness of the new Deployment is
observed by (a) k8s restart semantics and (b) the reconcile heartbeat in SQL:

```sql
SELECT max(finished_at) FROM sync_runs r
  JOIN source_accounts a ON a.id = r.source_account_id
 WHERE a.provider='google' AND r.stats->>'phase' = 'imap';
```

A value older than ~2× `MAIL_RECONCILE_INTERVAL` means the connector is wedged. Put
that query in the runbook. (Adding an MQTT liveness topic for non-worker spine
services is a separate idea — Future work.)

## Files likely to touch

Existing (all verified present):

- `go.mod` / `go.sum` — add `github.com/emersion/go-imap/v2` as a direct dependency
  (see Decisions). SMTP uses stdlib `net/smtp`; MIME parsing uses stdlib
  `net/mail` + `mime` + `mime/multipart` + `mime/quotedprintable`.
- `internal/connector/google/ingest.go` — `Cursor` gains `IMAPFolders
  map[string]FolderCursor`; `Stats` gains `IMAPListed/IMAPFetched/IMAPTruncated`
  (additive, `sync_runs.stats` is JSONB); `Config` gains `MaxMessageBytes`,
  `Folders`. `Run`, `IngestGmail`, `IngestCalendar`, `upsertRaw` untouched.
- `internal/connector/google/normalize.go` — one new `case strings.HasPrefix(
  it.externalID, "imap:")` in `Normalize`; the `gmail:`/`calendar:` branches and both
  existing mappers are untouched.
- `internal/connector/google/sink.go` — `Account` and `ListAccounts` gain
  `AuthType`, `IMAPHost/Port`, `SMTPHost/Port`; new `DecryptAppPassword`; new
  post-hoc `confirmDeliveryByBodyPrefix`. `upsertMessage` / `confirmDelivery` /
  `upsertEvent` / `pendingRaw` / `ownEmailSet` are NOT modified.
- `internal/connector/google/oauth.go` — `UpsertAppPasswordAccount(ctx, pool, email,
  password, key, hosts…, calendarInAvailability)`. Existing OAuth helpers untouched.
- `internal/connector/google/send.go` — `MailSender` router (criterion 13). Existing
  `BuildOutboundMIME`, `ScrubAIAttribution`, `GmailSender`, `AccountSender`,
  `BridgeSender`, `SendRejectedError` untouched.
- `internal/tools/createtask.go` — register `mail_search`, `mail_read_thread`.
- `internal/mcpserver/schemas.go` — add four entries to `agentTools`.
- `internal/policy/matrix.go` — `humanActor()` strips one optional `mcp:` prefix (OQ1 = A).
- `cmd/connectors/google/main.go` — `MAIL_SOURCE` selection + `--watch`; the 10-minute
  `context.WithTimeout` must not apply in watch mode (signal-cancelled context there).
- `cmd/google-auth/main.go` — `add-app-password` subcommand; `list` shows `auth_type`.
- `cmd/ops-mcp/main.go` — wire `tools.SetGmailSender(google.MailSender{…})` (it wires
  no sender today, so an MCP `send_delivery` would fail with "no gmail send adapter
  wired").
- `cmd/dashboard/main.go`, `cmd/opsctl/main.go` — swap `AccountSender`/`BridgeSender`
  for the router.
- `.mcp.json` — the `ops` server env needs `OPS_TOKEN_KEY` (to decrypt the app
  password for SMTP).
- `docs/runbooks/` — new `imap-mail-connector.md` (sibling of
  `gmail-local-connector.md`, `slack-web-connector.md`).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — after the operator pass: the OAuth-abandoned
  decision, app-password storage, folder names actually observed, whether Gmail
  preserved the submitted Message-ID, the new Deployment.
- `internal/triage/integration_test.go` — `cleanupTriage` learns the
  `itest-imap-%` foreign corpus (pact obligation).

New:

- `migrations/0011_imap_mail.sql`
- `internal/connector/google/imap.go` — `MailSource` interface + go-imap client:
  dial/TLS/login, `Folders()` with SPECIAL-USE discovery, `Search(folder, since)`,
  `Fetch(folder, uids)` using `BODY.PEEK[]` / headers-only above the size cap,
  `Idle(folder, ctx)`.
- `internal/connector/google/imap_ingest.go` — `IngestIMAP(ctx, src, sink, acct, cfg)`:
  per-folder run bookkeeping, UIDVALIDITY check, SINCE/UID range selection, raw
  envelope build, `upsertRaw` reuse, cursor advance last.
- `internal/connector/google/imap_watch.go` — `Watch(ctx, …)`: IDLE goroutines +
  reconcile ticker + backoff.
- `internal/connector/google/rfc822.go` — `NormalizeRFC822` and its pure helpers
  (header decode, text/plain walk, HTML fallback, thread-root derivation).
- `internal/connector/google/smtp.go` — `SMTPSender` + error classifier.
- `internal/tools/mail.go` — `mail_search`, `mail_read_thread` handlers + validators.
- Tests: `rfc822_test.go`, `imap_ingest_test.go` (fake `MailSource`),
  `imap_watch_test.go` (fake source, fake clock/ticker), `smtp_test.go` (in-process
  fake SMTP listener), `mail_tools_test.go`,
  `imap_integration_test.go` (compose db, fake source),
  `internal/tools/mail_integration_test.go`.
- Sibling repo (NOT this repo, listed for completeness):
  `~/projects/personal/kube/switchboard/` — Deployment `connector-google-watch`,
  pinned image tag.

## In scope

- Migration 0011 and the `add-app-password` credential path.
- IMAP ingest as a third source inside `internal/connector/google`, one-shot and
  `--watch`, INBOX + Sent, bounded backfill, per-folder UID cursors, UIDVALIDITY
  resync, size-capped raw capture.
- RFC822 normalizer feeding the existing canonical tables, existing dedup, existing
  direction rule, existing thread-key shape.
- SMTP transport behind the existing `tools.GmailSender` seam + the per-account router
  + error classification + the post-hoc loop-closure matcher.
- `mail_search` / `mail_read_thread` over `normalized_messages`.
- MCP-listing `approve_delivery` + `send_delivery` (OQ1 = A, unblocked) and wiring the sender
  into `cmd/ops-mcp`.
- Runbook + INSTITUTIONAL_KNOWLEDGE update; k8s Deployment manifest in the kube repo.

## Out of scope (do not bundle)

- **Gmail mutations from agents** — mark read, archive, label, move, delete. They
  mutate an external system, they are not outbound communication, and the policy
  matrix has no row for them. Explicitly rejected for v1.
- **Live-IMAP MCP tools.** `mail_search` reads `normalized_messages`. A tool that
  opens a provider connection per model call is a second reader, breaks when Gmail is
  slow, and is a `raw_api` tool by another name (invariant 3).
- **Calendar / CalDAV / ICS ingest, availability, `propose_slots` revival, calendar
  deliveries.** Named consequence 1; a separate ticket.
- **Deleting the Gmail-API path** (`gmail.go`, `oauth.go`, `bridge*.go`,
  `cmd/google-auth add`). It is the calendar path and the regression baseline.
- **Triage going live** (build-order step 6 go-live) and any change to triage's
  filter. IMAP inbound flows into the existing shadow triage with zero code changes.
- **Full-mailbox backfill, attachment ingestion, `content_chunks`/embeddings over the
  mail corpus, full-text/pgvector search.** `mail_search` is deliberately ILIKE +
  LIMIT.
- **A new `deliveries.channel` value, a new policy rule, autonomy promotion,
  per-channel rate-limit retuning.**
- **Upwork CRM connector changes.** It keeps its own IMAP connection and its
  `"New job:"` filter; this ticket does not touch it or its secrets.
- **A dashboard mail view.** The dashboard's `/deliveries` slice is unchanged.
- **MQTT liveness for spine services.**

## Invariants that apply

- **1. Raw-first.** The write happens in
  `internal/connector/google/imap_ingest.go`, in the per-folder fetch loop: the
  base64 RFC822 envelope goes through the existing `upsertRaw` (ingest.go:260) into
  `raw_source_items` for every message in the pass BEFORE the normalize phase runs and
  before any header is parsed. Reprocessing must always be possible:
  `--normalize-only --all` must rebuild every normalized row with no IMAP connection
  constructed (criterion 5), and per-account/per-folder duplicates are all kept — dedup
  is a normalize-time decision only. The size cap is an explicit, marked truncation
  (`"truncated": true`), not silent loss.
- **2. One funnel.** No new tables and no new task-like anything. IMAP messages become
  `normalized_messages` rows in the same canonical shape as Gmail-API messages, on the
  same `channel='gmail'`, deduped into one row per Message-ID. Ingestion mints zero
  tasks (criterion 19); triage owns task creation.
- **3. Everything through the executor.** `mail_search` and `mail_read_thread` are
  registered in `tools.Register` and reached only via `executor.Execute`; they hold no
  provider connection and expose no SQL. The connector and `google-auth` register
  nothing and are not agent-reachable (trusted spine, audited via `sync_runs`) — same
  stance as every shipped connector.
- **4. Nothing external without a delivery row.** The ONLY caller of
  `SMTPSender.Send` is `MailSender`, whose only caller is the `send_delivery` handler
  (delivery.go:336-511), which requires `status='approved'`, `send_enabled=true`, and
  a NULL `sent_external_id`, and commits `sending` + the reserved Message-ID before
  the socket is opened. Kill switch and rate limit gate the `sending` transition in
  the policy stage. The error classifier (criterion 14) is the load-bearing part: only
  a server-spoken refusal clears the reservation.
- **5. Own-message loop closure.** Two obligations, both in
  `internal/connector/google`: (a) the Sent folder is ingested — without it our sends
  never re-enter; (b) the direction rule (outbound iff From ∈ own-email set) is reused
  verbatim in `NormalizeRFC822`, so every copy of our own mail is `outbound` and is
  invisible to triage's inbound-only filter (`internal/triage/store.go:34`) — never
  re-triaged into a new task. Confirmation runs in `PGSink.upsertMessage` →
  `confirmDelivery` (Message-ID match, unchanged code) with the body-prefix matcher as
  the belt if the submission service rewrites Message-ID. An integration test asserts
  `confirmed_at` set AND `count(*) FROM tasks` unchanged.
- **6. Stealth attribution.** `BuildOutboundMIME` already calls
  `ScrubAIAttribution`; the SMTP path must submit those bytes UNCHANGED — no extra
  `X-Mailer`, no `User-Agent`, no `X-Switchboard-*` header, nothing that fingerprints
  the sender as automated. Test: the fake SMTP listener asserts the received DATA
  block equals `BuildOutboundMIME`'s output byte-for-byte (modulo dot-stuffing) and
  contains no header the builder did not write.
- **7. Orchestrator purity (discipline transfer).** `NormalizeRFC822`, thread-root
  derivation, the size/truncation rule, cursor arithmetic and the SMTP error
  classifier are pure functions unit-tested with no network and no db; all I/O lives
  behind the `MailSource` interface and the `PGSink`. The orchestrator itself is not
  touched by this ticket and gains no provider import.

## Sibling patterns to copy

- **THE connector shape:** `internal/connector/google/ingest.go` — `Sink` interface,
  `StartRun`/`fail()`/`FinishRun` bookkeeping, `upsertRaw` hash-compare in the phase
  (not the sink) for unit testability, cursor advanced last. `IngestIMAP` is a
  sibling of `IngestGmail`, not a rewrite. `internal/connector/upworkcrm/` is the
  original of that shape.
- **Third-source branching:** `cmd/connectors/google/main.go:54-93` already forks
  bridge vs direct on env; `MAIL_SOURCE` extends the same `if/else`.
- **Bounded external source + validation:** `internal/connector/google/bridge_ingest.go`
  (`BridgeMaxMessages`, identity check, cursor-preserving saves) and
  `internal/connector/slackweb/ingest.go` (per-object `upsertObservation`, stats,
  `fail()` closure).
- **Post-hoc confirmation matcher:** the shipped `upwork_chat` 120-char body-prefix
  matcher in `internal/connector/upworkcrm/` — copy it, do not invent a second idiom.
- **Sender seam + fake:** `internal/connector/google/send.go` (`AccountSender`,
  `BridgeSender`) and `internal/tools/delivery_lifecycle_integration_test.go`
  (`fakeGmailSender`, `tools.SetGmailSender`).
- **Tool + schema + MCP listing:** `internal/tools/delivery.go` (validator + handler +
  `marshalResult`), `internal/tools/createtask.go:39-73` (`Register`),
  `internal/mcpserver/schemas.go` (`agentTools` entry with a JSON Schema; `worker_id`
  never in a schema).
- **Long-running process shape:** `cmd/orchestratord` (signal handling, single
  instance via `pg_try_advisory_lock`, ticker + wake-up loop) — watch mode should take
  its own advisory lock (suggest `0x51570010`) so two Deployment replicas cannot both
  IDLE the same mailboxes.
- **pgcrypto round-trip test:** `internal/connector/google/oauth_integration_test.go`.
- **Cleanup pact:** `internal/connector/google/integration_test.go` and
  `internal/triage/integration_test.go` `cleanupTriage`.

## Verification protocol

Before commit:

1. `go test ./...` — offline. Must include the criterion-2 regression set unmodified.
2. `go vet ./...`. Grep checks: no `internal/executor` import from connector code; no
   IMAP/SMTP import outside `internal/connector/google`; no `net/smtp` call site
   outside `smtp.go`; the only `SMTPSender.Send` caller is `MailSender`.
3. `make integration` — `make db-up && make migrate` (0011 applied twice: second is a
   no-op), then `go test -tags integration -p 1 ./...`:
   - fake `MailSource` → raw-first assertions (raw rows exist with `normalized_at
     IS NULL` before normalize), then normalize → criteria 10/11;
   - immediate second pass ⇒ `raw_inserted=0`, cursor stable;
   - UIDVALIDITY bump ⇒ folder resync, new external_ids, still ONE normalized row per
     Message-ID;
   - same message in INBOX + Sent + a second account ⇒ N raw rows, 1 normalized row,
     `dedup_skipped` counted;
   - `--normalize-only --all` with the source unreachable ⇒ identical normalized rows;
   - delivery lifecycle: draft → approve → send (fake SMTP listener) → `delivery_sent`
     → inject the Sent-folder copy → `confirmed_at` + `delivery_confirmed` + `tasks`
     count unchanged; then the same with a REWRITTEN Message-ID ⇒ the body-prefix belt
     confirms it;
   - resend refusal on a row with `sent_external_id`;
   - `mail_search`/`mail_read_thread` through the executor with the audit rows
     asserted, and a `worker:*` actor denied on `send_delivery` with rule
     `human_only`.
4. Manual smoke, local, no cluster:
   - `psql -h 192.168.50.49 -U ops -d ops -c "\d source_accounts"` shows the new
     columns.
   - `printf '%s' "$APP_PW" | DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/google-auth
     add-app-password sspataro@gmail.com` → LOGIN verified, row written,
     `auth_type='app_password'`; `google-auth list` shows it; `SELECT
     app_password_encrypted IS NOT NULL` true and the plaintext appears nowhere in the
     output.
   - `DATABASE_URL=... MAIL_SOURCE=imap go run ./cmd/connectors/google --backfill 168h`
     (one week first, then 2160h) → `sync_runs` `ok` with plausible counters;
     `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL` → 0;
     spot-check one known thread; one own sent mail has `direction='outbound'`;
     `SELECT count(*) FROM tasks` unchanged.
   - Re-run immediately ⇒ `raw_inserted=0`.
   - `MAIL_SOURCE=imap go run ./cmd/connectors/google --watch`, send yourself a mail
     from a phone, watch it land within seconds; `Ctrl-C` shuts down clean.
   - IMAP hygiene: the test message is still UNREAD in Gmail's UI afterwards.
5. Real send smoke (this is the "usable alone" gate):
   `UPDATE source_accounts SET send_enabled=true WHERE account_email='…'`, seed a
   done_locally task + thread, `cmd/drafts` or `draft_delivery` via `opsctl call`,
   approve + send through the dashboard, confirm the mail arrives, then let the
   connector ingest the Sent copy and assert `confirmed_at` fills. **Record in
   INSTITUTIONAL_KNOWLEDGE.md whether the submitted `<sb-…>` Message-ID survived** —
   that determines which of the two matchers is load-bearing in production. If the
   Sent copy does not appear at all (submission service does not auto-append), that is
   the contingency in Future work.
6. MCP smoke: in an interactive session, `mail_search` for a known subject,
   `mail_read_thread` on the hit, then `approve_delivery` + `send_delivery` on a real
   drafted row — confirming the actor gate resolves as decided in OQ1 and that a
   `worker:*` identity is refused.
7. Deploy: build + push `192.168.50.20:5000/switchboard:<pinned tag>`, apply the
   Deployment from `~/projects/personal/kube/switchboard/`, confirm the reconcile
   heartbeat query advances. Secrets: `switchboard-db` + `switchboard-token-key`
   only — **no new secret is needed, because the app passwords live encrypted in the
   database.**

## Decisions made unilaterally (rationale attached; flag in review if wrong)

1. **`github.com/emersion/go-imap/v2` as a direct dependency.** The repo's
   "hand-rolled client, no SDK" precedent (`internal/provider/openai.go`,
   `gmail.go`) is about REST. IMAP is a stateful line protocol with literals, UID
   semantics, SPECIAL-USE, and IDLE; hand-rolling it is a multi-week bug farm. go-imap
   v2 is the standard Go client, MIT, small dependency tree, and has IDLE. Parsing
   stays stdlib (`net/mail`, `mime*`) so the normalizer remains pure and dependency-free.
   SMTP stays stdlib `net/smtp`.
2. **Accounts stay `provider='google'`, channel stays `'gmail'`.** They are the same
   mailboxes. Keeping the provider means the own-email direction set, the
   `normalized_messages_gmail_msgid_idx` dedup index, `draft_delivery`'s From
   resolution, `deliveries.channel` CHECK, `drafts/store.go`'s thread resolution, and
   `confirmDelivery` all work unchanged. Inventing `provider='imap'` or
   `channel='email'` would fork the vocabulary and silently disable dedup and loop
   closure. `auth_type` carries the real distinction.
3. **New column `app_password_encrypted` + `auth_type`, not reuse of
   `refresh_token_encrypted`.** Reuse would make "is this an OAuth refresh token or an
   IMAP password?" unanswerable from the row, and `TokenClient` would happily feed a
   password to the OAuth token endpoint. An explicit marker is one migration and zero
   ambiguity — and it lets one account hold both during any transition.
4. **`external_id = imap:{folder}:{uidvalidity}:{uid}`.** Stable within a UIDVALIDITY
   (so hash-compare upsert works), self-invalidating across one (so a UIDVALIDITY
   change forces resync for free), per-folder (so the Sent copy is captured as its own
   raw row — invariant 1 is per-account/per-source capture, dedup is normalize-time).
5. **Raw is base64 inside a JSON envelope.** `raw_source_items.raw_json` is JSONB;
   RFC822 bytes are not valid UTF-8 in general. Base64 is lossless and the envelope
   carries the IMAP metadata (uid, uidvalidity, flags, INTERNALDATE) that the bytes do
   not — all of which the normalizer needs. The alternative (a `raw_bytes BYTEA`
   column) forks the raw table for one source.
6. **1 MiB size cap with marked truncation.** A 90-day window over a 107k-message
   mailbox will contain multi-MB attachments; capturing them verbatim would put
   gigabytes of base64 into JSONB for zero triage value. Headers-only above the cap
   still yields Message-ID, direction, subject and threading. `"truncated": true`
   makes the loss queryable and the decision reversible (raise the cap, re-run
   `--full`).
7. **INBOX gets IDLE; Sent gets the 10-minute reconcile.** Halves the connection
   count and the failure surface. Sent-folder latency only affects delivery
   confirmation, which no rule waits on.
8. **Watch mode lives in `cmd/connectors/google` behind `--watch`, not a new binary.**
   One image entrypoint, shared flags/env, and backfill/replay reuse the identical
   one-shot path (an explicit requirement). The 10-minute context timeout applies to
   one-shot mode only.
9. **`MAIL_SOURCE` is an explicit three-way selector with a default that preserves
   today's behaviour.** Inferring the source from `auth_type` would silently change
   which code path runs the moment a row is added; an explicit env is greppable in the
   manifest and testable (criterion 2).
10. **`thread_key = gmail:{email}:{References[0] | In-Reply-To | own Message-ID}`.**
    The three-segment shape is forced by `splitGmailThreadKey`. Rooting on the
    References chain is the standard RFC 5322 threading heuristic and is deterministic
    per message with no ordering dependency. Consequence to accept: threads ingested
    under the Gmail-API path (third segment = Gmail thread id) and the IMAP path (third
    segment = root Message-ID) do not merge. Since Gmail-API mail ingest is abandoned,
    the practical fix is to re-normalize or leave the old rows; either way messages are
    still deduped by Message-ID, only the thread grouping differs.
11. **Loop closure gets a body-prefix belt.** Submission services are known to rewrite
    `Message-ID`; if that happens, the Message-ID matcher silently never fires and
    invariant 5 quietly stops holding. The belt is the already-shipped `upwork_chat`
    idiom, scoped by `from_account_id` + unconfirmed sent deliveries. It never
    overwrites `sent_external_id` (that is the invariant-4 idempotency token) — the
    observed id goes in the `delivery_confirmed` payload.
12. **`mail_search` is ILIKE + LIMIT, not full-text or vector.** Deterministic, no
    index migration, no embedding backfill, and honest about being a lookup over what
    was ingested. `content_chunks`/`embeddings` exist in the schema for the day this
    is not enough — that is a separate ticket, not a bundled one.
13. **`google-auth` gains a subcommand rather than a new `mail-auth` binary.** Same
    table, same `OPS_TOKEN_KEY`, same trusted-spine charter (it writes
    `source_accounts` directly, exactly like connectors do — opsctl's
    everything-through-the-executor charter deliberately does not cover it). A second
    binary would duplicate the pgcrypto plumbing and add an image entrypoint.
14. **Password on stdin only.** argv leaks through `ps`; env leaks through
    `/proc/*/environ` and shell history. stdin also makes the k8s/one-liner path
    (`kubectl get secret … | base64 -d | google-auth add-app-password …`) natural.
15. **No `X-*` header on outbound mail.** It would be a stable automation fingerprint
    on a client-visible surface (invariant 6) and it is not needed: the body-prefix
    belt covers Message-ID rewriting without marking the message.
16. **Advisory lock in watch mode.** Two replicas IDLE-ing the same mailboxes would
    double-fetch and race the cursor. Same single-instance idiom as `orchestratord`.

## Open questions — ANSWERED 2026-07-26, both option A

1. **Actor-prefix gate: A.** `policy.humanActor()` strips one optional leading `mcp:`
   before checking `dashboard:`/`opsctl:`/`manual:`. One function, one test; the
   policy core gains knowledge that a transport prefix exists, and any future
   transport wrapper must be added there too. Chosen because the audit row keeps the
   full `mcp:manual:salvo` — "which surface triggered this send" survives, which the
   adapter-side fix would have destroyed permanently.
2. **`mail_search` scope: A.** Every ingested `provider='google'` mailbox is
   searchable; no `agent_searchable` column, nothing added to migration 0011. The
   grounds: that mail already sits in `normalized_messages` where triage, drafts, and
   `task_context` read it, so a search tool is a new door to a room agents are
   already in — not a new trust boundary.
   **Consequence to state plainly in the tool descriptions:** the personal mailbox
   (~106k messages in the 90-day window) is readable by ANY Claude Code worker
   session, not only the interactive one. If that stops being acceptable, the
   retrofit is the opt-in flag from option B plus a filter in both handlers.

Everything else was resolvable from CLAUDE.md, INSTITUTIONAL_KNOWLEDGE.md, and the
shipped code.

## Future work (not this SPEC)

- Calendar without Google OAuth: CalDAV against `apidata.googleusercontent.com/caldav/v2`
  with the same app password, or ICS secret-address polling — revives
  `normalized_events`, availability, and `propose_slots`.
- Contingency if the submission service does NOT auto-append to Sent: IMAP `APPEND`
  the submitted MIME to the Sent folder from `send_delivery`'s success path (a write
  verb against the mailbox — needs its own review, since it makes the connector no
  longer read-only).
- Full-mailbox backfill + `content_chunks`/embeddings + semantic `mail_search`.
- Attachment ingestion (`normalized_documents`) for messages over the size cap.
- Gmail mutation tier (archive/label) as a new policy-matrix channel with its own
  approval tier, if ever wanted.
- Cross-account/cross-path thread unification.
- MQTT liveness topic for non-worker spine services (connector, dashboard, drafts).
- `OPS_TOKEN_KEY` rotation procedure (re-encrypt both credential columns in one
  transaction).
