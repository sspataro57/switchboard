package github_test

// Offline poller test (SPEC 09-jira-github-connectors, criterion 13): a fake
// GitHub API serves one task-linked PR + its latest workflow run; the poller
// must store BOTH fetched JSONs raw-first (pr:{owner}/{repo}#{n},
// run:{owner}/{repo}:{id}) BEFORE dispatching record_pr_event/record_ci_event
// through the executor seam. No live GitHub, ever.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/github"
	"github.com/sspataro57/switchboard/internal/executor"
)

type tracingExec struct {
	calls    []executor.Call
	recorder *[]string
}

func (f *tracingExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	f.calls = append(f.calls, call)
	if f.recorder != nil {
		*f.recorder = append(*f.recorder, "dispatch:"+call.Tool)
	}
	return executor.Result{Output: []byte(`{}`)}, nil
}

type fakeUpserter struct {
	stored   []string
	recorder *[]string
}

func (u *fakeUpserter) UpsertRaw(_ context.Context, externalID string, _ json.RawMessage) error {
	u.stored = append(u.stored, externalID)
	if u.recorder != nil {
		*u.recorder = append(*u.recorder, "raw:"+externalID)
	}
	return nil
}

func TestPoller_StoresRawFirstThenDispatches(t *testing.T) {
	const repo = "owner/repo"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + repo + "/pulls":
			fmt.Fprint(w, `[{"number":7,"state":"open","html_url":"https://gh/owner/repo/7",
				"head":{"ref":"task-5-fix","sha":"abc123"}}]`)
		case "/repos/" + repo + "/actions/runs":
			if r.URL.Query().Get("head_sha") != "abc123" {
				t.Errorf("runs queried with head_sha=%q, want abc123", r.URL.Query().Get("head_sha"))
			}
			fmt.Fprint(w, `{"workflow_runs":[{"id":901,"status":"completed","conclusion":"success",
				"html_url":"https://ci/901"}]}`)
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	var trace []string
	store := &fakeUpserter{recorder: &trace}
	ex := &tracingExec{recorder: &trace}
	p := github.NewPoller(api.Client(), api.URL, "tok", store, fakeResolver{taskID: 5, ok: true}, ex)

	n, err := p.PollRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("PollRepo: %v", err)
	}
	if n != 2 {
		t.Errorf("dispatched = %d, want 2 (pr + ci)", n)
	}

	want := []string{"pr:owner/repo#7", "run:owner/repo:901"}
	if len(store.stored) != 2 || store.stored[0] != want[0] || store.stored[1] != want[1] {
		t.Errorf("raw stored = %v, want %v (criterion 13)", store.stored, want)
	}

	// Raw-first ordering: each fetched JSON lands before its intent dispatches.
	wantTrace := []string{"raw:pr:owner/repo#7", "dispatch:record_pr_event", "raw:run:owner/repo:901", "dispatch:record_ci_event"}
	if len(trace) != len(wantTrace) {
		t.Fatalf("trace = %v, want %v", trace, wantTrace)
	}
	for i := range wantTrace {
		if trace[i] != wantTrace[i] {
			t.Fatalf("trace[%d] = %q, want %q (full trace %v)", i, trace[i], wantTrace[i], trace)
		}
	}
}

func TestPoller_UnlinkedPRStoredRawButNotDispatched(t *testing.T) {
	const repo = "owner/repo"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/"+repo+"/pulls" {
			fmt.Fprint(w, `[{"number":9,"state":"open","html_url":"https://gh/owner/repo/9",
				"head":{"ref":"unrelated-branch","sha":"def456"}}]`)
			return
		}
		t.Errorf("unexpected API path %s (unlinked PR must not fetch runs)", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer api.Close()

	store := &fakeUpserter{}
	ex := &tracingExec{}
	p := github.NewPoller(api.Client(), api.URL, "tok", store, fakeResolver{ok: false}, ex)

	n, err := p.PollRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("PollRepo: %v", err)
	}
	if n != 0 {
		t.Errorf("dispatched = %d, want 0 (not ours)", n)
	}
	// Invariant 1: raw lands even for PRs that resolve to no task.
	if len(store.stored) != 1 || store.stored[0] != "pr:owner/repo#9" {
		t.Errorf("raw stored = %v, want [pr:owner/repo#9]", store.stored)
	}
}
