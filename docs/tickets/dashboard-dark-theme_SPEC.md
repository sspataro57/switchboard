> Jira: SWT-62

# dashboard-dark-theme — the whole dashboard follows the tablet's night setting, from one shared template block, with no state, no script and no new route

**STATUS: DECIDED.** No open questions arose, so there is no
`dashboard-dark-theme_OPEN_QUESTIONS.md`. Every choice below is settled from the
code, the pinned tests, CLAUDE.md and the IK, and each is recorded under
"Decisions made unilaterally (with rationale)" with the measurement or the test
that forced it. PLAN ONLY — no code is written by this ticket.

## Source

Ad-hoc, from Salvador, 2026-09-16, verbatim:

> also plan for a dark theme on the ui

Context supplied with the request: he reads the board on a tablet at about
1000px CSS width, often at night, and on the desktop through
`kubectl -n ops port-forward svc/dashboard 8085:80`.

Not a build-order step. It is a presentation amendment to the SWT-10 dashboard
as extended by SWT-29 (`/funnel`, `/sources`), SWT-31/51 (Dismiss, Done),
SWT-52/56/57/59 (lights, auto-refresh, session tags, sections, incoming) and
SWT-43/44 (`/deliveries` Deny/Redo and the content hash).

## Goal

Every page the dashboard serves renders in a dark palette when the browser
reports `prefers-color-scheme: dark`, and is byte-identical to today otherwise —
delivered as ONE shared Go template block (`templates/theme.html`, invoked by all
eight pages), holding only colour overrides, with no JavaScript, no stored
preference, no new route, no Go change and no migration.

**Usable alone means:** with only the dashboard image rolled, at night, on the
tablet at 1000px:

- `/tasks?refresh=on` renders on a dark canvas. All six lights are still told
  apart (green done, yellow working, yellow *ring* stale, red needs input, blue
  next, grey ring none), the session tag's red/yellow borders still read, and the
  timestamps and `status` cells are legible rather than dim-grey-on-black.
- Tapping `actions` opens a popup that is a dark panel over the dark board, not a
  white flash; the Advanced filter popup likewise.
- Following any nav link — `/tasks/{id}`, `/deliveries`, `/plans`, `/plan/{id}`,
  `/briefs`, `/sources`, `/funnel` — keeps the dark rendering. There is no page
  that flashes white.
- On `/deliveries`, drafted / approved / sent / failed / rejected are still five
  distinguishable colours, and the Approve / Deny / Redo controls are unchanged
  in every respect except colour.
- By day (OS in light mode) every page is pixel-identical to today.
- Printing `/tasks` or `/briefs` produces a white page with dark ink.

## Decisions made unilaterally (with rationale)

### D1 — Where the CSS lives: a shared `{{define "theme"}}` block in a new `templates/theme.html`, invoked by all eight pages

Three candidates were weighed, as the brief asks.

**(a) A shared Go template block — CHOSEN.** A new file
`internal/dashboard/templates/theme.html` containing exactly one
`{{define "theme"}} <style> … </style> {{end}}`; each of the eight pages gains
ONE line in its `<head>`, after its own `</style>`:

```
  {{template "theme"}}
```

Why it wins:

- **It leaves every existing structure test's subject untouched.** The CSS-text
  scans read the page file and slice from the FIRST `<style>` to the FIRST
  `</style>` (`TestTasksTemplate_CompactLayoutStyles`,
  `TestTasksTemplate_LegendAndRingStyles`, both in
  `internal/dashboard/board_lights_structure_test.go` /
  `board_layout_structure_test.go`). Because the theme lives in a *different
  file*, `tasks.html` still has exactly one `<style>` pair and those scans keep
  scanning exactly what they scanned before. Nothing is weakened, and nothing has
  to be amended (see "Existing tests" below: zero amendments).
- **One spelling of the palette.** Every colour appears once, in one file, for
  all eight pages. Duplicating the block into eight templates (candidate (c))
  would create eight copies of a palette that must agree — the repo's recurring
  defect in a CSS costume.
- **One request.** The page stays a single self-contained HTTP response, which is
  what he gets over a port-forward. No second round trip, no cache-busting
  question, no 304 behaviour to reason about.
- **`//go:embed templates/*.html` and `template.ParseFS(templateFS,
  "templates/*.html")` (`internal/dashboard/server.go:23-43`) pick the new file up
  automatically.** No Go change at all.

**(b) A new route serving an embedded `.css` — REJECTED.** Three concrete
reasons, not taste:

1. **It moves the colours out of the structure tests' reach.** Every CSS
   assertion in this package reads `templates/*.html` through `templateFS` or the
   file system. A served asset would still *pass* those scans (the base rules stay
   where they are) while the theme itself became untested by them — the
   "predicate whose column no query selects" shape from the IK, in reverse.
   A new scan could be written against the `.css` file, so this is not fatal —
   but it buys nothing the partial does not.
2. **A second request on a port-forward, and an auth decision.** Every page route
   is wrapped in `s.auth.Require` (`server.go:54-91`). A `/static/theme.css` route
   would have to be deliberately un-gated (a stylesheet fetched before the session
   cookie exists) or gated (and then the login-redirect HTML is served as CSS on
   an expired session — an unstyled page at exactly the wrong moment). Neither is
   worth solving for a file of colours.
3. **Caching makes a rollback ambiguous.** A rolled-back image with a
   still-cached stylesheet is a support question that does not exist for inline
   CSS.

**(c) Duplicate the block into all eight templates — REJECTED.** Eight copies of
one palette; the first drift is silent and page-specific.

**No existing structure test is weakened, and nothing is compensating for a
loss.** What the SPEC *adds* is coverage of the new file:
`internal/dashboard/theme_structure_test.go` scans `templates/theme.html` with
the same idiom (`templateFS.ReadFile` + the rule-regex shape of
`TestTasksTemplate_CompactLayoutStyles`), and asserts the invocation line in each
of the eight pages using the loop shape of
`TestNavCarriesTheFunnelLinkNextToSources` (`internal/dashboard/funnel_test.go`),
which already iterates exactly these eight files.

### D2 — Switching: `prefers-color-scheme` only. No toggle, no cookie, no URL key

**Chosen: `@media screen and (prefers-color-scheme: dark)` plus
`:root { color-scheme: light dark; }`. Zero state, zero UI, zero JavaScript.**

The brief asks explicitly whether a JS-free mechanism is possible, and whether a
toggle could fit inside the existing script. Both answers, stated plainly:

- **`prefers-color-scheme` needs NO JavaScript at all**, so the flash-of-wrong-
  colour problem does not arise: the UA applies the media query before first
  paint. The one-script rule
  (`TestTasksTemplate_ScriptPostponesWhileAPopupIsOpen` and
  `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript` both assert
  `strings.Count(s, "<script") == 1`) is satisfied by construction, and the
  inline-handler scan (`\son[a-z]+=` must match exactly one `onchange=`) is
  untouched.
- **A JS toggle is disqualified, and here is the precise reason.** The one script
  renders only inside `{{if .AutoRefresh}}` — i.e. only when `refresh=on`. Any
  theme logic folded into it would not run on a board without auto-refresh, and
  would not exist at all on the other seven pages, none of which has a script.
  A second `<script>` is banned by the count. So a JS toggle needs a rule change,
  not a code change.

Why not a server-rendered toggle either (the no-JS variants):

- **A URL key is cheap but wrong here.** `boardKeys` (`board.go:98`) is the one
  list, and `boardBack` / `boardRefreshURLs` / `boardAdvanced` all iterate it, so
  adding `theme` would round-trip on `/tasks` for about ten lines. But the nav
  links are plain `href="/deliveries"` etc. in all eight templates — the key dies
  on the first navigation, and a half-dark dashboard is the outcome the brief
  names as worse than either extreme. It also does not survive a new visit.
- **A cookie is the only thing that persists, and it costs a route.** The
  dashboard's only cookie is the in-memory session (`sessionCookie = "sb_session"`,
  `auth.go:29,166-176`); there is no user-settings table and CLAUDE.md's schema
  has no place for one. A cookie-backed toggle needs: a GET route to set it, a
  read in all eight handlers, a class on `<html>`, and a decision about what a
  crafted cookie value may contain. That is a feature, not a theme.
- **Invariant 2 is the tiebreaker.** "Nothing is stored" is the strongest
  property this ticket can have: no preference row, no cookie, no URL state, no
  migration. The tablet already knows whether it is night.

**Cost of a later toggle, since it is a fair question:** one GET route
(`/prefs/theme`), a cookie write, a read in each of the eight handlers (or one
helper called by each), one `<html class="theme-dark">` hook, and swapping the
media query for `:root.theme-dark` plus keeping the media query as the default —
roughly a day with tests, and it does NOT need JavaScript. Nothing in this ticket
blocks it: the shared block is the exact place that change would land. Recorded
under Future work.

**If he ever wants always-dark regardless of the device**, that is deleting the
media-query wrapper — one line, reversible.

### D3 — Scope: all eight pages, in the same ticket

`tasks.html` (191), `funnel.html` (301), `deliveries.html` (140), `sources.html`
(95), `task.html` (81), `plan.html` (57), `plans.html` (44), `briefs.html` (25).

He said "the ui", and the failure mode of a partial job is exactly the one named
in the brief: a dark board that flashes white when he opens a task detail at
night. The nav on every page links to every other page
(`TestNavCarriesTheFunnelLinkNextToSources` pins that), so any page left light is
one tap away at all times.

The marginal cost is small and bounded, because the shared block already has to
exist: seven extra one-line `<head>` edits, and about a dozen extra selectors for
the page-specific classes (`.status-*`, `.ok/.bad/.warn`, `.frozen`,
`.headline div`, `code`, `pre`). Doing it later would mean opening the same file
again and re-running the same smoke.

### D4 — Only colour properties may appear in the dark block

The dark block's declarations are limited to:

```
color, background-color, border-color, box-shadow, color-scheme
```

(plus custom-property declarations, if the implementer uses them — see D5.)

This is mechanically enforced by the structure test, and it is what guarantees
the theme cannot move anything:

- `.session-tag`'s `max-width: 18rem`, `overflow: hidden`, `text-overflow:
  ellipsis`, `white-space: nowrap`, `display: inline-block` are untouchable, so
  the SWT-56 ellipsis behaviour and
  `TestTasksTemplate_SessionTagFirstInTitleCell`'s `<style>` assertions hold.
- `.popup`'s `position: absolute` / `z-index` and `details.advanced-filter` /
  `details.row-verbs`'s `position: relative; display: inline-block` are
  untouchable, so `TestTasksTemplate_CompactLayoutStyles`' popup assertions hold.
- `.light-stale` / `.light-none` keep `background: transparent` and their 2px
  border, so they stay RINGS: shape redundancy on top of colour, which
  `TestTasksTemplate_LegendAndRingStyles` pins.

**`border-color`, never the `border` shorthand.** `border: 1px solid X` would
reset `border-style`, and `.session-stale { border-style: dashed }` is
load-bearing (it is the stale session's second signal).

**`background-color`, never the `background` shorthand**, for the same class of
reason.

### D5 — The base (light) rules are byte-unchanged; dark is additive overrides, later in source order

No page's `<style>` block is edited at all. The dark block re-states only the
properties it must change, and wins by source order (the media query adds no
specificity, and `{{template "theme"}}` is placed *after* the page's own
`</style>`).

**A `var()` refactor of the existing rules is BANNED, and not by taste:**
`TestTasksTemplate_CompactLayoutStyles` asserts ten exact declaration lines by
`strings.Contains`, including

```
.light-done { background: #2e9d48; border: 2px solid #2e9d48; }
.session-tag { display: inline-block; max-width: 18rem; … border: 1px solid #9a9a9a; border-radius: .6rem; }
```

Replacing any hex with `var(--x)` turns that test red. The base rules stay
literal.

If the implementer wants one spelling per colour inside the dark block, custom
properties declared on `:root` *inside* the media block and referenced by the
dark rules are acceptable. (Checked against the test's naive rule parser
`([^{}]+)\{([^}]*)\}`: an `@media` wrapper makes the first inner rule part of the
media rule's "body" and later inner rules parse as ordinary rules; since
`check()` passes when ANY matching rule has all the wanted fragments, and the
base rules come first in every page file — which is what those scans read anyway —
no existing assertion changes meaning.)

### D6 — The palette, with measured contrast

**Target, held throughout: WCAG 2.1 AA — 4.5:1 for text, 3:1 for non-text state
indicators** (the six lights, the `advanced-active` marker border) against the
surface behind them. Decorative rules (table grid lines, panel borders) are
exempt and stated as such.

Surfaces:

| Token | Value | Used for |
|---|---|---|
| canvas | `#14171c` | `body` background |
| surface | `#1e222a` | `th`, `pre`, `code`, `.headline div`, `.popup` |
| line | `#39414b` | `td`/`th` borders, `.popup` border, `.headline div` border |
| text | `#e6e8eb` | `body` colour — **14.6:1** on canvas |
| muted | `#9aa3ad` | `.muted`, `h2`, `.status-rejected` — **7.0:1** on canvas |

Accents (one value per meaning, reused across pages):

| Token | Value | On canvas | Used for |
|---|---|---|---|
| red | `#f4776a` | **6.6:1** | `.light-input`, `.session-input` (text AND border), `.bad`, `.status-failed` |
| yellow | `#e8b90f` | **9.7:1** | *(unchanged)* `.light-working`, `.light-stale`, `.session-working`, `.session-stale`; `.warn` text, `.status-drafted`, `.status-proposed` |
| green | `#2e9d48` | **5.2:1** | *(unchanged)* `.light-done` |
| green-text | `#57d07a` | **9.2:1** | `.ok`, `.status-sent`, `.status-applied` |
| blue | `#6ea8fe` | **7.4:1** | `.light-next`, `.advanced-active` border, `a`, `.status-approved` |
| grey | `#9a9a9a` | **6.4:1** | *(unchanged)* `.light-none`, `.session-tag` border |
| link visited | `#c9a3ff` | **8.6:1** | `a:visited` |
| link hover | `#9cc4ff` | **10.1:1** | `a:hover` (new in dark only; light has no hover rule) |

Panels:

| Selector | background-color | border-color | colour |
|---|---|---|---|
| `.flash` | `#3a320f` | `#7a6a1f` | inherited (**10.4:1**) |
| `.orch-bad` | `#3a1a1a` | `#8a3a3a` | `#ffb3ab` (**9.2:1** on the panel) |
| `.frozen` (deliveries) | `#3a1a1a` | `#8a3a3a` | inherited |
| `div.warn` (plan.html) | `#3a1a1a` | `#8a3a3a` | inherited |

**Three light colours are deliberately NOT overridden** — green `#2e9d48`
(5.2:1), yellow `#e8b90f` (9.7:1) and grey `#9a9a9a` (6.4:1) already clear the
3:1 indicator target on `#14171c`. Fewer overrides means fewer chances to
contradict a pinned rule. Red (3.9:1 as an indicator but **3.9:1 as TEXT**, and
`.session-input` sets `color: #d63a2f`) and blue (3.7:1) are overridden: red
because it must clear 4.5:1 as text, blue for legibility beside the brighter
accents.

**Mutual distinguishability** is hue plus the existing shape redundancy: `stale`
and `none` are transparent-fill RINGS and stay so, which is how yellow-ring is
told from yellow-fill and grey-ring from everything else. That redundancy is
already pinned (`TestTasksTemplate_LegendAndRingStyles` requires `border` and
`transparent` on both), and D4 forbids the theme touching it.

### D7 — Two class names collide across templates; one is fixed by element, one is a recorded trade

Found by reading all eight files. A shared block must not assume a class means
one thing.

- **`.warn` has two meanings.** `funnel.html` / `sources.html` use it as TEXT
  (`<span class="warn">`, `<strong class="warn">`, `color: #8a6100`);
  `plan.html` uses it as a PANEL (`<div class="warn">`, background + border).
  **Resolved by element qualification:** `span.warn, strong.warn { color: … }`
  for the text meaning, `div.warn { background-color: …; border-color: … }` for
  the panel. No cost.
- **`.status-rejected` has two meanings and cannot be separated by an element**
  (both are `<td class="status-{{.Status}}">`): on `/deliveries` it is muted grey
  with `text-decoration: line-through`; on `/plans` it is red `#b3261e`.
  **Decision: one value, the muted grey `#9aa3ad`.** The deliveries page is the
  one he uses daily, and grey + line-through is its meaning.
  **Named cost:** in dark mode a *rejected plan import* on `/plans` renders grey
  instead of red. Light mode is unchanged. If that ever matters, `plans.html`
  gets its own class name — a one-line template change; recorded in Future work.
  The other three shared names agree already (`approved` blue both,
  `drafted`/`proposed` yellow both, `sent`/`applied` green both).

### D8 — Form controls, scrollbars and native widgets ride on `color-scheme`

`:root { color-scheme: light dark; }` is declared ONCE, **outside** the media
query, in the shared block. That is what makes the UA render `<select>`,
`<input type=text>`, `<textarea>`, `<button>` and the scrollbars in the dark
scheme when the user prefers dark, and leaves them exactly as today otherwise.

The theme therefore sets no control-specific colours. The rule the SPEC holds to:
**the dark block overrides only properties the light CSS explicitly sets, plus
`body`, plus links.** Links are the one exception that must be added although the
light CSS sets nothing: the UA defaults (`#0000EE` / `#551A8B`) are about 2:1 on
`#14171c` and would make the nav unreadable.

### D9 — Zebra striping is not added

There is no alternating-row rule in any template today; the grid is carried by
`td, th { border: 1px solid … }`. Adding striping would either change light mode
(out of scope) or make dark and light structurally different (a second layout to
reason about). The dark theme restates the border colour and the `th` background
and nothing else.

### D10 — Print: the dark block is `screen`-scoped

The media query is spelled `@media screen and (prefers-color-scheme: dark)`, not
`@media (prefers-color-scheme: dark)`. No page has a print stylesheet today and
nothing is routinely printed, but `/briefs` and `/tasks` are the two a human
might print or PDF, and `prefers-color-scheme` evaluation during printing is not
uniform across browsers. Scoping to `screen` makes the print rendering the
existing light one, unconditionally: no black-ink pages, by construction rather
than by browser goodwill.

## Acceptance criteria

Each is testable; the test that owns it is named in "Test plan".

1. **The file exists and is self-contained.**
   `internal/dashboard/templates/theme.html` contains exactly one
   `{{define "theme"}}` and its `{{end}}`, and NO other `{{` action. It contains
   exactly one `<style>` … `</style>` element and no other element.
2. **Nothing executable or external is in it.** `theme.html` contains no
   `<script`, no attribute matching `\son[a-z]+=`, no `hx-`/`htmx`, no
   `template.HTML`, and no external reference: none of `http://`, `https://`,
   `@import`, `url(` appears.
3. **All eight pages invoke it, in the head, after their own style.** For each of
   `tasks.html`, `task.html`, `deliveries.html`, `sources.html`, `funnel.html`,
   `plans.html`, `plan.html`, `briefs.html`: the exact line `{{template "theme"}}`
   appears exactly once, at an index greater than that file's `</style>` and less
   than `</head>`.
4. **One spelling.** The substrings `prefers-color-scheme` and `color-scheme`
   appear in `templates/theme.html` and in NO other file under
   `internal/dashboard/templates/`.
5. **The light CSS is untouched.** Each of the eight pages still contains exactly
   one `<style>` element of its own, and each still contains its light-mode
   anchors — `body { font-family: system-ui, sans-serif; margin: 2rem; color: #1a1a1a; }`
   in all eight; `.muted { color: #777;` where it exists today; `th { background:
   #f5f5f5` (tasks, deliveries, task, plan, plans) and `#f7f7f7` (funnel,
   sources). The ten declarations pinned by
   `TestTasksTemplate_CompactLayoutStyles` are byte-unchanged.
6. **Structure of the block.** `theme.html` holds exactly one
   `@media screen and (prefers-color-scheme: dark)` block, and exactly one
   `color-scheme: light dark;` declaration on `:root`, OUTSIDE that block.
7. **Property allow-list (D4).** Every declaration inside the dark block has a
   property in `{color, background-color, border-color, box-shadow,
   color-scheme}` or is a custom-property declaration (`--name:`). The strings
   `border:`, `background:`, `display:`, `position:`, `width`, `max-width`,
   `text-overflow`, `white-space`, `font`, `margin`, `padding` do not appear as
   properties inside it.
8. **Contrast, computed not eyeballed.** For each (foreground, surface) pair in
   D6's tables, the WCAG 2.1 contrast ratio computed in the test is ≥ 4.5 for
   text pairs and ≥ 3.0 for the light/indicator pairs. In addition, every 6-digit
   hex appearing in the dark block that is not on the declared surface/line
   allow-list (`#14171c`, `#1e222a`, `#39414b`, `#3a320f`, `#7a6a1f`, `#3a1a1a`,
   `#8a3a3a`) clears 3.0:1 against `#14171c`.
9. **The six lights stay six.** The dark block overrides `.light-input` and
   `.light-next` (background-color AND border-color) and nothing else under
   `.light-`; `.light-done`, `.light-working`, `.light-stale`, `.light-none` are
   absent from it. Their base rules — including `transparent` on the two rings —
   are unchanged.
10. **Session tags.** The dark block touches `.session-input` (`color` and
    `border-color` → `#f4776a`) only; `.session-tag`, `.session-stale`,
    `.session-working` are absent from it, so the grey border, the dashed style,
    the yellow and the ellipsis/max-width are as today.
11. **Panels.** `.flash`, `.orch-bad`, `.frozen` and `div.warn` each get
    D6's background-color and border-color (and `.orch-bad` its colour); the
    orchestrator alert's text clears 4.5:1 on its own panel.
12. **Tables and muted text.** `th` gets `background-color: #1e222a`; `td, th`
    get `border-color: #39414b`; `.muted` gets `#9aa3ad` (7.0:1, an improvement
    on light mode's `#777` on white, which measures ~4.5:1). No `nth-child`,
    `nth-of-type` or `tbody tr:` rule is added.
13. **Links.** `a`, `a:visited` and `a:hover` are set inside the dark block to
    D6's three values, and the dark block contains no `a { }` rule outside the
    media query.
14. **Popups.** `.popup` gets `background-color: #1e222a`, `border-color:
    #39414b` and a `box-shadow` visible on dark (`0 2px 10px rgba(0,0,0,.6)`);
    `.advanced-active` gets `border-color: #6ea8fe`. `.row-verbs .popup`'s
    `right: 0` is not restated (D4 forbids it).
15. **Print.** Because of criterion 6's `screen and`, a print rendering of
    `/tasks` and `/briefs` uses the light palette (verified in the smoke, step 4d).
16. **Rendered end to end.** With the real server, `GET` on each of `/tasks`,
    `/tasks/{id}`, `/deliveries`, `/sources`, `/funnel`, `/plans`, `/plans/{id}`,
    `/briefs` returns 200 and the body contains both
    `@media screen and (prefers-color-scheme: dark)` and that page's own base
    rules — proving `ParseFS` assembled the partial and that the page actually
    invokes it.
17. **Nothing else changed.** No file outside
    `internal/dashboard/templates/` and the new test files is modified (the IK
    entry and the kube handoff land at deliver time). Specifically unchanged:
    `board.go`, `sections.go`, `lights.go`, `export.go`, `server.go`, `auth.go`,
    `funnel.go`, `sources.go`, `migrations/`, `internal/tools`, `internal/policy`,
    `internal/mcpserver`, `internal/orchestrator`.
18. **Exports are byte-identical.** `boardQuery`, `TaskExportRow`, the CSV
    header golden and `/export/tasks.csv|json` are untouched; their tests pass
    unchanged.
19. **Every existing `internal/dashboard` test passes UNCHANGED** — unit,
    structure and integration. No existing test is amended by this ticket. That
    is part of the contract and the reviewer should check it.

## Data model changes

**None.** No migration, no column, no table, no `schema_migrations` bump.
Nothing about the theme is persisted anywhere — that is D2's whole point. The
migration-ordering landmines (0029/0030/0033/0034/0035 "apply before the image")
do not apply: this image is safe to roll against any schema the current image
runs against.

## API / MCP tool changes

**None.** No tool, no MCP schema, no profile pin, no policy rule, no route added
to `server.go`'s mux, no handler touched. `POST /tasks/{id}/dismiss`,
`POST /tasks/{id}/close` and every `/deliveries/{id}/…` route keep their exact
markup and handlers, so the SWT-43/44 content-hash tests
(`deliveries_structure_test.go`) and the SWT-31/51/52 form goldens
(`TestTasksTemplate_VerbFormsByteUnchanged`) stay green without edits.

Invariant 3's executor path is not entered at all: this ticket adds no action.

## MQTT topics

**None.**

## Files likely to touch

New:

- `internal/dashboard/templates/theme.html` — the `{{define "theme"}}` block:
  `:root { color-scheme: light dark; }` plus the single
  `@media screen and (prefers-color-scheme: dark)` block.
- `internal/dashboard/theme_structure_test.go` — criteria 1–15 (source/template
  scans and the contrast computation). ZERO I/O beyond `templateFS` and this
  package's own files.
- `internal/dashboard/theme_integration_test.go` — criterion 16
  (`//go:build integration`).

Changed (one line each, in `<head>`, after `</style>`):

- `internal/dashboard/templates/tasks.html`
- `internal/dashboard/templates/task.html`
- `internal/dashboard/templates/deliveries.html`
- `internal/dashboard/templates/sources.html`
- `internal/dashboard/templates/funnel.html`
- `internal/dashboard/templates/plans.html`
- `internal/dashboard/templates/plan.html`
- `internal/dashboard/templates/briefs.html`

At deliver time:

- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — a new entry, "Dashboard theme
  (dashboard-dark-theme)": the shared partial, the property allow-list, the two
  colliding class names, the `screen and` print rule.
- `docs/runbooks/HANDOFF-kube-dashboard-dark-theme.md`.

Deliberately NOT touched: every `.go` file in `internal/dashboard`; `migrations/`;
`kube/switchboard/*` (not ours — see Deploy); `skills/`.

## Landmines that apply (from `.claude/INSTITUTIONAL_KNOWLEDGE.md`)

- **The toggle's text is `{{if not .AutoRefresh}}` and must stay that way.**
  ("Board layout (SWT-57)"): two structure tests take the refresh block as the
  FIRST `{{if .AutoRefresh}}` in the file / in the titlebar, and the toggle sits
  before it. This ticket does not touch the body of `tasks.html` at all — do not
  "tidy" that spelling while in the file.
- **One `<script>`, one `onchange`, no other inline handler.** Counted by
  `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript` and
  `TestTasksTemplate_ScriptPostponesWhileAPopupIsOpen`. The theme adds none.
- **The Write tool decodes `\u` / `\U` escapes.** Do not paste escape sequences
  into the CSS or the tests; the palette is plain ASCII hex.
- **`template.HTML` / HTMX are banned** on the whole path
  (`TestTasksTemplate_NoBranchNoHTMXNoRawHTML`,
  `TestDashboard_NoRawHTMLOnTheSessionPath`). The theme is static text in a
  template; no value flows through it.
- **The compose Postgres is shared by every worktree.** Run the one integration
  test in its own database (Verification step 2).
- **`go test -overlay` does not reach these tests.** See Mutations.

## In scope / Out of scope

**In scope:**

- the shared `{{define "theme"}}` partial and its invocation from all eight pages;
- the dark palette of D6, held to WCAG AA by a computed test;
- the six lights, the session-tag borders, both popups, the `advanced-active`
  marker;
- the flash panel, the orchestrator-alert panel, the `frozen` panel, plan.html's
  validation panel;
- tables, borders, muted/secondary text, `pre`/`code`/`.headline` surfaces;
- link colours (default / visited / hover);
- `color-scheme` for native controls and scrollbars;
- the `screen`-scoped media query so printing stays light;
- the structure test, the contrast test, the integration render, the
  headless-Chromium smoke;
- the IK entry and the kube handoff at deliver time.

**Out of scope — each named because it is a tempting bundle:**

- **A theme toggle of any kind**, and any persistence for one (cookie, URL key,
  a preferences table). D2 records what it would cost.
- **Any change to the LIGHT palette**, including raising `.muted`'s `#777` on
  white (measures ~4.5:1, borderline AA) — a real finding, but a separate,
  visible change to a working page.
- Zebra striping, spacing, font sizes, column widths, or dropping the
  mostly-empty `sub`/`order`/`parent` columns (SWT-57 Future work).
- `<meta name="viewport">` or a phone breakpoint (SWT-57 Out of scope; still is).
- A CSS-variable refactor of the existing light rules (D5: pinned literals).
- A static-asset route, an asset pipeline, a CSS build step, or any external
  font/stylesheet.
- Any change to `boardQuery`, `TaskExportRow`, the CSV/JSON exports, or the
  board's sections/lights/ordering logic (`sections.go`, `lights.go`).
- New board or delivery verbs, and any change to Dismiss / Done / Approve / Deny
  / Redo semantics or markup.
- Tap-to-show light labels on touch screens, per-section collapse, push refresh
  (all SWT-52/57 Future work).
- Theming anything outside `internal/dashboard` — no other switchboard component
  serves HTML.

## Invariants that apply

1. **Raw-first** — not exercised. No connector, no ingestion, no
   `raw_source_items` write; the diff touches no connector package.
2. **One funnel** — no table, no task-like store, and crucially **nothing is
   stored at all**: no preference row, no cookie, no URL state. The board stays a
   rendering of the one `tasks` table; queues stay filters.
3. **Everything through the executor** — this ticket adds NO tool call, handler
   or route, so there is no validate → policy → audit path to enter. The claim is
   testable: `server.go` is byte-unchanged (criterion 17), and the two board verbs
   still run as single `executeTask` calls from byte-identical forms
   (`TestTasksTemplate_VerbFormsByteUnchanged` unchanged).
4. **Nothing external without a delivery row** — nothing is sent. Concretely for
   this step: `deliveries.html`'s Approve / Deny / Redo forms, their hidden
   `content_hash` inputs and the `mark-sent` / `mark-failed` controls are
   untouched, so `TestDeliveriesTemplate_ApproveFormCarriesContentHash` and
   `…_RejectFormsCarryContentHash` pass unchanged. A colour change must never be
   a reason a review surface renders a different control.
5. **Own-message loop closure** — untouched; no normalizer, matcher or
   `sent_external_id` path is in the diff.
6. **Stealth attribution** — nothing here is client-visible (the dashboard is
   port-forward-only, no Ingress while OIDC is unconfigured). Two concrete
   demands: no byline, credit or generator comment in `theme.html`, and **no
   external resource** — no CDN font, no hosted stylesheet — so the page makes no
   third-party request from his network (criterion 2).
7. **Orchestrator purity** — the orchestrator is not touched. There is no Go
   logic and no clock in this ticket at all: the theme is static CSS, and
   "night" is decided by the device, never by `time.Now()` (which
   `TestBoard_NoGoClockFeedsVisibilityOrALight` would catch anyway).

## Sibling patterns to copy

- **The shared line across all eight templates:**
  `TestNavCarriesTheFunnelLinkNextToSources` in
  `internal/dashboard/funnel_test.go` — it already loops the exact eight file
  names and asserts a per-file fragment with a positive control. Copy its shape
  for criterion 3.
- **Template CSS scanning:** `TestTasksTemplate_CompactLayoutStyles` in
  `internal/dashboard/board_layout_structure_test.go` — its `([^{}]+)\{([^}]*)\}`
  rule parser plus the `check` / `is` / `suffix` helpers are the right tool for
  criteria 6–14 over `theme.html`.
- **Reading an embedded template in a test:** `tasksHTML(t)` and
  `templateFS.ReadFile` (`board_lights_structure_test.go:275`).
- **Template block slicing:** `templateBlockAfter` (`board_structure_test.go:305`)
  and `elementEnd` (`board_layout_structure_test.go:33`) if the test needs to
  bound the `{{define}}` or the `<style>` element precisely.
- **Integration harness:** `dashGuard`, `dashPool`, `newDashServer`, `get`,
  `snippet` in `internal/dashboard/dashboard_integration_test.go`; `bdInsID` in
  `board_dismiss_integration_test.go`; `lsProject` / `lsDayStart` in
  `board_lights_integration_test.go` if the render test needs a seeded row.
  Follow the cleanup pact: own slug, FK-ordered, `policy_decisions` and
  `audit_events` by `task_id` first (the SWT-37 landmine), rerunnable.
- **Positive controls before a negative assertion:** `funnel_test.go`'s
  `POSITIVE CONTROL FAILED` idiom — a scan whose subject is absent must FATAL,
  not pass.
- **jobagent's `FOR UPDATE SKIP LOCKED`:** not used. This ticket claims nothing.
- **rag-svc's HTMX handlers:** deliberately not copied. The board has no HTMX
  (SWT-31 D8, pinned in three tests), and a theme needs none.

## Test plan

**Unit / structure — `internal/dashboard/theme_structure_test.go`** (no DB, no
network, reads only `templateFS` and this package's files):

- `TestThemeTemplate_IsOneDefineWithOneStyleAndNothingExecutable` — criteria 1, 2.
- `TestAllPagesInvokeTheThemeAfterTheirOwnStyle` — criterion 3, over the eight-file
  list, with a positive control that each file has a `</style>` and a `</head>`.
- `TestTheme_IsTheOnlyPlaceThatNamesColorScheme` — criterion 4.
- `TestPageStylesAreUnchanged` — criterion 5 (the light anchors per file).
- `TestThemeBlock_ShapeAndPropertyAllowList` — criteria 6, 7, 9, 10, 14: parse the
  block's declarations, assert the property allow-list, assert `.light-done` /
  `.light-working` / `.light-stale` / `.light-none` / `.session-tag` /
  `.session-stale` / `.session-working` are ABSENT from it.
- `TestThemeContrastMeetsAA` — criterion 8: a pure WCAG 2.1 relative-luminance
  computation over D6's pair table, plus the "every other hex clears 3:1" sweep
  with the surface allow-list. `rgba(...)` values are skipped explicitly (the one
  `box-shadow`).
- `TestThemePanelsAndLinks` — criteria 11, 12, 13.

**Integration — `internal/dashboard/theme_integration_test.go`**
(`//go:build integration`, env-gated on `DATABASE_URL`, isolated DB):

- `TestTheme_Integration_EveryPageCarriesTheDarkBlock` — criterion 16. Seed one
  project with one task and one plan import (the `seedDash` shape), then GET all
  eight paths through `newDashServer`'s logged-in client and assert 200 plus both
  markers. This is the assertion that would catch a renamed `{{define}}` (which
  makes `ExecuteTemplate` fail with a 500), a page that forgot the invocation, and
  an `embed`/`ParseFS` mistake — none of which a file scan can see.

**Headless-Chromium smoke, 1000×700, BOTH schemes** (Verification step 4).

**Everything existing** — the whole `internal/dashboard` unit and integration
suite must pass with zero edits (criterion 19).

## Mutations that must turn a test red

**Read this first: `go test -overlay` does NOT reach these tests.** The structure
tests read templates through `//go:embed` (baked into the binary at compile time
from the real files on disk) and read `.go` sources with `os.ReadFile` at run
time. An overlay changes neither. **Every mutation below must be a REAL edit in a
throwaway copy of the worktree**, never in the worktree itself:

```
cp -a /home/salvo/projects/personal/wt/darktheme \
      /tmp/claude-1000/-home-salvo-projects-personal-switchboard/<session>/scratchpad/mut
# or, to be sure nothing is shared:
#   tar -cf - -C /home/salvo/projects/personal/wt/darktheme . | \
#     (mkdir -p <scratchpad>/mut && tar -xf - -C <scratchpad>/mut)
cd <scratchpad>/mut && <edit> && go test ./internal/dashboard/   # expect RED
```

Delete the copy afterwards. Run each row, watch it fail, then discard.

| Mutation | Red test |
|---|---|
| Delete the `{{template "theme"}}` line from `funnel.html` (or any one page) | `TestAllPagesInvokeTheThemeAfterTheirOwnStyle`; and criterion 16's integration render for that path |
| Move the invocation ABOVE the page's own `</style>` | same test (the ordering assertion) — and the theme would silently lose to the light rules |
| Rename the define to `{{define "dark"}}` while leaving the call sites | the integration render (500 from `ExecuteTemplate`), NOT the file scan — this is why criterion 16 exists |
| Change the canvas to `#2a2f37` (raising it until muted text drops below 4.5:1) | `TestThemeContrastMeetsAA` |
| Swap `.muted` to `#6b7280` in the dark block | `TestThemeContrastMeetsAA` (the pair check, and the ≥3:1 sweep) |
| Add `.light-done { background-color: #1e6b32 }` to the dark block | `TestThemeBlock_ShapeAndPropertyAllowList` (the absent-list) and `TestThemeContrastMeetsAA` (2.4:1) |
| Use the `border:` shorthand for `.session-input` | the property allow-list (and `.session-stale`'s dashed style would silently die) |
| Add `max-width: 12rem` to `.session-tag` inside the dark block | the property allow-list |
| Drop the `screen and` from the media query | criterion 15's print check in the smoke (step 4d) — recorded as smoke-only, no automated test |
| Put `prefers-color-scheme` into `tasks.html`'s own `<style>` as well | `TestTheme_IsTheOnlyPlaceThatNamesColorScheme` |
| Replace `#2e9d48` with `var(--green)` in `tasks.html`'s base rule | the pre-existing `TestTasksTemplate_CompactLayoutStyles` |
| Add a second `<style>` to `deliveries.html` for a page-local override | `TestThemeTemplate_…`'s one-style-per-page half of criterion 5 |
| Add a `<script>` to `theme.html` | criterion 2, and `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript` stays green — which is the point: the count is per-file, so criterion 2 is the guard that matters |

## Verification protocol

Run in this order. Do not commit before step 4 passes.

1. **Unit:** `go test ./...`. Capture the exit status; do not chain a commit onto
   a pipeline (IK: "Gate commits on test exit status"). The SWT-48
   `TestAttributionTrend_*` flake between 20:00 and 24:00 EDT is pre-existing —
   re-run with `TZ=UTC` if it fires.
2. **Integration, in an ISOLATED database** (never prod; never the shared compose
   `ops` db — the 2026-09-12 landmine):

   ```
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_darktheme"
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_darktheme?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_darktheme?sslmode=disable' \
     go test -tags integration -p 1 -count=1 ./internal/dashboard/
   ```

   Run it TWICE, to prove it is rerunnable.
3. **Mutations:** every row of the table above, in a tar/`cp -a` copy of the
   worktree, each watched going red and then discarded.
4. **Headless-Chromium smoke at tablet width, in BOTH schemes.** Start the local
   dashboard (`DATABASE_URL=<ops_darktheme url> go run ./cmd/dashboard`, `:8085`),
   seed a throwaway project with ~15 ready tasks, one `needs_input` signal, one
   `working` signal, one `blocked`, one closed-today, one drafted delivery, then:

   a. **Light (control):**

      ```
      chromium --headless=new --window-size=1000,700 \
        --screenshot=<scratchpad>/light-tasks.png \
        'http://localhost:8085/dev/login?next=/tasks%3Frefresh%3Don'
      ```

      The screenshot must be indistinguishable from the pre-change board.

   b. **Dark:** re-run with the browser reporting a dark preference. Use
      `--force-dark-mode` if it flips the media query on this Chromium build, and
      otherwise drive `Emulation.setEmulatedMedia` over
      `--remote-debugging-port`. **Do not trust the flag: prove it.** Before the
      screenshot, evaluate
      `getComputedStyle(document.body).backgroundColor` and require
      `rgb(20, 23, 28)`. A smoke whose dark mode was never actually enabled is
      the "fixture shaped like the assertion" failure in a browser costume.
      (This evaluation happens in the debugger, not in the page — no template
      script is added.)

   c. **Eyes on, in dark, at 1000×700:**
      - `/tasks?refresh=on`: the six lights in the legend are individually
        identifiable; the yellow ring and the grey ring read as rings; a red row
        and a yellow row show their session tags with the right border colour;
        the `status` and `updated` cells are readable, not near-black.
      - Open `Advanced filter`: the popup is a dark panel with a visible edge over
        the board, and the board does not reload while it is open (SWT-57 L6 still
        holds — the theme touches nothing in the script).
      - Open a row's `actions`: same, and the Dismiss `<select>`, the note input
        and the buttons are dark native controls (that is `color-scheme` working).
      - Tap Done: the flash panel is legible on dark and shows once.
      - Force an orchestrator alert if convenient (or check `/funnel`) — the red
        alert panel is legible.
      - `/deliveries`: the five status colours differ; the textarea and the
        `subject` input are dark; Approve / Deny / Redo are all readable.
      - `/funnel`, `/sources`: `ok` / `stale` / `partial` / `never` differ; the
        `.headline` boxes and `<code>` spans have a visible surface.
      - `/tasks/{id}`, `/briefs`: the `<pre>` body blocks have a surface and the
        text is legible.
      - `/plans`, `/plans/{id}`: statuses readable (noting D7's grey `rejected`),
        and the validation panel legible.
      - Every page: nav links and in-table links are blue-on-dark and visited
        links are distinguishable.
   d. **Print check:** with the OS still in dark mode, open the browser's print
      preview for `/tasks` and for `/briefs`. Both must show the LIGHT rendering
      on a white page. This is criterion 15 and has no automated test.
   e. Drop the database afterwards (`DROP DATABASE ops_darktheme`).
5. **Deploy: image only, no migration.**
   - Build and push `192.168.50.20:5000/switchboard:<tag>` from this session.
   - Hand the tag bump to the kube session in
     `docs/runbooks/HANDOFF-kube-dashboard-dark-theme.md`. **That session owns
     `kube/switchboard/dashboard.yaml`; this session never edits manifests** (IK:
     "Kube manifests belong to the kube session").
   - Only `deployment/dashboard` changes behaviour. **No migration, no env var, no
     port, no manifest change beyond the tag, and no ordering constraint against
     any other workload** — nothing in this diff reads a column or a flag. Roll
     the same tag everywhere in one apply as usual, keeping the existing pins
     (classify-promote `--lane personal`; pipelined `PIPELINE_STAGES=…`).
   - **Post-roll smoke:** `kubectl -n ops port-forward svc/dashboard 8085:80`, then
     on the tablet at night open `/tasks?refresh=on` and confirm the board is
     dark and the lights read; open `/deliveries` and confirm it is dark too.
6. **Rollback:** roll `deployment/dashboard` back to the previous tag. Nothing is
   stored, no schema changed and no URL key was added, so rollback is instant and
   lossless and every bookmark stays valid. A partial rollback is also safe: a
   mixed fleet cannot disagree about anything, because no other workload renders
   HTML.

## Future work (not this ticket)

- **An explicit toggle**, if following the device ever proves wrong (e.g. he
  wants dark by day). D2 prices it: a `/prefs/theme` GET route, a cookie, a read
  in the eight handlers, a `<html>` class, and the media query kept as the
  default. Still no JavaScript.
- **Give `plans.html` its own status class**, so a rejected plan import can stay
  red in dark mode (D7's recorded cost).
- **Raise the LIGHT theme's `.muted` from `#777`** (~4.5:1 on white, borderline
  AA) — a visible change to a working page, so it deserves its own ticket and its
  own screenshot.
- **A `<meta name="viewport">` and a phone breakpoint** (carried over from
  SWT-57's Future work); the theme makes no difference to it either way.
- **A contrast check over the LIGHT palette too**, reusing this ticket's
  computation, once the light values are allowed to move.
