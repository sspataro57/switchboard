package tools

// SWT-45 (docs/tickets/jira-activity-revive_SPEC.md), the parts of the revive
// form and the activity flags that need no database: criteria 9 (validateReopen
// learns the revive form), 14 (the restore-target logic has ONE spelling, shared
// by both guarded forms), 16's structural half (human surfacing keys on
// policy.HumanActor, never a re-spelled prefix) and 17's validation half
// (capture_rule_add refuses J1's two illegal combinations BEFORE the INSERT).
// ZERO network, ZERO Postgres.
//
// `package tools` for reopen_test.go's and dismissal_reopen_test.go's reason,
// unchanged: the ACCEPT cases can only be expressed against the validators
// directly (a nil-pool Execute that validates runs the handler and derefs the
// pool).
//
// ---- IMPOSED SURFACE (the SPEC's "API / MCP tool changes"; Go names are this
// file's where the SPEC leaves them open) --------------------------------------
//
//	task_reopen args, three shapes (J6):
//	  plain     {task_id, reason, status?}
//	  SWT-36    {task_id, reason, dismissal_id, message_id}   // both or neither
//	  revive    {task_id, reason, message_id, revive: true}   // NEW
//	    revive requires message_id > 0; forbids dismissal_id and status.
//	    A message_id with neither revive nor dismissal_id is still refused
//	    (SWT-36's both-or-neither guard stays exactly as pinned).
//
//	internal/tools/close.go:
//	  func restoreTarget(...)  // ONE helper: open-status membership of
//	                           // closed_from_status (else ready) + the
//	                           // depUnsatisfiedPredicate re-derivation for
//	                           // ready|blocked. Called by reopenGuarded AND
//	                           // reviveGuarded; no other spelling in close.go.
//	  func reviveGuarded(...)  // J6 (a)..(h)
//	  reopenTask's plain path surfaces iff policy.HumanActor(actor) (J8).
//
//	capture_rule_add args gain {revive?: bool, addressed?: bool} (J1):
//	  revive    requires external_system AND a non-empty key_regex;
//	  addressed requires revive.
//
// RED TODAY FOR THE RIGHT REASON. reopenArgs has no `revive` and
// captureRuleAddArgs has neither flag, so json.Unmarshal drops them: a revive
// without a message id validates as a PLAIN reopen (the exact bypass J6 exists
// to refuse), a revive with a message id is refused by SWT-36's pair check, and
// a reviving rule-10 shape is accepted. close.go has no restoreTarget, no
// reviveGuarded and no policy.HumanActor call.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ---- criterion 9: validateReopen, the revive form ----------------------------

func TestValidateReopen_ReviveForm(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		wantOK     bool
		wantIn     []string // every fragment the refusal must name
		why        string
	}{
		{
			name:   "revive without message_id",
			args:   `{"task_id":7,"reason":"new Jira activity","revive":true}`,
			wantIn: []string{"revive", "message_id"},
			why: "a revive with no message would run as a PLAIN reopen: no inbound check, no " +
				"ingested-after-close guard, and (for a spine caller) a surfaced hold nobody earned",
		},
		{
			name:   "revive with a zero message_id",
			args:   `{"task_id":7,"reason":"new Jira activity","revive":true,"message_id":0}`,
			wantIn: []string{"message_id"},
			why:    "zero is indistinguishable from absent in an int64",
		},
		{
			name:   "revive with a negative message_id",
			args:   `{"task_id":7,"reason":"new Jira activity","revive":true,"message_id":-4}`,
			wantIn: []string{"message_id"},
			why:    "an id is > 0",
		},
		{
			name:   "revive with dismissal_id",
			args:   `{"task_id":7,"reason":"new Jira activity","revive":true,"message_id":9,"dismissal_id":3}`,
			wantIn: []string{"revive", "dismissal_id"},
			why: "J6 (c): the revive finds the OPEN dismissal itself, under the row lock — a caller-" +
				"supplied id is a stale-id skip waiting to happen, and two ways to say one thing",
		},
		{
			name:   "revive with status",
			args:   `{"task_id":7,"reason":"new Jira activity","revive":true,"message_id":9,"status":"ready"}`,
			wantIn: []string{"revive", "status"},
			why: "J6 (e): the RECORD decides the target (tasks.closed_from_status, else ready) — a status " +
				"argument would let a capture pass lift a holding task straight to ready",
		},
		{
			name:   "message_id alone (no revive, no dismissal_id)",
			args:   `{"task_id":7,"reason":"new inbound","message_id":9}`,
			wantIn: []string{"message_id", "dismissal_id"},
			why: "SWT-36's both-or-neither guard is unchanged: a message id with nothing saying which " +
				"guarded form it belongs to must never validate",
		},
		// The three accepted shapes. A validator that refused the revive would make
		// the whole feature unreachable while every refusal above passed.
		{name: "the revive form", args: `{"task_id":7,"reason":"capture: Jira mail about API-4103","message_id":9,"revive":true}`, wantOK: true},
		{name: "the SWT-36 guarded form", args: `{"task_id":7,"reason":"new inbound","dismissal_id":3,"message_id":9}`, wantOK: true},
		{name: "the plain form", args: `{"task_id":7,"reason":"ITS-1 left Done","status":"delivered"}`, wantOK: true},
		{name: "the plain form, no status", args: `{"task_id":7,"reason":"SWT-45 smoke"}`, wantOK: true},
		// revive:false is the plain form, not a revive.
		{name: "revive:false is the plain form", args: `{"task_id":7,"reason":"r","revive":false}`, wantOK: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateReopen([]byte(tc.args))
			if tc.wantOK {
				if err != nil {
					t.Errorf("validateReopen(%s) = %v, want nil — criterion 9 accepts this shape", tc.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateReopen(%s) = nil, want a refusal. %s", tc.args, tc.why)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("validateReopen(%s) = %q, which does not name %q (each refusal names the "+
						"offending field)", tc.args, err, want)
				}
			}
		})
	}
}

// ---- criterion 17 (validation half): the activity flags on capture_rule_add --

// The SPEC's J2 and J4 commands, verbatim in substance, and the rule-10 shape
// J1 exists to make un-flaggable. The owner's 2026-09-12 answer (a Slack or
// GitHub mention of a key IS Jira activity) is the mention-successor shape: a
// body_regex with an explicit, WHOLE-KEY key_regex — which is exactly what J1
// requires before --revive is legal.
const (
	rvJ2KeyRegex      = `^[^\n]*?\b((?:WEB|API|OPS)-[0-9]+)\b`
	rvJ4Pattern       = `\A[^\n]*(?:\bmentioned you on LHH-[0-9]+|\bassigned LHH-[0-9]+ to you)`
	rvJ4KeyRegex      = `\A[^\n]*?\b(LHH-[0-9]+)\b`
	rvRule10Pattern   = `(WEB|API|OPS)-[0-9]+`
	rvMentionKeyRegex = `\b((?:WEB|API|OPS)-[0-9]+)\b`
)

func TestParseCaptureRuleAdd_ActivityFlags(t *testing.T) {
	j2 := map[string]any{
		"project": "collaboratory", "criteria_type": "sender", "pattern": "jira@treetopllc.jira.com",
		"external_system": "jira", "key_regex": rvJ2KeyRegex,
		"url_template": "https://treetopllc.jira.com/browse/{key}", "priority": 92,
	}
	// with copies base and applies key/value pairs; a nil value deletes the key.
	with := func(base map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			k := kv[i].(string)
			if kv[i+1] == nil {
				delete(out, k)
				continue
			}
			out[k] = kv[i+1]
		}
		return out
	}

	for _, tc := range []struct {
		name   string
		args   map[string]any
		wantOK bool
		wantIn []string
		why    string
	}{
		{
			name: "revive on the rule-10 shape (no key_regex)",
			args: map[string]any{"project": "collaboratory", "criteria_type": "body_regex", "pattern": rvRule10Pattern,
				"external_system": "jira", "priority": 90, "revive": true},
			wantIn: []string{"revive", "key_regex"},
			why: "F1 + J1: with no key_regex the key is the pattern's FIRST group — the PREFIX. A " +
				"reviving prefix rule would resurrect the catch-all tasks 56/57/60 (hundreds of logs) on " +
				"every mention. This refusal is what makes that impossible",
		},
		{
			name:   "revive without external_system",
			args:   with(j2, "external_system", nil, "revive", true),
			wantIn: []string{"revive", "external_system"},
			why:    "an attribution-only rule creates nothing and so can revive nothing; J1 refuses the combination",
		},
		{
			name:   "addressed without revive",
			args:   with(j2, "addressed", true),
			wantIn: []string{"addressed", "revive"},
			why:    "J1: addressed IMPLIES revive; an 'addressed' rule that is not activity is a flag nothing reads",
		},
		{
			name:   "addressed with revive:false",
			args:   with(j2, "addressed", true, "revive", false),
			wantIn: []string{"addressed", "revive"},
			why:    "same rule, spelled explicitly",
		},
		{name: "J2 with --revive", args: with(j2, "revive", true), wantOK: true},
		{name: "J2 without flags (today's shape)", args: j2, wantOK: true},
		{
			name: "J4 with --revive --addressed",
			args: map[string]any{
				"project": "reengine", "criteria_type": "body_regex", "pattern": rvJ4Pattern,
				"external_system": "jira", "key_regex": rvJ4KeyRegex,
				"url_template": "https://avviato.atlassian.net/browse/{key}", "priority": 101,
				"revive": true, "addressed": true,
			},
			wantOK: true,
		},
		{
			// Owner answer 2026-09-12: mentions count. The successor to rule 10 is
			// legal with --revive BECAUSE it carries a whole-key key_regex.
			name: "the rule-10 successor (mention rule) with --revive and a whole-key key_regex",
			args: map[string]any{
				"project": "collaboratory", "criteria_type": "body_regex", "pattern": `\b(?:WEB|API|OPS)-[0-9]+\b`,
				"external_system": "jira", "key_regex": rvMentionKeyRegex, "priority": 90, "revive": true,
			},
			wantOK: true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			_, err = parseCaptureRuleAdd(args)
			if tc.wantOK {
				if err != nil {
					t.Errorf("parseCaptureRuleAdd(%s) = %v, want nil", args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseCaptureRuleAdd(%s) = nil, want a refusal BEFORE the INSERT (criterion 17). %s",
					args, tc.why)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("parseCaptureRuleAdd(%s) = %q, which does not name %q (criterion 17: the error names "+
						"the field)", args, err, want)
				}
			}
		})
	}
}

// ---- criterion 14: ONE restore-target helper -----------------------------------

// "The restore-target logic (open-status membership + depUnsatisfiedPredicate
// re-derivation) lives in ONE helper called by both guarded forms. A structural
// test fails a second spelling."
//
// Why it matters: SWT-36's Codex re-reviews found the dependency trap (a
// verbatim `blocked` strands a task whose dependencies completed while it was
// closed; a verbatim `ready` lets a worker claim it early). A second copy in the
// revive is how one of the two forms loses that fix the next time it is edited.
func TestClose_RestoreTargetHasOneSpelling(t *testing.T) {
	src := rvReadFile(t, "close.go")

	helper := rvFuncBody(src, "restoreTarget")
	if helper == "" {
		t.Fatalf("internal/tools/close.go declares no func restoreTarget. Criterion 14: the restore target " +
			"(closed_from_status when it is in openStatuses, else ready, then the dependency re-derivation " +
			"for ready|blocked) is ONE helper both guarded forms call")
	}
	if !strings.Contains(helper, "depUnsatisfiedPredicate") {
		t.Errorf("restoreTarget does not use depUnsatisfiedPredicate — the dependency re-derivation belongs " +
			"INSIDE the one helper")
	}
	if !strings.Contains(helper, "openStatuses") {
		t.Errorf("restoreTarget does not consult openStatuses — the membership test belongs inside the one helper")
	}
	if n := strings.Count(src, "depUnsatisfiedPredicate"); n != 1 {
		t.Errorf("close.go names depUnsatisfiedPredicate %d times, want exactly 1 (inside restoreTarget). A "+
			"second use is a second spelling of the re-derivation", n)
	}
	for _, caller := range []string{"reopenGuarded", "reviveGuarded"} {
		body := rvFuncBody(src, caller)
		if body == "" {
			t.Errorf("close.go declares no func %s (J6: the revive is a third form of task_reopen, handled by "+
				"reviveGuarded beside SWT-36's reopenGuarded)", caller)
			continue
		}
		if !strings.Contains(body, "restoreTarget(") {
			t.Errorf("%s does not call restoreTarget — criterion 14: both guarded forms share the one helper", caller)
		}
	}
}

// ---- criterion 16 (structural half): the human test is policy.HumanActor ------

// "It keys on policy.HumanActor, never a re-spelled prefix." The IK landmine
// ("an actor-prefix check is a transport label, not a trust boundary") is
// about exactly this: a handler that restates "dashboard:" / "opsctl:" /
// "manual:" drifts from the policy gate the day a new transport is added, and
// mcp:manual:salvo is the case it forgets first.
func TestReopen_HumanSurfacingKeysOnPolicyHumanActor(t *testing.T) {
	src := rvReadFile(t, "close.go")
	if !strings.Contains(src, "policy.HumanActor(") {
		t.Errorf("close.go never calls policy.HumanActor. J8: a human's plain task_reopen surfaces the task " +
			"(surfaced_at = now(), message NULL), and 'human' has ONE definition (SWT-20)")
	}
	code := rvStripComments(src)
	for _, lit := range []string{`"dashboard:"`, `"opsctl:"`, `"manual:"`, `"mcp:"`} {
		if strings.Contains(code, lit) {
			t.Errorf("close.go spells the actor prefix %s in code. Criterion 16: key on policy.HumanActor, never "+
				"a re-spelled prefix", lit)
		}
	}
}

// ---- helpers --------------------------------------------------------------------

func rvReadFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read internal/tools/%s: %v", name, err)
	}
	return string(b)
}

// rvFuncBody returns the source of `func name(` up to the next top-level func,
// or "" when the file does not declare it.
func rvFuncBody(src, name string) string {
	re := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return ""
	}
	body := src[loc[0]:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	return body
}

func rvStripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
