package dashboard

// board-incoming-first (SWT-59, docs/tickets/board-incoming-first_SPEC.md)
// criteria 2-4 and 7-10: the INCOMING section, pure. ZERO I/O. (Criterion 1,
// the seven-pair order, is the amended TestBoardSectionOrder_SevenPairsInOrder
// in sections_test.go; the structure halves of 2, 4, 11-14 live in
// board_incoming_structure_test.go.)
//
// IMPOSED SURFACE (greenfield: the SPEC's contract defines these signatures):
//
//	// internal/dashboard/sections.go
//	const incomingMessage = "message"
//	const incomingPRReview = "pr_review"
//	func incomingKind(fromMessage, prReview bool) string
//	func boardSectionOf(r taskRow) string
//	// board.go:  taskRow gains Incoming string (board-only, never an export column)
//	// lights.go: lightFacts gains FromMessage, PRReview bool (display-only)
//
// GREENFIELD NOTE, EXPECTED RED: none of the above exists, so package
// dashboard's test binary compile-FAILS (undefined: incomingMessage,
// incomingPRReview, incomingKind, boardSectionOf; unknown fields Incoming,
// FromMessage, PRReview).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - boardSections calls sectionFor instead of boardSectionOf -> IncomingFirstAndOrdered, IncomingPartition.
//   - the Light.Class != "done" / Status != "closed" guard dropped -> NamedCases, AgreesWithSectionFor, IncomingClosedIsDone.
//   - incoming sorted by id ASC -> MessagesBeforePRs, IDDescNoStatusTiebreak, IncomingFirstAndOrdered.
//   - PRs before messages -> MessagesBeforePRs, IncomingFirstAndOrdered.
//   - incoming after blocked in boardSectionOrder -> IncomingFirstAndOrdered, IncomingPartition.

import (
	"strings"
	"testing"
)

// Criteria 2 and 4: the new fields.
var _ = taskRow{Incoming: incomingMessage}
var _ = lightFacts{FromMessage: true, PRReview: true}

// incomingSectionKeys is I3's order: incoming first, then SWT-57's six
// (sectionKeys, unchanged, because sectionFor never returns incoming).
var incomingSectionKeys = append([]string{"incoming"}, sectionKeys...)

func incomingSectionIndex(key string) int {
	for i, k := range incomingSectionKeys {
		if k == key {
			return i
		}
	}
	return -1
}

// expectedSectionOf is I4's rule, written independently of boardSectionOf:
// incoming iff an incoming kind is set, the task is not closed and its light is
// not green; otherwise L1's table.
func expectedSectionOf(status, class, kind string) string {
	if kind != "" && status != "closed" && class != "done" {
		return "incoming"
	}
	return expectedSection(status, class)
}

var i5KindRank = map[string]int{"message": 0, "pr_review": 1}

// i5Less is I5's order within incoming, written independently of boardSections:
// light rank, then kind (message before pr_review), then id DESC. No status
// tiebreak.
func i5Less(a, b taskRow) bool {
	if ra, rb := l2LightRank[a.Light.Class], l2LightRank[b.Light.Class]; ra != rb {
		return ra < rb
	}
	if ka, kb := i5KindRank[a.Incoming], i5KindRank[b.Incoming]; ka != kb {
		return ka < kb
	}
	return a.ID > b.ID
}

func incRow(id int64, status, class, kind string, rank int) taskRow {
	return taskRow{ID: id, Status: status, Light: light{Class: class}, QueueRank: rank, Incoming: kind}
}

// ---- criterion 9: incomingKind --------------------------------------------------------

func TestIncomingKind_TruthTable(t *testing.T) {
	if incomingMessage != "message" || incomingPRReview != "pr_review" {
		t.Fatalf("incomingMessage = %q, incomingPRReview = %q, want \"message\" and \"pr_review\" (criterion 2)",
			incomingMessage, incomingPRReview)
	}
	for _, tc := range []struct {
		fromMessage, prReview bool
		want                  string
	}{
		{false, false, ""},
		{true, false, "message"},
		{false, true, "pr_review"},
		{true, true, "message"}, // message wins when both are set
	} {
		if got := incomingKind(tc.fromMessage, tc.prReview); got != tc.want {
			t.Errorf("incomingKind(%v, %v) = %q, want %q (criterion 9)", tc.fromMessage, tc.prReview, got, tc.want)
		}
	}
}

// ---- criterion 7: boardSectionOf's named cases -----------------------------------------

func TestBoardSectionOf_NamedCases(t *testing.T) {
	cases := []struct {
		name, status string
		f            lightFacts
		kind         string
		class        string // CONTROL: lightFor's class for these facts
		section      string
	}{
		{"ready, message", "ready", lightFacts{}, incomingMessage, "none", "incoming"},
		{"ready + QueueHead, pr_review (stays blue)", "ready", lightFacts{QueueHead: true, Lane: "saka"}, incomingPRReview, "next", "incoming"},
		{"holding + needs_input, message (stays red)", "holding", lightFacts{State: "needs_input", StateAt: stateAt}, incomingMessage, "input", "incoming"},
		{"blocked, message", "blocked", lightFacts{}, incomingMessage, "none", "incoming"},
		{"ready + fresh working, pr_review", "ready", lightFacts{State: "working", StateAt: stateAt}, incomingPRReview, "working", "incoming"},
		{"ready + stale working, message", "ready", lightFacts{State: "working", StateAt: stateAt, Stale: true}, incomingMessage, "stale", "incoming"},
		{"closed today, message", "closed", lightFacts{ClosedToday: true}, incomingMessage, "done", "done"},
		{"closed + open dismissal, message", "closed", lightFacts{OpenDismissalCode: "duplicate", ClosedToday: true}, incomingMessage, "none", "other"},
		{"done_locally, pr_review", "done_locally", lightFacts{}, incomingPRReview, "done", "done"},
		{"delivered, pr_review", "delivered", lightFacts{}, incomingPRReview, "done", "done"},
		{"ready, not incoming", "ready", lightFacts{}, "", "none", "queue"},
	}
	for i, tc := range cases {
		f := tc.f
		f.FromMessage, f.PRReview = tc.kind == incomingMessage, tc.kind == incomingPRReview
		l := lightFor(tc.status, f)
		if l.Class != tc.class {
			t.Fatalf("CONTROL %s: lightFor class = %q, want %q; the fixture no longer reaches the row it names", tc.name, l.Class, tc.class)
		}
		r := taskRow{ID: int64(i + 1), Status: tc.status, Light: l, Incoming: tc.kind}
		if got := boardSectionOf(r); got != tc.section {
			t.Errorf("boardSectionOf(%s, %s light, Incoming %q) = %q, want %q (criterion 7, I4: %s)",
				tc.status, l.Class, tc.kind, got, tc.section, tc.name)
		}
		if r.Light != l {
			t.Errorf("%s: the row's light changed to %+v; incoming keeps each row's own light (I4)", tc.name, r.Light)
		}
	}
}

// ---- criteria 4 and 8: agreement ------------------------------------------------------

func TestBoardSectionOf_AgreesWithSectionForEverywhere(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	took := map[string]bool{} // the SWT-57 sections incoming took rows from
	for _, st := range statuses {
		for _, fc := range factCombos {
			l := lightFor(st, fc.f)
			flipped := fc.f
			flipped.FromMessage, flipped.PRReview = true, true
			if lf := lightFor(st, flipped); lf != l {
				t.Errorf("%s/%s: lightFor changes with FromMessage/PRReview (%+v vs %+v); criterion 4: they are display-only",
					st, fc.name, l, lf)
			}
			plain := taskRow{ID: 1, Status: st, Light: l}
			if got, want := boardSectionOf(plain), sectionFor(st, l); got != want {
				t.Errorf("%s/%s: Incoming \"\": boardSectionOf = %q, sectionFor = %q; with no incoming kind they agree everywhere (criterion 8)",
					st, fc.name, got, want)
			}
			for _, kind := range []string{incomingMessage, incomingPRReview} {
				r := taskRow{ID: 1, Status: st, Light: l, Incoming: kind}
				got := boardSectionOf(r)
				if want := expectedSectionOf(st, l.Class, kind); got != want {
					t.Errorf("%s/%s: Incoming %q, light %s: boardSectionOf = %q, want %q (criterion 8: incoming iff status != closed "+
						"and class != done, otherwise sectionFor)", st, fc.name, kind, l.Class, got, want)
				}
				if got == "incoming" {
					took[sectionFor(st, l)] = true
				}
			}
		}
	}
	// I4: incoming outranks every light but green, so across the table it takes
	// rows from every SWT-57 section except done.
	for _, k := range []string{"blocked", "in_flight", "queue", "holding", "other"} {
		if !took[k] {
			t.Errorf("no row that sectionFor puts in %s landed in incoming; I4 says incoming outranks every light except green", k)
		}
	}
	if took["done"] {
		t.Errorf("a green row landed in incoming; a finished task is not waiting (I4, I8)")
	}
}

// ---- criterion 10: boardSections with incoming rows -------------------------------------

func TestBoardSections_IncomingFirstAndOrdered(t *testing.T) {
	rows := []taskRow{
		incRow(1, "ready", "none", incomingMessage, 3),
		incRow(2, "ready", "none", incomingPRReview, 2),
		incRow(3, "holding", "input", incomingPRReview, 0), // red PR
		incRow(4, "blocked", "none", incomingMessage, 0),
		incRow(5, "ready", "next", "", 1),
		incRow(6, "ready", "none", "", 4),
		incRow(7, "blocked", "none", "", 0),
		incRow(8, "closed", "done", incomingMessage, 0),
		incRow(9, "in_progress", "working", "", 0),
		incRow(10, "ready", "working", incomingMessage, 5),
		incRow(11, "ready", "stale", incomingPRReview, 0),
		incRow(12, "closed", "none", incomingMessage, 0), // dismissed
		incRow(13, "delivered", "done", incomingPRReview, 0),
		incRow(14, "needs_feedback", "input", "", 0),
		incRow(15, "ready", "none", incomingMessage, 0),
		incRow(16, "ready", "none", incomingPRReview, 0),
	}
	got := boardSections(rows)
	if s := sectionsString(got); s != "incoming:3,11,10,15,4,1,16,2 | blocked:14,7 | in_flight:9 | queue:5,6 | done:13,8 | other:12" {
		t.Errorf("boardSections = %q,\nwant %q\n(criterion 10: incoming first; light rank, then messages before PRs, then id DESC; "+
			"every other section keeps L2)", s, "incoming:3,11,10,15,4,1,16,2 | blocked:14,7 | in_flight:9 | queue:5,6 | done:13,8 | other:12")
	}
	if len(got) == 0 || got[0].Key != "incoming" || got[0].Title != "incoming" {
		t.Fatalf("the first section is not {incoming, incoming} (criterion 10, I3)")
	}
	for _, r := range got[0].Tasks {
		if r.ID == 3 && r.Light.Class != "input" {
			t.Errorf("the red incoming row renders light %q; incoming keeps each row's light (I4)", r.Light.Class)
		}
	}
}

func TestBoardSections_IncomingRedPRBeforeGreyMessage(t *testing.T) {
	rows := []taskRow{
		incRow(5, "ready", "none", incomingMessage, 0),
		incRow(9, "holding", "input", incomingPRReview, 0),
	}
	assertSections(t, "red pr_review, grey message", boardSections(rows), "incoming:9,5")
}

func TestBoardSections_IncomingMessagesBeforePRs(t *testing.T) {
	rows := []taskRow{
		incRow(1, "ready", "none", incomingPRReview, 0),
		incRow(2, "ready", "none", incomingMessage, 0),
		incRow(3, "ready", "none", incomingPRReview, 0),
		incRow(4, "ready", "none", incomingMessage, 0),
	}
	assertSections(t, "grey messages before grey PRs, id DESC within a kind", boardSections(rows), "incoming:4,2,3,1")
}

// I5: the status tiebreak of L2 is deliberately NOT used in incoming; L2 would
// put holding (10) before ready (20) before blocked (30).
func TestBoardSections_IncomingIDDescNoStatusTiebreak(t *testing.T) {
	rows := []taskRow{
		incRow(10, "holding", "none", incomingMessage, 0),
		incRow(20, "ready", "none", incomingMessage, 0),
		incRow(30, "blocked", "none", incomingMessage, 0),
	}
	assertSections(t, "one kind, one light, three statuses", boardSections(rows), "incoming:30,20,10")
}

func TestBoardSections_IncomingClosedIsDone(t *testing.T) {
	rows := []taskRow{
		incRow(6, "closed", "none", incomingMessage, 0), // dismissed, under ?status=closed
		incRow(7, "closed", "done", incomingMessage, 0),
		incRow(8, "ready", "none", incomingMessage, 0),
		incRow(9, "delivered", "done", incomingPRReview, 0),
	}
	assertSections(t, "finished incoming rows", boardSections(rows), "incoming:8 | done:9,7 | other:6")
}

// A board with no incoming rows yields exactly SWT-57's sections: the rows and
// the string of TestBoardSections_MixedBoard, unchanged.
func TestBoardSections_NoIncomingRowsIsSWT57Unchanged(t *testing.T) {
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
	assertSections(t, "no incoming rows", got,
		"blocked:12,4,3 | in_flight:9,15,10,5 | queue:6,13,1 | holding:2 | done:14,7 | other:8,11")
	for _, s := range got {
		if s.Key == "incoming" {
			t.Errorf("a board with no incoming kind renders an incoming section; empty sections are omitted")
		}
	}
}

// The partition and the orders over every status x factCombos x {none, message,
// pr_review}, fed in id order and reversed.
func TestBoardSections_IncomingPartition(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	var rows []taskRow
	id, ready := int64(0), 0
	for _, st := range statuses {
		for _, fc := range factCombos {
			for _, kind := range []string{"", incomingMessage, incomingPRReview} {
				id++
				f := fc.f
				f.FromMessage, f.PRReview = kind == incomingMessage, kind == incomingPRReview
				r := taskRow{ID: id, Status: st, Light: lightFor(st, f), Incoming: kind}
				if st == "ready" { // ranks descend as ids ascend; every third row is unranked
					if ready%3 != 2 {
						r.QueueRank = 1000 - ready
					}
					ready++
				}
				rows = append(rows, r)
			}
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
		sawIncoming := false
		for _, s := range got {
			i := incomingSectionIndex(s.Key)
			if i < 0 {
				t.Errorf("%s: unknown section key %q", in.name, s.Key)
				continue
			}
			if i <= prev {
				t.Errorf("%s: section %s out of boardSectionOrder (got %s)", in.name, s.Key, sectionsString(got))
			}
			prev = i
			if len(s.Tasks) == 0 {
				t.Errorf("%s: section %s is empty; empty sections are omitted", in.name, s.Key)
			}
			if s.Key == "incoming" {
				sawIncoming = true
			}
			for j, r := range s.Tasks {
				seen[r.ID]++
				if want := expectedSectionOf(r.Status, r.Light.Class, r.Incoming); want != s.Key {
					t.Errorf("%s: task %d (%s, %s, Incoming %q) is in %s, I4 says %s", in.name, r.ID, r.Status, r.Light.Class, r.Incoming, s.Key, want)
				}
				if j == 0 {
					continue
				}
				a := s.Tasks[j-1]
				if s.Key == "incoming" {
					if !i5Less(a, r) {
						t.Errorf("%s: in incoming, task %d (%s, %s) precedes task %d (%s, %s): against I5's order",
							in.name, a.ID, a.Light.Class, a.Incoming, r.ID, r.Light.Class, r.Incoming)
					}
				} else if !l2Less(s.Key, a, r) {
					t.Errorf("%s: in %s, task %d (%s, %s, rank %d) precedes task %d (%s, %s, rank %d): against L2's order",
						in.name, s.Key, a.ID, a.Status, a.Light.Class, a.QueueRank, r.ID, r.Status, r.Light.Class, r.QueueRank)
				}
			}
		}
		if !sawIncoming {
			t.Errorf("%s: no incoming section over a board with incoming rows (got %s)", in.name, sectionsString(got))
		}
		if len(seen) != len(rows) {
			t.Errorf("%s: %d distinct ids returned, want %d (criterion 10: every row in exactly one section)", in.name, len(seen), len(rows))
		}
		for _, r := range rows {
			if seen[r.ID] != 1 {
				t.Errorf("%s: task %d (%s, %s, Incoming %q) appears %d times, want exactly once", in.name, r.ID, r.Status,
					r.Light.Class, r.Incoming, seen[r.ID])
			}
		}
	}
	if strings.Join(incomingSectionKeys, ",") != "incoming,blocked,in_flight,queue,holding,done,other" {
		t.Fatalf("CONTROL: incomingSectionKeys = %v", incomingSectionKeys)
	}
}
