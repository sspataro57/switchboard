> Jira: SWT-101

# watch-json: a token-gated JSON read of the board's lights for the Pebble face

## Source

Ad-hoc, swb task 865. The Pebble watchface session queued it on Salvador's behalf. The swb watchface
(`~/projects/personal/pebble`, README "The swb route") needs this reply from
`https://switchboard.sspataro.com/watch.json`:

```json
{ "need_you": 2, "in_flight": 3, "incoming": 4, "done_today": 5,
  "waiting": [{"session": "pebble", "since": 1790000000}],
  "working": [{"session": "kube", "since": 1790000000}] }
```

Both lists are oldest first, and `since` is in Unix seconds. The courier sends
`Authorization: Bearer <token>`. The courier treats ANY reply as "home" (401, 404, 502, a login
page) and only a missing reply as "away" (pebble `src/pkjs/config.example.js`, README "Home or away").
It reads `waiting[0]`, `working[0]` and the largest `since` in `waiting` (pebble `src/pkjs/index.js`).
The host is LAN-only: there is no public DNS, and the Pi-hole resolves it to the ingress at
192.168.50.51 (IK "LAN DNS for `*.sspataro.com` is the PI-HOLE"). It must never be exposed to the
internet.

## Goal

Add `GET /watch.json` to the dashboard binary. It is authenticated only by a constant-time bearer
token from `SWB_WATCH_TOKEN`, and it returns the board header's own four tallies plus the session
names whose light is red or yellow. It does this by reusing the `/tasks` pipeline, with no second
SQL.

**Usable alone:** after the kube handoff (Secret + env var), Salvador puts the token in the pebble
`config.js`, installs the face, and the watch shows the same need-you / in-flight / incoming /
done-today numbers as the board, names the waiting and working sessions, and buzzes when a new
session starts waiting.

## Decisions (made here, with rationale)

**D1. The counts come from one spelling: `boardTallies`.** `listTasks` (board.go) builds rows in
this order: `boardRows` → `reopenMarkers` → `boardLightFacts` → the `taskRow` loop (`lightFor`,
`incomingKind`, …) → `boardSections` → `boardTallies`. The row-and-section part moves out of
`listTasks`, unchanged, into
`func (s *Server) boardView(r *http.Request) (secs []boardSection, facts map[int64]lightFacts, renderedAt string, err error)`.
`listTasks` calls it and keeps its display work (`boardPanes(data.Sections)`,
`boardTallies(data.Sections)`, refresh, projects, health). The watch calls the same `boardView`
and then `boardTallies(secs)`. The JSON maps
`need_you=NeedYou, in_flight=InFlight, incoming=Incoming, done_today=DoneToday`. `Queued` and
`Open` are not sent.
- The watch always renders the **unfiltered default board**. `boardQuery` reads
  `r.URL.Query()`, so the handler passes a clone with `URL.RawQuery = ""`. A query string on
  `/watch.json` never changes the reply. The four numbers therefore equal the header of a bare
  `/tasks` (no filters), including "done today" at `BoardTimeZone` midnight.

**D2. Sessions come from the lights, not from a new query.** Session membership follows each row's
`light`, which `lightFor` already computed:
- `waiting`: rows with `Light.Class == "input"` and `Light.Session != ""`. This is the session
  `needs_input` row. A worker's `needs_feedback` red has no session tag, so it is counted in
  `need_you` but is not listed.
- `working`: rows with `Light.Class == "working"` and `Light.Session != ""`. This is a fresh
  session `working` row. **Stale leases are excluded:** a `working` signal older than
  `tools.WorkingLease` (2 h) already has class `stale` (the board's "no signal"). The watch's
  yellow must not be kept alive by a session that has probably died. Such a task still counts in
  `in_flight`, because the tally counts both `working` and `stale`, exactly like the header. So
  `len(working)` can be less than `in_flight`. That is expected: claimed/in_progress worker rows
  are in `in_flight` too.
- **Name** = `facts[id].Session`, the raw stored name. Never use `Light.Session`, whose fallback
  is the display text "session unknown". An **empty name** is a pre-0036 marker: it is left out of
  the lists and still counts in the tallies. The watch shows names, and a made-up "unknown" could
  collide with a real window name.
- **since** = `working_state_at` as Unix seconds. This needs one additive, display-only column in
  `boardLightFacts`: `COALESCE(EXTRACT(EPOCH FROM t.working_state_at)::bigint, 0) AS state_unix`.
  It goes into a new `lightFacts.StateUnix int64`, and `lightFor` never reads it. It is the same
  statement, so it adds no SQL.
- **Duplicates:** one entry per session name. Within a list, keep the **oldest** `since`: it shows
  how long that session has waited, and the courier's "newest" buzz keys on a session entering the
  list, not on a second task. Across lists, **waiting wins**: a name in `waiting` is removed from
  `working`. A session binds one task, so this only happens with a leftover marker, and red is the
  safer error.
- **Order:** `since` ascending, then name ascending (deterministic). There is no cap: the courier
  reads the head and the max, and the lists are small.
- The logic is one pure function in `watch.go`:
  `func watchSessions(secs []boardSection, facts map[int64]lightFacts) (waiting, working []watchSession)`.
  It does no I/O and reads no clock (invariant 7's discipline, applied to a display helper).

**D3. The token.**
- Source: env `SWB_WATCH_TOKEN`, from a k8s Secret (see Kube handoff). `cmd/dashboard/main.go`
  calls `srv.SetWatchToken(os.Getenv("SWB_WATCH_TOKEN"))` before `srv.Handler()`, following the
  `SetBoardHub` pattern. The value is `strings.TrimSpace`d, because Secret values often carry a
  trailing newline.
- **Disabled = 404:** an unset or blank value, or one shorter than 32 bytes after trimming,
  leaves the route disabled. When the value is set but too short, a single `slog.Warn` without the
  value says so. Disabled means `http.NotFound` for every request, whatever the header says. **The
  route is registered anyway**, because otherwise `GET /watch.json` would fall through to
  `GET /` → `s.auth.Require` → a 302 to the login page, not a 404.
- **Check:** `Authorization` must be `Bearer <token>`. The scheme is case-insensitive
  (RFC 6750), and surrounding whitespace is trimmed. The server stores `sha256(token)`, and the
  comparison is
  `subtle.ConstantTimeCompare(sha256(presented), storedDigest) == 1`. Comparing equal-length
  digests means neither the value nor its length leaks through timing. A token in the query string
  or a cookie is never accepted.
- **Wrong or missing token → 401**, `Content-Type: text/plain; charset=utf-8`,
  `WWW-Authenticate: Bearer`, `Cache-Control: no-store`, and body `unauthorized\n`. No counts, no
  names, no JSON. The token and the presented header are never logged.
- **Not behind `s.auth.Require`, not behind the dev login, not behind `s.demoScoped`.**
  Registration:
  `mux.Handle("GET /watch.json", s.watchAuth(http.HandlerFunc(s.watchJSON)))`, with a comment
  explaining why it is open to the session layer. `watchAuth` does the 404/401 gate; `watchJSON`
  does the read.

**D4. Demo mode: the watch always shows the real board. This is an explicit OFF scope.**
Recommended over following demo mode because:
- The watch is on Salvador's wrist, never on a shared screen or tablet. Demo mode exists to filter
  what a client sees on the dashboard (SWT-99 Goal).
- The payload has four integers and his own session names (tmux window or folder names). It has
  no titles, projects, bodies or accounts.
- A demo is exactly when he is not looking at the board. A demo-filtered watch would silently drop
  a waiting session on a hidden project (`personal`, `foundry`) and show a quiet green while
  something needs him.
- Following demo mode would also add a new 503 path, the flag read, to a device that cannot tell
  a 503 from "home".

  **This narrows SWT-99 decision 3 ("every dashboard surface is filtered") by one route.** If
  Salvador disagrees, the flip is to wrap the handler in `s.demoScoped` and drop the D4 test and
  the `demoOffRoutes` entry.

Mechanics:
- `demo.go` gains `watchScope(ctx) context.Context`, which returns
  `withDemoScope(ctx, demoScope{})` (off), and
  `var demoOffRoutes = map[string]string{"GET /watch.json": "<one-line reason>"}`. This keeps the
  exception in the file where visibility is spelled.
- Neither `demoExempt` nor `demoSeams` fits. The watch adds no unmarked literal, because it reuses
  `boardRows`/`boardLightFacts`, which already carry `@demo.task`/`@demo.message`. It also adds no
  pool-taking cross-package call. So no entry is added to either map, and the demo structure test
  passes unchanged.
- Without `watchScope`, `demoScopeFrom` would default to ON-with-empty-lists and every count
  would be 0 (criterion 2 of SWT-99: "nothing, never everything"). The explicit call is therefore
  required, and a test pins it (criterion 12).
- Because the scope is off, `tasks.demo_hidden` has no effect on the watch, exactly as on a
  demo-off board.

**D5. Read-only, GET only, no-store.** The route writes nothing, calls no executor tool, and
writes no audit row. The existing dashboard reads are not audited either, and invariant 3 governs
tool calls, of which there are none. The mux pattern `GET /watch.json` also serves HEAD. Any other
method gets the mux's 405. Every response from the handler (200, 401, 404, 503) carries
`Cache-Control: no-store`. The 200 has `Content-Type: application/json`. A `boardView` error
returns 503 `service unavailable` in plain text with no data, and the error is logged server-side
only.

## Acceptance criteria

Unit tests use no DB. Integration tests are build tag `integration`, skip without `DATABASE_URL`,
and run in a branch-owned database (`ops_watch_json`, per the IK compose landmine). They clean up
their own fixtures first and in `t.Cleanup`, and they delete any `demo_mode` row they create.

**Gate, unit (`watch_test.go`: `s.watchAuth(next)` around a recording `next`, so no pool is needed)**
1. With no token configured (`SetWatchToken("")`, `"   "` and a 31-byte value), every request
   returns 404, both with and without a bearer header. `next` is never called, and the body
   contains no `{`.
2. With a token configured: no `Authorization` header, `Bearer wrong`, `Basic <b64>`,
   `Bearer <token>x`, `Bearer <token[:len-1]>`, and `?token=<token>` without a header each return
   401. `next` is never called. Each 401 has `WWW-Authenticate: Bearer` and
   `Cache-Control: no-store`, and its body is exactly `unauthorized\n`.
3. `Bearer <token>`, `bearer <token>` (lowercase scheme) and `Bearer  <token> ` (extra whitespace)
   each call `next` exactly once.
4. Through `s.Handler()` on a pool-less server (the `funnel_test.go` construction),
   `POST /watch.json` and `PUT /watch.json` return 405 with no JSON body.
5. The token is never written to logs: capture `slog` output across criteria 1–3 and assert that
   the token string does not appear.

**Sessions, unit (`watch_test.go`, table-driven over hand-built `[]boardSection` + facts)**
6. `waiting` lists only session `needs_input` rows. A `needs_feedback` row (class `input`, no
   session tag) is not listed.
7. `working` lists only fresh session `working` rows. A `stale` row is not listed. A
   claimed/in_progress worker row is not listed.
8. A row whose stored `facts[id].Session` is empty is not listed, and a session named `unknown`
   is listed as `unknown`.
9. Session `s` on two waiting tasks (since 100, 200) → one entry `{s, 100}`. Session `s` waiting
   (300) and working (100) → `s` only in `waiting`, with 300.
10. Order is `since` ascending, then name ascending. With no rows, both lists marshal as `[]`,
    never `null`.

**Structure (`watch_structure_test.go`, plus deliberate amendments)**
11. `watchJSON`'s body calls `s.boardView(` and `boardTallies(`. It contains no SQL string literal,
    no `s.pool`, no `demoQuery`, no `s.ex`/`Execute(`, and no `INSERT`/`UPDATE`/`DELETE`.
    `watchAuth` uses `subtle.ConstantTimeCompare` and contains no `==` or `bytes.Equal` on the
    token or digest.
12. `server.go` registers `mux.Handle("GET /watch.json", s.watchAuth(http.HandlerFunc(s.watchJSON)))`,
    and that line contains neither `Require(` nor `demoScoped(`. `withDemoScope(` is called only in
    `demoScoped` and `watchScope`. `demoOffRoutes` has exactly one key, `GET /watch.json`, with a
    non-empty one-line reason. `watchJSON` calls `watchScope(`.
13. **Amended deliberately:** `TestBoardServer_StaticRouteIsOpenAndTheRestIsNot`
    (board_departures_structure_test.go, criterion 30) adds `/watch.json` to the open list, and
    only when wrapped in `s.watchAuth(`. The four `listTasks` body tests that assert row-building
    (`TestListTasks_BuildsSectionsAndAdvancedFilters`, `TestListTasks_FeedsTheDisplayHelpers`,
    `TestListTasks_SetsIncomingFromTheFacts`, `TestListTasks_SetsTheActivityFields`) read the
    concatenation of the `listTasks` and `boardView` bodies. Every regex and ban is otherwise
    unchanged. The `Get("refresh")` count stays exactly 1 across both bodies.

**Integration (`watch_integration_test.go`, against `ops_watch_json`)**
14. Seed a fixture project with one task per board section, plus session markers set by fixture
    SQL: a `needs_input` task (`s-wait`), a fresh `working` task (`s-work`), a `working` task with
    `working_state_at = now() - interval '3 hours'` (`s-stale`), and a `needs_input` task with
    NULL `working_session`. Then `GET /watch.json` with the token → 200, and the JSON has exactly
    the six keys (decode with `DisallowUnknownFields`).
15. **The same computation:** in the same DB state, the four integers equal the first, second,
    third and fifth `<b>` numbers of a bare `GET /tasks` sign header (logged in via the dev login),
    read the way `board_departures_integration_test.go` reads them. Also check them against a
    hand-counted oracle for the fixture rows. Because the branch DB holds only test fixtures, the
    oracle is exact.
16. `waiting` contains `s-wait` and not the NULL-session task. `working` contains `s-work` and not
    `s-stale`. The `since` of `s-wait` equals
    `EXTRACT(EPOCH FROM working_state_at)::bigint` read back by SQL.
17. `GET /watch.json?project=<other>&status=closed` returns a body byte-identical to criterion
    14's (apart from any row changes between calls; the test makes none).
18. **Demo off-scope (D4):** insert `demo_mode = {"on": true, "projects": []}` and set the fixture
    parent `demo_hidden = true`. `/watch.json` returns the same counts and lists as criterion 14,
    while a logged-in `/tasks` shows zero rows. A `demo_mode` row with an undecodable value still
    gives 200 with the real numbers, never 503.
19. The 200 carries `Cache-Control: no-store` and `Content-Type: application/json`. `HEAD` returns
    200 with no body.

**Mutations that must turn a test red**
- M1: compute the counts with a `SELECT count(*)` in `watchJSON` → 11.
- M2: `==` instead of `ConstantTimeCompare` → 11.
- M3: drop the disabled check → 1.
- M4: wrap the route in `s.auth.Require` → 12, 14 (302).
- M5: include `stale` in `working` → 7, 16.
- M6: remove the dedup, or let `working` win → 9.
- M7: remove the `watchScope` call → 12, 14 (all zeros), 18.
- M8: pass `r` unchanged to `boardView` → 17.
- M9: drop `no-store` → 2, 19.
- M10: sort descending → 10.
- M11: nil slices → 10 (`null`).
- M12: read `Light.Session` instead of `facts[id].Session` → 8 ("session unknown" listed).

## Data model changes

None. No migration. `boardLightFacts` gains one computed column, `state_unix`, over an existing
column (`tasks.working_state_at`, 0033).

## API / MCP tool changes

- New route `GET /watch.json` (dashboard only). Request: `Authorization: Bearer <token>`.
  Response 200:
  `{"need_you":int,"in_flight":int,"incoming":int,"done_today":int,"waiting":[{"session":string,"since":int64}],"working":[…]}`.
  Other responses: 401, 404, 405 or 503, as plain text with no data.
- No MCP tools. No executor path, because nothing is invoked (D5).

## MQTT topics

None.

## Files likely to touch

- `internal/dashboard/watch.go` (new): `watchAuth`, `watchJSON`, `watchSessions`, `watchSession`,
  `SetWatchToken`.
- `internal/dashboard/board.go`: extract `boardView` from `listTasks`, and add `state_unix` to
  `boardLightFacts`.
- `internal/dashboard/lights.go`: `lightFacts.StateUnix` (display-only; documented as not read by
  `lightFor`).
- `internal/dashboard/demo.go`: `watchScope`, `demoOffRoutes`.
- `internal/dashboard/server.go`: route registration, the `watchDigest []byte` field on `Server`.
- `cmd/dashboard/main.go`: `SetWatchToken`, and the env var in the header comment.
- Tests: `internal/dashboard/watch_test.go`, `watch_structure_test.go`,
  `watch_integration_test.go`; amended `board_departures_structure_test.go`,
  `board_layout_structure_test.go`, `board_incoming_structure_test.go`,
  `board_activity_structure_test.go`.
- `docs/runbooks/HANDOFF-kube-watch-json.md` (new).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`: a short entry covering the route, the token, the demo
  off-scope and the LAN-only rule.

## Kube handoff (`docs/runbooks/HANDOFF-kube-watch-json.md`)

The kube session owns the manifests (memory: "Kube manifests belong to the kube session").
1. Secret `ops/swb-watch-token`, key `token`, value from `openssl rand -hex 32`. Create it BEFORE
   rolling the image.
2. `deployment/dashboard`: env `SWB_WATCH_TOKEN` from `secretKeyRef {name: swb-watch-token, key: token}`.
   Nothing else changes (no port or probe change).
3. **No ingress change.** Both `ops/dashboard` and `ops/dashboard-tls` already route every path to
   the service. Add no public DNS record and no external exposure of any kind.
4. Roll the new tag to all workloads in one apply, as usual. There is no migration.
5. Check: `curl -s -o /dev/null -w '%{http_code}' https://switchboard.sspataro.com/watch.json` →
   401. With `-H "Authorization: Bearer $TOKEN"` → 200 and JSON whose numbers match the board
   header. Without the Secret (rollback case) → 404.
6. Hand the token to Salvador for pebble `src/pkjs/config.js` (`swbToken`). It ends up inside the
   `.pbw` (pebble README).
7. Rollback: the previous tag. `/watch.json` then 302s to login, which the watch reads as "home,
   swb silent".

## In scope / Out of scope

In scope: the route, the token, the `boardView` extraction, the `state_unix` column, the demo
off-scope, the tests, the handoff, and the IK entry.

Out of scope:
- Any pebble-repo change, including its README line "The route is open", which becomes stale.
  Tell the Pebble session.
- Filters or query parameters on the route, per-project watch views, and `queued`/`open` in the
  payload.
- Pushing to the watch (MQTT, SSE). The courier polls.
- Refusing the plain-http `switchboard.home.arpa` host (see Future work).
- OIDC for the dashboard, and any change to `s.auth`.
- Changing `lightFor`, `boardSections`, `boardTallies` or lease semantics.

## Invariants that apply

- **2 One funnel:** reads the one `tasks` table through the board's own read. No new table, queue
  or status.
- **3 Everything through the executor:** there is no tool call, so there is nothing to route.
  Criterion 11 pins that the handler cannot grow a write or an executor call unnoticed.
- **4 Nothing external without a delivery row:** the reply is a read answered to Salvador's own
  device, not an outbound communication. Nothing is sent to any third party.
- **6 Stealth attribution:** the payload has no text beyond session names. Nothing is
  client-visible.
- **7 Orchestrator is pure:** untouched. `watchSessions` is kept pure in the same spirit.
- 1 and 5: not applicable (no capture, no sends).

## Sibling patterns to copy

- `SetBoardHub` in `cmd/dashboard/main.go` / `server.go` for post-construction wiring.
- `demoScoped` / `withDemoScope` in `internal/dashboard/demo.go` for putting a scope on the context.
- `board_departures_integration_test.go` (`newDashServer`, `layoutBoard`, sign-header parsing) for
  criterion 15.
- `funcBodySrc` / `readSrc` structure-test helpers already in the package.

## Verification protocol

1. `go test ./...`.
2. Branch DB:
   `psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_watch_json'`,
   `make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_watch_json?sslmode=disable'`,
   then
   `DATABASE_URL=… go test -tags integration -p 1 ./internal/dashboard/...`.
3. Smoke, local:
   `SWB_WATCH_TOKEN=$(openssl rand -hex 32) DATABASE_URL=… go run ./cmd/dashboard`, then
   `curl -i localhost:8085/watch.json` (401),
   `curl -i -H "Authorization: Bearer $SWB_WATCH_TOKEN" localhost:8085/watch.json` (200 JSON, and
   the numbers match `/tasks`), and a restart without the env var → 404.
4. Optionally, the pebble courier test against the local route:
   `node tests/courier/test.js` in the pebble repo, with `swbUrl` pointed at it.

No open questions arose. D4 is a judgement call that narrows SWT-99 decision 3. It is recorded
above with the one-line flip.

## Future work

- Refuse the route when `X-Forwarded-Proto: http` (the `switchboard.home.arpa` host), so a
  misconfigured courier can never send the token in cleartext on the LAN.
- Per-project `?project=` once the watch has a use for it.
