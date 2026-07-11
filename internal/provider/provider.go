// Package provider is the LLM provider adapter boundary (SPEC 06-gpt-triage):
// prompt + JSON schema in, structured result out. Provider details (endpoints,
// auth, request shapes, model ids) live ONLY here — a vendor import outside
// this package is a review flag. Adapters record nothing; workers own ai_runs.
package provider

import (
	"context"
	"encoding/json"
)

// Request is one structured completion call.
type Request struct {
	Model      string
	System     string
	User       string
	SchemaName string          // json_schema name, e.g. "triage_extraction"
	Schema     json.RawMessage // strict JSON Schema
	MaxTokens  int
}

// Response is the provider-neutral result.
type Response struct {
	Raw              json.RawMessage // the message content — schema-shaped JSON
	Model            string          // as reported by the API
	PromptTokens     int
	CompletionTokens int
	LatencyMS        int
}

// Client is the worker-facing contract. Implementations must be safe for
// sequential reuse; tests use fakes, never live providers.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
