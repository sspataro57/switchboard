package main

// slack-watch-sweep (SWT-75) structural checks on the slackweb binary, which
// now has two shapes: today's one-shot (the CronJob net, D4) and the resident
// `--watch` loop (D3). ZERO I/O beyond parsing Go sources.
//
//   - criterion 10: the rotation pass runs today's full sequence in the same
//     ORDER; the targeted pass runs Normalize -> EvaluateRules ->
//     AnnounceCaptured and does NOT run ReconcileUnconfirmed or ObserveOutbound.
//   - criterion 12: the capture announce uses connector "slackweb-watch", so
//     the MQTT client id differs from the CronJob's (IK: a shared client id
//     kicks the other holder off the broker).
//   - criterion 16: the watch list is read INSIDE the targeted pass, not once
//     at startup.
//   - criterion 21: lockkeys.SlackWatch is 0x5157_0011, declared ONCE, in the
//     import-free internal/lockkeys.
//   - criterion 22: the one-shot path calls the stand-down check and prints the
//     line the runbook tells an operator to look for.
//   - criterion 27 / invariant 4: the binary arms no tools.Set* seam, so this
//     process cannot send even if a delivery row asked it to.
//   - D9's port and the env names the kube hand-off's env-parity table lists.
//
// IMPOSED SURFACE (names chosen here; the SPEC names only the files —
// cmd/connectors/slackweb/{health.go,singleton.go} "twins of cmd/orchestratord's"
// and main.go's "--watch, watchMain, the D4 stand-down check, D8's refusals"):
//
//	func rotationPass(ctx context.Context, ...) (slackweb.Stats, error)
//	func targetedPass(ctx context.Context, ...) (slackweb.Stats, error)
//	func watchMain(ctx context.Context, ...) error
//	func watchSource() (slackweb.Source, error)           // D8's refusal
//	func standDown(ctx context.Context, pool *pgxpool.Pool) (bool, error) // D4
//	func startupLine(cfg slackweb.WatchConfig, targets int, mode string,
//	    horizon time.Duration) string                     // criterion 13
//	func checkCaptureConfig(mode string, horizon time.Duration) error
//
//	// internal/lockkeys/lockkeys.go
//	const SlackWatch int64 = 0x5157_0011
//
// GREEN TODAY and must stay green: the tools.Set* scan (it pins what this
// ticket must NOT change). EXPECTED RED: everything else, because none of the
// functions exists. (Under a plain `go test ./cmd/connectors/slackweb` the
// package compile-fails first on health_test.go's missing symbols.)

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

const toolsImportPath = "github.com/sspataro57/switchboard/internal/tools"

type swParsedFile struct {
	name  string
	file  *ast.File
	local map[string]string // local package name -> import path
}

func swParseNonTest(t *testing.T, dir string) []swParsedFile {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	var out []swParsedFile
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
		out = append(out, swParsedFile{name: name, file: f, local: local})
	}
	if len(out) == 0 {
		t.Fatalf("POSITIVE CONTROL FAILED: no non-test .go file in %s was scanned", dir)
	}
	return out
}

// swCallsIn returns, in SOURCE ORDER, every `pkg.Func(` call inside the named
// function, rendered as "pkg.Func".
func swCallsIn(t *testing.T, fname string) ([]string, bool) {
	t.Helper()
	var calls []string
	found := false
	for _, pf := range swParseNonTest(t, ".") {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || fn.Name.Name != fname {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					switch x := sel.X.(type) {
					case *ast.Ident:
						calls = append(calls, x.Name+"."+sel.Sel.Name)
					default:
						calls = append(calls, sel.Sel.Name)
					}
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					calls = append(calls, id.Name)
				}
				return true
			})
		}
	}
	return calls, found
}

func swIndexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want || strings.HasSuffix(c, "."+want) {
			return i
		}
	}
	return -1
}

func swIndexOfAny(calls []string, wants ...string) int {
	best := -1
	for _, w := range wants {
		if i := swIndexOf(calls, w); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// Criterion 10, first half: the rotation pass IS today's main.go:71-134
// sequence, in the same order. D11: "No task, delivery or orchestrator
// behaviour is touched" — and the order is behaviour, because
// ReconcileUnconfirmed runs AFTER Normalize so this pass's own confirmations
// are already stamped and a delivery confirmed moments ago is never flagged.
func TestWatchStructure_RotationPassRunsTodaysSequenceInOrder(t *testing.T) {
	calls, ok := swCallsIn(t, "rotationPass")
	if !ok {
		t.Fatalf("no func rotationPass in cmd/connectors/slackweb (criterion 10)")
	}
	want := []struct{ name, why string }{
		{"Ingest", "the export (Ingest / IngestWith with the rotation phase)"},
		{"Normalize", "normalize before anything reads normalized_messages"},
		{"ReconcileUnconfirmed", "after Normalize, so this pass's own confirmations are already stamped"},
		{"ObserveOutbound", "after the reconciler, so an in-flight send has had its chance to be confirmed"},
		{"EvaluateRules", "capture, through the executor"},
		{"AnnounceCaptured", "wake pipelined last, once the decisions are committed"},
	}
	prev := -1
	for _, w := range want {
		i := swIndexOf(calls, w.name)
		if w.name == "Ingest" {
			// Either spelling: Ingest keeps today's signature, IngestWith takes
			// the request and the phase (D6).
			i = swIndexOfAny(calls, "Ingest", "IngestWith")
		}
		if i < 0 {
			t.Errorf("rotationPass never calls %s — %s (criterion 10). Calls seen: %v", w.name, w.why, calls)
			continue
		}
		if i < prev {
			t.Errorf("rotationPass calls %s before the previous step; the order is behaviour, not style "+
				"(criterion 10: \"a rotation pass runs the full main.go:71-134 sequence in the same order\"). "+
				"Calls seen: %v", w.name, calls)
		}
		prev = i
	}
}

// Criterion 10, second half, and the SPEC's own mutation row ("run
// ReconcileUnconfirmed on a targeted pass"): a targeted pass runs Normalize ->
// EvaluateRules -> AnnounceCaptured and NOTHING else. Running the reconciler
// here would count per-minute passes as observation passes — D6's alarm, from
// the other direction.
func TestWatchStructure_TargetedPassSkipsTheReconciler(t *testing.T) {
	calls, ok := swCallsIn(t, "targetedPass")
	if !ok {
		t.Fatalf("no func targetedPass in cmd/connectors/slackweb (criterion 10)")
	}
	for _, banned := range []struct{ name, why string }{
		{"ReconcileUnconfirmed", "D6: a per-minute pass would satisfy DefaultUnconfirmedFlagPasses (3) in " +
			"three minutes and flag a healthy send"},
		{"ObserveOutbound", "capture.ObserveOutbound belongs to the rotation: it scans the whole corpus and " +
			"a targeted pass has read two conversations"},
	} {
		if i := swIndexOf(calls, banned.name); i >= 0 {
			t.Errorf("targetedPass calls %s — %s (criterion 10). Calls seen: %v", banned.name, banned.why, calls)
		}
	}
	prev := -1
	for _, name := range []string{"Normalize", "EvaluateRules", "AnnounceCaptured"} {
		i := swIndexOf(calls, name)
		if i < 0 {
			t.Errorf("targetedPass never calls %s; without it a watched message is ingested and then sits in "+
				"raw_source_items until the next rotation — the black hole this ticket exists to close "+
				"(criterion 10). Calls seen: %v", name, calls)
			continue
		}
		if i < prev {
			t.Errorf("targetedPass calls %s out of order (criterion 10). Calls seen: %v", name, calls)
		}
		prev = i
	}
}

// Criterion 16: the watch list is re-read on EVERY pass. Loading it once at
// startup is the SPEC's own mutation row, and it would mean `opsctl slack-watch
// add` takes effect on the next pod restart instead of the next minute.
func TestWatchStructure_TargetedPassReadsTheWatchList(t *testing.T) {
	calls, ok := swCallsIn(t, "targetedPass")
	if !ok {
		t.Fatalf("no func targetedPass in cmd/connectors/slackweb (criterion 16)")
	}
	if swIndexOf(calls, "WatchTargets") < 0 {
		t.Errorf("targetedPass never calls WatchTargets; the SWT-73 D10 rule is \"watch the table, not a "+
			"snapshot\" (criterion 16). Calls seen: %v", calls)
	}
}

// Criterion 12: the watcher's capture announce uses its OWN connector string,
// so its MQTT client id cannot collide with the CronJob's. IK: a shared client
// id kicks the other holder off the broker — and the CronJob still runs every
// two hours (D4), so the two WILL overlap.
func TestWatchStructure_AnnounceUsesItsOwnConnectorString(t *testing.T) {
	const watchConnector = "slackweb-watch"
	if pipeline.CaptureClientID(watchConnector) == pipeline.CaptureClientID("slackweb") {
		t.Fatalf("CaptureClientID(%q) == CaptureClientID(\"slackweb\") == %q; the two processes would kick "+
			"each other off the broker (criterion 12)", watchConnector, pipeline.CaptureClientID("slackweb"))
	}
	var seen bool
	for _, pf := range swParseNonTest(t, ".") {
		src, err := os.ReadFile(pf.name)
		if err != nil {
			t.Fatalf("read %s: %v", pf.name, err)
		}
		if strings.Contains(string(src), `"`+watchConnector+`"`) {
			seen = true
		}
	}
	if !seen {
		t.Errorf("no file in cmd/connectors/slackweb names the connector string %q. D9: the announce's "+
			"connector is what makes the client id switchboard-capture-slackweb-watch", watchConnector)
	}
}

// Criterion 22: the one-shot path stands down while the watcher lives. Without
// it a two-hourly tick lands on an idle browser, holds it for 15 minutes doing
// work the watcher already does, and blocks the watch list for those 15 minutes
// twice a day. The log line is quoted in the runbook and the hand-off, so it is
// pinned here verbatim.
func TestWatchStructure_OneShotStandsDown(t *testing.T) {
	calls, ok := swCallsIn(t, "run")
	if !ok {
		t.Fatalf("no func run in cmd/connectors/slackweb; the one-shot path is where the stand-down check goes")
	}
	if swIndexOf(calls, "standDown") < 0 {
		t.Errorf("the one-shot `run` never calls standDown. Criterion 22 / D4: at startup it takes "+
			"lockkeys.SlackWatch with pg_try_advisory_lock, releases it immediately, and skips the pass when "+
			"it was already held. Calls seen: %v", calls)
	}
	const skipLine = "slack watch is live; skipping this pass"
	var seen bool
	for _, pf := range swParseNonTest(t, ".") {
		src, err := os.ReadFile(pf.name)
		if err != nil {
			t.Fatalf("read %s: %v", pf.name, err)
		}
		if strings.Contains(string(src), skipLine) {
			seen = true
		}
	}
	if !seen {
		t.Errorf("cmd/connectors/slackweb never prints %q — criterion 22 quotes it, and it is what an operator "+
			"greps for in a CronJob log that did nothing", skipLine)
	}
}

// Criterion 21: one advisory key, in the import-free internal/lockkeys, with
// the low four hex digits D9 reserved. The literal is deliberately NOT restated
// here: internal/classify's repo-wide collision scan reads a restated 0x5157
// literal as a second owner (the internal/ticketstatus precedent).
func TestWatchStructure_LockKeyIsDeclaredOnceAndIs0011(t *testing.T) {
	// A pattern that MATCHES an advisory-lock literal without BEING one.
	keyPattern := regexp.MustCompile(`0x5157_?([0-9A-Fa-f]{4})`)
	declPattern := regexp.MustCompile(`SlackWatch\s+int64\s*=\s*(0x5157_?[0-9A-Fa-f]{4})`)

	path := filepath.Join("..", "..", "..", "internal", "lockkeys", "lockkeys.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read internal/lockkeys/lockkeys.go: %v", err)
	}
	m := declPattern.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("internal/lockkeys declares no `SlackWatch int64 = 0x5157_00NN`. D9: the key lives in the "+
			"import-free internal/lockkeys because the repo-wide collision scan walks internal/ only — and "+
			"because D4's CronJob stand-down check needs the same constant:\n%s", raw)
	}
	digits := strings.ToLower(keyPattern.FindStringSubmatch(string(m[1]))[1])
	if digits != "0011" {
		t.Errorf("lockkeys.SlackWatch's low four hex digits are %q, want \"0011\" — D9 surveyed the taken keys "+
			"(0005 orchestrator, 0006 triage, 0007 google per-account, 0015 capture, 0021 promote, 0022 "+
			"classify, 0023 ticketstatus, 0028 calendar booking) and 0x5157_0010 is reserved by SWT-73's "+
			"MailWatch", digits)
	}
	want, err := strconv.ParseInt("5157"+digits, 16, 64)
	if err != nil {
		t.Fatalf("parse the key: %v", err)
	}
	if lockkeys.SlackWatch != want {
		t.Errorf("lockkeys.SlackWatch = %#x but its source literal is %#x", lockkeys.SlackWatch, want)
	}
	if lockkeys.SlackWatch == lockkeys.Orchestrator {
		t.Error("lockkeys.SlackWatch collides with lockkeys.Orchestrator: two processes sharing an " +
			"advisory-lock key silently exclude each other, on a schedule, with no error (criterion 21)")
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
			"internal/classify's repo-wide collision scan (criterion 21)", digits, len(owners), owners)
	}
}

// Criterion 27 / invariant 4: the watcher arms NO tools.Set* seam, so every
// sender stays nil and this process cannot send — the structural guarantee, not
// a promise. The SPEC's mutation row is "call a tools.Set* seam in the watch
// main". Shape copied from cmd/orchestratord's TestOrchestratord_ArmsNoSenderSeam.
//
// GREEN TODAY: this main arms nothing. It must stay that way now that the
// binary links a resident loop beside the one-shot.
func TestWatchStructure_ArmsNoSenderSeam(t *testing.T) {
	registers := 0
	for _, pf := range swParseNonTest(t, ".") {
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
				t.Errorf("%s calls tools.%s — the watcher arms no seam: its senders stay nil so every "+
					"send-shaped handler errors, whatever a delivery row asks for (criterion 27, invariant 4)",
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

// D9's port and the env names the hand-off's env-parity table has to list
// (criterion 30). A default that disagrees with the manifest is a probe against
// a port nothing serves.
func TestWatchStructure_ReadsTheEnvNamesTheHandoffLists(t *testing.T) {
	var src strings.Builder
	for _, pf := range swParseNonTest(t, ".") {
		b, err := os.ReadFile(pf.name)
		if err != nil {
			t.Fatalf("read %s: %v", pf.name, err)
		}
		src.Write(b)
	}
	all := src.String()
	if !strings.Contains(all, `":8093"`) {
		t.Errorf(`cmd/connectors/slackweb has no ":8093" default (D9: hooksd owns :8090, orchestratord :8091, ` +
			`SWT-73's mail watcher :8092, and the hand-off's livenessProbe targets :8093)`)
	}
	for _, name := range []string{"SLACK_WEB_BRIDGE_URL", "SLACK_WATCH_HEALTH_ADDR"} {
		if !strings.Contains(all, name) {
			t.Errorf("cmd/connectors/slackweb never reads %q; it is in the hand-off's env-parity table "+
				"(criterion 30) and D8 refuses to start without the first", name)
		}
	}
}
