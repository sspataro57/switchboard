package orchestrator

import "testing"

// SWT-109 criterion 26: R12 is pure. An expired hold's offer fires one
// release_offer_holds {reason: expired}; a kept hold, a plain booking and an
// empty list fire nothing (the facts query never lists them, so the rule
// sees an empty list for all three).
func TestRuleHoldExpiry(t *testing.T) {
	acts := ruleHoldExpiry(Facts{ExpiredHoldOffers: []int64{236}})
	if len(acts) != 1 || acts[0].Tool != "release_offer_holds" ||
		acts[0].Args["delivery_id"] != int64(236) || acts[0].Args["reason"] != "expired" {
		t.Fatalf("expired hold: %+v", acts)
	}
	for _, f := range []Facts{{}, {ExpiredHoldOffers: nil}} {
		if acts := ruleHoldExpiry(f); len(acts) != 0 {
			t.Errorf("no expired offers fired %+v", acts)
		}
	}
	tick := Evaluate(Event{Type: EventTick}, Facts{ExpiredHoldOffers: []int64{7}}, Config{})
	found := false
	for _, a := range tick {
		if a.Tool == "release_offer_holds" {
			found = true
		}
		if a.Tool == "record_orchestration" {
			t.Errorf("R12 records no orchestration row")
		}
	}
	if !found {
		t.Errorf("the tick does not run R12: %+v", tick)
	}
}
