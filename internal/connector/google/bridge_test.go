package google

// Contract tests for docs/tickets/gmail-local-bridge_SPEC.md.
//
// GREENFIELD NOTE: bridge.go does not exist yet. These tests intentionally
// compile-fail until it supplies the following narrow surface:
//
//   const BridgeSchemaVersion = 1
//   type BridgeAccount struct { Alias, Email string }
//   type BridgeAccounts struct { SchemaVersion int; Accounts []BridgeAccount }
//   type BridgeExportRequest struct { Account string; AfterUnix int64; MaxMessages int }
//   type BridgeExport struct {
//       SchemaVersion int; Account BridgeAccount
//       Messages []json.RawMessage; MaxInternalDateMS int64
//   }
//   type BridgeSendRawRequest struct { FromEmail, Raw, ThreadID string }
//   type BridgeSendRawResult struct { SchemaVersion int; MessageID, ThreadID string; Sent bool }
//   type CommandBridge struct { binary string; runner bridgeCommandRunner }
//   func NewCommandBridge(binary string) (*CommandBridge, error)
//   func (*CommandBridge) Accounts(context.Context) (BridgeAccounts, error)
//   func (*CommandBridge) Export(context.Context, BridgeExportRequest) (BridgeExport, error)
//   func (*CommandBridge) SendRaw(context.Context, BridgeSendRawRequest) (BridgeSendRawResult, error)
//
// bridgeCommandRunner is the no-subprocess unit seam. Its separate binary and
// operation arguments make shell invocation unnecessary; bridgeCommandLimits
// pins finite stdin/stdout/stderr limits for the production exec runner.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type bridgeRunnerFunc func(context.Context, string, string, []byte, bridgeCommandLimits) ([]byte, []byte, error)

func (f bridgeRunnerFunc) Run(
	ctx context.Context,
	binary string,
	operation string,
	stdin []byte,
	limits bridgeCommandLimits,
) ([]byte, []byte, error) {
	return f(ctx, binary, operation, stdin, limits)
}

func newProtocolBridge(t *testing.T, run bridgeRunnerFunc) (*CommandBridge, string) {
	t.Helper()
	// Spaces and shell metacharacters are deliberate. The bridge must pass this
	// exact regular-file path directly to exec, never interpolate it into a shell.
	binary := filepath.Join(t.TempDir(), "gmail bridge;still-one-argument")
	if err := os.WriteFile(binary, []byte("test placeholder"), 0o700); err != nil {
		t.Fatalf("write placeholder bridge binary: %v", err)
	}
	b, err := NewCommandBridge(binary)
	if err != nil {
		t.Fatalf("NewCommandBridge: %v", err)
	}
	b.runner = run
	return b, binary
}

func requireFiniteBridgeLimits(t *testing.T, limits bridgeCommandLimits) {
	t.Helper()
	if limits.StdinBytes <= 0 || limits.StdoutBytes <= 0 || limits.StderrBytes <= 0 {
		t.Fatalf("bridge limits must all be finite and positive: %+v", limits)
	}
}

func TestNewCommandBridge_AcceptsOnlyAbsoluteRegularFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	regular := filepath.Join(tmp, "gmail-switchboard")
	if err := os.WriteFile(regular, []byte("placeholder"), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{name: "absolute regular file", path: regular, ok: true},
		{name: "empty", path: ""},
		{name: "relative", path: "bin/gmail-switchboard"},
		{name: "directory", path: tmp},
		{name: "missing", path: filepath.Join(tmp, "missing")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCommandBridge(tc.path)
			if tc.ok && err != nil {
				t.Fatalf("NewCommandBridge(%q): %v", tc.path, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("NewCommandBridge(%q) accepted a non-absolute/non-regular path", tc.path)
			}
		})
	}
}

func TestCommandBridge_AccountsMapsVersionedProtocolWithoutShell(t *testing.T) {
	wantJSON := `{"schema_version":1,"accounts":[{"alias":"personal","email":"me@example.com"},{"alias":"work-main","email":"me@corp.example"}]}`
	var wantBinary string
	b, binary := newProtocolBridge(t, func(_ context.Context, gotBinary, operation string, stdin []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
		requireFiniteBridgeLimits(t, limits)
		if gotBinary != wantBinary {
			t.Errorf("binary = %q, want exact path %q", gotBinary, wantBinary)
		}
		if operation != "accounts" {
			t.Errorf("operation = %q, want accounts", operation)
		}
		if len(stdin) != 0 {
			t.Errorf("accounts stdin = %q, want empty", stdin)
		}
		return []byte(wantJSON), nil, nil
	})
	wantBinary = binary

	got, err := b.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if got.SchemaVersion != BridgeSchemaVersion || len(got.Accounts) != 2 {
		t.Fatalf("Accounts = %+v", got)
	}
	if got.Accounts[0].Alias != "personal" || got.Accounts[0].Email != "me@example.com" {
		t.Errorf("first account = %+v", got.Accounts[0])
	}
}

func TestCommandBridge_ExportMapsRequestAndPreservesExactGmailJSON(t *testing.T) {
	raw := json.RawMessage(`{"id":"gm-1","threadId":"th-1","internalDate":"1784995200123","payload":{"headers":[{"name":"Message-ID","value":"<gm-1@example>"}]}}`)
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, operation string, stdin []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
		requireFiniteBridgeLimits(t, limits)
		if operation != "export" {
			t.Fatalf("operation = %q, want export", operation)
		}
		var request BridgeExportRequest
		if err := json.Unmarshal(stdin, &request); err != nil {
			t.Fatalf("decode export request: %v", err)
		}
		want := BridgeExportRequest{Account: "work-main", AfterUnix: 1784991600, MaxMessages: 321}
		if request != want {
			t.Errorf("export request = %+v, want %+v", request, want)
		}
		response := `{"schema_version":1,"account":{"alias":"work-main","email":"me@corp.example"},"messages":[` + string(raw) + `],"max_internal_date_ms":1784995200123}`
		return []byte(response), nil, nil
	})

	got, err := b.Export(context.Background(), BridgeExportRequest{
		Account: "work-main", AfterUnix: 1784991600, MaxMessages: 321,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got.Account.Alias != "work-main" || got.Account.Email != "me@corp.example" {
		t.Errorf("export account = %+v", got.Account)
	}
	if got.MaxInternalDateMS != 1784995200123 {
		t.Errorf("max_internal_date_ms = %d", got.MaxInternalDateMS)
	}
	if len(got.Messages) != 1 || string(got.Messages[0]) != string(raw) {
		t.Errorf("raw message changed across bridge:\n got %s\nwant %s", firstRaw(got.Messages), raw)
	}
}

func TestCommandBridge_ExportRejectsWrongAccountResponse(t *testing.T) {
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
		return []byte(`{"schema_version":1,"account":{"alias":"work-sales","email":"sales@corp.example"},"messages":[],"max_internal_date_ms":0}`), nil, nil
	})

	_, err := b.Export(context.Background(), BridgeExportRequest{Account: "personal", AfterUnix: 1, MaxMessages: 10})
	if err == nil {
		t.Fatal("Export accepted observations returned for a different account alias")
	}
}

func TestCommandBridge_SendRawMapsRequestAndResponse(t *testing.T) {
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, operation string, stdin []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
		requireFiniteBridgeLimits(t, limits)
		if operation != "send-raw" {
			t.Fatalf("operation = %q, want send-raw", operation)
		}
		var request BridgeSendRawRequest
		if err := json.Unmarshal(stdin, &request); err != nil {
			t.Fatalf("decode send-raw request: %v", err)
		}
		want := BridgeSendRawRequest{FromEmail: "me@corp.example", Raw: "cmF3LW1pbWU", ThreadID: "thread-9"}
		if request != want {
			t.Errorf("send-raw request = %+v, want %+v", request, want)
		}
		return []byte(`{"schema_version":1,"message_id":"gmail-api-9","thread_id":"thread-9","sent":true}`), nil, nil
	})

	got, err := b.SendRaw(context.Background(), BridgeSendRawRequest{
		FromEmail: "me@corp.example", Raw: "cmF3LW1pbWU", ThreadID: "thread-9",
	})
	if err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	if got.MessageID != "gmail-api-9" || got.ThreadID != "thread-9" || got.Sent == nil || !*got.Sent {
		t.Errorf("SendRaw = %+v", got)
	}
}

func TestBridgeSenderPassesExistingMIMEThroughUnchanged(t *testing.T) {
	var request BridgeSendRawRequest
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, operation string, stdin []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
		if operation != "send-raw" {
			t.Fatalf("operation = %q", operation)
		}
		if err := json.Unmarshal(stdin, &request); err != nil {
			t.Fatal(err)
		}
		return []byte(`{"schema_version":1,"message_id":"gmail-id","thread_id":"thread-1","sent":true}`), nil, nil
	})
	mime := []byte("From: me@example.com\r\nMessage-ID: <sb-1@example.com>\r\n\r\nbody\r\n")
	sender := &BridgeSender{Bridge: b}
	id, err := sender.Send(context.Background(), "me@example.com", mime, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "gmail-id" || request.FromEmail != "me@example.com" || request.ThreadID != "thread-1" {
		t.Fatalf("send result id=%q request=%+v", id, request)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(request.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(mime) {
		t.Fatalf("MIME changed across bridge:\n got %q\nwant %q", decoded, mime)
	}
}

func TestCommandBridge_ValidatesSchemaVersionForEveryOperation(t *testing.T) {
	tests := []struct {
		name     string
		response string
		call     func(*CommandBridge) error
	}{
		{
			name: "accounts", response: `{"schema_version":2,"accounts":[]}`,
			call: func(b *CommandBridge) error { _, err := b.Accounts(context.Background()); return err },
		},
		{
			name: "export", response: `{"schema_version":2,"account":{"alias":"personal","email":"me@example.com"},"messages":[]}`,
			call: func(b *CommandBridge) error {
				_, err := b.Export(context.Background(), BridgeExportRequest{Account: "personal", MaxMessages: 1})
				return err
			},
		},
		{
			name: "send-raw", response: `{"schema_version":2,"message_id":"m","thread_id":"t","sent":true}`,
			call: func(b *CommandBridge) error {
				_, err := b.SendRaw(context.Background(), BridgeSendRawRequest{FromEmail: "me@example.com", Raw: "cmF3"})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
				requireFiniteBridgeLimits(t, limits)
				return []byte(tc.response), nil, nil
			})
			err := tc.call(b)
			if err == nil || !strings.Contains(err.Error(), "schema_version") {
				t.Fatalf("call error = %v, want unsupported schema_version", err)
			}
		})
	}
}

func TestCommandBridge_BoundsStderrOnRunnerFailure(t *testing.T) {
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
		requireFiniteBridgeLimits(t, limits)
		return nil, []byte(strings.Repeat("secret-looking-error ", int(limits.StderrBytes)+1)), errors.New("exit status 1")
	})

	_, err := b.Accounts(context.Background())
	if err == nil {
		t.Fatal("Accounts succeeded after runner failure")
	}
	// The bridge may include a useful stderr prefix, but never the unbounded
	// subprocess stream in the returned/loggable error.
	if int64(len(err.Error())) > defaultBridgeCommandLimits.StderrBytes+1024 {
		t.Fatalf("runner error was not stderr-bounded: %d bytes", len(err.Error()))
	}
}

func TestCommandBridge_RejectsOversizedStdinBeforeRunner(t *testing.T) {
	called := false
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
		called = true
		return nil, nil, nil
	})
	b.limits.StdinBytes = 32
	_, err := b.SendRaw(context.Background(), BridgeSendRawRequest{
		FromEmail: "me@example.com", Raw: strings.Repeat("x", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
		t.Fatalf("oversized stdin error = %v", err)
	}
	if called {
		t.Fatal("runner was called with oversized stdin")
	}
}

func TestCommandBridge_RejectsOversizedStdoutFromRunner(t *testing.T) {
	b, _ := newProtocolBridge(t, func(_ context.Context, _ string, _ string, _ []byte, _ bridgeCommandLimits) ([]byte, []byte, error) {
		return []byte(strings.Repeat("x", 65)), nil, nil
	})
	b.limits.StdoutBytes = 64
	_, err := b.Accounts(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stdout exceeds") {
		t.Fatalf("oversized stdout error = %v", err)
	}
}

func firstRaw(messages []json.RawMessage) string {
	if len(messages) == 0 {
		return "<none>"
	}
	return string(messages[0])
}
