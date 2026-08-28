package provider_test

// Pure unit tests for the provider-locality boundary (SWT-21,
// docs/tickets/provider-locality_SPEC.md, acceptance criteria 2, 3, 4, 5, 6).
// ZERO database, ZERO network, ZERO model, no context on anything asserted here
// — invariant 7 generalised: the decision that governs whether a client's mail
// leaves the building is a total function over two small enums.
//
// Three properties this file exists to hold, each of which has bitten this repo
// in another costume:
//
//  1. LOCALITY IS DERIVED FROM THE ENDPOINT, NOT THE TYPE. ollama and llama.cpp
//     serve an OpenAI-compatible /v1 route, so the same adapter type serves both
//     lanes with nothing but a different base URL. A test that asserted
//     "provider.OpenAI is remote" would pass while the property was false.
//     TestLocalityOf_DerivesFromEndpointNotType asserts the two endpoints DIFFER
//     as part of the test, because a fixture whose two sides are the same string
//     tests nothing (the SWT-16/SWT-18 landmine).
//
//  2. THE ZERO VALUE OF EVERY ENUM IS THE SAFE ONE, and — criterion 3 — an
//     UNRECOGNISED value is safe too. A guard whose default branch is permissive
//     is decoration: the next contributor adds an enum member, every switch that
//     did not name it inherits the permissive default, and nothing fails.
//
//  3. Decide IS TOTAL. Every (Class, Availability) pair is asserted, and the
//     table is checked for coverage so a pair cannot silently go untested.

import (
	"context"
	"fmt"
	"testing"

	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- stubs (shared with router_test.go; no network, no model) ----------------

// stubClient is a provider.Client that records calls and never talks to
// anything. Its Describe() is settable because the ENDPOINT is the thing under
// test — a stub with a hardcoded descriptor could not express the property.
type stubClient struct {
	desc     provider.Descriptor
	calls    int
	requests []provider.Request
	resp     provider.Response
	err      error
}

func (s *stubClient) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	s.calls++
	s.requests = append(s.requests, req)
	return s.resp, s.err
}

func (s *stubClient) Describe() provider.Descriptor { return s.desc }

func localStub() *stubClient {
	return &stubClient{desc: provider.Descriptor{Name: "local-stub", Endpoint: "http://127.0.0.1:11434/v1"}}
}

func hostedStub() *stubClient {
	return &stubClient{desc: provider.Descriptor{Name: "hosted-stub", Endpoint: "https://api.openai.com/v1"}}
}

// ---- criterion 2: LocalityOf classifies the DESTINATION ----------------------

// The property that makes this ticket work at all: the same Go type, pointed at
// two different base URLs, must classify differently. A locality keyed on the
// adapter type would be a lie in both directions.
func TestLocalityOf_DerivesFromEndpointNotType(t *testing.T) {
	localURL := "http://192.168.50.45:11434/v1" // the measured ollama box
	hostedURL := "https://api.openai.com/v1"

	// The fixture must actually differ, or this test proves nothing.
	if localURL == hostedURL {
		t.Fatalf("fixture bug: both endpoints are %q — a test whose two sides are the same string tests nothing", localURL)
	}

	local := provider.NewOpenAI("sk-irrelevant", localURL)
	hosted := provider.NewOpenAI("sk-irrelevant", hostedURL)

	if fmt.Sprintf("%T", local) != fmt.Sprintf("%T", hosted) {
		t.Fatalf("fixture bug: the two adapters are different types (%T vs %T); the whole point is that they are the SAME type",
			local, hosted)
	}

	if got := provider.LocalityOf(local.Describe()); got != provider.LocalityLocal {
		t.Errorf("LocalityOf(%s) = %v, want LocalityLocal — the LAN ollama box is local", localURL, got)
	}
	if got := provider.LocalityOf(hosted.Describe()); got == provider.LocalityLocal {
		t.Errorf("LocalityOf(%s) = %v, want NOT local — repointing an adapter at a hosted API is exactly "+
			"the configuration change this guard exists to trip", hostedURL, got)
	}
}

// LocalityOf is total over whatever string an adapter declares. `wantLocal` is
// the only thing asserted: criterion 2 fixes which endpoints are LOCAL, and
// everything else is "not local" (the shipped code distinguishes Unknown from
// Remote; Decide treats them identically, so pinning which one would pin an
// implementation detail rather than the contract).
func TestLocalityOf_Table(t *testing.T) {
	cases := []struct {
		name      string
		endpoint  string
		wantLocal bool
		why       string
	}{
		{"loopback v4", "http://127.0.0.1:11434/v1", true, "the measured single-box deployment"},
		{"loopback v4, other /8 address", "http://127.9.9.9:8080/v1", true, "127.0.0.0/8 is all loopback"},
		{"loopback v6", "http://[::1]:11434/v1", true, "::1"},
		{"localhost name", "http://localhost:11434/v1", true, "criterion 2 names localhost exactly"},
		{"localhost uppercase", "http://LOCALHOST:11434/v1", true, "hostnames are case-insensitive"},
		{"private 192.168/16", "http://192.168.50.45:11434/v1", true, "the LAN box this repo actually runs on"},
		{"private 10/8", "http://10.1.2.3:11434/v1", true, "10/8"},
		{"private 172.16/12", "http://172.16.0.9:11434/v1", true, "172.16/12"},
		{"private 172.31/12 upper bound", "http://172.31.255.254:11434/v1", true, "172.16/12 runs to 172.31"},
		{"link-local 169.254/16", "http://169.254.1.1:11434/v1", true, "criterion 2 names 169.254/16"},
		{"unique local v6 fc00::/7", "http://[fd12:3456::1]:11434/v1", true, "fc00::/7"},
		{"https loopback", "https://127.0.0.1:8443/v1", true, "https is permitted by criterion 2"},

		{"hosted API", "https://api.openai.com/v1", false, "the thing being kept out"},
		{"public IP literal", "http://8.8.8.8/v1", false, "a public address is not local however it is spelled"},
		{"172.32 is NOT private", "http://172.32.0.1:11434/v1", false,
			"172.16/12 ends at 172.31 — an off-by-one here would classify a public block as local"},
		{"resolvable hostname", "http://ollama.lan:11434/v1", false,
			"deciding this needs DNS, which is I/O and a TOCTOU; criterion 2 forbids the lookup"},
		{"empty endpoint", "", false, "an adapter that declares nothing is not local (fail closed)"},
		{"whitespace endpoint", "   ", false, "same, spelled with spaces"},
		{"malformed", "://not a url", false, "unparseable is not local"},
		{"malformed brackets", "http://[::1", false, "unparseable is not local"},
		{"userinfo spoof", "http://127.0.0.1@evil.example.com/v1", false,
			"the HOST is evil.example.com; a substring check for 127.0.0.1 would send content to it"},
		{"loopback only in the query", "https://evil.example.com/v1?host=127.0.0.1", false,
			"same trap, spelled in the query string"},
		{"loopback only in the path", "https://evil.example.com/127.0.0.1/v1", false, "same trap, spelled in the path"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := provider.LocalityOf(provider.Descriptor{Name: "adapter", Endpoint: tc.endpoint})
			isLocal := got == provider.LocalityLocal
			if isLocal != tc.wantLocal {
				t.Errorf("LocalityOf(%q) = %v (local=%v), want local=%v — %s", tc.endpoint, got, isLocal, tc.wantLocal, tc.why)
			}
		})
	}
}

// Criterion 2, verbatim: LocalityLocal "only when the endpoint parses as an
// http/https URL whose host is a loopback or private-range IP literal ... or is
// exactly localhost". A non-HTTP scheme is therefore NOT local even when the
// host is loopback.
//
// This is a deliberate, narrow reading and it is the SPEC's: the adapter speaks
// HTTP, so an endpoint in another scheme is not a destination this boundary has
// validated — it is a string nobody checked, pointed at a port nobody named.
func TestLocalityOf_NonHTTPSchemeIsNotLocal(t *testing.T) {
	for _, endpoint := range []string{
		"ssh://127.0.0.1:11434/v1",
		"ftp://192.168.50.45/models",
		"file:///var/run/ollama.sock",
		"unix:///var/run/ollama.sock",
	} {
		if got := provider.LocalityOf(provider.Descriptor{Endpoint: endpoint}); got == provider.LocalityLocal {
			t.Errorf("LocalityOf(%q) = LocalityLocal; criterion 2 permits only http/https", endpoint)
		}
	}
}

// ---- criterion 3: zero AND unrecognised values are restricted ----------------

func TestClass_ZeroValueIsRestricted(t *testing.T) {
	var zero provider.Class
	if zero != provider.ClassRestricted {
		t.Errorf("zero Class = %v, want ClassRestricted — a struct field nobody filled in must fail closed", zero)
	}
	if provider.ClassRestricted == provider.ClassGeneral {
		t.Fatalf("ClassRestricted == ClassGeneral: the two sides of every class fixture in this suite are the same value")
	}
}

// Criterion 3: "a forgotten field, a typo, and an unrecognised future value all
// land in restricted". The third is the one that rots: someone adds a Class
// member for a lane that does not exist yet, and every predicate written as
// `!= ClassRestricted` silently treats it as permission to send.
func TestDecide_UnrecognisedClassIsRestricted(t *testing.T) {
	future := provider.Class(42) // neither ClassRestricted nor ClassGeneral

	for _, a := range []provider.Availability{
		provider.AvailAbsent, provider.AvailNotLocal, provider.AvailUnreachable,
	} {
		if got := provider.Decide(future, a); got != provider.DecideSkip {
			t.Errorf("Decide(Class(42), %v) = %v, want DecideSkip — an unrecognised class is not permission "+
				"(criterion 3: unrecognised future values land in restricted)", a, got)
		}
	}
}

// ---- criterion 4: MostRestrictive -------------------------------------------

func TestMostRestrictive(t *testing.T) {
	cases := []struct {
		name string
		in   []provider.Class
		want provider.Class
	}{
		{"empty fold", nil, provider.ClassRestricted},
		{"single general", []provider.Class{provider.ClassGeneral}, provider.ClassGeneral},
		{"single restricted", []provider.Class{provider.ClassRestricted}, provider.ClassRestricted},
		{"general then restricted", []provider.Class{provider.ClassGeneral, provider.ClassRestricted}, provider.ClassRestricted},
		{"restricted then general", []provider.Class{provider.ClassRestricted, provider.ClassGeneral}, provider.ClassRestricted},
		{"all general", []provider.Class{provider.ClassGeneral, provider.ClassGeneral, provider.ClassGeneral}, provider.ClassGeneral},
		{"one restricted late in a long fold",
			[]provider.Class{provider.ClassGeneral, provider.ClassGeneral, provider.ClassGeneral, provider.ClassRestricted},
			provider.ClassRestricted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := provider.MostRestrictive(tc.in...); got != tc.want {
				t.Errorf("MostRestrictive(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The empty fold gets its own test because it is the one a reader argues about:
// "nothing to restrict, so general". No — an empty fold means we know nothing
// about what is travelling, and knowing nothing is not permission. (SPEC
// criterion 4.)
func TestMostRestrictive_EmptyFoldIsRestricted(t *testing.T) {
	if got := provider.MostRestrictive(); got != provider.ClassRestricted {
		t.Errorf("MostRestrictive() = %v, want ClassRestricted", got)
	}
}

// A fold containing a class this build does not recognise must not come out
// permitted — same reasoning as TestDecide_UnrecognisedClassIsRestricted, at the
// other end of the pipe. The fold is the thing triage and drafts both call, so a
// hole here is a hole in both workers.
func TestMostRestrictive_UnrecognisedClassIsRestricted(t *testing.T) {
	future := provider.Class(42)
	got := provider.MostRestrictive(provider.ClassGeneral, future)
	if provider.Decide(got, provider.AvailAbsent) != provider.DecideSkip {
		t.Errorf("MostRestrictive(general, Class(42)) = %v, which Decide permits with no local lane; "+
			"an unrecognised member of the fold must make the whole request restricted", got)
	}
}

// ---- criterion 5: ClassOf ----------------------------------------------------

// The accepted deviation (OPEN_QUESTIONS Q2): unseen AND unmatched are both
// restricted. Only a positive project attribution whose project is not local-only
// is general. A version of this test that let unmatched be general would be
// testing the pre-deviation design — and the deviation is the whole security
// property, because it is what stops rule completeness being load-bearing.
func TestClassOf(t *testing.T) {
	cases := []struct {
		name             string
		state            provider.AttributionState
		projectLocalOnly bool
		want             provider.Class
		why              string
	}{
		{"unseen", provider.AttrUnseen, false, provider.ClassRestricted,
			"no capture_decisions row: the engine has not looked yet"},
		{"unseen, flag set", provider.AttrUnseen, true, provider.ClassRestricted,
			"the flag is irrelevant without a project"},
		{"unmatched", provider.AttrUnmatched, false, provider.ClassRestricted,
			"THE DEVIATION: the engine looked and placed nothing, so nothing has cleared this content"},
		{"unmatched, flag set", provider.AttrUnmatched, true, provider.ClassRestricted, "same"},
		{"project, ai_locality=any", provider.AttrProject, false, provider.ClassGeneral,
			"the only route to general: a positive attribution to a project marked any"},
		{"project, ai_locality=local_only", provider.AttrProject, true, provider.ClassRestricted,
			"the personal project, and any project that fails to say otherwise"},
		{"unrecognised state", provider.AttributionState(9), false, provider.ClassRestricted,
			"a state this build does not know is not a positive attribution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := provider.ClassOf(tc.state, tc.projectLocalOnly); got != tc.want {
				t.Errorf("ClassOf(%v, localOnly=%v) = %v, want %v — %s",
					tc.state, tc.projectLocalOnly, got, tc.want, tc.why)
			}
		})
	}
}

// Criterion 5 keeps unseen and unmatched as DISTINCT states even though both map
// to restricted today: they are different facts (the engine has not looked / the
// engine looked and placed nothing), they are reported separately (criterion 19's
// class_reasons), and collapsing them would destroy the distinction SWT-22 needs.
// Assert both exist as different values — not one state named "not general".
func TestAttributionState_UnseenAndUnmatchedAreDistinct(t *testing.T) {
	if provider.AttrUnseen == provider.AttrUnmatched {
		t.Errorf("AttrUnseen == AttrUnmatched: SWT-17 §8's three states have been collapsed into two. " +
			"Both map to restricted, but they are different facts and are reported separately")
	}
	if provider.AttrProject == provider.AttrUnmatched || provider.AttrProject == provider.AttrUnseen {
		t.Errorf("AttrProject collides with an unattributed state")
	}
	var zero provider.AttributionState
	if zero != provider.AttrUnseen {
		t.Errorf("zero AttributionState = %v, want AttrUnseen — an unfilled attribution field means "+
			"'nobody has looked', which is the restrictive reading", zero)
	}
}

// ---- criterion 6: Decide is pure, total and exhaustive -----------------------

// EVERY (Class, Availability) pair is asserted, and the table is checked for
// coverage of the full cross product so a pair cannot quietly go untested.
//
// Note the shipped Decision enum has two members (DecideSkip / DecideAllow)
// rather than the SPEC's three (Skip/Local/General): which lane an allowed
// request uses is carried by the Client that Router.Route returns, not by the
// Decision. The security content of criterion 6 is unaffected and is asserted
// here: restricted content is allowed on exactly one of the four availabilities.
func TestDecide_EveryPairIsAsserted(t *testing.T) {
	classes := []provider.Class{provider.ClassRestricted, provider.ClassGeneral}
	avails := []provider.Availability{
		provider.AvailAbsent, provider.AvailNotLocal, provider.AvailUnreachable, provider.AvailReady,
	}

	type pair struct {
		c provider.Class
		a provider.Availability
	}
	want := map[pair]provider.Decision{
		{provider.ClassRestricted, provider.AvailAbsent}:      provider.DecideSkip,
		{provider.ClassRestricted, provider.AvailNotLocal}:    provider.DecideSkip,
		{provider.ClassRestricted, provider.AvailUnreachable}: provider.DecideSkip,
		{provider.ClassRestricted, provider.AvailReady}:       provider.DecideAllow,
		{provider.ClassGeneral, provider.AvailAbsent}:         provider.DecideAllow,
		{provider.ClassGeneral, provider.AvailNotLocal}:       provider.DecideAllow,
		{provider.ClassGeneral, provider.AvailUnreachable}:    provider.DecideAllow,
		{provider.ClassGeneral, provider.AvailReady}:          provider.DecideAllow,
	}

	if len(want) != len(classes)*len(avails) {
		t.Fatalf("table has %d rows, want %d — every (Class, Availability) pair must have an assertion",
			len(want), len(classes)*len(avails))
	}
	for _, c := range classes {
		for _, a := range avails {
			exp, ok := want[pair{c, a}]
			if !ok {
				t.Fatalf("no assertion for (%v, %v); the cross product is the contract", c, a)
			}
			if got := provider.Decide(c, a); got != exp {
				t.Errorf("Decide(%v, %v) = %v, want %v", c, a, got, exp)
			}
		}
	}
}

// The headline property, stated on its own so it cannot be lost in a table
// edit: restricted content is NEVER allowed unless a local client exists, is
// local, and answered. Absent, not-local and unreachable are the same answer —
// criterion 6, and the reason the SPEC says "absent, undeclared and unreachable
// land in the same place".
func TestDecide_RestrictedIsSkippedUnlessLocalIsReady(t *testing.T) {
	for _, a := range []provider.Availability{
		provider.AvailAbsent, provider.AvailNotLocal, provider.AvailUnreachable,
	} {
		if got := provider.Decide(provider.ClassRestricted, a); got != provider.DecideSkip {
			t.Errorf("Decide(ClassRestricted, %v) = %v, want DecideSkip — a fallback to the general lane "+
				"is never correct for restricted content", a, got)
		}
	}
	if got := provider.Decide(provider.ClassRestricted, provider.AvailReady); got != provider.DecideAllow {
		t.Errorf("Decide(ClassRestricted, AvailReady) = %v, want DecideAllow — a boundary that refuses even "+
			"a ready local model is not fail-closed, it is broken", got)
	}
}

// An Availability member added later must not inherit a permissive default. This
// is the same failure mode as TestDecide_UnrecognisedClassIsRestricted; both
// exist because "exhaustiveness" is a property of the code, not of the enum.
func TestDecide_UnrecognisedAvailabilityIsSkip(t *testing.T) {
	if got := provider.Decide(provider.ClassRestricted, provider.Availability(99)); got != provider.DecideSkip {
		t.Errorf("Decide(ClassRestricted, Availability(99)) = %v, want DecideSkip", got)
	}
}

// ---- zero values, collected ---------------------------------------------------

// Every enum's zero value is the safe one. A struct nobody filled in must fail
// closed; this is the single assertion that keeps that true across four types.
func TestZeroValuesAreTheSafeOnes(t *testing.T) {
	var (
		loc provider.Locality
		cls provider.Class
		av  provider.Availability
		dec provider.Decision
	)
	if loc == provider.LocalityLocal {
		t.Errorf("zero Locality is LocalityLocal — an adapter that describes nothing would be treated as local")
	}
	if cls != provider.ClassRestricted {
		t.Errorf("zero Class = %v, want ClassRestricted", cls)
	}
	if av != provider.AvailAbsent {
		t.Errorf("zero Availability = %v, want AvailAbsent", av)
	}
	if dec != provider.DecideSkip {
		t.Errorf("zero Decision = %v, want DecideSkip", dec)
	}
	if provider.Decide(cls, av) != provider.DecideSkip {
		t.Errorf("Decide over the zero values permits the request; the zero state must be a refusal")
	}
	if provider.LocalityOf(provider.Descriptor{}) == provider.LocalityLocal {
		t.Errorf("LocalityOf(Descriptor{}) = LocalityLocal — an adapter that declares nothing must not be local")
	}
}

// ---- AvailabilityOf: the descriptor half of availability ---------------------

// AvailabilityOf is the pure part of availability — the part that needs no I/O
// and therefore cannot be broken by a network. It is NOT the whole answer: a
// local endpoint that has not been probed is only descriptor-ready, and
// criterion 22 makes the Router responsible for the rest (router_test.go).
func TestAvailabilityOf(t *testing.T) {
	if got := provider.AvailabilityOf(nil); got != provider.AvailAbsent {
		t.Errorf("AvailabilityOf(nil) = %v, want AvailAbsent — no client configured for the lane", got)
	}
	if got := provider.AvailabilityOf(hostedStub()); got != provider.AvailNotLocal {
		t.Errorf("AvailabilityOf(hosted) = %v, want AvailNotLocal — a client exists but sends elsewhere", got)
	}
	if got := provider.AvailabilityOf(&stubClient{}); got != provider.AvailNotLocal {
		t.Errorf("AvailabilityOf(client with an empty Descriptor) = %v, want AvailNotLocal — "+
			"declaring nothing is not declaring local", got)
	}
	got := provider.AvailabilityOf(localStub())
	if got == provider.AvailAbsent || got == provider.AvailNotLocal {
		t.Errorf("AvailabilityOf(local) = %v; a present client with a loopback endpoint has passed the "+
			"descriptor axis and only the probe remains", got)
	}
}
