> Jira: SWT-7

# 07-google-oauth-pollers — Google OAuth + Gmail/Calendar pollers + availability service

## Source

Build-order step 7, quoted from CLAUDE.md:

> 7. Google OAuth (one project, Desktop-app client, loopback flow, publish to
>    In Production to avoid 7-day token expiry; test users = the 5 accounts;
>    readonly scopes only). Gmail + Calendar pollers (5–15 min). Message-ID
>    dedup across accounts. Availability service (free/busy merge +
>    propose_slots — deterministic, no LLM).

Constrained by invariants 1 (raw-first), 2 (one funnel), 3 (propose_slots is a
tool → executor path), 5 (own-message loop closure — the Message-ID seam this
step must preserve for step 8), 7 (availability is deterministic, no LLM), and
the policy-matrix line "Calendar own blocks: auto (always via availability
service propose_slots)".

## Goal

Ship the Google connector — OAuth token plumbing (Desktop-app loopback flow,
pgcrypto-encrypted refresh tokens in `source_accounts`), a one-shot Gmail +
Calendar poller in the exact upworkcrm raw-first/normalize shape, cross-account
Message-ID dedup, and a deterministic availability service exposed as an
executor tool — all buildable and fully tested NOW against a faked Google API,
with the real GCP/OAuth setup isolated in an operator runbook.

**Usable alone means:** with the code shipped and CI green against the fake,
Salvador runs the operator runbook once (GCP project + Desktop client +
`google-auth add` per account — the only part that needs his browser), then
`go run ./cmd/connectors/google` on a cron cadence fills the ops db with the
raw + normalized mail/calendar history of all 5 accounts, deduped by
Message-ID, and `opsctl call --tool propose_slots` answers "when am I free?"
deterministically. Triage (step 6, shadow) starts seeing Gmail inbound with no
further work. The real-account smoke is a documented operator step, NOT a
gate on shipping the code.

## HARD ENVIRONMENT FACT (verified 2026-07-11)

No Google Cloud OAuth Desktop client credentials exist on this machine.
Creating the GCP project, consent screen (In Production), Desktop-app client,
and authorizing the 5 accounts requires Salvador's browser. This SPEC
therefore splits into:

- **(a) Buildable now** — everything below: OAuth token plumbing, pollers,
  dedup, normalizers, availability, all tested against `httptest` fakes and
  the compose db. Acceptance criteria 1–13 are satisfiable with the fake.
- **(b) Operator runbook** — the one-time GCP/consent/authorize steps and the
  real smoke, in "Verification protocol / Operator runbook". Criterion 14 is
  the runbook itself existing; the real smoke completes "pending OAuth
  authorization".

## Acceptance criteria

1. `go build ./...` and `go test ./...` pass offline. Connector unit tests
   (normalizers, body extraction, direction rule, cursor logic, availability
   merge/propose) need zero network and zero Postgres.
2. `cmd/google-auth add <email>` runs the loopback OAuth flow (opens/prints
   the auth URL, receives the code on `http://127.0.0.1:{port}/callback`,
   exchanges it), verifies the authorized account via Gmail
   `users.getProfile` equals `<email>`, and upserts one `source_accounts` row:
   `provider='google'`, `account_email=<email>`, `refresh_token_encrypted =
   pgp_sym_encrypt(token, OPS_TOKEN_KEY)`, `scopes` = exactly the two readonly
   scopes, `send_enabled=false`. Tested end-to-end against a fake token +
   profile endpoint (integration: real pgcrypto round-trip on the compose db).
3. Token plumbing: the connector decrypts the refresh token
   (`pgp_sym_decrypt`) and builds an `oauth2.TokenSource`; access tokens are
   never persisted; if Google returns a rotated refresh token it is
   re-encrypted and saved. The token never appears in logs, stats, or errors.
4. Raw-first (invariant 1): every Gmail message (format=full JSON) and every
   Calendar event lands verbatim in `raw_source_items` (`external_id`
   `gmail:{gmailMessageId}` / `calendar:{eventId}`, content_hash, per-account)
   before any normalization; the two phases are strictly ordered as in
   upworkcrm. `--normalize-only --all` rebuilds all normalized rows from raw
   alone with no Google client constructed.
5. Gmail incremental sync: cursor = max `internalDate` seen, stored per
   account in `source_accounts.sync_cursor` (`{"gmail_internal_date_ms": N}`),
   queried as `after:{floor((cursor-overlap)/1000)}`; first run backfills a
   configurable window (default 90d). Drafts (label `DRAFT`) and chats are
   skipped at fetch time. Fake-API test proves: second run re-reads only the
   overlap and inserts zero new raw rows.
6. Calendar incremental sync: initial full sync of `primary` with
   `singleEvents=true`, `timeMin=now-30d`, `timeMax=now+90d`, capturing
   `nextSyncToken` from the last page into `sync_cursor`
   (`{"calendar_sync_token": "..."}`); subsequent runs use `syncToken` only;
   HTTP 410 drops the token and re-windows. Cancelled instances are ingested
   and normalized with `status='cancelled'` (never deleted). Fake-API test
   covers token advancement and the 410 path.
7. Gmail normalization: one `normalized_messages` row per winning raw message —
   `external_message_id` = RFC 5322 `Message-ID` header verbatim (trimmed,
   angle brackets kept; fallback `gmail:{gmailMessageId}` when the header is
   absent), `sent_at` from `internalDate`, `subject`/`sender` from headers,
   `channel='gmail'`, `body_text` = first `text/plain` part (base64url
   decoded, nested multipart walked) else the API `snippet`. Thread upsert on
   `thread_key = gmail:{account_email}:{gmailThreadId}`.
8. Direction rule (invariant 5 seam): `direction='outbound'` iff the From
   address matches ANY `provider='google'` account_email (set loaded at
   normalize start), else `'inbound'` — so our own sends are outbound in
   every mailbox copy that wins dedup and can never be re-triaged.
9. Message-ID dedup ACROSS accounts: two accounts holding the same message
   (same Message-ID) yield TWO raw rows (raw-first is per-account) but ONE
   `normalized_messages` row. Enforced mechanically by a partial unique index
   on `(external_message_id) WHERE channel='gmail'` (migration 0005) and
   counted in stats (`dedup_skipped`); the losing raw item is still stamped
   `normalized_at`. Integration test: two fake accounts, same Message-ID →
   one normalized row, two raw rows.
10. Calendar normalization: one `normalized_events` row per raw event
    (upsert on `raw_source_item_id`), with `starts_at`/`ends_at` (from
    `dateTime` or all-day `date`), `title`, `status`, `transparency`
    (absent ⇒ `'opaque'`), `all_day`, `attendees` (JSON array of
    `{email, response_status, organizer, self}`).
11. Availability service: `internal/availability` is pure —
    `Merge(busy []Interval) []Interval` (sort + coalesce overlaps) and
    `ProposeSlots(busy, cfg) []Slot` (earliest-first, 30-min aligned, exactly
    `duration` long, inside working hours, within the window, not overlapping
    busy, up to `count`). Busy = normalized events on accounts with
    `calendar_in_availability=true` AND `status <> 'cancelled'` AND
    `transparency <> 'transparent'` (Google all-day events default
    transparent, so they fall out naturally). Unit tests: overlap merge,
    working-hours clipping, weekend skip, duration fit, determinism (same
    input ⇒ same output). No LLM, no network (invariant 7 discipline).
12. `propose_slots` is registered on the executor via `tools.Register`
    (invariant 3: validate → policy → audit → handler), args
    `{duration_minutes, window_start?, window_end?, count?}`, response
    `{slots: [{start, end}]}` in RFC 3339. Reachable via
    `opsctl call --tool propose_slots`. Read-only: writes nothing but its
    audit row.
13. Zero tasks, zero deliveries: no run of google-auth, the connector, or
    propose_slots changes `count(*)` of `tasks` or `deliveries`. Failure
    bookkeeping matches upworkcrm: `sync_runs` row per account × phase,
    errors leave `status='error'` + message, cursor un-advanced, exit
    non-zero; migration 0005 applies cleanly twice.
14. The operator runbook (below) is complete enough that Salvador can execute
    it without re-deriving anything: GCP console steps, client-secret path
    convention, per-account `google-auth add`, real smoke queries.

## Data model changes

Migration: `migrations/0005_google_connector.sql` (forward-only). No new
tables.

```sql
-- Calendar payload fields the first events writer needs (0001 left
-- normalized_events minimal for this step to extend).
ALTER TABLE normalized_events
  ADD COLUMN title        TEXT,
  ADD COLUMN status       TEXT,
  ADD COLUMN transparency TEXT,
  ADD COLUMN all_day      BOOLEAN NOT NULL DEFAULT false;

-- One event per raw item: upsert target (mirror of normalized_messages_raw_item_idx).
CREATE UNIQUE INDEX normalized_events_raw_item_idx
  ON normalized_events (raw_source_item_id);

-- Cross-account Message-ID dedup: the mechanical guarantee behind criterion 9.
CREATE UNIQUE INDEX normalized_messages_gmail_msgid_idx
  ON normalized_messages (external_message_id) WHERE channel = 'gmail';
```

Rows written at runtime (not by migration):

- `source_accounts`: written by `google-auth` ONLY (`provider='google'`, real
  `account_email`, encrypted refresh token, scopes, `send_enabled=false`,
  `calendar_in_availability` default true, `--no-availability` flag sets
  false). Unlike upworkcrm, the connector does NOT `EnsureAccount` — it
  iterates existing `provider='google'` rows and exits with a clear error if
  none exist ("run google-auth add first").
- `sync_cursor` shape (reusing the 0002 column, per its own comment):
  `{"gmail_internal_date_ms": 1751234567890, "calendar_sync_token": "CPj..."}`.
- `raw_source_items.external_id`: `gmail:{gmailMessageId}`,
  `calendar:{eventId}` (singleEvents instance ids like `{id}_{ts}` are
  already unique). content_hash = shared canonical-JSON sha256 (lifted from
  upworkcrm — see Files).
- Encryption idiom: SQL-side pgcrypto —
  `UPDATE ... SET refresh_token_encrypted = pgp_sym_encrypt($1, $2)` /
  `SELECT pgp_sym_decrypt(refresh_token_encrypted, $1)` — key from env
  `OPS_TOKEN_KEY` (required by google-auth and the connector; never stored,
  never logged). pgcrypto is already installed on both the real db
  (pre-created by superuser) and compose (migration 0001).

## API / MCP tool changes

One executor tool: **`propose_slots`** (added to `tools.Register` in
`internal/tools`, so it flows validate → policy check → audit start → handler
→ audit complete like every other tool; the static policy allow set picks it
up from `Registry.Names()`). Read-only. NOT MCP-listed this step (no worker
needs it yet); reachable via `opsctl call`. Step 8's calendar-write path will
consume it per the policy matrix ("always via availability service
propose_slots").

Request/response:

```json
{"duration_minutes": 30, "window_start": "2026-07-13T00:00:00+02:00",
 "window_end": "2026-07-18T00:00:00+02:00", "count": 3}
→ {"slots": [{"start": "...", "end": "..."}, ...]}
```

Defaults: window = next 5 business days, count = 3. Validate rejects
duration_minutes ≤ 0 or window_end ≤ window_start.

The connector and google-auth register nothing on the executor — same stance
as the upworkcrm SPEC: trusted ingestion spine, audited via `sync_runs`, not
agent-reachable.

## MQTT topics

None. The connector is one-shot like upworkcrm; no heartbeat (fleet topics
are for workers). Nothing outbound.

## Files likely to touch

Existing (verified in repo):

- `go.mod` — promote `golang.org/x/oauth2` from indirect to direct (already
  in the module graph at v0.35.0; zero new downloads).
- `internal/connector/upworkcrm/hash.go` — lift `ContentHash` into a shared
  package (`internal/connector/chash`), upworkcrm imports it; mechanical,
  tests move with it. (Alternative if review prefers zero churn: duplicate
  the ~20 lines; the lift is cleaner.)
- `internal/tools/` — new `proposeslots.go`; wire into `Register` (signature
  grows the availability config or reads env inside the wiring — follow
  whatever `Register(reg, pool)` extension is least invasive).
- `internal/triage/integration_test.go` — `cleanupTriage` must learn the new
  foreign corpus: add delete lines for `provider='itest-google-%'` fixtures
  (see Test-infra interplay below).
- `Makefile` — optional `google-sync` convenience target; `integration`
  already covers new build-tagged tests via `go test -p 1`.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — after the runbook executes: GCP
  project name, client-secret path, OPS_TOKEN_KEY location, per-account
  quirks.

New:

- `migrations/0005_google_connector.sql` — as above.
- `internal/connector/google/oauth.go` — `oauth2.Config` construction from
  the client-secret file (`installed` JSON shape), loopback listener + code
  exchange, TokenSource wrapper with the re-encrypt-on-rotation persistence
  hook, pgcrypto load/save of refresh tokens.
- `internal/connector/google/gmail.go` — REST client (net/http, baseURL
  parameter for fakes, OpenAI-adapter style): `users.getProfile`,
  `users.messages.list` (q, pageToken), `users.messages.get?format=full`.
- `internal/connector/google/calendar.go` — REST client:
  `calendars/primary/events` (timeMin/timeMax/singleEvents/pageToken/
  syncToken), 410-GONE detection.
- `internal/connector/google/ingest.go` — per-account gmail + calendar
  ingest phases: cursor read, list/fetch, raw upsert via the shared
  hash-compare idiom (insert / update+reset-normalized_at / unchanged),
  cursor advance on success, `sync_runs` bookkeeping per account × phase.
- `internal/connector/google/normalize.go` — pure mappers
  (`NormalizeGmailMessage(raw, ownEmails)` incl. header extraction, multipart
  text/plain walk, base64url decode, direction rule;
  `NormalizeCalendarEvent(raw)`), plus the phase driver reading only
  `raw_source_items` (pending or `--all`).
- `internal/connector/google/sink.go` — PGSink over the ops pool: account
  listing (no EnsureAccount), cursor, runs, raw upserts, message upsert with
  the dedup check (SELECT-first on `(channel='gmail', external_message_id)`
  belt + unique-index suspenders; loser counted `dedup_skipped`, still
  stamped normalized), event upsert on `raw_source_item_id`, thread upsert
  reusing the 0002 `thread_key` index.
- `internal/connector/google/{normalize,ingest,oauth}_test.go` — unit, fakes.
- `internal/connector/google/fake_google_test.go` — `httptest` fake serving
  token endpoint + the four API endpoints with canned pages, historyless
  `after:` filtering, syncToken advancement, 410 mode.
- `internal/connector/google/integration_test.go` — build tag `integration`,
  gated on `DATABASE_URL`, compose db + fake Google; joins the cleanup pact.
- `internal/availability/availability.go` + `availability_test.go` — pure
  Interval/Merge/ProposeSlots + `Config{WorkStart, WorkEnd, Days, Location}`.
- `internal/availability/store.go` — the one SQL read (busy intervals from
  `normalized_events` ⋈ `source_accounts` with the criterion-11 filter).
- `cmd/google-auth/main.go` — `google-auth add <email> [--no-availability]`,
  `google-auth list`. Env: `DATABASE_URL`, `OPS_TOKEN_KEY`,
  `GOOGLE_CLIENT_SECRET_FILE` (default
  `~/.config/switchboard/google_client_secret.json`).
- `cmd/connectors/google/main.go` — one-shot, upworkcrm-shaped flags:
  `[--full] [--normalize-only] [--all] [--account email] [--overlap 1h]
  [--backfill 2160h]`; 10-min context timeout (backfill is bigger than the
  CRM's 823 rows).

## In scope

- Migration 0005; google-auth CLI; one-shot connector for all
  `provider='google'` accounts (gmail phase → calendar phase → normalize
  phase, sequential per account); cursor persistence; Message-ID dedup;
  direction rule; availability package + `propose_slots` executor tool +
  `opsctl call` reachability; fake-Google test harness; pgcrypto round-trip
  integration test; operator runbook.
- Readonly scopes ONLY: `gmail.readonly`, `calendar.readonly`. `send_enabled`
  stays false everywhere.
- Test-infra interplay (bit before, per INSTITUTIONAL_KNOWLEDGE): the google
  integration suite (a) uses synthetic accounts
  `provider='google', account_email LIKE 'itest-google-%'` — production
  provider value, test-scoped emails, so the dedup index is exercised for
  real; (b) scopes ALL count assertions to its own account ids — no global
  counts, so it does not need to clean foreign corpora; (c) cleans its own
  leftovers first in FK order (messages/events → threads → raw → sync_runs →
  accounts); (d) because its inbound normalized_messages are visible to the
  GLOBAL triage pending filter, `cleanupTriage` in
  `internal/triage/integration_test.go` gains matching foreign-corpus
  deletes — that is the pact-join obligation on the triage side.

## Out of scope (do not bundle)

- **Step 8**: Gmail SEND adapter, deliveries, draft worker, dashboard
  approve/send, calendar WRITES (booking the proposed slot), Upwork assisted
  tier. This step only preserves the seams: Message-ID verbatim,
  From-inherited-from-thread data available, `propose_slots` callable.
  Re-consent with send/write scopes is step 8's (documented) problem —
  google-auth's scope list is a constant to extend then.
- **Step 9**: Jira/GitHub connectors.
- Triage changes: gmail inbound flows into the EXISTING shadow triage with
  zero code changes (its pending filter is channel-agnostic). Expect the
  UNMAPPED lane to grow — that is the step-6 shadow-diff routine, not work
  here.
- Gmail history API (`users.history.list`) incremental sync — Future work;
  see Decisions for why `after:` cursor wins now.
- Cross-account thread unification (same conversation in two mailboxes lands
  in per-account threads; the messages themselves are deduped) — Future work.
- Attachment ingestion, HTML→text extraction beyond the text/plain-or-snippet
  rule, embeddings backfill.
- Secondary calendars (only `primary` per account); shared-calendar ingest.
- Key rotation for OPS_TOKEN_KEY; token revocation UX (`gcloud`/console
  suffices).
- CronJob/deploy packaging (still no long-running service shipped).

## Invariants that apply

- **1. Raw-first** — the write happens in
  `internal/connector/google/ingest.go`: full-format Gmail JSON and verbatim
  Calendar event JSON into `raw_source_items` for the whole batch before the
  normalize phase starts; normalize reads only `raw_json` (criterion 4's
  no-client re-normalize proves it). Content change ⇒ raw updated +
  `normalized_at` reset. Dedup deliberately does NOT apply to raw: both
  accounts' copies are captured (reprocessing must always be possible);
  dedup is a normalize-time decision.
- **2. One funnel** — no new tables; messages and events land in the existing
  canonical objects. Nothing actionable is minted (criterion 13); triage owns
  task creation.
- **3. Everything through the executor** — `propose_slots` is registered via
  `tools.Register` like every sibling tool; no side door, no raw calendar
  query exposed to agents. google-auth and the connector register nothing and
  are not agent-reachable.
- **4. Nothing external without a delivery row** — nothing is sent: readonly
  scopes make this mechanical (Google will 403 a send attempt), and no code
  path constructs an outbound call. `send_enabled=false` on every row this
  step writes.
- **5. Own-message loop closure** — the load-bearing seam. Obligations here:
  (a) `external_message_id` = RFC Message-ID preserved verbatim — the key
  step 8's normalizer hook will match against `deliveries.sent_external_id`;
  (b) the any-own-account direction rule makes every copy of our own sends
  `outbound`, so the triage inbound filter can never re-triage them, even
  when the recipient-mailbox copy wins dedup; (c) zero tasks (criterion 13).
- **7. Orchestrator purity (discipline transfer)** — normalizers and the
  whole availability package are pure functions (unit-testable, zero
  network, no LLM — CLAUDE.md pins propose_slots as deterministic). HTTP
  edges live behind the client structs with injectable baseURLs.

(6 has no surface: nothing client-visible is authored.)

## Sibling patterns to copy

- **THE pattern**: `internal/connector/upworkcrm/` — SourceReader/Sink split,
  `Ingest`/`Normalize` phase functions with `fail()` run-bookkeeping,
  `upsertRaw` hash-compare decision in the phase (not the sink) for unit
  testability, cursor in `source_accounts.sync_cursor`, one-shot
  `cmd/connectors/upworkcrm/main.go` flag shape. Copy the shape file-for-file.
- **Hand-rolled REST client**: `internal/provider/openai.go` — net/http, no
  SDK, baseURL defaulted-but-injectable, package as the isolation boundary.
  The Gmail/Calendar clients follow it exactly.
- **Tool registration**: `internal/tools/getnext.go` + `tools.Register` —
  closure-wired handler, `internal/executor/registry.go` Tool struct,
  schemas/validate conventions from siblings.
- **Integration test shape**: `internal/connector/upworkcrm/integration_test.go`
  (build tag, env gate, rerunnable FK-ordered cleanup) and
  `internal/triage/integration_test.go` `cleanupTriage` (the pact to join).
- **opsctl call**: `cmd/opsctl/main.go` `parseCall` — propose_slots needs no
  new opsctl code; `opsctl call --tool propose_slots --args '{...}'` works
  as-is once the tool registers.

## Verification protocol

Buildable-now half (before any Google credentials exist):

1. `go test ./...` — unit green offline.
2. `make integration` — migrates 0001–0005 onto compose Postgres (twice-apply
   check on 0005), then: pgcrypto round-trip; fake-Google ingest → raw-first
   assertions → normalize → criteria 5–10; immediate second run → zero new
   raw rows, `dedup_skipped` stable; mutate a fake message + `--full` → raw
   updated in place, normalized updated not duplicated; `--normalize-only
   --all` with no fake reachable → identical normalized rows; calendar 410 →
   re-window recovery; two-account Message-ID dedup; propose_slots through
   the executor (audit row asserted); global `tasks`/`deliveries` counts
   unchanged (scoped assertions elsewhere per the pact).
3. `go vet ./...`; grep: no import of `internal/executor` from connector
   code, no send-scoped URL anywhere.

### Operator runbook (Salvador, once — the ONLY manual part)

4. GCP console (any Google account as owner): create project `switchboard`
   (one project for all 5 accounts). Enable **Gmail API** and **Google
   Calendar API**.
5. OAuth consent screen: External; app name `switchboard`; add the 5 account
   emails as test users; add scopes `gmail.readonly` + `calendar.readonly`;
   then **Publish to In Production** (readonly-only apps skip verification;
   staying in Testing expires refresh tokens after 7 days — this is the
   entire reason for publishing).
6. Credentials → Create OAuth client ID → **Desktop app**. Download the JSON
   to `~/.config/switchboard/google_client_secret.json` (chmod 600).
7. Generate the token key once: `openssl rand -base64 32` →
   `OPS_TOKEN_KEY` in `~/.bashrc` (same grep/eval caveat as OPENAI_API_KEY;
   record in INSTITUTIONAL_KNOWLEDGE.md).
8. Apply 0005 to the real ops db:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations`
   (twice; second all-skips).
9. Per account (×5): `DATABASE_URL="$OPS_DATABASE_URL" go run
   ./cmd/google-auth add <email>` — browser opens, pick the RIGHT account
   (google-auth rejects a mismatch via getProfile). `google-auth list` shows
   5 rows, scopes correct, send_enabled=false.
10. Real smoke ("usable alone", pending OAuth authorization until 9 is done):
    `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google`, then
    psql: latest `sync_runs` per account `status='ok'` with plausible stats;
    `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL` → 0;
    spot-check one known cross-account email → exactly one
    `normalized_messages` row; one own sent mail → `direction='outbound'`;
    `SELECT count(*) FROM tasks` unchanged. Re-run immediately →
    raw_inserted=0. `opsctl call --tool propose_slots --args
    '{"duration_minutes":30}'` → slots that visibly dodge a real busy block.
    Cron it at 5–15 min (crontab now; CronJob at deploy time).

## Decisions made unilaterally (rationale attached; flag in review if wrong)

- **golang.org/x/oauth2 + hand-rolled REST; no google.golang.org/api SDK.**
  The SDK is megabytes of generated code for the 4 endpoints we call
  (getProfile, messages.list, messages.get, events.list); x/oauth2 is small,
  focused, already in our module graph (indirect via the MCP SDK — promoting
  it is free), and handles the hard parts: loopback exchange, TokenSource
  auto-refresh. Matches the repo's OpenAI-adapter precedent. Endpoints from
  `golang.org/x/oauth2/google` (same module).
- **One `source_accounts` row per account, `provider='google'`, covering
  Gmail AND Calendar.** The schema is account-shaped (`refresh_token_encrypted`,
  `calendar_in_availability` are per-account, one consent grants both scopes,
  one refresh token serves both APIs). Two rows would duplicate the token and
  fork the cursor for no benefit. Both cursors live side by side in the one
  `sync_cursor` JSONB.
- **Gmail incremental via `messages.list q=after:{cursor}` + overlap, not the
  history API.** One code path that also does the initial backfill; identical
  to the proven upworkcrm cursor idiom; replay-safe (idempotent upserts make
  the overlap free); history ids expire and force a fallback path anyway.
  history.list is a bandwidth optimization irrelevant at 5 mailboxes × 5-min
  cadence — Future work if volume ever demands it. Cursor on `internalDate`
  (server receive time, monotone-enough with a 1h default overlap).
- **`format=full`, whole JSON into raw_json.** It IS the provider's parsed
  representation (headers + MIME tree + base64url bodies) — perfect for
  jsonb and re-normalization. `raw` would need a MIME parser and bloat the
  db; `metadata` lacks the body. Attachments arrive as attachmentId
  references, not bytes — fine, not ingested.
- **Skip DRAFT-labeled messages and chats at fetch time.** Same rationale as
  the CRM's `is_draft` skip: working state, not communications that
  happened. Once sent, the message enters the next pull normally.
- **Dedup = normalize-time, partial unique index as the guarantee, raw kept
  per-account.** Raw-first is per-account capture (invariant 1 — both copies
  must be reprocessable); "one funnel" wants the message once. First
  normalized copy wins; loser stamped normalized + counted. SELECT-first
  check keeps the code readable; the index makes it correct even if a
  concurrent writer ever appears. Scoped `WHERE channel='gmail'` so
  upwork_crm external ids can never collide with RFC Message-IDs.
- **Direction = outbound iff From ∈ {any google account_email}.** A purely
  per-mailbox rule (From == this account) breaks under dedup when the
  recipient copy of an account-to-account mail wins and would surface our own
  message as triageable inbound. The set-based rule is deterministic and
  closes that hole.
- **Thread key `gmail:{account_email}:{threadId}`.** Gmail thread ids are
  per-mailbox with no cross-mailbox correlation and no global-uniqueness
  guarantee; unqualified keys risk collisions, References-chain threading is
  real work for little step-7 value. Per-account threads now; unification is
  Future work.
- **Calendar: `primary` only, singleEvents, initial window now-30d…now+90d.**
  singleEvents without timeMax paginates unbounded recurrence expansions;
  the window bounds it and the syncToken carries increments thereafter (the
  official Calendar sync recipe). Availability only needs the near future.
- **Busy = status≠cancelled AND transparency≠transparent.** Google's own
  semantics: absent transparency means opaque; all-day events default to
  transparent ("Free"), so they exclude themselves without a special case.
- **Availability defaults: Mon–Fri 09:00–18:00, `AVAIL_TZ` default
  `Europe/Rome`, overridable via `AVAIL_WORK_START`/`AVAIL_WORK_END`/
  `AVAIL_WORK_DAYS`.** Env-with-defaults beats a config table for a
  single-user system; the pure functions take an explicit Config so tests
  never read env. Slots 30-min aligned, earliest-first, count default 3 —
  deterministic tie-breaking is the whole point.
- **`propose_slots` registered NOW as an executor tool (not opsctl-only, not
  deferred).** The policy matrix hard-codes "always via availability service
  propose_slots" for calendar blocks; registering the read-only tool this
  step means step 8 consumes an existing audited surface instead of
  retrofitting one. Cost: one tool file.
- **Separate `cmd/google-auth`, not an opsctl subcommand.** opsctl's charter
  is "never writes tool-action tables directly — everything through the
  executor"; the auth flow legitimately writes `source_accounts` directly as
  trusted spine (exactly like connectors do). Separate binary keeps both
  charters clean.
- **SQL-side pgcrypto (`pgp_sym_encrypt`/`pgp_sym_decrypt`) with
  `OPS_TOKEN_KEY` from env.** The schema pins pgcrypto for this column
  (CLAUDE.md: `refresh_token_encrypted [pgcrypto]`); doing it in SQL uses
  the pinned extension directly, round-trips are integration-testable, and
  the key never touches the db. Go-side AES would ignore the pinned design
  for no gain.
- **google-auth verifies the authorized identity via `users.getProfile`.**
  Five accounts in one browser is exactly the setup where the wrong account
  gets clicked; a mismatch aborts before anything is stored.
- **90d Gmail backfill default (`--backfill`), not full-mailbox.** Triage
  operates on current signal; a decade of archive is embedding fodder for a
  later step and would balloon first-run time and raw storage. `--backfill`
  can be raised any time — the cursor idiom makes deeper backfills additive.

No open questions — every ambiguity was resolvable from CLAUDE.md, the
shipped connector precedent, or documented Google API behavior; the genuinely
non-buildable part (GCP console) is isolated in the operator runbook, not a
question.

## Future work (not this SPEC)

- Step 8 re-consent: add `gmail.send` (+ `calendar.events` write) to
  google-auth's scope constant and re-run `add` per account; Gmail send
  adapter sets its own Message-ID so `sent_external_id` matches this step's
  seam.
- Gmail history API incremental sync if poll volume ever matters.
- Cross-account thread unification (References/In-Reply-To chain).
- Secondary/shared calendars; per-calendar availability flags.
- Full-mailbox backfill + embeddings/content_chunks over the mail corpus.
- Message-ID dedup for non-gmail channels if Jira/GitHub mail gateways ever
  collide.
- OPS_TOKEN_KEY rotation procedure (re-encrypt column in one transaction).
- k8s CronJob packaging for the poller cadence.
