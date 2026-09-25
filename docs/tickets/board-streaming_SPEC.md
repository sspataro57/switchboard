> Jira: SWT-89

# board-streaming: the board updates in place when tasks change, pushed from Postgres over SSE, with no polling reload

## Source

Ad-hoc request from Salvador, 2026-09-25, swb task **#657**:

> I don't like the task board polling it's frustrating. let's make it streaming push the changes to it

This is not a build-order step. It changes how SWT-52 (`board-status-lights`, D15 auto-refresh), SWT-57
(`board-layout-compact`, L6) and SWT-67 (`board-departures`, B12/B13/B21) refresh `/tasks`. It must keep
their data contracts. Several of their structure pins get deliberately amended; the list is in Part 7.

## Goal

Replace the board's timed full-page reload with server push. A Postgres trigger NOTIFYs a
board-only channel on every change to a table the board reads. One LISTEN connection per dashboard
process fans that change out to every open board over Server-Sent Events. Each board then re-fetches
its own server-rendered `/tasks` URL and swaps the changed regions into the live page. Go stays the
only author of every fact on the page.

**Usable alone means:** with migration 0044 applied and `deployment/dashboard` on the new image, and
nothing else changed, Salvador has `/kiosk` (or `/tasks?refresh=on`) open on the tablet and:

- closes a task from his laptop, or a Claude session signals `working`, or capture marks activity on a
  task. The tablet shows the change within about 2 seconds. The page does not reload, and only the
  rows that changed flip.
- the panel he is watching stays on the page it was on. Paging keeps turning every 9 s.
- if he has a row's `⋯` actions popup open, it stays open and untouched. The update lands when he
  closes it.
- the kiosk shell stays full-screen throughout.
- the elapsed column, the stale ring and the midnight rollover still move with nothing written,
  within a minute.
- if the stream is down (pod restart, proxy hiccup), the footer says so and the board polls in place
  every 5 s. It goes back to live on its own when the stream returns.
- after a deploy, the board picks up the new page version by itself.

## What exists (code-read, 2026-09-25)

- **The reload loop** (`templates/tasks.html`, the one `<script>`) arms only when `data-refresh="on"`.
  `tick()` calls `location.replace(data-reload)` every `boardRefreshInterval` (5 s). `busy()` postpones
  it while any of these holds: the tab is hidden, any `details[open]`, a focused form control, a dirty
  control, or a panel not back on page 1 (`pageCycleBusy`, i.e. `cycleDone` plus every panel on page
  1). The worst-case lag is `longest panel's pages × 9 s + 5 s`, which is about a minute with a
  six-page queue. **This lag is the complaint.**
- **`boardKeys`** = project, status, assignee_type, subproject, refresh. `boardBack`,
  `boardRefreshURLs` and `boardAdvanced` iterate it. `ReloadURL` is the query rebuilt over
  `boardKeys` with refresh=on and never carries flash. D15: every interval is a Go const, never a URL
  value.
- **`/kiosk`** (`kiosk.go`, `templates/kiosk.html`) frames `/tasks?refresh=on[&project=]`. Only the
  SHELL goes full-screen, because a reload ends the document's own full-screen (B21). The manifest's
  `start_url` is `/tasks?refresh=on`.
- **The existing NOTIFY** (`migrations/0003_orchestrator.sql`): the `AFTER INSERT ON task_events FOR
  EACH ROW` trigger `task_events_notify` runs `pg_notify('task_events', NEW.id::text)`. Its payload is
  the event id only. `orchestratord` LISTENs on it through `Engine.Listen`, which uses `pool.Acquire`
  and `WaitForNotification`. It treats a notification as a wake-up only; the cursor drain is the
  delivery path.
- **Board-visible changes that write NO `task_events` row** (verified by reading every `UPDATE tasks` /
  `INSERT INTO tasks` and every `insertTaskEvent` call site):
  - `task_mark_activity` (`internal/tools/activity.go:108`): only `activity_at` and
    `activity_by_message_id`. This is what moves a task into INCOMING.
  - `create_task` (`internal/tools/createtask.go:259`): a plain `INSERT INTO tasks`, no event.
    `create_child_task` and `apply_plan_import` do write events.
  - `task_signal` refresh (`internal/tools/signal.go:222`): the same state and session moves
    `working_state_at`, which clears a stale ring, and returns "no event".
  - `task_requeue`'s `reviewed_at` stamp (it does write a `reviewed` event), `task_set_source_thread`
    (`provenance.go:88`, moves `updated_at`, so the time cell changes), and `task_mark_surfaced`.
  - Rows in `classify_promotions` (the `from_message` fact, which puts a task in INCOMING),
    `external_refs` (the `pr_review` fact) and `task_dismissals` (the dismissal code, the "reopened
    after dismissal" marker, and `closed_today` visibility). The promoter writes its promotion row
    AFTER `create_task`, so a promoted task shows in QUEUE first and moves to INCOMING only when that
    row lands.
  - Priority changes DO write `priority_changed`, and signal state changes DO write
    `working_state_changed`.

  So `task_events` is not a complete change feed for the board, and LISTENing on it would miss the
  INCOMING path entirely.
- **Every `tasks` write lives in `internal/tools`**, but those tools run in many PROCESSES: the connector
  CronJobs (capture), the resident watchers, orchestratord, the MCP servers, opsctl and the dashboard.
  An in-process Go notify inside the dashboard would never see a connector's write.
- **Changes nothing writes at all**, all computed at render time on the DB clock: elapsed minutes, the
  2 h stale ring (`tools.WorkingLease`), the local-midnight rollover of LANDED TODAY, the `updated`
  stamp's HH:MM to date flip, and the orchestrator health alert (`pg_locks` plus backlog).
- **Tables the board's SQL reads** (`boardQuery`, `reopenMarkers`, `boardLightFacts`, and the project
  list in `listTasks`): `tasks`, `projects`, `task_dismissals`, `classify_promotions`, `external_refs`,
  `normalized_messages` (a PK join for the activity sender and channel).
- **Serving:** `cmd/dashboard/main.go` builds `http.Server{ReadHeaderTimeout: 10s}` with **no
  `WriteTimeout`**. A write timeout would kill a long-lived stream. `staticCacheHeaders` passes the
  writer through untouched. The Deployment has `replicas: 1` with `strategy: Recreate`. The two
  Ingresses (`dashboard` http, `dashboard-tls` https, class nginx) carry only `proxy-body-size`.
  ingress-nginx's defaults are `proxy-read-timeout 60s`, and response buffering is disabled per
  response by an upstream `X-Accel-Buffering: no`.
- **Pool:** `store.NewPool` is `pgxpool.New` with defaults, so `MaxConns = max(4, NumCPU)`.
- **Latest migration:** `0043_slack_channel_mentions.sql`. `internal/classify/structure_test.go`
  keeps the ledger of owned migration numbers (up to 43).

## Decisions made unilaterally (with rationale)

### S1: Transport: Server-Sent Events from the dashboard, not WebSocket, not MQTT-over-WS

- **SSE** is one-way (server to browser), which is all this needs. It is stdlib Go: `text/event-stream`
  plus `http.NewResponseController(w).Flush()`, with no new dependency. It rides the existing session
  cookie and `s.auth.Require`. It needs no Upgrade handling through ingress-nginx. The browser's
  `EventSource` reconnects by itself, with `retry:` set by the server.
- **Not WebSocket:** nothing flows browser to server, a WS library would be a new dependency, and the
  ingress would need upgrade handling.
- **Not MQTT-over-WS** from `192.168.50.45:9001`. That is a third-party origin, so
  `TestTasksTemplate_NoThirdPartyURL` forbids it and it fails "the page makes no off-host request". It
  would also need broker auth for browsers. The changes originate in Postgres, not in the fleet.
- **Not HTMX's SSE extension:** the board has no HTMX, pinned by `TestTasksTemplate_NoBranchNoHTMXNoRawHTML`.

### S2: The source of change: a new board-only NOTIFY channel fed by row triggers, not `task_events`

- Migration **0044** adds one function, `board_changed_notify()`. It runs
  `pg_notify('board_changed', TG_TABLE_NAME || ':' || <row id>)`, taking the id from NEW, or from OLD
  on DELETE. It is attached to exactly the four tables whose rows change what the board shows:
  **`tasks`, `task_dismissals`, `classify_promotions`, `external_refs`**. Each table gets:
  - `AFTER INSERT OR DELETE … FOR EACH ROW`;
  - `AFTER UPDATE … FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)`, so a no-op UPDATE is silent.
- **Why a trigger and not Go-side `pg_notify` calls in the tools:** the writers run in about ten
  processes, plus opsctl, psql and old binaries during a rollout. A trigger is the only place that sees
  every write. A later writer cannot forget it. The SWT-68 landmine is exactly "a later ticket's writer
  voids an earlier ticket's display argument".
- **Why not reuse `task_events`:** it is incomplete (see What exists): `task_mark_activity`,
  `create_task`, signal refreshes and the promotion, ref and dismissal rows never reach it. Also, the
  orchestrator treats every `task_events` notification as a drain wake-up, and its payload contract is
  "an event id". Board traffic does not belong on that channel. 0003 is untouched.
- **Why a row trigger and not a statement trigger:** a statement trigger fires on zero-row UPDATEs.
  The row trigger's `WHEN` clause also filters no-op UPDATEs.
- **Why the payload names the row:** the dashboard ignores the payload. A notification is a wake-up
  only, which is the orchestrator's discipline, and the fetch re-reads everything. The table:id form
  exists so the integration tests can wait for THEIR row. That keeps negative tests ("no notification
  for my no-op UPDATE") robust on a database other suites write to. The cost: Postgres de-duplicates
  identical payloads within one transaction, so a transaction touching N rows queues N notifications
  instead of one. The hub coalesces them (S4), and the largest known writer, `apply_plan_import`, is
  tens of rows.
- **Deliberately not triggered, and covered by the tick (S5) instead:**
  - `normalized_messages`. It is the ingestion firehose (tens of thousands of rows), and the board
    reads only the sender and channel of the message a task's `activity_by_message_id` points at. That
    pointer is a `tasks` write, so it already fires.
  - `projects`. Its slugs change about once a month.
  - The orchestrator health inputs (`pg_locks`, `task_events` backlog, `orchestrator_cursor`).
  - Every time-driven fact.
- **A structure test ties the two lists together** (criterion 4). Every table named after `FROM` or
  `JOIN` in the board's SQL must be in `boardNotifyTables` or `boardTickOnlyTables`. So a future board
  ticket that reads a new table fails until someone decides which list it belongs in.
- **New failure mode, stated:** `pg_notify` inside a trigger can fail a `tasks` write only if the
  notification queue is full. The queue holds 8 GB of unread notifications by default, and this
  channel's volume is thousands of short payloads a day. The trigger does nothing else.

### S3: What is pushed: a payload-free change signal. The browser re-fetches its own URL and swaps server HTML

- The stream carries **no task data**. It sends only `event: change` with an opaque counter as `data`,
  plus heartbeat comments. The board's facts reach the browser only through the existing
  `GET /tasks` render, with the same code, the same auth, the same `boardKeys` URL and the same
  tests.
- **Why not push rendered HTML or data over the stream:**
  - Each tab has its own filters, so the server would need a second render entry point per stream.
  - A hidden tab could not skip work, because the server cannot see visibility.
  - "Go computes every fact" would gain a second path to police.
- **One global signal, not a filtered one.** A project-filtered board still depends on global facts:
  the blue queue head is chosen over ALL ready tasks (SWT-52 D2). A per-filter signal would be a
  second spelling of the board's query.

### S4: Coalescing: in the hub (leading edge plus a floor), and single-flight in the browser

- **Hub:** a burst of notifications becomes broadcasts on a leading edge
  (`boardLiveDebounce = 250 * time.Millisecond` after the first notification). Broadcasts then come
  at most once per `boardLiveMinGap = 2 * time.Second` for as long as notifications keep arriving.
  There is always a **trailing** broadcast after the last notification, so no change is left
  unannounced. For a single change, the tablet updates in about 250 ms plus one render. A capture
  pass writing for ten seconds costs each tab about six renders, not hundreds.
- **Browser:** at most one fetch in flight. A signal that arrives during a fetch sets `pending`, which
  triggers exactly one follow-up fetch.

### S5: Time-driven facts: a hub tick every 60 s

`boardLiveTick = 60 * time.Second`. The hub broadcasts a `change` on this cadence whether or not
anything was written. This moves elapsed minutes, the stale ring, the midnight rollover, the time
stamps and the orchestrator alert. It is today's staleness for time-only facts, about one render a
minute per visible tab. It is also the safety net: under any undetected hub failure (a half-open TCP
connection, a lost NOTIFY), the board is never more than a minute stale.

### S6: The swap: `fetch` plus `DOMParser`, replacing named server-rendered regions. Never `innerHTML`

- The script fetches `data-reload` (the existing `ReloadURL`) with `credentials: "same-origin"` and
  `cache: "no-store"`. It parses the text with `new DOMParser().parseFromString(text, "text/html")`,
  which gives an inert document: its scripts do not run and nothing loads. It then replaces each live
  region with `document.importNode(newRegion, true)` through `replaceWith`. **`innerHTML`,
  `outerHTML` assignment, `insertAdjacentHTML`, `document.write`, `createContextualFragment` (which
  makes scripts executable), `eval`, `new Function` and `srcdoc` are all banned** in the script.
- **The live regions are marked in the template** with `data-live="<name>"`. There are exactly five:

  | name | element | why |
  |---|---|---|
  | `alert` | a new always-rendered wrapper `<div data-live="alert">` around `{{with .OrchAlert}}…{{end}}` | the alert comes and goes; the wrapper keeps a stable anchor |
  | `tally` | `<div class="tally" data-live="tally">` | the five counts |
  | `main` | `<main data-live="main">` | the panes, panels, rows and their popups |
  | `counts` | the ticker's `<span data-live="counts">Overall · …</span>` | the overall counts |
  | `indicator` | `<p id="auto-refresh" class="muted" data-live="indicator">` | "last refreshed HH:MM:SS" |

  Each region renders at template depth 0, or, for `indicator` only, inside the one
  `{{if .AutoRefresh}}` block. The fetched URL always carries refresh=on, so a fetched document has
  every region the live page has.
- **Not live, deliberately:**
  - the `.headbar`: the nav and the filter form, whose typed state must survive;
  - the sign header's title, clock, toggle and FULL button;
  - the `<script>` itself: a swap never replaces the script, and it never touches `body` or `html`;
  - the down-note (S9);
  - the flash (S7).
- **Structure mismatch or version change means one full reload.** Each render stamps
  `data-version="{{.BoardVersion}}"` on the script tag. `boardVersion` is the first 12 hex characters
  of the SHA-256 of the embedded `templates/tasks.html`, computed once at package init. The script
  falls back to `location.replace(target)` once, instead of swapping, in three cases:
  - the fetched document's script carries a different `data-version`, which means a deploy happened;
  - the fetched document lacks a live region the page has;
  - the response was redirected (`res.redirected`, i.e. the session expired and login answered) or is
    not `text/html`.

  A reload lands on login or on the new page. The login page carries no script (SWT-52 criterion 36),
  so this cannot loop.
- **Non-OK or network failure:** keep the page, do not reload, and retry on the next signal or poll.
  During a Recreate deploy's gap nginx answers 502/503. Reloading there would blank the tablet.

### S7: Keep what he is doing: popups, input, pages, scroll, flash

- **`busy()` postpones a swap** (the swap waits, it is not dropped) in four cases:
  - the tab is hidden;
  - an open `<details>` is **inside a live region** (a row's actions popup);
  - a focused form control is inside a live region;
  - a dirty control is inside a live region, keeping the `defaultSelected` rule verbatim.

  While a swap is postponed, the script re-checks every second and on `visibilitychange`. Scoping to
  live regions is new: an open Advanced-filter popup in the headbar no longer blocks updates, because
  a swap never touches it. A row popup left open still pauses updates, as it paused reloads (SWT-57
  L6). The "last refreshed" time keeps saying so.
- **The page-cycle clause is retired (B13, `pageCycleBusy` and `cycleDone`).** It existed only because
  a reload reset every panel to page 1. A swap preserves pages:
  - each panel's current page is recorded under its section key, read from the `<h2 id="section-…">`
    that Go renders;
  - it is restored after the swap and clamped to the new page count;
  - the paging timer, the tick counter and the flip rules otherwise carry on.

  The script must not spell the section keys. It reads them from the ids, and
  `TestTasksTemplate_NoIncoming` still bans the word anywhere in `tasks.html`.
- **Only changed rows flip.** A row counts as changed if its `href` is new to its panel, or if its
  `className` or its `.r` link's `textContent` differs from the old row with the same `href`. It then
  gets the existing `flip` class, for visible rows only. Unchanged rows do not animate. This is a
  comparison of what Go rendered twice, not a fact computed in JS.
- **Phone:** the window scroll position is left alone, with no `scrollTo`. Verify it in the smoke.
- **The flash** stays out of the live regions: a swap would erase a verb's receipt within a second of
  the redirect. The script sets `hidden` on `.flash` at the first swap that happens at least
  `data-interval` seconds (5 s) after page load. That matches today's rule that a flash lasts until
  the next refresh, and it gives a readable minimum.

### S8: The stream endpoint and the hub

- **`GET /tasks/stream`**, behind `s.auth.Require`, handled by `(*Server).boardStream`. Go 1.22's mux
  prefers this literal over `GET /tasks/{id}`, and a test pins that.
- The handler issues **no SQL and no executor call**. It:
  1. answers **503** with `Retry-After` when no hub is set or the hub is not listening;
  2. otherwise sets `Content-Type: text/event-stream`, `Cache-Control: no-cache` and
     `X-Accel-Buffering: no`, writes `retry: <boardLiveRetry in ms>` and flushes;
  3. then loops. On a hub broadcast it writes `event: change` + `data: <n>`. Every
     `boardLiveHeartbeat = 20 * time.Second` (under ingress-nginx's 60 s read timeout) it writes a
     `: ping` comment. It flushes after each write. It returns on client disconnect, on the request
     context, on a write error, or when the hub stops listening. The last case closes the stream, so
     browsers fall back honestly.
- **`BoardHub`** (`internal/dashboard/live.go`), one per process:
  - `NewBoardHub(pool)` builds it, `Run(ctx)` runs it, and `(*Server).SetBoardHub(h)` attaches it. The
    signature `NewServer(pool, ex, auth)` is unchanged, and tests that pass nil get the 503.
  - `Run` takes a connection with `pool.Acquire` and then **`Hijack()`s it out of the pool**, so a
    permanent LISTEN never shrinks the render pool. It sets
    `application_name = 'switchboard-board-live'` so `pg_stat_activity` shows it, runs
    `LISTEN board_changed`, and loops on `WaitForNotification`.
  - On any error it closes the connection, marks itself not listening (which ends every open stream),
    and reconnects with backoff (1 s doubling to 30 s). After a successful re-LISTEN it broadcasts once,
    because anything could have changed while it was blind.
  - The only statements it ever sends are the `SET` and the `LISTEN`.
- **Fan-out:** each subscriber gets a channel with capacity 1, and sends never block. If a subscriber
  already has a pending signal, the send is skipped, because one pending signal means "re-fetch".
  A slow or dead client can never stall the hub or the others. Unsubscribing happens in the handler's
  `defer`.
- **`cmd/dashboard/main.go`** builds the hub, starts `go hub.Run(ctx)` and calls
  `srv.SetBoardHub(hub)`. `http.Server` keeps **no `WriteTimeout`** (pinned). `/healthz` is
  unchanged: a blind hub must not get the pod that serves approvals restarted by its liveness probe.
  The board degrades to polling instead.
- **Constants**, in `live.go`: `boardChannel = "board_changed"`, `boardLiveDebounce`,
  `boardLiveMinGap`, `boardLiveTick`, `boardLiveHeartbeat`, and `boardLiveRetry = 5 * time.Second`
  (used for both the SSE `retry:` and the browser's re-open delay after a refused stream). None is
  ever a URL value, per the D15 rule. The hub and handler read them through a small config struct
  whose zero value means "the consts", so unit tests can shrink them without a request touching them.

### S9: The fallback, and what `?refresh=on` means now

- **`refresh=on` stays the switch** and is still the kiosk's and the installed app's URL. It now
  means **live**: the stream, plus in-place polling while the stream is down. Without it the board is
  static, as today. `boardKeys`, `boardBack`, `boardRefreshURLs`, `boardAdvanced`, the hidden
  `refresh` inputs, the toggle and its `{{if not .AutoRefresh}}` text are all unchanged. Rationale:
  - D15 made auto-refresh opt-in, and this keeps that decision;
  - every bookmark, the kiosk, the manifest and the installed app already say refresh=on;
  - making live the default would change `boardKeys` semantics that five tests and every verb form
    pin.

  This is reversible later (Future work).
- **Poll mode.** On `EventSource` `error`, the script enters poll mode: an in-place fetch and swap
  every `data-interval` (the existing `boardRefreshInterval`, 5 s, which is D15's accepted cost).
  - If `readyState` is `CONNECTING`, the browser keeps retrying by itself.
  - If it is `CLOSED` (a 503, a redirect to login, or a wrong content type), the script opens a new
    `EventSource` after `data-retry` seconds.

  On `open` it leaves poll mode and does **one catch-up fetch**, because changes may have been
  missed while disconnected, including the gap between page render and subscription.
- **What he sees:** a static, always-rendered `<p id="live-down" class="muted" hidden>live updates
  down — polling every {{.RefreshSeconds}} s</p>` in the ticker, outside `{{if .AutoRefresh}}` and
  outside every live region. The script toggles only its `hidden` attribute, so the words are
  server-rendered. The indicator's words change from `(every {{.RefreshSeconds}} s, last refreshed
  …)` to `(last refreshed {{.RenderedAt}})`, because "every 5 s" is no longer true. It stays a
  child-free `<p>`, so the two integration regexes that read it (`[^<]*` before the time) keep
  working.
- **Hidden tabs** keep the `EventSource` open (a goroutine, no SQL), fetch nothing, and do one fetch
  on becoming visible if a signal arrived in the meantime.

### S10: The kiosk shell stays, unchanged in behaviour

`/kiosk` still frames `/tasks?refresh=on`. The board no longer reloads on a timer, but full
navigations inside the frame still happen: a row tap to `/tasks/{id}`, a verb's POST and redirect, a
version reload (S6), and a login redirect. Each of those would end a full-screen that lived on the
board's own document. The FULL button's logic is unchanged. Only the comments in `kiosk.go` and
`kiosk.html` that say "the board reloads itself every few seconds" are corrected.

### S11: Load on Postgres (the D15 cost statement, revised)

| | Before (poll) | After (push) |
|---|---|---|
| Connections | 0 extra | **1 LISTEN per dashboard process** (replicas: 1), hijacked from the pool |
| Idle visible tab, no changes | 1 render per 5 s to 1 render per page cycle | 1 render per 60 s (tick) |
| Per change | nothing until the next reload | 1 render per visible tab, at most 1 per 2 s during bursts |
| Hidden tab | 0 | 0 (one catch-up render on show) |
| Stream down | n/a | 1 render per 5 s per visible tab (D15's accepted rate) |
| Trigger cost | none | one `pg_notify` per changed row of 4 small tables |

A render is still exactly one ordinary `/tasks` render, 6 to 8 statements (the SWT-52 table), with no
stream-only query. The stream handler issues none.

### S12: Browser connection limits

Each board tab holds one long-lived connection. Over HTTP/1.1 (the plain-http `switchboard.home.arpa`
host), a browser allows 6 connections per host, so six or more board tabs in one browser would starve
their own fetches. The tablet uses the https host, where ingress-nginx negotiates HTTP/2 and this does
not apply. This is a residual risk, and it is documented rather than engineered around.

**No open questions arose.** Every choice above follows from the code, the pinned contracts, or the
conventions (D15's opt-in, the wake-up-only NOTIFY discipline, "small, reversible, audited"). Each is
reversible in one place.

## Acceptance criteria

### Part 1: migration 0044 (`migrations/0044_board_changed_notify.sql`)

1. The file creates `board_changed_notify()` (plpgsql, `RETURNS trigger`). It calls
   `pg_notify('board_changed', TG_TABLE_NAME || ':' || <id>)`, taking the id from `OLD` on `DELETE` and
   from `NEW` otherwise, and returns `NULL` (the trigger is an AFTER trigger). Each of `tasks`,
   `task_dismissals`, `classify_promotions` and `external_refs` gets:
   - an `AFTER INSERT OR DELETE … FOR EACH ROW` trigger;
   - an `AFTER UPDATE … FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)` trigger.

   Nothing else is in the file: no table, no column, no change to `task_events_notify`, no trigger on
   any other table. It is forward-only.
2. The ledger in `internal/classify/structure_test.go` names 44 as this ticket's, and its `n != …`
   list gains `44`. If another branch takes 44 first, whichever merges second renumbers, as the
   ledger's own comments describe.
3. **Integration (applied schema), on an isolated DB:** a dedicated connection runs
   `LISTEN board_changed`. Then:
   - (a) an INSERT, a real UPDATE and a DELETE on each of the four tables each deliver a notification
     whose payload is `<table>:<that row's id>`;
   - (b) `UPDATE tasks SET title = title WHERE id = <mine>` delivers none for `tasks:<mine>` within
     500 ms (the WHEN clause);
   - (c) a transaction that updates my task and then rolls back delivers none;
   - (d) an INSERT into `normalized_messages` and an UPDATE of `projects` deliver nothing on
     `board_changed`;
   - (e) regression: a `task_events` insert still delivers exactly its id on channel `task_events`,
     and `board_changed` gets nothing for it;
   - (f) `pg_trigger` joined to `pg_proc` shows `board_changed_notify` attached to exactly the four
     tables, and to no other.

   Every wait filters on its own row's payload, so other suites' writes cannot fail it.

### Part 2: coverage: what the board reads is either triggered or ticked

4. `internal/dashboard/live.go` declares `boardNotifyTables = []string{"tasks", "task_dismissals",
   "classify_promotions", "external_refs"}` and `boardTickOnlyTables`, a map from table to reason, with
   `projects` and `normalized_messages` each carrying a one-line reason. A structure test (no DB):
   - (a) extracts every identifier following `FROM` or `JOIN` in the SQL string literals of
     `boardQuery`, `reopenMarkers`, `boardLightFacts` and `listTasks`, skipping subqueries such as
     `FROM (SELECT`;
   - (b) requires each identifier to be in exactly one of the two lists;
   - (c) reads `migrations/0044_*.sql` and requires its triggered tables to equal `boardNotifyTables`
     exactly.

   Adding a `JOIN deliveries` to `boardLightFacts` must turn it red.
5. **Integration: the paths that write no `task_events` wake the board.** Through the real executor
   (the `lightsExecutor` pattern), a LISTEN connection receives `tasks:<id>` for each of:
   - `task_mark_activity` on an open task (an inbound message fixture);
   - `create_task`;
   - a `task_signal` **refresh**, meaning the same state and session, where the tool reports
     `changed:false`;
   - `task_set_priority`;
   - `task_requeue`.

   It also receives `classify_promotions:<id>` for a promotion row insert, and
   `external_refs:<id>` for `link_external_ref`. This is the column-fed rule: the reason the channel
   exists must be proven against Postgres, not a fixture.

### Part 3: the hub (`internal/dashboard/live.go`)

6. **Coalescer, unit (deterministic, injected timer and clock):**
   - (a) one notification produces one broadcast, at `debounce` and not before;
   - (b) 100 notifications evenly over 10 s produce at most `ceil(10s / minGap) + 2` broadcasts, and
     at least 5;
   - (c) the last broadcast comes at or after the last notification (the trailing edge);
   - (d) no notification for `tick` produces exactly one broadcast per `tick`.
7. **Fan-out, unit:**
   - (a) a subscriber that never reads does not delay a second subscriber's receipt, measured with a
     bounded wait;
   - (b) a subscriber's channel never holds more than one pending signal;
   - (c) after N subscribe/unsubscribe cycles, `Subscribers()` is 0;
   - (d) marking the hub not-listening closes every subscriber's `Done` channel.
8. **Integration (isolated DB):**
   - (a) `Run` against the test pool reaches listening, and `pg_stat_activity` shows exactly one
     backend with `application_name = 'switchboard-board-live'` whose query is
     `LISTEN board_changed`;
   - (b) after `pool.Stat().TotalConns()` settles, the pool's own count does not include it (it was
     hijacked);
   - (c) a `task_mark_activity` through the executor reaches a subscriber within 2 s;
   - (d) `pg_terminate_backend(<that pid>)` makes the hub report not-listening, closes open
     subscriptions, and within 5 s (test config) it reconnects, reports listening, and broadcasts
     once;
   - (e) cancelling `Run`'s context releases the connection (the backend is gone from
     `pg_stat_activity`).
9. **Structure:**
   - `live.go` contains no `INSERT`, `UPDATE`, `DELETE`, `ex.Execute`, `executeTask` or
     `internal/tools` call;
   - its only SQL strings are the `SET application_name` and the `LISTEN`;
   - `boardChannel` is spelled `"board_changed"` exactly once in Go (in `live.go`), and the migration
     spells it once.

### Part 4: the stream handler and wiring

10. `server.go` registers `mux.Handle("GET /tasks/stream", s.auth.Require(http.HandlerFunc(s.boardStream)))`.
    A request for `/tasks/stream` reaches `boardStream`, not `showTask`, which the test proves with a
    nil-hub 503 rather than a 404. A request without a session is redirected like every other
    authenticated route.
11. **Handler, unit (fake hub, `httptest`):**
    - (a) with no hub, or a hub that is not listening, it returns 503 with `Retry-After` and no
      event-stream content type;
    - (b) otherwise the headers are exactly `Content-Type: text/event-stream`, `Cache-Control:
      no-cache` and `X-Accel-Buffering: no`;
    - (c) the first bytes are `retry: 5000` followed by a blank line;
    - (d) a hub broadcast produces `event: change` and a `data:` line;
    - (e) with a 50 ms test heartbeat, `: ping` lines arrive;
    - (f) cancelling the request context returns the handler and unsubscribes;
    - (g) the hub going not-listening ends the response.
12. **Handler, structure:** `boardStream`'s body contains no `s.pool`, `Query`, `Exec`, `QueryRow`,
    `executeTask` or `ex.`, and reads no URL query value. The stream takes no parameters at all.
13. **End to end (integration, `httptest.Server` over the real `Handler()`, real hub, isolated DB):**
    log in through `/dev/login`, open `GET /tasks/stream` with the cookie, then `task_mark_activity`
    through the executor. An `event: change` line is read from the live response body within 3 s,
    before the handler returns. This proves the flush works through `staticCacheHeaders` and
    `auth.Require`.
14. **`cmd/dashboard/main.go`, structure:**
    - it constructs the hub, starts `hub.Run` in a goroutine and calls `SetBoardHub`;
    - the file contains no `WriteTimeout` (a write deadline cuts every stream);
    - `ReadHeaderTimeout` is unchanged.

### Part 5: the page (`templates/tasks.html`, `board.go`)

15. `boardData` gains `StreamURL string` (always `"/tasks/stream"`), `RetrySeconds int` (from
    `boardLiveRetry`) and `BoardVersion string` (from `boardVersion`). `listTasks` sets all three from
    consts or vars and never from the request. The existing `TestListTasks_RefreshIsARenderFlagNotAQuery`
    bans still hold, and it gains a check that none of the three comes from `r.URL`.
    `boardQuery`, `boardBack`, `boardRefreshURLs`, `boardAdvanced` and `boardURL` do not mention any
    of them, which extends `TestBoardDepartures_UntouchedHelpersStayUntouched`'s `newNames` list.
16. `boardVersion` equals `hex(sha256(embedded templates/tasks.html))[:12]`. It is non-empty and
    computed once. A unit test recomputes it from `templateFS`.
17. **The script tag** keeps its four SWT-52/67 attributes and gains `data-stream="{{.StreamURL}}"`,
    `data-retry="{{.RetrySeconds}}"` and `data-version="{{.BoardVersion}}"`. There is still exactly
    ONE `<script` in the file, at template depth 0.
18. **The live regions (template structure):**
    - exactly five `data-live="…"` attributes, named `alert`, `tally`, `main`, `counts` and
      `indicator`, each exactly once;
    - each at template depth 0 except `indicator`, which sits inside the one `{{if .AutoRefresh}}`
      block;
    - no live region contains `<script`, `<form class="filters"`, `id="clock"`, `id="fs"`,
      `id="auto-refresh-toggle"`, `id="live-down"` or `{{if .Flash}}`;
    - `{{with .OrchAlert}}` sits inside the `alert` region;
    - the `.headbar` contains no `data-live`;
    - B15's band order (`TestTasksTemplate_HeaderBandsInOrder`) is unchanged.
19. **The indicator and the down-note:**
    - the indicator is exactly `<p id="auto-refresh" class="muted" data-live="indicator">auto-refresh on
      (last refreshed {{.RenderedAt}})</p>`, still the only content of the one `{{if .AutoRefresh}}`
      block;
    - `<p id="live-down" class="muted" hidden>live updates down — polling every {{.RefreshSeconds}}
      s</p>` appears exactly once in the ticker, after the indicator block and before
      `class="board-notes"`, outside every conditional.
20. **Script contract (structure).** The script:
    - (a) contains `EventSource`, `fetch(`, `DOMParser`, `parseFromString(`, `"text/html"`,
      `importNode(`, `replaceWith(`, `[data-live`, `data-version`, `res.redirected` (or
      `.redirected`), `location.replace(`, `setTimeout`, `document.hidden`, `details[open]`,
      `activeElement`, `defaultValue`, `defaultSelected`, `requestFullscreen`, `wakeLock` and
      `textContent`;
    - (b) contains none of `innerHTML`, `outerHTML =`, `insertAdjacentHTML`, `document.write`,
      `createContextualFragment`, `eval(`, `new Function`, `srcdoc`, `XMLHttpRequest`, `htmx`,
      `location.search`, `location.href`, `localStorage`, `sessionStorage` or `onchange`;
    - (c) contains exactly one `fetch(` and exactly one `new EventSource(`;
    - (d) `arm()` still returns early on `!refreshOn`, so no stream opens and nothing is fetched on a
      plain board;
    - (e) `busy()` still names `document.hidden`, `details[open]`, `activeElement` and `dirty(`, and
      no longer names the page cycle (`pageCycleBusy`/`cycleDone` are gone from the file).
21. **The rest of the page is unchanged:**
    - `TestTasksTemplate_NoIncoming`, `…NoThirdPartyURL`, `…NoBranchNoHTMXNoRawHTML` (with `live.go`
      added to its `template.HTML` scan), `…VerbFormsByteUnchanged`,
      `…FullButtonOpensTheKioskShell`, and every `boardKeys`/`boardBack`/`boardRefreshURLs` test pass
      unmodified;
    - the one-`onchange` and inline-handler counts are unchanged.

### Part 6: integration of the page

22. `GET /tasks?refresh=on`:
    - the rendered script carries `data-stream="/tasks/stream"`, `data-retry="5"` and a 12-hex
      `data-version`;
    - the five live regions are present, `#live-down` is present with `hidden`, and the indicator
      matches `id="auto-refresh"[^>]*>[^<]*\d{2}:\d{2}:\d{2}`.

    `GET /tasks` (refresh off) renders `data-refresh=""`, no indicator, and still the other four
    regions.
23. **Swap parity:** two renders of the same URL with no write between them produce byte-identical
    `data-live` regions, except the indicator's time. So a tick swap flips no row. The test extracts
    the regions from both bodies and compares them.
24. After `task_mark_activity` on a queued task, a re-render of the same URL moves that row's `href`
    from the `queue` panel's region into the INCOMING panel's region. The test finds panels through
    their `section-…` ids, built from Go's section keys and never spelled as the literal word in the
    test either, so the scan stays honest. This is the fetch the browser will make.

### Part 7: existing tests amended deliberately (each gets a comment naming this ticket)

25. Each of these tests changes in the way described:
    - **`board_refresh_test.go` `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript`:**
      - the indicator golden becomes criterion 19's;
      - `fetch(` moves from the banned list to "exactly once";
      - the new banned tokens of 20(b) join;
      - the new data attributes of 17 are required;
      - everything else is unchanged.
    - **`board_layout_structure_test.go` `TestTasksTemplate_ScriptContract`:**
      - `fetch(` moves from the banned list to the required list;
      - criterion 28's page-not-1 clause is **removed**, and B13 is retired by S7 (a swap preserves
        pages);
      - the required tokens of 20(a) are added.
    - **`board_departures_structure_test.go`:**
      - `TestTasksTemplate_ByteUnchangedFragmentsSurviveTheRestyle`: the indicator fragment becomes
        criterion 19's;
      - `TestTasksTemplate_OneAutoRefreshConditionalWrappingTheIndicatorOnly`: unchanged, and it must
        still pass, because the block still wraps exactly one `<p`;
      - `TestBoardDepartures_UntouchedHelpersStayUntouched`: additive, per criterion 15.
    - **`board_layout_structure_test.go` `TestTasksTemplate_NoBranchNoHTMXNoRawHTML`:** additive, with
      `live.go` joining the scanned files.
    - **`kiosk_test.go`:** unchanged. Only kiosk comments change.
    - **Integration** (`board_refresh_integration_test.go`, `board_departures_integration_test.go`):
      the indicator regexes survive unmodified. If one pins "every", amend it to criterion 19's
      words.

## Data model changes

**Migration 0044 (`migrations/0044_board_changed_notify.sql`):** one trigger function,
`board_changed_notify()`, and eight triggers (INSERT/DELETE plus UPDATE-when-changed on `tasks`,
`task_dismissals`, `classify_promotions` and `external_refs`), all on the new channel
`board_changed`. No table, no column, no index, no change to 0003's `task_events_notify`. No new task-like
structure. The ledger in `internal/classify/structure_test.go` gains 44.

## API / MCP tool changes

- **No MCP tool, no executor tool, no policy rule, no pin.**
- **One new HTTP route:** `GET /tasks/stream` (auth required, read-only, text/event-stream). Events:
  - `retry: 5000` once;
  - `event: change` / `data: <opaque counter>` per coalesced change or tick;
  - `: ping` comments every 20 s.

  It takes no parameters and carries no task data.
- `GET /tasks` renders three new script data attributes, five `data-live` markers, the reworded
  indicator and the hidden down-note. Its SQL is unchanged.
- The board's verbs (`POST /tasks/{id}/dismiss|close|requeue|attach`) are untouched.

## MQTT topics

None. MQTT-over-WS was considered and rejected (S1).

## Files likely to touch

New:
- `migrations/0044_board_changed_notify.sql`
- `internal/dashboard/live.go`: consts, `boardNotifyTables`, `boardTickOnlyTables`, the coalescer,
  `BoardHub` (`NewBoardHub`, `Run`, `Subscribe`, `Listening`, `Subscribers`), `(*Server).boardStream`,
  `(*Server).SetBoardHub`, `boardVersion`
- `internal/dashboard/live_test.go`: criteria 6, 7, 11, 16
- `internal/dashboard/board_live_structure_test.go`: criteria 4, 9, 10, 12, 14, 15, 17 to 20
- `internal/dashboard/board_live_integration_test.go` (`//go:build integration`): criteria 3, 5, 8, 13,
  22 to 24
- At deliver: `docs/runbooks/HANDOFF-kube-board-streaming.md`

Changed:
- `internal/dashboard/templates/tasks.html`: the data attributes, the five `data-live` markers, the
  `alert` wrapper, the indicator words, `#live-down`, and the script (stream, poll mode, swap, busy
  scoping, page preservation, flash hiding). B13's `pageCycleBusy`/`cycleDone` are removed.
- `internal/dashboard/board.go`: `boardData` (+3 fields) and `listTasks` (sets them).
- `internal/dashboard/server.go`: the `Server.live` field and the route.
- `cmd/dashboard/main.go`: hub construction, `go hub.Run(ctx)`, `SetBoardHub`.
- `internal/dashboard/kiosk.go`, `templates/kiosk.html`: comments only.
- The Part 7 tests, and `internal/classify/structure_test.go` (ledger).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`:
  - a new "Board streaming (board-streaming)" entry covering the channel, the trigger list and its
    coverage test, the hub, the SSE rules (no WriteTimeout, `X-Accel-Buffering`, 20 s heartbeat), the
    five regions, the version reload, and the HTTP/1.1 six-connection residual;
  - a one-line pointer from the SWT-52 and SWT-67 entries, saying D15's reload loop and B13 are
    superseded;
  - an update to the SWT-52 load table.

Deliberately NOT touched: `migrations/0003_orchestrator.sql`, `internal/orchestrator`,
`internal/tools` (no Go-side notify anywhere), `boardQuery`, `boardLightFacts`' SQL,
`reopenMarkers`, `lights.go`, `sections.go`, `display.go`, `export.go`, `auth.go`, the manifest and
static assets, and `kube/` (the kube session owns manifests; see Verification step 7).

## In scope / Out of scope

**In scope:**
- migration 0044;
- the hub, the stream route and its wiring;
- the in-place swap, poll-mode fallback, version reload, page preservation, changed-row flip and flash
  hiding;
- the indicator rewording and the down-note;
- the tests and amendments above;
- the IK entry and the kube handoff.

**Out of scope** (each named because it is tempting to bundle):
- **Live by default on every `/tasks` view** (retiring the toggle and `refresh`'s opt-in). This is
  Future work; it changes `boardKeys` semantics.
- **The nav's `Board` link dropping refresh=on** inside the installed app (the IK rough edge).
  Unchanged.
- Streaming any other page (`/tasks/{id}`, `/deliveries`, `/funnel`, `/sources`).
- A per-row DOM patch protocol, a JSON board API, or client-side rendering of any fact.
- Changing what the board shows: `lightFor`, `sectionFor`, `boardSectionOf`, the queue order, the
  tallies, the remarks, `boardQuery`, or the exports.
- New board verbs or any change to Dismiss, Done, Requeue or Attach.
- OIDC, TLS or Ingress changes. An ingress `proxy-buffering` annotation is only a contingency (step 7).
- Pushing to the worker fleet, the swb-push nudges, or MQTT.
- Suppressing his own edits from resurfacing (SWT-72 residual), and anything else in
  SWT-52/57/59/67/72's own Future work.

## Invariants that apply

1. **Raw-first:** not exercised. No connector, no ingestion. The one migration adds a trigger that
   performs no write.
2. **One funnel:** no table and no queue-like structure. The hub's state is in-memory (a counter and a
   subscriber set) and holds no task data. The stream carries none either. Queues stay filters on
   `tasks`, rendered by the unchanged `GET /tasks`.
3. **Everything through the executor:** the new surface is **read-only by construction**:
   - `boardStream` runs no SQL and no executor call (criterion 12);
   - the hub's only statements are `SET application_name` and `LISTEN` (criterion 9);
   - the trigger only calls `pg_notify`;
   - the browser's fetch is the existing authenticated `GET /tasks`.

   No write path is added. The four board verbs keep their `executeTask` calls and byte-unchanged
   forms. The stream exposes nothing an agent could call: it is not an MCP tool and takes no
   arguments.
4. **Nothing external without a delivery row:** nothing is sent. The page still makes no off-host
   request (`TestTasksTemplate_NoThirdPartyURL`, and S1's rejection of the broker's WS port).
5. **Own-message loop closure:** untouched. No normalizer or matcher changes, and
   `normalized_messages` deliberately carries no trigger.
6. **Stealth attribution:** nothing client-visible. The dashboard is Salvador's own LAN board, and no
   AI marker appears in the markup, the stream or `application_name`.
7. **Orchestrator purity:**
   - no file under `internal/orchestrator` changes;
   - the orchestrator's channel, trigger, payload contract and cursor drain are untouched, and
     criterion 3(e) pins that a `task_events` insert still notifies exactly its id on `task_events`;
   - board traffic never lands on the orchestrator's channel, so it causes no spurious drains;
   - the hub is dashboard code and makes no decisions: coalescing is timing, not a rule, and it
     writes no audit row because it performs no action.

## Sibling patterns to copy

- **LISTEN loop:** `internal/orchestrator/engine.go` `Engine.Listen` (`pool.Acquire`,
  `LISTEN`, `WaitForNotification`, return on ctx). Copy its shape, then add `Hijack()` and the
  reconnect backoff that the orchestrator leaves to its caller (`cmd/orchestratord/main.go`).
- **The NOTIFY trigger:** `migrations/0003_orchestrator.sql` (function plus
  `AFTER … FOR EACH ROW EXECUTE FUNCTION`). Same idiom, new channel.
- **NOTIFY in an integration test:** `internal/orchestrator/integration_test.go` criterion 3 (a
  dedicated connection LISTENs BEFORE the write, then waits with a deadline).
- **The real executor for writes in dashboard integration tests:** `lightsExecutor`, `lsCall`,
  `lsSignal` (`board_lights_integration_test.go`). Mark-activity fixtures: the SWT-72 suites
  (`board_activity_integration_test.go`).
- **Template structure scans:** `tasksHTML`, `templateBlockAfter`, `templateDepthAt`, `elementEnd`,
  `funcBodySrc`, `readSrc`, `parseDashboardSource(...).reach` (the `board_*_structure_test.go`
  files).
- **The column-fed rule:** criterion 5 is the SWT-21/SWT-64 lesson. The path this ticket exists for,
  an activity mark with no event, is proven against Postgres, and the mutation "LISTEN on
  `task_events` instead" must turn it red.
- **Not used:** jobagent's `FOR UPDATE SKIP LOCKED` (nothing is claimed), and rag-svc's HTMX handlers
  (the board bans HTMX, and SSE here is stdlib).

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|---|---|
| Drop the `WHEN (OLD.* IS DISTINCT FROM NEW.*)` clause | criterion 3(b) |
| Drop the `classify_promotions` triggers | criteria 3(a), 3(f), 4(c), 5 |
| Hub LISTENs on `task_events` instead of `board_changed` | criteria 5 (activity, create, signal refresh), 8(c), 13 |
| Add a trigger on `normalized_messages` | criteria 3(d), 3(f), 4(c) |
| Add `JOIN deliveries` to `boardLightFacts` without classifying it | criterion 4(b) |
| Coalescer broadcasts once per notification | criterion 6(b) |
| Coalescer drops the trailing edge | criterion 6(c) |
| Subscriber send made blocking | criterion 7(a) |
| Hub does not close subscribers when blind | criteria 7(d), 8(d), 11(g) |
| Hub reconnects without the catch-up broadcast | criterion 8(d) |
| `Acquire` without `Hijack` | criterion 8(b) |
| Handler omits `X-Accel-Buffering: no` | criterion 11(b) |
| Handler omits the `Flush` after an event | criterion 13 |
| Handler runs `s.pool.QueryRow(...)` | criterion 12 |
| Route registered without `s.auth.Require` | criterion 10 |
| `WriteTimeout: 30 * time.Second` added to `cmd/dashboard/main.go` | criterion 14 |
| `boardLiveTick` read from a query parameter | criterion 15 |
| Swap written with `innerHTML` / `insertAdjacentHTML` / `createContextualFragment` | criterion 20(b) |
| `data-live` put on `<body>` or on a wrapper that contains the `<script>` | criterion 18 |
| `data-live="alert"` placed inside `{{with .OrchAlert}}` | criterion 18 (depth) |
| `busy()` loses `details[open]` | criterion 20(e) |
| Script spells a section key literally (for example the INCOMING key) | `TestTasksTemplate_NoIncoming` |
| Indicator gains a child `<span>` before the time | criterion 22 and the existing integration regexes |
| `boardVersion` hard-coded | criterion 16 |

## Verification protocol

Run in this order. Do not commit before step 5 passes. Gate on exit status: capture `rc`, never
`go test | grep`.

1. **Unit:** `go test ./...`. The SWT-48 `TestAttributionTrend_*` flake (20:00 to 24:00 EDT) is
   pre-existing; re-run with `TZ=UTC` if it fires.
2. **Integration, ISOLATED database.** The shared compose db is a landmine here, because LISTEN tests
   see other suites' writes:

   ```
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_boardstream"
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_boardstream?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardstream?sslmode=disable' \
     go test -tags integration -p 1 -count=1 ./internal/dashboard/ ./internal/orchestrator/
   ```

   Run it twice (rerunnable), then `go test -tags integration -p 1 ./...` once against the same URL.
3. **Mutations:** each row above goes red, then is reverted.
4. **Manual NOTIFY check**, against the isolated db:
   - in one psql, run `LISTEN board_changed;`;
   - in another, `opsctl call --tool task_mark_activity …` on a seeded task;
   - the first psql prints `tasks:<id>` on its next command. `UPDATE tasks SET title=title …`
     prints nothing.
5. **Local smoke in a real browser** (Playwright, `channel="chrome"`: IK "verify UI work in a real
   browser"). Run `DATABASE_URL=<ops_boardstream url> go run ./cmd/dashboard` (:8085,
   `/dev/login?user=salvo`) and seed a throwaway project with about 25 human ready tasks so the queue
   pages.
   - **Stream:** `curl -N -b <cookie> localhost:8085/tasks/stream` shows `retry: 5000`, then `: ping`
     every 20 s, and `event: change` about 250 ms after an `opsctl` write.
   - **Tablet 1000×700, `/kiosk?project=<slug>`**, tap full screen, then:
     - wait for the queue panel to reach `page 2/…`;
     - from a terminal: `task_signal working` on a row in view, `task_mark_activity` on a queued row,
       `create_task`, and `task_close` of a queued row;
     - each change is visible within about 2 s;
     - the panel stays on page 2 (clamped if it shrank), and only changed rows flip;
     - the shell stays full-screen;
     - DevTools Network shows `fetch` calls to `/tasks?…refresh=on` and **no document reload**.
   - **Popup:** open a row's `⋯` and make a change from the terminal. The popup stays open and intact.
     Close it, and the update lands within about 1 s.
   - **Advanced filter** popup open in the headbar: updates still land (S7 scoping).
   - **Flash:** press Done in a row. The flash shows, stays at least 5 s, then goes at the next update.
   - **Stream down:** stop the dashboard process and restart it.
     - The footer shows "live updates down — polling every 5 s" while it is down.
     - The board neither blanks nor reloads during the gap.
     - On restart the note hides, and one catch-up fetch happens.
     - Edit `tasks.html` (whitespace) and restart. The board does exactly one full reload (version
       change) and lands full-screen inside the shell.
   - **Session expiry:** delete the session cookie in DevTools, then trigger a change. One
     `location.replace` lands on the login page, with no loop.
   - **Time-only:** with no writes, the elapsed cell of a `working` row advances within 60 s.
   - **Phone 390×844:** scroll halfway, make a change. The update lands and the scroll position holds.
   - **Hidden tab:** switch tabs, make three changes, switch back. Exactly one fetch.
   - Drop the db afterwards.
6. **Load check:** after the smoke, `pg_stat_activity` on the isolated db shows exactly one
   `switchboard-board-live` backend, idle in `LISTEN`, and no statement shape that `/tasks` did not
   already issue.
7. **Deploy (handoff to the kube session:** `docs/runbooks/HANDOFF-kube-board-streaming.md`).
   - Build and push `192.168.50.20:5000/switchboard:<tag>` here.
   - **Apply 0044 to pg-main first.** Check `SELECT max(version) FROM schema_migrations` against
     `ls migrations/`: the IK's drifted-db landmine.
   - Then bump only `deployment/dashboard`'s tag. No env var, no port, no probe change, and no
     Ingress annotation expected.
   - The order is soft:
     - a new image on a pre-0044 db simply never gets a notification and falls back to the 60 s
       tick;
     - 0044 under the old image fires notifications that nobody LISTENs to.

   **Post-roll:**
   - on the tablet, the https host `/kiosk` updates within about 2 s of an `opsctl` change;
   - through BOTH hosts,
     `curl -N -b <cookie> https://switchboard.sspataro.com/tasks/stream` shows a `: ping` inside
     20 s (ingress buffering is off);
   - if the ping arrives in a lump or only at close, the kube session adds
     `nginx.ingress.kubernetes.io/proxy-buffering: "off"` to both Ingresses. That is a contingency,
     not a planned change.
8. **Rollback:** the previous image tag. 0044 can stay: its triggers are harmless with no listener.
   Every URL, bookmark, the kiosk and the installed app keep working, because no key changed.

## Future work (not this ticket)

- **Live by default on every `/tasks` view:** retire the refresh toggle, and fix the installed app's
  nav-link rough edge along the way.
- **Stream the task detail page** (`/tasks/{id}`) with the same hub. Its events list is a natural
  consumer.
- **Hub health on `/funnel`** (listening or not, last notification age, subscriber count), beside the
  orchestrator section.
- **A per-row patch** (swap only changed rows instead of all of `<main>`), if a large board ever makes
  the full-region swap visibly costly.

## Open questions

None. See S1 to S12. Every ambiguity (transport, signal source, what is pushed, the swap mechanism,
coalescing, fallback, the kiosk, `?refresh=on`, the Postgres load, and the flash) is decided above with
its rationale, and each decision is reversible in one place.
