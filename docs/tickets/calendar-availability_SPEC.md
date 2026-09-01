> Jira: SWT-24

# calendar-availability — make the free/busy service refuse when it does not know, and give calendar ingestion a path that can actually run

**Status: FINAL.** No open questions arose; the one fork that survives
(`docs/tickets/calendar-availability_OPEN_QUESTIONS.md` was NOT created) is an
operator branch that changes no code and is recorded below under "Operator
branch, decided at consent time".

## Source

Not a build-order step. Ad-hoc, from the SWT-24 issue (Salvador, 2026-08-28),
which describes two separable problems. The build-order text it descends from is
step 7's last clause:

> 7. Gmail ingestion over **IMAP with app passwords** … Availability service
>    (free/busy merge + propose_slots — deterministic, no LLM).

and the policy-matrix row that gives the service its safety weight:

> | Calendar own blocks | auto (always via availability service propose_slots) |

The issue, condensed:

> **PROBLEM 1** — nothing populates `normalized_events`, because calendar needs
> OAuth. Gmail ingestion was replaced with IMAP + app passwords; calendar was
> not.
>
> **PROBLEM 2** — the availability service fails OPEN, independent of auth.
> `LoadEvents` reads an empty table, so `ProposeSlots` receives an empty busy set
> and proposes EVERY slot as free. All three accounts have
> `calendar_in_availability = true`, so it reads as a safety control while
> controlling nothing. FIX THIS HALF REGARDLESS: an empty busy set is "I do not
> know", not "you are free" — the same fail-closed rule SWT-21 applied to the
> provider boundary.

The CalDAV fork is closed by measurement, not argument (main session,
2026-08-31): a PROPFIND to `https://apidata.googleusercontent.com/caldav/v2/
sspataro@gmail.com/events/` with a **valid decrypted app password** returns HTTP
401, byte-identical to the control run with a garbage password; `/user/` returns
405. Google CalDAV v2 does not accept app passwords. ICS secret addresses are
read-only and Google-cached hours stale, and the policy matrix needs a WRITE path
eventually anyway. **OAuth is the only calendar path.**

## Premises, verified before writing

Repo premises were read at the paths given. Production premises are the main
session's measurements of 2026-08-31 and are marked as such — re-measure at
verification time rather than trusting the number.

1. **The availability service reads a table nothing writes.**
   `internal/availability/store.go:14-43` — `LoadEvents` joins
   `normalized_events → raw_source_items → source_accounts`, filtered
   `provider='google'`, `superseded_at IS NULL`, window-overlapping. It returns
   `[]Event` carrying `InAvailability`; `availability.Busy`
   (`availability.go:33-45`) drops anything not in availability, cancelled or
   transparent. **Measured (production, 2026-08-31): `normalized_events` has 0
   rows.** So `Busy` returns nil, `ProposeSlots(nil, cfg)` proposes every aligned
   slot inside working hours, and `propose_slots` answers "you are free all week"
   with total confidence.
2. **The flag is a constant discriminator over an empty table.** Measured: all
   three `provider='google'` rows have `calendar_in_availability = true`. This is
   the repo's recurring landmine (INSTITUTIONAL_KNOWLEDGE.md, SWT-18 entry and
   landmine 6) in its eighth costume: a predicate whose discriminating column is
   constant, over a table with no rows, tested only by fixtures that fabricate
   values for it.
3. **The calendar ingest path cannot run in the production configuration, and
   auth is only half the reason.**
   - `internal/connector/google/calendar.go:17-27` — `NewCalendarClient(hc
     *http.Client, …)`; identity rides on an oauth2 client.
   - Measured: all three `google` rows are `auth_type='app_password'` with NULL
     `refresh_token_encrypted` and empty `scopes`. There is no token to build
     that client from.
   - **And even with tokens it would not run.** `IngestCalendar`
     (`internal/connector/google/ingest.go:226-271`) is reached from exactly two
     places: `google.Run` (`ingest.go:362`), used only by the `gmail_api` mail
     source, and the bridge (`internal/connector/google/bridge_ingest.go:99`),
     used only by `MAIL_SOURCE=bridge`. Production is `MAIL_SOURCE=imap`
     (`cmd/connectors/google/mailsource.go:37-56`), whose `runIMAPIngest`
     (`mailsource.go:65-134`) does mail and nothing else, and the resident watch
     loop (`cmd/connectors/google/watch.go`) contains the string "calendar" zero
     times. **Restoring tokens alone would still ingest no events.**
4. **Everything downstream of the raw row already works and is provider-agnostic.**
   `Normalize` dispatches on the `calendar:` external-id prefix
   (`internal/connector/google/normalize.go:329-336`) → `NormalizeCalendarEvent`
   → `sink.upsertEvent` (`internal/connector/google/sink.go:346`), upserting on
   `normalized_events_raw_item_idx` (migration 0005). Whatever puts
   `calendar:{id}` raw rows in front of it gets normalized events.
5. **`calendar_in_availability` defaults to `true` and most connectors never name
   it.** `migrations/0001_initial.sql:19`. `internal/connector/github/store.go:37-40`,
   `internal/connector/upworkcrm/sink.go:27-30`, `internal/planimport/store.go:28-31`
   and the jira account writer all insert without it, so those rows carry `true`.
   Only slackweb sets it false explicitly (`internal/connector/slackweb/sink.go:31-35`).
   **Consequence that decides the readiness scope:** a readiness rule phrased as
   "every account with `calendar_in_availability` must be fresh" would demand
   calendar data from a GitHub account and refuse forever. The scope must stay
   `provider='google'`, exactly as `LoadEvents` already spells it.
6. **`sync_runs` already carries an honest phase discriminator for this
   connector.** `PGSink.StartRun` writes `jsonb_build_object('phase', $2::text)`
   (`sink.go:97-107`) and callers pass `"gmail"` (`ingest.go:152`), `"calendar"`
   (`ingest.go:228`, `bridge_ingest.go:209`) or `"imap"`
   (`imap_ingest.go:26`). `FinishRun` merges with `stats = stats || $3::jsonb`
   (`sink.go:114-118`) and `Stats` (`ingest.go:59-81`) has no `phase` field, so
   the key survives. This is NOT the upworkcrm trap recorded in
   INSTITUTIONAL_KNOWLEDGE.md ("do not tell run kinds apart by their stats
   payload") — there the two kinds marshalled the same struct; here the phase is
   written deliberately at StartRun and has three distinct values.
7. **`propose_slots` is registered on the executor and has no scheduled caller.**
   `internal/tools/createtask.go:56` registers it;
   `internal/tools/proposeslots.go` is the handler; it is NOT MCP-listed
   (`internal/tools/tools_unit_test.go:68-69` — "registered, reachable via opsctl
   call"). A repo-wide grep for `propose_slots` finds only the tool, its tests,
   the google integration test and docs. **So the fail-open answer is latent
   today and the blast radius of making it refuse is nil** — one wired-up cron
   away from booking over every real meeting, and zero callers to break.
8. **Calendar WRITE is already denied and stays denied.**
   `internal/policy/matrix.go:155-158` — a `calendar` delivery falls to
   `channel_not_live`. Nothing in this ticket changes that.
9. **`UpsertGoogleAccount` does not touch `auth_type` or `app_password_encrypted`**
   (`internal/connector/google/oauth.go:122-139`), and migration 0014's CHECK
   constraints (`source_accounts_auth_type_check`,
   `source_accounts_app_password_present`) permit a row that holds both an app
   password and a refresh token. **A dual-auth row is legal: IMAP/SMTP for mail,
   OAuth for calendar.** That is the shape this ticket targets — nothing about the
   live mail path changes.
10. **Migration state.** Highest file is `migrations/0018_bulk_project_and_classify_flag.sql`.
    **This ticket adds no migration**, so the "merging a migration is not applying
    it" deploy hazard does not arise.
11. **The `--full` flag's calendar clause refers to live code that production
    cannot reach.** `cmd/connectors/google/main.go:36` advertises "ignore gmail
    cursor, drop calendar sync token"; the calendar half is `cfg.Full` at
    `ingest.go:247-249` and `bridge_ingest.go:226`, both unreachable under
    `MAIL_SOURCE=imap`, and the gmail half is unreachable there too (IMAP uses
    per-folder UID cursors). The help string describes `gmail_api` mode only.
12. **The synced window is a code constant.** `ingest.go:19-22` —
    `calWindowPast = 30d`, `calWindowFuture = 90d`, both unexported. A caller
    asking `propose_slots` for a window 120 days out would get an empty busy set
    **even with a perfectly fresh sync**: the same fail-open bug on a second axis.

## Goal

Make `propose_slots` refuse to answer whenever it cannot prove it holds current
calendar data for every google account marked `calendar_in_availability`, and
give calendar ingestion a wired, credential-gated path that runs in the
production (`MAIL_SOURCE=imap`) configuration once — and only once — a human has
run the OAuth consent flow.

**Usable alone** means: after this ticket and before any consent flow is run,
`propose_slots` answers no caller with a fabricated free slot — it errors, names
the accounts it has no data for, and leaves an `audit_events` row with
`status='error'`. That half needs no Google credential to build, test or verify.
The ingest half is complete and tested against fakes in the same delivery; the
only thing it waits on is Salvador at a browser.

## Acceptance criteria

### Part A — fail-closed availability (the must-ship core)

1. `internal/availability` exposes exactly one database-backed entry point,
   `LoadBusy(ctx, pool, req) ([]Interval, error)`, which performs the readiness
   check **before** it reads a single row of `normalized_events`. `LoadEvents` is
   unexported (`loadEvents`) — the unsafe primitive is unreachable from outside
   the package, not merely discouraged.
2. A structural unit test (no build tag, no db — the shape of
   `internal/textmatch/callsites_test.go`) fails if the string
   `normalized_events` appears in a SQL literal anywhere under `internal/` or
   `cmd/` outside `internal/availability/store.go` and
   `internal/connector/google/sink.go` (the writer), test files excepted.
3. Readiness scope is `source_accounts` rows with
   `provider='google' AND calendar_in_availability` — the same two predicates
   `LoadEvents` already applies. Rows of other providers are out of scope even
   though most of them carry `calendar_in_availability = true` (premise 5).
4. An in-scope account is READY iff `sync_runs` holds a row for it with
   `status='ok' AND stats->>'phase'='calendar' AND finished_at >= now - maxSyncAge`.
5. `LoadBusy` returns a typed, `errors.As`-able error (`*availability.NotReadyError`)
   when **either** (a) any in-scope account is not ready, or (b) the scope is
   empty. The error text names each offending account email and its last
   successful calendar sync, or the word `never`. Case (b) is a refusal on
   purpose: "no calendar is in availability scope" is not "you have no meetings".
6. **A ready account with zero events in the window is answered, not refused.**
   The discriminator is the freshness of the *sync*, never the *count of events* —
   a genuinely empty week must still produce slots. An integration test seeds a
   fresh ok calendar run with no events and asserts slots come back.
7. **Window coverage:** `LoadBusy` refuses a requested window not fully inside
   `[now - google.CalendarWindowPast, now + google.CalendarWindowFuture]`. Those
   two constants are exported from `internal/connector/google` (renamed from
   `calWindowPast` / `calWindowFuture`) and referenced by the tool wiring — not
   re-spelled as literals in a second package.
8. `propose_slots` propagates the error unchanged. `executor.Execute` therefore
   writes `audit_events.status='error'` carrying the reason
   (`internal/executor/executor.go:93-100`). The tool **never** returns
   `{"slots":[]}` for a not-ready calendar: an empty array is exactly the
   ambiguous answer this ticket abolishes.
9. There is **no override** — no `force` argument, no env var, no actor that
   bypasses the refusal. In particular the check must not be keyed on the actor
   prefix (INSTITUTIONAL_KNOWLEDGE.md: "an actor-prefix check is a transport
   label, not a trust boundary"). A unit test asserts the refusal for the six
   actor shapes that exist in the repo (`dashboard:`, `opsctl:`, `mcp:worker:`,
   `mcp:manual:`, `drafts:gpt`, bare `worker:`).
10. The decision itself is a pure function —
    `availability.NotReady(states []AccountState, now time.Time, maxAge time.Duration) []AccountState`
    — with no clock read, no env read and no pool, matching the package doc
    ("pure functions over calendar intervals — no LLM, no network, no clock
    reads"). The SQL that produces `[]AccountState` lives in `store.go` beside
    `loadEvents`.
11. `AVAIL_MAX_SYNC_AGE` (Go duration, default `1h`) is read **only** in
    `availabilityConfig()` (`internal/tools/proposeslots.go:102-136`), next to the
    existing `AVAIL_*` knobs. An unparseable value is an error returned to the
    caller, never a silent fallback: a typo must not widen a safety window.
12. The existing propose_slots assertion in
    `internal/connector/google/integration_test.go:289-335` passes only after the
    fixture seeds a fresh `status='ok'`, `phase='calendar'` `sync_runs` row for
    both of its accounts; removing that row makes the call fail and the audit row
    read `error`. The fixture is now shaped like production, not like the
    assertion.
13. **Mutation proof (INSTITUTIONAL_KNOWLEDGE.md landmine 6, verbatim rule).**
    The readiness predicate is fed by a column, so its regression test is an
    integration test: replacing `a.calendar_in_availability` in the readiness
    SELECT with the literal `true` must turn a test red. The same suite seeds a
    google account with the flag **false** and a stale calendar, and asserts that
    account causes no refusal and contributes no busy interval.

### Part B — a calendar path that can run (machine-verifiable half)

14. `google.CalendarScopes = ["https://www.googleapis.com/auth/calendar.readonly"]`
    — calendar only. The consent this ticket asks for does not re-request the
    restricted Gmail scopes that migration 0014 abandoned OAuth to avoid.
15. `google-auth add-calendar <email> [--no-availability]` runs the existing
    `LoopbackFlow` with `CalendarScopes`, verifies the authorized identity
    against a new `CalendarClient.PrimaryCalendarID(ctx)` (the primary calendar's
    id is the account address) and aborts storing nothing on mismatch — the same
    rule `addCmd` applies with `GetProfile` (`cmd/google-auth/main.go:90-100`),
    which cannot be used here because it needs a Gmail scope.
16. `add-calendar` leaves `auth_type` and `app_password_encrypted` untouched. An
    integration test asserts that after it runs on an `app_password` row, the row
    still has its password, still has `auth_type='app_password'`, is still
    returned by `ListAppPasswordAccounts`, and now also has a refresh token and
    calendar scope. **The mail path is unchanged for that account.**
17. `cmd/connectors/google` gains a calendar phase that runs for every
    `provider='google'` account with a non-NULL `refresh_token_encrypted` **and**
    `calendar.readonly` in `scopes` — credential-gated, not `auth_type`-gated
    (premise 9). It takes the same per-account advisory lock the IMAP path takes
    (`sink.LockAccount`), records one failing account without aborting the others,
    and fails the pass only if none succeeded. Zero credentialed accounts prints
    a line and exits 0 — the shape of `mailsource.go:77-82`.
18. The calendar phase does **not** run when the mail source is `bridge` or
    `gmail_api`, both of which already ingest calendar inline (premise 3). No
    account gets two calendar passes in one invocation; a unit test over the
    selection function pins all three modes.
19. `IngestCalendar` writes its cursor with
    `SaveCursorField(accountID, "calendar_sync_token", …)`
    (`internal/connector/google/sink.go:445-457`) instead of the whole-blob
    `SaveCursor` it uses today (`ingest.go:261-266`). An integration test seeds a
    cursor containing `imap_folders`, runs a calendar pass, and asserts the IMAP
    positions survive — the exact clobber `bridge_ingest.go:190-200` already
    guards against on the bridge path.
20. `--calendar-only` runs the calendar phase followed by `Normalize`, and
    nothing else (no mail ingest, no `ObserveOutbound`, no capture-rules pass —
    those belong to the mail funnel and the watch loop already runs them).
    `--full` drops the calendar sync token in the new phase, and the `--full`
    help string names which mode each of its clauses applies to (premise 11).
21. An account whose token refresh fails leaves a `sync_runs` row with
    `status='error'` and no `ok` row, so Part A keeps refusing for it. Proven
    against the existing fake server (`internal/connector/google/fake_google_test.go`)
    and a failing token source — **no live Google credential in any test**.
22. **No calendar write.** The new code issues no POST/PUT/PATCH/DELETE against
    the Calendar API; a structural test over `internal/connector/google/calendar.go`
    asserts the only HTTP methods present are GETs. Invariant 4: an outbound
    calendar action needs a `deliveries` row and a policy tier, and `calendar` is
    still `channel_not_live`.
23. `docs/runbooks/calendar-availability.md` exists and records: the one human
    step (GCP project, consent screen with calendar scope only, Desktop client,
    `add-calendar` per account), how to verify a first sync, how to read a
    refusal from `propose_slots`, and the operator branch below.

## Data model changes

**None. No migration.** Highest applied file stays 0018.

Every input the readiness check needs already exists and is already written by
shipped code:

| need | column / table | written by |
|---|---|---|
| which calendars must be represented | `source_accounts.calendar_in_availability` (0001:19) | `google-auth add`, `add-calendar` |
| when we last looked at a calendar | `sync_runs.finished_at` + `status` + `stats->>'phase'` | `PGSink.StartRun`/`FinishRun` |
| the busy set | `normalized_events` (0001:68, 0005) | `sink.upsertEvent` |
| calendar credentials | `source_accounts.refresh_token_encrypted`, `scopes` | `UpsertGoogleAccount` (pgcrypto) |

Deliberately NOT added: an `availability_state` / `calendar_health` table. That
would be a second source of truth for "did the poller look", derived from
`sync_runs`, and would drift the first time a pass died between the write and the
derived write. Invariant 2's spirit — queues are filters, not tables — applies to
health as much as to work.

## API / MCP tool changes

No new tool, no MCP surface change. `propose_slots` keeps its name, its args
(`duration_minutes`, `window_start`, `window_end`, `count`), its registration in
`tools.Register` (`internal/tools/createtask.go:56`) and its off-MCP status
(reachable via `opsctl call`, and by the future calendar-block writer).

Its behaviour gains exactly one outcome:

```
ok      → {"slots":[{"start":"…","end":"…"}, …]}          (unchanged shape)
error   → tool propose_slots: calendar not ready: sspataro@gmail.com last
          successful calendar sync never; salvador@<org> last successful
          calendar sync 2026-08-29T04:11:07Z (older than AVAIL_MAX_SYNC_AGE=1h)
```

The refusal is produced inside the handler, so it travels the whole executor path
(invariant 3): validate → policy check (`propose_slots` is a static
fallthrough, unchanged) → audit start → handler → audit complete with
`status='error'` and the reason in `audit_events.error`
(`internal/executor/executor.go:93-100`).

`google-auth` gains one subcommand, `add-calendar`. `google-auth` is trusted
spine — it writes `source_accounts` directly like the connectors do and is
deliberately not an executor tool (`cmd/google-auth/main.go:1-5`); that stays
true.

## MQTT topics

None. Nothing in this ticket publishes or subscribes.

## Files likely to touch

**Part A**
- `internal/availability/store.go` — `loadEvents` (unexported), new
  `LoadAccountStates`, new `LoadBusy`.
- `internal/availability/availability.go` — `AccountState`, `NotReadyError`,
  pure `NotReady(...)`; `Busy`/`Merge`/`ProposeSlots` unchanged.
- `internal/availability/availability_test.go` — extend with the pure readiness
  cases; keep the existing criterion-11 tests untouched.
- `internal/availability/readiness_integration_test.go` (new, build tag
  `integration`) — the column-fed cases from criteria 6, 12, 13.
- `internal/availability/callsites_test.go` (new, plain unit) — criterion 2.
- `internal/tools/proposeslots.go` — call `LoadBusy`, read
  `AVAIL_MAX_SYNC_AGE`, pass the coverage horizon.
- `internal/connector/google/integration_test.go:289-335` — seed the fresh
  calendar run; add the removed-run assertion.

**Part B**
- `internal/connector/google/oauth.go` — `CalendarScopes`.
- `internal/connector/google/calendar.go` — `PrimaryCalendarID`; export
  `CalendarWindowPast` / `CalendarWindowFuture` (currently `ingest.go:19-22`).
- `internal/connector/google/ingest.go` — `IngestCalendar` cursor write via
  `SaveCursorField`; constant rename.
- `cmd/google-auth/main.go` — `add-calendar` subcommand + usage text.
- `cmd/connectors/google/main.go` — `--calendar-only`, `--full` help text,
  calendar phase call sites.
- `cmd/connectors/google/calendarsource.go` (new, sibling of `mailsource.go`) —
  which accounts are calendar-credentialed, whether the phase runs for this mail
  source, the per-account lock loop.
- `cmd/connectors/google/calendarsource_test.go` (new) — mode selection, unit.
- `internal/connector/google/fake_google_test.go` — a primary-calendar endpoint
  for the identity check.
- `docs/runbooks/calendar-availability.md` (new).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — a "Calendar + availability" entry at
  delivery (the CalDAV 401 measurement, the readiness contract, the dual-auth
  row).

## In scope / out of scope

**In scope**
- The fail-closed refusal, its typed error, its purity split and its structural
  guard.
- Window-coverage refusal (criterion 7) — the same bug on the horizon axis.
- Calendar-only OAuth consent, credential-gated calendar ingest wired into the
  IMAP configuration, cursor-field write, `--calendar-only`, corrected `--full`
  help.
- The runbook, including the single human step.

**Out of scope — including the things it is tempting to bundle**
- **Calendar WRITE of any kind**: own blocks, invites, `propose_slots` →
  `draft_delivery(channel='calendar')`. `calendar` is `channel_not_live`
  (`matrix.go:155-158`) and going live needs a delivery adapter, a policy tier
  and idempotency on `sent_external_id`. That is the build-order step 8 shape,
  and a separate ticket.
- **Running the consent flow.** It needs Salvador at a browser; the ticket is
  deliverable, reviewable and mergeable without it.
- **Unsuspending `connector-google` or adding a calendar CronJob.** Manifests
  live in the sibling kube repo; this delivery ends with a handoff note, not a
  manifest.
- **A calendar sweep inside the resident watch loop.** `--calendar-only` exists
  so a CronJob can run the phase alone; making the watch Deployment periodic on a
  second axis is a change to the one long-running ingest process and does not
  belong in the same diff as a safety gate.
- **Dashboard surfacing of calendar staleness** (`/sources` already lists
  accounts, `internal/dashboard/sources.go:79`). Future work.
- **Triage, classify, capture** — untouched. This ticket must not change
  `body_text`, any matcher, or any capture filter.
- **CalDAV and ICS.** Closed by measurement; do not re-open without new evidence.
- **Non-google calendar providers.** The scope predicate stays `provider='google'`.

## Invariants that apply

1. **Raw-first.** The calendar phase reuses `IngestCalendar` → `upsertRaw` →
   `sink.InsertRaw` (`ingest.go:288`, `ingest.go:301-325`, `sink.go:145-159`):
   provider JSON and `content_hash` land in `raw_source_items` under external id
   `calendar:{id}` before `Normalize` ever runs. **No new code path may write
   `normalized_events` directly** — `sink.upsertEvent` (`sink.go:346`) stays the
   only writer, and it already refuses to write for a superseded raw row
   (`sink.go:367-371`).
2. **One funnel.** No new table and no task-like sibling. Calendar events are
   `normalized_events`; calendar health is derived from `sync_runs`. Nothing in
   this ticket creates a task or a queue.
3. **Everything through the executor.** The refusal lives inside the
   `propose_slots` handler so that a refused call is audited exactly like an
   allowed one — validate → policy → audit start → handler → audit complete
   (`error`). No second entry point to availability: criterion 1 unexports the
   only bypass, criterion 2 mechanically enforces it.
4. **Nothing external without a delivery row.** This ticket reads calendars and
   writes nothing to Google. Criterion 22 pins that structurally, because the
   moment a calendar client can PATCH, a future caller can book without a
   `deliveries` row and without passing `channel_not_live`.
5. **Own-message loop closure.** No message path is touched. Two concrete
   demands: criterion 19 exists so a calendar pass cannot clobber the IMAP folder
   cursor (a lost cursor re-reads or skips mail, and skipped mail is a delivery
   confirmation that never lands), and nothing in this diff may alter
   `body_text` or `confirmDeliveryByBodyPrefix` — google has no reconciler, so a
   one-space shift is permanent (STANDING RULE, SWT-25).
6. **Stealth attribution.** No client-visible output is produced. Nothing to
   scrub; recorded so the reviewer does not go looking.
7. **Orchestrator purity.** The orchestrator is untouched and still never
   imports a provider adapter. The availability decision stays pure and
   unit-testable with no network: `NotReady` takes `(states, now, maxAge)` and
   returns a slice.

## Sibling patterns to copy

- **Fail closed at a boundary, with a named unavailable state:**
  `internal/provider/router.go:131-172` — a local client that does not implement
  `Prober`, or whose probe fails, is `AvailUnreachable`, not ready. The comment
  there explains why the convenient reading ("it's local, assume it works") was
  rejected. `NotReadyError` is that shape for calendars.
- **Absence of evidence, refused, in this very connector:**
  `internal/connector/google/sink.go:468-473` — `SupersedeAbsentCalendar`
  refuses an empty reset snapshot because "an empty replacement is
  indistinguishable from a broken leaf". Same argument, opposite direction.
- **Explicit path selection with an error on the unknown value:**
  `cmd/connectors/google/mailsource.go:37-56`, and its per-account loop
  (`mailsource.go:65-134`) for the advisory lock, the continue-on-error rule and
  the "no accounts" line that keeps a zero-work pass from looking like a working
  one.
- **Field-scoped cursor writes:** `internal/connector/google/bridge_ingest.go:190-200`
  with `sink.SaveCursorField` (`sink.go:445-457`).
- **A structural test that bans the bypass rather than documenting it:**
  `internal/textmatch/callsites_test.go` and
  `internal/connector/upworkcrm/keyspelling_test.go` — plain unit tests, no build
  tag, no db, scanning source for a forbidden spelling.
- **An integration test that dies when a column leaves the SELECT:** the SWT-21
  `ai_locality` story (INSTITUTIONAL_KNOWLEDGE.md landmine 6) and
  `internal/drafts/` — the unit test cannot catch it because the unit test is the
  thing supplying the value.
- **Queue/lock idiom:** unchanged; `pg_try_advisory_lock` per account as
  `sink.LockAccount` already does (`sink.go:404-416`).

## Verification protocol

Run in this order; nothing here needs a Google credential.

1. `go test ./...` — pure availability cases, the actor-shape sweep, the
   structural callsite test, the mail-source/calendar-source selection tests.
2. `make integration` (db-up + migrate + `go test -tags integration ./...`,
   serialized `-p 1` — the new availability integration suite makes scoped
   assertions, so it must clean its own fixtures in FK order and join the
   mutual-cleanup pact).
3. **Mutation checks — each must turn a test red, verified by hand:**
   - replace `a.calendar_in_availability` with `true` in the readiness SELECT
     (criterion 13);
   - delete the readiness call from `LoadBusy` (criterion 5);
   - widen the coverage horizon to ten years (criterion 7);
   - drop the `stats->>'phase'='calendar'` clause so a gmail/imap run counts as a
     calendar sync (criterion 4).
4. **Production smoke, read-mostly, no credential** (`OPS_DATABASE_URL` is in
   `~/.bashrc`; `.bashrc` early-exits for non-interactive shells — grep/eval it):
   - `psql "$OPS_DATABASE_URL" -c "SELECT account_email, auth_type, calendar_in_availability, refresh_token_encrypted IS NOT NULL AS has_token FROM source_accounts WHERE provider='google' ORDER BY id"`
   - `psql "$OPS_DATABASE_URL" -c "SELECT count(*) FROM normalized_events"` — re-measure; the SPEC's 0 is a measurement, not a constant.
   - `psql "$OPS_DATABASE_URL" -c "SELECT max(finished_at) FROM sync_runs WHERE status='ok' AND stats->>'phase'='calendar'"`
   - `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/opsctl call --tool propose_slots --args '{"duration_minutes":30}'`
     — **must fail**, naming the three accounts. Before this ticket the same
     command returns a week of free slots; capture both outputs in the delivery
     note, because that diff *is* the ticket.
   - `psql "$OPS_DATABASE_URL" -c "SELECT tool, status, left(error,160) FROM audit_events WHERE tool='propose_slots' ORDER BY id DESC LIMIT 1"`
     — `status='error'`, reason stored.
   - `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/connectors/google --calendar-only`
     — prints the "no google accounts with OAuth calendar credentials" line and
     exits 0. It must not touch mail, and `SELECT count(*) FROM sync_runs` must be
     unchanged by it.
5. **"Usable alone" check:** the refusal holds for every actor and every window,
   and no other surface regressed — `SELECT count(*) FROM tasks` and
   `FROM deliveries` unchanged across the smoke.
6. **Not part of delivery** (the human step, runbook §consent): GCP project +
   Calendar API + consent screen with `calendar.readonly` only + Desktop client,
   then per account `DATABASE_URL="$OPS_DATABASE_URL" OPS_TOKEN_KEY=… go run
   ./cmd/google-auth add-calendar <email>`, then
   `go run ./cmd/connectors/google --calendar-only --full`, then re-run the
   `opsctl call` above and watch it answer with real busy time.

## Decisions made unilaterally

- **Both halves ship in this ticket.** Part A alone would leave `propose_slots`
  permanently refusing with no path out of the refusal, which is safe but dead.
  Part B is fully machine-verifiable against the existing fake server, so it
  costs the delivery nothing in credential-blocked work. The alternative
  considered and rejected: ship A now, file B as SWT-26.
- **The freshness signal is a successful calendar `sync_run`, not the presence of
  events.** The issue's phrasing — "refuse when it holds no events for an account
  marked calendar_in_availability" — would refuse forever for a genuinely empty
  calendar and would never distinguish "quiet week" from "poller dead". Also
  rejected: `max(raw_source_items.ingested_at)` over `calendar:%` external ids,
  which measures when an event last *changed*, not when we last *looked*. Today
  the three signals agree (nothing has ever synced); they diverge the first
  quiet week after go-live.
- **A refusal is an error, not an empty slot list.** `{"slots":[]}` is safe in the
  narrow sense (nothing gets booked) and is exactly the silent failure this repo
  keeps paying for — indistinguishable from "fully booked", invisible in an audit
  row, and impossible to alert on. An error is loud, is stored, and reads
  `status='error'` in `audit_events`.
- **No override, and the scope-empty case refuses too.** If a future operator
  genuinely wants slots with no calendar behind them, the honest move is a
  different tool with a different name, not a flag on the safety gate.
- **`AVAIL_MAX_SYNC_AGE` defaults to 1h.** Four missed `*/15` polls before the
  service goes quiet; the failure mode of too-short is a loud refusal (cheap,
  reversible) and of too-long is a stale busy set used as truth (the bug). The
  runbook records that the value must exceed twice the poll period.
- **Readiness scope stays `provider='google'`**, matching `LoadEvents`. Widening
  it to "any account with the flag" would demand calendar data from GitHub,
  Upwork and plan accounts, which carry the flag only because 0001 defaults it to
  true (premise 5). The rejected alternative — a migration setting
  `calendar_in_availability=false` for non-google rows — changes no behaviour and
  buys only tidiness, and forward-only migrations are not for tidiness.
- **`calendar_in_availability` stays `true` on all three google accounts.** With
  the refusal in place the flag stops being a permission ("this calendar may be
  consulted", trivially satisfiable by an empty table) and becomes a requirement
  ("this calendar MUST be represented before I answer"). Setting it false to make
  `propose_slots` answer again would be re-introducing the exact fail-open this
  ticket removes, one `UPDATE` at a time.
- **Consent asks for `calendar.readonly` only.** Migration 0014 abandoned OAuth
  for mail because restricted Gmail scopes need Google verification and a CASA
  assessment and can be blocked by a Workspace admin. Re-requesting them to fix
  calendars would drag that whole problem back in for no benefit — IMAP and SMTP
  already work.
- **Credential-gated, not `auth_type`-gated.** `auth_type` names the MAIL path
  (`mailsender.go:80`, `ListAppPasswordAccounts`) and must keep saying
  `app_password` after consent, or mail breaks. Calendar keys on "has a refresh
  token and the calendar scope", making the row legitimately dual-auth.
- **No new env var for the calendar phase.** `mailsource.go`'s argument for an
  explicit `MAIL_SOURCE` is that two competing paths exist for the same data and
  the choice must be visible in a manifest diff. Calendar has one path, so the
  account row is the only switch, and the phase prints a line whether it finds
  credentials or not — a zero-work pass is never silent.

## Operator branch, decided at consent time (not a code fork)

Two of the three mailboxes are Workspace orgs Salvador does not administer
(migration 0014's rationale). An admin there can block the client id for
`calendar.readonly` as easily as for Gmail scopes. If consent fails for one of
them, the branch is:

- **(a) leave `calendar_in_availability = true`** on the blocked account —
  `propose_slots` stays refused for everyone, forever, until that calendar can be
  read. Honest and useless.
- **(b) set it `false`** on the blocked account — `propose_slots` answers from a
  knowingly incomplete busy set, and can book over a meeting that lives only in
  that org's calendar.

Neither is right in the abstract; it depends on whether client meetings live in
the blocked calendar. The code behaves correctly under both, so this does not
block the ticket. Record the choice, and the reason, in
`docs/runbooks/calendar-availability.md` when it is made.

## Future work (not this ticket)

- Calendar write: `propose_slots` → `draft_delivery(channel='calendar')` → a
  Calendar insert adapter, own blocks at the auto tier and invites at approve,
  with `sent_external_id` = the event id. Needs `channel_not_live` lifted for
  `calendar` and a rate limit.
- A calendar sweep in the resident watch loop, or a dedicated `connector-gcal`
  CronJob, so freshness is maintained without the mail pass.
- Dashboard: calendar freshness per account on `/sources`, so a refusal is
  visible before someone hits it.
- A morning-brief section listing today's meetings — deterministic SQL over
  `normalized_events`, no LLM (R7's shape).
- Extending readiness to any future calendar provider, at which point the
  `provider='google'` scope becomes a capability lookup rather than a literal.
