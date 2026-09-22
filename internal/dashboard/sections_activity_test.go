package dashboard

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D5 and
// criteria 29, 34, 35 and 36: the board's THIRD incoming kind, its place in the
// section's order, and the remark words. PURE — zero I/O, no clock (invariant
// 7). sections_incoming_test.go (SWT-59) is the neighbour this amends.
//
// ---- IMPOSED SURFACE (greenfield: the SPEC's contract defines it) -------------
//
//	// internal/dashboard/sections.go
//	const incomingActivity = "activity"
//	func incomingKind(fromMessage, prReview, activity bool) string   // message > pr_review > activity > ""
//	var incomingRank = map[string]int{..., incomingActivity: 2}
//	// boardSections' incoming comparator, D5's FOUR keys, in order:
//	//   1. light rank (unchanged — a red row still leads)
//	//   2. needs review before reviewed, whatever the kind
//	//   3. kind rank: message 0, pr_review 1, activity 2
//	//   4. later ActivityStamp first; otherwise id DESC (SWT-59's key)
//	// board.go:   taskRow gains NeedsReview bool, ActivityFrom, ActivityStamp string
//	// lights.go:  lightFacts gains NeedsReview bool, ActivityChannel, ActivitySender,
//	//             ActivityStamp string — DISPLAY-ONLY, never read by lightFor
//	// display.go: func activityRemark(channel string) string
//
// WHY KEY 2 EXISTS (D5's argument, which the SPEC asked to be made explicit).
// `activity` last in the kind list and nothing else would BURY the newest fact,
// because a brand-new ask is an old task id away from a comment on #73.
// `activity` first would invert his own order ("emails or slacks… same thing
// with prs") on a quiet day. Key 2 settles both: the unreviewed thing — the only
// thing in this section with a clock on it, and the only thing Requeue can
// clear — leads, and among the unreviewed his message-before-PR order holds.
//
// GREENFIELD NOTE, EXPECTED RED: incomingActivity, the third argument,
// activityRemark and the four taskRow/lightFacts fields do not exist, so
// package dashboard's test binary compile-FAILS.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - sort incoming without the needs-review key -> NeedsReviewLeadsTheSection.
//   - sort by id DESC instead of the stamp among two needs-review rows ->
//     TwoNeedsReviewRowsSortByStamp.
//   - activity first in the kind rank -> KindRankAmongUnreviewed.
//   - activityRemark's table changed -> ActivityRemarkTable.

import (
	"testing"
)

// Criteria 29 and 34: all EIGHT combinations, message > pr_review > activity.
func TestIncomingKind_EightCombinations(t *testing.T) {
	if incomingActivity != "activity" {
		t.Fatalf("incomingActivity = %q, want \"activity\" (criterion 29)", incomingActivity)
	}
	for _, tc := range []struct {
		fromMessage, prReview, activity bool
		want                            string
	}{
		{false, false, false, ""},
		{false, false, true, incomingActivity},
		{false, true, false, incomingPRReview},
		{false, true, true, incomingPRReview}, // pr_review beats activity
		{true, false, false, incomingMessage},
		{true, false, true, incomingMessage}, // message beats activity
		{true, true, false, incomingMessage},
		{true, true, true, incomingMessage}, // message beats both
	} {
		if got := incomingKind(tc.fromMessage, tc.prReview, tc.activity); got != tc.want {
			t.Errorf("incomingKind(%v, %v, %v) = %q, want %q (criterion 34: message > pr_review > activity)",
				tc.fromMessage, tc.prReview, tc.activity, got, tc.want)
		}
	}
	if incomingRank[incomingActivity] != 2 {
		t.Errorf("incomingRank[activity] = %d, want 2 (criterion 29)", incomingRank[incomingActivity])
	}
	if incomingRank[incomingMessage] != 0 || incomingRank[incomingPRReview] != 1 {
		t.Errorf("incomingRank = %v; SWT-59's message 0 / pr_review 1 is unchanged", incomingRank)
	}
}

// actRow is incRow plus this ticket's two ordering facts.
func actRow(id int64, status, class, kind string, needsReview bool, stamp string) taskRow {
	return taskRow{ID: id, Status: status, Light: light{Class: class}, Incoming: kind,
		NeedsReview: needsReview, ActivityStamp: stamp}
}

// ---- criterion 35: the order -------------------------------------------------------

// Key 2: a needs-review GREY `activity` row precedes a REVIEWED grey `message`
// row — the whole point of the section. Without key 2 the kind rank would put
// the message first and the newest fact would be buried.
func TestBoardSections_NeedsReviewLeadsTheSection(t *testing.T) {
	rows := []taskRow{
		actRow(700, "ready", "none", incomingMessage, false, ""),                       // a reviewed promoter row, high id
		actRow(73, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000"), // Katie's comment on #73
	}
	assertSections(t, "unreviewed activity before a reviewed message", boardSections(rows), "incoming:73,700")
}

// Key 1 still wins: a RED row leads, reviewed or not. "a red row still leads"
// is D5's first key, unchanged from SWT-59.
func TestBoardSections_ARedRowStillLeadsTheSection(t *testing.T) {
	rows := []taskRow{
		actRow(73, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000"),
		actRow(5, "holding", "input", incomingPRReview, false, ""), // red, reviewed
	}
	assertSections(t, "red before unreviewed grey", boardSections(rows), "incoming:5,73")
}

// Key 3, among two UNREVIEWED rows of the same light: his own order holds —
// message, then pr_review, then activity.
func TestBoardSections_KindRankAmongUnreviewed(t *testing.T) {
	rows := []taskRow{
		actRow(10, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000"),
		actRow(20, "ready", "none", incomingPRReview, true, "2026-09-22 13:21:00.000"),
		actRow(30, "ready", "none", incomingMessage, true, "2026-09-22 13:22:00.000"),
	}
	assertSections(t, "message, pr_review, activity among unreviewed", boardSections(rows), "incoming:30,20,10")
}

// Key 4: among two needs-review rows the LATER ActivityStamp leads; equal or
// empty stamps fall back to SWT-59's id DESC. The stamp is a lexicographically
// sortable 'YYYY-MM-DD HH24:MI:SS.US' string produced by Postgres in
// BoardTimeZone (criterion 28) — no Go clock is involved anywhere.
func TestBoardSections_TwoNeedsReviewRowsSortByStamp(t *testing.T) {
	rows := []taskRow{
		actRow(91, "ready", "none", incomingActivity, true, "2026-09-22 12:52:10.000001"),
		actRow(73, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000002"), // newest
		actRow(366, "ready", "none", incomingActivity, true, "2026-09-22 09:04:59.999999"),
	}
	assertSections(t, "newest activity first", boardSections(rows), "incoming:73,91,366")

	// Equal stamps: id DESC (SWT-59's key, unchanged).
	same := []taskRow{
		actRow(1, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000000"),
		actRow(9, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000000"),
		actRow(5, "ready", "none", incomingActivity, true, "2026-09-22 13:20:00.000000"),
	}
	assertSections(t, "equal stamps fall back to id DESC", boardSections(same), "incoming:9,5,1")

	// REVIEWED rows keep a stale stamp (activity, then Requeue or a close);
	// among them the stamp is not a key — SWT-59's id DESC is (D5 key 4 says
	// "among two needs-review rows").
	reviewed := []taskRow{
		actRow(3, "ready", "none", incomingMessage, false, "2026-09-22 13:20:00.000000"),
		actRow(7, "ready", "none", incomingMessage, false, "2026-09-22 09:00:00.000000"),
	}
	assertSections(t, "reviewed rows ignore the stamp", boardSections(reviewed), "incoming:7,3")

	// Empty stamps (an unreviewed row whose activity_at somehow renders blank):
	// id DESC, never a panic and never a random order.
	empty := []taskRow{
		actRow(2, "ready", "none", incomingActivity, true, ""),
		actRow(8, "ready", "none", incomingActivity, true, ""),
	}
	assertSections(t, "empty stamps fall back to id DESC", boardSections(empty), "incoming:8,2")
}

// The partition still holds with the third kind in play: every id exactly once,
// fed in id order and reversed, and the order is a total order (the same answer
// both ways).
func TestBoardSections_ActivityPartitionHolds(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	var rows []taskRow
	id := int64(0)
	for _, st := range statuses {
		for _, fc := range factCombos {
			for _, kind := range []string{"", incomingMessage, incomingPRReview, incomingActivity} {
				for _, nr := range []bool{false, true} {
					id++
					f := fc.f
					f.FromMessage = kind == incomingMessage
					f.PRReview = kind == incomingPRReview
					f.NeedsReview = kind == incomingActivity && nr
					stamp := ""
					if nr {
						stamp = "2026-09-22 13:20:00.0000" + string(rune('0'+int(id%10)))
					}
					rows = append(rows, taskRow{ID: id, Status: st, Light: lightFor(st, f), Incoming: kind,
						NeedsReview: nr, ActivityStamp: stamp})
				}
			}
		}
	}
	reversed := make([]taskRow, len(rows))
	for i, r := range rows {
		reversed[len(rows)-1-i] = r
	}
	forward, back := sectionsString(boardSections(rows)), sectionsString(boardSections(reversed))
	if forward != back {
		t.Errorf("boardSections is not a total order: fed in id order it gives\n  %s\nreversed it gives\n  %s",
			forward, back)
	}
	seen := map[int64]int{}
	for _, s := range boardSections(rows) {
		for _, r := range s.Tasks {
			seen[r.ID]++
			if want := expectedSectionOf(r.Status, r.Light.Class, r.Incoming); want != s.Key {
				t.Errorf("task %d (%s, %s, Incoming %q) is in %s, I4 says %s", r.ID, r.Status, r.Light.Class,
					r.Incoming, s.Key, want)
			}
		}
	}
	if len(seen) != len(rows) {
		t.Errorf("%d distinct ids returned, want %d (every row in exactly one section)", len(seen), len(rows))
	}
	for _, r := range rows {
		if seen[r.ID] != 1 {
			t.Errorf("task %d appears %d times, want exactly once", r.ID, seen[r.ID])
		}
	}
}

// Criterion 35's last clause, and the one that keeps SWT-59 honest: with NO
// needs-review row anywhere, the section strings are BYTE-IDENTICAL to SWT-59's
// mixed-board golden. The board he looks at on a quiet day must not move
// because a key was added.
func TestBoardSections_NoNeedsReviewIsSWT59Unchanged(t *testing.T) {
	rows := []taskRow{
		incRow(1, "ready", "none", incomingMessage, 3),
		incRow(2, "ready", "none", incomingPRReview, 2),
		incRow(3, "holding", "input", incomingPRReview, 0),
		incRow(4, "blocked", "none", incomingMessage, 0),
		incRow(5, "ready", "next", "", 1),
		incRow(6, "ready", "none", "", 4),
		incRow(7, "blocked", "none", "", 0),
		incRow(8, "closed", "done", incomingMessage, 0),
		incRow(9, "in_progress", "working", "", 0),
		incRow(10, "ready", "working", incomingMessage, 5),
		incRow(11, "ready", "stale", incomingPRReview, 0),
		incRow(12, "closed", "none", incomingMessage, 0),
		incRow(13, "delivered", "done", incomingPRReview, 0),
		incRow(14, "needs_feedback", "input", "", 0),
		incRow(15, "ready", "none", incomingMessage, 0),
		incRow(16, "ready", "none", incomingPRReview, 0),
	}
	const want = "incoming:3,11,10,15,4,1,16,2 | blocked:14,7 | in_flight:9 | queue:5,6 | done:13,8 | other:12"
	if got := sectionsString(boardSections(rows)); got != want {
		t.Errorf("with no needs-review row the board is not SWT-59's:\n got %s\nwant %s\n(criterion 35: the "+
			"section strings are byte-identical to SWT-59's mixed-board golden)", got, want)
	}
}

// ---- criterion 36: the remark words -------------------------------------------------

// D5: the remark is derived from the surfacing message's CHANNEL — real data,
// no column. D1 records why there is no activity_kind discriminator: after D11
// removes the promoter's ask-attach, `ask` would never be written, and a
// predicate that is constant in production is the inert-predicate landmine.
func TestActivityRemark_Table(t *testing.T) {
	for _, tc := range []struct{ channel, want string }{
		{"jira", "new comment"},
		{"gmail", "new email"},
		{"slack", "new slack"},
		{"upwork", "new message"}, // named in the SPEC as "anything else, including upwork"
		{"github", "new message"},
		{"", "new message"}, // an empty channel still reads as something arrived
		{"JIRA", "new message"},
		{"unknown_future_channel", "new message"},
	} {
		if got := activityRemark(tc.channel); got != tc.want {
			t.Errorf("activityRemark(%q) = %q, want %q (criterion 31/36)", tc.channel, got, tc.want)
		}
	}
	// It is never empty: a Remarks cell with nothing in it reads as a bug.
	for _, ch := range []string{"", " ", "x"} {
		if activityRemark(ch) == "" {
			t.Errorf("activityRemark(%q) is empty; a remark is never empty (the remarkFor rule)", ch)
		}
	}
}

// Criterion 27: the four new lightFacts fields are DISPLAY-ONLY — lightFor
// never reads them, so a surfaced row keeps its own light (a claude task in
// flight stays yellow with its session tag, D7 / OQ-2 = A).
func TestLightFor_IgnoresTheActivityFacts(t *testing.T) {
	statuses := append(append([]string(nil), boardStatusOrder...), "some_future_status")
	for _, st := range statuses {
		for _, fc := range factCombos {
			plain := lightFor(st, fc.f)
			loud := fc.f
			loud.NeedsReview = true
			loud.ActivityChannel, loud.ActivitySender = "jira", "Katie Evans (JIRA)"
			loud.ActivityStamp = "2026-09-22 13:20:00.000000"
			if got := lightFor(st, loud); got != plain {
				t.Errorf("%s/%s: lightFor changes with the activity facts (%+v vs %+v). Criterion 27: they are "+
					"display-only; incoming is PLACEMENT, and the light never reads it", st, fc.name, got, plain)
			}
		}
	}
}
