package dashboard

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criteria 29, 30 and 32: the
// /deliveries page's verbs. Nothing pinned the delivery buttons before this
// ticket (criterion 32). ZERO I/O beyond the embedded template and server.go.
//
// The form SHAPE is the contract: ONE inline POST form per row with a text
// input `note` and two submit buttons that differ only in `redraft`, so the
// note travels with whichever verdict is clicked. No onchange anywhere — a
// verdict that applies itself is a rejected draft nobody chose.
//
// GREENFIELD NOTE — EXPECTED RED: deliveries.html has no reject form, no
// rejected filter link and no .status-rejected style; server.go has no route.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDeliveriesTemplate_DenyAndRedoForm(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/deliveries.html")
	if err != nil {
		t.Fatalf("read embedded deliveries.html: %v", err)
	}
	s := string(raw)

	// Control: the Approve form exists and posts where the route is.
	if !strings.Contains(s, `action="/deliveries/{{.ID}}/approve"`) {
		t.Errorf("the Approve form no longer posts to /deliveries/{{.ID}}/approve")
	}

	reject := regexp.MustCompile(`<form[^>]*method="post"[^>]*action="/deliveries/\{\{\.ID\}\}/reject"` +
		`|<form[^>]*action="/deliveries/\{\{\.ID\}\}/reject"[^>]*method="post"`)
	if !reject.MatchString(s) {
		t.Errorf(`deliveries.html has no <form method="post" action="/deliveries/{{.ID}}/reject">. Criterion 30: ` +
			`one inline POST form per row, route POST /deliveries/{id}/reject (criterion 29)`)
	}
	for _, want := range []struct{ frag, why string }{
		{`name="note"`, "the optional free-text reason (D8) — what the redraft prompt consumes"},
		{`name="redraft" value="false">Deny</button>`, "Deny: rejected, never sent, no new draft"},
		{`name="redraft" value="true">Redo</button>`, "Redo: rejected, and the drafts worker writes a fresh one"},
		{`href="/deliveries?status=rejected"`, "the filter links gain `rejected`"},
		{`.status-rejected`, "the rejected status gets a style"},
		{`redraft requested`, "a rejected row shows \"redraft requested\" under its status when set"},
	} {
		if !strings.Contains(s, want.frag) {
			t.Errorf("deliveries.html lacks %s — %s (criterion 30)", want.frag, want.why)
		}
	}
	if n := strings.Count(s, "onchange"); n != 0 {
		t.Errorf("deliveries.html has %d onchange attribute(s), want 0 (criterion 30): nothing on this page may "+
			"submit itself", n)
	}
}

// Criterion 29: the route is on the auth-required mux and calls the executor
// with reject_delivery. (The handler's json.Marshal is proven end to end by
// deliveries_reject_integration_test.go's quote/backslash/newline note.)
func TestDeliveriesServer_RejectRoute(t *testing.T) {
	raw, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	s := string(raw)
	route := regexp.MustCompile(`mux\.Handle\("POST /deliveries/\{id\}/reject",\s*s\.auth\.Require\(`)
	if !route.MatchString(s) {
		t.Errorf(`server.go registers no mux.Handle("POST /deliveries/{id}/reject", s.auth.Require(...)). ` +
			`Criterion 29: the route lives on the auth-required mux like every other delivery verb`)
	}
	if !strings.Contains(s, `"reject_delivery"`) {
		t.Errorf("server.go never names the reject_delivery tool; the handler calls s.execute(..., \"reject_delivery\", ...)")
	}
}
