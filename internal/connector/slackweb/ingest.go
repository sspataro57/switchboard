package slackweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type Sink interface {
	EnsureAccount(ctx context.Context, workspace Workspace) (accountID int64, err error)
	StartRun(ctx context.Context, accountID int64) (runID int64, err error)
	RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
	InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
}

func Ingest(ctx context.Context, source Source, sink Sink) (Stats, error) {
	var total Stats
	exported, err := source.Export(ctx)
	if err != nil {
		return total, fmt.Errorf("export Slack observations: %w", err)
	}
	if exported.SchemaVersion != SchemaVersion {
		return total, fmt.Errorf("unsupported Slack bridge schema_version %d (want %d)", exported.SchemaVersion, SchemaVersion)
	}

	for _, workspace := range exported.Workspaces {
		if err := validateWorkspace(workspace); err != nil {
			return total, err
		}
		accountID, err := sink.EnsureAccount(ctx, workspace)
		if err != nil {
			return total, fmt.Errorf("ensure Slack workspace %s account: %w", workspace.ID, err)
		}
		runID, err := sink.StartRun(ctx, accountID)
		if err != nil {
			return total, fmt.Errorf("start Slack sync run for %s: %w", workspace.ID, err)
		}
		stats := Stats{WorkspacesSeen: 1}
		fail := func(cause error) (Stats, error) {
			_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
			total.add(stats)
			return total, cause
		}

		wm := workspaceMeta{ID: workspace.ID, Name: workspace.Name, URL: workspace.URL, OwnUserID: workspace.OwnUserID}
		for _, conversation := range workspace.Conversations {
			if err := validateConversation(workspace.ID, conversation); err != nil {
				return fail(err)
			}
			stats.ConversationsSeen++
			stats.MessagesSkipped += conversation.SkippedMessageCount
			cm := conversationMeta{
				ID: conversation.ID, Name: conversation.Name, Type: conversation.Type, URL: conversation.URL,
				SkippedMessageCount: conversation.SkippedMessageCount,
			}
			if err := upsertObservation(ctx, sink, accountID, "conversation:"+conversation.ID,
				rawObservation{Kind: "conversation", Workspace: wm, Conversation: cm}, &stats); err != nil {
				return fail(err)
			}
			for index := range conversation.Messages {
				message := conversation.Messages[index]
				if message.ID == "" {
					return fail(fmt.Errorf("Slack conversation %s contains a message without a stable id", conversation.ID))
				}
				stats.MessagesSeen++
				if err := upsertObservation(ctx, sink, accountID,
					"message:"+conversation.ID+":"+message.ID,
					rawObservation{Kind: "message", Workspace: wm, Conversation: cm, Message: &message}, &stats); err != nil {
					return fail(err)
				}
			}
		}
		if err := sink.FinishRun(ctx, runID, "ok", stats, ""); err != nil {
			return total, fmt.Errorf("finish Slack sync run for %s: %w", workspace.ID, err)
		}
		total.add(stats)
	}
	return total, nil
}

func validateWorkspace(workspace Workspace) error {
	if workspace.ID == "" || workspace.Name == "" || workspace.URL == "" {
		return fmt.Errorf("Slack bridge workspace is missing id, name, or url")
	}
	if workspace.OwnUserID == "" {
		return fmt.Errorf("Slack workspace %s is missing own_user_id", workspace.ID)
	}
	return nil
}

func validateConversation(workspaceID string, conversation Conversation) error {
	if conversation.ID == "" || conversation.Name == "" || conversation.URL == "" {
		return fmt.Errorf("Slack workspace %s contains a conversation missing id, name, or url", workspaceID)
	}
	switch conversation.Type {
	case "public_channel", "private_channel", "dm", "group_dm":
		return nil
	default:
		return fmt.Errorf("Slack conversation %s has unsupported type %q", conversation.ID, conversation.Type)
	}
}

func upsertObservation(ctx context.Context, sink Sink, accountID int64, externalID string, observation rawObservation, stats *Stats) error {
	raw, err := json.Marshal(observation)
	if err != nil {
		return fmt.Errorf("marshal raw %s: %w", externalID, err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	stored, exists, err := sink.RawHash(ctx, accountID, externalID)
	if err != nil {
		return fmt.Errorf("read stored hash for %s: %w", externalID, err)
	}
	switch {
	case !exists:
		if err := sink.InsertRaw(ctx, accountID, externalID, raw, hash); err != nil {
			return fmt.Errorf("insert raw %s: %w", externalID, err)
		}
		stats.RawInserted++
	case stored == hash:
		stats.RawUnchanged++
	default:
		if err := sink.UpdateRaw(ctx, accountID, externalID, raw, hash); err != nil {
			return fmt.Errorf("update raw %s: %w", externalID, err)
		}
		stats.RawUpdated++
	}
	return nil
}
