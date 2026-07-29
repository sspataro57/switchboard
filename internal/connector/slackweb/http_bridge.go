package slackweb

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
)

// HTTPBridge talks to the connector's bridge server instead of spawning it.
//
// The authenticated Slack browser is irreducibly host-bound to the Mac mini,
// while Switchboard runs in the cluster, so there is no local process to exec.
// The mini exposes export/draft over HTTP and this is the client for it. There
// is deliberately no send route on either side: sending stays a human action in
// Slack, recorded afterwards via mark_delivery_sent.
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

func (b *HTTPBridge) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Slack bridge request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+b.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := b.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Slack bridge %s: %w", path, err)
	}
	defer response.Body.Close()

	// One byte past the cap, so hitting it is distinguishable from a body that
	// merely ends there. The command bridge reports overflow explicitly; without
	// this, a truncated response that happens to remain valid JSON — a complete
	// document padded to exactly the cap, with more bytes behind it — would be
	// ingested as a complete export.
	out, err := io.ReadAll(io.LimitReader(response.Body, b.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Slack bridge %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		// The body carries the connector's error string; keep it short so a
		// failure cannot dump Slack content into logs.
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("Slack bridge %s returned %d: %s", path, response.StatusCode, snippet)
	}
	if int64(len(out)) > b.maxBytes {
		return nil, fmt.Errorf("Slack bridge %s output exceeded %d bytes", path, b.maxBytes)
	}
	return out, nil
}

func (b *HTTPBridge) Export(ctx context.Context) (Export, error) {
	out, err := b.post(ctx, "/export", nil)
	if err != nil {
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
	out, err := b.post(ctx, "/draft", in)
	if err != nil {
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
