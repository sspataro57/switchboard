package textmatch

// Structural enforcement of the ONE-SPELLING rule for reply-prefix stripping
// (bug gmail-reply-empty-subject-off-thread, Jira SWT-61).
//
// Plain unit test on purpose, like callsites_test.go: no build tag, no database,
// so it runs under `go test ./...`.
//
// THE RULE. "Strip the leading Re: prefixes" is spelled ONCE, in this package
// (StripReplyPrefix / ReplySubject). Today internal/capture/prreview.go owns a
// private `prSubjectReplyRe = ^(?i:re:\s*)+` used for PR-review board titles,
// and SWT-61's fix needs the same rule in internal/tools/delivery.go for a
// CLIENT-VISIBLE email subject. Two regexps that must agree is the SWT-13
// canonicalization landmine again: when they drift nothing errors — one surface
// doubles "Re: Re:" or eats a word, and only a human reading the mail notices.
//
// Why a scan and not just a call: a second spelling is invisible to every
// behavioural test, because each spelling passes its own tests.
//
// GREENFIELD NOTE — EXPECTED RED, three ways:
//  1. the package declares neither function, so this file does not compile
//     (undefined: StripReplyPrefix) — same as replysubject_test.go;
//  2. TestReplyPrefix_OneSpellingUnderInternal finds prreview.go still spelling
//     its own `^(?i:re:\s*)+`;
//  3. TestReplySubjectCallsites finds delivery.go never calling ReplySubject and
//     prreview.go never calling StripReplyPrefix.

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
)

// The home of the one spelling, relative to this package's directory.
const replySubjectHome = "internal/textmatch"

// replyPrefixSpelling matches a Go string literal that is an ANCHORED regexp
// about a reply prefix: a `^` somewhere, then `re` (not the tail of a longer
// word) optionally counted `[2]`, then a colon. It matches prreview.go's
// `^(?i:re:\s*)+` and would match `^[Rr]e:\s*` or `^(?i)re\[\d+\]:\s*`.
var replyPrefixSpelling = regexp.MustCompile(`(?i)\^.*?[^a-z]re\s*(\[[0-9]*\])?\s*:`)

// goSourceFiles walks the repo's own non-test Go sources under internal/ and cmd/.
func goSourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join("..", "..", root), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) < 50 {
		t.Fatalf("walked only %d source files; the scan has stopped matching the tree", len(out))
	}
	return out
}

// repoRel turns ../../internal/tools/delivery.go into internal/tools/delivery.go.
func repoRel(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "../../")
}

// stringLiterals returns every string literal in a Go file (raw literals
// included — the existing spelling is backtick-quoted).
func stringLiterals(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

// Criterion: ONE spelling. Nothing under internal/ or cmd/ builds its own
// reply-prefix regexp except this package.
func TestReplyPrefix_OneSpellingUnderInternal(t *testing.T) {
	// The check first requires its subject to exist: a scan with nothing to
	// scan proves nothing.
	var homeDeclares bool
	homeFiles, err := filepath.Glob(filepath.Join("*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", replySubjectHome, err)
	}
	for _, f := range homeFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		if strings.Contains(src, "func StripReplyPrefix(") && strings.Contains(src, "func ReplySubject(") {
			homeDeclares = true
		}
	}
	if !homeDeclares {
		t.Fatalf("no file in %s declares BOTH StripReplyPrefix and ReplySubject — the ONE spelling of the "+
			"reply-subject rule lives here (SWT-61 owner decision 4)", replySubjectHome)
	}

	for _, path := range goSourceFiles(t) {
		rel := repoRel(path)
		if strings.HasPrefix(rel, replySubjectHome+"/") {
			continue // the home is allowed to spell it; it IS the spelling
		}
		for _, lit := range stringLiterals(t, path) {
			if replyPrefixSpelling.MatchString(lit) {
				t.Errorf("%s spells its own reply-prefix regexp %q — SWT-61 owner decision 4: ONE spelling, "+
					"textmatch.StripReplyPrefix. Two regexps that must agree drift silently: one surface "+
					"doubles \"Re: Re:\" or eats a word and nothing errors", rel, lit)
			}
		}
	}
}

// Criterion: both consumers go through the shared helper — delivery.go for the
// client-visible email subject, prreview.go for the board title.
func TestReplySubjectCallsites(t *testing.T) {
	for _, c := range []struct{ rel, call, why string }{
		{"internal/tools/delivery.go", "textmatch.ReplySubject(",
			"SWT-61 decision 1: draftDelivery's gmail branch fills an absent subject with the reply subject of the " +
				"thread's LATEST INBOUND message, inside the executor, where no caller can bypass it"},
		{"internal/capture/prreview.go", "textmatch.StripReplyPrefix(",
			"SWT-61 decision 4: prReviewTitle adopts the shared strip rather than keeping prSubjectReplyRe, its own " +
				"second spelling of the same rule"},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", c.rel))
		if err != nil {
			t.Fatalf("read %s: %v", c.rel, err)
		}
		if !strings.Contains(string(b), c.call) {
			t.Errorf("%s never calls %s — %s", c.rel, c.call, c.why)
		}
	}
}
