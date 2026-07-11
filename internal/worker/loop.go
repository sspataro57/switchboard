// Package worker is the CLAUDE.md worker wrapper loop (SPEC 04-mcp-task-tools):
// heartbeat → get_next → claim → context → spawn `claude -p` → capture
// session_id → done | park | release. Every task-state mutation is an
// in-process executor.Execute call — the wrapper is an executor client exactly
// like opsctl, never a direct table writer. It calls no LLM API: it execs the
// claude binary and parses its result envelope.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
)

// Exec is the executor dependency.
type Exec interface {
	Execute(ctx context.Context, call executor.Call) (executor.Result, error)
}

// Fleet is the heartbeat/cmd surface of the fleet client.
type Fleet interface {
	PublishStatus(s fleet.Status) error
	SubscribeCmd(handler func(fleet.Cmd)) error
}

// Runner spawns the claude subprocess. Tests point it at a stub.
type Runner interface {
	Run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error)
}

// CmdRunner runs a real subprocess.
type CmdRunner struct{ Bin string }

func (r CmdRunner) Run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("run %s: %w", r.Bin, err)
	}
	return out, nil
}

// Result is the claude -p --output-format json envelope (the fields we read).
type Result struct {
	SessionID  string  `json:"session_id"`
	IsError    bool    `json:"is_error"`
	NumTurns   int     `json:"num_turns"`
	TotalCost  float64 `json:"total_cost_usd"`
	ResultText string  `json:"result"`
}

// ParseResult parses the claude result envelope.
func ParseResult(out []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(out, &r); err != nil {
		return Result{}, fmt.Errorf("parse claude result json: %w", err)
	}
	if r.SessionID == "" {
		return r, fmt.Errorf("claude result carries no session_id")
	}
	return r, nil
}

// Config wires one worker loop.
type Config struct {
	Client       string // worker_id for single-console
	Subproject   string
	Once         bool
	SystemPrompt string        // contents of prompts/worker-system.md
	MCPConfig    string        // path to a generated mcp config for the subprocess
	PollInterval time.Duration // default 30s
}

type Loop struct {
	cfg    Config
	ex     Exec
	fl     Fleet
	runner Runner

	cmds   chan fleet.Cmd
	paused bool
}

func New(cfg Config, ex Exec, fl Fleet, runner Runner) *Loop {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
	return &Loop{cfg: cfg, ex: ex, fl: fl, runner: runner, cmds: make(chan fleet.Cmd, 8)}
}

func (l *Loop) actor() string { return "opsworker:" + l.cfg.Client }

func (l *Loop) call(ctx context.Context, tool, args string) (json.RawMessage, error) {
	res, err := l.ex.Execute(ctx, executor.Call{Tool: tool, Actor: l.actor(), Args: []byte(args)})
	if err != nil {
		return nil, err
	}
	return res.Output, nil
}

func (l *Loop) publish(state string, taskID *int64) {
	if err := l.fl.PublishStatus(fleet.Status{State: state, TaskID: taskID, TS: time.Now().UTC()}); err != nil {
		slog.Warn("publish status failed", "state", state, "err", err)
	}
}

// Run is the loop. It returns when ctx is cancelled, or after one processed
// task (or one empty poll) in Once mode.
func (l *Loop) Run(ctx context.Context) error {
	if err := l.fl.SubscribeCmd(func(c fleet.Cmd) {
		select {
		case l.cmds <- c:
		default:
			slog.Warn("cmd channel full; dropping", "action", c.Action)
		}
	}); err != nil {
		return fmt.Errorf("subscribe cmd topic: %w", err)
	}

	// heartbeat ticker republishes current state for the process lifetime
	state := fleet.StateIdle
	var stateTask *int64
	hb := time.NewTicker(fleet.HeartbeatInterval)
	defer hb.Stop()
	setState := func(s string, task *int64) {
		state, stateTask = s, task
		l.publish(s, task)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hb.C:
				l.publish(state, stateTask)
			}
		}
	}()

	setState(fleet.StateIdle, nil)
	for {
		if ctx.Err() != nil {
			return nil
		}
		l.drainCmds()
		if l.paused {
			slog.Info("paused; not claiming")
			if l.cfg.Once {
				return nil
			}
			sleepCtx(ctx, l.cfg.PollInterval)
			continue
		}

		taskID, err := l.nextTask(ctx)
		if err != nil {
			return err
		}
		if taskID == 0 {
			if l.cfg.Once {
				slog.Info("no ready tasks; exiting (--once)")
				return nil
			}
			sleepCtx(ctx, l.cfg.PollInterval)
			continue
		}

		if err := l.processTask(ctx, taskID, setState); err != nil {
			slog.Error("task processing failed", "task", taskID, "err", err)
		}
		setState(fleet.StateIdle, nil)
		if l.cfg.Once {
			return nil
		}
	}
}

func (l *Loop) drainCmds() {
	for {
		select {
		case c := <-l.cmds:
			if c.Action == fleet.ActionPause {
				l.paused = true
			}
		default:
			return
		}
	}
}

// nextTask peeks and claims; returns 0 when nothing is claimable.
func (l *Loop) nextTask(ctx context.Context) (int64, error) {
	args := fmt.Sprintf(`{"client":%q}`, l.cfg.Client)
	if l.cfg.Subproject != "" {
		args = fmt.Sprintf(`{"client":%q,"subproject":%q}`, l.cfg.Client, l.cfg.Subproject)
	}
	out, err := l.call(ctx, "task_get_next", args)
	if err != nil {
		return 0, fmt.Errorf("get next: %w", err)
	}
	var next struct {
		Task *struct {
			ID int64 `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(out, &next); err != nil {
		return 0, fmt.Errorf("parse get_next output: %w", err)
	}
	if next.Task == nil {
		return 0, nil
	}

	if _, err := l.call(ctx, "task_claim",
		fmt.Sprintf(`{"task_id":%d,"worker_id":%q}`, next.Task.ID, l.cfg.Client)); err != nil {
		slog.Info("lost claim race; re-peeking", "task", next.Task.ID, "err", err)
		return 0, nil
	}
	return next.Task.ID, nil
}

// contextDoc is the slice of task_context the wrapper reads.
type contextDoc struct {
	Task struct {
		ID     int64  `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"task"`
	Project struct {
		RepoPath string `json:"repo_path"`
		Slug     string `json:"slug"`
	} `json:"project"`
	Feedback []struct {
		ID       string `json:"id"`
		Question string `json:"question"`
		Answer   string `json:"answer"`
		Status   string `json:"status"`
	} `json:"feedback"`
	Events []struct {
		EventType string `json:"event_type"`
		Payload   string `json:"payload"`
	} `json:"events"`
}

func (l *Loop) fetchContext(ctx context.Context, taskID int64, asHolder bool) (contextDoc, json.RawMessage, error) {
	args := fmt.Sprintf(`{"task_id":%d}`, taskID)
	if asHolder {
		args = fmt.Sprintf(`{"task_id":%d,"worker_id":%q}`, taskID, l.cfg.Client)
	}
	out, err := l.call(ctx, "task_context", args)
	if err != nil {
		return contextDoc{}, nil, fmt.Errorf("task context: %w", err)
	}
	var doc contextDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return contextDoc{}, nil, fmt.Errorf("parse context: %w", err)
	}
	return doc, out, nil
}

func (l *Loop) processTask(ctx context.Context, taskID int64, setState func(string, *int64)) error {
	doc, raw, err := l.fetchContext(ctx, taskID, true)
	if err != nil {
		return err
	}
	setState(fleet.StateWorking, &taskID)

	prompt := renderPrompt(taskID, raw)
	res, runErr := l.runClaude(ctx, doc.Project.RepoPath, []string{"-p", prompt})
	return l.afterRun(ctx, taskID, res, runErr, setState)
}

// afterRun records the session event and routes on the task's resulting status.
func (l *Loop) afterRun(ctx context.Context, taskID int64, res Result, runErr error, setState func(string, *int64)) error {
	if res.SessionID != "" {
		sess, _ := json.Marshal(map[string]any{
			"session_id": res.SessionID, "is_error": res.IsError,
			"num_turns": res.NumTurns, "cost_usd": res.TotalCost,
		})
		msg, _ := json.Marshal(string(sess))
		if _, err := l.call(ctx, "task_append_log",
			fmt.Sprintf(`{"task_id":%d,"message":%s,"kind":"session","worker_id":%q}`, taskID, msg, l.cfg.Client)); err != nil {
			slog.Warn("write session event failed", "err", err)
		}
	}

	doc, _, err := l.fetchContext(ctx, taskID, false)
	if err != nil {
		return err
	}

	switch {
	case doc.Task.Status == "done_locally":
		slog.Info("task done", "task", taskID, "session", res.SessionID)
		return nil
	case doc.Task.Status == "needs_feedback":
		if runErr != nil {
			// The task parked but the result envelope was lost — park anyway
			// (the feedback question is real), resume will fall back to a
			// fresh session.
			slog.Error("claude run result not captured before park", "task", taskID, "err", runErr)
		}
		setState(fleet.StateNeedsFeedback, &taskID)
		slog.Info("parked; waiting for resume cmd", "task", taskID)
		return l.park(ctx, taskID, setState)
	default:
		// still in_progress with a failed/errored run: log, release, same task retries later
		reason := "claude run did not complete the task"
		if runErr != nil {
			reason = runErr.Error()
		} else if res.IsError {
			reason = "claude reported is_error"
		}
		msg, _ := json.Marshal(reason)
		_, _ = l.call(ctx, "task_append_log",
			fmt.Sprintf(`{"task_id":%d,"message":%s,"kind":"error","worker_id":%q}`, taskID, msg, l.cfg.Client))
		reasonJSON, _ := json.Marshal(reason)
		if _, err := l.call(ctx, "task_release",
			fmt.Sprintf(`{"task_id":%d,"worker_id":%q,"reason":%s}`, taskID, l.cfg.Client, reasonJSON)); err != nil {
			return fmt.Errorf("release task %d: %w", taskID, err)
		}
		slog.Info("task released for retry", "task", taskID, "reason", reason)
		return nil
	}
}

// park waits for a resume cmd for this task, then resumes the captured session.
func (l *Loop) park(ctx context.Context, taskID int64, setState func(string, *int64)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case c := <-l.cmds:
			switch c.Action {
			case fleet.ActionPause:
				l.paused = true
			case fleet.ActionResume:
				var args struct {
					TaskID int64 `json:"task_id"`
				}
				_ = json.Unmarshal(c.Args, &args)
				if args.TaskID != 0 && args.TaskID != taskID {
					slog.Warn("resume for a different task; ignoring", "got", args.TaskID, "parked", taskID)
					continue
				}
				return l.resume(ctx, taskID, setState)
			default:
				slog.Info("ignoring cmd while parked", "action", c.Action)
			}
		}
	}
}

func (l *Loop) resume(ctx context.Context, taskID int64, setState func(string, *int64)) error {
	// holder context flips needs_feedback -> in_progress and carries the
	// answered feedback + latest session event.
	doc, raw, err := l.fetchContext(ctx, taskID, true)
	if err != nil {
		return err
	}
	answer := latestAnswer(doc)
	setState(fleet.StateWorking, &taskID)

	prompt := fmt.Sprintf("Feedback on #%d: %s", taskID, answer)
	var head []string
	if sessionID := latestSessionID(doc); sessionID != "" {
		head = []string{"--resume", sessionID, "-p", prompt}
	} else {
		// Session pointer lost (envelope parse failure on the previous run):
		// fall back to a fresh session with the full context re-injected.
		slog.Warn("no session event for task; resuming in a fresh session", "task", taskID)
		prompt = renderPrompt(taskID, raw) + "\n\n" + prompt
		head = []string{"-p", prompt}
	}
	res, runErr := l.runClaude(ctx, doc.Project.RepoPath, head)
	return l.afterRun(ctx, taskID, res, runErr, setState)
}

func (l *Loop) runClaude(ctx context.Context, dir string, head []string) (Result, error) {
	args := append(head,
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--append-system-prompt", l.cfg.SystemPrompt,
	)
	if l.cfg.MCPConfig != "" {
		args = append(args, "--mcp-config", l.cfg.MCPConfig, "--strict-mcp-config")
	}
	if dir == "" {
		dir = "."
	}
	extraEnv := []string{"OPS_WORKER_ID=" + l.cfg.Client}
	out, err := l.runner.Run(ctx, dir, extraEnv, args...)
	if err != nil {
		// claude exits non-zero on is_error runs but still emits a valid
		// result envelope — salvage it so the session event is recorded.
		if res, perr := ParseResult(out); perr == nil {
			return res, fmt.Errorf("%w (claude reported: %.200s)", err, res.ResultText)
		}
		return Result{}, fmt.Errorf("%w (output head: %.200s)", err, out)
	}
	res, perr := ParseResult(out)
	if perr != nil {
		return Result{}, fmt.Errorf("%w (output head: %.200s)", perr, out)
	}
	return res, nil
}

func renderPrompt(taskID int64, contextJSON json.RawMessage) string {
	return fmt.Sprintf(
		"You are working on switchboard task #%d. Your full task context document (JSON):\n\n%s\n\n"+
			"Complete this task now, following the worker rules in your system prompt.",
		taskID, contextJSON)
}

func latestSessionID(doc contextDoc) string {
	for i := len(doc.Events) - 1; i >= 0; i-- {
		if doc.Events[i].EventType != "session" {
			continue
		}
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(doc.Events[i].Payload), &p); err == nil && p.SessionID != "" {
			return p.SessionID
		}
	}
	return ""
}

func latestAnswer(doc contextDoc) string {
	for i := len(doc.Feedback) - 1; i >= 0; i-- {
		if doc.Feedback[i].Status == "answered" && doc.Feedback[i].Answer != "" {
			return doc.Feedback[i].Answer
		}
	}
	return "(no answer text found — check feedback_requests)"
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// WriteMCPConfig generates the subprocess MCP config pointing at ops-mcp.
func WriteMCPConfig(dir, opsMCPBin, databaseURL, workerID string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"ops": map[string]any{
				"command": opsMCPBin,
				"env": map[string]string{
					"DATABASE_URL":  databaseURL,
					"OPS_WORKER_ID": workerID,
				},
			},
		},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}
	path := filepath.Join(dir, "opsworker-mcp.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write mcp config: %w", err)
	}
	return path, nil
}
