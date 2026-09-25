package dashboard

// SWT-89 (board-streaming) criterion 24: the external integration suite finds
// the board's panels through their `section-…` ids, built from Go's own section
// keys, so neither the template nor the test spells a key (the
// TestTasksTemplate_NoIncoming discipline). The standard export_test idiom: a
// test-only export compiled into package dashboard for package dashboard_test.

// BoardLiveSectionKeys returns boardSectionOrder's keys, in board order.
func BoardLiveSectionKeys() []string {
	keys := make([]string, 0, len(boardSectionOrder))
	for _, s := range boardSectionOrder {
		keys = append(keys, s.Key)
	}
	return keys
}

// BoardNotifyTables exposes boardNotifyTables to the external integration
// suite's column-type guard.
func BoardNotifyTables() []string { return append([]string(nil), boardNotifyTables...) }
