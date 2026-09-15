package dashboard

import "sort"

// SWT-57 (board-layout-compact) L1/L2: the board's rows grouped into six
// sections derived from each row's LIGHT, so nothing blocked or in flight needs
// scrolling and the sections agree with the lights by construction. The status
// is read only for the grey ring, where the class alone cannot say why a row is
// not queued. Pure: no I/O, no clock.

// boardSection is one section of the board: its key (the h2 id suffix), its
// title and its rows, in display order.
type boardSection struct {
	Key, Title string
	Tasks      []taskRow
}

// boardSectionOrder is L2's section order. Empty sections are not rendered.
var boardSectionOrder = []boardSection{
	{Key: "blocked", Title: "blocked"},
	{Key: "in_flight", Title: "in flight"},
	{Key: "queue", Title: "queue"},
	{Key: "holding", Title: "holding"},
	{Key: "done", Title: "done"},
	{Key: "other", Title: "other"},
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

// boardSections partitions rows into boardSectionOrder's sections, omitting
// empty ones; every row lands in exactly one. The queue is ordered by QueueRank
// (tools.TaskQueueOrder, read by boardLightFacts), unranked rows after ranked
// ones by id; every other section by light rank, then the status's position in
// boardStatusOrder (unknown statuses last), then id.
func boardSections(rows []taskRow) []boardSection {
	if len(rows) == 0 {
		return nil
	}
	byKey := map[string][]taskRow{}
	for _, r := range rows {
		k := sectionFor(r.Status, r.Light)
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
		if s.Key == "queue" {
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
		} else {
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
