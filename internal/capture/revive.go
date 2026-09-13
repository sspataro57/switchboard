package capture

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md) J1: which capture rules'
// matches count as Jira activity that revives a closed task (or creates one) and
// surfaces it past the reconciler. Pure — the flags arrive as values read from
// the capture_rules and projects columns.

// overrides reports whether a match by a rule with these flags acts as activity
// on a project whose assignee gate is gateOn:
//
//	overrides = revive AND (NOT gateOn OR addressed)
//
// On a gate-off project `revive` alone overrides (owner decision 1: any
// activity). On a gated project only `addressed` does (decision 3: only mail
// addressed to him bypasses the assignee check); a revive-only rule there still
// goes through the SWT-40 Part D gate, so an operator cannot bypass the gate by
// forgetting which flag means what. `addressed` without `revive` is refused by
// migration 0030's CHECK and overrides nothing here either.
func overrides(revive, addressed, gateOn bool) bool {
	return revive && (!gateOn || addressed)
}
