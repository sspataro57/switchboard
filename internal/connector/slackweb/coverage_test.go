package slackweb_test

// Regression tests for bug slackweb-collab-export-stale (Jira SWT-39),
// switchboard's half, fix D: decode the leaf's coverage fields, record them in
// the run's sync_runs.stats, and stop calling a partial run `ok`.
// docs/bugs/slackweb-collab-export-stale_DIAGNOSIS.md.
//
// THE BUG. The leaf exports only the conversations it can scrape from the
// Slack UI in a run: 6-8 of Collaboratory's 38. Ingest counted whatever subset
// arrived and called FinishRun(..., "ok"), so every run read `ok` while a
// message sat unexported for 34 runs (raw 77849, asunda45, 17h late). Nothing
// on either side flagged the gap.
//
// ZERO I/O. Exports are decoded from JSON in the leaf's own wire shape
// (slackconnector src/switchboard/export.ts, branch fix/swt-39-export-coverage
// @ 050ed8c), so these tests pin the WIRE contract, not a Go literal of it.
// Run stats are inspected as the JSON FinishRun would merge into
// sync_runs.stats, because that column is what ReconcileUnconfirmed and the
// dashboard read.
//
// IMPOSED SURFACE (the leaf fixes the wire names; switchboard's Go names are
// chosen here):
//
//	type Workspace struct { ...existing...
//	    Enumerated []EnumeratedConversation `json:"enumerated,omitempty"`
//	    Read       []string                 `json:"read,omitempty"`
//	    Unreadable []UnreadableConversation `json:"unreadable,omitempty"`
//	    Deferred   []string                 `json:"deferred,omitempty"`
//	    Coverage   *Coverage                `json:"coverage,omitempty"` // pointer or value: tests compile either way
//	}
//	type EnumeratedConversation struct { ID, Name, Type, URL, Source string; Rank int }
//	type UnreadableConversation struct { ID, Name, Code, Reason string }
//	type Coverage struct { EnumeratedCount, KnownCount, ReadCount, UnreadableCount,
//	                       DeferredCount int; BudgetExhausted bool; ElapsedMS int }
//
//	Source.Export(ctx, ExportRequest) (Export, error)                    // fix F
//	Sink.KnownConversations(ctx) ([]KnownConversationRow, error)         // fix F
//	Sink.FinishRun(ctx, runID, status, stats Stats, errMsg)              // UNCHANGED
//
//	sync_runs.stats keys written by Ingest (merged over {"phase":"slack_web"}):
//	    "read":       [conversation ids]           — present, even when EMPTY, whenever the leaf sent coverage
//	    "deferred":   [conversation ids]
//	    "unreadable": [{"id","name","code","reason"}]
//	    "coverage":   {"enumerated_count","known_count","read_count",
//	                   "unreadable_count","deferred_count","budget_exhausted","elapsed_ms"}
//
//	run status: "partial" when deferred or unreadable is non-empty, or
//	coverage.budget_exhausted; "ok" otherwise. An export WITHOUT the new fields
//	(the old leaf) stays "ok".
//
// EXPECTED RED: compile failure. ExportRequest and KnownConversationRow are
// undefined, Source.Export takes no request, and Workspace has no coverage
// fields.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// ---- fakes shared with export_request_test.go and the integration file --------

// swt39Source records every request it is handed and returns a fixed export.
type swt39Source struct {
	export   slackweb.Export
	requests []slackweb.ExportRequest
}

func (s *swt39Source) Export(_ context.Context, req slackweb.ExportRequest) (slackweb.Export, error) {
	s.requests = append(s.requests, req)
	return s.export, nil
}

type swt39FinishedRun struct {
	workspaceID string
	status      string
	stats       map[string]json.RawMessage
	errMsg      string
}

// swt39Sink keeps one run per workspace and captures FinishRun's stats as the
// JSON PGSink.FinishRun merges into sync_runs.stats.
type swt39Sink struct {
	known       []slackweb.KnownConversationRow
	knownErr    error
	accountOf   map[string]int64
	workspaceOf map[int64]string
	runAccount  map[int64]int64
	nextID      int64
	stored      map[string]string
	runs        []swt39FinishedRun
}

func newSWT39Sink() *swt39Sink {
	return &swt39Sink{
		accountOf:   map[string]int64{},
		workspaceOf: map[int64]string{},
		runAccount:  map[int64]int64{},
		stored:      map[string]string{},
	}
}

func (s *swt39Sink) KnownConversations(context.Context) ([]slackweb.KnownConversationRow, error) {
	return s.known, s.knownErr
}

var errSWT39KnownLoad = errors.New("known-set load failed")

func (s *swt39Sink) EnsureAccount(_ context.Context, workspace slackweb.Workspace) (int64, error) {
	if id, ok := s.accountOf[workspace.ID]; ok {
		return id, nil
	}
	s.nextID++
	s.accountOf[workspace.ID] = s.nextID
	s.workspaceOf[s.nextID] = workspace.ID
	return s.nextID, nil
}

func (s *swt39Sink) StartRun(_ context.Context, accountID int64, _ time.Time) (int64, error) {
	s.nextID++
	s.runAccount[s.nextID] = accountID
	return s.nextID, nil
}

func (s *swt39Sink) RawHash(_ context.Context, accountID int64, externalID string) (string, bool, error) {
	h, ok := s.stored[fmt.Sprint(accountID, "/", externalID)]
	return h, ok, nil
}

func (s *swt39Sink) InsertRaw(_ context.Context, accountID int64, externalID string, _ json.RawMessage, hash string) error {
	s.stored[fmt.Sprint(accountID, "/", externalID)] = hash
	return nil
}

func (s *swt39Sink) UpdateRaw(_ context.Context, accountID int64, externalID string, _ json.RawMessage, hash string) error {
	s.stored[fmt.Sprint(accountID, "/", externalID)] = hash
	return nil
}

func (s *swt39Sink) FinishRun(_ context.Context, runID int64, status string, stats slackweb.Stats, errMsg string) error {
	raw, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	s.runs = append(s.runs, swt39FinishedRun{
		workspaceID: s.workspaceOf[s.runAccount[runID]],
		status:      status,
		stats:       fields,
		errMsg:      errMsg,
	})
	return nil
}

// runFor returns the ONE finished run Ingest wrote for a workspace.
func (s *swt39Sink) runFor(t *testing.T, workspaceID string) swt39FinishedRun {
	t.Helper()
	var found []swt39FinishedRun
	for _, r := range s.runs {
		if r.workspaceID == workspaceID {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("workspace %s has %d finished runs, want exactly 1 (runs: %+v)", workspaceID, len(found), s.runs)
	}
	return found[0]
}

// ---- leaf-shaped export builders ------------------------------------------------

func swt39ConvType(id string) string {
	switch id[0] {
	case 'D':
		return "dm"
	case 'G':
		return "private_channel"
	default:
		return "public_channel"
	}
}

// swt39Workspace renders one workspace in the leaf's JSON shape. conversations
// are the ones actually returned (each with one inbound message);
// coverageFields is "" for the OLD leaf, or the output of swt39CoverageFields.
func swt39Workspace(t *testing.T, workspaceID string, conversations []string, coverageFields string) string {
	t.Helper()
	convs := make([]map[string]any, 0, len(conversations))
	for _, id := range conversations {
		convs = append(convs, map[string]any{
			"id": id, "name": "conv-" + strings.ToLower(id), "type": swt39ConvType(id),
			"url":                   "https://app.slack.com/client/" + workspaceID + "/" + id,
			"skipped_message_count": 0,
			"messages": []map[string]any{{
				"id": "p1789073346665869", "timestamp": "2026-09-10T20:49:06.665Z",
				"author": "asunda45", "author_id": "U0CLIENT01", "direction": "inbound",
				"text": "it is now ready for review and merge",
			}},
		})
	}
	base, err := json.Marshal(map[string]any{
		"id": workspaceID, "name": "Collaboratory/LlamaSite",
		"url":         "https://app.slack.com/client/" + workspaceID,
		"own_user_id": "U0OWNER001", "conversations": convs,
	})
	if err != nil {
		t.Fatalf("marshal workspace: %v", err)
	}
	if coverageFields == "" {
		return string(base)
	}
	return strings.TrimSuffix(string(base), "}") + "," + coverageFields + "}"
}

// swt39CoverageFields renders the leaf's five per-workspace coverage fields
// (without braces, for splicing into a workspace object). Empty lists render
// as [] — never null — exactly as the leaf sends them.
func swt39CoverageFields(t *testing.T, workspaceID string, read, deferred []string, unreadable []map[string]string, known int, exhausted bool) string {
	t.Helper()
	if read == nil {
		read = []string{}
	}
	if deferred == nil {
		deferred = []string{}
	}
	if unreadable == nil {
		unreadable = []map[string]string{}
	}
	var enumerated []map[string]any
	add := func(id, name string) {
		enumerated = append(enumerated, map[string]any{
			"id": id, "name": name, "type": swt39ConvType(id), "source": "dms",
			"url": "https://app.slack.com/client/" + workspaceID + "/" + id,
		})
	}
	for _, id := range read {
		add(id, "conv-"+strings.ToLower(id))
	}
	for _, id := range deferred {
		add(id, "conv-"+strings.ToLower(id))
	}
	for _, u := range unreadable {
		add(u["id"], u["name"])
	}
	if enumerated == nil {
		enumerated = []map[string]any{}
	}
	out, err := json.Marshal(map[string]any{
		"enumerated": enumerated, "read": read, "unreadable": unreadable, "deferred": deferred,
		"coverage": map[string]any{
			"enumerated_count": len(enumerated), "known_count": known, "read_count": len(read),
			"unreadable_count": len(unreadable), "deferred_count": len(deferred),
			"budget_exhausted": exhausted, "elapsed_ms": 5210,
		},
	})
	if err != nil {
		t.Fatalf("marshal coverage: %v", err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(out), "{"), "}")
}

func swt39Export(t *testing.T, workspaces ...string) slackweb.Export {
	t.Helper()
	doc := `{"schema_version":1,"workspaces":[` + strings.Join(workspaces, ",") + `]}`
	var exported slackweb.Export
	if err := json.Unmarshal([]byte(doc), &exported); err != nil {
		t.Fatalf("decode leaf-shaped export: %v\n%s", err, doc)
	}
	return exported
}

func swt39StringList(t *testing.T, stats map[string]json.RawMessage, key string) ([]string, bool) {
	t.Helper()
	raw, ok := stats[key]
	if !ok {
		return nil, false
	}
	if string(raw) == "null" {
		t.Fatalf("stats[%q] is JSON null. A present-but-null read set is neither 'legacy, no read key' nor "+
			"'read these conversations' — ReconcileUnconfirmed cannot tell what it means. Omit the key or "+
			"write an array", key)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("stats[%q] = %s, want a JSON array of conversation ids: %v", key, raw, err)
	}
	return out, true
}

func swt39Sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ---- decode -----------------------------------------------------------------

// The leaf's per-workspace additions, verbatim from export.ts
// SwitchboardExportWorkspace, must survive decoding. Before the fix,
// encoding/json silently drops every one of them, so switchboard cannot even
// see the shortfall the leaf now reports.
func TestRegression_SWT39_ExportDecodesLeafCoverageFields(t *testing.T) {
	const doc = `{"schema_version":1,"workspaces":[{
	  "id":"T0HPR78RX","name":"Collaboratory/LlamaSite","url":"https://app.slack.com/client/T0HPR78RX",
	  "own_user_id":"U0OWNER001","conversations":[],
	  "enumerated":[
	    {"id":"D0B6FV6HFSR","name":"byeluri","type":"dm","url":"https://app.slack.com/client/T0HPR78RX/D0B6FV6HFSR","source":"dms","rank":2},
	    {"id":"D0AUD86LKGA","name":"asunda45","type":"dm","url":"https://app.slack.com/client/T0HPR78RX/D0AUD86LKGA","source":"known"},
	    {"id":"C03J2KTN1PD","name":"rd-asu-collaboratory","type":"public_channel","url":"https://app.slack.com/client/T0HPR78RX/C03J2KTN1PD","source":"sidebar"}
	  ],
	  "read":["D0B6FV6HFSR"],
	  "unreadable":[{"id":"D0AUD86LKGA","name":"asunda45","code":"CHANNEL_NOT_FOUND","reason":"conversation did not open"}],
	  "deferred":["C03J2KTN1PD"],
	  "coverage":{"enumerated_count":3,"known_count":38,"read_count":1,"unreadable_count":1,
	              "deferred_count":1,"budget_exhausted":true,"elapsed_ms":1200417}
	}]}`
	var exported slackweb.Export
	if err := json.Unmarshal([]byte(doc), &exported); err != nil {
		t.Fatalf("decode leaf export with coverage: %v", err)
	}
	ws := exported.Workspaces[0]

	if len(ws.Enumerated) != 3 {
		t.Fatalf("Enumerated = %+v, want the leaf's 3 entries", ws.Enumerated)
	}
	if e := ws.Enumerated[0]; e.ID != "D0B6FV6HFSR" || e.Name != "byeluri" || e.Type != "dm" ||
		e.URL != "https://app.slack.com/client/T0HPR78RX/D0B6FV6HFSR" || e.Source != "dms" || e.Rank != 2 {
		t.Errorf("Enumerated[0] = %+v, want byeluri / dm / source dms / rank 2", e)
	}
	if e := ws.Enumerated[1]; e.Source != "known" || e.Rank != 0 {
		t.Errorf("Enumerated[1] = %+v, want source known with no rank", e)
	}
	if !reflect.DeepEqual(ws.Read, []string{"D0B6FV6HFSR"}) {
		t.Errorf("Read = %v, want [D0B6FV6HFSR]", ws.Read)
	}
	if len(ws.Unreadable) != 1 || ws.Unreadable[0].ID != "D0AUD86LKGA" || ws.Unreadable[0].Name != "asunda45" ||
		ws.Unreadable[0].Code != "CHANNEL_NOT_FOUND" || ws.Unreadable[0].Reason != "conversation did not open" {
		t.Errorf("Unreadable = %+v, want the one asunda45 entry with code and reason", ws.Unreadable)
	}
	if !reflect.DeepEqual(ws.Deferred, []string{"C03J2KTN1PD"}) {
		t.Errorf("Deferred = %v, want [C03J2KTN1PD]", ws.Deferred)
	}
	cov := ws.Coverage
	if cov.EnumeratedCount != 3 || cov.KnownCount != 38 || cov.ReadCount != 1 || cov.UnreadableCount != 1 ||
		cov.DeferredCount != 1 || !cov.BudgetExhausted || cov.ElapsedMS != 1200417 {
		t.Errorf("Coverage = %+v, want 3/38/1/1/1, budget_exhausted, 1200417ms", cov)
	}
}

// Backward compatibility: the export an old leaf sends (no coverage fields)
// still decodes, and decodes to "no coverage information", not to "read nothing".
func TestRegression_SWT39_ExportWithoutCoverageFieldsStillDecodes(t *testing.T) {
	const doc = `{"schema_version":1,"workspaces":[{"id":"T0360B84U","name":"Avviato",
	  "url":"https://app.slack.com/client/T0360B84U","own_user_id":"U0OWNER001","conversations":[]}]}`
	var exported slackweb.Export
	if err := json.Unmarshal([]byte(doc), &exported); err != nil {
		t.Fatalf("an old-leaf export no longer decodes: %v", err)
	}
	ws := exported.Workspaces[0]
	if ws.Read != nil || len(ws.Enumerated) != 0 || len(ws.Unreadable) != 0 || len(ws.Deferred) != 0 {
		t.Errorf("an old-leaf export decoded with coverage lists: read=%v enumerated=%v unreadable=%v deferred=%v",
			ws.Read, ws.Enumerated, ws.Unreadable, ws.Deferred)
	}
}

// ---- run status --------------------------------------------------------------

// THE CORE OF FIX D. A run is `ok` only when the leaf says nothing in scope was
// left out. Before the fix every case below finishes "ok", which is exactly
// how 30 consecutive runs covering 7 of 38 conversations read as healthy.
func TestRegression_SWT39_IngestRunStatusFollowsCoverage(t *testing.T) {
	const ws = "TSWT39AA"
	const conv = "CSWT39AA1"
	cases := []struct {
		name     string
		coverage func(t *testing.T) string
		want     string
		why      string
	}{
		{
			name:     "old leaf, no coverage fields",
			coverage: func(*testing.T) string { return "" },
			want:     "ok",
			why:      "backward compatibility: an export without the new fields carries no evidence of a gap",
		},
		{
			name: "full coverage",
			coverage: func(t *testing.T) string {
				return swt39CoverageFields(t, ws, []string{conv}, nil, nil, 1, false)
			},
			want: "ok",
			why:  "everything in scope was read",
		},
		{
			name: "deferred conversations",
			coverage: func(t *testing.T) string {
				return swt39CoverageFields(t, ws, []string{conv}, []string{"DSWT39DEF", "GSWT39DE2"}, nil, 3, false)
			},
			want: "partial",
			why:  "the budget ran out before these were attempted, so their messages are not in this run",
		},
		{
			name: "unreadable conversation",
			coverage: func(t *testing.T) string {
				return swt39CoverageFields(t, ws, []string{conv}, nil,
					[]map[string]string{{"id": "DSWT39BAD", "name": "asunda45", "reason": "conversation did not open"}}, 2, false)
			},
			want: "partial",
			why:  "an enumerated conversation whose read failed used to be a mini-only warn and a silent `ok`",
		},
		{
			name: "budget exhausted with nothing listed",
			coverage: func(t *testing.T) string {
				return swt39CoverageFields(t, ws, []string{conv}, nil, nil, 1, true)
			},
			want: "partial",
			why:  "the leaf says reads stopped for budget; the run cannot claim completeness",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newSWT39Sink()
			source := &swt39Source{export: swt39Export(t, swt39Workspace(t, ws, []string{conv}, tc.coverage(t)))}
			if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
				t.Fatalf("Ingest: %v (a partial run is not a failed run; Ingest must not error)", err)
			}
			run := sink.runFor(t, ws)
			if run.status != tc.want {
				t.Errorf("run status = %q, want %q: %s", run.status, tc.want, tc.why)
			}
			if run.errMsg != "" {
				t.Errorf("run error = %q, want none; partial coverage is recorded in stats, not as an error", run.errMsg)
			}
			if got := len(sink.stored); got != 2 {
				t.Errorf("raw rows written = %d, want 2 (conversation + message). What WAS read is still "+
					"ingested raw-first; partial only changes the verdict", got)
			}
		})
	}
}

// Status is decided per workspace run. A partial Collaboratory must not drag an
// Avviato that was read in full down with it, and vice versa. Partial first, so
// a status variable that is not reset per workspace shows up.
func TestRegression_SWT39_IngestRunStatusIsPerWorkspace(t *testing.T) {
	sink := newSWT39Sink()
	source := &swt39Source{export: swt39Export(t,
		swt39Workspace(t, "TSWT39AA", []string{"CSWT39AA1"},
			swt39CoverageFields(t, "TSWT39AA", []string{"CSWT39AA1"}, []string{"DSWT39AA2"}, nil, 2, false)),
		swt39Workspace(t, "TSWT39BB", []string{"CSWT39BB1"},
			swt39CoverageFields(t, "TSWT39BB", []string{"CSWT39BB1"}, nil, nil, 1, false)),
	)}
	if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := sink.runFor(t, "TSWT39AA").status; got != "partial" {
		t.Errorf("TSWT39AA (one deferred) status = %q, want partial", got)
	}
	if got := sink.runFor(t, "TSWT39BB").status; got != "ok" {
		t.Errorf("TSWT39BB (fully read) status = %q, want ok", got)
	}
}

// ---- stats ------------------------------------------------------------------

// The run's stats carry what the leaf covered: counts, the read ids, the
// deferred ids and the unreadable entries. Fix E (ReconcileUnconfirmed) reads
// `read` from this column, and the dashboard reads the counts.
func TestRegression_SWT39_IngestRecordsCoverageInRunStats(t *testing.T) {
	const ws = "TSWT39AA"
	sink := newSWT39Sink()
	unreadable := []map[string]string{{"id": "DSWT39BAD", "name": "asunda45", "code": "SLACK_TIMEOUT", "reason": "timed out opening"}}
	source := &swt39Source{export: swt39Export(t, swt39Workspace(t, ws, []string{"CSWT39AA1"},
		swt39CoverageFields(t, ws, []string{"CSWT39AA1"}, []string{"DSWT39DEF", "GSWT39DE2"}, unreadable, 38, true)))}
	if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	stats := sink.runFor(t, ws).stats

	read, ok := swt39StringList(t, stats, "read")
	if !ok || !reflect.DeepEqual(read, []string{"CSWT39AA1"}) {
		t.Errorf("stats.read = %v (present=%v), want [CSWT39AA1]. ReconcileUnconfirmed decides from this "+
			"list whether a pass could have observed a send", read, ok)
	}
	deferred, ok := swt39StringList(t, stats, "deferred")
	if !ok || !reflect.DeepEqual(swt39Sorted(deferred), []string{"DSWT39DEF", "GSWT39DE2"}) {
		t.Errorf("stats.deferred = %v (present=%v), want [DSWT39DEF GSWT39DE2]", deferred, ok)
	}

	var gotUnreadable []map[string]string
	if raw, ok := stats["unreadable"]; !ok {
		t.Errorf("stats has no `unreadable` key; the failed read is recorded nowhere but the mini's log")
	} else if err := json.Unmarshal(raw, &gotUnreadable); err != nil {
		t.Errorf("stats.unreadable = %s, want an array of {id,name,code,reason}: %v", raw, err)
	} else if len(gotUnreadable) != 1 || gotUnreadable[0]["id"] != "DSWT39BAD" || gotUnreadable[0]["reason"] != "timed out opening" {
		t.Errorf("stats.unreadable = %v, want the one DSWT39BAD entry with its reason", gotUnreadable)
	}

	// The enumerated list is kept (id, source, rank) so "was it listed, and how
	// high" stays answerable after the run (SWT-39 review, MEDIUM 4).
	var enumerated []map[string]any
	if raw, ok := stats["enumerated"]; !ok {
		t.Errorf("stats has no `enumerated` key; which conversations the leaf listed is recorded nowhere")
	} else if err := json.Unmarshal(raw, &enumerated); err != nil {
		t.Errorf("stats.enumerated = %s: %v", raw, err)
	} else if len(enumerated) != 4 || enumerated[0]["id"] == nil || enumerated[0]["source"] != "dms" {
		t.Errorf("stats.enumerated = %v, want the leaf's 4 entries with id and source", enumerated)
	}

	var cov map[string]any
	if raw, ok := stats["coverage"]; !ok {
		t.Fatalf("stats has no `coverage` key. Stats present: %v", keysOf(stats))
	} else if err := json.Unmarshal(raw, &cov); err != nil {
		t.Fatalf("stats.coverage = %s: %v", raw, err)
	}
	for key, want := range map[string]any{
		"enumerated_count": float64(4), "known_count": float64(38), "read_count": float64(1),
		"unreadable_count": float64(1), "deferred_count": float64(2), "budget_exhausted": true,
	} {
		if cov[key] != want {
			t.Errorf("stats.coverage.%s = %v, want %v", key, cov[key], want)
		}
	}
}

// The trap in `omitempty`: the leaf sends "read": [] when it read nothing (all
// deferred). Dropping the empty list makes the stats look like an OLD-leaf run,
// and ReconcileUnconfirmed counts legacy runs as having observed every
// conversation. A run that read nothing would then count as a pass for every
// unconfirmed send in the workspace.
func TestRegression_SWT39_IngestRecordsAnEmptyReadSetAsEmptyNotAbsent(t *testing.T) {
	const ws = "TSWT39AA"
	sink := newSWT39Sink()
	source := &swt39Source{export: swt39Export(t, swt39Workspace(t, ws, nil,
		swt39CoverageFields(t, ws, nil, []string{"DSWT39DEF"}, nil, 1, true)))}
	if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	run := sink.runFor(t, ws)
	read, ok := swt39StringList(t, run.stats, "read")
	if !ok {
		t.Fatalf("stats has no `read` key for a run whose leaf reported read: []. Absent means 'legacy run, "+
			"count it as a pass for every conversation' to ReconcileUnconfirmed; this run read NOTHING. "+
			"Stats present: %v", keysOf(run.stats))
	}
	if len(read) != 0 {
		t.Errorf("stats.read = %v, want []", read)
	}
	if run.status != "partial" {
		t.Errorf("status = %q, want partial (one deferred, budget exhausted)", run.status)
	}
}

// An OLD-leaf export must not claim an empty read set either: that would make
// every legacy run count as "read nothing" and no unconfirmed send could ever
// be flagged. Either no `read` key (legacy) or the ids actually exported.
func TestRegression_SWT39_IngestLegacyExportClaimsNoEmptyReadSet(t *testing.T) {
	const ws = "TSWT39AA"
	sink := newSWT39Sink()
	source := &swt39Source{export: swt39Export(t, swt39Workspace(t, ws, []string{"CSWT39AA1"}, ""))}
	if _, err := slackweb.Ingest(context.Background(), source, sink); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	run := sink.runFor(t, ws)
	if run.status != "ok" {
		t.Errorf("old-leaf run status = %q, want ok (backward compatibility)", run.status)
	}
	if read, ok := swt39StringList(t, run.stats, "read"); ok && !reflect.DeepEqual(read, []string{"CSWT39AA1"}) {
		t.Errorf("old-leaf run stats.read = %v. Without coverage fields the key must be absent (legacy) or "+
			"hold exactly the exported conversations [CSWT39AA1]", read)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
