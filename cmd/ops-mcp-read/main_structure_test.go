package main

// SWT-35 (docs/tickets/task-list-mcp_SPEC.md) criterion 25: ops-mcp-read is
// read-only BY CONSTRUCTION. internal/mcpserver's profile tests pin what the
// read profile LISTS; this pins that the binary builds that profile and nothing
// else, and arms nothing — a sender wired behind a read-only list is one listing
// change away from a send, and the environment it would be armed from is
// inherited, not chosen (the launching shell exports OPS_TOKEN_KEY).
//
// ZERO I/O beyond parsing this directory's non-test sources. Every file is
// scanned (a second file could wire a seam), and package names are resolved
// from each file's imports (an aliased import must not slip past).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	toolsPath     = "github.com/sspataro57/switchboard/internal/tools"
	mcpserverPath = "github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestReadBinary_BuildsOnlyTheReadProfileAndArmsNothing(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	readProfile, files := 0, 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// local name → import path, so `t "…/internal/tools"` is still tools.
		local := map[string]string{}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(path, "/internal/connector/") {
				t.Errorf("%s imports %s — the connector packages are where senders come from; the read "+
					"binary must not be able to wire one", name, path)
			}
			n := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				n = imp.Name.Name
			}
			local[n] = path
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch pkg := local[id.Name]; {
			case pkg == toolsPath && strings.HasPrefix(sel.Sel.Name, "Set"):
				t.Errorf("%s calls tools.%s — the read binary arms no seam", name, sel.Sel.Name)
			case pkg == mcpserverPath && sel.Sel.Name == "New":
				t.Errorf("%s calls mcpserver.New, the FULL profile", name)
			case pkg == mcpserverPath && sel.Sel.Name == "NewWithProfile":
				last := call.Args[len(call.Args)-1]
				arg, _ := last.(*ast.SelectorExpr)
				if arg == nil || arg.Sel.Name != "ProfileRead" {
					t.Errorf("%s builds NewWithProfile with %T %v, want mcpserver.ProfileRead", name, last, last)
				} else {
					readProfile++
				}
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("POSITIVE CONTROL FAILED: no non-test .go file in cmd/ops-mcp-read was scanned")
	}
	if readProfile != 1 {
		t.Errorf("ops-mcp-read builds the read profile %d time(s), want exactly 1 — the positive control that "+
			"this scan sees the adapter construction at all", readProfile)
	}
}
