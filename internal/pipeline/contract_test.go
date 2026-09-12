package pipeline_test

// SWT-40 Part E, criteria E1 and E2 (docs/tickets/inquiry-promote_SPEC.md,
// "Part E", E-D3, "MQTT topics"). Unit tests for the pipeline wake-up contract.
// ZERO network, ZERO Postgres: the pure topic/payload/stage-graph surface, in the
// shape of internal/fleet/contract_test.go.
//
// GREENFIELD NOTE: package internal/pipeline does not exist yet; this file
// compile-FAILs under `go test ./internal/pipeline/` until it is implemented,
// the expected failure mode (the same one internal/fleet's tests had). The
// exported surface imposed here (the SPEC names contract.go, Wake, a strict
// Marshal, a lenient parse, Subscribers(event) []Stage and PipelineSweep; the
// rest is this file's contract, declared for the implementer):
//
//	// Event vocabulary (E-D3). Untyped string constants, as fleet's states are.
//	const (
//	    EventCaptured          = "captured"
//	    EventGated             = "gated"
//	    EventRouteClassified   = "route_classified"
//	    EventRouted            = "routed"
//	    EventInquiryClassified = "inquiry_classified"
//	    EventPromoted          = "promoted"
//	)
//	func Events() []string // exactly the six above
//
//	type Stage string
//	const (
//	    StageGate           Stage = "gate"
//	    StageRoute          Stage = "route"
//	    StageRouteApply     Stage = "route_apply"
//	    StageInquiry        Stage = "inquiry"
//	    StageInquiryPromote Stage = "inquiry_promote"
//	)
//	func Stages() []Stage // exactly the five above
//
//	func Topic(event string) string            // "ops/pipeline/" + event
//	func WorkerID(stage Stage) string          // "pipeline." + stage (fleet heartbeat id)
//	func StageClientID(stage Stage) string     // "switchboard-pipeline-" + stage
//	func CaptureClientID(connector string) string // "switchboard-capture-" + connector
//
//	const PipelineSweep  = 5 * time.Minute   // E-D4 sweep fallback
//	const LockRetryDelay = 30 * time.Second  // E-D4 lost-lock retry
//
//	type Wake struct {
//	    Event  string         `json:"event"`
//	    Source string         `json:"source"`
//	    Counts map[string]int `json:"counts"`   // diagnostic only
//	    MaxID  int64          `json:"max_id"`   // diagnostic only
//	    TS     time.Time      `json:"ts"`
//	}
//	func (w Wake) Marshal() ([]byte, error) // STRICT: unknown Event errors
//	func ParseWake(data []byte) (Wake, error) // LENIENT: only broken JSON errors
//
//	func Subscribers(event string) []Stage // pure static table, E-D3
//	func Upstream(stage Stage) []string    // its inverse: the events a stage subscribes to
//
//	// The one-method surfaces the pipeline needs from an MQTT client.
//	// *fleet.Client must satisfy all three (fleet gains a generic Publish and
//	// Subscribe, SPEC "Files likely to touch → Part E").
//	type Publisher interface {
//	    Publish(topic string, qos byte, retained bool, payload []byte) error
//	}
//	type Subscriber interface {
//	    Subscribe(filter string, handler func(topic string, payload []byte)) error
//	}
//	type StatusPublisher interface {
//	    PublishStatus(s fleet.Status) error
//	}
//
//	// PublishWake: strict Marshal, then Publish(Topic(w.Event), 1, false, payload).
//	// Returns the error for the caller to LOG; it never fails a stage (E-D3).
//	func PublishWake(pub Publisher, w Wake) error
//	// SubscribeWakes subscribes Topic(e) for every e in Upstream(stage) and calls
//	// notify on each parsed wake.
//	func SubscribeWakes(sub Subscriber, stage Stage, notify func(Wake)) error
//
//	// E2, the connector-main seam.
//	// CapturedWake builds the `captured` wake for one capture pass; ok is false
//	// iff the pass committed no decision (stats.Considered == 0).
//	func CapturedWake(connector string, stats capture.RulesStats) (w Wake, ok bool)
//	// AnnounceCaptured is the connector mains' one call after capture.EvaluateRules.
//	// It publishes one `captured` wake iff CapturedWake says so, on a spine client
//	// with id CaptureClientID(connector) connected only for the publish. broker ""
//	// skips with one log line. It NEVER returns an error: the bool reports whether
//	// a wake went out (false on skip, on an empty pass and on any publish failure).
//	func AnnounceCaptured(ctx context.Context, broker, connector string, stats capture.RulesStats) bool

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

// *fleet.Client is the production MQTT client for every pipeline participant.
// If it stops satisfying these, the pipeline has grown a second client.
var (
	_ pipeline.Publisher       = (*fleet.Client)(nil)
	_ pipeline.Subscriber      = (*fleet.Client)(nil)
	_ pipeline.StatusPublisher = (*fleet.Client)(nil)
)

var allEvents = []string{
	"captured", "gated", "route_classified", "routed", "inquiry_classified", "promoted",
}

// E1: the event vocabulary is exactly E-D3's six, spelled exactly.
func TestEventVocabulary(t *testing.T) {
	got := map[string]string{
		"captured":           pipeline.EventCaptured,
		"gated":              pipeline.EventGated,
		"route_classified":   pipeline.EventRouteClassified,
		"routed":             pipeline.EventRouted,
		"inquiry_classified": pipeline.EventInquiryClassified,
		"promoted":           pipeline.EventPromoted,
	}
	for want, c := range got {
		if c != want {
			t.Errorf("event constant = %q, want %q", c, want)
		}
	}
	events := append([]string(nil), pipeline.Events()...)
	sort.Strings(events)
	want := append([]string(nil), allEvents...)
	sort.Strings(want)
	if !reflect.DeepEqual(events, want) {
		t.Errorf("Events() = %v, want exactly %v", events, want)
	}
}

// E1: the stage vocabulary is exactly the five E-D3 consumers.
func TestStageVocabulary(t *testing.T) {
	pairs := map[pipeline.Stage]string{
		pipeline.StageGate:           "gate",
		pipeline.StageRoute:          "route",
		pipeline.StageRouteApply:     "route_apply",
		pipeline.StageInquiry:        "inquiry",
		pipeline.StageInquiryPromote: "inquiry_promote",
	}
	for s, want := range pairs {
		if string(s) != want {
			t.Errorf("stage constant = %q, want %q", s, want)
		}
	}
	got := stageStrings(pipeline.Stages())
	want := []string{"gate", "inquiry", "inquiry_promote", "route", "route_apply"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Stages() = %v, want exactly %v", got, want)
	}
}

// E1: topic builder. `ops/pipeline/{event}`, one level per event.
func TestTopic(t *testing.T) {
	for _, e := range allEvents {
		if got, want := pipeline.Topic(e), "ops/pipeline/"+e; got != want {
			t.Errorf("Topic(%q) = %q, want %q", e, got, want)
		}
	}
}

// E4 / E-D4: the heartbeat worker id is dotted `pipeline.{stage}`, so it lands
// on fleet's ops/workers/{id}/status and fleetd mirrors it as client `pipeline`.
// Each stage's spine client has its OWN client id (the duplicate-client-id
// landmine: two connections sharing an id kick each other off the broker).
func TestStageIdentities(t *testing.T) {
	seenClient := map[string]pipeline.Stage{}
	for _, s := range pipeline.Stages() {
		wid := pipeline.WorkerID(s)
		if wid != "pipeline."+string(s) {
			t.Errorf("WorkerID(%q) = %q, want pipeline.%s", s, wid, s)
		}
		if err := fleet.ValidateWorkerID(wid); err != nil {
			t.Errorf("WorkerID(%q) = %q is not a valid fleet worker id: %v", s, wid, err)
		}
		if got := fleet.StatusTopic(wid); got != "ops/workers/pipeline."+string(s)+"/status" {
			t.Errorf("status topic for %q = %q", s, got)
		}
		if c := fleet.ClientFromWorkerID(wid); c != "pipeline" {
			t.Errorf("fleet client for %q = %q, want pipeline", s, c)
		}
		cid := pipeline.StageClientID(s)
		if cid != "switchboard-pipeline-"+string(s) {
			t.Errorf("StageClientID(%q) = %q, want switchboard-pipeline-%s", s, cid, s)
		}
		if prev, dup := seenClient[cid]; dup {
			t.Errorf("stages %q and %q share client id %q", prev, s, cid)
		}
		seenClient[cid] = s
	}
	if got := pipeline.CaptureClientID("slackweb"); got != "switchboard-capture-slackweb" {
		t.Errorf("CaptureClientID(slackweb) = %q, want switchboard-capture-slackweb", got)
	}
	// A capture client can never collide with fleetd or a stage.
	if pipeline.CaptureClientID("jira") == pipeline.CaptureClientID("google") {
		t.Errorf("CaptureClientID is not distinct per connector")
	}
}

// E-D4: the sweep fallback and the lost-lock retry are pinned values.
func TestLoopIntervals(t *testing.T) {
	if pipeline.PipelineSweep != 5*time.Minute {
		t.Errorf("PipelineSweep = %v, want 5m", pipeline.PipelineSweep)
	}
	if pipeline.LockRetryDelay != 30*time.Second {
		t.Errorf("LockRetryDelay = %v, want 30s", pipeline.LockRetryDelay)
	}
}

// E1: Subscribers is the E-D3 table, row for row. Compared as sets: the order a
// stage is woken in carries no meaning (each re-queries its own inbox).
func TestSubscribers_EveryRowOfTheTable(t *testing.T) {
	table := map[string][]string{
		pipeline.EventCaptured:          {"gate", "inquiry", "route"},
		pipeline.EventGated:             {"inquiry"},
		pipeline.EventRouteClassified:   {"route_apply"},
		pipeline.EventRouted:            {"inquiry"},
		pipeline.EventInquiryClassified: {"inquiry_promote"},
		// `promoted` wakes no stage: the task boundary is the orchestrator's (E-D2).
		pipeline.EventPromoted: {},
	}
	for event, want := range table {
		got := stageStrings(pipeline.Subscribers(event))
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Subscribers(%q) = %v, want %v", event, got, want)
		}
	}
	for _, e := range pipeline.Events() {
		if _, ok := table[e]; !ok {
			t.Errorf("event %q is in Events() but has no row in this test's copy of E-D3", e)
		}
	}
}

// Unknown events wake nobody. A typo'd or future event must not fan out.
func TestSubscribers_UnknownEventWakesNobody(t *testing.T) {
	for _, e := range []string{"", "Captured", "personal_classified", "captured "} {
		if got := pipeline.Subscribers(e); len(got) != 0 {
			t.Errorf("Subscribers(%q) = %v, want none", e, got)
		}
	}
}

// The table is static: a caller mutating the returned slice must not rewire
// the stage graph for everyone else.
func TestSubscribers_ReturnsACopy(t *testing.T) {
	first := pipeline.Subscribers(pipeline.EventCaptured)
	if len(first) == 0 {
		t.Fatalf("Subscribers(captured) is empty")
	}
	first[0] = pipeline.Stage("hijacked")
	for _, s := range pipeline.Subscribers(pipeline.EventCaptured) {
		if s == "hijacked" {
			t.Fatalf("Subscribers returned the table's own backing slice; a caller rewired the graph")
		}
	}
}

// Upstream is the exact inverse of Subscribers: what pipelined subscribes a
// stage to and what the table says wakes it cannot disagree.
func TestUpstream_IsTheInverseOfSubscribers(t *testing.T) {
	want := map[pipeline.Stage][]string{
		pipeline.StageGate:           {"captured"},
		pipeline.StageRoute:          {"captured"},
		pipeline.StageRouteApply:     {"route_classified"},
		pipeline.StageInquiry:        {"captured", "gated", "routed"},
		pipeline.StageInquiryPromote: {"inquiry_classified"},
	}
	for stage, w := range want {
		got := append([]string(nil), pipeline.Upstream(stage)...)
		sort.Strings(got)
		if !reflect.DeepEqual(got, w) {
			t.Errorf("Upstream(%q) = %v, want %v", stage, got, w)
		}
	}
	for _, e := range pipeline.Events() {
		for _, s := range pipeline.Stages() {
			inSubs := containsStage(pipeline.Subscribers(e), s)
			inUp := containsString(pipeline.Upstream(s), e)
			if inSubs != inUp {
				t.Errorf("event %q / stage %q: Subscribers says %v, Upstream says %v", e, s, inSubs, inUp)
			}
		}
	}
}

// E1: the payload's JSON keys are the SPEC's spelling.
func TestWake_MarshalKeysAndRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	in := pipeline.Wake{
		Event:  pipeline.EventCaptured,
		Source: "slackweb",
		Counts: map[string]int{"unmatched": 1, "attributed": 3, "held": 0},
		MaxID:  123456,
		TS:     ts,
	}
	data, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, k := range []string{"event", "source", "counts", "max_id", "ts"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing from %s", k, data)
		}
	}
	out, err := pipeline.ParseWake(data)
	if err != nil {
		t.Fatalf("ParseWake: %v", err)
	}
	if out.Event != in.Event || out.Source != in.Source || out.MaxID != in.MaxID || !out.TS.Equal(ts) {
		t.Errorf("round trip: got %+v, want %+v", out, in)
	}
	if !reflect.DeepEqual(out.Counts, in.Counts) {
		t.Errorf("counts round trip: got %v, want %v", out.Counts, in.Counts)
	}
}

// E1: strict publish. Every vocabulary event marshals; anything else errors.
func TestWake_MarshalStrictRefusesUnknownEvents(t *testing.T) {
	for _, e := range pipeline.Events() {
		if _, err := (pipeline.Wake{Event: e, Source: "itest"}).Marshal(); err != nil {
			t.Errorf("Wake{Event:%q}.Marshal(): unexpected error %v", e, err)
		}
	}
	for _, bad := range []string{"", "Captured", "personal_classified", "dead"} {
		if _, err := (pipeline.Wake{Event: bad, Source: "itest"}).Marshal(); err == nil {
			t.Errorf("Wake{Event:%q}.Marshal(): expected strict-publish error", bad)
		}
	}
}

// E1: lenient consume. Unknown fields and unknown events are tolerated (a
// payload is diagnostic; a consumer re-queries its inbox whatever it says).
func TestParseWake_Lenient(t *testing.T) {
	w, err := pipeline.ParseWake([]byte(`{"event":"from_the_future","extra":{"a":1}}`))
	if err != nil {
		t.Fatalf("ParseWake (unknown event + field): %v", err)
	}
	if w.Event != "from_the_future" {
		t.Errorf("event = %q, want it verbatim", w.Event)
	}
	if w.Counts != nil && len(w.Counts) != 0 {
		t.Errorf("counts = %v, want empty when missing", w.Counts)
	}
	if _, err := pipeline.ParseWake([]byte(`{not json`)); err == nil {
		t.Errorf("ParseWake(garbage): expected error")
	}
}

// ---- E2 unit half: the connector seam -------------------------------------

// CapturedWake publishes iff the pass committed ≥1 decision. Each counted
// message in RulesStats is one committed capture_decisions row
// (Considered == Matched + Unmatched, rules_store.go).
func TestCapturedWake_OnlyWhenAPassCommittedADecision(t *testing.T) {
	if _, ok := pipeline.CapturedWake("slackweb", capture.RulesStats{}); ok {
		t.Errorf("CapturedWake on an empty pass: ok=true, want no wake")
	}
	cases := []capture.RulesStats{
		{Considered: 1, Unmatched: 1},
		{Considered: 4, Matched: 3, Unmatched: 1, TasksCreated: 1},
	}
	for _, st := range cases {
		w, ok := pipeline.CapturedWake("slackweb", st)
		if !ok {
			t.Errorf("CapturedWake(%+v): ok=false, want a wake", st)
			continue
		}
		if w.Event != pipeline.EventCaptured {
			t.Errorf("event = %q, want captured", w.Event)
		}
		if w.Source != "slackweb" {
			t.Errorf("source = %q, want slackweb", w.Source)
		}
		if _, err := w.Marshal(); err != nil {
			t.Errorf("CapturedWake produced an unpublishable wake: %v", err)
		}
	}
}

type pubCall struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

type fakePublisher struct {
	calls []pubCall
	err   error
}

func (f *fakePublisher) Publish(topic string, qos byte, retained bool, payload []byte) error {
	f.calls = append(f.calls, pubCall{topic, qos, retained, append([]byte(nil), payload...)})
	return f.err
}

// PublishWake: topic ops/pipeline/{event}, QoS 1, NOT retained (a retained
// wake-up re-fires on every reconnect; retained state is global on prod, IK).
func TestPublishWake_TopicQoSNotRetained(t *testing.T) {
	for _, e := range pipeline.Events() {
		fp := &fakePublisher{}
		if err := pipeline.PublishWake(fp, pipeline.Wake{Event: e, Source: "itest"}); err != nil {
			t.Fatalf("PublishWake(%q): %v", e, err)
		}
		if len(fp.calls) != 1 {
			t.Fatalf("PublishWake(%q): %d publishes, want 1", e, len(fp.calls))
		}
		c := fp.calls[0]
		if c.topic != pipeline.Topic(e) {
			t.Errorf("topic = %q, want %q", c.topic, pipeline.Topic(e))
		}
		if c.qos != 1 {
			t.Errorf("qos = %d, want 1", c.qos)
		}
		if c.retained {
			t.Errorf("PublishWake(%q) published RETAINED; wake-ups are never retained", e)
		}
		w, err := pipeline.ParseWake(c.payload)
		if err != nil || w.Event != e {
			t.Errorf("payload %s does not parse back to event %q (err=%v)", c.payload, e, err)
		}
	}
}

// Strict publish reaches the wire: an unknown event never gets published.
func TestPublishWake_UnknownEventPublishesNothing(t *testing.T) {
	fp := &fakePublisher{}
	if err := pipeline.PublishWake(fp, pipeline.Wake{Event: "bogus", Source: "itest"}); err == nil {
		t.Errorf("PublishWake(bogus): expected error")
	}
	if len(fp.calls) != 0 {
		t.Errorf("PublishWake(bogus) published %d messages, want 0", len(fp.calls))
	}
}

// A broker error comes back to the caller (to log). It is the CALLER's rule
// that this never fails a stage; the helper must not swallow it silently.
func TestPublishWake_ReturnsBrokerError(t *testing.T) {
	boom := errors.New("broker gone")
	fp := &fakePublisher{err: boom}
	if err := pipeline.PublishWake(fp, pipeline.Wake{Event: pipeline.EventCaptured, Source: "itest"}); !errors.Is(err, boom) {
		t.Errorf("PublishWake error = %v, want it to wrap %v", err, boom)
	}
}

// E-D3: with MQTT_BROKER unset the publish is skipped (fail-open for latency
// only). No broker is dialled, and nothing errors.
func TestAnnounceCaptured_BrokerUnsetSkips(t *testing.T) {
	if pipeline.AnnounceCaptured(t.Context(), "", "slackweb", capture.RulesStats{Considered: 3, Unmatched: 3}) {
		t.Errorf("AnnounceCaptured with no broker reported a published wake")
	}
}

type subCall struct {
	filter  string
	handler func(topic string, payload []byte)
}

type fakeSubscriber struct{ subs []subCall }

func (f *fakeSubscriber) Subscribe(filter string, handler func(topic string, payload []byte)) error {
	f.subs = append(f.subs, subCall{filter, handler})
	return nil
}

// E-D4: a stage subscribes to exactly its upstream topics and every wake on
// them notifies it.
func TestSubscribeWakes_SubscribesUpstreamTopicsAndNotifies(t *testing.T) {
	for _, stage := range pipeline.Stages() {
		fs := &fakeSubscriber{}
		var got []pipeline.Wake
		if err := pipeline.SubscribeWakes(fs, stage, func(w pipeline.Wake) { got = append(got, w) }); err != nil {
			t.Fatalf("SubscribeWakes(%q): %v", stage, err)
		}
		var filters []string
		for _, s := range fs.subs {
			filters = append(filters, s.filter)
		}
		sort.Strings(filters)
		var want []string
		for _, e := range pipeline.Upstream(stage) {
			want = append(want, pipeline.Topic(e))
		}
		sort.Strings(want)
		if !reflect.DeepEqual(filters, want) {
			t.Errorf("SubscribeWakes(%q) subscribed %v, want %v", stage, filters, want)
			continue
		}
		for i, s := range fs.subs {
			ev := pipeline.Upstream(stage)[0]
			for _, e := range pipeline.Upstream(stage) {
				if pipeline.Topic(e) == s.filter {
					ev = e
				}
			}
			payload, err := pipeline.Wake{Event: ev, Source: "itest"}.Marshal()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s.handler(s.filter, payload)
			if len(got) != i+1 {
				t.Errorf("stage %q: a wake on %s did not notify (notifications=%d)", stage, s.filter, len(got))
			}
		}
	}
}

func stageStrings(in []pipeline.Stage) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

func containsStage(in []pipeline.Stage, s pipeline.Stage) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}

func containsString(in []string, s string) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}
