package promote_test

// activity-resurfaces (SWT-72, docs/tickets/activity-resurfaces_SPEC.md) D11
// and criterion 39's second half: the pointer log onto the OLD task carries
// IDS ONLY — no message title, no sender, no body text.
//
// That is the whole reason the pointer is safe on a claude task, whose log
// feeds a worker prompt (the very worry behind C-D13, which criterion 16
// narrows rather than deletes). appendVerdictLog, the EXISTING attach log, does
// carry verdict text — which is why the new function must be its own, and why
// the scan below is over the new function's body rather than the package.
//
// ZERO I/O beyond reading this repo's own source.
//
// GREENFIELD NOTE, EXPECTED RED: internal/promote/store.go declares no pointer
// log function, so this test fails by name.
//
// MUTATION THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - the pointer log carries the message's title or sender text -> IdsOnly.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The exact text criterion 14 pins, as a template:
//
//	promote: ask #<new id> created from this thread (message <M>)
var pointerLogText = regexp.MustCompile(`promote: ask #%d created from this thread \(message %d\)`)

// funcBodyText returns the source text of the named top-level func in rel, ""
// when it is not declared there.
func oaFuncBody(t *testing.T, rel, name string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, b, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || fd.Body == nil {
			continue
		}
		return string(b[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
	}
	return ""
}

// oaPointerLogFunc finds the ONE function whose body carries criterion 14's
// text template, whatever it is called — the SPEC names the text, not the
// function.
func oaPointerLogFunc(t *testing.T) (name, body string) {
	t.Helper()
	const rel = "store.go"
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read internal/promote/store.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, b, 0)
	if err != nil {
		t.Fatalf("parse store.go: %v", err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		src := string(b[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
		if pointerLogText.MatchString(src) {
			return fd.Name.Name, src
		}
	}
	return "", ""
}

func TestPromote_ThePointerLogCarriesIdsOnly(t *testing.T) {
	name, body := oaPointerLogFunc(t)
	if name == "" {
		t.Fatalf("no function in internal/promote/store.go carries D11's pointer text " +
			"`promote: ask #%%d created from this thread (message %%d)`. Criterion 14: ONE task_append_log " +
			"through the executor as promote:inquiry lands on the thread's old task")
	}
	for _, banned := range []struct{ tok, why string }{
		{"v.Title", "criterion 39: IDS ONLY. The title is the ask's own words — untrusted text on a task whose " +
			"log may feed a worker prompt (the worry behind C-D13)"},
		{"v.Sender", "criterion 39: ids only"},
		{"v.Asker", "criterion 39: ids only"},
		{"v.Subject", "criterion 39: ids only"},
		{"v.Reason", "criterion 39: ids only"},
		{"textmatch.", "criterion 39: there is no text to truncate — that is appendVerdictLog's job, not this one"},
		{"rulesPreview", "criterion 39: ids only"},
	} {
		if strings.Contains(body, banned.tok) {
			t.Errorf("%s mentions %s — %s", name, banned.tok, banned.why)
		}
	}
	// It is a task_append_log through the EXECUTOR (invariant 3), not SQL.
	if !strings.Contains(body, `"task_append_log"`) {
		t.Errorf("%s does not call the task_append_log tool; D11's pointer is an executor call, never a direct "+
			"INSERT (invariant 3)", name)
	}
	// It must NOT mark activity: the new task is the thing to look at, and
	// surfacing the old one too would double the rows (D11, an explicit exclusion).
	if strings.Contains(body, "task_mark_activity") {
		t.Errorf("%s also marks activity on the old task. D11: \"It is NOT an activity mark: the new task is the "+
			"thing to look at, and surfacing the old one too would double the rows\" (criterion 14)", name)
	}
	// CONTROL: the EXISTING attach log does carry verdict text, so the scan above
	// is discriminating between two functions rather than passing vacuously.
	if attach := oaFuncBody(t, "store.go", "appendVerdictLog"); attach == "" {
		t.Errorf("CONTROL: store.go declares no appendVerdictLog; the attach log is still the personal lane's")
	} else if !strings.Contains(attach, "v.Title") {
		t.Errorf("CONTROL FAILED: appendVerdictLog no longer carries v.Title, so the ids-only scan above is not " +
			"telling two kinds of log apart")
	}
}

// D3's table: the promoter's activity mark is LANE-AGNOSTIC — one call site, no
// `kind` argument, no lane branch. After D11 the inquiry lane's only remaining
// attach targets a DISMISSED (closed) task, so the mark is a skip by
// construction; what the hook is really for is the PERSONAL lane's follow-up
// attach, the silent log line this ticket is about.
func TestPromote_TheActivityMarkIsLaneAgnostic(t *testing.T) {
	var name, body string
	for _, fn := range []string{"markVerdictActivity", "markActivity", "markAttachActivity"} {
		if b := oaFuncBody(t, "store.go", fn); b != "" {
			name, body = fn, b
			break
		}
	}
	if name == "" {
		t.Fatalf("internal/promote/store.go declares no activity-mark helper (criterion 9: in act's \"attached\" " +
			"branch, after recordTask and before reopenDismissed, it calls task_mark_activity as actorFor(v.Lane))")
	}
	if !strings.Contains(body, `"task_mark_activity"`) {
		t.Errorf("%s does not call the task_mark_activity tool (criterion 9, invariant 3)", name)
	}
	if !strings.Contains(body, "actorFor(") {
		t.Errorf("%s does not use actorFor(v.Lane) as the actor; D3's table says the actor is promote:{lane}", name)
	}
	for _, banned := range []string{"LaneInquiry", "LanePersonal", `"inquiry"`, `"personal"`} {
		if strings.Contains(body, banned) {
			t.Errorf("%s branches on %s. D3: \"One lane-agnostic call site, no kind argument, no lane branch\" — "+
				"the closed-task skip in the TOOL is what makes the inquiry lane a no-op", name, banned)
		}
	}
}
