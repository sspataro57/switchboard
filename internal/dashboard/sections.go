package dashboard

import "sort"

// SWT-57 (board-layout-compact) L1/L2: the board's rows grouped into sections
// derived from each row's LIGHT, so nothing blocked or in flight needs
// scrolling and the sections agree with the lights by construction. The status
// is read only for the grey ring, where the class alone cannot say why a row is
// not queued. Pure: no I/O, no clock.
//
// SWT-59 (board-incoming-first) amends L1 in exactly one place: a task the
// promoter created from an inbound email or Slack message, or a PR waiting for
// Salvador's review, goes in a new FIRST section, "incoming", whatever its light
// except green (finished work is not waiting). The row keeps its light;
// lightFor never reads the provenance facts.

// boardSection is one section of the board: its key (the h2 id suffix), its
// title and its rows, in display order.
type boardSection struct {
	Key, Title string
	Tasks      []taskRow
	// Class and Order are display-only (SWT-67): the panel's class names and its
	// CSS order inside its pane. Set ONLY by boardPanes; boardSections leaves
	// them zero.
	Class string
	Order int
}

// boardSectionOrder is the section order (SWT-57 L2, SWT-59 I3). Empty
// sections are not rendered.
var boardSectionOrder = []boardSection{
	{Key: "incoming", Title: "arrivals — incoming"},
	{Key: "blocked", Title: "needs you"},
	{Key: "in_flight", Title: "in flight"},
	{Key: "queue", Title: "departures — queue"},
	{Key: "holding", Title: "holding"},
	{Key: "done", Title: "landed today"},
	{Key: "other", Title: "other"},
}

// The two incoming kinds (SWT-59 I1, I2), in the section's order.
const (
	incomingMessage  = "message"   // the promoter created it from an email or Slack message
	incomingPRReview = "pr_review" // a human task carrying a github PR ref
)

// incomingKind names a row's incoming kind from its two provenance facts; a
// promoter task wins when both are set, "" when neither is.
func incomingKind(fromMessage, prReview bool) string {
	switch {
	case fromMessage:
		return incomingMessage
	case prReview:
		return incomingPRReview
	}
	return ""
}

// boardSectionOf is the one section function (SWT-59 I4): an incoming row goes
// to "incoming" unless it is closed or its light is green; every other row goes
// where sectionFor's light table puts it.
func boardSectionOf(r taskRow) string {
	if r.Incoming != "" && r.Status != "closed" && r.Light.Class != "done" {
		return "incoming"
	}
	return sectionFor(r.Status, r.Light)
}

// sectionFor is L1's table: the light's class decides, and only a grey ring
// consults the status.
func sectionFor(status string, l light) string {
	switch l.Class {
	case "input":
		return "blocked" // waiting on Salvador: a session's needs_input or a worker's needs_feedback
	case "working", "stale":
		return "in_flight"
	case "next":
		return "queue"
	case "done":
		return "done"
	}
	switch status {
	case "blocked":
		return "blocked" // waiting on a dependency
	case "ready":
		return "queue" // queued behind the head
	case "holding":
		return "holding"
	}
	return "other" // a dismissed closed row, or an unknown status
}

// lightRank is L2's within-section order: attention first.
var lightRank = map[string]int{"input": 0, "stale": 1, "working": 2, "next": 3, "done": 4, "none": 5}

// incomingRank is I5's second key: messages before PRs.
var incomingRank = map[string]int{incomingMessage: 0, incomingPRReview: 1}

// boardSections partitions rows into boardSectionOrder's sections, omitting
// empty ones; every row lands in exactly one (boardSectionOf). Incoming is
// ordered by light rank, then kind (messages before PRs), then id DESC — the
// newest first; the queue by QueueRank (tools.TaskQueueOrder, read by
// boardLightFacts), unranked rows after ranked ones by id; every other section
// by light rank, then the status's position in boardStatusOrder (unknown
// statuses last), then id.
func boardSections(rows []taskRow) []boardSection {
	if len(rows) == 0 {
		return nil
	}
	byKey := map[string][]taskRow{}
	for _, r := range rows {
		k := boardSectionOf(r)
		byKey[k] = append(byKey[k], r)
	}
	statusPos := map[string]int{}
	for i, st := range boardStatusOrder {
		statusPos[st] = i
	}
	pos := func(st string) int {
		if p, ok := statusPos[st]; ok {
			return p
		}
		return len(boardStatusOrder)
	}
	var out []boardSection
	for _, s := range boardSectionOrder {
		ts := byKey[s.Key]
		if len(ts) == 0 {
			continue
		}
		switch s.Key {
		case "incoming":
			sort.SliceStable(ts, func(i, j int) bool {
				a, b := ts[i], ts[j]
				if ra, rb := lightRank[a.Light.Class], lightRank[b.Light.Class]; ra != rb {
					return ra < rb
				}
				if ka, kb := incomingRank[a.Incoming], incomingRank[b.Incoming]; ka != kb {
					return ka < kb
				}
				return a.ID > b.ID
			})
		case "queue":
			sort.SliceStable(ts, func(i, j int) bool {
				a, b := ts[i], ts[j]
				switch {
				case a.QueueRank > 0 && b.QueueRank > 0:
					if a.QueueRank != b.QueueRank {
						return a.QueueRank < b.QueueRank
					}
				case a.QueueRank > 0:
					return true
				case b.QueueRank > 0:
					return false
				}
				return a.ID < b.ID
			})
		default:
			sort.SliceStable(ts, func(i, j int) bool {
				a, b := ts[i], ts[j]
				if ra, rb := lightRank[a.Light.Class], lightRank[b.Light.Class]; ra != rb {
					return ra < rb
				}
				if pa, pb := pos(a.Status), pos(b.Status); pa != pb {
					return pa < pb
				}
				return a.ID < b.ID
			})
		}
		out = append(out, boardSection{Key: s.Key, Title: s.Title, Tasks: ts})
	}
	return out
}
