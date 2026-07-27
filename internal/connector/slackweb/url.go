package slackweb

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	workspaceIDPattern    = regexp.MustCompile(`^T[A-Z0-9]+$`)
	conversationIDPattern = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)
	messageIDPattern      = regexp.MustCompile(`^p[0-9]+$`)
)

type Target struct {
	WorkspaceID    string
	ConversationID string
	MessageID      string
}

// ParseTargetURL validates the canonical channel/thread URLs accepted by the
// assisted Slack delivery path. Query strings, fragments, credentials, and
// non-Slack hosts are rejected rather than delegated to browser navigation.
func ParseTargetURL(value string) (Target, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return Target{}, fmt.Errorf("parse Slack target URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "app.slack.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.EscapedPath() != parsed.Path {
		return Target{}, fmt.Errorf("Slack target must be an https://app.slack.com/client URL without credentials, query, or fragment")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "client" {
		return Target{}, fmt.Errorf("Slack target path must be /client/{workspace}/{conversation}[/{message}]")
	}
	workspaceID, err := url.PathUnescape(parts[1])
	if err != nil || !workspaceIDPattern.MatchString(workspaceID) {
		return Target{}, fmt.Errorf("Slack target has an invalid workspace ID")
	}
	conversationID, err := url.PathUnescape(parts[2])
	if err != nil || !conversationIDPattern.MatchString(conversationID) {
		return Target{}, fmt.Errorf("Slack target has an invalid conversation ID")
	}
	target := Target{WorkspaceID: workspaceID, ConversationID: conversationID}
	if len(parts) == 4 {
		messageID, err := url.PathUnescape(parts[3])
		if err != nil || !messageIDPattern.MatchString(messageID) {
			return Target{}, fmt.Errorf("Slack target has an invalid message ID")
		}
		target.MessageID = messageID
	}
	return target, nil
}

// CanonicalURL rebuilds the exact string form that loop closure matches on.
// ParseTargetURL accepts variants the matcher never will — a trailing slash is
// the cheap example — so a delivery stored verbatim can be unconfirmable
// forever with no error anywhere. Store this, never the caller's spelling.
func (t Target) CanonicalURL() string {
	u := "https://app.slack.com/client/" + t.WorkspaceID + "/" + t.ConversationID
	if t.MessageID != "" {
		u += "/" + t.MessageID
	}
	return u
}
