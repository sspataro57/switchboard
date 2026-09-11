package main

// SWT-41 (docs/tickets/orchestrator-deploy_SPEC.md) structural checks on the
// orchestratord binary. ZERO I/O beyond parsing Go sources.
//
//   - criterion 10 / D2: main arms no tools.Set* seam — the structural
//     guarantee that this process cannot send, even if a future rule named a
//     send verb (invariant 4). Shape copied from cmd/ops-mcp-user's
//     main_structure_test.go: every non-test file is scanned and package names
//     are resolved from each file's imports, so an aliased import cannot slip
//     past.
//   - criterion 10 / D6: SWT-41 adds no pipeline contract — no import of
//     internal/pipeline and no `ops/pipeline` topic string in cmd/orchestratord
//     or internal/orchestrator. SWT-40 Part E owns that contract; a second
//     spelling here would be the repo's recurring defect.
//   - criterion 5 / D3: no os.Exit CALL outside func main (the lost-lock exit
//     goes through an injected exit func so it is unit-testable), and the loop
//     is actually wired to the tested helpers (checkLockOrExit,
//     newHealthHandler, ORCH_HEALTH_ADDR default :8091).
//
// GREEN TODAY and must stay green: the seam scan, the os.Exit scan and the
// pipeline guard (they pin what SWT-41 must NOT change). EXPECTED RED:
// TestOrchestratord_MainWiresLockCheckAndHealthz, until main.go calls the new
// helpers. (Under a plain `go test ./cmd/orchestratord` the package
// compile-fails first on main_test.go's missing symbols.)
//
// NOT ENCODABLE HERE: "no internal/pipeline DIRECTORY introduced by this
// ticket". SWT-40 creates internal/pipeline legitimately, so a repo-state test
// that forbade the directory would go red the day SWT-40 merges. That half is a
// diff check for review (`git diff main...HEAD --stat -- internal/pipeline`
// must be empty). What IS pinned permanently: this binary and the engine do
// not import it or spell its topic.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	toolsImportPath    = "github.com/sspataro57/switchboard/internal/tools"
	pipelineImportPath = "github.com/sspataro57/switchboard/internal/pipeline"
)

type parsedFile struct {
	name  string
	file  *ast.File
	local map[string]string // local package name -> import path
}

// parseNonTest parses every non-test .go file in dir.
func parseNonTest(t *testing.T, dir string) []parsedFile {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []parsedFile
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		local := map[string]string{}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			n := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				n = imp.Name.Name
			}
			local[n] = path
		}
		out = append(out, parsedFile{name: name, file: f, local: local})
	}
	if len(out) == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file in %s was scanned", dir)
	}
	return out
}

// Criterion 10: no tools.Set* seam, no connector import.
func TestOrchestratord_ArmsNoSenderSeam(t *testing.T) {
	registers := 0
	for _, pf := range parseNonTest(t, ".") {
		for n, path := range pf.local {
			if strings.Contains(path, "/internal/connector/") {
				t.Errorf("%s imports %s (as %s) — the connector packages are where senders come from; "+
					"orchestratord must not be able to wire one (D2, invariant 4)", pf.name, path, n)
			}
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || pf.local[id.Name] != toolsImportPath {
				return true
			}
			if strings.HasPrefix(sel.Sel.Name, "Set") {
				t.Errorf("%s calls tools.%s — orchestratord arms no seam: its senders stay nil so every "+
					"send-shaped handler errors, the structural guarantee behind D2 and invariant 4",
					pf.name, sel.Sel.Name)
			}
			if sel.Sel.Name == "Register" {
				registers++
			}
			return true
		})
	}
	if registers == 0 {
		t.Error("POSITIVE CONTROL FAILED: no tools.Register call seen, so this scan is not resolving the " +
			"tools import and a tools.Set* call would be invisible to it")
	}
}

// Criterion 5: no os.Exit CALL inside any function but main. Referencing
// os.Exit as a value (passing it as the injected exit func) is fine.
func TestOrchestratord_OsExitCalledOnlyFromMain(t *testing.T) {
	for _, pf := range parseNonTest(t, ".") {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Recv == nil && fn.Name.Name == "main" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && pf.local[id.Name] == "os" && sel.Sel.Name == "Exit" {
					t.Errorf("%s: func %s calls os.Exit. Criterion 5: the lost-lock exit goes through an "+
						"injected exit func; no os.Exit inside a testable function", pf.name, fn.Name.Name)
				}
				return true
			})
		}
	}
}

// Criteria 5 and 6 wiring: the unit-tested helpers are what the binary runs.
// A tested checkLockOrExit that the loop never calls is the SWT-21 "guard whose
// column no query selected" shape again.
func TestOrchestratord_MainWiresLockCheckAndHealthz(t *testing.T) {
	calls := map[string]int{}
	lits := map[string]bool{}
	for _, pf := range parseNonTest(t, ".") {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name != fn.Name.Name {
						calls[id.Name]++
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						if s, err := strconv.Unquote(x.Value); err == nil {
							lits[s] = true
						}
					}
				}
				return true
			})
		}
	}
	if calls["checkLockOrExit"] == 0 {
		t.Error("no function in cmd/orchestratord calls checkLockOrExit. D3: the main loop checks the lock " +
			"handle on every tick and exits non-zero when it is lost")
	}
	if calls["newHealthHandler"] == 0 {
		t.Error("no function in cmd/orchestratord calls newHealthHandler. D5: orchestratord serves GET " +
			"/healthz for the Kubernetes liveness probe")
	}
	if !lits["ORCH_HEALTH_ADDR"] {
		t.Error(`cmd/orchestratord does not read "ORCH_HEALTH_ADDR" (D2/D5: the probe address)`)
	}
	if !lits[":8091"] {
		t.Error(`cmd/orchestratord has no ":8091" default for ORCH_HEALTH_ADDR (D5; hooksd owns :8090, and ` +
			`the kube hand-off's livenessProbe targets :8091)`)
	}
}

// Criterion 10 / D6: SWT-41 defines no pipeline contract.
func TestSWT41_OrchestratorSpellsNoPipelineContract(t *testing.T) {
	needle := "ops/" + "pipeline" // built, so this file does not contain the needle it scans for
	for _, dir := range []string{".", filepath.Join("..", "..", "internal", "orchestrator")} {
		for _, pf := range parseNonTest(t, dir) {
			for _, path := range pf.local {
				if path == pipelineImportPath || strings.HasPrefix(path, pipelineImportPath+"/") {
					t.Errorf("%s imports %s. D6 (O4 = a): SWT-41 adds no pipeline subscriber or publisher; "+
						"the contract has one spelling, SWT-40 Part E", pf.name, path)
				}
			}
			src, err := os.ReadFile(pf.name)
			if err != nil {
				t.Fatalf("read %s: %v", pf.name, err)
			}
			if strings.Contains(string(src), needle) {
				t.Errorf("%s contains %q. D6: SWT-41 adds no ops/pipeline topic; writing one here would be a "+
					"second spelling of SWT-40's boundary", pf.name, needle)
			}
		}
	}
}
