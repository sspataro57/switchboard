package upworkcrm

import (
	"errors"
	"fmt"
	"strings"
)

// Thread keys for Upwork, in ONE spelling (SWT-19).
//
// Two shapes, and the difference is a parse rather than a guess about what the
// third segment contains:
//
//	roomed    upwork_crm:{client_id}:room:{room_id}
//	unroomed  upwork_crm:{client_id}:{channel}        <- byte-identical to the pre-SWT-19 key
//
// The `room:` tag is the point. The obvious alternative —
// `upwork_crm:{client}:{room_id}`, with the legacy `:upwork` as the fallback —
// makes the two forms distinguishable only by comparing the third segment against
// a magic literal, and room ids may contain anything. That is the SWT-13
// canonicalization landmine in yet another costume: two spellings of one rule,
// disagreeing silently. Segment COUNT separates them here, which no room id can
// forge.
//
// Why unroomed stays byte-identical: 2,009 of the ops corpus's 2,441 upwork
// messages have no room the source ever named (pre-2026-07-21 history plus 2
// API-era stragglers). Keeping their key unchanged means they need no re-key, no
// migration and no cleanup, and the 26 existing thread rows keep their identity,
// participants and FK edges. Only the 432 roomed rows move.
//
// EVERY producer and consumer goes through this file — normalize.go, sink.go's
// matcher, drafts/store.go, draft_delivery's validator. Do NOT re-spell the
// format in SQL. That is why the matcher does its client/room scoping in Go
// (sink.go): "any roomed key of this client" is not an equality, and writing it
// as a LIKE or split_part would put a second spelling in the database. This repo
// has paid for a second spelling four times; a structural test now fails any SQL
// that tries.
const threadKeyRoomTag = "room"

// ThreadRef is a parsed thread key.
//
// Roomed reports whether the SOURCE named a room for this message, in either of
// the two room columns. It is never inferred: see normalize.go on why filling a
// room in from clients.upwork_room_id or from the client's other messages is
// refused.
type ThreadRef struct {
	ClientID string
	RoomID   string // empty unless Roomed
	Channel  string // empty when Roomed; the communications.channel value otherwise
	Roomed   bool
}

// ThreadKey builds the canonical key. A non-empty roomID wins; otherwise the
// channel forms the legacy key.
//
// The channel is sanitized because it is the ONE remaining way this system could
// emit a key its own parser rejects — silently, at every station: the matcher
// returns nil, drafts skips the thread, draft_delivery refuses the target, and
// nothing logs. Two shapes do it: an EMPTY channel yields a trailing-colon key
// that ParseThreadKey rejects outright, and a channel CONTAINING A COLON is
// worse than rejected — `ThreadKey(c, "", "room:x")` would round-trip as ROOMED
// on room "x", silently inventing a room the source never named.
//
// Not reachable from production today (channel is the constant 'upwork', and a
// dry run over all 2,442 rows produced zero unparseable keys), so this is
// hardening, not a fix. It costs two lines and removes the only path by which
// the producer and the parser can disagree.
func ThreadKey(clientID, roomID, channel string) string {
	if roomID != "" {
		return Provider + ":" + clientID + ":" + threadKeyRoomTag + ":" + roomID
	}
	return Provider + ":" + clientID + ":" + sanitizeChannel(channel)
}

// sanitizeChannel keeps a channel value inside one key segment. A colon is
// replaced rather than rejected because NormalizeCommunication has no way to
// fail a single message without failing the whole normalize run, and losing one
// message's key precision beats losing the pass.
func sanitizeChannel(channel string) string {
	if channel == "" {
		return "unknown"
	}
	return strings.ReplaceAll(channel, ":", "_")
}

// ClientThreadPrefix is the "all threads of this client" prefix, built in Go so
// that a caller can filter on it in SQL by BIND PARAMETER rather than by
// spelling the format into a query string.
//
// The distinction is the whole point. A query that takes this value as $1 and
// matches LIKE $1 || '%' keeps one spelling. A query that concatenates the
// provider literal and the separators inline is a second spelling, and the two
// drift silently the day the format changes. A structural test fails the latter.
// Note what that test actually sees: raw string literals anywhere (including a
// backticked example inside a comment — that is how it caught two instances
// while this ticket was being written), but NOT plain // comment lines, which
// are skipped on purpose so prose can quote the old spelling to explain why it
// is gone.
func ClientThreadPrefix(clientID string) string {
	return Provider + ":" + clientID + ":"
}

// ParseThreadKey is the only reader of the format. Errors name the offending
// part, because a target_ref that does not parse is now permanently
// unconfirmable (the matcher excludes it) and "invalid key" would send the
// reader to the wrong place.
func ParseThreadKey(key string) (ThreadRef, error) {
	if key == "" {
		return ThreadRef{}, errors.New("empty thread key")
	}
	// SplitN with 4 so a room id keeps any embedded colons — room ids are opaque
	// and this function must not corrupt one by over-splitting.
	parts := strings.SplitN(key, ":", 4)
	if len(parts) < 3 {
		return ThreadRef{}, fmt.Errorf("thread key %q has %d segments, want at least 3 (upwork_crm:{client}:{channel})", key, len(parts))
	}
	if parts[0] != Provider {
		return ThreadRef{}, fmt.Errorf("thread key %q has provider %q, want %q", key, parts[0], Provider)
	}
	if parts[1] == "" {
		return ThreadRef{}, fmt.Errorf("thread key %q has an empty client id", key)
	}
	if len(parts) == 4 {
		if parts[2] != threadKeyRoomTag {
			return ThreadRef{}, fmt.Errorf("thread key %q has 4 segments so its third must be %q, got %q", key, threadKeyRoomTag, parts[2])
		}
		if parts[3] == "" {
			return ThreadRef{}, fmt.Errorf("thread key %q is roomed but its room id is empty", key)
		}
		return ThreadRef{ClientID: parts[1], RoomID: parts[3], Roomed: true}, nil
	}
	if parts[2] == "" {
		return ThreadRef{}, fmt.Errorf("thread key %q has an empty channel", key)
	}
	return ThreadRef{ClientID: parts[1], Channel: parts[2]}, nil
}

// SameConversation reports whether an observed message's thread and a delivery's
// target_ref may refer to the same conversation.
//
// THE RULE: a room MISMATCH is the only thing that excludes. An unknown room
// excludes nothing.
//
//	                  delivery roomed          delivery unroomed
//	message roomed    same room only           yes
//	message unroomed  yes                      yes
//
// The bottom-left cell is the one that looks wrong and is not. API-era outbound
// traffic is 98.9% roomed (186 of 188) — the send path is healthy and records
// its room in send_room_id — but 576 outbound rows are unroomed pre-2026-07-21
// history, 2 API-era rows carry no room at all, and `--all` replays that whole
// history through the matcher. Refusing that cell would make those permanently
// unconfirmable, silently, which is a worse bug than the one this ticket fixes.
// It is a LEGACY TOLERANCE, not an accommodation of a broken send path.
//
// Describe the result honestly: room-scoped for API-era traffic in both
// directions, client-wide for pre-2026-07-21 history. Not "room matching" flat —
// SWT-18 called its change that and was wrong on production data.
func SameConversation(message, delivery ThreadRef) bool {
	if message.ClientID != delivery.ClientID {
		return false
	}
	if message.Roomed && delivery.Roomed {
		return message.RoomID == delivery.RoomID
	}
	return true
}
