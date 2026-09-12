//go:build integration

package dashboard_test

// SWT-43 (docs/tickets/delivery-deny_SPEC.md) criteria 29-31, 33, 34 through
// the REAL dashboard handler: httptest + dev-login session, the real executor
// and policy Matrix (newDashServer), no fakes. Build-tagged `integration` AND
// env-gated on DATABASE_URL (dashGuard refuses 192.168.50.49).
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run DeliveriesReject ./internal/dashboard/
//
// THE json.Marshal PROOF (criterion 33): the Redo note contains a double
// quote, a backslash and a newline. A handler that builds the args with
// fmt.Sprintf produces invalid JSON (or a different string) for that input;
// only json.Marshal stores it byte-for-byte. Mutation: swap json.Marshal for
// Sprintf in the handler -> red.
//
// GREENFIELD NOTE — EXPECTED RED: no POST /deliveries/{id}/reject route (the
// POST falls through to a 404/405), no reject_delivery tool, and no 0028.
//
// Cleanup pact (dashboard_integration_test.go's): project itest-deny-dash-proj,
// FK order — its reject_delivery audit rows (policy_decisions first; the
// dashboard's execute sets no Call.TaskID, so they are found by args), the
// approvals it wrote (no FK to deliveries), events, deliveries, tasks, project;
// at start and end.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const ddSlug = "itest-deny-dash-proj"

func cleanupDenyDash(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const projs = `(SELECT id FROM projects WHERE slug='` + ddSlug + `')`
	const tasksOf = `(SELECT id FROM tasks WHERE project_id IN ` + projs + `)`
	const delIDs = `(SELECT id::text FROM deliveries WHERE task_id IN ` + tasksOf + `)`
	const ours = `(SELECT id FROM audit_events WHERE tool='reject_delivery' AND args->>'delivery_id' IN ` + delIDs + `)`
	for _, q := range []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN ` + ours,
		`DELETE FROM audit_events WHERE id IN ` + ours,
		`DELETE FROM approvals WHERE subject_type='delivery' AND subject_id IN (SELECT id FROM deliveries WHERE task_id IN ` + tasksOf + `)`,
		`DELETE FROM task_events WHERE task_id IN ` + tasksOf,
		`DELETE FROM deliveries WHERE task_id IN ` + tasksOf,
		`DELETE FROM tasks WHERE project_id IN ` + projs,
		`DELETE FROM projects WHERE slug='` + ddSlug + `'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// ddRow returns the rendered <tr> for delivery id (the template's first cell
// is <td>{{.ID}}</td>), or "" when the page does not list it.
func ddRow(page string, id int64) string {
	for _, seg := range strings.Split(page, "<tr") {
		if strings.Contains(seg, "<td>"+strconv.FormatInt(id, 10)+"</td>") {
			return seg
		}
	}
	return ""
}

func TestDeliveriesReject_Integration_DenyAndRedoThroughTheHandler(t *testing.T) {
	dashGuard(t)
	ctx := context.Background()
	pool := dashPool(t, ctx)
	defer pool.Close()
	cleanupDenyDash(t, ctx, pool)
	defer cleanupDenyDash(t, ctx, pool)

	var projectID, taskID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, slug, client, execution, delivery, repo_path, ai_locality)
		 VALUES ($1,$1,'itest-deny-dash','manual','dashboard','/tmp/itest','any') RETURNING id`, ddSlug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// done_locally: Redo's precondition (D7).
	if err := pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, title, assignee_type, status)
		 VALUES ($1,'DENY dash work','claude','done_locally') RETURNING id`, projectID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	drafted := func(body string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO deliveries (task_id, channel, body, subject, status, created_by)
			 VALUES ($1,'gmail',$2,'Re: login broken','drafted','itest-deny-dash') RETURNING id`, taskID, body).Scan(&id); err != nil {
			t.Fatalf("seed drafted delivery: %v", err)
		}
		return id
	}
	redo, deny, oddCase, untouched := drafted("DDASH redo body"), drafted("DDASH deny body"),
		drafted("DDASH odd-case body"), drafted("DDASH untouched body")

	ts, client := newDashServer(t, ctx, pool)
	defer ts.Close()
	noFollow := &http.Client{Jar: client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	post := func(id int64, note, redraft string) {
		t.Helper()
		resp, err := noFollow.PostForm(ts.URL+"/deliveries/"+strconv.FormatInt(id, 10)+"/reject",
			url.Values{"note": {note}, "redraft": {redraft}})
		if err != nil {
			t.Fatalf("POST /deliveries/%d/reject: %v", id, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /deliveries/%d/reject = %d, want 303 to /deliveries with the flash (criterion 29)", id, resp.StatusCode)
		}
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || loc.Path != "/deliveries" {
			t.Fatalf("redirect Location = %q, want /deliveries?flash=...", resp.Header.Get("Location"))
		}
		if flash := loc.Query().Get("flash"); flash != "reject_delivery ok" {
			t.Fatalf("flash = %q, want \"reject_delivery ok\" (the executor accepted the call)", flash)
		}
	}
	type stored struct {
		note    *string
		redraft bool
		status  string
	}
	read := func(id int64) stored {
		t.Helper()
		var s stored
		if err := pool.QueryRow(ctx,
			`SELECT status, rejection_note, redraft_requested_at IS NOT NULL FROM deliveries WHERE id=$1`, id).
			Scan(&s.status, &s.note, &s.redraft); err != nil {
			t.Fatalf("read delivery %d: %v", id, err)
		}
		return s
	}

	// ---- Redo, with a note that breaks Sprintf-built JSON --------------------
	const trickyNote = `he said "no" \ twice` + "\n" + `then: shorter`
	post(redo, trickyNote, "true")
	if s := read(redo); s.status != "rejected" || !s.redraft {
		t.Errorf("after Redo: status=%q redraft=%v, want rejected + redraft requested", s.status, s.redraft)
	} else if s.note == nil || *s.note != trickyNote {
		got := "<NULL>"
		if s.note != nil {
			got = *s.note
		}
		t.Errorf("stored rejection_note = %q, want byte-equal %q. The handler must build its args with "+
			"json.Marshal (criterion 33), never fmt.Sprintf", got, trickyNote)
	}
	var auditNote string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(args->>'note','') FROM audit_events WHERE tool='reject_delivery' AND actor='dashboard:salvo'
		  AND args->>'delivery_id'=$1 ORDER BY id DESC LIMIT 1`, strconv.FormatInt(redo, 10)).Scan(&auditNote); err != nil {
		t.Errorf("no reject_delivery audit row for delivery %d by dashboard:salvo: %v (invariant 3: one executor call)", redo, err)
	} else if auditNote != trickyNote {
		t.Errorf("audit args note = %q, want %q", auditNote, trickyNote)
	}

	// ---- Deny ------------------------------------------------------------------
	post(deny, "not needed", "false")
	if s := read(deny); s.status != "rejected" || s.redraft {
		t.Errorf("after Deny: status=%q redraft=%v, want rejected with redraft_requested_at NULL", s.status, s.redraft)
	}

	// ---- redraft is true IFF the submitted value is exactly "true" --------------
	post(oddCase, "case matters", "TRUE")
	if s := read(oddCase); s.redraft {
		t.Errorf("redraft=TRUE set redraft_requested_at; criterion 29: redraft is true iff the value is \"true\"")
	}

	// ---- GET /deliveries?status=rejected (criteria 30, 31) ----------------------
	code, page := get(t, client, ts.URL+"/deliveries?status=rejected")
	if code != 200 {
		t.Fatalf("GET /deliveries?status=rejected = %d\n%s", code, snippet(page))
	}
	for _, id := range []int64{redo, deny, oddCase} {
		if ddRow(page, id) == "" {
			t.Errorf("/deliveries?status=rejected does not list rejected delivery %d", id)
		}
	}
	if ddRow(page, untouched) != "" {
		t.Errorf("/deliveries?status=rejected lists the still-drafted delivery %d", untouched)
	}
	idStr := func(id int64) string { return strconv.FormatInt(id, 10) }

	redoRow := ddRow(page, redo)
	if !strings.Contains(redoRow, "redraft requested") {
		t.Errorf("the Redo row does not show \"redraft requested\" under its status (criterion 30)")
	}
	if strings.Contains(redoRow, "/deliveries/"+idStr(redo)+"/reject") {
		t.Errorf("a redraft-requested row still offers the reject form; D6: a Redo cannot be withdrawn")
	}
	denyRow := ddRow(page, deny)
	if !strings.Contains(denyRow, `action="/deliveries/`+idStr(deny)+`/reject"`) || !strings.Contains(denyRow, ">Redo<") {
		t.Errorf("a plain-rejected row must get the reject form with the Redo button (D6: the Deny->Redo upgrade)")
	}
	if strings.Contains(denyRow, ">Deny<") {
		t.Errorf("a plain-rejected row offers Deny again; it gets the Redo button only (criterion 30)")
	}
	if !strings.Contains(denyRow, "not needed") {
		t.Errorf("the rejected row does not show its note (criterion 30; listDeliveries selects it, criterion 31)")
	}
	for _, id := range []int64{redo, deny, oddCase} {
		row := ddRow(page, id)
		for _, verb := range []string{"/approve", "/send", "/mark-sent", "/edit"} {
			if strings.Contains(row, "/deliveries/"+idStr(id)+verb) {
				t.Errorf("rejected delivery %d still offers %s", id, verb)
			}
		}
	}

	// ---- an untouched drafted row gets both buttons, and still Approve ----------
	_, all := get(t, client, ts.URL+"/deliveries")
	row := ddRow(all, untouched)
	for _, want := range []string{
		`action="/deliveries/` + idStr(untouched) + `/reject"`, ">Deny<", ">Redo<", `name="note"`,
		`action="/deliveries/` + idStr(untouched) + `/approve"`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("drafted row %d lacks %s (criterion 30)", untouched, want)
		}
	}

	// ---- criterion 34: the task detail page reads 'rejected' unchanged ----------
	if _, detail := get(t, client, ts.URL+"/tasks/"+strconv.FormatInt(taskID, 10)); !strings.Contains(detail, "rejected") {
		t.Errorf("/tasks/%d does not show the rejected delivery's status", taskID)
	}
}
