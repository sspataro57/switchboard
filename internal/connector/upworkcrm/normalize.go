package upworkcrm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Identity is one person_identities row candidate.
type Identity struct {
	Provider string // upwork_crm | email | upwork_room
	Value    string
}

// NormalizedClient is the canonical projection of one clients row.
type NormalizedClient struct {
	ClientID    string
	DisplayName string
	Identities  []Identity
}

// NormalizedMessage is the canonical projection of one communications row.
type NormalizedMessage struct {
	ThreadKey         string // upwork_crm:{client uuid}:{channel}
	ClientID          string
	Direction         string
	SentAt            time.Time
	BodyText          string
	Subject           string
	Sender            string
	Channel           string
	ExternalMessageID string // verbatim from communications.external_id (invariant 5)
}

type rawClient struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Email        *string `json:"email"`
	UpworkRoomID *string `json:"upwork_room_id"`
}

type rawCommunication struct {
	ID             string    `json:"id"`
	ClientID       string    `json:"client_id"`
	Direction      string    `json:"direction"`
	Channel        string    `json:"channel"`
	Subject        *string   `json:"subject"`
	Body           *string   `json:"body"`
	CommunicatedAt time.Time `json:"communicated_at"`
	Sender         *string   `json:"sender"`
	ExternalID     *string   `json:"external_id"`

	// TWO room columns, and reading one of them is reading none (SWT-19).
	//
	//   upwork_room_id  the room a message was OBSERVED in   (inbound 212, outbound  84)
	//   send_room_id    the room a send was DISPATCHED to    (inbound   0, outbound 136)
	//
	// They are disjoint per row and share an identifier space (room_<hex>; 6
	// values appear in both). send_room_id is written by the send path and
	// nothing else — it agrees with send_requested_at on all 136 rows, with zero
	// disagreements. Nothing in the CRM is broken; it has recorded the room all
	// along.
	//
	// A normalizer reading upwork_room_id alone would file 136 outbound messages
	// under the legacy key WHILE BELIEVING IT HAD READ THE ROOM — 62% of the
	// roomed outbound corpus — and the keys it produced would look perfectly
	// well-formed. That is undetectable at the write site and is this ticket's
	// own bug reproduced inside its own fix. It is also how the 44.7% figure in
	// an early draft of the SPEC came about: one column, measured confidently.
	UpworkRoomID *string `json:"upwork_room_id"`
	SendRoomID   *string `json:"send_room_id"`
}

// roomID is COALESCE(upwork_room_id, send_room_id), spelled once.
//
// Observed wins when both are set. They are disjoint today, so this is a
// tiebreak for a case that does not exist — but leaving it to struct field order
// is how the next reader learns the wrong rule, and observed is the right winner
// because it is ground truth about where the message actually sits on Upwork,
// which is what the matcher is trying to identify.
//
// An empty string counts as absent, exactly like a missing JSON key: the 815
// pre-2026-07-11 raw rows lack the key entirely, and nothing guarantees a
// present-but-blank value never appears.
func (m rawCommunication) roomID() string {
	if v := deref(m.UpworkRoomID); v != "" {
		return v
	}
	return deref(m.SendRoomID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// NormalizeClient is a pure mapper from one raw clients row (verbatim source
// JSON) to its canonical projection. The upwork_crm identity is always
// present; email / upwork_room only when the columns carry values.
func NormalizeClient(raw json.RawMessage) (NormalizedClient, error) {
	var c rawClient
	if err := json.Unmarshal(raw, &c); err != nil {
		return NormalizedClient{}, fmt.Errorf("parse raw client: %w", err)
	}
	if c.ID == "" {
		return NormalizedClient{}, fmt.Errorf("raw client has no id")
	}
	nc := NormalizedClient{
		ClientID:    c.ID,
		DisplayName: c.Name,
		Identities:  []Identity{{Provider: Provider, Value: c.ID}},
	}
	if v := deref(c.Email); v != "" {
		nc.Identities = append(nc.Identities, Identity{Provider: "email", Value: v})
	}
	if v := deref(c.UpworkRoomID); v != "" {
		nc.Identities = append(nc.Identities, Identity{Provider: "upwork_room", Value: v})
	}
	return nc, nil
}

// NormalizeCommunication is a pure mapper from one raw communications row to
// its canonical projection.
func NormalizeCommunication(raw json.RawMessage) (NormalizedMessage, error) {
	var m rawCommunication
	if err := json.Unmarshal(raw, &m); err != nil {
		return NormalizedMessage{}, fmt.Errorf("parse raw communication: %w", err)
	}
	if m.ID == "" || m.ClientID == "" {
		return NormalizedMessage{}, fmt.Errorf("raw communication missing id/client_id")
	}
	return NormalizedMessage{
		// Roomed IFF the source named a room, in one of the two columns. Never
		// inferred: filling it from clients.upwork_room_id, or from the client's
		// other messages, would usually be right for 25 of 26 clients and
		// unfalsifiable — and for the one client with two rooms it would silently
		// merge two conversations, which is the entire reason this ticket exists.
		ThreadKey:         ThreadKey(m.ClientID, m.roomID(), m.Channel),
		ClientID:          m.ClientID,
		Direction:         m.Direction,
		SentAt:            m.CommunicatedAt,
		BodyText:          deref(m.Body),
		Subject:           deref(m.Subject),
		Sender:            deref(m.Sender),
		Channel:           m.Channel,
		ExternalMessageID: deref(m.ExternalID),
	}, nil
}

// IdentityResolver reports the current owner of an identity value.
type IdentityResolver interface {
	OwnerOf(ctx context.Context, provider, value string) (personID int64, ok bool, err error)
}

// ReconcileIdentities decides, with NO auto-merge, which identities to insert
// for personID and which are suspected merges (already owned by a different
// person — surfaced in stats, resolved by dashboard approval later).
func ReconcileIdentities(ctx context.Context, personID int64, ids []Identity, r IdentityResolver) (insert, suspected []Identity, err error) {
	for _, id := range ids {
		owner, ok, err := r.OwnerOf(ctx, id.Provider, id.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve owner of %s:%s: %w", id.Provider, id.Value, err)
		}
		switch {
		case !ok:
			insert = append(insert, id)
		case owner == personID:
			// already ours — no-op
		default:
			suspected = append(suspected, id)
		}
	}
	return insert, suspected, nil
}

// Normalize is the second phase: it reads ONLY raw_source_items (never the
// source database — criterion 7, re-normalize from raw alone) and upserts the
// canonical rows. Clients sort before communications by external_id, so people
// exist before messages reference them.
func Normalize(ctx context.Context, sink *PGSink, cfg Config) (Stats, error) {
	var stats Stats

	accountID, err := sink.EnsureAccount(ctx)
	if err != nil {
		return stats, fmt.Errorf("ensure source account: %w", err)
	}
	runID, err := sink.StartRun(ctx, accountID)
	if err != nil {
		return stats, fmt.Errorf("start sync run: %w", err)
	}
	fail := func(cause error) (Stats, error) {
		_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
		return stats, cause
	}

	items, err := sink.pendingRaw(ctx, accountID, cfg.All)
	if err != nil {
		return fail(fmt.Errorf("list raw items: %w", err))
	}

	for _, it := range items {
		switch {
		case strings.HasPrefix(it.externalID, "clients:"):
			nc, err := NormalizeClient(it.raw)
			if err != nil {
				return fail(fmt.Errorf("normalize %s: %w", it.externalID, err))
			}
			suspected, err := sink.upsertClient(ctx, nc)
			if err != nil {
				return fail(fmt.Errorf("apply %s: %w", it.externalID, err))
			}
			stats.SuspectedMerges += suspected
		case strings.HasPrefix(it.externalID, "communications:"):
			nm, err := NormalizeCommunication(it.raw)
			if err != nil {
				return fail(fmt.Errorf("normalize %s: %w", it.externalID, err))
			}
			if err := sink.upsertMessage(ctx, it.id, nm); err != nil {
				return fail(fmt.Errorf("apply %s: %w", it.externalID, err))
			}
		default:
			return fail(fmt.Errorf("raw item %d has unknown external_id shape %q", it.id, it.externalID))
		}
		if err := sink.markNormalized(ctx, it.id); err != nil {
			return fail(fmt.Errorf("stamp normalized_at for %s: %w", it.externalID, err))
		}
		stats.Normalized++
	}

	if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
		return stats, fmt.Errorf("finish sync run: %w", err)
	}
	return stats, nil
}
