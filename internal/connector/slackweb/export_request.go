package slackweb

// SWT-39 fix F: the export request. Switchboard sends the conversations it has
// already ingested (known) plus a read budget, so a run's coverage stops
// depending on what the Slack UI's virtualized lists happen to render. The
// leaf reads newly discovered conversations first, then known ones by recency,
// then the rest oldest-visited first, and defers what the budget cannot reach.

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"
)

// ExportRequest is the /export body. Every field is optional on the wire and
// OMITTED when zero: the leaf rejects budget_ms: 0, known: null and
// last_seen_ts: "" with a 500 that stops the export for every workspace.
type ExportRequest struct {
	Known            map[string][]KnownConversation `json:"known,omitempty"`
	BudgetMS         int                            `json:"budget_ms,omitempty"`
	MaxConversations int                            `json:"max_conversations,omitempty"`
	// Targets (SWT-75 D1): read EXACTLY these conversation ids per workspace,
	// with no enumeration and no rotation. Mutually exclusive with Known on
	// the leaf. Never set by the rotation; set only by BuildTargetedRequest.
	Targets map[string][]string `json:"targets,omitempty"`
}

// KnownConversation is one conversation switchboard knows, keyed under its
// workspace id. LastSeenTS is when switchboard last saw the leaf READ it, in
// Slack ts form (epoch seconds): the leaf reads known-but-unenumerated
// conversations least-recently-read first, and one with no ts first of all,
// so nothing starves. It is NOT the newest message's ts — that never changes
// for a dormant conversation and would pin the same old channels to the front
// forever (SWT-39 review).
type KnownConversation struct {
	ID         string `json:"id"`
	LastSeenTS string `json:"last_seen_ts,omitempty"`
	Name       string `json:"name,omitempty"`
}

// KnownConversationRow is one ingested conversation as the sink loads it.
// LastReadAt is the start of the latest completed run whose stats.read holds
// it; zero when no run recorded reading it.
type KnownConversationRow struct {
	WorkspaceID    string
	ConversationID string
	Name           string
	LastReadAt     time.Time
}

// ExportBudget bounds one export: wall clock and conversations read.
type ExportBudget struct {
	BudgetMS         int
	MaxConversations int
}

const (
	// DefaultExportBudgetMS is 15 minutes. The connector's 30-minute run
	// context also covers the second workspace's enumeration, normalize,
	// reconcile and capture, and the shared Slack tab serves MCP callers too
	// (SWT-39 review: 20 min left too little).
	DefaultExportBudgetMS = 900000
	// DefaultExportMaxConversations caps reads per export (15–19 s each).
	DefaultExportMaxConversations = 60
)

// ExportBudgetFromEnv reads SLACK_WEB_EXPORT_BUDGET_MS and
// SLACK_WEB_EXPORT_MAX_CONVERSATIONS. Unparseable or non-positive values fall
// back to the defaults — load-bearing here, not cosmetic: a 0 would fail the
// leaf's positive-integer check and 500 the export.
func ExportBudgetFromEnv() ExportBudget {
	return ExportBudget{
		BudgetMS:         positiveEnv("SLACK_WEB_EXPORT_BUDGET_MS", DefaultExportBudgetMS),
		MaxConversations: positiveEnv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", DefaultExportMaxConversations),
	}
}

func positiveEnv(name string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// The leaf's validation (slackconnector http-bridge.ts parseExportBody). A row
// failing these is dropped here, never sent: one bad raw row must not stop
// the export for every workspace.
var (
	leafWorkspaceIDRule    = regexp.MustCompile(`^T[A-Z0-9]{5,}$`)
	leafConversationIDRule = regexp.MustCompile(`^[CDG][A-Z0-9]{5,}$`)
)

// BuildExportRequest turns loaded rows into the /export body. Rows whose
// workspace or conversation id would fail the leaf's rules are returned as
// dropped. A conversation never recorded as read goes without last_seen_ts,
// which the leaf sorts first.
func BuildExportRequest(rows []KnownConversationRow, budget ExportBudget) (ExportRequest, []KnownConversationRow) {
	req := ExportRequest{BudgetMS: budget.BudgetMS, MaxConversations: budget.MaxConversations}
	var dropped []KnownConversationRow
	seen := map[string]bool{}
	for _, r := range rows {
		if !leafWorkspaceIDRule.MatchString(r.WorkspaceID) || !leafConversationIDRule.MatchString(r.ConversationID) {
			dropped = append(dropped, r)
			continue
		}
		key := r.WorkspaceID + "/" + r.ConversationID
		if seen[key] {
			continue
		}
		seen[key] = true
		k := KnownConversation{ID: r.ConversationID, Name: r.Name}
		if !r.LastReadAt.IsZero() {
			k.LastSeenTS = slackTS(r.LastReadAt)
		}
		if req.Known == nil {
			req.Known = map[string][]KnownConversation{}
		}
		req.Known[r.WorkspaceID] = append(req.Known[r.WorkspaceID], k)
	}
	return req, dropped
}

// WatchRow is one slack_watch row as the sink loads it (SWT-75 D2).
// LastReadAt comes from sync_runs (the latest ROTATION run whose stats.read
// lists the conversation), like KnownConversationRow's — never from a column.
type WatchRow struct {
	ID             int64
	WorkspaceID    string
	ConversationID string
	Label          string
	Enabled        bool
	LastReadAt     time.Time
}

// BuildTargetedRequest is BuildExportRequest's twin for a targeted pass: the
// /export body carries `targets` + budget_ms + max_conversations (= the number
// of ids sent) and NO known — the leaf refuses the two together, and `known`
// alone would run a full export every minute. A disabled row is not a target;
// a row failing the leaf's id rules is returned as dropped so the caller can
// log it by name (D2 moved the silent drop into a database CHECK, so this
// should never fire in production); a conversation is listed once. Zero
// fields are omitted on the wire: the leaf 500s on budget_ms: 0.
func BuildTargetedRequest(rows []WatchRow, budgetMS int) (ExportRequest, []WatchRow) {
	req := ExportRequest{BudgetMS: budgetMS}
	var dropped []WatchRow
	seen := map[string]bool{}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		if !leafWorkspaceIDRule.MatchString(r.WorkspaceID) || !leafConversationIDRule.MatchString(r.ConversationID) {
			dropped = append(dropped, r)
			continue
		}
		key := r.WorkspaceID + "/" + r.ConversationID
		if seen[key] {
			continue
		}
		seen[key] = true
		if req.Targets == nil {
			req.Targets = map[string][]string{}
		}
		req.Targets[r.WorkspaceID] = append(req.Targets[r.WorkspaceID], r.ConversationID)
		req.MaxConversations++
	}
	return req, dropped
}

// ErrNotTargeted: the leaf answered a targeted request without
// coverage.mode "targeted" on every workspace — an older leaf that ignored the
// field and ran a FULL export (SWT-75 D8). Nothing may be ingested from it.
var ErrNotTargeted = errors.New(`Slack bridge did not honour targets (coverage.mode is not "targeted")`)

// CheckTargetedMode refuses a response that does not report mode "targeted"
// for EVERY workspace it returned. A missing coverage block, a missing mode,
// or one workspace answering "full" all fail: each is exactly what an old leaf
// or a partially deployed one would produce.
func CheckTargetedMode(exported Export) error {
	for _, ws := range exported.Workspaces {
		if ws.Coverage == nil {
			return fmt.Errorf("workspace %s: no coverage block: %w", ws.ID, ErrNotTargeted)
		}
		if ws.Coverage.Mode != CoverageModeTargeted {
			return fmt.Errorf("workspace %s: coverage.mode %q: %w", ws.ID, ws.Coverage.Mode, ErrNotTargeted)
		}
	}
	return nil
}

// slackTS renders t in Slack ts form: epoch seconds with six fractional digits.
func slackTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}
