> Jira: SWT-29

# funnel-view — the ingestion/classification funnel as a dashboard page

**SETTLED 2026-09-08.** Both open questions were answered by Salvador and are
folded in: **Q1 = (a)** a new `GET /funnel` route, and `/sources` gives up its
run columns in the same ticket; **Q2 = (a)** the classify summary lives on
`/funnel` with everything else, one page. `funnel-view_OPEN_QUESTIONS.md` is
kept as the record of what each answer traded away. Ready for `test-author`.

## Source

Ad-hoc, not a build-order step. Salvador, 2026-09-08:

> The dashboard renders the deliveries slice; the bulk of what switchboard does —
> ingestion, classification, capture — lives in `classify report` (CLI),
> `sync_runs` psql queries and CronJob log lines. I looked at the board, found it
> stale, and the diagnosis is that 99% of the system's activity is invisible.
> Build a read-only ingestion/funnel visibility page on the existing dashboard:
> connector health, intake counts, classify shadow summary, capture attribution.

**One correction to the premise, verified in code before writing this:** the
dashboard is not deliveries-only. `internal/dashboard/server.go` already serves
`/tasks`, `/tasks/{id}`, `/briefs`, `/plans`, `/plans/{id}`, `/deliveries`, the
exports — and `/sources` (`internal/dashboard/sources.go`, shipped SWT-11),
which renders per-account LIFETIME totals (raw / normalized / pending / in /
out), the newest message per account, a per-channel table, and a "last run"
column that takes the newest `sync_runs` row of ANY phase and ANY status.

So section 1 of the request (connector health) is *partly* built and *wrongly*
built for this purpose — "latest run" is not "last successful sync per phase",
and nothing on that page knows about freshness. Sections 2, 3 and 4 (per-day
intake trend, classify shadow summary, capture attribution) have no web surface
at all. Q1 resolved that overlap: `/funnel` takes connector health and
`/sources` gives it up, in this ticket.

## Goal

A read-only `/funnel` page on the existing dashboard binary that answers, in one
screen and without psql: **is each connector still syncing, is mail still
arriving, how much of it do the capture rules claim, and what did the classifier
say.**

*Usable alone:* after this ticket a stale board can be diagnosed from a browser.
The page distinguishes "quiet because nothing arrived" from "quiet because a
connector died four days ago", from "quiet because the local classify box is
off" — three states that today require three different CLI/psql incantations and
are indistinguishable from the board.

## Acceptance criteria

1. `GET /funnel` is registered on the mux, wrapped in `s.auth.Require` exactly
   like every other page route, and renders HTTP 200 with a session.
2. The page is a window, not a control: **no POST route** exists under `/funnel`,
   and the new handler file never references `s.ex` / `executor.Call`. Enforced
   by an in-package structural test (the `board_structure_test.go` idiom) that
   scans the funnel source and the template for `<form method="post"`,
   `s.ex.Execute` and `hx-post`, and asserts `Handler()`'s registered patterns
   contain no `POST /funnel*`.
3. **Connector health** renders one row per `(source_account, phase)`, where
   `phase` is `sync_runs.stats->>'phase'` and rows with no phase key group under
   a single `(none)` bucket. Each row shows: provider, account email, phase, the
   **last successful** run (`status='ok' AND finished_at IS NOT NULL`,
   `max(finished_at)`), its age, and the run count in the window. An account with
   zero successful runs for a phase it has ever attempted still appears, showing
   `never`; an account with no runs at all appears once, under `(none)`, showing
   `never`.
4. Rows are classified `ok` / `stale` / `never` by a **pure** helper
   `funnelFreshness(last, now time.Time, max time.Duration) string`, unit-tested
   with relative times.
5. **Calendar freshness is the system's own rule, not a second spelling of it.**
   Rows whose phase is `calendar` are judged with the same
   `AVAIL_MAX_SYNC_AGE` value and the same predicate `propose_slots` uses:
   `availability.NotReady(states, now, maxAge)` over
   `availability.CalendarSyncStates(ctx, pool)` (see Data model / API below).
   The page prints the effective `AVAIL_MAX_SYNC_AGE` next to the section, and
   states in prose that `propose_slots` refuses while any in-scope account is
   stale. A page that says "green" while `propose_slots` refuses is the specific
   failure this criterion exists to prevent.
6. Every other phase is judged against `funnelDisplayStaleAfter = 3h`, a package
   constant, and the section says **in the page** that this threshold is
   display-only and gates nothing. (`*/15` CronJobs plus the resident mail watch
   loop make 3h ~12 missed passes; it is a display default, not a contract.)
7. **Intake trend**: one table, one row per day for the last N days (N from
   `?days=`, default 14, clamped to `[1,90]`, shared by every windowed section on
   the page), newest first. Columns: the day, one count per `source_account`
   (`raw_source_items.ingested_at::date`), a raw total, and the count of
   `normalized_messages` created that day (`normalized_messages.created_at::date`).
8. **Days with zero rows are rendered as zeros, not omitted.** The gap fill is a
   pure helper over the query result (`fillDays(...)`) with an injected `now`,
   unit-tested — an absent row is exactly the signal the page exists to show, and
   a missing table row is not a signal anyone notices.
9. Both intake axes use the **pipeline clock** (`ingested_at`, `created_at`), not
   `normalized_messages.sent_at`. Stated in the page's caption: `sent_at` is the
   provider's clock and answers a different question (when the mail was written),
   and a backfill would scatter it across years.
10. **Classify shadow summary** renders BOTH lanes — `classify.LanePersonal` and
    `classify.LaneResidue`, referenced through those vars, never as the string
    literals `"classify"` / `"classify_residue"`. Per lane: classified, flagged,
    by-kind counts, the four link states, and the skipped breakdown
    (`why the lane refused` / `why it was restricted`) with the same numbers
    `classify report --lane X --since Ndays` prints for the same window.
11. Those numbers come from a **refactor, not a re-spelling**:
    `classify.Summarize(ctx, pool, since, workerType) (classify.Summary, error)`
    owns the SQL and the Go fold that `ReportForWorker` performs today;
    `ReportForWorker` becomes a renderer over `Summary`. The dashboard calls
    `Summarize`. There is no second copy of the verdict query, the link-state
    fold, or the skipped-run fold anywhere.
12. `ReportForWorker`'s **text output is byte-identical** before and after the
    refactor, pinned by a characterization test over a seeded fixture. In
    particular the no-fallback note, the all-skipped sentence and the runbook
    pointer stay as string literals **inside `internal/classify/report.go`** —
    `TestReports_ShareTheNoFallbackNote` (`internal/classify/structure_test.go`)
    scans that file's string literals and fails vacuously-guarded if they move.
13. **Latest flags**: per lane, the flagged verdicts newest-first with time,
    `normalized_message_id`, kind, sender, subject, title and the resolved link
    URL (rendered as an `<a>` when present, an em dash when not), capped at 50
    rows per lane. Sender and subject come from the stored
    `ai_extractions.fields` exactly as the CLI report reads them — no join back
    to `normalized_messages` for a second copy of those values.
14. **Capture attribution** renders a per-day table over the same window with
    **THREE** states, never two: `matched` (latest decision action is
    `attributed` / `task` / `task_log`), `unmatched` (latest decision action is
    `unmatched`), and `not yet evaluated` (no decision row at all). The page
    labels the third one and says it is not the residue. Conflating unseen with
    unmatched is the named capture trap (IK, "Three states, and conflating the
    first two is the trap").
15. The attribution rows count **inbound messages only**, and the page says why:
    `internal/capture/rules_store.go` filters `direction='inbound'` (that line IS
    invariant 5), so an outbound message can never carry a decision on any pass
    in any mode — its absence is absent-because-impossible, and counting it as
    "not yet evaluated" would be a lie about 21k rows.
16. The latest-decision predicate is **capture's spelling**, exported from
    `internal/capture` as `AttributionTrend(...)`; the dashboard adds no SQL that
    picks a message's newest `capture_decisions` row.
17. **Sections degrade independently.** Each section's loader returns an error
    instead of aborting the response; the page renders every section that
    loaded, plus an inline, styled error line naming the failed section. The
    request still returns 200. The collector is a pure helper over injected
    loader funcs and is unit-tested with one deliberately failing loader and two
    succeeding ones.
18. The page **never names `normalized_events`** in SQL or in any non-comment
    line — `internal/availability/callsites_test.go` bans a second reader outside
    its two-file allowlist, and this page has no need for it (raw counts and
    `normalized_messages` answer every question here). The calendar column reads
    `sync_runs` only, through `availability.CalendarSyncStates`.
19. **No migration, no new table, no view, no materialized view, no cache table.**
    `ls migrations/` is unchanged at `0020` after this ticket.
20. A nav link labelled `Funnel` is added to the shared nav line of every
    template that carries one — `deliveries.html`, `sources.html`, `tasks.html`,
    `task.html`, `briefs.html`, `plans.html`, `plan.html` (7 files) — in one
    consistent position, next to `Sources`.
21. Integration tests, build-tagged `integration` and `DATABASE_URL`-gated, cover
    each of the four sections against the compose db on **:5433**, with the
    real `dashboard.Server` under `httptest` + dev-login, and they assert on
    **their own seeded rows** — never on a global total (see Verification).
22. **`/sources` gives up connector health in this same ticket (Q1-a).**
    `sourceRow` loses `LastRunAt`, `LastRunPhase`, `LastRunStatus`,
    `LastRunError` and `RunsTotal`; the SELECT loses the five `sync_runs` scalar
    subqueries; `templates/sources.html` loses its `Last run` and `Status`
    columns (and the `{{if eq .LastRunStatus "ok"}}` block) and gains one
    `.muted` line: runs and freshness live on `/funnel`. After this,
    `internal/dashboard/sources.go` **does not name `sync_runs` at all** —
    asserted by a structural test in the callsites idiom, with a positive control
    so the scan cannot pass vacuously (assert `funnel.go` DOES name it). Leaving
    both spellings live "for one release" is how the second one survives; the
    deletion ships with the addition.
23. **Section order on `/funnel` is health → intake → capture → classify**, with
    the classify block LAST. Q2's accepted cost is that it dominates the page;
    ordering is the only mitigation taken (the block is not trimmed, and its
    numbers are not summarised away — criterion 10 stands).

## Data model changes

**None. No migration.** Every fact on the page already exists:

| section | tables read | key columns |
|---|---|---|
| connector health | `source_accounts`, `sync_runs` | `provider`, `account_email`, `calendar_in_availability`, `status`, `finished_at`, `stats->>'phase'` |
| intake trend | `raw_source_items`, `normalized_messages`, `source_accounts` | `ingested_at`, `created_at`, `source_account_id` |
| capture attribution | `normalized_messages`, `capture_decisions` | `direction`, `created_at`, `message_id`, `id`, `action` |
| classify summary | `ai_runs`, `ai_extractions` | `worker_type`, `status`, `created_at`, `fields` |

Facts worth stating because they shape the queries:

- `sync_runs` has **no phase column**; the phase is a key in `stats`, written at
  `StartRun` by google (`imap` | `gmail` | `calendar`) and slackweb
  (`slack_web`). **jira and upworkcrm write no phase at all** —
  `internal/connector/jira/sink.go:75` and
  `internal/connector/upworkcrm/sink.go:73` insert with the default `'{}'`
  stats. So `(none)` is a real, expected bucket, not a bug.
- **One upworkcrm invocation writes TWO `sync_runs` rows** (ingest + normalize,
  IK entry). The page does NOT try to tell them apart — the same IK entry says
  both marshal the same struct and a stats-based discriminator would be the
  constant-discriminator landmine again. The run COUNT for upwork is therefore
  double the number of CronJob ticks, and the page's caption says so in one
  clause rather than silently presenting an inflated number.
- `capture_decisions_message_idx (message_id, id DESC)` already serves the
  latest-decision lateral. `capture_decisions` CHECK
  `(action='unmatched') = (project_id IS NULL)` is why the attribution fold keys
  on `action`, positively spelled, and never on `project_id IS NULL`.
- No index is added. `raw_source_items` has no `ingested_at` index and
  `ai_runs` none on `created_at`; at ~10^5 rows the aggregates are seq scans in
  the tens-to-low-hundreds of milliseconds for a page a human opens by hand.
  **If measurement (see Verification) says otherwise, the ticket still ships
  without an index and the index gets its own SPEC** — a migration added
  opportunistically inside a display ticket is how the schema grows claims
  nobody can trace.

## API / MCP tool changes

**No MCP tools. No executor tools. No policy surface. Nothing is executed by
this page** (invariant 3 below). One new HTTP route and four small Go seams:

1. `GET /funnel[?days=N]` — dashboard, session-required, HTML.

2. `internal/classify` (new file `summary.go`):
   ```go
   type Summary struct {
       WorkerType                     string
       Classified, Flagged            int
       ByKind                         map[string]int
       LinkResolved, LinkDeclined     int
       LinkNoneOffered, LinkRejected  int
       Skipped                        int
       ByAvailReason, ByClassReason   map[string]int
       Flags                          []Flag // newest first
   }
   type Flag struct {
       At        time.Time
       MessageID int64
       Kind, Sender, Subject, Title, LinkURL string
   }
   func Summarize(ctx context.Context, pool *pgxpool.Pool, since time.Duration, workerType string) (Summary, error)
   ```
   `ReportForWorker` keeps its signature and becomes `Summarize` + the existing
   `fmt.Fprint` block. `cmd/classify report` is untouched.

3. `internal/capture` (new file `attribution.go`, or appended to
   `rulesreport.go`):
   ```go
   type DayAttribution struct {
       Day                            time.Time
       Matched, Unmatched, Unevaluated int
   }
   // AttributionTrend buckets INBOUND normalized_messages by created_at::date
   // over the last `days` days by the LATEST capture decision for each message.
   func AttributionTrend(ctx context.Context, pool *pgxpool.Pool, days int) ([]DayAttribution, error)
   ```
   Query shape (the latest-decision lateral is `store.go`'s spelling; the LEFT
   join is what makes `Unevaluated` a third state rather than a silent merge):
   ```sql
   SELECT nm.created_at::date,
          count(*) FILTER (WHERE latest.action IS NOT NULL AND latest.action <> 'unmatched'),
          count(*) FILTER (WHERE latest.action = 'unmatched'),
          count(*) FILTER (WHERE latest.action IS NULL)
     FROM normalized_messages nm
     LEFT JOIN LATERAL (SELECT cd.action FROM capture_decisions cd
                         WHERE cd.message_id = nm.id
                         ORDER BY cd.id DESC LIMIT 1) latest ON true
    WHERE nm.direction = 'inbound'
      AND nm.created_at >= now() - $1::interval
    GROUP BY 1 ORDER BY 1 DESC
   ```

4. `internal/availability/store.go`: `loadAccountStates` becomes exported as
   ```go
   func CalendarSyncStates(ctx context.Context, pool *pgxpool.Pool) ([]AccountState, error)
   ```
   with `loadAccountStates` kept as a one-line internal alias **or** its call site
   updated — implementer's choice, one body either way. This is safe and is NOT
   the door SWT-24 closed: it reads `sync_runs` only, performs no free/busy
   answer, and `TestAvailability_ExportsNoUncheckedEventLoader` bans exactly one
   spelling (`func LoadEvents(`), which this is not. Bonus: `AccountState`'s doc
   comment already claims "the SQL that produces these lives in store.go
   (**LoadAccountStates**)" — today that comment names a function that does not
   exist; this makes it true.

5. `internal/tools/proposeslots.go`: extract the `AVAIL_MAX_SYNC_AGE` block out
   of `availabilityConfig` into
   ```go
   func MaxCalendarSyncAge() (time.Duration, error) // default 1h; error on non-duration or <= 0
   ```
   called by `availabilityConfig` (unchanged behaviour, same error strings, same
   tests) and by the dashboard. **Rejected alternative:** the dashboard reading
   `os.Getenv("AVAIL_MAX_SYNC_AGE")` with its own `ParseDuration` — a second
   parse drifts, and the drift's symptom is a green page while `propose_slots`
   refuses, which is worse than no page. `internal/availability` is not a
   candidate home: its package doc forbids env and clock reads (SWT-24
   criterion 10). `internal/dashboard` importing `internal/tools` is acyclic —
   no non-test file in `internal/tools` imports the dashboard, and
   `cmd/dashboard` already links both.

## MQTT topics

None. This ticket publishes and subscribes to nothing. (Worker/fleet state is
`worker_heartbeats` + `ops/workers/{id}/status`, and a fleet view is out of
scope — see below.)

## Files likely to touch

New:
- `internal/dashboard/funnel.go` — handler, row types, the four loaders, the
  pure helpers (`funnelFreshness`, `fillDays`, `runSections`, `clampDays`).
- `internal/dashboard/templates/funnel.html` — copy `templates/sources.html`'s
  head/style/nav block verbatim (same `.num` / `.warn` / `.bad` / `.ok` /
  `.muted` / `.headline` classes). Section order per criterion 23.
- `internal/dashboard/funnel_test.go` — pure helpers, the no-POST/no-executor
  scan, and criterion 22's `sync_runs`-moved-out scan (with its control).
- `internal/dashboard/funnel_integration_test.go` — build tag `integration`.
- `internal/classify/summary.go` — `Summary`, `Flag`, `Summarize`.
- `internal/capture/attribution.go` — `DayAttribution`, `AttributionTrend`.
- `internal/capture/attribution_integration_test.go`.

Modified:
- `internal/dashboard/server.go` — one `mux.Handle("GET /funnel", ...)` line
  next to the `/sources` line. That line carries the comment block explaining
  why the ingestion page exists; **update it rather than writing a second
  rationale** — after criterion 22 its claim ("the only page that reads the
  raw/normalized tables") is shared with `/funnel`, and the split between the two
  pages belongs in that comment.
- `internal/dashboard/sources.go` — criterion 22: drop the five `sync_runs`
  scalar subqueries, the five `sourceRow` fields and their scans.
- `internal/dashboard/templates/sources.html` — criterion 22: drop the `Last run`
  and `Status` columns (fix the empty-state `colspan`, currently 13), add the
  one-line pointer to `/funnel`.
- `internal/dashboard/templates/*.html` — nav line, 7 files (criterion 20).
- `internal/classify/report.go` — `ReportForWorker` reduced to rendering;
  `reportSkipped`'s fold moves into `Summarize`, its **printing stays here**.
- `internal/availability/store.go` — export the account-states loader.
- `internal/tools/proposeslots.go` — extract `MaxCalendarSyncAge`.
- `docs/runbooks/imap-mail-connector.md` — its "per-account ingest health (or
  just open the dashboard's /sources page)" line now points at `/funnel` for
  freshness. `docs/runbooks/calendar-availability.md` — one line saying the
  calendar sync age is visible on `/funnel`. Both cheap, and it is how the page
  gets found.

## In scope / Out of scope

**In scope:** the four read-only sections on one page, the `?days=` window, the
`/sources` column removal (criterion 22), the three package seams
(`classify.Summarize`, `capture.AttributionTrend`,
`availability.CalendarSyncStates`), the `MaxCalendarSyncAge` extraction, the nav
link, unit + integration tests.

**Out of scope — do not bundle:**
- Any write, action, form, approval, retry, re-run or "normalize now" button.
  The page executes nothing (see invariant 3).
- Any OTHER change to `/sources`. Criterion 22 is a deletion plus one line;
  its lifetime totals, channel table, headline numbers and empty states stay
  exactly as they are.
- **Fleet / worker heartbeat view.** `worker_heartbeats` and
  `ops/workers/{id}/status` are build-order step 3's surface; an MQTT subscriber
  in the dashboard is a different ticket with a different failure mode.
- **Triage's shadow diff** (`internal/triage/report.go`). Same shape, tempting to
  add as a fifth section; it is a separate population with its own contract, and
  triage's live-attach path is inert (IK). Future work.
- **Going live** with anything: capture mode, classify lanes, triage. This page
  is one input to those decisions and changes none of them.
- **`normalized_events` / calendar contents.** The busy set is not shown, ever
  (criterion 18). Calendar appears only as a sync-freshness row.
- Charts, JS, HTMX polling, auto-refresh, a JSON/CSV export of the funnel, or a
  new export route. Tables and a manual reload.
- A separate `/classify` page (Q2-b, rejected) — do not "tidy" the long page by
  splitting it later in the same ticket.
- Indexes, migrations, materialized views, a `funnel_stats` cache table
  (invariant 2's sibling-table smell in a reporting costume).
- Alerting: nothing emails, publishes, or creates a task when a connector goes
  stale. A stale-connector alarm is a real want and a real ticket (it needs a
  fire-once marker with a re-arm — IK).

## Invariants that apply

1. **Raw-first** — *not modified, and must stay that way.* The page READS
   `raw_source_items`; it must never write, backfill, stamp `normalized_at`, or
   offer a re-normalize control. The `raw / normalized / pending` distinction
   `/sources` displays is meaningful only because the invariant holds.
2. **One funnel** — *applies as a prohibition.* No new table, no view, no
   materialized rollup, no `funnel_stats`. Every number is computed from
   canonical tables at request time. A cached rollup is a second place the truth
   lives and is exactly what this invariant forbids in report clothing.
3. **Everything through the executor** — *applies vacuously, and that is the
   point.* The page performs **no tool calls**, so there is nothing to route
   through validate → policy → audit. The risk this invariant guards against
   here is a "small" action creeping onto a read page (retry a sync, mark a run
   ok, requeue normalization) and reaching the DB directly because the page
   already holds a pool. Criterion 2 makes that mechanically visible: no POST
   under `/funnel`, no `s.ex` in the funnel source. If an action is ever wanted
   here, it goes through `s.execute(...)` like `/deliveries` does, not around it.
4. **Nothing external without a delivery row** — *not touched.* The page sends
   nothing. It does not read `deliveries` either (that surface is `/deliveries`).
5. **Own-message loop closure** — *applies to how the numbers are counted.*
   Outbound rows are our own sends re-entering through ingestion; they are
   excluded from the capture-attribution section for the structural reason that
   `capture` only ever decides `direction='inbound'`, so an outbound row's
   missing decision means "not applicable", never "pending" (criterion 15). The
   intake section counts both directions and labels them as such — that is a
   volume question, not a decision question.
6. **Stealth attribution** — *not applicable.* Nothing on this page is
   client-visible: it renders internal operational counts behind Keycloak, and
   emits no drafts, commits or comments. Stated explicitly so a reviewer does not
   have to guess.
7. **Orchestrator purity** — *not touched;* the orchestrator is not modified. The
   adjacent rule that DOES bind: the dashboard must not import a provider adapter
   or call a model. The classify section reads STORED verdicts from
   `ai_extractions`; it never invokes ollama, never reaches
   `OPS_LOCAL_PROVIDER_URL`, and a page load must cost zero GPU seconds.

Additional repo rules that bind this ticket:

- **The one-reader rule** (`internal/availability/callsites_test.go`): the funnel
  code must not name `normalized_events` outside a whole-line `//` comment. The
  scan skips whole-line comments only — a trailing comment on a struct field
  trips it.
- **One spelling per fact**: the latest-capture-decision predicate, the classify
  verdict fold, the calendar readiness rule and the `AVAIL_MAX_SYNC_AGE` parse
  are each reused, not restated; and connector health moves rather than being
  copied (criterion 22). This is the repo's recurring defect and the reason four
  of the changes above exist.
- **Vocabulary**: `raw_source_items`, `normalized_messages`, `sync_runs`,
  `capture_decisions`, `ai_runs`, `ai_extractions`, `source_accounts`,
  worker types `classify` / `classify_residue`, actions `unmatched` /
  `attributed` / `task` / `task_log`, phases `imap` / `gmail` / `calendar` /
  `slack_web`. No synonyms, no invented phase names.

## Sibling patterns to copy

- **Handler + direct-SQL read shape:** `internal/dashboard/sources.go` — scalar
  subqueries with `COALESCE(...,'')` into string fields, `rows.Err()` checked,
  `s.tmpl.ExecuteTemplate` at the end. Copy the shape; change the error handling
  per criterion 17 (that file 500s the whole page, which is what this ticket
  fixes for a four-section page).
- **Multi-filter GET page:** `internal/dashboard/board.go` (`boardQuery`) for
  parameter handling and positional-arg building.
- **Template:** `internal/dashboard/templates/sources.html` — head, styles, nav,
  `{{range}}...{{else}}` empty-state rows with a `colspan` and a `.muted`
  explanation. Its two caption paragraphs are the register to write in.
- **Structural test:** `internal/dashboard/board_structure_test.go` (in-package,
  reads `templateFS`) for criteria 2 and 22's scans;
  `internal/availability/callsites_test.go` for the "control assertion so the
  scan cannot pass vacuously" idiom — a source scan needs a positive control.
- **Integration harness:** `internal/dashboard/dashboard_integration_test.go` —
  `dashGuard` (skip without `DATABASE_URL`, **fatal** on `192.168.50.49`),
  `newDashServer` (real `executor` + `dashboard.NewAuth(ctx,"","","","")` dev
  mode + `httptest` + cookie jar + `/dev/login?user=salvo`), FK-ordered cleanup
  keyed on a test-owned prefix, deferred both before and after seeding.
- **The report folds being reused:** `internal/classify/report.go`
  (`ReportForWorker`, `reportSkipped`) and `internal/capture/rulesreport.go`
  (`latestDecisions`, `topCounts`) — read both before writing any SQL; the
  comments in them are the argument for their exact shape.
- **Readiness:** `internal/availability/store.go` `loadAccountStates` + `NotReady`
  in `availability.go`, and `internal/tools/proposeslots.go`
  `availabilityConfig`.

## Decisions made unilaterally

(Q1 and Q2 are Salvador's, recorded in `funnel-view_OPEN_QUESTIONS.md`. These
are the SPEC's own.)

1. **Extract `classify.Summarize` rather than piping `ReportForWorker` into a
   `<pre>`.** The `<pre>` costs nothing and guarantees no second spelling, but it
   gives no links, no styling and no per-lane layout, and the request explicitly
   asks for the report "rendered as HTML" with a flags list. The refactor keeps
   one spelling because the SQL and the fold both move — nothing is copied.
2. **Day buckets use the pipeline clock** (`ingested_at`, `created_at`,
   `capture` decisions bucketed by their message's `created_at`), not `sent_at`.
   All three series then share one axis, and "nothing arrived on the 6th" is
   readable across them. `sent_at` answers "when was it written" and a backfill
   smears it over years.
3. **Non-calendar staleness is a constant (3h), not an env var.** It gates
   nothing; an env knob implies a contract. If it cries wolf, change the constant.
4. **Three attribution states, not two.** Forced by the capture contract, but
   recorded here because "matched vs unmatched" is what the request said and this
   SPEC ships three columns.
5. **`?days=` default 14, clamped `[1,90]`.** 90 days of daily rows is still one
   screen of table; unbounded lets a URL parameter schedule a full scan.
6. **Flags capped at 50 per lane**, newest first — `reportListLimit = 20` in
   capture exists for the same reason ("a report that prints 16,000 unmatched
   senders is one nobody reads"); 50 fits a page and stays scannable.

## Verification protocol

Before commit, in this order.

**1. Unit.**
```
go test ./...
```
Must include, newly green: the pure helpers (`funnelFreshness`, `fillDays`,
`clampDays`, the section collector), the structural no-POST/no-executor scan,
criterion 22's "sources.go no longer names `sync_runs`" scan **with its positive
control**, and the classify characterization test. Must stay green:
`TestReports_ShareTheNoFallbackNote`,
`TestNormalizedEventsHasExactlyOneReader`,
`TestAvailability_ExportsNoUncheckedEventLoader`, and the classify migration
guard (`0020` is still the highest and this ticket adds none).

**2. Integration** (compose db on :5433 — **never** 192.168.50.49):
```
make integration      # db-up + migrate + go test -tags integration -p 1 ./...
```
One assertion per section, each keyed to its own fixture:
- *connector health*: seed a `source_accounts` row `itest-funnel-a@local` plus
  two `sync_runs` rows — `status='ok', stats={"phase":"imap"}, finished_at =
  now() - interval '10 minutes'` and `status='error'` **newer** than it. Assert
  the page shows the 10-minute-old run (last SUCCESSFUL, not latest) and marks
  it fresh. Add a `phase='calendar'` row at `now() - interval '9 hours'` on a
  `provider='google', calendar_in_availability=true` account and assert `stale`.
- *intake*: insert raw items at `now()`, `now() - interval '2 days'` and
  `now() - interval '40 days'`; assert the first two appear in their day rows,
  the third does not at `?days=14`, and that a day between them renders a zero
  row rather than being absent.
- *capture*: seed three inbound messages — one with a latest `attributed`
  decision, one with a latest `unmatched` decision, one with none — plus one
  OUTBOUND message with no decision. Assert 1/1/1 in the three columns for
  today's row **for the seeded set**, and that the outbound message is in no
  column.
- *classify*: seed `ai_runs(worker_type='classify_residue', status='ok')` +
  `ai_extractions.fields` with one `actionable:true` and one `false`, plus one
  `status='skipped'` run carrying `avail_reasons`. Assert flagged=1,
  classified=2, the skip reason rendered, and the sender/subject of the flagged
  row present. Assert the same for `worker_type='classify'` and that neither
  lane's numbers appear under the other.
- *no-blank-page*: assert the page is 200 and contains all four section headings,
  in the criterion-23 order.
- *`/sources` still works*: assert `GET /sources` is 200, still shows the seeded
  account's raw/message counts, and **no longer** contains the `Last run` header.

All timestamps relative (`now() - interval '...'`); no literal dates anywhere.
**Cross-pollution pact** (IK, "integration suites cross-pollute"): these tables
are shared with 19 other suites under `-p 1`. Assert *containment* of seeded
values and *per-fixture* counts, never a global equality like "the page shows 3
accounts". Clean up in FK order, scoped by the `itest-funnel-` prefix, and note
that `capture_decisions.message_id` is `ON DELETE CASCADE`.

**3. Mutation checks** (the "test the column, not the fixture" rule):
- Drop `AND r.status='ok'` from the health query → the connector-health test must
  go red (it would otherwise be certifying a failing run as a sync).
- Change the capture fold's `LEFT JOIN LATERAL` to `JOIN` → the "not yet
  evaluated" assertion must go red.
- Delete a string literal from `report.go`'s output → the characterization test
  must go red.
- Re-add a `sync_runs` subquery to `sources.go` → criterion 22's scan must go red.

**4. Manual smoke against production data** (read-only page; safe):
```
eval "$(grep '^export OPS_DATABASE_URL=' ~/.bashrc)"
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/dashboard   # :8085, OIDC unset -> dev login
curl -s -c /tmp/j 'http://127.0.0.1:8085/dev/login?user=salvo' >/dev/null
curl -s -b /tmp/j 'http://127.0.0.1:8085/funnel?days=14' | head -100
curl -s -b /tmp/j 'http://127.0.0.1:8085/sources' | grep -c 'Last run'   # want 0
```
Bind/keep it on localhost: with `OIDC_ISSUER` unset `/dev/login` hands a session
to anyone who reaches it (IK, deploy notes). Cross-check each section:
```
psql "$OPS_DATABASE_URL" -c "SELECT a.provider, a.account_email, sr.stats->>'phase' AS phase,
       max(sr.finished_at) FROM source_accounts a JOIN sync_runs sr ON sr.source_account_id=a.id
 WHERE sr.status='ok' AND sr.finished_at IS NOT NULL GROUP BY 1,2,3 ORDER BY 1,2,3"
psql "$OPS_DATABASE_URL" -c "SELECT ingested_at::date, count(*) FROM raw_source_items
 WHERE ingested_at >= now() - interval '14 days' GROUP BY 1 ORDER BY 1 DESC"
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify report --lane residue --since 336h | head -30
DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/classify report --lane personal --since 336h | head -30
go run ./cmd/opsctl capture-rules report --since 2026-08-25
```
The classify totals on the page must equal the CLI's for the same window — that
equality is the whole claim of criterion 11. (`cmd/classify` reads
`DATABASE_URL`, not `OPS_DATABASE_URL` — IK landmine.)

**5. Cost measurement** (feeds the index decision, does not gate the ticket):
```
psql "$OPS_DATABASE_URL" -c "EXPLAIN ANALYZE <the intake query>"
curl -s -o /dev/null -w '%{time_total}\n' -b /tmp/j 'http://127.0.0.1:8085/funnel'
```
Record the number in the delivery comment. If the whole page exceeds ~2 s on
production data, **ship anyway** and open a follow-up SPEC for the index; do not
add a migration inside this ticket.

**6. Review.** `/ticket-review funnel-view` — go-reviewer against the seven
invariants, with attention to criterion 2 (nothing executes), criterion 18 (no
`normalized_events`) and criterion 22 (the deletion actually landed, and
`/sources` still renders). No codex pass needed: no executor, policy or delivery
code is touched.

## Future work (not this ticket)

- Stale-connector alarm (task or email) once the page proves which thresholds
  are real. Needs a fire-once marker WITH a re-arm on every new attempt (IK).
- Triage shadow diff as a fifth section, if triage is ever taken off shadow.
- A funnel JSON export for the morning brief.
- Per-day raw counts on an index, if step 5 says the page is slow.
- Fleet view (`worker_heartbeats` / MQTT) — build-order step 3's surface.
- A dedicated `/classify` page (Q2-b) if the funnel page becomes unreadable —
  the `classify.Summarize` seam already makes it a route, not a refactor.
