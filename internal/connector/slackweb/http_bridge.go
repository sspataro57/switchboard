package slackweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// HTTPBridge talks to the connector's bridge server instead of spawning it.
//
// The authenticated Slack browser is irreducibly host-bound to the Mac mini,
// while Switchboard runs in the cluster, so there is no local process to exec.
// The mini exposes export, draft, and send over HTTP and this is the client.
//
// SWT-12 deliberately reversed the guarantee this comment used to make — that no
// send route exists on either side. The assisted tier required remote-desktopping
// into the mini to press Send, which made it unusable, so switchboard now clicks
// Send itself after approve_delivery. What did NOT change: Send is its own method
// and route. Draft still refuses any result claiming sent:true, and that guard is
// not a thing this file relaxes.
type HTTPBridge struct {
	baseURL string
	token   string
	client  *http.Client
	// maxBytes caps a bridge response. Held per-bridge rather than read from the
	// package constant so the overflow boundary is testable without allocating
	// the real 64 MiB.
	maxBytes int64
}

// NewHTTPBridge validates the endpoint and credential up front so a
// misconfigured deployment fails at startup rather than mid-ingest.
func NewHTTPBridge(rawURL, token string, client *http.Client) (*HTTPBridge, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("SLACK_WEB_BRIDGE_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse SLACK_WEB_BRIDGE_URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("SLACK_WEB_BRIDGE_URL must be http or https")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("SLACK_WEB_BRIDGE_URL must include a host")
	}
	if len(token) < 32 {
		return nil, fmt.Errorf("Slack bridge token must be at least 32 characters")
	}
	if client == nil {
		// No client timeout: an export drives a real browser and takes minutes.
		// The caller's context is the deadline that matters.
		client = &http.Client{}
	}
	return &HTTPBridge{
		baseURL:  strings.TrimRight(rawURL, "/"),
		token:    token,
		client:   client,
		maxBytes: maxBridgeOutputBytes,
	}, nil
}

// TokenFromEnv reads the bridge credential, preferring a file so the secret can
// be mounted rather than kept in the process environment.
func TokenFromEnv() (string, error) {
	if path := os.Getenv("SLACK_WEB_BRIDGE_TOKEN_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read Slack bridge token file: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return strings.TrimSpace(os.Getenv("SLACK_WEB_BRIDGE_TOKEN")), nil
}

// post returns the body AND the status: 200 and 202 are both success (SWT-76
// D10 — a 202 is the leaf ACCEPTING a send, and only Send may interpret it;
// Export and Draft refuse it through requireOK). Everything else is a
// bridgeStatusError carrying the status.
func (b *HTTPBridge) post(ctx context.Context, path string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build Slack bridge request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+b.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := b.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("call Slack bridge %s: %w", path, err)
	}
	defer response.Body.Close()

	// One byte past the cap, so hitting it is distinguishable from a body that
	// merely ends there. The command bridge reports overflow explicitly; without
	// this, a truncated response that happens to remain valid JSON — a complete
	// document padded to exactly the cap, with more bytes behind it — would be
	// ingested as a complete export.
	out, err := io.ReadAll(io.LimitReader(response.Body, b.maxBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read Slack bridge %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		// The body carries the connector's error string; keep it short so a
		// failure cannot dump Slack content into logs.
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, response.StatusCode, &bridgeStatusError{status: response.StatusCode, path: path, snippet: snippet,
			retryAfter: retryAfterHeader(response.Header.Get("Retry-After"))}
	}
	if int64(len(out)) > b.maxBytes {
		return nil, response.StatusCode, fmt.Errorf("Slack bridge %s output exceeded %d bytes", path, b.maxBytes)
	}
	return out, response.StatusCode, nil
}

// requireOK is the refusal every path but Send applies to a 202: a queued
// export has no result and a queued draft was never typed, so letting either
// through would ingest an empty export or report words that are not in the
// composer (SWT-76 criterion 10).
func requireOK(path string, out []byte, status int) error {
	if status != http.StatusAccepted {
		return nil
	}
	var body struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(out, &body)
	snippet := strings.TrimSpace(string(out))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	return &BridgeQueuedError{Path: path, JobID: body.JobID, Body: snippet}
}

func (b *HTTPBridge) Export(ctx context.Context, req ExportRequest) (Export, error) {
	// Zero-value fields are omitted on the wire: the leaf answers 0, "" or null
	// with a 500 that stops the export for every workspace (SWT-39).
	body, err := json.Marshal(req)
	if err != nil {
		return Export{}, fmt.Errorf("marshal Slack export request: %w", err)
	}
	out, status, err := b.post(ctx, "/export", body)
	if err != nil {
		// SWT-75 criterion 14: the leaf's 503 is its browser queue refusing a
		// second sweep (job-queue.ts:144-152), thrown BEFORE any browser work.
		// It is a typed busy signal with the leaf's own Retry-After, so the
		// watch loop sleeps for as long as the queue asked and skips — never
		// a failure, never a guess.
		var status *bridgeStatusError
		if errors.As(err, &status) && status.status == http.StatusServiceUnavailable {
			return Export{}, &BridgeBusyError{RetryAfter: status.retryAfter, Body: status.snippet}
		}
		return Export{}, err
	}
	if err := requireOK("/export", out, status); err != nil {
		return Export{}, err
	}
	var exported Export
	if err := json.Unmarshal(out, &exported); err != nil {
		return Export{}, fmt.Errorf("parse Slack bridge export: %w", err)
	}
	if exported.SchemaVersion != SchemaVersion {
		return Export{}, fmt.Errorf("unsupported Slack bridge schema_version %d", exported.SchemaVersion)
	}
	return exported, nil
}

func (b *HTTPBridge) Draft(ctx context.Context, targetURL, text string) error {
	if targetURL == "" || text == "" {
		return fmt.Errorf("Slack draft requires target URL and text")
	}
	in, err := json.Marshal(map[string]string{"target_url": targetURL, "text": text})
	if err != nil {
		return fmt.Errorf("marshal Slack draft request: %w", err)
	}
	out, status, err := b.post(ctx, "/draft", in)
	if err != nil {
		return err
	}
	if err := requireOK("/draft", out, status); err != nil {
		return err
	}
	var result struct {
		Drafted bool `json:"drafted"`
		Sent    bool `json:"sent"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("parse Slack draft result: %w", err)
	}
	// Same invariant the command bridge enforces: a result claiming a send is
	// rejected outright rather than trusted.
	if !result.Drafted || result.Sent {
		return fmt.Errorf("Slack bridge returned an unsafe draft result")
	}
	return nil
}

// SendRejectedError is a DEFINITE bridge refusal: the send did not happen, so
// the failed->approved retry path is safe to reopen.
//
// Only failures that provably preceded the click carry this type — a 4xx (no
// token, writes disabled, unattended disabled, validation) or a leaf answering
// sent:false. A 5xx, a transport error, or a context timeout stays UNTYPED,
// because the click may have landed after the failure and invariant 4 errs
// toward never-resend.
type SendRejectedError struct {
	Status int
	Body   string
}

func (e *SendRejectedError) Error() string {
	if e.Status == 0 {
		return "Slack send rejected: " + e.Body
	}
	return fmt.Sprintf("Slack send rejected (%d): %s", e.Status, e.Body)
}

// SendOutcome distinguishes a click that HAPPENED from a leaf ACCEPTANCE
// (SWT-76 D1). Queued=false is a completed send (the leaf answered sent:true);
// Queued=true means the click is still pending on the leaf's browser queue and
// the delivery row must stay in 'sending' with its attempt unsettled. Callers
// branch on Queued, never on a zero value.
type SendOutcome struct {
	Queued    bool
	JobID     string
	QueuedAt  time.Time
	ExpiresIn time.Duration
}

// ErrBridgeQueued is the sentinel every *BridgeQueuedError matches: the leaf
// answered 202 on a path whose RESULT the caller needs (Export, Draft). Only
// Send interprets a 202 (SWT-76 criterion 10).
var ErrBridgeQueued = errors.New("Slack bridge queued the request")

// BridgeQueuedError is a 202 on a path that cannot accept one.
type BridgeQueuedError struct {
	Path, JobID, Body string
}

func (e *BridgeQueuedError) Error() string {
	return fmt.Sprintf("Slack bridge %s answered 202 (job %q); this path needs a result, not an acceptance: %s",
		e.Path, e.JobID, e.Body)
}

func (e *BridgeQueuedError) Is(target error) bool { return target == ErrBridgeQueued }

// ErrBridgeBusy is the sentinel every *BridgeBusyError matches: the bridge's
// queue refused the export before any browser work (SWT-75 D3/criterion 14).
var ErrBridgeBusy = errors.New("Slack bridge busy")

// BridgeBusyError is /export's 503 with the leaf's Retry-After (zero when the
// header was absent or unparseable).
type BridgeBusyError struct {
	RetryAfter time.Duration
	Body       string
}

func (e *BridgeBusyError) Error() string {
	return fmt.Sprintf("Slack bridge busy (retry after %s): %s", e.RetryAfter, e.Body)
}

func (e *BridgeBusyError) Is(target error) bool { return target == ErrBridgeBusy }

// retryAfterHeader parses a delay-seconds Retry-After; anything else is zero.
func retryAfterHeader(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// bridgeStatusError carries the HTTP status out of post so Send can tell a
// definite refusal from an ambiguous one. Its message is unchanged from the
// untyped error it replaced, so Export/Draft callers see the same text.
type bridgeStatusError struct {
	status     int
	path       string
	snippet    string
	retryAfter time.Duration
}

func (e *bridgeStatusError) Error() string {
	return fmt.Sprintf("Slack bridge %s returned %d: %s", e.path, e.status, e.snippet)
}

// Send clicks Send in the connector's browser. A completed click returns a
// zero SendOutcome and nil: a browser click reserves no message id, so the
// delivery's sent_external_id stays NULL and the next export stamps it by
// body prefix.
//
// SWT-76: maxQueue > 0 puts max_queue_ms in the request, which is the caller's
// declaration that it understands the queue and PERMITS the leaf to answer 202
// — accepted, not yet clicked — when the browser is busy (D1/D12). maxQueue 0
// omits the key and the leaf behaves exactly as before (wait or 503): that is
// the no-roll rollback. The caller derives maxQueue from its own lease
// (tools.sendSlackReply), because the caller owns the window that protects
// the row.
func (b *HTTPBridge) Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (SendOutcome, error) {
	if targetURL == "" || text == "" {
		return SendOutcome{}, &SendRejectedError{Body: "Slack send requires target URL and text"}
	}
	req := map[string]any{"target_url": targetURL, "text": text}
	if maxQueue > 0 {
		req["max_queue_ms"] = maxQueue.Milliseconds()
	}
	in, err := json.Marshal(req)
	if err != nil {
		return SendOutcome{}, &SendRejectedError{Body: fmt.Sprintf("marshal Slack send request: %v", err)}
	}
	// A deadline already blown before dispatch means nothing was sent.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return SendOutcome{}, &SendRejectedError{Body: "context already done before dispatch: " + ctxErr.Error()}
	}
	out, status, err := b.post(ctx, "/send", in)
	if err != nil {
		// SWT-75 D7: 503 (and 429) are DEFINITE too. The leaf's 503 is
		// QueueFullError, thrown inside JobQueue.run before the job function is
		// called (job-queue.ts:144-152) — provably pre-click, like the 4xx cases.
		// A 500 stays ambiguous: it can come from browser work that already
		// pressed Send.
		var refused *bridgeStatusError
		if errors.As(err, &refused) && ((refused.status >= 400 && refused.status < 500) ||
			refused.status == http.StatusServiceUnavailable) {
			return SendOutcome{}, &SendRejectedError{Status: refused.status, Body: refused.snippet}
		}
		// A failure to establish the connection at all is provably pre-click: no
		// TCP session means the request never reached the bridge, so the browser
		// never clicked. Anything after dial — a read failure, an EOF mid-response,
		// a timeout — stays ambiguous, because by then the click may have landed.
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return SendOutcome{}, &SendRejectedError{Body: "bridge unreachable (never dispatched): " + err.Error()}
		}
		return SendOutcome{}, err
	}
	if status == http.StatusAccepted {
		// The leaf ACCEPTED the send and will click it in the first gap. Not
		// checkSendResult: a 202 body carries no sent:true, and reading it as
		// sent:false would turn an acceptance into a definite refusal — the
		// exact defect this ticket removes (criterion 11). A body without a
		// job_id is a leaf defect to log, never a reason to treat the send as
		// refused: the row's protection is the unsettled attempt plus the
		// lease, not the id (criterion 12).
		return parseQueued(out), nil
	}
	return SendOutcome{}, checkSendResult(out)
}

// parseQueued reads the 202 body: {queued, job_id, queued_at, estimated_wait_ms,
// expires_in_ms}. Every field is optional; the status alone means queued.
func parseQueued(out []byte) SendOutcome {
	var body struct {
		JobID       string `json:"job_id"`
		QueuedAt    string `json:"queued_at"`
		ExpiresInMS int64  `json:"expires_in_ms"`
	}
	_ = json.Unmarshal(out, &body)
	if len(body.JobID) > 200 { // the same cap post applies to error snippets: leaf text, never unbounded
		body.JobID = body.JobID[:200]
	}
	outcome := SendOutcome{Queued: true, JobID: body.JobID, ExpiresIn: time.Duration(body.ExpiresInMS) * time.Millisecond}
	if t, err := time.Parse(time.RFC3339Nano, body.QueuedAt); err == nil {
		outcome.QueuedAt = t
	}
	if body.JobID == "" {
		slog.Warn("slack bridge answered 202 without a job_id; the send is queued but cannot be found in the leaf's log")
	}
	return outcome
}

// checkSendResult enforces that only an exact {drafted:false, sent:true} counts
// as a send. A "drafted" answer on the send path means the composer was filled
// and the click never happened; recording that as sent would leave a delivery in
// 'sent' with no message in Slack.
func checkSendResult(out []byte) error {
	var result struct {
		Drafted bool `json:"drafted"`
		Sent    bool `json:"sent"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		// Unintelligible: the leaf ran and may have clicked. Stay ambiguous.
		return fmt.Errorf("parse Slack send result: %w", err)
	}
	if !result.Sent {
		// The leaf states it did not send. Definite, so the row can retry.
		return &SendRejectedError{Body: "Slack bridge reported the message was not sent"}
	}
	if result.Drafted {
		// Claims both drafted and sent. Something happened; do not risk it.
		return fmt.Errorf("Slack bridge returned an unsafe send result: drafted and sent")
	}
	return nil
}
