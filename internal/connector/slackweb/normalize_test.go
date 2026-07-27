package slackweb_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

func rawMessage(t *testing.T, authorID, rootID string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"kind": "message",
		"workspace": map[string]any{
			"id": "T123", "name": "Example", "url": "https://app.slack.com/client/T123",
			"own_user_id": "UOWN",
		},
		"conversation": map[string]any{
			"id": "C456", "name": "general", "type": "public_channel",
			"url": "https://app.slack.com/client/T123/C456",
		},
		"message": map[string]any{
			"id": "p1710000060000002", "timestamp": "2024-03-09T16:01:00.002Z",
			"author": "Person", "author_id": authorID, "text": "Please review this.",
			"permalink":      "https://app.slack.com/client/T123/C456/p1710000060000002",
			"thread_root_id": rootID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNormalizeMessage_ThreadIdentityAndDirection(t *testing.T) {
	thread, inbound, err := slackweb.NormalizeMessage(rawMessage(t, "UCLIENT", "p1710000000000001"))
	if err != nil {
		t.Fatalf("NormalizeMessage(inbound): %v", err)
	}
	if got, want := thread.ThreadKey, "slack:T123:C456:p1710000000000001"; got != want {
		t.Errorf("thread key = %q, want %q", got, want)
	}
	if got, want := inbound.ExternalMessageID, "slack:T123:C456:p1710000060000002"; got != want {
		t.Errorf("external id = %q, want %q", got, want)
	}
	if inbound.Direction != "inbound" || inbound.Channel != slackweb.Channel {
		t.Errorf("direction/channel = %q/%q, want inbound/%q", inbound.Direction, inbound.Channel, slackweb.Channel)
	}
	wantTime := time.Date(2024, 3, 9, 16, 1, 0, 2_000_000, time.UTC)
	if !inbound.SentAt.Equal(wantTime) {
		t.Errorf("sent_at = %v, want %v", inbound.SentAt, wantTime)
	}

	_, outbound, err := slackweb.NormalizeMessage(rawMessage(t, "UOWN", "p1710000000000001"))
	if err != nil {
		t.Fatalf("NormalizeMessage(outbound): %v", err)
	}
	if outbound.Direction != "outbound" {
		t.Errorf("own message direction = %q, want outbound", outbound.Direction)
	}
	if got, want := outbound.TargetRef, "https://app.slack.com/client/T123/C456/p1710000000000001"; got != want {
		t.Errorf("target ref = %q, want %q", got, want)
	}
}

func TestNormalizeMessage_ChannelThreadAndRequiredIdentity(t *testing.T) {
	thread, _, err := slackweb.NormalizeMessage(rawMessage(t, "UCLIENT", ""))
	if err != nil {
		t.Fatalf("NormalizeMessage(channel): %v", err)
	}
	if got, want := thread.ThreadKey, "slack:T123:C456"; got != want {
		t.Errorf("channel thread key = %q, want %q", got, want)
	}

	var doc map[string]any
	if err := json.Unmarshal(rawMessage(t, "UCLIENT", ""), &doc); err != nil {
		t.Fatal(err)
	}
	workspace := doc["workspace"].(map[string]any)
	delete(workspace, "own_user_id")
	b, _ := json.Marshal(doc)
	if _, _, err := slackweb.NormalizeMessage(b); err == nil {
		t.Fatal("NormalizeMessage accepted a message without own_user_id")
	}

	doc = nil
	if err := json.Unmarshal(rawMessage(t, "UCLIENT", ""), &doc); err != nil {
		t.Fatal(err)
	}
	message := doc["message"].(map[string]any)
	delete(message, "author_id")
	b, _ = json.Marshal(doc)
	if _, _, err := slackweb.NormalizeMessage(b); err == nil {
		t.Fatal("NormalizeMessage guessed direction without an author_id")
	}
}
