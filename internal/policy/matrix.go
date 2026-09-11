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

// ViaMCPActor reports whether an actor arrived over the MCP transport — the ONE
// spelling of "arrived over MCP". executor.ViaMCP and the mcp_human_only rule
// both call it (SWT-37 V1), so the two layers cannot drift. Case-sensitive and
// anchored: the adapter writes the prefix, a model never does.
func ViaMCPActor(actor string) bool { return strings.HasPrefix(actor, MCPTransportPrefix) }

// sendShaped tools transition a delivery toward the outside world, so they need
// the channel/rate snapshot. Both belong here; they differ only in whether the
// kill switch can stop them — see freezeGated.
var sendShaped = map[string]bool{"send_delivery": true, "mark_delivery_sent": true,
	// SWT-28 (Q1 = b): the calendar auto tier's verb. sendShaped so it
	// consumes the channel's hourly allowance and reaches the channel branch —
	// an allow before the switch would be an allow with no rate limit and no
	// kill switch, which is the auto tier with both of its brakes missing.
	"book_calendar_block": true}

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
var freezeGated = map[string]bool{"send_delivery": true,
	// book_calendar_block actually sends (SWT-28). With no human gate on the
	// auto tier, set_sending_frozen is the ONLY thing that can halt a worker
	// that has decided to book — the operator's stop button.
	"book_calendar_block": true}

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
	// SWT-31: a dismissal is a human judgement recorded as TRAINING DATA
	// (task_dismissals), so the verb is gated on a human actor. task_close
	// stays open — the orchestrator calls it from R1, R8 and the feedback
	// rules — which is the whole reason this is a separate verb. Not
	// send-shaped and not snapshot-gated: nothing leaves the system, so
	// neither the kill switch nor the rate limit has any claim on it (the
	// mark_delivery_failed argument, verbatim).
	"task_dismiss": true,
}

// mcpHumanOnly tools require a human identity WHEN THEY ARRIVE OVER MCP
// (SWT-37 V1, rule mcp_human_only). They cannot be humanOnly: the orchestrator
// calls task_close (R2, R8) and task_mark_delivered (R8), and the Jira
// reconciler closes as ticketstatus:jira — in-process Go with fixed call sites,
// none taking a tool name from a model. The callers a model CAN steer are MCP
// sessions, so this is a transport rule ("this transport's non-human identities
// may not make these transitions"), not a trust boundary: an actor prefix is a
// label. Keep this map apart from humanOnly — folding them stalls the spine.
var mcpHumanOnly = map[string]bool{"task_close": true, "task_mark_delivered": true}

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
	// SWT-37 V1: a worker console (mcp:{client}) may not close or deliver over
	// MCP; every non-MCP caller keeps today's decision. Its own rule string, so
	// an audit tells it apart from human_only.
	if mcpHumanOnly[req.Tool] && ViaMCPActor(req.Actor) && !HumanActor(req.Actor) {
		return Decision{Decision: "deny", Rule: "mcp_human_only",
			Reason: fmt.Sprintf("%s over MCP requires a human session identity (mcp:manual:/mcp:dashboard:/mcp:opsctl:); got %q", req.Tool, req.Actor)}
	}
	// book_calendar_block is denied BY NAME on every channel but calendar,
	// BEFORE the channel switch (SWT-28 criterion 15). Once the verb is
	// sendShaped, any live branch would allow it — and it approves AND sends
	// in one call, so an allow on a gmail row is an agent sending an
	// unapproved client email. Its own rule string on purpose: audits and
	// operators must tell it apart from channel_assisted and channel_not_live.
	if req.Tool == "book_calendar_block" && snap.Channel != "calendar" {
		return Decision{Decision: "deny", Rule: "channel_mismatch",
			Reason: fmt.Sprintf("book_calendar_block only acts on a calendar delivery; this delivery's channel is %q", snap.Channel)}
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
	case "calendar":
		// SWT-28 (Q1 = b): the auto tier. send_delivery (the human two-step)
		// and book_calendar_block (agent-callable) are both live, rate-limited
		// like gmail/jira. mark_delivery_sent stays refused: this channel has
		// a real send path and a reservable id, so it has neither an assisted
		// tier nor a click-may-have-landed window — and that verb is not
		// freeze-gated, so allowing it here would be a route around the auto
		// tier's stop button.
		if req.Tool == "mark_delivery_sent" {
			return Decision{Decision: "deny", Rule: "channel_no_assisted_tier",
				Reason: "calendar has a real send path and a reservable event id; there is nothing to manually confirm"}
		}
		limit := snap.HourlyLimit
		if limit <= 0 {
			limit = 10
		}
		if snap.SentLastHour[snap.Channel] >= limit {
			return Decision{Decision: "deny", Rule: "rate_limit",
				Reason: fmt.Sprintf("channel %s hit the hourly send limit (%d)", snap.Channel, limit)}
		}
		return Decision{Decision: "allow", Rule: "matrix-send", Reason: "calendar booking within limits"}
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
	// SWT-37 V1: deny through Decide, otherwise fall through to the static
	// allow-list so every allowed call keeps its static-default audit row byte
	// for byte. The snapshot loader never runs: nothing here is send-shaped.
	if mcpHumanOnly[req.Tool] {
		if d := Decide(req, Snapshot{}); d.Decision == "deny" {
			return d, nil
		}
		return m.fallback.Check(ctx, req)
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
