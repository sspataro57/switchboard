package replyfold_test

// chat-on-closed-task (SWT-53, docs/tickets/chat-on-closed-task_SPEC.md) CC5
// and criterion 11: both inquiry-lane inboxes widen by ONE shared predicate,
// replyfold.InquiryEligibleLatestSQL. It is the one spelling of "the latest
// capture decision makes this message eligible": `attributed`, or a
// `task_log` that capture recorded as resurfacing onto a task that is STILL
// closed. Plain unit test, no database; the behaviour is proven by the
// classify and promote integration suites (criteria 12 and 13).
//
// ---- IMPOSED SURFACE ---------------------------------------------------------
//
//	// internal/replyfold/replyfold.go
//	const InquiryEligibleLatestSQL = `
//	  (latest.action = 'attributed'
//	   OR (latest.action = 'task_log' AND latest.resurface
//	       AND EXISTS (SELECT 1 FROM tasks lt WHERE lt.id = latest.task_id AND lt.status = 'closed')))`
//
//	Anchored on a LATERAL latest-decision row aliased `latest` that selects
//	cd.action, cd.project_id, cd.resurface and cd.task_id. classify's
//	inboxWhereInquiry and promote's inquiryInbox splice the constant in; no
//	other literal in internal/classify or internal/promote names
//	latest.resurface.
//
// RED TODAY: replyfold.InquiryEligibleLatestSQL does not exist, so this
// package's test binary does not compile.

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
		{"latest.action = 'attributed' or (", "the existing admission, unchanged, OR-ed with the new branch"},
		{"latest.action = 'task_log' and latest.resurface",
			"the new branch: a task_log capture RECORDED as resurfacing (CC3: the lanes only read the fact)"},
		{"exists (select 1 from tasks lt where lt.id = latest.task_id and lt.status = 'closed')",
			"\"still closed\" is RE-READ at both stages (CC5, T7): a task reopened since takes the message out"},
	} {
		if !strings.Contains(sql, want.frag) {
			t.Errorf("InquiryEligibleLatestSQL does not contain %q — %s\n%s", want.frag, want.why, sql)
		}
	}
	if regexp.MustCompile(`\bmode\b`).MatchString(sql) {
		t.Errorf("InquiryEligibleLatestSQL names mode: %s\nC2: the latest decision in ANY mode decides", sql)
	}
}

// Criterion 11: the ONE spelling. Any string literal in a non-test file of
// internal/classify or internal/promote naming latest.resurface is a second
// copy of the predicate, and the two inboxes would drift.
func TestInquiryEligibleLatestSQL_IsTheOneSpelling(t *testing.T) {
	if !strings.Contains(replyfold.InquiryEligibleLatestSQL, "latest.resurface") {
		t.Fatalf("POSITIVE CONTROL: InquiryEligibleLatestSQL itself does not name latest.resurface; the scan " +
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
				if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
					strings.Contains(lit.Value, "latest.resurface") {
					t.Errorf("%s spells latest.resurface in a literal of its own. Criterion 11: "+
						"replyfold.InquiryEligibleLatestSQL is the ONE spelling of the widened predicate", rel)
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned no source files; a scan with nothing to scan proves nothing")
	}

	// Both inboxes splice the constant in, select the two columns it reads,
	// and no longer spell the old attributed-only admission themselves.
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
		if !strings.Contains(region, "replyfold.InquiryEligibleLatestSQL") {
			t.Errorf("%s's %s does not use replyfold.InquiryEligibleLatestSQL (CC5: both inboxes widen by ONE "+
				"shared predicate)", in.rel, strings.TrimSpace(in.decl))
		}
		for _, col := range []string{"cd.resurface", "cd.task_id"} {
			if !strings.Contains(region, col) {
				t.Errorf("%s's latest-decision LATERAL does not select %s; the predicate reads latest.resurface "+
					"and latest.task_id (CC5)", in.rel, col)
			}
		}
		if strings.Contains(rfNorm(region), "latest.action = 'attributed'") {
			t.Errorf("%s's %s still spells `latest.action = 'attributed'` itself; the admission lives in "+
				"replyfold.InquiryEligibleLatestSQL only", in.rel, strings.TrimSpace(in.decl))
		}
	}
}
