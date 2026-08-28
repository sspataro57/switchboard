package provider_test

// Unit tests for the ollama adapter (SWT-22, acceptance criteria 1-9).
// Everything runs against an httptest.Server — ZERO real network, ZERO live
// model, ever. Criteria 2-8 extend internal/provider/openai_test.go and
// openai_unavailable_test.go's shape exactly: a recording handler for the wire
// contract, a closed listener for transport failure, a sleeping handler for a
// 1ms deadline.
//
// GREENFIELD NOTE: internal/provider/ollama.go does not exist yet, so this file
// compile-FAILS under `go test ./...` — that is the expected red state, the same
// one internal/triage/worker_test.go's header declares. For greenfield code the
// SPEC's contract IS the signature; the surface exercised here is the SPEC's
// "Internal Go surface added" block verbatim:
//
//	func NewOllama(baseURL, model string, opts ...OllamaOption) *Ollama
//	func (o *Ollama) Complete(ctx context.Context, req Request) (Response, error)
//	func (o *Ollama) Describe() Descriptor   // {Name: "ollama", Endpoint: base}
//	func (o *Ollama) Probe(ctx context.Context) error
//
// NOT exercised, deliberately: `OllamaOption`. The SPEC declares the variadic but
// names no option constructor, so a test would be pinning an identifier nobody
// has chosen. Every call below passes zero options, which is the shape criterion
// 4's "keep_alive DEFAULTING to 30m" is about.
//
// Identifiers are prefixed `ol` so this file coexists with openai_test.go's
// fixtures in the same package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

const olModel = "qwen3:8b"

// olSchema stands in for internal/classify's schema — small, and NOTE what it
// does not have: a `confidence` field (criterion 18). The adapter forwards it
// verbatim; it does not own or synthesize a schema.
var olSchema = json.RawMessage(`{"type":"object","additionalProperties":false,` +
	`"required":["actionable","kind","title","reason"],"properties":{` +
	`"actionable":{"type":"boolean"},` +
	`"kind":{"type":"string","enum":["payment_due","deadline","appointment","action_required","informational"]},` +
	`"title":{"type":"string"},"reason":{"type":"string"}}}`)

func olRequest() provider.Request {
	return provider.Request{
		Model:      olModel,
		System:     "You classify personal mail for actionability.",
		User:       "From: alerts@bank.example\nSubject: Your payment is due",
		SchemaName: "classify_verdict",
		Schema:     olSchema,
		MaxTokens:  256,
	}
}

// olCannedChat is the NATIVE /api/chat envelope. Note the field names: they are
// `message.content`, `done_reason`, `prompt_eval_count`, `eval_count`,
// `total_duration` — not choices/finish_reason/usage. That difference is the
// evidence behind criterion 2: the spike's failure signature was
// `done_reason: length`, a native-API field, so the measurement that chose this
// model was taken here and not on /v1.
func olCannedChat(content string) string {
	return olChatEnvelope(content, "stop")
}

func olChatEnvelope(content, doneReason string) string {
	env := map[string]any{
		"model":             olModel,
		"created_at":        "2026-08-28T12:00:00.000000000Z",
		"message":           map[string]any{"role": "assistant", "content": content},
		"done":              true,
		"done_reason":       doneReason,
		"total_duration":    int64(4_040_000_000), // 4.04s, the spike's cold-load figure
		"load_duration":     int64(3_400_000_000),
		"prompt_eval_count": 123,
		"eval_count":        45,
	}
	b, _ := json.Marshal(env)
	return string(b)
}

const olVerdict = `{"actionable":true,"kind":"payment_due","title":"Card payment due 2026-09-03","reason":"names an amount and a due date"}`

// olRecorder is an httptest handler that records the request line and body, and
// answers /api/chat with a canned native envelope. Anything else 404s — which is
// exactly what a stale `/v1` base URL produces against a real ollama, and what
// criterion 5's trimming exists to prevent.
type olRecorder struct {
	paths   []string
	methods []string
	bodies  [][]byte
	tagsErr int // non-zero: /api/tags answers this status
	models  []string
}

func (r *olRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.paths = append(r.paths, req.URL.Path)
		r.methods = append(r.methods, req.Method)
		r.bodies = append(r.bodies, body)
		switch req.URL.Path {
		case "/api/chat":
			io.WriteString(w, olCannedChat(olVerdict))
		case "/api/tags":
			if r.tagsErr != 0 {
				w.WriteHeader(r.tagsErr)
				return
			}
			models := make([]any, 0, len(r.models))
			for _, m := range r.models {
				models = append(models, map[string]any{"name": m, "size": 5_200_000_000})
			}
			b, _ := json.Marshal(map[string]any{"models": models})
			w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"404 page not found"}`)
		}
	}
}

func (r *olRecorder) lastBody(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	if len(r.bodies) == 0 {
		t.Fatalf("no request reached the server at all")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.bodies[len(r.bodies)-1], &m); err != nil {
		t.Fatalf("request body is not a JSON object: %v (%s)", err, r.bodies[len(r.bodies)-1])
	}
	return m
}

// ---- criterion 1: it IS a Client and a Prober -------------------------------

// The compiler is the assertion. Describe() being on the interface is what makes
// "where does this send" unforgettable (provider.go), and Prober is what makes
// the local lane reachable at all — router.probe treats a local client that
// cannot demonstrate reachability as UNREACHABLE, so an Ollama that forgot Probe
// would skip every restricted message while looking perfectly configured.
var (
	_ provider.Client = (*provider.Ollama)(nil)
	_ provider.Prober = (*provider.Ollama)(nil)
)

// ---- criterion 2: the NATIVE /api/chat route --------------------------------

func TestOllama_PostsToTheNativeChatEndpoint(t *testing.T) {
	rec := &olRecorder{models: []string{olModel}}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := provider.NewOllama(srv.URL, olModel)
	if _, err := c.Complete(context.Background(), olRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(rec.paths) != 1 {
		t.Fatalf("server saw %d request(s), want 1: %v", len(rec.paths), rec.paths)
	}
	if rec.paths[0] != "/api/chat" {
		t.Errorf("POST path = %q, want %q. Criterion 2: this adapter speaks ollama's NATIVE API, not the "+
			"OpenAI-compatible /v1 route. The evidence is the spike's own failure signature — `done_reason: "+
			"length` is a native response field (/v1 returns `finish_reason`), so the measurement that chose "+
			"qwen3:8b was taken against /api/chat — and `think` (criterion 3) is a native REQUEST field that "+
			"/v1 has nowhere to carry", rec.paths[0], "/api/chat")
	}
	if rec.methods[0] != http.MethodPost {
		t.Errorf("method = %q, want POST", rec.methods[0])
	}
}

// ---- criterion 3: think:false and stream:false, ON THE WIRE ------------------

// The most important assertion in this file, and the one that is invisible
// without reading a raw response.
//
// Measured twice on this box (the spike, and again during SWT-22 ticket-start):
// with `"think": true` and a 200-token budget qwen3:8b returned
//
//	done_reason: length | content length: 0 | thinking length: 974 | eval_count: 200
//
// — 0.00 score, 70/70 malformed outputs, the entire budget spent reasoning about
// a two-line message, and EMPTY content. The same request with `"think": false`
// returned done_reason `stop` and 242 characters of valid schema-conforming JSON.
//
// Asserted as KEY PRESENCE, not just value: `omitempty` on a bool field silently
// drops false, which puts the adapter back on ollama's default (thinking ON for
// a thinking model) with a struct that reads as if it disabled it.
func TestOllama_EveryRequestDisablesThinkingAndStreaming(t *testing.T) {
	rec := &olRecorder{models: []string{olModel}}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := provider.NewOllama(srv.URL, olModel)
	// TWO calls: "every request body carries", not "the first one does".
	for i := 0; i < 2; i++ {
		if _, err := c.Complete(context.Background(), olRequest()); err != nil {
			t.Fatalf("Complete #%d: %v", i+1, err)
		}
	}
	if len(rec.bodies) != 2 {
		t.Fatalf("server saw %d request(s), want 2", len(rec.bodies))
	}

	for i, raw := range rec.bodies {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request %d body is not a JSON object: %v (%s)", i+1, err, raw)
		}
		for _, field := range []string{"think", "stream"} {
			v, ok := body[field]
			if !ok {
				t.Errorf("request %d has no %q key at all. It must be present and FALSE, not omitted: "+
					"`json:\",omitempty\"` on a bool drops false and hands the decision back to ollama's "+
					"default. With thinking on, qwen3:8b scored 0.00 with 70/70 malformed outputs — empty "+
					"message.content and done_reason \"length\", the whole token budget spent reasoning "+
					"about a ten-word subject. It is invisible unless you read the raw response",
					i+1, field)
				continue
			}
			if strings.TrimSpace(string(v)) != "false" {
				t.Errorf("request %d field %q = %s, want false. With %q true this adapter measures 0.00 "+
					"(70/70 malformed, empty content, done_reason \"length\"); streaming true makes the "+
					"response unparseable by a single json.Unmarshal", i+1, field, v, field)
			}
		}
	}
}

// ---- criterion 4: format, options, keep_alive -------------------------------

func TestOllama_RequestCarriesSchemaOptionsAndKeepAlive(t *testing.T) {
	rec := &olRecorder{models: []string{olModel}}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := provider.NewOllama(srv.URL, olModel)
	if _, err := c.Complete(context.Background(), olRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	body := rec.lastBody(t)

	// `format` is the SCHEMA OBJECT itself on the native API — not
	// {"type":"json_schema","json_schema":{...}}, which is /v1's spelling. The
	// spike's finding 4 is what makes this load-bearing: left unconstrained the
	// model returns `kind` as "payment due", "violation_to_cure" and
	// "statement-availabl…" in one run — a column nothing can GROUP BY.
	format, ok := body["format"]
	if !ok {
		t.Fatalf("request has no `format` key; without it ollama returns free text and the enum in the "+
			"schema is decorative. Body: %s", rec.bodies[0])
	}
	var gotFormat, wantFormat any
	if err := json.Unmarshal(format, &gotFormat); err != nil {
		t.Fatalf("format is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(olSchema, &wantFormat); err != nil {
		t.Fatalf("test schema is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(gotFormat, wantFormat) {
		t.Errorf("format is not the caller's schema forwarded verbatim.\n got: %s\nwant: %s\n"+
			"The adapter does not own or rewrite the schema — internal/classify does", format, olSchema)
	}

	opts, ok := body["options"]
	if !ok {
		t.Fatalf("request has no `options` key; num_predict and temperature both live there on the native API")
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(opts, &options); err != nil {
		t.Fatalf("options is not a JSON object: %v (%s)", err, opts)
	}
	if got, ok := options["num_predict"]; !ok || strings.TrimSpace(string(got)) != "256" {
		t.Errorf("options.num_predict = %s (present=%v), want 256 — Request.MaxTokens maps HERE, not to "+
			"max_tokens. Sent to the wrong key it is ignored and the model runs to its own default",
			got, ok)
	}
	temp, ok := options["temperature"]
	if !ok {
		t.Errorf("options.temperature is absent. It must be PINNED to 0, not left to ollama's 0.8 default: " +
			"a classifier that answers differently on two identical messages cannot be scored against a " +
			"labelled set, which is the only thing this ticket allows anyone to tune against")
	} else {
		var f float64
		if err := json.Unmarshal(temp, &f); err != nil || f != 0 {
			t.Errorf("options.temperature = %s, want 0", temp)
		}
	}

	ka, ok := body["keep_alive"]
	if !ok {
		t.Fatalf("request has no `keep_alive` key. Ollama DISCHARGES models after 5 minutes — /api/tags " +
			"lists what is on disk, /api/ps what is resident — so without this every message pays a 3.4s " +
			"cold load instead of running warm at 0.25-0.38s")
	}
	var kaStr string
	if err := json.Unmarshal(ka, &kaStr); err != nil {
		t.Fatalf("keep_alive is not a JSON string: %v (%s)", err, ka)
	}
	if kaStr != "30m" {
		t.Errorf("keep_alive = %q, want %q by default (criterion 4)", kaStr, "30m")
	}

	// The prompt has to actually arrive. System and user go as two messages in
	// the native `messages` array.
	var msgs []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body["messages"], &msgs); err != nil {
		t.Fatalf("messages is not an array of {role, content}: %v (%s)", err, body["messages"])
	}
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("messages = %+v, want [{system ...} {user ...}]", msgs)
	}
	req := olRequest()
	if msgs[0].Content != req.System || msgs[1].Content != req.User {
		t.Errorf("the prompt did not reach the wire verbatim:\n system=%q\n user=%q", msgs[0].Content, msgs[1].Content)
	}

	// The model name reaches the wire. NOTE, honestly: this assertion cannot tell
	// whether it came from NewOllama's argument or from Request.Model, because
	// the fixture sets both to the same value — and the SPEC does not say which
	// wins when they disagree, so there is nothing to pin. An equivalent mutant,
	// named rather than dressed up as coverage.
	var model string
	if err := json.Unmarshal(body["model"], &model); err != nil || model != olModel {
		t.Errorf("model = %s, want %q", body["model"], olModel)
	}
}

// ---- criterion 5: NewOllama trims `/` and `/v1` -----------------------------

// The likely production case, not a hypothetical: docs/runbooks/provider-locality.md
// today tells the operator to set
// `OPS_LOCAL_PROVIDER_URL=http://127.0.0.1:11434/v1` for the OpenAI adapter, so a
// stale value is what this adapter will be handed first.
//
// And the failure is the WORST-SHAPED one available: `/v1/api/chat` is a 404,
// 404 is an HTTP error, an HTTP error is NOT ErrUnavailable (criterion 7) — so
// every message would count as an `unclassified_error` and the pass would trip
// criterion 16's ratio raise. A stale URL would read as a broken adapter instead
// of as a skipped pass.
func TestOllama_NewOllamaTrimsTrailingSlashAndV1(t *testing.T) {
	const base = "http://127.0.0.1:11434"
	for _, spelling := range []string{base, base + "/", base + "/v1", base + "/v1/"} {
		t.Run(spelling, func(t *testing.T) {
			d := provider.NewOllama(spelling, olModel).Describe()
			if d.Name != "ollama" {
				t.Errorf("Describe().Name = %q, want %q (criterion 5) — the name is what a skip record and "+
					"the report show an operator, and what ai_runs.provider stores", d.Name, "ollama")
			}
			if d.Endpoint != base {
				t.Errorf("NewOllama(%q).Describe().Endpoint = %q, want %q. An untrimmed base makes the POST "+
					"go to %s/api/chat, which 404s — and a 404 is an HTTP error, not ErrUnavailable, so it "+
					"trips criterion 16's unclassified-error raise instead of skipping",
					spelling, d.Endpoint, base, d.Endpoint)
			}
			if got := provider.LocalityOf(d); got != provider.LocalityLocal {
				t.Errorf("LocalityOf(%q) = %v, want LocalityLocal — a loopback endpoint that does not "+
					"classify local disables the restricted lane entirely", d.Endpoint, got)
			}
		})
	}

	// The LAN spelling, which is what Q2's answer makes reachable later: a
	// cluster-side consumer cannot use a service name (LocalityOf refuses any
	// host that is not an IP literal, deliberately — resolving one would be I/O
	// and a TOCTOU), so `192.168.50.x` is the shape that works.
	lan := provider.NewOllama("http://192.168.50.3:11434/v1", olModel)
	if got := provider.LocalityOf(lan.Describe()); got != provider.LocalityLocal {
		t.Errorf("LocalityOf(%q) = %v, want LocalityLocal — the private-LAN address is the documented path "+
			"for running ollama off-box (criterion 27), and it must survive trimming",
			lan.Describe().Endpoint, got)
	}
}

// The trimming has to reach the WIRE, not just Describe(). Describe() feeds the
// locality check; the base URL feeds the POST; an implementation that trimmed
// one and not the other would pass the assertions above and 404 in production.
func TestOllama_AStaleV1BaseStillPostsToApiChat(t *testing.T) {
	rec := &olRecorder{models: []string{olModel}}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c := provider.NewOllama(srv.URL+"/v1", olModel)
	resp, err := c.Complete(context.Background(), olRequest())
	if err != nil {
		t.Fatalf("Complete against a base URL ending in /v1 failed: %v. The runbook currently documents that "+
			"exact spelling, so this is the value the adapter will actually be given (criterion 5). Paths "+
			"seen by the server: %v", err, rec.paths)
	}
	if len(rec.paths) == 0 || rec.paths[0] != "/api/chat" {
		t.Errorf("paths = %v, want [/api/chat]; the /v1 was carried into the request path", rec.paths)
	}
	if len(resp.Raw) == 0 {
		t.Errorf("Response.Raw is empty for a successful call")
	}
}

// ---- criterion 6: Probe needs a 2xx AND the model ---------------------------

// "The server is up but qwen3:8b is gone" is a LIVE failure mode, not a
// hypothetical: the spike left six models (~25 GB) on the box and the plan is to
// `ollama rm` five of them. Get that wrong in either direction and the pass is
// wrong in a way nobody sees — an unchecked probe hands every message to a
// server that answers 404 per message (unclassified_error, ratio raise), and an
// over-eager error instead of a skip would fail the pass rather than retry it.
func TestOllama_Probe_RequiresTwoHundredAndTheModel(t *testing.T) {
	t.Run("control: server up and model present", func(t *testing.T) {
		rec := &olRecorder{models: []string{"gemma3:4b", olModel, "llama3.1:8b"}}
		srv := httptest.NewServer(rec.handler())
		defer srv.Close()

		if err := provider.NewOllama(srv.URL, olModel).Probe(context.Background()); err != nil {
			t.Fatalf("Probe returned %v for a server listing the configured model. If the control cannot "+
				"succeed, the three refusals below prove nothing — they would be satisfied by a Probe that "+
				"always fails, which disables the local lane completely and silently", err)
		}
		if len(rec.paths) != 1 || rec.paths[0] != "/api/tags" {
			t.Fatalf("Probe hit %v, want [/api/tags] — the cheapest thing ollama answers", rec.paths)
		}
		if rec.methods[0] != http.MethodGet {
			t.Errorf("Probe method = %q, want GET", rec.methods[0])
		}
	})

	t.Run("server up, model gone", func(t *testing.T) {
		rec := &olRecorder{models: []string{"gemma3:4b", "llama3.1:8b"}}
		srv := httptest.NewServer(rec.handler())
		defer srv.Close()

		err := provider.NewOllama(srv.URL, olModel).Probe(context.Background())
		if err == nil {
			t.Fatalf("Probe succeeded against a server that does not have %q. A 2xx alone is not evidence: "+
				"the plan is to `ollama rm` five of the six models the spike left behind, so this is the "+
				"failure that will actually happen — and it would produce a 404 per message, i.e. ~1,609 "+
				"unclassified errors and criterion 16's raise", olModel)
		}
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("Probe with an absent model returned %v, which does not wrap ErrUnavailable. It must be "+
				"a SKIP retried next pass — never an error, and never a hosted call", err)
		}
		if !strings.Contains(err.Error(), olModel) {
			t.Errorf("the probe error does not name the missing model (%v); an operator reading it must be "+
				"told what to `ollama pull`", err)
		}
	})

	t.Run("non-2xx", func(t *testing.T) {
		rec := &olRecorder{tagsErr: http.StatusInternalServerError}
		srv := httptest.NewServer(rec.handler())
		defer srv.Close()

		err := provider.NewOllama(srv.URL, olModel).Probe(context.Background())
		if err == nil || !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("Probe against a 500 returned %v; want an error wrapping ErrUnavailable (criterion 6)", err)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening now: `ollama serve` is not running

		err := provider.NewOllama(url, olModel).Probe(context.Background())
		if err == nil || !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("Probe against a closed listener returned %v; want an error wrapping ErrUnavailable — "+
				"the box being off is the ordinary state of a workstation-hosted lane (Q2, option A)", err)
		}
	})
}

// ---- criterion 7: failure typing --------------------------------------------

func TestOllama_TransportFailureIsErrUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := provider.NewOllama(url, olModel).Complete(context.Background(), olRequest())
	if err == nil {
		t.Fatalf("Complete against a closed listener returned no error")
	}
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("errors.Is(err, ErrUnavailable) = false (err=%v). Criterion 7 keeps SWT-21 criterion 8's "+
			"typing exactly: a refused connection is the local box being off, which is normal operation for "+
			"a lane that lives on a desktop — it must SKIP the message (avail_reason local_unreachable) and "+
			"not count toward the unclassified-error ratio", err)
	}
}

func TestOllama_DeadlineIsErrUnavailable(t *testing.T) {
	slow := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slow:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	// LIFO: Close() runs last, after the handler is released.
	defer srv.Close()
	defer close(slow)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	_, err := provider.NewOllama(srv.URL, olModel).Complete(ctx, olRequest())
	if err == nil {
		t.Fatalf("Complete past its deadline returned no error")
	}
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("deadline exceeded: errors.Is(err, ErrUnavailable) = false (err=%v). A 5.9 GB model paying "+
			"a 3.4s cold load is slow BY DESIGN; a pass that raises every time it is slow trains its "+
			"operator to ignore it", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause was lost: errors.Is(err, context.DeadlineExceeded) = false (err=%v). Two %%w "+
			"verbs — the sentinel for classification, the cause for diagnosis", err)
	}
}

// The other half, and the half that makes the first half mean anything. If these
// were wrapped, a permanently broken adapter would look like a busy box forever
// and criterion 16's ratio could never fire.
func TestOllama_ProtocolFailuresAreNotErrUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		why     string
		wantIn  []string
	}{
		{"HTTP 404 (the untrimmed /v1 case)", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"404 page not found"}`)
		}, "a wrong PATH is a broken configuration, not a busy box — and criterion 5 exists because this " +
			"is what a stale OPS_LOCAL_PROVIDER_URL produces", []string{"404"}},
		{"HTTP 500", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"model runner has unexpectedly stopped"}`)
		}, "the server answered; it is reachable and wrong, not absent", []string{"500"}},
		{"malformed JSON body", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `not json at all`)
		}, "a parse failure is a broken adapter, not a busy one", nil},
		{"empty message.content with done_reason length", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, olChatEnvelope("", "length"))
		}, "THE THINKING REGRESSION. Measured: done_reason `length`, content length 0, thinking length 974, " +
			"the whole 200-token budget spent reasoning about a two-line message. It must be LOUD and it " +
			"must name done_reason, or it reads like a busy box and criterion 3's fix gets 'cleaned up'",
			[]string{"done_reason", "length"}},
		{"done_reason is not stop", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, olChatEnvelope(olVerdict, "load"))
		}, "a truncated or aborted generation is not a verdict, even when some content came back",
			[]string{"done_reason", "load"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			_, err := provider.NewOllama(srv.URL, olModel).Complete(context.Background(), olRequest())
			if err == nil {
				t.Fatalf("%s returned no error", tc.name)
			}
			if errors.Is(err, provider.ErrUnavailable) {
				t.Errorf("%s was wrapped with ErrUnavailable (err=%v) — %s", tc.name, err, tc.why)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error %q does not mention %q. %s", tc.name, err, want, tc.why)
				}
			}
		})
	}
}

// ---- criterion 8: usage and latency come from the native fields -------------

func TestOllama_FillsUsageAndLatencyFromTheNativeResponse(t *testing.T) {
	rec := &olRecorder{models: []string{olModel}}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	resp, err := provider.NewOllama(srv.URL, olModel).Complete(context.Background(), olRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(resp.Raw) != olVerdict {
		t.Errorf("Response.Raw = %s, want the message.content verbatim: %s", resp.Raw, olVerdict)
	}
	if resp.Model != olModel {
		t.Errorf("Response.Model = %q, want %q as reported by the API", resp.Model, olModel)
	}
	if resp.PromptTokens != 123 {
		t.Errorf("PromptTokens = %d, want 123 from prompt_eval_count — ai_runs must read the same way for "+
			"both lanes, and this is the field the native API puts it in", resp.PromptTokens)
	}
	if resp.CompletionTokens != 45 {
		t.Errorf("CompletionTokens = %d, want 45 from eval_count", resp.CompletionTokens)
	}
	// total_duration is 4_040_000_000 ns = 4040 ms — the spike's cold-load figure,
	// chosen because a local httptest round trip can never take that long. A
	// wall-clock measurement here would come back in single-digit milliseconds,
	// so this discriminates between "converted the model's number" and "timed the
	// HTTP call and called it the same thing".
	if resp.LatencyMS != 4040 {
		t.Errorf("LatencyMS = %d, want 4040 (total_duration 4_040_000_000 ns → ms). The value must come from "+
			"the response, not from a stopwatch around the request: the whole point of recording it is that "+
			"a 3.4s cold load and a 0.3s warm call are visible in ai_runs", resp.LatencyMS)
	}
}

// ---- criterion 9: cmd/triage's local lane -----------------------------------

// A source scan, in internal/provider/structure_test.go's shape and using its
// repoFile helper.
//
// Scoped to the block that builds the local lane rather than the whole file,
// because cmd/triage legitimately still constructs NewOpenAI for the GENERAL
// lane — a file-wide ban would fail on the correct code.
func TestCmdTriage_LocalLaneIsOllamaAndRequiresItsModel(t *testing.T) {
	src := repoFile(t, "cmd/triage/main.go")

	start := strings.Index(src, "OPS_LOCAL_PROVIDER_URL")
	if start < 0 {
		t.Fatalf("cmd/triage/main.go never reads OPS_LOCAL_PROVIDER_URL; there is no local lane to check")
	}
	end := strings.Index(src[start:], "NewRouter(")
	if end < 0 {
		t.Fatalf("cannot find NewRouter( after the local-lane block; the scan has nothing to scope to")
	}
	block := src[start : start+end]

	if !strings.Contains(block, "provider.NewOllama") {
		t.Errorf("cmd/triage's local lane does not call provider.NewOllama. Criterion 9: the local stack is "+
			"ollama and its native API, so NewOpenAI pointed at :11434 sends /v1/chat/completions with no "+
			"`think` field at all — which is the 0.00-scoring configuration. Block:\n%s", block)
	}
	if strings.Contains(block, "provider.NewOpenAI") {
		t.Errorf("cmd/triage's local lane still constructs provider.NewOpenAI:\n%s", block)
	}
	if !strings.Contains(block, "OPS_LOCAL_MODEL") {
		t.Errorf("cmd/triage's local lane does not read OPS_LOCAL_MODEL")
	}

	// The fallback that criterion 9 removes, by its exact spelling. Today
	// `localModel := model` defaults the LOCAL lane to gpt-5-mini, which on
	// ollama is a 404 PER MESSAGE — an unclassified error on every message, which
	// reads as a broken adapter rather than as an absent lane.
	fallback := regexp.MustCompile(`localModel\s*:?=\s*model\b`)
	if fallback.MatchString(src) {
		t.Errorf("cmd/triage/main.go still falls back to the hosted model name for the local lane " +
			"(`localModel = model`). Criterion 9 makes OPS_LOCAL_MODEL REQUIRED when OPS_LOCAL_PROVIDER_URL " +
			"is set: missing → the local lane is ABSENT with one logged refusal (a skipped pass), not 14,000 " +
			"per-message 404s that trip the unclassified-error raise")
	}
	// ...and the refusal is LOGGED. Startup is the only moment an operator is
	// watching, and an absent lane with no log line is indistinguishable from a
	// lane nobody configured.
	logged := regexp.MustCompile(`(?s)OPS_LOCAL_MODEL.{0,400}slog\.(Warn|Error)`)
	if !logged.MatchString(block) {
		t.Errorf("nothing logs a refusal near the OPS_LOCAL_MODEL read. Criterion 9: missing model → the "+
			"lane is absent with ONE logged refusal; silence here means the operator learns about it from a "+
			"skipped-lane count hours later. Block:\n%s", block)
	}
}

// A tiny positive control for the scan above: the token it bans in the local
// block must be findable where it IS allowed, or a rename would make the ban
// pass while matching nothing anywhere. (Same reasoning as
// TestProviderConstructors_LiveInCmdOnly.)
func TestCmdTriage_StillBuildsTheGeneralLane(t *testing.T) {
	src := repoFile(t, "cmd/triage/main.go")
	if !strings.Contains(src, "provider.NewOpenAI") {
		t.Errorf("cmd/triage/main.go no longer constructs provider.NewOpenAI anywhere. Criterion 9 changes "+
			"the LOCAL lane only; the hosted lane is untouched, and without it the previous test bans a "+
			"token that cannot appear. %s", fmt.Sprintf("(file is %d bytes)", len(src)))
	}
}
