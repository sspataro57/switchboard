package replyfold_test

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md) CC5
// and criterion 11: both inquiry-lane inboxes widen by ONE shared predicate,
// replyfold.InquiryEligibleLatestSQL. It is the one spelling of "this message
// is eligible": the latest decision (any mode) is `attributed`, or the latest
// LIVE decision is a `task_log` that capture recorded as resurfacing onto a
// task that is STILL closed. Plain unit test, no database; the behaviour is
// proven by the classify and promote integration suites (criteria 12 and 13).
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// internal/replyfold/replyfold.go
//	const InquiryEligibleLatestSQL = `
//	  (latest.action = 'attributed'
//	   OR (live.action = 'task_log' AND live.resurface
//	       AND EXISTS (SELECT 1 FROM tasks lt WHERE lt.id = live.task_id AND lt.status = 'closed')))`
//	const InquiryLiveDecisionJoinSQL // LEFT JOIN LATERAL ... mode = 'live' ... ORDER BY id DESC LIMIT 1) live
//	const InquiryProjectIDSQL        // latest.project_id for attributed, else live.project_id
//	const InquiryLoggedOnTaskSQL     // 0 for attributed, else live.task_id
//
//	`latest` is each inbox's own LATERAL (newest decision, ANY mode; selects
//	cd.action and cd.project_id). `live` is InquiryLiveDecisionJoinSQL. CC5b:
//	the resurface branch reads only live decisions (a newer shadow `attributed`
//	row still wins through the attributed branch, the accepted Part A corner).
//	classify's inboxWhereInquiry and
//	promote's inquiryInbox splice the constants in; no other literal in
//	internal/classify or internal/promote names live.resurface.
//
// Edited in the SWT-53 review-fix batch (CC5b): the resurface branch moved
// from the any-mode `latest` row to the latest LIVE row, so the fragments and
// the per-inbox checks below follow it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/replyfold"
)

func rfNorm(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = strings.ReplaceAll(strings.ReplaceAll(s, "( ", "("), " )", ")")
	return regexp.MustCompile(`\s*=\s*`).ReplaceAllString(s, " = ")
}

func TestInquiryEligibleLatestSQL_IsTheWidenedPredicate(t *testing.T) {
	sql := rfNorm(replyfold.InquiryEligibleLatestSQL)
	if sql == "" {
		t.Fatalf("replyfold.InquiryEligibleLatestSQL is empty")
	}
	if !strings.HasPrefix(sql, "(") || !strings.HasSuffix(sql, ")") ||
		strings.Count(sql, "(") != strings.Count(sql, ")") {
		t.Errorf("InquiryEligibleLatestSQL = %q is not one balanced, parenthesised expression; it is AND-ed into "+
			"two WHERE clauses and an unparenthesised OR would swallow the clauses around it", sql)
	}
	for _, want := range []struct{ frag, why string }{
		// AMENDED deliberately by slack-channel-mentions (SWT-79 criterion 8): this
		// fragment pinned the old attributed-only text. The attributed branch now
		// excludes a Slack channel message capture flagged as not mentioning
		// Salvador ("channels is only when they mention me"), read from the SAME
		// any-mode `latest` row.
		{"(latest.action = 'attributed' and not latest.channel_unmentioned) or (",
			"SWT-79 D1/D2: the attributed admission AND NOT the capture-recorded channel_unmentioned fact, " +
				"parenthesised, OR-ed with the resurface branch"},
		{"live.action = 'task_log' and live.resurface",
			"the new branch reads the latest LIVE decision: a task_log capture RECORDED as resurfacing (CC3: the " +
				"lanes only read the fact; CC5b: the resurface branch reads only live decisions)"},
		{"exists (select 1 from tasks lt where lt.id = live.task_id and lt.status = 'closed')",
			"\"still closed\" is RE-READ at both stages (CC5, T7): a task reopened since takes the message out"},
	} {
		if !strings.Contains(sql, want.frag) {
			t.Errorf("InquiryEligibleLatestSQL does not contain %q — %s\n%s", want.frag, want.why, sql)
		}
	}
	// C2 for the attributed branch: the predicate itself names no mode. The
	// live restriction lives in InquiryLiveDecisionJoinSQL only.
	if regexp.MustCompile(`\bmode\b`).MatchString(sql) {
		t.Errorf("InquiryEligibleLatestSQL names mode: %s\nC2: the attributed branch reads the latest row in ANY "+
			"mode; the live restriction belongs to InquiryLiveDecisionJoinSQL", sql)
	}
	if strings.Contains(sql, "latest.resurface") || strings.Contains(sql, "latest.task_id") {
		t.Errorf("InquiryEligibleLatestSQL reads the resurface fact from the any-mode `latest` row: %s\nCC5b: a "+
			"newer shadow row would then add or remove the message", sql)
	}
}

// CC5b: the `live` row is the newest LIVE decision, and nothing else.
func TestInquiryLiveDecisionJoinSQL_IsTheLatestLiveRow(t *testing.T) {
	sql := rfNorm(replyfold.InquiryLiveDecisionJoinSQL)
	for _, want := range []string{
		"left join lateral (",
		"from capture_decisions lcd",
		"lcd.message_id = nm.id and lcd.mode = 'live'",
		"order by lcd.id desc limit 1) live on true",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("InquiryLiveDecisionJoinSQL does not contain %q:\n%s", want, sql)
		}
	}
	for _, col := range []string{"lcd.action", "lcd.project_id", "lcd.resurface", "lcd.task_id"} {
		if !strings.Contains(sql, col) {
			t.Errorf("InquiryLiveDecisionJoinSQL does not select %s:\n%s", col, sql)
		}
	}
	proj := rfNorm(replyfold.InquiryProjectIDSQL)
	if want := "case when latest.action = 'attributed' then latest.project_id else live.project_id end"; proj != want {
		t.Errorf("InquiryProjectIDSQL = %q, want %q (the attributed project unchanged; a resurfaced message is "+
			"attributed by its LIVE row, so a newer shadow row cannot drop it from the project join)", proj, want)
	}
	logged := rfNorm(replyfold.InquiryLoggedOnTaskSQL)
	if !strings.HasPrefix(logged, "case when latest.action = 'attributed' then 0 else") ||
		!strings.Contains(logged, "live.task_id") {
		t.Errorf("InquiryLoggedOnTaskSQL = %q; want 0 for an attributed admission, else the live row's task_id", logged)
	}
}

// Criterion 11: the ONE spelling. Any string literal in a non-test file of
// internal/classify or internal/promote naming live.resurface (or the live
// restriction) is a second copy of the predicate, and the two inboxes would
// drift.
func TestInquiryEligibleLatestSQL_IsTheOneSpelling(t *testing.T) {
	if !strings.Contains(replyfold.InquiryEligibleLatestSQL, "live.resurface") {
		t.Fatalf("POSITIVE CONTROL: InquiryEligibleLatestSQL itself does not name live.resurface; the scan " +
			"below would pass for the wrong reason")
	}
	root := filepath.Join("..", "..")
	scanned := 0
	for _, pkg := range []string{"internal/classify", "internal/promote"} {
		entries, err := os.ReadDir(filepath.Join(root, pkg))
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			rel := pkg + "/" + n
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, pkg, n), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			scanned++
			ast.Inspect(f, func(node ast.Node) bool {
				if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					for _, banned := range []string{"live.resurface", "latest.resurface", "lcd.mode"} {
						if strings.Contains(lit.Value, banned) {
							t.Errorf("%s spells %s in a literal of its own. Criterion 11: replyfold's "+
								"InquiryEligibleLatestSQL / InquiryLiveDecisionJoinSQL are the ONE spelling", rel, banned)
						}
					}
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned no source files; a scan with nothing to scan proves nothing")
	}

	// Both inboxes splice the constants in and no longer spell the old
	// attributed-only admission or the attributed project themselves.
	for _, in := range []struct{ rel, decl string }{
		{"internal/classify/store.go", "const inboxWhereInquiry"},
		{"internal/promote/inquiry.go", "\nfunc inquiryInbox("},
	} {
		b, err := os.ReadFile(filepath.Join(root, in.rel))
		if err != nil {
			t.Fatalf("read %s: %v", in.rel, err)
		}
		src := string(b)
		i := strings.Index(src, in.decl)
		if i < 0 {
			t.Errorf("%s no longer declares %q; the inbox moved", in.rel, strings.TrimSpace(in.decl))
			continue
		}
		region := src[i:]
		if strings.HasPrefix(in.decl, "const") {
			if j := strings.Index(region, "\n\n"); j > 0 {
				region = region[:j]
			}
		} else if j := strings.Index(region[1:], "\nfunc "); j > 0 {
			region = region[:j+1]
		}
		for _, c := range []string{"replyfold.InquiryEligibleLatestSQL", "replyfold.InquiryLiveDecisionJoinSQL",
			"replyfold.InquiryProjectIDSQL"} {
			if !strings.Contains(region, c) {
				t.Errorf("%s's %s does not use %s (CC5, CC5b: both inboxes share ONE spelling)",
					in.rel, strings.TrimSpace(in.decl), c)
			}
		}
		norm := rfNorm(region)
		if strings.Contains(norm, "latest.action = 'attributed'") {
			t.Errorf("%s's %s still spells `latest.action = 'attributed'` itself; the admission lives in "+
				"replyfold.InquiryEligibleLatestSQL only", in.rel, strings.TrimSpace(in.decl))
		}
		if strings.Contains(norm, "p.id = latest.project_id") {
			t.Errorf("%s's %s joins projects on latest.project_id; a resurfaced message is attributed by its LIVE "+
				"row (replyfold.InquiryProjectIDSQL)", in.rel, strings.TrimSpace(in.decl))
		}
	}
}

// ---- slack-channel-mentions (SWT-79) criteria 8 and 9 ---------------------------

// Criterion 8: the resurface branch is BYTE-IDENTICAL (SWT-53 unchanged), and
// the fact is read from `latest` (any mode: a newer shadow row decides), never
// from `live`.
func TestSWT79_InquiryEligibleLatestSQL_ResurfaceBranchByteIdentical(t *testing.T) {
	const resurface = `OR (live.action = 'task_log' AND live.resurface
	       AND EXISTS (SELECT 1 FROM tasks lt WHERE lt.id = live.task_id AND lt.status = 'closed')))`
	if !strings.Contains(replyfold.InquiryEligibleLatestSQL, resurface) {
		t.Errorf("InquiryEligibleLatestSQL's resurface branch is not byte-identical to SWT-53's:\n%s\nwant it to "+
			"contain:\n%s", replyfold.InquiryEligibleLatestSQL, resurface)
	}
	sql := rfNorm(replyfold.InquiryEligibleLatestSQL)
	if !strings.Contains(sql, "not latest.channel_unmentioned") {
		t.Errorf("InquiryEligibleLatestSQL does not exclude `latest.channel_unmentioned`:\n%s", sql)
	}
	if strings.Contains(sql, "live.channel_unmentioned") {
		t.Errorf("InquiryEligibleLatestSQL reads channel_unmentioned from the LIVE row; criterion 10: a newer shadow "+
			"row decides in either direction, so it is the any-mode `latest` row's fact:\n%s", sql)
	}
}

// Criterion 9: both inboxes' `latest` LATERALs select cd.channel_unmentioned
// (the spliced predicate reads latest.channel_unmentioned, so a missing select
// is a SQL error), and neither inbox spells the exclusion itself.
func TestSWT79_BothInboxesSelectTheColumnAndSpellNoAdmission(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, in := range []struct{ rel, decl string }{
		{"internal/classify/store.go", "const inboxWhereInquiry"},
		{"internal/promote/inquiry.go", "\nfunc inquiryInbox("},
	} {
		b, err := os.ReadFile(filepath.Join(root, in.rel))
		if err != nil {
			t.Fatalf("read %s: %v", in.rel, err)
		}
		src := string(b)
		i := strings.Index(src, in.decl)
		if i < 0 {
			t.Fatalf("%s no longer declares %q", in.rel, strings.TrimSpace(in.decl))
		}
		region := src[i:]
		if strings.HasPrefix(in.decl, "const") {
			if j := strings.Index(region, "\n\n"); j > 0 {
				region = region[:j]
			}
		} else if j := strings.Index(region[1:], "\nfunc "); j > 0 {
			region = region[:j+1]
		}
		norm := rfNorm(region)
		lateral := regexp.MustCompile(`join lateral \(select ([^)]*?) from capture_decisions cd`).FindStringSubmatch(norm)
		if lateral == nil {
			t.Errorf("%s's %s: no `JOIN LATERAL (SELECT … FROM capture_decisions cd` found", in.rel, strings.TrimSpace(in.decl))
			continue
		}
		if !strings.Contains(lateral[1], "cd.channel_unmentioned") {
			t.Errorf("%s's `latest` LATERAL selects %q, without cd.channel_unmentioned (criterion 9)", in.rel, lateral[1])
		}
		if strings.Contains(norm, "latest.channel_unmentioned") {
			t.Errorf("%s's %s spells latest.channel_unmentioned itself; the exclusion lives in "+
				"replyfold.InquiryEligibleLatestSQL only (one spelling)", in.rel, strings.TrimSpace(in.decl))
		}
	}
}
