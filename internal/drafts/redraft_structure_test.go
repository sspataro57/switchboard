package drafts_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criterion 25: locality (SWT-21)
// is unchanged by the redraft section, and that is STATED in a code comment
// beside the new prompt section — the rejected body was produced from this
// same task's context, and the note is Salvador's own instruction, so neither
// adds an attribution the class fold must see. ZERO I/O beyond parsing
// drafts.go.
//
// Why pin a comment: in a boundary file the comment is what the next session
// trusts (IK, "a comment can be a defect"). A later edit that feeds some OTHER
// row's body into this section would need the fold to change; the comment is
// where that reasoning lives.
//
// GREENFIELD NOTE — EXPECTED RED: renderUser has no comment at all today.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestDrafts_RenderUserStatesWhyTheRedraftSectionLeavesLocalityUnchanged(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "drafts.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse drafts.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "renderUser" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatalf("drafts.go declares no renderUser; criterion 22 appends the redraft section there")
	}
	var mentionsRedraft bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "RedraftOf" {
			mentionsRedraft = true
		}
		return true
	})
	if !mentionsRedraft {
		t.Errorf("renderUser never reads RedraftOf; criterion 22 appends the section only when RedraftOf != 0")
	}
	stated := false
	for _, cg := range f.Comments {
		if cg.Pos() < fn.Pos() || cg.End() > fn.End() {
			continue
		}
		text := strings.ToLower(cg.Text())
		if strings.Contains(text, "swt-21") || strings.Contains(text, "locality") {
			stated = true
		}
	}
	if !stated {
		t.Errorf("no comment inside renderUser mentions SWT-21 / locality. Criterion 25: state beside the new " +
			"section why it leaves the class fold unchanged (same task's own text; the note is Salvador's " +
			"instruction)")
	}
}
