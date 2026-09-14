package dashboard

// SWT-52 (docs/tickets/board-status-lights_SPEC.md) criteria 1-4: the light is a
// PURE Go function of (status, facts), and "next in queue" is a pure pick over
// candidates in queue order. ZERO I/O beyond reading this package's own source.
//
// IMPOSED SURFACE (SPEC criteria 1 and 4; headCandidate's fields are this file's
// choice — the SPEC names the type but not its fields):
//
//	// internal/dashboard/lights.go
//	type light struct{ Class, Label string }
//	type lightFacts struct {
//	    OpenDismissalCode string
//	    ClosedToday       bool
//	    State, StateAt    string // StateAt is "YYYY-MM-DD HH:MM" in BoardTimeZone
//	    Stale, QueueHead  bool
//	    Lane              string
//	}
//	func lightFor(status string, f lightFacts) light
//	type headCandidate struct {
//	    ID           int64
//	    AssigneeType string // "human" | "claude"
//	    ProjectID    int64
//	    ProjectSlug  string
//	    Client       string
//	    Subproject   string
//	}
//	func pickQueueHeads(cands []headCandidate, eligible func(int64) bool) map[int64]string
//
// StateAt's format is resolved here: lightFacts carries ONE time string, and the
// labels need HH:MM (fresh working, needs_input) and YYYY-MM-DD HH:MM (stale).
// So StateAt is the longer form (to_char(..., 'YYYY-MM-DD HH24:MI')) and lightFor
// shows its HH:MM part in the fresh labels.
//
// GREENFIELD NOTE — EXPECTED RED: lights.go does not exist, so package
// dashboard's test binary compile-FAILS (undefined: light, lightFacts, lightFor,
// headCandidate, pickQueueHeads).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The six classes (criterion 2). Nothing else may ever appear.
var lightClasses = map[string]bool{"done": true, "working": true, "stale": true, "input": true, "next": true, "none": true}

// Every FIXED label prefix in D1's table. A row's expected prefix must be the
// LONGEST of these that the label starts with, so "in progress" cannot pass for
// "in progress (claimed)" or the other way round.
var fixedLabelPrefixes = []string{
	"dismissed (", "done today", "done",
	"waiting on your input: worker parked on a question",
	"done locally; delivery pending", "delivered",
	"in progress (claimed)", "in progress", "in progress: PR open", "in progress: awaiting CI",
	"in progress: awaiting merge",
	"waiting on your input (a session, since ", "in progress (a session, last signal ",
	"in progress? no session signal since ",
	"next in queue (",
	"holding (review lane; not queued)", "blocked on a dependency", "ready, queued",
}

func assertLight(t *testing.T, name string, got light, wantClass, wantPrefix string) {
	t.Helper()
	if !lightClasses[got.Class] {
		t.Errorf("%s: class %q is not one of the six (done, working, stale, input, next, none) — criterion 2", name, got.Class)
	}
	if got.Class != wantClass {
		t.Errorf("%s: class = %q (label %q), want %q (D1)", name, got.Class, got.Label, wantClass)
	}
	if !strings.HasPrefix(got.Label, wantPrefix) {
		t.Errorf("%s: label = %q, want it to start with %q (D1)", name, got.Label, wantPrefix)
		return
	}
	for _, p := range fixedLabelPrefixes {
		if len(p) > len(wantPrefix) && strings.HasPrefix(p, wantPrefix) && strings.HasPrefix(got.Label, p) {
			t.Errorf("%s: label = %q reads as the D1 row %q, not %q", name, got.Label, p, wantPrefix)
		}
	}
}

const stateAt = "2026-09-14 10:05"

// fact combinations that reach distinct D1 rows.
var factCombos = []struct {
	name string
	f    lightFacts
}{
	{"no facts", lightFacts{}},
	{"working fresh", lightFacts{State: "working", StateAt: stateAt}},
	{"working stale", lightFacts{State: "working", StateAt: stateAt, Stale: true}},
	{"needs_input", lightFacts{State: "needs_input", StateAt: stateAt}},
	{"needs_input stale flag", lightFacts{State: "needs_input", StateAt: stateAt, Stale: true}},
	{"queue head", lightFacts{QueueHead: true, Lane: "saka"}},
	{"queue head + working", lightFacts{QueueHead: true, Lane: "saka", State: "working", StateAt: stateAt}},
	{"open dismissal", lightFacts{OpenDismissalCode: "duplicate"}},
	{"open dismissal + closed today", lightFacts{OpenDismissalCode: "duplicate", ClosedToday: true}},
	{"closed today", lightFacts{ClosedToday: true}},
	{"closed today + needs_input", lightFacts{ClosedToday: true, State: "needs_input", StateAt: stateAt}},
}

// expectedLight is D1's table, written independently of lightFor.
func expectedLight(status string, f lightFacts) (class, prefix string) {
	switch status {
	case "closed":
		if f.OpenDismissalCode != "" {
			return "none", "dismissed (" + f.OpenDismissalCode + ")"
		}
		if f.ClosedToday {
			return "done", "done today"
		}
		return "done", "done"
	case "needs_feedback":
		return "input", "waiting on your input: worker parked on a question"
	case "done_locally":
		return "done", "done locally; delivery pending"
	case "delivered":
		return "done", "delivered"
	case "claimed":
		return "working", "in progress (claimed)"
	case "in_progress":
		return "working", "in progress"
	case "pr_open":
		return "working", "in progress: PR open"
	case "awaiting_ci":
		return "working", "in progress: awaiting CI"
	case "awaiting_merge":
		return "working", "in progress: awaiting merge"
	case "holding", "ready", "blocked":
		switch {
		case f.State == "needs_input":
			return "input", "waiting on your input (a session, since "
		case f.State == "working" && f.Stale:
			return "stale", "in progress? no session signal since "
		case f.State == "working":
			return "working", "in progress (a session, last signal "
		}
		switch {
		case status == "ready" && f.QueueHead:
			return "next", "next in queue (" + f.Lane + ")"
		case status == "holding":
			return "none", "holding (review lane; not queued)"
		case status == "blocked":
			return "none", "blocked on a dependency"
		default:
			return "none", "ready, queued"
		}
	}
	return "none", status
}

// Criterion 2: every 0001 status plus one unknown, crossed with every fact
// combination above.
func TestLightFor_EveryStatusAcrossFactCombinations(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	if len(statuses) != 13 {
		t.Fatalf("POSITIVE CONTROL: boardStatusOrder has %d statuses, want the 12 of the 0001 CHECK", len(boardStatusOrder))
	}
	for _, st := range statuses {
		for _, fc := range factCombos {
			name := st + "/" + fc.name
			wantClass, wantPrefix := expectedLight(st, fc.f)
			got := lightFor(st, fc.f)
			assertLight(t, name, got, wantClass, wantPrefix)
			if st == "some_future_status" && got.Label != st {
				t.Errorf("%s: label = %q, want the status verbatim (D1 row 7)", name, got.Label)
			}
		}
	}
}

// The time parts of the session labels (D1 row 5, D6): HH:MM for the fresh
// labels, the full date and time for the stale ring.
func TestLightFor_SessionLabelsCarryTheSignalTime(t *testing.T) {
	for _, st := range []string{"holding", "ready", "blocked"} {
		in := lightFor(st, lightFacts{State: "needs_input", StateAt: stateAt})
		if in.Label != "waiting on your input (a session, since 10:05)" {
			t.Errorf("%s + needs_input label = %q, want %q", st, in.Label, "waiting on your input (a session, since 10:05)")
		}
		w := lightFor(st, lightFacts{State: "working", StateAt: stateAt})
		if w.Label != "in progress (a session, last signal 10:05)" {
			t.Errorf("%s + working label = %q, want %q", st, w.Label, "in progress (a session, last signal 10:05)")
		}
		s := lightFor(st, lightFacts{State: "working", StateAt: stateAt, Stale: true})
		if s.Label != "in progress? no session signal since 2026-09-14 10:05" {
			t.Errorf("%s + stale working label = %q, want %q", st, s.Label, "in progress? no session signal since 2026-09-14 10:05")
		}
	}
}

// Criterion 3: the named precedence cases, verbatim.
func TestLightFor_NamedPrecedenceCases(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		f            lightFacts
		class, label string
	}{
		{"closed beats a leftover needs_input", "closed", lightFacts{State: "needs_input", StateAt: stateAt}, "done", "done"},
		{"closed + open dismissal", "closed", lightFacts{OpenDismissalCode: "duplicate"}, "none", "dismissed (duplicate)"},
		{"needs_feedback beats a working marker", "needs_feedback", lightFacts{State: "working", StateAt: stateAt},
			"input", "waiting on your input: worker parked on a question"},
		{"ready + needs_input beats queue head", "ready", lightFacts{State: "needs_input", StateAt: stateAt, QueueHead: true, Lane: "saka"},
			"input", "waiting on your input (a session, since "},
		{"ready + working beats queue head", "ready", lightFacts{State: "working", StateAt: stateAt, QueueHead: true, Lane: "saka"},
			"working", "in progress (a session, last signal "},
		{"ready + stale working", "ready", lightFacts{State: "working", StateAt: stateAt, Stale: true}, "stale", "in progress? no session signal since "},
		{"in_progress ignores the marker", "in_progress", lightFacts{State: "needs_input", StateAt: stateAt}, "working", "in progress"},
		{"needs_input never goes stale", "holding", lightFacts{State: "needs_input", StateAt: stateAt, Stale: true},
			"input", "waiting on your input (a session, since "},
	} {
		assertLight(t, tc.name, lightFor(tc.status, tc.f), tc.class, tc.label)
	}
	if got := lightFor("closed", lightFacts{OpenDismissalCode: "duplicate"}); got.Label != "dismissed (duplicate)" {
		t.Errorf("closed + open dismissal label = %q, want exactly `dismissed (duplicate)` (criterion 3)", got.Label)
	}
	if got := lightFor("in_progress", lightFacts{State: "needs_input", StateAt: stateAt}); got.Label != "in progress" {
		t.Errorf("in_progress + needs_input label = %q, want exactly `in progress`: outside holding/ready/blocked "+
			"the session marker is ignored (criterion 3)", got.Label)
	}
	if got := lightFor("ready", lightFacts{QueueHead: true, Lane: "acme.web console"}); got.Label != "next in queue (acme.web console)" {
		t.Errorf("ready queue head label = %q, want `next in queue (acme.web console)`", got.Label)
	}
}

// ---- criterion 4: pickQueueHeads --------------------------------------------

func allEligible(int64) bool { return true }

func headsString(m map[int64]string) string {
	var ids []int64
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var parts []string
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10)+"="+m[id])
	}
	return strings.Join(parts, ", ")
}

func assertHeads(t *testing.T, name string, got map[int64]string, want map[int64]string) {
	t.Helper()
	if headsString(got) != headsString(want) {
		t.Errorf("%s: pickQueueHeads = {%s}, want {%s} (D2: first eligible candidate per lane, in input order, "+
			"mapped to its lane name)", name, headsString(got), headsString(want))
	}
}

func TestPickQueueHeads(t *testing.T) {
	human := func(id, proj int64, slug string) headCandidate {
		return headCandidate{ID: id, AssigneeType: "human", ProjectID: proj, ProjectSlug: slug, Client: "acme"}
	}
	claude := func(id, proj int64, slug, client, sub string) headCandidate {
		return headCandidate{ID: id, AssigneeType: "claude", ProjectID: proj, ProjectSlug: slug, Client: client, Subproject: sub}
	}

	// The caller hands candidates in tools.TaskQueueOrder, so "a higher
	// priority wins" means the FIRST candidate wins — never the lowest id.
	t.Run("a higher priority wins", func(t *testing.T) {
		got := pickQueueHeads([]headCandidate{human(20, 1, "saka"), human(10, 1, "saka")}, allEligible)
		assertHeads(t, "priority", got, map[int64]string{20: "saka"})
	})
	t.Run("a red or yellow candidate is skipped", func(t *testing.T) {
		busy := map[int64]bool{20: true}
		got := pickQueueHeads([]headCandidate{human(20, 1, "saka"), human(10, 1, "saka"), human(5, 1, "saka")},
			func(id int64) bool { return !busy[id] })
		assertHeads(t, "skip", got, map[int64]string{10: "saka"})
	})
	t.Run("two lanes in one project give two heads", func(t *testing.T) {
		got := pickQueueHeads([]headCandidate{human(1, 7, "saka"), claude(2, 7, "saka", "acme", ""), human(3, 7, "saka")}, allEligible)
		assertHeads(t, "two lanes", got, map[int64]string{1: "saka", 2: "acme console"})
	})
	t.Run("two subprojects of one client give two heads", func(t *testing.T) {
		got := pickQueueHeads([]headCandidate{
			claude(1, 7, "saka", "acme", "web"), claude(2, 7, "saka", "acme", "api"), claude(3, 7, "saka", "acme", "web"),
		}, allEligible)
		assertHeads(t, "two subprojects", got, map[int64]string{1: "acme.web console", 2: "acme.api console"})
	})
	// The claude lane is per (client, subproject), NOT per project:
	// task_get_next(client) draws across the client's projects.
	t.Run("one claude lane spans a client's projects", func(t *testing.T) {
		got := pickQueueHeads([]headCandidate{claude(1, 7, "saka", "acme", ""), claude(2, 8, "saka-two", "acme", "")}, allEligible)
		assertHeads(t, "claude lane per client", got, map[int64]string{1: "acme console"})
	})
	// The human lane is per project, whatever the subproject.
	t.Run("one human lane per project", func(t *testing.T) {
		a := human(1, 7, "saka")
		b := human(2, 7, "saka")
		b.Subproject = "web"
		c := human(3, 8, "other")
		got := pickQueueHeads([]headCandidate{a, b, c}, allEligible)
		assertHeads(t, "human lane per project", got, map[int64]string{1: "saka", 3: "other"})
	})
	t.Run("an empty lane gives no head", func(t *testing.T) {
		got := pickQueueHeads([]headCandidate{human(1, 7, "saka"), claude(2, 7, "saka", "acme", "")},
			func(id int64) bool { return id != 1 })
		assertHeads(t, "empty lane", got, map[int64]string{2: "acme console"})
		if got := pickQueueHeads(nil, allEligible); len(got) != 0 {
			t.Errorf("pickQueueHeads(nil) = %v, want no heads", got)
		}
	})
}

// ---- criterion 1: lightFor is pure ------------------------------------------

func TestLights_PureNoIONoClock(t *testing.T) {
	b, err := os.ReadFile("lights.go")
	if err != nil {
		t.Fatalf("read internal/dashboard/lights.go: %v. Criterion 1: the light map lives in its own file", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lights.go", b, 0)
	if err != nil {
		t.Fatalf("parse lights.go: %v", err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		switch {
		case p == "time", p == "context", p == "database/sql", p == "net/http", p == "os",
			strings.Contains(p, "pgx"), strings.HasSuffix(p, "internal/store"):
			t.Errorf("lights.go imports %q. Criterion 1: lightFor is pure — no pgx, no time package, no I/O; the "+
				"facts come from a separate read (boardLightFacts) and 'today' is decided on the DB clock", p)
		}
	}
	declared := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				declared[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok {
					declared[ts.Name.Name] = true
				}
			}
		}
	}
	for _, want := range []string{"light", "lightFacts", "lightFor"} {
		if !declared[want] {
			t.Errorf("lights.go does not declare %s (criterion 1)", want)
		}
	}
	for _, fn := range []string{"lightFor", "pickQueueHeads"} {
		body := funcBodySrc(t, "lights.go", fn)
		if body == "" {
			body = funcBodySrc(t, "board.go", fn)
		}
		if body == "" {
			t.Errorf("neither lights.go nor board.go declares %s", fn)
			continue
		}
		for _, banned := range []string{"time.", "s.pool", "Query(", "Exec("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q: it must be a pure function (criteria 1, 4)", fn, banned)
			}
		}
	}
}
