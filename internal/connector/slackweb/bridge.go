package slackweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

const maxBridgeOutputBytes = 64 << 20

type CommandBridge struct {
	node   string
	script string
}

func NewCommandBridge(node, script string) (*CommandBridge, error) {
	if node == "" {
		node = "node"
	}
	if script == "" {
		return nil, fmt.Errorf("SLACK_WEB_BRIDGE_SCRIPT is not set")
	}
	if !filepath.IsAbs(script) {
		return nil, fmt.Errorf("SLACK_WEB_BRIDGE_SCRIPT must be an absolute path")
	}
	info, err := os.Stat(script)
	if err != nil {
		return nil, fmt.Errorf("stat Slack bridge script: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Slack bridge script is not a regular file")
	}
	return &CommandBridge{node: node, script: script}, nil
}

func (b *CommandBridge) Export(ctx context.Context) (Export, error) {
	out, err := b.run(ctx, "export", nil)
	if err != nil {
		return Export{}, err
	}
	var exported Export
	if err := json.Unmarshal(out, &exported); err != nil {
		return Export{}, fmt.Errorf("parse Slack bridge export: %w", err)
	}
	if exported.SchemaVersion != SchemaVersion {
		return Export{}, fmt.Errorf("unsupported Slack bridge schema_version %d", exported.SchemaVersion)
	}
	return exported, nil
}

func (b *CommandBridge) Draft(ctx context.Context, targetURL, text string) error {
	if targetURL == "" || text == "" {
		return fmt.Errorf("Slack draft requires target URL and text")
	}
	in, err := json.Marshal(map[string]string{"target_url": targetURL, "text": text})
	if err != nil {
		return fmt.Errorf("marshal Slack draft request: %w", err)
	}
	out, err := b.run(ctx, "draft", in)
	if err != nil {
		return err
	}
	var result struct {
		Drafted bool `json:"drafted"`
		Sent    bool `json:"sent"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("parse Slack draft result: %w", err)
	}
	if !result.Drafted || result.Sent {
		return fmt.Errorf("Slack bridge returned an unsafe draft result")
	}
	return nil
}

// Send clicks Send through the local connector process. Like the HTTP bridge it
// returns nothing on success: a browser click reserves no message id, so the
// export's body-prefix matcher stamps sent_external_id later.
//
// The subprocess transport cannot distinguish a pre-click refusal from a
// post-click crash the way an HTTP status can, so only two things are DEFINITE
// here: input this method rejected itself, and a leaf that answered sent:false.
// A non-zero exit stays untyped and therefore ambiguous.
func (b *CommandBridge) Send(ctx context.Context, targetURL, text string) error {
	if targetURL == "" || text == "" {
		return &SendRejectedError{Body: "Slack send requires target URL and text"}
	}
	in, err := json.Marshal(map[string]string{"target_url": targetURL, "text": text})
	if err != nil {
		return &SendRejectedError{Body: fmt.Sprintf("marshal Slack send request: %v", err)}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &SendRejectedError{Body: "context already done before dispatch: " + ctxErr.Error()}
	}
	out, err := b.run(ctx, "send", in)
	if err != nil {
		// A process that never started cannot have clicked — same reasoning as the
		// HTTP bridge's dial failure, and the same classification the sibling Gmail
		// bridge already makes. exec wraps start failures (missing binary, not
		// executable) in *exec.Error; a non-zero EXIT is a different thing and stays
		// ambiguous, because the leaf may have clicked before dying.
		var startErr *exec.Error
		var pathErr *fs.PathError
		if errors.As(err, &startErr) || errors.As(err, &pathErr) {
			return &SendRejectedError{Body: "bridge never started (never dispatched): " + err.Error()}
		}
		return err
	}
	return checkSendResult(out)
}

func (b *CommandBridge) run(ctx context.Context, operation string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.node, b.script, operation)
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr cappedBuffer
	stdout.limit = maxBridgeOutputBytes
	stderr.limit = 64 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("Slack bridge %s: %w: %s", operation, err, bytes.TrimSpace(stderr.Bytes()))
		}
		return nil, fmt.Errorf("Slack bridge %s: %w", operation, err)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("Slack bridge %s output exceeded %d bytes", operation, maxBridgeOutputBytes)
	}
	return stdout.Bytes(), nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len() >= b.limit {
		b.overflow = true
		return len(p), nil
	}
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		b.overflow = true
		_, _ = b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// DeliveryBridge is both Slack delivery seams at once — tools.SlackDrafter and
// tools.SlackSender. Both transports satisfy it, which is what lets one
// constructed bridge serve drafting and sending in the same process.
type DeliveryBridge interface {
	Draft(ctx context.Context, targetURL, text string) error
	Send(ctx context.Context, targetURL, text string) error
}

// NewDeliveryBridgeFromEnv picks the transport the same way the connector's own
// poller does: SLACK_WEB_BRIDGE_URL selects HTTP, otherwise
// SLACK_WEB_BRIDGE_SCRIPT selects the local subprocess. It returns (nil, nil)
// when neither is set, so a caller can leave the seams unwired.
//
// Both trusted processes (opsctl, dashboard) go through here. Before SWT-12 they
// built a CommandBridge unconditionally and stat()ed a script on their own host,
// so a cluster-resident dashboard could neither draft nor send — the exact
// configuration the promotion exists to escape.
func NewDeliveryBridgeFromEnv() (DeliveryBridge, error) {
	if rawURL := os.Getenv("SLACK_WEB_BRIDGE_URL"); rawURL != "" {
		token, err := TokenFromEnv()
		if err != nil {
			return nil, err
		}
		bridge, err := NewHTTPBridge(rawURL, token, nil)
		if err != nil {
			return nil, err
		}
		return bridge, nil
	}
	if script := os.Getenv("SLACK_WEB_BRIDGE_SCRIPT"); script != "" {
		bridge, err := NewCommandBridge(os.Getenv("SLACK_WEB_NODE"), script)
		if err != nil {
			return nil, err
		}
		return bridge, nil
	}
	return nil, nil
}
