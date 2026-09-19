package dashboard

// board-departures (SWT-67): the board's pure DISPLAY helpers — lights.go's and
// sections.go's third sibling. Everything here is a function of its arguments:
// no I/O, no clock. Sections and lights stay decided by sections.go and
// lights.go; this file only says where a section is drawn and which words and
// colours a row shows.

import "fmt"

// boardPriorityMark is B8's threshold: a row at or above it shows the red ▲.
const boardPriorityMark = 2

// boardPanel is one row of B2's panel table: which pane draws the section, its
// CSS `order` inside that pane, and the panel's class names.
type boardPanel struct {
	Pane  string
	Order int
	Class string
}

// boardPanels covers boardSectionOrder exactly (unit-tested both ways). The
// split is a contiguous prefix/suffix of boardSectionOrder, so document order is
// unchanged; the VISUAL order inside a pane is CSS `order` (incoming is drawn
// last on the left while staying first in the document, SWT-59 I3).
var boardPanels = map[string]boardPanel{
	"incoming":  {Pane: "left", Order: 3, Class: "panel grow"},
	"blocked":   {Pane: "left", Order: 1, Class: "panel alarm"},
	"in_flight": {Pane: "left", Order: 2, Class: "panel"},
	"queue":     {Pane: "right", Order: 1, Class: "panel grow"},
	"holding":   {Pane: "right", Order: 2, Class: "panel"},
	"done":      {Pane: "right", Order: 3, Class: "panel"},
	"other":     {Pane: "right", Order: 4, Class: "panel"},
}

// boardPane is one column of the board.
type boardPane struct {
	Key      string
	Sections []boardSection
}

// boardPaneOrder is the panes' document order.
var boardPaneOrder = []string{"left", "right"}

// boardPanes splits the sections into the two panes, keeping the sections'
// order and stamping each with its panel's Class and Order. An empty pane is
// omitted. A key missing from boardPanels lands in the right pane rather than
// vanishing.
func boardPanes(secs []boardSection) []boardPane {
	if len(secs) == 0 {
		return nil
	}
	byPane := map[string][]boardSection{}
	for _, s := range secs {
		p, ok := boardPanels[s.Key]
		if !ok {
			p = boardPanels["other"]
		}
		s.Class, s.Order = p.Class, p.Order
		byPane[p.Pane] = append(byPane[p.Pane], s)
	}
	var out []boardPane
	for _, k := range boardPaneOrder {
		if len(byPane[k]) > 0 {
			out = append(out, boardPane{Key: k, Sections: byPane[k]})
		}
	}
	return out
}

// boardTally is B16: counts of what the board is SHOWING, after the filters.
type boardTally struct{ NeedYou, InFlight, Incoming, Queued, DoneToday, Open int }

// boardTallyItem is one tally of the sign header: a colour class, the words
// (lowercase; the CSS uppercases) and the count.
type boardTallyItem struct {
	Class, Label string
	Count        int
}

func boardTallies(secs []boardSection) boardTally {
	var t boardTally
	for _, s := range secs {
		n := len(s.Tasks)
		switch s.Key {
		case "blocked":
			t.NeedYou += n
		case "in_flight":
			t.InFlight += n
		case "incoming":
			t.Incoming += n
		case "queue", "holding":
			t.Queued += n
		case "done":
			t.DoneToday += n
		}
		if s.Key != "done" {
			t.Open += n
		}
	}
	return t
}

// Items is the sign header's five tallies in B16's order. The words live here
// so the template spells none of them. Open is the ticker's, not the header's.
func (t boardTally) Items() []boardTallyItem {
	return []boardTallyItem{
		{Class: "t-input", Label: "need you", Count: t.NeedYou},
		{Class: "t-work", Label: "in flight", Count: t.InFlight},
		{Class: "t-in", Label: "incoming", Count: t.Incoming},
		{Class: "t-queue", Label: "queued", Count: t.Queued},
		{Class: "t-done", Label: "done today", Count: t.DoneToday},
	}
}

// remarkFor is B7's table: the Remarks words for a light and a status,
// lowercase and never empty. B5: the status column is gone, so the status
// survives here.
func remarkFor(l light, status string) string {
	switch l.Class {
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
		return "in progress"
	case "done":
		switch status {
		case "done_locally":
			return "done locally"
		case "delivered":
			return "delivered"
		}
		return "done"
	}
	switch status {
	case "holding":
		return "holding"
	case "blocked":
		return "blocked"
	case "ready":
		return "queued"
	case "closed":
		return "dismissed"
	case "":
		return "unknown" // B7 says "the status verbatim", but a remark is never empty
	}
	return status
}

// elapsedFor is B6: HH:MM since the session signal for the three classes that
// tick, "" for every other. Hours are not capped.
func elapsedFor(class string, minutes int) string {
	switch class {
	case "input", "working", "stale":
	default:
		return ""
	}
	if minutes < 0 {
		minutes = 0
	}
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// projectHue is B9: the mock's fold over the slug's bytes, so the accepted
// colours are the colours that ship.
func projectHue(slug string) int {
	h := 0
	for i := 0; i < len(slug); i++ {
		h = (h*31 + int(slug[i])) % 360
	}
	return h
}

// projectLabel names the active project filter for the sign header.
func projectLabel(filter string) string {
	if filter == "" {
		return "all projects"
	}
	return filter
}
