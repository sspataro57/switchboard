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
