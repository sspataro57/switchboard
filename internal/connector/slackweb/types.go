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

	// SWT-39: what the leaf covered in this workspace. All absent from an old
	// leaf, which is how "no coverage information" stays distinct from "read
	// nothing" (the leaf sends read: [] for the latter).
	Enumerated []EnumeratedConversation `json:"enumerated,omitempty"`
	Read       []string                 `json:"read,omitempty"`
	Unreadable []UnreadableConversation `json:"unreadable,omitempty"`
	Deferred   []string                 `json:"deferred,omitempty"`
	Coverage   *Coverage                `json:"coverage,omitempty"`
}

// EnumeratedConversation is one conversation the leaf found in scope:
// source is sidebar | dms | known; rank is its 1-based position in the DMs
// view's recency list (0 when not listed there).
type EnumeratedConversation struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	URL    string `json:"url"`
	Source string `json:"source"`
	Rank   int    `json:"rank,omitempty"`
}

// UnreadableConversation is an in-scope conversation whose read failed. Code
// is the leaf's ConnectorError code when the failure was classified.
type UnreadableConversation struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason"`
}

// Coverage is the leaf's per-workspace tally for one export.
type Coverage struct {
	EnumeratedCount int  `json:"enumerated_count"`
	KnownCount      int  `json:"known_count"`
	ReadCount       int  `json:"read_count"`
	UnreadableCount int  `json:"unreadable_count"`
	DeferredCount   int  `json:"deferred_count"`
	BudgetExhausted bool `json:"budget_exhausted"`
	ElapsedMS       int  `json:"elapsed_ms"`
}

// reportsCoverage says whether the leaf sent any coverage field at all. An old
// leaf sends none, and its run keeps today's meaning.
func (w Workspace) reportsCoverage() bool {
	return w.Coverage != nil || w.Read != nil || w.Enumerated != nil || w.Deferred != nil || w.Unreadable != nil
}

// partialCoverage: the run cannot claim it read everything in scope.
func (w Workspace) partialCoverage() bool {
	return len(w.Deferred) > 0 || len(w.Unreadable) > 0 || (w.Coverage != nil && w.Coverage.BudgetExhausted)
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

// Source exports observations. The request carries the conversations
// switchboard already knows plus a read budget (SWT-39); a zero request asks
// for the leaf's old enumerate-only behaviour.
type Source interface {
	Export(ctx context.Context, req ExportRequest) (Export, error)
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

	// SWT-39 coverage, written per workspace run into sync_runs.stats. Read is
	// a pointer so an EMPTY read set is recorded as [] (the run read nothing)
	// while an old leaf's run carries no key at all (legacy: ReconcileUnconfirmed
	// keeps counting it as before). Never summed across workspaces.
	Read       *[]string                `json:"read,omitempty"`
	Deferred   []string                 `json:"deferred,omitempty"`
	Unreadable []UnreadableConversation `json:"unreadable,omitempty"`
	Coverage   *Coverage                `json:"coverage,omitempty"`
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
