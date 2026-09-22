package slackweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Sink interface {
	EnsureAccount(ctx context.Context, workspace Workspace) (accountID int64, err error)
	// StartRun takes the instant the export BEGAN, not the instant the row is
	// written. The export runs before any run row exists (it is what discovers
	// the workspaces), so defaulting started_at to now() would record the export's
	// END time. ReconcileUnconfirmed counts passes that could have OBSERVED a
	// message, which is a question about when the scrape began.
	// phase is stats->>'phase': PhaseSlackWeb for a rotation/full export,
	// PhaseSlackWebWatch for a targeted watch pass (SWT-75 D6) — the value
	// ReconcileUnconfirmed and KnownConversations filter on.
	StartRun(ctx context.Context, accountID int64, startedAt time.Time, phase string) (runID int64, err error)
	// KnownConversations lists every conversation already ingested, with when
	// a run last visited it (read or failed to read), so the export can read
	// them by URL least-recently-visited first instead of depending on what the
	// Slack UI happens to render (SWT-39).
	KnownConversations(ctx context.Context) ([]KnownConversationRow, error)
	// WatchTargets lists the ENABLED slack_watch rows whose workspace this
	// switchboard has already ingested (a slack_web source_accounts row), with
	// when the rotation last read each (SWT-75 D2). Read on every pass.
	WatchTargets(ctx context.Context) ([]WatchRow, error)
	RawHash(ctx context.Context, accountID int64, externalID string) (hash string, exists bool, err error)
	InsertRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	UpdateRaw(ctx context.Context, accountID int64, externalID string, raw json.RawMessage, hash string) error
	FinishRun(ctx context.Context, runID int64, status string, stats Stats, errMsg string) error
}

// Ingest is today's full export: the known-conversation request, under the
// rotation phase, a run row per workspace. The one-shot CronJob and the
// watcher's rotation pass both call it.
func Ingest(ctx context.Context, source Source, sink Sink) (Stats, error) {
	return IngestWith(ctx, source, sink, IngestOptions{Request: knownExportRequest(ctx, sink), Phase: PhaseSlackWeb})
}

// IngestOptions make the request and the run phase inputs (SWT-75 D6).
type IngestOptions struct {
	Request ExportRequest
	// Phase is stamped on every run row this pass writes; "" means PhaseSlackWeb.
	Phase string
	// Quiet is D6's volume discipline for the per-minute pass: write a run row
	// only when raw rows moved, when the pass failed, or on the first success
	// after a failure (WriteRunRow). A quiet watcher writes nothing.
	Quiet bool
	// Targeted refuses a response that does not carry coverage.mode "targeted"
	// on every workspace, BEFORE anything is ingested (D8). RunTargeted sets it.
	Targeted bool
}

// IngestWith is one export pass: raw-first (invariant 1) through
// upsertObservation, one sync_runs row per workspace under opts.Phase.
func IngestWith(ctx context.Context, source Source, sink Sink, opts IngestOptions) (Stats, error) {
	var total Stats
	phase := opts.Phase
	if phase == "" {
		phase = PhaseSlackWeb
	}
	// Before the export, so every run row records when the scrape actually began.
	exportStartedAt := time.Now()
	exported, err := source.Export(ctx, opts.Request)
	if err != nil {
		err = fmt.Errorf("export Slack observations: %w", err)
		recordWholePassFailure(ctx, sink, opts, exportStartedAt, phase, err)
		return total, err
	}
	if exported.SchemaVersion != SchemaVersion {
		err := fmt.Errorf("unsupported Slack bridge schema_version %d (want %d)", exported.SchemaVersion, SchemaVersion)
		recordWholePassFailure(ctx, sink, opts, exportStartedAt, phase, err)
		return total, err
	}
	if opts.Targeted {
		if err := CheckTargetedMode(exported); err != nil {
			recordWholePassFailure(ctx, sink, opts, exportStartedAt, phase, err)
			return total, err
		}
	}

	for _, workspace := range exported.Workspaces {
		if err := validateWorkspace(workspace); err != nil {
			return total, err
		}
		accountID, err := sink.EnsureAccount(ctx, workspace)
		if err != nil {
			return total, fmt.Errorf("ensure Slack workspace %s account: %w", workspace.ID, err)
		}
		// Quiet mode opens the run row LAZILY: only once WriteRunRow says the
		// pass is worth recording. The row still carries the export's start.
		var runID int64
		opened := false
		open := func() error {
			if opened {
				return nil
			}
			id, err := sink.StartRun(ctx, accountID, exportStartedAt, phase)
			if err != nil {
				return fmt.Errorf("start Slack sync run for %s: %w", workspace.ID, err)
			}
			runID, opened = id, true
			return nil
		}
		if !opts.Quiet {
			if err := open(); err != nil {
				return total, err
			}
		}
		stats := Stats{WorkspacesSeen: 1}
		fail := func(cause error) (Stats, error) {
			if err := open(); err == nil {
				_ = sink.FinishRun(ctx, runID, "error", stats, cause.Error())
			}
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
		status := "ok"
		if workspace.reportsCoverage() {
			read := workspace.Read
			if read == nil {
				read = []string{}
			}
			stats.Read = &read
			for _, e := range workspace.Enumerated {
				stats.Enumerated = append(stats.Enumerated, EnumeratedRecord{ID: e.ID, Source: e.Source, Rank: e.Rank})
			}
			stats.Deferred = workspace.Deferred
			stats.Unreadable = workspace.Unreadable
			stats.Coverage = workspace.Coverage
			if workspace.partialCoverage() {
				// What WAS read is ingested above, raw-first; partial only says the
				// run cannot claim it read everything in scope (SWT-39).
				status = "partial"
			}
		}
		if opts.Quiet {
			prev := ""
			if p, ok := sink.(lastRunStatusReader); ok {
				s, err := p.LastRunStatus(ctx, accountID, phase)
				if err != nil {
					slog.Warn("slack watch: read last run status; treating as none", "workspace", workspace.ID, "err", err)
				}
				prev = s
			}
			if !WriteRunRow(stats, nil, prev) {
				total.add(stats)
				continue
			}
		}
		if err := open(); err != nil {
			return total, err
		}
		if err := sink.FinishRun(ctx, runID, status, stats, ""); err != nil {
			return total, fmt.Errorf("finish Slack sync run for %s: %w", workspace.ID, err)
		}
		total.add(stats)
	}
	return total, nil
}

// lastRunStatusReader is the sink's optional "what did the last run of this
// phase say" — D6's "first success after a failure" input. PGSink implements
// it (pinned at compile time in sink.go); a fake sink need not.
type lastRunStatusReader interface {
	LastRunStatus(ctx context.Context, accountID int64, phase string) (string, error)
}

// passFailureRecorder is the sink's optional writer for a WHOLE-pass failure
// (criterion 7): the export itself failed, or the leaf's answer was refused,
// so no workspace loop ran and no run row was opened. PGSink implements it.
type passFailureRecorder interface {
	RecordPassFailure(ctx context.Context, workspaceID string, startedAt time.Time, phase, errMsg string) error
}

// recordWholePassFailure leaves one sync_runs error row per targeted workspace
// when a pass fails before any workspace could be processed — so a bridge that
// has been broken for hours is visible on /funnel, not only in a pod log. Not
// for a busy 503 (an expected skip), and not for the rotation (its request
// names no workspaces; the one-shot never wrote a row for this case either).
// Only the FIRST failure in a row writes (the sink checks the last status),
// so a broken leaf costs one row per workspace, not one per minute.
func recordWholePassFailure(ctx context.Context, sink Sink, opts IngestOptions, startedAt time.Time, phase string, cause error) {
	if errors.Is(cause, ErrBridgeBusy) || len(opts.Request.Targets) == 0 {
		return
	}
	rec, ok := sink.(passFailureRecorder)
	if !ok {
		return
	}
	// Detached from the pass's own context: the failure most worth a row is
	// the pass whose deadline fired, and that context is already dead.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for workspaceID := range opts.Request.Targets {
		if err := rec.RecordPassFailure(rctx, workspaceID, startedAt, phase, cause.Error()); err != nil {
			slog.Warn("slack watch: record pass failure", "workspace", workspaceID, "err", err)
		}
	}
}

// WriteRunRow is D6's volume discipline as a pure decision: a targeted pass
// writes a sync_runs row only when it inserted or updated raw rows, when it
// failed, or on the first success after a failure (prevStatus is the
// account+phase's latest run status, "" when there is none). A quiet watcher
// writes nothing — 1,440 rows/day/workspace for nothing is worse than the
// 230/day SWT-73's D9 already refused.
func WriteRunRow(stats Stats, passErr error, prevStatus string) bool {
	if passErr != nil {
		return true
	}
	if stats.RawInserted > 0 || stats.RawUpdated > 0 {
		return true
	}
	return prevStatus == "error"
}

// knownExportRequest builds the export request from what switchboard has
// already ingested. A failed load degrades to the zero request (the leaf's old
// behaviour) rather than skipping the export: coverage gets worse, ingestion
// does not stop.
func knownExportRequest(ctx context.Context, sink Sink) ExportRequest {
	budget := ExportBudgetFromEnv()
	rows, err := sink.KnownConversations(ctx)
	if err != nil {
		slog.Error("load known Slack conversations; exporting without them", "err", err)
		return ExportRequest{BudgetMS: budget.BudgetMS, MaxConversations: budget.MaxConversations}
	}
	req, dropped := BuildExportRequest(rows, budget)
	for _, d := range dropped {
		slog.Warn("known Slack conversation fails the leaf's id rules; not sent",
			"workspace", d.WorkspaceID, "conversation", d.ConversationID)
	}
	return req
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
