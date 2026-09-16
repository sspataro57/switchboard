> Jira: SWT-64

# mail-refetch-targeted: re-fetch named IMAP messages at a raised cap

**Status: FINAL.** No open questions arose. The seven decisions in the owner's brief are
recorded below under "Decisions from the brief"; the smaller choices code reading settled
are under "Decisions made unilaterally".

## Source

Ad-hoc, not a build-order step. Salvador, 2026-09-16:

> do the targeted tool and raise the cap to 100mib

## The problem, as established from the live system

The size cap is applied at FETCH time, not at parse time. `IngestIMAP` calls
`src.Fetch(ctx, folder.Name, uids[start:end], maxBytes)`
(`internal/connector/google/imap_ingest.go:118`) with `maxBytes` from
`cfg.MaxMessageBytes` — set by `google.MaxMessageBytes()`
(`cmd/connectors/google/main.go:96`), which defaults to
`DefaultMaxMessageBytes = 1 << 20` (`internal/connector/google/imap.go:43`).
`IMAPClientSource.Fetch` compares each message's server-reported `RFC822.SIZE` against it
(`imap.go:524`) and, over the cap, pulls headers plus one text part only
(`fetchHeadersAndText`, `imap.go:608`), marking the capture `Truncated` with a `parts`
manifest. **The attachment bytes were never put on the wire and are not in the database.**
No reprocessing of stored data can recover them; only a re-fetch from IMAP at a larger
`maxBytes` can.

Downstream, `mail_list_attachments` reports every part of such a row as
`available: false, unavailable_reason: "not stored: message was over the 1 MiB capture cap
(MAIL_MAX_MESSAGE_BYTES)"` (`internal/connector/google/attachments.go:73-83, 157-169`).

Live example verified 2026-09-16 through `opsctl call --tool mail_list_attachments`:
raw_source_item **73094** (Grady Womack, 8 Sep 2026, thread 141665) lists **8** attachments,
all unavailable, including an 895 KB `..._Keystone_Standard_and_Map_v23_....docx`. Sibling
Foundry messages on the same thread (raw 73080, 72046, 68594) DO have `available: true`
parts, so this is specific to over-cap messages.

**Why the existing connector cannot do this.** An incremental pass searches
`crit.FromUID = stored.UIDNext` (`imap_ingest.go:84`), so an old UID is never revisited. The
only two escapes are `cfg.Full` and a UIDVALIDITY change (`imap_ingest.go:74-85`), and both
re-run the whole `DefaultBackfill` (90d) window for every selected folder of the account.
The account is `sspataro@gmail.com` (`source_accounts` 1003), 106,930 messages — the mailbox
whose backfill already exceeded the pass deadline on every run, which is why the cursor is
now saved per batch (the livelock comment, `imap_ingest.go:130-142`). A mass resync to
recover a handful of messages is not acceptable.

## Goal

Add `opsctl mail refetch`: an explicitly-bounded, hand-run pass that re-fetches NAMED
already-ingested IMAP messages at a 100 MiB cap and upserts them over their existing
`raw_source_items` rows — without touching any folder cursor, and refusing any message whose
folder generation no longer matches the one its row was stored under.

**Usable alone means:** on the workstation, with no image build, no manifest change and no
deploy, Salvador runs

```
opsctl mail refetch --from '@foundryunderwriting.com' --since 720h --limit 50 --dry-run
opsctl mail refetch --from '@foundryunderwriting.com' --since 720h --limit 50
```

and `mail_list_attachments` on raw 73094 then reports the 8 parts as `available: true`,
readable with `mail_read_attachment`.

## Decisions from the brief (owner, 2026-09-16)

1. **It is an `opsctl` subcommand, not a new `cmd/` binary.** `opsctl` already builds the
   pool (`store.NewPool`, `cmd/opsctl/main.go:178`), already reaches the ops DB and, through
   `google.DecryptAppPassword` + `google.NewIMAPClientSource`, already has everything needed
   to reach the mail server. The Dockerfile builds only
   `./cmd/connectors/... ./cmd/tools/migrate ./cmd/dashboard ./cmd/google-auth ./cmd/classify
   ./cmd/orchestratord ./cmd/pipelined` (`Dockerfile:16-17`), so a new `cmd/` binary would
   need a Dockerfile edit, an image build and a kube roll before it could be used at all —
   for a tool whose whole job is a hand-run recovery. As a subcommand it is usable the moment
   it merges and `go install ./cmd/opsctl` runs. Against: `opsctl` grows a verb that talks to
   an external server, which no other `opsctl` verb except `capture-rules gate` (Jira lookup)
   and `prefill_delivery` (the Slack bridge) does. Accepted: the IMAP surface is read-only by
   construction (see invariant notes) and the pass is bounded by `--limit`.
2. **Selection is explicit and bounded, never "everything".** Two selector families,
   mutually exclusive; `--limit` is REQUIRED in both; an unbounded run is refused.
3. **A stored `uidvalidity` that no longer matches the folder's current UIDVALIDITY means
   the UID now names a different message or nothing.** Such a target is REFUSED, never
   fetched. This is the central criterion (6, 7) and is pinned by a mutation test.
4. **The cursor must not move.** The pass reads UIDs from `raw_source_items` and never calls
   `SaveCursor` / `SaveCursorField`, and never writes a `FolderCursor`.
5. **The downstream effect of the upsert is verified in code and pinned by an integration
   test** (criteria 12-16), not assumed.
6. **Two cap numbers, and only one of them ships here** (see "The two cap numbers").
7. **Safety: read-only with respect to tasks and deliveries.** No task creation, no delivery
   row, no send. It refuses an account whose app password it cannot decrypt.

## Acceptance criteria

### Selection and bounds

1. `opsctl mail refetch` accepts exactly two selector families and refuses a mix:
   - **finder:** `--from <substring>` (matched against `normalized_messages.sender`,
     literal substring via the `likeEscape` spelling in `internal/tools/mailattach.go:207`),
     optionally narrowed by `--since <Go duration|RFC3339>` and `--until <RFC3339>`;
   - **explicit:** `--raw-id N[,N...]` (`raw_source_items.id`).
   Giving both, or neither, is an error naming the two families.
2. `--limit N` is REQUIRED, `N >= 1`. Omitting it errors with
   `--limit is required: this tool never runs unbounded`. A run whose selection matches more
   than `N` rows takes the `N` oldest by `sent_at` and SAYS SO in the output (`matched=M
   limit=N truncated=true`), so a silent partial recovery is impossible.
3. `--account <email>` optionally narrows to one `source_accounts` row; without it the
   selection may span app-password accounts and each account is processed independently.
4. Only `imap:`-shaped rows are targets. A `gmail:` or `calendar:` row selected by id is
   refused by name (`not an IMAP-sourced row`), because those paths never stored attachment
   bytes at all (`attachments.go:60`, `GmailPathReason`).
5. `--dry-run` prints the plan — one line per target: raw id, account email, folder, UID,
   stored `uidvalidity` vs live `uidvalidity`, stored `size`, `truncated`, sender, `sent_at`,
   subject — and writes NOTHING: no `raw_source_items`, no `sync_runs`, no cursor, no
   advisory lock. It still contacts IMAP for the live UIDVALIDITY (that is the fact worth
   knowing before a live run) but issues no FETCH.

### The dangerous failure: a stale UIDVALIDITY

6. The IMAP coordinates come from `raw_source_items.raw_json`: `folder`, `uidvalidity`,
   `uid` (the shape `buildIMAPEnvelope` writes, `imap_ingest.go:220-242`). The row's
   `external_id` must equal `imap:{folder}:{uidvalidity}:{uid}`
   (`rfc822.go:52`); a row where the two disagree is refused (`envelope disagrees with
   external_id`) rather than reconciled — it was not written by this connector.
7. **A target whose stored `uidvalidity` differs from the folder's CURRENT UIDVALIDITY is
   refused, counted as `uidvalidity_changed`, and NEVER fetched.** The pass does not fetch
   whatever now sits at that UID, does not "resync", and does not fall back to a search. The
   live value comes from `MailSource.Folders(ctx)`, which fills `Folder.UIDValidity` from
   `SELECT` (`imap.go:428-435`).
8. A target whose folder is not in the source's selectable set (`SelectFolders`, INBOX plus
   `\Sent` or the `MAIL_FOLDERS` override, `imap.go:152`) is refused as
   `folder_not_selectable` — not fetched from some other folder.
9. A target the server returns no message for (the UID was expunged) is counted `gone` and
   writes nothing. Returned messages are matched to targets **by UID**
   (`FetchedMessage.UID`), never by position in the result slice.
10. Every refusal is a per-target outcome printed with its raw id and reason; the pass
    continues with the remaining targets. A transport failure (login, select, fetch) fails
    the pass with a wrapped error and exits non-zero.

### The fetch and the write

11. The fetch uses `maxBytes = RefetchMaxMessageBytes = 100 << 20` (100 MiB), overridable by
    `--max-bytes` (positive integer; non-positive is an error, never a silent default — the
    zero-cap hazard `MaxMessageBytes()` guards against at `imap.go:55-58`). It does NOT read
    `MAIL_MAX_MESSAGE_BYTES`: the connector's cap and this tool's cap are different numbers
    by design.
12. The row is written through the SAME raw-first path the connector uses: envelope built by
    `buildIMAPEnvelope` from the verified live folder + uidvalidity + the fetched message,
    hashed with `chash.ContentHash`, then `RawHash` → `InsertRaw` / `UpdateRaw`
    (`sink.go:130-171`). Consequence, asserted: **no new `raw_source_items` row is created** —
    `count(*)` for the account is identical before and after, and the target row's `id` is
    unchanged.
13. The pass NEVER calls `SaveCursor` or `SaveCursorField`, and `source_accounts.sync_cursor`
    for the account is byte-identical before and after a live run.
14. The refetched row has `normalized_at = NULL` (`InsertRaw`/`UpdateRaw` clear it) and
    `superseded_at = NULL`. The pass does NOT run `google.Normalize` itself (see
    "Decisions made unilaterally" D3); the next connector pass re-normalizes it.
15. **Re-normalization after a refetch preserves identity.** Asserted against Postgres:
    `upsertMessage` (`sink.go:233-283`) upserts `ON CONFLICT (raw_source_item_id)`, so the
    `normalized_messages.id` is unchanged; therefore
    - the message's `capture_decisions` rows still point at it (FK `message_id ... ON DELETE
      CASCADE`, `migrations/0015_capture_rules.sql:105`) and are neither orphaned nor
      duplicated;
    - no second `normalized_messages` row appears for the message, and no second
      `normalized_threads` row;
    - **no task is created and no capture decision is added**: capture's inbox excludes
      messages that already have a decision in the relevant mode, keyed on
      `capture_decisions.message_id` (`internal/capture/rules_store.go:634-694`), and
      `capture_decisions_live_uniq` (partial unique on `message_id WHERE mode='live'`,
      `0015:142`) makes a second live decision impossible even if it tried;
    - no classify lane re-runs: all three inboxes key their `NOT EXISTS` on
      `ai_extractions.raw_source_item_id` plus `ai_runs.worker_type`
      (`internal/classify/store.go:67-106`), and the raw id is unchanged.
16. A refetched OUTBOUND message (a Sent-folder copy) re-runs loop closure on
    re-normalization and must not double-confirm: `confirmDelivery` and
    `confirmDeliveryByBodyPrefix` are both guarded by `confirmed_at IS NULL`
    (`sink.go:316-343`, `sink.go:739-747`), so no second `delivery_confirmed` task event is
    emitted. Asserted, not assumed.
17. The pass takes the per-account advisory lock `PGSink.LockAccount`
    (`sink.go:518`) for a live run and refuses with
    `a google connector pass holds account N; retry shortly` if it is held. `--dry-run` takes
    no lock.
18. It writes one `sync_runs` row per account per live run via `StartRun`/`FinishRun` with
    phase **`imap_refetch`** — a distinct phase value, so `internal/availability/store.go:125`
    (`r.stats->>'phase' = 'calendar'`) is unaffected and the dashboard funnel's per-phase
    grouping (`internal/dashboard/funnel.go:223-225`) shows it as its own bucket. Stats
    carried: `imap_fetched`, `raw_updated`, `raw_unchanged`, plus the refusal counters.
19. **No task, no delivery, no send.** The command registers no executor tool, creates no
    `tasks` row, touches no `deliveries` row, and links no sender. `grep` for
    `tools.SetGmailSender` / `SetSlackSender` in the new code path returns nothing.
20. It refuses an account it cannot use: `OPS_TOKEN_KEY` unset, or
    `google.DecryptAppPassword` failing, is an error naming the account — never a skip that
    reads as "nothing to do".
21. The IMAP surface gains NO write verb. The pass uses only `Folders` and `Fetch` from the
    existing `MailSource` interface (`imap.go:107-120`); the existing structural test
    `TestIMAPClientSource_UsesBodyPeekAndNoWriteVerbs`
    (`imap_ingest_test.go:183`) stays green, and no new mutating method is added to the
    interface or to `imap.go`.

## The two cap numbers

They are deliberately different and only one of them is in this ticket.

| number | where | value | who changes it |
|---|---|---|---|
| refetch cap | `RefetchMaxMessageBytes`, this tool | **100 MiB** | shipped here (owner's instruction) |
| connector cap | `MAIL_MAX_MESSAGE_BYTES` on the `connector-google` CronJob | **currently UNSET → 1 MiB default** | a kube manifest change, NOT this ticket |

The connector cap exists to stop `raw_source_items` bloat across ~117k messages
(`imap.go:34-43`). At 100 MiB a single message can outweigh thousands of ordinary ones, which
is the wrong default for an always-on pass over a 106,930-message mailbox. The hand-run tool
is a different bargain: a human names a handful of messages and accepts their size.

**Recommendation for the always-on connector: `MAIL_MAX_MESSAGE_BYTES=26214400` (25 MiB)** —
above Gmail's ordinary 25 MB attachment ceiling for practical purposes, so most real client
attachments land on the first pass, while a pathological message is still capped. The value
is the owner's to choose at deploy. **The env change belongs to the kube session** (IK: "Kube
manifests belong to the kube session"); this ticket writes a handoff note, not a manifest.

Two cosmetic follow-ons of raising the connector cap, recorded so they are not read as bugs:
`truncatedReason()` prints the READER's cap, not the connector's (`attachments.go:76-83`, the
known SWT-42 gap), and `docs/runbooks/imap-mail-connector.md:81` plus
`internal/dashboard/templates/sources.html:69` name "1 MiB" in prose.

## Data model changes

**None.** No migration. No new table, no new column, no new index. The tool writes existing
columns of `raw_source_items` (`raw_json`, `content_hash`, `ingested_at`, `normalized_at`,
`superseded_at`) through the existing `Sink` methods, and one `sync_runs` row.

## API / MCP tool changes

**None.** No executor tool is added and nothing is exposed over MCP.

This is deliberate and is the invariant-3 answer, not an omission: `opsctl mail refetch` is a
CONNECTOR pass, not a tool action. Connector passes (`google`, `jira`, `upworkcrm`,
`slackweb`, and `opsctl capture-rules run` / `gate`, `opsctl ticket-status sync`) write
`raw_source_items` / `sync_runs` directly, exactly as `0015_capture_rules.sql:87-91` records
for capture: the executor path guards tools that act on `tasks`, `external_refs`,
`task_events` and `deliveries`, and this pass touches none of them. The `opsctl mail`
subcommand therefore follows `capture-rules run` (its own path, its own deadline) rather than
`capture-rules add` (a tool call through `run()`). If a future version ever creates a task or
a delivery, it goes through the executor.

## MQTT topics

**None.** The pass publishes nothing. In particular it does NOT call
`pipeline.AnnounceCaptured` — it commits no capture decision, and the wake-up contract is
"iff the pass committed at least one decision" (`cmd/connectors/google/main.go:224-227`).

## Files likely to touch

New:
- `internal/connector/google/refetch.go` — the pass. It must live in package `google`:
  `imapExternalID`, `buildIMAPEnvelope`, `imapRawEnvelope` and the `MessagePart` numbering are
  unexported there. Proposed surface:
  - `const RefetchMaxMessageBytes = 100 << 20`
  - `type RefetchQuery struct { AccountEmail, From string; Since, Until time.Time; RawIDs []int64; Limit int }`
  - `type RefetchTarget struct { RawID, AccountID int64; AccountEmail, ExternalID, Folder string; UIDValidity, UID uint32; StoredSize int; Truncated bool; Sender, Subject string; SentAt time.Time }`
  - `func SelectRefetchTargets(ctx context.Context, pool *pgxpool.Pool, q RefetchQuery) ([]RefetchTarget, error)`
    — pool-based, the `ListAppPasswordAccounts` / `DecryptAppPassword` precedent
    (`mailsender.go:252, 189`).
  - `func RefetchMessages(ctx context.Context, src MailSource, sink Sink, acct Account, targets []RefetchTarget, cfg RefetchConfig) (RefetchStats, error)`
    — takes the interfaces, so it is unit-testable against `fakeIMAP` + `fakeIMAPSink`.
- `internal/connector/google/refetch_test.go` — offline unit tests (fake source + fake sink).
- `internal/connector/google/refetch_integration_test.go` — build tag `integration`, fake
  IMAP source, real Postgres.
- `cmd/opsctl/mailrefetch.go` — flag parsing, pool, per-account loop, printing.
- `cmd/opsctl/mailrefetch_test.go` — flag-parsing unit tests (`gate_test.go` /
  `prreview_flags_test.go` shape).

Modified:
- `cmd/opsctl/main.go` — a `case "mail":` arm dispatching `refetch`, in the
  `capture-rules`/`ticket-status` two-level shape (`main.go:83-111`), plus the usage line at
  `main.go:37` and the package doc at `main.go:1-10`.
- `docs/runbooks/imap-mail-connector.md` — a "Recovering attachments on an over-cap message"
  section, and the cap table at line 81.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — one entry: the cap is a FETCH-time decision, so
  raising it does not repair already-stored rows; `opsctl mail refetch` is the repair; the
  UIDVALIDITY refusal is why it cannot fetch the wrong message.
- `docs/runbooks/HANDOFF-kube-mail-refetch.md` (new) — the `MAIL_MAX_MESSAGE_BYTES` env row
  for the kube session.

Explicitly NOT touched: `imap_ingest.go`, `imap.go` (no new interface method — criterion 21),
`normalize.go`, `sink.go`, any migration, any manifest.

## In scope / Out of scope

**In scope:** the `opsctl mail refetch` subcommand; the selection query; the UIDVALIDITY
refusal; the 100 MiB fetch; the raw-first upsert; `--dry-run`; the runbook and IK entries.

**Out of scope — do not bundle:**
- **Raising `MAIL_MAX_MESSAGE_BYTES` on the CronJob.** A manifest change; the kube session's.
  This ticket records the recommendation and the handoff.
- **Changing the connector's cap default** (`DefaultMaxMessageBytes`, `imap.go:43`). The env
  var is the knob; editing the constant changes every deployment silently.
- **Running `google.Normalize` from the tool.** D3 below.
- **A dashboard button or an MCP tool for refetching.** A hand-run recovery verb for
  Salvador; exposing it to sessions would put an outbound IMAP fetch behind model text.
- **Fixing `truncatedReason()` to print the CONNECTOR's cap** rather than the reader's (the
  SWT-42 known gap). Real, and unrelated to recovering bytes.
- **Backfilling every truncated row in the mailbox.** `SELECT count(*) FROM raw_source_items
  WHERE raw_json->>'truncated' = 'true'` is a number nobody has measured yet; a bulk sweep is
  a different ticket with a different bargain (and would want the connector cap raised first).
- **Attachment search / indexing / content_chunks.** Separate work entirely.

## Invariants that apply

1. **Raw-first.** The pass is a connector pass and nothing else: for each target it writes
   `raw_source_items` (raw envelope JSON + `content_hash`) through `Sink.InsertRaw` /
   `UpdateRaw` and stops. It performs NO normalization, NO extraction and no parse of the
   message beyond what `buildIMAPEnvelope` already stores. The write happens in
   `RefetchMessages` in `internal/connector/google/refetch.go`, calling the same
   `chash.ContentHash` → `RawHash` → `InsertRaw`/`UpdateRaw` sequence as `writeBatch`
   (`imap_ingest.go:176-205`). Because `normalized_at` is cleared, reprocessing is not merely
   possible, it is what happens next.
2. **One funnel.** No new table and no task-like structure. The recovered bytes reach the
   funnel through the existing `imap:` branch of `Normalize` (`normalize.go:312`) with the
   same `normalized_messages` row as before (criterion 15).
3. **Everything through the executor.** No tool is added; nothing an agent can call is added.
   The pass writes only connector-owned tables, the same standing this repo already gives
   `capture.EvaluateRules` and `ticketstatus`. Concretely: a review must confirm the new code
   contains no `INSERT INTO tasks`, `INSERT INTO task_events`, `INSERT INTO external_refs` or
   any `deliveries` write; if that ever becomes necessary, it goes through
   `executor.Execute`.
4. **Nothing external without a delivery row.** The pass sends nothing. It opens an IMAP
   connection that, by the interface's construction (`imap.go:20-32, 107-120`), has no verb
   that could alter a mailbox: SELECT is read-only and every fetch is `BODY.PEEK`. The one
   write against any mail server in this connector remains the SMTP submission in `smtp.go`,
   reachable only from an approved delivery row.
5. **Own-message loop closure.** A refetched Sent-folder message re-normalizes with the SAME
   `external_message_id` and the same `raw_source_item_id`, so it matches its delivery row
   exactly as before and can never be re-triaged into a new task (direction is recomputed
   from the own-email set, `rfc822.go:105-108`). The only new path is a SECOND pass through
   `confirmDelivery` / `confirmDeliveryByBodyPrefix`, which criterion 16 pins as idempotent
   via `confirmed_at IS NULL`.
6. **Stealth attribution.** Nothing client-visible is produced. Not applicable beyond the
   commit-message rule.
7. **Orchestrator purity.** The orchestrator is not touched and no LLM is called. The pass is
   deterministic: given the same targets and the same mailbox it writes the same bytes.

## Sibling patterns to copy

- **The per-account IMAP loop:** `runIMAPIngest` (`cmd/connectors/google/mailsource.go:65-134`)
  — `ListAppPasswordAccounts` → `LockAccount` → `DecryptAppPassword` →
  `NewIMAPClientSource` → pass → `Close()` → `release()`, with per-account errors recorded and
  the pass continuing. Copy that structure; do not invent a second credential path.
- **The raw-first write decision:** `writeBatch` (`imap_ingest.go:168-212`) and `upsertRaw`
  (`ingest.go:325-349`). Same hash-compare, same three outcomes, same counters.
- **The two-level opsctl verb with its own pool and a dry run:** `cmd/opsctl/gate.go:37-94`
  (`parseCaptureRulesGate` / `runCaptureRulesGate`), dispatched from `main.go:95-111`. Copy
  its shape: parse first, then open the pool, then branch on `--dry-run` before anything that
  writes.
- **Counters printed unconditionally, zeros included:** `printGateStats` (`gate.go:96`) and
  `printCaptureRules` (`cmd/connectors/google/main.go:254`) — "a pass that found nothing and
  a pass that never ran must not look the same".
- **Offline test doubles:** `fakeIMAP` and `fakeIMAPSink`
  (`internal/connector/google/fake_imap_test.go:185, 293`). `fakeIMAPSink` already records
  cursor saves and run phases, which is exactly what criteria 13 and 18 need.
- **Integration suite conventions:** `internal/connector/google/imap_integration_test.go:1-86`
  — build tag, `DATABASE_URL` skip, the `192.168.50.49` prod refusal, `itest-imap-%` scoping,
  rerunnable FK-ordered cleanup, and the cross-suite cleanup pact.
- **Isolated database, per branch:** IK landmine 2026-09-12. Run this branch's integration
  suite against its own database, never the shared compose `ops`:
  `psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE
  ops_mailrefetch"`, `make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_mailrefetch?sslmode=disable'`,
  then point `DATABASE_URL` there.

## Test plan

### Unit (no network, no Postgres) — `internal/connector/google/refetch_test.go`

U1. **UIDVALIDITY refusal.** `fakeIMAP` reports INBOX with UIDVALIDITY 99 and holds a
    DIFFERENT message at UID 7; the target says UIDVALIDITY 12, UID 7.
    Assert: zero `Fetch` calls for that UID, zero sink writes, `stats.UIDValidityChanged == 1`,
    and the refusal names the raw id.
U2. **Happy path.** Stored uidvalidity matches; the fake returns a full message.
    Assert: `Fetch` was called with `maxBytes == RefetchMaxMessageBytes`; the written
    `external_id` is byte-identical to the target's; the envelope has `truncated: false` and a
    non-empty `rfc822_b64`; `RawUpdated == 1`.
U3. **The cursor never moves.** `fakeIMAPSink` fails the test if `SaveCursor` or
    `SaveCursorField` is called during a refetch.
U4. **UID matching, not position.** The fake returns the requested UIDs out of order and omits
    one. Assert each envelope carries its own UID and the omitted one is counted `gone` with
    no write.
U5. **Folder not selectable.** A target in `[Gmail]/All Mail`; the source lists INBOX + Sent
    only. Assert refusal, no fetch.
U6. **Envelope / external_id disagreement** is refused.
U7. **Bounds** (`cmd/opsctl/mailrefetch_test.go`): missing `--limit`, `--limit 0`, both
    selector families, neither family, `--max-bytes 0`, `--max-bytes -1` all error, each with
    its own message.

### Integration (isolated Postgres, FAKE IMAP source — never prod, never the shared compose `ops`)

I1. **Selection reads the COLUMNS.** Seed two `itest-mailrefetch-%` app-password accounts with
    `imap:`-shaped raw rows (one truncated over-cap, one not) plus their
    `normalized_messages`. `SelectRefetchTargets` with `--from` returns the right rows with
    `Folder`, `UIDValidity`, `UID` read out of `raw_json`.
I2. **Identity survives the refetch** (criterion 15). Record
    `normalized_messages.id`, the `capture_decisions` row ids and `tasks` count; run the
    refetch; run `google.Normalize`; assert the `normalized_messages.id` is UNCHANGED, exactly
    one row for that raw id, the `capture_decisions` row still resolves, exactly one live
    decision, and the task count is unchanged.
I3. **No new raw row, no cursor movement** (criteria 12, 13). `count(*)` of
    `raw_source_items` and the exact `sync_cursor` JSON are identical before and after.
I4. **The `sync_runs` row** carries `stats->>'phase' = 'imap_refetch'` and status `ok`
    (criterion 18), and `internal/availability`'s calendar readiness is unaffected by it.
I5. **Outbound idempotence** (criterion 16). A Sent-folder raw row whose delivery is already
    `confirmed_at`-stamped: after refetch + normalize, no second `delivery_confirmed`
    `task_events` row.
I6. **`--dry-run` writes nothing**: raw rows, `sync_runs` count and `sync_cursor` all
    unchanged, and the plan lines are on stdout.

### Mutations that must turn a test red

(If a mutation stays green, the test is testing its own fixture — IK, "test the column, not
the fixture".)

M1. Drop the `stored.UIDValidity == live.UIDValidity` comparison → **U1 red** (a write lands
    for a message that is not the one stored).
M2. In `SelectRefetchTargets`, replace `raw_json->>'uidvalidity'` (or `->>'folder'`) with a
    literal → **I1 red**. This is the required column-level mutation: the predicate's input
    comes from a column, so the regression test belongs in the integration suite.
M3. Remove the `--limit` requirement → **U7 red**.
M4. Let `--dry-run` fall through to the write path → **I6 red**.
M5. Add a `SaveCursor` call after the write → **U3 red**, and **I3 red**.
M6. Match fetched messages by slice position instead of UID → **U4 red**.
M7. Pass `google.MaxMessageBytes()` instead of `RefetchMaxMessageBytes` to `Fetch` → **U2
    red** (the cap assertion), which is the whole point of the ticket.
M8. Change `upsertMessage`'s conflict target away from `raw_source_item_id` → **I2 red**
    (a duplicate normalized row, orphaned capture decision).

## Verification protocol (before commit)

1. `go test ./...` — green, including the existing
   `TestIMAPClientSource_UsesBodyPeekAndNoWriteVerbs`.
2. Integration on an ISOLATED database:
   ```
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_mailrefetch"
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_mailrefetch?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_mailrefetch?sslmode=disable' TZ=UTC \
     go test -tags integration -p 1 -count=1 ./internal/connector/google/ ./cmd/opsctl/
   ```
   Then the full suite the same way before delivery.
3. Run each mutation in M1-M8, confirm red, revert.
4. **Dry run against production, which writes nothing:**
   ```
   go install ./cmd/opsctl
   DATABASE_URL="$OPS_DATABASE_URL" OPS_TOKEN_KEY=... \
     opsctl mail refetch --from '@foundryunderwriting.com' --since 720h --limit 50 --dry-run
   ```
   Expect raw 73094 in the plan with `truncated=true`, its stored and live `uidvalidity`
   EQUAL, and the folder resolvable. If they are not equal, stop: the live run would refuse
   everything, and that is the tool working correctly.
5. **Snapshot the two things that must not change**, before the live run:
   ```
   psql "$OPS_DATABASE_URL" -c "SELECT sync_cursor FROM source_accounts WHERE id=1003" -At > /tmp/cursor.before
   psql "$OPS_DATABASE_URL" -c "SELECT count(*) FROM raw_source_items WHERE source_account_id=1003" -At
   psql "$OPS_DATABASE_URL" -c "SELECT id FROM normalized_messages WHERE raw_source_item_id=73094"
   psql "$OPS_DATABASE_URL" -c "SELECT id, mode, action, project_id FROM capture_decisions WHERE message_id=(SELECT id FROM normalized_messages WHERE raw_source_item_id=73094) ORDER BY id"
   ```
6. **Live run, smallest first:**
   `opsctl mail refetch --raw-id 73094 --limit 1`. Expect `raw_updated=1`,
   `uidvalidity_changed=0`, `gone=0`.
7. **Re-run the same four reads and diff.** `sync_cursor` byte-identical, raw count identical,
   `normalized_messages.id` identical, the same capture decision ids, still exactly one live
   decision. Also `SELECT count(*) FROM tasks` unchanged.
8. **The user-visible check:**
   ```
   opsctl call --tool mail_list_attachments --args '{"raw_source_item_id":73094}'
   ```
   Expect the 8 parts with `available: true` (the `..._Keystone_Standard_and_Map_v23_....docx`
   among them) and `truncated: false` on the source. Read one:
   `opsctl call --tool mail_read_attachment --args '{"raw_source_item_id":73094,"index":1}'`.
   Note this works BEFORE re-normalization: `mail_list_attachments` reads `raw_json` through a
   join on `raw_source_items` (`internal/tools/mailattach.go:190-197`), not the normalized
   body.
9. **Then the widened run:** `--from '@foundryunderwriting.com' --since 720h --limit 50`, and
   spot-check two more messages the same way.
10. **After the next `connector-google` pass** (or a hand-run `--normalize-only`), confirm the
    re-normalize: `normalized_at` is set again, `body_text` no longer carries the
    `[Attachments not stored: ...]` line (`rfc822.go:115-120, 362`), the
    `normalized_messages.id` is still the one from step 5, and no new task appeared.

## Rollback

**Say it plainly: the old `raw_json` is overwritten in place and there is no way back to it
from the database.** `raw_source_items` keeps no version history — `superseded_at`
(`migrations/0010_calendar_reset.sql:16`) is a tombstone flag for the Calendar replacement
semantics, not a version chain, and both `InsertRaw` and `UpdateRaw` replace `raw_json`
wholesale (`sink.go:145-171`).

That is acceptable **for the intended write**: the row being overwritten is a TRUNCATED
capture — headers plus one text part — and the replacement is the same message in full. The
new bytes strictly contain the old information. The only thing lost is the `parts` manifest
(now redundant, the real parts are present) and, after re-normalization, the
`[Attachments not stored: ...]` line appended to `body_text`. Nothing downstream keys on
either.

It is NOT acceptable for the wrong write, and there is no undo for it: fetching whatever now
sits at a stale UID and upserting it over a real message would destroy that message's stored
bytes with no recovery except another re-fetch — assuming anyone noticed. **That asymmetry is
why criterion 7 is a refusal and not a warning**, why it is pinned by mutation M1, and why the
pass never "resyncs" on a UIDVALIDITY mismatch.

If a run goes wrong in the recoverable direction (bytes fetched but something downstream looks
off), the repair is another refetch of the same targets, plus
`google --normalize-only --account <email>` to rebuild the normalized rows from raw. Both are
idempotent.

## Decisions made unilaterally

- **D1. Command name `opsctl mail refetch`** (two-level, like `capture-rules` and
  `ticket-status`), leaving room for future `mail` verbs without another top-level word.
- **D2. The pass lives in `internal/connector/google`, not in `cmd/opsctl`.** The envelope
  builder, the external-id spelling and the raw upsert are unexported package members; a copy
  in `cmd` would be a second spelling of `imap:{folder}:{uidvalidity}:{uid}` — precisely the
  SWT-13 landmine class. `cmd/opsctl` holds flags, the pool and printing only.
- **D3. The tool does not normalize.** `google.Normalize` is global over every pending google
  raw row (`normalize.go:293`, `pendingRaw`) — there is no per-row or per-account scoping — so
  calling it from a targeted recovery tool would drag in unrelated pending rows under a 30s-ish
  CLI deadline. Leaving `normalized_at = NULL` is the connector's own contract (invariant 1:
  ingest and normalize are separate phases), the next CronJob pass picks it up, and the
  attachments are readable immediately anyway (verification step 8). `google --normalize-only`
  remains the manual escape.
- **D4. The advisory lock is taken for live runs** (criterion 17) even though the pass writes
  no cursor. The hazard it closes is narrow but real: a concurrent `--full` or post-resync pass
  could re-fetch the same UID at the 1 MiB cap and overwrite the recovered bytes. A skip that
  says why is cheaper than a silent regression.
- **D5. `--dry-run` still contacts IMAP** (for live UIDVALIDITY) but issues no FETCH. A dry run
  that could not tell you "this target will be refused" would not be worth running.
- **D6. Refusals are outcomes, not errors.** A pass with refusals prints them and exits 0;
  only a transport or database failure exits non-zero. Rationale: a partial recovery is the
  normal case, and an exit code that cannot distinguish "3 of 8 messages have been re-filed by
  Gmail" from "the connection died" trains the operator to ignore it.
- **D7. `--from` matches `normalized_messages.sender`, not the raw headers.** It is the
  indexed, normalized field the rest of the repo searches on, and the finder in
  `mail_list_attachments` already reads it that way.

## Future work (not this ticket)

- A bulk `--all-truncated` sweep once the connector cap is raised, with a measured count of
  affected rows first.
- `truncatedReason()` printing the CONNECTOR's cap rather than the reader's, so a session that
  reads a truncated row is told the truth after the env changes (SWT-42 known gap).
- A truncated manifest that also lists the text/plain leaf kept as the body (the other SWT-42
  gap), which today hides a named `.txt` on an HTML-only oversize message.
- Recording, per raw row, the cap in force at capture time, so "was this truncated under the
  old cap?" is answerable without arithmetic on `size`.
