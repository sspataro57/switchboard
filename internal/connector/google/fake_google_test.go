package google_test

// Shared httptest fake of the Google REST surface for the offline poller unit
// tests (poller_test.go) AND the compose-db integration suite
// (integration_test.go). NO build tag: it compiles into both the default
// `go test` binary and the `-tags integration` binary, so the fake is defined
// once. Never a live Google call (invariant 7 discipline; the SPEC pins
// httptest fakes for the whole buildable-now half).
//
// It serves exactly the four endpoints the hand-rolled clients call
// (SPEC "Files": gmail.go / calendar.go), keyed by the {userId} path segment so
// ONE server can stand in for multiple accounts (userId == account_email, a
// valid Gmail identifier) — the cross-account Message-ID dedup test needs the
// same server to return per-account corpora:
//
//   GET  /gmail/v1/users/{user}/profile
//   GET  /gmail/v1/users/{user}/messages            (q, pageToken)
//   GET  /gmail/v1/users/{user}/messages/{id}?format=full
//   GET  /calendar/v3/calendars/primary/events      (syncToken|timeMin/timeMax/singleEvents/pageToken)
//
// Gmail identity is the {userId} path segment. Calendar has no per-user path
// (always `primary`), so identity there rides on the OAuth-bearing *http.Client
// — in production the token; in tests an X-Fake-User header injected by
// userHTTPClient. The fake records every request so tests can assert the
// cursor/pagination/syncToken/410 contract, and supports a 410-on-syncToken
// mode.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// recordedReq is one captured inbound API request.
type recordedReq struct {
	User      string
	Q         string // gmail messages.list q
	PageToken string
	SyncToken string // calendar events.list syncToken
	TimeMin   string // calendar events.list timeMin
	TimeMax   string // calendar events.list timeMax
	Single    string // calendar singleEvents
}

// fakeGmailMsg is one canned message: its {id, threadId} for the list, and the
// full format=full JSON for the get.
type fakeGmailMsg struct {
	id       string
	threadID string
	full     json.RawMessage
}

type fakeGoogle struct {
	mu sync.Mutex

	srv *httptest.Server

	gmail         map[string][]fakeGmailMsg // user -> messages (list order)
	gmailPageSize int                       // 0 => single page
	getStatus     map[string]int            // messageID -> forced non-200 status

	calendar          map[string][]json.RawMessage // user -> event items
	calNextSyncToken  string
	cal410OnSyncToken bool

	listReqs    []recordedReq
	getReqs     []recordedReq
	calListReqs []recordedReq
}

func newFakeGoogle() *fakeGoogle {
	f := &fakeGoogle{
		gmail:            map[string][]fakeGmailMsg{},
		getStatus:        map[string]int{},
		calendar:         map[string][]json.RawMessage{},
		calNextSyncToken: "SYNCTOK-1",
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeGoogle) close()      { f.srv.Close() }
func (f *fakeGoogle) url() string { return f.srv.URL }

func (f *fakeGoogle) addGmail(user string, m fakeGmailMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gmail[user] = append(f.gmail[user], m)
}

func (f *fakeGoogle) addCalendar(user string, item json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calendar[user] = append(f.calendar[user], item)
}

func (f *fakeGoogle) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	q := r.URL.Query()

	switch {
	case strings.HasPrefix(path, "/gmail/v1/users/") && strings.HasSuffix(path, "/profile"):
		user := strings.TrimSuffix(strings.TrimPrefix(path, "/gmail/v1/users/"), "/profile")
		writeJSON(w, 200, map[string]any{"emailAddress": user})

	case strings.HasPrefix(path, "/gmail/v1/users/") && strings.Contains(path, "/messages/"):
		// messages.get: .../users/{user}/messages/{id}
		rest := strings.TrimPrefix(path, "/gmail/v1/users/")
		parts := strings.SplitN(rest, "/messages/", 2)
		user, id := parts[0], parts[1]
		f.getReqs = append(f.getReqs, recordedReq{User: user, PageToken: q.Get("pageToken")})
		if code, ok := f.getStatus[id]; ok {
			writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": "forced error"}})
			return
		}
		for _, m := range f.gmail[user] {
			if m.id == id {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write(m.full)
				return
			}
		}
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})

	case strings.HasPrefix(path, "/gmail/v1/users/") && strings.HasSuffix(path, "/messages"):
		// messages.list
		user := strings.TrimSuffix(strings.TrimPrefix(path, "/gmail/v1/users/"), "/messages")
		f.listReqs = append(f.listReqs, recordedReq{User: user, Q: q.Get("q"), PageToken: q.Get("pageToken")})
		f.writeGmailList(w, user, q.Get("pageToken"))

	case strings.HasPrefix(path, "/calendar/v3/calendars/") && strings.HasSuffix(path, "/events"):
		user := calendarUser(r)
		f.calListReqs = append(f.calListReqs, recordedReq{
			User: user, SyncToken: q.Get("syncToken"), TimeMin: q.Get("timeMin"),
			TimeMax: q.Get("timeMax"), Single: q.Get("singleEvents"), PageToken: q.Get("pageToken"),
		})
		if f.cal410OnSyncToken && q.Get("syncToken") != "" {
			writeJSON(w, http.StatusGone, map[string]any{"error": map[string]any{"code": 410, "message": "Sync token is no longer valid"}})
			return
		}
		items := f.calendar[user]
		writeJSON(w, 200, map[string]any{"items": rawSlice(items), "nextSyncToken": f.calNextSyncToken})

	default:
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": 404, "message": "no route: " + path}})
	}
}

// writeGmailList paginates the canned messages when gmailPageSize > 0.
func (f *fakeGoogle) writeGmailList(w http.ResponseWriter, user, pageToken string) {
	msgs := f.gmail[user]
	start := 0
	if pageToken != "" {
		_, _ = fmt.Sscanf(pageToken, "pg-%d", &start)
	}
	end := len(msgs)
	var next string
	if f.gmailPageSize > 0 && start+f.gmailPageSize < len(msgs) {
		end = start + f.gmailPageSize
		next = fmt.Sprintf("pg-%d", end)
	}
	out := make([]map[string]any, 0, end-start)
	for _, m := range msgs[start:end] {
		out = append(out, map[string]any{"id": m.id, "threadId": m.threadID})
	}
	body := map[string]any{"messages": out}
	if next != "" {
		body["nextPageToken"] = next
	}
	writeJSON(w, 200, body)
}

// calendarUser resolves the account for a calendar request. In tests the
// per-account http.Client (userHTTPClient) sets X-Fake-User; a ?user= override
// is also honoured for convenience.
func calendarUser(r *http.Request) string {
	if u := r.URL.Query().Get("user"); u != "" {
		return u
	}
	return r.Header.Get("X-Fake-User")
}

// userHTTPClient returns an *http.Client that tags every request with the
// account email, standing in for the per-account OAuth token so ONE fake server
// can serve multiple accounts' calendars. Pass it where production wires the
// oauth2 http.Client.
func userHTTPClient(email string) *http.Client {
	return &http.Client{Transport: fakeUserRT{email: email, base: http.DefaultTransport}}
}

type fakeUserRT struct {
	email string
	base  http.RoundTripper
}

func (rt fakeUserRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Fake-User", rt.email)
	return rt.base.RoundTrip(r)
}

func rawSlice(items []json.RawMessage) []json.RawMessage {
	if items == nil {
		return []json.RawMessage{}
	}
	return items
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// gmailFull builds a canned format=full Gmail message JSON (a single text/plain
// body) with the given RFC Message-ID, From/To, thread and internalDate.
func gmailFull(id, threadID, messageID, from, to string, internalDateMS int64, body string) json.RawMessage {
	m := map[string]any{
		"id":           id,
		"threadId":     threadID,
		"labelIds":     []string{"INBOX"},
		"snippet":      "snippet for " + id,
		"internalDate": fmt.Sprintf("%d", internalDateMS),
		"payload": map[string]any{
			"mimeType": "text/plain",
			"headers": []map[string]string{
				{"name": "Message-ID", "value": messageID},
				{"name": "Subject", "value": "Subject " + id},
				{"name": "From", "value": from},
				{"name": "To", "value": to},
			},
			"body": map[string]any{"data": b64url(body)},
		},
	}
	b, _ := json.Marshal(m)
	return b
}

// calFull builds a canned singleEvents calendar item (timed, opaque).
func calFull(id, summary, start, end string) json.RawMessage {
	m := map[string]any{
		"id":           id,
		"status":       "confirmed",
		"summary":      summary,
		"transparency": "opaque",
		"start":        map[string]string{"dateTime": start},
		"end":          map[string]string{"dateTime": end},
	}
	b, _ := json.Marshal(m)
	return b
}
