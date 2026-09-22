package slackweb

// slack-watch-sweep (SWT-75, docs/tickets/slack-watch-sweep_SPEC.md) criteria 5,
// 6 and 7: the targeted /export request, the rows it may and may not carry, and
// the coverage.mode guard that is the ONLY proof a leaf honoured `targets`.
// Pure: no database, no browser, one httptest server for the wire shape.
//
// IMPOSED SURFACE (D1, D2 and the SPEC's "Leaf HTTP contract" — greenfield, so
// the SPEC's contract defines the signature; the names marked * are chosen here
// because the SPEC fixes the behaviour but not the identifier):
//
//	// internal/connector/slackweb/export_request.go
//	// ExportRequest gains ONE optional field, mutually exclusive with Known:
//	Targets map[string][]string `json:"targets,omitempty"`
//
//	// *WatchRow is one slack_watch row as the sink loads it. LastReadAt comes
//	// from sync_runs, like KnownConversationRow's.
//	type WatchRow struct {
//		ID             int64
//		WorkspaceID    string
//		ConversationID string
//		Label          string
//		Enabled        bool
//		LastReadAt     time.Time
//	}
//
//	// *BuildTargetedRequest is BuildExportRequest's twin: it returns the /export
//	// body for a targeted pass — targets + budget_ms + max_conversations and NO
//	// known — plus the rows it DROPPED for failing the leaf's id rules.
//	func BuildTargetedRequest(rows []WatchRow, budgetMS int) (ExportRequest, []WatchRow)
//
//	// internal/connector/slackweb/types.go
//	Coverage.Mode string `json:"mode,omitempty"`   // "targeted" | "full"
//	const (
//		PhaseSlackWeb        = "slack_web"        // *today's literal, sink.go:128
//		PhaseSlackWebWatch   = "slack_web_watch"  // *D6's new phase
//		CoverageModeTargeted = "targeted"
//		CoverageModeFull     = "full"
//	)
//
//	// *CheckTargetedMode refuses a response that does not report
//	// coverage.mode == "targeted" for every workspace it returned (D8).
//	var ErrNotTargeted error
//	func CheckTargetedMode(exported Export) error
//
// GREENFIELD NOTE, EXPECTED RED: none of these exist, so this file
// compile-FAILs ("undefined: BuildTargetedRequest", "undefined: WatchRow",
// "unknown field Targets"...). That is the expected red state.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - send `known` instead of `targets` on a watch pass -> criterion 5.
//   - accept a response without `coverage.mode` -> criterion 7.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func watchRow(ws, conv string, enabled bool) WatchRow {
	return WatchRow{WorkspaceID: ws, ConversationID: conv, Label: conv, Enabled: enabled}
}

// Criterion 5: {targets, budget_ms, max_conversations} and NO known key.
// `targets` and `known` are mutually exclusive on the leaf (parseExportBody
// rejects both together), and — the reason this is the SPEC's first mutation
// row — a watch pass that sent `known` would run a FULL 15-minute export every
// minute, because `known` only REORDERS the leaf's queue, it never restricts it
// (export.ts:329-585).
func TestBuildTargetedRequest_SendsTargetsAndNeverKnown(t *testing.T) {
	req, dropped := BuildTargetedRequest([]WatchRow{
		watchRow("T0360B84U", "DSAV4HJ2F", true),
		watchRow("T0HPR78RX", "D04F7LXRB8B", true),
	}, 150000)
	if len(dropped) != 0 {
		t.Fatalf("BuildTargetedRequest dropped %v; both ids satisfy the leaf's rules", dropped)
	}
	if req.Known != nil {
		t.Errorf("BuildTargetedRequest set Known=%v. Criterion 5: a targeted request carries NO known — the "+
			"leaf rejects targets+known together, and `known` alone would run a full export every minute", req.Known)
	}
	if req.MaxConversations != 2 {
		t.Errorf("max_conversations = %d, want 2 (D3: len(targets) — the pass reads exactly the watch list)",
			req.MaxConversations)
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the request is not a JSON object: %v", err)
	}
	const wantBody = `{
	  "targets": {"T0360B84U": ["DSAV4HJ2F"], "T0HPR78RX": ["D04F7LXRB8B"]},
	  "budget_ms": 150000,
	  "max_conversations": 2
	}`
	if err := json.Unmarshal([]byte(wantBody), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targeted /export body =\n%s\nwant key for key:\n%s\n(the SPEC's \"Leaf HTTP contract\")", body, wantBody)
	}
}

// Criterion 5, second half: every zero field is OMITTED. The leaf answers
// budget_ms: 0 with a 500 that stops the export for every workspace
// (http-bridge.ts:118-124), so a mis-read env must degrade to "no bound",
// never to a 500 — the positiveEnv discipline, one level up.
func TestBuildTargetedRequest_OmitsZeroFields(t *testing.T) {
	req, _ := BuildTargetedRequest([]WatchRow{watchRow("T0360B84U", "DSAV4HJ2F", true)}, 0)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the request is not a JSON object: %v", err)
	}
	if raw, ok := got["budget_ms"]; ok {
		t.Errorf("a zero budget put budget_ms=%s on the wire; the leaf 500s on a non-positive value and the "+
			"export stops for EVERY workspace (criterion 5)", raw)
	}
	if raw, ok := got["known"]; ok {
		t.Errorf("known=%s is on the wire (criterion 5)", raw)
	}
}

// Criterion 6, the half that needs no database: a DISABLED row is not a target,
// and a row whose ids would fail the leaf's regexes is DROPPED rather than sent
// — loudly, as a return value the caller logs, because BuildExportRequest's
// silent drop (export_request.go:101-104) is exactly the failure D2 moved into
// a database CHECK.
func TestBuildTargetedRequest_SkipsDisabledAndMalformedRows(t *testing.T) {
	rows := []WatchRow{
		watchRow("T0360B84U", "DSAV4HJ2F", true),
		watchRow("T0360B84U", "DPAUSED01", false), // disabled
		watchRow("t0360b84u", "DLOWERWS1", true),  // workspace fails the leaf's rule
		watchRow("T0360B84U", "XYZ", true),        // conversation fails the leaf's rule
		watchRow("T0360B84U", "DSAV4HJ2F", true),  // duplicate of the first
	}
	req, dropped := BuildTargetedRequest(rows, 150000)

	got := req.Targets["T0360B84U"]
	if !reflect.DeepEqual(got, []string{"DSAV4HJ2F"}) {
		t.Errorf("targets[T0360B84U] = %v, want exactly [DSAV4HJ2F]: a disabled row is not swept, a malformed "+
			"id is never sent, and one conversation is listed once (criterion 6)", got)
	}
	if _, ok := req.Targets["t0360b84u"]; ok {
		t.Errorf("targets carries the lowercased workspace %q; the leaf drops it and the pass would then read "+
			"nothing while reporting success", "t0360b84u")
	}
	if len(dropped) != 2 {
		t.Errorf("BuildTargetedRequest dropped %v, want the two malformed rows returned to the caller so it "+
			"can log them by name (criterion 6)", dropped)
	}
	if req.MaxConversations != 1 {
		t.Errorf("max_conversations = %d after the skips, want 1 — it counts what is SENT, not what was loaded",
			req.MaxConversations)
	}
}

// Criterion 6, the rule that keeps the browser idle: an empty enabled set means
// NO targeted pass at all. Here that is "no targets on the wire"; the "no HTTP
// call" half is TestRunTargeted_EmptyWatchListNeverTouchesTheBridge in
// watch_test.go.
func TestBuildTargetedRequest_EmptyEnabledSetIsEmpty(t *testing.T) {
	for name, rows := range map[string][]WatchRow{
		"no rows":      nil,
		"all disabled": {watchRow("T0360B84U", "DSAV4HJ2F", false)},
		"all dropped":  {watchRow("T0360B84U", "XYZ", true)},
	} {
		rows := rows
		t.Run(name, func(t *testing.T) {
			req, _ := BuildTargetedRequest(rows, 150000)
			if len(req.Targets) != 0 {
				t.Errorf("BuildTargetedRequest(%s) = %v targets, want none: an empty watch list must not put "+
					"the browser to work (criterion 6)", name, req.Targets)
			}
			body, _ := json.Marshal(req)
			if strings.Contains(string(body), "targets") {
				t.Errorf("an empty target set still put `targets` on the wire: %s", body)
			}
		})
	}
}

// Criterion 7 / D8: coverage.mode is the ONLY proof the leaf honoured
// `targets`. An old leaf "silently ignores unknown keys" (http-bridge.ts:126-170),
// so it answers a one-minute targeted request with a fifteen-minute FULL export
// — and every minute after that. A response that does not report
// mode == "targeted" for every workspace it returned is REFUSED.
//
// MUTATION: make CheckTargetedMode accept a missing mode and this goes red.
func TestCheckTargetedMode_RefusesAnythingButTargeted(t *testing.T) {
	ws := func(id string, cov *Coverage) Workspace {
		return Workspace{ID: id, Name: id, URL: "https://app.slack.com/client/" + id,
			OwnUserID: "U" + id, Coverage: cov}
	}
	for _, tc := range []struct {
		name    string
		export  Export
		wantErr bool
	}{
		{"both targeted", Export{SchemaVersion: SchemaVersion, Workspaces: []Workspace{
			ws("T0360B84U", &Coverage{Mode: CoverageModeTargeted, ReadCount: 1}),
			ws("T0HPR78RX", &Coverage{Mode: CoverageModeTargeted, ReadCount: 1}),
		}}, false},
		{"an old leaf: no coverage block at all", Export{SchemaVersion: SchemaVersion, Workspaces: []Workspace{
			ws("T0360B84U", nil),
		}}, true},
		{"coverage present, mode absent", Export{SchemaVersion: SchemaVersion, Workspaces: []Workspace{
			ws("T0360B84U", &Coverage{ReadCount: 38}),
		}}, true},
		{"mode full", Export{SchemaVersion: SchemaVersion, Workspaces: []Workspace{
			ws("T0360B84U", &Coverage{Mode: CoverageModeFull, ReadCount: 38}),
		}}, true},
		{"one workspace honoured it, one did not", Export{SchemaVersion: SchemaVersion, Workspaces: []Workspace{
			ws("T0360B84U", &Coverage{Mode: CoverageModeTargeted}),
			ws("T0HPR78RX", &Coverage{Mode: CoverageModeFull}),
		}}, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTargetedMode(tc.export)
			if tc.wantErr && err == nil {
				t.Errorf("CheckTargetedMode(%s) = nil. D8: an old leaf ignores unknown request keys and would "+
					"answer a per-minute targeted request with a full 15-minute export — nothing may be ingested "+
					"from such a response (criterion 7)", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("CheckTargetedMode(%s) = %v, want nil", tc.name, err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrNotTargeted) {
				t.Errorf("CheckTargetedMode(%s) = %v, which is not ErrNotTargeted; the loop tells this refusal "+
					"apart from a bridge failure — it stands targeted passes down and keeps rotating (criterion 7)",
					tc.name, err)
			}
		})
	}
}

// The wire half of criterion 7: `mode` survives the HTTP transport, and
// SchemaVersion stays 1 (the SPEC's "API / MCP tool changes": bumping it would
// force a lockstep deploy of two repos across two machines for one additive
// field).
func TestHTTPBridgeExport_DecodesCoverageMode(t *testing.T) {
	const doc = `{"schema_version":1,"workspaces":[{
	  "id":"T0360B84U","name":"Avviato","url":"https://app.slack.com/client/T0360B84U",
	  "own_user_id":"U0OWNER001","conversations":[],
	  "read":["DSAV4HJ2F"],"deferred":[],"unreadable":[],
	  "coverage":{"enumerated_count":1,"known_count":1,"read_count":1,"unreadable_count":0,
	              "deferred_count":0,"budget_exhausted":false,"elapsed_ms":18000,"mode":"targeted"}
	}]}`
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(doc))
	}))
	defer server.Close()

	bridge, err := NewHTTPBridge(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := BuildTargetedRequest([]WatchRow{watchRow("T0360B84U", "DSAV4HJ2F", true)}, 150000)
	exported, err := bridge.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !strings.Contains(string(gotBody), `"targets"`) {
		t.Errorf("the POSTed body carries no targets: %s", gotBody)
	}
	if exported.SchemaVersion != 1 || SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d/%d, want 1 and 1: the response gains one OPTIONAL field and old binaries "+
			"ignore it, so bumping the version would couple two repos on two machines for nothing",
			exported.SchemaVersion, SchemaVersion)
	}
	if exported.Workspaces[0].Coverage == nil || exported.Workspaces[0].Coverage.Mode != CoverageModeTargeted {
		t.Errorf("Coverage.Mode = %+v, want %q through the HTTP transport (criterion 7 reads it from the "+
			"decoded response, not from a hand-built struct)", exported.Workspaces[0].Coverage, CoverageModeTargeted)
	}
	if err := CheckTargetedMode(exported); err != nil {
		t.Errorf("CheckTargetedMode on a real targeted response = %v, want nil", err)
	}
}

// D6's two phase literals, pinned where both are visible at once. The rotation
// phase must stay byte-identical to today's sink.go:128 literal: every existing
// sync_runs row carries it, and ReconcileUnconfirmed / KnownConversations
// filter on it (criteria 18, 19).
func TestPhaseConstants(t *testing.T) {
	if PhaseSlackWeb != "slack_web" {
		t.Errorf("PhaseSlackWeb = %q, want \"slack_web\" — the literal StartRun has stamped on every run row "+
			"since SWT-12 (sink.go:128). Changing it orphans every historical row from both readers", PhaseSlackWeb)
	}
	if PhaseSlackWebWatch != "slack_web_watch" {
		t.Errorf("PhaseSlackWebWatch = %q, want \"slack_web_watch\" (D6)", PhaseSlackWebWatch)
	}
	if PhaseSlackWeb == PhaseSlackWebWatch {
		t.Fatal("the two phases are the same string; D6's whole point is that a targeted pass is INVISIBLE to " +
			"every existing sync_runs consumer")
	}
	// A prefix relationship is fine for equality filters and fatal for a LIKE.
	// Say it out loud so nobody spells either filter with one.
	if !strings.HasPrefix(PhaseSlackWebWatch, PhaseSlackWeb) {
		t.Logf("note: %q is not a prefix of %q", PhaseSlackWeb, PhaseSlackWebWatch)
	}
}

// LastReadAt is a time.Time on WatchRow, so the /sources panel and
// slack_watch_list read "when each was last read" from sync_runs rather than
// from a column slack_watch would have to maintain.
func TestWatchRow_CarriesLastReadAt(t *testing.T) {
	r := WatchRow{LastReadAt: time.Unix(1789073346, 0)}
	if r.LastReadAt.IsZero() {
		t.Fatal("WatchRow.LastReadAt does not hold a time")
	}
}
