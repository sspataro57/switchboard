package google_test

// Unit tests for the Gmail send adapter (SPEC 08-draft-deliveries, criterion 5
// + invariants 4/5/6). NO build tag: compiles into the default `go test` binary
// alongside fake_google_test.go and poller_test.go — so symbol names here are
// unique (fakeSend*, decodeRawB64) to avoid colliding with that shared fake.
// NEVER a live Google call: BuildOutboundMIME is pure and GmailSender.Send is
// exercised against a local httptest server.
//
// GREENFIELD NOTE: internal/connector/google/send.go does not exist yet, so this
// file compile-FAILs under `go test ./...` until it is written. Imposed exported
// surface (send.go — the SPEC's "pure BuildOutboundMIME(...) + GmailSender{hc,
// baseURL} posting {raw, threadId}"):
//
//   // OutboundMessage is the header/body material the send_delivery handler
//   // resolved deterministically (From from the thread's mailbox — never
//   // model-chosen; To = last inbound sender; In-Reply-To = last message-id;
//   // References = the thread's message-id chain in sent order; MessageID = the
//   // self-chosen <sb-...> id persisted BEFORE the send).
//   type OutboundMessage struct {
//       From, To, Subject, Body, MessageID, InReplyTo string
//       References []string
//       Date       time.Time
//   }
//
//   // BuildOutboundMIME assembles a text/plain UTF-8 RFC 2822 message and scrubs
//   // AI attribution (Co-Authored-By / "Generated with ...") from the body.
//   func BuildOutboundMIME(msg OutboundMessage) ([]byte, error)
//
//   // GmailSender posts {raw: base64url(mime), threadId} to
//   // /gmail/v1/users/{userID}/messages/send (baseURL injectable, like the
//   // read client). Returns the API message id; wraps a non-2xx response.
//   type GmailSender struct { /* hc, baseURL */ }
//   func NewGmailSender(hc *http.Client, baseURL string) *GmailSender
//   func (s *GmailSender) Send(ctx context.Context, userID string, rawMIME []byte, threadID string) (id string, err error)

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/google"
)

const (
	sendFrom    = "salvo@example.com"
	sendTo      = "client@acme.example"
	sendSubject = "Re: staging login"
	sendMsgID   = "<sb-700-abcdef@example.com>"
	sendParent1 = "<first@acme.example>"
	sendParent2 = "<second@acme.example>"
	sendThread  = "gmail-thread-123"
)

func sampleOutbound(body string) google.OutboundMessage {
	return google.OutboundMessage{
		From:       sendFrom,
		To:         sendTo,
		Subject:    sendSubject,
		Body:       body,
		MessageID:  sendMsgID,
		InReplyTo:  sendParent2,                        // last message in the thread
		References: []string{sendParent1, sendParent2}, // chain in sent order
		Date:       time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
	}
}

// ---- BuildOutboundMIME: headers + scrub --------------------------------------

func TestBuildOutboundMIME_HeadersAndThreading(t *testing.T) {
	raw, err := google.BuildOutboundMIME(sampleOutbound("Thanks — pushed the fix to staging."))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("assembled bytes are not a parseable RFC 2822 message: %v\n%s", err, raw)
	}
	h := msg.Header

	// From is the thread's account email — NEVER model-chosen (invariant 6).
	if got := addrOnly(t, h.Get("From")); got != sendFrom {
		t.Errorf("From = %q, want %q (inherited from the thread, never model-chosen)", got, sendFrom)
	}
	if got := addrOnly(t, h.Get("To")); got != sendTo {
		t.Errorf("To = %q, want %q (last inbound sender)", got, sendTo)
	}
	if got := h.Get("Subject"); got != sendSubject {
		t.Errorf("Subject = %q, want %q", got, sendSubject)
	}
	// Self-chosen Message-ID header (invariant 5 seam) equals the persisted id.
	if got := h.Get("Message-ID"); got != sendMsgID {
		t.Errorf("Message-ID = %q, want the self-chosen id %q", got, sendMsgID)
	}
	if got := h.Get("In-Reply-To"); got != sendParent2 {
		t.Errorf("In-Reply-To = %q, want the last thread message-id %q", got, sendParent2)
	}
	refs := h.Get("References")
	for _, want := range []string{sendParent1, sendParent2} {
		if !strings.Contains(refs, want) {
			t.Errorf("References %q missing %q (full chain in sent order)", refs, want)
		}
	}
	if strings.Index(refs, sendParent1) > strings.Index(refs, sendParent2) {
		t.Errorf("References order wrong: %q must precede %q in %q", sendParent1, sendParent2, refs)
	}
	if ct := h.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "text/plain") ||
		!strings.Contains(strings.ToLower(ct), "utf-8") {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if h.Get("Date") == "" {
		t.Errorf("Date header missing")
	}
}

func TestBuildOutboundMIME_ScrubsAIAttribution(t *testing.T) {
	// A body carrying attribution trailers must be scrubbed by the adapter
	// (belt-and-suspenders with the draft-write scrub — invariant 6).
	body := "Fix is live on staging.\n\n" +
		"Co-Authored-By: Claude <noreply@anthropic.com>\n" +
		"Generated with Claude Code\n"
	raw, err := google.BuildOutboundMIME(sampleOutbound(body))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	assembled := string(raw)
	for _, banned := range []string{"Co-Authored-By", "Generated with", "Claude", "anthropic"} {
		if strings.Contains(assembled, banned) {
			t.Errorf("assembled message still contains AI-attribution marker %q — adapter must scrub it\n%s", banned, assembled)
		}
	}
	// The legitimate content survives.
	if !strings.Contains(assembled, "Fix is live on staging.") {
		t.Errorf("scrub removed legitimate body content:\n%s", assembled)
	}
}

// ---- GmailSender.Send: POST raw + threadId -----------------------------------

type fakeSendServer struct {
	srv       *httptest.Server
	gotUser   string
	gotRaw    []byte
	gotThread string
	status    int // response status; 0 => 200
	calls     int
}

func newFakeSendServer() *fakeSendServer {
	f := &fakeSendServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		// path: /gmail/v1/users/{user}/messages/send
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages/send") {
			http.Error(w, "unexpected route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		f.gotUser = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/"), "/messages/send")
		var body struct {
			Raw      string `json:"raw"`
			ThreadID string `json:"threadId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.gotThread = body.ThreadID
		f.gotRaw = decodeRawB64(body.Raw)
		code := f.status
		if code == 0 {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gmail-msg-xyz", "threadId": body.ThreadID})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "boom"}})
		}
	}))
	return f
}

func (f *fakeSendServer) close() { f.srv.Close() }

func decodeRawB64(s string) []byte {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b
		}
	}
	return nil
}

func TestGmailSender_Send_PostsRawWithThreadID(t *testing.T) {
	f := newFakeSendServer()
	defer f.close()

	raw, err := google.BuildOutboundMIME(sampleOutbound("hello"))
	if err != nil {
		t.Fatalf("BuildOutboundMIME: %v", err)
	}
	sender := google.NewGmailSender(http.DefaultClient, f.srv.URL)
	id, err := sender.Send(context.Background(), sendFrom, raw, sendThread)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "gmail-msg-xyz" {
		t.Errorf("Send returned id %q, want the API message id gmail-msg-xyz", id)
	}
	if f.calls != 1 {
		t.Errorf("send calls = %d, want 1", f.calls)
	}
	if f.gotUser != sendFrom {
		t.Errorf("posted to user %q, want %q (userID path segment)", f.gotUser, sendFrom)
	}
	if f.gotThread != sendThread {
		t.Errorf("posted threadId = %q, want %q", f.gotThread, sendThread)
	}
	if len(f.gotRaw) == 0 {
		t.Fatalf("server received no decodable base64url raw body")
	}
	// The posted raw round-trips to the MIME with the self-chosen Message-ID.
	msg, err := mail.ReadMessage(strings.NewReader(string(f.gotRaw)))
	if err != nil {
		t.Fatalf("posted raw is not a parseable message: %v", err)
	}
	if got := msg.Header.Get("Message-ID"); got != sendMsgID {
		t.Errorf("posted Message-ID = %q, want %q", got, sendMsgID)
	}
}

func TestGmailSender_Send_NonOKWrapsError(t *testing.T) {
	f := newFakeSendServer()
	f.status = http.StatusInternalServerError
	defer f.close()

	sender := google.NewGmailSender(http.DefaultClient, f.srv.URL)
	_, err := sender.Send(context.Background(), sendFrom, []byte("raw"), sendThread)
	if err == nil {
		t.Fatalf("Send: want an error on a non-2xx response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("Send error = %q, want it to carry the HTTP status", err)
	}
}

// addrOnly extracts the bare address from a possibly display-name header.
func addrOnly(t *testing.T, header string) string {
	t.Helper()
	if header == "" {
		return ""
	}
	a, err := mail.ParseAddress(header)
	if err != nil {
		return header // let the caller's equality check report the mismatch
	}
	return a.Address
}
