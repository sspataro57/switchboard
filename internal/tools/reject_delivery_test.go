package tools

// validateRejectDelivery — SWT-43 (docs/tickets/delivery-deny_SPEC.md)
// criterion 5. ZERO network, ZERO Postgres.
//
// WHY `package tools`: the dismiss_test.go reason. Driving Execute with a nil
// pool can only assert REFUSALS (an accepted call runs the handler and panics
// on the nil pool), and half of criterion 5 is what is ACCEPTED. The
// registration half (reject_delivery reaches Validate at all) lives in
// tools_unit_test.go's allToolNames / toolsUnderTest.
//
// GREENFIELD NOTE — EXPECTED RED. validateRejectDelivery does not exist, so this
// file compile-FAILS internal/tools until delivery.go declares it.
//
// IMPOSED SURFACE (SPEC "API / MCP tool changes" + "Files likely to touch"):
//
//	type rejectDeliveryArgs struct {
//	    DeliveryID int64  `json:"delivery_id"`
//	    Note       string `json:"note,omitempty"`
//	    Redraft    bool   `json:"redraft,omitempty"`
//	}
//	func validateRejectDelivery(args []byte) error
//
// `redraft` defaults to false. A whitespace-only note is stored as NULL — that
// half is the HANDLER's and is asserted in reject_delivery_integration_test.go.

import (
	"strings"
	"testing"
)

// rejectNoteMaxRunes is criterion 5's cap. The note travels into a model
// prompt (criterion 22), so it is bounded; RUNES, not bytes, because Salvador
// writes Italian and a byte cap would refuse an accented note at half length.
const rejectNoteMaxRunes = 2000

func TestValidateRejectDelivery_RefusesIncompleteArgs(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"empty object", `{}`},
		{"zero delivery_id", `{"delivery_id":0}`},
		{"missing delivery_id with a note", `{"note":"wrong tone"}`},
		{"missing delivery_id with redraft", `{"redraft":true}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateRejectDelivery([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateRejectDelivery(%s) = nil, want a refusal. A verdict with no delivery "+
					"named would reject nothing, or worse, default to some row", tc.args)
			}
			if !strings.Contains(err.Error(), "delivery_id") {
				t.Errorf("validateRejectDelivery(%s) = %q, which does not name delivery_id", tc.args, err)
			}
		})
	}
}

// The cap is in runes. 2,000 two-byte runes (4,000 bytes) is legal; one more
// rune is not. A byte-counting validator passes the ASCII case and fails the
// first one, which is exactly the split this test needs to see.
func TestValidateRejectDelivery_NoteCapIsTwoThousandRunes(t *testing.T) {
	atCap := strings.Repeat("é", rejectNoteMaxRunes)
	if err := validateRejectDelivery([]byte(`{"delivery_id":7,"note":"` + atCap + `"}`)); err != nil {
		t.Errorf("a %d-rune note (%d bytes) was refused: %v. Criterion 5 caps RUNES; a byte cap refuses "+
			"accented text at half the intended length", rejectNoteMaxRunes, len(atCap), err)
	}
	for _, over := range []string{
		strings.Repeat("é", rejectNoteMaxRunes+1),
		strings.Repeat("a", rejectNoteMaxRunes+1),
	} {
		err := validateRejectDelivery([]byte(`{"delivery_id":7,"note":"` + over + `"}`))
		if err == nil {
			t.Errorf("a %d-rune note was accepted; criterion 5 caps it at %d (it travels into a model prompt)",
				len([]rune(over)), rejectNoteMaxRunes)
			continue
		}
		if !strings.Contains(err.Error(), "note") {
			t.Errorf("over-cap refusal %q does not name the note field", err)
		}
	}
}

// A non-boolean redraft is refused rather than coerced. "true" as a STRING is
// what a hand-built form payload produces; coercing it would make the Redo
// decision depend on how a caller spelled it.
func TestValidateRejectDelivery_RedraftMustBeABoolean(t *testing.T) {
	for _, args := range []string{
		`{"delivery_id":7,"redraft":"true"}`,
		`{"delivery_id":7,"redraft":"false"}`,
		`{"delivery_id":7,"redraft":1}`,
		`{"delivery_id":7,"redraft":"yes"}`,
	} {
		if err := validateRejectDelivery([]byte(args)); err == nil {
			t.Errorf("validateRejectDelivery(%s) = nil, want a refusal: redraft is a JSON boolean (criterion 5)", args)
		}
	}
}

func TestValidateRejectDelivery_Accepts(t *testing.T) {
	for _, args := range []string{
		`{"delivery_id":7}`, // redraft defaults to false, note optional
		`{"delivery_id":7,"redraft":false}`,
		`{"delivery_id":7,"redraft":true}`,
		`{"delivery_id":7,"note":""}`,
		`{"delivery_id":7,"note":"   "}`, // stored as NULL by the handler, not refused here
		`{"delivery_id":7,"note":"shorter, no apology","redraft":true}`,
	} {
		if err := validateRejectDelivery([]byte(args)); err != nil {
			t.Errorf("validateRejectDelivery(%s) = %v, want accepted", args, err)
		}
	}
}
