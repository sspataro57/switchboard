package jira_test

// Unit tests for SWT-32 (docs/tickets/jira-status-sync_SPEC.md) criteria 6 and 7:
// the two facts this ticket reads out of a STORED raw issue — the status
// category and the assignee — and the three-way distinction D14 turns on.
// ZERO I/O: IssueFacts is a pure function of the bytes the poller already wrote
// (invariant 7's habit applied outside the orchestrator, the same way
// normalize_test.go treats NormalizeIssue).
//
// GREENFIELD NOTE — EXPECTED RED. internal/connector/jira/facts.go does not
// exist, so this file compile-FAILs the jira_test package with "undefined:
// jira.IssueFacts" / "undefined: jira.Facts". That IS the red state for a
// spec-first test. Verified in the authoring session against a throwaway stub
// returning zero values: every assertion below then fires on its own merits
// (StatusCategory="" want "done", AssigneeKnown=false want true, and so on)
// rather than on the missing symbol.
//
// IMPOSED SURFACE — the SPEC fixes it verbatim in criterion 6, so nothing here
// is invented:
//
//	// Facts are the only two things this ticket reads out of an issue.
//	// StatusKnown / AssigneeKnown separate absent-because-impossible from
//	// absent-because-pending (D14): a null assignee is a POSITIVE statement of
//	// unassignment, an absent `assignee` key is missing evidence.
//	type Facts struct {
//	    StatusCategory string // fields.status.statusCategory.key
//	    StatusName     string // fields.status.name — DIAGNOSTIC ONLY (D2)
//	    StatusKnown    bool
//	    Assignee       string // fields.assignee.accountId
//	    AssigneeKnown  bool
//	}
//	func IssueFacts(raw json.RawMessage) (Facts, error)
//
// The category VOCABULARY is spelled with string literals below rather than
// through package constants, deliberately: 'new' | 'indeterminate' | 'done' is
// what Jira itself emits and what migration 0023's CHECK stores, so the test
// asserts the values the provider and the database will see. A constant that
// drifted from either would satisfy a test written against the constant.

import (
	"encoding/json"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/jira"
)

// factsIssue builds a stored raw issue the way the poller stores one: the full
// GET /rest/api/2/issue/{key} document with fields.comment already stripped by
// splitIssueComments. statusBlock and assigneeBlock are pasted in verbatim so a
// case can express "the key is absent" — which no Go struct can express, and
// which is exactly criterion 7's third case.
func factsIssue(key, statusBlock, assigneeBlock string) json.RawMessage {
	doc := `{"id":"10001","key":"` + key + `",` +
		`"self":"https://factstest.atlassian.net/rest/api/2/issue/10001",` +
		`"fields":{"summary":"a stored issue","description":"body",` +
		`"created":"2026-08-01T10:00:00.000+0000","updated":"2026-09-01T10:00:00.000+0000",` +
		`"reporter":{"accountId":"acc-reporter","displayName":"A Reporter"}`
	if statusBlock != "" {
		doc += `,"status":` + statusBlock
	}
	if assigneeBlock != "" {
		doc += `,"assignee":` + assigneeBlock
	}
	doc += `}}`
	return json.RawMessage(doc)
}

// statusOf renders a Jira status object the way the provider serialises it —
// name and statusCategory side by side, which is what makes D2 testable at all.
func statusOf(name, categoryKey string) string {
	return `{"self":"https://factstest.atlassian.net/rest/api/2/status/3",` +
		`"description":"","iconUrl":"","name":"` + name + `","id":"3",` +
		`"statusCategory":{"self":"https://factstest.atlassian.net/rest/api/2/statuscategory/3",` +
		`"id":3,"key":"` + categoryKey + `","colorName":"green","name":"` + name + `"}}`
}

// ---- criterion 6: the status discriminator is the CATEGORY, never the name ----

// "Unit tests: done, indeterminate and new fixtures."
//
// The three keys are a Jira-level structure — every custom workflow status maps
// into exactly one of them — which is the whole of D2's argument for reading the
// category instead of a list of names an admin can retype at will.
func TestIssueFacts_ReadsTheStatusCategory(t *testing.T) {
	for _, tc := range []struct{ name, category string }{
		{"Done", "done"},
		{"In Progress", "indeterminate"},
		{"To Do", "new"},
	} {
		tc := tc
		t.Run(tc.category, func(t *testing.T) {
			got, err := jira.IssueFacts(factsIssue("ITS-1", statusOf(tc.name, tc.category), `null`))
			if err != nil {
				t.Fatalf("IssueFacts: %v", err)
			}
			if !got.StatusKnown {
				t.Errorf("IssueFacts(status %q/%q).StatusKnown = false, want true — the issue carries "+
					"fields.status.statusCategory.key, which is the fact the whole pass turns on "+
					"(criterion 6)", tc.name, tc.category)
			}
			if got.StatusCategory != tc.category {
				t.Errorf("IssueFacts(status %q/%q).StatusCategory = %q, want %q — read from "+
					"fields.status.statusCategory.key and nothing else", tc.name, tc.category,
					got.StatusCategory, tc.category)
			}
			if got.StatusName != tc.name {
				t.Errorf("IssueFacts(status %q/%q).StatusName = %q, want %q. The name IS stored, as a "+
					"DIAGNOSTIC column (ticket_status_syncs.status_name) so a human reading the report "+
					"sees 'Won't Do' rather than 'done' — but nothing branches on it (D2)",
					tc.name, tc.category, got.StatusName, tc.name)
			}
		})
	}
}

// D2, sharpened into the one case that catches a name list: a status NAMED
// `Closed` whose category is `indeterminate`.
//
// This is not a hypothetical. Jira status names are per-project workflow
// configuration and an admin may map anything into any category; a reader that
// keyed on {'Done','Closed','Resolved',...} would drop this ticket's task off
// the board while the ticket is still open work. The corresponding negative —
// a status NAMED "In Progress" that an admin mapped into `done` — must close.
func TestIssueFacts_NeverConsultsTheStatusName(t *testing.T) {
	got, err := jira.IssueFacts(factsIssue("ITS-2", statusOf("Closed", "indeterminate"), `null`))
	if err != nil {
		t.Fatalf("IssueFacts: %v", err)
	}
	if got.StatusCategory != "indeterminate" {
		t.Errorf("IssueFacts(name=Closed, category=indeterminate).StatusCategory = %q, want "+
			"\"indeterminate\". D2: the NAME is never consulted — a status-name list is this repo's "+
			"recurring magic-literal defect in a fresh costume, and it would pass every fixture until "+
			"the day a client renames a column", got.StatusCategory)
	}

	got, err = jira.IssueFacts(factsIssue("ITS-3", statusOf("In Progress", "done"), `null`))
	if err != nil {
		t.Fatalf("IssueFacts: %v", err)
	}
	if got.StatusCategory != "done" {
		t.Errorf("IssueFacts(name=\"In Progress\", category=done).StatusCategory = %q, want \"done\" — "+
			"the mirror image of the case above, and the one where a name list keeps a finished "+
			"ticket's task on the board forever", got.StatusCategory)
	}
}

// "missing fields.status -> StatusKnown=false; a status with no statusCategory
// -> StatusKnown=false."
//
// Both are EVIDENCE GAPS, and criterion 32 spends them as `unreadable` — no
// action in either direction. A reader that folded either into "" and let the
// caller compare it against 'done' would leave the task alone by luck rather
// than by decision; a reader that folded it into 'done' would drop a task
// because a field was missing.
func TestIssueFacts_UnknownStatusIsNotAnEmptyCategory(t *testing.T) {
	for _, tc := range []struct{ name, statusBlock string }{
		{"no status field at all", ""},
		{"status with no statusCategory", `{"name":"Done","id":"3"}`},
		{"status is null", `null`},
		{"statusCategory with no key", `{"name":"Done","statusCategory":{"id":3,"colorName":"green"}}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := jira.IssueFacts(factsIssue("ITS-4", tc.statusBlock, `null`))
			if err != nil {
				t.Fatalf("IssueFacts: %v", err)
			}
			if got.StatusKnown {
				t.Errorf("IssueFacts(%s).StatusKnown = true (category %q), want false. Criterion 32: "+
					"an unreadable status is counted and NOTHING happens to the task — a task must "+
					"never vanish off the board because a field was absent", tc.name, got.StatusCategory)
			}
			if got.StatusCategory != "" {
				t.Errorf("IssueFacts(%s).StatusCategory = %q, want \"\" when StatusKnown is false. A "+
					"category the reader invented is worse than no category: migration 0023's CHECK "+
					"only accepts new/indeterminate/done, so an invented value fails at INSERT time in "+
					"the driver instead of being refused here", tc.name, got.StatusCategory)
			}
		})
	}
}

// ---- criterion 7: three assignee shapes, and the two that look alike ---------

// "assignee: {accountId} -> Assignee=..., AssigneeKnown=true; assignee: null ->
// Assignee="", AssigneeKnown=true (a positive statement of unassignment);
// fields with no assignee key -> AssigneeKnown=false."
//
// The last two are the recorded absent-because-impossible / absent-because-
// pending landmine in its most literal form: they are DISTINGUISHABLE in the
// JSON, so the reader must distinguish them. Criterion 7 names the mechanism —
// probe `fields` as map[string]json.RawMessage; a struct with a *jiraUser field
// unmarshals both shapes to nil and cannot tell them apart, which is precisely
// how a task with missing evidence would be dropped as "unassigned".
func TestIssueFacts_ThreeAssigneeShapes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		block        string
		wantAssignee string
		wantKnown    bool
		wantKnownWhy string
	}{
		{
			name:         "assigned",
			block:        `{"accountId":"5b1abc9876543210fedcba01","displayName":"Salvador S","active":true}`,
			wantAssignee: "5b1abc9876543210fedcba01",
			wantKnown:    true,
			wantKnownWhy: "an accountId is the identity D12 compares against the account's own_account_id",
		},
		{
			name:         "explicitly unassigned",
			block:        `null`,
			wantAssignee: "",
			wantKnown:    true,
			wantKnownWhy: "D14: Jira serialises an unassigned issue as \"assignee\": null — the key is " +
				"PRESENT with a null value, which is a positive fact and is acted on (unassigned counts " +
				"as not-mine)",
		},
		{
			name:         "no assignee key in fields",
			block:        "",
			wantAssignee: "",
			wantKnown:    false,
			wantKnownWhy: "evidence MISSING, not evidence of absence: criterion 32 counts this " +
				"`unreadable` and takes no action in either direction",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := jira.IssueFacts(factsIssue("ITS-5", statusOf("In Progress", "indeterminate"), tc.block))
			if err != nil {
				t.Fatalf("IssueFacts: %v", err)
			}
			if got.AssigneeKnown != tc.wantKnown {
				t.Errorf("IssueFacts(%s).AssigneeKnown = %v, want %v — %s",
					tc.name, got.AssigneeKnown, tc.wantKnown, tc.wantKnownWhy)
			}
			if got.Assignee != tc.wantAssignee {
				t.Errorf("IssueFacts(%s).Assignee = %q, want %q",
					tc.name, got.Assignee, tc.wantAssignee)
			}
			// The status half must be unaffected by any assignee shape: the two
			// facts are read independently, and criterion 32's gate cases only
			// make sense if a missing assignee leaves the status readable.
			if !got.StatusKnown || got.StatusCategory != "indeterminate" {
				t.Errorf("IssueFacts(%s) lost the status while reading the assignee: {%q, known=%v}",
					tc.name, got.StatusCategory, got.StatusKnown)
			}
		})
	}
}

// An `assignee` object with no accountId at all — a shape Jira emits for a
// deleted or anonymised user. Known (the key is present, someone is named) but
// with no id to compare, which must not silently read as "assigned to me" when
// own_account_id is also empty. The pass's fail-safe (D12) then counts it
// unreadable; what this test pins is that IssueFacts does not invent an id.
func TestIssueFacts_AssigneeWithNoAccountID(t *testing.T) {
	got, err := jira.IssueFacts(factsIssue("ITS-6", statusOf("To Do", "new"),
		`{"displayName":"Former Employee","active":false}`))
	if err != nil {
		t.Fatalf("IssueFacts: %v", err)
	}
	if got.Assignee != "" {
		t.Errorf("IssueFacts(assignee with no accountId).Assignee = %q, want \"\" — there is no "+
			"accountId in the document and D12's comparison is on accountId ONLY (no display-name "+
			"matching, ever: the slackweb landmine)", got.Assignee)
	}
}

// ---- the reader's own refusals ------------------------------------------------

// Malformed bytes are an ERROR, not a zero Facts. The distinction matters
// because the driver treats an error and an unknown fact differently: an
// unreadable fact is a counted no-op on ONE ref, while bytes that will not parse
// mean the stored row is corrupt and the operator should hear about it.
func TestIssueFacts_RefusesUnparseableBytes(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"not json", `{"key":"ITS-7"`},
		{"fields is not an object", `{"key":"ITS-7","fields":42}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := jira.IssueFacts(json.RawMessage(tc.raw)); err == nil {
				t.Errorf("IssueFacts(%s) = nil error, want a parse failure: a stored row that will not "+
					"parse is a corrupt snapshot, and returning a zero Facts would make it look like "+
					"an ordinary evidence gap", tc.name)
			}
		})
	}
}

// Jira's REAL fourth category key — `undefined` ("No Category", id 1) — is an
// evidence gap, not a value: migration 0023's CHECK admits exactly
// new/indeterminate/done, so a fourth key passed through as "known" would
// abort the reconciler at INSERT time, every tick, at the same ref
// (go-reviewer F1, 2026-09-09; added with the fix).
func TestIssueFacts_UnrecognisedCategoryKeyIsAnEvidenceGap(t *testing.T) {
	for _, key := range []string{"undefined", "something-new-from-atlassian"} {
		key := key
		t.Run(key, func(t *testing.T) {
			block := `{"name":"No Category","id":"1","statusCategory":{"id":1,"key":"` + key + `","name":"No Category"}}`
			got, err := jira.IssueFacts(factsIssue("ITS-8", block, `null`))
			if err != nil {
				t.Fatalf("IssueFacts: %v", err)
			}
			if got.StatusKnown || got.StatusCategory != "" {
				t.Errorf("IssueFacts(category key %q) = {known=%v, category=%q}, want an evidence gap — "+
					"the pass counts it unreadable and touches nothing, instead of wedging on the "+
					"state table's CHECK", key, got.StatusKnown, got.StatusCategory)
			}
		})
	}
}
