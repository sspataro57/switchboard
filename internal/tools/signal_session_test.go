package tools

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criteria 1-4: task_signal's
// `session` argument — the <name> from ListAgents' "This session is <name> [ref]"
// — and its ONE validator, NormalizeSessionName. ZERO network, ZERO Postgres.
//
// IMPOSED SURFACE (SPEC S3, criterion 1):
//
//	// internal/tools/signal.go
//	const SessionNameMax = 200
//	func NormalizeSessionName(s string) (string, error)
//	type signalArgs struct { …; Session string `json:"session,omitempty"` }
//
// HOW TEST RUNES ARE WRITTEN (criterion 2, the IK "No python escapes into Go"
// lesson): every invisible or unusual rune is BUILT from its value with
// string(rune(0x…)); no code point is pasted and no backslash-u escape appears in
// a string literal a tool might decode.
//
// GREENFIELD NOTE — EXPECTED RED: SessionNameMax and NormalizeSessionName do not
// exist, so package tools' test binary compile-FAILS until signal.go declares
// them.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- criterion 1: the declarations ---------------------------------------------

func TestSessionNameMax_DeclaredOnceInSignalGo(t *testing.T) {
	if SessionNameMax != 200 {
		t.Errorf("SessionNameMax = %d, want 200 (S3: the platform's own session-title cap, not 64)", SessionNameMax)
	}
	src := parseToolsSource(t)
	if got := src.valueFile["SessionNameMax"]; got != "signal.go" {
		t.Errorf("SessionNameMax is declared in %q, want signal.go (criterion 1: spelled once)", got)
	}
}

func TestSignalArgs_SessionField(t *testing.T) {
	f, ok := reflect.TypeOf(signalArgs{}).FieldByName("Session")
	if !ok {
		t.Fatalf("signalArgs has no Session field (criterion 1)")
	}
	if f.Type.Kind() != reflect.String {
		t.Errorf("signalArgs.Session is %s, want string", f.Type)
	}
	if tag := f.Tag.Get("json"); tag != "session,omitempty" {
		t.Errorf("signalArgs.Session json tag = %q, want \"session,omitempty\" (criterion 1)", tag)
	}
}

// ---- criterion 2: the unit table ------------------------------------------------

// emoji200 is exactly SessionNameMax runes, every one a multi-byte emoji, so a
// byte cap or a 64-rune cap refuses it (mutation "Cap at 64").
func emoji200() string { return strings.Repeat(string(rune(0x1F6A6)), 200) }

func TestNormalizeSessionName_Accepts(t *testing.T) {
	family := string([]rune{0x1F468, 0x200D, 0x1F469, 0x200D, 0x1F467}) // ZWJ-joined family
	for _, tc := range []struct{ name, in, want string }{
		{"switchboard-67", "switchboard-67", "switchboard-67"},
		{"gonoble", "gonoble", "gonoble"},
		{"kube-c7", "kube-c7", "kube-c7"},
		{"foundry-ux-lab-f5", "foundry-ux-lab-f5", "foundry-ux-lab-f5"},
		{"surrounding spaces are trimmed", "  kube-c7  ", "kube-c7"},
		{"a trailing emoji", "Fix the board " + string(rune(0x1F6A6)), "Fix the board " + string(rune(0x1F6A6))},
		{"a ZWJ family emoji", family, family},
		{"a Latin-1 accent", "caf" + string(rune(0xE9)), "caf" + string(rune(0xE9))},
		{"interior ASCII spaces", "fix the lights board today", "fix the lights board today"},
		{"exactly 200 multi-byte runes", emoji200(), emoji200()},
		{"the S10 fallback", "kube (no ListAgents)", "kube (no ListAgents)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSessionName(tc.in)
			if err != nil {
				t.Fatalf("NormalizeSessionName(%q) refused: %v — S3 accepts it", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeSessionName(%q) = %q, want %q byte-exact (the trim is the only rewrite)", tc.in, got, tc.want)
			}
		})
	}
	if n := utf8.RuneCountInString(emoji200()); n != 200 {
		t.Fatalf("CONTROL: the 200-rune fixture has %d runes", n)
	}
}

func TestNormalizeSessionName_Refuses(t *testing.T) {
	type refusal struct {
		name, in string
		wantIn   []string
	}
	cases := []refusal{
		{"empty", "", []string{"missing session", "ListAgents", "This session is"}},
		{"blank", "   ", []string{"missing session", "ListAgents", "This session is"}},
		{"201 runes", emoji200() + "x", []string{"200"}},
		{"the whole ListAgents line", "This session is kube-c7 [x]", []string{"pass only the <name>"}},
		{"the whole line, lower case", "this session is kube-c7", []string{"pass only the <name>"}},
	}
	// Every refused rune is named by %U.
	for _, tc := range []struct {
		name string
		in   string
		r    rune
	}{
		{"newline", "a\nb", '\n'},
		{"tab", "a\tb", '\t'},
		{"NUL", "a" + string(rune(0)) + "b", 0},
		{"bidi override U+202E", string(rune(0x202E)) + "evil", 0x202E},
		{"zero-width space U+200B", "zero" + string(rune(0x200B)) + "width", 0x200B},
		{"byte-order mark U+FEFF", string(rune(0xFEFF)) + "bom", 0xFEFF},
		{"NBSP inside a name", "a" + string(rune(0xA0)) + "b", 0xA0},
	} {
		cases = append(cases, refusal{tc.name, tc.in, []string{runeU(tc.r)}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSessionName(tc.in)
			if err == nil {
				t.Fatalf("NormalizeSessionName(%q) = %q, nil; S3 refuses it", tc.in, got)
			}
			for _, w := range tc.wantIn {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("NormalizeSessionName(%q) refused with %q, which does not contain %q (criterion 2)", tc.in, err, w)
				}
			}
		})
	}
}

// runeU is %U without importing fmt twice in the table above.
func runeU(r rune) string {
	const hex = "0123456789ABCDEF"
	s := ""
	for v := uint32(r); ; v >>= 4 {
		s = string(hex[v&0xF]) + s
		if v < 16 {
			break
		}
	}
	for len(s) < 4 {
		s = "0" + s
	}
	return "U+" + s
}

// Criterion 2, last bullet: a refused value is echoed by %.64q, so a pasted blob
// cannot flood the error or the audit row.
func TestNormalizeSessionName_EchoIsBounded(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"5,000 runes (too long)", strings.Repeat("k", 5000)},
		{"5,000 runes with a tab (bad rune)", strings.Repeat("k", 4999) + "\t"},
		{"the whole ListAgents line, padded", "This session is " + strings.Repeat("k", 5000)},
	} {
		_, err := NormalizeSessionName(tc.in)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if n := len(err.Error()); n >= 400 {
			t.Errorf("%s: the refusal is %d bytes, want < 400: echo the value with %%.64q", tc.name, n)
		}
	}
}

// ---- criterion 3: validateSignal -------------------------------------------------

func TestValidateSignal_Session(t *testing.T) {
	for _, st := range []string{"working", "needs_input"} {
		for _, tc := range []struct{ name, args, wantIn string }{
			{"no session", `{"task_id":412,"state":"` + st + `"}`, "missing session"},
			{"empty session", `{"task_id":412,"state":"` + st + `","session":""}`, "missing session"},
			{"blank session", `{"task_id":412,"state":"` + st + `","session":"   "}`, "missing session"},
			{"the whole line", `{"task_id":412,"state":"` + st + `","session":"This session is kube-c7 [x]"}`, "pass only the <name>"},
			{"a tab", `{"task_id":412,"state":"` + st + `","session":"kube\tc7"}`, "U+0009"},
		} {
			err := validateSignal([]byte(tc.args))
			if err == nil {
				t.Errorf("validateSignal(%s) = nil; S2: %s needs a valid session", tc.args, st)
			} else if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("validateSignal(%s) = %q, want it to contain %q", tc.args, err, tc.wantIn)
			}
		}
		if err := validateSignal([]byte(`{"task_id":412,"state":"` + st + `","session":"kube-c7"}`)); err != nil {
			t.Errorf("validateSignal(%s with session kube-c7) = %v, want nil", st, err)
		}
	}
	// clear: optional, validated when present.
	for _, args := range []string{`{"task_id":412,"state":"clear"}`, `{"task_id":412,"state":"clear","session":"shell"}`} {
		if err := validateSignal([]byte(args)); err != nil {
			t.Errorf("validateSignal(%s) = %v, want nil (S2: clear needs no session)", args, err)
		}
	}
	for _, args := range []string{
		`{"task_id":412,"state":"clear","session":"This session is kube-c7"}`,
		`{"task_id":412,"state":"clear","session":"a\nb"}`,
	} {
		if err := validateSignal([]byte(args)); err == nil {
			t.Errorf("validateSignal(%s) = nil; S2: a session on clear is validated the same way", args)
		}
	}
	// "   " on clear trims to empty = absent; that is not an error.
	if err := validateSignal([]byte(`{"task_id":412,"state":"clear","session":"   "}`)); err != nil {
		t.Errorf("validateSignal(clear with a blank session) = %v, want nil (blank is missing, and clear needs none)", err)
	}
}

// ---- criterion 4: one spelling -----------------------------------------------------

func funcCalls(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func TestSignalSession_OneSpelling(t *testing.T) {
	src := parseToolsSource(t)
	for _, fn := range []string{"validateSignal", "signalTask"} {
		d, ok := src.funcs[fn]
		if !ok {
			t.Errorf("internal/tools declares no %s", fn)
			continue
		}
		if !funcCalls(d, "NormalizeSessionName") {
			t.Errorf("%s does not reference NormalizeSessionName. Criterion 4: both the validator and the handler "+
				"normalize (the handler never trusts that validation ran)", fn)
		}
	}

	// No other non-test file under internal/ trims a field named Session.
	base := filepath.Join("..")
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.ToSlash(path) == "../tools/signal.go" {
			return nil
		}
		f, perr := parseGoFile(path)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "TrimSpace" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "strings" {
				return true
			}
			if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "Session" {
				t.Errorf("%s calls strings.TrimSpace on a field named Session: a second spelling of the S3 "+
					"normalization. Criterion 4: call tools.NormalizeSessionName", path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}

func parseGoFile(path string) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
}
