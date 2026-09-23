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

// InquiryEligibleLatestSQL is the ONE spelling of "the message's latest capture
// decision makes it eligible for the inquiry lane" (chat-on-closed-task, SWT-53
// CC5). Both inquiry inboxes, classify's inboxWhereInquiry and promote's
// inquiryInbox, splice it in, together with InquiryLiveDecisionJoinSQL and
// InquiryProjectIDSQL. It reads two per-message rows:
//
//   - `latest`: the inbox's own LATERAL, the newest decision in ANY mode, which
//     selects cd.action and cd.project_id (unchanged since before SWT-53);
//   - `live`: InquiryLiveDecisionJoinSQL, the newest LIVE decision.
//
// The branches:
//
//   - `latest.action = 'attributed'`: the existing admission, in ANY mode (C2: a
//     gate resolution, a route row or a shadow re-point is the message's current
//     attribution) — EXCEPT a decision capture recorded as a Slack channel
//     message that does not mention Salvador (SWT-79, channel_unmentioned; read
//     from the same `latest` row, so a newer row decides either way);
//   - or the latest LIVE decision is a `task_log` that capture RECORDED as
//     resurfacing (the lanes only read the fact, CC3), onto a task that is STILL
//     closed. "Still closed" is re-read here at both stages, so a task reopened
//     since takes the message back out: its log line is on the board again (T7).
//
// CC5b: the resurface branch reads only `live`, so a shadow row never ADDS a
// message through it (a shadow task_log with resurface=true over a live
// resurface=false), and a newer shadow row of any other action never REMOVES a
// live resurfaced message. The one ACCEPTED exception (owner-session decision
// 2026-09-14, the Part A re-point contract): a NEWER shadow `attributed` row
// takes precedence. The message is then admitted by the attributed branch
// under the shadow row's project, with LoggedOnTaskID = 0 and no
// logged_on_closed_task line, or dropped if that project is not armed.
// Parenthesised as one expression, because it is AND-ed into WHERE clauses.
const InquiryEligibleLatestSQL = `
	  ((latest.action = 'attributed' AND NOT latest.channel_unmentioned)
	   OR (live.action = 'task_log' AND live.resurface
	       AND EXISTS (SELECT 1 FROM tasks lt WHERE lt.id = live.task_id AND lt.status = 'closed')))`

// InquiryLiveDecisionJoinSQL is the message's newest LIVE capture decision,
// aliased `live`: the only row the resurface branch of InquiryEligibleLatestSQL
// reads (CC5b). A LEFT JOIN, because a message may have no live row at all.
// Requires the alias `nm` for normalized_messages, and must be joined BEFORE
// any ON clause that splices InquiryEligibleLatestSQL.
const InquiryLiveDecisionJoinSQL = `
	  LEFT JOIN LATERAL (SELECT lcd.action, lcd.project_id, lcd.resurface, lcd.task_id
	                       FROM capture_decisions lcd
	                      WHERE lcd.message_id = nm.id AND lcd.mode = 'live'
	                      ORDER BY lcd.id DESC LIMIT 1) live ON true`

// InquiryProjectIDSQL is the project the admission names: the latest
// decision's for an `attributed` admission (unchanged), otherwise the latest
// LIVE decision's, so a newer shadow row (an `unmatched` one has no project)
// cannot drop a resurfaced message from the project join.
const InquiryProjectIDSQL = `CASE WHEN latest.action = 'attributed' THEN latest.project_id ELSE live.project_id END`

// InquiryLoggedOnTaskSQL is promote's Verdict.LoggedOnTaskID (CC6): the closed
// task the live task_log names when the message was admitted by the resurface
// branch, and 0 for an `attributed` admission.
const InquiryLoggedOnTaskSQL = `CASE WHEN latest.action = 'attributed' THEN 0 ELSE COALESCE(live.task_id, 0) END`

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
