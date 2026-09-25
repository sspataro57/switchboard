package dashboard

// task-detail-source-message (SWT-65, docs/tickets/task-detail-source-message_SPEC.md)
// test plan section A: the STRUCTURAL half of criteria 6, 9, 12, 13, 14 and D8.
// Plain unit test — ZERO I/O beyond this package's own source, this package's
// embedded templates and a source scan of internal/tools. No database, no
// network, no model.
//
// IMPOSED SURFACE (SPEC "Files likely to touch" + D7/D8/D10):
//
//	// internal/dashboard/sourcemessage.go (NEW)
//	const sourceBodyCap = 262144
//	func sourceMessageHeading(channel string, viaThread bool) string
//	func (s *Server) loadSourceMessage(ctx context.Context, taskID int64, sourceThreadID *int64) (*sourceMessage, error)
//	type sourceMessage struct{ …; Thread []sourceThreadMessage; … }
//	// internal/dashboard/board.go: taskDetail gains SourceMessage *sourceMessage
//	// internal/dashboard/templates/task.html: the {{if .SourceMessage}} block
//	//   delimited by <!-- source-message: begin --> / <!-- source-message: end -->
//	// internal/tools: MailThreadMaxMessages, MailThreadBodyCap, LatestInboundOrder,
//	//   MailAttachmentsForRawItem
//
// GREENFIELD NOTE — EXPECTED RED: sourcemessage.go does not exist, task.html
// carries no marker comments, internal/tools still spells the two caps
// unexported and has no MailAttachmentsForRawItem. Every test in this file
// therefore fails on a real assertion (not a compile error): this file
// references NO new Go identifier, deliberately, so that section A's failures
// are readable today. The compile-level greenfield failure lives in
// task_detail_test.go (sourceMessageHeading).
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC section E):
//   - 10 (body rendered with template.HTML) → TestTaskDetail_NoRawHTML
//   - 11 (bare URLs wrapped in <a href>)    → TestTaskTemplate_SourceSectionIsInert (the tag scan)
//   - 13 (thread messages rendered inline)  → TestTaskTemplate_ThreadIsCollapsed
//   - 14 (<details open>)                   → TestTaskTemplate_SourceSectionIsInert
//   - 17 (surfaced_by_message_id as branch 0) → TestShowTask_DoesNotReadSurfacedBy
//   - 18 (sourceBodyCap collapsed into MailThreadBodyCap) → TestMailThreadCaps_OneSpelling

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two marker comments D4 puts around the section so a scan can slice it.
const (
	tdSectionBegin = "<!-- source-message: begin -->"
	tdSectionEnd   = "<!-- source-message: end -->"
	tdSectionTag   = `<section class="source-message">`
)

func tdTaskHTML(t *testing.T) string {
	t.Helper()
	raw, err := templateFS.ReadFile("templates/task.html")
	if err != nil {
		t.Fatalf("read embedded task.html: %v", err)
	}
	return string(raw)
}

// tdSection slices the source-message block. A missing marker is a FATAL, not a
// skip: the markers are the reason criterion 12 is testable at all, and a scan
// that quietly asserts nothing when they are absent is the SWT-21 landmine in
// the shape of a test (this is the positive control for every scan below).
func tdSection(t *testing.T, s string) string {
	t.Helper()
	i := strings.Index(s, tdSectionBegin)
	j := strings.Index(s, tdSectionEnd)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("task.html has no %s … %s block (criterion 12: the section is delimited by the two marker "+
			"comments precisely so a structure test can slice it; begin=%d end=%d)", tdSectionBegin, tdSectionEnd, i, j)
	}
	return s[i+len(tdSectionBegin) : j]
}

// ---- criterion 13: nothing on the path turns a stored body into trusted HTML ----

func TestTaskDetail_NoRawHTML(t *testing.T) {
	files := []string{"board.go", "sourcemessage.go", "templates/task.html"}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("read %s: %v — criterion 13 scans the WHOLE path to the page; a file that is missing is a "+
				"file this scan does not cover", f, err)
			continue
		}
		for _, banned := range []string{"template.HTML(", "template.HTMLAttr(", "template.HTML ", "safeHTML"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s uses %q. Criterion 13 / D4: these bodies are other people's mail and every value reaches "+
					"the page through html/template's contextual escaping", f, banned)
			}
		}
	}
}

// ---- criterion 12: the section's markup has no sink a body could reach ----------

// tdTag matches one opening tag in the template. Escaped body text (which is
// what a correct render produces for `<img src=x onerror=alert(1)>`) is NOT a
// tag and is deliberately invisible here — the template source has no body text
// in it at all, so anything this finds is markup the implementation chose.
var tdTag = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)([^>]*)>`)

// The tags the section may use. Anything else — a, img, iframe, script, object,
// embed, form — is either a sink or a link, and D4 bans both.
var tdAllowedTags = map[string]bool{
	"section": true, "h2": true, "h3": true, "p": true, "pre": true, "div": true, "span": true,
	"details": true, "summary": true, "table": true, "thead": true, "tbody": true,
	"tr": true, "th": true, "td": true, "ul": true, "li": true, "code": true,
	"em": true, "strong": true, "br": true, "hr": true, "small": true,
}

var tdBannedAttr = regexp.MustCompile(`(?i)\s(href|src|srcset|style|on[a-z]+)\s*=`)

func TestTaskTemplate_SourceSectionIsInert(t *testing.T) {
	whole := tdTaskHTML(t)
	sec := tdSection(t, whole)

	// CONTRACT DECISION (confirm at implementation): html/template STRIPS HTML
	// comments from its output, so D4's marker comments live in this SOURCE and
	// can never appear in a response. The RENDERED section is delimited by one
	// <section class="source-message"> … </section>, inside the markers, so the
	// integration test can slice it too — a scan of the whole page would let the
	// nav's own <a href> mask a failure (criterion 12).
	if n := strings.Count(sec, tdSectionTag); n != 1 {
		t.Errorf("the marker comments enclose %d %s elements, want exactly 1: html/template strips comments, so the "+
			"rendered page needs an element to slice on", n, tdSectionTag)
	}
	if n := strings.Count(sec, "<section"); n != 1 {
		t.Errorf("the source-message block opens %d <section> elements, want 1 (no nesting: the slice must be "+
			"unambiguous)", n)
	}

	for _, banned := range []string{"<a ", "<a>", "<img", "<iframe", "<script", "<object", "<embed", "<form"} {
		if strings.Contains(strings.ToLower(sec), banned) {
			t.Errorf("the source-message section contains %q. Criterion 12 / D4: no auto-linking, no remote images, "+
				"no HTML mail — the section renders inert text only", banned)
		}
	}
	for _, m := range tdTag.FindAllStringSubmatch(sec, -1) {
		name, attrs := strings.ToLower(m[1]), m[2]
		if !tdAllowedTags[name] {
			t.Errorf("the source-message section opens a <%s> tag (%q). Criterion 12: the section's markup is a "+
				"fixed, sink-free set of elements", name, strings.TrimSpace(m[0]))
		}
		if a := tdBannedAttr.FindString(attrs); a != "" {
			t.Errorf("the source-message section's <%s> carries a %q attribute (%q). Criterion 12 / D4: NO attribute "+
				"takes a body-derived value, so there is no href, src, style or on* sink to escape out of",
				name, strings.TrimSpace(a), strings.TrimSpace(m[0]))
		}
	}

	// D3: task.html has no <script> today and must still have none — the
	// dashboard structure tests count script tags and inline handlers.
	if n := strings.Count(strings.ToLower(whole), "<script"); n != 0 {
		t.Errorf("task.html contains %d <script occurrences, want 0 (criterion 12 / D3: the whole section is "+
			"server-rendered markup; nothing here needs JS)", n)
	}
	// No <details … open>: every collapsed block starts closed (criterion 9).
	for _, m := range tdTag.FindAllStringSubmatch(whole, -1) {
		if strings.EqualFold(m[1], "details") && regexp.MustCompile(`(?i)\bopen\b`).MatchString(m[2]) {
			t.Errorf("task.html renders %q — no <details> is rendered open (criteria 9, 12)", strings.TrimSpace(m[0]))
		}
	}
}

// ---- criterion 9 (structure): the rest of the thread is COLLAPSED --------------

func TestTaskTemplate_ThreadIsCollapsed(t *testing.T) {
	sec := tdSection(t, tdTaskHTML(t))
	lower := strings.ToLower(sec)
	if !strings.Contains(lower, "<details>") {
		t.Errorf("the source-message section has no <details> block. Criterion 9 / D3: every OTHER message on the " +
			"thread renders in its own CLOSED <details>")
	}
	if !strings.Contains(lower, "<summary") {
		t.Errorf("the source-message section has no <summary>. Criterion 9: the summary line is direction, sender, " +
			"sent_at and the first 120 runes")
	}
	// The source message itself renders INLINE, above the first <details> (D3:
	// "one message in full, the rest of the thread collapsed"). If the first
	// <details> opens before the body <pre>, the body is inside a fold.
	firstDetails := strings.Index(lower, "<details")
	firstPre := strings.Index(lower, "<pre")
	if firstPre < 0 {
		t.Errorf("the source-message section renders no <pre>: the source body renders in full, inline, verbatim " +
			"(criterion 7)")
	} else if firstDetails >= 0 && firstDetails < firstPre {
		t.Errorf("the section's first <details> (offset %d) opens before its first <pre> (offset %d): the SOURCE "+
			"message must not be inside the collapsed block (criteria 7, 9)", firstDetails, firstPre)
	}
}

// ---- criterion 6: surfaced_by_message_id is read by nothing on this path -------

func TestShowTask_DoesNotReadSurfacedBy(t *testing.T) {
	ps := parseDashboardSource(t)
	if _, ok := ps.text["showTask"]; !ok {
		t.Fatalf("internal/dashboard declares no showTask")
	}
	if _, ok := ps.text["loadSourceMessage"]; !ok {
		t.Errorf("internal/dashboard declares no loadSourceMessage. SPEC 'Files likely to touch': showTask calls " +
			"(*Server).loadSourceMessage after the existing reads (this is the positive control — without it the " +
			"scan below certifies an empty string)")
	}
	src, seen := ps.reach("showTask")
	if !seen["loadSourceMessage"] {
		t.Errorf("showTask does not reach loadSourceMessage: the section is not wired into the page")
	}
	if strings.Contains(src, "surfaced_by_message_id") {
		t.Errorf("showTask (or what it reaches) names surfaced_by_message_id. Criterion 6 / D1: it is a REVIVE " +
			"marker — the message that resurfaced a closed task — not a provenance pointer, and 0 of the 51 open " +
			"tasks carry it")
	}
	for _, f := range []string{"board.go", "sourcemessage.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		// A `//` comment line may explain why the column is absent (the SWT-19
		// keyspelling precedent: prose may quote what is gone).
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "surfaced_by_message_id") {
				t.Errorf("%s names surfaced_by_message_id outside a comment: %q (criterion 6)", f, strings.TrimSpace(line))
			}
		}
	}
}

// ---- criterion 14: MailAttachmentsForRawItem's callers are pinned --------------

func tdNonTestGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	// "." is internal/dashboard; ".." is internal/.
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(out) < 50 {
		t.Fatalf("POSITIVE CONTROL FAILED: the walk of internal/ found only %d non-test .go files", len(out))
	}
	return out
}

func TestMailAttachmentsForRawItem_CallersArePinned(t *testing.T) {
	const name = "MailAttachmentsForRawItem"
	var definition, callers []string
	for _, path := range tdNonTestGoFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		if !strings.Contains(src, name) {
			continue
		}
		if strings.Contains(src, "func "+name+"(") {
			definition = append(definition, path)
			continue
		}
		callers = append(callers, path)
	}
	if len(definition) != 1 || filepath.ToSlash(definition[0]) != "../tools/mailattach.go" {
		t.Errorf("%s is defined in %v, want exactly [../tools/mailattach.go] (D10: internal/dashboard imports no "+
			"connector package, and internal/tools already imports internal/connector/google — that is the seam)",
			name, definition)
	}
	if len(callers) == 0 {
		t.Errorf("POSITIVE CONTROL FAILED: no non-test file under internal/ CALLS %s. Criterion 14 pins the caller "+
			"set; with no call site the pin certifies nothing", name)
	}
	for _, path := range callers {
		if !strings.HasPrefix(filepath.ToSlash(path), "../dashboard/") {
			t.Errorf("%s names %s. Criterion 14 / D9: its only non-test callers are in internal/dashboard, and no "+
				"MCP-listed tool reaches it — the helper is UNGATED by design (it performs no locality judgement), "+
				"so the caller set is what keeps that decision from being quietly undone", path, name)
		}
	}
}

// ---- D8: no second spelling of an existing rule --------------------------------

// tdCodeLines returns path's lines with `//` comment lines dropped: prose may
// quote a spelling to explain it (the SWT-19 keyspelling precedent).
func tdCodeLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestMailThreadCaps_OneSpelling(t *testing.T) {
	// (a) the two caps and the order fragment are declared, exported, once.
	decls := map[string]*regexp.Regexp{
		"MailThreadMaxMessages": regexp.MustCompile(`MailThreadMaxMessages\s*=\s*50\b`),
		"MailThreadBodyCap":     regexp.MustCompile(`MailThreadBodyCap\s*=\s*8\s*\*\s*1024\b`),
		"LatestInboundOrder":    regexp.MustCompile(`LatestInboundOrder\s*=\s*"sent_at DESC, id DESC"`),
	}
	files := tdNonTestGoFiles(t)
	for name, re := range decls {
		found := 0
		for _, path := range files {
			for _, line := range tdCodeLines(t, path) {
				if re.MatchString(line) {
					found++
				}
			}
		}
		if found != 1 {
			t.Errorf("%s is declared %d times under internal/, want exactly 1 (D8: three constants MOVE rather than "+
				"get re-typed — the MCP thread read and the dashboard thread read must cut at the same width by "+
				"construction, the tools.TaskQueueOrder precedent)", name, found)
		}
	}

	// (b) the unexported spellings are gone: a surviving one is a second name
	// for the same number, which is how the two reads drift apart.
	for _, old := range []string{"mailThreadMaxMessages", "mailThreadBodyCap"} {
		for _, path := range files {
			for _, line := range tdCodeLines(t, path) {
				if strings.Contains(line, old) {
					t.Errorf("%s still spells %s (D8: it is EXPORTED as %s and mailReadThread uses the exported "+
						"name): %q", path, old, strings.ToUpper(old[:1])+old[1:], strings.TrimSpace(line))
				}
			}
		}
	}

	// (c) the order fragment appears exactly once on ITS OWN path — the
	// declaration. Scoped to internal/tools and internal/dashboard on purpose:
	// D8 moves the fragment latestInboundMessage and the dashboard's branch-3
	// subquery share. An unrelated `ORDER BY sent_at DESC, id DESC` over another
	// table (internal/triage/store.go has one) is not this rule and dragging it
	// in would be the "one spelling" scan certifying a coincidence.
	const frag = `sent_at DESC, id DESC`
	var sites []string
	for _, path := range files {
		p := filepath.ToSlash(path)
		if !strings.HasPrefix(p, "../tools/") && !strings.HasPrefix(p, "../dashboard/") {
			continue
		}
		for _, line := range tdCodeLines(t, path) {
			if strings.Contains(line, frag) {
				sites = append(sites, p+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(sites) != 1 || !strings.Contains(sites[0], "LatestInboundOrder") {
		t.Errorf("the order fragment %q must appear in code exactly once, at the LatestInboundOrder declaration; it "+
			"appears at %d site(s) and none of them is that declaration. latestInboundMessage AND the dashboard's "+
			"branch-3 subquery both BIND the const (D8, criterion 4) — the tools.TaskQueueOrder precedent.\n%s",
			frag, len(sites), strings.Join(sites, "\n"))
	}

	// (d) sourceBodyCap is the DASHBOARD's own number and is deliberately NOT
	// collapsed into MailThreadBodyCap (D8, mutation 18). One bounds a model's
	// context window; the other bounds one human's page.
	sm := "sourcemessage.go"
	if b, err := os.ReadFile(sm); err != nil {
		t.Errorf("read %s: %v (D8: sourceBodyCap lives here)", sm, err)
	} else {
		src := string(b)
		if !regexp.MustCompile(`sourceBodyCap\s*=\s*262144\b`).MatchString(src) {
			t.Errorf("%s does not declare sourceBodyCap = 262144 (D3: the 256 KiB safety cap, the only thing that "+
				"may drop text, and it says so when it does)", sm)
		}
		if regexp.MustCompile(`sourceBodyCap\s*=\s*(tools\.)?MailThreadBodyCap`).MatchString(src) {
			t.Errorf("%s defines sourceBodyCap AS MailThreadBodyCap. D8: the two cap numbers are deliberately "+
				"different, the SWT-64 shape — do not unify them", sm)
		}
	}
}

// ---- criterion 15's other half: the page style ---------------------------------
//
// swb 692 (Salvador, 2026-09-25: "the looks and feel on all the screens should
// match the board style and branding") replaced task.html's own light <style>
// block with the shared static/swb-1.css. What the SWT-65 pin protected still
// holds there: <pre> wraps (a long body stays readable at his 1000px tablet
// width, section D) and tables collapse to the page width.
func TestTaskTemplate_StyleBlockKeepsItsRules(t *testing.T) {
	s := tdTaskHTML(t)
	if !strings.Contains(s, `href="/static/swb-1.css"`) {
		t.Fatalf("task.html does not link static/swb-1.css (swb 692)")
	}
	css, err := staticFS.ReadFile("static/swb-1.css")
	if err != nil {
		t.Fatalf("read static/swb-1.css: %v", err)
	}
	for _, rule := range []string{"white-space: pre-wrap", "table { border-collapse: collapse; width: 100%;"} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("static/swb-1.css lost %q: a long body must stay readable without horizontal scroll at his "+
				"1000px tablet width (section D)", rule)
		}
	}
}

// D10, the other half: the helper exists so internal/dashboard never grows its
// own understanding of the IMAP envelope. Criterion 14 pins where
// MailAttachmentsForRawItem is CALLED from; this pins what the calling package
// may import, which is the property the helper was created to preserve.
//
// Test files are deliberately exempt: the integration suite builds RFC822
// fixtures with google's own writer and asserts its positive controls against
// google's walk rather than a hand-copied expectation — the opposite of a second
// implementation.
func TestDashboard_DoesNotImportAConnector(t *testing.T) {
	files := tdNonTestGoFiles(t)
	checked := 0
	for _, path := range files {
		if !strings.HasPrefix(filepath.ToSlash(path), "../dashboard/") {
			continue
		}
		checked++
		for _, line := range tdCodeLines(t, path) {
			if strings.Contains(line, "switchboard/internal/connector/") {
				t.Errorf("%s imports a connector package (%s). The dashboard reads stored mail through "+
					"tools.MailAttachmentsForRawItem and nothing else (D10): a second reader of the IMAP "+
					"envelope is a second thing to keep in step with the capture format",
					path, strings.TrimSpace(line))
			}
		}
	}
	// A scan over no files certifies nothing.
	if checked == 0 {
		t.Fatal("the scan found no non-test Go file under ../dashboard/ — it cannot prove anything")
	}
}
