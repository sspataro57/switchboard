package slackweb_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

type fakeSource struct{ export slackweb.Export }

func (f *fakeSource) Export(context.Context) (slackweb.Export, error) { return f.export, nil }

type rawWrite struct {
	workspaceID string
	externalID  string
	raw         json.RawMessage
	hash        string
}

type fakeSink struct {
	stored  map[string]string
	inserts []rawWrite
	updates []rawWrite
	runs    map[string]string
}

func newFakeSink() *fakeSink {
	return &fakeSink{stored: map[string]string{}, runs: map[string]string{}}
}

func (s *fakeSink) EnsureAccount(_ context.Context, workspace slackweb.Workspace) (int64, error) {
	if workspace.ID == "" || workspace.OwnUserID == "" {
		panic("invalid workspace reached sink")
	}
	return 7, nil
}
func (s *fakeSink) StartRun(_ context.Context, _ int64, _ time.Time) (int64, error) { return 9, nil }
func (s *fakeSink) RawHash(_ context.Context, _ int64, externalID string) (string, bool, error) {
	h, ok := s.stored[externalID]
	return h, ok, nil
}
func (s *fakeSink) InsertRaw(_ context.Context, _ int64, externalID string, raw json.RawMessage, hash string) error {
	s.inserts = append(s.inserts, rawWrite{externalID: externalID, raw: raw, hash: hash})
	s.stored[externalID] = hash
	return nil
}
func (s *fakeSink) UpdateRaw(_ context.Context, _ int64, externalID string, raw json.RawMessage, hash string) error {
	s.updates = append(s.updates, rawWrite{externalID: externalID, raw: raw, hash: hash})
	s.stored[externalID] = hash
	return nil
}
func (s *fakeSink) FinishRun(_ context.Context, _ int64, status string, _ slackweb.Stats, errMsg string) error {
	s.runs[status] = errMsg
	return nil
}

func exportFixture(text string) slackweb.Export {
	return slackweb.Export{
		SchemaVersion: 1,
		Workspaces: []slackweb.Workspace{{
			ID: "T123", Name: "Example", URL: "https://app.slack.com/client/T123", OwnUserID: "UOWN",
			Conversations: []slackweb.Conversation{{
				ID: "C456", Name: "general", Type: "public_channel", URL: "https://app.slack.com/client/T123/C456",
				Messages: []slackweb.Message{{
					ID: "p1710000000000001", Timestamp: "2024-03-09T16:00:00.001Z",
					Author: "Client", AuthorID: "UCLIENT", Text: text,
					Permalink: "https://app.slack.com/client/T123/C456/p1710000000000001",
				}},
			}},
		}},
	}
}

func TestIngest_RawInsertUnchangedAndUpdate(t *testing.T) {
	ctx := context.Background()
	sink := newFakeSink()
	source := &fakeSource{export: exportFixture("first")}

	stats, err := slackweb.Ingest(ctx, source, sink)
	if err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	if stats.RawInserted != 2 || len(sink.inserts) != 2 {
		t.Fatalf("first inserts = %d/%d, want conversation + message", stats.RawInserted, len(sink.inserts))
	}
	if sink.inserts[0].externalID != "conversation:C456" || sink.inserts[1].externalID != "message:C456:p1710000000000001" {
		t.Errorf("external ids = %q, %q", sink.inserts[0].externalID, sink.inserts[1].externalID)
	}
	if sink.runs["ok"] != "" {
		t.Errorf("successful run missing/has error: %#v", sink.runs)
	}

	stats, err = slackweb.Ingest(ctx, source, sink)
	if err != nil {
		t.Fatalf("unchanged Ingest: %v", err)
	}
	if stats.RawUnchanged != 2 || stats.RawInserted != 0 || len(sink.inserts) != 2 {
		t.Errorf("unchanged stats = %+v, inserts=%d", stats, len(sink.inserts))
	}

	source.export = exportFixture("changed")
	stats, err = slackweb.Ingest(ctx, source, sink)
	if err != nil {
		t.Fatalf("changed Ingest: %v", err)
	}
	if stats.RawUpdated != 1 || len(sink.updates) != 1 {
		t.Fatalf("changed updates = %d/%d, want one message update", stats.RawUpdated, len(sink.updates))
	}
	if sink.updates[0].externalID != "message:C456:p1710000000000001" {
		t.Errorf("updated external id = %q", sink.updates[0].externalID)
	}
}

func TestIngest_RejectsUnsupportedSchemaBeforeWriting(t *testing.T) {
	sink := newFakeSink()
	_, err := slackweb.Ingest(context.Background(), &fakeSource{export: slackweb.Export{SchemaVersion: 2}}, sink)
	if err == nil {
		t.Fatal("Ingest accepted unsupported bridge schema")
	}
	if len(sink.inserts) != 0 || len(sink.updates) != 0 {
		t.Fatal("unsupported schema wrote raw rows")
	}
}
