package pipeline

import (
	"context"
	"log/slog"
	"time"
)

// downstream is E-D3's "published by" column: the wake a stage publishes after
// a pass that moved rows out of its inbox. Every successor it names subscribes
// to that event in the subscribers table, so the two halves of the graph are
// one fact read in two directions (downstream_test.go checks they agree).
var downstream = map[Stage]string{
	StageGate:           EventGated,
	StageRoute:          EventRouteClassified,
	StageRouteApply:     EventRouted,
	StageInquiry:        EventInquiryClassified,
	StageInquiryPromote: EventPromoted,
}

// Downstream is the event a stage publishes after a pass that processed rows:
// a pure lookup.
func Downstream(stage Stage) (event string, ok bool) {
	event, ok = downstream[stage]
	return event, ok
}

// PublishAfterPass wraps pass so that a pass reporting processed > 0 publishes
// ONE wake for Downstream(stage) through pub (PublishWake: QoS 1, never
// retained; Source is the stage name; Counts carries {"processed": n}, which is
// diagnostic only).
//
// It is a WRAPPER, applied by cmd/pipelined when it builds each stage, and
// never part of the loop core in stage.go: a publish inside the loop would
// couple every stage's drain to the broker's health.
//
// The pass's (n, err) comes back UNCHANGED. A publish failure is logged, never
// returned (E-D3: a publish never fails its stage). A pass that committed rows
// and then errored still announces them: those rows are in the downstream
// inbox either way, and the wake is only latency. A nil pub publishes nothing.
func PublishAfterPass(stage Stage, pub Publisher, pass PassFunc) PassFunc {
	event, ok := Downstream(stage)
	return func(ctx context.Context) (int, error) {
		n, err := pass(ctx)
		if n <= 0 || !ok || pub == nil {
			return n, err
		}
		w := Wake{Event: event, Source: string(stage), Counts: map[string]int{"processed": n}, TS: time.Now().UTC()}
		if perr := PublishWake(pub, w); perr != nil {
			slog.Warn("downstream wake not published; the sweep covers it", "stage", stage, "event", event, "err", perr)
		}
		return n, err
	}
}
