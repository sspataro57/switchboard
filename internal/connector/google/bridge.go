package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const BridgeSchemaVersion = 1

type BridgeAccount struct {
	Alias string `json:"alias"`
	Email string `json:"email"`
}

type BridgeAccounts struct {
	SchemaVersion int             `json:"schema_version"`
	Accounts      []BridgeAccount `json:"accounts"`
}

type BridgeExportRequest struct {
	Account     string `json:"account"`
	AfterUnix   int64  `json:"after_unix"`
	MaxMessages int    `json:"max_messages"`
}

type BridgeExport struct {
	SchemaVersion     int               `json:"schema_version"`
	Account           BridgeAccount     `json:"account"`
	Messages          []json.RawMessage `json:"messages"`
	MaxInternalDateMS int64             `json:"max_internal_date_ms"`
}

type BridgeCalendarExportRequest struct {
	Account   string `json:"account"`
	SyncToken string `json:"sync_token,omitempty"`
	TimeMin   string `json:"time_min"`
	TimeMax   string `json:"time_max"`
	MaxEvents int    `json:"max_events"`
}

type BridgeCalendarExport struct {
	SchemaVersion int               `json:"schema_version"`
	Account       BridgeAccount     `json:"account"`
	Events        []json.RawMessage `json:"events"`
	NextSyncToken string            `json:"next_sync_token"`
	Reset         bool              `json:"reset"`
}

type BridgeSendRawRequest struct {
	FromEmail string `json:"from_email"`
	Raw       string `json:"raw"`
	ThreadID  string `json:"thread_id,omitempty"`
}

type BridgeSendRawResult struct {
	SchemaVersion int    `json:"schema_version"`
	MessageID     string `json:"message_id"`
	ThreadID      string `json:"thread_id"`
	// Pointer, not bool: an omitted field would decode to false and be read as
	// "definitely not sent", releasing the reservation. A version-skewed leaf
	// that DID send could then be re-approved into a duplicate email.
	Sent *bool `json:"sent"`
}

type bridgeCommandLimits struct {
	StdinBytes  int64
	StdoutBytes int64
	StderrBytes int64
}

var defaultBridgeCommandLimits = bridgeCommandLimits{
	StdinBytes: 32 << 20, StdoutBytes: 256 << 20, StderrBytes: 64 << 10,
}

type bridgeCommandRunner interface {
	Run(ctx context.Context, binary, operation string, stdin []byte, limits bridgeCommandLimits) (stdout, stderr []byte, err error)
}

type execBridgeCommandRunner struct{}

func (execBridgeCommandRunner) Run(ctx context.Context, binary, operation string, stdin []byte, limits bridgeCommandLimits) ([]byte, []byte, error) {
	if int64(len(stdin)) > limits.StdinBytes {
		return nil, nil, fmt.Errorf("bridge stdin exceeds %d bytes", limits.StdinBytes)
	}
	cmd := exec.CommandContext(ctx, binary, operation)
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := &bridgeCappedBuffer{limit: limits.StdoutBytes}
	stderr := &bridgeCappedBuffer{limit: limits.StderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stdout.overflow {
		return nil, stderr.Bytes(), fmt.Errorf("bridge stdout exceeds %d bytes", limits.StdoutBytes)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type bridgeCappedBuffer struct {
	bytes.Buffer
	limit    int64
	overflow bool
}

func (b *bridgeCappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - int64(b.Len())
	if remaining <= 0 {
		b.overflow = true
		return original, nil
	}
	if int64(len(p)) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

// BridgeNotRunError marks a failure that provably happened BEFORE the leaf
// could touch the network: rejected input, a marshal failure, an oversized
// request, or a binary that never started. On the send path these are DEFINITE
// non-sends, so the delivery reservation can safely be released; every other
// failure (the process ran) stays ambiguous and keeps the reservation.
type BridgeNotRunError struct {
	Op  string
	Err error
}

func (e *BridgeNotRunError) Error() string {
	return fmt.Sprintf("Gmail bridge %s never ran: %v", e.Op, e.Err)
}

func (e *BridgeNotRunError) Unwrap() error { return e.Err }

type CommandBridge struct {
	binary string
	runner bridgeCommandRunner
	limits bridgeCommandLimits
}

func NewCommandBridge(binary string) (*CommandBridge, error) {
	if binary == "" {
		return nil, fmt.Errorf("GMAIL_CONNECTOR_BRIDGE is not set")
	}
	if !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("GMAIL_CONNECTOR_BRIDGE must be an absolute path")
	}
	info, err := os.Stat(binary)
	if err != nil {
		return nil, fmt.Errorf("stat Gmail connector bridge: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Gmail connector bridge is not a regular file")
	}
	return &CommandBridge{binary: binary, runner: execBridgeCommandRunner{}, limits: defaultBridgeCommandLimits}, nil
}

func (b *CommandBridge) Accounts(ctx context.Context) (BridgeAccounts, error) {
	out, err := b.run(ctx, "accounts", nil)
	if err != nil {
		return BridgeAccounts{}, err
	}
	var result BridgeAccounts
	if err := json.Unmarshal(out, &result); err != nil {
		return BridgeAccounts{}, fmt.Errorf("parse Gmail bridge accounts: %w", err)
	}
	if err := validateBridgeVersion(result.SchemaVersion); err != nil {
		return BridgeAccounts{}, err
	}
	if len(result.Accounts) == 0 {
		return BridgeAccounts{}, fmt.Errorf("Gmail bridge returned no accounts")
	}
	seenAlias := map[string]bool{}
	seenEmail := map[string]bool{}
	for _, account := range result.Accounts {
		if strings.TrimSpace(account.Alias) == "" || strings.TrimSpace(account.Email) == "" {
			return BridgeAccounts{}, fmt.Errorf("Gmail bridge returned an account without alias or email")
		}
		alias := strings.ToLower(account.Alias)
		email := strings.ToLower(account.Email)
		if seenAlias[alias] || seenEmail[email] {
			return BridgeAccounts{}, fmt.Errorf("Gmail bridge returned duplicate account alias or email")
		}
		seenAlias[alias], seenEmail[email] = true, true
	}
	return result, nil
}

func (b *CommandBridge) Export(ctx context.Context, request BridgeExportRequest) (BridgeExport, error) {
	if strings.TrimSpace(request.Account) == "" || request.MaxMessages <= 0 {
		return BridgeExport{}, fmt.Errorf("Gmail bridge export requires account and positive max_messages")
	}
	in, err := json.Marshal(request)
	if err != nil {
		return BridgeExport{}, fmt.Errorf("marshal Gmail bridge export request: %w", err)
	}
	out, err := b.run(ctx, "export", in)
	if err != nil {
		return BridgeExport{}, err
	}
	var result BridgeExport
	if err := json.Unmarshal(out, &result); err != nil {
		return BridgeExport{}, fmt.Errorf("parse Gmail bridge export: %w", err)
	}
	if err := validateBridgeVersion(result.SchemaVersion); err != nil {
		return BridgeExport{}, err
	}
	if !strings.EqualFold(result.Account.Alias, request.Account) {
		return BridgeExport{}, fmt.Errorf("Gmail bridge returned account %q for requested account %q", result.Account.Alias, request.Account)
	}
	if result.Account.Email == "" {
		return BridgeExport{}, fmt.Errorf("Gmail bridge export account has no email")
	}
	if len(result.Messages) > request.MaxMessages {
		return BridgeExport{}, fmt.Errorf("Gmail bridge returned %d messages above requested maximum %d", len(result.Messages), request.MaxMessages)
	}
	return result, nil
}

func (b *CommandBridge) CalendarExport(ctx context.Context, request BridgeCalendarExportRequest) (BridgeCalendarExport, error) {
	if strings.TrimSpace(request.Account) == "" || request.MaxEvents <= 0 {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge calendar-export requires account and positive max_events")
	}
	timeMin, err := time.Parse(time.RFC3339, strings.TrimSpace(request.TimeMin))
	if err != nil {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge calendar-export time_min must be RFC3339: %w", err)
	}
	timeMax, err := time.Parse(time.RFC3339, strings.TrimSpace(request.TimeMax))
	if err != nil {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge calendar-export time_max must be RFC3339: %w", err)
	}
	if !timeMax.After(timeMin) || timeMax.Sub(timeMin) > 366*24*time.Hour {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge calendar-export window must be positive and at most 366 days")
	}
	in, err := json.Marshal(request)
	if err != nil {
		return BridgeCalendarExport{}, fmt.Errorf("marshal Gmail bridge calendar-export request: %w", err)
	}
	out, err := b.run(ctx, "calendar-export", in)
	if err != nil {
		return BridgeCalendarExport{}, err
	}
	var result BridgeCalendarExport
	if err := json.Unmarshal(out, &result); err != nil {
		return BridgeCalendarExport{}, fmt.Errorf("parse Gmail bridge calendar-export response: %w", err)
	}
	if err := validateBridgeVersion(result.SchemaVersion); err != nil {
		return BridgeCalendarExport{}, err
	}
	if !strings.EqualFold(result.Account.Alias, request.Account) {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge returned Calendar account %q for requested account %q", result.Account.Alias, request.Account)
	}
	if strings.TrimSpace(result.Account.Email) == "" {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge Calendar export account has no email")
	}
	if strings.TrimSpace(result.NextSyncToken) == "" {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge Calendar export has no next_sync_token")
	}
	if len(result.Events) > request.MaxEvents {
		return BridgeCalendarExport{}, fmt.Errorf("Gmail bridge returned %d Calendar events above requested maximum %d", len(result.Events), request.MaxEvents)
	}
	return result, nil
}

func (b *CommandBridge) SendRaw(ctx context.Context, request BridgeSendRawRequest) (BridgeSendRawResult, error) {
	if strings.TrimSpace(request.FromEmail) == "" || strings.TrimSpace(request.Raw) == "" {
		return BridgeSendRawResult{}, &BridgeNotRunError{Op: "send-raw",
			Err: fmt.Errorf("send-raw requires from_email and raw")}
	}
	in, err := json.Marshal(request)
	if err != nil {
		return BridgeSendRawResult{}, &BridgeNotRunError{Op: "send-raw", Err: err}
	}
	out, err := b.run(ctx, "send-raw", in)
	if err != nil {
		return BridgeSendRawResult{}, err
	}
	var result BridgeSendRawResult
	if err := json.Unmarshal(out, &result); err != nil {
		return BridgeSendRawResult{}, fmt.Errorf("parse Gmail bridge send response: %w", err)
	}
	if err := validateBridgeVersion(result.SchemaVersion); err != nil {
		return BridgeSendRawResult{}, err
	}
	if result.Sent == nil {
		// Ran, answered, but said nothing about whether it sent. Ambiguous.
		return BridgeSendRawResult{}, fmt.Errorf("Gmail bridge send response omitted sent")
	}
	if !*result.Sent {
		if result.MessageID != "" {
			// Contradictory: not sent, yet it names a message. Treat as
			// ambiguous rather than releasing the reservation on a guess.
			return BridgeSendRawResult{}, fmt.Errorf("Gmail bridge reported sent=false with message id %q", result.MessageID)
		}
		// An explicit, internally consistent refusal: definite non-send.
		return BridgeSendRawResult{}, &SendRejectedError{
			Body: "Gmail bridge reported the message was not sent"}
	}
	if result.MessageID == "" {
		// Claims sent but names nothing — genuinely ambiguous, stays untyped.
		return BridgeSendRawResult{}, fmt.Errorf("Gmail bridge reported a send with no message id")
	}
	return result, nil
}

func (b *CommandBridge) run(ctx context.Context, operation string, stdin []byte) ([]byte, error) {
	if int64(len(stdin)) > b.limits.StdinBytes {
		return nil, &BridgeNotRunError{Op: operation,
			Err: fmt.Errorf("stdin exceeds %d bytes", b.limits.StdinBytes)}
	}
	stdout, stderr, err := b.runner.Run(ctx, b.binary, operation, stdin, b.limits)
	if int64(len(stderr)) > b.limits.StderrBytes {
		stderr = stderr[:b.limits.StderrBytes]
	}
	if int64(len(stdout)) > b.limits.StdoutBytes {
		return nil, fmt.Errorf("Gmail bridge %s stdout exceeds %d bytes", operation, b.limits.StdoutBytes)
	}
	if err != nil {
		// A binary that could not be started (missing, not executable) never
		// reached the provider; an ExitError means it ran, which is ambiguous.
		// *exec.Error only. A *fs.PathError can in principle surface AFTER the
		// process started (a stdin-copy error reported by Wait), and calling
		// that a definite non-send is the one mistake invariant 4 cannot
		// tolerate.
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, &BridgeNotRunError{Op: operation, Err: err}
		}
		if len(stderr) > 0 {
			return nil, fmt.Errorf("Gmail bridge %s: %w: %s", operation, err, bytes.TrimSpace(stderr))
		}
		return nil, fmt.Errorf("Gmail bridge %s: %w", operation, err)
	}
	return stdout, nil
}

func validateBridgeVersion(got int) error {
	if got != BridgeSchemaVersion {
		return fmt.Errorf("unsupported Gmail bridge schema_version %d (want %d)", got, BridgeSchemaVersion)
	}
	return nil
}
