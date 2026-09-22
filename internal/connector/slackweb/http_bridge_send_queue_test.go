package slackweb

// slack-send-queue (SWT-76, docs/tickets/slack-send-queue_SPEC.md) Part 2 —
// the Go client: criteria 10, 11, 12, 13, plus the D3 half that belongs to the
// wire (max_queue_ms travels in the REQUEST) and D10's CommandBridge rule.
//
// ZERO Slack, zero browser, zero mini. The leaf's 202 is faked with httptest
// exactly as the SWT-75 tests do (send_503_test.go) — the leaf's own half of
// this ticket is the slackconnector repo's and is not testable from here.
//
// ---------------------------------------------------------------------------
// GREENFIELD NOTE — this file compile-FAILs today, which is the expected red.
// `Send` takes three arguments and returns `error`; `SendOutcome`,
// `ErrBridgeQueued` and `BridgeQueuedError` do not exist; `post` returns two
// values. Nothing below can build until the surface does.
//
// IMPOSED SURFACE (the SPEC's "API / MCP tool changes" section, with ONE
// deviation, named and argued):
//
//	// SendOutcome distinguishes a click that HAPPENED from a leaf ACCEPTANCE.
//	// Queued=false is a completed send (the leaf answered sent:true); Queued=true
//	// means the click is still pending on the leaf's browser queue and the row
//	// must stay in 'sending' with its attempt unsettled.
//	type SendOutcome struct {
//		Queued    bool
//		JobID     string
//		QueuedAt  time.Time
//		ExpiresIn time.Duration
//	}
//
//	func (b *HTTPBridge) Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (SendOutcome, error)
//	func (b *CommandBridge) Send(ctx context.Context, targetURL, text string, maxQueue time.Duration) (SendOutcome, error)
//	func (b *HTTPBridge) post(ctx context.Context, path string, body []byte) ([]byte, int, error)
//
//	// The typed refusal Export and Draft return on a 202 (criterion 10), shaped
//	// like ErrBridgeBusy/BridgeBusyError so callers match with errors.Is.
//	var ErrBridgeQueued = errors.New("Slack bridge queued the request")
//	type BridgeQueuedError struct{ Path, JobID, Body string }
//	func (e *BridgeQueuedError) Error() string
//	func (e *BridgeQueuedError) Is(target error) bool
//
// DEVIATION FROM THE SPEC, deliberate: the SPEC's "Files likely to touch" puts
// `SendOutcome` in `internal/tools/delivery.go` beside `SlackSender`. That
// cannot compile. `internal/tools` imports `internal/connector/slackweb`
// (delivery.go uses slackweb.ParseTargetURL and *slackweb.SendRejectedError),
// so a SendOutcome defined in tools and returned by *slackweb.HTTPBridge would
// be an import cycle. The type therefore lives in slackweb — where
// SendRejectedError, ErrBridgeBusy and the rest of the wire vocabulary already
// live — and `tools.SlackSender` names `slackweb.SendOutcome` in its method
// signature, which is what lets cmd/dashboard keep passing ONE
// slackweb.NewDeliveryBridgeFromEnv() object into SetSlackSender
// (sender_seam_test.go's compile-time proof).
//
// EXISTING TESTS THAT MUST BE RE-SIGNATURED BY THE IMPLEMENTER (not by this
// file — they pin behaviour this ticket does not change and their assertions
// stay verbatim): send_test.go and send_503_test.go call the 3-argument Send.
// Criterion 13's classification is re-pinned below under the NEW signature so
// that the classification cannot be lost in the edit.
//
// MUTATION MAP (SPEC "Mutations that must turn a test red"):
//	Return 202 from /export                      -> TestQueue_ExportAndDraftRefuseA202
//	Treat a 202 as bridgeStatusError in post     -> TestQueue_PostReturnsStatusAnd200And202AreSuccess
//	Run checkSendResult on a 202 body            -> TestQueue_SendOn202ReturnsAQueuedOutcome
//	Revert the 503 -> SendRejectedError mapping  -> TestQueue_SWT75ClassificationSurvivesTheNewSignature

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Compile-time proof of the seam's new shape on BOTH transports (D10). An
// inline interface, not tools.SlackSender: this is package slackweb and
// internal/tools imports it, so naming tools here is the cycle the deviation
// note above exists to avoid. sender_seam_test.go (external package) pins the
// tools.SlackSender side.
var (
	_ interface {
		Send(context.Context, string, string, time.Duration) (SendOutcome, error)
	} = (*HTTPBridge)(nil)
	_ interface {
		Send(context.Context, string, string, time.Duration) (SendOutcome, error)
	} = (*CommandBridge)(nil)
)

const queueMaxWait = 10 * time.Minute

// ---- criterion 10: post reports the status; 200 and 202 are both success ----

func TestQueue_PostReturnsStatusAnd200And202AreSuccess(t *testing.T) {
	var answer int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(answer)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []int{http.StatusOK, http.StatusAccepted} {
		answer = want
		out, status, err := bridge.post(context.Background(), "/send", []byte(`{}`))
		if err != nil {
			t.Fatalf("post on HTTP %d = %v, want nil: criterion 10 makes 200 AND 202 success, because a 202 "+
				"is the leaf ACCEPTING the send — treating it as a bridgeStatusError would make every queued "+
				"send AMBIGUOUS and wedge the row in `sending` (D12)", want, err)
		}
		if status != want {
			t.Errorf("post returned status %d, want %d — the caller cannot tell a click from an acceptance "+
				"without it", status, want)
		}
		if len(out) == 0 {
			t.Errorf("post returned an empty body on HTTP %d; the 202's job_id lives there", want)
		}
	}

	// Everything else is still a bridgeStatusError with its status, unchanged.
	answer = http.StatusInternalServerError
	if _, _, err := bridge.post(context.Background(), "/send", []byte(`{}`)); err == nil {
		t.Fatal("post on HTTP 500 = nil error; only 200 and 202 are success")
	} else {
		var st *bridgeStatusError
		if !errors.As(err, &st) || st.status != http.StatusInternalServerError {
			t.Errorf("post on HTTP 500 = %v (%T), want a *bridgeStatusError carrying 500", err, err)
		}
	}
}

// Criterion 10, second half: ONLY Send interprets a 202. A 202 on /export means
// the leaf queued browser work whose RESULT those paths require — letting it
// through would ingest an empty export or report a draft that was never typed.
func TestQueue_ExportAndDraftRefuseA202(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"queued":true,"job_id":"job-abc"}`))
	}))
	defer server.Close()
	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("export", func(t *testing.T) {
		exported, err := bridge.Export(context.Background(), ExportRequest{BudgetMS: 150000, MaxConversations: 2})
		if err == nil {
			t.Fatalf("Export on a 202 = nil error and export %+v; criterion 10: a queued export has no result, "+
				"and ingesting the empty one would look like a Slack workspace that went quiet", exported)
		}
		if !errors.Is(err, ErrBridgeQueued) {
			t.Fatalf("Export on a 202 = %v (%T), want an error matching ErrBridgeQueued (a TYPED refusal, so a "+
				"caller can tell it from a busy 503 or a broken leaf)", err, err)
		}
		if exported.SchemaVersion != 0 || len(exported.Workspaces) != 0 {
			t.Errorf("Export on a 202 returned content %+v; it must ingest NOTHING", exported)
		}
	})

	t.Run("draft", func(t *testing.T) {
		err := bridge.Draft(context.Background(), sendTarget, "hi")
		if err == nil {
			t.Fatal("Draft on a 202 = nil error; the composer was never typed, and reporting success would tell " +
				"the assisted tier a human can go press Send on words that are not there")
		}
		if !errors.Is(err, ErrBridgeQueued) {
			t.Fatalf("Draft on a 202 = %v (%T), want an error matching ErrBridgeQueued", err, err)
		}
	})
}

// ---- criterion 11: Send maps 202 to a queued outcome, 200 to a click --------

func TestQueue_SendOn202ReturnsAQueuedOutcome(t *testing.T) {
	const jobID = "send-7f3c19"
	const queuedAt = "2026-09-22T14:05:09Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		// NOTE: no "sent" key and no "drafted" key. checkSendResult would read
		// sent:false here and turn the acceptance into a DEFINITE refusal —
		// exactly the mutation "run checkSendResult on a 202 body".
		_, _ = w.Write([]byte(`{"queued":true,"job_id":"` + jobID + `","queued_at":"` + queuedAt + `",
			"estimated_wait_ms":213000,"expires_in_ms":600000}`))
	}))
	defer server.Close()
	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

	out, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
	if err != nil {
		t.Fatalf("Send on a 202 = %v, want nil error: the leaf ACCEPTED the send and will click it in the "+
			"first gap. An error here is the whole defect the ticket removes — the row goes to `failed` and "+
			"costs Salvador a second approval (criterion 11)", err)
	}
	if !out.Queued {
		t.Fatalf("Send on a 202 = %+v, want Queued:true. The caller branches on Queued, never on a zero "+
			"value: a Queued:false outcome here would record a send that has not happened", out)
	}
	if out.JobID != jobID {
		t.Errorf("SendOutcome.JobID = %q, want %q — it is what the dashboard label and the delivery_queued "+
			"event name, and what the mini's log is grepped for", out.JobID, jobID)
	}
	want, _ := time.Parse(time.RFC3339, queuedAt)
	if !out.QueuedAt.Equal(want) {
		t.Errorf("SendOutcome.QueuedAt = %s, want the leaf's queued_at %s", out.QueuedAt, want)
	}
	if out.ExpiresIn != 600*time.Second {
		t.Errorf("SendOutcome.ExpiresIn = %s, want 600s from expires_in_ms (the leaf's TTL, in MILLISECONDS "+
			"on the wire)", out.ExpiresIn)
	}
}

// The unchanged half, byte for byte: a 200 {drafted:false, sent:true} is a
// completed click. checkSendResult still owns this body.
func TestQueue_SendOn200IsAnUnqueuedCompletedSend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"drafted":false,"sent":true}`))
	}))
	defer server.Close()
	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

	out, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
	if err != nil {
		t.Fatalf("Send on a 200 send result = %v, want nil", err)
	}
	if out.Queued {
		t.Errorf("Send on a 200 = %+v, want Queued:false — the click HAPPENED, and marking it queued would "+
			"leave the row in `sending` forever waiting for a job that already ran", out)
	}

	// And a 200 that is NOT an exact send result is still refused (checkSendResult
	// is untouched on the 200 path).
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"drafted":true,"sent":false}`))
	}))
	defer server2.Close()
	bridge2, _ := NewHTTPBridge(server2.URL, testToken, server2.Client())
	if _, err := bridge2.Send(context.Background(), sendTarget, "hi", queueMaxWait); err == nil {
		t.Error("Send accepted a 200 {drafted:true,sent:false}; only {drafted:false,sent:true} is a click")
	}
}

// Criterion 12: the protection is the unsettled attempt plus the lease, NOT the
// id (D4). A leaf that forgets job_id is a leaf defect to log, never a reason to
// treat an accepted send as refused.
func TestQueue_SendOn202WithoutAJobIDIsStillQueued(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"queued":true}`))
	}))
	defer server.Close()
	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

	out, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
	if err != nil {
		t.Fatalf("Send on a 202 with no job_id = %v, want nil error (D4: the missing id is a logged leaf "+
			"defect; the row is protected by send_settled_at IS NULL and the 15-minute lease)", err)
	}
	if !out.Queued {
		t.Fatalf("Send on a 202 with no job_id = %+v, want Queued:true", out)
	}
	if out.JobID != "" {
		t.Errorf("SendOutcome.JobID = %q, want an empty string — nothing may invent an id", out.JobID)
	}
}

// ---- D3's wire half: max_queue_ms travels in the REQUEST --------------------

// D12's version gate, on our side of it: the presence of max_queue_ms is the
// caller's declaration that it understands the queue. Sending it is what PERMITS
// the leaf to answer 202 at all — and rollback lever 1
// (SLACK_SEND_QUEUE_MAX_WAIT=0 -> "never queue") is implemented by OMITTING it,
// with no leaf roll and no restart.
func TestQueue_MaxQueueMSTravelsInTheRequestAndZeroOmitsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxQueue time.Duration
		wantKey  bool
		wantMS   float64
	}{
		{"ten minutes", queueMaxWait, true, 600000},
		{"zero never queues", 0, false, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)
				_, _ = w.Write([]byte(`{"drafted":false,"sent":true}`))
			}))
			defer server.Close()
			bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
			if _, err := bridge.Send(context.Background(), sendTarget, "hi", tc.maxQueue); err != nil {
				t.Fatalf("Send = %v", err)
			}

			if body["target_url"] != sendTarget || body["text"] != "hi" {
				t.Errorf("request body = %v, want target_url and text verbatim (D11: unchanged)", body)
			}
			got, present := body["max_queue_ms"]
			if present != tc.wantKey {
				t.Fatalf("max_queue_ms present = %v, want %v. D3: the bound travels in the REQUEST because the "+
					"CALLER owns the 15-minute lease that protects the row; D12: its presence is the version "+
					"gate, so omitting it is the no-roll rollback to today's wait-or-503 behaviour. Body: %v",
					present, tc.wantKey, body)
			}
			if tc.wantKey {
				if n, ok := got.(float64); !ok || n != tc.wantMS {
					t.Errorf("max_queue_ms = %v (%T), want %v as a JSON number in MILLISECONDS "+
						"(the leaf reads it with optionalPositiveInteger)", got, got, tc.wantMS)
				}
			}
		})
	}
}

// ---- criterion 13: the SWT-75 classification is intact ----------------------

// Re-pinned under the NEW signature so the edit that adds maxQueue cannot lose
// it. The full argument lives in send_503_test.go; the fact restated here is
// that D1's 202 branch moves NOTHING across the definite/ambiguous line.
func TestQueue_SWT75ClassificationSurvivesTheNewSignature(t *testing.T) {
	definite := []int{http.StatusServiceUnavailable, http.StatusTooManyRequests,
		http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest}
	for _, status := range definite {
		status := status
		t.Run("definite/"+http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"busy","retry_after_seconds":45}`))
			}))
			defer server.Close()
			bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

			out, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
			var rejected *SendRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("Send on HTTP %d = %v (%T), want *SendRejectedError. The 503 is still the leaf's "+
					"pre-click refusal — queue full (D9) or an estimate over max_queue_ms (D3) — and D11 says "+
					"the SWT-75 mapping does not move", status, err, err)
			}
			if rejected.Status != status {
				t.Errorf("SendRejectedError.Status = %d, want %d", rejected.Status, status)
			}
			if out.Queued {
				t.Errorf("a refused send returned Queued:true (%+v); a refusal is not an acceptance", out)
			}
		})
	}

	t.Run("ambiguous/500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"SLACK_UI_CHANGED"}`))
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
		out, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
		var rejected *SendRejectedError
		if err == nil || errors.As(err, &rejected) {
			t.Errorf("Send on HTTP 500 = (%+v, %v); a 500 can come from browser work that already clicked and "+
				"must stay AMBIGUOUS and untyped", out, err)
		}
		if out.Queued {
			t.Errorf("an ambiguous failure returned Queued:true (%+v)", out)
		}
	})

	t.Run("definite/dial failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
		server.Close() // no TCP session is possible, so the request never left
		_, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
		var rejected *SendRejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("dial failure = %v (%T), want *SendRejectedError", err, err)
		}
	})

	t.Run("ambiguous/EOF mid-response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
		_, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
		var rejected *SendRejectedError
		if errors.As(err, &rejected) {
			t.Errorf("a failure AFTER dispatch = *SendRejectedError, want untyped/ambiguous: %v", err)
		}
	})
}

// ---- D10: the CLI transport can never queue ---------------------------------

// CommandBridge spawns a one-shot node process. It has no queue and cannot
// answer 202, so it reports a completed send and ignores maxQueue. The point of
// the assertion is that a naked zero value is never MISREAD: Queued must be
// false on the success path, so `if out.Queued` is a safe branch for a caller
// that does not know which transport it holds.
func TestQueue_CommandBridgeNeverQueues(t *testing.T) {
	bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"drafted":false,"sent":true}`))
	out, err := bridge.Send(context.Background(), sendTarget, "hi", queueMaxWait)
	if err != nil {
		t.Fatalf("CommandBridge.Send = %v, want nil", err)
	}
	if out.Queued || out.JobID != "" {
		t.Errorf("CommandBridge.Send = %+v, want a zero SendOutcome: the subprocess transport has no queue "+
			"and structurally cannot answer 202 (D10)", out)
	}
}
