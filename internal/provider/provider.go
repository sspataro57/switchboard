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
	// Think is the 2026-09-07 A/B experiment knob: DEFAULT FALSE, and the
	// zero value produces wire bytes identical to before the field existed
	// (ollama's `think` key is not omitempty, so false is still sent
	// explicitly). Enabling it without a raised MaxTokens reproduces the
	// measured 0.00-score regression — see ollama.go's thinking note. Only
	// the ollama adapter reads it; hosted adapters ignore it.
	Think bool
	// NumCtx overrides ollama's context window (0 = server default). Only
	// meaningful with Think, whose ~1k reasoning tokens can push a long email
	// past the default 4096 and silently truncate the prompt.
	NumCtx int
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
//
// Describe() is on the INTERFACE rather than in a registry or a config field,
// deliberately (SWT-21). It is the answer to "what cannot be forgotten": a new
// adapter that does not declare where it sends does not compile. A registry can
// be missed, a config flag can be wrong, and a locality inferred from the
// adapter's type would be a lie in both directions — llama.cpp and ollama serve
// an OpenAI-compatible /v1 route, so the same adapter type serves both the local
// and the hosted lane with nothing but a different base URL.
//
// The zero Descriptor classifies as not-local (locality.go), so forgetting to
// populate it fails closed rather than silently permitting restricted content.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
	Describe() Descriptor
}
