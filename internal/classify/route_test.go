package classify_test

// SWT-40 Part B (docs/tickets/inquiry-promote_SPEC.md, B-D3, B-D4, B-D8;
// criteria B1 and B3) — the fourth classify lane, `route`, and its output
// contract. Fake Store + fake provider.Client: ZERO network, ZERO Postgres,
// ZERO live model. The inbox SQL half (criterion B2) is in
// route_integration_test.go, because every one of its clauses turns on a value
// Postgres produces.
//
// ---- IMPOSED SURFACE (internal/classify/route.go; the SPEC's B1/B3 names) ----
//
//	const RoutePromptVersion = "route-v1"
//	const RouteSchemaName    = "route_verdict"
//	var   RouteVerdictSchema json.RawMessage
//	      // {project_index: integer|null, evidence: string, reason: string},
//	      // all three required, additionalProperties:false. NO confidence, NO
//	      // url, NO link* (B-D4, B1).
//	const RouteSystemPrompt = `…` // ONE bilingual prompt, no sender or client literal
//	var   RouteContract = Contract{SchemaName: RouteSchemaName, Schema: RouteVerdictSchema, …}
//	var   LaneRoute = Lane{Name: "route", WorkerType: "classify_route",
//	          System: RouteSystemPrompt, PromptVersion: RoutePromptVersion,
//	          LabelsPath: "docs/evals/route-from-rules.jsonl", Contract: RouteContract}
//	      // LaneByName("route") and LaneByWorkerType("classify_route") resolve it.
//
//	// One row of the closed candidate set (B-D1), numbered 1-based in the prompt.
//	type RouteCandidate struct {
//	    ProjectID   int64
//	    Slug, Name, Client, Description string // the row's description is source_account_projects.description
//	    IsDefault   bool
//	}
//	// The ONE conversion from project_index to a candidate, ResolveLink's shape:
//	// nil, 0, negative or past the end → (zero, false).
//	func ResolveCandidate(cands []RouteCandidate, idx *int) (RouteCandidate, bool)
//	// The grounding gate that replaces confidence (B-D4): evidence, whitespace-
//	// collapsed and case-folded, is a substring of the sender, the subject or
//	// the body — each field on its own, same collapse and fold. Empty evidence
//	// grounds nothing.
//	func Grounded(evidence, sender, subject, body string) bool
//
//	// PendingMessage gains two fields, filled by the route inbox only:
//	    SourceAccountID int64            // raw_source_items.source_account_id (B-D9's carve-out)
//	    Candidates      []RouteCandidate // the account's source_account_projects rows
//
// What a route verdict RECORDS in ai_extractions.fields (the keys capture's
// route_apply reads back — see internal/capture/route_integration_test.go):
//
//	project_index          the model's answer, verbatim (number or null)
//	project_id             the RESOLVED candidate's project id; null when the
//	                       index is null, 0 or out of range
//	grounded               true iff a candidate was resolved AND Grounded(evidence, …)
//	evidence, reason       verbatim
//	normalized_message_id, source_account_id
//
// Grounding is decided HERE, at classify time, the way ResolveLink resolves
// link_url at classify time: route_apply never re-reads a body, and capture
// never imports this package for it.
//
// GREENFIELD NOTE — EXPECTED RED: none of the names above exist, so the
// classify_test package compile-FAILS ("undefined: classify.LaneRoute").

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- fixtures -----------------------------------------------------------------

// rtCandidates is the handsonconnect seed's SHAPE (O2/O3): two candidates, the
// first one the default. Slugs are test-owned so no prompt assertion can be
// satisfied by a production project name leaking into the prompt.
func rtCandidates() []classify.RouteCandidate {
	return []classify.RouteCandidate{
		{ProjectID: 41, Slug: "itest-alpha", Name: "Alpha Portal", Client: "Alpha Org",
			Description: "university partner integrations and activity sync", IsDefault: true},
		{ProjectID: 42, Slug: "itest-beta", Name: "Beta Engine", Client: "Beta Org",
			Description: "the beta engine rebuild and its data pipeline"},
	}
}

// rtMessages builds messages in the shape the ROUTE inbox yields: inbound,
// latest decision unmatched, so NO project (0015's CHECK), Attribution
// AttrUnmatched — which is what makes ClassOf restrict them (B-D8).
func rtMessages(n int) []classify.PendingMessage {
	out := make([]classify.PendingMessage, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, classify.PendingMessage{
			MessageID:       int64(i),
			RawSourceItemID: int64(3000 + i),
			ThreadID:        int64(i),
			SentAt:          time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
			Sender:          "Pat Doe <pat@univ.example.test>",
			Subject:         "Questions about the Beta Engine activity feed",
			Channel:         "gmail",
			BodyText:        "Hi, can you confirm the   Beta Engine\nfeed timeline before Friday?",
			Direction:       "inbound",
			Attribution:     provider.AttrUnmatched,
			SourceAccountID: 1009,
			Candidates:      rtCandidates(),
		})
	}
	return out
}

func rtCfg() classify.Config {
	return classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LaneRoute, Since: 720 * time.Hour}
}

func rtVerdict(index, evidence string) string {
	return fmt.Sprintf(`{"project_index":%s,"evidence":%q,"reason":"itest reason"}`, index, evidence)
}

func rtIntp(i int) *int { return &i }

// ---- B-D3 / B1: the lane --------------------------------------------------------

func TestLaneRoute_Values(t *testing.T) {
	l := classify.LaneRoute
	if l.Name != "route" {
		t.Errorf("LaneRoute.Name = %q, want \"route\" (B1; `--lane route` matches it)", l.Name)
	}
	if l.WorkerType != "classify_route" {
		t.Errorf("LaneRoute.WorkerType = %q, want \"classify_route\" (B-D3). Every inbox keys its NOT EXISTS on "+
			"worker_type: a shared value would hide a message classified by one lane from another, forever", l.WorkerType)
	}
	if classify.RoutePromptVersion != "route-v1" || l.PromptVersion != classify.RoutePromptVersion {
		t.Errorf("RoutePromptVersion = %q, LaneRoute.PromptVersion = %q; want route-v1 for both (B1)",
			classify.RoutePromptVersion, l.PromptVersion)
	}
	if l.System != classify.RouteSystemPrompt {
		t.Errorf("LaneRoute.System is not classify.RouteSystemPrompt")
	}
	if l.LabelsPath != "docs/evals/route-from-rules.jsonl" {
		t.Errorf("LaneRoute.LabelsPath = %q, want docs/evals/route-from-rules.jsonl (B10's label file)", l.LabelsPath)
	}
	if !reflect.DeepEqual(l.Contract, classify.RouteContract) {
		t.Errorf("LaneRoute.Contract is not classify.RouteContract")
	}
	if classify.RouteContract.SchemaName != classify.RouteSchemaName || classify.RouteSchemaName != "route_verdict" {
		t.Errorf("RouteContract.SchemaName = %q, RouteSchemaName = %q; want route_verdict for both",
			classify.RouteContract.SchemaName, classify.RouteSchemaName)
	}
	if string(classify.RouteContract.Schema) != string(classify.RouteVerdictSchema) {
		t.Errorf("RouteContract.Schema is not RouteVerdictSchema verbatim")
	}
	for _, other := range []classify.Lane{classify.LanePersonal, classify.LaneResidue, classify.LaneInquiry} {
		if other.WorkerType == l.WorkerType || other.PromptVersion == l.PromptVersion || other.System == l.System {
			t.Errorf("LaneRoute shares a worker_type, prompt version or prompt with %q (B-D3: its own lane)", other.Name)
		}
		if reflect.DeepEqual(other.Contract, l.Contract) {
			t.Errorf("LaneRoute shares %q's contract. B-D3: residue's contract is pinned equal to personal's "+
				"(SWT-23) and the inquiry lane asks a different question — routing gets its own", other.Name)
		}
	}
}

func TestLaneByName_And_ByWorkerType_ResolveTheRouteLane(t *testing.T) {
	got, err := classify.LaneByName("route")
	if err != nil || got.Name != "route" {
		t.Errorf("LaneByName(\"route\") = %+v, %v; `classify run|report|eval --lane route` needs it", got.Name, err)
	}
	byWT, ok := classify.LaneByWorkerType("classify_route")
	if !ok || byWT.Name != "route" {
		t.Errorf("LaneByWorkerType(\"classify_route\") = %q, %v; Summarize and /funnel resolve lanes by worker_type",
			byWT.Name, ok)
	}
	if _, err := classify.LaneByName("Route"); err == nil {
		t.Errorf("LaneByName(\"Route\") succeeded; a lane name is matched exactly, never folded")
	}
}

// ---- B-D4 / B1: the output contract, no confidence -----------------------------

func TestRouteSchema_IsTheBDFourContract(t *testing.T) {
	var schema struct {
		Type                 string                     `json:"type"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(classify.RouteVerdictSchema, &schema); err != nil {
		t.Fatalf("classify.RouteVerdictSchema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("type = %q, want object", schema.Type)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Errorf("additionalProperties is not false; a model that invents a field (confidence, a url) would have it stored")
	}
	want := map[string][]string{
		"project_index": {"integer", "null"},
		"evidence":      {"string"},
		"reason":        {"string"},
	}
	for name, types := range want {
		raw, ok := schema.Properties[name]
		if !ok {
			t.Errorf("schema has no %q property; B-D4's contract is {project_index: integer|null, evidence: string, "+
				"reason: string}", name)
			continue
		}
		if got := csTypeSet(t, name, liType(t, raw)); !reflect.DeepEqual(got, types) {
			t.Errorf("%s type = %v, want %v (null is how the model says 'none of these': ORDINARY output)", name, got, types)
		}
	}
	for name := range schema.Properties {
		if _, ok := want[name]; !ok {
			t.Errorf("schema has an extra property %q; the route contract is exactly three fields", name)
		}
	}
	req := map[string]bool{}
	for _, r := range schema.Required {
		req[r] = true
	}
	for name := range want {
		if !req[name] {
			t.Errorf("schema does not require %q", name)
		}
	}

	// B1: no confidence, no url, no link* — anywhere in the document.
	banned := regexp.MustCompile(`(?i)^(confidence|url|uri|href|link.*)$`)
	var whole any
	_ = json.Unmarshal(classify.RouteVerdictSchema, &whole)
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if banned.MatchString(k) {
					t.Errorf("the route schema declares %q at %s. B-D4: a self-reported confidence cannot be used "+
						"(qwen3:8b returns a constant) — the GROUNDING gate replaces it; and the model never authors a "+
						"url or a link (B1)", k, path)
				}
				if strings.EqualFold(k, "format") {
					if s, ok := child.(string); ok && regexp.MustCompile(`(?i)ur[il]`).MatchString(s) {
						t.Errorf("the route schema declares format %q at %s", s, path)
					}
				}
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		case string:
			if banned.MatchString(v) {
				t.Errorf("the route schema names %q at %s (a required-list or enum entry)", v, path)
			}
		}
	}
	walk(whole, "$")
}

// B1: ONE bilingual prompt, and it names no sender and no client. The candidate
// rows arrive in the USER half, per message, from source_account_projects; a
// project name baked into the system prompt is a rule in a costume and would
// outlive the row that justified it.
func TestRoutePrompt_BilingualAndNamesNoSenderOrClient(t *testing.T) {
	p := classify.RouteSystemPrompt
	if len(p) < 300 {
		t.Fatalf("RouteSystemPrompt is %d characters; the prompt is the work", len(p))
	}
	lower := strings.ToLower(p)
	if !strings.Contains(lower, "english") || !strings.Contains(lower, "spanish") {
		t.Errorf("the route prompt is not bilingual: English and Spanish are treated identically (B1)")
	}
	for _, lit := range []string{"collaboratory", "reengine", "re-engine", "handsonconnect", "rochester",
		"llamasite", "avviato", "treetop", "lhh"} {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(lit) + `\b`).MatchString(lower) {
			t.Errorf("the route prompt names %q. B1: no sender or client literal — the candidates come from the "+
				"account's rows, numbered in the user half", lit)
		}
	}
	if strings.Contains(p, "@") {
		t.Errorf("the route prompt contains an '@': a sender address is a sender literal (B1)")
	}
	for _, want := range []string{"project_index", "evidence", "null"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the route prompt never mentions %q; the model must know what each field means", want)
		}
	}
	if !regexp.MustCompile(`verbatim|exact|quote`).MatchString(lower) {
		t.Errorf("the route prompt does not ask for the evidence to be QUOTED from the message. B-D4: a choice " +
			"counts only if the evidence is a substring of the sender, subject or body — a paraphrase is ungrounded")
	}
}

// ---- B3: ResolveCandidate ------------------------------------------------------

func TestResolveCandidate(t *testing.T) {
	c := rtCandidates()
	for _, tc := range []struct {
		name   string
		idx    *int
		ok     bool
		wantID int64
	}{
		{"null", nil, false, 0},
		{"zero (the list is 1-based)", rtIntp(0), false, 0},
		{"negative", rtIntp(-1), false, 0},
		{"out of range", rtIntp(3), false, 0},
		{"first", rtIntp(1), true, 41},
		{"last", rtIntp(2), true, 42},
	} {
		got, ok := classify.ResolveCandidate(c, tc.idx)
		if ok != tc.ok || got.ProjectID != tc.wantID {
			t.Errorf("%s: ResolveCandidate = (%+v, %v), want (project %d, %v)", tc.name, got, ok, tc.wantID, tc.ok)
		}
	}
	if got, ok := classify.ResolveCandidate(nil, rtIntp(1)); ok || got.ProjectID != 0 {
		t.Errorf("ResolveCandidate(empty list, 1) = (%+v, %v); any index into an empty list is rejected", got, ok)
	}
}

// ---- B3: Grounded --------------------------------------------------------------

func TestGrounded(t *testing.T) {
	sender := "Pat Doe <pat@univ.example.test>"
	subject := "Questions about the Beta Engine activity feed"
	body := "Hi, can you confirm the   Beta Engine\nfeed timeline before Friday?"
	for _, tc := range []struct {
		name     string
		evidence string
		want     bool
	}{
		{"verbatim span with different spacing and case passes", "beta engine feed   TIMELINE", true},
		{"a span of the subject passes", "the Beta Engine activity feed", true},
		{"a span of the sender passes", "pat@univ.example.test", true},
		{"a no-break space collapses like any whitespace", "Beta\u00a0Engine feed", true},
		{"a paraphrase fails", "the beta engine schedule question", false},
		{"a claim about the message fails", "the sender works on beta", false},
		{"empty evidence grounds nothing", "", false},
		{"whitespace-only evidence grounds nothing", "  \n\t ", false},
		{"a span straddling subject and body is not a substring of either", "activity feed Hi, can you", false},
	} {
		if got := classify.Grounded(tc.evidence, sender, subject, body); got != tc.want {
			t.Errorf("%s: Grounded(%q) = %v, want %v (B-D4: whitespace-collapsed, case-folded substring of the "+
				"sender, subject or body)", tc.name, tc.evidence, got, tc.want)
		}
	}
}

// ---- B2 (unit half): --since is required ---------------------------------------

func TestRun_RouteLane_RefusesAnUnboundedPass(t *testing.T) {
	local := cfLocal()
	store := &lnStore{cfStore: cfStore{pending: rtMessages(2)}}
	cfg := rtCfg()
	cfg.Since = 0
	_, err := classify.Run(context.Background(), store, provider.NewRouter(nil, local, time.Minute), cfg)
	if err == nil {
		t.Fatalf("Run accepted an unbounded route pass; B2: --since is required on the route lane")
	}
	if !strings.Contains(err.Error(), "--since") {
		t.Errorf("the refusal does not tell the operator what to pass: %v", err)
	}
	if store.pendingCalls != 0 || local.calls != 0 {
		t.Errorf("the refused pass read the inbox %d time(s) and called the model %d time(s); it must do neither",
			store.pendingCalls, local.calls)
	}
}

// ---- B-D3 / B-D4: what a route pass sends and what it records -----------------

func TestRun_RouteLane_SendsTheRouteContractAndTheNumberedCandidates(t *testing.T) {
	local := cfLocal()
	local.verdict = rtVerdict("2", "Beta Engine feed timeline")
	msgs := rtMessages(1)
	// A prior-context sentinel: the route prompt carries ONLY the message and the
	// candidate rows (B-D8), so thread context must never reach it.
	msgs[0].ThreadContext = []classify.ThreadMessage{{MessageID: 99, Direction: "inbound", BodyText: "PRIOR-CONTEXT-SENTINEL"}}
	store := &lnStore{cfStore: cfStore{pending: msgs}}

	stats, err := classify.Run(context.Background(), store, provider.NewRouter(nil, local, time.Minute), rtCfg())
	if err != nil {
		t.Fatalf("Run(route): %v", err)
	}
	if stats.Processed != 1 || len(local.requests) != 1 {
		t.Fatalf("stats = %+v, requests = %d; want one verdict", stats, len(local.requests))
	}
	req := local.requests[0]
	if req.System != classify.RouteSystemPrompt {
		t.Errorf("Request.System is not RouteSystemPrompt")
	}
	if req.SchemaName != classify.RouteSchemaName || string(req.Schema) != string(classify.RouteVerdictSchema) {
		t.Errorf("Request schema = %q/%s, want the route contract", req.SchemaName, req.Schema)
	}
	if strings.Contains(req.User, "PRIOR-CONTEXT-SENTINEL") {
		t.Errorf("the route prompt carries thread context; B-D8: only the message and the candidate rows")
	}
	lines := strings.Split(req.User, "\n")
	for i, c := range rtCandidates() {
		num := fmt.Sprintf("%d.", i+1)
		found := false
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, num) && strings.Contains(ln, c.Slug) && strings.Contains(ln, c.Name) &&
				strings.Contains(ln, c.Description) {
				found = true
			}
		}
		if !found {
			t.Errorf("the user prompt has no line %q naming candidate %s (slug, name and description on one numbered "+
				"line, B-D4):\n%s", num, c.Slug, req.User)
		}
	}
	if !strings.Contains(req.User, "Beta Engine") {
		t.Errorf("the user prompt does not carry the message itself")
	}

	oks := store.withStatus("ok")
	if len(oks) != 1 || oks[0].WorkerType != "classify_route" {
		t.Fatalf("recorded runs = %+v, want one ok run under worker_type classify_route", oks)
	}
	in := cfDecode(t, oks[0].Input)
	if in["prompt_version"] != classify.RoutePromptVersion {
		t.Errorf("ai_runs.input.prompt_version = %v, want %q", in["prompt_version"], classify.RoutePromptVersion)
	}
}

// What the verdict row records is what route_apply decides from, so each case
// asserts the RESOLVED project and the grounding bit, not just the model's words.
func TestRun_RouteLane_RecordsTheResolvedCandidateAndTheGroundingBit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		verdict      string
		wantProject  any // float64 id or nil
		wantGrounded bool
	}{
		{"grounded choice", rtVerdict("2", "beta   engine FEED timeline"), float64(42), true},
		{"ungrounded choice keeps the choice, not the grounding", rtVerdict("2", "they mean the engine rebuild"), float64(42), false},
		{"null index: nothing chosen, nothing grounded", rtVerdict("null", "Beta Engine"), nil, false},
		{"index 0: rejected", rtVerdict("0", "Beta Engine"), nil, false},
		{"index past the end: rejected", rtVerdict("7", "Beta Engine"), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := cfLocal()
			local.verdict = tc.verdict
			store := &lnStore{cfStore: cfStore{pending: rtMessages(1)}}
			if _, err := classify.Run(context.Background(), store, provider.NewRouter(nil, local, time.Minute), rtCfg()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(store.extractions) != 1 {
				t.Fatalf("extractions = %d, want 1 — every classified message writes one, whatever it chose", len(store.extractions))
			}
			var f map[string]any
			if err := json.Unmarshal(store.extractions[0].fields, &f); err != nil {
				t.Fatalf("fields: %v", err)
			}
			for _, k := range []string{"project_index", "project_id", "grounded", "evidence", "reason",
				"normalized_message_id", "source_account_id"} {
				if _, ok := f[k]; !ok {
					t.Errorf("ai_extractions.fields has no %q key: %s", k, store.extractions[0].fields)
				}
			}
			if !reflect.DeepEqual(f["project_id"], tc.wantProject) {
				t.Errorf("fields.project_id = %v, want %v — the id ResolveCandidate produced, never a model string",
					f["project_id"], tc.wantProject)
			}
			if g, _ := f["grounded"].(bool); g != tc.wantGrounded {
				t.Errorf("fields.grounded = %v, want %v", f["grounded"], tc.wantGrounded)
			}
			if f["source_account_id"] != float64(1009) || f["normalized_message_id"] != float64(1) {
				t.Errorf("fields ids = account %v / message %v, want 1009 / 1", f["source_account_id"], f["normalized_message_id"])
			}
		})
	}
}

// ---- B-D8: local-only. Unmatched is restricted through ClassOf ----------------

func TestRun_RouteLane_NeverReachesTheHostedClient(t *testing.T) {
	t.Run("with a local lane: classified locally, zero hosted calls", func(t *testing.T) {
		general, local := cfHosted(), cfLocal()
		local.verdict = rtVerdict("1", "Beta Engine")
		store := &lnStore{cfStore: cfStore{pending: rtMessages(3)}}
		if _, err := classify.Run(context.Background(), store, provider.NewRouter(general, local, time.Minute), rtCfg()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if general.calls != 0 {
			t.Errorf("the hosted client saw %d call(s). B-D8: an unmatched message is ClassRestricted through ClassOf; "+
				"the route lane is local-only", general.calls)
		}
		if local.calls != 3 {
			t.Errorf("local calls = %d, want 3 (the control: these messages CAN be classified)", local.calls)
		}
	})
	t.Run("no local lane: nothing is sent anywhere, nothing is recorded as a verdict", func(t *testing.T) {
		general := cfHosted()
		store := &lnStore{cfStore: cfStore{pending: rtMessages(3)}}
		stats, err := classify.Run(context.Background(), store, provider.NewRouter(general, nil, time.Minute), rtCfg())
		if err != nil {
			t.Fatalf("a fully skipped route pass errored: %v (a refusal is the boundary working)", err)
		}
		if general.calls != 0 || len(store.extractions) != 0 || stats.Skipped != 3 {
			t.Errorf("hosted calls = %d, extractions = %d, stats = %+v; want 0 / 0 / 3 skipped — a missing verdict "+
				"leaves the message pending_verdict, never defaulted (B-D2)", general.calls, len(store.extractions), stats)
		}
	})
}
