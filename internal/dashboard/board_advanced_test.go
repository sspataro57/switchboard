package dashboard

// board-layout-compact (SWT-57, docs/tickets/board-layout-compact_SPEC.md)
// criterion 8, the unit table: boardAdvanced, the third iterator over the ONE
// key list (the IK's "Five keys, one list"; boardRefreshURLs' sibling). Its
// structure half (it iterates boardKeys and never names an advanced key) is in
// board_layout_structure_test.go. ZERO I/O.
//
// IMPOSED SURFACE (SPEC criterion 8):
//
//	// board.go
//	type boardFilter struct{ Key, Value string }
//	func boardAdvanced(q url.Values) (active []boardFilter, clearURL string)
//
// GREENFIELD NOTE — EXPECTED RED: boardFilter and boardAdvanced do not exist,
// so package dashboard's test binary compile-FAILS.

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

var _ = boardFilter{Key: "status", Value: "ready"}

func TestBoardAdvanced(t *testing.T) {
	q := func(raw string) url.Values {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", raw, err)
		}
		return v
	}
	for _, tc := range []struct {
		name, query string
		chips       []boardFilter
		clear       string
	}{
		{"no advanced key", "project=saka&refresh=on", nil, ""},
		{"empty query", "", nil, ""},
		{"flash alone is not a filter", "project=saka&flash=task_close+ok", nil, ""},
		{"every advanced value empty", "project=saka&status=&assignee_type=&subproject=", nil, ""},
		{"one advanced key keeps project and refresh=on", "project=saka&status=ready&refresh=on",
			[]boardFilter{{"status", "ready"}}, "/tasks?project=saka&refresh=on"},
		{"all three keys, chips in boardKeys order", "subproject=web&assignee_type=human&status=ready&project=saka",
			[]boardFilter{{"status", "ready"}, {"assignee_type", "human"}, {"subproject", "web"}}, "/tasks?project=saka"},
		{"refresh=1 is not on", "project=saka&status=ready&refresh=1",
			[]boardFilter{{"status", "ready"}}, "/tasks?project=saka"},
		{"refresh=ON is not on", "project=saka&status=ready&refresh=ON",
			[]boardFilter{{"status", "ready"}}, "/tasks?project=saka"},
		{"empty values are omitted", "project=saka&status=&assignee_type=human&subproject=&refresh=on",
			[]boardFilter{{"assignee_type", "human"}}, "/tasks?project=saka&refresh=on"},
		{"flash and foreign keys never carried",
			"project=saka&status=ready&refresh=on&flash=task_close+ok&next=%2F%2Fevil.example&x=1",
			[]boardFilter{{"status", "ready"}}, "/tasks?project=saka&refresh=on"},
		{"only advanced keys: clear is /tasks", "status=ready&subproject=web",
			[]boardFilter{{"status", "ready"}, {"subproject", "web"}}, "/tasks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chips, clear := boardAdvanced(q(tc.query))
			if fmt.Sprint(chips) != fmt.Sprint(tc.chips) || len(chips) != len(tc.chips) {
				t.Errorf("boardAdvanced(%q) chips = %v, want %v (criterion 8)", tc.query, chips, tc.chips)
			}
			if clear != tc.clear {
				t.Errorf("boardAdvanced(%q) clearURL = %q, want %q (criterion 8)", tc.query, clear, tc.clear)
			}
			for _, banned := range []string{"flash", "next", "evil", "x=1"} {
				if strings.Contains(clear, banned) || strings.Contains(fmt.Sprint(chips), banned) {
					t.Errorf("boardAdvanced(%q) carries %q (chips %v, clear %q): boardKeys only, never flash (criterion 8)",
						tc.query, banned, chips, clear)
				}
			}
		})
	}
}
