package classify_test

// SWT-33 (docs/tickets/inquiry-classify_SPEC.md) — the UNIT half of the inquiry
// lane: criteria 1, 2, 4, 5, 6, 7, 8, 11, 13, 16 and 19, plus D6's --since
// refusal. Fake Store + fake provider.Client — ZERO network, ZERO Postgres,
// ZERO live model, the seam worker_test.go and lane_test.go already use.
//
// The SQL halves live in inquiry_integration_test.go, deliberately: criteria 9,
// 12, 15, 17, 18, 26 and 30 all turn on values Postgres produces (the inbox
// filter's `p.ai_inquiry`, `direction='outbound'`, the stored `channel`), and a
// fake supplying them would be supplying the very values the query is meant to
// compute — SWT-21's sixth landmine instance, whose standing rule is that a
// predicate fed by a COLUMN gets its regression test against a real database.
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
// The SPEC fixes the BEHAVIOUR; the Go spellings below are this file's, chosen
// as the smallest growth of the existing types that carries every input the
// criteria name. Where the SPEC named a spelling (LaneInquiry, ai_inquiry,
// thread_scope, the field names of criterion 13) it is used verbatim.
//
//	// criterion 2: the Lane gains a Contract, and there are TWO contracts for
//	// THREE lanes. Schema is compared as TEXT below (fmt %s), so an
//	// implementation may keep it as json.RawMessage or narrow it to string —
//	// the assertion criterion 2 asks for ("LanePersonal.Contract ==
//	// LaneResidue.Contract") is spelled as reflect.DeepEqual so the choice
//	// stays open.
//	type Contract struct {
//	    SchemaName    string          // provider.Request.SchemaName
//	    Schema        json.RawMessage // provider.Request.Schema
//	    DecisionKey   string          // "actionable" | "needs_reply"
//	    CategoryKey   string          // "kind"       | "ask_kind"
//	    PositiveLabel string          // the eval fixture's positive token
//	}
//	type Lane struct { Name, WorkerType, System, PromptVersion, LabelsPath string; Contract Contract }
//
//	var  LaneInquiry Lane
//	const InquiryPromptVersion = "inquiry-v1"
//	const InquirySystemPrompt  = `…`
//	var   InquiryVerdictSchema json.RawMessage
//
//	// criterion 19: Summarize resolves the lane from the worker_type it is
//	// already given, so ReportForWorker's pinned signature does not move.
//	func LaneByWorkerType(workerType string) (Lane, bool)
//
//	// criteria 13 + 16: what a verdict must be able to record, and the PRIOR
//	// context the model is shown. ThreadContext is the SELECTED window, not the
//	// whole thread — InquiryContext is the pure selection rule the PGStore
//	// loader applies, which is what lets criterion 16's "a later message never
//	// appears in the request" be a unit test at all.
//	type ThreadMessage struct {
//	    MessageID int64
//	    SentAt    time.Time
//	    Direction string // 'inbound' | 'outbound'
//	    BodyText  string
//	}
//	func InquiryContext(target PendingMessage, thread []ThreadMessage) []ThreadMessage
//	type PendingMessage struct {
//	    …
//	    ThreadKey         string          // normalized_threads.thread_key, VERBATIM
//	    ExternalMessageID string          // normalized_messages.external_message_id
//	    ThreadContext     []ThreadMessage // PRIOR only, oldest -> newest
//	}
//
// GREENFIELD NOTE — EXPECTED RED. classify.LaneInquiry, classify.Contract,
// classify.InquiryVerdictSchema, classify.InquirySystemPrompt,
// classify.InquiryContext, classify.LaneByWorkerType and PendingMessage's three
// new fields do not exist, so this file compile-FAILS `go test ./internal/classify/`
// with "undefined: classify.LaneInquiry" and friends. That IS the red state for
// a spec-first test. Verified in the authoring session against a throwaway stub
// declaring the surface above and returning zero values: every assertion here
// then fires on its own merits (hosted calls = 3 want 0, thread_scope = "" want
// "thread", and so on) rather than on the missing package.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- fixtures ----------------------------------------------------------------

// The inquiry contract's two canned answers. Schema-valid for
// InquiryVerdictSchema (criterion 5) and NOT for VerdictSchema — a fake that
// returned the actionability shape here would decode to a zero verdict and make
// every needs_reply assertion below pass for the wrong reason.
const (
	iqNeedsReply = `{"needs_reply":true,"ask_kind":"question","asker":"Dana Ruiz",` +
		`"ask":"Can you confirm the rotation date for the staging key?",` +
		`"reason":"a direct question addressed to the recipient, unanswered in the context shown"}`
	iqNoReply = `{"needs_reply":false,"ask_kind":"fyi","asker":"Dana Ruiz","ask":"",` +
		`"reason":"a deploy notification; nothing is asked of the recipient"}`
)

func iqLocal() *cfClient {
	return &cfClient{
		desc:    provider.Descriptor{Name: "ollama", Endpoint: "http://127.0.0.1:11434"},
		verdict: iqNeedsReply,
	}
}

const (
	// A ROOTED slack key: slack:{ws}:{conv}:{thread_root}. slackweb appends
	// ThreadRootID to the key (normalize.go:71-76), so a threaded message's
	// normalized_threads.thread_key is thread-exact.
	iqRootedKey = "slack:T0HPR78RX:C07QINQUIRY:p1757000000000100"
	// The SAME conversation with no thread root — the whole channel. This is
	// the shape one measured production key holds 9,704 messages under, and the
	// reason "answered" and "spoke since" are two counters (criterion 17).
	iqConversationKey = "slack:T0HPR78RX:C07QINQUIRY"
	iqExternalMsgID   = "slack:T0HPR78RX:C07QINQUIRY:p1757000000000900"
)

// iqMessages builds messages in the shape the INQUIRY inbox actually yields,
// and it differs from cfMessages in the one field the whole ticket turns on:
// ProjectLocalOnly is FALSE. `collaboratory` is ai_locality='any' (criterion
// 11), so ClassOf(AttrProject, false) returns ClassGeneral for these rows — and
// a fixture that quietly set LocalOnly=true would make criterion 11's pin pass
// while the lane it describes was a no-op in production. That is this repo's
// "a predicate whose discriminating column is a constant in production"
// landmine, pre-empted in the fixture.
func iqMessages(n int) []classify.PendingMessage {
	out := make([]classify.PendingMessage, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, classify.PendingMessage{
			MessageID:         int64(i),
			RawSourceItemID:   int64(3000 + i),
			ThreadID:          int64(500 + i),
			ThreadKey:         iqRootedKey,
			ExternalMessageID: iqExternalMsgID,
			SentAt:            time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
			Sender:            "Dana Ruiz",
			Subject:           "llamasite-eng",
			Channel:           "slack",
			BodyText:          "Can you confirm the rotation date for the staging key?",
			Direction:         "inbound",

			ProjectID:        4242,
			ProjectSlug:      "collaboratory",
			ProjectLocalOnly: false, // ai_locality='any' — see the note above
			Attribution:      provider.AttrProject,
		})
	}
	return out
}

// iqCfg is the inquiry lane's run config. Since is set because --since is
// REQUIRED on this lane (D6); a helper that left it zero would make every test
// below assert against the refusal instead of the lane.
func iqCfg() classify.Config {
	return classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LaneInquiry, Since: 168 * time.Hour}
}

func iqFields(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("ai_extractions.fields is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

// ---- criterion 1: the lane exists and `--lane inquiry` resolves it ------------

func TestLaneInquiry_ValuesAndTheThirdSpelling(t *testing.T) {
	if classify.LaneInquiry.Name != "inquiry" {
		t.Errorf("LaneInquiry.Name = %q, want \"inquiry\" — the name is what `--lane` matches and what the "+
			"runbook quotes", classify.LaneInquiry.Name)
	}
	if classify.LaneInquiry.WorkerType != "classify_inquiry" {
		t.Errorf("LaneInquiry.WorkerType = %q, want %q. Every lane's inbox keys its NOT EXISTS on this "+
			"value, so a shared one makes a message classified by one lane permanently invisible to the "+
			"others (SWT-23 criterion 11's defect, third instance)",
			classify.LaneInquiry.WorkerType, "classify_inquiry")
	}
	if classify.InquiryPromptVersion != "inquiry-v1" {
		t.Errorf("classify.InquiryPromptVersion = %q, want \"inquiry-v1\"", classify.InquiryPromptVersion)
	}
	if classify.LaneInquiry.PromptVersion != classify.InquiryPromptVersion {
		t.Errorf("LaneInquiry.PromptVersion = %q, want %q — one spelling, or the stamp in ai_runs.input "+
			"stops matching the constant a session greps for",
			classify.LaneInquiry.PromptVersion, classify.InquiryPromptVersion)
	}
	if classify.LaneInquiry.LabelsPath != "docs/evals/inquiry-needs-reply.jsonl" {
		t.Errorf("LaneInquiry.LabelsPath = %q, want docs/evals/inquiry-needs-reply.jsonl. `eval` defaults "+
			"--labels to the lane's own fixture precisely so an inquiry lane is never scored against the "+
			"personal labels by omission", classify.LaneInquiry.LabelsPath)
	}
	if classify.LaneInquiry.System != classify.InquirySystemPrompt {
		t.Errorf("LaneInquiry.System is not classify.InquirySystemPrompt")
	}

	// All three worker_types distinct, asserted as a set rather than pairwise:
	// the pairwise spelling is where the third lane gets forgotten.
	seen := map[string][]string{}
	for _, l := range []classify.Lane{classify.LanePersonal, classify.LaneResidue, classify.LaneInquiry} {
		seen[l.WorkerType] = append(seen[l.WorkerType], l.Name)
	}
	if len(seen) != 3 {
		t.Errorf("the three lanes use %d distinct worker_type values: %v. Sharing one makes a message "+
			"classified by one lane invisible to the others, forever", len(seen), seen)
	}
}

func TestLaneByName_ResolvesInquiryAndNamesAllThreeValidSpellings(t *testing.T) {
	got, err := classify.LaneByName("inquiry")
	if err != nil {
		t.Fatalf("LaneByName(\"inquiry\") = %v; criterion 1 requires `classify run|report|eval --lane "+
			"inquiry` to resolve", err)
	}
	if got.WorkerType != classify.LaneInquiry.WorkerType {
		t.Errorf("LaneByName(\"inquiry\") resolved to worker_type %q, want %q", got.WorkerType,
			classify.LaneInquiry.WorkerType)
	}

	// Controls: the two shipped names still resolve. Without them "names all
	// three" is satisfied by a resolver that resolves nothing.
	for _, name := range []string{"personal", "residue"} {
		if _, err := classify.LaneByName(name); err != nil {
			t.Fatalf("POSITIVE CONTROL FAILED: LaneByName(%q) = %v; the two shipped lanes must still "+
				"resolve", name, err)
		}
	}

	_, err = classify.LaneByName("inquiries")
	if err == nil {
		t.Fatalf("LaneByName(\"inquiries\") returned no error; a typo must never silently classify one " +
			"population with another's prompt")
	}
	for _, want := range []string{"personal", "residue", "inquiry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unknown-lane refusal does not name %q: %q. Criterion 1: it names all THREE valid "+
				"spellings — an operator who mistyped one needs the list, not the reproach", want, err.Error())
		}
	}
}

// ---- criterion 4: the zero lane is still refused, naming three ----------------

func TestRun_ZeroLaneRefusal_NamesAllThreeLanes(t *testing.T) {
	local := iqLocal()
	store := &cfStore{pending: iqMessages(3)}

	_, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), local, time.Minute),
		classify.Config{Model: "qwen3:8b", MaxTokens: 512}) // no Lane
	if err == nil {
		t.Fatalf("Run accepted a Config with no Lane. Criterion 4: the zero value stays REFUSED, before " +
			"any I/O")
	}
	for _, want := range []string{"personal", "residue", "inquiry"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("the zero-lane refusal does not name %q: %q. Criterion 4: the refusal names all three "+
				"lanes, because the message is the only place a caller learns what the choices are",
				want, err.Error())
		}
	}
	if len(store.runs) != 0 || local.calls != 0 {
		t.Errorf("the refused run recorded %d ai_runs row(s) and made %d model call(s); the refusal is a "+
			"configuration error and belongs before any I/O", len(store.runs), local.calls)
	}
}

// ---- criterion 2: THREE lanes, TWO contracts ----------------------------------

// D1's argument, restated because it is what this test defends: the
// actionability contract's `kind` enum is the vocabulary of a bill
// (payment_due | deadline | appointment | action_required | informational), not
// of a conversation, and `actionable` scored against inquiry labels would make
// two different questions' recall/precision falsely comparable. So the inquiry
// lane carries its OWN contract — and the guard that preserves what "one
// contract, both lanes" was protecting (SWT-23's 0.94 / 0.50 comparability) is
// the direct equality of the OTHER two.
func TestLaneContracts_PersonalAndResidueShareOne_InquiryHasItsOwn(t *testing.T) {
	personal := classify.LanePersonal.Contract
	residue := classify.LaneResidue.Contract
	inquiry := classify.LaneInquiry.Contract

	// Criterion 2's headline. Spelled as DeepEqual rather than `==` only so the
	// implementation may keep Schema as json.RawMessage; it is the same claim.
	if !reflect.DeepEqual(personal, residue) {
		t.Errorf("LanePersonal.Contract != LaneResidue.Contract:\n personal: %+v\n residue:  %+v\n"+
			"Criterion 2: this equality is what replaces the old 'one contract, both lanes' field scan, "+
			"and it is what keeps the residue's recall/precision readable against the personal lane's "+
			"0.94 / 0.50. Two lanes that answer different questions are two experiments", personal, residue)
	}
	if reflect.DeepEqual(inquiry, personal) {
		t.Errorf("LaneInquiry.Contract equals the actionability contract. D1: `kind` is "+
			"payment_due|deadline|appointment|action_required|informational — the vocabulary of a bill, "+
			"not of a conversation — and scoring `actionable` against inquiry labels makes two different "+
			"questions' numbers look comparable when they are not: %+v", inquiry)
	}

	// The shared contract IS the actionability contract, byte for byte
	// (criterion 2's second sentence). Compared as TEXT so the field may be
	// json.RawMessage or string.
	asText := func(v any) string { return fmt.Sprintf("%s", v) }
	if personal.SchemaName != classify.SchemaName {
		t.Errorf("LanePersonal.Contract.SchemaName = %q, want classify.SchemaName (%q) — the constant is "+
			"unchanged and the contract must point AT it, not restate it", personal.SchemaName, classify.SchemaName)
	}
	if asText(personal.Schema) != asText(classify.VerdictSchema) {
		t.Errorf("LanePersonal.Contract.Schema is not classify.VerdictSchema verbatim.\n got: %s\nwant: %s\n"+
			"Criterion 2: VerdictSchema and SchemaName are unchanged BYTE FOR BYTE, and "+
			"TestSchema_MatchesTheOutputContract still passes untouched",
			asText(personal.Schema), asText(classify.VerdictSchema))
	}
	if personal.DecisionKey != "actionable" || personal.CategoryKey != "kind" || personal.PositiveLabel != "actionable" {
		t.Errorf("the actionability contract's keys are {decision:%q category:%q positive:%q}, want "+
			"{actionable kind actionable}", personal.DecisionKey, personal.CategoryKey, personal.PositiveLabel)
	}

	// The inquiry contract's own keys. These are read by three separate pieces
	// of code — the fields assembly (criterion 13), the report/Summarize fold
	// (criteria 17, 30) and the eval scorer (criterion 23) — so a wrong value
	// here is three wrong answers with no error anywhere.
	if asText(inquiry.Schema) != asText(classify.InquiryVerdictSchema) {
		t.Errorf("LaneInquiry.Contract.Schema is not classify.InquiryVerdictSchema:\n%s", asText(inquiry.Schema))
	}
	if inquiry.SchemaName == classify.SchemaName {
		t.Errorf("LaneInquiry.Contract.SchemaName is %q — the same structured-output name as the "+
			"actionability contract. Two contracts, two names, or a response logged under one name cannot "+
			"be told from the other", inquiry.SchemaName)
	}
	if inquiry.DecisionKey != "needs_reply" {
		t.Errorf("LaneInquiry.Contract.DecisionKey = %q, want \"needs_reply\" (criterion 23: the eval "+
			"scores the lane's decision field, not `actionable`)", inquiry.DecisionKey)
	}
	if inquiry.CategoryKey != "ask_kind" {
		t.Errorf("LaneInquiry.Contract.CategoryKey = %q, want \"ask_kind\" — the key Summarize groups by",
			inquiry.CategoryKey)
	}
	if inquiry.PositiveLabel != "needs_reply" {
		t.Errorf("LaneInquiry.Contract.PositiveLabel = %q, want \"needs_reply\" (criterion 22's label "+
			"vocabulary is needs_reply | not, and criterion 23 validates the file against THIS token so a "+
			"personal file cannot be scored as an inquiry file)", inquiry.PositiveLabel)
	}
}

// ---- criterion 5: the inquiry output contract ---------------------------------

func TestInquiryVerdictSchema_FiveRequiredFieldsAndNothingElse(t *testing.T) {
	var schema struct {
		Type                 string                     `json:"type"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(classify.InquiryVerdictSchema, &schema); err != nil {
		t.Fatalf("classify.InquiryVerdictSchema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("InquiryVerdictSchema type = %q, want \"object\"", schema.Type)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Errorf("InquiryVerdictSchema does not set additionalProperties:false. A model that invents a "+
			"field must have it REJECTED rather than stored unchallenged — prompt.go's recorded reason, "+
			"unchanged: %v", schema.AdditionalProperties)
	}

	want := map[string]string{
		"needs_reply": "boolean",
		"ask_kind":    "string",
		"asker":       "string",
		"ask":         "string",
		"reason":      "string",
	}
	for name, typ := range want {
		raw, ok := schema.Properties[name]
		if !ok {
			t.Errorf("InquiryVerdictSchema has no %q property; criterion 5 names exactly five", name)
			continue
		}
		var prop struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(raw, &prop); err != nil {
			t.Errorf("property %q does not parse: %v", name, err)
			continue
		}
		if prop.Type != typ {
			t.Errorf("property %q type = %q, want %q", name, prop.Type, typ)
		}
		if name == "ask_kind" {
			gotEnum := append([]string(nil), prop.Enum...)
			wantEnum := []string{"question", "request", "decision", "scheduling", "fyi"}
			if !reflect.DeepEqual(gotEnum, wantEnum) {
				t.Errorf("ask_kind enum = %v, want %v. It is an ENUM for the measured reason `kind` is one: "+
					"left unconstrained the model returns the same concept in three casings in a single run, "+
					"producing a report column nothing can GROUP BY", gotEnum, wantEnum)
			}
		}
	}
	for name := range schema.Properties {
		if _, ok := want[name]; !ok {
			t.Errorf("InquiryVerdictSchema declares an extra property %q. Criterion 5: five fields, no "+
				"sixth. In particular there is NO link_index on this contract (criterion 6): "+
				"normalized_messages.links is written by the google normalizer only, so on a "+
				"slack/jira-dominated project the field would be null on every row — a stored constant, "+
				"which is the landmine this repo keeps paying for", name)
		}
	}

	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	for name := range want {
		if !required[name] {
			t.Errorf("InquiryVerdictSchema does not require %q. All five are required: an omitted field "+
				"decodes to a zero value, and 'the model said false' and 'the model said nothing' must not "+
				"be the same row", name)
		}
	}
	if len(schema.Required) != len(want) {
		t.Errorf("InquiryVerdictSchema requires %v (%d entries), want exactly the five properties",
			schema.Required, len(schema.Required))
	}
}

// The SWT-25 URL scan, GENERALISED rather than copied (criterion 5: "the URL
// scan is extended to the new schema; today it walks classify.VerdictSchema
// only"). Walking every package-level *Schema var found by the parser means a
// FOURTH contract inherits the guard without anyone remembering to add it — the
// failure mode a hand-maintained list has.
//
// It also re-asserts criterion 5's no-confidence rule at the schema level. The
// package-wide `confidence` scan in structure_test.go covers the new file
// automatically and gets NO exemption; this is the same claim said where the
// schema is.
func TestSchemas_NoneDeclaresAURLOrAConfidenceField(t *testing.T) {
	// TWO patterns, deliberately different. `urlValue` is what may never appear
	// as a value — a URL the model authored. `inquiryProp` is wider by one token
	// and is applied ONLY to the inquiry schema's property names, because
	// `link_index` is a LEGITIMATE property of the actionability contract (it is
	// an INDEX, which is the whole SWT-25 design) and a shared pattern would
	// flag it. Getting that distinction wrong is how a scan starts reporting the
	// thing it was written to permit.
	urlValue := regexp.MustCompile(`(?i)(url|href|uri)`)
	inquiryProp := regexp.MustCompile(`(?i)(url|href|uri|link)`)
	confidence := regexp.MustCompile(`(?i)confiden`)

	found := map[string]string{} // var name -> schema JSON
	for _, rel := range csSources(t, "internal/classify") {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !strings.HasSuffix(name.Name, "Schema") || i >= len(vs.Values) {
						continue
					}
					var lit *ast.BasicLit
					ast.Inspect(vs.Values[i], func(n ast.Node) bool {
						if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && lit == nil {
							lit = bl
						}
						return true
					})
					if lit == nil {
						continue
					}
					if s, err := strconv.Unquote(lit.Value); err == nil {
						found[name.Name] = s
					}
				}
			}
		}
	}

	// Vacuity floor, and the specific pin: BOTH contracts are walked. A scan
	// that found one schema would pass today and miss the one this ticket adds.
	if len(found) < 2 {
		t.Fatalf("found %d package-level *Schema var(s) in internal/classify (%v); criterion 5 extends "+
			"this scan to the inquiry schema, so there are TWO — VerdictSchema and InquiryVerdictSchema. "+
			"A scan with nothing to scan proves nothing", len(found), found)
	}
	for _, want := range []string{"VerdictSchema", "InquiryVerdictSchema"} {
		if _, ok := found[want]; !ok {
			t.Errorf("the schema scan never saw %s; it is declared elsewhere, or not at all", want)
		}
	}

	for name, raw := range found {
		var whole any
		if err := json.Unmarshal([]byte(raw), &whole); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
			continue
		}
		var walk func(node any, path string)
		walk = func(node any, path string) {
			switch v := node.(type) {
			case map[string]any:
				for k, child := range v {
					if path == "$.properties" && confidence.MatchString(k) {
						t.Errorf("%s declares a property %q. qwen3:8b returns exactly 0.95 on everything it "+
							"flags — 27 true positives and 17 false positives, IDENTICAL — so a confidence "+
							"field looks like a dial and is a constant", name, k)
					}
					if path == "$.properties" && name == "InquiryVerdictSchema" && inquiryProp.MatchString(k) {
						t.Errorf("%s declares a property %q. Criterion 6: NO link field on this contract, "+
							"and criterion 5's URL scan: the model must never author a URL", name, k)
					}
					if strings.EqualFold(k, "format") {
						if s, ok := child.(string); ok && urlValue.MatchString(s) {
							t.Errorf("%s declares %s: %q — a URI-typed value is still a URL the model authored",
								name, path, s)
						}
					}
					walk(child, path+"."+k)
				}
			case []any:
				for i, child := range v {
					// ENUM values only. `required` is a list of PROPERTY NAMES,
					// and scanning it would re-flag link_index — a legitimate
					// field of the actionability contract.
					if strings.HasSuffix(path, ".enum") {
						if s, ok := child.(string); ok && urlValue.MatchString(s) {
							t.Errorf("%s carries the enum value %q at %s[%d] — a url-shaped enum value is "+
								"still a URL the model can emit", name, s, path, i)
						}
					}
					walk(child, path+"["+strconv.Itoa(i)+"]")
				}
			}
		}
		walk(whole, "$")
	}
}

// ---- criteria 7 + 8: the prompt -----------------------------------------------

func TestInquiryPrompt_SaysWhatCountsWhatDoesNotAndTheObjective(t *testing.T) {
	prompt := classify.InquirySystemPrompt
	if len(prompt) < 600 {
		t.Fatalf("InquirySystemPrompt is %d characters. THE WIRING IS NOT THE WORK: the lane, the filter "+
			"and the config are a day; the prompt is the ticket, and this one has to teach a 8B model the "+
			"difference between a question and chatter in a client channel", len(prompt))
	}
	lower := strings.ToLower(prompt)

	// What COUNTS (criterion 7, first list).
	for _, want := range []struct{ re, why string }{
		{`question`, "a direct question is the base case"},
		{`decision`, "a request for a DECISION — the ask_kind that most often reads as chatter"},
		{`schedul`, "a scheduling ask"},
		{`@|mention`, "an explicit @-mention asking for something; on Slack that IS the ask"},
	} {
		if !regexp.MustCompile(want.re).MatchString(lower) {
			t.Errorf("the inquiry prompt never matches /%s/ — %s (criterion 7)", want.re, want.why)
		}
	}

	// What does NOT (criterion 7, second list). Each of these is a family the
	// lane's population is MOSTLY made of; leaving one out is a false-alarm
	// generator on a surface Salvador is expected to trust.
	for _, want := range []struct{ re, why string }{
		{`fyi`, "an FYI"},
		{`status update`, "a status update"},
		{`bot|ci\b|notification`, "a bot/CI notification — the bulk of an engineering channel"},
		{`already answered`, "a question ALREADY ANSWERED in the context shown; without this sentence the " +
			"thread context is decoration"},
		{`someone else|addressed to another|another person`, "a question addressed to someone else in the " +
			"conversation — the single most common false positive shape in a group channel"},
		{`thanks|acknowledg`, "thanks/acknowledgement"},
	} {
		if !regexp.MustCompile(want.re).MatchString(lower) {
			t.Errorf("the inquiry prompt never matches /%s/ — it must name %s as NOT needing a reply "+
				"(criterion 7)", want.re, want.why)
		}
	}

	// The transcript sentence: the model must be told what the lines above the
	// message ARE, or it will read them as more of the message.
	if !regexp.MustCompile(`prior|earlier|above|transcript|context`).MatchString(lower) {
		t.Errorf("the inquiry prompt never explains that the transcript above the message is PRIOR " +
			"CONTEXT. That sentence is what turns 'Salvador already answered this two messages up' into " +
			"needs_reply:false, which is the whole reason context is loaded (criterion 7)")
	}

	// Bilingual, on the same terms as the other two prompts.
	if !regexp.MustCompile(`spanish`).MatchString(lower) {
		t.Errorf("the inquiry prompt is not bilingual on the same terms as the other two (criterion 7). " +
			"51 of the personal corpus's messages are Spanish and the originals are kept rather than " +
			"translated; a prompt that does not say so invites a translation pass nobody can compare against")
	}

	// CRITERION 8 — the objective, and it is the OPPOSITE of the other two lanes.
	if !regexp.MustCompile(`precision`).MatchString(lower) {
		t.Errorf("the inquiry prompt does not state a PRECISION-leaning objective (criterion 8). The other " +
			"two lanes say RECALL IS THE OBJECTIVE, and copying that sentence here is the likeliest way to " +
			"get this prompt wrong")
	}
	if regexp.MustCompile(`recall is the objective`).MatchString(lower) {
		t.Errorf("the inquiry prompt carries the other lanes' \"RECALL IS THE OBJECTIVE\" sentence. " +
			"Criterion 8: a missed bill is a late fee (recall), but a false 'someone is waiting on you' in " +
			"a client channel is a false alarm on a surface Salvador is expected to TRUST, and this lane's " +
			"population is mostly chatter")
	}
	// And the one line saying WHY, so the next reader does not "fix" it back.
	if !regexp.MustCompile(`false alarm|trust|waiting on you`).MatchString(lower) {
		t.Errorf("the inquiry prompt states the precision objective without the one line of reasoning " +
			"criterion 8 asks for. A stated objective with no argument is an objective the next session " +
			"reverses to match the other two prompts")
	}

	// Criterion 6, prompt side: no link contract on this lane.
	if strings.Contains(lower, "link_index") {
		t.Errorf("the inquiry prompt carries the link_index paragraph. Criterion 6: there is no link field " +
			"on this contract, and asking for one would ask the model to answer with a number for a list " +
			"it is never shown")
	}
}

// ---- criterion 11: THE ROUTED CLASS IS PINNED TO RESTRICTED -------------------

// Without this pin the whole ticket is a NO-OP, and the no-op is INVISIBLE:
// cmd/classify's buildRouter passes general = nil, `collaboratory` is
// ai_locality='any', so ClassOf(AttrProject,false) returns ClassGeneral and
// Router.Route returns (nil, DecideSkip, no_general_provider) for EVERY message.
// The pass exits 0, the report is empty, and an empty inbox and a lane that
// refuses its entire population look identical.
//
// So the assertion that matters is not "zero hosted calls" — a lane that
// classifies NOTHING also makes zero hosted calls. It is that the messages ARE
// CLASSIFIED, by the local client, with an extraction each.
func TestRun_InquiryLane_PinsTheRoutedClassToRestricted(t *testing.T) {
	t.Run("with general=nil (cmd/classify's router) the lane CLASSIFIES", func(t *testing.T) {
		local := iqLocal()
		store := &cfStore{pending: iqMessages(3)}

		// general = nil is not a test convenience: it is exactly what
		// cmd/classify's buildRouter constructs (`provider.NewRouter(nil, local, 0)`).
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(nil, local, time.Minute), iqCfg())
		if err != nil {
			t.Fatalf("Run(inquiry): %v", err)
		}
		if local.calls != 3 || stats.Processed != 3 || len(store.extractions) != 3 {
			t.Fatalf("local calls = %d, stats = %+v, extractions = %d; want 3/3/3.\n"+
				"THIS IS CRITERION 11. The lane must route provider.ClassRestricted regardless of the "+
				"message's own class. `collaboratory` is ai_locality='any', so ClassOf returns "+
				"ClassGeneral, and with a nil general client every message comes back "+
				"(nil, DecideSkip, no_general_provider) — the pass exits 0 and the report looks like an "+
				"empty inbox. Zero hosted calls does NOT prove containment here, because a lane that "+
				"classifies nothing also makes zero hosted calls.",
				local.calls, stats, len(store.extractions))
		}
		if stats.Skipped != 0 {
			t.Errorf("stats.Skipped = %d, want 0. Any skip on this fixture is the no_general_provider "+
				"refusal criterion 11 exists to prevent", stats.Skipped)
		}
	})

	t.Run("control: the SAME fixture through the personal lane's class fold SKIPS", func(t *testing.T) {
		local := iqLocal()
		store := &cfStore{pending: iqMessages(3)}

		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(nil, local, time.Minute),
			classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LanePersonal})
		if err != nil {
			t.Fatalf("Run(personal over the inquiry fixture): %v", err)
		}
		if stats.Skipped != 3 || local.calls != 0 {
			t.Fatalf("personal lane over the same fixture: skipped = %d, local calls = %d; want 3/0.\n"+
				"This control is what makes the subtest above mean something: it proves the fixture really "+
				"IS ClassGeneral under the unpinned fold, so 'the inquiry lane classified it' is the PIN "+
				"working and not the fixture being restricted anyway.", stats.Skipped, local.calls)
		}
	})

	t.Run("the hosted client records ZERO Complete calls, and the control proves it could have", func(t *testing.T) {
		hosted, local := cfHosted(), iqLocal()
		store := &cfStore{pending: iqMessages(3)}

		if _, err := classify.Run(context.Background(), store,
			provider.NewRouter(hosted, local, time.Minute), iqCfg()); err != nil {
			t.Fatalf("Run(inquiry): %v", err)
		}
		if hosted.calls != 0 {
			t.Errorf("the hosted client recorded %d Complete call(s) for inquiry messages. The pin is not a "+
				"downgrade of anything — it is the REFUSAL TO WIDEN, and it is required twice over, "+
				"because the prompt carries thread-NEIGHBOUR bodies as well as the target's", hosted.calls)
		}
		if local.calls != 3 {
			t.Errorf("the local client saw %d call(s), want 3 — the control that says these messages CAN "+
				"be classified, so 'zero hosted calls' means the boundary refused rather than nothing "+
				"happened", local.calls)
		}

		// The other half of the control: the SAME router and fixture on the
		// personal lane DO reach the hosted client. Without it, "zero hosted
		// calls" is satisfied by a fixture nothing would ever route hosted.
		hosted2, local2 := cfHosted(), iqLocal()
		if _, err := classify.Run(context.Background(), &cfStore{pending: iqMessages(3)},
			provider.NewRouter(hosted2, local2, time.Minute),
			classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LanePersonal}); err != nil {
			t.Fatalf("Run(personal): %v", err)
		}
		if hosted2.calls != 3 {
			t.Fatalf("POSITIVE CONTROL FAILED: the personal lane sent %d of 3 messages to the hosted "+
				"client. If this fixture never routes hosted on ANY lane, the zero above proves nothing "+
				"about the pin", hosted2.calls)
		}
	})

	t.Run("classReasonOf files an inquiry skip under lane_local_only", func(t *testing.T) {
		store := &cfStore{pending: iqMessages(4)}
		if _, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), nil, time.Minute), iqCfg()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		skips := store.withStatus("skipped")
		if len(skips) != 1 {
			t.Fatalf("recorded %d skipped rows, want exactly 1 aggregate row per refused pass", len(skips))
		}
		reasons, ok := cfDecode(t, skips[0].Input)["class_reasons"].(map[string]any)
		if !ok {
			t.Fatalf("input.class_reasons missing from %s", skips[0].Input)
		}
		if _, ok := reasons["lane_local_only"]; !ok {
			t.Errorf("class_reasons = %v, want the skips filed under \"lane_local_only\" (criterion 11c). "+
				"Today classReasonOf returns \"thread_context\" for an AttrProject message whose project "+
				"is not local_only — which would file every inquiry skip under a reason that describes a "+
				"different mechanism entirely, in the one column an operator reads", reasons)
		}
		if _, ok := reasons["thread_context"]; ok {
			t.Errorf("class_reasons = %v — an inquiry skip was filed under \"thread_context\". The lane is "+
				"restricted because the LANE pins the class, not because a neighbour dragged it down", reasons)
		}
	})
}

// ---- criteria 6 + 16: what the rendered prompt contains, and what it must not --

func TestRenderedPrompt_InquiryLane_CarriesPriorContextAndNoNumberedLinks(t *testing.T) {
	target := iqMessages(1)[0]
	target.SentAt = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// A message WITH links, which criterion 6 requires to render no numbered
	// list. normalized_messages.links is written by the google normalizer only,
	// so on this project the column is null on every row — the numbered block
	// would be a stored constant, which is the landmine this repo keeps paying
	// for. The fixture supplies links anyway, because a test on a null column
	// would pass for the wrong reason.
	target.Links = []classify.Link{
		{Text: "OPEN THE RUNBOOK", URL: "https://example.test/runbook"},
		{Text: "UNSUBSCRIBE", URL: "https://example.test/u"},
	}

	thread := []classify.ThreadMessage{
		{MessageID: 10, SentAt: target.SentAt.Add(-3 * time.Hour), Direction: "inbound",
			BodyText: "IQPRIOR-THEM deploy went out at noon"},
		{MessageID: 11, SentAt: target.SentAt.Add(-2 * time.Hour), Direction: "outbound",
			BodyText: "IQPRIOR-ME acknowledged, watching the dashboards"},
		// STRICTLY LATER than the target. Criterion 16: prior only, never a
		// message with a later sent_at/id — that is what lets a label stay
		// valid forever and makes a re-run a re-run.
		{MessageID: 99, SentAt: target.SentAt.Add(time.Hour), Direction: "inbound",
			BodyText: "IQLATER-THIS-MUST-NEVER-APPEAR the answer arrived after the fact"},
	}
	target.ThreadContext = classify.InquiryContext(target, thread)

	local := iqLocal()
	if _, err := classify.Run(context.Background(), &cfStore{pending: []classify.PendingMessage{target}},
		provider.NewRouter(nil, local, time.Minute), iqCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(local.requests) != 1 {
		t.Fatalf("the local client saw %d requests, want 1", len(local.requests))
	}
	user := local.requests[0].User

	if !strings.Contains(user, target.BodyText) {
		t.Errorf("the rendered prompt does not contain the TARGET message's body:\n%s", user)
	}
	for _, want := range []string{"IQPRIOR-THEM", "IQPRIOR-ME"} {
		if !strings.Contains(user, want) {
			t.Errorf("the rendered prompt is missing prior context %q. Criterion 16: up to "+
				"inquiryContextMax prior messages of the SAME thread, oldest->newest. Without them the "+
				"model cannot see that Salvador already answered two messages up, which is the whole "+
				"reason this lane loads context:\n%s", want, user)
		}
	}
	if strings.Contains(user, "IQLATER-THIS-MUST-NEVER-APPEAR") {
		t.Errorf("the rendered prompt contains a message that arrived AFTER the target. Criterion 16 and "+
			"D2: the prompt shows PRIOR context only, so a label stays valid forever and a re-run is a "+
			"re-run. A window that slides with the thread means yesterday's verdict cannot be "+
			"reproduced:\n%s", user)
	}
	// The direction tags. Both must appear, or a transcript of unattributed
	// lines tells the model nothing about who is waiting on whom.
	for _, tag := range []string{"me:", "them:"} {
		if !strings.Contains(user, tag) {
			t.Errorf("the rendered context has no %q tag. Criterion 16: each prior message is tagged from "+
				"normalized_messages.direction — 'them:' for inbound, 'me:' for outbound:\n%s", tag, user)
		}
	}
	// Ordering: oldest -> newest, and the target last.
	iThem := strings.Index(user, "IQPRIOR-THEM")
	iMe := strings.Index(user, "IQPRIOR-ME")
	iTarget := strings.Index(user, target.BodyText)
	if !(iThem < iMe && iMe < iTarget) {
		t.Errorf("the rendered prompt is not oldest->newest with the target last (them@%d me@%d target@%d)."+
			" A transcript in another order is a transcript the model reads as a different conversation",
			iThem, iMe, iTarget)
	}

	// CRITERION 6: no numbered candidate block on a contract without link_index.
	if strings.Contains(user, "Numbered links in this message") {
		t.Errorf("the inquiry prompt renders the numbered-candidate block. Criterion 6: renderUser's link "+
			"block is CONTRACT-CONDITIONAL — an inquiry verdict has no link_index to answer with, so the "+
			"list is a question the model would try to answer into a field that does not exist:\n%s", user)
	}
	if strings.Contains(user, "OPEN THE RUNBOOK") {
		t.Errorf("the inquiry prompt lists a link candidate's anchor text. Criterion 6: no numbered list " +
			"is rendered for a contract without link_index")
	}

	// And the request carries the lane's own contract, not the shared one.
	if local.requests[0].SchemaName == classify.SchemaName {
		t.Errorf("the inquiry request used SchemaName %q — the actionability contract's name (D1)",
			local.requests[0].SchemaName)
	}
	if fmt.Sprintf("%s", local.requests[0].Schema) != fmt.Sprintf("%s", classify.InquiryVerdictSchema) {
		t.Errorf("the inquiry request did not carry InquiryVerdictSchema:\n%s", local.requests[0].Schema)
	}
	if local.requests[0].System != classify.InquirySystemPrompt {
		t.Errorf("the inquiry request did not carry InquirySystemPrompt")
	}
}

// InquiryContext is the pure half of criterion 16 — the ordering, the strict
// (sent_at, id) bound and the cap. It is a separate function from the SQL
// loader for the reason the SPEC gives: PGStore.neighbours filters
// direction='inbound' for capture-decision reasons (invariant 5) and must not
// be reused here, because a transcript with our own replies removed is a
// transcript in which nothing was ever answered.
func TestInquiryContext_IsPriorOnlyOldestFirstAndCapped(t *testing.T) {
	target := iqMessages(1)[0]
	target.MessageID = 50
	target.SentAt = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	var thread []classify.ThreadMessage
	for i := 1; i <= 9; i++ {
		thread = append(thread, classify.ThreadMessage{
			MessageID: int64(i),
			SentAt:    target.SentAt.Add(-time.Duration(10-i) * time.Minute),
			Direction: "inbound",
			BodyText:  fmt.Sprintf("prior-%d", i),
		})
	}
	// Same instant as the target, higher id: LATER by the (sent_at, id) rule.
	thread = append(thread, classify.ThreadMessage{
		MessageID: 51, SentAt: target.SentAt, Direction: "outbound", BodyText: "tiebreak-later"})
	// Same instant, lower id: prior.
	thread = append(thread, classify.ThreadMessage{
		MessageID: 49, SentAt: target.SentAt, Direction: "outbound", BodyText: "tiebreak-prior"})
	// Strictly later.
	thread = append(thread, classify.ThreadMessage{
		MessageID: 60, SentAt: target.SentAt.Add(time.Hour), Direction: "inbound", BodyText: "later"})

	got := classify.InquiryContext(target, thread)

	if len(got) == 0 {
		t.Fatalf("InquiryContext returned nothing for a thread of %d messages; the loader would then be "+
			"indistinguishable from 'no context existed', which criterion 13's context_messages field "+
			"exists to tell apart", len(thread))
	}
	if len(got) > 6 {
		t.Errorf("InquiryContext returned %d messages; inquiryContextMax is 6 (criterion 16). The cap is "+
			"the prompt-size bound, and prompt size is what raises this lane's measured latency", len(got))
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.SentAt.Before(prev.SentAt) || (cur.SentAt.Equal(prev.SentAt) && cur.MessageID < prev.MessageID) {
			t.Fatalf("InquiryContext is not oldest->newest at index %d: %v/%d then %v/%d",
				i, prev.SentAt, prev.MessageID, cur.SentAt, cur.MessageID)
		}
	}
	for _, m := range got {
		if m.SentAt.After(target.SentAt) || (m.SentAt.Equal(target.SentAt) && m.MessageID >= target.MessageID) {
			t.Errorf("InquiryContext returned message %d (%v), which is NOT strictly before the target "+
				"(%d, %v). The bound is (sent_at, id) — sent_at alone leaves same-second Slack messages "+
				"ordered by nothing, and a context window that can include the answer makes the verdict "+
				"unreproducible", m.MessageID, m.SentAt, target.MessageID, target.SentAt)
		}
	}
	// The cap keeps the NEAREST prior messages, not an arbitrary six: a window
	// that kept the oldest would show the model the start of a conversation and
	// hide the exchange the question sits in.
	if len(got) == 6 && got[len(got)-1].BodyText != "tiebreak-prior" {
		t.Errorf("the last context message is %q, want \"tiebreak-prior\" — the cap keeps the six messages "+
			"NEAREST the target", got[len(got)-1].BodyText)
	}
	// Both directions travel. drafts/PGStore.neighbours' inbound-only filter
	// must not be reused here.
	dirs := map[string]int{}
	for _, m := range got {
		dirs[m.Direction]++
	}
	if dirs["outbound"] == 0 {
		t.Errorf("InquiryContext dropped every outbound message: %v. Criterion 16 says BOTH directions — "+
			"a transcript with our own replies removed is a transcript in which nothing was ever "+
			"answered, and the model would flag every question Salvador has already handled", dirs)
	}
}

// ---- criterion 13: the verdict carries the thread identity --------------------

// "Capture the exact thread so the reply goes to that same thread when done."
// This ticket AIMS NOTHING — it records the facts that make aiming possible, so
// a later drafting ticket re-derives none of them.
func TestRun_InquiryLane_VerdictRecordsTheThreadIdentity(t *testing.T) {
	target := iqMessages(1)[0]
	target.ThreadContext = []classify.ThreadMessage{
		{MessageID: 10, SentAt: target.SentAt.Add(-time.Hour), Direction: "inbound", BodyText: "earlier"},
		{MessageID: 11, SentAt: target.SentAt.Add(-time.Minute), Direction: "outbound", BodyText: "mine"},
	}

	store := &cfStore{pending: []classify.PendingMessage{target}}
	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(nil, iqLocal(), time.Minute), iqCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.extractions) != 1 {
		t.Fatalf("recorded %d extractions, want 1", len(store.extractions))
	}
	f := iqFields(t, store.extractions[0].fields)

	// The model's five, from the lane's own contract.
	for k, want := range map[string]any{
		"needs_reply": true,
		"ask_kind":    "question",
		"asker":       "Dana Ruiz",
		"ask":         "Can you confirm the rotation date for the staging key?",
	} {
		if got := f[k]; got != want {
			t.Errorf("fields[%q] = %v, want %v — decoded through the LANE'S contract, not the "+
				"actionability one (D1)", k, got, want)
		}
	}
	if _, ok := f["actionable"]; ok {
		t.Errorf("fields carries an `actionable` key: %v. The inquiry lane's decision is needs_reply; a "+
			"row carrying both would let a fold read whichever it found first", f["actionable"])
	}

	// The bookkeeping every lane records (criterion 13's tail).
	for k, want := range map[string]any{
		"sender":                target.Sender,
		"subject":               target.Subject,
		"channel":               target.Channel,
		"project_slug":          target.ProjectSlug,
		"normalized_message_id": float64(target.MessageID),
		"project_id":            float64(target.ProjectID),
	} {
		if got := f[k]; got != want {
			t.Errorf("fields[%q] = %v (%T), want %v. It is stored HERE, not looked up at report time: the "+
				"report reads ai_extractions alone, and a printer that joined back would describe a "+
				"message that may since have been re-normalised", k, got, got, want)
		}
	}
	// `channel` in particular is criterion 30's grouping key, and it comes from
	// the STORED fields — never a re-join to normalized_messages for a second
	// copy of what was classified.
	if f["channel"] != "slack" {
		t.Errorf("fields[\"channel\"] = %v, want \"slack\". Criterion 30's by-channel breakdown groups on "+
			"THIS value; without it the CLI report and /funnel would each have to re-derive it and could "+
			"disagree", f["channel"])
	}

	// The thread identity (criterion 13's head).
	if f["thread_id"] != float64(target.ThreadID) {
		t.Errorf("fields[\"thread_id\"] = %v, want %d — the normalized_threads row id, which is what "+
			"tasks.source_thread_id will carry via task_set_source_thread", f["thread_id"], target.ThreadID)
	}
	if f["thread_key"] != iqRootedKey {
		t.Errorf("fields[\"thread_key\"] = %v, want %q VERBATIM as stored at classify time. slackweb "+
			"appends ThreadRootID to the key, so this string is thread-exact and re-deriving it later is "+
			"exactly what this ticket exists to avoid", f["thread_key"], iqRootedKey)
	}
	if f["thread_scope"] != "thread" {
		t.Errorf("fields[\"thread_scope\"] = %v, want \"thread\" for a rooted slack key (%q). The scope is "+
			"the honest limit stated as data: `conversation` says there is no thread to reply INTO yet",
			f["thread_scope"], iqRootedKey)
	}
	if f["external_message_id"] != iqExternalMsgID {
		t.Errorf("fields[\"external_message_id\"] = %v, want %q. For a conversation-scoped verdict this is "+
			"the ONLY thing a later ticket can root a new thread at — its last segment is the same p… "+
			"token slackweb.ParseTargetURL accepts as a message id", f["external_message_id"], iqExternalMsgID)
	}
	// context_messages: without it, "no context existed" and "context was not
	// loaded" are the same row.
	if f["context_messages"] != float64(2) {
		t.Errorf("fields[\"context_messages\"] = %v, want 2. Criterion 13 names it explicitly: a verdict "+
			"with no count cannot tell an operator whether the model saw the thread or whether the loader "+
			"silently returned nothing", f["context_messages"])
	}
}

// A message with NO thread records thread_scope='none' — the third value, and
// the one a fold must not read as "answered".
func TestRun_InquiryLane_NoThread_RecordsScopeNone(t *testing.T) {
	m := iqMessages(1)[0]
	m.ThreadID = 0
	m.ThreadKey = ""
	m.ThreadContext = nil

	store := &cfStore{pending: []classify.PendingMessage{m}}
	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(nil, iqLocal(), time.Minute), iqCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.extractions) != 1 {
		t.Fatalf("recorded %d extractions, want 1 — a message with no thread is still CLASSIFIED, and "+
			"recording nothing would make 'no thread' look like 'not in the inbox'", len(store.extractions))
	}
	f := iqFields(t, store.extractions[0].fields)
	if f["thread_scope"] != "none" {
		t.Errorf("fields[\"thread_scope\"] = %v, want \"none\" for a message with no thread_id (criterion "+
			"13). Defaulting it to \"conversation\" would put the message into the "+
			"spoke-in-conversation-since counter, which is a claim about a thread that does not exist",
			f["thread_scope"])
	}
	if f["context_messages"] != float64(0) {
		t.Errorf("fields[\"context_messages\"] = %v, want 0", f["context_messages"])
	}
}

// ---- D6: --since is REQUIRED on this lane -------------------------------------

func TestRun_InquiryLane_RefusesAnUnboundedPass(t *testing.T) {
	t.Run("no --since: refuse, read nothing, send nothing", func(t *testing.T) {
		local := iqLocal()
		store := &lnStore{cfStore: cfStore{pending: iqMessages(3)}}

		_, err := classify.Run(context.Background(), store,
			provider.NewRouter(nil, local, time.Minute),
			classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LaneInquiry})
		if err == nil {
			t.Fatalf("Run accepted an unbounded inquiry pass. D6: --since is REQUIRED on this lane. An " +
				"inquiry has a shelf life of DAYS, so a verdict on a six-month-old message is GPU spent " +
				"on nothing — and the armed project's historical corpus is unbounded from the code's " +
				"point of view")
		}
		msg := err.Error()
		if !strings.Contains(msg, "--since") {
			t.Errorf("the refusal does not tell the operator what to pass: %q", msg)
		}
		if !regexp.MustCompile(`(?i)gpu|hour|minute`).MatchString(msg) {
			t.Errorf("the refusal names no unit of cost: %q. SWT-23's shape: the arithmetic goes IN the "+
				"message, because a refusal that does not show its working teaches the reader to pass "+
				"--since 87600h to make it go away", msg)
		}
		if len(regexp.MustCompile(`\d`).FindAllString(msg, -1)) < 2 {
			t.Errorf("the refusal carries no arithmetic: %q. It must name the rate and the per-verdict "+
				"cost the estimate is built from — and the SPEC says to RE-MEASURE the median during "+
				"verification rather than quoting 10 s, so whatever number is here must be the one that "+
				"was measured", msg)
		}
		if store.pendingCalls != 0 || local.calls != 0 {
			t.Errorf("the refused pass read the inbox %d time(s) and called the model %d time(s); it must "+
				"do neither", store.pendingCalls, local.calls)
		}
	})

	t.Run("control: with --since the same fixture runs", func(t *testing.T) {
		local := iqLocal()
		store := &cfStore{pending: iqMessages(3)}
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(nil, local, time.Minute), iqCfg())
		if err != nil {
			t.Fatalf("Run(inquiry, --since 168h): %v. Without this control the refusal above is satisfied "+
				"by a lane that never runs at all", err)
		}
		if local.calls != 3 || stats.Processed != 3 {
			t.Fatalf("local calls = %d, stats = %+v; want 3 classified", local.calls, stats)
		}
	})

	t.Run("the personal lane keeps today's behaviour", func(t *testing.T) {
		store := &cfStore{pending: cfMessages(2)}
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), cfLocal(), time.Minute), cfCfg())
		if err != nil {
			t.Fatalf("an unbounded PERSONAL pass was refused: %v. --since stays optional there; making it "+
				"mandatory breaks every command in the SWT-22 runbook", err)
		}
		if stats.Processed != 2 {
			t.Errorf("stats = %+v, want 2 processed", stats)
		}
	})
}

// ---- criteria 11 + 24: the run's bookkeeping ---------------------------------

func TestRun_InquiryLane_RecordsItsOwnWorkerTypeAndPromptVersion(t *testing.T) {
	store := &cfStore{pending: iqMessages(1)}
	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(nil, iqLocal(), time.Minute), iqCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	oks := store.withStatus("ok")
	if len(oks) != 1 {
		t.Fatalf("recorded %d status='ok' rows, want 1", len(oks))
	}
	if got := oks[0].WorkerType; got != "classify_inquiry" {
		t.Errorf("ai_runs.worker_type = %q, want \"classify_inquiry\". The value comes from "+
			"cfg.Lane.WorkerType; hardcoding another lane's makes the message permanently invisible to "+
			"that lane and puts an inquiry verdict in the promoter's inbox", got)
	}
	in := cfDecode(t, oks[0].Input)
	if got, _ := in["prompt_version"].(string); got != classify.InquiryPromptVersion {
		t.Errorf("ai_runs.input.prompt_version = %v, want %q — runInput reads cfg.Lane.PromptVersion, not "+
			"a package constant", in["prompt_version"], classify.InquiryPromptVersion)
	}
}

// ---- criterion 19: Summarize keeps its signature and resolves the lane --------

// The compile-time half. summary_structure_test.go:39 already pins
// ReportForWorker's signature this way; criterion 19 adds the same pin for
// Summarize, because the inquiry fold is the thing most likely to be spelled as
// a new `Summarize(ctx, pool, since, lane Lane)` — which would break
// `cmd/classify report`, /funnel and the golden characterization in one edit.
var _ func(context.Context, *pgxpool.Pool, time.Duration, string) (classify.Summary, error) = classify.Summarize
var _ func(context.Context, *pgxpool.Pool, io.Writer, time.Duration, string) error = classify.ReportForWorker

func TestLaneByWorkerType_ResolvesTheThreeAndRefusesAnythingElse(t *testing.T) {
	for _, l := range []classify.Lane{classify.LanePersonal, classify.LaneResidue, classify.LaneInquiry} {
		got, ok := classify.LaneByWorkerType(l.WorkerType)
		if !ok {
			t.Errorf("LaneByWorkerType(%q) = not found; criterion 19: Summarize resolves the lane from the "+
				"worker_type it is already given, which is how the contract-aware fold happens without "+
				"changing the signature", l.WorkerType)
			continue
		}
		if got.Name != l.Name {
			t.Errorf("LaneByWorkerType(%q).Name = %q, want %q", l.WorkerType, got.Name, l.Name)
		}
	}

	// The unrecognised case, spelled with the value that actually exists: the
	// byte-identical golden report test seeds `itest-swt29-golden`, which is NOT
	// a lane and must keep today's behaviour (the actionability fold). A
	// LaneByWorkerType that defaulted to LaneInquiry would silently rewrite that
	// golden.
	if _, ok := classify.LaneByWorkerType("itest-swt29-golden"); ok {
		t.Errorf("LaneByWorkerType(\"itest-swt29-golden\") reports a lane. Criterion 19: an unrecognised " +
			"worker_type keeps TODAY'S behaviour — summary_integration_test.go's golden uses exactly this " +
			"value so an absolute assertion has an isolated population, and it is not a third lane")
	}
	if _, ok := classify.LaneByWorkerType(""); ok {
		t.Errorf("LaneByWorkerType(\"\") reports a lane; the empty worker_type is not one")
	}
}
