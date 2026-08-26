package upworkcrm_test

// Unit tests for the ONE spelling of the Upwork thread key (SWT-19,
// docs/tickets/upwork-room-identity_SPEC.md §1/§2, acceptance criteria 1 and 2).
//
// ZERO I/O by contract: criterion 1 requires ThreadKey/ParseThreadKey to be pure
// (no context.Context, no *pgxpool.Pool) so they run under plain `go test ./...`.
// That is not decoration — every producer and consumer of the key goes through
// these two functions precisely so the format is never re-spelled in SQL, and a
// format function that needed a database could not be called from the places
// that need it (the matcher's Go-side scoping, draft_delivery's validator).
//
//	roomed    upwork_crm:{client_id}:room:{room_id}
//	unroomed  upwork_crm:{client_id}:{channel}      <- byte-identical to today's key
//
// GREENFIELD NOTE: threadkey.go is being written on the main thread as these land.
// Until it exists this file compile-FAILs on undefined ThreadKey / ParseThreadKey /
// ThreadRef — the expected red state, not something to work around.
//
// Imposed surface (SPEC §2):
//
//	type ThreadRef struct { ClientID, RoomID, Channel string; Roomed bool }
//	func ThreadKey(clientID, roomID, channel string) string
//	func ParseThreadKey(key string) (ThreadRef, error)

import (
	"os"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/upworkcrm"
)

const (
	tkClient  = "e2ef9b65-9813-4d79-ac10-0e1813f788ff" // the real two-room client (fact 7)
	tkRoomA   = "room_1a2b3c4d5e"
	tkRoomB   = "room_9f8e7d6c5b"
	tkChannel = "upwork" // the ONLY value communications.channel has ever held (fact 1)
)

// Criterion 1: round trip. For every roomed and unroomed input,
// ParseThreadKey(ThreadKey(...)) returns the inputs.
//
// The roomed and unroomed cases for one client MUST produce different keys —
// asserted below rather than assumed. A key builder whose two shapes collide
// would make every "roomed vs unroomed" assertion in this ticket vacuous, which
// is IK's named landmine ("a fixture whose two sides are the same string tests
// nothing") in its key-format costume.
func TestThreadKey_RoundTrip(t *testing.T) {
	cases := []struct {
		name              string
		clientID, roomID  string
		channel           string
		wantKey           string
		wantRoomed        bool
		wantRoom, wantChn string
	}{
		{
			name: "roomed", clientID: tkClient, roomID: tkRoomA, channel: tkChannel,
			wantKey: "upwork_crm:" + tkClient + ":room:" + tkRoomA,
			// The channel is DROPPED by the roomed form on purpose: it is a
			// constant in the source and carries no information, while the room
			// does. Parsing a roomed key must not resurrect it.
			wantRoomed: true, wantRoom: tkRoomA, wantChn: "",
		},
		{
			name: "roomed, second room of the SAME client", clientID: tkClient, roomID: tkRoomB, channel: tkChannel,
			wantKey:    "upwork_crm:" + tkClient + ":room:" + tkRoomB,
			wantRoomed: true, wantRoom: tkRoomB, wantChn: "",
		},
		{
			// Byte-identical to the pre-SWT-19 key. 2,009 of the 2,441 ops
			// messages stay on this exact spelling, so a change here is a
			// silent re-key of the whole legacy corpus (SPEC decision 1).
			name: "unroomed keeps today's key", clientID: tkClient, roomID: "", channel: tkChannel,
			wantKey:    "upwork_crm:" + tkClient + ":" + tkChannel,
			wantRoomed: false, wantRoom: "", wantChn: tkChannel,
		},
		{
			// Criterion 2's last clause: a room id containing a colon survives
			// the round trip intact. Room ids are opaque; over-splitting one
			// corrupts it into a key that matches nothing.
			name: "room id containing a colon", clientID: tkClient, roomID: "room_ab:cd:ef", channel: tkChannel,
			wantKey:    "upwork_crm:" + tkClient + ":room:room_ab:cd:ef",
			wantRoomed: true, wantRoom: "room_ab:cd:ef", wantChn: "",
		},
	}

	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := upworkcrm.ThreadKey(tc.clientID, tc.roomID, tc.channel)
			if key != tc.wantKey {
				t.Fatalf("ThreadKey(%q,%q,%q) = %q, want %q", tc.clientID, tc.roomID, tc.channel, key, tc.wantKey)
			}
			ref, err := upworkcrm.ParseThreadKey(key)
			if err != nil {
				t.Fatalf("ParseThreadKey(%q): %v", key, err)
			}
			if ref.ClientID != tc.clientID {
				t.Errorf("ClientID = %q, want %q", ref.ClientID, tc.clientID)
			}
			if ref.Roomed != tc.wantRoomed {
				t.Errorf("Roomed = %v, want %v — roomed-ness must be a PARSE (segment count + the `room` tag), "+
					"never a guess about what the third segment contains", ref.Roomed, tc.wantRoomed)
			}
			if ref.RoomID != tc.wantRoom {
				t.Errorf("RoomID = %q, want %q", ref.RoomID, tc.wantRoom)
			}
			if ref.Channel != tc.wantChn {
				t.Errorf("Channel = %q, want %q", ref.Channel, tc.wantChn)
			}
			if prev, dup := seen[key]; dup {
				t.Errorf("key %q is also produced by case %q: two distinct inputs collide on one key, so every "+
					"assertion distinguishing them is vacuous", key, prev)
			}
			seen[key] = tc.name
		})
	}
}

// Criterion 2: every rejection, each with an error naming the offending part.
// "invalid thread key" would send the reader to the wrong place — and a
// target_ref that does not parse is now PERMANENTLY unconfirmable (§4 excludes
// it from the candidate set), so the message is the only diagnosis anyone gets.
func TestParseThreadKey_Rejections(t *testing.T) {
	cases := []struct {
		name, key string
		// a substring the error must contain, naming the offending part
		wantIn string
	}{
		{"wrong provider prefix", "upwork:" + tkClient + ":" + tkChannel, "provider"},
		{"slack's provider prefix", "slack:T1:C1", "provider"},
		{"empty client id", "upwork_crm::" + tkChannel, "client"},
		{"3 segments with an empty third", "upwork_crm:" + tkClient + ":", "channel"},
		{"4 segments whose third is not `room`", "upwork_crm:" + tkClient + ":rooms:" + tkRoomA, "room"},
		{"4 segments with an empty room id", "upwork_crm:" + tkClient + ":room:", "room"},
		{"two segments", "upwork_crm:" + tkClient, "segment"},
		{"one segment", "upwork_crm", "segment"},
		{"empty key", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := upworkcrm.ParseThreadKey(tc.key)
			if err == nil {
				t.Fatalf("ParseThreadKey(%q) = %+v, nil error; want a rejection. An unparseable target_ref is "+
					"excluded from the matcher's candidate set, so accepting a malformed key here produces a "+
					"delivery that can never be confirmed and never errors", tc.key, ref)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("ParseThreadKey(%q) error = %q, want it to name the offending part (%q)", tc.key, err.Error(), tc.wantIn)
			}
			if ref != (upworkcrm.ThreadRef{}) {
				t.Errorf("ParseThreadKey(%q) returned %+v alongside an error; a rejected key must yield the zero ThreadRef "+
					"so a caller that ignores err cannot act on half-parsed identity", tc.key, ref)
			}
		})
	}
}

// A parsed key re-spells to itself: ThreadKey(ParseThreadKey(k)) == k. This is
// what makes "store the canonical spelling, not the caller's" (criterion 7) a
// no-op for an already-canonical target instead of a silent rewrite.
func TestParseThreadKey_ReSpellsToItself(t *testing.T) {
	for _, key := range []string{
		"upwork_crm:" + tkClient + ":room:" + tkRoomA,
		"upwork_crm:" + tkClient + ":" + tkChannel,
		"upwork_crm:" + tkClient + ":room:room_ab:cd",
	} {
		ref, err := upworkcrm.ParseThreadKey(key)
		if err != nil {
			t.Fatalf("ParseThreadKey(%q): %v", key, err)
		}
		if got := upworkcrm.ThreadKey(ref.ClientID, ref.RoomID, ref.Channel); got != key {
			t.Errorf("ThreadKey(ParseThreadKey(%q)) = %q, want the input unchanged", key, got)
		}
	}
}

// Criterion 1's purity clause, mechanically. A source scan rather than a
// convention: the reason ThreadKey/ParseThreadKey must not take a context or a
// pool is that draft_delivery's VALIDATE stage and the matcher's in-Go scoping
// both call them, and validate runs before any handler touches the database.
// The day someone "just adds a lookup" here, the format quietly becomes
// unavailable to half its callers.
func TestThreadKeyIsPure(t *testing.T) {
	src, err := os.ReadFile("threadkey.go")
	if err != nil {
		t.Fatalf("read threadkey.go: %v", err)
	}
	body := string(src)
	for _, banned := range []string{"context.Context", "pgxpool", "database/sql"} {
		if strings.Contains(body, banned) {
			t.Errorf("threadkey.go mentions %q: the key format must be a pure function of its arguments "+
				"(criterion 1), reachable from draft_delivery's validate stage, which runs before any pool is touched", banned)
		}
	}
}

// Producer/consumer agreement over the whole realistic input space, table-driven
// so it is deterministic rather than a fuzz.
//
// This is the unit-layer form of a property the implementer verified against the
// live corpus with a read-only dry run: over every production communications row
// the normalizer produced ZERO keys ParseThreadKey could not read, and zero
// normalize failures. That run is evidence, not a test — it cannot be re-run in
// CI and the corpus changes every 15 minutes. The cases below cover the same
// ground with the shapes the source actually emits: uuid client ids,
// `room_<hex>` rooms, the one channel value that exists, and the awkward cases
// (a colon inside a room id, a client id that is not a uuid).
func TestThreadKey_EveryProducedKeyParses(t *testing.T) {
	clients := []string{
		"e2ef9b65-9813-4d79-ac10-0e1813f788ff",
		"43431d4c-d34a-43f2-b49b-2dc70c52c096", // the three-room client
		"itest-not-a-uuid",
	}
	rooms := []string{
		"", // unroomed
		"room_1a2b3c4d5e",
		"room_ff00ff00ff",
		"room_with:a:colon",
	}
	channels := []string{tkChannel, "chat", "email"}

	produced := map[string]string{}
	for _, c := range clients {
		for _, r := range rooms {
			for _, ch := range channels {
				key := upworkcrm.ThreadKey(c, r, ch)
				ref, err := upworkcrm.ParseThreadKey(key)
				if err != nil {
					t.Errorf("ThreadKey(%q,%q,%q) produced %q, which its own parser rejects: %v — a producer and "+
						"a consumer of ONE format that disagree is the whole failure class this file exists for",
						c, r, ch, key, err)
					continue
				}
				if ref.ClientID != c {
					t.Errorf("%q round-tripped to client %q, want %q", key, ref.ClientID, c)
				}
				if ref.Roomed != (r != "") {
					t.Errorf("%q round-tripped to Roomed=%v, want %v", key, ref.Roomed, r != "")
				}
				if ref.RoomID != r {
					t.Errorf("%q round-tripped to room %q, want %q", key, ref.RoomID, r)
				}
				// A roomed key drops the channel; an unroomed one keeps it.
				wantChn := ch
				if r != "" {
					wantChn = ""
				}
				if ref.Channel != wantChn {
					t.Errorf("%q round-tripped to channel %q, want %q", key, ref.Channel, wantChn)
				}
				// Distinct inputs must not collide. A roomed key ignores the
				// channel, so those collisions are expected and keyed out.
				id := c + "\x00" + r
				if r == "" {
					id += "\x00" + ch
				}
				if prev, dup := produced[key]; dup && prev != id {
					t.Errorf("key %q is produced by two distinct inputs (%q and %q)", key, prev, id)
				}
				produced[key] = id
			}
		}
	}
}
