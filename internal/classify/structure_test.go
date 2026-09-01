package classify_test

// Structural tests for SWT-22 — the criteria this SPEC asks to be enforced
// mechanically rather than by review: 10 (the advisory-lock key and its
// collision scan), 13 (the honesty label), 18 (NO confidence field, and the
// output contract), 19 (one prompt for all senders), 20 (the closing note both
// reports must carry), 21/24 (the labelled set's shape), 25 and 26 (the two
// runbooks), and the data-model guard on migrations.
//
// EXTENDED BY SWT-25 (link-preservation), which changes several of the facts
// SWT-22 pinned here and therefore had to edit this file rather than add a
// second one beside it (criterion 26): the output contract is FIVE fields now,
// the migration guard pins 0017 instead of asserting that no migration exists,
// and three new scans cover the URL-free schema (criterion 19), the no-fetch
// rule (24) and the docs (25, 27, 28).
//
// ZERO I/O beyond reading this repo's own source, docs and eval file. Same shape
// as internal/provider/structure_test.go and internal/capture/rules_structure_test.go,
// and the same rule applies: every assertion first REQUIRES its subject to
// exist, because a source scan that passes because there was nothing to scan is
// the "fixture that proves nothing" landmine wearing a lab coat.
//
// GREENFIELD NOTE: internal/classify, cmd/classify, docs/runbooks/local-classifier.md
// and docs/evals/personal-actionability.jsonl do not exist yet, so this file
// compile-FAILS and — once it compiles — fails on the missing files. Expected red.

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/classify"
)

// ---- helpers -----------------------------------------------------------------

func csRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// csGoCode returns a Go file with COMMENTS REMOVED. These files explain
// themselves — criterion 13 REQUIRES a comment about the class fold and
// criterion 18 requires one about confidence — so a banned-token scan over raw
// text would trip on its own subject's prose. What remains is code, including
// string literals, which is where a smuggled reference would actually live.
func csGoCode(t *testing.T, rel string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, 0)
	if err != nil {
		t.Errorf("parse %s: %v", rel, err)
		return ""
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		t.Errorf("print %s: %v", rel, err)
		return ""
	}
	return buf.String()
}

// csSources lists the non-test .go files of a package, relative to the repo
// root, and FAILS when there are none.
func csSources(t *testing.T, pkg string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", pkg))
	if err != nil {
		t.Fatalf("read %s: %v", pkg, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(pkg, n))
	}
	if len(out) == 0 {
		t.Fatalf("no non-test .go files in %s; a scan with nothing to scan proves nothing", pkg)
	}
	return out
}

// csStringLiterals returns every string literal in a Go file, joined by spaces
// and whitespace-collapsed.
//
// Needed because the sentence criterion 20 shares between two reports is printed
// across several Fprintln calls, so a plain strings.Contains over the source
// fails on a rendering that is correct. Joining the literals compares what the
// operator READS, not how it was typed.
func csStringLiterals(t *testing.T, rel string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var parts []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			parts = append(parts, s)
		}
		return true
	})
	joined := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if len(joined) < 40 {
		t.Fatalf("%s yielded %d characters of string literals; the scan below would pass vacuously", rel, len(joined))
	}
	return joined
}

// ---- criterion 18: NO confidence field, anywhere ------------------------------

// The constant-discriminator landmine wearing a model's clothes. qwen3:8b
// returns exactly 0.95 on everything it flags — 27 true positives and 17 false
// positives, IDENTICAL. A confidence gate built on that would look principled,
// pass review, and do nothing; a human-review lane keyed on it would be keyed on
// a constant. CLAUDE.md's "per-field confidence → human-review lane" is a TRIAGE
// contract and does not carry over.
//
// So the field is removed entirely rather than stored with a comment saying it
// means nothing — a stored number grows a consumer.
func TestSchema_HasNoConfidenceFieldAnywhere(t *testing.T) {
	var schema any
	if err := json.Unmarshal(classify.VerdictSchema, &schema); err != nil {
		t.Fatalf("classify.VerdictSchema is not valid JSON: %v", err)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if strings.EqualFold(k, "confidence") {
					t.Errorf("the output schema contains a `confidence` key at %s.%s. qwen3:8b returns "+
						"EXACTLY 0.95 on everything it flags — 27 true positives and 17 false positives, "+
						"identical — so this is not a dial: it is the 'predicate whose discriminating column "+
						"is a constant' landmine wearing a model's clothes. Criterion 18 removes the field "+
						"rather than storing a number a future reader will threshold on. The way to trade "+
						"precision back is a SECOND PASS with a different model (Future work)", path, k)
				}
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range v {
				walk(child, path+"["+strconv.Itoa(i)+"]")
			}
		case string:
			if strings.EqualFold(v, "confidence") {
				t.Errorf("the output schema names \"confidence\" at %s (a required-list or enum entry)", path)
			}
		}
	}
	walk(schema, "$")

	// And nowhere in the package's CODE either — a struct field, a JSON tag, or a
	// literal in a prompt would all reintroduce it. Comments are stripped, so the
	// doc comment criterion 18 asks for does not trip its own scan.
	for _, rel := range csSources(t, "internal/classify") {
		if strings.Contains(strings.ToLower(csGoCode(t, rel)), "confidence") {
			t.Errorf("%s mentions `confidence` in code (comments were stripped). Criterion 18: there is no "+
				"confidence field anywhere — not in the schema, not in ai_extractions.fields, not in the "+
				"prompt", rel)
		}
	}
}

// The rest of criterion 18's output contract, deliberately small. `kind` MUST be
// an enum: the spike measured that an unconstrained one comes back as
// "payment due", "violation_to_cure" and "statement-availabl…" in a single run —
// a column nothing can GROUP BY, which is this repo's recurring landmine in yet
// another costume. `format` enforces the enum, so the schema is where it lives.
//
// AMENDED BY SWT-25 (link-preservation), criteria 18 and 26. The contract is now
// FIVE fields: {actionable, kind, title, reason, link_index}. `link_index` is
// `{"type": ["integer","null"]}` — an INDEX into normalized_messages.links, 1-based,
// never a URL. This test is updated in the SAME change as prompt.go rather than
// left contradicting it, because "a comment that states the opposite of its code"
// is a defect this repo has shipped twice (SWT-21) and criterion 26 exists to
// stop a third.
func TestSchema_MatchesTheOutputContract(t *testing.T) {
	var schema struct {
		Type                 string   `json:"type"`
		AdditionalProperties *bool    `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           map[string]struct {
			Type json.RawMessage `json:"type"`
			Enum []string        `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(classify.VerdictSchema, &schema); err != nil {
		t.Fatalf("classify.VerdictSchema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Errorf("schema does not set additionalProperties:false; a model that invents a field would have " +
			"it stored unchallenged")
	}
	want := map[string][]string{
		"actionable": {"boolean"},
		"kind":       {"string"},
		"title":      {"string"},
		"reason":     {"string"},
		"link_index": {"integer", "null"},
	}
	for name, types := range want {
		p, ok := schema.Properties[name]
		if !ok {
			t.Errorf("schema has no %q property; the contract is {actionable, kind, title, reason, "+
				"link_index} and nothing else. SWT-25 criterion 18 made it FIVE fields: the model answers "+
				"with the NUMBER of the link a person would open, and the application resolves that number "+
				"to a URL", name)
			continue
		}
		if got := csTypeSet(t, name, p.Type); !reflect.DeepEqual(got, types) {
			t.Errorf("schema property %q has type %v, want %v", name, got, types)
		}
	}
	for name := range schema.Properties {
		if _, ok := want[name]; !ok {
			t.Errorf("schema has an extra property %q; the contract is deliberately small", name)
		}
	}
	gotRequired := map[string]bool{}
	for _, r := range schema.Required {
		gotRequired[r] = true
	}
	for name := range want {
		if !gotRequired[name] {
			t.Errorf("schema does not require %q", name)
		}
	}
	kindEnum := schema.Properties["kind"].Enum
	wantEnum := []string{"payment_due", "deadline", "appointment", "action_required", "informational"}
	if len(kindEnum) != len(wantEnum) {
		t.Fatalf("kind enum = %v, want %v. Left a free string, the model returns the same concept in three "+
			"casings — \"payment due\", \"violation_to_cure\", \"statement-availabl…\" — which is a report "+
			"column nothing can GROUP BY", kindEnum, wantEnum)
	}
	got := map[string]bool{}
	for _, k := range kindEnum {
		got[k] = true
	}
	for _, k := range wantEnum {
		if !got[k] {
			t.Errorf("kind enum is missing %q: %v", k, kindEnum)
		}
	}
}

// csTypeSet reads a JSON-Schema `type`, which is legally either a string or an
// array of strings, and returns it sorted so a union can be compared.
func csTypeSet(t *testing.T, prop string, raw json.RawMessage) []string {
	t.Helper()
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		t.Fatalf("schema property %q has a `type` that is neither a string nor an array: %s", prop, raw)
	}
	sort.Strings(many)
	return many
}

// ---- SWT-25 criterion 19: the model CANNOT emit a URL -------------------------

// The structural half of this ticket, and the reason the whole design returns an
// index. A string field named `link` would put the guarantee back into prompt
// discipline, where it does not survive a model update — and a URL a model wrote
// is a URL nothing verified, stored in ai_extractions and printed in a report a
// human acts on.
func TestSchema_HasNoURLTypedField(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(classify.VerdictSchema, &schema); err != nil {
		t.Fatalf("classify.VerdictSchema is not valid JSON: %v", err)
	}
	if len(schema.Properties) == 0 {
		t.Fatalf("the schema declares no properties; a scan with nothing to scan proves nothing")
	}
	urlish := regexp.MustCompile(`(?i)(url|href|link_url|uri)`)

	// link_index is the ONLY new property, and its declared type contains only
	// integer and null. Asserted here as well as in the contract test above so
	// this test fails on a MISSING link_index too: a test that can only ever go
	// red when someone adds a bad field is a test that passes today for the
	// wrong reason.
	li, ok := schema.Properties["link_index"]
	if !ok {
		t.Errorf("the schema has no `link_index` property. This is the field that makes 'the model never " +
			"authors a URL' STRUCTURAL rather than a matter of prompt discipline: the numbered list offers " +
			"TEXTS, the model answers with a position, and the application resolves it")
	} else if got := csTypeSet(t, "link_index", liType(t, li)); !reflect.DeepEqual(got, []string{"integer", "null"}) {
		t.Errorf("link_index type = %v, want [integer null] exactly. `null` is not optional: it is how the "+
			"model says 'none of these', which is ORDINARY output — and a schema-constrained decoder needs "+
			"the union to be able to emit it at all", got)
	}

	for name := range schema.Properties {
		if urlish.MatchString(name) {
			t.Errorf("the output schema declares a property %q. THE MODEL MUST NEVER AUTHOR A URL: the "+
				"numbered candidate list is offered as TEXTS, the model returns link_index, and the "+
				"application resolves the index against normalized_messages.links. Returning an INDEX is "+
				"what makes that structural — a string field here makes it a matter of prompt discipline, "+
				"which does not survive a model update", name)
		}
	}
	// And the whole schema document, so a url can not arrive as an enum value, a
	// `format: uri`, or a nested property either.
	var whole any
	if err := json.Unmarshal(classify.VerdictSchema, &whole); err != nil {
		t.Fatalf("classify.VerdictSchema is not valid JSON: %v", err)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if strings.EqualFold(k, "format") {
					if s, ok := child.(string); ok && urlish.MatchString(s) {
						t.Errorf("the schema declares %s: %q — a URI-typed value is still a URL the model authored", path, s)
					}
				}
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range v {
				walk(child, path+"["+strconv.Itoa(i)+"]")
			}
		}
	}
	walk(whole, "$")
}

// liType pulls the `type` member out of one raw schema property.
func liType(t *testing.T, prop json.RawMessage) json.RawMessage {
	t.Helper()
	var obj struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(prop, &obj); err != nil || len(obj.Type) == 0 {
		t.Fatalf("schema property link_index declares no `type`: %s", prop)
	}
	return obj.Type
}

// ---- criterion 19: the prompt, and ONE of it ---------------------------------

func TestPrompt_StatesTheObjectiveAndTheAttachmentCeiling(t *testing.T) {
	lower := strings.ToLower(classify.SystemPrompt)
	if len(lower) < 200 {
		t.Fatalf("classify.SystemPrompt is %d characters; the spike measured that the PROMPT is more "+
			"load-bearing than the model — a bare 'Classify.' called an HOA violation notice carrying a "+
			"fine actionable:false, and a proper system prompt scored the same model 5/5", len(lower))
	}
	if !strings.Contains(lower, "recall") && !strings.Contains(lower, "late fee") &&
		!strings.Contains(lower, "miss") {
		t.Errorf("the prompt never states the objective it was MEASURED on: recall first, because a missed " +
			"payment notice is a late fee and a false alarm costs a second to dismiss. Without it the next " +
			"editor will tune for precision, which is the wrong direction and looks like an improvement")
	}
	if !strings.Contains(lower, "attach") {
		t.Errorf("the prompt does not handle the attachment ceiling (criterion 19). The Pines fine amount " +
			"and cure-by date live in a PDF inside rfc822_b64 that nothing extracts, so no classifier can " +
			"read them today — 'HOA violation notice — open the attachment' is the honest best, and " +
			"guessing an amount or a date is the failure mode this sentence prevents")
	}
	// The near-miss class is the one clause the spike measured the cost of
	// dropping: a SHORTENED prompt, identical except that it stopped naming
	// statement-available / balance / card-was-used as informational, flipped
	// "your statement is available" to actionable — on the single most common
	// message shape in the corpus (883 BofA messages).
	if !strings.Contains(lower, "statement") {
		t.Errorf("the prompt does not name the near-miss class (statement-available / balance / " +
			"card-was-used as informational). Dropping that one clause was measured to flip the most common " +
			"message shape in the corpus into a false positive")
	}

	// SWT-25 criterion 23: ONE short paragraph about the numbered list. The
	// attachment clause above stays EXACTLY as it is — "defer to the attachment"
	// and "defer to the portal" are different shapes, and only the second one is
	// answerable now.
	numbered := regexp.MustCompile(`number|numbered|list`)
	if !numbered.MatchString(lower) {
		t.Errorf("the prompt never mentions the numbered list of links (criterion 23). The list is the " +
			"COMPLETE set of links available, and the model has to be told to answer with the NUMBER of the " +
			"one a person would open — not with a URL, which it must never author")
	}
	if !strings.Contains(lower, "null") {
		t.Errorf("the prompt never mentions `null` (criterion 23). link_index: null is ORDINARY output: it " +
			"is the answer when none of the offered links is the one to act on AND the answer when no list " +
			"is shown at all — which is the common case, because the two HOA First Notices have no usable " +
			"link, only a tracking pixel. A model that is not told this invents a number")
	}
	invent := regexp.MustCompile(`never invent|do not invent|don't invent|never make up`)
	if !invent.MatchString(lower) {
		t.Errorf("the prompt does not forbid inventing a number (criterion 23). Out-of-range is handled " +
			"structurally by ResolveLink — rejected, recorded, never an error — but the prompt still has to " +
			"ask for an index that exists")
	}
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		t.Errorf("the SystemPrompt contains a URL. The prompt offers anchor TEXTS ONLY (criterion 17); " +
			"showing the model a URL is showing it the shape of the thing it must never author")
	}
}

// ONE PROMPT FOR ALL SENDERS. Per-sender prompts are rules in a costume:
// unbounded maintenance, untestable in aggregate, unattributable when they
// misfire. What Pines actually shows is a missing CONTEXT FACT, and a fact
// belongs in a column (Future work), never in a branch.
func TestPackage_HasNoPerSenderBranch(t *testing.T) {
	files := csSources(t, "internal/classify")

	// (a) a sender-shaped comparison
	branch := []*regexp.Regexp{
		regexp.MustCompile(`(?i)sender[^\n]{0,40}(==|!=)\s*"`),
		regexp.MustCompile(`(?i)strings\.(Contains|HasPrefix|HasSuffix|EqualFold)\([^)\n]*sender[^)\n]*"`),
		regexp.MustCompile(`(?i)switch[^\n{]{0,40}sender`),
	}
	// (b) a sender LITERAL. A per-sender prompt has to name a sender somewhere,
	// so this catches the shape even when the branch is spelled some way (a)
	// does not anticipate.
	literal := []*regexp.Regexp{
		regexp.MustCompile(`"[^"\n]*@[A-Za-z0-9.-]+\.[A-Za-z]{2,}[^"\n]*"`),
		regexp.MustCompile(`(?i)"[^"\n]*pinesproperty[^"\n]*"`),
	}

	for _, rel := range files {
		code := csGoCode(t, rel)
		for _, re := range branch {
			if m := re.FindString(code); m != "" {
				t.Errorf("%s branches on the sender (%q). Criterion 19: ONE prompt for all senders — "+
					"per-sender prompts are rules in a costume, and the missing thing at Pines is a FACT "+
					"('this sender emits both routine announcements and fine notices') that belongs in a "+
					"column injected into the one prompt, editable through opsctl and visible in the report",
					rel, strings.TrimSpace(m))
			}
		}
		// The literal scan has one honest false positive: an example address in the
		// prompt's own prose is not a branch. The opt-out is a marker comment, in
		// the shape internal/provider/structure_test.go's
		// `locality-default-is-deliberate` established — visible in a diff and
		// greppable in review, which a silent exemption would not be.
		if strings.Contains(csRepoFile(t, rel), "sender-literal-is-not-a-branch") {
			continue
		}
		for _, re := range literal {
			if m := re.FindString(code); m != "" {
				t.Errorf("%s contains a sender literal (%s). The classifier's job is the sender nobody has "+
					"written a rule for yet; a sender the code knows by name belongs in capture_rules, where "+
					"it is reproducible and attributable. If this is an EXAMPLE in the prompt rather than a "+
					"branch, say so with the marker comment sender-literal-is-not-a-branch",
					rel, strings.TrimSpace(m))
			}
		}
	}
}

// ---- criterion 13: the honesty label ------------------------------------------

// This ticket contains a constant-in-production predicate BY CONSTRUCTION, and
// the SPEC's answer is to LABEL it rather than let the class fold read as a
// guard. The label has to be in the code, because the code is where the next
// reader will decide whether the fold is protecting anything.
func TestPackage_CarriesTheHonestyLabel(t *testing.T) {
	var all strings.Builder
	for _, rel := range csSources(t, "internal/classify") {
		all.WriteString(csRepoFile(t, rel))
		all.WriteString("\n")
	}
	lower := strings.ToLower(all.String())

	if !regexp.MustCompile(`by construction`).MatchString(lower) ||
		!regexp.MustCompile(`classrestricted|restricted`).MatchString(lower) {
		t.Errorf("no comment in internal/classify says that every message this worker sees is " +
			"ClassRestricted BY CONSTRUCTION. Criterion 13 requires it: the inbox selects only " +
			"ai_locality='local_only' projects, so the class fold cannot change an outcome here and a unit " +
			"test that supplies the class proves nothing. Without the sentence the fold reads as protection " +
			"it is not providing — which is how this repo has been bitten seven times")
	}
	if !regexp.MustCompile(`inbox filter`).MatchString(lower) {
		t.Errorf("the honesty label does not name what DOES protect this worker. Criterion 13: (a) the " +
			"inbox filter, pinned by criterion 12's integration test, and (b) the Router's refusal, pinned " +
			"by criterion 14's zero-hosted-calls test. A label that only says 'this is inert' invites " +
			"someone to delete the fold")
	}
}

// ---- criterion 10: the advisory-lock key has no collision ---------------------

func TestAdvisoryLockKey_HasNoCollisionInTheRepo(t *testing.T) {
	key := regexp.MustCompile(`0x5157_?[0-9A-Fa-f]{4}`)
	found := map[string][]string{} // normalised key -> files

	err := filepath.Walk(filepath.Join("..", "..", "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range key.FindAllString(string(b), -1) {
			norm := strings.ToLower(strings.ReplaceAll(m, "_", ""))
			found[norm] = append(found[norm], strings.TrimPrefix(path, "../../"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	// Control: the scan must see the keys that are already taken, or a green
	// result means "the regex matched nothing" rather than "no collision".
	if len(found) < 4 {
		t.Fatalf("found %d distinct 0x5157xxxx keys in internal/ (%v); orchestrator, triage, google/drafts "+
			"and capture are all supposed to be there, so this scan is not seeing them", len(found), found)
	}

	owners := found["0x51570022"]
	if len(owners) == 0 {
		t.Fatalf("no file in internal/ declares 0x5157_0022; criterion 10 gives classify that key")
	}
	for _, f := range owners {
		if !strings.HasPrefix(f, "internal/classify/") {
			t.Errorf("0x5157_0022 also appears in %s. Two workers sharing an advisory-lock key silently "+
				"exclude each other: whichever runs second exits as if another instance were already "+
				"running, on a schedule, with no error", f)
		}
	}
}

// ---- criterion 20: one rule, two renderings, one test -------------------------

// The half a counter cannot carry. Nothing in a skipped-count distinguishes
// "idle by design" from "broken", and the obvious fix a reader invents for
// "broken" is a fallback to the hosted lane — the one change the boundary
// exists to prevent. So both reports have to say it, and the test that says
// "both" is what stops the second one drifting.
func TestReports_ShareTheNoFallbackNote(t *testing.T) {
	for _, rel := range []string{"internal/triage/report.go", "internal/classify/report.go"} {
		text := strings.ToLower(csStringLiterals(t, rel))

		noFallback := regexp.MustCompile(`fall(ing)?\s?back[^.]{0,80}(never|not) the fix|` +
			`(never|not)[^.]{0,60}fall(ing)?\s?back`)
		if !noFallback.MatchString(text) {
			t.Errorf("%s never prints that falling back to a hosted provider is not the fix. An operator "+
				"reading an all-skipped report has to be told this in the report itself: by the time they "+
				"open the runbook they have already invented the fallback", rel)
		}
		allSkipped := regexp.MustCompile(`(all[- ]skipped|every message[^.]{0,40}skipped|skips? everything)` +
			`[^.]{0,200}(expect|by design|success|normal|not an outage)`)
		if !allSkipped.MatchString(text) {
			t.Errorf("%s never prints that an all-skipped pass is EXPECTED when the local box is down "+
				"(criterion 20). Without it, 'the box was off' and 'nothing was actionable' are the same "+
				"report — which is precisely the alarm criterion 17 exists to split", rel)
		}
		if !strings.Contains(text, "provider-locality") && !strings.Contains(text, "runbook") {
			t.Errorf("%s does not point at a runbook; the note is the entry point to the longer argument", rel)
		}
	}
}

// ---- criteria 21 and 24: the labelled set --------------------------------------

func TestLabelsFile_IsIdsAndLabelsOnly(t *testing.T) {
	const rel = "docs/evals/personal-actionability.jsonl"
	raw := csRepoFile(t, rel)

	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := map[float64]bool{}
	lines := 0

	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("%s:%d does not parse as one JSON object: %v", rel, i+1, err)
			continue
		}
		for _, banned := range []string{"subject", "body", "sender", "from", "text", "snippet"} {
			if _, ok := obj[banned]; ok {
				t.Errorf("%s:%d carries a %q key. THE FILE CARRIES NO MESSAGE CONTENT — not a subject, not "+
					"a body, not a sender. It is committed to a git repo; the whole reason this classifier "+
					"is local is that this mail must not be copied around", rel, i+1, banned)
			}
		}
		id, ok := obj["message_id"].(float64)
		if !ok || id <= 0 {
			t.Errorf("%s:%d has no positive numeric message_id: %v", rel, i+1, obj["message_id"])
		} else if seen[id] {
			t.Errorf("%s:%d repeats message_id %.0f; a duplicated id weights one message twice in the score",
				rel, i+1, id)
		} else {
			seen[id] = true
		}
		switch obj["label"] {
		case "actionable", "not":
		default:
			t.Errorf("%s:%d label = %v, want \"actionable\" or \"not\"", rel, i+1, obj["label"])
		}
		h, _ := obj["subject_sha256"].(string)
		if !hex16.MatchString(h) {
			t.Errorf("%s:%d subject_sha256 = %q, want 16 lowercase hex characters. It is what makes a "+
				"re-pointed id VISIBLE (criterion 23) — the labels are the fixture and this fixture has "+
				"already been wrong once", rel, i+1, h)
		}
		for k := range obj {
			switch k {
			case "message_id", "label", "subject_sha256", "note":
			default:
				t.Errorf("%s:%d has an unexpected key %q; the record is ids + labels + a subject hash + an "+
					"optional note, and nothing else", rel, i+1, k)
			}
		}
	}

	if lines < 100 {
		t.Errorf("%s has %d labelled messages; criterion 24's minimum to merge is 100 HAND-CHECKED messages "+
			"drawn from the personal population, including the Pines announcements-vs-violations pairs. The "+
			"spike's labels do not carry over: they were regex-generated and scored every model 0.10-0.27 "+
			"because the FIXTURE was wrong, not the models", rel, lines)
	}
}

// ---- criterion 25: the new runbook ---------------------------------------------

// A test on prose, and it earns it the same way TestRunbook_DocumentsCaptureBeforeTriage
// does. Both of these rules are prose-shaped, neither can be enforced by code,
// and both are exactly what a future session would "clean up": `think: false`
// looks like a stray flag, and a confidence field looks like a missing feature.
func TestRunbook_LocalClassifier(t *testing.T) {
	doc := strings.ToLower(csRepoFile(t, "docs/runbooks/local-classifier.md"))

	think := regexp.MustCompile(`think[^a-z0-9]{0,6}false`)
	if !think.MatchString(doc) {
		t.Errorf("the runbook does not state the think-disabled rule. With thinking ON, qwen3 scored 0.00 " +
			"with 70/70 malformed outputs — empty content, done_reason \"length\", the whole budget spent " +
			"reasoning about a ten-word subject. It is invisible unless you read the raw response, which is " +
			"why the runbook has to show HOW to see it")
	}
	if !strings.Contains(doc, "done_reason") {
		t.Errorf("the runbook never mentions done_reason; criterion 25 requires it to show how to SEE the " +
			"failure that `think: false` prevents, not merely to assert the setting")
	}
	confidence := regexp.MustCompile(`confiden[^.\n]{0,160}(constant|0\.95|never|not a (dial|threshold))|` +
		`(constant|0\.95)[^.\n]{0,160}confiden`)
	if !confidence.MatchString(doc) {
		t.Errorf("the runbook does not state that self-reported confidence is a CONSTANT and must never be " +
			"a threshold. qwen3:8b returns exactly 0.95 on everything it flags, true and false positives " +
			"alike; a future session will otherwise read the missing field as an omission and add it back")
	}
	for _, want := range []string{"qwen3:8b", "per-sender", "ops_local_model", "classify eval", "second pass"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the runbook never mentions %q; criterion 25 lists the model and why, that per-sender "+
				"prompts are out, the env contract, the eval workflow, and the second pass as the named "+
				"future dial", want)
		}
	}
	if !strings.Contains(doc, "local_unreachable") {
		t.Errorf("the runbook does not show what a SKIP looks like versus an unremarkable verdict " +
			"(criterion 25). That difference is criterion 17's whole point and it is the thing an operator " +
			"has to read off a report without opening psql")
	}
}

// ---- criterion 26: the old runbook, corrected ---------------------------------

func TestRunbook_ProviderLocalityUpdatedForSWT22(t *testing.T) {
	const rel = "docs/runbooks/provider-locality.md"
	doc := csRepoFile(t, rel)
	lower := strings.ToLower(doc)

	if strings.Contains(lower, "11434/v1") {
		t.Errorf("%s still tells the operator to set OPS_LOCAL_PROVIDER_URL to a /v1 URL. The local adapter "+
			"is ollama-NATIVE now, and an untrimmed /v1 would make every request 404 — an HTTP error, not "+
			"ErrUnavailable, so it trips the unclassified-error raise instead of skipping. NewOllama trims "+
			"it (criterion 5), but the runbook must stop teaching the wrong value", rel)
	}
	if !strings.Contains(lower, "/api/chat") && !strings.Contains(lower, "ollama-native") &&
		!strings.Contains(lower, "native api") {
		t.Errorf("%s does not say the local adapter speaks ollama's native API (criterion 26); a reader "+
			"pointing it at an OpenAI-compatible server would get no `think` field at all", rel)
	}
	required := regexp.MustCompile(`(?is)ops_local_model.{0,300}required|required.{0,300}ops_local_model`)
	if !required.MatchString(lower) {
		t.Errorf("%s does not say OPS_LOCAL_MODEL is REQUIRED when OPS_LOCAL_PROVIDER_URL is set "+
			"(criterion 26). The old fallback sent the hosted model name to ollama, which is a 404 per "+
			"message; the new behaviour is an ABSENT lane, and an operator has to know which one they are "+
			"looking at", rel)
	}
	// Whitespace-COLLAPSED before matching. The first version of this test grepped
	// the raw text for the literal phrase, and the runbook wrapped between
	// "adapter" and "exists" — so the sentence it was written to catch sat in the
	// file untouched while the test passed. A guard that cannot match its own
	// target is worse than no guard: it reports the prose as corrected.
	collapsed := regexp.MustCompile(`\s+`).ReplaceAllString(lower, " ")
	if strings.Contains(collapsed, "no local adapter exists yet") {
		t.Errorf("%s still says 'no local adapter exists yet'. SWT-22 ships one, so that sentence is now "+
			"false — and it is the sentence that tells an operator an all-skipped report is fine. Left "+
			"stale, it turns a real outage into an expected state (criterion 26)", rel)
	}
}

// ---- data model: SWT-25 adds exactly ONE migration, 0017 ----------------------

// REPLACES SWT-22's TestNoMigrationWasAdded, which asserted the highest
// migration stays 0016. That guard was CORRECT for SWT-22 and is wrong for
// SWT-25, and criterion 26 says what to do about it: rewrite it to the new
// truth, do NOT delete it. A guard that becomes wrong and gets deleted is how
// the next ticket adds a migration nobody notices — and the migrate runner keys
// on schema_migrations.version with NO checksum, so a stray or edited file is
// skipped silently and the schema diverges with no error anywhere.
func TestMigration0017_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	// Control first: 0016 must be there, or a glob returning nothing below would
	// prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0016_provider_locality.sql")); err != nil {
		t.Fatalf("migrations/0016_provider_locality.sql is missing: %v", err)
	}

	const rel = "migrations/0017_normalized_message_links.sql"
	if _, err := os.Stat(filepath.Join("..", "..", rel)); err != nil {
		t.Fatalf("%s does not exist: %v. SWT-25 criterion 12 adds exactly one forward-only migration, and "+
			"0017 is its number (0016_provider_locality.sql is the current highest). Merging a migration is "+
			"not applying it — check `SELECT max(version) FROM schema_migrations` before deploying", rel, err)
	}
	sql := strings.ToLower(csRepoFile(t, rel))
	if !regexp.MustCompile(`(?s)alter\s+table\s+normalized_messages.{0,120}add\s+column\s+links`).MatchString(sql) {
		t.Errorf("%s does not ALTER TABLE normalized_messages ADD COLUMN links. The links belong on the "+
			"canonical normalized_messages row (invariant 2: no new table, no new vocabulary) — "+
			"normalized_events.attendees JSONB NOT NULL DEFAULT '[]' is the precedent", rel)
	}
	if !strings.Contains(sql, "jsonb") || !strings.Contains(sql, "default '[]'") {
		t.Errorf("%s does not declare the column JSONB NOT NULL DEFAULT '[]'::jsonb. The default is what "+
			"keeps the upwork / jira / slackweb inserts — which name no links column — working", rel)
	}
	if !strings.Contains(sql, "jsonb_typeof(links) = 'array'") {
		t.Errorf("%s has no CHECK (jsonb_typeof(links) = 'array'). The model answers with a 1-based "+
			"POSITION into this value; something that is not an array has no positions", rel)
	}
	if strings.Contains(sql, "drop column") || regexp.MustCompile(`(?s)--\s*down`).MatchString(sql) {
		t.Errorf("%s looks like it carries a down migration. Migrations here are FORWARD-ONLY, no exceptions", rel)
	}

	// And nothing ABOVE 0017: one new file, exactly.
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	num := regexp.MustCompile(`^(\d{4})_.*\.sql$`)
	seenAny := false
	for _, e := range entries {
		m := num.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		seenAny = true
		n, _ := strconv.Atoi(m[1])
		// 0019 is SWT-20's (delivery provenance); 0018 is SWT-23's and arrives
		// when ticket-local-classifier merges. Anything else above 0017 is a
		// migration nobody's ticket owns.
		if n > 17 && n != 18 && n != 19 {
			t.Errorf("migrations/%s exists but no ticket's data-model section names it. `ls "+
				"migrations/` must only show files a SPEC accounts for", e.Name())
		}
	}
	if !seenAny {
		t.Fatalf("no numbered migrations found; the scan has nothing to scan")
	}
}

// ---- SWT-25 criterion 24: nothing in internal/classify fetches anything -------

// The other half of criterion 1's scan. internal/classify has no HTTP import
// today and must not grow one: the moment this code makes a request it becomes a
// beacon-follower, and the whole reason `img src` is never extracted is that we
// do not do that. Invariant 4 in its sharper reading — a URL in the corpus is
// never dereferenced, so no outbound request exists to need a delivery row.
//
// It also re-states invariant 1's concrete demand: NOTHING in internal/classify
// may read raw_source_items or decode MIME. The links arrive in a COLUMN, filled
// by the normalizer; a second decoder beside the normalizer is the side door
// invariant 1 exists to prevent, and SWT-22 rejected it explicitly.
func TestClassifyPackage_FetchesNothingAndDecodesNoMIME(t *testing.T) {
	banned := []struct{ token, why string }{
		{`"net/http"`, "criterion 24: nothing fetches a link, anywhere, ever — no HEAD to expand a tracking redirect, no title fetch, no screenshot"},
		{`"net/url"`, "the application resolves an INDEX to a stored URL; it never parses, rewrites or unwraps one"},
		{`"mime"`, "invariant 1: nothing in internal/classify decodes MIME — that is the normalizer's job and a second decoder is the side door raw-first exists to close"},
		{"raw_source_items", "invariant 1: the classifier reads normalized rows and the links COLUMN, never raw"},
	}
	for _, rel := range csSources(t, "internal/classify") {
		code := csGoCode(t, rel)
		for _, b := range banned {
			if strings.Contains(code, b.token) {
				t.Errorf("%s mentions %q in code (comments stripped) — %s", rel, b.token, b.why)
			}
		}
	}
}

// ---- SWT-25 criterion 26: the five statements this change makes false ---------

// "A comment that states the opposite of its code" is a defect this repo has
// shipped twice (SWT-21, in a boundary file, where the comment is what the next
// reader trusts). Criterion 26 names five statements that become false the
// moment link_index exists and requires all of them fixed in the SAME change.
// Three are mechanical and this test owns them; the fourth is
// TestSchema_MatchesTheOutputContract above (already updated), and the fifth is
// TestMigration0017_IsTheOnlyOneThisTicketAdds.
func TestContradictions_AreFixedInTheSameChange(t *testing.T) {
	t.Run("prompt.go no longer says four fields", func(t *testing.T) {
		src := csRepoFile(t, "internal/classify/prompt.go")
		lower := strings.ToLower(src)
		four := regexp.MustCompile(`four fields`)
		if four.MatchString(lower) {
			t.Errorf("internal/classify/prompt.go still says \"four fields, nothing else\". The contract is " +
				"FIVE now: {actionable, kind, title, reason, link_index}. Leaving the sentence makes the " +
				"file's own documentation contradict the schema three lines below it")
		}
		if !strings.Contains(lower, "five fields") {
			t.Errorf("internal/classify/prompt.go does not say the contract is five fields. Criterion 26 " +
				"asks for the correction, not the deletion: the sentence is what tells the next reader the " +
				"schema is deliberately small")
		}
		if !strings.Contains(lower, "link_index") || !regexp.MustCompile(`index into|never a url|not a url`).MatchString(lower) {
			t.Errorf("internal/classify/prompt.go does not note that link_index is an INDEX into " +
				"normalized_messages.links and never a URL. That one line is the whole architectural claim " +
				"of this ticket, sitting where someone editing the schema will read it")
		}
	})

	t.Run("the SWT-22 SPEC is amended in place", func(t *testing.T) {
		const rel = "docs/tickets/local-classifier_SPEC.md"
		spec := csRepoFile(t, rel)
		if !strings.Contains(spec, "SWT-25") {
			t.Errorf("%s never mentions SWT-25. Criterion 26: criterion 18's four-field statement is amended "+
				"IN PLACE with a dated pointer, and 19b's \"DESCOPED to SWT-25\" gains \"DELIVERED by "+
				"SWT-25 (date)\" — the rest of 19b stays, because it is the record of WHY and it is still "+
				"true", rel)
		}
		if !regexp.MustCompile(`(?i)delivered by swt-25`).MatchString(spec) {
			t.Errorf("%s's criterion 19b does not record that SWT-25 DELIVERED it. A descope note with no "+
				"closing line is a ticket that reads as still open forever", rel)
		}
		// The four-field sentence in criterion 18 must carry its correction next
		// to it, not be left standing alone.
		c18 := regexp.MustCompile(`(?s)reason: string\}`)
		if c18.MatchString(spec) && !regexp.MustCompile(`(?s)reason: string\}.{0,600}SWT-25`).MatchString(spec) {
			t.Errorf("%s still states the four-field output contract with no pointer to SWT-25 beside it. "+
				"The two statements must not coexist unqualified: whichever a reader finds first is the one "+
				"they will believe", rel)
		}
	})
}

// ---- SWT-25 criterion 27: the runbook's Links section --------------------------

// A test on prose, and it earns it the same way TestRunbook_LocalClassifier does.
// Every rule here is one a future session would "clean up": zero candidates looks
// like a bug, `img src` refusal looks like an oversight, and the backfill command
// is the only thing standing between "links work" and "links work for mail that
// arrives from now on".
func TestRunbook_DocumentsLinkPreservation(t *testing.T) {
	doc := strings.ToLower(csRepoFile(t, "docs/runbooks/local-classifier.md"))

	if !strings.Contains(doc, "link_index") {
		t.Errorf("the runbook never mentions link_index. Criterion 27: the Links section explains why the " +
			"model returns an INDEX and never a URL — the one thing a reader must not 'improve' into a " +
			"string field")
	}
	if !strings.Contains(doc, "img") {
		t.Errorf("the runbook does not say that `img src` is never extracted and never followed. Name the " +
			"/wf/open SendGrid pixel: without the example the rule reads as fussiness, and the next person " +
			"widening the extractor 'to catch the Pines link' adds a tracking beacon to a task")
	}
	if !strings.Contains(doc, "normalize-only") {
		t.Errorf("the runbook does not give the backfill command (`--normalize-only --all`). The historical " +
			"corpus is the point: without the backfill only mail that arrives after the deploy has links, " +
			"and the eval re-run measures the wrong thing")
	}
	idempotent := regexp.MustCompile(`idempotent|raw_source_item_id|upsert`)
	if !idempotent.MatchString(doc) {
		t.Errorf("the runbook does not say the backfill is IDEMPOTENT (the upsert keys on " +
			"raw_source_item_id, so message ids — and therefore capture_decisions, ai_extractions and the " +
			"eval labels — survive it). An operator who does not know that will not run it")
	}
	null := regexp.MustCompile(`null[^.\n]{0,80}(ordinary|normal|common|expected)|` +
		`(ordinary|normal|common|expected)[^.\n]{0,80}null`)
	if !null.MatchString(doc) {
		t.Errorf("the runbook does not say that link_index: null is ORDINARY. The two HOA First Notices " +
			"have no usable link at all — only the tracking pixel — and a reader who reads null as a " +
			"failure will go looking for a broken extractor")
	}
	if !regexp.MustCompile(`links\.go|drop.?list`).MatchString(doc) {
		t.Errorf("the runbook does not say how to add a drop-list entry (edit the list in links.go, re-run " +
			"--normalize-only --all, re-run the eval). The lists live in Go rather than a table precisely " +
			"because that loop is the workflow")
	}
	if !regexp.MustCompile(`body_text|normalize time`).MatchString(doc) {
		t.Errorf("the runbook does not say where links come from (normalize time, raw -> the links column) " +
			"and why NOT from body_text. body_text carries no URL at all in 837 of 1,613 personal messages: " +
			"that measurement is the reason this ticket exists and it belongs in the runbook")
	}
}

// ---- SWT-25 criterion 28: the institutional-knowledge entry --------------------

// This file is what the next session reads instead of re-deriving the landmines.
// The entry is short by design; the test pins the four facts that cost something
// to rediscover.
func TestInstitutionalKnowledge_RecordsLinkPreservation(t *testing.T) {
	doc := csRepoFile(t, ".claude/INSTITUTIONAL_KNOWLEDGE.md")
	lower := strings.ToLower(doc)

	if !strings.Contains(doc, "SWT-25") {
		t.Fatalf(".claude/INSTITUTIONAL_KNOWLEDGE.md has no SWT-25 entry. Criterion 28 asks for a short " +
			"'Link preservation (SWT-25)' section: agents read this file at session start instead of " +
			"re-deriving what it holds")
	}
	for _, want := range []struct{ token, why string }{
		{"links", "the column, on normalized_messages"},
		{"position", "the array POSITION is the identity — nothing may reorder it after write"},
		{"link_index", "the index contract: the model answers with a number, the application resolves it"},
		{"img", "the img-src refusal and why (the /wf/open tracking pixel)"},
		{"normalize-only", "the backfill command, and that it is idempotent because the upsert keys on raw_source_item_id"},
		{"body_text", "the standing rule: body_text must never change without checking confirmDeliveryByBodyPrefix"},
	} {
		if !strings.Contains(lower, strings.ToLower(want.token)) {
			t.Errorf("the SWT-25 entry does not mention %q — %s", want.token, want.why)
		}
	}
}

// ---- SWT-25 criterion 25: the eval re-run is recorded as a SECOND ROW ----------

// The eval RUN is a manual verification step (a live local model over 280
// labels); what a test can hold is the RECORD. Criterion 25 asks for a second
// row in the runbook's table beside the 2026-08-30 baseline of 0.83 / 0.58, and
// a note saying what happened to the two content-behind-a-link false negatives
// the runbook already names — 27871 and 84710 — INCLUDING if they did not move.
//
// "A drop in recall is a result, not a failure to hide": the candidate list is a
// prompt change, and prompt changes have measured costs in this corpus.
func TestRunbook_RecordsTheSWT25EvalRerun(t *testing.T) {
	doc := csRepoFile(t, "docs/runbooks/local-classifier.md")

	// Control: the baseline row must still be there. If it is gone, the "second
	// row" assertion below would be satisfied by a rewritten table, which is
	// exactly the thing that makes a before/after unreadable.
	if !strings.Contains(doc, "2026-08-30") {
		t.Fatalf("the 2026-08-30 baseline row (0.83 / 0.58) is missing from the runbook table. The new row " +
			"goes ALONGSIDE it — the point of the table is the delta, and a table with one row measures " +
			"nothing")
	}
	rows := regexp.MustCompile(`(?m)^\|\s*20\d\d-\d\d-\d\d\s*\|`).FindAllString(doc, -1)
	if len(rows) < 2 {
		t.Errorf("the runbook's score table has %d dated row(s), want at least 2: criterion 25 re-runs the "+
			"SWT-22 eval UNCHANGED — same 280 labels, same file, same command — and records n / recall / "+
			"precision / median latency / date as a second row", len(rows))
	}
	for _, id := range []string{"27871", "84710"} {
		if !strings.Contains(doc, id) {
			t.Errorf("the runbook does not mention message %s. Criterion 25: the note under the new row "+
				"names what moved for the two content-behind-a-link false negatives — a doctor's-office "+
				"portal message and a portal notice built from an unfilled template — and says plainly if "+
				"they did NOT move. They are the two cases this ticket was supposed to help", id)
		}
	}
	if !regexp.MustCompile(`(?i)drift`).MatchString(doc) {
		t.Errorf("the runbook does not mention label drift for the re-run. Expected drift exclusions are " +
			"ZERO, because re-normalization UPSERTS (ids and subjects are stable) — so any drift the " +
			"harness prints is a finding to investigate, not a nuisance to re-hash")
	}
}
