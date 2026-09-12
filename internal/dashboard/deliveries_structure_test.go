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
//
// SWT-44 review fix 1 (content-bound approval), zero db: the Approve form
// carries the hash of the words the page rendered, and the approve handler
// passes it through as expect_content_hash, built with json.Marshal — a form
// value is caller-controlled text, and a Sprintf'd JSON object would let it
// add or replace keys (delivery_id included).
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - drop the hidden input → TestDeliveriesTemplate_ApproveFormCarriesContentHash.
//   - route approve back through action() (no hash) → the handler test.
//   - Sprintf the args → the injection row.
//   - forward a hash-less POST to the executor → TestApproveAction_RefusesAPostWithoutTheHash.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
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

func TestDeliveriesTemplate_ApproveFormCarriesContentHash(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/deliveries.html")
	if err != nil {
		t.Fatalf("read embedded deliveries.html: %v", err)
	}
	m := regexp.MustCompile(`(?s)<form[^>]*action="/deliveries/\{\{\.ID\}\}/approve"[^>]*>(.*?)</form>`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("deliveries.html has no Approve form posting to /deliveries/{{.ID}}/approve")
	}
	inner := m[1]
	for _, want := range []string{`type="hidden"`, `name="content_hash"`, `value="{{.ContentHash}}"`} {
		if !strings.Contains(inner, want) {
			t.Errorf("the Approve form lacks %s; without the rendered hash an edit made after the page loaded is "+
				"approved unseen (SWT-44). Form: %s", want, inner)
		}
	}
}

type captureExec struct{ calls []executor.Call }

func (c *captureExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	c.calls = append(c.calls, call)
	return executor.Result{}, nil
}

func TestApproveAction_PassesTheHashAsJSON(t *testing.T) {
	auth, err := NewAuth(context.Background(), "", "", "", "")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	for _, tc := range []struct {
		name, hash string
		want       map[string]any
	}{
		{"hash", "ab12", map[string]any{"delivery_id": float64(7), "expect_content_hash": "ab12"}},
		{"injection", `x","delivery_id":9,"y":"`, map[string]any{"delivery_id": float64(7), "expect_content_hash": `x","delivery_id":9,"y":"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &captureExec{}
			s := &Server{ex: ex, auth: auth}
			form := url.Values{}
			if tc.hash != "" {
				form.Set("content_hash", tc.hash)
			}
			req := httptest.NewRequest(http.MethodPost, "/deliveries/7/approve", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("id", "7")
			s.approveAction(httptest.NewRecorder(), req)
			if len(ex.calls) != 1 || ex.calls[0].Tool != "approve_delivery" {
				t.Fatalf("executor calls = %+v, want one approve_delivery", ex.calls)
			}
			var got map[string]any
			if err := json.Unmarshal(ex.calls[0].Args, &got); err != nil {
				t.Fatalf("approve args %s are not JSON: %v", ex.calls[0].Args, err)
			}
			if len(got) != len(tc.want) {
				t.Errorf("approve args = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("approve args[%s] = %v, want %v (args %s)", k, got[k], v, ex.calls[0].Args)
				}
			}
		})
	}
}

// SWT-44 second review (Codex): the dashboard is the review surface, so its
// Approve REQUIRES the content hash. A POST without one — a page rendered
// before the deploy, or a crafted POST — is refused before the executor, with
// a flash telling him to reload and review; it never becomes an unbound
// approve. (approve_delivery itself keeps the hash optional for opsctl and
// the full-profile MCP; this is the dashboard route only.)
//
// MUTATION: send the call without the hash, as before → "reached the executor".
func TestApproveAction_RefusesAPostWithoutTheHash(t *testing.T) {
	auth, err := NewAuth(context.Background(), "", "", "", "")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"absent", url.Values{}},
		{"empty", url.Values{"content_hash": {""}}},
		{"blank", url.Values{"content_hash": {"   "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &captureExec{}
			s := &Server{ex: ex, auth: auth}
			req := httptest.NewRequest(http.MethodPost, "/deliveries/7/approve", strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("id", "7")
			rec := httptest.NewRecorder()
			s.approveAction(rec, req)
			if len(ex.calls) != 0 {
				t.Fatalf("an approve POST with no content_hash reached the executor: %+v — the dashboard approve must "+
					"be bound to the words it showed", ex.calls)
			}
			if rec.Code != http.StatusSeeOther {
				t.Errorf("status = %d, want %d (back to /deliveries with a flash)", rec.Code, http.StatusSeeOther)
			}
			loc, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("Location %q: %v", rec.Header().Get("Location"), err)
			}
			if loc.Path != "/deliveries" || !strings.Contains(loc.Query().Get("flash"), "reload the page and review it again") {
				t.Errorf("redirect = %q, want /deliveries with a flash saying to reload the page and review it again",
					rec.Header().Get("Location"))
			}
		})
	}
}
