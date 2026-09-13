package replyfold_test

// SWT-40 Part C, C-D7 and criterion C6 (docs/tickets/inquiry-promote_SPEC.md):
// the replied-since fold MOVES out of internal/classify into this LEAF package,
// so the classify report and the inquiry promoter read ONE spelling of "has
// Salvador replied since" and "had he posted on the thread before".
//
// Plain unit test: no build tag, no database. The fold's SQL is proven against
// Postgres by internal/promote's inquiry integration suite (C4's mutations:
// direction and strictness) and by classify's summary integration suite (its
// output stays byte-identical, C6).
//
// ---- IMPOSED SURFACE (internal/replyfold/replyfold.go) -----------------------
//
//	// The thread-scope values (moved from classify; classify's own constants
//	// become aliases of these or are deleted — the strings are stored on every
//	// inquiry verdict and must not change).
//	const ScopeThread = "thread"; ScopeConversation = "conversation"; ScopeNone = "none"
//	// The three replied-since states (moved from classify's summary.go).
//	const StateOpen = "open"; StateAnsweredInThread = "answered in thread"
//	const StateSpokeSince = "spoke in conversation since"
//
//	// ScopeOf is the scope rule (moved from classify.threadScopeOf): none when
//	// the message has no thread, conversation for an UNROOTED slack key (asked
//	// of slackweb.IsRootedThreadKey), thread otherwise.
//	func ScopeOf(channel string, threadID int64, threadKey string) string
//	// State places a flagged verdict in exactly one state (moved from
//	// classify.inquiryState). A replied-since `none` stays open.
//	func State(scope string, repliedSince bool) string
//
//	// The SQL, anchored on an ai_extractions row aliased `e` whose fields carry
//	// normalized_message_id and thread_id (every inquiry verdict does). JoinSQL
//	// is the LEFT JOINs (aliases t and lo), set-based — ONE scan per query,
//	// never a correlated EXISTS per row (the 21.5 s production measurement in
//	// summary.go's comment).
//	const JoinSQL = `...`
//	// RepliedSinceCol: a STRICTLY later outbound on the verdict's thread (ties
//	// read OPEN, SWT-33 note 9).
//	const RepliedSinceCol = `COALESCE(lo.last_outbound > t.sent_at, false)`
//	// PriorParticipationCol: an outbound on the thread STRICTLY before the ask
//	// (C-D3's "he posted on that thread strictly before the ask"), read from the
//	// SAME join (the earliest outbound per thread), so it costs no second scan.
//	const PriorParticipationCol = `COALESCE(lo.first_outbound < t.sent_at, false)`
//
// GREENFIELD NOTE — EXPECTED RED: internal/replyfold has no non-test Go files,
// so `go test ./internal/replyfold/` fails with "no non-test Go files" (and,
// once a stub exists, "undefined: replyfold.ScopeOf" and friends).

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/classify"
	"github.com/sspataro57/switchboard/internal/replyfold"
)

// ---- the scope rule ------------------------------------------------------------

func TestScopeOf(t *testing.T) {
	cases := []struct {
		channel  string
		threadID int64
		key      string
		want     string
		why      string
	}{
		{"gmail", 0, "", "none", "no thread: nothing can be replied IN"},
		{"slack", 0, "", "none", "no thread wins over the channel"},
		{"gmail", 7, "gmail:acct:thread-1", "thread", "a gmail thread is thread-exact"},
		{"jira", 7, "jira:sspataro.atlassian.net:WEB-1204", "thread", "a jira issue is one thread"},
		{"slack", 7, "slack:T0HPR78RX:C07ABCDEF:p1757000000000100", "thread",
			"a rooted slack key names ONE thread (slackweb.IsRootedThreadKey)"},
		{"slack", 7, "slack:T0HPR78RX:C07ABCDEF", "conversation",
			"an unrooted slack channel key is the whole conversation — the weaker claim"},
		{"slack", 7, "slack:T0360B84U:D01EJRX6P45", "conversation",
			"an unthreaded DM is conversation-level too: the KEY cannot say 'thread'"},
	}
	for _, tc := range cases {
		if got := replyfold.ScopeOf(tc.channel, tc.threadID, tc.key); got != tc.want {
			t.Errorf("ScopeOf(%q, %d, %q) = %q, want %q — %s", tc.channel, tc.threadID, tc.key, got, tc.want, tc.why)
		}
	}
}

func TestState(t *testing.T) {
	cases := []struct {
		scope   string
		replied bool
		want    string
	}{
		{"thread", true, "answered in thread"},
		{"conversation", true, "spoke in conversation since"},
		{"none", true, "open"}, // the fold never upgrades a claim the data does not carry
		{"thread", false, "open"},
		{"conversation", false, "open"},
		{"none", false, "open"},
	}
	for _, tc := range cases {
		if got := replyfold.State(tc.scope, tc.replied); got != tc.want {
			t.Errorf("State(%q, %v) = %q, want %q", tc.scope, tc.replied, got, tc.want)
		}
	}
}

// The strings are STORED (thread_scope on every inquiry verdict) and RENDERED
// (the report's three states). Moving them must not change a byte, and classify
// must not keep a second, drifting copy with different values.
func TestConstants_AreTheStoredSpellingsAndClassifyAgrees(t *testing.T) {
	for _, c := range []struct{ got, want, name string }{
		{replyfold.ScopeThread, "thread", "ScopeThread"},
		{replyfold.ScopeConversation, "conversation", "ScopeConversation"},
		{replyfold.ScopeNone, "none", "ScopeNone"},
		{replyfold.StateOpen, "open", "StateOpen"},
		{replyfold.StateAnsweredInThread, "answered in thread", "StateAnsweredInThread"},
		{replyfold.StateSpokeSince, "spoke in conversation since", "StateSpokeSince"},
	} {
		if c.got != c.want {
			t.Errorf("replyfold.%s = %q, want %q (stored on verdicts / rendered by the report)", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct{ classify, fold, name string }{
		{classify.ScopeThread, replyfold.ScopeThread, "ScopeThread"},
		{classify.ScopeConversation, replyfold.ScopeConversation, "ScopeConversation"},
		{classify.ScopeNone, replyfold.ScopeNone, "ScopeNone"},
		{classify.StateOpen, replyfold.StateOpen, "StateOpen"},
		{classify.StateAnsweredInThread, replyfold.StateAnsweredInThread, "StateAnsweredInThread"},
		{classify.StateSpokeSince, replyfold.StateSpokeSince, "StateSpokeSince"},
	} {
		if c.classify != c.fold {
			t.Errorf("classify.%s = %q but replyfold.%s = %q: one spelling (C-D7)", c.name, c.classify, c.name, c.fold)
		}
	}
}

// ---- the SQL fragments -----------------------------------------------------------

// Strictness, both directions, pinned on the text: "later" is a STRICTLY later
// sent_at (a same-instant reply does not answer the question — ties read open),
// and "before" is STRICTLY before (a same-instant outbound is not prior
// participation). The integration suite's tie fixture is the behavioural half.
func TestColumns_AreStrictInBothDirections(t *testing.T) {
	col := strings.Join(strings.Fields(replyfold.RepliedSinceCol), " ")
	if !regexp.MustCompile(`last_outbound\s*>\s*t\.sent_at`).MatchString(col) || strings.Contains(col, ">=") {
		t.Errorf("RepliedSinceCol = %q; want a STRICT `lo.last_outbound > t.sent_at` (ties read open, SWT-33 note 9)", col)
	}
	if !strings.Contains(col, "COALESCE") || !strings.Contains(col, "false") {
		t.Errorf("RepliedSinceCol = %q; a thread with no outbound must read false, not NULL", col)
	}
	prior := strings.Join(strings.Fields(replyfold.PriorParticipationCol), " ")
	if !regexp.MustCompile(`first_outbound\s*<\s*t\.sent_at`).MatchString(prior) || strings.Contains(prior, "<=") {
		t.Errorf("PriorParticipationCol = %q; want a STRICT `lo.first_outbound < t.sent_at` (C-D3: strictly "+
			"before the ask; a same-instant post is not participation)", prior)
	}
	if !strings.Contains(prior, "COALESCE") || !strings.Contains(prior, "false") {
		t.Errorf("PriorParticipationCol = %q; a thread with no outbound must read false, not NULL", prior)
	}
}

// The join reads thread_id, direction and sent_at from normalized_messages and
// NOTHING else (SWT-33 criterion 30's allowlist, moved here with the SQL). What
// the model was shown — sender, subject, channel, body — comes from the stored
// verdict fields, never a join back. An identifier outside the list fails.
func TestJoinSQL_ReadsOnlyThreadDirectionAndSentAt(t *testing.T) {
	allowed := map[string]bool{}
	for _, w := range strings.Fields(`left join normalized_messages t on id e fields normalized_message_id
		bigint select thread_id max min sent_at as last_outbound first_outbound from where direction outbound
		and is not null group by lo nullif coalesce false`) {
		allowed[w] = true
	}
	for name, lit := range map[string]string{
		"JoinSQL": replyfold.JoinSQL, "RepliedSinceCol": replyfold.RepliedSinceCol,
		"PriorParticipationCol": replyfold.PriorParticipationCol,
	} {
		if strings.TrimSpace(lit) == "" {
			t.Errorf("replyfold.%s is empty", name)
			continue
		}
		for _, w := range regexp.MustCompile(`[A-Za-z_]+`).FindAllString(lit, -1) {
			if !allowed[strings.ToLower(w)] {
				t.Errorf("replyfold.%s uses %q, which the replied-since allowlist does not carry. The fold reads "+
					"thread_id, direction and sent_at; anything else is a second copy of what was classified", name, w)
			}
		}
	}
	j := strings.ToLower(strings.Join(strings.Fields(replyfold.JoinSQL), " "))
	if !strings.Contains(j, "direction = 'outbound'") {
		t.Errorf("JoinSQL does not filter direction = 'outbound' (invariant 5's marker): %s", j)
	}
	if !strings.Contains(j, "group by thread_id") {
		t.Errorf("JoinSQL is not set-based (no GROUP BY thread_id): a correlated EXISTS per verdict is the 21.5 s "+
			"production shape summary.go records. %s", j)
	}
	if strings.Contains(j, "exists") {
		t.Errorf("JoinSQL carries an EXISTS; the fold is one set-based scan per query")
	}
}

// ---- C6: a LEAF package ----------------------------------------------------------

// "replyfold imports only stdlib and slackweb". It is shared by classify (which
// reaches internal/provider by construction) and promote (which must NOT reach
// provider — its transitive-reachability test walks through this package), so
// one wrong import here breaks the promoter's structural no-LLM guarantee.
func TestReplyfold_ImportsOnlyStdlibAndSlackweb(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	const slackweb = "github.com/sspataro57/switchboard/internal/connector/slackweb"
	sources := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		sources++
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", n), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, spec := range f.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			stdlib := !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
			if !stdlib && p != slackweb {
				t.Errorf("internal/replyfold/%s imports %q. C6: this leaf imports only stdlib and slackweb", n, p)
			}
		}
	}
	if sources == 0 {
		t.Fatalf("internal/replyfold has no non-test Go files; a scan with nothing to scan proves nothing")
	}
}

// The scope rule reaches the slack key shape ONLY through slackweb's helper —
// the key-spelling rule of SWT-33 criterion 14, extended to this package (C5).
func TestReplyfold_AsksSlackwebAboutTheKeyShape(t *testing.T) {
	var code strings.Builder
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		code.Write(b)
	}
	src := code.String()
	if !strings.Contains(src, "slackweb.IsRootedThreadKey(") {
		t.Errorf("internal/replyfold never calls slackweb.IsRootedThreadKey; the scope rule's rooted/unrooted " +
			"question has ONE spelling and it lives in slackweb")
	}
	surgery := regexp.MustCompile(`strings\.(Split|SplitN|Count|LastIndex|Index|HasSuffix|HasPrefix|Contains|Fields)\b`)
	for i, line := range strings.Split(src, "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "//") {
			continue
		}
		if (strings.Contains(line, "hreadKey") || strings.Contains(line, "thread_key") || strings.Contains(line, "slack:")) &&
			surgery.MatchString(line) {
			t.Errorf("internal/replyfold line %d picks apart a thread key: %s", i+1, tl)
		}
	}
}
