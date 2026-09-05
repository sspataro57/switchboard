> Jira: PENDING-SYNC

# pipedream-calendar — replace the calendar ingestion TRANSPORT with a polled Pipedream endpoint, keeping everything SWT-24 shipped

**Status: FINAL.** No open questions arose; the two forks that could have become
questions (the empty-snapshot handling and one-workflow-vs-three) are settled
below under "Decisions made unilaterally", each with the alternative it beat.
`docs/tickets/pipedream-calendar_OPEN_QUESTIONS.md` was deliberately NOT created.

## Source

Not a build-order step. Ad-hoc, from Salvador on 2026-09-04:

> the calendar integration is shitty, I would like to use Pipedream instead.

The pain is the operator half of SWT-24, not its code: a GCP project, a
calendar-only consent screen, a Desktop OAuth client, per-account browser
consent, and — unless the app is published to In Production — refresh tokens that
expire after 7 days (`docs/runbooks/calendar-availability.md` §"The one human
step"). Two of the three mailboxes are Workspace orgs Salvador does not
administer, so an admin can block the client id outright (same runbook, §"If a
Workspace admin blocks the consent"). Pipedream holds the Google OAuth grant and
manages refresh on its side; the cluster keeps no Google calendar credential at
all.

Salvador's own evaluation checklist (Jira SWT-24, comment 10316) is the frame
this SPEC answers against: **what transits a third party**, **read-vs-write
direction**, **failure mode**, **raw-first preservation**. Each is answered
explicitly below (§"What transits Pipedream", §"Invariants that apply").

The build-order text all of this still descends from is step 7's last clause —
"Availability service (free/busy merge + propose_slots — deterministic, no
LLM)" — and the policy-matrix row that gives it weight: "Calendar own blocks |
auto (always via availability service propose_slots)".

## Decided before this SPEC (not reopened here)

- **Full event objects transit Pipedream.** Salvador chose this explicitly on
  2026-09-05, having been offered the intervals-only alternative (the workflow
  returning `{start,end,busy}` triples and nothing else) with the privacy
  tradeoff spelled out. He wants the morning-brief / "what are my meetings
  today" door open, which needs titles and attendees. The consequence is
  recorded honestly in §"What transits Pipedream" — it is a real cost, not a
  footnote.
- **The connector POLLS.** A Pipedream HTTP-triggered workflow endpoint, called
  outbound from the cluster. No inbound ingress, no webhook receiver, nothing
  new exposed. (`cmd/hooksd` exists and is still not publicly exposed; this
  ticket does not change that.)
- **Everything transport-agnostic from SWT-24 stays**: fail-closed
  `LoadBusy`/readiness (`sync_runs.status='ok'` +
  `stats->>'phase'='calendar'` + freshness), the horizon constants, normalize
  dispatch on the `calendar:` external-id prefix, `sink.upsertEvent`,
  raw-first.

## Premises, verified before writing

Repo premises were read at the paths and line numbers given, on this branch
(`ticket-pipedream-calendar`, clean from main).

1. **Everything downstream of the raw row is already transport-agnostic.**
   `Normalize` dispatches on the `calendar:` external-id prefix
   (`internal/connector/google/normalize.go:329-336`) →
   `NormalizeCalendarEvent` → `sink.upsertEvent`
   (`internal/connector/google/sink.go:346-382`, upsert on
   `normalized_events_raw_item_idx`). Whatever puts `calendar:{id}` raw rows in
   front of it produces normalized events. **This ticket therefore changes a
   transport and nothing else.**
2. **The normalizer wants Google's event object, verbatim, and one bad item
   stalls the whole funnel.** `calEvent`
   (`internal/connector/google/normalize.go:209-222`) reads `id`, `status`,
   `summary`, `transparency`, `start.dateTime|date`, `end.*`, `attendees[]`.
   `Normalize` returns an error on the FIRST unparseable calendar item
   (`normalize.go:331-333`), and `cmd/connectors/google/main.go:181-185` returns
   on that error **before** `capture.ObserveOutbound` and `capture.EvaluateRules`
   ever run. So a single reshaped or truncated event object from a third party
   would stall mail normalization, outbound observation and the capture pass —
   and keep stalling every pass until the row is superseded by hand. **This is
   the sharpest new risk of a third-party transport and is why criterion 8
   validates every item BEFORE any raw row is written.**
3. **The readiness contract keys on the run, not the transport.**
   `availability.LoadBusy` (`internal/availability/store.go:38-64`) refuses
   before reading events; `loadAccountStates` (`store.go:71-101`) scopes to
   `provider='google' AND calendar_in_availability` and takes
   `MAX(finished_at)` over `sync_runs` rows with `status='ok' AND
   stats->>'phase'='calendar'`. Those rows are written by
   `PGSink.StartRun`/`FinishRun` (`sink.go:97-122`; `FinishRun` merges with
   `stats = stats || $3::jsonb`, so the `phase` key set at StartRun survives).
   **A new transport that calls StartRun("calendar")/FinishRun keeps readiness
   working with zero changes to `internal/availability`.**
4. **The horizon constants are exported and shared.**
   `google.CalendarWindowPast` = 30d / `CalendarWindowFuture` = 90d
   (`internal/connector/google/ingest.go:19-27`), passed into `LoadBusy` by
   `internal/tools/proposeslots.go:100-107`. The poll window must be computed
   from these same constants, never re-spelled.
5. **Snapshot replacement already exists and is already correct.**
   `PGSink.SupersedeAbsentCalendar` (`sink.go:459-515`) stamps every
   `calendar:%` observation of one account that is inside `[windowFrom,
   windowTo)` and absent from `keep`, then cancels the matching
   `normalized_events`, in one transaction. It **refuses an empty `keep`**
   (`sink.go:469-473`: "an empty replacement is indistinguishable from a broken
   leaf"). The bridge path calls it on a Google sync-token reset
   (`bridge_ingest.go:275-286`) bounded to `now±CalendarWindow*`. **A
   full-snapshot-every-poll transport is exactly the bridge's reset case, on
   every pass.**
6. **Raw-first uniqueness is per account, so `calendar:{id}` cannot collide
   across accounts.** `upsertRaw` (`ingest.go:316-340`) →
   `InsertRaw`/`UpdateRaw` (`sink.go:145-171`) key on
   `(source_account_id, external_id)`; `pendingRaw` (`sink.go:183-198`) and
   `loadEvents` (`availability/store.go:108-137`) both join back through
   `raw_source_items.source_account_id`. The same physical meeting present on two
   of the three calendars produces two raw rows, two events and two identical
   busy intervals, which `availability.Merge` coalesces
   (`availability.go:49-69`) — harmless. **The real attribution risk is not id
   collision, it is writing account A's events under account B's id, which is a
   new risk created by one response carrying three calendars; criterion 7 closes
   it.**
7. **The cursor is a shared blob and the calendar phase writes exactly one key.**
   `Cursor` holds `gmail_internal_date_ms`, `calendar_sync_token` and
   `imap_folders` (`ingest.go:53-62`); `IngestCalendar` writes only
   `calendar_sync_token` via `SaveCursorField` (`ingest.go:271-281`,
   `sink.go:442-457`) because the resident watch loop moves `imap_folders`
   underneath it. **The Pipedream path has no sync token and must write NO
   cursor at all**, which satisfies that constraint by construction.
8. **The current account selection is credential-gated and cannot work here.**
   `ListCalendarCredentialedAccounts`
   (`internal/connector/google/calendaraccounts.go:21-35`) requires
   `refresh_token_encrypted IS NOT NULL AND calendar.readonly = ANY(scopes)`.
   Under Pipedream the credential lives at Pipedream and our rows keep neither.
   A new selection is required; §"Decisions" explains why it is the availability
   scope.
9. **The phase wiring to extend.** `calendarPhaseRuns`
   (`cmd/connectors/google/calendarsource.go:19-21`) — imap only, because bridge
   and gmail_api ingest calendar inline and two passes would race the same cursor
   key. `runCalendarIngest` (`calendarsource.go:38-110`) — per-account
   `sink.LockAccount`, one failing account recorded without aborting the others,
   a pre-ingest factory failure recorded as an `error` run
   (`calendarsource.go:70-87`), zero credentialed accounts printing a line and
   exiting 0. Call sites: `cmd/connectors/google/main.go:100-112`
   (`--calendar-only`) and `main.go:174-178` (after mail, imap only).
10. **The HTTP-leaf client this repo already has.**
    `internal/connector/slackweb/http_bridge.go:41-120` — URL scheme/host and
    ≥32-char token validated at construction ("a misconfigured deployment fails
    at startup rather than mid-ingest"), `Authorization: Bearer`,
    `io.ReadAll(io.LimitReader(body, max+1))` so hitting the cap is
    distinguishable from ending there, a status error carrying a ≤200-char
    snippet, and a token read that prefers a file
    (`SLACK_WEB_BRIDGE_TOKEN_FILE`) so the secret can be mounted. **This is the
    file to copy.**
11. **The bridge's snapshot-integrity refusals are the second file to copy.**
    `runBridgeCalendarAccount` (`bridge_ingest.go:207-298`): schema-version
    check, export-identity check against the discovered account, a missing
    `next_sync_token` refusal, a `BridgeMaxEvents` cap, a per-event `id`
    requirement, and — in the Gmail half, `bridge_ingest.go:177-193` — the rule
    that an **absent** watermark field must be a refusal rather than a zero,
    because "an omitted field (decoding to zero) silently skips the check".
12. **Structural guards that must stay green and constrain the new files.**
    `internal/availability/callsites_test.go` bans the string
    `normalized_events` in non-comment lines anywhere under `internal/` or
    `cmd/` outside `availability/store.go` and `connector/google/sink.go`, and
    bans re-exporting `LoadEvents`. The new code touches neither.
    `cmd/connectors/google/calendarsource_test.go` scans `main.go` for the
    `--full` and `--calendar-only` usage strings — editing those strings must
    keep naming a mail source and the calendar phase.
13. **Integration-fixture pact.** Readiness scope is GLOBAL, so suites freshen
    every in-scope google calendar they do not own before asserting on
    `propose_slots` (`internal/connector/google/integration_test.go:303-316`),
    and `internal/availability/readiness_integration_test.go` /
    `cmd/connectors/google/calendarsource_integration_test.go` clean their
    fixtures before AND after, in FK order, under `make integration`'s `-p 1`.
    Note the tension this ticket inherits: the SWT-24 phase suite deliberately
    seeds `calendar_in_availability=FALSE` accounts to stay out of availability
    scope, while this ticket's selection IS that scope, so its fixtures must be
    `TRUE` and must be cleaned aggressively.
14. **Migration state.** Highest file is `migrations/0019_delivery_provenance.sql`.
    **This ticket adds no migration**, so the "merging a migration is not
    applying it" deploy hazard does not arise.
15. **Production premises are INHERITED, not re-measured.** This session had no
    shell, so `psql "$OPS_DATABASE_URL"` could not be run. As of SWT-24's
    measurement (2026-08-31, recorded in `.claude/INSTITUTIONAL_KNOWLEDGE.md`
    and `calendar-availability_SPEC.md`): three `provider='google'` rows, all
    `calendar_in_availability=true`, all `auth_type='app_password'` with NULL
    `refresh_token_encrypted` and empty `scopes`; `normalized_events` empty; no
    successful calendar `sync_runs` ever. **Re-measure at verification time
    (§"Verification protocol" step 4) — these are measurements, not constants.**

## Goal

Give the calendar phase a second, selectable transport — a polled Pipedream HTTP
workflow that returns full Google event objects for the three calendars — so
calendar ingestion can run with no Google credential in the cluster and no
consent dance, while every SWT-24 guarantee (raw-first, fail-closed readiness,
windowed replacement, cursor safety) holds unchanged and a Pipedream outage
leaves `propose_slots` refusing rather than answering from stale or fabricated
data.

**Usable alone** means: after this ticket, with `CAL_SOURCE=pipedream`,
`PIPEDREAM_CALENDAR_URL` and a token, `go run ./cmd/connectors/google
--calendar-only` performs a complete, audited calendar sync — raw rows,
normalized events, honest `sync_runs` — and `propose_slots` answers from real
busy time; with `CAL_SOURCE` unset the binary behaves byte-for-byte as SWT-24
shipped it. The whole thing is **deliverable, reviewable and mergeable against an
`httptest` fake of the Pipedream endpoint with zero real Pipedream credentials**;
the only thing waiting on a human is creating the workflow and clicking "Connect
account" three times, split out exactly as SWT-24 split out the OAuth consent.

## Acceptance criteria

### Transport selection and configuration

1. `selectCalendarSource(os.Getenv("CAL_SOURCE")) (calendarSource, error)` in
   `cmd/connectors/google/calendarsource.go` resolves `""` → `oauth`,
   `"oauth"` → `oauth`, `"pipedream"` → `pipedream`, and **any other value to an
   error naming the accepted values** — the `selectMailSource` shape
   (`mailsource.go:37-56`) and its argument verbatim: a fallback would turn
   `CAL_SOURCE=pipdream` into a connector that silently keeps doing the old
   thing and reports success. Unit test pins all four cases.
2. With `CAL_SOURCE` unset the pass is behaviourally identical to SWT-24: the
   OAuth phase runs at both call sites, `--calendar-only` on today's production
   prints `calendar: no google accounts with OAuth calendar credentials …` and
   exits 0, and **no Pipedream code executes** (no env read beyond `CAL_SOURCE`,
   no HTTP client constructed). The existing `calendarsource_test.go` and
   `calendarsource_integration_test.go` pass unedited.
3. The endpoint and its credential come from the environment only:
   `PIPEDREAM_CALENDAR_URL`, and `PIPEDREAM_CALENDAR_TOKEN_FILE` (preferred,
   mountable) falling back to `PIPEDREAM_CALENDAR_TOKEN`. **Never in
   `source_accounts`, never in a flag, never in a migration, never printed.**
   The constructor validates `http`/`https` scheme, non-empty host and a
   ≥32-character token and fails **before any `sync_runs` row exists** — a
   configuration error writes nothing at all, while a transport or protocol
   error (which means we did attempt a poll) writes per-account `error` runs
   (criterion 13). That line is drawn explicitly and tested on both sides.
4. **No secret reaches an error string or a log line.** A unit test points the
   client at an `httptest` server on a distinctive host, forces a 401, a 500, a
   malformed body and a dial failure, and asserts that no returned error
   contains the token or the URL's host/path. Status errors carry the HTTP
   status and a ≤200-character body snippet, the slackweb shape
   (`http_bridge.go:107-115`).

### The poll and its verification

5. One `POST` per pass to the single endpoint, `Authorization: Bearer <token>`,
   `Content-Type: application/json`, body:
   `{"schema_version":1,"time_min":…,"time_max":…,"calendars":[<in-scope emails>]}`.
   `time_min`/`time_max` are `cfg.now().Add(-google.CalendarWindowPast)` and
   `cfg.now().Add(google.CalendarWindowFuture)` in RFC3339 — **the constants,
   never re-spelled** (premise 4). The request carries no switchboard data
   beyond the account emails and that window.
6. **Whole-poll refusals, before any write, producing an `error` run for every
   in-scope account:** a non-200 status; an unreadable or non-JSON body; a body
   at or over `PipedreamMaxResponseBytes` (16 MiB, the read cap
   `calendar.go:97` already uses, detected with the `cap+1` LimitReader trick);
   an unknown `schema_version`; and — the sharpest one — a `time_min`/`time_max`
   echo that does not equal what we sent. **The echo check is what stops the
   snapshot model from destroying data**: a workflow that quietly returns "the
   next 7 days" while we supersede against a 120-day window would cancel every
   real event outside those 7 days on the first poll.
7. **Attribution is by identity, never by position.** Each response entry
   carries `calendar_id`; it is attributed to the in-scope account whose
   `account_email` equals it, case-insensitively. Index/order matching is
   forbidden. An in-scope account **absent** from the response gets a
   `status='error'` run (never an `ok` run, never silence). A returned calendar
   that matches no in-scope account is ignored, counted, and printed by name —
   it means the workflow is wired to an account switchboard does not know about.
8. **Per-entry integrity, checked before a single raw row is written for that
   account:** `status` must be `"ok"` (anything else → that account's run is
   `error`, carrying the entry's `error` string); `event_count` must be
   **present** (a pointer — absent is a refusal, not a zero, premise 11) and
   equal `len(events)`; `len(events)` must be below `PipedreamMaxEvents`
   (10 000, matching `BridgeMaxEvents`); every event must parse through
   `NormalizeCalendarEvent` (the same pure mapper the Normalize phase will run —
   one spelling of the shape, never an approximation of it) and must **not**
   carry a non-empty `recurrence` array, which is the proof the workflow queried
   with `singleEvents=true`. A single bad item fails that account's snapshot
   wholesale and writes nothing for it. Rationale is premise 2: a stored bad
   item stalls mail normalization, not just calendars.
9. **Raw-first.** Each event object is written **verbatim as received** under
   `external_id = "calendar:" + id` for that account's `source_account_id`
   through the existing `upsertRaw` → `InsertRaw`/`UpdateRaw` path — provider
   JSON plus `content_hash` in `raw_source_items` before anything normalizes.
   The new code contains **no** `normalized_events` statement and no
   `upsertEvent` call; `internal/availability/callsites_test.go` stays green
   unedited.
10. **Windowed replacement.** After the upserts for an account,
    `SupersedeAbsentCalendar(accountID, keep = every present external id,
    windowFrom = time_min, windowTo = time_max)` — the same bounds the request
    used. Every poll is a full snapshot, so every poll is the bridge's reset
    case (premise 5): an event deleted in Google is absent from the next
    snapshot, gets superseded, and its `normalized_events` row is cancelled, so
    it leaves the busy set.
11. **Empty snapshot: never call the supersede with an empty `keep`.** The sink
    refuses it by design (premise 5) and that refusal must not be reached. A
    verified entry with zero events finishes the run **`ok`** (we did look, and
    the answer was valid), sets `stats.calendar_empty_snapshot = 1`, and prints
    a line naming the account and how many live in-window observations were
    kept. Consequence, stated because it is real: if a calendar goes from
    populated to genuinely empty, its stale events keep contributing busy time —
    over-busy, the conservative direction, self-healing on the first non-empty
    snapshot. Tested both ways (empty-with-nothing-stored → clean `ok`;
    empty-with-stored-events → `ok` + the counter + the events still present).
12. **The Pipedream path writes no cursor.** It calls neither `SaveCursor` nor
    `SaveCursorField`. An integration test seeds a `sync_cursor` containing
    `imap_folders` and a `calendar_sync_token`, runs a full pass, and asserts
    the column is byte-identical afterwards.
13. **One `sync_runs` row per in-scope account per pass**, phase `"calendar"`,
    written through `StartRun`/`FinishRun`; `ok` **only** when that account's
    entry was verified, written and superseded. Every refusal path above lands
    as `status='error'` with the reason. An integration test drives a failing
    fake (503, then a missing account, then a bad item) and asserts that
    afterwards `availability.LoadBusy` — through
    `executor.Execute("propose_slots")`, so the refusal is audited — still
    refuses and names the account. **A Pipedream outage never produces an `ok`
    run.**
14. **Per-account advisory lock**, `sink.LockAccount`, taken **before**
    `StartRun` and released after `FinishRun`; a busy account increments
    `AccountsBusy` and is skipped with **no run row** (another pass is doing the
    same work). One failing account never aborts the others; the pass fails only
    if none succeeded — `runCalendarIngest`'s contract (premise 9), reused.
15. **Selection is the availability scope.**
    `google.ListAvailabilityScopeAccounts(ctx, pool, onlyEmail)` returns
    `provider='google' AND calendar_in_availability` rows (narrowed by
    `--account` for debugging only). Zero in-scope accounts prints a line and
    exits 0, writing nothing — a zero-work pass must never look like a working
    one. The Pipedream path uses only `Account.ID` and `Account.Email`; it reads
    no `auth_type`, no scopes, no encrypted anything.
16. **The polled set and the demanded set are proven equal on real Postgres.**
    An integration test seeds one google account with
    `calendar_in_availability=true` and one with `false`, and asserts: the
    poller requests and writes runs for exactly the `true` one; `LoadBusy`
    demands freshness for exactly the `true` one; the stale `false` one causes
    no refusal and contributes no busy interval. This is the column-fed
    regression test the institutional rule requires — a unit test cannot catch a
    predicate drift when the unit test is the thing supplying the value.
17. **Stats.** `Stats` gains `calendar_source` (string, `omitempty` —
    **diagnostic only**, so an operator can tell from `sync_runs.stats` which
    transport produced a run) and `calendar_empty_snapshot` (int, `omitempty`).
    A comment and this criterion state that **nothing may branch on either**:
    readiness keys on `status` and `stats->>'phase'` alone, unchanged, and
    discriminating run kinds by a stats payload is a landmine this repo has
    already paid for (IK: "One upworkcrm invocation writes TWO sync_runs rows").
18. `--calendar-only` under `CAL_SOURCE=pipedream` runs the Pipedream phase
    followed by `Normalize` and nothing else (criterion 20 of SWT-24, unchanged),
    and needs **neither `OPS_TOKEN_KEY` nor `GOOGLE_CLIENT_SECRET_FILE`** — a
    strictly smaller secret surface than the OAuth path.
19. The in-mail-pass call site (`main.go:174-178`, still gated on
    `calendarPhaseRuns(source)` — imap only) dispatches through the same source
    switch, so the two call sites can never disagree about which transport ran.
    `calendarPhaseRuns` itself is unchanged.
20. **No file under `internal/availability` changes.** `git diff --stat` shows
    none, and the SWT-24 readiness suites (`availability_test.go`,
    `readiness_test.go`, `readiness_integration_test.go`, `callsites_test.go`)
    pass untouched. The readiness contract is inherited, not re-implemented.
21. **Every test runs against an `httptest` fake with zero Pipedream
    credentials and zero network.** No test may point a fake endpoint at
    `OPS_DATABASE_URL` — fabricated calendar data must never reach production
    (called out because the fake is trivially runnable against any DSN).
22. `docs/runbooks/calendar-availability.md` gains a Pipedream section: the exact
    workflow shape (trigger, custom response, three connected Google accounts,
    `singleEvents=true`, paging, the response envelope), the human step, how to
    create the secret, the CronJob handoff for the kube session, the
    poll-cadence ↔ `AVAIL_MAX_SYNC_AGE` ↔ Pipedream-invocation coupling, and the
    privacy consequence of full event objects transiting a third party.
23. The same runbook records that the SWT-24 OAuth path is **kept and dormant**,
    and that `CAL_SOURCE=oauth` (or unsetting it) is the way back with no code
    change.

## Data model changes

**None. No migration.** Highest file stays
`migrations/0019_delivery_provenance.sql`.

Every input already exists and is already written by shipped code:

| need | column / table | written by |
|---|---|---|
| which calendars must be polled and must be fresh | `source_accounts.calendar_in_availability` (0001:19) | operator `UPDATE`, `google-auth` |
| when we last looked at a calendar | `sync_runs.finished_at` + `status` + `stats->>'phase'` | `PGSink.StartRun`/`FinishRun` |
| the provider JSON | `raw_source_items` (raw + `content_hash`) | `sink.InsertRaw`/`UpdateRaw` |
| the busy set | `normalized_events` | `sink.upsertEvent` (unchanged, still the only writer) |
| deletions | `raw_source_items.superseded_at` + `normalized_events.status` | `SupersedeAbsentCalendar` (unchanged) |

Deliberately NOT added: a `calendar_source` column on `source_accounts`. The
transport is a deployment property, not a per-account fact — putting it in a row
would let the code path change with no manifest diff, nothing to grep and no log
line, which is precisely the argument `selectMailSource` already makes
(`mailsource.go:22-36`). Also NOT added: any table holding Pipedream state; the
endpoint is stateless from our side, by design.

## API / MCP tool changes

**None.** No new executor tool, no MCP surface change, no policy change.
`propose_slots` keeps its name, args, registration
(`internal/tools/createtask.go:56`), off-MCP status and its SWT-24 behaviour
verbatim — including the refusal text, which is produced by
`availability.NotReadyError` and is not touched here.

The only new external interface is the Pipedream endpoint contract, which is a
LEAF contract and not part of switchboard's API surface:

```
POST {PIPEDREAM_CALENDAR_URL}
Authorization: Bearer {PIPEDREAM_CALENDAR_TOKEN}
{ "schema_version": 1,
  "time_min": "2026-08-06T09:00:00Z",
  "time_max": "2026-12-04T09:00:00Z",
  "calendars": ["a@example.com","b@example.com","c@example.com"] }

200
{ "schema_version": 1,
  "time_min": "2026-08-06T09:00:00Z",     // echoed; must equal the request
  "time_max": "2026-12-04T09:00:00Z",
  "calendars": [
    { "calendar_id": "a@example.com", "status": "ok", "event_count": 37,
      "events": [ <google calendar v3 event objects, verbatim> ] },
    { "calendar_id": "b@example.com", "status": "error",
      "error": "invalid_grant: token has been expired or revoked" }
  ] }
```

Non-200, or any violation of criteria 6-8, is a refusal; nothing is written for
the affected account and its `sync_runs` row says `error`.

**Executor relationship (invariant 3).** This is connector code, not a tool:
like every other connector it writes `raw_source_items`/`sync_runs` through its
own sink and reaches the executor only for task-shaped side effects (the
capture-rules pass in the full mail run, `main.go:203-211`, untouched here).
The one audited surface that consumes this data — `propose_slots` — keeps going
through validate → policy → audit start → handler → audit complete, and this
ticket does not add a second door to it.

## MQTT topics

None. Nothing here publishes or subscribes.

## What transits Pipedream (Salvador's checklist, answered)

- **What transits.** Outbound from us: the three account email addresses, a time
  window, and the bearer token. Nothing about tasks, deliveries, messages or
  clients. Inbound to us: **complete Google Calendar v3 event objects** for
  three mailboxes over a 120-day window — titles, descriptions, locations,
  conference links, organizer and every attendee's email address, including
  client meetings. Chosen deliberately (§"Decided before this SPEC") so a
  morning brief can say what the meetings ARE, not merely when we are busy.
- **The honest cost.** Those payloads are visible in Pipedream's execution log
  UI and retained under Pipedream's retention policy. Anyone with access to that
  Pipedream account — or anyone who compromises it — can read the recent
  calendars of all three mailboxes without touching Google. The intervals-only
  alternative was offered and declined; if that judgement changes, the change is
  a workflow edit plus stricter ingest validation, not a schema change.
- **Direction.** Read-only. The workflow calls `calendar.events.list` and
  nothing else; connect the Google accounts with a read scope where Pipedream
  permits it. Switchboard issues no calendar write on any transport: `calendar`
  remains `channel_not_live` in the policy matrix
  (`internal/policy/matrix.go:155-158`), and a calendar write would still need a
  `deliveries` row (invariant 4).
- **Failure mode.** Pipedream down, rate-limited, workflow deleted, token
  rotated, Google connection revoked → non-200 or a per-entry `error` → `error`
  `sync_runs` rows, no `ok` row → after `AVAIL_MAX_SYNC_AGE` (1h)
  `propose_slots` refuses by name and the refusal is audited. Nothing else in
  switchboard degrades: mail is IMAP and never touches this path.
- **Raw-first preservation.** Preserved, with one honest caveat that a third
  party makes unavoidable: the raw row is the provider JSON **as the third party
  handed it to us**, and we cannot prove Pipedream did not alter it. What
  mitigates that is refusing anything we can check — the window echo, the
  calendar-id identity, the declared count, the `recurrence` absence, and the
  full parse through the same mapper Normalize will use — plus the fact that
  Pipedream never chooses the window, the account set, or which events count.

## Files likely to touch

**New**
- `internal/connector/google/pipedream.go` — `PipedreamCalendarClient` (URL and
  token validation, Bearer, capped read, status error with snippet),
  `TokenFromEnv`-style reader, request/response envelope types,
  `PipedreamCalendarSchemaVersion`, `PipedreamMaxEvents`,
  `PipedreamMaxResponseBytes`, the request timeout constant.
- `internal/connector/google/pipedream_ingest.go` — `RunPipedreamCalendar(ctx,
  source PipedreamCalendarSource, sink CalendarSnapshotSink, accounts
  []Account, cfg Config) (Stats, error)`: one call, envelope verification,
  per-account attribution, validation, lock, `StartRun` → upserts → supersede →
  `FinishRun`. `CalendarSnapshotSink` = `Sink` + `LockAccount` +
  `SupersedeAbsentCalendar` (no cursor methods needed — criterion 12 is true by
  construction of the interface).
- `internal/connector/google/pipedream_test.go` — `httptest` fake: happy path,
  401/500/dial failure, oversize body, secret-redaction assertions.
- `internal/connector/google/pipedream_ingest_test.go` — fake sink (the
  `bridgeFixtureSink` shape, `bridge_ingest_test.go:477`): window-echo mismatch,
  wrong `calendar_id`, missing account, absent `event_count`, count mismatch,
  cap, `recurrence` present, unparseable event, empty snapshot both ways.
- `internal/connector/google/pipedream_integration_test.go` (build tag
  `integration`) — `sync_runs` honesty per account, supersede-on-deletion,
  cursor byte-identity, the outage → `propose_slots` refusal chain, criterion 16's
  scope equality.

**Modified**
- `internal/connector/google/ingest.go` — two `Stats` fields (+ `add`).
- `internal/connector/google/calendaraccounts.go` —
  `ListAvailabilityScopeAccounts`, beside `ListCalendarCredentialedAccounts`,
  each doc-commented to point at the other and at the equality test.
- `cmd/connectors/google/calendarsource.go` — `selectCalendarSource`,
  `runCalendarPhase` dispatch, `runPipedreamCalendarIngest` (selection, client
  construction, printing).
- `cmd/connectors/google/calendarsource_test.go` — the `CAL_SOURCE` table.
- `cmd/connectors/google/main.go` — dispatch at both call sites, env list in the
  package doc comment, `--calendar-only` usage string kept truthful (the
  existing scan test reads it).
- `docs/runbooks/calendar-availability.md` — the Pipedream section.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — at delivery, an entry under "Calendar &
  availability" recording CAL_SOURCE, the snapshot/supersede coupling, the
  empty-snapshot behaviour and the scope-equality rule.

## In scope / out of scope

**In scope**
- A second calendar transport selected by `CAL_SOURCE`, defaulting to today's
  behaviour.
- The polled envelope, its refusals, per-account attribution and honest
  `sync_runs`.
- Snapshot-as-replacement wiring onto the existing `SupersedeAbsentCalendar`.
- The account-selection change to the availability scope, and the test proving
  the polled set equals the demanded set.
- Runbook: workflow shape, human step, secret, cadence, kube handoff, privacy.

**Out of scope — including what it is tempting to bundle**
- **Removing the SWT-24 OAuth path.** Kept dormant (§"Decisions").
- **Creating the Pipedream workflow and connecting the three Google accounts.**
  That is the human step, split out exactly as SWT-24 split out the consent; the
  ticket is deliverable, reviewable and mergeable without it.
- **Kube manifests.** The CronJob and the `switchboard-pipedream` secret belong
  to the kube session and the sibling repo (`~/projects/personal/kube/
  switchboard/`); this delivery ends with a handoff note in the runbook. Two
  sessions cross-committing in one repo is a recorded mistake.
- **A calendar sweep inside the resident watch loop.** SWT-24 put it out of
  scope for a reason that has not changed: making the one long-running ingest
  process periodic on a second axis does not belong in a transport diff. The
  CronJob calling `--calendar-only` is the shape.
- **Gmail over Pipedream.** Mail is IMAP and works; moving it would touch
  `body_text` and therefore `confirmDeliveryByBodyPrefix`, which google has no
  reconciler for (STANDING RULE, SWT-25). Not here, not by accident.
- **Calendar WRITE of any kind** (own blocks, invites, `draft_delivery(channel=
  'calendar')`). Still `channel_not_live`.
- **Changing the readiness contract, `AVAIL_MAX_SYNC_AGE` semantics, the
  horizon constants, `propose_slots`' shape or its refusal text.**
- **Dashboard surfacing of calendar staleness**, and the morning-brief meetings
  section that full event objects now make possible. Future work.
- **Non-google calendars, CalDAV, ICS.** Unchanged; CalDAV stays closed by
  measurement.

## Invariants that apply

1. **Raw-first.** The new transport's ONLY write into the funnel is
   `upsertRaw(ctx, sink, acct.ID, "calendar:"+id, eventJSON, &stats)` in
   `pipedream_ingest.go` → `sink.InsertRaw`/`UpdateRaw` — provider JSON plus
   `content_hash` in `raw_source_items` **before** `Normalize` runs, exactly as
   `IngestCalendar` and `runBridgeCalendarAccount` do. No new code writes
   `normalized_events` (criterion 9), and reprocessing stays possible:
   `--normalize-only --all` rebuilds events from raw with no Pipedream call.
   Caveat recorded above: raw is what the third party handed us, which is why
   criteria 6-8 refuse everything checkable before it becomes raw.
2. **One funnel.** No new table, no sibling of `sync_runs`, no
   `calendar_health`. Calendar freshness stays derived from `sync_runs`;
   calendar events stay `normalized_events`. No task is created by this ticket.
3. **Everything through the executor.** No new tool and no new handler. The one
   audited consumer, `propose_slots`, is unchanged and keeps its single door
   (`LoadBusy`); the structural ban on a second `normalized_events` reader stays
   green with the new files inside its scan scope.
4. **Nothing external without a delivery row.** This ticket reads and writes
   nothing to Google or to any client surface. The only outbound network call is
   a GET-shaped POST to our own workflow carrying a window and three email
   addresses. `calendar` stays `channel_not_live`; no `deliveries` row is
   created, read or modified.
5. **Own-message loop closure.** No message path is touched. Two concrete
   demands: the Pipedream path writes **no cursor field at all** (criterion 12),
   so it cannot roll back `imap_folders` and cause skipped mail — a skipped mail
   is a delivery confirmation that never lands; and nothing in this diff may
   alter `body_text` or `confirmDeliveryByBodyPrefix` (google has no
   reconciler). Also premise 2: a malformed calendar item that reaches raw would
   stall `Normalize`, which is the pass that stamps delivery confirmations —
   another reason criterion 8 validates before writing.
6. **Stealth attribution.** No client-visible output. Recorded so the reviewer
   does not go looking. (Commits carry no AI reference, as always.)
7. **Orchestrator purity.** The orchestrator is untouched, imports no provider
   adapter and calls no LLM. `internal/availability` stays pure and unchanged
   (criterion 20); no LLM is anywhere near this ticket.

## Sibling patterns to copy

- **The HTTP leaf client:** `internal/connector/slackweb/http_bridge.go:41-120`
  — construction-time URL/token validation, Bearer header, `LimitReader(max+1)`
  so the cap is detectable, a typed status error with a truncated snippet, and
  `TokenFromEnv` preferring `*_TOKEN_FILE`. Copy its structure; do **not** copy
  its send/ambiguity typing (`SendRejectedError`), which exists because a click
  may have landed — a read poll has no such hazard.
- **Snapshot integrity refusals:** `internal/connector/google/bridge_ingest.go:207-298`
  (`runBridgeCalendarAccount`) — identity check against the account we asked
  for, schema version, event cap, per-event id, supersede-before-cursor
  ordering; and `bridge_ingest.go:177-193` for the rule that an **absent**
  declared field is a refusal, not a zero.
- **The windowed replacement itself:** `internal/connector/google/sink.go:459-515`,
  including why an empty `keep` is refused.
- **Explicit source selection with an error on the unknown value, and the
  per-account loop:** `cmd/connectors/google/mailsource.go:37-56` and `:65-134`
  (advisory lock, continue-on-error, "no accounts" line), plus
  `cmd/connectors/google/calendarsource.go:38-110` for the pre-ingest failure
  that still records an `error` run.
- **Fail closed at a boundary with a named unavailable state:**
  `internal/provider/router.go:131-172` — the comment explaining why "it's
  local, assume it works" was rejected is the same argument as "the response was
  empty, assume the calendar is".
- **Test shapes:** `cmd/connectors/google/calendarsource_integration_test.go`
  (httptest + injected factory + fixture cleanup in FK order),
  `internal/connector/google/bridge_ingest_test.go:477` (`bridgeFixtureSink`),
  `internal/connector/google/integration_test.go:303-316` (the freshen-foreign-
  calendars pact every propose_slots assertion joins).
- **Queue/lock idiom:** unchanged — `pg_try_advisory_lock` per account via
  `sink.LockAccount` (`sink.go:396-440`), the jobagent lineage.

## Verification protocol

Nothing in steps 1-4 needs a Pipedream credential or network access.

1. `go test ./...` — source selection table, envelope validation table,
   attribution/refusal cases against the fake sink, secret-redaction assertions,
   and the untouched SWT-24 unit suites (`availability`, `callsites_test.go`,
   `calendarsource_test.go`).
2. `make integration` (db-up + migrate + `go test -tags integration ./...`,
   serialized `-p 1`). The new suite seeds **in-scope** (`calendar_in_availability
   = true`) accounts, which every `propose_slots` assertion in the repo can see,
   so it must clean before AND after in FK order and join the freshen pact
   (premise 13).
3. **Mutation checks — each must turn a test red, verified by hand:**
   - delete the `time_min`/`time_max` echo comparison (criterion 6) → the test
     that feeds a 7-day snapshot against a 120-day window must fail, proving it
     would have cancelled 113 days of real events;
   - replace the `calendar_id` ↔ `account_email` match with positional
     assignment (criterion 7) → the cross-attribution test goes red;
   - make an in-scope account absent from the response finish `ok` instead of
     `error` (criterion 7/13) → the outage → refusal chain goes green-when-it-
     should-refuse, i.e. the test fails;
   - change `event_count` handling from a pointer to a plain int (criterion 8) →
     the absent-count test goes red;
   - swap `ListAvailabilityScopeAccounts`' predicate for
     `auth_type='app_password'` (criterion 15/16) → the scope-equality
     integration test goes red;
   - drop the `recurrence` refusal (criterion 8) → the unexpanded-series test
     goes red.
4. **Production smoke, read-only, no Pipedream credential** (`OPS_DATABASE_URL`
   lives in `~/.bashrc`, which early-exits for non-interactive shells — grep/eval
   it, don't source it). Re-measure premise 15 here; the numbers above are
   SWT-24's:
   - `psql "$OPS_DATABASE_URL" -c "SELECT id, account_email, auth_type, calendar_in_availability, refresh_token_encrypted IS NOT NULL AS has_token FROM source_accounts WHERE provider='google' ORDER BY id"`
   - `psql "$OPS_DATABASE_URL" -c "SELECT count(*) FROM normalized_events"`
   - `psql "$OPS_DATABASE_URL" -c "SELECT count(*), max(finished_at) FROM sync_runs WHERE status='ok' AND stats->>'phase'='calendar'"`
   - `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google --calendar-only`
     with `CAL_SOURCE` **unset** — must print the SWT-24 "no OAuth calendar
     credentials" line and exit 0; `SELECT count(*) FROM sync_runs` unchanged
     (criterion 2).
   - `CAL_SOURCE=pipedream DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google --calendar-only`
     with no URL set — must fail immediately with a configuration error and
     `SELECT count(*) FROM sync_runs` unchanged (criterion 3).
   - `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool propose_slots --args '{"duration_minutes":30}'`
     — must still refuse, naming the three accounts, and
     `SELECT tool, status, left(error,160) FROM audit_events WHERE tool='propose_slots' ORDER BY id DESC LIMIT 1`
     must read `error`. Nothing in this ticket makes it answer before real data
     exists.
   - **Never point the httptest fake at `OPS_DATABASE_URL`.**
5. **"Usable alone" check:** with `CAL_SOURCE` unset nothing changed
   (`sync_runs`, `raw_source_items`, `normalized_events`, `tasks` and
   `deliveries` counts identical across the smoke); with `CAL_SOURCE=pipedream`
   and a fake endpoint against the LOCAL test db, one pass produces three `ok`
   runs, raw rows, normalized events and a `propose_slots` answer.
6. **Not part of delivery — the human step** (runbook §Pipedream): create the
   workflow, connect the three Google accounts, set the shared secret, `curl`
   the endpoint once and time it, create the k8s secret and hand the CronJob to
   the kube session. Then, once: run `--calendar-only` against production with
   the real endpoint, check three `ok` calendar runs and `count(*) FROM
   normalized_events > 0`, and re-run the `opsctl call` above to watch it answer
   with real busy time. Capture the before/after of that command in the delivery
   note — that diff is the ticket.

## Decisions made unilaterally

- **One workflow, one endpoint, one call per poll — not three.** The response is
  keyed by `calendar_id` and the connector iterates ITS OWN in-scope list, so
  per-account honesty is a property of how the response is mapped, not of how
  many endpoints exist (criterion 7 makes a missing account an `error` run
  either way). What one endpoint buys: one human step instead of three, one
  secret instead of a per-account URL map that can drift, and roughly a third of
  the Pipedream invocations — at one poll per 20 minutes that is ~72
  invocations/day instead of ~216, which is the difference between comfortably
  inside a free tier and not. The cost — a single slow response covering three
  calendars — is bounded by the workflow doing three `events.list` calls, well
  inside Pipedream's custom-response deadline; the runbook makes timing that
  `curl` part of the human step, and the request already carries a `calendars`
  list, so narrowing a poll to one calendar is a config-shaped change if it ever
  bites.
- **The SWT-24 OAuth path is kept, dormant.** It works if consent is ever
  granted, it is fully tested, and it is the fallback if Pipedream is dropped —
  `CAL_SOURCE=oauth` and nothing else. Deleting it would churn `oauth.go`,
  `calendar.go`, `google-auth add-calendar`, `calendaraccounts.go` and two test
  suites to buy tidiness. The honest cost of keeping it, recorded: there are now
  two calendar transports, and "which one ran" is answered by `CAL_SOURCE` in
  the manifest plus `stats->>'calendar_source'` on the run.
- **Selection is the availability scope, not a new flag or an env list.** The
  set we poll is then, by construction, the set readiness demands: there is no
  configuration in which `propose_slots` requires a calendar the poller never
  asks for, and the operator has ONE lever (`calendar_in_availability`) that
  moves both sides together — the same lever SWT-24's "operator branch" already
  documents. Rejected: a `source_accounts.calendar_source` column (a migration,
  and it hides the transport from the manifest); an env list of emails (drifts
  from the DB silently); polling every `provider='google'` row (perpetual error
  runs for accounts nobody wants a calendar for). Note the fixture consequence
  this creates and criterion 2/premise 13 handle: the new integration fixtures
  are in availability scope while they exist.
- **The two scope predicates are proven equal by an integration test, not
  shared as a string constant.** A shared const would cover only the
  `calendar_in_availability` half — the `provider='google'` half already lives
  inside `accountSelect`'s WHERE clause — and a predicate that looks shared but
  is half-restated is this repo's recurring defect in a new costume. A test that
  makes Postgres produce both sets and asserts they are the same set is stronger
  and is the form the institutional rule demands for column-fed predicates.
- **An empty verified snapshot finishes the run `ok`, keeps the stale events,
  and never calls the supersede.** The sink refuses an empty `keep` by design,
  so the alternatives were: (a) let the refusal error the run — a genuinely
  empty calendar would then refuse forever and take the whole service down with
  it, since one unready account refuses `propose_slots` for everyone; (b) finish
  `ok` and keep what we hold. `ok` is honest — the run means "we looked and got
  a valid answer", which is true — and the residual error is over-busy (refusing
  slots that are actually free), the conservative direction, self-healing on the
  first non-empty snapshot. It is counted (`calendar_empty_snapshot`) and
  printed rather than silent. This is the one place the design accepts stale
  data, and it accepts it in the direction that cannot book over a meeting.
- **A configuration error writes no `sync_runs` rows; a transport error writes
  per-account `error` rows.** The distinction is "did we attempt a poll".
  An absent run and an error run both keep `propose_slots` refusing after an
  hour, but an error row says WHY and a missing URL is not a calendar fact.
- **Every poll is a full snapshot and therefore always a replacement.**
  Pipedream has no sync-token increment to offer, and asking the workflow to
  carry our token would put cursor state in a third party. `CalendarResets` is
  deliberately NOT incremented per poll — it means "Google dropped our token"
  and would become meaningless; `CalendarSuperseded` carries the meaningful
  count. The cost of full snapshots — re-transferring 120 days of events every
  20 minutes — is paid in bandwidth and in Pipedream compute, not in database
  churn: `upsertRaw`'s `content_hash` short-circuit means an unchanged event
  writes nothing.
- **`--full` means nothing new on this transport** (there is no token to drop)
  and is left alone; the existing usage string keeps naming its modes, and the
  scan test keeps checking that.
- **The in-mail-pass calendar call stays** (imap only, unchanged from SWT-24)
  rather than being restricted to `--calendar-only`. The runbook notes the one
  operational consequence: if both the mail one-shot and the dedicated calendar
  CronJob run with `CAL_SOURCE=pipedream`, invocations double — pick one, and
  prefer the dedicated CronJob since production mail runs in the resident watch
  loop, which has no calendar phase.

## Deploy (kube session handoff — not this delivery)

Recorded here so the runbook section has a source; the manifests belong to
`~/projects/personal/kube/switchboard/`.

- CronJob `connector-gcal`, image `switchboard:<pinned tag>`, command
  `[/usr/local/bin/google]`, args `[--calendar-only]`, schedule `*/20`.
- Env: `CAL_SOURCE=pipedream`, `DATABASE_URL` from `switchboard-db`,
  `PIPEDREAM_CALENDAR_URL` + `PIPEDREAM_CALENDAR_TOKEN_FILE` from a new
  out-of-band secret `switchboard-pipedream`, created with `--from-file` per the
  recorded `--from-literal` landmine. **No `OPS_TOKEN_KEY`, no client secret
  file** — this path decrypts nothing.
- Cadence coupling, one rule: the poll period must be less than half
  `AVAIL_MAX_SYNC_AGE` (default 1h), so `*/20` is the floor for the default and
  moving to `*/30` requires raising `AVAIL_MAX_SYNC_AGE` to at least 90m.
  Pipedream's free-tier daily invocation/credit allowance must be checked
  against the resulting rate (~72/day at `*/20`) **before** scheduling — that
  check is a human step, not a code assumption.
- Migrations: none, so no migrate Job is required for this ticket. The standing
  rule still applies before pushing any image — compare
  `SELECT max(version) FROM schema_migrations` against `ls migrations/`.

## Future work (not this ticket)

- Morning-brief meetings section — deterministic SQL over `normalized_events`,
  no LLM (R7's shape). This is the door full event objects were chosen to open.
- Dashboard: per-account calendar freshness on `/sources`, so a refusal is
  visible before someone hits it.
- Calendar WRITE: `propose_slots` → `draft_delivery(channel='calendar')` → an
  insert adapter, own blocks at the auto tier, invites at approve, with
  `sent_external_id` = the event id. Needs `channel_not_live` lifted and a rate
  limit — and, on this transport, a Pipedream workflow that writes, which
  reverses the read-only property this ticket relies on. Decide deliberately.
- A calendar sweep in the resident watch loop, if the CronJob proves an awkward
  fit.
- Narrowing what transits: an intervals-only workflow mode, if the privacy
  judgement changes.
- Extending readiness to a second calendar provider, at which point
  `provider='google'` becomes a capability lookup rather than a literal.
