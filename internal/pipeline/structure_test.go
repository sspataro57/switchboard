package pipeline_test

// SWT-40 Part E structural checks (docs/tickets/inquiry-promote_SPEC.md). ZERO
// I/O beyond parsing Go sources.
//
//   - E1: no `retain=true` publish of an `ops/pipeline/` topic ANYWHERE in the
//     repo (cmd/ and internal/, test files included). A retained wake-up
//     re-fires on every reconnect, and retained state is global on the
//     production broker (IK MQTT landmine).
//   - E2: every connector main that runs a capture pass announces it with
//     pipeline.AnnounceCaptured, AFTER capture.EvaluateRules (the V3 mutation
//     "publish before the commit" turns this red), naming its own connector
//     and passing the stats the pass returned.
//
// GREENFIELD NOTE: compile-FAILs with the rest of package pipeline_test until
// internal/pipeline exists. Once it compiles, the retain scan passes on today's
// tree (it pins what E must NOT introduce) and the connector-main test is RED
// until the four mains call AnnounceCaptured.
//
// The retain rule, precisely:
//   - Outside internal/pipeline: a 4-argument `.Publish(topic, qos, retained,
//     payload)` whose topic expression spells "ops/pipeline" or calls the
//     pipeline package's Topic (through whatever local name the file imports it
//     as) must pass the literal `false` as retained.
//   - Inside internal/pipeline's own non-test files, EVERY 4-argument Publish
//     must pass literal `false`, unless its topic is a fleet.StatusTopic call
//     (the retained heartbeat). There the topic is often a variable, so the
//     topic text cannot be trusted to reveal it.
//   - A will (SetWill / SetBinaryWill) on a pipeline topic is flagged too.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const pipelineImportPath = "github.com/sspataro57/switchboard/internal/pipeline"
const captureImportPath = "github.com/sspataro57/switchboard/internal/capture"

func repoRoot() string { return filepath.Join("..", "..") }

// retainViolations returns one line per retained pipeline publish in f.
// inPipelinePkg selects the stricter in-package rule.
func retainViolations(fset *token.FileSet, f *ast.File, inPipelinePkg bool) []string {
	pipelineNames := map[string]bool{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != pipelineImportPath {
			continue
		}
		name := "pipeline"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		pipelineNames[name] = true
	}
	src := func(n ast.Node) string {
		var b bytes.Buffer
		_ = printer.Fprint(&b, fset, n)
		return b.String()
	}
	isPipelineTopic := func(e ast.Expr) bool {
		s := src(e)
		if strings.Contains(s, "ops/pipeline") {
			return true
		}
		for name := range pipelineNames {
			if strings.Contains(s, name+".Topic(") {
				return true
			}
		}
		if inPipelinePkg && strings.Contains(s, "Topic(") && !strings.Contains(s, "StatusTopic(") {
			return true
		}
		return false
	}
	isFalse := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "false"
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos())
		switch sel.Sel.Name {
		case "Publish":
			if len(call.Args) != 4 {
				return true
			}
			topic, retained := call.Args[0], call.Args[2]
			if isFalse(retained) {
				return true
			}
			if isPipelineTopic(topic) {
				out = append(out, pos.String()+": "+src(call))
				return true
			}
			if inPipelinePkg && !strings.Contains(src(topic), "StatusTopic(") {
				out = append(out, pos.String()+": "+src(call)+" (inside internal/pipeline, retained must be the literal false)")
			}
		case "SetWill", "SetBinaryWill":
			if len(call.Args) >= 1 && isPipelineTopic(call.Args[0]) {
				out = append(out, pos.String()+": "+src(call)+" (a will on a wake-up topic)")
			}
		}
		return true
	})
	return out
}

func parseSource(t *testing.T, name, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse probe %s: %v", name, err)
	}
	return fset, f
}

// Positive control: a scanner that matches nothing passes everything.
func TestRetainScan_FlagsItsProbes(t *testing.T) {
	flagged := []string{
		`package x
import p "github.com/sspataro57/switchboard/internal/pipeline"
func f(c interface{ Publish(string, byte, bool, []byte) error }) { c.Publish(p.Topic(p.EventCaptured), 1, true, nil) }`,
		`package x
func f(c interface{ Publish(string, byte, bool, []byte) error }) { c.Publish("ops/pipeline/captured", 1, true, nil) }`,
		`package x
import "github.com/sspataro57/switchboard/internal/pipeline"
func f(c interface{ Publish(string, byte, bool, []byte) error }, r bool) { c.Publish(pipeline.Topic("gated"), 1, r, nil) }`,
		`package x
func f(o interface{ SetBinaryWill(string, []byte, byte, bool) }) { o.SetBinaryWill("ops/pipeline/captured", nil, 1, true) }`,
	}
	for i, s := range flagged {
		fset, f := parseSource(t, "probe.go", s)
		if v := retainViolations(fset, f, false); len(v) == 0 {
			t.Errorf("probe %d is a retained pipeline publish and was NOT flagged:\n%s", i, s)
		}
	}
	// In-package: a variable topic with a non-false retained is flagged.
	fset, f := parseSource(t, "probe.go", `package pipeline
func f(c interface{ Publish(string, byte, bool, []byte) error }, topic string) { c.Publish(topic, 1, true, nil) }`)
	if v := retainViolations(fset, f, true); len(v) == 0 {
		t.Errorf("in-package retained publish on a variable topic was NOT flagged")
	}

	clean := []string{
		`package x
import "github.com/sspataro57/switchboard/internal/pipeline"
func f(c interface{ Publish(string, byte, bool, []byte) error }) { c.Publish(pipeline.Topic("captured"), 1, false, nil) }`,
		`package x
import "github.com/sspataro57/switchboard/internal/fleet"
func f(c interface{ Publish(string, byte, bool, []byte) error }) { c.Publish(fleet.StatusTopic("pipeline.gate"), 1, true, nil) }`,
	}
	for i, s := range clean {
		fset, f := parseSource(t, "probe.go", s)
		if v := retainViolations(fset, f, false); len(v) != 0 {
			t.Errorf("clean probe %d was flagged: %v", i, v)
		}
	}
	fset, f = parseSource(t, "probe.go", `package pipeline
import "github.com/sspataro57/switchboard/internal/fleet"
func f(c interface{ Publish(string, byte, bool, []byte) error }) { c.Publish(fleet.StatusTopic(WorkerID("gate")), 1, true, nil) }`)
	if v := retainViolations(fset, f, true); len(v) != 0 {
		t.Errorf("in-package retained STATUS publish was flagged: %v", v)
	}
}

// E1: nothing in the repo publishes a pipeline wake-up retained.
func TestNoRetainedPipelinePublishAnywhere(t *testing.T) {
	root := repoRoot()
	pipelineDir := filepath.Join(root, "internal", "pipeline")
	scanned := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); n == "testdata" || n == "vendor" || n == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			scanned++
			inPkg := filepath.Dir(path) == pipelineDir && !strings.HasSuffix(path, "_test.go")
			for _, v := range retainViolations(fset, f, inPkg) {
				t.Errorf("retained pipeline publish: %s\n"+
					"ops/pipeline/* is QoS 1, NOT retained (SWT-40 E-D3): a retained wake-up re-fires on every "+
					"reconnect, and retained state is global on the production broker.", v)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files from %s; the walk is not seeing the repo", scanned, root)
	}
}

// E2: every connector main that runs a capture pass announces it.
func TestConnectorMainsAnnounceCapturedAfterTheirPass(t *testing.T) {
	root := repoRoot()
	dirs, err := filepath.Glob(filepath.Join(root, "cmd", "connectors", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	withCapture := 0
	for _, dir := range dirs {
		connector := filepath.Base(dir)
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			local := map[string]string{}
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				n := p[strings.LastIndex(p, "/")+1:]
				if imp.Name != nil {
					n = imp.Name.Name
				}
				local[n] = p
			}
			isCall := func(call *ast.CallExpr, pkgPath, fn string) bool {
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != fn {
					return false
				}
				id, ok := sel.X.(*ast.Ident)
				return ok && local[id.Name] == pkgPath
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var evalPos token.Pos
				var statsName string
				var announces []*ast.CallExpr
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.AssignStmt:
						if len(x.Rhs) == 1 {
							if call, ok := x.Rhs[0].(*ast.CallExpr); ok && isCall(call, captureImportPath, "EvaluateRules") {
								evalPos = call.Pos()
								if id, ok := x.Lhs[0].(*ast.Ident); ok {
									statsName = id.Name
								}
							}
						}
					case *ast.CallExpr:
						if isCall(x, pipelineImportPath, "AnnounceCaptured") {
							announces = append(announces, x)
						}
					}
					return true
				})
				if evalPos == token.NoPos {
					continue
				}
				withCapture++
				where := filepath.ToSlash(filepath.Join("cmd", "connectors", connector, filepath.Base(path))) + " func " + fn.Name.Name
				if len(announces) != 1 {
					t.Errorf("%s runs capture.EvaluateRules and calls pipeline.AnnounceCaptured %d times, want exactly 1 "+
						"(SWT-40 E2: publish `captured` after the capture pass)", where, len(announces))
					continue
				}
				a := announces[0]
				if a.Pos() < evalPos {
					t.Errorf("%s announces BEFORE capture.EvaluateRules: a wake that arrives before the decisions "+
						"commit sends a consumer to an inbox that is still empty (SWT-40 E2/V3)", where)
				}
				if len(a.Args) != 4 {
					t.Errorf("%s: AnnounceCaptured has %d args, want (ctx, broker, connector, stats)", where, len(a.Args))
					continue
				}
				if lit, ok := a.Args[2].(*ast.BasicLit); !ok || lit.Value != strconv.Quote(connector) {
					t.Errorf("%s: AnnounceCaptured's connector argument must be the literal %q (the wake's source and "+
						"the spine client id switchboard-capture-%s)", where, connector, connector)
				}
				if id, ok := a.Args[3].(*ast.Ident); !ok || id.Name != statsName {
					t.Errorf("%s: AnnounceCaptured must be passed the stats capture.EvaluateRules returned (%q)", where, statsName)
				}
			}
		}
		if src, err := os.ReadFile(filepath.Join(dir, "main.go")); err == nil &&
			strings.Contains(string(src), "EvaluateRules") && !strings.Contains(string(src), `"MQTT_BROKER"`) {
			t.Errorf("cmd/connectors/%s/main.go runs a capture pass but never reads MQTT_BROKER (E-D3: connector "+
				"mains need MQTT_BROKER; unset skips the publish)", connector)
		}
	}
	// google, jira, slackweb and upworkcrm run a capture pass today; github does not.
	if withCapture < 4 {
		t.Fatalf("found %d connector funcs calling capture.EvaluateRules, want at least 4; the scan is blind", withCapture)
	}
}
