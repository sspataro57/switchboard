package slackweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123"

// A response that is a complete document padded to exactly the cap, with more
// bytes behind it, is the one truncation that stays valid JSON: cutting an
// object mid-structure fails to parse on its own, but this does not. It must be
// refused on the length, not on the parse.
func TestHTTPBridgeExportRejectsTruncatedBodyThatStaysValidJSON(t *testing.T) {
	const cap = 128
	doc := `{"schema_version":1,"workspaces":[{"id":"T1","name":"n","url":"u","own_user_id":"U1","conversations":[]}`
	padded := doc + strings.Repeat(" ", cap-len(doc))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(padded + `,{"id":"T2"}]}`))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	bridge.maxBytes = cap

	_, err = bridge.Export(context.Background())
	if err == nil {
		t.Fatal("expected a truncated export to be refused, not ingested as complete")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected an overflow error naming the cap, got %v", err)
	}
}

func TestHTTPBridgeExportAcceptsBodyExactlyAtCap(t *testing.T) {
	doc := `{"schema_version":1,"workspaces":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the cap must still pass: the guard rejects overflow, not fullness.
	bridge.maxBytes = int64(len(doc))

	if _, err := bridge.Export(context.Background()); err != nil {
		t.Fatalf("a body exactly at the cap must be accepted, got %v", err)
	}
}

func TestNewHTTPBridgeRejectsBadConfig(t *testing.T) {
	cases := map[string]struct{ url, token string }{
		"empty url":   {"", testToken},
		"bad scheme":  {"ftp://mini:8787", testToken},
		"no host":     {"http://", testToken},
		"short token": {"http://mini:8787", "tooshort"},
		"empty token": {"http://mini:8787", ""},
		"not a url":   {"://nope", testToken},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHTTPBridge(tc.url, tc.token, nil); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestHTTPBridgeExportSendsBearerAndChecksSchema(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"workspaces":[]}`))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	exported, err := bridge.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exported.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d", exported.SchemaVersion)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if gotPath != "/export" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestHTTPBridgeExportRejectsWrongSchemaVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":999,"workspaces":[]}`))
	}))
	defer server.Close()

	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
	if _, err := bridge.Export(context.Background()); err == nil {
		t.Fatal("expected a schema version mismatch to fail closed")
	}
}

func TestHTTPBridgeSurfacesNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
	_, err := bridge.Export(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a 401 to surface, got %v", err)
	}
}

func TestHTTPBridgeDraftRejectsClaimedSend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A bridge that claims it sent must never be accepted.
		_, _ = w.Write([]byte(`{"drafted":true,"sent":true}`))
	}))
	defer server.Close()

	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
	if err := bridge.Draft(context.Background(), "https://app.slack.com/client/T1/C1", "hi"); err == nil {
		t.Fatal("expected an unsafe draft result to be rejected")
	}
}

func TestHTTPBridgeDraftAcceptsDraftOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/draft" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"drafted":true,"sent":false}`))
	}))
	defer server.Close()

	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
	if err := bridge.Draft(context.Background(), "https://app.slack.com/client/T1/C1", "hi"); err != nil {
		t.Fatal(err)
	}
}
