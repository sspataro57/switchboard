package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The SWT-8 delivery policy matrix. The CORE (Decide) is a pure function of
// (Request, Snapshot) — kill switch, per-channel hourly rate limit, channel
// tiers, human-only gate. I/O lives in the SnapshotLoader (orchestrator Facts
// pattern). Non-delivery tools fall through to the static allow-list.

// Snapshot is the read-only world the loader gathered for one delivery call.
type Snapshot struct {
	SendingFrozen bool
	SentLastHour  map[string]int
	Channel       string
	HourlyLimit   int
}

// sendShaped tools transition a delivery toward the outside world.
var sendShaped = map[string]bool{"send_delivery": true, "mark_delivery_sent": true}

// humanOnly tools require a human actor prefix.
var humanOnly = map[string]bool{
	"update_delivery": true, "approve_delivery": true, "send_delivery": true,
	"mark_delivery_sent": true, "set_sending_frozen": true,
}

// snapshotGated tools need the loader (channel/rate/freeze state).
var snapshotGated = sendShaped

func humanActor(actor string) bool {
	for _, p := range []string{"dashboard:", "opsctl:", "manual:"} {
		if strings.HasPrefix(actor, p) {
			return true
		}
	}
	return false
}

// Decide is the pure matrix core over the delivery-gated tools.
func Decide(req Request, snap Snapshot) Decision {
	if humanOnly[req.Tool] && !humanActor(req.Actor) {
		return Decision{Decision: "deny", Rule: "human_only",
			Reason: fmt.Sprintf("%s requires a human actor (dashboard:/opsctl:/manual:); got %q", req.Tool, req.Actor)}
	}
	if !sendShaped[req.Tool] {
		return Decision{Decision: "allow", Rule: "matrix-human", Reason: "human delivery action"}
	}
	if snap.SendingFrozen {
		return Decision{Decision: "deny", Rule: "kill_switch",
			Reason: "global kill switch is on: all sending transitions are frozen"}
	}
	switch snap.Channel {
	case "gmail":
		limit := snap.HourlyLimit
		if limit <= 0 {
			limit = 10
		}
		if snap.SentLastHour[snap.Channel] >= limit {
			return Decision{Decision: "deny", Rule: "rate_limit",
				Reason: fmt.Sprintf("channel %s hit the hourly send limit (%d)", snap.Channel, limit)}
		}
		if req.Tool == "mark_delivery_sent" {
			// manual confirmation is the assisted tier's verb, but harmless on gmail
			return Decision{Decision: "allow", Rule: "matrix-send", Reason: "manual confirmation"}
		}
		return Decision{Decision: "allow", Rule: "matrix-send", Reason: "gmail send within limits"}
	case "upwork_chat":
		if req.Tool == "mark_delivery_sent" {
			return Decision{Decision: "allow", Rule: "matrix-assisted", Reason: "assisted-tier manual confirmation"}
		}
		return Decision{Decision: "deny", Rule: "channel_assisted",
			Reason: "upwork_chat is assisted: copy/prefill, then mark_delivery_sent"}
	default:
		return Decision{Decision: "deny", Rule: "channel_not_live",
			Reason: fmt.Sprintf("channel %q has no live send adapter yet", snap.Channel)}
	}
}

// SnapshotLoader gathers the Snapshot for one delivery-gated request.
type SnapshotLoader interface {
	Load(ctx context.Context, req Request) (Snapshot, error)
}

type matrix struct {
	loader   SnapshotLoader
	fallback Checker
}

// NewMatrix wraps the static allow-list: delivery-gated tools go through
// Decide; everything else falls through to the fallback.
func NewMatrix(loader SnapshotLoader, fallback Checker) Checker {
	return &matrix{loader: loader, fallback: fallback}
}

func (m *matrix) Check(ctx context.Context, req Request) (Decision, error) {
	if humanOnly[req.Tool] && !humanActor(req.Actor) {
		return Decide(req, Snapshot{}), nil
	}
	if !snapshotGated[req.Tool] {
		if humanOnly[req.Tool] {
			return Decide(req, Snapshot{}), nil
		}
		return m.fallback.Check(ctx, req)
	}
	snap, err := m.loader.Load(ctx, req)
	if err != nil {
		return Decision{}, fmt.Errorf("load policy snapshot for %s: %w", req.Tool, err)
	}
	return Decide(req, snap), nil
}

// deliveryIDArgs parses the delivery id out of the call args.
func deliveryIDArgs(args json.RawMessage) int64 {
	var a struct {
		DeliveryID int64 `json:"delivery_id"`
	}
	_ = json.Unmarshal(args, &a)
	return a.DeliveryID
}
