package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Ollama is the adapter for a local ollama server (SWT-22).
//
// IT SPEAKS THE NATIVE /api/chat API, NOT THE OpenAI-COMPATIBLE /v1 ROUTE, and
// that is evidence rather than preference. The spike that chose qwen3:8b
// measured its failure signature as `done_reason: length` with empty content —
// `done_reason` is a NATIVE response field (/v1 returns `finish_reason`), so the
// measurement was taken here. And `think` (see below) is a native REQUEST field
// that /v1 has nowhere to carry: pointing NewOpenAI at :11434 would silently
// send the 0.00-scoring configuration.
//
// Why a second adapter at all, when OpenAI already serves a local lane: the two
// differ on the three fields that decide whether the output is usable —
// `think`, `format` (a bare schema object, not /v1's json_schema envelope), and
// `keep_alive`. None of them exist on /v1.
type Ollama struct {
	baseURL string
	model   string
	http    *http.Client

	keepAlive string
}

// OllamaOption is the variadic the SPEC declares. No option constructor is named
// yet, so none is invented here — a knob nobody has asked for is a knob nobody
// can be wrong about.
type OllamaOption func(*Ollama)

// defaultKeepAlive keeps a loaded model resident between messages.
//
// Measured on this box: ollama DISCHARGES a model after 5 minutes by default
// (/api/tags lists what is on disk, /api/ps what is resident), and reloading
// qwen3:8b costs 3.40s against 0.25-0.38s for a warm call. A batch pass over
// ~1,609 messages that reloaded per message would take hours instead of minutes.
//
// 30m, not -1: pinning is the wrong default for a lane that may share a card
// with a desktop. It also would not do what it looks like — verified, a
// keep_alive of -1 sets expiry to the year 2318 and the model is STILL evicted
// when another one needs the VRAM. Ollama's own eviction is correct; do not
// write code that tries to help it.
const defaultKeepAlive = "30m"

// NewOllama builds the adapter.
//
// It trims a trailing "/" AND a trailing "/v1", and says so when it does. That
// is not tidiness: docs/runbooks/provider-locality.md USED to tell the operator
// to set OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434/v1, for the OpenAI
// adapter that served this lane before SWT-22. The runbook now says "no /v1" —
// but anyone who configured the box while it said otherwise still has the old
// value exported. Untrimmed it would POST to /v1/api/chat, which 404s — and a
// 404 is an HTTP error, not ErrUnavailable, so every message would be recorded
// as an unclassified_error and the pass would trip the ratio raise. A stale URL
// would read as a broken adapter instead of a misconfigured one.
func NewOllama(baseURL, model string, opts ...OllamaOption) *Ollama {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed := strings.TrimSuffix(base, "/v1"); trimmed != base {
		slog.Warn("ollama base URL ended in /v1; trimming it",
			"given", baseURL, "using", trimmed,
			"why", "the native API lives at /api/chat, and /v1/api/chat is a 404 that reads as a broken adapter")
		base = strings.TrimRight(trimmed, "/")
	}
	o := &Ollama{
		baseURL:   base,
		model:     model,
		keepAlive: defaultKeepAlive,
		// No retries, matching OpenAI: the worker's skip bookkeeping plus the
		// next pass IS the retry. The timeout is generous because a cold load is
		// 3.4s and a slow box is normal operation, not a fault.
		http: &http.Client{Timeout: 120 * time.Second},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Describe reports where this adapter POSTs, which is what LocalityOf
// classifies. It returns the CONFIGURED base rather than a constant for the same
// reason OpenAI does: locality is a property of the destination, not of the type.
func (o *Ollama) Describe() Descriptor {
	return Descriptor{Name: "ollama", Endpoint: o.baseURL}
}

// olTagsResponse is /api/tags: the models present ON DISK.
type olTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// Probe requires BOTH a 2xx AND the configured model to be present.
//
// "The server is up but the model is gone" is a live failure mode, not a
// hypothetical: the spike left six models (~25 GB) on the box and the plan is to
// `ollama rm` five of them. A probe that accepted the 2xx alone would hand every
// message to a server that answers 404 per message — ~1,609 unclassified errors
// and a ratio raise, reported as a broken adapter.
//
// Everything here is ErrUnavailable, including the absent model: to the router
// this is a STATE, not a fault. The message names the model so an operator is
// told what to `ollama pull` rather than left to guess.
func (o *Ollama) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("%w: build probe: %v", ErrUnavailable, err)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: /api/tags returned %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: read /api/tags: %v", ErrUnavailable, err)
	}
	var tags olTagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		return fmt.Errorf("%w: parse /api/tags: %v", ErrUnavailable, err)
	}
	for _, m := range tags.Models {
		// Exact match, and also the bare name: ollama reports "qwen3:8b" but an
		// operator may have configured "qwen3:8b" against a server that lists it
		// with an explicit ":latest". Equality first, so the common case cannot
		// be broken by the fallback.
		if m.Name == o.model || strings.TrimSuffix(m.Name, ":latest") == strings.TrimSuffix(o.model, ":latest") {
			return nil
		}
	}
	have := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		have = append(have, m.Name)
	}
	return fmt.Errorf("%w: model %q is not present on the server (has: %s); pull it with `ollama pull %s`",
		ErrUnavailable, o.model, strings.Join(have, ", "), o.model)
}

// olMessage is one entry of the native `messages` array.
type olMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// olOptions is ollama's per-request `options` object.
type olOptions struct {
	// NumPredict is where Request.MaxTokens goes on the native API. Sent as
	// max_tokens it is silently ignored and the model runs to its own default.
	NumPredict int `json:"num_predict,omitempty"`
	// Temperature is PINNED to 0 rather than left to ollama's 0.8. A classifier
	// that answers differently on two identical messages cannot be scored against
	// a labelled set, and the labelled set is the only thing this ticket permits
	// anyone to tune against.
	//
	// NOT omitempty: 0 is the value we mean, and omitempty would drop it and
	// restore the 0.8 default with a struct that reads as if it pinned it.
	Temperature float64 `json:"temperature"`
}

// olRequest is the native /api/chat request.
//
// NOTE the absence of omitempty on Think and Stream. That is the single most
// important detail in this file: `json:",omitempty"` on a bool DROPS false, so
// the field would vanish from the wire and ollama would fall back to its own
// default — thinking ON for a thinking model — while this struct still read as
// though it had disabled it.
type olRequest struct {
	Model     string          `json:"model"`
	Messages  []olMessage     `json:"messages"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   olOptions       `json:"options"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Think     bool            `json:"think"`
	Stream    bool            `json:"stream"`
}

// olResponse is the native /api/chat response. The field names are the evidence
// for criterion 2: message.content, done_reason, prompt_eval_count, eval_count,
// total_duration — none of which are /v1's spelling.
type olResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	TotalDuration   int64  `json:"total_duration"`
	LoadDuration    int64  `json:"load_duration"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

func (o *Ollama) Complete(ctx context.Context, req Request) (Response, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	body := olRequest{
		Model:    model,
		Messages: []olMessage{{Role: "system", Content: req.System}, {Role: "user", Content: req.User}},
		// The caller's schema, forwarded VERBATIM. On the native API `format` IS
		// the schema object — not /v1's {"type":"json_schema","json_schema":{…}}
		// envelope. The adapter does not own, rewrite or synthesize a schema;
		// internal/classify does.
		Format:    req.Schema,
		Options:   olOptions{NumPredict: req.MaxTokens, Temperature: 0},
		KeepAlive: o.keepAlive,
		Think:     false,
		Stream:    false,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal ollama request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("build ollama request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := o.http.Do(httpReq)
	if err != nil {
		// A TRANSPORT failure is unavailability, not a broken adapter — the local
		// box being off or busy is the ordinary state of a workstation-hosted
		// lane. Two %w verbs, exactly as OpenAI does it: the sentinel so a caller
		// can classify, the cause so a human can diagnose.
		return Response{}, fmt.Errorf("ollama request: %w: %w", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read ollama response: %w", err)
	}
	// A NON-2xx is NOT ErrUnavailable. The server answered; it is reachable and
	// wrong. This is the other half of the typing, and the half that makes the
	// first half mean anything — wrap these too and a permanently broken adapter
	// looks like a busy box forever, and the unclassified-error ratio can never
	// fire. 404 is the untrimmed-/v1 case specifically.
	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		return Response{}, fmt.Errorf("ollama HTTP %d: %.300s", httpResp.StatusCode, respBody)
	}

	var parsed olResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Response{}, fmt.Errorf("parse ollama response: %w (body head: %.200s)", err, respBody)
	}
	if parsed.Error != "" {
		return Response{}, fmt.Errorf("ollama API error: %s", parsed.Error)
	}

	// THE THINKING REGRESSION, caught loudly and by name.
	//
	// Measured twice on this box: with thinking on, qwen3:8b returns
	// done_reason "length", message.content of length ZERO, and ~974 characters
	// of `thinking`, having spent its entire token budget reasoning about a
	// two-line message. 70/70 malformed, a 0.00 score.
	//
	// The error names done_reason because that is the only visible symptom, and
	// because a vague "empty response" would read as a busy box — which is how
	// criterion 3's `"think": false` gets "cleaned up" by a later contributor.
	if parsed.DoneReason != "" && parsed.DoneReason != "stop" {
		return Response{}, fmt.Errorf(
			"ollama returned done_reason %q with %d bytes of content: the generation did not finish. "+
				"done_reason %q with EMPTY content is the thinking regression — verify the request carried "+
				`"think": false (an omitempty bool silently drops it), or raise MaxTokens`,
			parsed.DoneReason, len(parsed.Message.Content), parsed.DoneReason)
	}
	if strings.TrimSpace(parsed.Message.Content) == "" {
		return Response{}, fmt.Errorf(
			"ollama returned empty message.content (done_reason %q): there is no verdict to parse",
			parsed.DoneReason)
	}

	respModel := parsed.Model
	if respModel == "" {
		respModel = model
	}
	return Response{
		Raw:              json.RawMessage(parsed.Message.Content),
		Model:            respModel,
		PromptTokens:     parsed.PromptEvalCount,
		CompletionTokens: parsed.EvalCount,
		// LatencyMS comes from the MODEL'S OWN total_duration, not from a
		// stopwatch around the HTTP call. That is the point of recording it: a
		// 3.4s cold load and a 0.3s warm call are the numbers an operator needs
		// in ai_runs, and a wall-clock measurement would blur them with network
		// and scheduling noise.
		LatencyMS: int(parsed.TotalDuration / int64(time.Millisecond)),
	}, nil
}

// Compile-time proof, mirroring the test's own assertion. Describe() being on
// Client is what makes "where does this send" unforgettable; Prober is what makes
// the local lane reachable at all, since router.probe treats a local client that
// cannot demonstrate reachability as UNREACHABLE.
var (
	_ Client = (*Ollama)(nil)
	_ Prober = (*Ollama)(nil)
)
