package google

// The Pipedream calendar HTTP leaf (SWT-27): one POST per pass to a
// Pipedream HTTP-triggered workflow that holds the Google OAuth grant and
// returns full Calendar v3 event objects for the in-scope calendars. The
// cluster keeps no Google calendar credential at all.
//
// Sibling of internal/connector/slackweb/http_bridge.go, deliberately:
// construction-time URL/token validation (a misconfigured deployment fails at
// startup, before any sync_runs row exists), Bearer auth, a capped read with
// the cap+1 LimitReader trick, a status error carrying a truncated snippet,
// and a token read preferring a mounted file. NOT copied: SendRejectedError —
// a read poll has no "the click may have landed" hazard.
//
// SECRETS NEVER REACH ERROR STRINGS. The endpoint URL is the token's
// neighbour — a leaked workflow path plus a leaked token is the whole secret —
// so no returned error may carry the token or the URL's host/path. That is why
// the transport error below is CLASSIFIED rather than %w-wrapped: Go's
// *url.Error embeds the whole URL, and the net errors under it embed the host.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// PipedreamCalendarSchemaVersion is the envelope contract version.
	PipedreamCalendarSchemaVersion = 1
	// PipedreamMaxEvents matches BridgeMaxEvents: an entry AT the cap is
	// indistinguishable from a truncated one.
	PipedreamMaxEvents = BridgeMaxEvents
	// PipedreamMaxResponseBytes is the read cap calendar.go already uses.
	PipedreamMaxResponseBytes = 16 << 20
	// pipedreamRequestTimeout bounds one poll. The workflow does three
	// events.list calls inside Pipedream's custom-response deadline, so this is
	// generous; the caller's context still wins.
	pipedreamRequestTimeout = 120 * time.Second
)

// PipedreamCalendarRequest is the poll envelope. It carries NO switchboard
// data beyond the in-scope account emails and the shared horizon window.
type PipedreamCalendarRequest struct {
	SchemaVersion int      `json:"schema_version"`
	TimeMin       string   `json:"time_min"`
	TimeMax       string   `json:"time_max"`
	Calendars     []string `json:"calendars"`
}

// PipedreamCalendarEntry is one calendar's slice of the response.
type PipedreamCalendarEntry struct {
	CalendarID string `json:"calendar_id"`
	Status     string `json:"status"`
	// EventCount is a POINTER on purpose: an ABSENT field decoding to zero
	// would silently skip the count check, and with zero events the entry
	// would pass as a verified empty snapshot — a fresh ok run for a calendar
	// we never actually read (the bridge's absent-watermark rule).
	EventCount *int              `json:"event_count"`
	Events     []json.RawMessage `json:"events"`
	Error      string            `json:"error"`
}

// PipedreamCalendarResponse is the workflow's answer. TimeMin/TimeMax are the
// ECHO the ingest verifies against the request: a workflow that quietly
// answers a narrower window than asked would otherwise turn the snapshot
// replacement into data loss.
type PipedreamCalendarResponse struct {
	SchemaVersion int                      `json:"schema_version"`
	TimeMin       string                   `json:"time_min"`
	TimeMax       string                   `json:"time_max"`
	Calendars     []PipedreamCalendarEntry `json:"calendars"`
}

// PipedreamCalendarSource is the seam RunPipedreamCalendar consumes, so the
// ingest half is testable with a fake and no HTTP at all.
type PipedreamCalendarSource interface {
	FetchCalendars(ctx context.Context, req PipedreamCalendarRequest) (PipedreamCalendarResponse, error)
}

// PipedreamCalendarClient implements PipedreamCalendarSource over HTTP.
// maxBytes is a field rather than the constant read inline so the overflow
// boundary is testable without allocating 16 MiB (slackweb's seam, verbatim).
type PipedreamCalendarClient struct {
	endpoint string
	token    string
	client   *http.Client
	maxBytes int64
}

// NewPipedreamCalendarClient validates the endpoint and credential up front so
// a misconfigured deployment fails at startup rather than mid-ingest — before
// any sync_runs row exists (criterion 3's line).
func NewPipedreamCalendarClient(rawURL, token string, hc *http.Client) (*PipedreamCalendarClient, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("PIPEDREAM_CALENDAR_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("PIPEDREAM_CALENDAR_URL does not parse")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("PIPEDREAM_CALENDAR_URL must be http or https")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("PIPEDREAM_CALENDAR_URL must include a host")
	}
	if len(token) < 32 {
		return nil, fmt.Errorf("Pipedream calendar token must be at least 32 characters")
	}
	if hc == nil {
		hc = &http.Client{Timeout: pipedreamRequestTimeout}
	}
	return &PipedreamCalendarClient{
		endpoint: strings.TrimRight(rawURL, "/"),
		token:    token,
		client:   hc,
		maxBytes: PipedreamMaxResponseBytes,
	}, nil
}

// PipedreamTokenFromEnv reads the workflow credential, preferring a FILE so
// the secret can be mounted rather than kept in the process environment (the
// recorded --from-literal landmine; slackweb's TokenFromEnv, verbatim).
func PipedreamTokenFromEnv() (string, error) {
	if path := os.Getenv("PIPEDREAM_CALENDAR_TOKEN_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read Pipedream calendar token file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return strings.TrimSpace(os.Getenv("PIPEDREAM_CALENDAR_TOKEN")), nil
}

// FetchCalendars performs the one POST of a pass and decodes the envelope.
// Every failure is a refusal for the whole poll; the ingest layer turns it
// into per-account error runs.
func (c *PipedreamCalendarClient) FetchCalendars(ctx context.Context, req PipedreamCalendarRequest) (PipedreamCalendarResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return PipedreamCalendarResponse{}, fmt.Errorf("marshal pipedream calendar request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		// The error from NewRequest can embed the URL; the constructor already
		// validated it, so this is unreachable in practice — classify anyway.
		return PipedreamCalendarResponse{}, fmt.Errorf("build pipedream calendar request")
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		// CLASSIFIED, never %w-wrapped: *url.Error embeds the whole endpoint
		// URL and the net errors under it embed the host — either would leak
		// half the secret into every CronJob log.
		reason := "transport failure"
		if ctx.Err() != nil {
			reason = "cancelled or timed out"
		}
		return PipedreamCalendarResponse{}, fmt.Errorf(
			"pipedream calendar workflow unreachable (%s; endpoint withheld — check PIPEDREAM_CALENDAR_URL and the network)", reason)
	}
	defer resp.Body.Close()

	// One byte past the cap, so hitting it is distinguishable from a body that
	// merely ends there: a complete document padded to exactly the cap, with
	// more bytes behind it, is the one truncation that stays valid JSON — and
	// a snapshot is a REPLACEMENT, so a truncated one that still parses would
	// supersede every event it lost.
	out, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return PipedreamCalendarResponse{}, fmt.Errorf("read pipedream calendar response: body read failed")
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return PipedreamCalendarResponse{}, fmt.Errorf(
			"pipedream calendar workflow: HTTP %d: %s", resp.StatusCode, snippet)
	}
	if int64(len(out)) >= c.maxBytes {
		return PipedreamCalendarResponse{}, fmt.Errorf(
			"pipedream calendar response at or over %d bytes; refusing a possibly truncated snapshot", c.maxBytes)
	}
	var decoded PipedreamCalendarResponse
	if err := json.Unmarshal(out, &decoded); err != nil {
		return PipedreamCalendarResponse{}, fmt.Errorf("parse pipedream calendar response: not the envelope")
	}
	return decoded, nil
}
