package provider

import (
	"context"
	"sync"
	"time"
)

// Router picks the lane for one request and says why (SWT-21).
//
// It is the ONLY place the two clients meet, and it deliberately offers no
// fallback. "Try local, fall back to the configured provider on error" is the
// well-intentioned implementation a contributor will reach for, and it is the
// bug this whole ticket exists to prevent: it turns a slow local model into a
// silent hosted one, on precisely the content that must never go there. There is
// no code path here that hands restricted content to the general client.
//
// The pure decision lives in locality.go. This file adds exactly one thing the
// pure file cannot do: ask whether the local lane is answering right now.
type Router struct {
	general Client
	local   Client

	// probeTTL keeps a busy local box from being probed once per message. The
	// local lane is a 4B model at low priority on a machine that stays usable as
	// a desktop, so "briefly not answering" is normal operation, not news.
	probeTTL time.Duration

	mu         sync.Mutex
	lastProbe  time.Time
	lastResult Availability
}

// Reason is a short, stable string for the audit trail and the report. Stable
// because operators and tests both match on it; short because it lands in a
// column, not a log line.
type Reason string

// The strings are the SPEC's vocabulary (criteria 20-21), not a restatement of
// the Go identifiers. `no_local_provider` and `local_endpoint_not_private` say
// what an operator must DO — set the variable, or fix the one they already set —
// where "absent" and "not_local" would have made a typo'd URL and a missing one
// read the same at 3am.
const (
	ReasonGeneralLane Reason = "general_lane"
	// ReasonGeneralAbsent: general content, no hosted client configured. Not a
	// locality refusal — nothing was restricted — but it takes the same
	// (nil, skip) shape so no caller needs a second rule.
	ReasonGeneralAbsent    Reason = "no_general_provider"
	ReasonLocalReady       Reason = "local_ready"
	ReasonLocalAbsent      Reason = "no_local_provider"
	ReasonLocalNotLocal    Reason = "local_endpoint_not_private"
	ReasonLocalUnreachable Reason = "local_unreachable"

	// ReasonUnclassifiedError is not a ROUTING outcome: the router permitted the
	// lane and the call itself failed. It shares this vocabulary because it lands
	// in the same `avail_reason` field and the report groups by that one column —
	// and because the distinction it draws is the load-bearing one. An
	// unreachable box is a busy 4B model at low priority, which is normal
	// operation; an unclassified error is a broken adapter, which is news. Merge
	// them and criterion 18's ratio can never be computed.
	ReasonUnclassifiedError Reason = "unclassified_error"
)

// probeTimeout bounds one health check. Short on purpose: this asks "are you
// there", not "do some work".
const probeTimeout = 2 * time.Second

// NewRouter builds a router. Either client may be nil: a deployment with no
// local model is the state this ticket ships into, and a deployment with no
// general client is the end state for triage. Both must behave sensibly.
func NewRouter(general, local Client, probeTTL time.Duration) *Router {
	if probeTTL <= 0 {
		probeTTL = 30 * time.Second
	}
	return &Router{general: general, local: local, probeTTL: probeTTL}
}

// Route returns the client to use, the decision, and the reason.
//
// A DecideSkip always comes back with a nil Client. The caller cannot
// accidentally use a returned client it was told not to use, because there
// isn't one.
func (r *Router) Route(ctx context.Context, c Class) (Client, Decision, Reason) {
	if c == ClassGeneral {
		// Only a POSITIVELY general class takes the hosted lane. Spelled
		// `c != ClassRestricted` this handed an unrecognised value — a future
		// enum member, a typo, a zero-value struct built without a constructor —
		// straight to the hosted client. Same fail-open bug as Decide's, one
		// function along, which is why both now switch on the permitted case.
		//
		// General content is otherwise unaffected: this ticket restricts, it
		// never widens.
		//
		// A nil general client is a SKIP, not an allow. Every caller dereferences
		// the returned client immediately, and since criterion 21 made
		// OPENAI_API_KEY optional, "no hosted client" is a supported triage
		// configuration rather than a misconfiguration — so this is reachable and
		// would be a nil dereference. Returning the same (nil, skip) shape as the
		// restricted refusals keeps ONE rule for callers: a skip never comes with
		// a client, and a client is never nil.
		if r.general == nil {
			return nil, DecideSkip, ReasonGeneralAbsent
		}
		return r.general, DecideAllow, ReasonGeneralLane
	}

	avail := AvailabilityOf(r.local)
	if avail == AvailReady {
		// Only now is a probe worth its round trip: the client exists and its
		// endpoint is local, so the only open question is whether it answers.
		avail = r.probe(ctx)
	}

	if Decide(c, avail) == DecideAllow {
		return r.local, DecideAllow, ReasonLocalReady
	}
	switch avail {
	case AvailAbsent:
		return nil, DecideSkip, ReasonLocalAbsent
	case AvailNotLocal:
		return nil, DecideSkip, ReasonLocalNotLocal
	default:
		return nil, DecideSkip, ReasonLocalUnreachable
	}
}

// probe asks the local client whether it can serve, caching the answer for
// probeTTL.
//
// A local client that does NOT implement Prober is UNREACHABLE, not ready.
//
// I argued the other way first — that locality is the security property and
// availability is separate, so refusing an adapter for lacking an optional
// method makes Prober mandatory by the back door. The SPEC is right and that
// argument was wrong: the whole design principle here is that a declaration is
// not evidence. An adapter that says "I am local" and offers no way to check is
// exactly the case where the declaration is all there is, and this boundary does
// not accept declarations as proof anywhere else either.
//
// The practical consequence is the same trap criterion 10 describes: a test fake
// that declares a local endpoint and skips Prober will now cause its suite to
// SKIP rather than exercise its subject. That is the correct direction — it
// fails visibly instead of passing hollowly — but the fakes need a Probe.
func (r *Router) probe(ctx context.Context) Availability {
	p, ok := r.local.(Prober)
	if !ok {
		return AvailUnreachable
	}

	r.mu.Lock()
	if !r.lastProbe.IsZero() && time.Since(r.lastProbe) < r.probeTTL {
		cached := r.lastResult
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()

	// The probe gets its OWN short deadline rather than inheriting the pass's.
	// The local lane is a low-priority model on a machine kept usable as a
	// desktop, so a box that accepts a connection and then thinks about it is
	// normal — and without this, one such box wedges the entire pass behind a
	// health check.
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	err := p.Probe(pctx)
	cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastProbe = time.Now()
	if err != nil {
		r.lastResult = AvailUnreachable
	} else {
		r.lastResult = AvailReady
	}
	return r.lastResult
}

// NOTE: there is deliberately no SkipError type here.
//
// One was written — a struct wrapping ErrUnavailable so a caller could classify
// a refusal with errors.Is — and nothing ever constructed it. A refusal is not
// an error in this design: Route returns (nil, DecideSkip, Reason) and the
// caller records a skip. An unused error type sitting in the boundary file would
// be read as a live path by the next person to open it.
