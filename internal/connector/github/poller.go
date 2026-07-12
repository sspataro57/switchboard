package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sspataro57/switchboard/internal/executor"
)

// RawUpserter stores poller-fetched JSON raw-first (invariant 1), hash-diffed.
type RawUpserter interface {
	UpsertRaw(ctx context.Context, externalID string, raw json.RawMessage) error
}

// Poller is the interim gh-token REST poller: until hooksd has a public
// endpoint, it sweeps open task-linked PRs and their latest workflow runs and
// dispatches the SAME intents through the SAME tools — webhook go-live is
// config, not code.
type Poller struct {
	hc       *http.Client
	baseURL  string
	token    string
	store    RawUpserter
	resolver TaskResolver
	ex       Dispatcher
}

func NewPoller(hc *http.Client, baseURL, token string, store RawUpserter, resolver TaskResolver, ex Dispatcher) *Poller {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Poller{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), token: token, store: store, resolver: resolver, ex: ex}
}

func (p *Poller) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %s: HTTP %d: %.200s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// PollRepo sweeps one repo's PRs (all states, most recently updated first) and
// dispatches pr/ci intents for task-linked ones. The record_* tools' dedup
// makes replays free.
func (p *Poller) PollRepo(ctx context.Context, repo string) (dispatched int, err error) {
	var prsRaw []json.RawMessage
	if err := p.get(ctx, "/repos/"+repo+"/pulls?state=all&sort=updated&direction=desc&per_page=30", &prsRaw); err != nil {
		return 0, err
	}

	for _, prRaw := range prsRaw {
		var pr struct {
			Number  int    `json:"number"`
			State   string `json:"state"`
			Merged  bool   `json:"merged"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			MergedAt *string `json:"merged_at"`
		}
		if err := json.Unmarshal(prRaw, &pr); err != nil {
			return dispatched, fmt.Errorf("parse PR in %s: %w", repo, err)
		}
		// Raw-first (invariant 1): the fetched PR JSON lands before any
		// interpretation, task-linked or not.
		if err := p.store.UpsertRaw(ctx, fmt.Sprintf("pr:%s#%d", repo, pr.Number), prRaw); err != nil {
			return dispatched, err
		}

		ref := PRRef{Repo: repo, PR: pr.Number, HeadBranch: pr.Head.Ref}
		taskID, ok, err := p.resolver.Resolve(ctx, ref)
		if err != nil {
			return dispatched, err
		}
		if !ok {
			continue
		}

		action := "opened"
		if pr.State == "closed" {
			if pr.MergedAt != nil {
				action = "merged"
			} else {
				action = "closed"
			}
		}
		if err := p.dispatch(ctx, "record_pr_event", map[string]any{
			"task_id": taskID, "action": action, "pr": pr.Number, "url": pr.HTMLURL,
		}); err != nil {
			return dispatched, err
		}
		dispatched++

		// latest workflow run for the head SHA
		var runs struct {
			WorkflowRuns []json.RawMessage `json:"workflow_runs"`
		}
		if err := p.get(ctx, "/repos/"+repo+"/actions/runs?head_sha="+pr.Head.SHA+"&per_page=1", &runs); err != nil {
			return dispatched, err
		}
		if len(runs.WorkflowRuns) == 0 {
			continue
		}
		var run struct {
			ID         int64  `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		}
		if err := json.Unmarshal(runs.WorkflowRuns[0], &run); err != nil {
			return dispatched, fmt.Errorf("parse workflow run in %s: %w", repo, err)
		}
		if err := p.store.UpsertRaw(ctx, fmt.Sprintf("run:%s:%d", repo, run.ID), runs.WorkflowRuns[0]); err != nil {
			return dispatched, err
		}
		phase := "started"
		args := map[string]any{"task_id": taskID, "run_id": run.ID, "run_url": run.HTMLURL}
		if run.Status == "completed" {
			phase = "completed"
			args["conclusion"] = run.Conclusion
		}
		args["phase"] = phase
		if err := p.dispatch(ctx, "record_ci_event", args); err != nil {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

func (p *Poller) dispatch(ctx context.Context, tool string, args map[string]any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal %s args: %w", tool, err)
	}
	if _, err := p.ex.Execute(ctx, executor.Call{Tool: tool, Actor: "ghpoll:github", Args: raw}); err != nil {
		return fmt.Errorf("dispatch %s: %w", tool, err)
	}
	return nil
}
