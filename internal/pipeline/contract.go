// Package pipeline is SWT-40's message-level wake-up contract (Part E): topic
// shapes, the Wake payload, the static stage graph, and the per-stage loop that
// cmd/pipelined runs. Modelled on internal/fleet/contract.go.
//
// Postgres stays the queue of record; MQTT is a wake-up only (E-D1). A payload
// never carries work: a consumer re-queries its own inbox whatever the wake
// says, so a lost wake costs latency up to one sweep and a duplicate costs one
// empty inbox query. Message-level boundaries are direct MQTT publishes; the
// task-level boundary stays the orchestrator's (E-D2: task_events.task_id is
// NOT NULL, so a message with no task cannot be an orchestrator event).
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/fleet"
)

// Event vocabulary (E-D3).
const (
	EventCaptured          = "captured"
	EventGated             = "gated"
	EventRouteClassified   = "route_classified"
	EventRouted            = "routed"
	EventInquiryClassified = "inquiry_classified"
	EventPromoted          = "promoted"
)

// Events is the whole vocabulary; Marshal refuses anything else.
func Events() []string {
	return []string{EventCaptured, EventGated, EventRouteClassified, EventRouted, EventInquiryClassified, EventPromoted}
}

// Stage names one pipelined consumer.
type Stage string

const (
	StageGate           Stage = "gate"
	StageRoute          Stage = "route"
	StageRouteApply     Stage = "route_apply"
	StageInquiry        Stage = "inquiry"
	StageInquiryPromote Stage = "inquiry_promote"
)

// Stages is every consumer the graph knows.
func Stages() []Stage {
	return []Stage{StageGate, StageRoute, StageRouteApply, StageInquiry, StageInquiryPromote}
}

// Topic is ops/pipeline/{event}: QoS 1, NEVER retained (a retained wake-up
// re-fires on every reconnect, and retained state is global on the production
// broker). structure_test.go scans the repo for a retained publish.
func Topic(event string) string { return "ops/pipeline/" + event }

// WorkerID is a stage's fleet heartbeat id: dotted, so fleetd mirrors every
// stage as client `pipeline`.
func WorkerID(stage Stage) string { return "pipeline." + string(stage) }

// StageClientID is a stage's own MQTT client id. One per connection: a
// duplicate client id kicks the other connection off the broker.
func StageClientID(stage Stage) string { return "switchboard-pipeline-" + string(stage) }

// CaptureClientID is a connector main's client id for its captured publish.
func CaptureClientID(connector string) string { return "switchboard-capture-" + connector }

const (
	// PipelineSweep is the fallback that runs a stage's pass with no wake
	// (E-D4). It also bounds how late a grace-pending verdict releases.
	PipelineSweep = 5 * time.Minute
	// LockRetryDelay is how soon a stage that lost its advisory lock tries again.
	LockRetryDelay = 30 * time.Second
)

// Wake is the payload on every ops/pipeline topic. Counts and MaxID are
// diagnostic only: consumers never branch on them (the upworkcrm landmine).
type Wake struct {
	Event  string         `json:"event"`
	Source string         `json:"source"`
	Counts map[string]int `json:"counts"`
	MaxID  int64          `json:"max_id"`
	TS     time.Time      `json:"ts"`
}

func knownEvent(e string) bool {
	for _, x := range Events() {
		if x == e {
			return true
		}
	}
	return false
}

// Marshal is the strict publish path: an event outside the vocabulary errors.
func (w Wake) Marshal() ([]byte, error) {
	if !knownEvent(w.Event) {
		return nil, fmt.Errorf("event %q is not in the pipeline vocabulary", w.Event)
	}
	out, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("marshal wake: %w", err)
	}
	return out, nil
}

// ParseWake is the lenient consume path: unknown fields and events are
// tolerated; only broken JSON errors.
func ParseWake(data []byte) (Wake, error) {
	var w Wake
	if err := json.Unmarshal(data, &w); err != nil {
		return Wake{}, fmt.Errorf("parse wake payload: %w", err)
	}
	return w, nil
}

// subscribers is E-D3's table, the whole message-level stage graph. `promoted`
// wakes no stage: past promotion the next step is the orchestrator's (E-D2).
var subscribers = map[string][]Stage{
	EventCaptured:          {StageGate, StageRoute, StageInquiry},
	EventGated:             {StageInquiry},
	EventRouteClassified:   {StageRouteApply},
	EventRouted:            {StageInquiry},
	EventInquiryClassified: {StageInquiryPromote},
	EventPromoted:          nil,
}

// Subscribers is the stages an event wakes: a pure lookup returning a copy.
func Subscribers(event string) []Stage {
	return append([]Stage(nil), subscribers[event]...)
}

// Upstream is Subscribers' inverse: the events a stage subscribes to.
func Upstream(stage Stage) []string {
	var out []string
	for _, e := range Events() {
		for _, s := range subscribers[e] {
			if s == stage {
				out = append(out, e)
			}
		}
	}
	return out
}

// Publisher, Subscriber and StatusPublisher are what the pipeline needs from an
// MQTT client; *fleet.Client satisfies all three.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

type Subscriber interface {
	Subscribe(filter string, handler func(topic string, payload []byte)) error
}

type StatusPublisher interface {
	PublishStatus(s fleet.Status) error
}

// PublishWake publishes w on its topic, QoS 1, not retained. The error is for
// the caller to LOG: a publish never fails its stage (E-D3).
func PublishWake(pub Publisher, w Wake) error {
	payload, err := w.Marshal()
	if err != nil {
		return fmt.Errorf("publish wake: %w", err)
	}
	if err := pub.Publish(Topic(w.Event), 1, false, payload); err != nil {
		return fmt.Errorf("publish wake %s: %w", w.Event, err)
	}
	return nil
}

// SubscribeWakes subscribes every upstream topic of stage and calls notify on
// each wake. A payload that does not parse still notifies: the wake is the
// signal, the payload is diagnostic.
func SubscribeWakes(sub Subscriber, stage Stage, notify func(Wake)) error {
	for _, e := range Upstream(stage) {
		topic := Topic(e)
		err := sub.Subscribe(topic, func(t string, payload []byte) {
			w, err := ParseWake(payload)
			if err != nil {
				slog.Warn("malformed wake payload; waking anyway", "topic", t, "err", err)
			}
			notify(w)
		})
		if err != nil {
			return fmt.Errorf("subscribe %s: %w", topic, err)
		}
	}
	return nil
}

// CapturedWake builds the `captured` wake for one capture pass; ok is false iff
// the pass committed no decision. Every considered message is one committed
// capture_decisions row (Considered == Matched + Unmatched).
func CapturedWake(connector string, stats capture.RulesStats) (Wake, bool) {
	if stats.Considered == 0 {
		return Wake{}, false
	}
	return Wake{
		Event:  EventCaptured,
		Source: connector,
		Counts: map[string]int{
			"considered":    stats.Considered,
			"matched":       stats.Matched,
			"unmatched":     stats.Unmatched,
			"tasks_created": stats.TasksCreated,
			"appended":      stats.Appended,
		},
		TS: time.Now().UTC(),
	}, true
}

// announceTimeout bounds a connector's connect, so a dead broker never holds
// up a connector main.
const announceTimeout = 10 * time.Second

// AnnounceCaptured is a connector main's one call after capture.EvaluateRules:
// one `captured` wake iff the pass committed a decision, on a spine client
// connected only for the publish. It never returns an error — fail-open for
// LATENCY only, never correctness: the stages' sweep covers a lost wake. The
// bool reports whether a wake went out.
func AnnounceCaptured(ctx context.Context, broker, connector string, stats capture.RulesStats) bool {
	w, ok := CapturedWake(connector, stats)
	if !ok {
		return false
	}
	if broker == "" {
		slog.Info("MQTT_BROKER unset; captured wake skipped (the pipeline sweep covers it)", "connector", connector)
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, announceTimeout)
	defer cancel()
	cl, err := fleet.NewSpineClient(cctx, broker, AnnounceClientID(connector))
	if err != nil {
		slog.Warn("captured wake not published: broker unreachable", "connector", connector, "err", err)
		return false
	}
	defer cl.Disconnect()
	if err := PublishWake(cl, w); err != nil {
		slog.Warn("captured wake not published", "connector", connector, "err", err)
		return false
	}
	return true
}

// AnnounceClientID is one announce connection's client id: CaptureClientID plus
// a random suffix. Two capture passes of one connector can overlap (the google
// CronJob and its IMAP IDLE watcher), and two connections sharing an id kick
// each other off the broker. The connector name travels in the wake's Source.
func AnnounceClientID(connector string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return CaptureClientID(connector) + "-" + hex.EncodeToString(b[:])
}

// DialStage connects a stage's own client: id StageClientID(stage) and a
// RETAINED {"state":"dead"} will on its heartbeat topic, so a killed pod shows
// dead. The client publishes the heartbeat and satisfies Publisher/Subscriber.
func DialStage(ctx context.Context, broker string, stage Stage) (*fleet.Client, error) {
	return fleet.NewWillClient(ctx, broker, StageClientID(stage), WorkerID(stage))
}
