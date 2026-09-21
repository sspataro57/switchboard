package tools

// gmail-delivery-cc (SWT-69): the PURE half of the Cc contract — the D5
// normalizer, the D6 absent-vs-empty shapes, the validator's gmail-only rule
// (D3) and the D8 content hash. ZERO network, ZERO Postgres: NormalizeCc is
// pure and the validators are called directly (package tools), so an accepted
// case never reaches a handler that would dereference a nil pool.
//
// GREENFIELD — EXPECTED RED: internal/tools/cc.go does not exist, so
// NormalizeCc and MaxCcAddresses are undefined; draftDeliveryArgs has no Cc,
// updateDeliveryArgs has no Cc, and DeliveryContentHash still takes two
// arguments. Package tools' tests compile-FAIL until the SPEC's named surface
// exists:
//
//	const MaxCcAddresses = 10
//	func NormalizeCc(in []string) ([]string, error)
//	func DeliveryContentHash(subject, body string, cc []string) string
//	draftDeliveryArgs.Cc  []string  `json:"cc,omitempty"`
//	updateDeliveryArgs.Cc *[]string `json:"cc,omitempty"`
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations", 2/3/4/6/8):
//   - store the caller's raw string instead of addr.Address → the display-name
//     rows.
//   - drop the dedupe, or make it case-sensitive → the dedupe rows.
//   - lower-case the local part → "local part is preserved".
//   - drop the printable-ASCII floor → the non-ASCII, space and control rows.
//   - change MaxCcAddresses without the schema CHECK → TestMaxCcAddresses
//     (its other half is the migration guard, which reads the CHECK).
//   - drop the gmail-only refusal from validateDraftDelivery → every
//     non-gmail row.
//   - leave DeliveryContentHash on (subject, body) → the hash rows.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The count limit is a NAMED const (D5) and the schema CHECK is pinned equal to
// it by TestMigration0038_Integration_DeliveryCcShape. Ten is the number in the
// SPEC's data-model section; changing it here without changing the migration is
// mutation 4.
func TestMaxCcAddresses_IsTen(t *testing.T) {
	if MaxCcAddresses != 10 {
		t.Errorf("MaxCcAddresses = %d, want 10 (D5: more than ten carbon copies is a mailing list, a different "+
			"feature with different policy — and the schema CHECK is pinned equal to this const)", MaxCcAddresses)
	}
}

// D5, the accepting half: what LANDS is address-only, domain lower-cased, local
// part untouched, deduped case-insensitively with the first spelling kept.
func TestNormalizeCc_StoresTheAddressOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			// (a) A display name is DROPPED, not stored: it is words on a
			// client-visible surface and would be model-chosen (invariant 6).
			name: "display name dropped",
			in:   []string{"Katie <kevans@cecollaboratory.com>"},
			want: []string{"kevans@cecollaboratory.com"},
		},
		{
			name: "RFC 2047 encoded display name dropped too",
			in:   []string{"=?utf-8?q?Katie?= <kevans@cecollaboratory.com>"},
			want: []string{"kevans@cecollaboratory.com"},
		},
		{
			name: "angle-addr without a name",
			in:   []string{"<kevans@cecollaboratory.com>"},
			want: []string{"kevans@cecollaboratory.com"},
		},
		{
			// (b) The DOMAIN is lower-cased; the LOCAL PART is left exactly as
			// given (RFC 5321 makes it case-sensitive to the receiving host,
			// and this is someone else's mailbox, not one of ours).
			name: "domain lower-cased, local part preserved",
			in:   []string{"Katie <KEvans@Example.COM>"},
			want: []string{"KEvans@example.com"},
		},
		{
			name: "surrounding whitespace is not part of the address",
			in:   []string{"   kevans@example.com   "},
			want: []string{"kevans@example.com"},
		},
		{
			// (c) Dedupe is case-insensitive over the WHOLE normalized address,
			// first occurrence wins (it keeps the caller's local-part spelling).
			name: "case-insensitive dedupe, first wins",
			in:   []string{"KEvans@example.com", "kevans@EXAMPLE.com", "Katie <kevans@example.com>"},
			want: []string{"KEvans@example.com"},
		},
		{
			name: "order is the caller's, minus duplicates",
			in:   []string{"b@x.io", "a@x.io", "B@X.IO", "c@x.io"},
			want: []string{"b@x.io", "a@x.io", "c@x.io"},
		},
		{
			name: "exactly MaxCcAddresses is legal",
			in:   ccList(MaxCcAddresses),
			want: ccList(MaxCcAddresses),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCc(tc.in)
			if err != nil {
				t.Fatalf("NormalizeCc(%q) = error %v, want %q", tc.in, err, tc.want)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("NormalizeCc(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// D6, draft side: absent, null and [] all mean "no Cc" — ONE representation.
func TestNormalizeCc_EmptyInputIsNoCc(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got, err := NormalizeCc(in)
		if err != nil {
			t.Errorf("NormalizeCc(%v) = error %v, want no Cc and no error", in, err)
		}
		if len(got) != 0 {
			t.Errorf("NormalizeCc(%v) = %q, want an empty list (the column is NOT NULL DEFAULT '{}': there is "+
				"exactly ONE representation of \"no Cc\")", in, got)
		}
	}
}

// D5, the refusing half. Every one of these is a value that must never reach
// the column, the header or the envelope.
func TestNormalizeCc_Refuses(t *testing.T) {
	long := strings.Repeat("x", 250) + "@example.com" // 262 bytes
	for _, tc := range []struct {
		name string
		in   []string
		// A word the refusal must contain, so the caller is told WHICH rule
		// and WHICH address (the repo's "name the real reason" rule).
		wantIn string
	}{
		{"not an address at all", []string{"not an address"}, "not an address"},
		{"empty element", []string{""}, ""},
		{"whitespace-only element", []string{"   "}, ""},
		{"two addresses in one element", []string{"a@x.io, b@x.io"}, "a@x.io"},
		// The header-injection floor. ParseAddress refuses these today; the
		// printable-ASCII rule is the belt that keeps it true if the parse
		// ever changes.
		{"CR LF injection", []string{"a@x.io\r\nBcc: victim@x.io"}, "a@x.io"},
		{"bare LF injection", []string{"a@x.io\nBcc: victim@x.io"}, "a@x.io"},
		{"NUL", []string{"a@x.io\x00"}, "a@x.io"},
		// There is no SMTPUTF8 support in SubmitSMTP: a non-ASCII octet in a
		// header is a defect in both transports. ParseAddress ACCEPTS both of
		// these, so only the byte floor catches them.
		{"non-ASCII local part", []string{"josé@example.com"}, "é"},
		{"non-ASCII domain", []string{"a@exämple.com"}, "ä"},
		// A quoted local part unquotes to an address carrying a SPACE (0x20),
		// which is outside 0x21-0x7E.
		{"space inside a quoted local part", []string{`"john doe"@example.com`}, "john doe"},
		{"over 254 bytes", []string{long}, "254"},
		{"eleven addresses", ccList(MaxCcAddresses + 1), "10"},
		// A refusal is about ONE bad entry; the good ones around it must not
		// make it pass.
		{"one bad entry among good ones", []string{"a@x.io", "not an address", "b@x.io"}, "not an address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCc(tc.in)
			if err == nil {
				t.Fatalf("NormalizeCc(%q) = %q, want a refusal", tc.in, got)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("NormalizeCc(%q) refused with %q; it must name %q so the caller can fix it",
					tc.in, err, tc.wantIn)
			}
		})
	}
}

// D8 depends on a property D5's byte floor does NOT give on its own: a comma is
// an unambiguous separator in the content hash ONLY if no stored address can
// contain one. `"a,b"@example.com` parses, and net/mail hands back the UNQUOTED
// local part `a,b@example.com` — every byte of which is inside 0x21-0x7E. Such
// a value would also be an invalid unquoted address in the Cc header and would
// split into two envelope recipients in recipientsFromMIME.
//
// Refusing it or re-quoting it are both acceptable; storing it raw is not.
func TestNormalizeCc_StoredAddressCannotCarryAComma(t *testing.T) {
	got, err := NormalizeCc([]string{`"a,b"@example.com`})
	if err != nil {
		return // refused: fine, that is the simplest spelling
	}
	for _, a := range got {
		if strings.Contains(a, ",") {
			t.Errorf("NormalizeCc stored %q: a stored address containing a comma breaks D8 (the hash joins the "+
				"cc with commas), breaks the Cc header's own separator, and splits into two envelope "+
				"recipients in recipientsFromMIME", a)
		}
	}
}

// D3 + criterion 4: a non-empty cc on any channel but gmail is refused BY NAME,
// before any other channel rule — the RequireChannel placement
// (delivery.go:132-136). The calendar case is the proof of "before": that draft
// is also missing target_ref/subject/start/end, and the refusal must still be
// about the cc.
func TestValidateDraftDelivery_CcIsGmailOnly(t *testing.T) {
	const slackTarget = "https://app.slack.com/client/TITEST/CITEST/p1750000000000000"
	for _, tc := range []struct {
		name string
		args string
	}{
		{"slack_reply", `{"task_id":4,"channel":"slack_reply","body":"b","target_ref":"` + slackTarget + `","cc":["k@example.com"]}`},
		{"upwork_chat", `{"task_id":4,"channel":"upwork_chat","body":"b","target_ref":"upwork_crm:itest-cc-client:upwork","cc":["k@example.com"]}`},
		{"jira_comment", `{"task_id":4,"channel":"jira_comment","body":"b","target_ref":"site.atlassian.net/ABC-1","cc":["k@example.com"]}`},
		{"calendar, refused for the cc and not for its missing fields", `{"task_id":4,"channel":"calendar","body":"b","cc":["k@example.com"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDraftDelivery([]byte(tc.args))
			if err == nil {
				t.Fatalf("validateDraftDelivery(%s) accepted a cc on a %s draft; that channel's send path has no "+
					"concept of a carbon copy and would silently drop a recipient the caller asked for", tc.args, tc.name)
			}
			if !strings.Contains(err.Error(), "cc") || !strings.Contains(err.Error(), "gmail") {
				t.Errorf("refusal = %q, want it to name cc and gmail", err)
			}
		})
	}

	// An ABSENT, null or empty cc changes nothing for those channels.
	for _, args := range []string{
		`{"task_id":4,"channel":"slack_reply","body":"b","target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000"}`,
		`{"task_id":4,"channel":"slack_reply","body":"b","target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000","cc":null}`,
		`{"task_id":4,"channel":"slack_reply","body":"b","target_ref":"https://app.slack.com/client/TITEST/CITEST/p1750000000000000","cc":[]}`,
	} {
		if err := validateDraftDelivery([]byte(args)); err != nil {
			t.Errorf("validateDraftDelivery(%s) = %v; an absent/null/empty cc is not a cc", args, err)
		}
	}
}

// Criterion 3 + criterion 4: syntax and count are checked in the PURE
// validator (args only), so a malformed cc never reaches the database.
func TestValidateDraftDelivery_CcSyntaxAndCount(t *testing.T) {
	gmail := func(cc string) string {
		return `{"task_id":4,"channel":"gmail","body":"b","thread_id":5,"cc":` + cc + `}`
	}
	list, err := json.Marshal(ccList(MaxCcAddresses))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tooMany, err := json.Marshal(ccList(MaxCcAddresses + 1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, tc := range []struct {
		args string
		ok   bool
	}{
		{gmail(`["kevans@cecollaboratory.com"]`), true},
		{gmail(`["Katie <kevans@cecollaboratory.com>"]`), true},
		{gmail(`[]`), true},
		{gmail(`null`), true},
		{gmail(string(list)), true},
		{gmail(`["not an address"]`), false},
		{gmail(`[""]`), false},
		{gmail(`["a@x.io\r\nBcc: victim@x.io"]`), false},
		{gmail(`["josé@example.com"]`), false},
		{gmail(string(tooMany)), false},
	} {
		err := validateDraftDelivery([]byte(tc.args))
		if tc.ok && err != nil {
			t.Errorf("validateDraftDelivery(%s) = %v, want accepted", tc.args, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateDraftDelivery(%s) accepted it; the syntax and count rules are the validator's "+
				"(pure, args only) so a malformed cc never reaches the INSERT", tc.args)
		}
	}
}

// D6 + criterion 7: `cc` ALONE is now a valid update — the "nothing to update"
// refusal accepts subject OR body OR cc.
func TestValidateUpdateDelivery_CcAloneIsEnough(t *testing.T) {
	for _, tc := range []struct {
		args string
		ok   bool
	}{
		{`{"delivery_id":51,"cc":["kevans@cecollaboratory.com"]}`, true},
		{`{"delivery_id":51,"cc":[]}`, true}, // clear
		{`{"delivery_id":51,"body":"words"}`, true},
		{`{"delivery_id":51}`, false},                    // still nothing to update
		{`{"delivery_id":51,"cc":null}`, false},          // null reads as ABSENT (D6), so still nothing
		{`{"delivery_id":51,"cc":["nope"]}`, false},      // same syntax rule as draft
		{`{"delivery_id":51,"cc":["josé@x.io"]}`, false}, // same byte floor
		{`{"cc":["kevans@cecollaboratory.com"]}`, false}, // missing delivery_id
	} {
		err := validateUpdateDelivery([]byte(tc.args))
		if tc.ok && err != nil {
			t.Errorf("validateUpdateDelivery(%s) = %v, want accepted", tc.args, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateUpdateDelivery(%s) was accepted, want a refusal", tc.args)
		}
	}
}

// D6's three cases must be DISTINGUISHABLE, which is why the field is
// *[]string: absent and null are "leave it alone", [] is "clear it". A plain
// []string would collapse absent and [] into nil and the clear verb would be
// unreachable (mutation 8).
func TestUpdateDeliveryArgs_CcDistinguishesAbsentNullAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    string
		present bool
		want    []string
	}{
		{"absent leaves it unchanged", `{"delivery_id":51,"body":"x"}`, false, nil},
		{"null reads as absent", `{"delivery_id":51,"cc":null}`, false, nil},
		{"empty list clears", `{"delivery_id":51,"cc":[]}`, true, []string{}},
		{"a list replaces", `{"delivery_id":51,"cc":["a@x.io","b@x.io"]}`, true, []string{"a@x.io", "b@x.io"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a updateDeliveryArgs
			if err := json.Unmarshal([]byte(tc.args), &a); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.args, err)
			}
			if (a.Cc != nil) != tc.present {
				t.Fatalf("%s: Cc pointer present = %v, want %v (absent and null must both read as \"unchanged\"; "+
					"[] must read as \"clear\")", tc.args, a.Cc != nil, tc.present)
			}
			if tc.present && !equalStrings(*a.Cc, tc.want) {
				t.Errorf("%s: *Cc = %q, want %q", tc.args, *a.Cc, tc.want)
			}
		})
	}
}

// D8 + criterion 10: the content-bound approval covers the Cc. Without this, a
// Cc added by update_delivery between the page render and the Approve click
// would pass the hash check designed to catch exactly that, and "Salvador saw
// every Cc" would be false in precisely that window.
func TestDeliveryContentHash_CoversTheCc(t *testing.T) {
	const subject, body = "Re: Rochester schedule", "Thursday works."
	cc := []string{"kevans@cecollaboratory.com", "b@x.io"}

	sum := sha256.Sum256([]byte(subject + "\x00" + body + "\x00" + strings.Join(cc, ",")))
	if got, want := DeliveryContentHash(subject, body, cc), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("DeliveryContentHash = %s, want sha256(subject + NUL + body + NUL + join(cc, \",\")) = %s", got, want)
	}
	// Adding, removing or reordering a Cc changes the hash.
	base := DeliveryContentHash(subject, body, nil)
	for _, other := range [][]string{
		{"kevans@cecollaboratory.com"},
		cc,
		{"b@x.io", "kevans@cecollaboratory.com"},
	} {
		if DeliveryContentHash(subject, body, other) == base {
			t.Errorf("cc %q hashes the same as no cc: a Cc added after the page render would pass the approve "+
				"check that exists to catch it", other)
		}
	}
	if DeliveryContentHash(subject, body, cc) == DeliveryContentHash(subject, body, []string{"b@x.io", "kevans@cecollaboratory.com"}) {
		t.Error("reordering the cc keeps the hash; the hash is over the stored list, in order")
	}
	// nil and an empty list are the same "no Cc" (D6: ONE representation).
	if DeliveryContentHash(subject, body, nil) != DeliveryContentHash(subject, body, []string{}) {
		t.Error("nil cc and empty cc hash differently; the column has exactly one representation of \"no Cc\"")
	}
	// The NUL boundary still holds with the third field in play.
	if DeliveryContentHash("ab", "c", nil) == DeliveryContentHash("a", "bc", nil) {
		t.Error("subject and body are not separated")
	}
	if DeliveryContentHash(subject, "a", []string{"b@x.io"}) == DeliveryContentHash(subject, "a\x00b@x.io", nil) {
		t.Error("the body and the cc are not separated: words can slide across the boundary under one hash")
	}
}

// ---- helpers -----------------------------------------------------------------

// ccList returns n distinct, valid, already-normalized addresses.
func ccList(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("cc%d@example.com", i))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// D14 (amended in review): the sent record carries cc whenever one was
// APPROVED — as [] when the send-time drop emptied it — and no key otherwise.
func TestRecordSentCc(t *testing.T) {
	for _, tc := range []struct {
		name           string
		approved, sent []string
		want           string
	}{
		{"no Cc approved", nil, nil, `{}`},
		{"approved and sent", []string{"a@x.io"}, []string{"a@x.io"}, `{"cc":["a@x.io"]}`},
		{"approved, the drop emptied it", []string{"a@x.io"}, []string{}, `{"cc":[]}`},
		{"approved, nil sent set", []string{"a@x.io"}, nil, `{"cc":[]}`},
	} {
		p := map[string]any{}
		recordSentCc(p, tc.approved, tc.sent)
		got, _ := json.Marshal(p)
		if string(got) != tc.want {
			t.Errorf("%s: payload = %s, want %s", tc.name, got, tc.want)
		}
	}
}
