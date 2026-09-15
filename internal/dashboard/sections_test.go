package dashboard

// board-layout-compact (SWT-57, docs/tickets/board-layout-compact_SPEC.md)
// criteria 1-5: the six light-derived board sections. ZERO I/O beyond reading
// this package's own source.
//
// IMPOSED SURFACE (SPEC criteria 1 and 5):
//
//	// internal/dashboard/sections.go (new)
//	type boardSection struct{ Key, Title string; Tasks []taskRow }
//	var boardSectionOrder // the six {Key, Title} pairs, in order
//	func sectionFor(status string, l light) string
//	func boardSections(rows []taskRow) []boardSection
//	// board.go:  taskRow gains QueueRank int, Updated string
//	// lights.go: lightFacts gains QueueRank int, UpdatedStamp string (display-only)
//
// GREENFIELD NOTE — EXPECTED RED: sections.go does not exist and taskRow /
// lightFacts lack the new fields, so package dashboard's test binary
// compile-FAILS (undefined: boardSection, boardSectionOrder, sectionFor,
// boardSections; unknown fields QueueRank, Updated, UpdatedStamp).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - sectionFor maps by status only (ready → queue whatever the light) → NamedCases, AgreesWithLightFor.
//   - a row appended to two sections → Partition.
//   - queue sorted by id instead of QueueRank → RanksOutOfIDOrder, MixedBoard, Partition.
//   - lightFor reads QueueRank → DisplayOnlyFieldsNeverFeedTheLight.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Criterion 1 / 5: the types and the new fields.
var _ = boardSection{Key: "queue", Title: "queue", Tasks: []taskRow{{QueueRank: 1, Updated: "10:05"}}}
var _ = lightFacts{QueueRank: 1, UpdatedStamp: "10:05"}

// The six section keys, in SPEC L2 order.
var sectionKeys = []string{"blocked", "in_flight", "queue", "holding", "done", "other"}

func sectionIndex(key string) int {
	for i, k := range sectionKeys {
		if k == key {
			return i
		}
	}
	return -1
}

// ---- criterion 1: the declarations, the order, the purity -----------------------

// AMENDED deliberately by board-incoming-first (SWT-59,
// docs/tickets/board-incoming-first_SPEC.md, I3): the SPEC's ONE amendment of an
// existing test. boardSectionOrder gains incoming/incoming as its FIRST pair,
// above blocked, so the six pairs become seven; the other six are unchanged.
func TestBoardSectionOrder_SevenPairsInOrder(t *testing.T) {
	want := "incoming/incoming, blocked/blocked, in_flight/in flight, queue/queue, holding/holding, done/done, other/other"
	var got []string
	for _, s := range boardSectionOrder {
		got = append(got, s.Key+"/"+s.Title)
	}
	if strings.Join(got, ", ") != want {
		t.Errorf("boardSectionOrder = [%s], want [%s] (criterion 1, L2's section order)", strings.Join(got, ", "), want)
	}
}

func TestSections_PureNoIONoClock(t *testing.T) {
	b, err := os.ReadFile("sections.go")
	if err != nil {
		t.Fatalf("read internal/dashboard/sections.go: %v. Criterion 1: the sections live in their own file", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sections.go", b, 0)
	if err != nil {
		t.Fatalf("parse sections.go: %v", err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		switch {
		case p == "time", p == "context", p == "net/http", p == "os", p == "database/sql", strings.Contains(p, "pgx"):
			t.Errorf("sections.go imports %q. Criterion 1: sectionFor and boardSections are pure — no I/O, no clock", p)
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
	for name, kind := range map[string]string{"boardSection": "type", "boardSectionOrder": "var",
		"sectionFor": "func", "boardSections": "func"} {
		if declared[name] != kind {
			t.Errorf("sections.go does not declare %s %s (criterion 1)", kind, name)
		}
	}
	for _, fn := range []string{"sectionFor", "boardSections"} {
		body := funcBodySrc(t, "sections.go", fn)
		if body == "" {
			continue // reported above
		}
		for _, banned := range []string{"s.pool", "Query(", "Exec(", "time."} {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q: it must be a pure function (criterion 1)", fn, banned)
			}
		}
	}
}

// ---- criterion 2: L1's named cases ------------------------------------------------

func TestSectionFor_NamedCases(t *testing.T) {
	cases := []struct {
		name, status string
		f            lightFacts
		class        string // CONTROL: lightFor's class for these facts
		section      string
	}{
		{"needs_feedback", "needs_feedback", lightFacts{}, "input", "blocked"},
		{"holding + needs_input", "holding", lightFacts{State: "needs_input", StateAt: stateAt}, "input", "blocked"},
		{"ready + needs_input + QueueHead", "ready",
			lightFacts{State: "needs_input", StateAt: stateAt, QueueHead: true, Lane: "saka"}, "input", "blocked"},
		{"blocked, no marker", "blocked", lightFacts{}, "none", "blocked"},
		{"blocked + fresh working", "blocked", lightFacts{State: "working", StateAt: stateAt}, "working", "in_flight"},
		{"ready + stale working", "ready", lightFacts{State: "working", StateAt: stateAt, Stale: true}, "stale", "in_flight"},
		{"ready + fresh working + QueueHead", "ready",
			lightFacts{State: "working", StateAt: stateAt, QueueHead: true, Lane: "saka"}, "working", "in_flight"},
		{"claimed", "claimed", lightFacts{}, "working", "in_flight"},
		{"in_progress", "in_progress", lightFacts{}, "working", "in_flight"},
		{"pr_open", "pr_open", lightFacts{}, "working", "in_flight"},
		{"awaiting_ci", "awaiting_ci", lightFacts{}, "working", "in_flight"},
		{"awaiting_merge", "awaiting_merge", lightFacts{}, "working", "in_flight"},
		{"ready + QueueHead", "ready", lightFacts{QueueHead: true, Lane: "saka"}, "next", "queue"},
		{"ready, no head", "ready", lightFacts{}, "none", "queue"},
		{"holding", "holding", lightFacts{}, "none", "holding"},
		{"closed today", "closed", lightFacts{ClosedToday: true}, "done", "done"},
		{"done_locally", "done_locally", lightFacts{}, "done", "done"},
		{"delivered", "delivered", lightFacts{}, "done", "done"},
		{"closed + open dismissal", "closed", lightFacts{OpenDismissalCode: "duplicate", ClosedToday: true}, "none", "other"},
		{"some_future_status", "some_future_status", lightFacts{}, "none", "other"},
	}
	for _, tc := range cases {
		l := lightFor(tc.status, tc.f)
		if l.Class != tc.class {
			t.Fatalf("CONTROL %s: lightFor class = %q, want %q — the fixture no longer reaches the L1 row it names", tc.name, l.Class, tc.class)
		}
		if got := sectionFor(tc.status, l); got != tc.section {
			t.Errorf("sectionFor(%q, %s light) = %q, want %q (criterion 2, L1: %s)", tc.status, l.Class, got, tc.section, tc.name)
		}
	}
}

// ---- criterion 3: agreement with lightFor by construction ---------------------------

// expectedSection is L1's table, written independently of sectionFor: the class
// decides, and the status is read ONLY for the grey ring.
func expectedSection(status, class string) string {
	switch class {
	case "input":
		return "blocked"
	case "working", "stale":
		return "in_flight"
	case "next":
		return "queue"
	case "done":
		return "done"
	}
	switch status { // class none
	case "blocked":
		return "blocked"
	case "ready":
		return "queue"
	case "holding":
		return "holding"
	}
	return "other"
}

func TestSectionFor_AgreesWithLightForEverywhere(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	if len(statuses) != 13 {
		t.Fatalf("POSITIVE CONTROL: boardStatusOrder has %d statuses, want the 12 of the 0001 CHECK", len(boardStatusOrder))
	}
	readySeen := map[string]bool{}
	for _, st := range statuses {
		for _, fc := range factCombos {
			l := lightFor(st, fc.f)
			got := sectionFor(st, l)
			if sectionIndex(got) < 0 {
				t.Errorf("%s/%s: sectionFor = %q, not one of the six keys %v (criterion 3)", st, fc.name, got, sectionKeys)
				continue
			}
			if want := expectedSection(st, l.Class); got != want {
				t.Errorf("%s/%s: light %s, sectionFor = %q, want %q (criterion 3: the section agrees with the light)",
					st, fc.name, l.Class, got, want)
			}
			if st == "ready" {
				readySeen[got] = true
			}
		}
	}
	// A status-only grouping puts every ready row in one section; the facts put
	// ready rows in three (L1 "Why the light, not the status").
	for _, k := range []string{"blocked", "in_flight", "queue"} {
		if !readySeen[k] {
			t.Errorf("no ready row landed in %s across factCombos; L1 puts a red ready row in blocked and a yellow one in "+
				"in_flight — sectionFor must not decide by status alone", k)
		}
	}
}

// ---- criterion 4: boardSections -------------------------------------------------

func secRow(id int64, status, class string, rank int) taskRow {
	return taskRow{ID: id, Status: status, Light: light{Class: class}, QueueRank: rank}
}

func sectionsString(secs []boardSection) string {
	var parts []string
	for _, s := range secs {
		ids := make([]string, 0, len(s.Tasks))
		for _, r := range s.Tasks {
			ids = append(ids, strconv.FormatInt(r.ID, 10))
		}
		parts = append(parts, s.Key+":"+strings.Join(ids, ","))
	}
	return strings.Join(parts, " | ")
}

var l2LightRank = map[string]int{"input": 0, "stale": 1, "working": 2, "next": 3, "done": 4, "none": 5}

func l2StatusPos(st string) int {
	for i, s := range boardStatusOrder {
		if s == st {
			return i
		}
	}
	return len(boardStatusOrder) // unknown statuses sort last
}

// l2Less is L2's within-section order, written independently of boardSections.
func l2Less(key string, a, b taskRow) bool {
	if key == "queue" {
		if (a.QueueRank == 0) != (b.QueueRank == 0) {
			return b.QueueRank == 0 // ranked rows before unranked ones
		}
		if a.QueueRank != b.QueueRank {
			return a.QueueRank < b.QueueRank
		}
		return a.ID < b.ID
	}
	if ra, rb := l2LightRank[a.Light.Class], l2LightRank[b.Light.Class]; ra != rb {
		return ra < rb
	}
	if pa, pb := l2StatusPos(a.Status), l2StatusPos(b.Status); pa != pb {
		return pa < pb
	}
	return a.ID < b.ID
}

func assertSections(t *testing.T, name string, got []boardSection, want string) {
	t.Helper()
	if s := sectionsString(got); s != want {
		t.Errorf("%s: boardSections = %q, want %q (criterion 4)", name, s, want)
	}
}

func TestBoardSections_MixedBoard(t *testing.T) {
	rows := []taskRow{
		secRow(1, "ready", "none", 3),
		secRow(2, "holding", "none", 0),
		secRow(3, "blocked", "none", 0),
		secRow(4, "needs_feedback", "input", 0),
		secRow(5, "in_progress", "working", 0),
		secRow(6, "ready", "next", 1),
		secRow(7, "closed", "done", 0),
		secRow(8, "closed", "none", 0), // dismissed
		secRow(9, "ready", "stale", 4),
		secRow(10, "claimed", "working", 0),
		secRow(11, "some_future_status", "none", 0),
		secRow(12, "holding", "input", 0),
		secRow(13, "ready", "none", 2),
		secRow(14, "done_locally", "done", 0),
		secRow(15, "blocked", "working", 0),
	}
	got := boardSections(rows)
	assertSections(t, "mixed board", got,
		"blocked:12,4,3 | in_flight:9,15,10,5 | queue:6,13,1 | holding:2 | done:14,7 | other:8,11")
	titles := map[string]string{}
	for _, s := range boardSectionOrder {
		titles[s.Key] = s.Title
	}
	for _, s := range got {
		if s.Title != titles[s.Key] {
			t.Errorf("section %s has title %q, want %q from boardSectionOrder", s.Key, s.Title, titles[s.Key])
		}
	}
}

func TestBoardSections_RanksOutOfIDOrder(t *testing.T) {
	rows := []taskRow{
		secRow(3, "ready", "none", 0),
		secRow(5, "ready", "none", 2),
		secRow(7, "ready", "none", 0),
		secRow(10, "ready", "next", 1), // rank 1 on the HIGHEST id
	}
	assertSections(t, "ranks out of id order", boardSections(rows), "queue:10,5,3,7")
}

func TestBoardSections_RedBeforeGreyInBlocked(t *testing.T) {
	rows := []taskRow{
		secRow(5, "blocked", "none", 0),
		secRow(20, "blocked", "input", 0),
		secRow(30, "needs_feedback", "input", 0),
	}
	assertSections(t, "red and grey in blocked", boardSections(rows), "blocked:20,30,5")
}

func TestBoardSections_StaleBeforeWorking(t *testing.T) {
	rows := []taskRow{
		secRow(1, "ready", "working", 1),
		secRow(2, "ready", "stale", 2),
		secRow(3, "awaiting_merge", "working", 0),
		secRow(4, "claimed", "working", 0),
	}
	assertSections(t, "stale before working", boardSections(rows), "in_flight:2,1,4,3")
}

func TestBoardSections_EmptyInputIsNil(t *testing.T) {
	if got := boardSections(nil); got != nil {
		t.Errorf("boardSections(nil) = %v, want nil (criterion 4)", got)
	}
	if got := boardSections([]taskRow{}); got != nil {
		t.Errorf("boardSections([]taskRow{}) = %v, want nil (criterion 4)", got)
	}
}

func TestBoardSections_SingleSection(t *testing.T) {
	got := boardSections([]taskRow{secRow(4, "holding", "none", 0), secRow(2, "holding", "none", 0)})
	if len(got) != 1 {
		t.Fatalf("boardSections over two holding rows = %d sections, want 1 (empty sections are not rendered)", len(got))
	}
	assertSections(t, "single section", got, "holding:2,4")
}

// The partition and the orders, over every status x factCombos, fed in id order
// and reversed (boardSections must sort, not trust the input order).
func TestBoardSections_Partition(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	var rows []taskRow
	id, ready := int64(0), 0
	for _, st := range statuses {
		for _, fc := range factCombos {
			id++
			r := taskRow{ID: id, Status: st, Light: lightFor(st, fc.f)}
			if st == "ready" { // ranks descend as ids ascend; every third row is unranked
				if ready%3 != 2 {
					r.QueueRank = 100 - ready
				}
				ready++
			}
			rows = append(rows, r)
		}
	}
	reversed := make([]taskRow, len(rows))
	for i, r := range rows {
		reversed[len(rows)-1-i] = r
	}
	for _, in := range []struct {
		name string
		rows []taskRow
	}{{"id order", rows}, {"reversed", reversed}} {
		got := boardSections(in.rows)
		seen := map[int64]int{}
		prev := -1
		for _, s := range got {
			i := sectionIndex(s.Key)
			if i < 0 {
				t.Errorf("%s: unknown section key %q", in.name, s.Key)
				continue
			}
			if i <= prev {
				t.Errorf("%s: section %s out of boardSectionOrder (got %s)", in.name, s.Key, sectionsString(got))
			}
			prev = i
			if len(s.Tasks) == 0 {
				t.Errorf("%s: section %s is empty; empty sections are omitted (criterion 4)", in.name, s.Key)
			}
			for j, r := range s.Tasks {
				seen[r.ID]++
				if want := sectionFor(r.Status, r.Light); want != s.Key {
					t.Errorf("%s: task %d (%s, %s) is in %s, sectionFor says %s", in.name, r.ID, r.Status, r.Light.Class, s.Key, want)
				}
				if j > 0 && !l2Less(s.Key, s.Tasks[j-1], r) {
					t.Errorf("%s: in %s, task %d (%s, %s, rank %d) precedes task %d (%s, %s, rank %d) — against L2's order",
						in.name, s.Key, s.Tasks[j-1].ID, s.Tasks[j-1].Status, s.Tasks[j-1].Light.Class, s.Tasks[j-1].QueueRank,
						r.ID, r.Status, r.Light.Class, r.QueueRank)
				}
			}
		}
		if len(seen) != len(rows) {
			t.Errorf("%s: %d distinct ids returned, want %d (criterion 4: every row in exactly one section)", in.name, len(seen), len(rows))
		}
		for _, r := range rows {
			if seen[r.ID] != 1 {
				t.Errorf("%s: task %d (%s, %s) appears %d times, want exactly once (criterion 4)", in.name, r.ID, r.Status, r.Light.Class, seen[r.ID])
			}
		}
	}
}

// ---- criterion 5: the display-only facts ----------------------------------------

func TestLightFacts_DisplayOnlyFieldsNeverFeedTheLight(t *testing.T) {
	body := funcBodySrc(t, "lights.go", "lightFor")
	if body == "" {
		t.Fatalf("lights.go declares no lightFor")
	}
	for _, f := range []string{"QueueRank", "UpdatedStamp"} {
		if strings.Contains(body, f) {
			t.Errorf("lightFor's body mentions %s. Criterion 5: it is a display-only fact; the light never reads it", f)
		}
	}
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	for _, st := range statuses {
		for _, fc := range factCombos {
			g := fc.f
			g.QueueRank, g.UpdatedStamp = 3, "10:05"
			if a, b := lightFor(st, fc.f), lightFor(st, g); a != b {
				t.Errorf("%s/%s: lightFor changes with QueueRank/UpdatedStamp (%+v vs %+v); criterion 5", st, fc.name, a, b)
			}
		}
	}

	// Both new fields are documented as display-only.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lights.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse lights.go: %v", err)
	}
	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && ts.Name.Name == "lightFacts" {
			st, _ = ts.Type.(*ast.StructType)
			return false
		}
		return true
	})
	if st == nil {
		t.Fatalf("lights.go declares no lightFacts struct")
	}
	var structComments []string
	for _, cg := range f.Comments {
		if cg.Pos() >= st.Pos() && cg.End() <= st.End() {
			structComments = append(structComments, cg.Text())
		}
	}
	for _, name := range []string{"QueueRank", "UpdatedStamp"} {
		found, documented := false, false
		for _, fl := range st.Fields.List {
			for _, nm := range fl.Names {
				if nm.Name != name {
					continue
				}
				found = true
				for _, cg := range []*ast.CommentGroup{fl.Doc, fl.Comment} {
					if cg != nil && strings.Contains(strings.ToLower(cg.Text()), "display") {
						documented = true
					}
				}
			}
		}
		for _, c := range structComments {
			if strings.Contains(c, name) && strings.Contains(strings.ToLower(c), "display") {
				documented = true
			}
		}
		if !found {
			t.Errorf("lightFacts has no %s field (criterion 5)", name)
		} else if !documented {
			t.Errorf("lightFacts.%s is not documented as display-only (criterion 5)", name)
		}
	}
}
