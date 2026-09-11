package slackweb_test

// Regression tests for bug slackweb-collab-export-stale (Jira SWT-39),
// switchboard's half, fix F: send the leaf the conversations switchboard
// already knows (`known`) plus a read budget, so coverage stops depending on
// what the Slack UI's virtual lists happen to render.
// docs/bugs/slackweb-collab-export-stale_DIAGNOSIS.md, option C1.
//
// ZERO I/O. The db loader (PGSink.KnownConversations) is covered in
// coverage_integration_test.go; this file pins the pure body builder and the
// Ingest wiring.
//
// THE LEAF'S VALIDATION (slackconnector src/switchboard/http-bridge.ts
// parseExportBody, fix/swt-39-export-coverage @ 050ed8c). A body failing ANY
// of these is an HTTP 500 and the export does not run, for EVERY workspace:
//
//	workspace key   /^T[A-Z0-9]{5,}$/
//	id              /^[CDG][A-Z0-9]{5,}$/
//	last_seen_ts    /^\d+(\.\d+)?$/        (optional; "" FAILS it)
//	budget_ms, max_conversations   positive integers (optional; 0 FAILS it)
//	known           an object (null FAILS it); each value an array (null FAILS it)
//
// So one bad raw row must be dropped (and logged) by switchboard, never sent.
//
// IMPOSED SURFACE (chosen here; the wire names are the leaf's):
//
//	type ExportRequest struct {
//	    Known            map[string][]KnownConversation `json:"known,omitempty"`
//	    BudgetMS         int                            `json:"budget_ms,omitempty"`
//	    MaxConversations int                            `json:"max_conversations,omitempty"`
//	}
//	type KnownConversation struct {
//	    ID         string `json:"id"`
//	    LastSeenTS string `json:"last_seen_ts,omitempty"`
//	    Name       string `json:"name,omitempty"`
//	}
//	type KnownConversationRow struct { WorkspaceID, ConversationID, Name string; LastReadAt time.Time }
//	type ExportBudget struct { BudgetMS, MaxConversations int }
//	const DefaultExportBudgetMS = 900000   // 15 min (review: 20 left too little of the 30-min run)
//	const DefaultExportMaxConversations = 60
//	func ExportBudgetFromEnv() ExportBudget   // SLACK_WEB_EXPORT_BUDGET_MS, SLACK_WEB_EXPORT_MAX_CONVERSATIONS
//	func BuildExportRequest(rows []KnownConversationRow, budget ExportBudget) (ExportRequest, []KnownConversationRow /*dropped*/)
//
//	Ingest: rows := sink.KnownConversations(ctx);
//	        req, dropped := BuildExportRequest(rows, ExportBudgetFromEnv()); log dropped;
//	        source.Export(ctx, req)
//
// LastReadAt is when a run last READ the conversation (SWT-39 review: the leaf
// sorts known-unenumerated conversations least-recently-read first, so the ts
// must be a last-read time, never the newest message's ts). The builder renders
// it in Slack ts form ("1789073346.665869").
//
// EXPECTED RED: compile failure (every symbol above is undefined).

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

var (
	leafWorkspaceKey = regexp.MustCompile(`^T[A-Z0-9]{5,}$`)
	leafConversation = regexp.MustCompile(`^[CDG][A-Z0-9]{5,}$`)
	leafSlackTS      = regexp.MustCompile(`^\d+(\.\d+)?$`)
)

func swt39KnownByID(t *testing.T, req slackweb.ExportRequest, workspaceID string) map[string]slackweb.KnownConversation {
	t.Helper()
	out := map[string]slackweb.KnownConversation{}
	for _, k := range req.Known[workspaceID] {
		if _, dup := out[k.ID]; dup {
			t.Errorf("known[%s] lists %s twice", workspaceID, k.ID)
		}
		out[k.ID] = k
	}
	return out
}

// The body builder: ts conversion, name carried through, and every row that
// would fail the leaf's regexes dropped rather than sent. The ids are prod's
// (T0HPR78RX = Collaboratory, D0AUD86LKGA = asunda45, the DM this bug is about).
func TestRegression_SWT39_BuildExportRequestConvertsTsAndDropsInvalidIDs(t *testing.T) {
	good := []slackweb.KnownConversationRow{
		{WorkspaceID: "T0HPR78RX", ConversationID: "D0AUD86LKGA", Name: "asunda45", LastReadAt: time.Unix(1789073346, 665869000)},
		{WorkspaceID: "T0HPR78RX", ConversationID: "C03J2KTN1PD", Name: "rd-asu-collaboratory", LastReadAt: time.Unix(1788998471, 987859000)},
		// Known conversation no run has recorded reading: still sent, no ts (the leaf reads it first).
		{WorkspaceID: "T0HPR78RX", ConversationID: "D0B6FV6HFSR", Name: "byeluri"},
		// Same in another workspace.
		{WorkspaceID: "T0360B84U", ConversationID: "GABCDEF12", Name: "avviato-ops"},
	}
	bad := []slackweb.KnownConversationRow{
		{WorkspaceID: "T123", ConversationID: "CABCDEF12", Name: "workspace id too short"},
		{WorkspaceID: "", ConversationID: "CABCDEF12", Name: "no workspace id"},
		{WorkspaceID: "t0hpr78rx", ConversationID: "CABCDEF12", Name: "lowercase workspace id"},
		{WorkspaceID: "T0HPR78RX", ConversationID: "C456", Name: "conversation id too short"},
		{WorkspaceID: "T0HPR78RX", ConversationID: "U0ABCDEF1", Name: "a user id, not a conversation"},
		{WorkspaceID: "T0HPR78RX", ConversationID: "D0aud86lkga", Name: "lowercase conversation id"},
	}
	rows := append(append([]slackweb.KnownConversationRow{}, good[:2]...), bad...)
	rows = append(rows, good[2:]...)

	req, dropped := slackweb.BuildExportRequest(rows, slackweb.ExportBudget{BudgetMS: 1200000, MaxConversations: 60})

	if req.BudgetMS != 1200000 || req.MaxConversations != 60 {
		t.Errorf("budget = %d ms / %d conversations, want the ExportBudget passed in (1200000 / 60)",
			req.BudgetMS, req.MaxConversations)
	}
	if len(dropped) != len(bad) {
		t.Errorf("dropped %d rows, want the %d that fail the leaf's regexes: %+v", len(dropped), len(bad), dropped)
	}

	keys := make([]string, 0, len(req.Known))
	for k := range req.Known {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"T0360B84U", "T0HPR78RX"}) {
		t.Fatalf("known workspace keys = %v, want [T0360B84U T0HPR78RX]. Every other key fails "+
			"/^T[A-Z0-9]{5,}$/ and would 500 the whole export", keys)
	}

	collab := swt39KnownByID(t, req, "T0HPR78RX")
	if len(collab) != 3 {
		t.Errorf("known[T0HPR78RX] = %+v, want exactly the 3 valid conversations", req.Known["T0HPR78RX"])
	}
	if got := collab["D0AUD86LKGA"]; got.LastSeenTS != "1789073346.665869" || got.Name != "asunda45" {
		t.Errorf("asunda45 = %+v, want last_seen_ts 1789073346.665869 (its last-read time) and name asunda45", got)
	}
	if got := collab["C03J2KTN1PD"]; got.LastSeenTS != "1788998471.987859" {
		t.Errorf("C03J2KTN1PD last_seen_ts = %q, want 1788998471.987859", got.LastSeenTS)
	}
	if got, ok := collab["D0B6FV6HFSR"]; !ok || got.LastSeenTS != "" {
		t.Errorf("byeluri = %+v (present=%v), want present with NO last_seen_ts (never recorded as read)", got, ok)
	}
	if got, ok := swt39KnownByID(t, req, "T0360B84U")["GABCDEF12"]; !ok || got.LastSeenTS != "" {
		t.Errorf("GABCDEF12 = %+v (present=%v), want present with NO last_seen_ts (never recorded as read)", got, ok)
	}

	// Property check on the whole output, in the leaf's own terms.
	for ws, list := range req.Known {
		if !leafWorkspaceKey.MatchString(ws) {
			t.Errorf("sent workspace key %q fails the leaf's regex", ws)
		}
		for _, k := range list {
			if !leafConversation.MatchString(k.ID) {
				t.Errorf("sent conversation id %q fails the leaf's regex", k.ID)
			}
			if k.LastSeenTS != "" && !leafSlackTS.MatchString(k.LastSeenTS) {
				t.Errorf("sent last_seen_ts %q for %s fails the leaf's regex", k.LastSeenTS, k.ID)
			}
		}
	}
}

// On the wire, an empty optional field must be ABSENT. The leaf rejects
// "last_seen_ts": "" with a 500 (it fails /^\d+(\.\d+)?$/), which would stop
// the export for every workspace.
func TestRegression_SWT39_BuildExportRequestOmitsEmptyOptionalFields(t *testing.T) {
	req, _ := slackweb.BuildExportRequest([]slackweb.KnownConversationRow{
		{WorkspaceID: "T0HPR78RX", ConversationID: "D0B6FV6HFSR"},
	}, slackweb.ExportBudget{BudgetMS: 1200000, MaxConversations: 60})
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var body struct {
		Known map[string][]map[string]any `json:"known"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request %s: %v", raw, err)
	}
	entries := body.Known["T0HPR78RX"]
	if len(entries) != 1 {
		t.Fatalf("known[T0HPR78RX] = %v, want one entry. Body: %s", entries, raw)
	}
	if _, ok := entries[0]["last_seen_ts"]; ok {
		t.Errorf("entry carries last_seen_ts with no stored message: %s. \"\" fails the leaf's regex -> 500", raw)
	}
	if _, ok := entries[0]["name"]; ok {
		t.Errorf("entry carries an empty name: %s", raw)
	}
}

// Budget defaults and the env override, following the connector's convention
// (UnconfirmedFlagPasses): unparseable or non-positive falls back to the
// default. For this knob that fallback is load-bearing, not cosmetic: 0 or a
// negative number fails the leaf's positive-integer check and 500s the export.
func TestRegression_SWT39_ExportBudgetFromEnv(t *testing.T) {
	if slackweb.DefaultExportBudgetMS != 900000 || slackweb.DefaultExportMaxConversations != 60 {
		t.Errorf("defaults = %d ms / %d conversations, want 900000 / 60",
			slackweb.DefaultExportBudgetMS, slackweb.DefaultExportMaxConversations)
	}
	t.Run("unset -> defaults", func(t *testing.T) {
		t.Setenv("SLACK_WEB_EXPORT_BUDGET_MS", "")
		t.Setenv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", "")
		if got := slackweb.ExportBudgetFromEnv(); got.BudgetMS != 900000 || got.MaxConversations != 60 {
			t.Errorf("ExportBudgetFromEnv() = %+v, want {900000 60}", got)
		}
	})
	t.Run("override honoured", func(t *testing.T) {
		t.Setenv("SLACK_WEB_EXPORT_BUDGET_MS", "900000")
		t.Setenv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", "40")
		if got := slackweb.ExportBudgetFromEnv(); got.BudgetMS != 900000 || got.MaxConversations != 40 {
			t.Errorf("ExportBudgetFromEnv() = %+v, want {900000 40}", got)
		}
	})
	t.Run("garbage and non-positive fall back", func(t *testing.T) {
		for _, v := range []string{"0", "-5", "20m", "lots"} {
			t.Setenv("SLACK_WEB_EXPORT_BUDGET_MS", v)
			t.Setenv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", v)
			if got := slackweb.ExportBudgetFromEnv(); got.BudgetMS != 900000 || got.MaxConversations != 60 {
				t.Errorf("ExportBudgetFromEnv() with %q = %+v, want the defaults {900000 60}", v, got)
			}
		}
	})
}

// The wiring: Ingest loads the known set from its sink, filters it, and hands
// it with the budget to the source. Before the fix the export request did not
// exist at all and the leaf read only what enumeration found.
func TestRegression_SWT39_IngestSendsKnownSetAndBudgetToSource(t *testing.T) {
	t.Setenv("SLACK_WEB_EXPORT_BUDGET_MS", "")
	t.Setenv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", "")
	sink := newSWT39Sink()
	sink.known = []slackweb.KnownConversationRow{
		{WorkspaceID: "T0HPR78RX", ConversationID: "D0AUD86LKGA", Name: "asunda45", LastReadAt: time.Unix(1789073346, 665869000)},
		{WorkspaceID: "T0HPR78RX", ConversationID: "C456", Name: "legacy fixture id, fails the leaf regex"},
	}
	source := &swt39Source{export: slackweb.Export{SchemaVersion: slackweb.SchemaVersion}}

	if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(source.requests) != 1 {
		t.Fatalf("source.Export called %d times, want 1", len(source.requests))
	}
	req := source.requests[0]
	if req.BudgetMS != 900000 || req.MaxConversations != 60 {
		t.Errorf("request budget = %d / %d, want the defaults 900000 / 60", req.BudgetMS, req.MaxConversations)
	}
	collab := swt39KnownByID(t, req, "T0HPR78RX")
	if got, ok := collab["D0AUD86LKGA"]; !ok || got.LastSeenTS != "1789073346.665869" {
		t.Errorf("known[T0HPR78RX] = %+v, want asunda45 D0AUD86LKGA with last_seen_ts 1789073346.665869. "+
			"This is the conversation 34 `ok` runs never read", req.Known["T0HPR78RX"])
	}
	if _, ok := collab["C456"]; ok {
		t.Errorf("known[T0HPR78RX] includes C456, which fails the leaf's id regex and would 500 the export")
	}
}

// The known set cannot be loaded (db hiccup): Ingest still exports, with the
// budget and no known list — coverage degrades to enumeration, ingestion does
// not stop (SWT-39 review, LOW 8).
func TestRegression_SWT39_IngestDegradesWhenKnownSetFailsToLoad(t *testing.T) {
	t.Setenv("SLACK_WEB_EXPORT_BUDGET_MS", "")
	t.Setenv("SLACK_WEB_EXPORT_MAX_CONVERSATIONS", "")
	sink := newSWT39Sink()
	sink.knownErr = errSWT39KnownLoad
	source := &swt39Source{export: slackweb.Export{SchemaVersion: slackweb.SchemaVersion}}
	if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
		t.Fatalf("Ingest with a failing known-set load: %v (want it to export anyway)", err)
	}
	if len(source.requests) != 1 {
		t.Fatalf("source.Export called %d times, want 1", len(source.requests))
	}
	req := source.requests[0]
	if req.Known != nil {
		t.Errorf("known = %+v after a failed load, want none", req.Known)
	}
	if req.BudgetMS != 900000 || req.MaxConversations != 60 {
		t.Errorf("budget = %d / %d, want the defaults still sent", req.BudgetMS, req.MaxConversations)
	}
}

// slackTS form: epoch seconds, six fractional digits, matching the leaf's regex.
func TestRegression_SWT39_LastReadRendersAsSlackTS(t *testing.T) {
	req, _ := slackweb.BuildExportRequest([]slackweb.KnownConversationRow{
		{WorkspaceID: "T0HPR78RX", ConversationID: "D0AUD86LKGA", LastReadAt: time.Unix(1789099200, 0)},
	}, slackweb.ExportBudget{BudgetMS: 900000, MaxConversations: 60})
	if got := req.Known["T0HPR78RX"][0].LastSeenTS; got != "1789099200.000000" || !leafSlackTS.MatchString(got) {
		t.Errorf("last_seen_ts = %q, want 1789099200.000000", got)
	}
}
