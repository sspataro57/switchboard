package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI is the chat.completions adapter with native strict structured
// outputs. net/http, no SDK — one endpoint, and this package is the isolation
// boundary either way. No retries: the worker's error bookkeeping plus the
// next cron run is the retry.
type OpenAI struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// Probe answers "can you serve right now" without running a completion
// (SWT-21). It exists because the boundary refuses a local client that cannot
// demonstrate reachability — a declaration is not evidence.
//
// GET {base}/models is the cheapest thing an OpenAI-compatible server answers,
// and ollama and llama.cpp both serve it. Any non-2xx or transport failure is
// ErrUnavailable rather than a wrapped error: to the router this is a state, not
// a fault.
func (o *OpenAI) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return fmt.Errorf("%w: build probe: %v", ErrUnavailable, err)
	}
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: probe returned %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}

// Describe reports the endpoint this adapter will POST to, which is what
// LocalityOf classifies (SWT-21). It returns the CONFIGURED base URL rather than
// a constant, because that is the whole point: this same type serves the local
// lane when pointed at ollama or llama.cpp, and the hosted lane when pointed at
// api.openai.com. Repointing it is exactly the change the boundary must catch.
func (o *OpenAI) Describe() Descriptor {
	return Descriptor{Name: "openai", Endpoint: o.baseURL}
}

// NewOpenAI builds the adapter. baseURL defaults to the public API when empty.
func NewOpenAI(apiKey, baseURL string) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaRequest struct {
	Model               string      `json:"model"`
	Messages            []oaMessage `json:"messages"`
	MaxCompletionTokens int         `json:"max_completion_tokens,omitempty"`
	ResponseFormat      struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	body := oaRequest{
		Model:               req.Model,
		Messages:            []oaMessage{{Role: "system", Content: req.System}, {Role: "user", Content: req.User}},
		MaxCompletionTokens: req.MaxTokens,
	}
	body.ResponseFormat.Type = "json_schema"
	body.ResponseFormat.JSONSchema.Name = req.SchemaName
	body.ResponseFormat.JSONSchema.Strict = true
	body.ResponseFormat.JSONSchema.Schema = req.Schema

	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("build openai request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	httpResp, err := o.http.Do(httpReq)
	if err != nil {
		// A TRANSPORT failure is unavailability, not a broken adapter (SWT-21
		// criterion 8). Wrapping it in ErrUnavailable is what lets the caller
		// SKIP the message instead of counting it toward the consecutive-error
		// abort — and that distinction is the difference between "the local box
		// is busy" and "this adapter is broken". A 4B model at low priority on a
		// machine kept usable as a desktop makes refused connections and blown
		// deadlines routine, so treating them as faults would exit the run
		// non-zero on a normal Tuesday.
		// TWO %w verbs: the sentinel for classification, the cause for diagnosis.
		// %v on the cause would let a caller ask "is this unavailability?" but
		// leave a human unable to ask "why" — and a boundary that swallows the
		// reason is how a busy box and a wrong URL become the same log line.
		return Response{}, fmt.Errorf("openai request: %w: %w", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()
	latency := int(time.Since(start).Milliseconds())
	if latency == 0 {
		latency = 1
	}

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read openai response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("openai HTTP %d: %.300s", httpResp.StatusCode, respBody)
	}

	var parsed oaResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Response{}, fmt.Errorf("parse openai response: %w (body head: %.200s)", err, respBody)
	}
	if parsed.Error != nil {
		return Response{}, fmt.Errorf("openai API error (%s): %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("openai response has no choices (body head: %.200s)", respBody)
	}
	choice := parsed.Choices[0]
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		return Response{}, fmt.Errorf("openai model refused: %s", *choice.Message.Refusal)
	}
	if choice.Message.Content == nil || *choice.Message.Content == "" {
		return Response{}, fmt.Errorf("openai response has empty content (finish_reason %s)", choice.FinishReason)
	}

	return Response{
		Raw:              json.RawMessage(*choice.Message.Content),
		Model:            parsed.Model,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		LatencyMS:        latency,
	}, nil
}
