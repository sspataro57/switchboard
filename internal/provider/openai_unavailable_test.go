package provider_test

// SWT-21 acceptance criteria 1 and 8: the adapter declares where it sends, and
// its TRANSPORT failures are typed.
//
// Everything runs against httptest — no live provider, ever. The two shapes are
// driven the way criterion 8 names them: a closed listener (connection refused)
// and a handler that sleeps past a 1ms client deadline.
//
// Why the typing matters, in the terms of criterion 18: "the local box is busy"
// and "the local adapter is broken" must not share an alarm. The first is normal
// operation for a 4B model at low priority and must never fail a pass; the second
// is news. errors.Is(err, ErrUnavailable) is the only thing that can tell them
// apart, because both arrive as a non-nil error from the same method.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/provider"
)

func unavailableTestRequest() provider.Request {
	return provider.Request{
		Model:      "qwen3:8b",
		System:     "s",
		User:       "u",
		SchemaName: "triage_extraction",
		Schema:     triageSchema,
		MaxTokens:  64,
	}
}

// A refused connection is the commonest shape of "the local box is not serving
// right now": nothing is listening on 11434 because the unit is stopped.
func TestOpenAI_ConnectionFailureIsErrUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := provider.NewOpenAI(testAPIKey, url)
	_, err := c.Complete(context.Background(), unavailableTestRequest())
	if err == nil {
		t.Fatalf("Complete against a closed listener returned no error")
	}
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("Complete against a closed listener: errors.Is(err, ErrUnavailable) = false (err=%v). "+
			"Criterion 8: connection failures are wrapped, so criterion 18 can skip the message instead of "+
			"counting it as a broken adapter", err)
	}
}

// A deadline is the second shape, and the one the SPEC calls out by name: "a
// timeout is normal operation, not an error".
func TestOpenAI_DeadlineIsErrUnavailable(t *testing.T) {
	slow := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slow:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	// Defers run LIFO: srv.Close() is registered first so it runs LAST, after the
	// handler has been released. The other order makes Close() block for 5s on a
	// connection its own handler is still sitting in.
	defer srv.Close()
	defer close(slow)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	c := provider.NewOpenAI(testAPIKey, srv.URL)
	_, err := c.Complete(ctx, unavailableTestRequest())
	if err == nil {
		t.Fatalf("Complete past its deadline returned no error")
	}
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("deadline exceeded: errors.Is(err, ErrUnavailable) = false (err=%v). Criterion 18 lists "+
			"'including deadline exceeded' explicitly — a 4B model at low priority is slow by design", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the underlying cause was lost: errors.Is(err, context.DeadlineExceeded) = false (err=%v). "+
			"Wrapping with a sentinel must not replace the diagnosis", err)
	}
}

// The other half of criterion 8, and the half that makes the first half mean
// anything: HTTP-status, parse and schema failures are NOT wrapped. If they
// were, a permanently broken adapter would look like a busy box forever and
// criterion 18's ratio raise could never fire.
func TestOpenAI_ProtocolFailuresAreNotErrUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		why     string
	}{
		{"HTTP 500", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
		}, "the server answered; it is reachable and wrong, not absent"},
		{"HTTP 400", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"bad schema","type":"invalid_request_error"}}`)
		}, "a malformed request to a local adapter is the unclassified_error tier"},
		{"malformed JSON body", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `not json at all`)
		}, "a parse failure is a broken adapter, not a busy one"},
		{"no choices", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"model":"qwen3:8b","choices":[]}`)
		}, "a schema violation is a broken adapter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c := provider.NewOpenAI(testAPIKey, srv.URL)
			_, err := c.Complete(context.Background(), unavailableTestRequest())
			if err == nil {
				t.Fatalf("%s returned no error", tc.name)
			}
			if errors.Is(err, provider.ErrUnavailable) {
				t.Errorf("%s was wrapped with ErrUnavailable (err=%v) — %s", tc.name, err, tc.why)
			}
		})
	}
}

// Criterion 1: Describe() is on the interface, and it reports the CONFIGURED
// endpoint. The assertion that carries weight is the second one: the same
// adapter type, constructed twice, yields two different localities.
func TestOpenAI_DescribeReportsTheConfiguredEndpoint(t *testing.T) {
	var c provider.Client = provider.NewOpenAI(testAPIKey, "http://127.0.0.1:11434/v1")
	d := c.Describe()
	if d.Endpoint == "" {
		t.Fatalf("Describe().Endpoint is empty; an adapter that declares nothing fails closed, so this " +
			"would silently disable the local lane")
	}
	if d.Name == "" {
		t.Errorf("Describe().Name is empty; the name is what a skip record and the report show an operator")
	}
	if provider.LocalityOf(d) != provider.LocalityLocal {
		t.Errorf("LocalityOf(%q) = %v, want LocalityLocal", d.Endpoint, provider.LocalityOf(d))
	}

	// Default construction (empty baseURL) must describe the PUBLIC API, not an
	// empty string that some future reader might treat as "unset, therefore
	// harmless".
	def := provider.NewOpenAI(testAPIKey, "")
	if provider.LocalityOf(def.Describe()) == provider.LocalityLocal {
		t.Errorf("NewOpenAI(key, \"\") describes %q, which classifies local — the default base URL is the "+
			"hosted API", def.Describe().Endpoint)
	}
}
