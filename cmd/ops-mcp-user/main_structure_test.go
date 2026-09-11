package main

// SWT-37 (docs/tickets/mcp-task-verbs_SPEC.md) criterion 18, MOVED from
// cmd/ops-mcp-read/main_structure_test.go (SWT-35 criterion 25). ops-mcp-user
// is narrow BY CONSTRUCTION: internal/mcpserver's profile tests pin what the
// user profile LISTS; this pins that the binary builds that profile and nothing
// else, and arms nothing. A sender wired behind a narrow list is one listing
// change away from a send, and the environment it would be armed from is
// inherited, not chosen (the launching shell exports OPS_TOKEN_KEY — the SWT-35
// landmine).
//
// IMPOSED SURFACE (SPEC V3/V4):
//
//	cmd/ops-mcp-user/main.go — `git mv` of cmd/ops-mcp-read/main.go, building
//	mcpserver.NewWithProfile(ex, workerID, mcpserver.ProfileUser) exactly once
//	and serving under the name "ops-mcp-user". cmd/ops-mcp-read must not exist.
//
// GREENFIELD NOTE — EXPECTED RED. This directory holds only this test until the
// implementer moves main.go here, so the positive control fails ("no non-test
// .go file ... was scanned") and so does TestUserBinary_OldDirectoryIsGone
// (cmd/ops-mcp-read still exists). NOTE FOR THE MOVE: this directory already
// exists, so `git mv cmd/ops-mcp-read cmd/ops-mcp-user` would nest the old
// directory inside it. Move the FILE (`git mv cmd/ops-mcp-read/main.go
// cmd/ops-mcp-user/main.go`); the old test was removed with this commit.
//
// ZERO I/O beyond parsing this directory's non-test sources and one os.Stat.
// Every file is scanned (a second file could wire a seam), and package names
// are resolved from each file's imports (an aliased import must not slip past).

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	toolsPath     = "github.com/sspataro57/switchboard/internal/tools"
	mcpserverPath = "github.com/sspataro57/switchboard/internal/mcpserver"
)

func TestUserBinary_BuildsOnlyTheUserProfileAndArmsNothing(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	userProfile, files := 0, 0
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
				t.Errorf("%s imports %s — the connector packages are where senders come from; the user "+
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
				t.Errorf("%s calls tools.%s — the user binary arms no seam (no sender, no booker)", name, sel.Sel.Name)
			case pkg == mcpserverPath && sel.Sel.Name == "New":
				t.Errorf("%s calls mcpserver.New, the FULL profile", name)
			case pkg == mcpserverPath && sel.Sel.Name == "NewWithProfile":
				last := call.Args[len(call.Args)-1]
				arg, _ := last.(*ast.SelectorExpr)
				if arg == nil || arg.Sel.Name != "ProfileUser" {
					t.Errorf("%s builds NewWithProfile with %T %v, want mcpserver.ProfileUser", name, last, last)
				} else {
					userProfile++
				}
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("POSITIVE CONTROL FAILED: no non-test .go file in cmd/ops-mcp-user was scanned")
	}
	if userProfile != 1 {
		t.Errorf("ops-mcp-user builds the user profile %d time(s), want exactly 1 — the positive control that "+
			"this scan sees the adapter construction at all", userProfile)
	}
}

// V4: the old directory must not exist, so a stale `go install
// ./cmd/ops-mcp-read` cannot silently rebuild the old surface, and the
// runbook's `rm -f …/ops-mcp-read` is the last trace of it.
func TestUserBinary_OldDirectoryIsGone(t *testing.T) {
	_, err := os.Stat("../ops-mcp-read")
	if err == nil {
		t.Fatal("cmd/ops-mcp-read still exists: SWT-37 V4 renames it to cmd/ops-mcp-user in one commit; a " +
			"leftover directory lets `go install ./cmd/ops-mcp-read` rebuild a binary whose name contradicts its surface")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("os.Stat(../ops-mcp-read) = %v, want not-exist", err)
	}
}
