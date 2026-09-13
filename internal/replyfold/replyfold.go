// Package replyfold is the ONE spelling of the replied-since fold (SWT-33's
// "still open", moved here by SWT-40 Part C, C-D7): the thread-scope rule, the
// three states a flagged inquiry verdict can be in, and the SQL that asks
// "has Salvador replied since" and "had he posted on the thread before".
//
// It is a LEAF on purpose (criterion C6): it imports only the standard library
// and internal/connector/slackweb. Two packages read it — internal/classify
// (the report, which reaches internal/provider by construction) and
// internal/promote (the inquiry gate, which must NOT reach provider; its
// transitive-reachability test walks through this package). One wrong import
// here would break the promoter's structural no-LLM guarantee.
//
// The fold is a statement about the world NOW (a later outbound message on
// the verdict's thread), never written back: the verdict row is never updated,
// so the rule stays tunable without re-running the GPU.
package replyfold

import "github.com/sspataro57/switchboard/internal/connector/slackweb"

// The three values of a verdict's thread_scope: WHICH claim a later outbound
// message on the verdict's thread can support. The strings are STORED on every
// inquiry verdict and must not change.
//
//   - thread: the key names one thread (a gmail thread, a jira issue, a slack
//     message with a thread root). A later outbound there is a reply in it.
//   - conversation: an unthreaded slack channel or DM. There is no thread to
//     reply INTO, and a later outbound only means Salvador has spoken in the
//     conversation since.
//   - none: the message has no thread at all.
const (
	ScopeThread       = "thread"
	ScopeConversation = "conversation"
	ScopeNone         = "none"
)

// The three states of a FLAGGED inquiry verdict (SWT-33 criterion 17), one
// spelling for every renderer. Never collapsed: `answered in thread` is a later
// outbound on a thread-EXACT key; `spoke in conversation since` is a later
// outbound anywhere in an unthreaded channel or DM, which in a busy channel
// says little about THIS question.
const (
	StateOpen             = "open"
	StateAnsweredInThread = "answered in thread"
	StateSpokeSince       = "spoke in conversation since"
)

// ScopeOf decides a message's thread scope from its columns. Whether a slack
// key is rooted is asked of slackweb.IsRootedThreadKey, the ONE reading of the
// shape its normalizer builds; this package never takes the key apart.
func ScopeOf(channel string, threadID int64, threadKey string) string {
	switch {
	case threadID == 0:
		return ScopeNone
	case channel == slackweb.Channel && !slackweb.IsRootedThreadKey(threadKey):
		return ScopeConversation
	default:
		return ScopeThread
	}
}

// State places one flagged verdict in exactly one of the three states. A
// replied-since verdict of scope `none` cannot occur (a message with no thread
// has nothing to be replied in) and stays open if it does: the fold never
// upgrades a claim the data does not carry.
func State(scope string, repliedSince bool) string {
	switch {
	case repliedSince && scope == ScopeThread:
		return StateAnsweredInThread
	case repliedSince && scope == ScopeConversation:
		return StateSpokeSince
	default:
		return StateOpen
	}
}

// JoinSQL is the fold's LEFT JOINs, anchored on an ai_extractions row aliased
// `e` whose fields carry normalized_message_id and thread_id (every inquiry
// verdict does). It reads thread_id, direction and sent_at from
// normalized_messages and nothing else; what the model was shown comes from
// the stored verdict fields, never a join back.
//
// Its evidence is `direction = 'outbound'`, invariant 5's own marker.
//
// SET-BASED ON PURPOSE: the earliest and latest outbound instant per thread,
// computed once per query. The first cut was a correlated EXISTS per verdict,
// and normalized_messages has no index on thread_id: that shape measured
// 21.5 s over 1,806 verdicts on the production db (2026-09-10). min() and
// max() ignore NULL stamps, so a NULL-stamped outbound is never either end.
const JoinSQL = `
	      LEFT JOIN normalized_messages t
	             ON t.id = (e.fields->>'normalized_message_id')::bigint
	      LEFT JOIN (SELECT thread_id, max(sent_at) AS last_outbound, min(sent_at) AS first_outbound
	                   FROM normalized_messages
	                  WHERE direction = 'outbound' AND thread_id IS NOT NULL
	                  GROUP BY thread_id) lo
	             ON lo.thread_id = NULLIF(e.fields->>'thread_id', '')::bigint`

// RepliedSinceCol is true when the verdict's thread carries an outbound message
// sent STRICTLY after the classified one. Ties read OPEN (SWT-33 note 9): a
// reply stamped in the same instant as the question does not answer it, and an
// id tie-break would be wrong (a late-ingested older message gets a higher id).
// Reading "open" when unsure shows one extra line, never one fewer.
const RepliedSinceCol = `COALESCE(lo.last_outbound > t.sent_at, false)`

// PriorParticipationCol is true when the thread carries an outbound message
// sent STRICTLY before the ask: C-D3's "he posted on that thread strictly
// before the ask". It reads the SAME join (the earliest outbound per thread),
// so it costs no second scan. A same-instant post is not participation.
const PriorParticipationCol = `COALESCE(lo.first_outbound < t.sent_at, false)`
