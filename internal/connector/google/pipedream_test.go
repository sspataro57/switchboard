package google

// Unit tests for the Pipedream calendar HTTP leaf (pipedream-calendar /
// docs/tickets/pipedream-calendar_SPEC.md, acceptance criteria 3, 4, 5 and the
// transport half of 6). ZERO network beyond loopback, ZERO Pipedream
// credentials: every case runs against an httptest server, which is criterion
// 21 ("every test runs against an httptest fake with zero Pipedream
// credentials and zero network").
//
// This file is the sibling of internal/connector/slackweb/http_bridge_test.go
// because the client is the sibling of internal/connector/slackweb/http_bridge.go
// (SPEC "Sibling patterns to copy"): construction-time URL/token validation, a
// Bearer header, LimitReader(cap+1) so hitting the cap is detectable, and a
// status error carrying a truncated snippet. What is NOT copied is
// SendRejectedError — a read poll has no "the click may have landed" hazard.
//
// GREENFIELD NOTE: none of this exists yet, so this file compile-FAILS — the
// expected red for SPEC-named greenfield surface. Imposed contract
// (internal/connector/google/pipedream.go; the SPEC names the identifiers in
// "Files likely to touch" and the envelope in "API / MCP tool changes", the
// Go spelling is imposed here):
//
//	const (
//	    PipedreamCalendarSchemaVersion = 1
//	    PipedreamMaxEvents             = 10000     // matches BridgeMaxEvents
//	    PipedreamMaxResponseBytes      = 16 << 20  // the calendar.go:97 read cap
//	)
//
//	type PipedreamCalendarRequest struct {
//	    SchemaVersion int      `json:"schema_version"`
//	    TimeMin       string   `json:"time_min"`
//	    TimeMax       string   `json:"time_max"`
//	    Calendars     []string `json:"calendars"`
//	}
//
//	type PipedreamCalendarEntry struct {
//	    CalendarID string            `json:"calendar_id"`
//	    Status     string            `json:"status"`
//	    EventCount *int              `json:"event_count"` // POINTER: absent is a refusal, not a zero
//	    Events     []json.RawMessage `json:"events"`
//	    Error      string            `json:"error"`
//	}
//
//	type PipedreamCalendarResponse struct {
//	    SchemaVersion int                      `json:"schema_version"`
//	    TimeMin       string                   `json:"time_min"`
//	    TimeMax       string                   `json:"time_max"`
//	    Calendars     []PipedreamCalendarEntry `json:"calendars"`
//	}
//
//	// PipedreamCalendarSource is the seam RunPipedreamCalendar consumes, so
//	// the ingest half is testable with a fake and no HTTP at all.
//	type PipedreamCalendarSource interface {
//	    FetchCalendars(ctx context.Context, req PipedreamCalendarRequest) (PipedreamCalendarResponse, error)
//	}
//
//	// PipedreamCalendarClient implements it over HTTP. maxBytes is a field
//	// rather than the constant read inline so the overflow boundary is
//	// testable without allocating 16 MiB — slackweb's HTTPBridge.maxBytes,
//	// verbatim.
//	type PipedreamCalendarClient struct { ...; maxBytes int64 }
//	func NewPipedreamCalendarClient(rawURL, token string, hc *http.Client) (*PipedreamCalendarClient, error)
//	func PipedreamTokenFromEnv() (string, error)

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pipedreamTestToken is 43 characters and deliberately distinctive: criterion 4
// greps returned errors for it.
const pipedreamTestToken = "pdtok-DISTINCTIVE-0123456789abcdefghijklmno"

// pipedreamTestPath is the workflow path. Pipedream endpoint URLs ARE the
// credential's neighbour — a leaked endpoint plus a leaked token is the whole
// secret — so criterion 4 greps for the path too.
const pipedreamTestPath = "/p_DISTINCTIVEworkflowpath"

// okEnvelope is a minimal valid response body.
func okEnvelope(timeMin, timeMax string) string {
	return `{"schema_version":1,"time_min":"` + timeMin + `","time_max":"` + timeMax + `","calendars":[]}`
}

// ---------------------------------------------------------------------------
// Criterion 3: the constructor validates the endpoint and the credential, so a
// misconfigured deployment fails at STARTUP. The line this draws is the whole
// point: a configuration error must happen before any sync_runs row exists,
// which is only possible if the failure is at construction rather than mid-poll
// (the run-row half of that line is asserted in the cmd integration suite).
// ---------------------------------------------------------------------------

func TestNewPipedreamCalendarClient_ValidatesEndpointAndTokenAtConstruction(t *testing.T) {
	cases := map[string]struct {
		url, token string
		wantErr    bool
	}{
		"empty url":              {"", pipedreamTestToken, true},
		"bad scheme":             {"ftp://eo.pipedream.net/p_abc", pipedreamTestToken, true},
		"no host":                {"http://", pipedreamTestToken, true},
		"not a url":              {"://nope", pipedreamTestToken, true},
		"empty token":            {"https://eo.pipedream.net/p_abc", "", true},
		"token one char short":   {"https://eo.pipedream.net/p_abc", strings.Repeat("k", 31), true},
		"token exactly 32 chars": {"https://eo.pipedream.net/p_abc", strings.Repeat("k", 32), false},
		"https ok":               {"https://eo.pipedream.net/p_abc", pipedreamTestToken, false},
		"http ok":                {"http://localhost:9999/p_abc", pipedreamTestToken, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewPipedreamCalendarClient(tc.url, tc.token, nil)
			if tc.wantErr && err == nil {
				t.Fatalf("NewPipedreamCalendarClient(%q, %d-char token) returned no error. A misconfigured "+
					"deployment must fail at startup rather than mid-ingest: criterion 3 says a configuration "+
					"error writes NOTHING AT ALL, and that is only true if it is caught before the account "+
					"loop opens a single sync_runs row", tc.url, len(tc.token))
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("NewPipedreamCalendarClient(%q, %d-char token) = %v, want a client", tc.url, len(tc.token), err)
			}
		})
	}
}

// Criterion 3: the credential comes from the environment only, preferring a
// FILE so the secret can be mounted rather than kept in the process
// environment (slackweb's TokenFromEnv, verbatim). Never from source_accounts,
// never from a flag, never from a migration.
func TestPipedreamTokenFromEnv_PrefersTheMountedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(pipedreamTestToken+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Setenv("PIPEDREAM_CALENDAR_TOKEN", "the-inline-one-that-must-lose")
	t.Setenv("PIPEDREAM_CALENDAR_TOKEN_FILE", path)
	got, err := PipedreamTokenFromEnv()
	if err != nil {
		t.Fatalf("PipedreamTokenFromEnv with a file: %v", err)
	}
	if got != pipedreamTestToken {
		t.Errorf("PipedreamTokenFromEnv = %q, want the file's contents trimmed. The FILE wins so the secret "+
			"can be mounted (kube: --from-file, the recorded --from-literal landmine)", got)
	}

	t.Setenv("PIPEDREAM_CALENDAR_TOKEN_FILE", "")
	got, err = PipedreamTokenFromEnv()
	if err != nil {
		t.Fatalf("PipedreamTokenFromEnv without a file: %v", err)
	}
	if got != "the-inline-one-that-must-lose" {
		t.Errorf("PipedreamTokenFromEnv = %q, want the PIPEDREAM_CALENDAR_TOKEN fallback", got)
	}
}

// ---------------------------------------------------------------------------
// Criterion 5: one POST per pass to the single endpoint, Bearer auth, JSON
// content type, and the exact envelope. The window itself is computed by
// RunPipedreamCalendar from google.CalendarWindowPast/Future (asserted in
// pipedream_ingest_test.go); this test pins that the client transmits what it
// is handed, unaltered, and adds nothing of its own.
// ---------------------------------------------------------------------------

func TestPipedreamCalendarClient_PostsBearerAndTheRequestEnvelope(t *testing.T) {
	var (
		gotMethod, gotAuth, gotType, gotPath string
		gotBody                              []byte
		calls                                int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okEnvelope("2026-08-06T09:00:00Z", "2026-12-04T09:00:00Z")))
	}))
	defer srv.Close()

	client, err := NewPipedreamCalendarClient(srv.URL+pipedreamTestPath, pipedreamTestToken, srv.Client())
	if err != nil {
		t.Fatalf("NewPipedreamCalendarClient: %v", err)
	}
	req := PipedreamCalendarRequest{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		TimeMin:       "2026-08-06T09:00:00Z",
		TimeMax:       "2026-12-04T09:00:00Z",
		Calendars:     []string{"a@example.com", "b@example.com", "c@example.com"},
	}
	if _, err := client.FetchCalendars(context.Background(), req); err != nil {
		t.Fatalf("FetchCalendars: %v", err)
	}

	if calls != 1 {
		t.Errorf("the client made %d HTTP calls for one pass, want exactly 1 (the SPEC's one-workflow "+
			"decision is what keeps ~72 invocations/day inside the free tier)", calls)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != pipedreamTestPath {
		t.Errorf("path = %q, want the configured workflow path %q", gotPath, pipedreamTestPath)
	}
	if gotAuth != "Bearer "+pipedreamTestToken {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer <token>")
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, gotBody)
	}
	if sent["schema_version"] != float64(PipedreamCalendarSchemaVersion) {
		t.Errorf("request schema_version = %v, want %d", sent["schema_version"], PipedreamCalendarSchemaVersion)
	}
	if sent["time_min"] != "2026-08-06T09:00:00Z" || sent["time_max"] != "2026-12-04T09:00:00Z" {
		t.Errorf("request window = %v..%v, want the caller's bounds transmitted unaltered "+
			"(the echo check in criterion 6 compares against exactly these)", sent["time_min"], sent["time_max"])
	}
	cals, _ := sent["calendars"].([]any)
	if len(cals) != 3 {
		t.Fatalf("request calendars = %v, want the three in-scope emails", sent["calendars"])
	}
	// Criterion 5's last clause: the request carries NO switchboard data beyond
	// the account emails and the window. Nothing about tasks, deliveries,
	// messages or clients transits a third party.
	for _, key := range []string{"tasks", "deliveries", "messages", "projects", "clients", "account_ids", "token"} {
		if _, present := sent[key]; present {
			t.Errorf("request body carries %q. Criterion 5: the request carries no switchboard data beyond "+
				"the account emails and the window — full payload was %s", key, gotBody)
		}
	}
	if len(sent) != 4 {
		t.Errorf("request body has %d top-level keys (%v), want exactly 4: schema_version, time_min, "+
			"time_max, calendars", len(sent), gotBody)
	}
}

// ---------------------------------------------------------------------------
// Criterion 4: NO SECRET REACHES AN ERROR STRING. Four failure shapes, and in
// none of them may the returned error contain the token or the URL's host or
// path.
//
// The dial-failure case is the demanding one and is meant to be: Go's transport
// error is a *url.Error whose message embeds the whole URL, so wrapping it with
// %w leaks the endpoint into every CronJob log. The implementation must
// classify or scrub rather than wrap — that is the criterion, not an accident
// of this test.
// ---------------------------------------------------------------------------

func TestPipedreamCalendarClient_ErrorsCarryNoTokenAndNoEndpoint(t *testing.T) {
	longBody := strings.Repeat("workflow blew up. ", 40) // > 200 chars

	cases := []struct {
		name       string
		handler    http.HandlerFunc
		dead       bool // point at an unroutable host instead of the server
		wantInErr  string
		wantStatus bool
	}{
		{
			name: "401 unauthorized",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			},
			wantInErr: "401", wantStatus: true,
		},
		{
			name: "500 with a long body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, longBody, http.StatusInternalServerError)
			},
			wantInErr: "500", wantStatus: true,
		},
		{
			name: "200 with a body that is not JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("<html>Pipedream is having a moment</html>"))
			},
		},
		{
			name: "dial failure",
			dead: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := ""
			var hc *http.Client
			if tc.dead {
				// .invalid never resolves (RFC 2606), so this is a dial
				// failure with no network dependency at all.
				endpoint = "http://pipedream-DISTINCTIVEHOST.invalid" + pipedreamTestPath
			} else {
				srv := httptest.NewServer(tc.handler)
				defer srv.Close()
				endpoint, hc = srv.URL+pipedreamTestPath, srv.Client()
			}
			parsed, err := url.Parse(endpoint)
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}

			client, err := NewPipedreamCalendarClient(endpoint, pipedreamTestToken, hc)
			if err != nil {
				t.Fatalf("NewPipedreamCalendarClient: %v", err)
			}
			_, err = client.FetchCalendars(context.Background(), PipedreamCalendarRequest{
				SchemaVersion: PipedreamCalendarSchemaVersion,
				TimeMin:       "2026-08-06T09:00:00Z",
				TimeMax:       "2026-12-04T09:00:00Z",
				Calendars:     []string{"a@example.com"},
			})
			if err == nil {
				t.Fatalf("FetchCalendars succeeded on %s; every one of these is a refusal (criterion 6)", tc.name)
			}
			msg := err.Error()

			if strings.Contains(msg, pipedreamTestToken) {
				t.Errorf("the error leaks the bearer token: %q", msg)
			}
			if strings.Contains(msg, parsed.Host) {
				t.Errorf("the error leaks the endpoint host %q: %q. Criterion 4: a Pipedream endpoint URL is "+
					"the token's neighbour — an endpoint in a CronJob log is half the secret", parsed.Host, msg)
			}
			if strings.Contains(msg, parsed.Path) {
				t.Errorf("the error leaks the workflow path %q: %q", parsed.Path, msg)
			}
			if tc.wantInErr != "" && !strings.Contains(msg, tc.wantInErr) {
				t.Errorf("error %q does not carry the HTTP status %s; an operator needs to tell 401 "+
					"(rotate the token) from 500 (the workflow is broken)", msg, tc.wantInErr)
			}
			if tc.wantStatus && len(msg) > 400 {
				t.Errorf("status error is %d characters; the body snippet is capped at 200 so a failure "+
					"cannot dump calendar content into logs (slackweb http_bridge.go:107-115)", len(msg))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 6: a body AT OR OVER PipedreamMaxResponseBytes is a refusal,
// detected with the cap+1 LimitReader trick.
//
// NOTE THE DELIBERATE DIFFERENCE FROM SLACKWEB, which accepts a body exactly at
// its cap. The SPEC says "at or over", so the comparison is >=. The reason the
// boundary matters at all: a complete document padded to exactly the cap, with
// more bytes behind it, is the one truncation that stays valid JSON — it would
// otherwise be ingested as a complete snapshot, and a snapshot is a REPLACEMENT
// (criterion 10). A truncated snapshot silently supersedes real events.
// ---------------------------------------------------------------------------

func TestPipedreamCalendarClient_RefusesABodyAtOrOverTheCap(t *testing.T) {
	doc := okEnvelope("2026-08-06T09:00:00Z", "2026-12-04T09:00:00Z")

	for _, tc := range []struct {
		name string
		cap  int64
		body string
	}{
		{name: "exactly at the cap", cap: int64(len(doc)), body: doc},
		{
			name: "over the cap, still valid JSON",
			cap:  int64(len(doc)),
			body: doc + strings.Repeat(" ", 64),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client, err := NewPipedreamCalendarClient(srv.URL+pipedreamTestPath, pipedreamTestToken, srv.Client())
			if err != nil {
				t.Fatalf("NewPipedreamCalendarClient: %v", err)
			}
			client.maxBytes = tc.cap

			if _, err := client.FetchCalendars(context.Background(), PipedreamCalendarRequest{
				SchemaVersion: PipedreamCalendarSchemaVersion,
				Calendars:     []string{"a@example.com"},
			}); err == nil {
				t.Fatalf("a %d-byte body against a %d-byte cap was accepted. Criterion 6 refuses at or over "+
					"the cap: a snapshot is a full REPLACEMENT, so a truncated one that still parses would "+
					"supersede every event it lost", len(tc.body), tc.cap)
			}
		})
	}
}

// A comfortably-under-cap body is accepted: the guard rejects overflow, not
// success. Without this control the cap test passes on a client that refuses
// everything.
func TestPipedreamCalendarClient_AcceptsABodyUnderTheCap(t *testing.T) {
	doc := okEnvelope("2026-08-06T09:00:00Z", "2026-12-04T09:00:00Z")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()

	client, err := NewPipedreamCalendarClient(srv.URL+pipedreamTestPath, pipedreamTestToken, srv.Client())
	if err != nil {
		t.Fatalf("NewPipedreamCalendarClient: %v", err)
	}
	client.maxBytes = int64(len(doc)) + 1

	resp, err := client.FetchCalendars(context.Background(), PipedreamCalendarRequest{
		SchemaVersion: PipedreamCalendarSchemaVersion,
		Calendars:     []string{"a@example.com"},
	})
	if err != nil {
		t.Fatalf("a body under the cap was refused: %v", err)
	}
	if resp.SchemaVersion != PipedreamCalendarSchemaVersion {
		t.Errorf("decoded schema_version = %d, want %d", resp.SchemaVersion, PipedreamCalendarSchemaVersion)
	}
	if resp.TimeMin != "2026-08-06T09:00:00Z" || resp.TimeMax != "2026-12-04T09:00:00Z" {
		t.Errorf("the client dropped the echoed window (%q..%q); criterion 6's echo check has nothing to "+
			"compare without it", resp.TimeMin, resp.TimeMax)
	}
}

// The constants are part of the contract: PipedreamMaxEvents matches
// BridgeMaxEvents (criterion 8) and PipedreamMaxResponseBytes matches the read
// cap calendar.go already uses (criterion 6). Pinned so a "tidy-up" that
// re-spells either one has to argue with a test.
func TestPipedreamConstantsMatchTheirSiblings(t *testing.T) {
	if PipedreamCalendarSchemaVersion != 1 {
		t.Errorf("PipedreamCalendarSchemaVersion = %d, want 1 (the envelope in the SPEC)", PipedreamCalendarSchemaVersion)
	}
	if PipedreamMaxEvents != BridgeMaxEvents {
		t.Errorf("PipedreamMaxEvents = %d but BridgeMaxEvents = %d; criterion 8 pins them equal (10000)",
			PipedreamMaxEvents, BridgeMaxEvents)
	}
	if PipedreamMaxResponseBytes != 16<<20 {
		t.Errorf("PipedreamMaxResponseBytes = %d, want 16 MiB — the read cap calendar.go already uses",
			PipedreamMaxResponseBytes)
	}
}
