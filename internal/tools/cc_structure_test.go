package tools

// gmail-delivery-cc (SWT-69) criterion 11 + invariant 4(c): send_delivery reads
// the delivery's `cc` in the SAME `FOR UPDATE OF d` SELECT as body/subject/
// thread and builds the outbound message from it INSIDE that transaction — no
// second read, no read after the lock is released.
//
// This is the structural MINIMUM (SPEC mutation 5). The behavioural half is
// TestSendDelivery_Integration_CcOnTheWire, and the IK rule from SWT-21 defect
// 6 applies to it: mutate the phase-1 SELECT's `d.cc` to a literal `'{}'` and
// that integration test must go red, otherwise it is feeding itself its own
// fixture. This test alone cannot tell a locked read from a second query, which
// is why it is the minimum and not the contract.
//
// ZERO I/O: it parses delivery.go.
//
// EXPECTED RED: the phase-1 SELECT does not mention cc.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestSendDelivery_Phase1SelectReadsTheCc(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "delivery.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse delivery.go: %v", err)
	}

	var locked []string // every string literal inside sendDelivery that locks the row
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Name.Name != "sendDelivery" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "FOR UPDATE OF d") {
				locked = append(locked, lit.Value)
			}
			return true
		})
	}
	if len(locked) == 0 {
		t.Fatal("POSITIVE CONTROL FAILED: sendDelivery has no `FOR UPDATE OF d` SELECT; this test is looking at " +
			"the wrong function")
	}
	for _, q := range locked {
		if strings.Contains(q, "d.cc") {
			return
		}
	}
	t.Errorf("sendDelivery's phase-1 locked SELECT does not read d.cc:\n%s\n\nCriterion 11: the cc must be read "+
		"in the same transaction that commits `sending` and the reserved Message-ID, so the send can never use a "+
		"Cc a concurrent writer changed after the lock (invariant 4c).", strings.Join(locked, "\n"))
}
