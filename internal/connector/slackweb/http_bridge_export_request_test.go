package slackweb

// Regression tests for bug slackweb-collab-export-stale (Jira SWT-39), fix F on
// the HTTP transport: HTTPBridge.Export POSTs the request body the leaf now
// accepts, and decodes the coverage fields it now returns.
//
// Before the fix Export posted a nil body (http_bridge.go `b.post(ctx,
// "/export", nil)`), so the leaf could only read what its UI scrape
// enumerated: 6-8 of Collaboratory's 38 conversations per run.
//
// Contract: slackconnector src/switchboard/http-bridge.ts parseExportBody and
// export.ts SwitchboardExportWorkspace, fix/swt-39-export-coverage @ 050ed8c.
// Every failure of the leaf's validation is an HTTP 500 that stops the export
// for EVERY workspace, so the zero-value fields have to be ABSENT on the wire.
//
// EXPECTED RED: compile failure (ExportRequest, KnownConversation and the new
// Workspace fields are undefined; Export takes no request).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRegression_SWT39_HTTPBridgeExportPostsKnownAndBudget(t *testing.T) {
	var gotBody []byte
	var gotPath, gotType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"schema_version":1,"workspaces":[]}`))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	req := ExportRequest{
		Known: map[string][]KnownConversation{
			"T0HPR78RX": {
				{ID: "D0AUD86LKGA", LastSeenTS: "1789073346.665869", Name: "asunda45"},
				{ID: "C03J2KTN1PD"},
			},
		},
		BudgetMS:         1200000,
		MaxConversations: 60,
	}
	if _, err := bridge.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if gotPath != "/export" {
		t.Errorf("path = %q, want /export", gotPath)
	}
	if !strings.HasPrefix(gotType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}

	var got, want map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("the /export body is not a JSON object (%v): %q. Before SWT-39 it was empty, and the leaf "+
			"read only what its UI scrape enumerated", err, gotBody)
	}
	const wantBody = `{
	  "known": {"T0HPR78RX": [
	    {"id": "D0AUD86LKGA", "last_seen_ts": "1789073346.665869", "name": "asunda45"},
	    {"id": "C03J2KTN1PD"}
	  ]},
	  "budget_ms": 1200000,
	  "max_conversations": 60
	}`
	if err := json.Unmarshal([]byte(wantBody), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/export body =\n%s\nwant (key for key):\n%s\nThe second entry must carry NO last_seen_ts and "+
			"NO name: \"\" fails the leaf's /^\\d+(\\.\\d+)?$/ and 500s the whole export", gotBody, wantBody)
	}
}

// A zero request must not put a zero or a null on the wire. The leaf answers
// budget_ms: 0 / max_conversations: 0 ("must be a positive integer") and
// known: null ("must map workspace ids") with a 500. An empty body is fine:
// it is the leaf's old behaviour.
func TestRegression_SWT39_HTTPBridgeExportOmitsUnsetRequestFields(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"schema_version":1,"workspaces":[]}`))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Export(context.Background(), ExportRequest{}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(strings.TrimSpace(string(gotBody))) == 0 {
		return
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("a non-empty /export body must be a JSON object, got %q: %v", gotBody, err)
	}
	for _, key := range []string{"budget_ms", "max_conversations"} {
		if raw, ok := got[key]; ok {
			t.Errorf("zero request sent %s=%s; the leaf rejects a non-positive value with a 500", key, raw)
		}
	}
	if raw, ok := got["known"]; ok && string(raw) == "null" {
		t.Errorf("zero request sent known=null; the leaf rejects it with a 500")
	}
}

// The response side: the leaf's per-workspace coverage fields reach the caller
// through the HTTP transport, not only through a direct json.Unmarshal.
func TestRegression_SWT39_HTTPBridgeExportDecodesCoverage(t *testing.T) {
	const doc = `{"schema_version":1,"workspaces":[{
	  "id":"T0HPR78RX","name":"Collaboratory/LlamaSite","url":"https://app.slack.com/client/T0HPR78RX",
	  "own_user_id":"U0OWNER001","conversations":[],
	  "enumerated":[{"id":"D0AUD86LKGA","name":"asunda45","type":"dm","url":"https://app.slack.com/client/T0HPR78RX/D0AUD86LKGA","source":"known"}],
	  "read":[],
	  "unreadable":[],
	  "deferred":["D0AUD86LKGA"],
	  "coverage":{"enumerated_count":1,"known_count":38,"read_count":0,"unreadable_count":0,
	              "deferred_count":1,"budget_exhausted":true,"elapsed_ms":1200003}
	}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	exported, err := bridge.Export(context.Background(), ExportRequest{BudgetMS: 1200000, MaxConversations: 60})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	ws := exported.Workspaces[0]
	if len(ws.Enumerated) != 1 || ws.Enumerated[0].Source != "known" {
		t.Errorf("Enumerated = %+v, want the one known-sourced entry", ws.Enumerated)
	}
	if ws.Read == nil || len(ws.Read) != 0 {
		t.Errorf("Read = %#v, want a non-nil EMPTY slice: the leaf said it read nothing, which is not "+
			"the same as an old leaf that said nothing about reads", ws.Read)
	}
	if !reflect.DeepEqual(ws.Deferred, []string{"D0AUD86LKGA"}) {
		t.Errorf("Deferred = %v, want [D0AUD86LKGA]", ws.Deferred)
	}
	if ws.Coverage.KnownCount != 38 || ws.Coverage.DeferredCount != 1 || !ws.Coverage.BudgetExhausted {
		t.Errorf("Coverage = %+v, want known 38, deferred 1, budget_exhausted", ws.Coverage)
	}
}
