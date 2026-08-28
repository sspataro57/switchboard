package provider_test

// Unit tests for provider.Router (SWT-21 acceptance criteria 7 and 22). ZERO
// network, ZERO model: the local and general lanes are stubs that record calls.
//
// The assertion that matters in almost every test below is the NEGATIVE one:
// the general (hosted) stub recorded ZERO Complete calls. Criterion 7 says there
// is no method that hands a remote client to a restricted class; the way to test
// "no path exists" is to drive every refusal shape there is and check the hosted
// lane stayed untouched each time.
//
// Router.Route's signature and the Reason constants are read from the shipped
// router.go, not imposed. What IS imposed here is criterion 22's reading of a
// local client that cannot be probed — see
// TestRouter_LocalWithoutProber_IsNotReady, which currently fails on purpose.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

// probeStub is a local-lane stub that also implements provider.Prober.
type probeStub struct {
	stubClient
	probeErr    error
	probes      int
	probeBlocks chan struct{} // when non-nil, Probe blocks on it (or on ctx)
}

func (p *probeStub) Probe(ctx context.Context) error {
	p.probes++
	if p.probeBlocks != nil {
		select {
		case <-p.probeBlocks:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.probeErr
}

func readyLocal() *probeStub {
	return &probeStub{stubClient: stubClient{
		desc: provider.Descriptor{Name: "local", Endpoint: "http://127.0.0.1:11434/v1"},
	}}
}

func unreachableLocal() *probeStub {
	p := readyLocal()
	p.probeErr = fmt.Errorf("dial tcp 127.0.0.1:11434: %w", provider.ErrUnavailable)
	return p
}

// ---- criterion 7: no path from restricted content to the hosted lane ---------

func TestRouter_RestrictedWithNoLocalClient_NeverTouchesGeneral(t *testing.T) {
	general := hostedStub()
	r := provider.NewRouter(general, nil, time.Minute)

	client, decision, reason := r.Route(context.Background(), provider.ClassRestricted)

	if decision != provider.DecideSkip {
		t.Errorf("Route(restricted) with no local client = %v, want DecideSkip", decision)
	}
	if client != nil {
		t.Errorf("Route returned a client (%v) alongside a skip; a caller cannot misuse a client it was "+
			"not given, so a refusal must come back nil", client)
	}
	if general.calls != 0 {
		t.Errorf("the general (hosted) stub recorded %d Complete call(s); criterion 7: there is no method "+
			"that hands a remote client to a restricted class", general.calls)
	}
	if reason == "" {
		t.Errorf("Route returned an empty Reason; the refusal has to be recordable (criteria 19-20)")
	}
}

// Criterion 7 again, from the other side: the general lane still works. Without
// this control the suite would pass against a Router that refuses everything —
// a guard that blocks all traffic proves nothing about the one it must block.
func TestRouter_GeneralClass_UsesTheGeneralLane(t *testing.T) {
	general := hostedStub()
	r := provider.NewRouter(general, nil, time.Minute)

	client, decision, _ := r.Route(context.Background(), provider.ClassGeneral)
	if decision != provider.DecideAllow {
		t.Fatalf("Route(general) = %v, want DecideAllow — this ticket restricts, it never widens", decision)
	}
	if client != provider.Client(general) {
		t.Fatalf("Route(general) returned %v, want the general client", client)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Model: "gpt-5-mini"}); err != nil {
		t.Fatalf("Complete on the general lane: %v", err)
	}
	if general.calls != 1 {
		t.Errorf("general lane recorded %d calls, want 1", general.calls)
	}
}

// A Class this build does not recognise must route like restricted. The shipped
// Route tests `c != ClassRestricted`, which sends any future class member — a
// lane added by SWT-22 or later, a value from a struct field nobody set to a
// known constant — straight to the hosted client (criterion 3).
func TestRouter_UnrecognisedClassRoutesLikeRestricted(t *testing.T) {
	general := hostedStub()
	r := provider.NewRouter(general, nil, time.Minute)

	client, decision, _ := r.Route(context.Background(), provider.Class(42))
	if decision != provider.DecideSkip || client != nil {
		t.Errorf("Route(Class(42)) = (%v, %v), want (nil, DecideSkip) — an unrecognised class is not permission",
			client, decision)
	}
	if general.calls != 0 {
		t.Errorf("the general stub was handed an unrecognised class and recorded %d call(s)", general.calls)
	}
}

// ---- criterion 7 + the SPEC's "three axes land in the same place" ------------

// "Absent, undeclared and unreachable land in the same place." Each axis is
// driven separately, all three must yield a skip with the hosted lane untouched,
// and the three REASONS must differ — the outcome is deliberately identical, the
// record deliberately is not (criteria 19-20 report them separately).
func TestRouter_ThreeAxesOfUnavailabilityAllSkip(t *testing.T) {
	undeclared := &stubClient{desc: provider.Descriptor{Name: "declares-nothing"}} // empty Endpoint
	hostedLocalSlot := hostedStub()                                                // "local" lane pointed at a hosted API

	cases := []struct {
		name  string
		local provider.Client
		why   string
	}{
		{"absent", nil, "no local client configured at all (the state this ticket ships into)"},
		{"undeclared endpoint", undeclared, "an adapter whose Descriptor declares nothing"},
		{"endpoint is not local", hostedLocalSlot,
			"OPS_LOCAL_PROVIDER_URL pointed at a hosted API — criterion 21's negative smoke"},
		{"local but unreachable", unreachableLocal(), "the 4B box is busy; normal operation, not an error"},
	}

	reasons := map[string]provider.Reason{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			general := hostedStub()
			r := provider.NewRouter(general, tc.local, time.Minute)

			client, decision, reason := r.Route(context.Background(), provider.ClassRestricted)
			if decision != provider.DecideSkip {
				t.Errorf("Route(restricted) with a %s local lane = %v, want DecideSkip — %s", tc.name, decision, tc.why)
			}
			if client != nil {
				t.Errorf("Route returned a non-nil client with a skip decision (%s)", tc.name)
			}
			if general.calls != 0 {
				t.Errorf("the general stub recorded %d call(s) after a %s local lane; the whole point is that "+
					"an unavailable local lane is NOT a reason to use the hosted one", general.calls, tc.name)
			}
			if l, ok := tc.local.(*probeStub); ok && l.calls != 0 {
				t.Errorf("the local stub was asked to Complete %d time(s) despite being unavailable", l.calls)
			}
			reasons[tc.name] = reason
		})
	}

	// The record must discriminate even though the decision does not. Criterion 20
	// breaks skips down by avail_reason and names three routing reasons
	// (no_local_provider / local_endpoint_not_private / local_unreachable), so the
	// three axes that produce them must not collapse into one string: an operator
	// reading the report has to be able to tell "nothing is configured" from
	// "configured at a hosted URL" from "configured and not answering", because
	// the three have three different fixes.
	//
	// "undeclared endpoint" is deliberately NOT required to be a fourth distinct
	// reason — an empty endpoint is not private, so sharing that reason is
	// correct. It is required not to look like "nothing configured", which would
	// send an operator to set a variable that is already set.
	seen := map[provider.Reason][]string{}
	for _, name := range []string{"absent", "endpoint is not local", "local but unreachable"} {
		reason := reasons[name]
		if reason == "" {
			t.Errorf("axis %q produced an empty Reason", name)
		}
		seen[reason] = append(seen[reason], name)
	}
	for reason, names := range seen {
		if len(names) > 1 {
			t.Errorf("axes %v all report reason %q; criterion 20 breaks skips down BY reason, so an operator "+
				"could not tell 'no local model configured' from 'configured at a hosted URL' or "+
				"'configured but not answering'", names, reason)
		}
	}
	if reasons["undeclared endpoint"] == reasons["absent"] {
		t.Errorf("an adapter with an empty Descriptor reports %q, the same reason as having no local client "+
			"at all; the fix for those two is not the same", reasons["absent"])
	}
}

// ---- criterion 22: availability is PROBED, not assumed -----------------------

func TestRouter_ReadyLocal_RoutesToTheLocalClient(t *testing.T) {
	general := hostedStub()
	local := readyLocal()
	r := provider.NewRouter(general, local, time.Minute)

	client, decision, _ := r.Route(context.Background(), provider.ClassRestricted)
	if decision != provider.DecideAllow {
		t.Fatalf("Route(restricted) with a ready local client = %v, want DecideAllow", decision)
	}
	if client != provider.Client(local) {
		t.Fatalf("Route returned %v, want the local client", client)
	}
	if local.probes != 1 {
		t.Errorf("local client was probed %d times, want 1 — availability is probed, not assumed", local.probes)
	}
	if general.calls != 0 {
		t.Errorf("general stub recorded %d call(s) on the restricted lane", general.calls)
	}
}

// Criterion 22, verbatim: "A local client that does not implement Prober, or
// whose probe fails, is AvailUnreachable — 'declares itself local but is
// unreachable is not a permitted processor right now', INCLUDING THE CASE WHERE
// THE DECLARATION IS ALL THERE IS."
//
// The shipped router.probe treats a non-Prober local client as ready and argues
// the case in its doc comment. That is a real argument and it may win — but the
// SPEC is FINAL and says the opposite, so the disagreement belongs in a failing
// test rather than in a comment. What the SPEC is protecting: with the
// permissive reading, an adapter can reach the restricted lane on the strength
// of its own Descriptor alone, and "declares itself local" becomes the entire
// gate for a message that must never leave the building.
func TestRouter_LocalWithoutProber_IsNotReady(t *testing.T) {
	general := hostedStub()
	local := localStub() // implements Client, NOT Prober
	r := provider.NewRouter(general, local, time.Minute)

	client, decision, reason := r.Route(context.Background(), provider.ClassRestricted)
	if decision != provider.DecideSkip {
		t.Errorf("Route(restricted) with a local client that cannot be probed = %v, want DecideSkip "+
			"(criterion 22: a declaration alone is not availability)", decision)
	}
	if client != nil {
		t.Errorf("Route handed back %v for an unprobeable local client", client)
	}
	if general.calls != 0 || local.calls != 0 {
		t.Errorf("clients were used: general=%d local=%d", general.calls, local.calls)
	}
	if reason == "" {
		t.Errorf("empty Reason for an unprobeable local client")
	}
}

func TestRouter_ProbeFailureIsSkipAndNeverFallsBack(t *testing.T) {
	general := hostedStub()
	local := unreachableLocal()
	r := provider.NewRouter(general, local, time.Minute)

	client, decision, _ := r.Route(context.Background(), provider.ClassRestricted)
	if decision != provider.DecideSkip || client != nil {
		t.Fatalf("Route with a failing probe = (%v, %v), want (nil, DecideSkip)", client, decision)
	}
	if general.calls != 0 {
		t.Errorf("general stub recorded %d call(s) after a failed local probe — 'try local, fall back to the "+
			"configured provider' is the bug this ticket exists to prevent", general.calls)
	}
	if local.probes != 1 {
		t.Errorf("local probes = %d, want 1", local.probes)
	}
}

// Criterion 22: "the Router probes it ONCE per pass". A pass is one Router, so
// two routings must share one probe — otherwise every message in a 16,000-message
// inbox pays a round trip to a machine the SPEC describes as normally busy.
func TestRouter_ProbesOncePerPass(t *testing.T) {
	local := readyLocal()
	r := provider.NewRouter(hostedStub(), local, time.Minute)

	for i := 0; i < 5; i++ {
		if _, decision, _ := r.Route(context.Background(), provider.ClassRestricted); decision != provider.DecideAllow {
			t.Fatalf("route %d = %v, want DecideAllow", i, decision)
		}
	}
	if local.probes != 1 {
		t.Errorf("local probed %d times over 5 routings, want 1 (once per pass)", local.probes)
	}
}

// Criterion 22: the probe has "a short deadline". A local box that accepts the
// connection and then never answers must not wedge the pass — an unreachable
// local lane is normal operation and must cost a bounded amount of time.
//
// The test drives it with a probe that blocks until its own context is done and
// a caller context with NO deadline, so the only thing that can free it is a
// deadline the Router imposes.
func TestRouter_ProbeHasItsOwnDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	local := readyLocal()
	local.probeBlocks = release
	r := provider.NewRouter(hostedStub(), local, time.Minute)

	done := make(chan provider.Decision, 1)
	go func() {
		_, decision, _ := r.Route(context.Background(), provider.ClassRestricted)
		done <- decision
	}()

	select {
	case decision := <-done:
		if decision != provider.DecideSkip {
			t.Errorf("Route with a hanging probe = %v, want DecideSkip", decision)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("Route did not return within 3s while the local probe hung; criterion 22 requires the probe " +
			"to carry a short deadline of its own, because the caller's context is a whole pass")
	}
}

// ErrUnavailable is the sentinel a probe or an adapter returns to say "not right
// now" — errors.Is-checkable so callers classify it with code rather than by
// matching strings (criterion 8, and criterion 18's first tier).
func TestErrUnavailable_IsMatchable(t *testing.T) {
	wrapped := fmt.Errorf("probe ollama: %w", provider.ErrUnavailable)
	if !errors.Is(wrapped, provider.ErrUnavailable) {
		t.Errorf("errors.Is(wrapped, ErrUnavailable) = false")
	}
	if errors.Is(errors.New("openai HTTP 500"), provider.ErrUnavailable) {
		t.Errorf("a plain error matched ErrUnavailable; criterion 18 needs the two tiers to be distinguishable")
	}
}

// A nil general client is a SKIP, not an allow.
//
// Criterion 21 made OPENAI_API_KEY optional, so "no hosted client" is a
// SUPPORTED triage configuration rather than a misconfiguration. Every caller
// dereferences the returned client immediately, so the previous shape —
// (nil, DecideAllow) — was a nil dereference waiting for the first general
// message to arrive in a triage deployment with no key.
func TestRouter_GeneralWithNoGeneralClient_SkipsInsteadOfReturningNil(t *testing.T) {
	r := provider.NewRouter(nil, nil, time.Minute)

	client, decision, reason := r.Route(context.Background(), provider.ClassGeneral)
	if decision != provider.DecideSkip {
		t.Errorf("decision = %v, want DecideSkip — there is no client to serve general content with", decision)
	}
	if client != nil {
		t.Errorf("client = %v, want nil", client)
	}
	if reason != provider.ReasonGeneralAbsent {
		t.Errorf("reason = %q, want %q; the operator has to be able to tell 'no hosted key' apart from a "+
			"locality refusal, because they have completely different fixes", reason, provider.ReasonGeneralAbsent)
	}
}

// The control for the case above: with a general client present, general content
// still takes the hosted lane untouched. This ticket restricts, it never widens.
func TestRouter_GeneralWithAGeneralClient_IsUnaffected(t *testing.T) {
	hosted := hostedStub()
	r := provider.NewRouter(hosted, nil, time.Minute)

	client, decision, reason := r.Route(context.Background(), provider.ClassGeneral)
	if decision != provider.DecideAllow || client != provider.Client(hosted) || reason != provider.ReasonGeneralLane {
		t.Errorf("Route(ClassGeneral) = (%v, %v, %q), want the hosted client allowed on the general lane",
			client, decision, reason)
	}
}
