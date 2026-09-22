package main

// `opsctl task-match` (SWT-74 D6, criterion 28): capture's own matcher over a
// message, a comm task or a pasted line, through the executor, printed like
// create-task. A read: it proposes, never routes.
//
//	opsctl task-match --message N   [--project slug] [--limit N]
//	opsctl task-match --task N      (a comm task: its activity message is matched)
//	opsctl task-match --text "…"    (a pasted line; the answer is partial)

import (
	"encoding/json"
	"flag"
	"fmt"
)

func parseTaskMatch(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("task-match", flag.ContinueOnError)
	message := fs.Int64("message", 0, "a normalized_messages id")
	task := fs.Int64("task", 0, "a comm task id (its activity message is matched)")
	text := fs.String("text", "", "a pasted line")
	project := fs.String("project", "", "optional: only proposals in this project slug")
	limit := fs.Int("limit", 0, "optional: at most N proposals (1..20, default 5)")
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	given := 0
	payload := map[string]any{}
	if *message > 0 {
		payload["message_id"] = *message
		given++
	}
	if *task > 0 {
		payload["task_id"] = *task
		given++
	}
	if *text != "" {
		payload["text"] = *text
		given++
	}
	if given != 1 {
		return "", nil, fmt.Errorf("usage: opsctl task-match --message N | --task N | --text \"…\" [--project slug] [--limit N]")
	}
	if *project != "" {
		payload["project"] = *project
	}
	if *limit > 0 {
		payload["limit"] = *limit
	}
	raw, err := json.Marshal(payload)
	return "task_match", raw, err
}
