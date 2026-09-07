package mcpserver_test

// The MCP surface for calendar booking (SWT-28 /
// docs/tickets/calendar-booking_SPEC.md, acceptance criterion 25). ZERO
// network: the adapter is driven with adapter_test.go's fakeExec and worker id.
//
// WHY THE LISTING IS PART OF THE CONTRACT AND NOT A DETAIL. Q1 was answered (b):
// the auto tier ships now, which means "a Claude Code worker books its own
// focus time with no human in the loop" has to be TRUE. `book_calendar_block`
// is the only verb that can do it — send_delivery is policy.humanOnly and
// widening that gate was explicitly off the table — so if the tool is not in
// tools/list, the adapter rejects it by name before the executor is ever
// reached and the entire ticket is unreachable from the surface it was built
// for. Criterion 24's cmd/ops-mcp wiring is the other half of the same claim.
//
// The register to write its Description in is mark_delivery_sent's narrowing
// comment (delivery.go:680-698): say what an INJECTED call can and cannot do.
// Here the honest answer is: it can put a block on Salvador's own calendar, at
// a time provably free, on an account a human explicitly write-enabled, at most
// ten per hour, every call audited, stoppable with set_sending_frozen.
//
// GREENFIELD NOTE: schemas.go has no book_calendar_block entry, so both tests
// here FAIL today. They deliberately do NOT edit adapter_test.go —
// TestListTools_ExactlyAgentAllowlist's `wantAgentTools` must gain
// "book_calendar_block" in the same change (criterion 25 says so explicitly),
// and that pre-existing test going red is the signal, not a surprise.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/mcpserver"
)

// Criterion 25: book_calendar_block is MCP-listed, with {"delivery_id":
// integer} required and nothing else — the same arg shape send_delivery uses,
// which is what lets policy's pgloader resolve the channel snapshot with no
// loader change (SPEC premise 15).
func TestListTools_IncludesBookCalendarBlock(t *testing.T) {
	srv := mcpserver.New(&fakeExec{}, testWorkerID)

	list := srv.ListTools()
	var found *mcpserver.Tool
	for i := range list {
		if list[i].Name == "book_calendar_block" {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatal("book_calendar_block is not in tools/list. Q1 answered (b) — the auto tier ships now — and " +
			"this is the ONLY verb an agent can use to book: send_delivery is policy.humanOnly and widening " +
			"that shared gate was off the table. Unlisted, the adapter refuses the name before the executor " +
			"is reached and every worker call fails, which makes the ticket's central claim untrue")
	}
	if found.Description == "" {
		t.Error("book_calendar_block has no description; an agent-facing verb that writes to the outside " +
			"world must say what it does and what stops it (kill switch, hourly limit, calendar_write_enabled, " +
			"and the conflict/freshness/horizon refusal)")
	}

	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(found.InputSchema, &schema); err != nil {
		t.Fatalf("book_calendar_block InputSchema is not a JSON Schema object: %v (%s)", err, found.InputSchema)
	}
	if _, ok := schema.Properties["delivery_id"]; !ok {
		t.Errorf("book_calendar_block schema has no delivery_id property (%s)", found.InputSchema)
	}
	if len(schema.Properties) != 1 {
		t.Errorf("book_calendar_block schema has %d properties (%s), want exactly delivery_id. Its args are "+
			"{delivery_id} ONLY so policy's pgloader resolves the channel snapshot with no loader change "+
			"(premise 15) — and so an injected call cannot choose a time, a calendar or a summary; all three "+
			"come from the drafted row", len(schema.Properties), found.InputSchema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "delivery_id" {
		t.Errorf("book_calendar_block required = %v, want [delivery_id]", schema.Required)
	}

	// Out of scope, pinned so it cannot half-arrive: attendee invites are the
	// approve-tier matrix row. A later `attendees` argument here would promote
	// that row to auto without any policy change being visible.
	for _, banned := range []string{"attendees", "start", "end", "calendar_id", "target_ref", "summary"} {
		if _, present := schema.Properties[banned]; present {
			t.Errorf("book_calendar_block schema exposes %q. The verb approves and sends an ALREADY DRAFTED "+
				"row; every field of the booking comes from that row, which went through draft_delivery's "+
				"validation and ScrubAIAttribution. %q here would let an injected call choose it directly",
				banned, banned)
		}
	}
}

// Criterion 25's other half: draft_delivery's schema admits the calendar
// channel and the two interval fields. Without this an agent cannot produce the
// row book_calendar_block consumes, so the surface would be listed but unusable.
func TestListTools_DraftDeliverySchemaAdmitsCalendar(t *testing.T) {
	srv := mcpserver.New(&fakeExec{}, testWorkerID)

	var raw json.RawMessage
	for _, tl := range srv.ListTools() {
		if tl.Name == "draft_delivery" {
			raw = tl.InputSchema
		}
	}
	if len(raw) == 0 {
		t.Fatal("draft_delivery is not listed")
	}

	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("draft_delivery InputSchema does not parse: %v (%s)", err, raw)
	}

	ch, ok := schema.Properties["channel"]
	if !ok {
		t.Fatalf("draft_delivery schema has no channel property (%s)", raw)
	}
	if len(ch.Enum) == 0 {
		t.Fatalf("draft_delivery's channel has no enum (%s); the enum is what tells a model which channels "+
			"exist at all", raw)
	}
	if !calendarEnumContains(ch.Enum, "calendar") {
		t.Errorf("draft_delivery's channel enum is %v, want \"calendar\" among them. An agent that cannot "+
			"draft a calendar row cannot reach book_calendar_block, so the auto tier stops at the schema",
			ch.Enum)
	}
	for _, f := range []string{"start", "end"} {
		if _, present := schema.Properties[f]; !present {
			t.Errorf("draft_delivery schema is missing %q (RFC3339, calendar only). The interval is the "+
				"calendar row's identity — deliveries_calendar_identity_check refuses the row without it — "+
				"and a model cannot supply a field the schema does not name", f)
		}
	}

	// The description should point at where the time comes from: the matrix row
	// is "auto (always via availability service propose_slots)".
	for _, tl := range srv.ListTools() {
		if tl.Name == "draft_delivery" && !strings.Contains(strings.ToLower(tl.Description), "calendar") {
			t.Logf("note: draft_delivery's description does not mention the calendar channel; not a failure, "+
				"but propose_slots is where a model should get start/end (description = %q)", tl.Description)
		}
	}
}

func calendarEnumContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
