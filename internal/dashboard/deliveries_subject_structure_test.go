package dashboard

// Floor 3 of three for bug gmail-reply-empty-subject-off-thread (Jira SWT-61):
// the review surface must SHOW the delivery's own subject, and must say so when
// there isn't one.
//
// WHAT HAPPENED. The "Goes to" cell shows `Thread: {{.ThreadSubject}}` — the
// THREAD's subject, resolved by tools.ResolveGmailRoute — while the delivery's
// own subject appears only as the `value=` of a text input. Salvador approved
// delivery #36 looking at a plausible thread subject next to an empty input;
// nothing on the page marked the row as subject-less, and it went to a client
// with no Subject header at all.
//
// The two subjects are DIFFERENT things (the thread's is frozen first-writer-
// wins and goes stale: prod thread 159886), so the page must distinguish them
// at a glance.
//
// This test RENDERS the template rather than grepping it: the contract is what
// a reviewer sees, and an assertion on markup would pass on a subject hidden in
// an attribute — which is exactly the defect.
//
// EXPECTED RED: deliveries.html renders no "(no subject)" marker, and on a
// drafted row the subject exists only inside value="...".

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"
	"testing"
)

// valueAttr strips every value="..." attribute, so "the subject is visible"
// cannot be satisfied by an input's value — the #36 defect exactly.
var valueAttr = regexp.MustCompile(`value="[^"]*"`)

func renderDeliveries(t *testing.T, rows ...deliveryRow) string {
	t.Helper()
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "deliveries.html", pageData{Deliveries: rows}); err != nil {
		t.Fatalf("render deliveries.html: %v", err)
	}
	return buf.String()
}

// A gmail row awaiting a human verdict shows the words that would be sent —
// including the fact that there are none.
func TestDeliveriesTemplate_MarksASubjectLessRow(t *testing.T) {
	for _, status := range []string{"drafted", "approved"} {
		t.Run(status, func(t *testing.T) {
			html := renderDeliveries(t, deliveryRow{
				ID: 36, TaskID: 164, TaskTitle: "placeholder task", Channel: "gmail",
				Status: status, Subject: "", Body: "placeholder body",
				CreatedBy: "mcp:manual:salvo", ThreadSubject: "placeholder thread subject",
				From: "placeholder-from@example.com", To: "placeholder-to@example.com",
			})
			if !strings.Contains(html, "(no subject)") {
				t.Errorf("a %s gmail row with an empty subject renders no \"(no subject)\" marker. SWT-61: the "+
					"dashboard is the review surface, and #36 was approved because an empty subject was "+
					"indistinguishable from an unread one — the Thread: line showed a plausible subject next to "+
					"an empty input.\n%s", status, html)
			}
		})
	}
}

// ...and when there IS a subject, the reviewer can read it as text, not only as
// the value of an edit box.
func TestDeliveriesTemplate_RendersTheDeliverySubjectAsText(t *testing.T) {
	const subject = "Re: placeholder subject"
	for _, status := range []string{"drafted", "approved"} {
		t.Run(status, func(t *testing.T) {
			html := renderDeliveries(t, deliveryRow{
				ID: 35, TaskID: 164, TaskTitle: "placeholder task", Channel: "gmail",
				Status: status, Subject: subject, Body: "placeholder body",
				CreatedBy: "opsctl:salvo", ThreadSubject: "placeholder thread subject",
				From: "placeholder-from@example.com", To: "placeholder-to@example.com",
			})
			if strings.Contains(html, "(no subject)") {
				t.Errorf("a %s row WITH a subject is marked \"(no subject)\"", status)
			}
			visible := valueAttr.ReplaceAllString(html, `value=""`)
			if !strings.Contains(visible, subject) {
				t.Errorf("a %s gmail row's subject %q appears only inside a value=\"...\" attribute. SWT-61: the "+
					"delivery's OWN subject must be readable next to the Thread: line — they are different "+
					"strings (the thread's is frozen first-writer-wins and goes stale)\n%s", status, subject, html)
			}
		})
	}
}

// Control: the thread's subject keeps its own label, so the two cannot be
// confused for one another. GREEN today; it must stay that way.
func TestDeliveriesTemplate_ThreadSubjectStaysLabelled(t *testing.T) {
	html := renderDeliveries(t, deliveryRow{
		ID: 36, TaskID: 164, Channel: "gmail", Status: "drafted",
		Subject: "", Body: "placeholder body",
		From: "placeholder-from@example.com", To: "placeholder-to@example.com",
		ThreadSubject: "placeholder thread subject",
	})
	if !strings.Contains(html, "Thread: placeholder thread subject") {
		t.Errorf("the Thread: line is gone; the page must show BOTH the thread's subject and the delivery's "+
			"own, distinguishably (SWT-61)\n%s", html)
	}
}

// Control: the marker is GMAIL-SCOPED. jira_comment, slack_reply and
// upwork_chat legitimately carry no subject — their send paths never read one
// (delivery.go:1557, 1651; upwork has no direct send path) — so marking every
// such row would put a permanent "(no subject)" on the board. An alarm that is
// always on is one nobody reads, and the row it needs to flag is the gmail one.
func TestDeliveriesTemplate_NoSubjectMarkerIsGmailOnly(t *testing.T) {
	for _, channel := range []string{"slack_reply", "jira_comment", "upwork_chat"} {
		t.Run(channel, func(t *testing.T) {
			html := renderDeliveries(t, deliveryRow{
				ID: 40, TaskID: 164, TaskTitle: "placeholder task", Channel: channel,
				Status: "drafted", Subject: "", Body: "placeholder body",
				CreatedBy: "mcp:manual:salvo",
			})
			if strings.Contains(html, "(no subject)") {
				t.Errorf("a subject-less %s row is marked \"(no subject)\". SWT-61's subject rule is gmail-scoped: "+
					"these channels never send a subject, so the marker would be permanently lit and stop being "+
					"read on the one row that matters.\n%s", channel, html)
			}
		})
	}
}
