package provider

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// The locality boundary (SWT-21): personal content may only be processed by a
// model running locally, enforced here rather than configured per worker.
//
// EVERYTHING IN THIS FILE IS PURE. No context, no pgx, no net/http, no
// os.Getenv. The decision that governs whether a client's mail leaves the
// building is a total function over two small enums, unit-testable with no
// database and no model, in the discipline internal/capture/rules.go
// established. If you need I/O to decide, you are deciding the wrong thing.
//
// WHY IT EXISTS. Triage sends full message bodies to a hosted API, and shadow
// mode does not change that — shadow means "extract everything, create nothing".
// SWT-17 then concentrated the exposure without anyone intending it: the capture
// rules now claim two thirds of the corpus deterministically, so triage's inbox
// is precisely the residue they could not place, which is disproportionately
// personal — bank alerts, a mortgage servicer, an HSA, a medical biller.
//
// WHY IT IS NOT CONFIGURATION. The provider is per-worker config today, so a
// config change — by a person or by a future session — starts sending financial
// mail to a hosted API silently, with no error anywhere.

// Locality is what a Descriptor's endpoint tells us about where content goes.
type Locality int

const (
	// LocalityUnknown is the ZERO VALUE and means "not local". Fail closed: an
	// adapter that forgets to describe itself, or describes something we cannot
	// parse, must never be treated as safe for restricted content. A permissive
	// zero value is how a guard becomes decoration.
	LocalityUnknown Locality = iota
	LocalityLocal
	LocalityRemote
)

// Class is how restricted a piece of content is.
type Class int

const (
	// ClassRestricted is the ZERO VALUE, for the same reason as above: content
	// nobody has positively classified is content we do not send anywhere but
	// locally.
	ClassRestricted Class = iota
	ClassGeneral
)

// AttributionState is what the capture engine has decided about a message —
// SWT-17 §8's three states, which this boundary reuses rather than inventing a
// parallel vocabulary.
type AttributionState int

const (
	// AttrUnseen: no capture_decisions row. The engine has not looked yet.
	AttrUnseen AttributionState = iota
	// AttrUnmatched: evaluated, no rule covered it. This is triage's inbox.
	AttrUnmatched
	// AttrProject: positively attributed to a project.
	AttrProject
)

// Availability is what we know about a client right now.
type Availability int

const (
	// AvailAbsent is the ZERO VALUE — no client configured for this lane.
	AvailAbsent Availability = iota
	// AvailNotLocal: a client exists but its endpoint is not local.
	AvailNotLocal
	// AvailUnreachable: a local client exists but did not answer a probe.
	AvailUnreachable
	// AvailReady: a client exists, is local, and answered.
	AvailReady
)

// Decision is what to do with one request.
type Decision int

const (
	// DecideSkip is the ZERO VALUE. Skipping is always safe; sending is not.
	DecideSkip Decision = iota
	DecideAllow
)

// ErrUnavailable is returned by a Prober that cannot serve right now. It is a
// FIRST-CLASS OUTCOME, not an error condition: the local lane is a 4B model
// running at low priority on a machine that stays usable as a desktop, so slow
// and briefly absent are normal operation. A pass that exits non-zero every time
// that box is busy trains its operator to ignore it.
var ErrUnavailable = errors.New("provider: unavailable")

// Descriptor is what every Client must say about itself. Describe() is on the
// Client interface rather than a registry, so a new adapter cannot forget to
// declare — the compiler refuses to build it.
type Descriptor struct {
	Name     string
	Endpoint string
}

// Prober is an OPTIONAL capability: a client that can say whether it is able to
// serve right now, cheaply, without running a completion.
//
// Optional rather than part of Client, because the two answer different
// questions. Where a client SENDS is a security property and must be
// unforgettable, so Describe() is mandatory and the compiler enforces it.
// Whether it is ANSWERING is an availability property, it needs I/O, and a
// hosted adapter has no cheap way to answer it — making it mandatory would put a
// network call in the way of every adapter for the benefit of one lane.
//
// A local client that does NOT implement this is treated as UNREACHABLE, not
// ready (router.probe, criterion 22). The whole design principle here is that a
// declaration is not evidence: an adapter that says "I am local" and offers no
// way to check is exactly the case where the declaration is all there is.
//
// This comment said the opposite for a while — "treated as ready" — matching an
// argument I made and lost rather than the code. In the canonical file for the
// boundary, that is the sentence the next contributor would have trusted.
type Prober interface {
	Probe(ctx context.Context) error
}

// LocalityOf classifies an endpoint by WHERE IT SENDS, not by what the adapter
// is called.
//
// This is the answer to "what cannot be forged or forgotten". llama.cpp and
// ollama both serve an OpenAI-compatible /v1 route, so the SAME adapter type
// serves both lanes with a different base URL — a locality declared by type
// would be a lie in both directions. Keying on the destination also means the
// exact change this ticket fears, repointing a worker at a hosted API, is the
// change that trips the guard.
//
// It also means the measured deployment needs no special case: 127.0.0.1:11434
// and a 192.168.50.x LAN address both classify local.
//
// Anything unparseable, empty, or not demonstrably local is LocalityUnknown,
// which Decide treats exactly like remote.
func LocalityOf(d Descriptor) Locality {
	raw := strings.TrimSpace(d.Endpoint)
	if raw == "" {
		return LocalityUnknown
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// A bare "localhost:11434" parses with an empty Host; try again with a
		// scheme rather than guessing from the string.
		u, err = url.Parse("http://" + raw)
		if err != nil || u.Host == "" {
			return LocalityUnknown
		}
	}
	// http/https only. A scheme we do not understand is a destination we cannot
	// reason about, and "I cannot reason about it" is not permission.
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return LocalityUnknown
	}
	host := u.Hostname()
	if host == "" {
		return LocalityUnknown
	}
	if strings.EqualFold(host, "localhost") {
		return LocalityLocal
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name we cannot resolve without I/O. Deciding this would need DNS,
		// and this file is pure on purpose — so it is not local.
		return LocalityRemote
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return LocalityLocal
	}
	return LocalityRemote
}

// ClassOf maps a message's attribution to how restricted it is.
//
// ONLY a positive project attribution that is not local-only is general. Unseen
// and unmatched are BOTH restricted, which is the accepted deviation from this
// SPEC's first draft and the difference between a boundary that holds and one
// that is only as good as the sender list: a personal message no rule happens to
// match is `unmatched`, and treating that as general would have made rule
// completeness load-bearing for a security property.
//
// The cost is accepted and documented: triage's whole inbox is unmatched, so
// triage cannot process anything until a local adapter exists (SWT-22).
func ClassOf(state AttributionState, projectLocalOnly bool) Class {
	if state != AttrProject {
		return ClassRestricted
	}
	if projectLocalOnly {
		return ClassRestricted
	}
	return ClassGeneral
}

// MostRestrictive folds the classes of everything that will travel together.
//
// A request is not just its focus message: AssembleContext pulls thread
// neighbours by thread_id alone, with no reference to THEIR attribution, so an
// unmatched message whose thread contains a personal sibling would otherwise
// carry that sibling's body to a hosted API. The class of a request is the class
// of its most restricted part.
//
// No arguments yields ClassRestricted, deliberately: an empty fold means we know
// nothing, and knowing nothing is not permission.
func MostRestrictive(classes ...Class) Class {
	if len(classes) == 0 {
		return ClassRestricted
	}
	for _, c := range classes {
		// Anything that is not positively ClassGeneral restricts the fold. The
		// earlier spelling kept an unrecognised value and returned IT, so a fold
		// containing Class(42) came out permitted — the same fail-open bug as
		// Decide's, one function along.
		if c != ClassGeneral {
			return ClassRestricted
		}
	}
	return ClassGeneral
}

// Decide is the whole boundary in one total function.
//
//	                 absent   not-local   unreachable   ready
//	restricted       skip     skip        skip          allow
//	general          allow    allow       allow         allow
//
// Restricted content is allowed exactly once: when a local client exists and
// answered. Absent, not-local and unreachable are the SAME answer, because a
// guard that distinguishes them invites a fallback — and "try local, fall back
// to the configured provider" is the well-intentioned implementation that turns
// a slow local model into a silent hosted one, on precisely the messages that
// must never go there.
//
// General content is unaffected: this ticket restricts, it never widens.
//
// Written as an explicit switch over the restricted case rather than a default,
// so adding an Availability value forces a decision here instead of inheriting
// whatever the zero value happens to be.
func Decide(c Class, a Availability) Decision {
	// Switch on the PERMITTED class, never on the restricted one. Spelled
	// `c != ClassRestricted` this read "anything I do not recognise is fine",
	// so Class(42) — a future value, a typo, a field nobody set through a
	// constructor — was ALLOWED and handed the hosted client. Fail-closed has to
	// mean closed against the unknown, not just against the known-bad.
	if c != ClassGeneral {
		c = ClassRestricted
	}
	if c == ClassGeneral {
		return DecideAllow
	}
	switch a {
	case AvailReady:
		return DecideAllow
	case AvailAbsent, AvailNotLocal, AvailUnreachable:
		return DecideSkip
	default:
		// An Availability value that did not exist when this was written.
		// Refusing is the only safe reading of "I do not know what that means".
		return DecideSkip
	}
}

// AvailabilityOf reports what we know about a client WITHOUT probing it — the
// part of availability that is a pure function of its Descriptor.
//
// The probe is separate (router.go) because it needs I/O and this file does not
// do I/O. Splitting them also means the common refusals — no client, wrong
// endpoint — cost nothing and cannot be broken by a network.
func AvailabilityOf(c Client) Availability {
	if c == nil {
		return AvailAbsent
	}
	if LocalityOf(c.Describe()) != LocalityLocal {
		return AvailNotLocal
	}
	return AvailReady
}
