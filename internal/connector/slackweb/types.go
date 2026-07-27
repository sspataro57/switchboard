// Package slackweb connects Switchboard's Go ingestion spine to the local
// TypeScript/Playwright Slack Web leaf adapter.
package slackweb

import "context"

const (
	Provider        = "slack_web"
	Channel         = "slack"
	DeliveryChannel = "slack_reply"
	SchemaVersion   = 1
)

type Export struct {
	SchemaVersion int         `json:"schema_version"`
	Workspaces    []Workspace `json:"workspaces"`
}

type Workspace struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	URL           string         `json:"url"`
	OwnUserID     string         `json:"own_user_id"`
	Conversations []Conversation `json:"conversations"`
}

type Conversation struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Type                string    `json:"type"`
	URL                 string    `json:"url"`
	SkippedMessageCount int       `json:"skipped_message_count"`
	Messages            []Message `json:"messages"`
}

type Message struct {
	ID               string `json:"id"`
	Timestamp        string `json:"timestamp,omitempty"`
	Author           string `json:"author,omitempty"`
	AuthorID         string `json:"author_id,omitempty"`
	Direction        string `json:"direction"`
	Text             string `json:"text"`
	Permalink        string `json:"permalink,omitempty"`
	ThreadReplyCount int    `json:"thread_reply_count,omitempty"`
	ThreadRootID     string `json:"thread_root_id,omitempty"`
}

type workspaceMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	OwnUserID string `json:"own_user_id"`
}

type conversationMeta struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Type                string `json:"type"`
	URL                 string `json:"url"`
	SkippedMessageCount int    `json:"skipped_message_count,omitempty"`
}

type rawObservation struct {
	Kind         string           `json:"kind"`
	Workspace    workspaceMeta    `json:"workspace"`
	Conversation conversationMeta `json:"conversation"`
	Message      *Message         `json:"message,omitempty"`
}

type Source interface {
	Export(ctx context.Context) (Export, error)
}

type Stats struct {
	WorkspacesSeen    int `json:"workspaces_seen"`
	ConversationsSeen int `json:"conversations_seen"`
	MessagesSeen      int `json:"messages_seen"`
	MessagesSkipped   int `json:"messages_skipped_identity"`
	RawInserted       int `json:"raw_inserted"`
	RawUpdated        int `json:"raw_updated"`
	RawUnchanged      int `json:"raw_unchanged"`
	Normalized        int `json:"normalized"`
}

func (s *Stats) add(other Stats) {
	s.WorkspacesSeen += other.WorkspacesSeen
	s.ConversationsSeen += other.ConversationsSeen
	s.MessagesSeen += other.MessagesSeen
	s.MessagesSkipped += other.MessagesSkipped
	s.RawInserted += other.RawInserted
	s.RawUpdated += other.RawUpdated
	s.RawUnchanged += other.RawUnchanged
	s.Normalized += other.Normalized
}

type Config struct {
	All bool
}
