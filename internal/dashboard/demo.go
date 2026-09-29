package dashboard

// demo-mode (SWT-99, docs/tickets/demo-mode_SPEC.md): a READ filter on the web
// dashboard for a live client demo, switched only in the database
// (ops_flags.name = 'demo_mode'). This file is the one place visibility is
// spelled: every dashboard query that reads a guarded table carries an
// @demo.<kind>(<alias>) marker that demoSQL expands here, the verbs' pre-executor
// checks live here, and so do the exemption and seam lists the structure test
// holds the rest of the package to (demo_structure_test.go).
//
// Nothing here writes. The dashboard never writes the flag or tasks.demo_hidden;
// psql from docs/runbooks/demo-mode.md is their only writer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// briefTitlePattern is R7's morning-brief title pattern, the orchestrator's
// dedup key. listBriefs and @demo.task share this ONE spelling (criterion 5):
// a brief's body summarises every project, so demo mode hides brief tasks
// wholesale rather than parsing their text (D4).
const briefTitlePattern = "Morning brief %"

// demoScope is one request's view: On with the allowlisted project slugs
// (compared exactly) and source-account emails (stored lower-cased, compared
// case-insensitively).
type demoScope struct {
	On       bool
	Projects []string
	Accounts []string
}

// decodeDemoScope implements the SPEC's D1 table. An absent row is off; a
// clean {"on": false} is off; {"on": true} is on with the lists given (a
// missing list is empty); EVERY other shape fails closed: on, nothing visible.
func decodeDemoScope(raw []byte, present bool) demoScope {
	if !present {
		return demoScope{}
	}
	closed := demoScope{On: true}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return closed
	}
	onRaw, ok := obj["on"]
	if !ok {
		return closed
	}
	var on bool
	if err := json.Unmarshal(onRaw, &on); err != nil || strings.TrimSpace(string(onRaw)) == "null" {
		return closed
	}
	list := func(key string) ([]string, bool) {
		v, ok := obj[key]
		if !ok {
			return nil, true
		}
		var out []string
		if err := json.Unmarshal(v, &out); err != nil || strings.TrimSpace(string(v)) == "null" {
			return nil, false
		}
		return out, true
	}
	projects, okP := list("projects")
	accounts, okA := list("source_accounts")
	if !okP || !okA {
		return closed // a malformed list fails closed even beside on:false
	}
	if !on {
		return demoScope{}
	}
	for i, a := range accounts {
		accounts[i] = strings.ToLower(strings.TrimSpace(a))
	}
	return demoScope{On: true, Projects: projects, Accounts: accounts}
}

// accountVisible is @demo.account's Go-side twin, for addresses that arrive in
// Go rather than SQL (the resolved From on /deliveries). Off shows every account.
func (sc demoScope) accountVisible(email string) bool {
	if !sc.On {
		return true
	}
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false
	}
	for _, a := range sc.Accounts {
		if a == e {
			return true
		}
	}
	return false
}

type demoScopeKey struct{}

func withDemoScope(ctx context.Context, sc demoScope) context.Context {
	return context.WithValue(ctx, demoScopeKey{}, sc)
}

// demoScopeFrom returns the request's scope. A context without one is a
// handler reached around the wrapper: it gets demo ON with empty lists, so it
// shows nothing rather than everything (criterion 2).
func demoScopeFrom(ctx context.Context) demoScope {
	if sc, ok := ctx.Value(demoScopeKey{}).(demoScope); ok {
		return sc
	}
	return demoScope{On: true}
}

// loadDemoScope reads the flag. An absent row is off; a failed read is an
// error the wrapper turns into a 503 (D1: never "off").
func (s *Server) loadDemoScope(ctx context.Context) (demoScope, error) {
	if s.demoStub {
		return demoScope{}, nil
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value::text FROM ops_flags WHERE name = 'demo_mode'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return decodeDemoScope(nil, false), nil
	}
	if err != nil {
		return demoScope{}, err
	}
	return decodeDemoScope(raw, true), nil
}

// demoScoped loads the scope once per request and hands it to the handler in
// the request context. It sits inside s.auth.Require on every authenticated
// route and nowhere else (/healthz, /static and the login routes never read
// the flag, criterion 4).
func (s *Server) demoScoped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.loadDemoScope(r.Context())
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r.WithContext(withDemoScope(r.Context(), sc)))
	})
}

// demoFragments is the whole visibility rule, one fragment per marker kind.
// In each, ALIAS is the marker's argument, ON / PROJECTS / ACCOUNTS are the
// scope's binds and SEQ makes the fragment's own aliases unique within one
// statement. Every fragment is wrapped in (NOT ON OR …) by demoSQL, so demo-off
// runs the same text with ON = false (criterion 5).
//
// task: the task's project is allowlisted, it is not an R7 brief, and neither
// it nor any ancestor carries demo_hidden (D9: a hidden task hides every
// descendant, walked at read time; UNION, not UNION ALL, so a corrupt
// parent_id cycle terminates).
var demoFragments = map[string]string{
	"project": `ALIAS.slug = ANY(PROJECTS)`,
	"task": `EXISTS (SELECT 1 FROM projects zdp_SEQ WHERE zdp_SEQ.id = ALIAS.project_id AND zdp_SEQ.slug = ANY(PROJECTS))
	     AND ALIAS.title NOT LIKE 'BRIEF'
	     AND NOT EXISTS (WITH RECURSIVE zanc_SEQ(id, parent_id, demo_hidden) AS (
	           SELECT zta_SEQ.id, zta_SEQ.parent_id, zta_SEQ.demo_hidden FROM tasks zta_SEQ WHERE zta_SEQ.id = ALIAS.id
	           UNION
	           SELECT ztb_SEQ.id, ztb_SEQ.parent_id, ztb_SEQ.demo_hidden FROM tasks ztb_SEQ JOIN zanc_SEQ ON ztb_SEQ.id = zanc_SEQ.parent_id)
	         SELECT 1 FROM zanc_SEQ WHERE zanc_SEQ.demo_hidden)`,
	"delivery": `ALIAS.channel <> 'upwork_chat'
	     AND EXISTS (SELECT 1 FROM tasks zdt_SEQ WHERE zdt_SEQ.id = ALIAS.task_id AND TASK(zdt_SEQ))`,
	"account": `lower(ALIAS.account_email) = ANY(ACCOUNTS)`,
	"message": `ALIAS.channel <> 'upwork'
	     AND EXISTS (SELECT 1 FROM raw_source_items zri_SEQ JOIN source_accounts zsa_SEQ ON zsa_SEQ.id = zri_SEQ.source_account_id
	                  WHERE zri_SEQ.id = ALIAS.raw_source_item_id AND lower(zsa_SEQ.account_email) = ANY(ACCOUNTS))`,
	"ref": `ALIAS.system <> 'upwork_crm'`,
}

var demoMarker = regexp.MustCompile(`@demo\.([A-Za-z_]+)\(([A-Za-z_][A-Za-z0-9_]*)\)`)

// demoSQL expands every @demo.<kind>(<alias>) marker in q. The caller's args
// come first, unchanged; each scope value a marker uses is then bound ONCE per
// statement ($on as a bool, the projects and the lower-cased accounts as text
// arrays), numbered after the caller's. An unknown marker is an error.
func demoSQL(sc demoScope, q string, args []any) (string, []any, error) {
	out := append([]any(nil), args...)
	binds := map[string]string{}
	bind := func(name string, v any) string {
		if p, ok := binds[name]; ok {
			return p
		}
		out = append(out, v)
		p := fmt.Sprintf("$%d", len(out))
		switch v.(type) {
		case bool:
			p += "::boolean"
		case []string:
			p += "::text[]"
		}
		binds[name] = p
		return p
	}
	projects := func() string { return bind("projects", demoList(sc.Projects)) }
	accounts := func() string { return bind("accounts", demoLowered(sc.Accounts)) }
	seq := 0
	var expand func(kind, alias string) (string, error)
	expand = func(kind, alias string) (string, error) {
		frag, ok := demoFragments[kind]
		if !ok {
			return "", fmt.Errorf("demoSQL: unknown marker @demo.%s", kind)
		}
		seq++
		// Every placeholder is substituted BEFORE the caller's alias goes in,
		// so an alias can never be mistaken for a placeholder (review: an
		// uppercase alias such as PROJECTS would otherwise be rewritten).
		s := strings.ReplaceAll(frag, "SEQ", fmt.Sprint(seq))
		s = strings.ReplaceAll(s, "BRIEF", strings.ReplaceAll(briefTitlePattern, "'", "''"))
		if strings.Contains(s, "PROJECTS") {
			s = strings.ReplaceAll(s, "PROJECTS", projects())
		}
		if strings.Contains(s, "ACCOUNTS") {
			s = strings.ReplaceAll(s, "ACCOUNTS", accounts())
		}
		for strings.Contains(s, "TASK(") {
			i := strings.Index(s, "TASK(")
			j := strings.Index(s[i:], ")")
			inner, err := expand("task", s[i+len("TASK("):i+j])
			if err != nil {
				return "", err
			}
			s = s[:i] + "(" + inner + ")" + s[i+j+1:]
		}
		return strings.ReplaceAll(s, "ALIAS", alias), nil
	}
	var firstErr error
	expanded := demoMarker.ReplaceAllStringFunc(q, func(m string) string {
		sub := demoMarker.FindStringSubmatch(m)
		body, err := expand(sub[1], sub[2])
		if err != nil && firstErr == nil {
			firstErr = err
		}
		on := bind("on", sc.On)
		return "(NOT " + on + " OR (" + body + "))"
	})
	if firstErr != nil {
		return "", nil, firstErr
	}
	if strings.Contains(expanded, "@demo.") {
		return "", nil, fmt.Errorf("demoSQL: malformed @demo. marker in %q", q)
	}
	return expanded, out, nil
}

// demoList never binds a nil slice: pgx sends nil as SQL NULL, and
// x = ANY(NULL) is NULL, which NOT $on OR … would then treat as unknown.
func demoList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func demoLowered(v []string) []string {
	out := make([]string, len(v))
	for i, a := range v {
		out[i] = strings.ToLower(a)
	}
	return out
}

// demoQuery / demoQueryRow run q through demoSQL with the request's scope.
func (s *Server) demoQuery(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	sql, all, err := demoSQL(demoScopeFrom(ctx), q, args)
	if err != nil {
		return nil, err
	}
	return s.pool.Query(ctx, sql, all...)
}

func (s *Server) demoQueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	sql, all, err := demoSQL(demoScopeFrom(ctx), q, args)
	if err != nil {
		return demoErrRow{err}
	}
	return s.pool.QueryRow(ctx, sql, all...)
}

type demoErrRow struct{ err error }

func (r demoErrRow) Scan(...any) error { return r.err }

// visibleTask / visibleDelivery / visiblePlan are the verbs' pre-executor
// checks (criterion 17, D3): false for a hidden id and a nonexistent id alike.
func (s *Server) visibleTask(ctx context.Context, sc demoScope, id int64) (bool, error) {
	return s.demoExists(ctx, sc, `SELECT EXISTS (SELECT 1 FROM tasks t WHERE t.id = $1 AND @demo.task(t))`, id)
}

func (s *Server) visibleDelivery(ctx context.Context, sc demoScope, id int64) (bool, error) {
	return s.demoExists(ctx, sc, `SELECT EXISTS (SELECT 1 FROM deliveries d WHERE d.id = $1 AND @demo.delivery(d))`, id)
}

func (s *Server) visiblePlan(ctx context.Context, sc demoScope, id int64) (bool, error) {
	return s.demoExists(ctx, sc, `SELECT EXISTS (SELECT 1 FROM plan_imports pi JOIN projects p ON p.id = pi.project_id
	                              WHERE pi.id = $1 AND @demo.project(p))`, id)
}

func (s *Server) demoExists(ctx context.Context, sc demoScope, q string, id int64) (bool, error) {
	if s.demoStub {
		return true, nil
	}
	sql, args, err := demoSQL(sc, q, []any{id})
	if err != nil {
		return false, err
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// demoExempt names the owners whose guarded reads carry no marker, each with
// its reason (criterion 7). The structure test allows only the kinds the SPEC
// lists.
var demoExempt = map[string]string{
	"demoFragments": "the visibility rule itself: its subqueries are what the markers expand to",
	"reopenMarkers": "its task ids come from boardRows, which already passed @demo.task",
	"taskEvents":    "keyed on a task id that showTask already passed through @demo.task",
	"taskFeedback":  "keyed on a task id that showTask already passed through @demo.task",
}

// demoSeams names every call into another package that takes the dashboard's
// pool, with why its result is safe (or gated) in demo mode (criterion 8).
var demoSeams = map[string]string{
	"orchestrator.Health":             "global event counts: /tasks skips it in demo and /funnel shows only its verdict",
	"capture.AttributionTrend":        "all-account totals: /funnel does not run it in demo (D7)",
	"classify.Summarize":              "every lane incl. personal, raw senders and subjects: /funnel does not run it in demo (D7)",
	"promote.CountersByLane":          "all-lane counters: /funnel does not run it in demo (D7)",
	"availability.CalendarSyncStates": "only colours calendar rows the @demo.account health query already returned",
	"tools.ResolveGmailRoute":         "a visible delivery's route; its From is shown only if accountVisible (criterion 20)",
	"tools.MailAttachmentsForRawItem": "keyed on the raw item of a source message that already passed @demo.message",
}
