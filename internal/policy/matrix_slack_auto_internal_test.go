package policy

// slack-auto-tier (SWT-77) criteria 10 and 24, the MAP SHAPE. Internal because
// the maps are unexported (mcpverbs_internal_test.go's reason). ZERO I/O.
//
// D4: send_slack_reply is sendShaped (the loader, the rate limit, the channel
// branch) AND freezeGated (the kill switch) — book_calendar_block's shape.
// D6/D7: it is in NEITHER humanOnly NOR mcpHumanOnly. mcpHumanOnly is the trap:
// matrix.Check routes an mcpHumanOnly tool that passes the actor test to the
// STATIC allow-list, so the snapshot loader — and with it every brake — never
// runs.
//
// GREENFIELD NOTE — EXPECTED RED until matrix.go names the tool.
//
// MUTATIONS THAT MUST TURN THIS RED: drop it from sendShaped (the loader never
// runs), drop it from freezeGated (set_sending_frozen stops nothing), add it
// to humanOnly (worker consoles refused, D6) or mcpHumanOnly (D7's trap).

import "testing"

func TestPolicy_SendSlackReplyShape(t *testing.T) {
	const tool = "send_slack_reply"
	for name, m := range map[string]map[string]bool{
		"sendShaped": sendShaped, "freezeGated": freezeGated, "snapshotGated": snapshotGated,
	} {
		if !m[tool] {
			t.Errorf("send_slack_reply is not in %s (criterion 10 / D4): without it the verb is an allow with "+
				"no rate limit and no kill switch — the auto tier with both brakes missing", name)
		}
	}
	for name, m := range map[string]map[string]bool{"humanOnly": humanOnly, "mcpHumanOnly": mcpHumanOnly} {
		if m[tool] {
			t.Errorf("send_slack_reply is in %s (criterion 10). D6: both profiles, every conversation. D7: "+
				"mcpHumanOnly would route it to the static allow-list and skip the loader entirely", name)
		}
	}
}

// Criterion 24: the existing delivery verbs keep their entries exactly.
func TestPolicy_SlackAutoTier_ExistingEntriesUnchanged(t *testing.T) {
	for _, tool := range []string{
		"send_delivery", "approve_delivery", "update_delivery", "prefill_delivery",
		"mark_delivery_sent", "mark_delivery_failed", "reject_delivery",
	} {
		if !humanOnly[tool] {
			t.Errorf("%s left humanOnly (criterion 24: its policy entry is unchanged)", tool)
		}
	}
	if !sendShaped["send_delivery"] || !freezeGated["send_delivery"] {
		t.Errorf("send_delivery must stay sendShaped and freezeGated")
	}
	if !sendShaped["mark_delivery_sent"] || freezeGated["mark_delivery_sent"] {
		t.Errorf("mark_delivery_sent must stay sendShaped and NOT freezeGated (SWT-12 Q4)")
	}
	for _, tool := range []string{"approve_delivery", "update_delivery", "prefill_delivery", "mark_delivery_failed", "reject_delivery"} {
		if sendShaped[tool] || freezeGated[tool] {
			t.Errorf("%s gained a send-shaped/freeze entry; criterion 24 keeps its policy entries unchanged", tool)
		}
	}
	if !sendShaped["book_calendar_block"] || !freezeGated["book_calendar_block"] {
		t.Errorf("book_calendar_block's auto-tier entries must be unchanged")
	}
}
