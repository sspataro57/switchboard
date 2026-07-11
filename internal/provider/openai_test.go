package provider_test

// Unit tests for the OpenAI provider adapter (SPEC 06-gpt-triage, acceptance
// criterion 1 + "Provider adapter" section). Everything runs against an
// httptest.Server — ZERO real network, ZERO live LLM. The adapter is the sole
// isolation boundary (criterion 11): these tests pin the wire contract
// (strict json_schema response_format, model from the request, auth header)
// and the response parsing (content JSON, usage tokens, latency measured,
// refusal/non-2xx → wrapped errors).
//
// GREENFIELD NOTE: package internal/provider does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented — the expected
// failure mode. Imposed exported surface exercised here (the SPEC's
// provider.go + openai.go); for greenfield code the SPEC's contract IS the
// signature:
//
//   type Request struct {
//       Model      string
//       System     string
//       User       string
//       SchemaName string          // json_schema name, e.g. "triage_extraction"
//       Schema     json.RawMessage // strict JSON Schema (the caller's schema)
//       MaxTokens  int
//   }
//   type Response struct {
//       Raw              json.RawMessage // message content — schema-shaped JSON
//       Model            string          // model as reported by the API
//       PromptTokens     int
//       CompletionTokens int
//       LatencyMS        int
//   }
//   type Client interface {
//       Complete(ctx context.Context, req Request) (Response, error)
//   }
//   // OpenAI net/http implementation, no SDK. apiKey + base url injected so the
//   // test can point base at httptest; *OpenAI implements Client.
//   func NewOpenAI(apiKey, baseURL string) *OpenAI

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

const testAPIKey = "sk-test-abc123"

// triageSchema is a stand-in for the caller's strict schema — the adapter must
// forward it verbatim, it does not own or synthesize it.
var triageSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["actionable"],"properties":{"actionable":{"type":"object","additionalProperties":false,"required":["value","confidence"],"properties":{"value":{"type":"boolean"},"confidence":{"type":"number"}}}}}`)

// canned OpenAI chat.completions envelope: the assistant message content is the
// schema-shaped JSON string the worker cares about.
func cannedCompletion(content string) string {
	env := map[string]any{
		"id":    "chatcmpl-xyz",
		"model": "gpt-5-mini-2025-01-01",
		"choices": []any{
			map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": content},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     123,
			"completion_tokens": 45,
			"total_tokens":      168,
		},
	}
	b, _ := json.Marshal(env)
	return string(b)
}

func newRequest() provider.Request {
	return provider.Request{
		Model:      "gpt-5-mini",
		System:     "you are a triage assistant",
		User:       "please classify this message",
		SchemaName: "triage_extraction",
		Schema:     triageSchema,
		MaxTokens:  512,
	}
}

// TestOpenAI_RequestShape pins the outbound wire contract: endpoint, auth,
// model, messages, and the strict json_schema response_format carrying the
// caller's schema verbatim.
func TestOpenAI_RequestShape(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, cannedCompletion(`{"actionable":{"value":true,"confidence":0.9}}`))
	}))
	defer srv.Close()

	var c provider.Client = provider.NewOpenAI(testAPIKey, srv.URL)
	if _, err := c.Complete(context.Background(), newRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Errorf("request path = %q, want suffix /chat/completions", gotPath)
	}
	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q, want Bearer %s", gotAuth, testAPIKey)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["model"] != "gpt-5-mini" {
		t.Errorf("body.model = %v, want gpt-5-mini (model comes from the request, never a constant)", gotBody["model"])
	}

	// messages: [{system}, {user}] in order.
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("body.messages = %v, want 2 messages (system, user)", gotBody["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	usr, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "you are a triage assistant" {
		t.Errorf("messages[0] = %v, want system/you are a triage assistant", sys)
	}
	if usr["role"] != "user" || usr["content"] != "please classify this message" {
		t.Errorf("messages[1] = %v, want user/please classify this message", usr)
	}

	// response_format: strict json_schema with the caller's schema verbatim.
	rf, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("body.response_format missing/not object: %v", gotBody["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("response_format.json_schema missing: %v", rf)
	}
	if js["name"] != "triage_extraction" {
		t.Errorf("json_schema.name = %v, want triage_extraction", js["name"])
	}
	if js["strict"] != true {
		t.Errorf("json_schema.strict = %v, want true (native strict structured output)", js["strict"])
	}
	// The schema is forwarded verbatim (deep-equal against the decoded caller schema).
	var wantSchema any
	_ = json.Unmarshal(triageSchema, &wantSchema)
	if !reflect.DeepEqual(js["schema"], wantSchema) {
		t.Errorf("json_schema.schema not forwarded verbatim:\n got=%v\nwant=%v", js["schema"], wantSchema)
	}

	// MaxTokens is forwarded under whichever field name the adapter uses
	// (max_tokens or max_completion_tokens — newer models require the latter).
	if !maxTokensPresent(gotBody, 512) {
		t.Errorf("MaxTokens=512 not present under max_tokens or max_completion_tokens; body=%v", gotBody)
	}
}

func maxTokensPresent(body map[string]any, want float64) bool {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := body[k]; ok {
			if n, ok := v.(float64); ok && n == want {
				return true
			}
		}
	}
	return false
}

// TestOpenAI_ResponseParsing pins content extraction, usage tokens, model
// echo, and that latency is actually measured.
func TestOpenAI_ResponseParsing(t *testing.T) {
	content := `{"actionable":{"value":true,"confidence":0.8},"kind":{"value":"question","confidence":0.7}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Millisecond) // make LatencyMS observably non-zero
		io.WriteString(w, cannedCompletion(content))
	}))
	defer srv.Close()

	c := provider.NewOpenAI(testAPIKey, srv.URL)
	resp, err := c.Complete(context.Background(), newRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Raw is the assistant message content — the schema-shaped JSON, nothing else.
	var got, want any
	if err := json.Unmarshal(resp.Raw, &got); err != nil {
		t.Fatalf("resp.Raw is not valid JSON: %v (%s)", err, resp.Raw)
	}
	_ = json.Unmarshal([]byte(content), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resp.Raw = %s, want the message content %s", resp.Raw, content)
	}
	if resp.Model != "gpt-5-mini-2025-01-01" {
		t.Errorf("resp.Model = %q, want the API-reported model", resp.Model)
	}
	if resp.PromptTokens != 123 {
		t.Errorf("resp.PromptTokens = %d, want 123", resp.PromptTokens)
	}
	if resp.CompletionTokens != 45 {
		t.Errorf("resp.CompletionTokens = %d, want 45", resp.CompletionTokens)
	}
	if resp.LatencyMS <= 0 {
		t.Errorf("resp.LatencyMS = %d, want > 0 (latency must be measured)", resp.LatencyMS)
	}
}

// TestOpenAI_Non200IsWrappedError: a 429/5xx with an API error body surfaces as
// a wrapped error (not retried in the adapter — the worker's bookkeeping is the
// retry). The error text should carry a body excerpt.
func TestOpenAI_Non200IsWrappedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited, slow down","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	c := provider.NewOpenAI(testAPIKey, srv.URL)
	_, err := c.Complete(context.Background(), newRequest())
	if err == nil {
		t.Fatalf("Complete: expected an error on HTTP 429")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error %q does not carry the API error body excerpt", err.Error())
	}
}

// TestOpenAI_MalformedJSONIsError: a 200 whose body is not a valid completion
// envelope is an error, not a silent empty Response.
func TestOpenAI_MalformedJSONIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{not valid json`)
	}))
	defer srv.Close()

	c := provider.NewOpenAI(testAPIKey, srv.URL)
	if _, err := c.Complete(context.Background(), newRequest()); err == nil {
		t.Fatalf("Complete: expected an error on malformed response body")
	}
}

// TestOpenAI_RefusalIsWrappedError: choices[0].message.refusal set (content
// null) surfaces as a wrapped error — the worker records it as a failed run,
// it does not parse a refusal string as an extraction (SPEC provider section).
func TestOpenAI_RefusalIsWrappedError(t *testing.T) {
	env := map[string]any{
		"model": "gpt-5-mini-2025-01-01",
		"choices": []any{
			map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": nil, "refusal": "I cannot help with that."},
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
	body, _ := json.Marshal(env)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := provider.NewOpenAI(testAPIKey, srv.URL)
	if _, err := c.Complete(context.Background(), newRequest()); err == nil {
		t.Fatalf("Complete: expected an error when the model refuses")
	}
}
