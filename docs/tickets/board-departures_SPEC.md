> Jira: SWT-67

# board-departures — the board as an airport departures display: split-flap rows, two panel columns, auto-paging, a phone machine-status view, and a full-screen path

## Source

Ad-hoc, from Salvador, 2026-09-19. The board at `/tasks` "shows things but is
super boring", and Chrome's chrome wastes screen on his tablet. He supplied
airport departure-board photos and a factory machine-status display as
inspiration. A static mock was built from live data and accepted verbatim:

> that is perfect accepted

swb task **#420**. This is not a build-order step. It restyles what SWT-52
(`board-status-lights`), SWT-56 (`signal-session-name`), SWT-57
(`board-layout-compact`) and SWT-59 (`board-incoming-first`) built, and it must
not break their contracts.

**The visual contract is the mock**, not this prose:

- `docs/tickets/board-departures-mock/mock.html` (read its CSS and JS)
- `docs/tickets/board-departures-mock/tablet.png`, `phone.png`

**The mock's JavaScript is not a model for the implementation.** It computes
lights, sections, queue heads, stamps and remarks in JS from a JSON dump
*because it is a static file with no server*. In this ticket every one of those
facts is computed in Go, server-side, exactly as it is today.

## Goal

Re-skin `GET /tasks` as a dark amber departures board — split-flap row strips in
two panel columns that never scroll and auto-page, a yellow sign header with
tallies and a clock, a ticker footer, and a single-column machine-status view on
the phone — while every fact on the page keeps coming from `sections.go`,
`lights.go` and `boardLightFacts`, and every verb keeps going through the
executor unchanged.

**Usable alone means:** with only `deployment/dashboard` on the new image and no
other change anywhere, Salvador opens `/tasks?refresh=on` on the tablet and:

- the board fills the screen: a yellow sign block, `SWITCHBOARD`, the active
  project, the five tallies, a flip clock and a `FULL` button;
- the left column shows `NEEDS YOU`, `IN FLIGHT` and `ARRIVALS — INCOMING`; the
  right shows `DEPARTURES — QUEUE`, `HOLDING` and `LANDED TODAY`; empty sections
  are absent, as today;
- every row is one split-flap strip: time, id (with a red `▲` when priority ≥ 2),
  title, a coloured project chip, the Claude session tag, and a light dot with
  its remark and elapsed time;
- no panel scrolls: a panel that does not fit pages every 9 s with a flip, and
  says `page 2/3`;
- the ticker footer carries the counts, the six-light legend and the last
  refresh time;
- tapping a row opens `/tasks/{id}`; tapping the `⋯` at the row's end opens the
  same `actions` popup with Dismiss and (on human rows) Done, and both still
  work;
- `FULL` puts the board full-screen with one tap, over plain http;
- on his phone the same URL is a single column of machine-status lines — dot,
  title, id, elapsed, remark — with the queue cut to its top five and a sticky
  footer;
- the fonts come from the dashboard itself: the page makes no third-party
  request.

Nothing is sent. No tool, no policy, no migration, no orchestrator rule, no
delivery, no schema.

## What exists (code-read, 2026-09-19)

- `internal/dashboard/templates/tasks.html` — one `<style>`, one `<script>`
  (inside `{{if .AutoRefresh}}`), a `.headbar` (nav + `.topbar` filter form),
  a `.titlebar` (`<h1>Board</h1>`, the refresh toggle, the refresh block),
  flash, orchestrator alert, `{{range .Sections}}` with one `<table>` per
  section, then the legend and the board note.
- `internal/dashboard/sections.go` — `boardSection{Key,Title,Tasks}`,
  `boardSectionOrder` (incoming, blocked, in_flight, queue, holding, done,
  other), `boardSectionOf`, `sectionFor`, `lightRank`, `boardSections`. Pure.
- `internal/dashboard/lights.go` — `light{Class,Label,Session}`, `lightFacts`,
  `lightFor`, `signalStamp`, `headCandidate`, `pickQueueHeads`. Pure, scanned
  for purity.
- `internal/dashboard/board.go` — `taskRow`, `boardData`, `BoardTimeZone`,
  `boardRefreshInterval`, `boardKeys`, `boardDayStart`, `boardQuery`,
  `listTasks`, `reopenMarkers`, `boardLightFacts` (two statements),
  `boardBack`, `boardRefreshURLs`, `boardAdvanced`, `boardURL`,
  `dismissTaskAction`, `closeTaskAction`.
- `internal/dashboard/server.go` — `//go:embed templates/*.html`, the mux. The
  ONLY unauthenticated route today is `GET /healthz`. **There is no static-asset
  route and no static asset of any kind.**
- No `<meta name="viewport">` anywhere (SWT-57 put it explicitly out of scope).

## Decisions made unilaterally (with rationale)

### B1 — Nothing about the DATA moves: sections, lights, order and membership are byte-unchanged

`sections.go` and `lights.go` keep their logic. `boardSections` keeps its keys,
its order, its membership rule (`boardSectionOf`) and its within-section sort.
`lightFor` keeps its table and its six classes. `boardLightFacts` keeps its two
statements and gains exactly one new expression (B6).

What this ticket changes is: the section TITLES (B3), where each section is
DRAWN (B2), and the row's MARKUP and CSS. Everything else on the page is a new
*display* field computed in Go from facts that already exist.

### B2 — Two columns without changing DOM order: panes plus CSS `order`

The accepted design draws the left column as **needs you → in flight →
arrivals-incoming**, but `boardSectionOrder` puts `incoming` FIRST and
`board_incoming_integration_test.go` pins `<h2 id="section-incoming">` as the
first `<h2>` on the page (SWT-59 I3).

- The DOM order of the sections is **unchanged**: `incoming, blocked, in_flight`
  in the left pane, then `queue, holding, done, other` in the right pane. The
  split is a contiguous prefix/suffix of `boardSectionOrder`, so document order
  across the whole page is byte-identical to today's and every existing
  section-order assertion stays green.
- The VISUAL order inside a pane comes from CSS `order` on a flex column, fed by
  a per-section integer (`boardSection.Order`). `incoming` gets `order: 3` and so
  is drawn last in the left column, as the mock does, while staying first in the
  document — which is also the right reading order for a screen reader.
- Flex `order` (not CSS grid placement) because each pane is an independent flex
  column: panel heights in the left pane must not be coupled to the right pane's
  rows, which is exactly what a shared grid would do.

**The panel table** (one map in `display.go`, unit-tested to cover
`boardSectionOrder` exactly, in both directions):

| key | pane | order | class |
|-----|------|-------|-------|
| `incoming` | left | 3 | `panel grow` |
| `blocked` | left | 1 | `panel alarm` |
| `in_flight` | left | 2 | `panel` |
| `queue` | right | 1 | `panel grow` |
| `holding` | right | 2 | `panel` |
| `done` | right | 3 | `panel` |
| `other` | right | 4 | `panel` |

`grow` is the panel that takes the pane's spare height; `alarm` is the red
heading. Both are class names from a Go table, so the template needs no branch.

### B3 — `holding` stays its own panel; `other` gets one too

The mock concatenates the holding rows into the queue panel. It stays SEPARATE
here:

- merging would need a second spelling of section membership (in the template or
  a Go merge), and `boardSections`' seven keys are the SWT-57/59 contract that
  four test files read;
- the two counts say different things — `HOLDING (3)` is the review lane, not
  the queue — and the panel heading is where the count lives;
- drawn immediately under `DEPARTURES — QUEUE` in the right column, it reads the
  same way the mock does.

`other` (a dismissed row under `?status=closed`, or an unknown status) gets the
last panel in the right column. It is empty on the default board and so is not
rendered — but it can never again be a section with nowhere to go.

**Titles change with the design** (keys do not; `boardSectionOrder` in
`sections.go`, lowercase because the CSS uppercases):

| key | title today | title here |
|-----|-------------|-----------|
| `incoming` | `incoming` | `arrivals — incoming` |
| `blocked` | `blocked` | `needs you` |
| `in_flight` | `in flight` | `in flight` |
| `queue` | `queue` | `departures — queue` |
| `holding` | `holding` | `holding` |
| `done` | `done` | `landed today` |
| `other` | `other` | `other` |

The heading markup keeps `<h2 id="section-{{.Key}}">{{.Title}} ({{len .Tasks}})</h2>`
**byte-unchanged**, so `layoutSections()` keeps parsing it; the page indicator
(`page 2/3`) is a SIBLING `<span class="pg">` outside the `<h2>`, because the
existing regex requires `</h2>` right after the count.

### B4 — The row: an anchor of cells, with the verbs OUTSIDE it

The whole row is a link, and a `<form>` or `<details>` inside an `<a>` is
invalid HTML. So each row is a wrapper holding two siblings:

```
<div class="row s-{class}">
  <a class="r" href="/tasks/{id}"> … the cells … </a>
  <details class="row-verbs"><summary>⋯</summary><div class="popup"> … Dismiss, Done … </div></details>
</div>
```

- The anchor reserves an empty last grid column the width of the `⋯` affordance;
  the `<details>` is positioned over it (`position: absolute; right: 0`), so the
  verbs never overlap text and the rest of the strip is one big touch target.
- The Dismiss and Done forms are **byte-identical** to SWT-51/SWT-52's, inner
  indentation included (`TestTasksTemplate_VerbFormsByteUnchanged` pins it).
  Done stays inside the one `{{if eq .AssigneeType "human"}}`.
- The summary text becomes `⋯` (U+22EF) instead of `actions`, with
  `aria-label="actions"`; `<summary>` is a real control, so it is reachable by
  keyboard and by touch.
- An open `<details>` still pauses auto-refresh (SWT-57 L6), unchanged.

**Cells, in this order** (tablet grid; the Gate column collapses to zero width in
the right pane, by CSS only — one row markup, two column templates):

| # | cell | content |
|---|------|---------|
| 1 | `mdot` | the phone's light dot (hidden ≥ 761 px) |
| 2 | `time` | `{{.Updated}}`, `title="{{.UpdatedAt}}"` — the SWT-57 stamp, unchanged |
| 3 | `id` | `{{.ID}}` plus the priority mark (B8) |
| 4 | `title` | `{{.Title}}` and the `reopened after dismissal (code)` note |
| 5 | `chip` | the project slug, hue from `--chip-h` (B9) |
| 6 | `gate` | the session tag, when `.Light.Session` is set (B10) |
| 7 | `rem` | the light span, `{{.Remark}}`, `{{.Elapsed}}` |

### B5 — The status COLUMN goes; the status itself does not

SWT-57 L4 added a `status` cell four days ago and the mock has none. Dropping it
would lose `pr_open` vs `in_progress`. So the status is folded into the
**Remarks** words instead of being deleted: `remarkFor` (B7) returns `pr open`,
`awaiting ci`, `awaiting merge`, `claimed`, `holding`, `blocked`, `dismissed`,
`delivered`, `done locally` … — every status in `boardStatusOrder` reaches the
row as words. Nothing is lost; one column is. The light's full sentence
(`Light.Label`) stays on the light span's `title` and `aria-label`, as today.

The rarely-filled `sub`, `order`, `parent` and `assignee` cells are dropped —
SWT-57's own Future work listed exactly this. The fields stay on `taskRow`
(`task.html` reads them through `taskDetail`).

### B6 — Elapsed time is a DB-clock fact, formatted by a pure Go function

"Elapsed time since the session signal" must not be `Date.now()` in JS (the mock)
and must not be `time.Now()` in Go (`TestBoard_NoGoClockFeedsVisibilityOrALight`
bans it on every path that feeds a light or visibility).

- `boardLightFacts`' FIRST statement gains one expression:
  `COALESCE(GREATEST(0, FLOOR(EXTRACT(EPOCH FROM now() - t.working_state_at) / 60))::int, 0) AS state_age_min`.
  No new statement, no new query — D15's cost table is unchanged.
- `lightFacts.StateAgeMinutes int`, documented display-only. `lightFor` never
  reads it (structure test, the `FromMessage`/`QueueRank` precedent).
- `elapsedFor(class string, minutes int) string` is pure: `"HH:MM"` zero-padded
  for `input`, `working` and `stale`; `""` for every other class (a done or
  queued row shows no clock, exactly as the mock does). Hours are not capped —
  a three-day stale ring reads `72:14`, which is the point.

### B7 — Remarks are a pure Go table, lowercase; the CSS uppercases

`remarkFor(l light, status string) string` in `display.go`, pure, exhaustive over
`boardStatusOrder` × the six classes:

| class | status | remark |
|-------|--------|--------|
| `input` | any | `waiting on you` |
| `working` | `claimed` | `claimed` |
| `working` | `in_progress` | `in progress` |
| `working` | `pr_open` | `pr open` |
| `working` | `awaiting_ci` | `awaiting ci` |
| `working` | `awaiting_merge` | `awaiting merge` |
| `working` | `holding`/`ready`/`blocked` (a session signal) | `in progress` |
| `stale` | any | `no signal` |
| `next` | any | `next up` |
| `done` | `done_locally` | `done locally` |
| `done` | `delivered` | `delivered` |
| `done` | `closed` | `done` |
| `none` | `holding` | `holding` |
| `none` | `blocked` | `blocked` |
| `none` | `ready` | `queued` |
| `none` | `closed` | `dismissed` |
| `none` | anything else | the status verbatim |

Lowercase in Go, uppercased by `text-transform` in CSS, so the same string reads
right in the tablet strip and in the phone line and the Go table stays legible.

### B8 — The priority mark is real text, not CSS `content`

The mock spells it `.p2 .id::after { content: "▲" }`, which is invisible to a
screen reader and needs one class per priority value. Here:

- `taskRow.HighPriority bool` = `Priority >= boardPriorityMark` (a const, `2`);
- the template renders
  `{{if .HighPriority}}<span class="prio" role="img" aria-label="priority {{.Priority}}" title="priority {{.Priority}}">▲</span>{{end}}`.

`tasks.priority` is not touched and the board's ordering is not touched: the mark
is decoration on the existing value.

### B9 — The project chip's hue is a pure Go fold, carried as a CSS custom property

- `projectHue(slug string) int` reproduces the mock's fold over the slug's
  BYTES — `h = (h*31 + b) % 360` — so the accepted screenshots' colours are the
  colours that ship. Project slugs are ASCII; a non-ASCII slug would differ from
  the mock's UTF-16 fold, which is stated and irrelevant.
- The template writes `style="--chip-h:{{.ProjectHue}}"` and the CSS reads
  `background: hsl(var(--chip-h, 0) 55% 34%)`. A custom property whose value is
  digits is the safest thing to put through `html/template`'s CSS context; no
  `template.HTML`/`HTMLAttr` anywhere (pinned).
- The chip shows the FULL slug and is truncated by CSS (`max-width` +
  `text-overflow: ellipsis`), never by Go — the SWT-56 session-name precedent
  ("the board ellipsizes in CSS only; Go never truncates a name").

### B10 — The session tag becomes the Gate cell, keeping its class and title

`light.Session` is unchanged (SWT-56: set only on `input`/`working`/`stale`,
`session unknown` for a pre-0036 marker). The tag moves out of the title cell
into its own Gate cell, keeping `class="session-tag session-{{.Light.Class}}"`
and `title="{{.Light.Label}}"` so the SWT-56 CSS rules and the integration
helper keep their anchors. The right pane hides the Gate column in CSS; the
value is still in the markup.

### B11 — Paging shows and hides server-rendered rows; it never builds any

"Panels never scroll: a panel shows as many rows as fit and auto-pages" is a
VIEWPORT fact, so it is JS. But the mock rebuilds rows with `innerHTML`, and
`innerHTML` is on the refresh script's banned-token list for good reason.

- Every row of every section is rendered by Go, always. The script only sets
  `row.hidden` outside the current window, writes `page n/m` with `textContent`,
  and adds the flip class with a per-row `style.animationDelay`.
- Rows per page = `floor(panel rows-box height / row height)`, recomputed on
  `resize`.
- `boardPageInterval = 9 * time.Second`, a const in `board.go`, reaching the
  script as `data-page-interval`. Never a URL value — the D15 rule for the same
  reason (a caller-set `0.05` is an animation loop on a shared box).
- With JS off, `.rows { overflow-y: auto }` and every row is reachable by
  scrolling; the script adds a class to the body that switches to
  `overflow: hidden` + paging. The board degrades, it does not break.
- The phone shows every row of every panel (no paging) and the page scrolls
  normally. The queue panel is cut to its first five rows by CSS
  (`.rows > .row:nth-child(n+6) { display: none }` under the media query), while
  the heading still shows the true count — which is the mock's behaviour, and
  the count is why it is honest.

### B12 — One script, rendered always; auto-refresh arms from a data attribute

Today the one `<script>` sits inside `{{if .AutoRefresh}}`. The clock, paging,
fullscreen and wake lock must run with auto-refresh OFF, and a second `<script>`
would break the "exactly one" count that two tests keep.

- ONE `<script>` for the whole board, outside every conditional, carrying
  `data-reload="{{.ReloadURL}}"`, `data-interval="{{.RefreshSeconds}}"`,
  `data-page-interval="{{.PageSeconds}}"` and `data-refresh="{{.RefreshMode}}"`.
- `boardData.RefreshMode string` is `"on"` or `""`, set beside `AutoRefresh` in
  `listTasks`. A DATA field, not a second `{{if .AutoRefresh}}` — the SWT-57
  landmine is that two structure tests take the FIRST `{{if .AutoRefresh}}` in
  the file as the refresh block, and the file keeps exactly one (the indicator).
- The refresh loop arms only when `data-refresh === "on"`. Its rules are
  otherwise **unchanged**: `setTimeout` self-arming, `location.replace(target)`,
  and `busy()` postponing on `document.hidden`, an open `<details>`, a focused
  form control, or a dirty control (with the select's `defaultSelected` rule
  spelled exactly as it is today).
- The script still contains none of `fetch(`, `XMLHttpRequest`, `htmx`,
  `location.search`, `location.href`, `innerHTML`, `localStorage`,
  `sessionStorage`, `onchange`.

### B13 — Auto-refresh waits for the page cycle to come back to page 1

With paging at 9 s and reloads at 5 s, a reload would reset every panel to page 1
before page 2 was ever drawn: the second page would be unreachable exactly when
he is watching.

- `busy()` gains ONE clause: postpone while any panel is not on its first page.
  The cycle is then page 1 → … → last page → page 1 → reload.
- Worst-case staleness is `pages × 9 s + 5 s`; the indicator's "last refreshed
  HH:MM:SS" shows it truthfully, exactly as an open popup already does
  (SWT-57 L6, same shape, same honesty).
- A reload always lands on page 1. Stated because it is the acceptable half of
  the trade: paging state is not preserved across a reload and nothing tries to.

### B14 — The clock is the device clock, and is the ONLY thing on the page that is

The flip clock ticks from `new Date()` in the browser. It is a wall clock, not a
fact about the work. Every data-derived time on the page is still Postgres in
`BoardTimeZone`: the `updated` stamp, the signal stamps in the labels, the
elapsed minutes, `RenderedAt`. `TestBoard_NoGoClockFeedsVisibilityOrALight`
stays green because no Go clock is added.

### B15 — Header, footer, and where the kept furniture goes

Three bands, in this document order (so the SWT-57 order test's spirit — nav,
filters, title, flash, alert, rows — survives):

1. `.headbar`: `<nav>` (Board, Plans, Briefs, Deliveries, Sources, Funnel, CSV,
   JSON) and `.topbar` (the filter `<form class="filters">` with the project
   select, the hidden `refresh` input, the `<details class="advanced-filter">`
   popup, and `clear advanced`). **All byte-unchanged**, restyled dark and
   small. It is the chrome bar, not the sign.
2. `<header class="sign">`: the yellow sign block with the plane glyph,
   `<h1>Switchboard <small>{{.ProjectLabel}}</small></h1>`, the tally, the flip
   clock, the `FULL` button, and the auto-refresh toggle
   `<a id="auto-refresh-toggle" href="{{.RefreshToggleURL}}">` (byte-unchanged,
   with its `{{if not .AutoRefresh}}` text — the SWT-57 landmine).
3. `{{if .Flash}}` and `{{with .OrchAlert}}` as full-width blocks, unchanged
   markup, restyled for the dark board. Both stay ABOVE `<main>`: a flash is a
   verb's only receipt and the orchestrator alert is the one thing that outranks
   the board.

`<main>` holds the two panes. `<footer class="ticker">` holds, in order: the
overall counts, the legend `<p class="muted" id="light-legend">` with its text
**byte-unchanged** (both legend tests keep passing), the
`{{if .AutoRefresh}}<p id="auto-refresh" …>` indicator **byte-unchanged**, and a
closed `<details class="board-notes">` whose popup carries the board note
("Queues are filters on the one tasks table…", byte-unchanged) and the SWT-56
session sentence. The legend is always visible; the prose is one tap away, which
is what "may be restyled/condensed, but the legend must remain" asks for.

`.ProjectLabel` is a Go field (the project filter, or `all projects`) so the
header needs no branch.

### B16 — Tallies are pure Go over the sections

`boardTally{NeedYou, InFlight, Incoming, Queued, DoneToday, Open int}` from
`boardTallies(secs []boardSection)`, pure and unit-tested:

- `NeedYou` = `blocked`, `InFlight` = `in_flight`, `Incoming` = `incoming`,
  `DoneToday` = `done`;
- `Queued` = `queue` + `holding` (one number for "waiting to start");
- `Open` = every row on the page minus `done`.

They are counts of what the board is SHOWING — i.e. after the filters — which is
what a tally beside a filtered board must mean.

### B17 — Assets: self-hosted, embedded, served unauthenticated from `/static/`

The dashboard serves no asset today and makes no third-party request; the mock's
`fonts.googleapis.com` link cannot ship.

- **Vendored** under `internal/dashboard/static/`:
  `fonts/b612mono-400.woff2`, `fonts/b612mono-700.woff2`,
  `fonts/barlowcondensed-500.woff2`, `-600.woff2`, `-700.woff2`,
  `fonts/OFL-B612.txt`, `fonts/OFL-BarlowCondensed.txt`,
  `fonts/SOURCES.md` (upstream URL, commit/version and `sha256` of each file),
  `manifest.webmanifest`, `icon-192.png`, `icon-512.png`.
- **Licences:** both families are SIL OFL 1.1 — B612 (Polarsys/Airbus) and
  Barlow Condensed (Jeremy Tribby). OFL permits redistribution WITH the licence
  text, which is why `OFL-*.txt` ships beside the fonts; the Reserved Font Name
  clause means the files keep their family names and are not renamed. A
  latin-only subset is allowed (record the exact `pyftsubset` command in
  `SOURCES.md` if one is taken); an unsubsetted woff2 is fine at these sizes.
- **Route:** `mux.Handle("GET /static/{path...}", http.StripPrefix("/static/", http.FileServerFS(staticSub)))`
  where `staticSub` is `fs.Sub(staticFS, "static")` over a new
  `//go:embed static` — so the binary still carries everything and nothing new
  lands in the image build.
- **Not behind `s.auth.Require`,** deliberately, with the reason written in the
  code: a manifest, its icons and a `@font-face` file are fetched by the browser
  WITHOUT credentials by default, so an authenticated route would 302 them to the
  login page and silently break the installability path — and the bytes are
  public font files, an icon and a static manifest, with no task data of any
  kind. It sits beside `GET /healthz`, the only other open route. `fs.Sub` over
  an embedded FS cannot traverse out of `static/`.
- Served with `Cache-Control: public, max-age=31536000, immutable` (filenames
  carry the weight, and the whole set is replaced together).
- `@font-face` lives in the existing inline `<style>`, `font-display: swap`,
  each family followed by a local fallback (`DejaVu Sans Mono, ui-monospace,
  monospace` / `Arial Narrow, system-ui, sans-serif`), so a failed font costs
  the look and not the board.

### B18 — Full screen: a button that works today, a manifest that works after the cert

Per the accepted proposal, and no native app:

1. **The `FULL` button** calls `document.documentElement.requestFullscreen()` /
   `document.exitFullscreen()` from a `click` listener registered with
   `addEventListener` (no inline handler — pinned). The Fullscreen API needs a
   user gesture but **not** a secure context, so this works over plain http
   today. The button is a `<button type="button">` OUTSIDE every `<form>`, so
   focusing it never makes `busy()` postpone a reload.
2. **The manifest and meta tags** ship now:
   `<link rel="manifest" href="/static/manifest.webmanifest">`,
   `<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
   `<meta name="theme-color" content="#0b0b0c">`,
   `<meta name="mobile-web-app-capable" content="yes">`,
   `<meta name="apple-mobile-web-app-capable" content="yes">`;
   manifest `display: "fullscreen"`, `display_override: ["fullscreen","standalone"]`,
   `start_url: "/tasks"`, `scope: "/"`, `id: "/tasks"`, both icons `any maskable`.
   **State the truth: on a non-secure origin Chrome does not process a manifest
   for installation**, so "Add to Home screen" today makes a plain shortcut that
   opens in a tab. Shipping it now means the cert flip is the only remaining
   step, with nothing to re-do in this repo.
3. **Screen Wake Lock** is attempted and MUST degrade silently:
   `if (navigator.wakeLock) { navigator.wakeLock.request("screen").then(…).catch(function(){}); }`
   guarded, with the promise rejection swallowed and re-acquired on
   `visibilitychange`. `navigator.wakeLock` is `undefined` in an insecure
   context, so today the guard is simply false. No console error, no banner, no
   user-visible difference.
4. **No native or mobile app**, and no service worker (a service worker is also
   secure-context-only and would add an offline cache nobody asked for).

**Out of scope and a follow-up handoff:** giving the dashboard a certificate the
tablet trusts (and an Ingress, if it still has none — the IK says "port-forward
only, Ingress commented out" while the owner clicks `http://switchboard.home.arpa/…`,
so the kube session must first establish which is true). That is
`docs/runbooks/HANDOFF-kube-board-departures.md` §Follow-up, not this ticket.

### B19 — What the reload does to paging, and what it does to nothing else

A reload resets every panel to page 1 (B13 makes that harmless). `boardKeys`,
`boardBack`, `boardRefreshURLs`, `boardAdvanced`, the hidden `refresh` inputs and
the flash-carrying rules are **untouched**. Nothing about paging, fullscreen or
the clock is stored: no cookie, no `localStorage`, no server state, no URL key.

### B20 — No schema, no tool, no policy, no executor change

Confirmed by reading: every new fact is derived from columns the board already
selects, except `state_age_min`, which is arithmetic on
`tasks.working_state_at` — already selected in the same statement. The two verbs
keep their handlers, their args, their actor and their `executeTask` call. If
implementation finds otherwise, STOP: it means a decision above is wrong.

### B21 — AMENDMENT (2026-09-19, found in implementation): FULL opens a full-screen shell, `/kiosk`

B18-1 was wrong as written. A full-page navigation ends the document's
fullscreen, and the board's auto-refresh IS a full-page navigation
(`location.replace`), so FULL on `/tasks` would last until the next refresh —
and `requestFullscreen` needs a fresh tap, so the script cannot re-enter.
Verified in a browser and confirmed by review. Salvador chose "both": a shell
now, the installed app (B18-2) when the certificate lands.

- `GET /kiosk[?project=…]` (behind `s.auth.Require`, `kiosk.go` +
  `templates/kiosk.html`): a shell document holding
  `<iframe id="board" src="/tasks?refresh=on[&project=…]">`. It reads nothing
  and executes nothing; only the project filter is carried, query-escaped.
- The shell shows one button, "Tap for full screen", which calls
  `requestFullscreen` on the SHELL's document. The board then reloads inside the
  iframe and the shell stays fullscreen (browser-tested across a reload). Leaving
  fullscreen brings the button back; "stay in the window" dismisses it.
- The board's FULL button (`data-kiosk="{{.KioskURL}}"`): framed by the shell it
  toggles the shell's fullscreen (`window.top`, same origin — a tap in a frame
  counts for its ancestors); anywhere else it `location.assign`s the shell.
- Same rules as the board: no third-party URL, nothing stored, no fetch, no
  inline handler, wake lock guarded and swallowed. Tests: `kiosk_test.go`.
- Also found in implementation and fixed here: B13 alone does not save page 2 —
  a 5 s reload always pre-empts a 9 s page turn from page 1 — so the reload loop
  additionally waits for one full paging cycle (`cycleDone`). Worst-case
  staleness is `longest panel's pages × 9 s + 5 s`, as B13 already stated.
- `/static/` serves only real embedded files; a miss or a directory is a plain
  404 with no cache header (review finding: an `immutable` 404 would be cached
  for a year, and the open route should list nothing).

## Acceptance criteria

### Part 1 — pure Go display helpers (`internal/dashboard/display.go`, new)

1. `display.go` declares, all pure (no `time.`, no `s.pool`, no `Query(`, no
   `Exec(`; structure-scanned like `lights.go` and `sections.go`):
   - `const boardPriorityMark = 2`;
   - `type boardPane struct{ Key string; Sections []boardSection }`;
   - `type boardTally struct{ NeedYou, InFlight, Incoming, Queued, DoneToday, Open int }`;
   - `var boardPanels map[string]boardPanel` (B2's table);
   - `func boardPanes(secs []boardSection) []boardPane`;
   - `func boardTallies(secs []boardSection) boardTally`;
   - `func remarkFor(l light, status string) string`;
   - `func elapsedFor(class string, minutes int) string`;
   - `func projectHue(slug string) int`;
   - `func projectLabel(filter string) string`.
2. `boardPanels` covers `boardSectionOrder` exactly: a unit test fails if a key
   is missing from either side. `boardPanes` returns two panes (`left`, `right`)
   in that order, each holding its sections **in `boardSectionOrder` order**,
   each section carrying its `Class` and `Order`; an empty pane is omitted; nil
   input gives nil.
3. `remarkFor` is exhaustive: a table test over every status in
   `boardStatusOrder` plus one unknown status, crossed with `lights_test.go`'s
   `factCombos`, asserts B7's table, that the result is never empty, and that it
   is always lowercase.
4. `elapsedFor` returns `""` for `done`, `next` and `none` whatever the minutes;
   `"00:00"` at 0, `"01:05"` at 65, `"72:14"` at 4334, for `input`, `working`
   and `stale`.
5. `projectHue` is deterministic and in `[0,360)`: a golden table over at least
   `saka`, `collaboratory`, `personal`, `bulk`, `itest-layout-proj` and `""`,
   with the values computed by the mock's fold. Two different slugs in the
   golden set must give two different hues (a positive control against a
   constant-returning implementation).
6. `boardTallies` is B16's arithmetic, with a case where `?status=closed` makes
   `other` non-empty (it counts in `Open`, not in `DoneToday`).
7. `projectLabel("")` is `all projects`; `projectLabel("saka")` is `saka`.

### Part 2 — the read and the handler (`internal/dashboard/board.go`, `lights.go`)

8. `lightFacts` gains `StateAgeMinutes int`, documented display-only beside
   `QueueRank`/`UpdatedStamp`. `lightFor`'s body never mentions it (structure
   test, the SWT-59 shape).
9. `boardLightFacts` still issues **at most two** `s.pool.Query` calls. Its FIRST
   statement gains exactly one expression, aliased `state_age_min`, built from
   `now() - t.working_state_at` with `GREATEST(0, …)` and a `COALESCE(…, 0)`;
   the scan sets `lightFacts.StateAgeMinutes`. The SECOND statement is
   **byte-unchanged** (`TestBoardLightFacts_SecondStatementByteUnchanged` stays
   green). No new `time.` anywhere on the path.
10. `taskRow` gains `Remark string`, `Elapsed string`, `ProjectHue int`,
    `HighPriority bool` — all board-only; `TaskExportRow` is unchanged and the
    export goldens are untouched.
11. `boardData` gains `Panes []boardPane`, `Tally boardTally`,
    `ProjectLabel string`, `RefreshMode string`, `PageSeconds int`, and KEEPS
    `Sections` (its input to `boardPanes`) and every SWT-52/57 field. No field
    named `Columns` (the SWT-57 ban).
12. `listTasks` sets them from the pure helpers: `Remark: remarkFor(tr.Light, t.Status)`,
    `Elapsed: elapsedFor(tr.Light.Class, f.StateAgeMinutes)`,
    `ProjectHue: projectHue(t.Project)`,
    `HighPriority: t.Priority >= boardPriorityMark`,
    `data.Panes = boardPanes(data.Sections)`,
    `data.Tally = boardTallies(data.Sections)`,
    `data.ProjectLabel = projectLabel(r.URL.Query().Get("project"))`,
    `data.PageSeconds = int(boardPageInterval / time.Second)`,
    and `RefreshMode = "on"` exactly when `AutoRefresh`. It still uses no
    `strconv`, `Atoi`, `ParseFloat` or `ParseDuration`
    (`TestListTasks_RefreshIsARenderFlagNotAQuery` stays green).
13. `const boardPageInterval = 9 * time.Second` in `board.go`, never read from a
    request; a unit test pins the value and that `listTasks` derives
    `PageSeconds` from it.
14. `boardQuery`, `boardBack`, `boardRefreshURLs`, `boardAdvanced`, `boardURL`,
    `boardKeys`, `boardDayStart`, `BoardTimeZone`, `reopenMarkers`,
    `dismissTaskAction` and `closeTaskAction` are **byte-unchanged**.

### Part 3 — sections (`internal/dashboard/sections.go`)

15. Only `boardSectionOrder`'s seven `Title` strings change (B3). Keys, order,
    `boardSectionOf`, `sectionFor`, `lightRank`, `incomingRank` and
    `boardSections`' sorting are byte-unchanged, and `sections.go` stays pure.
16. `boardSection` gains `Class string` and `Order int`, documented display-only
    and set **only** by `boardPanes`; `boardSections` never sets them (a unit
    test asserts they are zero on its output).

### Part 4 — the template (`internal/dashboard/templates/tasks.html`)

17. `<head>` carries the viewport, theme-color, mobile-web-app-capable and
    apple-mobile-web-app-capable metas and `<link rel="manifest" href="/static/manifest.webmanifest">`.
18. **No third-party URL anywhere in the file**: a structure test fails on
    `fonts.googleapis.com`, `fonts.gstatic.com`, `//` in any `src`/`href` other
    than an in-app absolute path, and on `http://` or `https://` appearing at
    all. Every `url(` in the `<style>` block starts `/static/`.
19. The three bands of B15 in document order, with these BYTE-UNCHANGED
    fragments present exactly once each: the project select with its one
    `onchange`; the hidden refresh input in the filter form; the
    `<details class="advanced-filter">` and its three text inputs and
    `<button>Filter</button>`; `<a id="advanced-clear" href="{{.ClearAdvancedURL}}">clear advanced</a>`;
    `<a id="auto-refresh-toggle" href="{{.RefreshToggleURL}}">` with its
    `{{if not .AutoRefresh}}` text; the indicator `<p id="auto-refresh" class="muted">auto-refresh on (every {{.RefreshSeconds}} s, last refreshed {{.RenderedAt}})</p>`;
    `{{if .Flash}}`; `{{with .OrchAlert}}`; the legend paragraph; the board note.
20. Exactly ONE `{{if .AutoRefresh}}` in the file, wrapping the indicator only.
    The `<script>` is outside it.
21. `<main>` is `{{range .Panes}}<div class="col col-{{.Key}}">{{range .Sections}}` …
    `{{end}}</div>{{end}}`, with the `{{else}}`
    `<p class="muted">No tasks match the filters.</p>` on the pane range. The
    panel is
    `<section class="{{.Class}}" style="order:{{.Order}}"><div class="panelhead"><h2 id="section-{{.Key}}">{{.Title}} ({{len .Tasks}})</h2><span class="pg"></span></div><div class="cols">…</div><div class="rows">{{range .Tasks}}…{{end}}</div></section>`.
    No `<table>`, `<tr>`, `<th>` or `<td>` remains in the file.
22. The row is B4's wrapper: one `<a class="r" href="/tasks/{{.ID}}">` holding the
    seven cells in order, and the `<details class="row-verbs">` as the wrapper's
    LAST child, **outside** the anchor (a structure test asserts the
    `<details` index is greater than the index of the `</a>` that closes the row
    link, and that no `<form`, `<details`, `<button` or `<input` lies between
    `<a class="r"` and that `</a>`).
23. The light span is unchanged markup
    `<span class="light light-{{.Light.Class}}" role="img" aria-label="{{.Light.Label}}" title="{{.Light.Label}}"></span>`,
    exactly once in the file, now inside the `rem` cell immediately before
    `{{.Remark}}`; its actions still reference only `.Light.Class` and
    `.Light.Label`.
24. The session tag is unchanged markup
    `<span class="session-tag session-{{.Light.Class}}" title="{{.Light.Label}}">{{.Light.Session}}</span>`
    inside `{{if .Light.Session}}`, exactly once, now in the `gate` cell.
25. The Dismiss and Done forms are byte-identical to today's, inner indentation
    included, with their five hidden filter inputs; Done stays inside the one
    `{{if eq .AssigneeType "human"}}`; Dismiss renders on every row.
26. The template contains no `eq .Status`, no `eq .Light`, no `hx-`/`htmx`, and
    exactly one inline event handler (the project select's `onchange`). `.Incoming`
    and the literal word `incoming` never appear (SWT-59's ban — the titles come
    from Go).
27. Exactly one `<script`, outside every conditional, carrying `data-reload`,
    `data-interval`, `data-page-interval` and `data-refresh`. It contains
    `setTimeout`, `location.replace(`, `document.hidden`, `activeElement`,
    `defaultValue`, `details[open]`, `addEventListener("visibilitychange"`,
    `requestFullscreen`, `wakeLock`, `textContent`, and NONE of `fetch(`,
    `XMLHttpRequest`, `htmx`, `location.search`, `location.href`, `innerHTML`,
    `localStorage`, `sessionStorage`, `onchange`.
28. `busy()` postpones on all five conditions: hidden tab, open `<details>`,
    focused form control, dirty control (the `defaultSelected` rule unchanged),
    and a panel not on page 1 (B13).
29. CSS: the six `.light-*` classes exist, `stale` and `none` are rings
    (`transparent` fill + border); `.session-tag` keeps a `max-width` and
    `text-overflow: ellipsis`; `.chip` reads `hsl(var(--chip-h …`; there is a
    `@media (max-width: 760px)` block (the phone view) and a
    `@media (min-width: 761px)` block (hiding `.mdot`); `.rem` is
    `text-transform: uppercase`; `.rows` is `overflow-y: auto` by default.

### Part 5 — static assets and routes (`internal/dashboard/server.go`, `static/`)

30. `//go:embed static` plus `fs.Sub`, and the route of B17 registered WITHOUT
    `s.auth.Require`, with the reason in a comment. A structure test asserts the
    route exists, that it is not wrapped in `Require(`, and that `GET /tasks`
    and every POST still are.
31. A unit test reads the embedded FS and asserts: the five `.woff2` files are
    present and non-empty and begin with the woff2 signature `wOF2`; both
    `OFL-*.txt` are present and contain `SIL OPEN FONT LICENSE`; `SOURCES.md`
    names a URL and a `sha256` for each font file; `icon-192.png` and
    `icon-512.png` are present and are PNGs.
32. `manifest.webmanifest` parses as JSON and has `display: "fullscreen"`,
    `display_override` containing `fullscreen`, `start_url: "/tasks"`,
    `scope: "/"`, a `theme_color`, and two icons with `sizes` 192 and 512 and
    `purpose` including `maskable`.

### Part 6 — integration (`internal/dashboard/board_departures_integration_test.go`, new; `//go:build integration`)

Reuses `dashGuard`/`dashPool`/`newDashServer`/`get`/`snippet`, `bdInsID`,
`lightsExecutor`/`lsSignal`/`lsDayStart`, and the REWRITTEN row helpers of
Part 7. Own slug `itest-dep-proj`, FK-ordered cleanup (policy_decisions and
audit_events by `task_id` FIRST — the SWT-37 landmine), rerunnable, fixture
instants computed in SQL.

33. **Panes.** With rows in every section, the page has exactly two
    `<div class="col col-left">` / `col-right` wrappers; `section-incoming`,
    `section-blocked`, `section-in_flight` are inside the left one and
    `section-queue`, `section-holding`, `section-done` inside the right one; the
    first `<h2>` on the page is still `<h2 id="section-incoming">`; the document
    order of the seven `<h2>`s is `boardSectionOrder`; and the `style="order:N"`
    of each panel matches B2's table.
34. **Row shape.** For each seeded task: exactly one row wrapper carries
    `href="/tasks/{id}"`; its light class and label are what `lightFor` gives;
    its remark text equals `remarkFor`'s value uppercased-in-CSS (assert the raw
    lowercase text in the markup); its elapsed cell is `HH:MM`-shaped for a
    signalled row and empty for a `done`/`next`/`none` row; the `time` cell is
    `HH:MM` for a row updated today and `YYYY-MM-DD` for one updated before local
    midnight, with the raw `updated_at::text` in `title` (SWT-57 criterion 22,
    carried over).
35. **Verbs survive the restyle.** Every rendered row contains a form posting to
    `/tasks/{id}/dismiss`; every HUMAN row also contains one posting to
    `/tasks/{id}/close`, and no claude row does; each form carries the five
    hidden filter values of the current query including `refresh=on`; and the
    `<details class="row-verbs">` opens after the row anchor closes. A real POST
    of each verb through the real executor still flashes `task_dismiss ok` /
    `task_close ok` and lands back on the same filtered board (the existing
    dismiss/close integration tests cover the semantics; this asserts the markup
    that reaches them).
36. **Priority, chip, gate.** A priority-2 task's row carries the `prio` span
    with `aria-label="priority 2"` and a priority-0 task's does not; both rows
    carry `--chip-h:` with the value `projectHue(slug)`; a signalled row carries
    the `session-tag` with the session name in its own gate cell and a
    non-signalled row carries none.
37. **No third-party fetch, and the assets are reachable.** The rendered page
    contains no `fonts.googleapis.com`, `fonts.gstatic.com` or `https://`; every
    asset URL it references returns 200 from the test server **without a session
    cookie** (a fresh `http.Client` with no jar), with the woff2 files served as
    `font/woff2` and the manifest as `application/manifest+json`; `/static/../server.go`
    and `/static/..%2fserver.go` do not return Go source.
38. **Tallies and footer.** The header's five tally numbers equal
    `boardTallies` over the sections the page shows, including under
    `?assignee_type=human`; the footer carries the legend with its six
    `legend-light` spans; with `refresh=on` the indicator carries an `HH:MM:SS`
    time and there is exactly one `<script`.
39. **Filters still round-trip.** `?project=…&assignee_type=human&refresh=on`
    marks the advanced summary `advanced-active`, renders `advanced-clear`
    pointing at `/tasks?project=…&refresh=on`, keeps `value="human"` inside the
    one filter form, and renders no `<details … open>`.

### Part 7 — existing tests: what carries, what is rewritten, what is amended

**Carried over UNCHANGED** (they must stay green; the reviewer checks this):

- `board_structure_test.go`: `TestBoardTemplate_ProjectFilterAutoSubmits`,
  `TestBoardTemplate_DismissFormIsPlainPostWithNoAutoSubmit`,
  `TestBoardHandler_RebuildsFiltersRatherThanEchoingRawQuery`,
  `TestBoardQuery_DefaultShowsTodaysDoneUntilLocalMidnight`,
  `TestBoardTemplate_DoneFormIsItsOwnPlainPost`,
  `TestBoardTemplate_DoneFormIsHumanOnlyAndStatusBlind`,
  `TestBoardServer_CloseRouteIsRegistered`,
  `TestBoardHandler_CloseTaskActionRunsNoSQL`,
  `TestBoardHandlers_ShareOneFilterRebuild`.
- `board_lights_structure_test.go`: `TestBoardDayStart_OneSpelling`,
  `TestBoardQuery_DefaultPredicateExactSQL`, `TestTaskExportRow_FieldsUnchanged`,
  `TestBoardLightFacts_IsASeparateRead`,
  `TestBoard_NoGoClockFeedsVisibilityOrALight`, `TestTasksTemplate_BoardNote`,
  `TestTasksTemplate_VerbFormsByteUnchanged`,
  `TestTasksTemplate_LegendAndRingStyles`,
  `TestBoardLightFacts_FirstStatementSelectsTheSession`,
  `TestDashboard_NoRawHTMLOnTheSessionPath`.
- `board_refresh_test.go`: `TestBoardRefresh_IntervalAndKeys`,
  `TestBoardRefreshURLs`, `TestBoardQuery_IgnoresRefresh`,
  `TestBoardBack_RebuildsFiveKeys`,
  `TestListTasks_RefreshIsARenderFlagNotAQuery`.
- `board_layout_structure_test.go`:
  `TestBoardLightFacts_FirstStatementFormatsTheUpdatedStamp` (the stamp's SQL is
  unchanged — see the deviation note in B5/Out of scope),
  `TestBoardAdvanced_IteratesBoardKeys`,
  `TestTasksTemplate_AdvancedFilterPopup`,
  `TestTasksTemplate_LegendAndNoteAtTheBottom` (its `sectionsBlock` helper now
  finds the INNER `{{range .Sections}}`; the legend and note still start after
  it — document that in a comment).
- `board_incoming_structure_test.go`: every test, including
  `TestTasksTemplate_NoIncoming` — which is why no CSS selector, class or
  comment in `tasks.html` may contain the word `incoming`.
- `board_reopen_structure_test.go`: unchanged (it only requires the phrase
  `reopened after dismissal` somewhere in the template or `board.go`).
- `lights_test.go`, `sections_incoming_test.go`, `board_close_test.go`,
  `board_advanced_test.go`, `export_test.go`, `funnel*`, `deliveries*`,
  `task_detail*`, `auth_test.go`: untouched.

**REWRITTEN because the markup changes** (each keeps its assertions, changes its
selector, and carries a comment naming this ticket):

- `board_lights_structure_test.go`
  - `TestTasksTemplate_LightSpanBeforeTheID` → `…LightSpanInTheRemarkCell`:
    still exactly one light span, still only `.Light.Class`/`.Light.Label`, now
    required to sit inside the `rem` cell immediately before `{{.Remark}}`.
  - `TestTasksTemplate_SessionTagFirstInTitleCell` → `…SessionTagIsTheGateCell`:
    same markup, same one-occurrence rule, same CSS assertions, new position.
- `board_layout_structure_test.go`
  - `TestTasksTemplate_FirstLineIsTheTopbar` → `…HeaderBandsInOrder` (B15):
    nav+topbar, then the sign header, then flash and alert, then `<main>`; the
    filter form's controls outside the `<details>` are still exactly the project
    select and the hidden refresh input; the toggle's `{{if not .AutoRefresh}}`
    landmine is re-pinned.
  - `TestTasksTemplate_SectionsRange` → `…PanesAndPanels` (criterion 21): no
    `<table>`/`<th>`/`<td>`, one `<h2` in the template, the `{{else}}` on the
    pane range.
  - `TestTasksTemplate_RowCellsAndHeaders` → `…RowCells` (criterion 22): the
    seven cells in order, no status cell, the `time` cell keeping
    `title="{{.UpdatedAt}}"`.
  - `TestTasksTemplate_RowVerbsBehindAnActionsPopup` → `…RowVerbsOutsideTheRowLink`
    (criterion 22's second half).
  - `TestTasksTemplate_ScriptPostponesWhileAPopupIsOpen` → `…ScriptContract`
    (criteria 27, 28).
  - `TestTasksTemplate_CompactLayoutStyles` → `…DeparturesStyles`: new CSS
    goldens (the palette is the ticket); it KEEPS the `.popup`,
    `details.advanced-filter`, `details.row-verbs`, `.advanced-active` and
    `.session-tag` ellipsis rules and the six `.light-*` classes with the ring
    shape.
- Integration helpers (one change each, used by several files):
  - `boardLight` and `onBoard` (`board_lights_integration_test.go`) are rebuilt
    on a new row-scoped helper `boardRow(body string, id int64) string` that
    slices the row wrapper containing `href="/tasks/{id}"`. Every call site's
    assertions are unchanged.
  - `sessionTag` (`board_session_integration_test.go`) reads the tag inside
    `boardRow`.
  - `lyRowID`, `lyRowHTML`, the per-id "renders exactly once" regexes and the
    `updated` cell regex (`board_layout_integration_test.go`), the per-id regex
    in `board_incoming_integration_test.go`, and `brrRow`
    (`board_reopen_integration_test.go`) all move onto `boardRow`.

**AMENDED deliberately** (not deleted; each gets a comment naming this ticket):

- `sections_test.go` `TestBoardSectionOrder_SevenPairsInOrder`: the seven
  `Key/Title` pairs gain the new titles (B3). The keys and the ORDER are
  unchanged, which is the half that matters.
- `board_layout_integration_test.go` `TestBoardLayout_Integration_SectionsFollowTheLights`
  and `…StatusFilterKeepsGrouping`: their `want` strings take the new titles.
  Membership, counts and row order are unchanged.
- `board_incoming_integration_test.go` `assertIncomingIDs`: `s.title != "incoming"`
  becomes the new title.
- `board_layout_structure_test.go` `TestListTasks_BuildsSectionsAndAdvancedFilters`:
  additive — the new `boardData` fields and the `boardPanes`/`boardTallies`
  calls. The `Columns`/`statusColumn`/`byStatus` bans stay.
- `board_layout_structure_test.go` `TestTasksTemplate_NoBranchNoHTMXNoRawHTML`:
  additive — `display.go` joins the scanned file list.
- `board_refresh_test.go` `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript`:
  the script moves OUT of the `{{if .AutoRefresh}}` block and gains
  `data-refresh`/`data-page-interval`; every other assertion (one script, the
  indicator's bytes, the toggle outside the block, the hidden refresh input, the
  banned tokens, the one-`onchange` count) is unchanged.

## Data model changes

**None.** No migration, no new column, no new table; the ledger in
`internal/classify/structure_test.go` is untouched. The only new SQL is one
arithmetic expression over `tasks.working_state_at`, a column the same statement
already reads.

## API / MCP tool changes

**No tool, no pin, no schema, no policy rule, no executor change.** One new HTTP
route, `GET /static/{path...}`, serving embedded bytes with no database access
and no executor call (B17). `GET /tasks` renders differently;
`POST /tasks/{id}/dismiss` and `POST /tasks/{id}/close` are byte-unchanged and
still run through `executeTask` → executor → policy → audit.

## MQTT topics

None.

## Files likely to touch

New:
- `internal/dashboard/display.go` — Part 1's pure helpers.
- `internal/dashboard/display_test.go` — criteria 1–7.
- `internal/dashboard/board_departures_structure_test.go` — criteria 8–13, 17–32.
- `internal/dashboard/board_departures_integration_test.go` — criteria 33–39.
- `internal/dashboard/static/fonts/*.woff2`, `OFL-B612.txt`,
  `OFL-BarlowCondensed.txt`, `SOURCES.md`.
- `internal/dashboard/static/manifest.webmanifest`, `icon-192.png`,
  `icon-512.png`.
- At deliver time: `docs/runbooks/HANDOFF-kube-board-departures.md`.

Changed:
- `internal/dashboard/templates/tasks.html` — the whole restyle (Part 4).
- `internal/dashboard/board.go` — `taskRow` (+4 fields), `boardData` (+5),
  `boardPageInterval`, `listTasks`, `boardLightFacts`' first statement.
- `internal/dashboard/lights.go` — `lightFacts.StateAgeMinutes` only.
- `internal/dashboard/sections.go` — the seven titles, and
  `boardSection.Class`/`Order`.
- `internal/dashboard/server.go` — `//go:embed static`, the `/static/` route.
- The test files of Part 7.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — a new entry, "Board departures view",
  plus one line in the SWT-52 entry (the script is now unconditional and
  paging postpones reloads) and one in the SWT-57 entry (panes and CSS `order`
  keep DOM order).

Deliberately NOT touched: `boardQuery`, `export.go`, `auth.go`, every other
template, `internal/tools`, `internal/policy`, `internal/mcpserver`,
`internal/orchestrator`, `internal/capture`, `migrations/`, `skills/`,
`kube/` (another session owns the manifests).

## In scope / Out of scope

**In scope:** the departures restyle of `/tasks` and only `/tasks`; the panes
and panel table; the new section titles; the row markup and its verbs
affordance; the Go display helpers; the one new SQL expression; the single
script (clock, paging, fullscreen, wake lock, refresh); self-hosted fonts, the
manifest, the icons and the `/static/` route; the phone breakpoint and the
viewport meta; the tests above; the IK entry and the kube handoff.

**Out of scope, each named because it is a tempting bundle:**

- **The TLS certificate / Ingress** that would make the manifest installable and
  the wake lock work. Follow-up kube handoff (B18).
- Any change to `boardQuery`, `TaskExportRow` or the CSV/JSON exports.
- Any change to `lightFor`'s table, `sectionFor`'s table, `boardSectionOf`,
  `pickQueueHeads` or the queue order.
- New board verbs (Start, Stop, Answer, Reopen, Delivered) or any change to
  Dismiss/Done semantics or to `task_signal`.
- Changing the `updated` stamp's format to the mock's `MM/DD` — a deliberate
  deviation: the `HH:MM` / `YYYY-MM-DD` spelling is a shipped, tested contract
  (SWT-57 L8) and it is also the cell's `title`. The Time column is widened
  instead. One line to reverse later if he wants the year gone.
- Restyling `/tasks/{id}`, `/deliveries`, `/plans`, `/briefs`, `/sources`,
  `/funnel` or the exports. Nav links stay; those pages keep today's look.
- Push refresh (NOTIFY / MQTT-over-WS) instead of the 5 s poll.
- A service worker, offline mode, a native or mobile app, or any install prompt
  handling.
- Per-user preferences of any kind (remembered page, column choice, theme):
  nothing about this board is stored anywhere.
- Anything in SWT-52's, SWT-56's, SWT-57's or SWT-59's own Future work.

## Invariants that apply

1. **Raw-first** — not exercised. No connector, no ingestion, no
   `raw_source_items`. The diff touches no provider code.
2. **One funnel** — no table, no status, no queue-like structure is added. The
   panes are a computed VIEW of `boardSections`' output, which is a computed
   view of the one filtered `tasks` read. "Queues are filters, not tables" holds
   literally: the `queue` panel is still the ready rows in
   `tools.TaskQueueOrder`, ranked by Postgres. Nothing is persisted — no cookie,
   no preference, no paging state, no server state.
3. **Everything through the executor** — the board's only actions remain Dismiss
   (`task_dismiss`) and Done (`task_close`), each one `executeTask` call with
   `TaskID` set, from forms whose bytes do not change. Concretely for THIS
   ticket: the verbs move inside the DOM (out of a `<td>`, into a row-wrapper
   `<details>` beside the row link) and **nothing else about them moves** — same
   route, same handler, same args, same actor `dashboard:{user}`, same
   `boardBack` redirect, same policy decision and same audit rows. The new
   `/static/` route executes nothing, reads no database and takes no actor. No
   handler is added that touches `tasks`.
4. **Nothing external without a delivery row** — nothing is sent; no
   `deliveries` row is read or written; the page makes no outbound request at
   all, which criterion 18 and 37 enforce by banning third-party URLs.
5. **Own-message loop closure** — untouched; no normalizer, no external id, no
   matcher.
6. **Stealth attribution** — nothing here is client-visible: the dashboard is
   Salvador's own board, reachable only inside the home network. No AI byline,
   name or marker appears in the markup, the manifest, the icons or the vendored
   font metadata; the `session` tag it does show is the Claude session's
   self-reported name, which SWT-56 already put there for him alone.
7. **Orchestrator purity** — no file under `internal/orchestrator` changes. All
   the new Go is pure display logic (`display.go`, structure-scanned for
   `time.`, `s.pool`, `Query(`, `Exec(`), and every time-dependent fact —
   "today", "stale", "elapsed", "last refreshed" — is still decided by Postgres
   `now()` in `BoardTimeZone` and arrives as a boolean, an int or a formatted
   string. The one clock in the browser is decoration (B14).

## Sibling patterns to copy

- **Pure function + purity scan:** `lightFor`/`TestLights_PureNoIONoClock`
  (`lights.go`, `lights_test.go`) and `sectionFor`/`TestSections_PureNoIONoClock`.
  `display.go` and `display_test.go` are their third sibling; reuse
  `factCombos` and `boardStatusOrder` for the exhaustive remark table.
- **Display-only facts that the light must not read:** `QueueRank`/`UpdatedStamp`
  (SWT-57) and `FromMessage`/`PRReview` (SWT-59), with
  `TestLightFacts_IncomingFactsAreDisplayOnly` as the scan to copy for
  `StateAgeMinutes`.
- **A fact computed in SQL and formatted in Go:** `UpdatedStamp` in
  `boardLightFacts`' first statement — including
  `TestBoardLayout_Integration_UpdatedStampIsShortAndColumnFed`, the
  column-fed integration test whose mutation (replace the expression with a
  literal) must turn it red. `state_age_min` gets the same treatment.
- **Template structure scans:** `tasksHTML`, `templateBlockAfter`,
  `elementEnd`, `allBlocksAfter`, `funcBodySrc`, `structFieldNames`
  (`board_structure_test.go`, `board_lights_structure_test.go`,
  `board_layout_structure_test.go`).
- **Row-local integration asserts that ignore row order:** `boardLight`,
  `onBoard`, `sessionTag`, `lyRowHTML`; this ticket funnels them through one
  `boardRow` slicer.
- **SQL-computed fixture instants that survive the SWT-48 evening:** `lsDayStart`.
- **The real executor for signal fixtures:** `lightsExecutor`, `lsCall`,
  `lsSignal`.
- **Embedded assets:** `//go:embed templates/*.html` in `server.go` is the only
  embed in this package; `static` follows it. For the handler, `fs.Sub` +
  `http.FileServerFS` (Go 1.25 in `go.mod`).
- **`FOR UPDATE SKIP LOCKED` / jobagent:** not used. This ticket claims nothing.
- **rag-svc's HTMX handlers:** deliberately NOT copied. The board has no HTMX
  (SWT-31 D8, pinned by two tests) and needs none: `<details>`, a GET form and
  one plain script do all of it.

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|----------|----------|
| `boardPanes` drops a section key (e.g. `other`) | criterion 2's coverage test; criterion 33 |
| `boardPanes` reorders a pane's sections by `Order` instead of keeping `boardSectionOrder` | criterion 33 (the `<h2>` document order), SWT-59's first-`<h2>` test |
| `incoming` given `order: 0` | criterion 33's per-panel `style="order:N"` |
| `remarkFor` returns one constant | criterion 3 (the table), criterion 34 |
| `remarkFor` loses its `pr_open` branch | criterion 3 (B5: the status must survive as words) |
| `elapsedFor` returns a value for `done` | criteria 4, 34 |
| `projectHue` returns a constant | criterion 5's two-distinct-hues control, criterion 36 |
| Replace `state_age_min`'s expression with a literal `0` | criterion 34 (the elapsed cell reads `00:00` for a signalled row) — the column-fed rule; the unit test cannot catch it, by construction |
| `lightFor` reads `StateAgeMinutes` | criterion 8's structure scan |
| Move the `<details class="row-verbs">` inside the row `<a>` | criterion 22, and an HTML validity check by eye |
| Drop the Dismiss form from claude rows | criterion 35 |
| Re-indent one line inside the Done form | `TestTasksTemplate_VerbFormsByteUnchanged` |
| Put the `@font-face` `src` back on `fonts.gstatic.com` | criteria 18, 37 |
| Wrap `/static/` in `s.auth.Require` | criterion 37 (the cookie-less client gets 302) |
| Remove the page-not-1 clause from `busy()` | criterion 28 |
| Paging rebuilt with `innerHTML` | criterion 27's banned tokens |
| Two `<script>` blocks (paging split out) | criterion 27, `…AutoRefreshToggleIndicatorAndOneScript` |
| A CSS rule named `.panel-incoming` | `TestTasksTemplate_NoIncoming` |
| Add a `Columns` field to `boardData` | `TestListTasks_BuildsSectionsAndAdvancedFilters` |

## Verification protocol

Run in this order. Do not commit before step 5 passes.

1. **Unit:** `go test ./...`. The SWT-48 `TestAttributionTrend_*` flake
   (20:00–24:00 EDT) is pre-existing; re-run with `TZ=UTC` if it fires.
2. **Integration, on an ISOLATED database** — never prod, never the shared
   compose `ops` db (the 2026-09-12 landmine):

   ```
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_boarddep"
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_boarddep?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boarddep?sslmode=disable' \
     go test -tags integration -p 1 -count=1 ./internal/dashboard/
   ```

   Run it TWICE (rerunnable), then `go test -tags integration -p 1 ./...` once
   against the same URL.
3. **Mutations:** each row of the table above, red then reverted.
4. **Asset provenance:** `sha256sum internal/dashboard/static/fonts/*.woff2`
   matches `SOURCES.md`, and `grep -ri "gstatic\|googleapis" internal/dashboard/`
   returns nothing.
5. **Local smoke, both viewports.** `DATABASE_URL=<ops_boarddep url> go run ./cmd/dashboard`
   (:8085, `/dev/login?user=salvo`). Seed a throwaway project with ~25 human
   `ready` tasks (one priority 2), two blocked (one via
   `opsctl call --tool task_signal --args '{"task_id":N,"state":"needs_input","session":"shell"}'`,
   one status `blocked`), two in flight (one `working` signal, one claude
   `in_progress`), one `holding`, one closed today.
   - **Tablet, Chrome DevTools device toolbar 1000×700**, `/tasks?project=<slug>&refresh=on`:
     - the sign header, the five tallies, the ticking clock and `FULL` are on one
       line; the chrome bar above it still has the nav, the project select and
       `Advanced filter`;
     - the left column reads NEEDS YOU, IN FLIGHT, ARRIVALS — INCOMING; the right
       DEPARTURES — QUEUE, HOLDING, LANDED TODAY; nothing scrolls;
     - the queue panel pages: `page 1/3` → `page 2/3` with the flip, about every
       9 s, and the "last refreshed" time does NOT move until the cycle returns to
       page 1 (B13);
     - a row's `⋯` opens the popup, reloads stop, Done flashes once and the
       filters and `refresh=on` survive; closing it un-paused, reloads resume;
     - `FULL` goes full-screen and back; the DevTools console is clean (no wake
       lock error over http);
     - view-source: exactly one `<script`, no `<details … open`, no
       `googleapis`;
     - the network panel shows the five woff2 files served from `/static/` and
       nothing off-host.
   - **Phone, 390×844:** one column, no column headers, dot + title + id +
     elapsed + remark, the queue cut to five rows with the true count in the
     heading, a sticky footer, normal scrolling, no paging.
   - **JS off:** the board still renders every row and the panels scroll.
   - Drop the db afterwards (`DROP DATABASE ops_boarddep`).
6. **Deploy: image only, no migration.** Build and push
   `192.168.50.20:5000/switchboard:<tag>` here; hand the tag bump to the kube
   session in `docs/runbooks/HANDOFF-kube-board-departures.md`. Only
   `deployment/dashboard` needs it. No env var, no port, no manifest change
   beyond the tag, no ordering constraint. **Post-roll:**
   `kubectl -n ops port-forward svc/dashboard 8085:80`, open `/tasks?refresh=on`
   on the real tablet, confirm the fonts load from `/static/` and
   `pg_stat_activity` shows no new statement shape.
7. **Rollback:** previous image tag. Nothing is stored, no schema changed, every
   bookmark stays valid (the URL keys are untouched).

## Future work (not this ticket)

- **A certificate the tablet trusts** (and an Ingress if there is none), which
  turns the shipped manifest into a real installable full-screen app and lets the
  wake lock hold the screen on. Kube handoff, §Follow-up.
- **Tap-to-read a light's label** on touch screens (no hover), now that every
  row has a `<details>` neighbour to copy.
- **Per-section collapse**, or an anchor bar, if a panel's page count gets silly.
- **Push refresh** (NOTIFY or MQTT-over-WS) instead of the 5 s poll — SWT-52's
  Future work, which would also retire the paging/reload interaction of B13.
- **A board Stop verb** to clear a stale ring without closing the task (SWT-52
  Future work); it belongs in the same row popup.
- **The same skin on `/tasks/{id}`** and the other pages, once this one has
  survived a week of use.

## Open questions

None. Every ambiguity the mock left — where the row verbs live, whether
`holding` merges into the queue, where `other` goes, what happens to the status
column and to the `MM/DD` stamp, whether `/static/` sits behind auth, and how
paging interacts with the 5 s reload — is decided above under "Decisions made
unilaterally", each with its rationale and each reversible in one place.
