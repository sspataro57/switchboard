package upworkcrm_test

// THE highest-value test in SWT-19: the normalizer reads BOTH room columns
// (SPEC §2, acceptance criterion 3), plus criterion 4's determinism.
//
// ZERO I/O — NormalizeCommunication is a pure mapper over one raw_source_items
// row.
//
// Why this test carries more weight than the rest of the suite, in the SPEC's
// own words: `communications` has TWO room columns and reading one of them is
// reading none.
//
//	upwork_room_id  the room a message was OBSERVED in    (inbound 212, outbound  84)
//	send_room_id    the room a send was DISPATCHED to     (inbound   0, outbound 136)
//
// (Counts measured against the production source on 2026-08-26. They are
// ILLUSTRATION, not invariants — the corpus is live and grows every 15 minutes.
// The shape is what matters: the majority of ROOMED OUTBOUND rows carry their
// room in send_room_id, and confirmUpworkDelivery runs for outbound only.)
//
// A normalizer that reads `upwork_room_id` alone produces keys that look
// perfectly well-formed, passes every unit test willing to fabricate a room, and
// quietly files the send_room_id majority of roomed outbound under the LEGACY
// key. Nothing errors. That is indistinguishable from correct behaviour at the
// write site — the same shape as SWT-18's RoomDiscrimination test proving "room
// scoping" with channel values the source has never emitted (IK: "a predicate
// whose discriminating column is a constant in production is a no-op that passes
// any test willing to fabricate values for it").
//
// So the send_room_id-only case below is the one assertion that separates a
// correct implementation from the bug this ticket exists to prevent, and the
// four cases are asserted separately rather than as a table of one shape.
//
// GREENFIELD NOTE: compile-FAILs until normalize.go's rawCommunication gains
// UpworkRoomID (`upwork_room_id`) and SendRoomID (`send_room_id`) and
// threadkey.go lands. Expected red state.

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

const (
	rsClient = "e2ef9b65-9813-4d79-ac10-0e1813f788ff" // the real two-room client
	// Real-shaped room ids: `room_<hex>`, the identifier space BOTH columns draw
	// from (6 values appear in both). Not "chat"/"room-b" — those are the channel
	// values SWT-18 fabricated and the source has never emitted.
	rsObservedRoom   = "room_1a2b3c4d5e"
	rsDispatchedRoom = "room_9f8e7d6c5b"
	rsChannel        = "upwork"
)

// rsRaw builds one raw communications row. A nil pointer means the JSON key is
// ABSENT; a non-nil empty string means the key is present and blank. Both must
// be treated as "no room" (criterion 3's last sentence).
func rsRaw(t *testing.T, upworkRoom, sendRoom *string) json.RawMessage {
	t.Helper()
	row := map[string]any{
		"id":              "aaaaaaaa-0000-0000-0000-00000000rs01",
		"client_id":       rsClient,
		"direction":       "outbound",
		"channel":         rsChannel,
		"subject":         nil,
		"body":            "shipped the importer, numbers tomorrow",
		"communicated_at": "2026-08-01T10:00:00Z",
		"sender":          "me",
		"external_id":     "story_7f3a91",
	}
	if upworkRoom != nil {
		row["upwork_room_id"] = *upworkRoom
	}
	if sendRoom != nil {
		row["send_room_id"] = *sendRoom
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal raw communication: %v", err)
	}
	return raw
}

func strp(s string) *string { return &s }

// The legacy key, spelled here by hand ON PURPOSE. This is the one place a
// second spelling is wanted: it is the assertion that the unroomed form is
// byte-identical to the pre-SWT-19 key, which is what lets 2,009 legacy messages
// and 26 existing thread rows keep their identity with no migration. Building it
// with ThreadKey would make the assertion tautological.
const rsLegacyKey = "upwork_crm:" + rsClient + ":" + rsChannel

func TestNormalizeCommunication_RoomSource_BothColumns(t *testing.T) {
	// Fixture validity, hard-failed: the two room ids must DIFFER, or "observed
	// wins" and "dispatched is read" are the same assertion and neither is
	// tested. IK's named landmine from this exact matcher — a fixture whose two
	// sides are the same string tests nothing.
	if rsObservedRoom == rsDispatchedRoom {
		t.Fatalf("fixture invalid: the observed and dispatched room ids are the same string (%q), so no case below "+
			"can distinguish which column was read", rsObservedRoom)
	}

	t.Run("only upwork_room_id -> roomed on that value", func(t *testing.T) {
		nm, err := upworkcrm.NormalizeCommunication(rsRaw(t, strp(rsObservedRoom), nil))
		if err != nil {
			t.Fatalf("NormalizeCommunication: %v", err)
		}
		assertRoomed(t, nm.ThreadKey, rsObservedRoom)
	})

	t.Run("only send_room_id -> roomed on that value", func(t *testing.T) {
		// THE case a one-column implementation gets wrong. On the production
		// source this is the majority of roomed OUTBOUND rows (136 of 220 as of
		// 2026-08-26), and outbound is the only direction confirmUpworkDelivery
		// runs for — so reading upwork_room_id alone leaves the matcher looking
		// at the legacy thread while the write path still looks correct.
		nm, err := upworkcrm.NormalizeCommunication(rsRaw(t, nil, strp(rsDispatchedRoom)))
		if err != nil {
			t.Fatalf("NormalizeCommunication: %v", err)
		}
		if nm.ThreadKey == rsLegacyKey {
			t.Fatalf("thread_key = %q, the LEGACY key: send_room_id was not read. This is the one-column bug — "+
				"the room a send was DISPATCHED to is recorded in send_room_id, not upwork_room_id, and the "+
				"resulting keys are well-formed, so nothing anywhere errors", nm.ThreadKey)
		}
		assertRoomed(t, nm.ThreadKey, rsDispatchedRoom)
	})

	t.Run("both set -> observed (upwork_room_id) wins, no error", func(t *testing.T) {
		// The columns are disjoint per row today, so this is a tiebreak for a
		// case that does not exist — written and tested anyway because leaving
		// precedence to struct field order is how the next reader learns the
		// wrong rule (SPEC decision 3). Observed wins because it is ground truth
		// about where the message actually is on Upwork, which is what the
		// matcher is trying to identify.
		nm, err := upworkcrm.NormalizeCommunication(rsRaw(t, strp(rsObservedRoom), strp(rsDispatchedRoom)))
		if err != nil {
			t.Fatalf("both columns set must NOT be an error (they are merely disjoint today): %v", err)
		}
		assertRoomed(t, nm.ThreadKey, rsObservedRoom)
	})

	t.Run("neither -> the byte-identical legacy key", func(t *testing.T) {
		nm, err := upworkcrm.NormalizeCommunication(rsRaw(t, nil, nil))
		if err != nil {
			t.Fatalf("NormalizeCommunication: %v", err)
		}
		if nm.ThreadKey != rsLegacyKey {
			t.Errorf("thread_key = %q, want the unchanged legacy key %q. A room-less row must keep TODAY's key "+
				"byte for byte: 2,009 of the 2,441 ops messages (as of 2026-08-26) are pre-2026-07-21 history "+
				"whose source rows no longer exist, and re-keying them would need a migration for no benefit",
				nm.ThreadKey, rsLegacyKey)
		}
		ref, err := upworkcrm.ParseThreadKey(nm.ThreadKey)
		if err != nil {
			t.Fatalf("the legacy key must still parse: %v", err)
		}
		if ref.Roomed {
			t.Errorf("ParseThreadKey(%q).Roomed = true; a row the source gave no room for is NOT roomed. "+
				"Rooms are never inferred — not from clients.upwork_room_id, not from the client's other "+
				"messages (SPEC §3): for the multi-room client that inference silently merges two conversations",
				nm.ThreadKey)
		}
	})
}

// Criterion 3's last sentence: an empty string is treated as absent, exactly as
// a missing JSON key is. Both spellings occur — pre-2026-07-11 raw rows lack the
// key entirely, and nothing guarantees a present-but-blank value never appears.
// Getting this wrong yields `upwork_crm:{client}:room:` — a key ParseThreadKey
// REJECTS, which makes the delivery permanently unconfirmable.
func TestNormalizeCommunication_RoomSource_EmptyStringIsAbsent(t *testing.T) {
	cases := []struct {
		name                 string
		upworkRoom, sendRoom *string
		wantKey              string
		wantRoomed           bool
		wantRoomIDWhenRoomed string
	}{
		{"both empty strings", strp(""), strp(""), rsLegacyKey, false, ""},
		{"both keys absent", nil, nil, rsLegacyKey, false, ""},
		{"observed empty, dispatched set", strp(""), strp(rsDispatchedRoom),
			"upwork_crm:" + rsClient + ":room:" + rsDispatchedRoom, true, rsDispatchedRoom},
		{"observed set, dispatched empty", strp(rsObservedRoom), strp(""),
			"upwork_crm:" + rsClient + ":room:" + rsObservedRoom, true, rsObservedRoom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nm, err := upworkcrm.NormalizeCommunication(rsRaw(t, tc.upworkRoom, tc.sendRoom))
			if err != nil {
				t.Fatalf("NormalizeCommunication: %v", err)
			}
			if nm.ThreadKey != tc.wantKey {
				t.Errorf("thread_key = %q, want %q", nm.ThreadKey, tc.wantKey)
			}
			ref, err := upworkcrm.ParseThreadKey(nm.ThreadKey)
			if err != nil {
				t.Fatalf("the normalizer produced a key its own parser rejects (%q): %v — an empty room column "+
					"must be treated as ABSENT, not as a room id", nm.ThreadKey, err)
			}
			if ref.Roomed != tc.wantRoomed {
				t.Errorf("Roomed = %v, want %v", ref.Roomed, tc.wantRoomed)
			}
			if tc.wantRoomed && ref.RoomID != tc.wantRoomIDWhenRoomed {
				t.Errorf("RoomID = %q, want %q", ref.RoomID, tc.wantRoomIDWhenRoomed)
			}
		})
	}
}

// Criterion 4: the same raw row normalizes to identical output on repeated
// calls, INCLUDING a roomed one. normalize_test.go already covers the unroomed
// shape; the room read adds a branch, and a branch is where determinism goes.
func TestNormalizeCommunication_RoomedIsDeterministic(t *testing.T) {
	raw := rsRaw(t, nil, strp(rsDispatchedRoom))
	a, err := upworkcrm.NormalizeCommunication(raw)
	if err != nil {
		t.Fatalf("NormalizeCommunication (a): %v", err)
	}
	b, err := upworkcrm.NormalizeCommunication(raw)
	if err != nil {
		t.Fatalf("NormalizeCommunication (b): %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("roomed normalization is not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}

func assertRoomed(t *testing.T, key, wantRoom string) {
	t.Helper()
	ref, err := upworkcrm.ParseThreadKey(key)
	if err != nil {
		t.Fatalf("ParseThreadKey(%q): %v", key, err)
	}
	if !ref.Roomed {
		t.Fatalf("thread_key %q is not roomed; want the room %q", key, wantRoom)
	}
	if ref.RoomID != wantRoom {
		t.Errorf("room id = %q, want %q — the WRONG column was read", ref.RoomID, wantRoom)
	}
	if ref.ClientID != rsClient {
		t.Errorf("client id = %q, want %q", ref.ClientID, rsClient)
	}
	if want := "upwork_crm:" + rsClient + ":room:" + wantRoom; key != want {
		t.Errorf("thread_key = %q, want %q", key, want)
	}
}
