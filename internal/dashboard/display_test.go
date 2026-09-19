package dashboard

// board-departures (SWT-67, docs/tickets/board-departures_SPEC.md) Part 1,
// criteria 1-7: the PURE display helpers of internal/dashboard/display.go.
// ZERO I/O beyond this package's own source. display.go is lights.go's and
// sections.go's third sibling — same shape, same purity scan.
//
// IMPOSED SURFACE (SPEC criterion 1 — the SPEC names every symbol; the
// boardPanel struct's FIELD names are the implementer's, so this file reads the
// pane/order/class table through boardPanes' output and boardPanels' KEYS only):
//
//	// display.go
//	const boardPriorityMark = 2
//	type boardPane struct{ Key string; Sections []boardSection }
//	type boardTally struct{ NeedYou, InFlight, Incoming, Queued, DoneToday, Open int }
//	var boardPanels map[string]boardPanel
//	func boardPanes(secs []boardSection) []boardPane
//	func boardTallies(secs []boardSection) boardTally
//	func remarkFor(l light, status string) string
//	func elapsedFor(class string, minutes int) string
//	func projectHue(slug string) int
//	func projectLabel(filter string) string
//	// display.go — forced by criterion 26, see board_departures_structure_test.go
//	type boardTallyItem struct{ Class, Label string; Count int }
//	func (boardTally) Items() []boardTallyItem
//	// sections.go
//	type boardSection struct{ Key, Title string; Tasks []taskRow; Class string; Order int }
//
// GREENFIELD NOTE — EXPECTED RED: display.go does not exist and boardSection has
// no Class/Order, so package dashboard's test binary compile-FAILS. Once it
// compiles, every test here fails until the SPEC is implemented.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - boardPanes drops a section key (e.g. other) -> PanelTableCoversEverySection, Panes.
//   - boardPanes reorders a pane by Order instead of boardSectionOrder -> Panes.
//   - incoming given order: 0 -> Panes.
//   - remarkFor returns one constant, or loses its pr_open branch -> RemarkForTable, RemarkForEveryLightAndStatus.
//   - elapsedFor returns a value for done -> ElapsedFor.
//   - projectHue returns a constant -> ProjectHue (the two-distinct-hues control).
//   - boardSections sets Class/Order -> BoardSectionsNeverSetsTheDisplayFields.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// ---- criterion 1: display.go exists, declares the surface, and is PURE ----------

func TestDisplay_PureNoIONoClock(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "display.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse internal/dashboard/display.go: %v. Criterion 1: the pure display helpers live in their "+
			"own file, the lights.go / sections.go sibling", err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		switch {
		case p == "time", p == "context", p == "net/http", p == "os", p == "database/sql", strings.Contains(p, "pgx"):
			t.Errorf("display.go imports %q. Criterion 1 / invariant 7: every helper here is pure — no I/O, no clock; "+
				"the elapsed minutes arrive as an int from Postgres (B6)", p)
		}
	}
	declared := map[string]string{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				declared[d.Name.Name] = "func"
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					declared[s.Name.Name] = "type"
				case *ast.ValueSpec:
					for _, n := range s.Names {
						declared[n.Name] = d.Tok.String()
					}
				}
			}
		}
	}
	for name, kind := range map[string]string{
		"boardPriorityMark": "const", "boardPane": "type", "boardTally": "type", "boardPanels": "var",
		"boardPanes": "func", "boardTallies": "func", "remarkFor": "func", "elapsedFor": "func",
		"projectHue": "func", "projectLabel": "func",
	} {
		if declared[name] != kind {
			t.Errorf("display.go does not declare %s %s (criterion 1)", kind, name)
		}
	}
	for _, fn := range []string{"boardPanes", "boardTallies", "remarkFor", "elapsedFor", "projectHue", "projectLabel"} {
		body := funcBodySrc(t, "display.go", fn)
		if body == "" {
			continue // reported above
		}
		for _, banned := range []string{"s.pool", "Query(", "Exec(", "time."} {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q: every helper in display.go is a pure function of its arguments (criterion 1)", fn, banned)
			}
		}
	}
}

// B8: the priority mark's threshold is a const, not a literal 2 in the handler.
func TestBoardPriorityMark_IsTwo(t *testing.T) {
	if boardPriorityMark != 2 {
		t.Errorf("boardPriorityMark = %d, want 2 (B8: the red ▲ marks priority >= 2)", boardPriorityMark)
	}
}

// ---- criterion 2: the panel table and the panes --------------------------------

// dpSections builds one non-empty boardSection per key of boardSectionOrder, in
// boardSectionOrder, as boardSections would.
func dpSections(keys ...string) []boardSection {
	var out []boardSection
	for _, s := range boardSectionOrder {
		for _, k := range keys {
			if k != s.Key {
				continue
			}
			out = append(out, boardSection{Key: s.Key, Title: s.Title,
				Tasks: []taskRow{{ID: 1, Title: "T", Status: "ready"}}})
		}
	}
	return out
}

func dpAllKeys() []string {
	var keys []string
	for _, s := range boardSectionOrder {
		keys = append(keys, s.Key)
	}
	return keys
}

// Criterion 2, half one: boardPanels covers boardSectionOrder EXACTLY — a key
// missing from either side fails. (The table's values are asserted through
// boardPanes below, so this file needs no boardPanel field name.)
func TestBoardPanels_CoversEverySectionKeyBothWays(t *testing.T) {
	inOrder := map[string]bool{}
	for _, s := range boardSectionOrder {
		inOrder[s.Key] = true
		if _, ok := boardPanels[s.Key]; !ok {
			t.Errorf("boardPanels has no entry for section %q. Criterion 2 / B2: a section with nowhere to go "+
				"disappears from the board silently — `other` is exactly why the table must be total", s.Key)
		}
	}
	for k := range boardPanels {
		if !inOrder[k] {
			t.Errorf("boardPanels names %q, which is not a boardSectionOrder key (criterion 2: the table covers "+
				"boardSectionOrder EXACTLY, in both directions)", k)
		}
	}
	if len(boardPanels) != len(boardSectionOrder) {
		t.Errorf("boardPanels has %d entries, want %d (one per boardSectionOrder key)", len(boardPanels), len(boardSectionOrder))
	}
}

// Criterion 2, half two: two panes in order; each pane's sections stay in
// boardSectionOrder (the DOM order SWT-59 I3 pins), carrying B2's Class and
// Order. The visual order is CSS `order`, never a Go re-sort.
func TestBoardPanes_TwoPanesKeepDocumentOrderAndCarryTheTable(t *testing.T) {
	panes := boardPanes(dpSections(dpAllKeys()...))
	if len(panes) != 2 {
		t.Fatalf("boardPanes over every section returned %d pane(s), want 2 (left, right) (criterion 2)", len(panes))
	}
	if panes[0].Key != "left" || panes[1].Key != "right" {
		t.Fatalf("boardPanes keys = [%s %s], want [left right] in that order (criterion 2)", panes[0].Key, panes[1].Key)
	}
	want := map[string]struct {
		pane  string
		order int
		class string
	}{
		"incoming":  {"left", 3, "panel grow"},
		"blocked":   {"left", 1, "panel alarm"},
		"in_flight": {"left", 2, "panel"},
		"queue":     {"right", 1, "panel grow"},
		"holding":   {"right", 2, "panel"},
		"done":      {"right", 3, "panel"},
		"other":     {"right", 4, "panel"},
	}
	seen := map[string]bool{}
	for _, p := range panes {
		var keys []string
		for _, s := range p.Sections {
			keys = append(keys, s.Key)
			w, ok := want[s.Key]
			if !ok {
				t.Errorf("pane %s holds unknown section %q", p.Key, s.Key)
				continue
			}
			seen[s.Key] = true
			if p.Key != w.pane {
				t.Errorf("section %s is in pane %s, want %s (criterion 2 / B2's table)", s.Key, p.Key, w.pane)
			}
			if s.Order != w.order {
				t.Errorf("section %s has Order %d, want %d (B2: the VISUAL order is CSS `order`, so incoming is "+
					"drawn last in the left column while staying first in the document)", s.Key, s.Order, w.order)
			}
			if s.Class != w.class {
				t.Errorf("section %s has Class %q, want %q (B2: `grow` takes the pane's spare height, `alarm` is the "+
					"red heading — both from the Go table, so the template needs no branch)", s.Key, s.Class, w.class)
			}
			if s.Title == "" || len(s.Tasks) != 1 {
				t.Errorf("section %s lost its Title or its Tasks passing through boardPanes (criterion 2)", s.Key)
			}
		}
		// Within a pane the sections keep boardSectionOrder: the DOM order of the
		// <h2>s across the page must stay byte-identical to today's (B2, SWT-59 I3).
		var wantKeys []string
		for _, s := range boardSectionOrder {
			if want[s.Key].pane == p.Key {
				wantKeys = append(wantKeys, s.Key)
			}
		}
		if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
			t.Errorf("pane %s sections = [%s], want [%s] — boardSectionOrder order, NOT Order order (criterion 2: the "+
				"split is a contiguous prefix/suffix of boardSectionOrder)", p.Key, strings.Join(keys, " "), strings.Join(wantKeys, " "))
		}
	}
	for _, s := range boardSectionOrder {
		if !seen[s.Key] {
			t.Errorf("boardPanes dropped section %q: every boardSectionOrder key reaches a pane (criterion 2)", s.Key)
		}
	}
}

func TestBoardPanes_EmptyPaneOmittedAndNilIsNil(t *testing.T) {
	if got := boardPanes(nil); got != nil {
		t.Errorf("boardPanes(nil) = %v, want nil (criterion 2)", got)
	}
	if got := boardPanes([]boardSection{}); len(got) != 0 {
		t.Errorf("boardPanes(empty) = %v, want no panes (criterion 2)", got)
	}
	left := boardPanes(dpSections("incoming", "blocked", "in_flight"))
	if len(left) != 1 || left[0].Key != "left" {
		t.Fatalf("boardPanes with left-only sections = %v, want exactly the left pane (criterion 2: an empty pane is omitted)", left)
	}
	right := boardPanes(dpSections("queue", "done"))
	if len(right) != 1 || right[0].Key != "right" {
		t.Fatalf("boardPanes with right-only sections = %v, want exactly the right pane (criterion 2)", right)
	}
	if len(right[0].Sections) != 2 || right[0].Sections[0].Key != "queue" || right[0].Sections[1].Key != "done" {
		t.Errorf("the right pane = %v, want [queue done] — absent sections are not invented (criterion 2)", right[0].Sections)
	}
}

// Criterion 16: Class and Order are set ONLY by boardPanes; boardSections leaves
// them zero, so the display fields cannot drift into the data layer.
func TestBoardSectionsNeverSetsTheDisplayFields(t *testing.T) {
	rows := []taskRow{
		{ID: 1, Status: "ready", Light: light{Class: "none", Label: "ready, queued"}},
		{ID: 2, Status: "blocked", Light: light{Class: "none", Label: "blocked on a dependency"}},
		{ID: 3, Status: "holding", Light: light{Class: "none", Label: "holding (review lane; not queued)"}},
	}
	secs := boardSections(rows)
	if len(secs) == 0 {
		t.Fatalf("CONTROL: boardSections returned nothing for %d rows", len(rows))
	}
	for _, s := range secs {
		if s.Class != "" || s.Order != 0 {
			t.Errorf("boardSections set section %s Class=%q Order=%d; criterion 16: both are display-only and set "+
				"ONLY by boardPanes", s.Key, s.Class, s.Order)
		}
	}
}

// ---- criterion 3: remarkFor is exhaustive, lowercase and never empty -----------

// expectedRemark is B7's table, written independently of remarkFor.
func expectedRemark(class, status string) string {
	switch class {
	case "input":
		return "waiting on you"
	case "stale":
		return "no signal"
	case "next":
		return "next up"
	case "working":
		switch status {
		case "claimed":
			return "claimed"
		case "pr_open":
			return "pr open"
		case "awaiting_ci":
			return "awaiting ci"
		case "awaiting_merge":
			return "awaiting merge"
		}
		return "in progress" // in_progress, and holding/ready/blocked with a session signal
	case "done":
		switch status {
		case "done_locally":
			return "done locally"
		case "delivered":
			return "delivered"
		}
		return "done" // closed
	}
	switch status { // none
	case "holding":
		return "holding"
	case "blocked":
		return "blocked"
	case "ready":
		return "queued"
	case "closed":
		return "dismissed"
	}
	return status
}

// B7's table, row by row, including the B5 rows that carry the dropped status
// column's information as words.
func TestRemarkFor_Table(t *testing.T) {
	for _, tc := range []struct{ class, status, want string }{
		{"input", "needs_feedback", "waiting on you"},
		{"input", "ready", "waiting on you"},
		{"input", "holding", "waiting on you"},
		{"working", "claimed", "claimed"},
		{"working", "in_progress", "in progress"},
		{"working", "pr_open", "pr open"},
		{"working", "awaiting_ci", "awaiting ci"},
		{"working", "awaiting_merge", "awaiting merge"},
		{"working", "holding", "in progress"},
		{"working", "ready", "in progress"},
		{"working", "blocked", "in progress"},
		{"stale", "ready", "no signal"},
		{"stale", "in_progress", "no signal"},
		{"next", "ready", "next up"},
		{"done", "done_locally", "done locally"},
		{"done", "delivered", "delivered"},
		{"done", "closed", "done"},
		{"none", "holding", "holding"},
		{"none", "blocked", "blocked"},
		{"none", "ready", "queued"},
		{"none", "closed", "dismissed"},
		{"none", "some_future_status", "some_future_status"},
	} {
		got := remarkFor(light{Class: tc.class, Label: "x"}, tc.status)
		if got != tc.want {
			t.Errorf("remarkFor(%s, %s) = %q, want %q (criterion 3 / B7's table; B5: the status column is GONE, so "+
				"pr_open vs in_progress survives here or nowhere)", tc.class, tc.status, got, tc.want)
		}
	}
}

// Criterion 3: every status in boardStatusOrder (plus one unknown) crossed with
// lights_test.go's factCombos, through the real lightFor — so a remark can only
// be right for lights the board can actually produce.
func TestRemarkFor_EveryLightAndStatus(t *testing.T) {
	statuses := append(append([]string{}, boardStatusOrder...), "some_future_status")
	for _, st := range statuses {
		for _, fc := range factCombos {
			l := lightFor(st, fc.f)
			name := st + "/" + fc.name + " (light " + l.Class + ")"
			got := remarkFor(l, st)
			if want := expectedRemark(l.Class, st); got != want {
				t.Errorf("%s: remarkFor = %q, want %q (criterion 3)", name, got, want)
			}
			if got == "" {
				t.Errorf("%s: remarkFor is EMPTY; every row shows words (criterion 3)", name)
			}
			if got != strings.ToLower(got) {
				t.Errorf("%s: remarkFor = %q, want lowercase — the CSS uppercases (B7)", name, got)
			}
			if strings.Contains(got, "<") || strings.Contains(got, "{") {
				t.Errorf("%s: remarkFor = %q, want plain words (B7: a Go table, not markup)", name, got)
			}
		}
	}
}

// ---- criterion 4: elapsedFor --------------------------------------------------

func TestElapsedFor(t *testing.T) {
	for _, class := range []string{"input", "working", "stale"} {
		for _, tc := range []struct {
			min  int
			want string
		}{{0, "00:00"}, {65, "01:05"}, {4334, "72:14"}, {9, "00:09"}, {600, "10:00"}} {
			if got := elapsedFor(class, tc.min); got != tc.want {
				t.Errorf("elapsedFor(%q, %d) = %q, want %q (criterion 4: zero-padded HH:MM, hours NOT capped — a "+
					"three-day stale ring reads 72:14, which is the point)", class, tc.min, got, tc.want)
			}
		}
	}
	for _, class := range []string{"done", "next", "none"} {
		for _, m := range []int{0, 1, 65, 4334} {
			if got := elapsedFor(class, m); got != "" {
				t.Errorf("elapsedFor(%q, %d) = %q, want \"\" — a done or queued row shows no clock, exactly as the "+
					"mock does (criterion 4)", class, m, got)
			}
		}
	}
	if got := elapsedFor("some_future_class", 65); got != "" {
		t.Errorf("elapsedFor(unknown class) = %q, want \"\" (criterion 4: only input, working and stale tick)", got)
	}
}

// ---- criterion 5: projectHue --------------------------------------------------

// The golden values are the mock's fold (h = (h*31 + byte) % 360) over each
// slug's BYTES, so the accepted screenshots' colours are the colours that ship.
func TestProjectHue(t *testing.T) {
	golden := []struct {
		slug string
		hue  int
	}{
		{"saka", 356},
		{"collaboratory", 115},
		{"personal", 88},
		{"bulk", 250},
		{"itest-layout-proj", 286},
		{"", 0},
	}
	seen := map[int]string{}
	distinct := 0
	for _, g := range golden {
		got := projectHue(g.slug)
		if got != g.hue {
			t.Errorf("projectHue(%q) = %d, want %d (criterion 5 / B9: h = (h*31 + b) %% 360 over the slug's bytes — "+
				"the mock's fold, so the accepted colours ship)", g.slug, got, g.hue)
		}
		if got < 0 || got >= 360 {
			t.Errorf("projectHue(%q) = %d, want [0,360) (criterion 5)", g.slug, got)
		}
		if _, dup := seen[got]; !dup {
			distinct++
		}
		seen[got] = g.slug
		if again := projectHue(g.slug); again != got {
			t.Errorf("projectHue(%q) is not deterministic: %d then %d", g.slug, got, again)
		}
	}
	// The positive control against a constant-returning implementation.
	if distinct < 2 {
		t.Errorf("every slug in the golden set folds to the same hue (%v); two different slugs must give two "+
			"different hues (criterion 5)", seen)
	}
}

// ---- criterion 6: boardTallies ------------------------------------------------

func TestBoardTallies(t *testing.T) {
	rows := func(n int) []taskRow {
		out := make([]taskRow, n)
		for i := range out {
			out[i] = taskRow{ID: int64(i + 1), Title: "T"}
		}
		return out
	}
	secs := []boardSection{
		{Key: "incoming", Tasks: rows(4)},
		{Key: "blocked", Tasks: rows(3)},
		{Key: "in_flight", Tasks: rows(2)},
		{Key: "queue", Tasks: rows(7)},
		{Key: "holding", Tasks: rows(1)},
		{Key: "done", Tasks: rows(5)},
	}
	got := boardTallies(secs)
	want := boardTally{NeedYou: 3, InFlight: 2, Incoming: 4, Queued: 8, DoneToday: 5, Open: 17}
	if got != want {
		t.Errorf("boardTallies = %+v, want %+v (criterion 6 / B16: Queued is queue+holding — one number for "+
			"'waiting to start'; Open is every row on the page minus done)", got, want)
	}

	// ?status=closed makes `other` non-empty: a dismissed row counts in Open, never
	// in DoneToday.
	withOther := append(append([]boardSection{}, secs...), boardSection{Key: "other", Tasks: rows(2)})
	gotOther := boardTallies(withOther)
	if gotOther.DoneToday != 5 {
		t.Errorf("with a non-empty `other`, DoneToday = %d, want 5: only the done section is done (criterion 6)", gotOther.DoneToday)
	}
	if gotOther.Open != 19 {
		t.Errorf("with a non-empty `other`, Open = %d, want 19 (every row minus done, `other` included) (criterion 6)", gotOther.Open)
	}
	if empty := boardTallies(nil); empty != (boardTally{}) {
		t.Errorf("boardTallies(nil) = %+v, want the zero tally (criterion 6)", empty)
	}
	// The tallies count what the board is SHOWING — i.e. after the filters (B16).
	filtered := boardTallies([]boardSection{{Key: "queue", Tasks: rows(2)}})
	if filtered.Queued != 2 || filtered.Open != 2 || filtered.NeedYou != 0 {
		t.Errorf("boardTallies over a filtered board = %+v, want Queued 2 / Open 2 / NeedYou 0 (B16)", filtered)
	}
}

// boardTally.Items() is the ACCESSOR the template ranges over (see the SPEC
// contradiction noted in board_departures_structure_test.go: the sign header
// cannot spell {{.Tally.Incoming}} without turning TestTasksTemplate_NoIncoming
// red). The six int fields of criterion 1 are unchanged; this only decides the
// order, the words and the colour classes — in Go, where B3 already puts the
// section titles.
func TestBoardTally_ItemsAreB16sFiveInOrder(t *testing.T) {
	tl := boardTally{NeedYou: 3, InFlight: 2, Incoming: 4, Queued: 8, DoneToday: 5, Open: 17}
	items := tl.Items()
	if len(items) != 5 {
		t.Fatalf("boardTally.Items() returned %d items, want the five the sign header shows (B15, B16)", len(items))
	}
	wantCounts := []int{3, 2, 4, 8, 5}
	for i, it := range items {
		if it.Count != wantCounts[i] {
			t.Errorf("item %d Count = %d, want %d (B16's order: need you, in flight, incoming, queued, done today)",
				i, it.Count, wantCounts[i])
		}
		if strings.TrimSpace(it.Label) == "" {
			t.Errorf("item %d has no Label; the WORDS come from Go — the template may not spell them (criterion 26)", i)
		}
		if it.Label != strings.ToLower(it.Label) {
			t.Errorf("item %d Label = %q, want lowercase — the CSS uppercases (the B3/B7 convention)", i, it.Label)
		}
	}
	// Open is deliberately NOT one of the five: it is the ticker's count (B15).
	for _, it := range items {
		if it.Count == tl.Open {
			t.Errorf("Items() carries Open (%d); the header shows five tallies and the ticker shows the overall "+
				"counts (B15, B16)", tl.Open)
		}
	}
}

// ---- criterion 7: projectLabel ------------------------------------------------

func TestProjectLabel(t *testing.T) {
	if got := projectLabel(""); got != "all projects" {
		t.Errorf("projectLabel(\"\") = %q, want \"all projects\" (criterion 7: the header needs no branch)", got)
	}
	if got := projectLabel("saka"); got != "saka" {
		t.Errorf("projectLabel(%q) = %q, want %q (criterion 7)", "saka", got, "saka")
	}
}
