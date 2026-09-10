package main

// SWT-33 criterion 23 and D6's CLI half. `loadLabels` is a pure file parse with
// ZERO I/O beyond the file it is handed, which is the whole reason this can be a
// unit test at all — no database, no model, no network.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// The lane threads through, exactly as it already does for --labels'
//	// default and for the run config. Criterion 23: "loadLabels validates
//	// against the lane's positive token so a personal file cannot be scored as
//	// an inquiry file and vice versa" — which a (path) signature cannot do,
//	// because the token is the only thing that distinguishes the two files.
//	func loadLabels(path string, lane classify.Lane) ([]classify.Label, error)
//
// GREENFIELD NOTE — EXPECTED RED. loadLabels takes only a path today and
// classify.LaneInquiry does not exist, so this file compile-FAILS cmd/classify
// with "too many arguments in call to loadLabels" and "undefined:
// classify.LaneInquiry". That IS the red state for a spec-first test.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/classify"
)

func writeLabels(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "labels.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

const (
	lblActionable = `{"message_id":1,"label":"actionable","subject_sha256":"0123456789abcdef"}`
	lblNeedsReply = `{"message_id":1,"label":"needs_reply","subject_sha256":"0123456789abcdef"}`
	lblNot        = `{"message_id":2,"label":"not","subject_sha256":"fedcba9876543210"}`
)

// ---- criterion 23: the vocabulary is the LANE'S ------------------------------

// The two files' positive tokens are the only thing that tells them apart —
// same shape, same keys, same hash format — so a loader that accepts both
// accepts a personal file as an inquiry set. The consequence is not a crash: it
// is a recall/precision pair computed over labels that answer a different
// question, printed with no warning, in a runbook table beside numbers that are
// real.
func TestLoadLabels_RefusesTheOtherLanesVocabulary(t *testing.T) {
	cases := []struct {
		name    string
		lane    classify.Lane
		line    string
		wantErr bool
		why     string
	}{
		{"inquiry file on the inquiry lane", classify.LaneInquiry, lblNeedsReply, false,
			"the positive control: needs_reply is this lane's token and must load"},
		{"the shared negative on the inquiry lane", classify.LaneInquiry, lblNot, false,
			"`not` is the negative token in BOTH vocabularies (criterion 22: needs_reply | not)"},
		{"personal file on the inquiry lane", classify.LaneInquiry, lblActionable, true,
			"an `actionable` label scored as needs_reply would make two different questions' " +
				"recall/precision falsely comparable — D1's whole argument for a second contract"},
		{"personal file on the personal lane", classify.LanePersonal, lblActionable, false,
			"the positive control in the other direction: SWT-22's file must still load unchanged"},
		{"inquiry file on the personal lane", classify.LanePersonal, lblNeedsReply, true,
			"and the refusal is symmetric, or `--lane personal --labels inquiry-needs-reply.jsonl` " +
				"silently scores 40 conversation labels as bills"},
		{"personal file on the residue lane", classify.LaneResidue, lblActionable, false,
			"the residue shares the personal lane's contract and its vocabulary (SWT-23 criterion 10); " +
				"the token check must not split the two lanes that DO share one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadLabels(writeLabels(t, tc.line), tc.lane)
			switch {
			case tc.wantErr && err == nil:
				t.Errorf("loadLabels(%s on --lane %s) returned %d label(s) and no error. %s",
					tc.line, tc.lane.Name, len(got), tc.why)
			case !tc.wantErr && err != nil:
				t.Errorf("loadLabels(%s on --lane %s) = %v. %s", tc.line, tc.lane.Name, err, tc.why)
			}
		})
	}
}

// The refusal has to NAME the token, or the operator's next move is to edit the
// file rather than the flag.
func TestLoadLabels_RefusalNamesTheExpectedToken(t *testing.T) {
	_, err := loadLabels(writeLabels(t, lblActionable), classify.LaneInquiry)
	if err == nil {
		t.Fatalf("loadLabels accepted an `actionable` label on the inquiry lane")
	}
	if !strings.Contains(err.Error(), classify.LaneInquiry.Contract.PositiveLabel) {
		t.Errorf("the refusal does not name the expected token %q: %v",
			classify.LaneInquiry.Contract.PositiveLabel, err)
	}
}

// ---- criterion 22: the file carries NO message content ----------------------

// Unchanged from SWT-22 and re-asserted for the third file, because the reason
// is unchanged: the set is COMMITTED, and a subject or body in it puts client
// conversation into git. The inquiry set is drawn from a client's Slack, which
// makes it the most sensitive of the three.
func TestLoadLabels_StillRefusesMessageContent(t *testing.T) {
	for _, banned := range []string{"subject", "body", "body_text", "sender"} {
		line := `{"message_id":1,"label":"needs_reply","subject_sha256":"0123456789abcdef","` +
			banned + `":"leaked"}`
		if _, err := loadLabels(writeLabels(t, line), classify.LaneInquiry); err == nil {
			t.Errorf("loadLabels accepted a line carrying a %q key. The labelled set is ids, labels and a "+
				"subject hash — nothing else. It is committed to a git repo", banned)
		}
	}
	// Control: the same line WITHOUT the banned key loads, so the refusal above
	// is about the key and not about the fixture.
	if _, err := loadLabels(writeLabels(t, lblNeedsReply), classify.LaneInquiry); err != nil {
		t.Fatalf("POSITIVE CONTROL FAILED: a clean inquiry label line was refused: %v", err)
	}
}

// ---- criterion 1 + D6: the CLI surface ---------------------------------------

// A source scan, because the flag strings and the usage line are prose that no
// behavioural test reaches — and an operator who cannot spell the flag cannot
// run the lane at all.
func TestClassifyMain_OffersTheInquiryLaneAndSaysSinceIsRequired(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read cmd/classify/main.go: %v", err)
	}
	src := string(raw)

	// POSITIVE CONTROL: the two shipped lanes are still offered.
	for _, keep := range []string{"personal", "residue"} {
		if !strings.Contains(src, keep) {
			t.Fatalf("POSITIVE CONTROL FAILED: cmd/classify/main.go no longer mentions the %q lane", keep)
		}
	}
	if !strings.Contains(src, "inquiry") {
		t.Errorf("cmd/classify/main.go never mentions the inquiry lane. Criterion 1: " +
			"`classify run|report|eval --lane inquiry` resolves it, and the flag's own help text is the " +
			"only place an operator learns the third spelling exists")
	}
	// buildRouter's general = nil is the OTHER half of criterion 11's argument
	// and it must not be "fixed" to accommodate this lane's ClassGeneral
	// messages. Pinned here because that is exactly the change a reader would
	// make after seeing no_general_provider skips.
	if !strings.Contains(src, "provider.NewRouter(nil,") {
		t.Errorf("cmd/classify's buildRouter no longer constructs the router with a NIL general client. " +
			"That nil is this lane's containment (criterion 10): there is nothing to fall back TO. " +
			"Criterion 11 pins the ROUTED CLASS to restricted precisely so the lane works WITHOUT a " +
			"hosted client — adding one to make the skips go away is the single change the whole " +
			"boundary exists to prevent")
	}
}

// Codex adversarial review, round 8: the record is CLOSED. A free-text note or
// an unknown key is one paste away from committing a client's message.
func TestLoadLabels_RefusesFreeTextNotesAndUnknownKeys(t *testing.T) {
	// The key "note" with its "t" written as a JSON unicode escape (backslash,
	// u, 0074). Built from rune 92 so this source holds no escape sequence that
	// an editor or a tool could decode away.
	escapedNoteKey := "no" + string(rune(92)) + "u0074e"
	if !strings.Contains(escapedNoteKey, string(rune(92))) {
		t.Fatalf("POSITIVE CONTROL FAILED: escapedNoteKey %q carries no backslash", escapedNoteKey)
	}
	for _, tc := range []struct {
		name string
		line string
	}{
		{"message-like note", `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef",` +
			`"note":"Katie: does a data upsert for Collab members match their email?"}`},
		{"unknown key", `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef","comment":"x"}`},
		// Codex round 9: every allowed field is constrained, not just the keys.
		{"free text in subject_sha256", `{"message_id":1,"label":"not",` +
			`"subject_sha256":"can you confirm the rotation date?"}`},
		{"free text in stratum", `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef",` +
			`"stratum":"asked about the invoice schedule"}`},
		// Codex round 10: json.Unmarshal keeps the LAST value of a repeated key,
		// so text in an earlier one would pass every check above.
		{"duplicate note hiding text", `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef",` +
			`"note":"Katie asked about the upsert","note":"` + classify.OwnerBlanketNote + `"}`},
		{"duplicate hash hiding text", `{"message_id":1,"label":"not",` +
			`"subject_sha256":"can you confirm the rotation date?","subject_sha256":"0123456789abcdef"}`},
		// The second key is spelled with a JSON unicode escape for its "t",
		// which decodes to "note": a byte-level key comparison would miss it.
		{"escaped duplicate key", `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef",` +
			`"note":"client text","` + escapedNoteKey + `":"` + classify.OwnerBlanketNote + `"}`},
	} {
		if _, err := loadLabels(writeLabels(t, tc.line), classify.LaneInquiry); err == nil {
			t.Errorf("loadLabels accepted a line with a %s; the labelled set is committed and its record is "+
				"closed", tc.name)
		}
	}
	// Control: the one allowed note, and a line with no note, both load.
	ok := `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef","stratum":"uniform","note":"` +
		classify.OwnerBlanketNote + `"}`
	if _, err := loadLabels(writeLabels(t, ok, lblNot), classify.LaneInquiry); err != nil {
		t.Fatalf("POSITIVE CONTROL FAILED: the allowed owner-blanket note was refused: %v", err)
	}
	// A valid stratum on the personal lane is refused: its set has none.
	strat := `{"message_id":1,"label":"not","subject_sha256":"0123456789abcdef","stratum":"uniform"}`
	if _, err := loadLabels(writeLabels(t, strat), classify.LanePersonal); err == nil {
		t.Errorf("loadLabels accepted a stratum on the personal lane, whose labelled set carries none")
	}
	if _, err := loadLabels(writeLabels(t, strat), classify.LaneResidue); err != nil {
		t.Errorf("CONTROL FAILED: a valid stratum on the residue lane was refused: %v", err)
	}
}
