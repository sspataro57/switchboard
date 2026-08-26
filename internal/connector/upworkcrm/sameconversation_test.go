package upworkcrm

import "testing"

// The candidate rule as a PURE test (SWT-19, added after review).
//
// SameConversation is the central rule of the ticket and its only other coverage
// is the integration suite, which skips when DATABASE_URL is unset — so before
// this file `go test ./...` proved nothing about it. A rule that decides which
// deliveries can be confirmed should not be provable only on a machine with a
// database.
//
// THE RULE: a room MISMATCH is the only thing that excludes. An unknown room
// excludes nothing.
//
//	                  delivery roomed        delivery unroomed
//	message roomed    same room only         candidate
//	message unroomed  candidate              candidate
//
// The bottom-left cell is the one a future reader will want to "tighten", and
// tightening it is the bug. API-era outbound traffic is 98.9% roomed (186 of
// 188) because the send path is HEALTHY and records its room in send_room_id —
// but 576 outbound rows are pre-2026-07-21 history with no room in either
// column, 2 API-era rows carry none, and `--all` replays all of that through the
// matcher. Refusing that cell makes those permanently unconfirmable, silently.
// It is a legacy tolerance, not an accommodation of a broken send path. (The
// 44.7% figure in early SWT-19 drafts was one column, measured wrongly.)
func TestSameConversation_TruthTable(t *testing.T) {
	const (
		clientA = "aaaaaaaa-0000-0000-0000-00000000000a"
		clientB = "bbbbbbbb-0000-0000-0000-00000000000b"
		room1   = "room_9f45f6ecccffad7c804183574cba479f"
		room2   = "room_6f162de2235e2b5cee735c8268fe30a0"
	)
	roomed := func(client, room string) ThreadRef {
		return ThreadRef{ClientID: client, RoomID: room, Roomed: true}
	}
	legacy := func(client string) ThreadRef {
		return ThreadRef{ClientID: client, Channel: "upwork"}
	}

	cases := []struct {
		name             string
		message, deliver ThreadRef
		want             bool
		why              string
	}{
		{
			name: "roomed message, roomed delivery, SAME room", message: roomed(clientA, room1),
			deliver: roomed(clientA, room1), want: true,
			why: "the one cell where room identity actually pays",
		},
		{
			name: "roomed message, roomed delivery, DIFFERENT room", message: roomed(clientA, room1),
			deliver: roomed(clientA, room2), want: false,
			why: "the ONLY exclusion the rule makes. Two production clients have several rooms " +
				"(one has three), and binding a send in room 2 to a message from room 1 burns the " +
				"external id on the wrong row under deliveries_sent_external_idx, permanently",
		},
		{
			name: "roomed message, UNROOMED delivery", message: roomed(clientA, room1),
			deliver: legacy(clientA), want: true,
			why: "a delivery drafted before the re-key has a legacy target_ref. Excluding it would " +
				"break confirmations that worked before this ticket",
		},
		{
			name: "UNROOMED message, roomed delivery", message: legacy(clientA),
			deliver: roomed(clientA, room1), want: true,
			why: "the legacy tolerance. 576 outbound rows have no room in either column; refusing " +
				"here makes every one of them permanently unconfirmable, silently. Do not tighten this",
		},
		{
			name: "unroomed message, unroomed delivery", message: legacy(clientA),
			deliver: legacy(clientA), want: true,
			why: "the pre-SWT-19 behaviour, unchanged for the legacy corpus",
		},
		{
			name: "different clients, both unroomed", message: legacy(clientA),
			deliver: legacy(clientB), want: false,
			why: "client identity is checked first and always; the body prefix is not evidence across clients",
		},
		{
			name: "different clients, same room id", message: roomed(clientA, room1),
			deliver: roomed(clientB, room1), want: false,
			why: "a shared room id must not defeat the client check — the client comparison comes first",
		},
		{
			name: "different clients, message roomed and delivery not", message: roomed(clientA, room1),
			deliver: legacy(clientB), want: false,
			why: "the unknown-room tolerance must never reach across clients",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameConversation(tc.message, tc.deliver); got != tc.want {
				t.Errorf("SameConversation(%+v, %+v) = %v, want %v — %s", tc.message, tc.deliver, got, tc.want, tc.why)
			}
		})
	}
}

// Every candidate SWT-18's predicate accepted must still be a candidate.
//
// SWT-18 selected deliveries with `target_ref = thread_key`, so proving the new
// rule is a superset of the old one is what shows this ticket confirms strictly
// more, never less. A regression here would be invisible in production — the
// symptom is a delivery that quietly stops being confirmable.
func TestSameConversation_IsASupersetOfExactKeyEquality(t *testing.T) {
	keys := []string{
		ThreadKey("aaaaaaaa-0000-0000-0000-00000000000a", "", "upwork"),
		ThreadKey("aaaaaaaa-0000-0000-0000-00000000000a", "room_9f45f6ecccffad7c804183574cba479f", ""),
		ThreadKey("bbbbbbbb-0000-0000-0000-00000000000b", "", "upwork"),
		ThreadKey("bbbbbbbb-0000-0000-0000-00000000000b", "room_6f162de2235e2b5cee735c8268fe30a0", ""),
	}
	for _, key := range keys {
		ref, err := ParseThreadKey(key)
		if err != nil {
			t.Fatalf("ParseThreadKey(%q): %v — a key this package produced must parse", key, err)
		}
		// target_ref == thread_key was SWT-18's whole predicate.
		if !SameConversation(ref, ref) {
			t.Errorf("SameConversation rejects the identical key %q, so a confirmation SWT-18 made "+
				"would now be lost. The new rule must be a strict superset of exact equality", key)
		}
	}
}
