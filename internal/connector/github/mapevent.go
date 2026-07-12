// Package github is the GitHub event surface (SPEC 09-jira-github-connectors):
// a pure payload→intent mapper shared by the HMAC webhook receiver (cmd/hooksd)
// and the interim gh-token poller. Neither writes task state directly — every
// mutation is a spine tool call through the executor (invariant 3).
package github

import (
	"encoding/json"
	"fmt"
)

// Intent is one spine tool call once a task_id is resolved. Args are the tool
// args WITHOUT task_id (the dispatcher injects the resolved id).
type Intent struct {
	Tool  string // "record_pr_event" | "record_ci_event"
	Args  map[string]any
	Match PRRef
}

// PRRef carries the PR-resolution coordinates.
type PRRef struct {
	Repo       string // "{owner}/{repo}"
	PR         int
	HeadBranch string
}

// MapEvent is pure: (X-GitHub-Event, raw JSON body) -> zero or more intents.
// Handled: pull_request (opened/reopened/closed), workflow_run
// (requested/in_progress/completed). check_suite is deliberately ignored — it
// would add a second overlapping CI signal for nothing.
func MapEvent(eventType string, payload []byte) ([]Intent, error) {
	switch eventType {
	case "pull_request":
		return mapPullRequest(payload)
	case "workflow_run":
		return mapWorkflowRun(payload)
	default:
		return nil, nil
	}
}

func mapPullRequest(payload []byte) ([]Intent, error) {
	var p struct {
		Action      string `json:"action"`
		PullRequest struct {
			Number  int    `json:"number"`
			Merged  bool   `json:"merged"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("parse pull_request payload: %w", err)
	}

	var action string
	switch p.Action {
	case "opened", "reopened":
		action = "opened"
	case "closed":
		if p.PullRequest.Merged {
			action = "merged"
		} else {
			action = "closed"
		}
	default:
		return nil, nil
	}

	return []Intent{{
		Tool: "record_pr_event",
		Args: map[string]any{
			"action": action,
			"pr":     p.PullRequest.Number,
			"url":    p.PullRequest.HTMLURL,
		},
		Match: PRRef{Repo: p.Repository.FullName, PR: p.PullRequest.Number, HeadBranch: p.PullRequest.Head.Ref},
	}}, nil
}

func mapWorkflowRun(payload []byte) ([]Intent, error) {
	var p struct {
		Action      string `json:"action"`
		WorkflowRun struct {
			ID           int64  `json:"id"`
			Conclusion   string `json:"conclusion"`
			HeadBranch   string `json:"head_branch"`
			HTMLURL      string `json:"html_url"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"workflow_run"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("parse workflow_run payload: %w", err)
	}

	var phase string
	switch p.Action {
	case "requested", "in_progress":
		phase = "started"
	case "completed":
		phase = "completed"
	default:
		return nil, nil
	}

	args := map[string]any{
		"phase":   phase,
		"run_id":  p.WorkflowRun.ID,
		"run_url": p.WorkflowRun.HTMLURL,
	}
	if phase == "completed" {
		args["conclusion"] = p.WorkflowRun.Conclusion
	}

	pr := 0
	if len(p.WorkflowRun.PullRequests) > 0 {
		pr = p.WorkflowRun.PullRequests[0].Number
	}
	return []Intent{{
		Tool:  "record_ci_event",
		Args:  args,
		Match: PRRef{Repo: p.Repository.FullName, PR: pr, HeadBranch: p.WorkflowRun.HeadBranch},
	}}, nil
}
