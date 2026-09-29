package dashboard

// board-splitflap (SWT-102, docs/tickets/board-splitflap_SPEC.md): the opt-in
// "flaps" animation style for the /tasks board. ZERO network. The structure
// checks read only the embedded tasks.html; the golden runner shells out to a
// local `node` on a temp dir (D10) and nothing else.
//
//	AC 2   TestBoardFlap_ToggleMarkupAndPlace
//	AC 3   TestBoardFlap_ToggleBindsWithoutAnInlineHandler
//	AC 4   TestBoardFlap_StorageDiscipline   (the three amended script tests point here)
//	AC 6   TestBoardFlap_ActiveGoldens            ─┐
//	AC 7   TestBoardFlap_ReelGoldens               │ node: the pure block between
//	AC 8   TestBoardFlap_BoardReelGoldens          │ "// flap: pure begin" and
//	AC 9   TestBoardFlap_DrumGoldens               │ "// flap: pure end", evaluated by
//	AC 10  TestBoardFlap_FoldAndShadeGoldens       │ testdata/flap/harness.js against
//	AC 11  TestBoardFlap_LineGoldens               │ testdata/flap/golden.json
//	D10    TestBoardFlap_Constants                ─┘
//	AC 12  TestBoardFlap_PureBlockIsPure
//	AC 13  TestBoardFlap_MotionClock
//	AC 20  TestBoardFlap_ScriptTagUnchanged (the "no data attribute on the <script>" half)
//
// NODE. The golden tests need node (/usr/bin/node on the workstation). Without it
// they t.Skip LOUDLY — unless SWB_REQUIRE_NODE=1, which turns the skip into a
// t.Fatal. The verification protocol runs
//
//	SWB_REQUIRE_NODE=1 go test -run TestBoardFlap -v ./internal/dashboard/
//
// and it must show PASS, never SKIP. There is no Go port of the rules: the one
// JS implementation is checked against one oracle, test_reel.c's cases.
//
// BINDING SURFACE (SPEC D10; names are the SPEC's): the pure block declares
// FLAP_TEXT_MS, FLAP_DIGIT_MS, FLAP_FRAME_MS, FLAP_GAP_MS, FLAP_MIN, FLAP_MAX,
// FLAP_CELLS and flapActive, flapAim, flapFlips, flapAt, drumAim, drumAt,
// flapShade, flapFold, flapLineStep, flapLineStarts. ONE READING THIS FILE MAKES
// that the SPEC leaves implicit: drumAim returns the drum as an object whose
// integer `.flips` is the flip count (reel.h's Drum.flips), because nothing else
// in the named API exposes it. AC 13 also reads D7 literally: the motion clock is
// `motionMs += FLAP_FRAME_MS` and the frame timer is `setTimeout(frame,
// FLAP_FRAME_MS)`.
//
// GREENFIELD NOTE — EXPECTED RED before the implementation: the toggle, the
// storage key and the pure block do not exist, so every test here fails on a
// missing marker, missing markup or a missing localStorage use. Nothing fails to
// compile.
//
// NOT ENCODED HERE (browser or delivery checks, SPEC verification 4 and 1):
// AC 1 (default unchanged: the existing SWT-67/SWT-89 structure tests are its
// guard), 5, 14-19 (Playwright), 13's "an idle board fires no 33 ms timer", and
// 20's `git diff --stat main -- '*.go' ':!*_test.go'` — a permanent test of that
// would fail every later ticket that touches Go, so it stays a delivery check.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC verification 3):
//   - drop the `size <= 10` guard                        -> ReelGoldens (A->D), DrumGoldens is unaffected
//   - raise FLAP_MAX to 26                               -> ReelGoldens (MON->TUE), Constants
//   - iterate with split("") instead of Array.from       -> BoardReelGoldens (cells after an emoji)
//   - start a cross-drum cell at the old index           -> ReelGoldens (5->C, 9->10)
//   - swap the shade thresholds / make flapFold linear   -> FoldAndShadeGoldens
//   - drop the step cap                                  -> LineGoldens
//   - flapActive true on a null pref                     -> ActiveGoldens
//   - remove one try                                     -> StorageDiscipline
//   - give the toggle onclick=                           -> ToggleBindsWithoutAnInlineHandler (and the amended script tests)
//   - move the toggle into <header class="sign">         -> ToggleMarkupAndPlace, the amended HeaderBandsInOrder
//   - reference document inside the pure block           -> PureBlockIsPure (and the goldens: the vm context has no document)
//   - add performance.now()                              -> MotionClock

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const (
	flapToggle     = `<button type="button" id="flap-toggle" class="flap-toggle" aria-pressed="false" title="split-flap animation (this browser only)" hidden>flaps</button>`
	flapPureBegin  = "// flap: pure begin"
	flapPureEnd    = "// flap: pure end"
	flapStorageKey = "swb.board.anim"
)

// ---- AC 2: the toggle's markup and place -------------------------------------------------------

func TestBoardFlap_ToggleMarkupAndPlace(t *testing.T) {
	s := tasksHTML(t)
	if n := strings.Count(s, flapToggle); n != 1 {
		t.Fatalf("tasks.html carries the toggle %d times, want exactly once:\n  %s\n(AC 2: the words are server-rendered and "+
			"constant; the script only removes hidden and sets aria-pressed)", n, flapToggle)
	}
	if n := strings.Count(s, `id="flap-toggle"`); n != 1 {
		t.Errorf("tasks.html has %d id=\"flap-toggle\", want exactly 1 (AC 2)", n)
	}
	bi := strings.Index(s, flapToggle)

	const topOpen = `<div class="topbar">`
	ts := strings.Index(s, topOpen)
	te, ok := elementEnd(s, ts, "div")
	if ts < 0 || !ok {
		t.Fatalf("tasks.html has no closed %s", topOpen)
	}
	if !(ts < bi && bi < te) {
		t.Errorf("the toggle is not inside %s (AC 2 / D3: a small toggle on the first line, next to the advanced "+
			"filter marker)", topOpen)
	}
	fi := strings.Index(s, `<form class="filters"`)
	fe := fi + strings.Index(s[fi:], "</form>")
	if fi < 0 || fe < fi {
		t.Fatalf("tasks.html lost its filter form")
	}
	if fi <= bi && bi < fe+len("</form>") {
		t.Errorf("the toggle sits inside <form class=\"filters\">; it follows </form> so the form's \"controls outside " +
			"the <details>\" contract is unchanged (AC 2 / D3)")
	}
	if bi < fe {
		t.Errorf("the toggle comes before the filter form's </form>; it is at the END of the topbar (AC 2 / D3)")
	}
	const clearLine = `{{if .AdvancedFilters}}<a id="advanced-clear" href="{{.ClearAdvancedURL}}">clear advanced</a>{{end}}`
	ci := strings.Index(s, clearLine)
	if ci < 0 {
		t.Errorf("tasks.html lost the byte-unchanged clear-advanced line %s", clearLine)
	} else if bi < ci+len(clearLine) {
		t.Errorf("the toggle comes before the {{if .AdvancedFilters}}…clear advanced…{{end}} line; it follows it " +
			"(AC 2 / D3)")
	}
	if d := templateDepthAt(s, bi); d != 0 {
		t.Errorf("the toggle sits %d template block(s) deep, want 0 (AC 2: it renders on every board page; the "+
			"script decides whether to unhide it)", d)
	}
	if sg := strings.Index(s, `<header class="sign">`); sg >= 0 {
		if sge, ok := elementEnd(s, sg, "header"); ok && sg <= bi && bi < sge {
			t.Errorf("the toggle sits in <header class=\"sign\">; it belongs on the first line, the .topbar (AC 2)")
		}
	}
	for _, n := range liveNames {
		if _, _, start, end, ok := liveRegion(s, n); ok && start <= bi && bi < end {
			t.Errorf("the toggle sits inside the %s live region; a swap would replace it (AC 2 / D3)", n)
		}
	}
	if _, script := liveScript(t, s); strings.Contains(script, flapToggle) {
		t.Errorf("the toggle's markup appears inside the <script>; it is server-rendered markup (AC 2)")
	}
}

// ---- AC 3: no inline handler; the toggle binds with addEventListener ---------------------------

func TestBoardFlap_ToggleBindsWithoutAnInlineHandler(t *testing.T) {
	s := tasksHTML(t)
	handlers := regexp.MustCompile(`\son[a-z]+=`).FindAllString(s, -1)
	if len(handlers) != 1 || strings.TrimSpace(handlers[0]) != "onchange=" {
		t.Errorf("tasks.html has inline event-handler attributes %v, want only the project select's one onchange (AC 3)",
			handlers)
	}
	_, script := liveScript(t, s)
	direct := regexp.MustCompile(`getElementById\(\s*["']flap-toggle["']\s*\)\s*\.addEventListener\(\s*["']click["']`)
	if direct.MatchString(script) {
		return
	}
	m := regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*=\s*document\.getElementById\(\s*["']flap-toggle["']\s*\)`).
		FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("the script never looks up the toggle with document.getElementById(\"flap-toggle\") (AC 3)")
	}
	bind := regexp.MustCompile(regexp.QuoteMeta(m[1]) + `\.addEventListener\(\s*["']click["']`)
	if !bind.MatchString(script) {
		t.Errorf("the toggle (%s) is never bound with %s.addEventListener(\"click\", …) (AC 3)", m[1], m[1])
	}
}

// ---- AC 4: storage discipline -------------------------------------------------------------------
//
// Written ONCE here. TestTasksTemplate_AutoRefreshToggleIndicatorAndOneScript,
// TestTasksTemplate_ScriptContract and TestTasksTemplate_LiveScriptContract were
// AMENDED by this ticket to drop "localStorage" from their banned lists and point
// at this test.

func TestBoardFlap_StorageDiscipline(t *testing.T) {
	s := tasksHTML(t)
	_, script := liveScript(t, s)
	for _, banned := range []string{"sessionStorage", "document.cookie"} {
		if strings.Contains(s, banned) {
			t.Errorf("tasks.html contains %q; the one stored thing is the flap preference in localStorage (AC 4 / D2)", banned)
		}
	}
	uses := regexp.MustCompile(`\blocalStorage\b`).FindAllStringIndex(script, -1)
	if len(uses) == 0 {
		t.Fatalf("the script never uses localStorage; D2 keeps the flap preference under %q (AC 4)", flapStorageKey)
	}
	if strings.Count(s, "localStorage") != len(uses) {
		t.Errorf("tasks.html names localStorage outside the <script> (AC 4)")
	}
	for _, u := range uses {
		if !flapInsideSwallowingTry(script, u[0]) {
			line := script[strings.LastIndex(script[:u[0]], "\n")+1:]
			if nl := strings.Index(line, "\n"); nl >= 0 {
				line = line[:nl]
			}
			t.Errorf("localStorage is used outside a `try { … } catch (e) {}` whose catch swallows (AC 4 / D2: storage can "+
				"be denied — Safari private mode, a partitioned iframe, a full quota — and the board falls back to rows):\n  %s",
				strings.TrimSpace(line))
		}
	}
	quoted := strings.Count(s, `"`+flapStorageKey+`"`) + strings.Count(s, `'`+flapStorageKey+`'`)
	if quoted != 1 || strings.Count(s, flapStorageKey) != 1 {
		t.Errorf("the key literal %q appears %d time(s) quoted and %d time(s) in all, want exactly once (AC 4: one key, "+
			"one literal)", flapStorageKey, quoted, strings.Count(s, flapStorageKey))
	}
	for _, m := range regexp.MustCompile(`localStorage\s*\.\s*(?:getItem|setItem|removeItem)\s*\(\s*(["'])([^"']*)["']`).
		FindAllStringSubmatch(script, -1) {
		if m[2] != flapStorageKey {
			t.Errorf("the script touches the localStorage key %q; the only key is %q (AC 4 / D2)", m[2], flapStorageKey)
		}
	}
	if !strings.Contains(script, "removeItem(") {
		t.Errorf("the script never calls removeItem(; turning flaps off REMOVES the key (AC 4 / D2)")
	}
}

// flapInsideSwallowingTry reports whether script[at] lies inside the braces of a
// `try {` block that is followed by `catch (x) {}` with an empty body.
func flapInsideSwallowingTry(script string, at int) bool {
	tryRE := regexp.MustCompile(`\btry\s*\{`)
	swallow := regexp.MustCompile(`^\s*catch\s*\(\s*[A-Za-z_$][\w$]*\s*\)\s*\{\s*\}`)
	for _, m := range tryRE.FindAllStringIndex(script[:at], -1) {
		open := m[1] - 1
		closeAt, ok := flapMatchBrace(script, open)
		if !ok || closeAt < at {
			continue
		}
		if swallow.MatchString(script[closeAt+1:]) {
			return true
		}
	}
	return false
}

// flapMatchBrace returns the index of the } matching the { at open, skipping
// quoted strings and // and /* */ comments.
func flapMatchBrace(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			for i++; i < len(s) && s[i] != c; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
			} else if i+1 < len(s) && s[i+1] == '*' {
				if e := strings.Index(s[i+2:], "*/"); e >= 0 {
					i += 2 + e + 1
				}
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// ---- AC 12: the pure block --------------------------------------------------------------------

// flapPureBlock returns the text strictly between the two marker lines, or
// fails the test.
func flapPureBlock(t *testing.T) string {
	t.Helper()
	s := tasksHTML(t)
	b, e, err := flapMarkers(s)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return s[b:e]
}

// flapMarkers finds the exact marker lines and returns the block's [start, end).
func flapMarkers(s string) (int, int, error) {
	var begins, ends []int
	off := 0
	for _, line := range strings.SplitAfter(s, "\n") {
		switch strings.TrimSpace(line) {
		case flapPureBegin:
			begins = append(begins, off+len(line))
		case flapPureEnd:
			ends = append(ends, off)
		}
		off += len(line)
	}
	if len(begins) != 1 || len(ends) != 1 {
		return 0, 0, fmt.Errorf("tasks.html has %d %q line(s) and %d %q line(s), want exactly one of each (AC 12 / D10: "+
			"the pure rules are one delimited block inside the one <script>)", len(begins), flapPureBegin, len(ends), flapPureEnd)
	}
	if ends[0] < begins[0] {
		return 0, 0, fmt.Errorf("%q comes before %q (AC 12)", flapPureEnd, flapPureBegin)
	}
	return begins[0], ends[0], nil
}

func TestBoardFlap_PureBlockIsPure(t *testing.T) {
	s := tasksHTML(t)
	b, e, err := flapMarkers(s)
	if err != nil {
		t.Fatalf("%v", err)
	}
	si := strings.Index(s, "<script")
	sj := si + strings.Index(s[si:], "</script>")
	if !(si < b && e < sj) {
		t.Errorf("the pure block is not inside the one <script> (AC 12)")
	}
	// D10: "at the top of the IIFE" — between `(function () {` and the begin
	// marker there is nothing but comments and whitespace.
	iife := regexp.MustCompile(`\(function\s*\(\s*\)\s*\{`).FindStringIndex(s[si:sj])
	if iife == nil {
		t.Errorf("the script is no longer one IIFE `(function () { … })();` (D10)")
	} else {
		head := s[si+iife[1] : strings.LastIndex(s[:b-1], "\n")+1]
		for _, line := range strings.Split(head, "\n") {
			if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "//") && l != `"use strict";` {
				t.Errorf("code precedes the pure block inside the IIFE: %q. D10: the pure code sits at the TOP of the IIFE", l)
				break
			}
		}
	}
	block := s[b:e]
	if strings.TrimSpace(block) == "" {
		t.Fatalf("the pure block is empty (AC 12)")
	}
	if strings.Contains(block, "{{") {
		t.Errorf("the pure block contains {{ — html/template would read it as an action (AC 12)")
	}
	for _, name := range []string{"document", "window", "navigator", "localStorage", "Date", "performance", "setTimeout", "this"} {
		if loc := regexp.MustCompile(`\b` + name + `\b`).FindStringIndex(block); loc != nil {
			t.Errorf("the pure block names %q (AC 12 / D10: it never names document, window, navigator, localStorage, Date, "+
				"performance, setTimeout or this — not even in a comment): …%s…", name, block[maxInt(0, loc[0]-30):minInt(len(block), loc[1]+30)])
		}
	}
	for _, fn := range []string{"flapActive", "flapAim", "flapFlips", "flapAt", "drumAim", "drumAt", "flapShade", "flapFold",
		"flapLineStep", "flapLineStarts"} {
		if !regexp.MustCompile(`\bfunction\s+` + fn + `\s*\(`).MatchString(block) {
			t.Errorf("the pure block does not declare function %s (D10: the names are binding)", fn)
		}
	}
	for _, c := range []string{"FLAP_TEXT_MS", "FLAP_DIGIT_MS", "FLAP_FRAME_MS", "FLAP_GAP_MS", "FLAP_MIN", "FLAP_MAX", "FLAP_CELLS"} {
		if !regexp.MustCompile(`\b` + c + `\s*=`).MatchString(block) {
			t.Errorf("the pure block does not declare %s (D10)", c)
		}
	}
	if bad := flapTopLevelNonDecls(block); len(bad) > 0 {
		t.Errorf("the pure block holds top-level statements other than `var` and `function` declarations (D10): %q", bad)
	}
}

// flapTopLevelNonDecls returns the top-level statements of js (comments
// stripped) that start with neither `var` nor `function`.
func flapTopLevelNonDecls(js string) []string {
	var bad []string
	i := 0
	skipSpace := func() {
		for i < len(js) {
			switch {
			case js[i] == ' ' || js[i] == '\t' || js[i] == '\n' || js[i] == '\r' || js[i] == ';':
				i++
			case strings.HasPrefix(js[i:], "//"):
				for i < len(js) && js[i] != '\n' {
					i++
				}
			case strings.HasPrefix(js[i:], "/*"):
				if e := strings.Index(js[i+2:], "*/"); e >= 0 {
					i += 2 + e + 2
				} else {
					i = len(js)
				}
			default:
				return
			}
		}
	}
	// skipTo advances past the end of the current statement: for a var, the ;
	// at depth 0; for a function, its closing }.
	skipStatement := func(isFunc bool) {
		depth := 0
		for ; i < len(js); i++ {
			switch c := js[i]; c {
			case '"', '\'', '`':
				for i++; i < len(js) && js[i] != c; i++ {
					if js[i] == '\\' {
						i++
					}
				}
			case '/':
				if strings.HasPrefix(js[i:], "//") {
					for i < len(js) && js[i] != '\n' {
						i++
					}
				} else if strings.HasPrefix(js[i:], "/*") {
					if e := strings.Index(js[i+2:], "*/"); e >= 0 {
						i += 2 + e + 1
					}
				}
			case '(', '[', '{':
				depth++
			case ')', ']':
				depth--
			case '}':
				depth--
				if isFunc && depth == 0 {
					i++
					return
				}
			case ';':
				if !isFunc && depth == 0 {
					i++
					return
				}
			}
		}
	}
	for {
		skipSpace()
		if i >= len(js) {
			return bad
		}
		rest := js[i:]
		switch {
		case regexp.MustCompile(`^function\s`).MatchString(rest):
			skipStatement(true)
		case regexp.MustCompile(`^var\s`).MatchString(rest):
			skipStatement(false)
		default:
			end := strings.IndexAny(rest, "\n;")
			if end < 0 {
				end = len(rest)
			}
			bad = append(bad, strings.TrimSpace(rest[:end]))
			skipStatement(false)
		}
	}
}

// ---- AC 13: the motion clock --------------------------------------------------------------------

func TestBoardFlap_MotionClock(t *testing.T) {
	s := tasksHTML(t)
	_, script := liveScript(t, s)
	for _, banned := range []string{"performance.now", "Date.now", "requestAnimationFrame"} {
		if strings.Contains(s, banned) {
			t.Errorf("tasks.html contains %q. AC 13 / D7: motion runs on a motion clock advanced by FLAP_FRAME_MS per frame, "+
				"never on a wall clock, and frames are a self-arming setTimeout", banned)
		}
	}
	if n := strings.Count(s, "new Date("); n != 1 {
		t.Errorf("tasks.html has %d new Date(, want exactly 1 — the decorative clock's (AC 13 / B14)", n)
	}
	if !regexp.MustCompile(`\bmotionMs\s*\+=\s*FLAP_FRAME_MS\b`).MatchString(script) {
		t.Errorf("the script never advances the motion clock with `motionMs += FLAP_FRAME_MS` (AC 13 / D7)")
	}
	if !regexp.MustCompile(`\bsetTimeout\(\s*frame\s*,\s*FLAP_FRAME_MS\s*\)`).MatchString(script) {
		t.Errorf("the frame loop is not the self-arming `setTimeout(frame, FLAP_FRAME_MS)` (AC 13 / D7, prv_schedule)")
	}
	if regexp.MustCompile(`\bsetInterval\(\s*frame\b`).MatchString(script) {
		t.Errorf("the frame loop runs on setInterval; D7: it arms only while something is turning")
	}
	if !regexp.MustCompile(`\bfunction\s+frame\s*\(`).MatchString(script) {
		t.Errorf("the script declares no function frame() (AC 13 / D7)")
	}
}

// ---- AC 20: no data attribute on the <script> tag ------------------------------------------------

func TestBoardFlap_ScriptTagUnchanged(t *testing.T) {
	const want = `<script data-reload="{{.ReloadURL}}" data-interval="{{.RefreshSeconds}}" data-page-interval="{{.PageSeconds}}" ` +
		`data-refresh="{{.RefreshMode}}" data-stream="{{.StreamURL}}" data-retry="{{.RetrySeconds}}" data-version="{{.BoardVersion}}">`
	tag, _ := liveScript(t, tasksHTML(t))
	if tag != want {
		t.Errorf("the <script> tag changed:\n  %s\nwant byte-unchanged:\n  %s\n(AC 20: nothing server-side changes — no data "+
			"attribute, the preference lives only in the browser)", tag, want)
	}
}

// ---- the node golden runner (AC 6-11, D10) --------------------------------------------------------

type flapCellIs struct {
	Cell int    `json:"cell"`
	Is   string `json:"is"`
}

type flapFlipIs struct {
	Flip int    `json:"flip"`
	Is   string `json:"is"`
}

type flapReelCase struct {
	Name       string       `json:"name"`
	Src        string       `json:"src"`
	Shown      string       `json:"shown"`
	Target     string       `json:"target"`
	Upto       int          `json:"upto"`
	Flips      *int         `json:"flips"`
	FlipsMin   *int         `json:"flipsMin"`
	FlipsMax   *int         `json:"flipsMax"`
	Len        *int         `json:"len"`
	Path       []flapCellIs `json:"path"`
	PathPrefix []flapCellIs `json:"pathPrefix"`
	Still      []flapCellIs `json:"still"`
	At         []flapFlipIs `json:"at"`
	CellAt     []struct {
		Flip int    `json:"flip"`
		Cell int    `json:"cell"`
		Is   string `json:"is"`
	} `json:"cellAt"`
	Class []struct {
		Cell  int    `json:"cell"`
		Class string `json:"class"`
		Flip  int    `json:"flip"`
	} `json:"class"`
}

type flapDrumCase struct {
	Src    string `json:"src"`
	Place  int    `json:"place"`
	Shown  int    `json:"shown"`
	Target int    `json:"target"`
	Full   bool   `json:"full"`
	Upto   int    `json:"upto"`
	Flips  int    `json:"flips"`
	At     []struct {
		Flip int `json:"flip"`
		Is   int `json:"is"`
	} `json:"at"`
}

type flapGolden struct {
	Consts map[string]float64 `json:"consts"`
	Active []struct {
		Pref    *string `json:"pref"`
		Reduced bool    `json:"reduced"`
		Phone   bool    `json:"phone"`
		Want    bool    `json:"want"`
	} `json:"active"`
	Reel  []flapReelCase `json:"reel"`
	Drums []flapDrumCase `json:"drums"`
	Shade []struct {
		H    int `json:"h"`
		Full int `json:"full"`
		Want int `json:"want"`
	} `json:"shade"`
	Fold []struct {
		Part  int      `json:"part"`
		Flap  string   `json:"flap"`
		Scale *float64 `json:"scale"`
		Tol   float64  `json:"tol"`
		Below *float64 `json:"below"`
		Above *float64 `json:"above"`
	} `json:"fold"`
	Step []struct {
		N      int             `json:"n"`
		PageMs int             `json:"pageMs"`
		Want   json.RawMessage `json:"want"`
	} `json:"step"`
	Starts []struct {
		Base int             `json:"base"`
		Ends []int           `json:"ends"`
		Durs []int           `json:"durs"`
		Gap  int             `json:"gap"`
		Step json.RawMessage `json:"step"`
		Want []int           `json:"want"`
	} `json:"starts"`
}

// The exhaustive sweeps of test_every_change_rattles and test_drums.
var flapTextDrums = []string{"0123456789", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz"}

func flapPlaceRange(place int) (low, high int) {
	switch place {
	case 0:
		return -1, 2
	case 2:
		return 0, 5
	default:
		return 0, 9
	}
}

func loadFlapGolden(t *testing.T) *flapGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "flap", "golden.json"))
	if err != nil {
		t.Fatalf("read testdata/flap/golden.json: %v", err)
	}
	var g flapGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse testdata/flap/golden.json: %v", err)
	}
	if len(g.Reel) == 0 || len(g.Drums) == 0 || len(g.Active) == 0 {
		t.Fatalf("POSITIVE CONTROL: golden.json parsed to %d reel, %d drum, %d active cases", len(g.Reel), len(g.Drums), len(g.Active))
	}
	return &g
}

// flapRequests is every call the harness makes, keyed by id.
func flapRequests(g *flapGolden) []map[string]any {
	var req []map[string]any
	names := make([]string, 0, len(g.Consts))
	for k := range g.Consts {
		names = append(names, k)
	}
	req = append(req, map[string]any{"id": "consts", "op": "consts", "names": names})
	for i, c := range g.Active {
		var pref any
		if c.Pref != nil {
			pref = *c.Pref
		}
		req = append(req, map[string]any{"id": fmt.Sprintf("active/%d", i), "op": "active", "pref": pref,
			"reduced": c.Reduced, "phone": c.Phone})
	}
	for i, c := range g.Reel {
		req = append(req, map[string]any{"id": fmt.Sprintf("reel/%d", i), "op": "reel", "shown": c.Shown,
			"target": c.Target, "upto": c.Upto})
	}
	for _, d := range flapTextDrums {
		for _, a := range d {
			for _, b := range d {
				req = append(req, map[string]any{"id": "reelx/" + string(a) + string(b), "op": "reel",
					"shown": string(a), "target": string(b)})
			}
		}
	}
	for i, c := range g.Drums {
		req = append(req, map[string]any{"id": fmt.Sprintf("drum/%d", i), "op": "drum", "place": c.Place,
			"shown": c.Shown, "target": c.Target, "full": c.Full, "upto": c.Upto})
	}
	for place := 0; place < 4; place++ {
		low, high := flapPlaceRange(place)
		for from := low; from <= high; from++ {
			for to := low; to <= high; to++ {
				req = append(req, map[string]any{"id": fmt.Sprintf("drumx/%d/%d/%d", place, from, to), "op": "drum",
					"place": place, "shown": from, "target": to, "full": false})
			}
		}
	}
	for i, c := range g.Shade {
		req = append(req, map[string]any{"id": fmt.Sprintf("shade/%d", i), "op": "shade", "h": c.H, "full": c.Full})
	}
	for i, c := range g.Fold {
		req = append(req, map[string]any{"id": fmt.Sprintf("fold/%d", i), "op": "fold", "part": c.Part})
	}
	for i, c := range g.Step {
		req = append(req, map[string]any{"id": fmt.Sprintf("step/%d", i), "op": "step", "n": c.N, "pageMs": c.PageMs})
	}
	for i, c := range g.Starts {
		req = append(req, map[string]any{"id": fmt.Sprintf("starts/%d", i), "op": "starts", "base": c.Base,
			"ends": c.Ends, "durs": c.Durs, "gap": c.Gap, "step": c.Step})
	}
	return req
}

type flapReply struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

// runFlapHarness writes block and harness.js into a temp dir and runs node on
// them. It is the only place the tests touch node.
func runFlapHarness(nodeBin, dir, block string, req []map[string]any) (map[string]flapReply, error) {
	harness, err := os.ReadFile(filepath.Join("testdata", "flap", "harness.js"))
	if err != nil {
		return nil, fmt.Errorf("read testdata/flap/harness.js: %w", err)
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"flap.js": []byte(block), "harness.js": harness, "request.json": reqJSON}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(nodeBin, filepath.Join(dir, "harness.js"), filepath.Join(dir, "flap.js"), filepath.Join(dir, "request.json"))
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("node harness.js failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var replies []flapReply
	if err := json.Unmarshal(out, &replies); err != nil {
		return nil, fmt.Errorf("parse harness output: %v\n%s", err, out)
	}
	if len(replies) != len(req) {
		return nil, fmt.Errorf("harness answered %d of %d calls", len(replies), len(req))
	}
	byID := map[string]flapReply{}
	for _, r := range replies {
		byID[r.ID] = r
	}
	return byID, nil
}

var flapNodeRun struct {
	once    sync.Once
	golden  *flapGolden
	replies map[string]flapReply
	err     error
}

// flapNode returns the golden file and the harness's replies for every call,
// running node once per test binary. It skips (or, under SWB_REQUIRE_NODE=1,
// fails) when node is not on PATH, and fails when the pure block is missing.
func flapNode(t *testing.T) (*flapGolden, map[string]flapReply) {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		msg := "SKIPPING the board-splitflap golden tests: `node` is not on PATH, so the flap block's pure rules " +
			"(AC 6-11) are NOT being checked. Install node (/usr/bin/node on the workstation) or set SWB_REQUIRE_NODE=1 " +
			"to make this a failure."
		if os.Getenv("SWB_REQUIRE_NODE") == "1" {
			t.Fatalf("SWB_REQUIRE_NODE=1 but node is not on PATH: %v", err)
		}
		t.Skip(msg)
	}
	g := loadFlapGolden(t)
	block := flapPureBlock(t)
	flapNodeRun.once.Do(func() {
		dir, err := os.MkdirTemp("", "board-flap-")
		if err != nil {
			flapNodeRun.err = err
			return
		}
		defer os.RemoveAll(dir)
		flapNodeRun.golden = g
		flapNodeRun.replies, flapNodeRun.err = runFlapHarness(nodeBin, dir, block, flapRequests(g))
	})
	if flapNodeRun.err != nil {
		t.Fatalf("%v", flapNodeRun.err)
	}
	return flapNodeRun.golden, flapNodeRun.replies
}

// flapValue decodes reply id's value into v, failing the test on a harness error.
func flapValue(t *testing.T, replies map[string]flapReply, id string, v any) bool {
	t.Helper()
	r, ok := replies[id]
	if !ok {
		t.Errorf("%s: no reply from the harness", id)
		return false
	}
	if r.Error != "" {
		t.Errorf("%s: the pure block threw: %s", id, r.Error)
		return false
	}
	if err := json.Unmarshal(r.Value, v); err != nil {
		t.Errorf("%s: cannot decode %s: %v", id, r.Value, err)
		return false
	}
	return true
}

type flapTrace struct {
	Flips int      `json:"flips"`
	Trace []string `json:"trace"`
}

type flapDrumTrace struct {
	Flips int   `json:"flips"`
	Trace []int `json:"trace"`
}

func flapRuneAt(s string, i int) (rune, bool) {
	r := []rune(s)
	if i < 0 || i >= len(r) {
		return 0, false
	}
	return r[i], true
}

func flapClassOK(r rune, class string) bool {
	switch class {
	case "upper":
		return r >= 'A' && r <= 'Z'
	case "lower":
		return r >= 'a' && r <= 'z'
	case "digit":
		return r >= '0' && r <= '9'
	}
	return false
}

// checkFlapReel applies one reel case's expectations to the harness's trace.
// trace[i] is flapAt(plan, i) for i in 0..max(flips, upto).
func checkFlapReel(t *testing.T, c flapReelCase, tr flapTrace) {
	t.Helper()
	name := fmt.Sprintf("%s (%s: %q -> %q)", c.Name, c.Src, c.Shown, c.Target)
	if len(tr.Trace) < tr.Flips+1 {
		t.Errorf("%s: trace too short", name)
		return
	}
	for i, s := range tr.Trace {
		if !utf8.ValidString(s) || strings.ContainsRune(s, utf8.RuneError) {
			t.Errorf("%s: flip %d shows %q, which is not valid text (a lone surrogate; D4: characters are code points)", name, i, s)
		}
	}
	if c.Flips != nil && tr.Flips != *c.Flips {
		t.Errorf("%s: flapFlips = %d, want %d", name, tr.Flips, *c.Flips)
	}
	if c.FlipsMin != nil && tr.Flips < *c.FlipsMin {
		t.Errorf("%s: flapFlips = %d, want >= %d", name, tr.Flips, *c.FlipsMin)
	}
	if c.FlipsMax != nil && tr.Flips > *c.FlipsMax {
		t.Errorf("%s: flapFlips = %d, want <= %d", name, tr.Flips, *c.FlipsMax)
	}
	if c.Len != nil {
		for i, s := range tr.Trace {
			if n := utf8.RuneCountInString(s); n != *c.Len {
				t.Errorf("%s: flip %d shows %q, %d characters, want %d (D9: reel_at always has the target's length)", name, i, s, n, *c.Len)
				break
			}
		}
	}
	path := func(cell int) string {
		var b strings.Builder
		for i := 0; i <= tr.Flips; i++ {
			r, ok := flapRuneAt(tr.Trace[i], cell)
			if !ok {
				b.WriteString("?")
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}
	for _, p := range c.Path {
		if got := path(p.Cell); got != p.Is {
			t.Errorf("%s: cell %d's path is %q, want %q", name, p.Cell, got, p.Is)
		}
	}
	for _, p := range c.PathPrefix {
		if got := path(p.Cell); !strings.HasPrefix(got, p.Is) {
			t.Errorf("%s: cell %d's path is %q, want it to start %q", name, p.Cell, got, p.Is)
		}
	}
	for _, st := range c.Still {
		want, _ := utf8.DecodeRuneInString(st.Is)
		for i, s := range tr.Trace {
			if r, ok := flapRuneAt(s, st.Cell); !ok || r != want {
				t.Errorf("%s: cell %d shows %q at flip %d (%q); it never moves from %q", name, st.Cell, string(r), i, s, st.Is)
				break
			}
		}
	}
	for _, a := range c.At {
		if a.Flip >= len(tr.Trace) {
			t.Errorf("%s: no flip %d in the trace", name, a.Flip)
		} else if tr.Trace[a.Flip] != a.Is {
			t.Errorf("%s: flapAt(plan, %d) = %q, want %q", name, a.Flip, tr.Trace[a.Flip], a.Is)
		}
	}
	for _, a := range c.CellAt {
		want, _ := utf8.DecodeRuneInString(a.Is)
		if a.Flip >= len(tr.Trace) {
			t.Errorf("%s: no flip %d in the trace", name, a.Flip)
		} else if r, ok := flapRuneAt(tr.Trace[a.Flip], a.Cell); !ok || r != want {
			t.Errorf("%s: cell %d at flip %d is %q (%q), want %q", name, a.Cell, a.Flip, string(r), tr.Trace[a.Flip], a.Is)
		}
	}
	for _, k := range c.Class {
		lo, hi := k.Flip, k.Flip
		if k.Flip < 0 {
			lo, hi = 0, len(tr.Trace)-1
		}
		for i := lo; i <= hi && i < len(tr.Trace); i++ {
			if r, ok := flapRuneAt(tr.Trace[i], k.Cell); !ok || !flapClassOK(r, k.Class) {
				t.Errorf("%s: cell %d at flip %d is %q (%q), want a %s character", name, k.Cell, i, string(r), tr.Trace[i], k.Class)
				break
			}
		}
	}
}

func flapReelCases(t *testing.T, src func(string) bool) {
	g, replies := flapNode(t)
	n := 0
	for i, c := range g.Reel {
		if !src(c.Src) {
			continue
		}
		n++
		var tr flapTrace
		if flapValue(t, replies, fmt.Sprintf("reel/%d", i), &tr) {
			checkFlapReel(t, c, tr)
		}
	}
	if n == 0 {
		t.Fatalf("POSITIVE CONTROL: no golden reel cases selected")
	}
}

// ---- AC 6 ----

func TestBoardFlap_ActiveGoldens(t *testing.T) {
	g, replies := flapNode(t)
	for i, c := range g.Active {
		var got bool
		pref := "null"
		if c.Pref != nil {
			pref = fmt.Sprintf("%q", *c.Pref)
		}
		if flapValue(t, replies, fmt.Sprintf("active/%d", i), &got) && got != c.Want {
			t.Errorf("flapActive(%s, %v, %v) = %v, want %v (AC 6 / D1: flaps only on the exact pref \"flaps\", no reduced "+
				"motion, not the phone)", pref, c.Reduced, c.Phone, got, c.Want)
		}
	}
}

// ---- AC 7 ----

func TestBoardFlap_ReelGoldens(t *testing.T) {
	flapReelCases(t, func(src string) bool { return src != "board" })

	// test_every_change_rattles, over each drum and every (a, b).
	_, replies := flapNode(t)
	for _, d := range flapTextDrums {
		size := len(d)
		for _, a := range d {
			for _, b := range d {
				id := "reelx/" + string(a) + string(b)
				var tr flapTrace
				if !flapValue(t, replies, id, &tr) {
					continue
				}
				if a == b {
					if tr.Flips != 0 {
						t.Errorf("%c->%c: %d flips, want 0 (test_every_change_rattles)", a, b, tr.Flips)
					}
					continue
				}
				if tr.Flips < 6 || tr.Flips > 16 {
					t.Errorf("%c->%c: %d flips, want 6..16 (test_every_change_rattles)", a, b, tr.Flips)
					continue
				}
				if tr.Trace[tr.Flips] != string(b) {
					t.Errorf("%c->%c: lands on %q, want %q", a, b, tr.Trace[tr.Flips], string(b))
				}
				for i := 0; i < tr.Flips; i++ {
					here := strings.Index(d, tr.Trace[i])
					if here < 0 || len(tr.Trace[i]) != 1 || string(d[(here+1)%size]) != tr.Trace[i+1] {
						t.Errorf("%c->%c: flap %d %q is followed by %q, not the next flap on its drum", a, b, i, tr.Trace[i], tr.Trace[i+1])
						break
					}
				}
			}
		}
	}
}

// ---- AC 8 ----

func TestBoardFlap_BoardReelGoldens(t *testing.T) {
	flapReelCases(t, func(src string) bool { return src == "board" })
}

// ---- AC 9 ----

func TestBoardFlap_DrumGoldens(t *testing.T) {
	g, replies := flapNode(t)
	for i, c := range g.Drums {
		name := fmt.Sprintf("drum_aim(place %d, %d -> %d, full_turn %v)", c.Place, c.Shown, c.Target, c.Full)
		var tr flapDrumTrace
		if !flapValue(t, replies, fmt.Sprintf("drum/%d", i), &tr) {
			continue
		}
		if tr.Flips != c.Flips {
			t.Errorf("%s: flips = %d, want %d (test_drums)", name, tr.Flips, c.Flips)
		}
		for _, a := range c.At {
			if a.Flip >= len(tr.Trace) {
				t.Errorf("%s: no flip %d in the trace", name, a.Flip)
			} else if tr.Trace[a.Flip] != a.Is {
				t.Errorf("%s: drumAt(%d) = %d, want %d (test_drums)", name, a.Flip, tr.Trace[a.Flip], a.Is)
			}
		}
	}
	// test_drums' sweep: every move on every drum rattles, lands right, and
	// shows only flaps that drum carries, one forward flap at a time.
	for place := 0; place < 4; place++ {
		low, high := flapPlaceRange(place)
		for from := low; from <= high; from++ {
			for to := low; to <= high; to++ {
				name := fmt.Sprintf("drum_aim(place %d, %d -> %d)", place, from, to)
				var tr flapDrumTrace
				if !flapValue(t, replies, fmt.Sprintf("drumx/%d/%d/%d", place, from, to), &tr) {
					continue
				}
				if tr.Trace[0] != from || tr.Trace[tr.Flips] != to {
					t.Errorf("%s: shows %d at 0 and %d at %d, want %d and %d", name, tr.Trace[0], tr.Trace[tr.Flips], tr.Flips, from, to)
				}
				if from == to && tr.Flips != 0 {
					t.Errorf("%s: %d flips, want 0", name, tr.Flips)
				}
				if from != to && (tr.Flips < 6 || tr.Flips > 15) {
					t.Errorf("%s: %d flips, want 6..15", name, tr.Flips)
				}
				for i := 0; i <= tr.Flips; i++ {
					if tr.Trace[i] < low || tr.Trace[i] > high {
						t.Errorf("%s: flip %d shows %d, not a flap of this drum (%d..%d)", name, i, tr.Trace[i], low, high)
						break
					}
				}
				for i := 0; i < tr.Flips; i++ {
					next := tr.Trace[i] + 1
					if tr.Trace[i] == high {
						next = low
					}
					if tr.Trace[i+1] != next {
						t.Errorf("%s: flap %d (%d) is followed by %d, want %d (forward only)", name, i, tr.Trace[i], tr.Trace[i+1], next)
						break
					}
				}
			}
		}
	}
}

// ---- AC 10 ----

func TestBoardFlap_FoldAndShadeGoldens(t *testing.T) {
	g, replies := flapNode(t)
	for i, c := range g.Shade {
		var got int
		if flapValue(t, replies, fmt.Sprintf("shade/%d", i), &got) && got != c.Want {
			t.Errorf("flapShade(%d, %d) = %d, want %d (AC 10, prv_shade: >= 85%% is 0, >= 40%% is 1, else 2)", c.H, c.Full, got, c.Want)
		}
	}
	for i, c := range g.Fold {
		var got struct {
			Flap  string `json:"flap"`
			Scale any    `json:"scale"`
		}
		if !flapValue(t, replies, fmt.Sprintf("fold/%d", i), &got) {
			continue
		}
		if got.Flap != c.Flap {
			t.Errorf("flapFold(%d).flap = %q, want %q (AC 10 / D8)", c.Part, got.Flap, c.Flap)
		}
		scale, ok := got.Scale.(float64)
		if !ok {
			t.Errorf("flapFold(%d).scale = %v, want a finite number (AC 10)", c.Part, got.Scale)
			continue
		}
		switch {
		case c.Scale != nil && math.Abs(scale-*c.Scale) > c.Tol:
			t.Errorf("flapFold(%d).scale = %.6f, want %.6f ±%g (AC 10 / D8: cos easing)", c.Part, scale, *c.Scale, c.Tol)
		case c.Below != nil && !(scale < *c.Below):
			t.Errorf("flapFold(%d).scale = %.6f, want below %g (AC 10)", c.Part, scale, *c.Below)
		case c.Above != nil && !(scale > *c.Above):
			t.Errorf("flapFold(%d).scale = %.6f, want above %g (AC 10)", c.Part, scale, *c.Above)
		}
	}
}

// ---- AC 11 ----

func TestBoardFlap_LineGoldens(t *testing.T) {
	g, replies := flapNode(t)
	for i, c := range g.Step {
		var got any
		if !flapValue(t, replies, fmt.Sprintf("step/%d", i), &got) {
			continue
		}
		var want any
		_ = json.Unmarshal(c.Want, &want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("flapLineStep(%d, %d) = %v, want %v (AC 11 / D6)", c.N, c.PageMs, got, want)
		}
	}
	for i, c := range g.Starts {
		var got []any
		if !flapValue(t, replies, fmt.Sprintf("starts/%d", i), &got) {
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(intsAsAny(c.Want)) {
			t.Errorf("flapLineStarts(%d, %v, %v, %d, %s) = %v, want %v (AC 11 / D6: a line starts once every moving line "+
				"above it has rested, plus the gap, capped at base + k × step)", c.Base, c.Ends, c.Durs, c.Gap, c.Step, got, c.Want)
		}
	}
}

func intsAsAny(xs []int) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = float64(x)
	}
	return out
}

// ---- D10: the binding constants ----

func TestBoardFlap_Constants(t *testing.T) {
	g, replies := flapNode(t)
	var got map[string]any
	if !flapValue(t, replies, "consts", &got) {
		return
	}
	for name, want := range g.Consts {
		if v, ok := got[name].(float64); !ok || v != want {
			t.Errorf("%s = %v, want %v (D10; face.c / reel.h)", name, got[name], want)
		}
	}
}
