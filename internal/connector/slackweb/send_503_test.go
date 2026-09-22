// 2026-09-22 (SWT-76): every bridge.Send call gained the maxQueue argument (0 = never queue) and the SendOutcome return; assertions unchanged.
package slackweb

// slack-watch-sweep (SWT-75) Part 7, criterion 26 — D7: HTTPBridge.Send maps
// 503 (and 429) to SendRejectedError; 500 and a mid-response EOF stay
// ambiguous. Plus the /export half of the same status: a 503 there is
// ErrBridgeBusy carrying the leaf's Retry-After (criterion 14).
//
// WHY A 503 IS DEFINITE, and this is the whole argument. The leaf's 503 comes
// from QueueFullError, thrown inside JobQueue.run BEFORE the job function is
// called (slackconnector src/browser/job-queue.ts:144-152) and converted at
// src/switchboard/http-bridge.ts:267-273. No other path in the leaf returns
// 503. It is therefore provably PRE-CLICK, exactly like the 4xx cases and the
// failed dial that send_test.go already classifies as definite — and unlike a
// 500, which may be thrown by browser work that has already pressed Send.
// 429 is included for the same reason should it ever be added.
//
// WHY IT IS NOT HOUSEKEEPING. Today a Slack send that meets a busy browser
// wedges the delivery row in `sending`, where nothing retries it and a human
// must run mark_delivery_failed (itself lease-guarded for 15 minutes). This
// ticket makes the browser busy most of every minute, so "rare" becomes
// "constant": every approved Slack send would otherwise wedge (D7). This is
// also the standing ACTION in the project memory
// ("slack leaf now serialises + can 503").
//
// IMPOSED SURFACE: SendRejectedError already exists (http_bridge.go:178-188)
// and its meaning is unchanged — "a DEFINITE bridge refusal: the send did not
// happen, so the failed->approved retry path is safe to reopen". This ticket
// only moves 503/429 across the line. ErrBridgeBusy / BridgeBusyError are
// specified in watch_test.go's imposed-surface block.
//
// EXPECTED RED: the 503/429 cases fail their own assertion (Send leaves them
// untyped today, http_bridge.go:220-232), and the /export cases fail to
// compile until ErrBridgeBusy exists.
//
// MUTATION: revert Send's 503 mapping to ambiguous -> TestSend_503And429AreDefinite.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Criterion 26, first half.
func TestSend_503And429AreDefinite(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"bridge busy: a sweep is running"}`))
			}))
			defer server.Close()
			bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
			if err != nil {
				t.Fatal(err)
			}

			_, err = bridge.Send(context.Background(), sendTarget, "hi", 0)
			if err == nil {
				t.Fatalf("Send on HTTP %d = nil error", status)
			}
			var rejected *SendRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("Send on HTTP %d = %v (%T), want *SendRejectedError. D7: the leaf throws this from "+
					"QueueFullError inside JobQueue.run, BEFORE the job function is called "+
					"(job-queue.ts:144-152) — the browser provably never clicked, so the row must return to "+
					"`failed` and stay re-approvable instead of wedging in `sending` for a human (criterion 26)",
					status, err, err)
			}
			if rejected.Status != status {
				t.Errorf("SendRejectedError.Status = %d, want %d", rejected.Status, status)
			}
		})
	}
}

// Criterion 26, second half: the line D7 does NOT move. A 500 may be thrown by
// browser work that has already pressed Send, and a broken response means the
// request WAS dispatched. Invariant 4 errs toward never-resend: a retry of a
// click that did land is a double-post into a client conversation.
func TestSend_500AndAMidResponseEOFStayAmbiguous(t *testing.T) {
	t.Run("500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"SLACK_UI_CHANGED"}`))
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
		var rejected *SendRejectedError
		if _, err := bridge.Send(context.Background(), sendTarget, "hi", 0); errors.As(err, &rejected) {
			t.Errorf("Send on HTTP 500 = *SendRejectedError; D7 moves 503 and 429 ONLY. A 500 can come from "+
				"browser work that already clicked: %v", err)
		}
	})

	t.Run("EOF mid-response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close() // request received, no response written
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
		var rejected *SendRejectedError
		if _, err := bridge.Send(context.Background(), sendTarget, "hi", 0); errors.As(err, &rejected) {
			t.Errorf("a failure AFTER dispatch = *SendRejectedError, want untyped/ambiguous: %v", err)
		}
	})
}

// Criterion 14's input: /export's 503 is a TYPED busy signal with the leaf's own
// Retry-After, so the loop sleeps for as long as the queue asked (bounded by the
// interval) instead of guessing. The leaf answers a second concurrent export
// with 503 + Retry-After because maxSweepDepth is 0 (bridge-server.ts:73) —
// this is the single most likely outcome of a targeted pass landing during a
// rotation, and it must never look like a failure.
func TestExport_503IsATypedBusySkip(t *testing.T) {
	const retryAfter = 45
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"bridge busy","retry_after_ms":45000}`))
	}))
	defer server.Close()
	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	_, err = bridge.Export(context.Background(), ExportRequest{BudgetMS: 150000, MaxConversations: 2})
	if err == nil {
		t.Fatal("Export against a 503 = nil error")
	}
	if !errors.Is(err, ErrBridgeBusy) {
		t.Fatalf("Export on 503 = %v (%T), want an error matching ErrBridgeBusy. Criterion 14: a busy browser "+
			"is a SKIPPED PASS, and it is the ordinary outcome of a targeted pass meeting the 12-15 minute "+
			"rotation", err, err)
	}
	var busy *BridgeBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("Export on 503 = %v, want a *BridgeBusyError carrying Retry-After", err)
	}
	if busy.RetryAfter != retryAfter*time.Second {
		t.Errorf("BridgeBusyError.RetryAfter = %s, want %ds from the leaf's own Retry-After header: guessing "+
			"when to come back is how a per-minute loop hammers a queue that already told us", busy.RetryAfter, retryAfter)
	}
}

// A 500 from /export is NOT a busy signal. Conflating them would make a broken
// leaf look like a busy one and hide it behind a polite sleep forever.
func TestExport_500IsNotBusy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"SLACK_UI_CHANGED"}`))
	}))
	defer server.Close()
	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
	if _, err := bridge.Export(context.Background(), ExportRequest{BudgetMS: 150000}); errors.Is(err, ErrBridgeBusy) {
		t.Errorf("Export on 500 matched ErrBridgeBusy (%v); only the queue's 503 means busy "+
			"(http-bridge.ts:267-273 is the only 503 in the leaf)", err)
	}
}
