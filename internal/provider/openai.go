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
		return Response{}, fmt.Errorf("openai request: %w", err)
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
