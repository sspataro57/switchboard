package tools

// slack-auto-tier (SWT-77) criterion 1 and D2, STRUCTURALLY. ZERO I/O beyond
// parsing this package's source (send_guard_structure_test.go's idiom). It
// names no new Go symbol, so the package's test binary compiles today and the
// failures are the facts below, not a build error.
//
//   - slackSender.Send has exactly ONE call site in internal/tools (non-test
//     files). A second one is a second door to the Mac mini's browser that
//     skips sendSlackReply's phase-1 row, fence and failure model (invariant
//     3/4). This half PASSES today and must keep passing — it is the guard.
//   - send_slack_reply is registered as {"send_slack_reply",
//     validateSendSlackReply, sendSlackReplyTool} and both functions live in a
//     new delivery_slack.go (the delivery_calendar.go precedent), unexported,
//     with no exported function in that file (invariant 3: the executor is the
//     only entry point).
//   - D2: the handler COMPOSES the two existing halves — it calls draftDelivery
//     (canonical target, scrub, closed-task refusal) and sendSlackReply (the
//     bridge) rather than re-spelling either.
//
// GREENFIELD NOTE — EXPECTED RED (except the one-call-site guard):
// delivery_slack.go does not exist and createtask.go has no entry.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlackSenderSend_HasExactlyOneCallSite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Send" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "slackSender" {
				sites = append(sites, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	if len(sites) != 1 {
		t.Fatalf("slackSender.Send has %d call sites in internal/tools (%v), want exactly 1 — sendSlackReply's. "+
			"Criterion 1 / D2: send_slack_reply reaches the bridge THROUGH sendSlackReply, so the phase-1 row, "+
			"the attempt fence and the SWT-76 failure model are the only way out", len(sites), sites)
	}
	if !strings.HasPrefix(sites[0], "delivery.go:") {
		t.Errorf("the one slackSender.Send call site is %s, want it in delivery.go (sendSlackReply)", sites[0])
	}
}

func TestSendSlackReply_RegisteredWithValidatorAndHandler(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "createtask.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse createtask.go: %v", err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || len(cl.Elts) != 3 {
			return true
		}
		lit, ok := cl.Elts[0].(*ast.BasicLit)
		if !ok || lit.Value != `"send_slack_reply"` {
			return true
		}
		found = true
		v, _ := cl.Elts[1].(*ast.Ident)
		h, _ := cl.Elts[2].(*ast.Ident)
		if v == nil || v.Name != "validateSendSlackReply" {
			t.Errorf("send_slack_reply's validator is %v, want validateSendSlackReply (criterion 1)", cl.Elts[1])
		}
		if h == nil || h.Name != "sendSlackReplyTool" {
			t.Errorf("send_slack_reply's handler is %v, want sendSlackReplyTool (criterion 1)", cl.Elts[2])
		}
		return false
	})
	if !found {
		t.Fatal(`createtask.go registers no {"send_slack_reply", validateSendSlackReply, sendSlackReplyTool} ` +
			"entry (criterion 1: registered in tools.Register like every other handler, so the executor is its " +
			"only entry point)")
	}
}

func TestSendSlackReply_HandlerFileShape(t *testing.T) {
	const file = "delivery_slack.go"
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("%s does not exist: the SPEC places sendSlackReplyArgs, validateSendSlackReply and "+
			"sendSlackReplyTool there (the delivery_calendar.go precedent)", file)
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	decls := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		decls[fd.Name.Name] = fd
		if fd.Name.IsExported() {
			t.Errorf("%s declares exported func %s. Criterion 1: no exported function reaches the handler — "+
				"the executor is the only entry point (invariant 3)", file, fd.Name.Name)
		}
	}
	for _, name := range []string{"validateSendSlackReply", "sendSlackReplyTool"} {
		if decls[name] == nil {
			t.Errorf("%s does not declare %s", file, name)
		}
	}
	h := decls["sendSlackReplyTool"]
	if h == nil || h.Body == nil {
		return
	}
	calls := map[string]bool{}
	ast.Inspect(h.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				calls[id.Name] = true
			}
		}
		return true
	})
	for _, want := range []string{"draftDelivery", "sendSlackReply"} {
		if !calls[want] {
			t.Errorf("sendSlackReplyTool never calls %s. D2: the handler COMPOSES the existing halves — "+
				"draftDelivery owns canonicalisation, the scrub and the closed-task refusal; sendSlackReply owns "+
				"the bridge. A second spelling of either is the repo's most-repeated landmine", want)
		}
	}
}
