package capture

// comms-inbox (SWT-74, docs/tickets/comms-inbox_SPEC.md) D6, criteria 20 and
// 21: task_match must NOT be a second matcher. internal/capture/explain.go
// exports ONE entry point that runs capture's own decideMessage in
// RulesModeShadow and returns a value struct; it writes NOTHING, and it shares
// pendingMessages' column projection and scan helper so the two cannot drift.
//
// ZERO I/O: every test here is a compile-time pin or a source scan. The
// behavioural half is internal/tools' match_integration_test.go (criterion 46),
// where a real pass and a real ExplainMessage agree on the same message.
//
// ---- IMPOSED SURFACE (SPEC D6, verbatim) --------------------------------------
//
//	// internal/capture/explain.go (new)
//	type Explanation struct {
//	    MessageID int64
//	    Action    string // unmatched | attributed | task | task_log | held
//	    Project   string
//	    RuleID    int64
//	    RuleKind  string
//	    System    string
//	    Key       string
//	    TaskID    int64  // the task the rule would file onto (action task_log)
//	    Reason    string // decideMessage's own reason string
//	    Deferred  bool
//	}
//	func ExplainMessage(ctx context.Context, pool *pgxpool.Pool, messageID int64) (Explanation, error)
//	func ExplainText(ctx context.Context, pool *pgxpool.Pool, msg Message) (Explanation, error)
//
//	// internal/capture/rules_store.go — ONE projection, ONE scan helper, shared
//	// with pendingMessages (criterion 21):
//	const pendingMessageCols = `m.id, m.raw_source_item_id, … `
//	func scanPendingMessage(rows pgx.Rows|pgx.Row) (pendingMessage, error)
//
// ExplainMessage is direction-blind and horizon-blind — the caller is asking a
// question, not running a pass — and reports the direction rather than
// filtering it (invariant 5: task_match writes nothing, so an outbound message
// can be EXPLAINED but never becomes a comm).
//
// GREENFIELD NOTE — EXPECTED RED: internal/capture/explain.go does not exist,
// so Explanation, ExplainMessage and ExplainText are undefined and this file
// compile-FAILS the package's unit binary.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - task_match writes anything -> ExplainGoWritesNothing.
//   - explain.go re-implements the matcher instead of calling decideMessage ->
//     ExplainRunsCapturesOwnDecision.
//   - a second column projection beside pendingMessages' -> OneProjectionOneScan.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- criterion 20: the exported shape, compile-time ------------------------------

// D6's struct, field for field. A compile-time pin: a renamed or re-typed field
// is a build failure here rather than a silently absent JSON key in the tool's
// proposal (task_match forwards these values as `rule_id`, `rule_kind`,
// `external_system`, `external_key`, `why`).
var _ = Explanation{
	MessageID: 1, Action: "task_log", Project: "collaboratory", RuleID: 75, RuleKind: "body_regex",
	System: "jira", Key: "WEB-10469", TaskID: 452, Reason: "rule 75 (body_regex): …", Deferred: false,
}

var (
	_ func(context.Context, *pgxpool.Pool, int64) (Explanation, error)   = ExplainMessage
	_ func(context.Context, *pgxpool.Pool, Message) (Explanation, error) = ExplainText
)

// ---- criterion 20: it writes NOTHING ----------------------------------------------

func TestExplainGo_WritesNothing(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/explain.go")
	for _, banned := range []struct{ tok, why string }{
		{"INSERT", "criterion 20: task_match is read-only — it writes nothing but its audit row, which the " +
			"EXECUTOR writes (task_list's shape)"},
		{"UPDATE", "criterion 20: read-only"},
		{"DELETE", "criterion 20: read-only"},
		{"ex.Execute", "criterion 20: no tool call — an explanation is a question, not an act"},
		{"executor.", "criterion 20: the explainer never reaches the executor; the TOOL does"},
		{"insertDecision", "criterion 20: a live decision is FOREVER (capture_decisions_live_uniq). An " +
			"explanation that claimed the message would silently stop the next real pass from acting on it"},
		{"recordDecision", "criterion 20: read-only"},
		{"RulesModeLive", "D6: ExplainMessage runs decideMessage in RulesModeShadow — the mode only WORDS the " +
			"reason, and shadow is the one that acts on nothing"},
	} {
		if strings.Contains(src, banned.tok) {
			t.Errorf("internal/capture/explain.go mentions %q — %s", banned.tok, banned.why)
		}
	}
	if !strings.Contains(src, "pool.Query") && !strings.Contains(src, "pool.QueryRow") {
		t.Errorf("internal/capture/explain.go issues no read at all; it must LOAD the message and the rules " +
			"(loadRules) before it can answer (criterion 20)")
	}
}

// D6: "It must not be a second matcher." The tool calls ONE new exported entry
// point, which runs the function that decides every live attach.
func TestExplain_RunsCapturesOwnDecision(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/explain.go")
	msg := rvFuncSrc(src, "ExplainMessage")
	if msg == "" {
		t.Fatalf("explain.go declares no ExplainMessage (criterion 20)")
	}
	for _, want := range []struct{ tok, why string }{
		{"decideMessage(", "D6: ExplainMessage runs decideMessage ITSELF — the function that decides every live " +
			"attach. A second matcher is two answers to \"which task does this belong to\", and the whole " +
			"point of the verb is that it agrees with capture"},
		{"loadRules(", "D6: the rules come from the COLUMNS, like every pass"},
		{"RulesModeShadow", "D6: shadow mode — it acts on nothing"},
	} {
		if !strings.Contains(msg, want.tok) {
			t.Errorf("ExplainMessage does not call %s — %s", want.tok, want.why)
		}
	}
	txt := rvFuncSrc(src, "ExplainText")
	if txt == "" {
		t.Fatalf("explain.go declares no ExplainText (criterion 20: the \"match a particular line\" case — his " +
			"own words in the source)")
	}
	if !strings.Contains(txt, "Evaluate(") {
		t.Errorf("ExplainText does not call capture.Evaluate. D6: it evaluates the PURE matcher over a " +
			"caller-built Message and resolves the derived key through taskForExternalRef")
	}
	if !strings.Contains(txt, "taskForExternalRef(") {
		t.Errorf("ExplainText does not resolve the key through taskForExternalRef; that resolution IS the " +
			"rule_ref proposal (D6's rank-0 source)")
	}
	// It cannot run the own-action guard or the PR-trust check (both need a
	// stored message), and D6 requires it to SAY so rather than pretend.
	for _, banned := range []struct{ tok, why string }{
		{"ownActionGuard(", "D6: ExplainText has no stored message, so the own-action guard cannot run — the " +
			"Reason says so and the tool marks the proposal partial:true (criterion 25)"},
		{"prMailTrusted(", "D6: the PR-trust check needs stored raw headers"},
	} {
		if strings.Contains(txt, banned.tok) {
			t.Errorf("ExplainText calls %s — %s", banned.tok, banned.why)
		}
	}
}

// ---- criterion 21: one projection, one scan, no filters ---------------------------

func TestExplainMessage_OneProjectionOneScanNoDirectionNoHorizon(t *testing.T) {
	store := mustReadRepoFile(t, "internal/capture/rules_store.go")
	explain := mustReadRepoFile(t, "internal/capture/explain.go")

	if !strings.Contains(store, "pendingMessageCols") {
		t.Fatalf("rules_store.go declares no pendingMessageCols. Criterion 21: ExplainMessage and " +
			"pendingMessages share ONE column projection and ONE scan helper — pendingMessages is \"the one " +
			"spelling of the message projection capture evaluates\", and a second spelling is a message the " +
			"explainer reads differently from the pass that decides it")
	}
	pending := rvFuncSrc(store, "pendingMessages")
	if pending == "" {
		t.Fatalf("rules_store.go no longer declares pendingMessages")
	}
	if !strings.Contains(pending, "pendingMessageCols") {
		t.Errorf("pendingMessages does not use pendingMessageCols; the const must be the projection it ACTUALLY " +
			"runs, or the sharing is decoration (criterion 21)")
	}
	if !strings.Contains(explain, "pendingMessageCols") {
		t.Errorf("explain.go does not use pendingMessageCols (criterion 21)")
	}
	if !strings.Contains(store, "scanPendingMessage") {
		t.Errorf("rules_store.go declares no scanPendingMessage helper; criterion 21 pins ONE scan, because the " +
			"scan is where participants and the channel are folded in")
	}
	if !strings.Contains(explain, "scanPendingMessage") {
		t.Errorf("explain.go does not use the shared scan helper (criterion 21)")
	}

	msg := rvFuncSrc(explain, "ExplainMessage")
	for _, banned := range []struct{ tok, why string }{
		{"direction = 'inbound'", "criterion 21: ExplainMessage applies NO direction filter — it REPORTS the " +
			"direction so a session can see that an outbound message is not a comm (invariant 5: it writes " +
			"nothing, so nothing can be re-triaged)"},
		{`direction='inbound'`, "criterion 21: no direction filter"},
		{"cfg.Horizon", "criterion 21: horizon-blind — the caller is asking a question, not running a pass"},
		{"DefaultRulesHorizon", "criterion 21: horizon-blind"},
		{"sent_at >=", "criterion 21: horizon-blind"},
	} {
		if strings.Contains(msg, banned.tok) {
			t.Errorf("ExplainMessage contains %q — %s", banned.tok, banned.why)
		}
	}
}
