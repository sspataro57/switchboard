package dashboard

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

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

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
		// No hash in the form (a page from before SWT-44): the dashboard sends
		// none rather than an empty one, and the tool keeps today's behaviour.
		{"absent", "", map[string]any{"delivery_id": float64(7)}},
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
