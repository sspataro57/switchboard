package worker

// Fake-driven tests for the wrapper loop's routing (SPEC 04-mcp-task-tools,
// acceptance criterion 8, offline form): afterRun's done/park/release
// three-way branch, resume via captured session id vs fresh-session fallback,
// and pause. Zero network, zero Postgres, zero subprocesses — Exec, Fleet and
// Runner are fakes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
)

// fakeExec scripts the executor: task status evolves as the loop acts.
type fakeExec struct {
	mu     sync.Mutex
	status string // current task status returned by task_context
	events []map[string]any
	calls  []string // tool names in order

	claimErr error
	// statusAfterRun is what the post-run context read reports (the stub
	// claude "moved" the task there).
	statusAfterRun string
}

func (f *fakeExec) log(tool string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tool)
}

func (f *fakeExec) called(tool string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == tool {
			return true
		}
	}
	return false
}

func (f *fakeExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	f.log(call.Tool)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch call.Tool {
	case "task_get_next":
		return executor.Result{Output: []byte(`{"task":{"id":7,"project":"p","title":"t","priority":0}}`)}, nil
	case "task_claim":
		if f.claimErr != nil {
			return executor.Result{}, f.claimErr
		}
		f.status = "claimed"
		return executor.Result{Output: []byte(`{"claim_id":1,"task_id":7,"expires_at":"x"}`)}, nil
	case "task_context":
		var a struct {
			WorkerID string `json:"worker_id"`
		}
		_ = json.Unmarshal(call.Args, &a)
		if a.WorkerID != "" && (f.status == "claimed" || f.status == "needs_feedback") {
			f.status = "in_progress"
		}
		doc := map[string]any{
			"task":    map[string]any{"id": 7, "title": "t", "status": f.status},
			"project": map[string]any{"repo_path": "/tmp", "slug": "p"},
			"feedback": []map[string]string{
				{"id": "1", "question": "q", "answer": "use B", "status": "answered"},
			},
			"events": f.events,
		}
		raw, _ := json.Marshal(doc)
		return executor.Result{Output: raw}, nil
	case "task_append_log":
		var a struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(call.Args, &a)
		if a.Kind == "session" {
			f.events = append(f.events, map[string]any{"event_type": "session", "payload": a.Message})
		}
		return executor.Result{Output: []byte(`{"event_id":1}`)}, nil
	case "task_release":
		f.status = "ready"
		return executor.Result{Output: []byte(`{"task_id":7,"status":"ready"}`)}, nil
	case "mark_done_local":
		f.status = "done_locally"
		return executor.Result{Output: []byte(`{}`)}, nil
	}
	return executor.Result{}, fmt.Errorf("unexpected tool %s", call.Tool)
}

// fakeFleet records published states and lets the test deliver cmds when a
// given state is published.
type fakeFleet struct {
	mu      sync.Mutex
	states  []string
	handler func(fleet.Cmd)
	onState map[string]fleet.Cmd // publish of state -> deliver cmd once
}

func (f *fakeFleet) PublishStatus(s fleet.Status) error {
	f.mu.Lock()
	f.states = append(f.states, s.State)
	cmd, ok := f.onState[s.State]
	if ok {
		delete(f.onState, s.State)
	}
	h := f.handler
	f.mu.Unlock()
	if ok && h != nil {
		h(cmd)
	}
	return nil
}

func (f *fakeFleet) SubscribeCmd(handler func(fleet.Cmd)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
	return nil
}

func (f *fakeFleet) published(state string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.states {
		if s == state {
			return true
		}
	}
	return false
}

// fakeRunner returns canned envelopes and records argv per run.
type fakeRunner struct {
	mu   sync.Mutex
	outs [][]byte
	errs []error
	argv [][]string
}

func (r *fakeRunner) Run(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argv = append(r.argv, args)
	i := len(r.argv) - 1
	var out []byte
	var err error
	if i < len(r.outs) {
		out = r.outs[i]
	}
	if i < len(r.errs) {
		err = r.errs[i]
	}
	return out, err
}

func envelope(session string) []byte {
	return []byte(`{"type":"result","is_error":false,"session_id":"` + session + `","num_turns":1,"total_cost_usd":0.01}`)
}

func newLoop(fx Exec, ff *fakeFleet, fr *fakeRunner) *Loop {
	return New(Config{Client: "acme", Once: true, SystemPrompt: "rules", PollInterval: 10 * time.Millisecond}, fx, ff, fr)
}

// afterRun routing: done_locally -> finish, no release.
func TestLoop_DoneRouting(t *testing.T) {
	fx := &fakeExec{statusAfterRun: "done_locally"}
	ff := &fakeFleet{onState: map[string]fleet.Cmd{}}
	fr := &fakeRunner{outs: [][]byte{envelope("s1")}}
	// stub claude "completes" the task: post-run pure context read sees done.
	fxWrap := &statusOverride{fakeExec: fx}

	if err := newLoop(fxWrap, ff, fr).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fx.called("task_release") {
		t.Error("done path must not release")
	}
	if !fx.called("task_append_log") {
		t.Error("session event was not recorded")
	}
	if !ff.published(fleet.StateWorking) || !ff.published(fleet.StateIdle) {
		t.Errorf("states published = %v, want idle and working present", ff.states)
	}
}

// statusOverride makes the post-run (pure) context read report statusAfterRun,
// mimicking the subprocess having moved the task.
type statusOverride struct{ *fakeExec }

func (s *statusOverride) Execute(ctx context.Context, call executor.Call) (executor.Result, error) {
	if call.Tool == "task_context" {
		var a struct {
			WorkerID string `json:"worker_id"`
		}
		_ = json.Unmarshal(call.Args, &a)
		if a.WorkerID == "" && s.statusAfterRun != "" {
			s.mu.Lock()
			s.status = s.statusAfterRun
			s.mu.Unlock()
		}
	}
	return s.fakeExec.Execute(ctx, call)
}

// Failure routing: runner error + task still in_progress -> log + release.
func TestLoop_ReleaseOnFailure(t *testing.T) {
	fx := &fakeExec{}
	ff := &fakeFleet{onState: map[string]fleet.Cmd{}}
	fr := &fakeRunner{outs: [][]byte{nil}, errs: []error{errors.New("boom")}}

	if err := newLoop(fx, ff, fr).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fx.called("task_release") {
		t.Fatalf("failed run must release the task; calls: %v", fx.calls)
	}
	if fx.status != "ready" {
		t.Errorf("task status = %q, want ready after release", fx.status)
	}
}

// Park + resume: post-run needs_feedback parks; a resume cmd triggers
// claude --resume <captured session id>.
func TestLoop_ParkAndResumeWithCapturedSession(t *testing.T) {
	fx := &fakeExec{statusAfterRun: "needs_feedback"}
	resumeArgs, _ := json.Marshal(map[string]int64{"task_id": 7, "feedback_request_id": 1})
	ff := &fakeFleet{onState: map[string]fleet.Cmd{
		fleet.StateNeedsFeedback: {Action: fleet.ActionResume, Args: resumeArgs},
	}}
	fr := &fakeRunner{outs: [][]byte{envelope("sess-first"), envelope("sess-second")}}
	fxWrap := &parkThenDone{statusOverride: statusOverride{fakeExec: fx}}

	if err := newLoop(fxWrap, ff, fr).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fr.argv) != 2 {
		t.Fatalf("runner invocations = %d, want 2 (initial + resume)", len(fr.argv))
	}
	second := strings.Join(fr.argv[1], " ")
	if !strings.Contains(second, "--resume sess-first") {
		t.Errorf("resume argv = %q, want --resume sess-first", second)
	}
	if !strings.Contains(second, "Feedback on #7: use B") {
		t.Errorf("resume prompt missing answered feedback: %q", second)
	}
	if !ff.published(fleet.StateNeedsFeedback) {
		t.Errorf("needs_feedback state never published: %v", ff.states)
	}
}

// parkThenDone: first post-run read reports needs_feedback, the one after the
// resume run reports done_locally.
type parkThenDone struct {
	statusOverride
	pureReads int
}

func (p *parkThenDone) Execute(ctx context.Context, call executor.Call) (executor.Result, error) {
	if call.Tool == "task_context" {
		var a struct {
			WorkerID string `json:"worker_id"`
		}
		_ = json.Unmarshal(call.Args, &a)
		if a.WorkerID == "" {
			p.pureReads++
			if p.pureReads > 1 {
				p.statusAfterRun = "done_locally"
			}
		}
	}
	return p.statusOverride.Execute(ctx, call)
}

// Fresh-session fallback: no session event recorded -> resume runs WITHOUT
// --resume and re-injects context.
func TestLoop_ResumeFreshSessionFallback(t *testing.T) {
	fx := &fakeExec{statusAfterRun: "needs_feedback"}
	resumeArgs, _ := json.Marshal(map[string]int64{"task_id": 7})
	ff := &fakeFleet{onState: map[string]fleet.Cmd{
		fleet.StateNeedsFeedback: {Action: fleet.ActionResume, Args: resumeArgs},
	}}
	// First run's envelope is lost (parse failure -> no session event).
	fr := &fakeRunner{
		outs: [][]byte{[]byte("not json"), envelope("sess-fresh")},
		errs: []error{nil, nil},
	}
	fxWrap := &parkThenDone{statusOverride: statusOverride{fakeExec: fx}}

	if err := newLoop(fxWrap, ff, fr).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fr.argv) != 2 {
		t.Fatalf("runner invocations = %d, want 2", len(fr.argv))
	}
	second := strings.Join(fr.argv[1], " ")
	if strings.Contains(second, "--resume") {
		t.Errorf("fallback must not use --resume: %q", second)
	}
	if !strings.Contains(second, "Feedback on #7") {
		t.Errorf("fallback prompt missing feedback text: %q", second)
	}
}

// Pause: a pause cmd stops claiming; in Once mode the loop exits without
// touching a task.
func TestLoop_PauseStopsClaiming(t *testing.T) {
	fx := &fakeExec{}
	ff := &fakeFleet{onState: map[string]fleet.Cmd{}}
	fr := &fakeRunner{}
	l := newLoop(fx, ff, fr)

	// Deliver pause as soon as the loop subscribes (before first claim).
	ff.onState[fleet.StateIdle] = fleet.Cmd{Action: fleet.ActionPause}

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fx.called("task_claim") {
		t.Errorf("paused loop must not claim; calls: %v", fx.calls)
	}
	if len(fr.argv) != 0 {
		t.Errorf("paused loop spawned claude: %v", fr.argv)
	}
}
