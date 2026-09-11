package tools

// SWT-37 criterion 29 (go-reviewer): every function that approves, sends,
// books or prefills a delivery calls refuseClosedTask, so closed work never
// gets words in front of a client. The integration tests exercise approve,
// the Slack send and prefill end to end; this pins the other paths (gmail,
// Jira, calendar book and send) structurally, so deleting a guard is red even
// where no fixture drives that channel. ZERO I/O beyond parsing two files.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestSendPaths_AllCallRefuseClosedTask(t *testing.T) {
	want := map[string]bool{
		"approveDelivery":   false,
		"sendDelivery":      false,
		"sendJiraComment":   false,
		"sendSlackReply":    false,
		"prefillDelivery":   false,
		"bookCalendarBlock": false,
		"sendCalendarBlock": false,
	}
	for _, file := range []string{"delivery.go", "delivery_calendar.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if _, tracked := want[fd.Name.Name]; !tracked {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuseClosedTask" {
						want[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	for fn, guarded := range want {
		if !guarded {
			t.Errorf("%s never calls refuseClosedTask: a delivery on a CLOSED task could be approved, sent, "+
				"booked or prefilled through it (SWT-37 criterion 29)", fn)
		}
	}
}
