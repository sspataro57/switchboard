package slackweb

// SWT-39 fix F: the export request. Switchboard sends the conversations it has
// already ingested (known) plus a read budget, so a run's coverage stops
// depending on what the Slack UI's virtualized lists happen to render. The
// leaf reads newly discovered conversations first, then known ones by recency,
// then the rest oldest-visited first, and defers what the budget cannot reach.

import (
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

// slackTS renders t in Slack ts form: epoch seconds with six fractional digits.
func slackTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}
