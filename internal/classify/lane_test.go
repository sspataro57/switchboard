package classify_test

// SWT-23 criteria 10, 14 (unit half), 16, 21, 22, 23 and 24, plus invariant 4's
// pin for the new lane. Fake Store + fake provider.Client — ZERO network, ZERO
// Postgres, ZERO live model. The SQL half (criteria 11, 13 and 17) is in
// residue_integration_test.go, deliberately: any assertion about a value that
// comes from a database COLUMN belongs in a test that makes Postgres produce it
// (SWT-21's 6th landmine instance).
//
// IMPOSED SURFACE, from the SPEC's "Internal Go surface added" block:
//
//	type Lane struct {
//	    Name          string // "personal" | "residue"
//	    WorkerType    string // ai_runs.worker_type
//	    System        string // the system prompt
//	    PromptVersion string
//	    LabelsPath    string // default eval fixture
//	}
//	var LanePersonal, LaneResidue Lane
//	const ResiduePromptVersion = "residue-v1"
//	const ResidueSystemPrompt = `…`
//	type Config struct { …; Lane Lane }   // the ZERO VALUE IS REFUSED, not defaulted
//
// VerdictSchema and SchemaName are NOT lane-scoped — one contract, both lanes —
// and TestSchema_MatchesTheOutputContract (structure_test.go) is untouched. That
// is the assertion that this ticket did not fork the contract.
//
// GREENFIELD NOTE: Lane, LaneResidue, ResidueSystemPrompt and Config.Lane do not
// exist yet, so this file compile-FAILS. Expected red.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/provider"
)

// ---- fixtures ----------------------------------------------------------------

// lnResidueMessages builds messages in the shape the RESIDUE inbox actually
// yields, which is the opposite of cfMessages' in every field that matters:
// there is NO project (0015's CHECK makes `(action='unmatched') = (project_id IS
// NULL)` a schema fact), so ProjectID is 0, ProjectSlug is empty,
// ProjectLocalOnly is false — and Attribution is AttrUnmatched (criterion 14).
//
// A fixture that copied cfMessages and only changed the lane would be asserting
// against a population this inbox cannot contain.
func lnResidueMessages(n int) []classify.PendingMessage {
	out := make([]classify.PendingMessage, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, classify.PendingMessage{
			MessageID:       int64(i),
			RawSourceItemID: int64(2000 + i),
			ThreadID:        int64(i),
			SentAt:          time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Sender:          "jobalerts-noreply@jobs.example.test",
			Subject:         "12 new jobs for you",
			Channel:         "gmail",
			BodyText:        "Recommended jobs based on your profile",
			Direction:       "inbound",
			Attribution:     provider.AttrUnmatched,
		})
	}
	return out
}

func lnResidueCfg() classify.Config {
	return classify.Config{
		Model: "qwen3:8b", MaxTokens: 512,
		Lane:  classify.LaneResidue,
		Since: 720 * time.Hour, // REQUIRED on this lane — criterion 16
	}
}

// lnStore counts reads, so a refusal can be shown to happen BEFORE the inbox is
// queried rather than after.
type lnStore struct {
	cfStore
	pendingCalls int
}

func (s *lnStore) PendingMessages(ctx context.Context, cfg classify.Config) ([]classify.PendingMessage, error) {
	s.pendingCalls++
	return s.cfStore.PendingMessages(ctx, cfg)
}

// ---- criterion 10: there are exactly two lanes -------------------------------

// "Exactly two" is a real constraint rather than tidiness: a third lane means a
// third worker_type, a third prompt nothing measured, and a third population
// whose class nobody argued about. When one is genuinely wanted, this test is
// the place the argument gets written down.
func TestLane_ThereAreExactlyTwoAndTheyAreDeclaredInGo(t *testing.T) {
	declared := map[string]string{} // var name -> file
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
				isLane := false
				if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "Lane" {
					isLane = true
				}
				for _, v := range vs.Values {
					if cl, ok := v.(*ast.CompositeLit); ok {
						if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "Lane" {
							isLane = true
						}
					}
				}
				if !isLane {
					continue
				}
				for _, name := range vs.Names {
					declared[name.Name] = rel
				}
			}
		}
	}

	if len(declared) != 2 {
		t.Fatalf("internal/classify declares %d package-level Lane value(s) (%v); criterion 10 says there "+
			"are EXACTLY TWO, LanePersonal and LaneResidue. A third lane is a third worker_type, a third "+
			"prompt nothing measured and a third population whose class nobody argued about — if one is "+
			"genuinely wanted, the argument belongs in a SPEC, not in a var block", len(declared), declared)
	}
	for _, want := range []string{"LanePersonal", "LaneResidue"} {
		if _, ok := declared[want]; !ok {
			t.Errorf("no package-level Lane named %q; declared: %v", want, declared)
		}
	}
}

// The two lanes' values, and the ONE reason they differ. Everything asserted
// here is read by classifyAll, so a wrong value is a residue prompt over
// personal mail or the reverse — silently, with a verdict recorded either way.
func TestLane_ValuesAreTheTwoContracts(t *testing.T) {
	if classify.LanePersonal.Name != "personal" || classify.LaneResidue.Name != "residue" {
		t.Errorf("lane names are %q/%q, want personal/residue — the name is what `--lane` matches and what "+
			"a runbook quotes", classify.LanePersonal.Name, classify.LaneResidue.Name)
	}

	// CRITERION 11, the value with a defect attached to it. The two lanes' inbox
	// filters both key their NOT EXISTS on worker_type, so ONE shared value would
	// mean: a residue message classified today, then claimed tomorrow by a
	// capture rule into a local_only + ai_classify project, is PERMANENTLY
	// invisible to the personal lane — already "classified", by a different
	// prompt, with a verdict nobody would look for. The integration suite seeds
	// exactly that sequence; this is the constant it depends on.
	if classify.LanePersonal.WorkerType != "classify" {
		t.Errorf("LanePersonal.WorkerType = %q, want %q. Changing it orphans every ai_run this worker has "+
			"already written and re-classifies the whole personal corpus",
			classify.LanePersonal.WorkerType, "classify")
	}
	if classify.LaneResidue.WorkerType != "classify_residue" {
		t.Errorf("LaneResidue.WorkerType = %q, want %q (criterion 11)", classify.LaneResidue.WorkerType,
			"classify_residue")
	}
	if classify.LanePersonal.WorkerType == classify.LaneResidue.WorkerType {
		t.Fatalf("both lanes use worker_type %q. See above: sharing it makes a message classified by one "+
			"lane invisible to the other, forever", classify.LanePersonal.WorkerType)
	}

	if classify.LanePersonal.PromptVersion != classify.PromptVersion {
		t.Errorf("LanePersonal.PromptVersion = %q, want classify.PromptVersion (%q) — one spelling, or the "+
			"stamp in ai_runs.input stops matching the constant a session greps for",
			classify.LanePersonal.PromptVersion, classify.PromptVersion)
	}
	if classify.ResiduePromptVersion != "residue-v1" {
		t.Errorf("classify.ResiduePromptVersion = %q, want \"residue-v1\" (criterion 24)",
			classify.ResiduePromptVersion)
	}
	if classify.LaneResidue.PromptVersion != classify.ResiduePromptVersion {
		t.Errorf("LaneResidue.PromptVersion = %q, want %q", classify.LaneResidue.PromptVersion,
			classify.ResiduePromptVersion)
	}
	if classify.LanePersonal.PromptVersion == classify.LaneResidue.PromptVersion {
		t.Errorf("both lanes stamp prompt_version %q. Without a distinct stamp, two runs that disagree are "+
			"indistinguishable from a model that drifted", classify.LanePersonal.PromptVersion)
	}

	if classify.LanePersonal.System != classify.SystemPrompt {
		t.Errorf("LanePersonal.System is not classify.SystemPrompt; the personal lane's prompt is measured " +
			"(0.94 recall / 0.50 precision, 2026-08-31) and must not be re-typed")
	}
	if classify.LaneResidue.System != classify.ResidueSystemPrompt {
		t.Errorf("LaneResidue.System is not classify.ResidueSystemPrompt")
	}
	if classify.LanePersonal.System == classify.LaneResidue.System {
		t.Fatalf("both lanes carry the same system prompt. THE PROMPT IS THE WORK of this ticket: the " +
			"personal prompt opens 'You classify one personal (non-work) email', and ~961 of the residue " +
			"is Slack or Upwork WORK conversations — telling the model the mail is personal is telling it " +
			"something this SPEC measured to be false (criterion 22)")
	}

	for _, lane := range []classify.Lane{classify.LanePersonal, classify.LaneResidue} {
		if lane.LabelsPath == "" {
			t.Errorf("lane %q has no LabelsPath; `classify eval --lane %s` must default to the right "+
				"fixture, or the residue lane is one typo away from being scored against the personal "+
				"labels", lane.Name, lane.Name)
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", lane.LabelsPath)); err != nil {
			t.Errorf("lane %q points LabelsPath at %s, which does not exist: %v",
				lane.Name, lane.LabelsPath, err)
		}
	}
	if classify.LanePersonal.LabelsPath == classify.LaneResidue.LabelsPath {
		t.Errorf("both lanes point at %s. The personal set does not transfer: the residue's actionable "+
			"base rate is a couple of percent and its labels are drawn fresh and STRATIFIED (criteria "+
			"18-19)", classify.LanePersonal.LabelsPath)
	}
}

// ---- criterion 10: ONE contract, not one per lane ----------------------------

// The argument is in the SPEC's "three answers", (a): reusing VerdictSchema
// verbatim is the only way the residue's recall/precision can be read against
// the personal lane's 0.94 / 0.50. A different schema makes the two lanes two
// experiments.
func TestLane_DoesNotForkTheOutputContract(t *testing.T) {
	fields := map[string]bool{}
	rt := reflect.TypeOf(classify.Lane{})
	for i := 0; i < rt.NumField(); i++ {
		fields[rt.Field(i).Name] = true
	}
	want := []string{"Name", "WorkerType", "System", "PromptVersion", "LabelsPath"}
	for _, f := range want {
		if !fields[f] {
			t.Errorf("classify.Lane has no %q field; the SPEC's struct is %v", f, want)
		}
	}
	for f := range fields {
		switch f {
		case "Name", "WorkerType", "System", "PromptVersion", "LabelsPath":
		default:
			t.Errorf("classify.Lane has an extra field %q. If it is a schema or a schema name, that is the "+
				"fork criterion 10 refuses: ONE contract for both lanes, so the two numbers are comparable "+
				"and TestSchema_MatchesTheOutputContract keeps meaning something", f)
		}
	}

	// And no lane-scoped schema smuggled in beside the struct.
	for _, rel := range csSources(t, "internal/classify") {
		code := csGoCode(t, rel)
		for _, banned := range []string{"ResidueVerdictSchema", "ResidueSchemaName", "residueVerdictSchema"} {
			if strings.Contains(code, banned) {
				t.Errorf("%s declares %s. VerdictSchema and SchemaName are NOT lane-scoped (criterion 10): "+
					"one contract, both lanes", rel, banned)
			}
		}
	}
}

// ---- criterion 10: the zero value is REFUSED, not defaulted ------------------

// "A caller that forgets it gets an error instead of the personal prompt over
// the residue." A default here would be the quietest possible bug: the run
// succeeds, the rows land, and the only symptom is a prompt asking whether a
// Nextdoor digest is a personal bill.
func TestRun_ZeroLaneIsRefusedNotDefaulted(t *testing.T) {
	local := cfLocal()
	store := &lnStore{cfStore: cfStore{pending: cfMessages(3)}}

	stats, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), local, time.Minute),
		classify.Config{Model: "qwen3:8b", MaxTokens: 512}) // no Lane
	if err == nil {
		t.Fatalf("Run accepted a Config with no Lane and returned %+v. Criterion 10: the zero value is "+
			"REFUSED rather than defaulted — defaulting to the personal lane means the residue gets "+
			"classified by a prompt that opens 'You classify one personal (non-work) email', with a "+
			"verdict recorded and nothing to notice it by", stats)
	}
	if !regexp.MustCompile(`(?i)lane`).MatchString(err.Error()) {
		t.Errorf("the refusal does not name the lane: %v", err)
	}
	if store.pendingCalls != 0 {
		t.Errorf("Run read the inbox %d time(s) before refusing. The refusal is a configuration error and "+
			"belongs before any I/O", store.pendingCalls)
	}
	if local.calls != 0 {
		t.Errorf("the local client was called %d time(s) during a refused run", local.calls)
	}
}

// ---- criterion 16: --since is REQUIRED for the residue lane ------------------

// Not a silent default: a default would be a 29-hour job started by a typo.
// 14,737 × 7.2 s = 106,106 s = 29.5 GPU-hours, and the 60-minute estimate that
// said otherwise was 14,737 × 0.25 s — a warm micro-benchmark of a ten-word
// prompt, measured on a different workload. The two numbers differ by 25-29×.
func TestRun_ResidueLane_RefusesAnUnboundedPass(t *testing.T) {
	t.Run("no --since: refuse, read nothing, send nothing", func(t *testing.T) {
		local := cfLocal()
		store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(3)}}

		_, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), local, time.Minute),
			classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LaneResidue})
		if err == nil {
			t.Fatalf("Run accepted an unbounded residue pass. Criterion 16: --since is REQUIRED on this " +
				"lane and the refusal is the only thing between a typo and a 29-hour GPU job")
		}
		msg := err.Error()
		if !regexp.MustCompile(`\d{2},?\d{3}`).MatchString(msg) {
			t.Errorf("the refusal does not name the COUNT of the residue (~14,737 messages): %q\n"+
				"Criterion 16 wants the arithmetic in the message, because a refusal that does not show "+
				"its working teaches the reader to pass --since 87600h to make it go away", msg)
		}
		if !strings.Contains(msg, "7.2") {
			t.Errorf("the refusal does not name the measured 7.2 s median: %q\nThat number is the whole "+
				"argument (docs/runbooks/local-classifier.md:179, SWT-25, 2026-08-31) and it is the one a "+
				"reader must not re-derive from the 0.25 s warm benchmark", msg)
		}
		if !strings.Contains(msg, "--since") {
			t.Errorf("the refusal does not tell the operator what to pass: %q", msg)
		}
		if !regexp.MustCompile(`(?i)hour|gpu`).MatchString(msg) {
			t.Errorf("the refusal states neither the hours nor that they are GPU-hours: %q", msg)
		}
		if store.pendingCalls != 0 || local.calls != 0 {
			t.Errorf("the refused pass read the inbox %d time(s) and called the model %d time(s); it must "+
				"do neither", store.pendingCalls, local.calls)
		}
	})

	t.Run("control: with --since the same fixture runs", func(t *testing.T) {
		local := cfLocal()
		store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(3)}}
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), local, time.Minute), lnResidueCfg())
		if err != nil {
			t.Fatalf("Run on the residue lane with --since: %v. Without this control the refusal above is "+
				"satisfied by a lane that never runs at all", err)
		}
		if local.calls != 3 || stats.Processed != 3 {
			t.Fatalf("local calls = %d, stats = %+v; want 3 classified", local.calls, stats)
		}
	})

	t.Run("the personal lane keeps today's behaviour", func(t *testing.T) {
		local := cfLocal()
		store := &lnStore{cfStore: cfStore{pending: cfMessages(2)}}
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), local, time.Minute),
			classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LanePersonal})
		if err != nil {
			t.Fatalf("an unbounded PERSONAL pass was refused: %v. Criterion 16 scopes the requirement to "+
				"the residue lane — the personal population is 1,600 and bounded, and making --since "+
				"mandatory there breaks every command in the SWT-22 runbook", err)
		}
		if stats.Processed != 2 {
			t.Errorf("stats = %+v, want 2 processed", stats)
		}
	})
}

// ---- criteria 11 (unit half) and 24: the lane is what the row records --------

func TestRun_ResidueLane_RecordsItsOwnWorkerTypeAndPromptVersion(t *testing.T) {
	local := cfLocal()
	store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(1)}}

	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), local, time.Minute), lnResidueCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	oks := store.withStatus("ok")
	if len(oks) != 1 {
		t.Fatalf("recorded %d status='ok' rows, want 1", len(oks))
	}
	if got := oks[0].WorkerType; got != "classify_residue" {
		t.Errorf("ai_runs.worker_type = %q, want %q (criterion 11). The value comes from cfg.Lane.WorkerType "+
			"— hardcoding 'classify' here is exactly the defect criterion 11 describes: the message would "+
			"then be invisible to the personal lane forever once a rule claims it", got, "classify_residue")
	}
	in := cfDecode(t, oks[0].Input)
	if got, _ := in["prompt_version"].(string); got != classify.ResiduePromptVersion {
		t.Errorf("ai_runs.input.prompt_version = %v, want %q (criterion 24). runInput must read "+
			"cfg.Lane.PromptVersion, not the package constant — with the personal stamp on a residue "+
			"verdict, two runs that disagree are indistinguishable from a model that drifted",
			in["prompt_version"], classify.ResiduePromptVersion)
	}

	if len(local.requests) != 1 {
		t.Fatalf("the local client saw %d requests, want 1", len(local.requests))
	}
	req := local.requests[0]
	if req.System != classify.ResidueSystemPrompt {
		t.Errorf("Request.System is not classify.ResidueSystemPrompt:\n%q", req.System)
	}
	// ONE contract, both lanes — the schema is not lane-scoped.
	if req.SchemaName != classify.SchemaName || string(req.Schema) != string(classify.VerdictSchema) {
		t.Errorf("Request schema = %q/%s, want the shared classify contract (criterion 10). Reusing it is "+
			"the only way the residue's recall/precision can be read against the personal lane's "+
			"0.94 / 0.50", req.SchemaName, req.Schema)
	}

	// And the personal lane still records its own, in the same process.
	personal := &lnStore{cfStore: cfStore{pending: cfMessages(1)}}
	if _, err := classify.Run(context.Background(), personal,
		provider.NewRouter(cfHosted(), cfLocal(), time.Minute),
		classify.Config{Model: "qwen3:8b", MaxTokens: 512, Lane: classify.LanePersonal}); err != nil {
		t.Fatalf("Run(personal): %v", err)
	}
	if got := personal.withStatus("ok")[0].WorkerType; got != "classify" {
		t.Errorf("the personal lane recorded worker_type %q; SWT-22's rows must keep their value or the "+
			"whole personal corpus becomes pending again", got)
	}
}

// ---- criterion 14: Attribution is attrUnmatched, and both consequences -------

// `scanMessages` sets AttrProject today because the personal inbox requires an
// 'attributed' decision. The residue inbox requires the opposite, so the value
// changes — and TWO things follow from it, both asserted here because each is a
// separate mechanism.
func TestRun_ResidueLane_IsRestrictedThroughTheNonProjectBranch(t *testing.T) {
	t.Run("classOf yields ClassRestricted: the hosted lane is never reached", func(t *testing.T) {
		general := cfHosted()
		local := cfLocal()
		store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(3)}}

		if _, err := classify.Run(context.Background(), store,
			provider.NewRouter(general, local, time.Minute), lnResidueCfg()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if general.calls != 0 {
			t.Errorf("the hosted client recorded %d Complete call(s) for residue messages. An unmatched "+
				"message has NO project, so ProjectLocalOnly is false and only ClassOf's "+
				"`state != AttrProject` branch (provider/locality.go:195-198) makes it restricted — that "+
				"branch is SWT-21's deliberate choice so rule completeness is never load-bearing for "+
				"containment. If these went to the hosted lane, the fixture's Attribution is being "+
				"overwritten with AttrProject somewhere", general.calls)
		}
		if local.calls != 3 {
			t.Errorf("the local client saw %d call(s), want 3 — the control that says these messages CAN "+
				"be classified, so 'zero hosted calls' means the boundary refused rather than nothing "+
				"happened", local.calls)
		}
	})

	t.Run("classReasonOf files the skip under unmatched", func(t *testing.T) {
		store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(4)}}
		if _, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), nil, time.Minute), lnResidueCfg()); err != nil {
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
		if _, ok := reasons["unmatched"]; !ok {
			t.Errorf("class_reasons = %v, want the skips filed under \"unmatched\" (criterion 14). On the "+
				"personal lane they are filed under \"project_local_only\"; if both lanes report the same "+
				"reason, a residue skip and a personal skip are indistinguishable in the report and the "+
				"one number an operator reads stops discriminating", reasons)
		}
		if _, ok := reasons["project_local_only"]; ok {
			t.Errorf("class_reasons = %v — a residue message was filed under project_local_only. It has no "+
				"project at all: 0015's CHECK makes (action='unmatched') = (project_id IS NULL) a schema "+
				"fact", reasons)
		}
	})
}

// ---- invariant 4: zero hosted calls for the residue lane, with its control ---

// SWT-21's thesis, re-pinned on the wider inbox. Premise 4 says ClassOf already
// restricts every unmatched message, so this is not a new guarantee to build —
// it IS a guarantee to pin, because the inbox this ticket opens is ten times the
// size of the one that was pinned before.
func TestRun_ResidueLane_NoLocalProvider_MakesZeroHostedCalls(t *testing.T) {
	t.Run("control: with a local lane the same fixture IS classified", func(t *testing.T) {
		local := cfLocal()
		store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(3)}}
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(cfHosted(), local, time.Minute), lnResidueCfg())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if local.calls != 3 || stats.Processed != 3 || len(store.extractions) != 3 {
			t.Fatalf("local calls = %d, stats = %+v, extractions = %d; want 3/3/3. Without this control "+
				"the assertion below is satisfied by a worker that does nothing",
				local.calls, stats, len(store.extractions))
		}
	})

	t.Run("no local lane: nothing is sent anywhere", func(t *testing.T) {
		general := cfHosted()
		store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(3)}}
		stats, err := classify.Run(context.Background(), store,
			provider.NewRouter(general, nil, time.Minute), lnResidueCfg())
		if err != nil {
			t.Fatalf("Run returned an error for a fully skipped residue pass: %v. A refusal is the "+
				"boundary working: exit zero, retry next pass", err)
		}
		if general.calls != 0 {
			t.Errorf("the hosted client recorded %d Complete call(s). The residue is not 'less sensitive' "+
				"than the personal pile — it is UNCLASSIFIED, which is why SWT-21 made every "+
				"non-AttrProject state restricted", general.calls)
		}
		if len(store.extractions) != 0 || stats.Skipped != 3 {
			t.Errorf("extractions = %d, stats = %+v; want 0 extractions and 3 skipped — no skip of any "+
				"kind writes an extraction, which is what leaves the message in the inbox",
				len(store.extractions), stats)
		}
	})
}

// ---- criterion 22: what the residue prompt must NOT say ----------------------

func TestResiduePrompt_DoesNotAskTriagesQuestionsOrClaimTheMailIsPersonal(t *testing.T) {
	prompt := classify.ResidueSystemPrompt
	if len(prompt) < 400 {
		t.Fatalf("ResidueSystemPrompt is %d characters. THE WIRING IS NOT THE WORK — the lane, the filter "+
			"and the config are a day; the prompt is the ticket. The spike measured that a bare "+
			"'Classify.' called an HOA violation notice actionable:false", len(prompt))
	}
	lower := strings.ToLower(prompt)

	if strings.Contains(lower, "personal (non-work)") {
		t.Errorf("the residue prompt opens with the personal lane's sentence. The residue is UNCLASSIFIED " +
			"BY DEFINITION — premise 6 establishes that ~961 of its messages are Slack or Upwork work " +
			"conversations, and the address-less census measured 1,287 — so telling the model the mail is " +
			"personal is telling it something this SPEC has measured to be false")
	}
	for _, banned := range []struct{ token, why string }{
		{"attach_to_task_id", "that is triage's field (internal/triage/prompt.go:19-60); this classifier " +
			"creates nothing and attaches to nothing"},
		{"open task", "the open-task list is triage's context. A flagged residue message has NO project, " +
			"so there is nothing to file it under — that is the API section's whole point"},
		{"client", "asking a client-work question of a Nextdoor digest is the mistake this ticket exists " +
			"to avoid"},
		{"project", "an unmatched message has no project: 0015's CHECK makes " +
			"(action='unmatched') = (project_id IS NULL) a schema fact"},
	} {
		if strings.Contains(lower, banned.token) {
			t.Errorf("the residue prompt mentions %q (criterion 22) — %s", banned.token, banned.why)
		}
	}
}

// ---- criterion 23: what it must say, each clause justified by the census -----

func TestResiduePrompt_StatesTheObjectiveTheSecurityClauseAndTheFamilies(t *testing.T) {
	lower := strings.ToLower(classify.ResidueSystemPrompt)

	// (a) The objective, UNCHANGED from the personal lane. It is tempting to
	// argue that in a pile of marketing mail precision matters more; it is still
	// wrong, because a false positive costs a second in a report nobody is paged
	// by and this lane is shadow.
	if !strings.Contains(lower, "recall") {
		t.Errorf("the residue prompt never states that RECALL IS THE OBJECTIVE. Without it the next editor " +
			"tunes for precision — which in a pile that is ~2%% actionable looks like an improvement all " +
			"the way down to zero recall")
	}
	if !regexp.MustCompile(`late fee|missed payment|fine`).MatchString(lower) {
		t.Errorf("the prompt does not say what a miss COSTS (a late fee on a missed payment or fine " +
			"notice). The measured version of that sentence is what makes 'when genuinely torn, answer " +
			"true' land")
	}
	if !regexp.MustCompile(`torn|unsure|in doubt`).MatchString(lower) {
		t.Errorf("the prompt does not tell the model what to do when it is genuinely torn (answer true)")
	}

	// (b) The account-security clause — NEW for this lane, and the reason
	// google.com (239) is on the refusal list: security alerts are the most
	// actionable mail in the corpus and marketing is the least, under one domain.
	security := regexp.MustCompile(`sign-in|sign in|password|verify|two-factor|device`)
	if !security.MatchString(lower) {
		t.Errorf("the prompt never names an ACCOUNT-SECURITY action (verify a sign-in, reset a password, " +
			"confirm a device) as actionable. Criterion 23 adds it for this lane specifically: google.com " +
			"carries 239 residue messages and is refused BECAUSE it mixes exactly this with marketing")
	}

	// (c) A named human asking for something, as distinct from a template. This
	// is the clause that catches the 1,287 address-less upwork conversations.
	if !regexp.MustCompile(`human|person|template|automated`).MatchString(lower) {
		t.Errorf("the prompt does not distinguish a NAMED HUMAN asking for something from a template. " +
			"1,287 of the residue is upwork conversations with no email address at all — real people " +
			"waiting on an answer, sitting in the same pile as Humble Bundle")
	}

	// (d) The not-actionable families, named explicitly because the census says
	// these ARE the residue. Naming a FAMILY is not naming a sender: the
	// per-sender-branch guard (structure_test.go:435) still applies.
	for _, fam := range []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`job alert|job recommendation|recommended job|jobs? digest`),
			"indeed 680, ziprecruiter 320, fastweb 157, glassdoor 142, monster 109"},
		{regexp.MustCompile(`social|professional network|network notification|connection request`),
			"linkedin 695, nextdoor 726, pinterest 296, facebookmail 156, instagram 144"},
		{regexp.MustCompile(`repositor|service notification|build|ci `),
			"github 455, statuspage 231 — notifications with no request addressed to the recipient"},
		{regexp.MustCompile(`newsletter|publication|reading digest`),
			"medium 382, nytimes 192, washingtonpost 177"},
		{regexp.MustCompile(`marketing|offer|sale|wishlist|promotion`),
			"amazon 407, humblebundle 356, motorola 122, cinemark 119, ezcontacts 101"},
		{regexp.MustCompile(`shipping|delivery preview|package`),
			"usps informeddelivery 146 — a preview with nothing to do"},
	} {
		if !fam.re.MatchString(lower) {
			t.Errorf("the prompt does not name the not-actionable family matching %s (%s). Criterion 23: "+
				"this is the half the residue needs, and the census is what says so — a prompt that only "+
				"lists what IS actionable leaves the model guessing about 90%% of its input",
				fam.re, fam.why)
		}
	}

	// (e) THE TRAP CLAUSE, as its own paragraph. This is the residue's equivalent
	// of the personal lane's near-miss clause (prompt.go:63-67), whose removal
	// was MEASURED to flip "your statement is available" on 883 messages.
	trap := regexp.MustCompile(`ends tonight|final notice|act now|last chance|countdown|needs attention`)
	var found string
	for _, para := range strings.Split(lower, "\n\n") {
		if trap.MatchString(para) {
			found = para
			break
		}
	}
	if found == "" {
		t.Errorf("the prompt has no marketing-urgency trap clause. Criterion 23 requires it VERBATIM in " +
			"spirit: \"Ends tonight\", \"final notice\", \"act now\", \"your account needs attention\", a " +
			"countdown — none of these is a consequence. Without it the model flags the entire retail " +
			"segment, which is 1,105 messages of the corpus by the census's own numbers")
	} else if !regexp.MustCompile(`consequence|lose|loses|real`).MatchString(found) {
		t.Errorf("the trap paragraph names the urgency words but not the RULE that resolves them: "+
			"actionable means the recipient loses something real by not acting, not that the sender wants "+
			"a click.\nparagraph: %q", found)
	} else if len(strings.Fields(found)) < 20 {
		t.Errorf("the trap clause is %d words. Criterion 23: it is its own PARAGRAPH and 'must not be "+
			"trimmed for brevity' — the personal lane's equivalent was measured, and a shortened prompt "+
			"identical in every other way flipped the most common message shape in the corpus",
			len(strings.Fields(found)))
	}

	// (f) Bilingual, same as the personal lane. 51 of 1,609 personal messages are
	// Spanish; the originals are never translated.
	if !strings.Contains(lower, "spanish") || !strings.Contains(lower, "english") {
		t.Errorf("the prompt is not bilingual (criterion 23). English and Spanish are treated identically " +
			"and originals are never translated — a translation pass is a second inference and a new " +
			"failure mode that silently changes an answer with nothing left to compare against")
	}
}

// ---- criterion 23, last bullet: ONE spelling of the link contract ------------

// "A structural test asserts the same sentences appear in both prompts, so a
// future edit to one cannot silently change the link contract of the other."
// The link paragraph is the one place a prompt edit can break the APPLICATION's
// contract rather than just its accuracy: ResolveLink resolves an index against
// the message's own stored candidates, and a prompt that stops saying "answer
// with the number" gets URLs back into a field typed integer-or-null.
func TestResiduePrompt_SharesTheLinkParagraphWithThePersonalPrompt(t *testing.T) {
	personal := lnLinkParagraph(t, classify.SystemPrompt, "classify.SystemPrompt")
	residue := lnLinkParagraph(t, classify.ResidueSystemPrompt, "classify.ResidueSystemPrompt")

	if lnCollapse(personal) != lnCollapse(residue) {
		t.Errorf("the two prompts spell the link contract differently.\npersonal: %q\nresidue:  %q\n"+
			"Criterion 23: ONE spelling — the numbered list is the complete set, answer with the number a "+
			"person would open to act, null when none of them is that or no list is shown, never invent a "+
			"number. Two spellings drift, and the drift is invisible until link_index starts coming back "+
			"as something ResolveLink rejects", personal, residue)
	}
	for _, want := range []string{"link_index", "null"} {
		if !strings.Contains(strings.ToLower(residue), want) {
			t.Errorf("the shared link paragraph does not mention %q", want)
		}
	}
	if strings.Contains(residue, "http://") || strings.Contains(residue, "https://") {
		t.Errorf("the residue prompt's link paragraph contains a URL. The prompt offers anchor TEXTS ONLY; " +
			"showing the model a URL is showing it the shape of the thing it must never author")
	}
}

// lnLinkParagraph pulls the paragraph that carries the link contract out of a
// prompt, and FAILS when there is none — a comparison of two empty strings is
// the "scan with nothing to scan" landmine.
func lnLinkParagraph(t *testing.T, prompt, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?i)link_index`)
	for _, para := range strings.Split(prompt, "\n\n") {
		if re.MatchString(para) && regexp.MustCompile(`(?i)number|list`).MatchString(para) {
			return strings.TrimSpace(para)
		}
	}
	t.Fatalf("%s has no paragraph explaining link_index (SWT-25's contract). It must be carried into this "+
		"lane verbatim: the residue is mostly HTML marketing mail, which is exactly where the candidate "+
		"list is most often non-empty", name)
	return ""
}

func lnCollapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// A last belt-and-braces on the bookkeeping: the residue lane's ai_runs.input
// still carries the ids that make a verdict reproducible, even though there is
// no project to name.
func TestRun_ResidueLane_InputStillCarriesTheIDs(t *testing.T) {
	store := &lnStore{cfStore: cfStore{pending: lnResidueMessages(1)}}
	if _, err := classify.Run(context.Background(), store,
		provider.NewRouter(cfHosted(), cfLocal(), time.Minute), lnResidueCfg()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	in := cfDecode(t, store.withStatus("ok")[0].Input)
	for _, k := range []string{"normalized_message_id", "raw_source_item_id", "user_prompt"} {
		if _, ok := in[k]; !ok {
			t.Errorf("ai_runs.input has no %q key: %s", k, mustJSON(in))
		}
	}
	if slug, ok := in["project_slug"].(string); ok && slug != "" {
		t.Errorf("ai_runs.input.project_slug = %q for a residue message. It has no project — verification "+
			"step 7 reads exactly this: every classify row is 'personal', every classify_residue row has "+
			"an empty slug", slug)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
