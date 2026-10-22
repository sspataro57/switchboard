package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SWT-109 criterion 25: cmd/pipelined arms tools.SetCalendarBooker and NO
// other seam, and only when the calendar_push stage is enabled. Every other
// stage runs without a calendar writer.
func TestPipelined_ArmsOnlyTheCalendarBookerBehindTheStage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var calls []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "tools" && strings.HasPrefix(sel.Sel.Name, "Set") {
				calls = append(calls, f+":"+sel.Sel.Name)
			}
			return true
		})
	}
	if len(calls) != 1 || calls[0] != "calendarpush.go:SetCalendarBooker" {
		t.Fatalf("tools.Set* calls in cmd/pipelined = %v, want exactly [calendarpush.go:SetCalendarBooker]", calls)
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(main)
	guard := strings.Index(src, "if slices.Contains(stages, pipeline.StageCalendarPush) {")
	arm := strings.Index(src, "armCalendarBooker()")
	if guard < 0 || arm < 0 || arm < guard || strings.Count(src, "armCalendarBooker()") != 1 {
		t.Errorf("main.go must call armCalendarBooker() exactly once, inside the calendar_push stage check")
	}
}
