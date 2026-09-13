package slackweb

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type NormalizedThread struct {
	ThreadKey string
	Subject   string
}

type NormalizedMessage struct {
	ThreadKey         string
	ExternalMessageID string
	Direction         string
	SentAt            time.Time
	Subject           string
	Sender            string
	SenderID          string
	BodyText          string
	Channel           string
	TargetRef         string
}

func NormalizeConversation(raw json.RawMessage) (NormalizedThread, error) {
	var observation rawObservation
	if err := json.Unmarshal(raw, &observation); err != nil {
		return NormalizedThread{}, fmt.Errorf("parse raw Slack conversation: %w", err)
	}
	if observation.Kind != "conversation" {
		return NormalizedThread{}, fmt.Errorf("raw Slack item kind %q is not conversation", observation.Kind)
	}
	if err := validateObservationIdentity(observation); err != nil {
		return NormalizedThread{}, err
	}
	return NormalizedThread{
		ThreadKey: channelThreadKey(observation.Workspace.ID, observation.Conversation.ID),
		Subject:   observation.Conversation.Name,
	}, nil
}

func NormalizeMessage(raw json.RawMessage) (NormalizedThread, NormalizedMessage, error) {
	var observation rawObservation
	if err := json.Unmarshal(raw, &observation); err != nil {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("parse raw Slack message: %w", err)
	}
	if observation.Kind != "message" || observation.Message == nil {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("raw Slack item is not a message")
	}
	if err := validateObservationIdentity(observation); err != nil {
		return NormalizedThread{}, NormalizedMessage{}, err
	}
	message := *observation.Message
	if message.ID == "" {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("raw Slack message has no stable id")
	}
	if message.Timestamp == "" {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("raw Slack message %s has no timestamp", message.ID)
	}
	if message.AuthorID == "" {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("raw Slack message %s has no author id; refusing to guess direction", message.ID)
	}
	sentAt, err := time.Parse(time.RFC3339Nano, message.Timestamp)
	if err != nil {
		return NormalizedThread{}, NormalizedMessage{}, fmt.Errorf("parse Slack message %s timestamp: %w", message.ID, err)
	}

	threadKey := channelThreadKey(observation.Workspace.ID, observation.Conversation.ID)
	targetRef := strings.TrimRight(observation.Conversation.URL, "/")
	if message.ThreadRootID != "" {
		threadKey += ":" + message.ThreadRootID
		targetRef += "/" + message.ThreadRootID
	}
	direction := "inbound"
	if message.AuthorID == observation.Workspace.OwnUserID {
		direction = "outbound"
	}
	thread := NormalizedThread{ThreadKey: threadKey, Subject: observation.Conversation.Name}
	normalized := NormalizedMessage{
		ThreadKey:         threadKey,
		ExternalMessageID: "slack:" + observation.Workspace.ID + ":" + observation.Conversation.ID + ":" + message.ID,
		Direction:         direction,
		SentAt:            sentAt,
		Subject:           observation.Conversation.Name,
		Sender:            message.Author,
		SenderID:          message.AuthorID,
		BodyText:          message.Text,
		Channel:           Channel,
		TargetRef:         targetRef,
	}
	return thread, normalized, nil
}

func validateObservationIdentity(observation rawObservation) error {
	if observation.Workspace.ID == "" || observation.Workspace.OwnUserID == "" {
		return fmt.Errorf("raw Slack observation is missing workspace id or own_user_id")
	}
	if observation.Conversation.ID == "" || observation.Conversation.Name == "" || observation.Conversation.URL == "" {
		return fmt.Errorf("raw Slack observation is missing conversation id, name, or url")
	}
	return nil
}

func channelThreadKey(workspaceID, conversationID string) string {
	return "slack:" + workspaceID + ":" + conversationID
}

// IsRootedThreadKey reports whether a slack thread key names ONE thread
// (slack:{ws}:{conv}:{root}, which NormalizeMessage builds for a message with a
// thread root) rather than a whole channel or DM (slack:{ws}:{conv}, which
// channelThreadKey builds). It is the ONE reading of that difference (SWT-33
// criterion 14): no SQL and no other package may pick the key apart, because a
// second spelling drifts from the builder with no error anywhere.
//
// Segment COUNT decides, as upworkcrm's ParseThreadKey does — no workspace,
// conversation or message id contains a colon. A non-slack key is not rooted by
// this rule; the caller decides what a jira or gmail thread is.
func IsRootedThreadKey(threadKey string) bool {
	parts := strings.Split(threadKey, ":")
	if len(parts) != 4 || parts[0] != "slack" {
		return false
	}
	for _, p := range parts[1:] {
		if p == "" {
			return false
		}
	}
	return true
}

// IsDirectMessageKey reports whether a slack thread key names a 1:1 DIRECT
// MESSAGE conversation, rooted or not (SWT-40 C-D4): the CONVERSATION segment
// of slack:{ws}:{conv}[:{root}] starts with an upper-case `D`, the leaf's own DM
// fallback and the shape production stores (slack:T0360B84U:D01EJRX6P45). It is
// the one reading of that fact, beside channelThreadKey (the builder) and
// IsRootedThreadKey (the other reader).
//
// Group DMs (mpim, legacy `G…` ids) are excluded: C-D3 addresses a 1:1 DM to
// Salvador because he is the only person it can be addressed to, and a group
// DM is a small channel. Segment COUNT and position decide, as they do for
// IsRootedThreadKey, and the comparison is case-sensitive: the stored key
// keeps Slack's exported case.
func IsDirectMessageKey(threadKey string) bool {
	parts := strings.Split(threadKey, ":")
	if (len(parts) != 3 && len(parts) != 4) || parts[0] != "slack" {
		return false
	}
	for _, p := range parts[1:] {
		if p == "" {
			return false
		}
	}
	return strings.HasPrefix(parts[2], "D")
}
