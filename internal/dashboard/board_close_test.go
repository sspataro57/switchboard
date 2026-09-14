package dashboard

// SWT-51 (docs/tickets/board-done-button_SPEC.md) criteria 6, 7 and 8: the
// board's Done handler, POST /tasks/{id}/close. ZERO I/O: a capturing executor
// stands in for the real one, and the request goes through the REAL mux built
// by (*Server).Handler with a dev-login session, so the route registration,
// the auth wrapper and the handler are exercised together.
//
// IMPOSED SURFACE: only the route (criterion 5) and the handler's observable
// behaviour — one executor.Call{Tool: "task_close", Actor: "dashboard:{user}",
// TaskID: &id, Args: {"task_id": id, "reason": ...}}, a 303 to /tasks with the
// rebuilt filters and the flash. The method name closeTaskAction is pinned by
// board_structure_test.go, not here: going through the mux keeps this file
// compiling before the handler exists.
//
// GREENFIELD NOTE — EXPECTED RED: POST /tasks/{id}/close is not registered, so
// the mux answers 405 (the path only matches the GET-only `GET /` pattern) and
// the executor sees zero calls.
//
// MUTATIONS THAT MUST TURN THIS FILE RED:
//   - build the args with fmt.Sprintf → the injection row (task_id 999 / extra key y).
//   - call task_dismiss → every args row (tool name, and no `reason` key).
//   - forget strings.TrimSpace → the blank and padded rows.
//   - echo r.URL.RawQuery or a posted `next` → the redirect test.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/executor"
)

// closeErrExec records calls like captureExec but answers with a fixed error,
// standing in for task_close's refusal.
type closeErrExec struct {
	calls []executor.Call
	err   error
}

func (c *closeErrExec) Execute(_ context.Context, call executor.Call) (executor.Result, error) {
	c.calls = append(c.calls, call)
	return executor.Result{}, c.err
}

// boardCloseHarness builds the real mux around ex and logs in as salvo through
// the dev-login stub, returning the handler and the session cookie.
func boardCloseHarness(t *testing.T, ex Exec) (http.Handler, []*http.Cookie) {
	t.Helper()
	auth, err := NewAuth(context.Background(), "", "", "", "")
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	h := (&Server{ex: ex, auth: auth}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev/login?user=salvo", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("dev login set no session cookie (status %d)", rec.Code)
	}
	return h, cookies
}

func postBoardClose(h http.Handler, cookies []*http.Cookie, target string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Criteria 6 + 7 (D1): exactly one task_close call as dashboard:{user} with the
// path id on Call.TaskID; args are EXACTLY {task_id, reason}; the reason is
// "done on the board", or "done on the board: " + the trimmed note. A posted
// reason_code (the Dismiss form's field) never rides along.
func TestCloseTaskAction_ArgsAreTaskIDAndReasonOnly(t *testing.T) {
	const inject = `x","task_id":999,"y":"`
	for _, tc := range []struct {
		name    string
		form    url.Values
		wantWhy string
	}{
		{"no note field", url.Values{}, "done on the board"},
		{"empty note", url.Values{"note": {""}}, "done on the board"},
		{"blank note", url.Values{"note": {"   "}}, "done on the board"},
		{"note", url.Values{"note": {"shipped in 1.4"}}, "done on the board: shipped in 1.4"},
		{"padded note", url.Values{"note": {"  shipped in 1.4 \n"}}, "done on the board: shipped in 1.4"},
		{"injection note", url.Values{"note": {inject}}, "done on the board: " + inject},
		{"stray reason_code", url.Values{"note": {"ok"}, "reason_code": {"duplicate"}}, "done on the board: ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &captureExec{}
			h, cookies := boardCloseHarness(t, ex)
			rec := postBoardClose(h, cookies, "/tasks/7/close", tc.form)

			if len(ex.calls) != 1 {
				t.Fatalf("POST /tasks/7/close answered %d and made %d executor call(s), want exactly one. "+
					"Criteria 5-6: the route is registered on the auth-required mux and its handler makes ONE "+
					"s.executeTask(w, r, \"task_close\", args, id, back) call", rec.Code, len(ex.calls))
			}
			call := ex.calls[0]
			if call.Tool != "task_close" {
				t.Errorf("tool = %q, want task_close (D1: no new tool; Done is the existing task_close)", call.Tool)
			}
			if call.Actor != "dashboard:salvo" {
				t.Errorf("actor = %q, want dashboard:salvo (dashboard:{session user}; the actor is what tells a "+
					"board close apart from other closes)", call.Actor)
			}
			if call.TaskID == nil || *call.TaskID != 7 {
				t.Errorf("Call.TaskID = %v, want 7. Criterion 10: executeTask sets it so audit_events.task_id "+
					"is the task, never NULL", call.TaskID)
			}
			var got map[string]any
			if err := json.Unmarshal(call.Args, &got); err != nil {
				t.Fatalf("args %s are not JSON: %v (criterion 7: json.Marshal, never string concatenation)", call.Args, err)
			}
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "reason,task_id" {
				t.Errorf("args keys = %v, want exactly [reason task_id]. Criterion 6: closeArgs is {task_id, reason} "+
					"and nothing else — a posted field must not add a key (args %s)", keys, call.Args)
			}
			if got["task_id"] != float64(7) {
				t.Errorf("args task_id = %v, want 7 — the PATH id (criterion 7: a note must not be able to "+
					"replace it; args %s)", got["task_id"], call.Args)
			}
			if got["reason"] != tc.wantWhy {
				t.Errorf("args reason = %q, want %q (D1: `done on the board`, or `done on the board: <trimmed note>`)",
					got["reason"], tc.wantWhy)
			}
		})
	}
}

// Criterion 6: a non-numeric or non-positive id is a 400 and reaches no executor.
func TestCloseTaskAction_BadIDReachesNoExecutor(t *testing.T) {
	for _, id := range []string{"abc", "0", "-3"} {
		t.Run(id, func(t *testing.T) {
			ex := &captureExec{}
			h, cookies := boardCloseHarness(t, ex)
			rec := postBoardClose(h, cookies, "/tasks/"+id+"/close", url.Values{"note": {"x"}})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /tasks/%s/close = %d, want 400 (criterion 6: a bad task id is a 400 from the "+
					"registered Done handler)", id, rec.Code)
			}
			if len(ex.calls) != 0 {
				t.Errorf("POST /tasks/%s/close reached the executor: %+v — a bad id must reach no executor", id, ex.calls)
			}
		})
	}
}

// Criterion 8 (and D5): 303 to /tasks + the four rebuilt filters + flash —
// `task_close ok` or the executor's error text verbatim — never an echo of the
// request's RawQuery or of any other posted key.
func TestCloseTaskAction_RedirectRebuildsFiltersAndCarriesTheFlash(t *testing.T) {
	const refusal = "task 7 is in_progress; refusing to close active work"
	for _, tc := range []struct {
		name      string
		ex        Exec
		form      url.Values
		wantQuery map[string]string
	}{
		{
			name: "ok with all four filters",
			ex:   &captureExec{},
			form: url.Values{
				"project": {"acme"}, "status": {"blocked"}, "assignee_type": {"human"}, "subproject": {"api"},
				"note": {"shipped"}, "next": {"//evil.example"}, "flash": {"spoofed"}, "reason_code": {"duplicate"},
			},
			wantQuery: map[string]string{
				"project": "acme", "status": "blocked", "assignee_type": "human", "subproject": "api",
				"flash": "task_close ok",
			},
		},
		{
			name: "refusal flashes the executor's error, unset filters stay absent",
			ex:   &closeErrExec{err: errors.New(refusal)},
			form: url.Values{"project": {"acme"}, "status": {""}, "note": {"x"}},
			wantQuery: map[string]string{
				"project": "acme", "flash": refusal,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cookies := boardCloseHarness(t, tc.ex)
			// The request URL carries its own query: the handler must read the
			// POSTED filters and never echo this one.
			rec := postBoardClose(h, cookies, "/tasks/7/close?next=%2F%2Fevil.example&project=fromurl", tc.form)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("POST /tasks/7/close = %d, want 303 back to the filtered board (criterion 8)", rec.Code)
			}
			raw := rec.Header().Get("Location")
			loc, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("Location %q: %v", raw, err)
			}
			if loc.Host != "" || loc.Scheme != "" || loc.Path != "/tasks" {
				t.Errorf("Location = %q, want the path /tasks on this app (criterion 8)", raw)
			}
			q := loc.Query()
			if len(q) != len(tc.wantQuery) {
				t.Errorf("Location query = %v, want exactly %v. Criterion 8 / D5: rebuilt from the four known "+
					"filter keys plus flash — never r.URL.RawQuery, never another posted key (next, note, "+
					"reason_code, a spoofed flash)", q, tc.wantQuery)
			}
			for k, v := range tc.wantQuery {
				if got := q.Get(k); got != v {
					t.Errorf("Location %s = %q, want %q (Location %q)", k, got, v, raw)
				}
			}
			for _, banned := range []string{"next", "note", "reason_code"} {
				if _, present := q[banned]; present {
					t.Errorf("Location %q carries %q; only the four filters and the flash round-trip", raw, banned)
				}
			}
		})
	}
}
