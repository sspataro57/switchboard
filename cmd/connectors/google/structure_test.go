package main

// imap-idle-watch (SWT-73) structural checks on the google connector binary,
// which has two drivers: the one-shot CronJob pass (the 2-hourly net under
// D3/OQ-1 = B-reduced) and the resident `--watch` loop this ticket deploys.
// ZERO I/O beyond parsing Go sources and reading files in the repo.
//
//   - criterion 1:  lockkeys.MailWatch is 0x5157_0010, declared ONCE, in the
//                   import-free internal/lockkeys.
//   - criterion 3:  no os.Exit CALL outside func main — the lost-lock exit goes
//                   through runWatch's error return (cmd/orchestratord's shape).
//   - criterion 7:  the health handler's inputs are the pass clock and the lock
//                   handle ONLY; nothing about IDLE state reaches it.
//   - criteria 11/12: the startup path resolves the capture config once and
//                   prints one line (checkCaptureConfig + startupLine wired).
//   - criterion 13: watchMain never calls selectMailSource, and main.go's header
//                   says MAIL_SOURCE is irrelevant in watch mode.
//   - criterion 15: watchPass runs the same five calls in the same order, with
//                   the same counter line and the same AnnounceCaptured.
//   - criterion 16: the connector arms NO tools.Set* seam (invariant 4).
//   - criterion 17: the capture announce keeps the connector string "google"
//                   and a per-connection random client id.
//   - criterion 18: no migration and no dashboard/MCP surface is added here.
//   - criterion 20: the watcher writes exactly the phases "imap" and "imap_idle".
//   - D2/D7: the ":8092" default and the two new env names the hand-off's
//                   env-parity table lists.
//
// IMPOSED SURFACE (package main; the SPEC names the FILES —
// cmd/connectors/google/{health.go,singleton.go} "twins of cmd/orchestratord's"
// — and fixes the behaviour; these names are chosen here, copied from
// cmd/connectors/slackweb, which D1/D4/D7 name as the pattern to re-spell
// locally: a connector must not import internal/orchestrator, invariant 7):
//
//	// internal/lockkeys/lockkeys.go
//	const MailWatch int64 = 0x5157_0010
//
//	// cmd/connectors/google/singleton.go (new)
//	type watchLock struct{ ... }
//	func (l *watchLock) Alive(ctx context.Context) error
//	func (l *watchLock) Release()
//	func tryMailWatchLock(ctx context.Context, pool *pgxpool.Pool) (*watchLock, bool, error)
//
//	// cmd/connectors/google/health.go (new)
//	type aliveChecker interface{ Alive(ctx context.Context) error }
//	var errStandby error
//	type standbyLock struct{} // Alive always answers errStandby
//	func newWatchHealthHandler(reconcile time.Duration, now func() time.Time,
//	    lastPass func() time.Time, lock aliveChecker) http.Handler
//
//	// cmd/connectors/google/watch.go
//	type watchConfig struct{ Reconcile, IdleRefresh, PassTimeout, StandbyRetry time.Duration; HealthAddr string }
//	func watchConfigFromEnv() watchConfig
//	func checkCaptureConfig(cfg capture.RulesConfig) error
//	func startupLine(cfg watchConfig, rules capture.RulesConfig, accounts int) string
//	type lockHandle interface{ Alive(ctx context.Context) error; Release() }
//	type watchDeps struct{ ... }
//	func newWatcher(cfg watchConfig, mail google.Config, deps watchDeps) *watcher
//	func (w *watcher) Run(ctx context.Context) error
//
// GREENFIELD NOTE — EXPECTED RED: lockkeys.MailWatch does not exist, so this
// file does not compile ("undefined: lockkeys.MailWatch"), and neither do the
// other new test files in this package. That is the expected failure mode for
// every assertion below that names a not-yet-written symbol. The scans that are
// GREEN TODAY and must STAY green are marked per test: they pin what this
// ticket must NOT change (criteria 15, 16, 17, 18, 19).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/lockkeys"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

const mwToolsImportPath = "github.com/sspataro57/switchboard/internal/tools"

type mwParsedFile struct {
	name  string
	file  *ast.File
	local map[string]string // local package name -> import path
}

func mwParseNonTest(t *testing.T, dir string) []mwParsedFile {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []mwParsedFile
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
		out = append(out, mwParsedFile{name: name, file: f, local: local})
	}
	if len(out) == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file in %s was scanned", dir)
	}
	return out
}

// mwFuncDecl finds a top-level func (or method) by name.
func mwFuncDecl(t *testing.T, fname string) (*ast.FuncDecl, string, bool) {
	t.Helper()
	for _, pf := range mwParseNonTest(t, ".") {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == fname && fn.Body != nil {
				return fn, pf.name, true
			}
		}
	}
	return nil, "", false
}

// mwCallsIn returns, in SOURCE ORDER, every call inside the named function,
// rendered as "pkg.Func" or "Func".
func mwCallsIn(t *testing.T, fname string) ([]string, bool) {
	t.Helper()
	fn, _, ok := mwFuncDecl(t, fname)
	if !ok {
		return nil, false
	}
	var calls []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := f.X.(*ast.Ident); ok {
				calls = append(calls, x.Name+"."+f.Sel.Name)
			} else {
				calls = append(calls, f.Sel.Name)
			}
		case *ast.Ident:
			calls = append(calls, f.Name)
		}
		return true
	})
	return calls, true
}

func mwIndexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want || strings.HasSuffix(c, "."+want) {
			return i
		}
	}
	return -1
}

// mwAllSource concatenates every non-test source file in this package.
func mwAllSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, pf := range mwParseNonTest(t, ".") {
		raw, err := os.ReadFile(pf.name)
		if err != nil {
			t.Fatalf("read %s: %v", pf.name, err)
		}
		b.Write(raw)
	}
	return b.String()
}

// ---- criterion 1: the singleton key ------------------------------------------

// Criterion 1: one advisory key, in the import-free internal/lockkeys, with the
// low four hex digits SWT-11 decision 16 reserved. The literal is deliberately
// NOT restated anywhere else: internal/classify's repo-wide collision scan
// (internal/classify/structure_test.go) reads a restated 0x5157 literal as a
// second owner, and a duplicate key is two processes silently excluding each
// other on a schedule with no error.
//
// MUTATION: give MailWatch a key another workload already owns -> this test and
// the repo-wide collision scan.
func TestMailWatchStructure_LockKeyIsDeclaredOnceAndIs0010(t *testing.T) {
	keyPattern := regexp.MustCompile(`0x5157_?([0-9A-Fa-f]{4})`)
	declPattern := regexp.MustCompile(`MailWatch\s+int64\s*=\s*(0x5157_?[0-9A-Fa-f]{4})`)

	path := filepath.Join("..", "..", "..", "internal", "lockkeys", "lockkeys.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read internal/lockkeys/lockkeys.go: %v", err)
	}
	m := declPattern.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("internal/lockkeys declares no `MailWatch int64 = 0x5157_00NN`. D4: the key lives in the "+
			"import-free internal/lockkeys because the repo-wide collision scan walks internal/ only, and "+
			"because a connector must not import internal/orchestrator to share one constant "+
			"(invariant 7):\n%s", raw)
	}
	digits := strings.ToLower(keyPattern.FindStringSubmatch(string(m[1]))[1])
	if digits != "0010" {
		t.Errorf("lockkeys.MailWatch's low four hex digits are %q, want \"0010\" — D4 takes SWT-11 decision "+
			"16's own suggestion, free today (0005 orchestrator, 0006 triage, 0007 google per-account, "+
			"0011 slack watch, 0015 capture, 0021 promote, 0022 classify, 0023 ticketstatus, 0028 calendar "+
			"booking)", digits)
	}
	want, err := strconv.ParseInt("5157"+digits, 16, 64)
	if err != nil {
		t.Fatalf("parse the key: %v", err)
	}
	if lockkeys.MailWatch != want {
		t.Errorf("lockkeys.MailWatch = %#x but its source literal is %#x", lockkeys.MailWatch, want)
	}
	for name, other := range map[string]int64{"Orchestrator": lockkeys.Orchestrator, "SlackWatch": lockkeys.SlackWatch} {
		if lockkeys.MailWatch == other {
			t.Errorf("lockkeys.MailWatch collides with lockkeys.%s: two processes sharing an advisory-lock "+
				"key silently exclude each other, on a schedule, with no error (criterion 1)", name)
		}
	}
	// A doc comment naming this workload (criterion 1: "with a doc comment
	// naming this workload"). Grepping `MailWatch` in six months must say what
	// holds it.
	text := string(raw)
	idx := strings.Index(text, "MailWatch int64")
	if idx < 0 {
		t.Fatal("MailWatch is declared but not as `MailWatch int64 = ...`")
	}
	doc := text[:idx]
	if !strings.Contains(doc, "// MailWatch") {
		t.Errorf("lockkeys.MailWatch has no doc comment starting `// MailWatch`; criterion 1 asks for one " +
			"naming this workload (connector-google-watch, the resident IMAP IDLE loop)")
	}

	// One owner under internal/, so the repo-wide collision scan stays green.
	owners := map[string]bool{}
	err = filepath.Walk(filepath.Join("..", "..", "..", "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for _, lit := range keyPattern.FindAllStringSubmatch(string(b), -1) {
			if strings.ToLower(lit[1]) == digits {
				owners[p] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(owners) != 1 {
		t.Errorf("the key 0x5157_%s appears in %d files under internal/ (%v), want exactly one "+
			"(internal/lockkeys/lockkeys.go). A key restated elsewhere reads as a second owner to "+
			"internal/classify's repo-wide collision scan (criterion 1)", digits, len(owners), owners)
	}
}

// ---- criterion 3: os.Exit lives in main --------------------------------------

// Criterion 3: losing the lock makes runWatch RETURN a non-nil error; main is
// the only place that calls os.Exit. That is what makes the lost-lock path
// unit-testable at all (cmd/orchestratord/structure_test.go:16-19's shape) —
// a watcher that called os.Exit from inside the loop could only be tested by
// running a subprocess.
//
// GREEN TODAY (main.go already exits only in main) and must stay green as the
// loop grows a lock.
func TestMailWatchStructure_NoOsExitOutsideMain(t *testing.T) {
	seen := 0
	for _, pf := range mwParseNonTest(t, ".") {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
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
				id, ok := sel.X.(*ast.Ident)
				if !ok || pf.local[id.Name] != "os" || sel.Sel.Name != "Exit" {
					return true
				}
				seen++
				if fn.Name.Name != "main" || fn.Recv != nil {
					t.Errorf("%s: os.Exit is called inside %s. Criterion 3: the lock-loss exit is a non-nil "+
						"error RETURNED by runWatch and only main turns it into an exit code — otherwise the "+
						"lost-lock path can only be tested by spawning a process", pf.name, fn.Name.Name)
				}
				return true
			})
		}
	}
	if seen == 0 {
		t.Error("POSITIVE CONTROL FAILED: no os.Exit call was seen anywhere in cmd/connectors/google, so " +
			"this scan is not resolving the os import and a stray os.Exit would be invisible to it")
	}
}

// ---- criterion 7: what the health verdict may look at -------------------------

// Criterion 7, structural half: "the health handler's inputs are the pass clock
// and the lock handle only". D7 excludes IDLE state from the verdict on
// purpose — one mailbox in backoff must not restart the pod, because a restart
// cannot fix invalid_grant and would thrash the three healthy mailboxes.
// Pinning the SIGNATURE is what stops a later "…, accounts []accountState"
// parameter from quietly re-admitting it.
//
// MUTATION: make /healthz fail when any account is in backoff -> criteria 7, 9.
func TestMailWatchStructure_HealthHandlerTakesOnlyTheClockAndTheLock(t *testing.T) {
	fn, file, ok := mwFuncDecl(t, "newWatchHealthHandler")
	if !ok {
		t.Fatalf("no func newWatchHealthHandler in cmd/connectors/google. D7: GET /healthz on :8092 is the " +
			"twin of cmd/orchestratord's newHealthHandler, re-spelled locally (a connector must not import " +
			"the orchestrator, invariant 7)")
	}
	var params []string
	for _, field := range fn.Type.Params.List {
		rendered := mwRenderType(field.Type)
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			params = append(params, rendered)
		}
	}
	want := []string{"time.Duration", "func() time.Time", "func() time.Time", "aliveChecker"}
	if strings.Join(params, ", ") != strings.Join(want, ", ") {
		t.Errorf("%s: newWatchHealthHandler(%s), want (%s). Criterion 7: the verdict's ONLY inputs are the "+
			"reconcile interval, the clock, the last COMPLETED pass and the lock handle. An account list, an "+
			"IDLE state or a *pgxpool.Pool in this signature is how \"one mailbox in backoff\" becomes \"the "+
			"kubelet restarts the pod\" — which cannot fix invalid_grant and thrashes the healthy mailboxes",
			file, strings.Join(params, ", "), strings.Join(want, ", "))
	}
	// Nothing about idling may be named inside the handler at all.
	var body strings.Builder
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			body.WriteString(id.Name + " ")
		}
		return true
	})
	for _, banned := range []string{"idle", "backoff", "watching", "accounts"} {
		if strings.Contains(strings.ToLower(body.String()), banned) {
			t.Errorf("newWatchHealthHandler's body names %q. D7 keeps IDLE state, mail volume and the "+
				"account set OUT of the health verdict (criterion 7)", banned)
		}
	}
}

func mwRenderType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return mwRenderType(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + mwRenderType(t.X)
	case *ast.FuncType:
		var in, out []string
		if t.Params != nil {
			for _, f := range t.Params.List {
				n := len(f.Names)
				if n == 0 {
					n = 1
				}
				for i := 0; i < n; i++ {
					in = append(in, mwRenderType(f.Type))
				}
			}
		}
		if t.Results != nil {
			for _, f := range t.Results.List {
				n := len(f.Names)
				if n == 0 {
					n = 1
				}
				for i := 0; i < n; i++ {
					out = append(out, mwRenderType(f.Type))
				}
			}
		}
		s := "func(" + strings.Join(in, ", ") + ")"
		if len(out) == 1 {
			s += " " + out[0]
		} else if len(out) > 1 {
			s += " (" + strings.Join(out, ", ") + ")"
		}
		return s
	case *ast.ArrayType:
		return "[]" + mwRenderType(t.Elt)
	case *ast.ChanType:
		return "chan " + mwRenderType(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.Ellipsis:
		return "..." + mwRenderType(t.Elt)
	default:
		return "?"
	}
}

// ---- criteria 11/12: the startup path is wired --------------------------------

// Criteria 11 and 12: one resolution of the capture config at startup, one
// refusal, one line. D8's landmine is that a CronJob failing this check is one
// red run an operator sees, while a resident loop failing it "logs an error on
// every wake and looks alive while capturing nothing".
//
// MUTATIONS: accept a sub-2h live CAPTURE_RULES_SINCE / refuse shadow mode ->
// criterion 11 (the behaviour is pinned in watchmain_test.go; this pins that
// the loop actually calls it).
func TestMailWatchStructure_StartupChecksAndPrintsTheConfig(t *testing.T) {
	var calls []string
	for _, fname := range []string{"runWatch", "watchMain"} {
		if c, ok := mwCallsIn(t, fname); ok {
			calls = append(calls, c...)
		}
	}
	if len(calls) == 0 {
		t.Fatal("neither runWatch nor watchMain was found in cmd/connectors/google")
	}
	if mwIndexOf(calls, "checkCaptureConfig") < 0 {
		t.Errorf("the watch startup path never calls checkCaptureConfig. Criterion 11: it resolves the "+
			"capture config ONCE, before the first pass, and exits non-zero on a live horizon below "+
			"capture.MinLiveRulesHorizon. Calls seen: %v", calls)
	}
	if mwIndexOf(calls, "startupLine") < 0 {
		t.Errorf("the watch startup path never calls startupLine. Criterion 12: mode, horizon, reconcile, "+
			"idle refresh, pass timeout, account count and health address, so \"a pass that would capture "+
			"nothing is legible from the first ten lines of kubectl logs\". Calls seen: %v", calls)
	}
	if mwIndexOf(calls, "newWatchHealthHandler") < 0 && mwIndexOf(calls, "serveHealth") < 0 {
		t.Errorf("the watch startup path never serves /healthz (criterion 6, D7). Calls seen: %v", calls)
	}
}

// ---- criterion 13: watch mode is IMAP-only by construction --------------------

// Criterion 13: watch mode branches BEFORE MAIL_SOURCE is consulted
// (main.go:50-56), so setting MAIL_SOURCE on the Deployment is harmless but is
// NOT what selects the path. A test says so, and the header documents it —
// otherwise the next operator debugging a silent watcher edits the wrong env
// var.
//
// GREEN TODAY for the call scan (watchMain does not call selectMailSource) and
// must stay green; RED TODAY for the header, which says nothing about watch mode.
func TestMailWatchStructure_WatchModeNeverSelectsAMailSource(t *testing.T) {
	for _, fname := range []string{"watchMain", "runWatch", "watchPass"} {
		calls, ok := mwCallsIn(t, fname)
		if !ok {
			t.Errorf("no func %s in cmd/connectors/google (criterion 13)", fname)
			continue
		}
		if i := mwIndexOf(calls, "selectMailSource"); i >= 0 {
			t.Errorf("%s calls selectMailSource. Criterion 13: watch mode is IMAP-only BY CONSTRUCTION — it "+
				"branches at main.go:50-56 before MAIL_SOURCE is read, and making the resident path depend "+
				"on that variable would mean a Deployment could silently be pointed at the bridge or the "+
				"Gmail API. Calls seen: %v", fname, calls)
		}
	}

	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	header := string(raw)
	if i := strings.Index(header, "package main"); i > 0 {
		header = header[:i]
	}
	lower := strings.ToLower(header)
	if !strings.Contains(lower, "--watch") {
		t.Errorf("main.go's header block never mentions --watch, so `go doc` and a reader of the file learn " +
			"nothing about the resident mode this ticket deploys (criterion 13)")
	}
	if !strings.Contains(lower, "mail_source") ||
		!regexp.MustCompile(`(?s)mail_source.{0,400}watch|watch.{0,400}mail_source`).MatchString(lower) {
		t.Errorf("main.go's header does not say that MAIL_SOURCE is IRRELEVANT in watch mode (criterion 13). " +
			"It is the variable an operator would reach for first when the watcher ingests nothing, and it " +
			"is not the one that selects the path")
	}
	for _, name := range []string{"MAIL_PASS_TIMEOUT", "MAIL_WATCH_HEALTH_ADDR", "MAIL_RECONCILE_INTERVAL", "MAIL_IDLE_REFRESH"} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("main.go's header env block does not list %q; criterion 24's env-parity table between "+
				"cronjob/connector-google and the new Deployment is built from this block", name)
		}
	}
}

// ---- criterion 15: watchPass is untouched -------------------------------------

// Criterion 15: "watchPass's body is byte-unchanged apart from the context it
// receives: the same five calls in the same order, the same counter lines, the
// same AnnounceCaptured". The ORDER is behaviour, not style — ObserveOutbound
// runs after Normalize so this pass's own delivery confirmations are already
// stamped and a message switchboard sent is never relabelled as sent by hand
// (invariant 5).
//
// GREEN TODAY and must stay green. MUTATION: reorder or drop a call in
// watchPass -> criterion 15.
func TestMailWatchStructure_WatchPassKeepsItsFiveCallsInOrder(t *testing.T) {
	calls, ok := mwCallsIn(t, "watchPass")
	if !ok {
		t.Fatalf("no func watchPass in cmd/connectors/google (criterion 15)")
	}
	want := []struct{ name, why string }{
		{"runIMAPIngest", "raw-first: raw_source_items before anything reads them (invariant 1)"},
		{"Normalize", "normalized_messages is what every mail tool reads; loop closure runs in upsertMessage"},
		{"ObserveOutbound", "AFTER Normalize, so this pass's own confirmations are stamped (invariant 5)"},
		{"EvaluateRules", "capture, through the executor built once at watch.go:80 (invariant 3)"},
		{"printCaptureRules", "the counter line, printed unconditionally — zeros included"},
		{"AnnounceCaptured", "wake pipelined last, once the decisions are committed"},
	}
	prev := -1
	for _, w := range want {
		i := mwIndexOf(calls, w.name)
		if i < 0 {
			t.Errorf("watchPass never calls %s — %s (criterion 15). Calls seen: %v", w.name, w.why, calls)
			continue
		}
		if i < prev {
			t.Errorf("watchPass calls %s before the previous step; the order is behaviour, not style "+
				"(criterion 15). Calls seen: %v", w.name, calls)
		}
		prev = i
	}
	// D1: the pass body is not where this ticket works. The lock, the bound, the
	// account refresh and the recovery row all live in the loop AROUND it.
	for _, banned := range []string{"tryMailWatchLock", "Alive", "newWatchHealthHandler", "ListIMAPAccounts"} {
		if i := mwIndexOf(calls, banned); i >= 0 {
			t.Errorf("watchPass calls %s. D1: this ticket adds an operational SKIN — singleton lock, pass "+
				"bound, health endpoint, startup validation, account refresh — and watchPass's body is not "+
				"touched (criterion 15). Calls seen: %v", banned, calls)
		}
	}
}

// ---- criterion 16: no sender seam ---------------------------------------------

// Criterion 16 / invariant 4: the connector arms NO tools.Set* seam, so every
// sender stays nil and this process cannot send, whatever a delivery row asks
// for. Shape copied from cmd/orchestratord's TestOrchestratord_ArmsNoSenderSeam.
//
// GREEN TODAY and must stay green now that the binary also runs resident.
// MUTATION: call tools.SetGmailSender in this main -> criterion 16.
func TestMailWatchStructure_ArmsNoSenderSeam(t *testing.T) {
	registers := 0
	for _, pf := range mwParseNonTest(t, ".") {
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
			if !ok || pf.local[id.Name] != mwToolsImportPath {
				return true
			}
			if strings.HasPrefix(sel.Sel.Name, "Set") {
				t.Errorf("%s calls tools.%s — the watcher arms no seam: its senders stay nil so every "+
					"send-shaped handler errors, whatever a delivery row asks for (criterion 16, "+
					"invariant 4)", pf.name, sel.Sel.Name)
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

// ---- criterion 17: the announce client id -------------------------------------

// Criterion 17: the capture announce id stays switchboard-capture-google-{random}.
// IK: a shared client id kicks the other holder off the broker — and under
// OQ-1 = B-reduced the CronJob and the watcher announce concurrently every two
// hours, so this is a live collision, not a hypothetical one.
//
// GREEN TODAY (pipeline.AnnounceClientID already appends four random bytes) and
// must stay green. MUTATION: use a fixed capture announce client id -> criterion 17.
func TestMailWatchStructure_AnnounceClientIDIsPerConnection(t *testing.T) {
	const connector = "google"
	a, b := pipeline.AnnounceClientID(connector), pipeline.AnnounceClientID(connector)
	if a == b {
		t.Errorf("two announces from one process share the client id %q. Two connections with one id kick "+
			"each other off the broker, and D3 has the CronJob and the watcher announcing concurrently "+
			"(criterion 17)", a)
	}
	prefix := pipeline.CaptureClientID(connector) + "-"
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, prefix) {
			t.Errorf("announce client id %q does not start with %q; criterion 17 pins the id as "+
				"switchboard-capture-google-{random}", id, prefix)
		}
	}
	// The watcher keeps the CronJob's connector string: unlike slackweb (which
	// took "slackweb-watch"), criterion 17 says google's stays "google", and the
	// random suffix is what separates the two connections.
	calls, ok := mwCallsIn(t, "watchPass")
	if !ok {
		t.Fatal("no func watchPass in cmd/connectors/google")
	}
	if mwIndexOf(calls, "AnnounceCaptured") < 0 {
		t.Fatal("watchPass does not announce at all (criterion 15)")
	}
	fn, _, _ := mwFuncDecl(t, "watchPass")
	found := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AnnounceCaptured" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					found = s
				}
			}
		}
		return true
	})
	if found != connector {
		t.Errorf("watchPass announces as connector %q, want %q. Criterion 17 keeps the id "+
			"switchboard-capture-google-{random}; the random suffix, not a second connector name, is what "+
			"keeps the CronJob and the watcher off each other's broker session", found, connector)
	}
}

// ---- criterion 18: nothing else moves -----------------------------------------

// Criterion 18: "No migration. No new table, column, MCP tool, dashboard route
// or policy rule."
//
// PARTIALLY ENCODABLE. What is pinned here: no migration file carries this
// ticket's vocabulary, and this binary reaches neither the MCP server nor the
// dashboard. What is NOT encodable as a repo-state test: "no new column
// anywhere", because migrations legitimately grow for other tickets (0040 is
// already claimed by comms-inbox) — that half is a diff check at review
// (`git diff main...HEAD --stat -- migrations/` must be empty).
//
// GREEN TODAY and must stay green.
func TestMailWatchStructure_AddsNoMigrationAndNoNewSurface(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("POSITIVE CONTROL FAILED: migrations/ is empty")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		lower := strings.ToLower(string(raw))
		for _, needle := range []string{"mail_watch", "mailwatch", "watch_lease", "imap_idle"} {
			if strings.Contains(lower, needle) {
				t.Errorf("migrations/%s mentions %q. Criterion 18: this ticket has NO data-model change — "+
					"the lock is a session lock and health is an HTTP probe, so there is nothing to store",
					e.Name(), needle)
			}
		}
	}
	for _, pf := range mwParseNonTest(t, ".") {
		for _, path := range pf.local {
			for _, banned := range []string{
				"github.com/sspataro57/switchboard/internal/mcpserver",
				"github.com/sspataro57/switchboard/internal/dashboard",
			} {
				if path == banned || strings.HasPrefix(path, banned+"/") {
					t.Errorf("%s imports %s — criterion 18 adds no MCP tool and no dashboard route; the only "+
						"new network surface is GET /healthz for the kubelet", pf.name, path)
				}
			}
		}
	}
}

// ---- criterion 20: exactly two phases -----------------------------------------

// Criterion 20: the phases the WATCHER writes are exactly "imap" (per account,
// per pass, inside IngestIMAP) and "imap_idle" (D9's error row and its recovery
// row). A new phase string would appear on /funnel as a permanently-`never`
// column, the cosmetic trap imap_refetch already has.
//
// The scan is scoped to watch.go because the one-shot path legitimately writes
// "calendar" from calendarsource.go.
func TestMailWatchStructure_WatcherWritesOnlyTheTwoKnownPhases(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "watch.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse watch.go: %v", err)
	}
	allowed := map[string]bool{"imap": true, "imap_idle": true}
	seen := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "StartRun" {
			return true
		}
		seen++
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			if !allowed[s] {
				t.Errorf("watch.go opens a sync_runs row with phase %q. Criterion 20: the watcher writes "+
					"exactly \"imap\" and \"imap_idle\" — /funnel groups by stats->>'phase' with "+
					"last_ok = max(finished_at) FILTER (status IN ('ok','partial')), so a third phase "+
					"is a column that reads `never` forever", s)
			}
		}
		return true
	})
	if seen == 0 {
		t.Error("POSITIVE CONTROL FAILED: watch.go opens no sync_runs row at all. D9's per-account error " +
			"row (watch.go:200-202) is the only operator-visible evidence that a mailbox stopped listening")
	}
}

// ---- D2/D7: the port and the env names ----------------------------------------

// D7's port and the env names criterion 24's env-parity table lists. A default
// that disagrees with the manifest is a liveness probe against a port nothing
// serves.
func TestMailWatchStructure_ReadsTheEnvNamesTheHandoffLists(t *testing.T) {
	all := mwAllSource(t)
	if !strings.Contains(all, `":8092"`) {
		t.Errorf(`cmd/connectors/google has no ":8092" default (D7: hooksd owns :8090, orchestratord :8091, ` +
			`slackweb's watcher :8093, and the hand-off's livenessProbe targets :8092)`)
	}
	for _, name := range []string{"MAIL_PASS_TIMEOUT", "MAIL_WATCH_HEALTH_ADDR"} {
		if !strings.Contains(all, name) {
			t.Errorf("cmd/connectors/google never reads %q. D6/D7 add exactly these two knobs, and "+
				"criterion 24's env-parity table between cronjob/connector-google and "+
				"deployment/connector-google-watch is built from them", name)
		}
	}
	// The SPEC's own file list (D "Files likely to touch"): the health handler
	// and the singleton lock are their own files, twins of cmd/orchestratord's,
	// so `ls cmd/connectors/google` says what the resident mode is made of.
	for _, f := range []string{"health.go", "singleton.go", "watch.go"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("cmd/connectors/google/%s does not exist: the SPEC places the health handler and the "+
				"singleton lock in their own files beside the loop (health.go = newHealthHandler's twin, "+
				"singleton.go = internal/orchestrator/engine.go:215-265 re-spelled locally)", f)
		}
	}
}
