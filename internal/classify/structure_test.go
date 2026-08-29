package classify_test

// Structural tests for SWT-22 — the criteria this SPEC asks to be enforced
// mechanically rather than by review: 10 (the advisory-lock key and its
// collision scan), 13 (the honesty label), 18 (NO confidence field, and the
// output contract), 19 (one prompt for all senders), 20 (the closing note both
// reports must carry), 21/24 (the labelled set's shape), 25 and 26 (the two
// runbooks), and the data-model claim that this ticket adds no migration.
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
	"regexp"
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
func TestSchema_MatchesTheOutputContract(t *testing.T) {
	var schema struct {
		Type                 string   `json:"type"`
		AdditionalProperties *bool    `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
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
	want := map[string]string{
		"actionable": "boolean",
		"kind":       "string",
		"title":      "string",
		"reason":     "string",
	}
	for name, typ := range want {
		p, ok := schema.Properties[name]
		if !ok {
			t.Errorf("schema has no %q property; criterion 18's contract is "+
				"{actionable, kind, title, reason} and nothing else", name)
			continue
		}
		if p.Type != typ {
			t.Errorf("schema property %q has type %q, want %q", name, p.Type, typ)
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

// ---- data model: this ticket adds no migration --------------------------------

func TestNoMigrationWasAdded(t *testing.T) {
	// Control first: 0016 must be there, or a glob returning nothing below would
	// prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0016_provider_locality.sql")); err != nil {
		t.Fatalf("migrations/0016_provider_locality.sql is missing: %v", err)
	}
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
		if n > 16 {
			t.Errorf("migrations/%s exists. The SPEC's data-model section is \"None\": the highest applied "+
				"migration stays 0016, and `go run ./cmd/tools/migrate` must be a no-op. Verdicts are rows in "+
				"the existing ai_runs (worker_type='classify') + ai_extractions, discriminated exactly as "+
				"plan_import's are — a classified_messages or personal_alerts table would be invariant 2's "+
				"violation", e.Name())
		}
	}
	if !seenAny {
		t.Fatalf("no numbered migrations found; the scan has nothing to scan")
	}
}
