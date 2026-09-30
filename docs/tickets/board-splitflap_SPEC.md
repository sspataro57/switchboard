> Jira: SWT-102

# board-splitflap: an optional split-flap animation for the /tasks board, like the Pebble face

## Source

Ad-hoc, swb task 973. Salvador, 2026-09-29: "board animation like the pebble face (make this optional
animation style)". Follow-up from the coordinator: "a small toggle at the top of the board, next to the
advanced filter marker", which puts it on the first line (`.headbar` / `.topbar`), not in the sign header
beside auto-refresh.

The Pebble session sent the motion parameters. The authoritative source is
`~/projects/personal/pebble`:

- `src/c/reel.c` / `reel.h` hold the pure cell rules.
- `src/c/face.c` holds the timing constants, `prv_line_start`, `prv_turn`, `prv_draw_wheel`, `prv_shade`,
  `prv_schedule` and `face_set`'s line order.
- `src/c/gfx.c` holds `gfx_fold_down` / `gfx_fold_up`.
- `tests/host/test_reel.c` holds the worked examples.

Parameters as sent, all confirmed against the source:

- Big digits turn at 150 ms a flip (`DIGIT_FLIP_MS`) and text cells at 90 ms (`TEXT_FLIP_MS`). A frame is
  33 ms (`FRAME_MS`). A line waits 120 ms after the line above settles (`LINE_GAP_MS`).
- A cell moves only when its character changes. The changing cells in a line start together and stop one
  by one.
- The drums are `0-9`, `A-Z` and `a-z`, and a cell turns forward only. Any other character is swapped in
  at once.
- A changing cell turns at least 6 flips (`REEL_MIN_FLIPS`). A drum of 10 or fewer flaps adds a whole turn
  when the target is under 6 flips away. A letter under 6 away starts 6 flaps before its target. A text
  cell turns at most 16 flips (`REEL_MAX_FLIPS`).
- The big-digit fold has a 2 px seam. The upper flap falls with cos easing and then the lower flap lands.
  The shade steps down at 85 % and 40 % of the flap's height. Small text cells have no fold: they just
  change every 90 ms.
- Lines animate one at a time, top to bottom.
- Pitfall: never time the animation on the wall clock. It runs on a motion clock that advances by the
  frame interval.

## Goal

Add a second, opt-in animation style to the /tasks board, "flaps". In this style the board's text cells
turn character by character through their drums, exactly as `reel.c` defines. The clock digits fold like
the Pebble's big digits. The style is chosen per browser with a small toggle on the first line.
Today's row page-turn ("rows") stays the default.

**Usable alone:** after the image rolls, Salvador taps FLAPS on the wall tablet (inside `/kiosk`) or on
any browser. From then on that browser's board flaps on every page turn, every live SSE swap and every
minute of the clock, and it keeps doing so across reloads and relaunches. Every other browser, and every
browser that never taps it, sees the board exactly as today.

## Decisions (made here, with rationale)

**D1. "Optional animation style" means two styles, chosen per browser.** The styles are `rows` (today:
the `.flip` rotateX on each row, staggered 45 ms, the default) and `flaps` (this ticket). Flaps is
**active** only when all three of these hold:
- the stored preference is exactly `"flaps"`;
- `(prefers-reduced-motion: reduce)` does not match;
- the phone query `(max-width: 760px)` does not match. The phone view is a machine-status list. It hides
  `.time` and `.chip`, rows have auto height and nothing pages, so there is no board to flap.

In every other case the board behaves byte-for-byte as today. The rule is one pure function,
`flapActive(pref, reduced, phone)`, and it is golden-tested (AC 6).

**D2. The choice is remembered in `localStorage`, not in a URL key, and never on the server.**
- The key is `swb.board.anim` and its value is `"flaps"`. Turning flaps off REMOVES the key.
- Every read and write is inside `try { … } catch (e) {}`. Storage can be denied (Safari private mode,
  partitioned iframes, a full quota). If it is, the board falls back to `rows` and the toggle still
  works for the life of the page.
- Why not a URL key: the wall tablet reaches the board through the installed app. Its manifest pins
  `start_url: /tasks?refresh=on` (IK "The installed app (SWT-72)"), and the nav's `Board` link is plain
  `/tasks`. Both drop any key on every launch, and changing the manifest means shipping it under a new
  name.
- Why storage covers the kiosk: `/kiosk` frames `/tasks?refresh=on` on the same origin, so the framed
  board shares storage with the unframed board. One tap in either place sets it for that device.
- `boardKeys` stays five keys. B19's "no URL key" half stands. B19's "no `localStorage`" half is
  **deliberately amended** for this one key (see "Test amendments").

**D3. The toggle is a small button at the END of the `.topbar`.** It follows `</form>` and the
conditional `clear advanced` link, so with no advanced filter active it sits right beside the
"Advanced filter" marker.

```html
<button type="button" id="flap-toggle" class="flap-toggle" aria-pressed="false" title="split-flap animation (this browser only)" hidden>flaps</button>
```

- The words are server-rendered and constant. The script only removes `hidden` (when flaps could be
  active: not on the phone, not under reduced motion) and sets `aria-pressed`.
- It binds with `addEventListener`. The file keeps exactly one inline handler, the project select's
  `onchange`.
- It sits OUTSIDE the filter form, so the form's "controls outside the `<details>`" contract is
  unchanged.
- It sits outside every `data-live` region, so a swap never replaces it.
- CSS: the headbar's own `.85rem` uppercase Barlow, a 1px `#3a3a40` border, grey text, and amber text and
  border when `[aria-pressed="true"]`. It must be about 3.5rem wide so the headbar still fits on one line
  at about 1000 px (IK "the filter I want it next to JSON link").

What a tap does:
- **On:** save, add `body.flaps`, then rattle every visible line in from blank. This is the Pebble's
  first build: every cell starts on its drum's first flap. The clock digits spin from their first flap.
- **Off:** remove the key, settle every running cell to its final text at once, remove the fold layers
  and `body.flaps`. Nothing else happens until the next page turn, which uses `rows`.
- **Page load with flaps active:** load behaves like tapping on. The initial `layout(true)` rattles the
  board in instead of adding `.flip`.

**D4. Which text becomes split-flap cells.** A cell is ONE existing Text node. The script rewrites only
its `nodeValue`. It never touches sibling elements, and it never uses `textContent` on an element that
has element children.

| Line | Cells (Text node) | Flip | Group, line index |
|------|-------------------|------|-------------------|
| Header tally | the text of each of the four `.tally b` | 90 ms | `tally`, 0 |
| Wall clock | the four `#clock span` digits, as drums, with the fold | 150 ms | `clock`, 0 |
| Panel heading | the `h2[id^=section-]` text (`TITLE (N)`). Unchanged characters stay still, so in practice only the count moves | 90 ms | panel key, 0 |
| Row (one line per visible slot) | `.time` text; `.id` FIRST Text node (the `▲` `.prio` child untouched); `.title` FIRST Text node (the `.muted` suffixes untouched); `.chip` text; `.rem` LAST Text node (after the light span); `.el` text | 90 ms | panel key, 1 + slot |
| Footer counts | the `[data-live="counts"]` text | 90 ms | `counts`, 0 |

Not animated: the session tag (a self-reported name of up to 200 runes in a coloured pill), the `.pg`
page label, the flash, the orchestrator alert, the indicator's time, and the nav and filters. A cell
whose Text node is missing (for example an empty title renders no leading text) is skipped.

**Cap.** At most `FLAP_CELLS = 32` characters of a cell turn. Characters from position 32 on show their
target at once, from frame 0. This differs from the Pebble, which truncates at `REEL_LEN - 1 = 23`: the
board keeps the full text and only leaves the tail unanimated. The cap bounds per-frame string work on
long titles. The left pane's title column shows about 30 to 50 monospace characters, so the visible part
of most titles turns.

**Characters are code points** (`Array.from(s)`), never UTF-16 units. An emoji or any astral character
is one non-drum cell and is swapped in whole, so a frame never shows half a surrogate pair. This is the
board's version of `test_other_characters`' degree-sign rule.

**The clock's drums are `drum_aim`'s**, places 0 to 3 across `HHMM`. The hour tens carry blank, 0, 1, 2.
The minute tens carry 0 to 5. The other places carry 0 to 9. The board renders 24-hour zero-padded time,
so blank never rests on it, but it is passed on the 23:59 to 00:00 wrap exactly as on the watch. Blank
renders as U+00A0 (no-break space), never `""`: an empty flex item would lose height and shift the header.

**D5. Diffing is by SLOT, not by row identity, as on a real departures board.** A slot is
(panel key, visible index on the current page). What a slot shows can change in three ways:
- **Page turn** (`show(p, true)`): before the rows' `hidden` changes, read each visible slot's cell
  strings. After it changes, aim the newly visible row in slot i from old slot i's strings.
- **SSE swap** (`swap()`): before `replaceWith`, read every panel's heading string and each visible
  slot's cell strings, keyed by panel key (the same `h2` id `collect()` keys pages by). Also read the four
  tally strings by index and the counts string. After `collect()` and `layout(false)` restore each panel's
  page, aim each new visible slot from the old one.
- **Minute tick** (clock and `.el`): the clock re-aims its drums. `.el` changes only through a swap
  (Go renders it), so it follows the swap path.

**The shown string is the Text node's current `nodeValue`**, so an interrupted animation re-aims from
exactly what is on screen (the Pebble's `prv_turn` reads `prv_shown`). A re-aim whose target equals the
slot's running target KEEPS the running plan and its start time; it is rebound to the new node. This is
`prv_turn`'s early return. Without it, the `request()` that `setLive(true)` fires on stream open would
restart the load-time rattle a second later.

What happens when rows or panels come and go:
- **A slot with no old row** (a row appears, a panel grew, the last page is shorter, a new panel) aims
  from `""`: every cell comes up from its drum's first flap (`reel_aim` pads with spaces).
- **An old slot with no new row** (a row left, a panel shrank) is simply gone. Today the row element is
  removed or hidden; nothing flaps out.
- A row that moved one slot down flaps where it now sits. This is authentic, and per-cell diffing keeps
  unchanged characters still.

Running state is keyed by slot and cell class, never by node. The frame loop holds node references that
are rebound after every swap. A node no longer in the document is dropped.

**D6. Lines within a group run top to bottom; groups run concurrently.** The Pebble has four lines. The
board has up to 6 panels × 15 rows. Strictly one line at a time board-wide would take longer than the
9 s page interval (`PageSeconds`), so page 2 would turn before page 1 settled.
- Each group (`tally`, `clock`, `counts`, one per panel) sequences its own lines with the Pebble rule. A
  line starts once every line above it that is still moving has come to rest, plus `LINE_GAP_MS`. A line
  at rest holds nobody up.
- There is one addition: a per-group **step cap**. A line never starts later than
  `base + k × step`, where `step = flapLineStep(n, pageMs)`, with `n` the group's line count and
  `pageMs` from `data-page-interval`. It is computed as follows:
  - `step = floor((pageMs − 1500 − 16 × 90) / (n − 1))` for `n > 1`;
  - no cap (`Infinity`) for `n ≤ 1`.

  So a 5-line panel keeps the Pebble's pure sequence (a step of 1515 ms is never binding), while a
  15-line grow panel is capped at 432 ms and every line of a page has settled by `pageMs − 1500`.
  Starts stay monotonic top to bottom.

**D7. Timing runs on a motion clock, never the wall clock.**
- `motionMs` starts at 0 and advances by exactly `FRAME_MS` (33) per frame.
- Frames are a self-arming `setTimeout(frame, FRAME_MS)` that runs ONLY while something is turning. This
  is `prv_schedule`: an idle board has no animation timer.
- Aims, line starts, flips done (`floor((motionMs − start) / flipMs)`) and fold phase all read `motionMs`.
- `Date.now`, `performance.now` and `new Date` never feed motion. The one `new Date()` stays the
  decorative clock's time source (B14).
- If the tab is throttled, the animation slows rather than skipping to the end.
- If a frame fires while `document.hidden`, every cell settles at once and the loop stops.
- A clock update while hidden sets the digits directly.

**D8. The fold (clock digits only), from `prv_draw_wheel` / `gfx_fold_*` / `prv_shade`.** Take one flip
in progress, with `part = ((motionMs − start) mod 150) × 1000 / 150`, and `H` the half height:
- **`part < 500`:**
  - The top half shows the NEXT digit.
  - A flap showing the OLD digit's top half is anchored at the seam (`transform-origin: bottom`) with
    `scaleY(cos(π/2 × part / 500))`.
  - The bottom half shows the OLD digit.
- **`part ≥ 500`:**
  - The top half shows NEXT.
  - A flap showing NEXT's bottom half is anchored at the seam (`transform-origin: top`) with
    `scaleY(cos(π/2 × (1000 − part) / 500))`.
  - The bottom half shows OLD.
- **Shade** by `h / H` (the scale): `≥ 0.85` gives level 0, `≥ 0.40` gives level 1, below that level 2.
  The flap gets `filter: brightness(1 | .7 | .45)`.
- **Seam** in flaps mode is exactly 2 px: `body.flaps .clock span` uses a gradient stop at
  `calc(50% − 1px)` to `calc(50% + 1px)`.
- **DOM:** while a digit turns, its span (`position: relative; overflow: hidden` under `body.flaps`)
  holds three absolutely positioned children: top half, bottom half and flap. They are built with
  `createElement` + `textContent`, and each is a `50% − 1px` box with overflow hidden holding a full-size
  copy of the digit. They are removed when the digit settles, so a span at rest holds only its Text node,
  as today.
- **No layout shift:** only transforms and filters animate.

**D9. No layout shift, bounded CPU.**
- `reel_at` always returns the TARGET's length. From frame 0 a cell has its final character count.
  Cells beyond the target vanish at once, and new cells start on a drum flap.
- Row cells are monospace (B612 Mono) in fixed grid columns with `overflow: hidden`.
- Under `body.flaps`, the proportional-font lines (`h2`, the counts span) get
  `font-variant-numeric: tabular-nums`. The digits are the only characters that usually move there.
- The frame loop does no layout reads (no `offsetHeight` or `getBoundingClientRect`), only `nodeValue`
  writes and transform/filter writes. Row height never changes, because `white-space: nowrap` and
  `height: var(--row)` are unchanged, so paging's `offsetHeight` measurement is unaffected.
- A `nodeValue` is written only when the shown string differs from the last one written: a text cell
  changes every 90 ms, but frames come every 33 ms.
- Only visible rows animate. A hidden row is given its final text directly.
- Plans are computed once per aim (per cell: flips and start flap). A frame is `O(turning cells)`, with no
  allocation beyond the output strings.

**D10. The pure rules are a delimited block inside the one script, tested by node from Go.** Constraints:
- The script stays ONE inline `<script>`, as tests pin. There is no `/static/*.js`, no library and no
  build step.
- The pure code sits at the top of the IIFE between the exact lines `// flap: pure begin` and
  `// flap: pure end`. It holds only `var` constants and `function` declarations, and it never names
  `document`, `window`, `navigator`, `localStorage`, `Date`, `performance`, `setTimeout` or `this`.
- `internal/dashboard/board_flap_test.go` extracts the block from the embedded `tasks.html`. It writes the
  block and `testdata/flap/harness.js` into `t.TempDir()`, runs `node` on it with
  `testdata/flap/golden.json`, and compares the results in Go.
- If `exec.LookPath("node")` fails, the test calls `t.Skip` with a loud message, unless
  `SWB_REQUIRE_NODE=1`, in which case it calls `t.Fatal`. Node is `/usr/bin/node` on the workstation.
- The goldens are `test_reel.c`'s cases transcribed verbatim, plus the board's own additions (below).
  There is no Go port of the rules: one implementation, one oracle.

The pure API that the goldens exercise (the names are binding for test-author):

```js
var FLAP_TEXT_MS = 90, FLAP_DIGIT_MS = 150, FLAP_FRAME_MS = 33, FLAP_GAP_MS = 120,
    FLAP_MIN = 6, FLAP_MAX = 16, FLAP_CELLS = 32;
function flapActive(pref, reduced, phone)          // bool, D1
function flapAim(shown, target)                     // plan: per code point {to, flips, start, drum}; reel_aim + FLAP_CELLS
function flapFlips(plan)                            // reel_flips
function flapAt(plan, flips)                        // reel_at: string, target length, tail past FLAP_CELLS = target
function drumAim(place, shown, target, fullTurn)    // drum_aim; blank is -1
function drumAt(drum, flips)                        // drum_at
function flapShade(h, full)                         // prv_shade: 0 | 1 | 2
function flapFold(part)                             // {flap: "upper"|"lower", scale} for part in [0,1000)
function flapLineStep(n, pageMs)                    // D6
function flapLineStarts(base, ends, durs, gap, step)// D6; ends[k] = running end or 0, durs[k] = new duration or 0
```

## Acceptance criteria

1. **Default unchanged.** With no `swb.board.anim` in storage, the board behaves as today. The first
   layout, page turns and swaps add `.flip` to rows exactly as now (45 ms stagger on turns, 0 ms on swaps,
   class removed on `animationend`), and no `.flap-*` element or `body.flaps` class exists. Verified in
   the browser and by the unchanged SWT-67/SWT-89 structure tests.
2. **Toggle markup and place.** `tasks.html` carries exactly one
   `<button type="button" id="flap-toggle" class="flap-toggle" aria-pressed="false" title="split-flap animation (this browser only)" hidden>flaps</button>`.
   It sits inside `<div class="topbar">`, after the filter form's `</form>` and after the
   `{{if .AdvancedFilters}}…clear advanced…{{end}}` line. It is outside the form, outside every
   `data-live` region and not in `<header class="sign">`. It is at template depth 0.
3. **No inline handler.** The inline-handler regex still finds only the project select's `onchange`, and
   the toggle binds with `addEventListener("click"`.
4. **Storage discipline.** Every `localStorage` occurrence in the script is inside a `try {` … `}` block
   whose `catch` swallows. The only key literal is `"swb.board.anim"`, appearing once. `sessionStorage`
   and `document.cookie` stay banned. Turning off calls `removeItem`.
5. **Persistence.** In Chrome (Playwright): tap FLAPS, reload, and the board is still flaps and
   `aria-pressed="true"`. Open `/kiosk` in the same profile, and the framed board is flaps. Tap off inside
   the kiosk, reload `/tasks`, and it is rows.
6. **`flapActive` goldens:**
   - `("flaps", false, false)` gives true.
   - `(null, false, false)` gives false.
   - `("", false, false)` gives false.
   - `("rows", false, false)` gives false.
   - `("flaps", true, false)` gives false.
   - `("flaps", false, true)` gives false.

   In the browser, with `reducedMotion: "reduce"` emulated or at a 700 px viewport, the toggle stays
   `hidden` and rows flip as today even with the key set.
7. **Reel goldens (verbatim from `test_reel.c`), through `flapAim`/`flapAt`/`flapFlips`.**
   - One-cell paths:
     - `2→3` = `234567890123`
     - `9→0` = `901234567890`
     - `2→8` = `2345678`
     - `8→2` = `890123456789012`
     - `A→H` = `ABCDEFGH`
     - `x→f` = `xyzabcdef`
     - `A→D` = `XYZABCD`; at flip 200 it is still `D`.
   - `MON→TUE`:
     - flips = 16;
     - cell 0 starts `MNOPQRST` and is `T` at flips 8 and 16;
     - cell 1 starts `OPQRSTU` and is `U` at 7;
     - cell 2 = `OPQRSTUVWXYZABCDE`;
     - at 16 the whole string is `TUE`.
   - `12m→13m`: flips = 11. Cells 0 and 2 never move. Cell 1 is `2` at 0, `7` at 5 and `3` at 11.
   - `TUE→TUE`: flips = 0.
   - Exhaustive, over each drum and every from/to pair (a, b):
     - a = b gives 0 flips;
     - otherwise 6 ≤ flips ≤ 16, the cell lands on b, and each flap is the next on its drum.
   - `A→Z`: flips 16; `J` at 0, `Y` at 15, `Z` at 16.
   - Lengths:
     - `9→10`: length 2 at flip 0, and cell 1 is `0` at flip 0.
     - `pebble→kube`: length 4 throughout.
     - `→SEP`: flips in [6, 16] and uppercase throughout.
     - `pebble→""`: flips 0 and `""`.
   - Signs:
     - `7%→82%`: `%` at flip 0.
     - `a_b→c-d`: `-` from flip 0; `c-d` at 6.
     - `7°→24°`: `°` never moves and the string is valid at every flip.
     - `24°→-3°`: `-` from flip 0.
     - `5→C`: an uppercase letter at flip 0; `C` at 6.
     - `5→A`: flips = 6.
8. **Board-only reel goldens.**
   - A 40-character target from `""`: `flapAt(plan, 0)` has length 40, and characters 32 to 39 equal the
     target's from frame 0.
   - `"a🛫b"→"c🛬d"`: the emoji position is always exactly the target emoji (one code point, never a lone
     surrogate), and cells 0 and 2 flap.
9. **Drum goldens (verbatim `test_drums`)**, including:
   - the four drums' exhaustive property (lands right, forward only, only its own flaps,
     6 ≤ flips ≤ 15);
   - place 0 `-1→1` = 6 flips, via 0;
   - place 0 `1→-1` = 6 flips;
   - full-turn resets: place 1 `4→4` gives 10, place 2 `3→3` gives 6, place 0 `-1→-1` gives 8.
10. **Fold and shade goldens.**
    - `flapShade`: (85, 100) gives 0; (84, 100) gives 1; (40, 100) gives 1; (39, 100) gives 2.
    - `flapFold`:
      - part 0 gives upper, scale 1;
      - part 250 gives upper, ≈0.7071 (±1e-4);
      - part 499 gives upper, below 0.004;
      - part 500 gives lower, scale 0;
      - part 750 gives lower, ≈0.7071;
      - part 999 gives lower, above 0.9999.
11. **Line goldens.**
    - `flapLineStep`:
      - `(1, 9000)` gives Infinity;
      - `(5, 9000)` gives 1515;
      - `(15, 9000)` gives 432.
    - `flapLineStarts`:
      - `(0, [0,0,0,0], [900,540,0,1440], 120, 1515)` gives `[0,1020,1680,1680]`;
      - `(0, [0,0,0,0], [900,900,900,900], 120, 400)` gives `[0,400,800,1200]`;
      - `(1000, [1600,0,0], [0,540,540], 120, 1515)` gives `[1000,1720,2380]`: a line still running from
        an earlier aim holds the ones below.
12. **Purity scan.** The block between `// flap: pure begin` and `// flap: pure end` exists exactly once,
    inside the one `<script>`. It contains none of `document`, `window`, `navigator`, `localStorage`,
    `Date`, `performance`, `setTimeout`, `this` or `{{`.
13. **Motion clock.** The script contains no `performance.now`. `Date.now` stays banned file-wide
    (existing B6 test). Exactly one `new Date(` remains (the decorative clock). The frame function
    advances the motion clock by `FLAP_FRAME_MS`, and the frame timer is armed only when a plan is
    turning. In the browser, with Chrome's timer instrumentation, an idle flaps board fires no timer at
    33 ms.
14. **Only changing characters move; slot diffing.** In the browser with flaps active and refresh on:
    - `UPDATE tasks SET title = …` changing one word of one visible task leaves every other row's Text
      nodes untouched (a `MutationObserver` records `characterData` only on that row's `.title` node, plus
      heading, tally and counts nodes only if their text changed).
    - A page turn aims slot i of page 2 from slot i of page 1.
    - A new task appearing in slot 0 rattles every slot below it that now shows a different row.
15. **Re-aim keeps a same-target plan.** On load with flaps active and refresh on, the stream-open swap
    that arrives mid-rattle does not restart any cell (a cell's flip count never goes backwards in a
    recorded frame trace).
16. **No layout shift.** During a load rattle, a page turn and a swap in flaps mode, a
    `PerformanceObserver({type: "layout-shift", buffered: true})` records a total of 0, and every row's
    `offsetHeight` equals `var(--row)`'s computed value.
17. **Clock fold.** Across a minute change in flaps mode, the changed `#clock span`s gain three
    `.flap-*` children for the flip's duration and have none once settled. The seam is 2 px. At rest,
    `#clock span` holds a single Text node. Blank renders as U+00A0.
18. **Off settles at once.** Tapping off mid-rattle leaves every cell at its target text in the same task,
    removes every `.flap-*` element and `body.flaps`, and the next page turn uses `.flip`.
19. **Headbar fits.** At a 1000 px wide viewport with `project` set and no advanced filter,
    `.headbar`'s height equals its single-line height (the same as with the toggle `hidden`).
20. **Nothing server-side changes.**
    - `boardKeys`, `boardBack`, `boardRefreshURLs`, `boardAdvanced`, `listTasks` and every Go file outside
      `_test.go` are byte-unchanged.
    - `git diff --stat main -- '*.go' ':!*_test.go'` is empty.
    - No migration, no route, no data attribute on the `<script>` tag.
    - `boardVersion` changes (the template hash). That is expected: open boards reload once after the
      roll.

## Data model changes

None. No migration, no table, no column. Nothing is stored server-side; the preference is one browser
`localStorage` key.

## API / MCP tool changes

None. No endpoint, no MCP tool, no executor path. The toggle makes no request. Invariant 3 is untouched
because there is no tool call.

## MQTT topics

None.

## Files likely to touch

- `internal/dashboard/templates/tasks.html`: the toggle button in `.topbar`, and the CSS under
  `body.flaps` (`.flap-toggle`, the 2 px clock seam, fold layers, `tabular-nums`). In the one script:
  - the pure block;
  - pref read/write (`try/catch`);
  - the `flapActive` wiring and the `matchMedia` change listeners;
  - the slot snapshot/aim in `show()` and `swap()`;
  - the frame loop;
  - the clock through `drumAim` when active.

  Today's `.flip` path stays intact for `rows`.
- `internal/dashboard/board_flap_test.go` (new): the structure checks (AC 2, 3, 4, 12, 13, 20) and the
  node golden runner (AC 6 to 11).
- `internal/dashboard/testdata/flap/harness.js`, `internal/dashboard/testdata/flap/golden.json` (new).
  `testdata` is already skipped by the `.js` scans in `demo_structure_test.go` and
  `board_live_structure_test.go`.
- Test amendments (below): `board_refresh_test.go`, `board_layout_structure_test.go`,
  `board_live_structure_test.go`.
- `.claude/INSTITUTIONAL_KNOWLEDGE.md`: a short "Board split-flap (board-splitflap)" section. It covers
  the storage key and why not a URL key; the pure block and the node test with `SWB_REQUIRE_NODE`; slot
  diffing; the motion clock; and B19's amendment. Also add a one-line pointer under "Board departures
  view".
- `docs/runbooks/HANDOFF-kube-board-splitflap.md`: image tag only, no migration (per the "kube manifests
  belong to the kube session" rule).

## Test amendments (deliberate, each with an AMENDED comment naming this ticket)

1. **`TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript`** (`board_refresh_test.go`),
   **`TestTasksTemplate_ScriptContract`** (`board_layout_structure_test.go`) and
   **`TestTasksTemplate_LiveScriptContract`** (`board_live_structure_test.go`).
   - `"localStorage"` leaves each banned list.
   - Each points at AC 4's check, which is written once in `board_flap_test.go`.
   - `sessionStorage` stays banned, and each list gains `"document.cookie"`.
   - The error texts' "stores nothing (B19)" becomes "stores only the flap preference (board-splitflap D2)".
2. **`TestTasksTemplate_HeaderBandsInOrder`** (`board_layout_structure_test.go`, the rewrite of
   `FirstLineIsTheTopbar`).
   - None of its current assertions fail with the toggle present. Its banned list does not name it, and
     the form-controls check reads only inside `<form>`.
   - Its contract comment says "the topbar holds ONLY the filter form and the clear link", and that
     changes. Amend the comment to three items and add the positive assertion: `id="flap-toggle"` is
     inside the topbar, after `</form>` and after `id="advanced-clear"`, and not in the sign header.
3. **`TestTasksTemplate_FiveLiveRegions`** (`board_live_structure_test.go`): add `id="flap-toggle"` to
   the list of things no live region may contain. This strengthens the test; nothing fails today.

Not amended, and they must stay green:
- `TestTasksTemplate_ByteUnchangedFragmentsSurviveTheRestyle` (every pinned fragment is untouched);
- `TestBoardRefresh_IntervalAndKeys` and criterion 14's `boardKeys` check (five keys);
- every `<script` count, the one `fetch(`, the one `new EventSource(`, the `arm()` guard, and `busy()`'s
  four clauses;
- the `Date.now` / `new Date(t` ban;
- `TestTasksTemplate_NoThirdPartyURL`;
- the kiosk tests (`kiosk.html` is not touched, and its own `localstorage` ban stands).

The headbar's 1000 px fit is not testable in Go. It is AC 19, checked in the browser.

## In scope / Out of scope

**In scope:** the flaps style and toggle as above, on `/tasks` and therefore inside `/kiosk`.

**Out of scope:**
- Any server-side preference, cookie or URL key. The manifest is untouched.
- Flapping the session tag, the `.pg` page label, the flash, the alert or the ticker legend.
- Making today's `rows` flip honour `prefers-reduced-motion` (a separate small ticket; see Future work).
- Sound.
- The phone layout.
- Other pages (`swb-1.css` pages).
- Changing `PageSeconds`, paging or streaming cadence (SWT-89), or `/watch.json` (SWT-101).
- Anything in the pebble repo.
- Build-order steps 8 to 10 (board-adjacent dashboard work such as the plan-import board and briefs).

## Invariants that apply

1. **Raw-first:** n/a. Nothing is captured.
2. **One funnel:** n/a. No task data is created or read beyond what the board already renders. The
   script only moves server-rendered text.
3. **Everything through the executor:** unaffected. The toggle makes no request and no tool call. Row
   verbs are byte-unchanged.
4. **Nothing external without a delivery row:** n/a. The page still makes no third-party request
   (`TestTasksTemplate_NoThirdPartyURL`).
5. **Own-message loop closure:** n/a.
6. **Stealth attribution:** the toggle's words and title are plain ("flaps", "split-flap animation (this
   browser only)"). No attribution text anywhere, and commits carry no AI trailer.
7. **Orchestrator is pure:** n/a. In spirit, the animation rules are pure and golden-tested (D10), and
   the DOM layer only applies them.

The board-specific contracts that stay binding:
- SWT-89 S6: no string-to-DOM sink. Fold layers use `createElement` + `textContent`, and cells use
  `nodeValue`.
- The IK landmines:
  - measure rows with `offsetHeight`;
  - remove `.flip` on `animationend`;
  - no element with a running animation or transform may contain a row's `position: fixed` popup. The fold
    transforms live only on children of `#clock span`, and row cells never get a transform.
- B14: one decorative browser clock.

## Sibling patterns to copy

- `~/projects/personal/pebble/src/c/reel.c`: port `prv_drum_of`, `prv_cell_flips`, `reel_aim`,
  `reel_flips`, `reel_at`, `prv_flaps`, `prv_index`, `drum_aim` and `drum_at` line for line.
  `tests/host/test_reel.c` becomes `golden.json`.
- `~/projects/personal/pebble/src/c/face.c`:
  - `prv_line_start` (+ D6's cap) becomes `flapLineStarts`.
  - `prv_turn`'s same-target early return and its re-aim from `prv_shown` become D5.
  - `prv_schedule` / `prv_frame` (frames only while turning, the clock advanced by the frame) become D7.
  - `prv_draw_wheel` + `prv_shade` become D8.
- `internal/dashboard/templates/tasks.html` itself: `swap()`'s before/after snapshot keyed by the `h2` id
  and `show()`'s per-panel loop are the two hook points. `rowKey`/`rowSig` show the existing snapshot
  idiom.
- `internal/orchestrator/deps_test.go` shows the `exec.LookPath` + external-binary pattern in a Go test.

## Verification protocol

1. `go test ./internal/dashboard/...` and then `go test ./...`: green. Confirm that `board_flap_test.go`
   ran and did not skip:
   `SWB_REQUIRE_NODE=1 go test -run TestBoardFlap -v ./internal/dashboard/` must show PASS, never SKIP.
   Gate the commit on the exit status (memory "gate commits on test exit status").
2. `make test-integration` (the `integration` tag): green, unchanged. No integration test is added,
   because nothing server-side changes.
3. Mutation spot-checks (each must turn a test red, then revert):
   - drop the `size <= 10` guard (AC 7 `A→D`);
   - raise `FLAP_MAX` to 26 (AC 7 `MON→TUE`);
   - iterate with `split("")` instead of `Array.from` (AC 8 emoji);
   - start a cross-drum cell at the old index (AC 7 `5→C`, `9→10`);
   - swap the shade thresholds (AC 10);
   - make `flapFold` linear (AC 10);
   - drop the step cap (AC 11);
   - make `flapActive` true on a null pref (AC 6);
   - remove one `try` (AC 4);
   - give the toggle `onclick=` (AC 3);
   - move the toggle into `<header class="sign">` (amended HeaderBandsInOrder);
   - reference `document` inside the pure block (AC 12);
   - add `performance.now()` (AC 13).
4. Real browser (Playwright, `channel="chrome"`, IK "Verify UI work in a real browser"), against a local
   dashboard with seeded tasks and `refresh=on`: walk AC 1, 5, 6, 14 to 19.
   - Drive a swap with `psql` (`UPDATE tasks SET title = … WHERE id = …`) and watch it with a
     `MutationObserver`.
   - Record a 2 s frame trace during a load rattle to check AC 15.
   - Do not use `--virtual-time-budget` screenshots; they hid both earlier layout landmines.
5. On the wall tablet after the roll: open `/kiosk`, tap FLAPS, and watch one page turn and one live
   change. Relaunch the installed app and confirm it is still flaps.

No questions arose that CLAUDE.md, the IK, the code or Salvador's toggle-placement answer do not settle;
there is no OPEN_QUESTIONS file.

## Implementation notes (deviations recorded at delivery)

- **D9, counts line.** Under `body.flaps` the footer counts span (`footer.ticker > span`) is set in `var(--mono)`,
  not only `tabular-nums`. Its letters rattle too, and in proportional type every flip changed its width and
  pushed `#light-legend` sideways (measured CLS 0.0009; 0 after the change). The selector cannot be
  `[data-live="counts"]`: `TestTasksTemplate_FiveLiveRegions` takes the first `data-live="counts"` in the file
  as the region, and a CSS selector ahead of the markup would be read as that region.
- **D8, fold leaves.** The three `b.flap-*` leaves are full-size copies clipped with `clip-path: inset(...)` at
  `50% ± 1px`, not `50% − 1px` overflow boxes. Same picture, simpler CSS; the leaf's `transform-origin` is the
  seam. While a digit turns, the span's Text node already holds the new digit, so the 2 px seam strip shows a
  sliver of it.
- **D5/D9, waiting lines.** A cell whose line has not started yet shows `flapAt(plan, 0)` (its target length,
  first flaps) rather than its old text, so from frame 0 it has its final character count.
- **Idle timer.** `flapArm()` arms a frame only when some run exists; the 5 s clock tick with no digit change
  arms nothing.

### Retune after first live use (swb 986, 2026-09-29, Salvador)

Supersedes D4's heading row, D5/D6's per-panel groups, the D9 "waiting lines" note above and D10's timing
constants:
- **Line by line, not garbage first.** A line keeps its old text (or, coming up from blank, its own text)
  until its turn; only then does it rattle. Nothing on the board scrambles ahead of its line.
- **Rows aligned across panels.** The tally and every panel's rows share one sequence ("board"): row k of
  in flight, departures, holding… starts together. Page turns aim every turning panel in one pass in
  `turn()`; `show()` no longer aims.
- **About 4x faster.** `FLAP_TEXT_MS` 90 → 22, `FLAP_GAP_MS` 120 → 30, `FLAP_FRAME_MS` 33 → 16 (so each flip
  still gets a frame). `flapLineStep` subtracts `FLAP_MAX × FLAP_TEXT_MS`. Measured: ~340 ms per line.
  The clock keeps 150 ms per digit flip.
- **Panel headings never flap.** They are fixed, as on a real board; their count just updates.
- **swb 989: twice as fast again.** `FLAP_TEXT_MS` 22 → 11, `FLAP_GAP_MS` 30 → 15; the frame stays 16 ms, so
  a frame may advance two flips (drum letters skip, which at this speed does not read). ~170 ms a line.

## Future work

- Honour `prefers-reduced-motion` in the default `rows` flip too (today it always animates).
- Flap the `.pg` page label and the session tag.
- Share `golden.json` with the pebble repo so a `reel.c` change is caught on both sides.
- A "reset" full-board rattle when the orchestrator alert appears (the Pebble's colour-change reset).
- Refuse the plain-http host for `/watch.json` (SWT-101 future work; unrelated, noted only because it was
  read here).
