package policy

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criterion 6, the map half.
// Internal (package policy) because the maps are unexported. ZERO I/O.
//
// GREENFIELD NOTE — EXPECTED RED: reject_delivery is in no map yet.

import "testing"

func TestPolicy_RejectDeliveryMapMembership(t *testing.T) {
	if !humanOnly["reject_delivery"] {
		t.Errorf("reject_delivery is not in humanOnly. D9: a delivery verdict on a worker's own words belongs " +
			"to the human; it joins mark_delivery_failed and prefill_delivery")
	}
	for name, m := range map[string]map[string]bool{
		"sendShaped":    sendShaped,
		"freezeGated":   freezeGated,
		"snapshotGated": snapshotGated,
		"mcpHumanOnly":  mcpHumanOnly,
	} {
		if m["reject_delivery"] {
			t.Errorf("reject_delivery is in %s. Criterion 6: it moves a row AWAY from the world, so neither the "+
				"loader, the rate limit nor the kill switch has a claim on it; and it is humanOnly, not a "+
				"transport rule", name)
		}
	}
}
