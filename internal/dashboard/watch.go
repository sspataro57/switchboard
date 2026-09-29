package dashboard

// watch-json (SWT-101, docs/tickets/watch-json_SPEC.md): GET /watch.json, the
// Pebble swb watchface's read of the board. It sits outside the session layer
// (no s.auth.Require, no dev login, no demo wrapper): a bearer token from
// SWB_WATCH_TOKEN is its only gate, and the host it is reached on is LAN-only.
// It writes nothing and calls no tool: the counts are the board header's own
// boardTallies over boardView's sections, and the session lists are read off
// the same rows' lights.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// SetWatchToken arms /watch.json. The value is trimmed (Secret values often
// carry a trailing newline); blank or shorter than 32 bytes leaves the route
// disabled (404), so a placeholder or a typo can never guard it. Only sha256(token) is kept, and the value is never logged.
func (s *Server) SetWatchToken(token string) {
	token = strings.TrimSpace(token)
	s.watchDigest = nil
	if token == "" {
		return
	}
	if len(token) < 32 {
		slog.Warn("watch: SWB_WATCH_TOKEN is shorter than 32 bytes; /watch.json stays disabled")
		return
	}
	d := sha256.Sum256([]byte(token))
	s.watchDigest = d[:]
}

// watchAuth is the route's whole gate: disabled -> 404, a missing or wrong
// bearer token -> 401 with no data, else next. Every reply is no-store. The
// comparison is between equal-length digests in constant time, so neither the
// token nor its length leaks through timing.
func (s *Server) watchAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(s.watchDigest) == 0 {
			http.NotFound(w, r)
			return
		}
		scheme, presented, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		presented = strings.TrimSpace(presented)
		if ok && strings.EqualFold(scheme, "Bearer") && presented != "" {
			d := sha256.Sum256([]byte(presented))
			if subtle.ConstantTimeCompare(d[:], s.watchDigest) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized\n"))
	})
}

// watchSession is one listed session: its stored name and since when its
// light has held, in Unix seconds.
type watchSession struct {
	Session string `json:"session"`
	Since   int64  `json:"since"`
}

type watchReply struct {
	NeedYou   int            `json:"need_you"`
	InFlight  int            `json:"in_flight"`
	Incoming  int            `json:"incoming"`
	DoneToday int            `json:"done_today"`
	Waiting   []watchSession `json:"waiting"`
	Working   []watchSession `json:"working"`
}

// watchJSON answers the courier. It always renders the unfiltered default
// board (a query string never changes the reply) with demo mode explicitly OFF
// (watch-json D4: Salvador's wrist, never a shared screen).
func (s *Server) watchJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rr := r.Clone(watchScope(r.Context()))
	rr.URL.RawQuery = ""
	secs, facts, _, err := s.boardView(rr)
	if err != nil {
		slog.Error("watch: board read failed", "err", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable\n"))
		return
	}
	t := boardTallies(secs)
	waiting, working := watchSessions(secs, facts)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(watchReply{
		NeedYou: t.NeedYou, InFlight: t.InFlight, Incoming: t.Incoming, DoneToday: t.DoneToday,
		Waiting: waiting, Working: working,
	})
}

// watchSessions reads the session lists off the rows' lights (watch-json D2).
// waiting = a session's needs_input (red with a session tag); working = a
// fresh session working signal (a stale one is class "stale" and not listed).
// The name is the stored facts[id].Session, never the light's display text;
// an empty name is not listed. One entry per session with its OLDEST since, a
// session in both lists stays only in waiting, and both lists are ordered by
// since then name, never nil. Pure: no I/O, no clock.
func watchSessions(secs []boardSection, facts map[int64]lightFacts) (waiting, working []watchSession) {
	wait := map[string]int64{}
	work := map[string]int64{}
	keep := func(m map[string]int64, name string, since int64) {
		if prev, ok := m[name]; !ok || since < prev {
			m[name] = since
		}
	}
	for _, sec := range secs {
		for _, t := range sec.Tasks {
			f := facts[t.ID]
			name := f.Session
			if name == "" || t.Light.Session == "" {
				continue
			}
			switch t.Light.Class {
			case "input":
				keep(wait, name, f.StateUnix)
			case "working":
				keep(work, name, f.StateUnix)
			}
		}
	}
	for name := range wait {
		delete(work, name)
	}
	return watchList(wait), watchList(work)
}

func watchList(m map[string]int64) []watchSession {
	out := make([]watchSession, 0, len(m))
	for name, since := range m {
		out = append(out, watchSession{Session: name, Since: since})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Session < out[j].Session
	})
	return out
}
