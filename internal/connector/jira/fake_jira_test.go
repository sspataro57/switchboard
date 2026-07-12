package jira_test

// Shared httptest fake of the Jira Cloud REST v2 surface (SPEC
// 09-jira-github-connectors, Decision 3: v2 everywhere). NO build tag: it
// compiles into both the offline poller unit binary (poller_test.go) and the
// `-tags integration` connector walk (integration_test.go), so the fake is
// defined once. NEVER a live Jira call (invariant 7 discipline).
//
// It serves exactly the four endpoints the hand-rolled client calls
// (SPEC criterion 3 + Decision 3):
//
//   GET /rest/api/2/myself                        (own accountId, cached in cursor)
//   GET /rest/api/2/search/jql   (jql, nextPageToken)   — the new v2 endpoint
//   GET /rest/api/2/issue/{key}  (full issue incl. fields.comment)
//   GET /rest/api/2/issue/{key}/comment (startAt)       — comment paging
//
// The fake records every request so tests can pin the JQL scoping/order,
// nextPageToken pagination, the once-per-run /myself, and the
// total>inline comment-paging branch. forceIssueStatus injects a non-200 on an
// issue GET for the cursor-unadvanced error path.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

const (
	fakeSiteHost = "sspataro.atlassian.net"
	fakeOwnAcc   = "acc-own-bot"
	fakeOtherAcc = "acc-client-777"
)

type fakeComment struct {
	id      string
	author  string // accountId
	body    string
	created string
}

type fakeIssue struct {
	key         string
	updated     string // Jira v2 timestamp, e.g. 2026-07-01T10:00:00.000+0000
	created     string
	summary     string
	description string
	reporter    string // accountId
	assignee    string
	comments    []fakeComment
	inlineLimit int // how many comments the issue GET embeds; rest via /comment
}

type fakeJira struct {
	mu  sync.Mutex
	srv *httptest.Server

	ownAccountID   string
	issues         []fakeIssue // search order (ORDER BY updated ASC)
	searchPageSize int         // 0 => single page

	myselfCalls int
	searchJQLs  []string
	issueGets   []string
	commentGets []string

	forceIssueStatus map[string]int // issue key -> forced non-200 on GET /issue/{key}
}

func newFakeJira() *fakeJira {
	f := &fakeJira{ownAccountID: fakeOwnAcc, forceIssueStatus: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeJira) close()      { f.srv.Close() }
func (f *fakeJira) url() string { return f.srv.URL }

func (f *fakeJira) add(i fakeIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues = append(f.issues, i)
}

func (f *fakeJira) find(key string) *fakeIssue {
	for i := range f.issues {
		if f.issues[i].key == key {
			return &f.issues[i]
		}
	}
	return nil
}

func (f *fakeJira) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	q := r.URL.Query()

	switch {
	case path == "/rest/api/2/myself":
		f.myselfCalls++
		writeJJSON(w, 200, map[string]any{"accountId": f.ownAccountID, "displayName": "Bot"})

	case path == "/rest/api/2/search/jql":
		f.searchJQLs = append(f.searchJQLs, q.Get("jql"))
		f.writeSearchPage(w, q.Get("nextPageToken"))

	case strings.HasPrefix(path, "/rest/api/2/issue/") && strings.HasSuffix(path, "/comment"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/rest/api/2/issue/"), "/comment")
		f.commentGets = append(f.commentGets, key)
		iss := f.find(key)
		if iss == nil {
			writeJJSON(w, 404, map[string]any{"errorMessages": []string{"no issue " + key}})
			return
		}
		start := 0
		if s := q.Get("startAt"); s != "" {
			start, _ = strconv.Atoi(s)
		}
		comments := make([]map[string]any, 0)
		for _, c := range iss.comments[min(start, len(iss.comments)):] {
			comments = append(comments, commentObj(c))
		}
		writeJJSON(w, 200, map[string]any{
			"startAt": start, "maxResults": len(iss.comments), "total": len(iss.comments),
			"comments": comments,
		})

	case strings.HasPrefix(path, "/rest/api/2/issue/"):
		key := strings.TrimPrefix(path, "/rest/api/2/issue/")
		f.issueGets = append(f.issueGets, key)
		if code, ok := f.forceIssueStatus[key]; ok {
			writeJJSON(w, code, map[string]any{"errorMessages": []string{"forced error"}})
			return
		}
		iss := f.find(key)
		if iss == nil {
			writeJJSON(w, 404, map[string]any{"errorMessages": []string{"no issue " + key}})
			return
		}
		writeJJSON(w, 200, issueObj(*iss))

	default:
		writeJJSON(w, 404, map[string]any{"errorMessages": []string{"no route: " + path}})
	}
}

// writeSearchPage paginates the canned issues by searchPageSize. The page
// carries {key, fields:{updated}} entries plus a nextPageToken until exhausted.
func (f *fakeJira) writeSearchPage(w http.ResponseWriter, pageToken string) {
	start := 0
	if pageToken != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(pageToken, "tok-"))
	}
	end := len(f.issues)
	var next string
	if f.searchPageSize > 0 && start+f.searchPageSize < len(f.issues) {
		end = start + f.searchPageSize
		next = "tok-" + strconv.Itoa(end)
	}
	items := make([]map[string]any, 0, end-start)
	for _, iss := range f.issues[start:end] {
		items = append(items, map[string]any{
			"key":    iss.key,
			"fields": map[string]any{"updated": iss.updated},
		})
	}
	body := map[string]any{"issues": items, "isLast": next == ""}
	if next != "" {
		body["nextPageToken"] = next
	}
	writeJJSON(w, 200, body)
}

func commentObj(c fakeComment) map[string]any {
	return map[string]any{
		"id":      c.id,
		"author":  map[string]any{"accountId": c.author, "displayName": "U " + c.author},
		"body":    c.body,
		"created": c.created,
		"updated": c.created,
	}
}

func issueObj(iss fakeIssue) map[string]any {
	inline := iss.inlineLimit
	if inline <= 0 || inline > len(iss.comments) {
		inline = len(iss.comments)
	}
	inlineComments := make([]map[string]any, 0, inline)
	for _, c := range iss.comments[:inline] {
		inlineComments = append(inlineComments, commentObj(c))
	}
	return map[string]any{
		"key": iss.key,
		"fields": map[string]any{
			"summary":     iss.summary,
			"description": iss.description,
			"created":     iss.created,
			"updated":     iss.updated,
			"reporter":    map[string]any{"accountId": iss.reporter, "displayName": "R " + iss.reporter},
			"assignee":    map[string]any{"accountId": iss.assignee, "displayName": "A " + iss.assignee},
			"comment": map[string]any{
				"total": len(iss.comments), "startAt": 0, "maxResults": inline,
				"comments": inlineComments,
			},
		},
	}
}

func writeJJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
