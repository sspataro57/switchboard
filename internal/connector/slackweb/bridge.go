package slackweb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
