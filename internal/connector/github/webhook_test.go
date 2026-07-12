package github_test

// Offline tests for the GitHub webhook receiver + the pure payload→intent
// mapper (SPEC 09-jira-github-connectors, criteria 10-12, Decision 9). ZERO real
// network: the receiver is exercised via httptest against HMAC-signed fake
// payloads and an injected fake executor/store/resolver — NEVER a live GitHub
// call, and hooksd itself performs no task_events/tasks writes (invariant 3:
// only Executor.Execute mutates, actor hooksd:github).
//
// GREENFIELD NOTE: package internal/connector/github does not exist yet; this
// file compile-FAILs under `go test ./...` until it is implemented — the
// expected failure mode. For greenfield code the SPEC's contract IS the
// signature. Imposed exported surface (the SPEC's mapevent.go + receiver.go):
//
//   // Intent is one spine tool call the receiver/poller will make once it has
//   // resolved a task_id. Tool is the spine tool name; Args are the tool args
//   // WITHOUT task_id (the dispatcher injects the resolved id). Match carries the
//   // PR-resolution coordinates (criterion 11).
//   type Intent struct {
//       Tool  string         // "record_pr_event" | "record_ci_event"
//       Args  map[string]any // tool args minus task_id
//       Match PRRef
//   }
//   type PRRef struct { Repo string; PR int; HeadBranch string } // Repo = "{owner}/{repo}"
//
//   // MapEvent is PURE: (X-GitHub-Event, raw JSON body) -> zero or more intents.
//   // Handled: pull_request (opened/reopened/closed), workflow_run
//   // (requested/in_progress/completed). Everything else (incl. check_suite,
//   // Decision 9) -> nil, nil.
//   func MapEvent(eventType string, payload []byte) ([]Intent, error)
//
//   // The dispatch collaborators the receiver depends on (all faked here):
//   type RawStore interface {
//       // StoreDelivery stores the webhook body raw-first under the synthetic
//       // provider='github' account, external_id=delivery:{guid} (criterion 10).
//       // already=true when the guid was already stored (redelivery dedup).
//       StoreDelivery(ctx context.Context, guid, eventType string, body []byte) (already bool, err error)
//   }
//   type TaskResolver interface {
//       // Resolve maps a PRRef to a task (newest active external_ref, or the
//       // task-{N}-* head-branch fallback). ok=false => not ours: no events.
//       Resolve(ctx context.Context, ref PRRef) (taskID int64, ok bool, err error)
//   }
//   type Dispatcher interface {
//       Execute(ctx context.Context, call executor.Call) (executor.Result, error)
//   }
//
//   // NewReceiver builds the http.Handler: HMAC verify -> raw store -> resolve ->
//   // dispatch. secret is GITHUB_WEBHOOK_SECRET.
//   func NewReceiver(secret string, store RawStore, resolver TaskResolver, ex Dispatcher) http.Handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/executor"
)

const webhookSecret = "s3cr3t-test-key"

// sign computes the GitHub X-Hub-Signature-256 header value for body.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---- payload fixtures ---------------------------------------------------------

func prPayload(action string, number int, merged bool, branch string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"number": number,
		"pull_request": map[string]any{
			"number":   number,
			"merged":   merged,
			"html_url": "https://github.com/sspataro57/switchboard/pull/" + itoaG(number),
			"head":     map[string]any{"ref": branch},
		},
		"repository": map[string]any{"full_name": "sspataro57/switchboard"},
	})
	return b
}

func workflowRunPayload(action, conclusion string, runID int, branch string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"workflow_run": map[string]any{
			"id":          runID,
			"name":        "CI",
			"conclusion":  conclusion,
			"head_branch": branch,
			"html_url":    "https://github.com/sspataro57/switchboard/actions/runs/" + itoaG(runID),
			"pull_requests": []map[string]any{
				{"number": 12},
			},
		},
		"repository": map[string]any{"full_name": "sspataro57/switchboard"},
	})
	return b
}

// ---- MapEvent: pure table tests ----------------------------------------------

func TestMapEvent_PullRequest(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		merged     bool
		wantAction string
	}{
		{"opened", "opened", false, "opened"},
		{"reopened", "reopened", false, "opened"},
		{"closed-merged", "closed", true, "merged"},
		{"closed-unmerged", "closed", false, "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intents, err := github.MapEvent("pull_request", prPayload(tc.action, 12, tc.merged, "task-5-fix"))
			if err != nil {
				t.Fatalf("MapEvent: %v", err)
			}
			if len(intents) != 1 {
				t.Fatalf("want exactly 1 intent, got %d (%+v)", len(intents), intents)
			}
			in := intents[0]
			if in.Tool != "record_pr_event" {
				t.Errorf("Tool = %q, want record_pr_event", in.Tool)
			}
			if got, _ := in.Args["action"].(string); got != tc.wantAction {
				t.Errorf("action arg = %q, want %q", got, tc.wantAction)
			}
			if got := argIntG(t, in.Args, "pr"); got != 12 {
				t.Errorf("pr arg = %d, want 12", got)
			}
			if u, _ := in.Args["url"].(string); !strings.Contains(u, "/pull/12") {
				t.Errorf("url arg = %q, want the PR html_url", u)
			}
			if in.Match.Repo != "sspataro57/switchboard" || in.Match.PR != 12 || in.Match.HeadBranch != "task-5-fix" {
				t.Errorf("Match = %+v, want {sspataro57/switchboard 12 task-5-fix} (criterion 11 coordinates)", in.Match)
			}
		})
	}
}

func TestMapEvent_WorkflowRun(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		conclusion string
		wantPhase  string
		wantConcl  string
	}{
		{"requested", "requested", "", "started", ""},
		{"in_progress", "in_progress", "", "started", ""},
		{"completed-success", "completed", "success", "completed", "success"},
		{"completed-failure", "completed", "failure", "completed", "failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intents, err := github.MapEvent("workflow_run", workflowRunPayload(tc.action, tc.conclusion, 4242, "task-5-fix"))
			if err != nil {
				t.Fatalf("MapEvent: %v", err)
			}
			if len(intents) != 1 {
				t.Fatalf("want exactly 1 intent, got %d (%+v)", len(intents), intents)
			}
			in := intents[0]
			if in.Tool != "record_ci_event" {
				t.Errorf("Tool = %q, want record_ci_event", in.Tool)
			}
			if got, _ := in.Args["phase"].(string); got != tc.wantPhase {
				t.Errorf("phase arg = %q, want %q", got, tc.wantPhase)
			}
			if got := argIntG(t, in.Args, "run_id"); got != 4242 {
				t.Errorf("run_id arg = %d, want 4242", got)
			}
			if tc.wantConcl != "" {
				if got, _ := in.Args["conclusion"].(string); got != tc.wantConcl {
					t.Errorf("conclusion arg = %q, want %q", got, tc.wantConcl)
				}
			}
		})
	}
}

// check_suite adds a second overlapping CI signal for nothing (Decision 9): the
// mapper deliberately ignores it. Unknown events are ignored too.
func TestMapEvent_IgnoredEvents(t *testing.T) {
	for _, et := range []string{"check_suite", "star", "ping", "push"} {
		t.Run(et, func(t *testing.T) {
			intents, err := github.MapEvent(et, []byte(`{"action":"completed"}`))
			if err != nil {
				t.Fatalf("MapEvent(%s): %v", et, err)
			}
			if len(intents) != 0 {
				t.Errorf("event %q must map to zero intents; got %+v", et, intents)
			}
		})
	}
}

// ---- receiver: HMAC + dispatch -----------------------------------------------

type fakeExec struct {
	calls []executor.Call
}

func (f *fakeExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	f.calls = append(f.calls, call)
	return executor.Result{Output: []byte(`{}`)}, nil
}

type fakeRawStore struct {
	stored   []string // external_ids stored (delivery:{guid})
	order    []string // interleaved "store"/"dispatch" trace via shared recorder
	already  bool
	recorder *[]string
}

func (s *fakeRawStore) StoreDelivery(_ context.Context, guid, _ string, _ []byte) (bool, error) {
	s.stored = append(s.stored, "delivery:"+guid)
	if s.recorder != nil {
		*s.recorder = append(*s.recorder, "store")
	}
	return s.already, nil
}

type fakeResolver struct {
	taskID int64
	ok     bool
}

func (r fakeResolver) Resolve(_ context.Context, _ github.PRRef) (int64, bool, error) {
	return r.taskID, r.ok, nil
}

func post(t *testing.T, h http.Handler, event, guid, sig string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", guid)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReceiver_ValidSignatureStoresAndDispatches(t *testing.T) {
	ex := &fakeExec{}
	store := &fakeRawStore{}
	h := github.NewReceiver(webhookSecret, store, fakeResolver{taskID: 5, ok: true}, ex)

	body := prPayload("opened", 12, false, "task-5-fix")
	rec := post(t, h, "pull_request", "guid-abc", sign(webhookSecret, body), body)

	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("valid signature -> status %d, want 2xx", rec.Code)
	}
	// Raw-first: the delivery landed under delivery:{guid}.
	if len(store.stored) != 1 || store.stored[0] != "delivery:guid-abc" {
		t.Errorf("raw store = %v, want [delivery:guid-abc] (raw-first, criterion 10)", store.stored)
	}
	// One dispatched executor call, task_id injected, actor hooksd:github.
	if len(ex.calls) != 1 {
		t.Fatalf("dispatched calls = %d, want 1", len(ex.calls))
	}
	c := ex.calls[0]
	if c.Tool != "record_pr_event" {
		t.Errorf("dispatched tool = %q, want record_pr_event", c.Tool)
	}
	if !strings.HasPrefix(c.Actor, "hooksd:github") {
		t.Errorf("dispatched actor = %q, want hooksd:github prefix (invariant 3)", c.Actor)
	}
	var args map[string]any
	if err := json.Unmarshal(c.Args, &args); err != nil {
		t.Fatalf("dispatched args not JSON: %v", err)
	}
	if got := argIntG(t, args, "task_id"); got != 5 {
		t.Errorf("dispatched task_id = %d, want the resolved task 5 (receiver injects it)", got)
	}
}

func TestReceiver_BadSignatureRejected401(t *testing.T) {
	ex := &fakeExec{}
	store := &fakeRawStore{}
	h := github.NewReceiver(webhookSecret, store, fakeResolver{taskID: 5, ok: true}, ex)

	body := prPayload("opened", 12, false, "task-5-fix")
	rec := post(t, h, "pull_request", "guid-bad", sign("WRONG-KEY", body), body)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad signature -> status %d, want 401", rec.Code)
	}
	if len(store.stored) != 0 {
		t.Errorf("bad signature must not store raw (no processing); stored=%v", store.stored)
	}
	if len(ex.calls) != 0 {
		t.Errorf("bad signature must not dispatch; calls=%v", ex.calls)
	}
}

func TestReceiver_MissingSignatureRejected401(t *testing.T) {
	ex := &fakeExec{}
	store := &fakeRawStore{}
	h := github.NewReceiver(webhookSecret, store, fakeResolver{taskID: 5, ok: true}, ex)

	body := prPayload("opened", 12, false, "task-5-fix")
	rec := post(t, h, "pull_request", "guid-missing", "", body)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing signature -> status %d, want 401", rec.Code)
	}
	if len(store.stored) != 0 || len(ex.calls) != 0 {
		t.Errorf("missing signature must not store or dispatch")
	}
}

// A PR with no matching task (resolver ok=false) is stored raw but produces no
// events (criterion 11: "not ours to route").
func TestReceiver_NoTaskMatchStoresButNoDispatch(t *testing.T) {
	ex := &fakeExec{}
	store := &fakeRawStore{}
	h := github.NewReceiver(webhookSecret, store, fakeResolver{ok: false}, ex)

	body := prPayload("opened", 99, false, "feature/not-ours")
	rec := post(t, h, "pull_request", "guid-nomatch", sign(webhookSecret, body), body)

	if rec.Code < 200 || rec.Code >= 300 {
		t.Errorf("unmatched PR -> status %d, want 2xx (accepted, stored, ignored)", rec.Code)
	}
	if len(store.stored) != 1 {
		t.Errorf("unmatched PR must still be stored raw-first; stored=%v", store.stored)
	}
	if len(ex.calls) != 0 {
		t.Errorf("unmatched PR must dispatch no events; calls=%v", ex.calls)
	}
}

// Raw-first ORDERING: the raw store is written before any dispatch.
func TestReceiver_StoresRawBeforeDispatch(t *testing.T) {
	trace := []string{}
	ex := &orderedExec{recorder: &trace}
	store := &fakeRawStore{recorder: &trace}
	h := github.NewReceiver(webhookSecret, store, fakeResolver{taskID: 5, ok: true}, ex)

	body := prPayload("opened", 12, false, "task-5-fix")
	post(t, h, "pull_request", "guid-order", sign(webhookSecret, body), body)

	if len(trace) < 2 || trace[0] != "store" {
		t.Errorf("trace = %v, want the raw store BEFORE dispatch (invariant 1)", trace)
	}
}

type orderedExec struct {
	recorder *[]string
}

func (e *orderedExec) Execute(_ context.Context, _ executor.Call) (executor.Result, error) {
	*e.recorder = append(*e.recorder, "dispatch")
	return executor.Result{Output: []byte(`{}`)}, nil
}

// ---- tiny helpers -------------------------------------------------------------

func argIntG(t *testing.T, args map[string]any, key string) int {
	t.Helper()
	v, ok := args[key]
	if !ok {
		t.Fatalf("args missing %q (%v)", key, args)
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		t.Fatalf("arg %q is %T, want a number", key, v)
		return 0
	}
}

func itoaG(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
