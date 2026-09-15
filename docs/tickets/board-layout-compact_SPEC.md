> Jira: SWT-57

# board-layout-compact — nothing blocked or in flight needs scrolling: sections from the lights, a project-only first line, advanced filters in a popup, the legend at the bottom

**STATUS: DECIDED.** No open questions arose. Every choice below was settled from
CLAUDE.md, the SWT-51/52/56 SPECs, the IK and the code, and each one is recorded
with its rationale under "Decisions made unilaterally". The owner said "build it
once the spec is ready".

## Source

Ad-hoc, from Salvador, 2026-09-15, verbatim:

> see /home/salvo/Dropbox/violations/20260915_084829.jpg there is a lof of
> realstate wasted. put the legend at the botom. the filter at the fist line to
> filter only by project. the other filters a popup that opens on advanced
> filter. I don't want o scroll to find out what is flying or blocked put those 2
> at the top blocked first and then the q

**The screenshot** is `/tasks` on his tablet, at about 1000px CSS width. From
the top:
- the nav, then the `Board` heading;
- a filter row with four controls (`all projects` select; `status`,
  `assignee_type` and `subproject` text inputs) and a Filter button;
- the muted "Queues are filters…" note, then the lights legend (two lines), then
  "auto-refresh: turn on";
- `HOLDING (1)`, then `READY (15)`, and so on.

Each row is about 90px tall, for two reasons:
- The last cell holds the Dismiss and Done forms. `form.inline` has no CSS rule
  in `tasks.html`, so the reason select, two note inputs and two buttons stack.
- `updated` prints the raw `updated_at::text`, e.g.
  `2026-09-14 21:45:59.649448+00`, which wraps onto two lines.

About one and a half rows fit above the fold, so the in-flight and blocked
sections are off screen whenever holding or ready has rows.

Not a build-order step. It is a layout amendment to the SWT-10 board, as extended
by SWT-31 (Dismiss), SWT-51 (Done), SWT-52 (lights, auto-refresh) and SWT-56
(session tags).

## Goal

`/tasks` renders its rows in six sections derived from each row's light:
**blocked**, **in flight**, **queue**, **holding**, **done**, **other**.
- A task appears in exactly one section, and each header shows its count.
- The first line is the project select plus an "Advanced filter" popup (a
  `<details>`) and the auto-refresh toggle.
- The legend and the board note sit at the bottom.
- Each row is one line tall: the verbs sit behind a per-row `actions` popup, and
  `updated` shows a short stamp in the board's time zone.

It is dashboard-only: no migration, no tool, no policy change, no new route.
`boardQuery`, `TaskExportRow` and the exports are byte-unchanged.

**Usable alone means:** with only the new dashboard image rolled, Salvador opens
`/tasks?refresh=on` on the tablet.
- Without scrolling, the first thing under the heading is one line: project
  select, `Advanced filter`, `auto-refresh on (…)`.
- Directly under it are `BLOCKED (n)` and `IN FLIGHT (n)`:
  - a red row, whether a session's `needs_input` or a worker's `needs_feedback`,
    comes before a dependency-blocked grey row;
  - a yellow row sits under IN FLIGHT;
  - then `QUEUE (n)`, in the order `task_get_next` would pick, with the blue head
    first for a single project.
- He taps `actions` on a finished row, taps Done, and the flash reads
  `task_close ok`. The board did not reload while the popup was open.
- He opens `Advanced filter`, types `human` into assignee_type, and taps Filter.
  The summary now reads `Advanced filter: assignee_type=human` in the marked
  style, with a `clear advanced` link beside it.
- Switching project keeps that filter, and it survives a Done.
- The legend and the "Queues are filters…" note are at the bottom of the page.

## Decisions made unilaterally (with rationale)

### L1 — "blocked first and then the q": sections come from the light, and the code confirms the reading

"Blocked" covers both kinds: waiting on Salvador (red) and waiting on a
dependency (status `blocked`). The code does not contradict this. It makes the
split clean, because `lightFor` already separates the two:
- a worker's `needs_feedback` → class `input`;
- a session's `needs_input` on `holding`/`ready`/`blocked` → class `input`;
- status `blocked` with no marker → class `none`, labelled "blocked on a
  dependency" (lights.go:95).

The section is a pure function of the row's status and its `lightFor` result:

| Light class | Status | Section |
|-------------|--------|---------|
| `input` (red) | any | **blocked** |
| `working` (yellow), `stale` (yellow ring) | any | **in flight** |
| `next` (blue) | `ready` | **queue** |
| `done` (green) | `closed` (not dismissed), `done_locally`, `delivered` | **done** |
| `none` (grey ring) | `blocked` | **blocked** |
| `none` | `ready` (queued behind the head) | **queue** |
| `none` | `holding` | **holding** |
| `none` | anything else: a dismissed `closed` row (visible only under `?status=closed`), or an unknown status | **other** |

**Why the light, not the status.** The status can be read ONLY for the grey ring,
where the class alone cannot say why a row is not queued. Everywhere else the
class decides, so the sections agree with the lights by construction. The
consequences:
- A `ready` or `blocked` row carrying a fresh `working` marker is in flight, not
  queued.
- A `holding` row carrying `needs_input` is blocked.
- A `claimed`/`in_progress`/`pr_open`/`awaiting_ci`/`awaiting_merge` row is in
  flight, because `lightFor` gives it `working` whatever its marker.

A status-only grouping would have put a yellow `ready` row in the queue. That is
the contradiction this avoids.

**Why six sections, not five.** The brief allowed "done today and anything
else" as one section. It is split because a dismissed row is grey and a green
section would misstate it. An unknown status is neither done nor queued. The
split costs one table entry, and the `other` section is empty on the default
board.

### L2 — Section order, and the order within a section

- **Section order:** `blocked`, `in flight`, `queue`, `holding`, `done`,
  `other`. Empty sections are not rendered, as today.
- **Within `queue`: `tools.TaskQueueOrder`, never a second spelling.**
  - `boardLightFacts`' second statement already reads every ready task
    `ORDER BY tools.TaskQueueOrder` (the D2 queue-head candidates). It now also
    records each candidate's 1-based position as `lightFacts.QueueRank`.
  - The queue section sorts on it. Unranked rows sort after ranked ones, by id:
    a row that became ready between the two statements has no rank.
  - This is the global order, which is also `task_list`'s. For one project's
    human lane it equals that queue, so the blue head is first. Re-sorting by
    priority/plan_order/created_at in Go would be the second spelling the IK
    forbids ("never a second literal").
- **Within every other section:**
  1. Light rank: `input`, `stale`, `working`, `next`, `done`, `none`. So red rows
     lead `blocked`, and a possibly-dead session's ring leads `in flight`:
     attention first.
  2. The status's position in `boardStatusOrder`, so in-flight rows read along
     the pipeline, `claimed` → … → `awaiting_merge`. Unknown statuses sort last.
  3. id ASC, today's order.

### L3 — A status filter narrows the rows; grouping still applies

With `?status=X`, `boardQuery` returns only status X, byte-unchanged
(TestBoardQuery_DefaultPredicateExactSQL pins it). Those rows then go through the
same `boardSections`. For example:
- `?status=ready` shows `blocked` (ready rows with `needs_input`), `in flight`
  (ready rows with a `working` marker) and `queue`;
- `?status=closed` shows `done` and `other` (dismissed).

There is no second code path and no "flat list when filtered". A filter narrows,
and the sections still tell the truth about what is left.

### L4 — The status moves into the row (a new `status` column)

The old section headers named the status. The new ones do not: `in flight`
mixes five statuses, and `blocked` mixes three.
- The light's label carries the detail, but only on hover, and the tablet has
  no hover.
- So each row gains a narrow `status` cell rendering `{{.Status}}` as muted text
  after the title. It is text only: the template still never branches on the
  status (`eq .Status` stays banned).
- The width comes out of the collapsed actions column (L7), which frees far
  more.

### L5 — The first line: project select, the Advanced filter popup, the auto-refresh toggle

**The shape.** One flex container, `<div class="topbar">`, holds:
- the ONE existing GET filter form, which contains:
  - the project select (unchanged, with the file's one `onchange`);
  - the hidden `refresh` input (unchanged);
  - a `<details class="advanced-filter">`;
- the `clear advanced` link, rendered only when an advanced filter is active;
- the auto-refresh toggle;
- the `{{if .AutoRefresh}}` block (indicator `<p>` and script).

The indicator's `<p>` stays byte-identical to the pinned string, and CSS makes it
sit inline (`.topbar p { margin: 0 }`).

**Implementation note (2026-09-15): the toggle's text is spelled
`{{if not .AutoRefresh}}auto-refresh: turn on{{else}}auto-refresh: turn off{{end}}`.**
With the toggle before the refresh block, the old `{{if .AutoRefresh}}…` spelling
made both `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript` and
`TestTasksTemplate_FirstLineIsTheTopbar` (each takes the refresh block as the FIRST
`{{if .AutoRefresh}}`) read the toggle's text as the block. The opening tag and the
rendered words are unchanged; go-reviewer accepted it as forced by "existing tests
pass unchanged".

**The advanced inputs stay INSIDE the one form.** The `status`,
`assignee_type` and `subproject` text inputs and the Filter button move into the
`<details>`, which lies inside the filter form.
- A form control inside a CLOSED `<details>` is still a submitted control. So
  the project select's auto-submit carries the three advanced values, exactly as
  today, and the five-key round trip is unchanged.
- A separate popup form would have dropped them on every project change.

**`<details>`, not `<dialog>`.**
- A `<dialog>` needs `showModal()` from script, or the 2025 `commandfor`
  invoker, which older tablet browsers lack.
- Script would be a second `<script` or new behaviour in the refresh script, and
  an `on*=` attribute is banned.
- `<details>` opens with no script at all.
- CSS positions its panel as an overlay (`position: absolute`), which is what
  makes it a popup rather than a row that pushes the board down.

**An active advanced filter is VISIBLY MARKED, and the popup is never rendered
open.**
- An open-by-default overlay would cover the top of the board, the very rows he
  asked to see without scrolling.
- Instead, when any of the three keys is non-empty:
  - the `<summary>` reads `Advanced filter: status=ready, subproject=web`, with
    the keys in `boardKeys` order;
  - it carries `class="advanced-active"` (bold, bordered);
  - a `clear advanced` link (`id="advanced-clear"`) sits beside it, outside the
    popup, so no tap is needed to see or undo the filter.
- So a hidden filter can never silently narrow the board: its values are on the
  first line whenever it is active.

**The chips and the clear URL come from ONE pure helper over `boardKeys`**:
`boardAdvanced(q url.Values) (active []boardFilter, clearURL string)`.
- The advanced keys are `boardKeys` minus `project` and `refresh`. So a filter
  key added to `boardKeys` later becomes advanced automatically, and the helper
  never spells `status`, `assignee_type` or `subproject` itself.
- `clearURL` keeps `project`, and `refresh=on` iff the query says `on`. It is
  re-encoded by `url.Values` into `boardURL`. It never carries `flash` or a
  foreign key.
- It is the IK's "Five keys, one list" rule: `boardBack`, `boardRefreshURLs`
  and now `boardAdvanced` all iterate the one list.

### L6 — Auto-refresh postpones while any `<details>` is open (D15 amended, one clause)

**The problem.** SWT-52 D15's script postpones on a hidden tab, a focused form
control or a dirty control. Tapping a `<summary>` focuses the summary, which is
not a form control. Without a new rule, the 5 s reload would collapse the
popup he just opened, before he reaches Done or types a filter.

**The clause.** `busy()` gains one line:
`if (document.querySelector("details[open]")) return true;`
- The markup NEVER renders a `<details>` open (L5; a structure test bans an
  `open` attribute on any `<details>` tag). So an open one always means he
  opened it.
- The rule is the same shape as the dirty-select rule: state differing from the
  markup's.
- A postponed reload re-arms, as today.

**The cost.** A popup left open stops the reloads until it is closed. That is
truthful: the indicator's "last refreshed" time ages, exactly as with a
half-typed note.

**What stays the same:**
- the script's other rules and its banned tokens;
- one `<script`, one `onchange`;
- no other `on…=` attribute;
- no storage, no fetch.

### L7 — Per-row verbs behind a per-row `actions` popup; the forms stay byte-identical

The last cell becomes:

```
<td><details class="row-verbs"><summary>actions</summary><div class="popup">
  …the Dismiss form, byte-identical…
  {{if eq .AssigneeType "human"}}
  …the Done form, byte-identical…
  {{end}}
</div></details></td>
```

**Why this is squarely "real estate wasted".** The stacked forms make every row
about four lines tall. Collapsed, a row is one line, so roughly three times as
many rows fit on the tablet screen.

**Why a neutral `actions` summary**, not "Done / Dismiss":
- Claude rows have no Done.
- Naming Done in the summary would need a second
  `{{if eq .AssigneeType "human"}}`, and
  `TestBoardTemplate_DoneFormIsHumanOnlyAndStatusBlind` pins exactly one.
- The summary carries no template action at all.

**The cost:** one extra tap before Done or Dismiss. Accepted because the gain is
the whole screen, and because a verb one tap away is safer against a stray tap
on a touch screen.

**Reversal** is one template edit; nothing is stored.

**`TestTasksTemplate_VerbFormsByteUnchanged` stays untouched and green.** Its
regex spans `<form … action="/tasks/{{.ID}}/dismiss">` to `</form>`, including
the inner lines' exact indentation (10 spaces before inputs, 8 before
`</form>`). The implementer re-indents only the lines OUTSIDE the two forms.
Everything the SWT-31/51/52 tests pin about the forms holds unchanged:
- the five hidden inputs;
- two forms, not one;
- no `onchange` in them;
- Done inside the one human conditional, and Dismiss outside it.

The popup is an overlay anchored right (`right: 0`), so opening one does not
reflow the table.

### L8 — `updated` shows a short stamp in `BoardTimeZone`, computed in SQL; the raw value stays in `title`

- **The expression.** `boardLightFacts`' FIRST statement, the one that already
  reads session facts and the render time, gains:

  ```sql
  COALESCE(CASE WHEN t.updated_at >= <boardDayStart($2)>
                THEN to_char(t.updated_at AT TIME ZONE $2, 'HH24:MI')
                ELSE to_char(t.updated_at AT TIME ZONE $2, 'YYYY-MM-DD') END, '') AS updated
  ```

  It shows `HH:MM` for an update since local midnight, and the date otherwise:
  at most 10 characters, one line.
- **What it uses:** the DB clock, `BoardTimeZone`, and the one `boardDayStart`
  spelling. That is D5's rule, and `TestBoard_NoGoClockFeedsVisibilityOrALight`
  already scans `boardLightFacts`.
- **Why not in Go.** No Go `LoadLocation` is needed, and none is wanted: the
  pods run UTC, and a Go zone needs tzdata in the image.
- **Why here.** `boardQuery` and `TaskExportRow` cannot change, because they
  feed the exports.
- **The cost.** It is the same statement, so there is no added query: D15's
  count and its tracer test are unchanged.
- **How it is carried.** It travels as `lightFacts.UpdatedStamp`, a
  display-only fact that `lightFor` never reads (structure test). The cell is
  `<td class="muted" title="{{.UpdatedAt}}">{{.Updated}}</td>`:
  - the full raw timestamp is one hover away on a desktop, and on `/tasks/{id}`
    everywhere;
  - `listTasks` falls back to the raw `UpdatedAt` when the fact is missing, so
    the cell is never blank for a row that has a value.

### L9 — The legend and the note move to the bottom, content byte-unchanged

The "Queues are filters…" paragraph and the `id="light-legend"` paragraph move,
unedited, to after the sections block (after its `{{else}}…{{end}}`). Their text
stays unchanged, including the SWT-56 sentence and "Hover a light for its
reason". `TestTasksTemplate_BoardNote` and the SWT-56 legend-sentence check stay
green as they are.

### L10 — `boardStatusOrder` stays, as L2's tiebreak

It no longer orders the sections. It stays the within-section pipeline tiebreak
(L2), and `lights_test.go`'s positive control iterates it as the 0001 CHECK's
twelve statuses. Its comment is updated, and the var is not deleted.

## Acceptance criteria

### Part 1 — the sections (pure Go, `internal/dashboard/sections.go`, new)

1. `sections.go` declares:
   - `type boardSection struct{ Key, Title string; Tasks []taskRow }`;
   - `var boardSectionOrder`, the six `{Key, Title}` pairs in order:
     `blocked/blocked`, `in_flight/in flight`, `queue/queue`,
     `holding/holding`, `done/done`, `other/other`;
   - `func sectionFor(status string, l light) string`;
   - `func boardSections(rows []taskRow) []boardSection`.

   The file imports none of `time`, `context`, `net/http`, `os`,
   `database/sql` or any `pgx` path. The two function bodies contain no
   `s.pool`, `Query(`, `Exec(` or `time.` (structure scan, the
   `TestLights_PureNoIONoClock` shape).
2. `sectionFor` implements L1's table exactly. Named cases:

   | status + facts | light | section |
   |----------------|-------|---------|
   | `needs_feedback` | input | blocked |
   | `holding` + `needs_input` | input | blocked |
   | `ready` + `needs_input` + QueueHead | input | blocked |
   | `blocked`, no marker | none | blocked |
   | `blocked` + fresh `working` | working | in_flight |
   | `ready` + stale `working` | stale | in_flight |
   | `ready` + fresh `working` + QueueHead | working | in_flight |
   | `claimed`, `in_progress`, `pr_open`, `awaiting_ci`, `awaiting_merge` | working | in_flight |
   | `ready` + QueueHead | next | queue |
   | `ready`, no head | none | queue |
   | `holding` | none | holding |
   | `closed` today, `done_locally`, `delivered` | done | done |
   | `closed` + open dismissal | none | other |
   | `some_future_status` | none | other |

3. **Agreement by construction.** A table over every status in
   `boardStatusOrder` plus `some_future_status`, crossed with
   `lights_test.go`'s `factCombos`, computes `l := lightFor(st, f)` and asserts:
   - `sectionFor(st, l)` is one of the six keys;
   - class `input` → `blocked`; `working`/`stale` → `in_flight`; `next` →
     `queue`; `done` → `done`;
   - class `none` → the L1 status rows.
4. **`boardSections`:**
   - It returns sections in `boardSectionOrder`, omitting empty ones.
   - The partition holds: the multiset of returned ids equals the input's, with
     no id twice.
   - `queue` is ordered by `QueueRank` ascending, then rank-0 rows by id.
   - Every other section follows L2: light rank, then the `boardStatusOrder`
     position with unknown statuses last, then id.
   - Unit cases:
     - a mixed board, asserting the exact section/key/id sequence;
     - ranks deliberately out of id order (rank 1 on the higher id);
     - a red and a grey row in `blocked` (red first);
     - `stale` before `working`;
     - an empty input → `nil`;
     - a single section.
5. **The facts.**
   - `taskRow` gains `QueueRank int` and `Updated string`.
   - `lightFacts` gains `QueueRank int` and `UpdatedStamp string`. Both are
     documented as display-only, and `lightFor`'s body mentions neither
     (structure test).
   - `light`, `lightFor`'s table, labels and classes are unchanged.
   - The existing `lights_test.go` passes untouched.

### Part 2 — the handler and the read (`internal/dashboard/board.go`)

6. **`boardLightFacts`.**
   - Its first statement adds L8's `updated` expression, using
     `boardDayStart("$2")` and both `to_char` formats.
   - The candidate loop sets `QueueRank` to the 1-based candidate position for
     each id whose `statusOf` is `ready` (the same guard `eligible` uses).
   - It still issues at most two statements.
   - `TestBoardLightFacts_IsASeparateRead`,
     `TestBoardLightFacts_FirstStatementSelectsTheSession`,
     `TestBoard_NoGoClockFeedsVisibilityOrALight` and
     `TestBoardRefresh_Integration_NoExtraQueries` pass unchanged.
7. **`listTasks`.**
   - It builds each `taskRow` as today, plus `QueueRank` and `Updated`
     (`facts[id].UpdatedStamp`, falling back to the raw `UpdatedAt` when empty).
   - It sets `data.Sections = boardSections(rows)`.
   - It sets `data.AdvancedFilters, data.ClearAdvancedURL =
     boardAdvanced(r.URL.Query())`.
   - `boardData.Columns` and `statusColumn` are removed.
   - `Filters`, `AutoRefresh`, `RefreshSeconds`, `RefreshToggleURL`,
     `ReloadURL` and `RenderedAt` are unchanged.
   - No SQL sits in a refresh-conditioned branch
     (`TestListTasks_RefreshIsARenderFlagNotAQuery` unchanged).
8. **`boardAdvanced(q url.Values) ([]boardFilter, string)`**, where
   `boardFilter` is `{Key, Value string}`. It is pure and iterates `boardKeys`
   (structure test: the body mentions `boardKeys`, and none of `"status"`,
   `"assignee_type"`, `"subproject"`). Unit table:
   - no advanced key → `nil, ""`;
   - `project=saka&status=ready&refresh=on` → `[{status ready}]`,
     `/tasks?project=saka&refresh=on`;
   - all three keys → chips in `boardKeys` order;
   - `refresh=1` → the clear URL has no `refresh`;
   - empty values are omitted;
   - `flash`, `next=//evil.example` and `x=1` appear in neither output;
   - with only advanced keys, clear → `/tasks`.

   The helper lives in board.go and never contains the word `RawQuery`
   (`TestBoardHandler_RebuildsFiltersRatherThanEchoingRawQuery` scans all of
   board.go).

### Part 3 — the template (`internal/dashboard/templates/tasks.html`)

9. **The first line.**
   - In document order: nav, `<h1>`, flash, orchestrator alert, then
     `<div class="topbar">`.
   - The topbar holds the filter form, then the `{{if .AdvancedFilters}}`
     clear link, the `id="auto-refresh-toggle"` link, and the
     `{{if .AutoRefresh}}` block.
   - The toggle's markup `<a id="auto-refresh-toggle"
     href="{{.RefreshToggleURL}}">` and the indicator `<p>` are byte-unchanged,
     and the toggle stays outside the `{{if .AutoRefresh}}` block.
   - The filter form's only controls outside the `<details>` are the project
     select, byte-unchanged with its `onchange`, and the hidden `refresh`
     input.
10. **The popup.**
    - `<details class="advanced-filter">` lies inside `<form class="filters">`,
      and it contains the three text inputs (names, placeholders and
      `$.Filters` values unchanged) and `<button>Filter</button>`.
    - No `<details` tag anywhere in the file carries an `open` attribute.
    - The `<summary>` renders `Advanced filter`, plus, inside
      `{{if .AdvancedFilters}}`, `class="advanced-active"` and the `: key=value,
      …` list from `{{range .AdvancedFilters}}`.
    - `<a id="advanced-clear" href="{{.ClearAdvancedURL}}">clear advanced</a>`
      sits inside `{{if .AdvancedFilters}}` and outside the `<details>`.
    - No `<dialog`.
11. **The script.**
    - `busy()` contains `document.querySelector("details[open]")` (L6).
    - The file still has exactly one `<script` (inside `{{if .AutoRefresh}}`)
      and exactly one `onchange`.
    - The only inline handler remains the project select's.
    - Every token `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript`
      requires or bans is unchanged.
12. **The sections.**
    - `{{range .Sections}}` renders `<h2 id="section-{{.Key}}">{{.Title}} ({{len
      .Tasks}})</h2>` and one table per section.
    - `{{else}}` keeps `<p class="muted">No tasks match the filters.</p>`.
    - `{{range .Columns}}` and `{{.Status}}` in a header are gone.
13. **The row.**
    - Header cells, in order: id, title, status, project, sub, assignee, prio,
      order, parent, updated, and an empty last header.
    - The id cell's light span, and the title cell's session tag and reopen
      marker, are byte-unchanged. The SWT-52/56 structure tests are untouched.
    - The status cell is `<td class="muted">{{.Status}}</td>`.
    - The updated cell is `<td class="muted" title="{{.UpdatedAt}}">{{.Updated}}</td>`.
14. **The verbs** (L7).
    - The last cell is `<details class="row-verbs"><summary>actions</summary>`
      holding a `<div class="popup">` with the Dismiss form, then
      `{{if eq .AssigneeType "human"}}` and the Done form.
    - The summary contains no `{{`.
    - `TestTasksTemplate_VerbFormsByteUnchanged`,
      `TestBoardTemplate_DoneFormIsItsOwnPlainPost`,
      `TestBoardTemplate_DoneFormIsHumanOnlyAndStatusBlind` and
      `TestBoardTemplate_DismissFormIsPlainPostWithNoAutoSubmit` pass UNCHANGED.
15. **The bottom.**
    - The board-note `<p>` and the `id="light-legend"` `<p>` lie after the
      closing `{{end}}` of `{{range .Sections}}`, with text byte-identical to
      today's.
    - Nothing between `<div class="topbar">` and `{{range .Sections}}` mentions
      `Lights:` or `Queues are filters`.
16. **The CSS.**
    - The `<style>` block gains `.topbar` (flex, wrap), `.topbar p { margin: 0
      }`, the two details rules (`position: relative; display: inline-block`),
      and `.popup` (`position: absolute; z-index; background: #fff; border`).
    - `.row-verbs .popup { right: 0 }`, `.advanced-active` (bold, bordered) and
      `.popup form { margin: .2rem 0; white-space: nowrap }`.
    - The old `form.filters` margin rule is replaced.
    - The six `.light-*` rules and the four `.session-*` rules are unchanged.
17. `eq .Status`, `eq .Light`, `hx-` and `htmx` stay absent. Every dynamic value
    reaches the page through `html/template`'s contextual escaping: no
    `template.HTML` anywhere on the path (`TestDashboard_NoRawHTMLOnTheSessionPath`
    unchanged).

### Part 4 — unchanged surfaces (asserted by the existing tests)

18. The following are untouched, and their tests pass unchanged:
    - `boardQuery` (TestBoardQuery_DefaultPredicateExactSQL,
      TestBoardQuery_DefaultShowsTodaysDoneUntilLocalMidnight,
      TestBoardQuery_IgnoresRefresh);
    - `TaskExportRow` (TestTaskExportRow_FieldsUnchanged);
    - the CSV header goldens (`export_test.go`);
    - `boardKeys`, `boardBack`, `boardRefreshURLs` (`board_refresh_test.go`);
    - `server.go`: no route added;
    - `task.html`;
    - everything outside `internal/dashboard`.

### Part 5 — integration (`internal/dashboard/board_layout_integration_test.go`, new; `//go:build integration`)

Harness: `dashGuard`, `dashPool`, `newDashServer`, `get` and `snippet` from
`dashboard_integration_test.go`; `bdInsID`; `lightsExecutor`, `boardLight` and
`lsDayStart` from `board_lights_integration_test.go`. Its own slug is
`itest-layout-proj`, with an FK-ordered cleanup (`policy_decisions` and
`audit_events` by `task_id` first, the SWT-37 landmine), and it must be
rerunnable.

19. **`TestBoardLayout_Integration_SectionsFollowTheLights`.** Seed one project:

    | Row | Seed | Expected section |
    |-----|------|------------------|
    | A | human `ready`, `task_signal needs_input` session `kube-c7` through `lightsExecutor` | blocked, first |
    | B | human `blocked` (raw insert) | blocked, after A |
    | E | claude `needs_feedback` (raw insert) | blocked, red, before B |
    | C | claude `in_progress` (raw insert) | in flight |
    | D | human `ready`, `task_signal working` | in flight |
    | Q1 | human `ready`, priority 0, created first | queue, SECOND |
    | Q2 | human `ready`, priority 2, created after Q1 | queue, FIRST (blue) |
    | H | human `holding` | holding |
    | X | human `closed`, `closed_at` = `lsDayStart + 1 hour` | done |

    `GET /tasks?project=itest-layout-proj` asserts:
    - The headers appear in exactly this order, with these counts:
      `section-blocked … (3)`, `section-in_flight … (2)`, `section-queue …
      (2)`, `section-holding … (1)`, `section-done … (1)`, and no
      `section-other`.
    - Each seeded id's light span occurs exactly once.
    - Each row lies between its section's `<h2` and the next `<h2`.
    - Within `blocked`, A and E (red) precede B (grey).
    - Q2 precedes Q1. id order would put Q1 first, so this proves the rank comes
      from Postgres.
    - Every row's `boardLight` class matches L1 for its section.
    - The legend (`id="light-legend"`) and the note come after the last
      `</table>`.
    - The topbar comes before `section-blocked`.
20. **`?status=ready` keeps grouping (L3).**
    `GET /tasks?project=…&status=ready` shows `section-blocked` (A only),
    `section-in_flight` (D only) and `section-queue` (Q2, Q1), and no other
    section.
21. **The advanced filter is marked, round-trips and is escaped.**
    - `GET /tasks?project=P&assignee_type=human&refresh=on`:
      - the `<summary` carries `advanced-active` and the text
        `assignee_type=human`;
      - `id="advanced-clear"` has an href whose query is exactly
        `project=P&refresh=on`;
      - no `<details` tag in the body has `open`;
      - the `assignee_type` input inside the `<form class="filters"` …
        `</form>` span has `value="human"`;
      - the Done form in row Q1 carries hidden `project=P`,
        `assignee_type=human`, `refresh=on` (regex over the rendered form);
      - C and E (claude) are absent.
    - `GET /tasks?project=P&subproject=<b>x</b>`: the summary contains
      `subproject=&lt;b&gt;x&lt;/b&gt;` and never the raw `<b>x`.
    - `GET /tasks?project=P` (no advanced key): no `advanced-active`, and no
      `advanced-clear`.
22. **The updated stamp (L8), column-fed.** Seed Q1 with `updated_at =
    lsDayStart + interval '2 hours'` and H with `updated_at = lsDayStart -
    interval '2 hours'`. The fixture instants are SQL, so the test passes at any
    hour, including 20:00–24:00 EDT (SWT-48).
    - Q1's updated cell text equals `SELECT to_char(updated_at AT TIME ZONE
      'America/New_York','HH24:MI')`.
    - H's equals the `YYYY-MM-DD` form.
    - Each cell's `title` equals the row's `updated_at::text`.
    - Neither cell text contains `+00`.
23. Every existing `internal/dashboard` integration test passes unchanged in the
    same run, notably:
    - `board_lights_integration_test.go`;
    - `board_session_integration_test.go`;
    - `board_refresh_integration_test.go` (the tracer count, and the four hidden
      refresh inputs);
    - `board_close_integration_test.go`;
    - `board_dismiss_integration_test.go`;
    - `dashboard_integration_test.go`.

## Data model changes

None. No migration, and `internal/classify/structure_test.go`'s ledger is
untouched. No column is read that the board does not already read:
`updated_at` is already selected by `boardQuery`, and here it is re-read in
`boardLightFacts`' first statement for formatting.

## API / MCP tool changes

None. No tool, no pin, no schema, no policy, no route. `GET /tasks` renders
differently. `POST /tasks/{id}/dismiss` and `/close` are unchanged, and their
forms are byte-identical. Invariant 3's executor path for the two verbs is
untouched (`executeTask`).

## MQTT topics

None.

## Files likely to touch

New:
- `internal/dashboard/sections.go`: L1/L2 (`boardSection`,
  `boardSectionOrder`, `sectionFor`, `boardSections`).
- `internal/dashboard/sections_test.go`: criteria 1–5.
- `internal/dashboard/board_layout_structure_test.go`: criteria 6 (the
  `QueueRank`/`updated` structure half), 8's structure half, and 9–17.
- `internal/dashboard/board_layout_integration_test.go`: criteria 19–22.

Changed:
- `internal/dashboard/board.go`:
  - `taskRow` (+`QueueRank`, `Updated`);
  - `boardData` (`Columns` → `Sections`, +`AdvancedFilters`,
    +`ClearAdvancedURL`); `statusColumn` removed;
  - `listTasks`;
  - `boardLightFacts` (the first statement's `updated`, `QueueRank` in the
    candidate loop);
  - `boardAdvanced` and `boardFilter`;
  - the `boardStatusOrder` comment.
- `internal/dashboard/lights.go`: `lightFacts` +`QueueRank`, +`UpdatedStamp`,
  both display-only.
- `internal/dashboard/templates/tasks.html`: L4–L9.
- `internal/dashboard/board_lights_structure_test.go`: the ONE deliberate
  amendment (below).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`:
  - a new entry, "Board layout (board-layout-compact)";
  - one line in the SWT-52 entry: the script also postpones on an open
    `<details>`.
- At deliver time: `docs/runbooks/HANDOFF-kube-board-layout-compact.md`.

Deliberately NOT touched:
- `boardQuery`, `export.go`, `server.go`, `auth.go`;
- `templates/task.html` and every other template;
- `internal/tools`, `internal/policy`, `internal/mcpserver`,
  `internal/orchestrator`;
- `migrations/`;
- `skills/`.

## Existing tests: exactly one deliberate amendment

- **`TestTasksTemplate_LegendAndRingStyles`** (`board_lights_structure_test.go`)
  is AMENDED, not deleted, with a comment naming this ticket.
  - Today it takes the legend as the text between the filter form's `</form>`
    and `{{range .Columns}}`. Both anchors move: the range is renamed, and the
    legend moves below it.
  - It will take the legend as the `<p … id="light-legend">` … `</p>` element.
    It asserts the same six word pairs there, and additionally that the element
    starts after the end of the `{{range .Sections}}` block (via
    `templateBlockAfter`).
  - The `<style>` ring assertions and the HTMX ban stay byte-identical.

Every other existing test passes UNCHANGED. That is part of the contract, and
the reviewer should check it. Listed because each one reads the parts that move:
- `TestTasksTemplate_VerbFormsByteUnchanged`, `…DoneFormIsItsOwnPlainPost`,
  `…DoneFormIsHumanOnlyAndStatusBlind`,
  `…DismissFormIsPlainPostWithNoAutoSubmit`: the forms are byte-identical
  inside the new `<details>`.
- `TestBoardTemplate_ProjectFilterAutoSubmits`: the select is byte-identical;
  one `onchange`.
- `TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript`: the toggle is
  outside the refresh block, the hidden refresh input is inside the filter
  form, the indicator is byte-identical, there is one script, and its tokens
  hold.
- `TestTasksTemplate_LightSpanBeforeTheID`,
  `TestTasksTemplate_SessionTagFirstInTitleCell`,
  `TestTasksTemplate_BoardNote`.
- `TestLightFor_*`, `TestPickQueueHeads`, `TestLights_PureNoIONoClock`.
- `TestBoardRefresh_Integration_Rendering` (≥ 4 `name="refresh" value="on"`).

## In scope / Out of scope

**In scope:**
- the six light-derived sections and their ordering;
- the `status` cell;
- the topbar with the project select and the Advanced filter popup, its active
  marker and `clear advanced`;
- the script's one `details[open]` clause;
- the per-row `actions` popup;
- the short `updated` stamp;
- the legend and note at the bottom;
- the tests above;
- the IK entry and the kube handoff.

**Out of scope, named because each is a tempting bundle:**
- Any change to `boardQuery`, `TaskExportRow` or the exports, including
  exporting the section.
- New board verbs (Start, Stop, Answer, Delivered, Reopen) or any change to
  Dismiss or Done semantics.
- Dropping or merging the mostly-empty `sub`/`order`/`parent` columns, and
  shrinking `body` margins.
- A `<meta name="viewport">` tag or a phone breakpoint.
- Tap-to-show light labels on touch screens.
- A configurable section order, collapsible sections, or remembered popup
  state.
- Sections or lights on `/tasks/{id}`, `/funnel` or `/deliveries`.
- Push refresh (NOTIFY or MQTT-over-WS) instead of the 5 s poll.
- Anything in SWT-52's or SWT-56's own Future work.

## Invariants that apply

1. **Raw-first:** not exercised. Nothing is ingested, and no connector code
   changes.
2. **One funnel:** no table and no status is added.
   - The sections are a computed VIEW of the one `tasks` table's filtered rows:
     `boardQuery` unchanged, grouped in Go at render time.
   - "Queues are filters, not tables" holds literally: the `queue` section is
     the ready rows in `TaskQueueOrder`, read from the same statement that
     picks the blue heads.
   - Nothing is stored: no popup state, no preference, no cookie.
3. **Everything through the executor:** the dashboard's only actions remain
   Dismiss (`task_dismiss`) and Done (`task_close`).
   - Each is one `executeTask` call, from forms that are byte-identical.
   - The new code performs no write and adds no SQL of its own beyond two
     extra expressions in an existing read (`boardLightFacts`' first
     statement) and a rank assigned in Go from its second.
   - No handler, route or tool is added.
4. **Nothing external without a delivery row:** nothing is sent, and no
   delivery is read or written.
5. **Own-message loop closure:** untouched.
6. **Stealth attribution:** nothing client-visible. The dashboard is Salvador's
   own, port-forward only.
7. **Orchestrator purity:** the orchestrator is not touched.
   - `sectionFor` and `boardSections` are pure dashboard functions (no I/O, no
     clock, structure-scanned).
   - "Today" in the `updated` stamp is the DB clock in `BoardTimeZone`,
     D5's rule, and never Go's clock.

## Sibling patterns to copy

- **Pure function plus purity scan:** `lightFor` and `TestLights_PureNoIONoClock`
  (`lights.go`, `lights_test.go`). `sectionFor`/`boardSections` follow them, and
  the agreement test reuses `lights_test.go`'s `factCombos` and
  `boardStatusOrder`.
- **One key list, re-encoded through `url.Values`:** `boardRefreshURLs` and its
  table test (`board_refresh_test.go`). `boardAdvanced` is its sibling. Use
  `boardURL` for the `?`-less empty case.
- **Template structure scans:** `tasksHTML`, `templateBlockAfter` and
  `funcBodySrc` (`board_lights_structure_test.go`, `board_structure_test.go`).
- **Row-local integration asserts that ignore row order:** `boardLight`,
  `onBoard` and `sessionTag`. For section membership, slice the body between
  `<h2 id="section-…">` markers.
- **SQL-computed fixture instants that survive the SWT-48 evening:**
  `lsDayStart` in `board_lights_integration_test.go`.
- **The real executor for signal fixtures:** `lightsExecutor` and `lsCall`.
- **Queue claims / `FOR UPDATE SKIP LOCKED`:** not used. This ticket claims
  nothing.
- **The rag-svc HTMX handlers:** deliberately not copied. The board has no HTMX
  (SWT-31 D8, pinned), and `<details>` needs none.

## Mutations that must turn a test red (run each, watch it fail, revert)

| Mutation | Red test |
|----------|----------|
| `sectionFor` maps by status only (`ready` → queue whatever the light) | criterion 2 (`ready` + working) and 19 (D in queue) |
| Append a row to two sections (e.g. `blocked` status also into in_flight) | criterion 4's partition, criterion 19's exactly-once |
| Sort `queue` by id instead of `QueueRank` | criteria 4 and 19 (Q2 before Q1) |
| Drop the `QueueRank` assignment in `boardLightFacts` (the column-fed rule: only Postgres supplies the rank) | criterion 19 (Q2 before Q1). The unit test cannot catch it, by construction |
| Render the advanced `<details>` with `open` when a filter is active | criteria 10 and 21 |
| Move the three advanced inputs into a separate popup form | criteria 10 and 21 (the value inside the filter form) |
| Drop the summary's `{{if .AdvancedFilters}}` marker | criteria 10 and 21 |
| Build the clear URL from the raw query, or keep `flash` | criterion 8's table |
| Remove `details[open]` from `busy()` | criterion 11 |
| Re-indent a line inside the Dismiss form | `TestTasksTemplate_VerbFormsByteUnchanged` |
| Put the legend back above the sections | criteria 15 and 19, and the amended `TestTasksTemplate_LegendAndRingStyles` |
| Render `{{.UpdatedAt}}` as the cell text | criterion 22 (`+00` present, text ≠ `HH24:MI`) |
| Replace the `updated` expression with a literal `''` | criterion 22 (the fallback shows raw) |
| Make `lightFor` read `QueueRank` | criterion 5's structure test |

## Verification protocol

Run in this order. Do not commit before step 4 passes.

1. **Unit:** `go test ./...`. The SWT-48 `TestAttributionTrend_*` flake
   (20:00–24:00 EDT) is pre-existing; re-run with `TZ=UTC` if it fires.
2. **Integration, on an ISOLATED database.** Never prod, and never the shared
   compose `ops` DB (the IK 2026-09-12 landmine).

   ```
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c "CREATE DATABASE ops_boardlayout"
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_boardlayout?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_boardlayout?sslmode=disable' \
     go test -tags integration -p 1 -count=1 ./internal/dashboard/
   ```

   Run it TWICE, to prove it is rerunnable. Then run the full
   `go test -tags integration -p 1 ./...` against the same URL once.
3. **Mutations:** run each row of the table above and watch it go red.
4. **Local smoke at tablet width.**
   - Start `DATABASE_URL=<ops_boardlayout url> go run ./cmd/dashboard` (:8085,
     `/dev/login?user=salvo`).
   - Seed a throwaway project with about 20 `ready` human tasks, two blocked (one
     via `opsctl call --tool task_signal --args
     '{"task_id":N,"state":"needs_input","session":"shell"}'`, one status
     `blocked`), and two in flight (one `working` signal, one claude
     `in_progress`).
   - In Chrome DevTools' device toolbar at 1000×700, open
     `/tasks?project=<slug>&refresh=on` and check:
     - The first line holds the project select, `Advanced filter` and the
       auto-refresh indicator/toggle, with no second filter row.
     - `BLOCKED (2)` and `IN FLIGHT (2)` are fully visible without scrolling.
       The red row is above the grey one, and `QUEUE` starts below them with
       the blue head first.
     - Every row is one line. `updated` reads `HH:MM` (or a date), and each row
       shows its status.
     - The legend and note are at the very bottom.
     - Open `Advanced filter`: the panel overlays the board, and the page does
       not reload for 15 s. Type `human` into assignee_type, then Filter. The
       summary reads `Advanced filter: assignee_type=human` in the marked
       style, and `clear advanced` shows. Change the project: the filter
       survives. Tap `clear advanced`: it is gone, and `refresh=on` survives.
     - Open one row's `actions`: no reload for 15 s. Tap Done: the flash shows
       once, and `refresh=on` and the filters survive. Close an opened
       `actions` without acting: reloads resume within about 5 s.
     - `?status=ready` still shows BLOCKED / IN FLIGHT / QUEUE.
     - View source: exactly one `<script`, and no `<details … open`.
   - Drop the DB afterwards (`DROP DATABASE ops_boardlayout`).
5. **Deploy: image only, no migration.**
   - Build and push `192.168.50.20:5000/switchboard:<tag>` here.
   - Hand the tag bump to the kube session in
     `docs/runbooks/HANDOFF-kube-board-layout-compact.md`. That session owns
     `kube/switchboard/dashboard.yaml`; this session never edits manifests.
   - Only `deployment/dashboard` needs the new tag. Nothing else changed, so the
     other workloads may take it whenever convenient. There is no ordering
     constraint, no env var, no port and no manifest change beyond the tag.
   - **Post-roll smoke:** `kubectl -n ops port-forward svc/dashboard 8085:80`,
     then open `/tasks?refresh=on` on the tablet:
     - the sections render;
     - `pg_stat_activity` shows no new statement shape;
     - the tracer-equivalent check is by eye: one render's statements are
       unchanged.
6. **Rollback:** roll `deployment/dashboard` back to the previous tag. Nothing
   is stored and no schema changed, so rollback is instant and lossless. Any
   bookmark stays valid, because the URL keys are unchanged.

## Future work (not this ticket)

- **Show the light's label on tap** (touch screens have no hover), e.g. a
  per-row `<details>` on the light, once L6's details rule is proven in use.
- **Drop or merge the rarely-filled `sub`/`order`/`parent` columns** into the
  title cell, and add a phone breakpoint with `<meta name="viewport">`.
- **A board Stop verb** to clear a stale ring without closing it (SWT-52 Future
  work). It would sit in the same `actions` popup.
- **Per-section collapse,** or an anchor bar (`#section-queue`), if the queue
  grows past a screen.
- **Push refresh** instead of polling (SWT-52 Future work).
