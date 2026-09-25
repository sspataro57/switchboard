//go:build integration

package tools_test

// SWT-56 (docs/tickets/signal-session-name_SPEC.md) criteria 5-8 and 11:
// task_signal's session through executor.Execute on the PRODUCTION wiring
// (queueMatrixExecutor) — the stored name, the event's session/from_session,
// the result's session, the refusal that leaves the row untouched, and the read
// gating of a dangling name (the mixed-binary case).
//
//	DATABASE_URL='postgres://ops:ops@localhost:5433/ops_isosess?sslmode=disable' \
//	  go test -tags integration -p 1 -count=1 -run SignalSession ./internal/tools/
//
// Reuses verbsPool, seedProject and the sg* helpers (signal_integration_test.go).
//
// GREENFIELD NOTE — EXPECTED RED: until 0036 is applied the working_session
// column does not exist (sgRead fails), and until signal.go reads `session`
// nothing is stored, no event carries it and a missing session is accepted.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC criterion 26):
//   - drop `working_session = $3` from the set → EventsResultAndColumn.
//   - make validateSignal accept a missing session → MissingSessionLeavesTheRow.
//   - stop gating from_session on from != '' → DanglingNameIsInvisible.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
)

// ssEvent is the newest working_state_changed payload of a task, with its keys.
func ssEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (keys string, p map[string]string) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='working_state_changed'
		ORDER BY id DESC LIMIT 1`, id).Scan(&raw); err != nil {
		t.Fatalf("last working_state_changed of %d: %v", id, err)
	}
	p = map[string]string{}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	var ks []string
	for k, v := range m {
		ks = append(ks, k)
		s, _ := v.(string)
		p[k] = s
	}
	sort.Strings(ks)
	return strings.Join(ks, ","), p
}

func ssWantEvent(t *testing.T, step string, keys string, p map[string]string, from, to, fromSession, session string) {
	t.Helper()
	if keys != "from,from_session,session,to,worker_id" {
		t.Errorf("%s: event keys = %s, want exactly from,from_session,session,to,worker_id (S5)", step, keys)
	}
	if p["from"] != from || p["to"] != to || p["from_session"] != fromSession || p["session"] != session {
		t.Errorf("%s: event = %v, want from %q to %q from_session %q session %q (S5, S6)", step, p, from, to, fromSession, session)
	}
}

func TestSignalSession_Integration_EventsResultAndColumn(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	h := sgTask(t, ctx, pool, proj, "SIGSESS events", "human", "ready")

	// set, with surrounding spaces: the TRIMMED name is stored (S3.1).
	out, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, "working", "  kube-c7  ")
	if err != nil {
		t.Fatalf("working as kube-c7: %v", err)
	}
	if sgStr(out["session"]) != "kube-c7" || string(out["changed"]) != "true" {
		t.Errorf("set result = %v, want session kube-c7, changed true (criterion 7)", out)
	}
	if r := sgRead(t, ctx, pool, h); r.session != "kube-c7" || r.state != "working" {
		t.Errorf("after set: session %q state %q, want kube-c7 / working (criterion 5: working_session = $3)", r.session, r.state)
	}
	k, p := ssEvent(t, ctx, pool, h)
	ssWantEvent(t, "set", k, p, "", "working", "", "kube-c7")
	if p["worker_id"] != "manual:salvo" {
		t.Errorf("set event worker_id = %q, want manual:salvo", p["worker_id"])
	}
	var audited string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(args->>'session','') FROM audit_events WHERE tool='task_signal' AND task_id=$1
		AND status='ok' ORDER BY id DESC LIMIT 1`, h).Scan(&audited); err != nil {
		t.Fatalf("audit args: %v", err)
	}
	if strings.TrimSpace(audited) != "kube-c7" {
		t.Errorf("audit_events.args session = %q, want the caller's session (Invariants §3: the audit row carries it)", audited)
	}

	// refresh: same state, same session → timestamp moves, no event (S6).
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_state_at = now() - interval '1 hour' WHERE id=$1`, h); err != nil {
		t.Fatalf("age: %v", err)
	}
	out, err = sgExecS(ctx, ex, "mcp:manual:salvo", h, "working", "kube-c7")
	if err != nil || string(out["changed"]) != "false" {
		t.Errorf("refresh = (%v, %v), want changed false", out, err)
	}
	if n := sgCount(t, ctx, pool, `SELECT count(*) FROM tasks WHERE id=$1 AND working_state_at > now() - interval '1 minute'`, h); n != 1 {
		t.Errorf("refresh did not move working_state_at")
	}
	if n := sgEvents(t, ctx, pool, h); n != 1 {
		t.Errorf("events after refresh = %d, want 1 (a refresh is not news)", n)
	}

	// takeover: same state, another session → news; last writer wins (S6).
	out, err = sgExecS(ctx, ex, "mcp:manual:salvo", h, "working", "switchboard-67")
	if err != nil || string(out["changed"]) != "true" || sgStr(out["session"]) != "switchboard-67" {
		t.Errorf("takeover = (%v, %v), want changed true, session switchboard-67", out, err)
	}
	if n := sgEvents(t, ctx, pool, h); n != 2 {
		t.Errorf("events after takeover = %d, want 2 (a new session is news)", n)
	}
	k, p = ssEvent(t, ctx, pool, h)
	ssWantEvent(t, "takeover", k, p, "working", "working", "kube-c7", "switchboard-67")
	if r := sgRead(t, ctx, pool, h); r.session != "switchboard-67" {
		t.Errorf("after takeover session = %q, want switchboard-67 (last writer wins)", r.session)
	}

	// state change, same session.
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, "needs_input", "switchboard-67"); err != nil {
		t.Fatalf("needs_input: %v", err)
	}
	k, p = ssEvent(t, ctx, pool, h)
	ssWantEvent(t, "state change", k, p, "working", "needs_input", "switchboard-67", "switchboard-67")

	// clear with no session: all three NULL; the event records whose marker left.
	out, err = sgExecS(ctx, ex, "opsctl:salvo", h, "clear", "")
	if err != nil {
		t.Fatalf("clear without session: %v (S2: clear needs none)", err)
	}
	if sgStr(out["session"]) != "" || sgStr(out["state"]) != "" || string(out["changed"]) != "true" {
		t.Errorf("clear result = %v, want state \"\", session \"\", changed true (criterion 7)", out)
	}
	if r := sgRead(t, ctx, pool, h); r.state != "" || r.stateAt != "" || r.session != "" {
		t.Errorf("after clear: %+v, want all three NULL (criterion 6)", r)
	}
	k, p = ssEvent(t, ctx, pool, h)
	ssWantEvent(t, "clear", k, p, "needs_input", "", "switchboard-67", "")

	// clear again: no-op, no event.
	before := sgEvents(t, ctx, pool, h)
	if out, err := sgExecS(ctx, ex, "opsctl:salvo", h, "clear", ""); err != nil || string(out["changed"]) != "false" {
		t.Errorf("second clear = (%v, %v), want a no-op", out, err)
	}
	if n := sgEvents(t, ctx, pool, h); n != before {
		t.Errorf("a no-op clear wrote an event (%d → %d)", before, n)
	}

	// clear WITH a session is recorded in the event.
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, "working", "kube-c7"); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	if _, err := sgExecS(ctx, ex, "opsctl:salvo", h, "clear", "shell"); err != nil {
		t.Fatalf("clear as shell: %v", err)
	}
	k, p = ssEvent(t, ctx, pool, h)
	ssWantEvent(t, "clear with a session", k, p, "working", "", "kube-c7", "shell")

	// clear with an INVALID session is refused, and changes nothing.
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, "needs_input", "kube-c7"); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	marked := sgRead(t, ctx, pool, h)
	if _, err := sgExecS(ctx, ex, "opsctl:salvo", h, "clear", "This session is kube-c7"); err == nil {
		t.Errorf("clear with the whole ListAgents line was accepted (S2: a clear's session is validated too)")
	}
	if r := sgRead(t, ctx, pool, h); r != marked {
		t.Errorf("a refused clear changed the row: %+v → %+v", marked, r)
	}
}

// S2 / Goal: a working or needs_input with no session is refused BY NAME, the
// light does not change, and the next signal carrying the name is accepted.
func TestSignalSession_Integration_MissingSessionLeavesTheRow(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	h := sgTask(t, ctx, pool, proj, "SIGSESS missing", "human", "ready")
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, "needs_input", "kube-c7"); err != nil {
		t.Fatalf("seed needs_input: %v", err)
	}
	before, events := sgRead(t, ctx, pool, h), sgEvents(t, ctx, pool, h)
	for _, state := range []string{"working", "needs_input"} {
		for _, sess := range []string{"", "   "} {
			_, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, state, sess)
			if err == nil {
				t.Errorf("%s with session %q was accepted; S2 requires a session", state, sess)
				continue
			}
			for _, w := range []string{"missing session", "tmux window name", "Your swb session name is"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%s with session %q refused with %q, which does not say %q (S3.2: tells it how to find its name)",
						state, sess, err, w)
				}
			}
		}
	}
	if r := sgRead(t, ctx, pool, h); r != before {
		t.Errorf("a refused signal changed the row: %+v → %+v", before, r)
	}
	if n := sgEvents(t, ctx, pool, h); n != events {
		t.Errorf("a refused signal wrote an event (%d → %d)", events, n)
	}
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", h, "working", "kube-c7"); err != nil {
		t.Errorf("the next signal, carrying the name, was refused: %v", err)
	}
}

// Criterion 8: the SWT-52 refusals are unchanged, and they run AFTER argument
// validation — a claude task with no session is refused for the session.
func TestSignalSession_Integration_ValidationRunsBeforeTheStatusRefusals(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")

	c := sgTask(t, ctx, pool, proj, "SIGSESS claude", "claude", "ready")
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", c, "working", ""); err == nil || !strings.Contains(err.Error(), "missing session") {
		t.Errorf("working on a claude task with no session = %v, want the validator's missing-session refusal (criterion 8)", err)
	}
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", c, "working", "kube-c7"); err == nil || !strings.Contains(err.Error(), "human") {
		t.Errorf("working on a claude task with a session = %v, want the human-tasks-only refusal (unchanged)", err)
	}
	closed := sgTask(t, ctx, pool, proj, "SIGSESS closed", "human", "closed")
	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", closed, "needs_input", "kube-c7"); err == nil ||
		!strings.Contains(err.Error(), "reopen it first") {
		t.Errorf("needs_input on a closed task with a session = %v, want `reopen it first` (unchanged)", err)
	}
	for _, id := range []int64{c, closed} {
		if r := sgRead(t, ctx, pool, id); r.state != "" || r.session != "" {
			t.Errorf("refused task %d was marked: %+v", id, r)
		}
	}
}

// Criterion 11 (S4 read gating): a dangling name under a NULL state — an old
// binary's clear — is invisible to task_context and is never reported as the
// previous holder.
func TestSignalSession_Integration_DanglingNameIsInvisible(t *testing.T) {
	ctx := context.Background()
	pool := verbsPool(t, ctx)
	ex := queueMatrixExecutor(pool)
	proj := seedProject(t, ctx, pool, sgSlug, "itest-mcp-tools-signalclient")
	g := sgTask(t, ctx, pool, proj, "SIGSESS ghost", "human", "ready")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET working_session = 'ghost' WHERE id=$1`, g); err != nil {
		t.Fatalf("seed the dangling name: %v", err)
	}

	args, _ := json.Marshal(map[string]any{"task_id": g})
	res, err := ex.Execute(ctx, executor.Call{Tool: "task_context", Actor: "opsctl:salvo", Args: args, TaskID: &g})
	if err != nil {
		t.Fatalf("task_context: %v", err)
	}
	var doc struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(res.Output, &doc); err != nil {
		t.Fatalf("task_context output: %v", err)
	}
	for _, key := range []string{"working_state", "working_state_at", "working_session"} {
		v, ok := doc.Task[key]
		if !ok {
			t.Errorf("task_context's task has no %q key (S13)", key)
		} else if v != "" {
			t.Errorf("task_context task.%s = %v for a row with NULL state and a dangling name, want \"\" (S4 read gating)", key, v)
		}
	}

	if _, err := sgExecS(ctx, ex, "mcp:manual:salvo", g, "working", "kube-c7"); err != nil {
		t.Fatalf("working as kube-c7: %v", err)
	}
	k, p := ssEvent(t, ctx, pool, g)
	ssWantEvent(t, "set over a dangling name", k, p, "", "working", "", "kube-c7")
	if r := sgRead(t, ctx, pool, g); r.session != "kube-c7" {
		t.Errorf("after the set session = %q, want kube-c7 (the next set overwrites the dangling name)", r.session)
	}
}
