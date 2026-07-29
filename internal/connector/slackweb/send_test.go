package slackweb

// Unit tests for the SWT-12 bridge `send` operation on BOTH transports
// (slack-send-promotion SPEC criteria 6 + 9). ZERO Slack, zero browser: the
// command bridge runs a shell stub (stubBridge / shellQuote from bridge_test.go)
// and the HTTP bridge talks to an httptest server.
//
// GREENFIELD NOTE: `Send` does not exist on either bridge yet, so this file
// compile-FAILs until it does — the expected red state.
//
// Contract the SPEC pins (criterion 6): the seam is
// `Send(ctx, targetURL, text string) error` and "the Go side rejects any send
// result that is not exactly {drafted:false, sent:true}".
//
// IMPOSED (the SPEC leaves the exact injection shape open, exactly as SWT-9 did
// for JiraSender): criterion 9 requires the delivery handler to tell a DEFINITE
// bridge refusal (HTTP 4xx — unauthorized, writes/unattended disabled,
// validation; the request never reached the click) apart from an AMBIGUOUS one
// (HTTP 5xx, transport error, context timeout — the click MAY have landed). The
// Go client cannot see a status through an untyped error, so this mirrors the
// gmail precedent (`google.SendRejectedError`, send.go:155) verbatim:
//
//	// SendRejectedError is a DEFINITE bridge refusal: the send did not happen,
//	// so the failed->approved retry path is safe to reopen.
//	type SendRejectedError struct {
//		Status int
//		Body   string
//	}
//	func (e *SendRejectedError) Error() string
//
// HTTPBridge.Send returns *SendRejectedError for 4xx and leaves 5xx/transport
// errors untyped — invariant 4 errs toward never-resend.
//
// DELIBERATE REVERSAL (SPEC criterion 4 + named consequence 7): http_bridge.go's
// doc comment currently promises "there is deliberately no send route on either
// side". This ticket revises that comment on purpose. What must NOT change is
// Draft: `send` is its OWN method and route, never a relaxation of the
// never-sends guard. TestSendIsNotARelaxationOfDraft pins that separation, and
// bridge_test.go's TestCommandBridgeDraftRejectsSent /
// TestHTTPBridgeDraftRejectsClaimedSend stay untouched.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sendTarget = "https://app.slack.com/client/T1/C1"

// ---- CommandBridge.Send --------------------------------------------------------

func TestCommandBridgeSendAcceptsOnlyExactSendResult(t *testing.T) {
	bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"drafted":false,"target_url":"`+sendTarget+`","sent":true}`))
	if err := bridge.Send(context.Background(), sendTarget, "hi"); err != nil {
		t.Fatalf("Send({drafted:false,sent:true}) = %v, want nil", err)
	}
}

func TestCommandBridgeSendRejectsAnythingElse(t *testing.T) {
	// The Go-side backstop: the leaf must report a completed send and nothing
	// else. A "drafted" result on the send path means the composer was populated
	// and the click never happened — recording that as sent would put a delivery
	// in 'sent' with no message in Slack.
	cases := map[string]string{
		"not sent":             `{"drafted":false,"sent":false}`,
		"drafted instead":      `{"drafted":true,"sent":false}`,
		"drafted and sent":     `{"drafted":true,"sent":true}`,
		"empty object":         `{}`,
		"not an object at all": `[]`,
	}
	for name, payload := range cases {
		payload := payload
		t.Run(name, func(t *testing.T) {
			bridge := stubBridge(t, "printf '%s' "+shellQuote(payload))
			if err := bridge.Send(context.Background(), sendTarget, "hi"); err == nil {
				t.Fatalf("Send accepted %s; only {drafted:false, sent:true} is a send", payload)
			}
		})
	}
}

func TestCommandBridgeSendRequiresTargetAndText(t *testing.T) {
	bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"drafted":false,"sent":true}`))
	if err := bridge.Send(context.Background(), "", "hi"); err == nil {
		t.Fatal("Send accepted an empty target URL")
	}
	if err := bridge.Send(context.Background(), sendTarget, ""); err == nil {
		t.Fatal("Send accepted empty text")
	}
}

func TestCommandBridgeSendPassesSendArgvAndRequestOnStdin(t *testing.T) {
	// The operation argument must be `send` (not `draft`) and the JSON request
	// must reach the leaf intact: a silently empty stdin would click nothing and
	// still report success.
	bridge := stubBridge(t, `
read -r line
case "$1:$line" in
  send:*app.slack.com/client/T1/C1*hello*) printf '{"drafted":false,"sent":true}' ;;
  *) printf 'bad argv or stdin: %s %s' "$1" "$line" >&2; exit 3 ;;
esac
`)
	if err := bridge.Send(context.Background(), sendTarget, "hello"); err != nil {
		t.Fatalf("Send = %v, want the stub to have seen argv 'send' and the JSON request", err)
	}
}

func TestCommandBridgeSendSurfacesStderrOnFailure(t *testing.T) {
	bridge := stubBridge(t, `printf 'SLACK_CONNECTOR_UNATTENDED_SEND is not enabled' >&2; exit 1`)
	err := bridge.Send(context.Background(), sendTarget, "hi")
	if err == nil || !strings.Contains(err.Error(), "UNATTENDED_SEND") {
		t.Fatalf("Send error = %v, want the leaf's stderr surfaced", err)
	}
}

// ---- HTTPBridge.Send -----------------------------------------------------------

func TestHTTPBridgeSendPostsToSendRouteWithBearer(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"drafted":false,"target_url":"` + sendTarget + `","sent":true}`))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Send(context.Background(), sendTarget, "hello"); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if gotPath != "/send" {
		t.Errorf("path = %q, want /send (its OWN route, not /draft)", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("authorization header = %q, want the bridge bearer token", gotAuth)
	}
	if gotBody["target_url"] != sendTarget || gotBody["text"] != "hello" {
		t.Errorf("body = %v, want {target_url, text} verbatim", gotBody)
	}
}

func TestHTTPBridgeSendRejectsAnythingButAnExactSendResult(t *testing.T) {
	cases := map[string]string{
		"not sent":         `{"drafted":false,"sent":false}`,
		"drafted instead":  `{"drafted":true,"sent":false}`,
		"drafted and sent": `{"drafted":true,"sent":true}`,
		"empty object":     `{}`,
	}
	for name, payload := range cases {
		payload := payload
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
			if err := bridge.Send(context.Background(), sendTarget, "hi"); err == nil {
				t.Fatalf("Send accepted %s; only {drafted:false, sent:true} is a send", payload)
			}
		})
	}
}

// Criterion 9, first half: a 4xx is DEFINITE — the leaf refused before the
// click (no token, writes disabled, unattended disabled, validation). The
// delivery handler must be able to see that and reopen failed->approved, so the
// error is typed.
func TestHTTPBridgeSendClassifies4xxAsDefiniteRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"writes_disabled"}`))
			}))
			defer server.Close()
			bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

			err := bridge.Send(context.Background(), sendTarget, "hi")
			if err == nil {
				t.Fatalf("Send on HTTP %d = nil error", status)
			}
			var rejected *SendRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("Send on HTTP %d = %v (%T), want *SendRejectedError — a 4xx never reached the "+
					"click, so the delivery must be re-approvable (criterion 9)", status, err, err)
			}
			if rejected.Status != status {
				t.Errorf("SendRejectedError.Status = %d, want %d", rejected.Status, status)
			}
		})
	}
}

// Criterion 9, second half: a 5xx or a transport error is AMBIGUOUS — the click
// may have landed. It must NOT be typed as a definite rejection, or the row
// becomes re-approvable and a retry can double-post into a client channel.
func TestHTTPBridgeSendLeaves5xxAndTransportErrorsAmbiguous(t *testing.T) {
	t.Run("500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"SLACK_UI_CHANGED"}`))
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

		err := bridge.Send(context.Background(), sendTarget, "hi")
		if err == nil {
			t.Fatal("Send on HTTP 500 = nil error")
		}
		var rejected *SendRejectedError
		if errors.As(err, &rejected) {
			t.Fatalf("Send on HTTP 500 = *SendRejectedError; a 500 may have happened AFTER the click, so "+
				"it must stay ambiguous and untyped (criterion 9 / named consequence 2). err=%v", err)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())
		server.Close() // nothing listening: a transport error, not a status

		err := bridge.Send(context.Background(), sendTarget, "hi")
		if err == nil {
			t.Fatal("Send against a closed bridge = nil error")
		}
		var rejected *SendRejectedError
		if errors.As(err, &rejected) {
			t.Fatalf("transport error = *SendRejectedError, want untyped/ambiguous: %v", err)
		}
	})

	t.Run("context timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"drafted":false,"sent":true}`))
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := bridge.Send(ctx, sendTarget, "hi")
		if err == nil {
			t.Fatal("Send with a cancelled context = nil error")
		}
		var rejected *SendRejectedError
		if errors.As(err, &rejected) {
			t.Fatalf("cancelled context = *SendRejectedError, want untyped/ambiguous: %v", err)
		}
	})
}

func TestHTTPBridgeSendRequiresTargetAndText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Send must validate its arguments before calling the bridge")
		_, _ = w.Write([]byte(`{"drafted":false,"sent":true}`))
	}))
	defer server.Close()
	bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

	if err := bridge.Send(context.Background(), "", "hi"); err == nil {
		t.Error("Send accepted an empty target URL")
	}
	if err := bridge.Send(context.Background(), sendTarget, ""); err == nil {
		t.Error("Send accepted empty text")
	}
}

// ---- send is not a relaxation of draft -----------------------------------------

// This diff deliberately reverses a written "there must never be a send here"
// guarantee for `send`. The test suite is what stops that reversal leaking into
// `draft`: the two operations must hit different routes/argv and keep opposite
// result guards, so a leaf that confuses them fails loudly on both sides.
func TestSendIsNotARelaxationOfDraft(t *testing.T) {
	t.Run("HTTP: distinct routes, opposite guards", func(t *testing.T) {
		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			// Answer every route with a SEND result. /send must accept it;
			// /draft must refuse it (the never-sends guard).
			_, _ = w.Write([]byte(`{"drafted":false,"sent":true}`))
		}))
		defer server.Close()
		bridge, _ := NewHTTPBridge(server.URL, testToken, server.Client())

		if err := bridge.Send(context.Background(), sendTarget, "hi"); err != nil {
			t.Fatalf("Send = %v, want nil", err)
		}
		if err := bridge.Draft(context.Background(), sendTarget, "hi"); err == nil {
			t.Fatal("Draft accepted a send result; the never-sends guard regressed into the draft path")
		}
		if len(paths) != 2 || paths[0] != "/send" || paths[1] != "/draft" {
			t.Fatalf("routes hit = %v, want [/send /draft] (send is its own route)", paths)
		}
	})

	t.Run("command: distinct argv, opposite guards", func(t *testing.T) {
		// One stub answering both operations with a send result.
		bridge := stubBridge(t, "printf '%s' "+shellQuote(`{"drafted":false,"sent":true}`))
		if err := bridge.Send(context.Background(), sendTarget, "hi"); err != nil {
			t.Fatalf("Send = %v, want nil", err)
		}
		if err := bridge.Draft(context.Background(), sendTarget, "hi"); err == nil {
			t.Fatal("Draft accepted a send result; the never-sends guard regressed into the draft path")
		}
	})
}
