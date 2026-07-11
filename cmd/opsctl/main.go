// opsctl is a minimal CLI client of the executor — it never writes tool-action
// tables directly (invariant 3). Usage:
//
//	opsctl create-task --project <slug> --title "..." [--body ... --assignee human|claude --priority N --subproject X]
//	opsctl call --tool <name> [--args '<json>']   (raw executor call; used by the negative smoke)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: opsctl <create-task|call> [flags]")
		os.Exit(2)
	}

	var toolName string
	var args json.RawMessage
	var err error

	switch os.Args[1] {
	case "create-task":
		toolName, args, err = parseCreateTask(os.Args[2:])
	case "call":
		toolName, args, err = parseCall(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "opsctl:", err)
		os.Exit(2)
	}

	if err := run(toolName, args); err != nil {
		fmt.Fprintln(os.Stderr, "opsctl:", err)
		os.Exit(1)
	}
}

func parseCreateTask(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("create-task", flag.ContinueOnError)
	project := fs.String("project", "", "project slug (required)")
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task body")
	assignee := fs.String("assignee", "", "assignee_type: human (default) | claude")
	priority := fs.Int("priority", 0, "priority")
	subproject := fs.String("subproject", "", "subproject")
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}

	payload := map[string]any{"project": *project, "title": *title}
	if *body != "" {
		payload["body"] = *body
	}
	if *assignee != "" {
		payload["assignee_type"] = *assignee
	}
	if *priority != 0 {
		payload["priority"] = *priority
	}
	if *subproject != "" {
		payload["subproject"] = *subproject
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("marshal args: %w", err)
	}
	return "create_task", raw, nil
}

func parseCall(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	tool := fs.String("tool", "", "tool name (required)")
	rawArgs := fs.String("args", "{}", "tool args as JSON")
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	if *tool == "" {
		return "", nil, fmt.Errorf("--tool is required")
	}
	if !json.Valid([]byte(*rawArgs)) {
		return "", nil, fmt.Errorf("--args is not valid JSON")
	}
	return *tool, json.RawMessage(*rawArgs), nil
}

func run(toolName string, args json.RawMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	ex := executor.New(reg, policy.NewStatic(reg.Names()...), audit.NewPGStore(pool))

	res, err := ex.Execute(ctx, executor.Call{Tool: toolName, Actor: actor(), Args: args})
	if err != nil {
		return err
	}
	fmt.Println(string(res.Output))
	return nil
}

func actor() string {
	if u := os.Getenv("USER"); u != "" {
		return "opsctl:" + u
	}
	return "opsctl"
}
