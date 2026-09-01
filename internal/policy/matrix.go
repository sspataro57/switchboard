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

// MCPTransportPrefix is the marker the MCP server puts in front of its worker
// identity. Defined here, in the lowest layer that needs it, because two layers
// do and they must not drift: HumanActor strips it before the human check, and
// executor.ViaMCP tests for it so a handler can narrow what it will do over that
// transport. Any future transport wrapper belongs here too.
const MCPTransportPrefix = "mcp:"

// sendShaped tools transition a delivery toward the outside world, so they need
// the channel/rate snapshot. Both belong here; they differ only in whether the
// kill switch can stop them — see freezeGated.
var sendShaped = map[string]bool{"send_delivery": true, "mark_delivery_sent": true}

// freezeGated is the subset the kill switch can actually prevent: an actual
// send. The switch is for switchboard — it governs what switchboard itself puts
// in front of a client. mark_delivery_sent transmits nothing; it records a send
// that already happened, either through the Slack Web connector's own token gate
// or by hand. Freezing it would not un-send anything, it would only leave a
// message that is provably in a client channel with no delivery row saying so,
// and would block resolving a stuck 'sending' row a human verified.
//
// Keep the two maps distinct. Collapsing them by removing mark_delivery_sent
// from sendShaped would ALSO drop its rate limit and its whole channel branch
// (this function returns early for anything not sendShaped), widening the tool
// while appearing to narrow it. matrix_test.go pins that corner.
var freezeGated = map[string]bool{"send_delivery": true}

// humanOnly tools require a human actor prefix.
var humanOnly = map[string]bool{
	"update_delivery": true, "approve_delivery": true, "send_delivery": true,
	"mark_delivery_sent": true, "prefill_delivery": true, "set_sending_frozen": true,
	"approve_plan_import": true, "reject_plan_import": true, "apply_plan_import": true,
	// mark_delivery_failed asserts a human looked at the conversation — Slack, or
	// Upwork since SWT-19 — and the message is NOT there. It is human-only but
	// deliberately not send-shaped: it moves a row AWAY from the world, so
	// neither the kill switch nor the rate limit has any claim on it. Adding a
	// channel therefore needs no change here, which is why the channel check
	// lives in the handler.
	"mark_delivery_failed": true,
	// SWT-17: capture rules decide which project a captured message belongs to,
	// so they are the funnel's steering. Human-only for the same reason the
	// plan-import verdicts are: an agent that could add a rule could route any
	// client's traffic to any project and then be handed the work. Not
	// send-shaped — nothing leaves the system — so neither the kill switch nor
	// a rate limit has any claim on them.
	"capture_rule_add": true, "capture_rule_set_enabled": true,
}

// snapshotGated tools need the loader (channel/rate/freeze state).
var snapshotGated = sendShaped

// HumanActor reports whether an actor string names a person rather than an
// automated caller. EXPORTED since SWT-20 (criterion 18): draft_delivery's
// room-choice gate asks the same question, and a handler that restated the
// prefixes would drift from Decide's gate invisibly. ONE definition.
func HumanActor(actor string) bool {
	// The MCP adapter prefixes every call with its transport ("mcp:" + worker
	// id), so an interactive session arrives as "mcp:manual:salvo" and would
	// otherwise be refused alongside the workers this gate exists to stop.
	// Strip exactly one such prefix — "mcp:mcp:..." is not a human.
	//
	// Deliberately done here rather than by not prefixing in the adapter: the
	// audit row keeps the full unmodified actor, so which surface triggered a
	// send stays answerable. Any future transport wrapper must be added here.
	actor = strings.TrimPrefix(actor, MCPTransportPrefix)
	for _, p := range []string{"dashboard:", "opsctl:", "manual:"} {
		if strings.HasPrefix(actor, p) {
			return true
		}
	}
	return false
}

// Decide is the pure matrix core over the delivery-gated tools.
func Decide(req Request, snap Snapshot) Decision {
	if humanOnly[req.Tool] && !HumanActor(req.Actor) {
		return Decision{Decision: "deny", Rule: "human_only",
			Reason: fmt.Sprintf("%s requires a human actor (dashboard:/opsctl:/manual:); got %q", req.Tool, req.Actor)}
	}
	if !sendShaped[req.Tool] {
		return Decision{Decision: "allow", Rule: "matrix-human", Reason: "human delivery action"}
	}
	if freezeGated[req.Tool] && snap.SendingFrozen {
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
	case "jira_comment":
		limit := snap.HourlyLimit
		if limit <= 0 {
			limit = 10
		}
		if snap.SentLastHour[snap.Channel] >= limit {
			return Decision{Decision: "deny", Rule: "rate_limit",
				Reason: fmt.Sprintf("channel %s hit the hourly send limit (%d)", snap.Channel, limit)}
		}
		return Decision{Decision: "allow", Rule: "matrix-send", Reason: "jira comment within limits"}
	case "upwork_chat":
		if req.Tool == "mark_delivery_sent" {
			return Decision{Decision: "allow", Rule: "matrix-assisted", Reason: "assisted-tier manual confirmation"}
		}
		return Decision{Decision: "deny", Rule: "channel_assisted",
			Reason: "upwork_chat is assisted: copy/prefill, then mark_delivery_sent"}
	case "slack_reply":
		// SWT-12: promoted from assisted to approve. The connector clicks Send
		// through its bridge after switchboard approval, so send_delivery is no
		// longer denied here — remote-desktopping into the Mac mini to press
		// send is what made the assisted tier unusable. The assisted verbs
		// survive: prefill_delivery as a fallback when the bridge-server is
		// down, and mark_delivery_sent to record a send made through the leaf's
		// own token gate.
		limit := snap.HourlyLimit
		if limit <= 0 {
			limit = 10
		}
		if snap.SentLastHour[snap.Channel] >= limit {
			return Decision{Decision: "deny", Rule: "rate_limit",
				Reason: fmt.Sprintf("channel %s hit the hourly send limit (%d)", snap.Channel, limit)}
		}
		if req.Tool == "mark_delivery_sent" {
			return Decision{Decision: "allow", Rule: "matrix-send", Reason: "manual confirmation"}
		}
		return Decision{Decision: "allow", Rule: "matrix-send", Reason: "slack reply within limits"}
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
	if humanOnly[req.Tool] && !HumanActor(req.Actor) {
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
