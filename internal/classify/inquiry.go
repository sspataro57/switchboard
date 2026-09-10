package classify

// The inquiry lane (SWT-33): does this client message contain an inquiry that
// needs a reply from Salvador?
//
// A DIFFERENT QUESTION from the other two lanes, which is why it carries its
// own Contract rather than a new prompt over the actionability schema. That
// schema's `kind` enum is the vocabulary of a bill, not of a conversation, and
// scoring `actionable` against inquiry labels would make two different
// questions' recall/precision falsely comparable.
//
// THE SPLIT THIS LANE RESTS ON — agency at the leaves, determinism at the
// spine: the MODEL answers "is this an inquiry to me", seeing only PRIOR thread
// context, so a verdict stays a stable, re-scoreable property of one message.
// Whether it is STILL OPEN is a deterministic fold in Summarize at READ time
// (a later outbound message on the same thread), which keeps the rule tunable
// without re-running the GPU and loses no data when it is tuned wrong.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
)

// InquiryPromptVersion stamps every inquiry ai_runs.input, distinct from the
// other lanes' stamps: without it, two runs that disagree are indistinguishable
// from a model that drifted.
const InquiryPromptVersion = "inquiry-v1"

// InquirySchemaName is this contract's structured-output name.
const InquirySchemaName = "inquiry_verdict"

// inquiryContextMax bounds how many PRIOR thread messages the model is shown.
// Six is enough to see "Salvador already answered this two messages up" — the
// case the whole lane would otherwise get wrong — without turning a busy
// channel into a prompt nobody measured.
const inquiryContextMax = 6

// InquiryVerdictSchema is the inquiry contract's structured output. Five
// required fields, no sixth, and deliberately NO link_index: normalized_messages.links
// is written by the google normalizer only, so on a slack/jira-dominated
// project it would be null on every row — a stored constant, which is the
// landmine this repo keeps paying for.
var InquiryVerdictSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["needs_reply", "ask_kind", "asker", "ask", "reason"],
  "properties": {
    "needs_reply": {"type": "boolean"},
    "ask_kind": {"type": "string", "enum": ["question", "request", "decision", "scheduling", "fyi"]},
    "asker": {"type": "string"},
    "ask": {"type": "string"},
    "reason": {"type": "string"}
  }
}`)

// InquirySystemPrompt is ONE prompt for every sender and every channel
// (per-sender prompts are rules in a costume — the same rule the other two
// lanes follow). It is bilingual on the same terms.
//
// THE OBJECTIVE LEANS ON PRECISION, unlike the other two lanes, and the prompt
// says why in one line: a missed bill is a late fee, but a false "someone is
// waiting on you" in a client channel is a false alarm on a surface that has to
// be trusted, and this population is mostly chatter.
const InquirySystemPrompt = `You read one message from a work conversation and decide whether it contains
an inquiry that needs a REPLY FROM THE RECIPIENT.

Messages may be in English or Spanish; treat both identically. The transcript
above the message, when one is shown, is PRIOR context — messages that came
before it in the same conversation, oldest first, tagged "me:" for the
recipient's own messages and "them:" for everyone else.

needs_reply = true when someone is waiting on the recipient:
  - a direct question addressed to the recipient
  - a request for a decision, an approval, or a choice between options
  - a request for information, an estimate, a status, or work
  - a scheduling ask: a time to meet, a date to confirm, availability
  - an explicit mention of the recipient attached to any of the above

needs_reply = false, which is most of this conversation:
  - an FYI, an announcement, or a status update nobody asked the recipient about
  - an automated notification: a build, a deploy, a CI result, a ticket
    transition, a bot message
  - a question the transcript shows was ALREADY ANSWERED by the recipient
  - a question addressed to someone else in the conversation, not the recipient
  - thanks, acknowledgement, agreement, or social conversation
  - the recipient's own message

If the message asks something the recipient has already answered in the
transcript shown, that is needs_reply = false. Answering twice is not the job.

PRECISION IS THE OBJECTIVE. A false "someone is waiting on you" costs trust in
a surface the recipient is meant to rely on, and most messages here are
chatter. When you are genuinely torn, answer false.

Fields:
  needs_reply  true or false, as above.
  ask_kind     question | request | decision | scheduling | fyi
  asker        who is waiting, as named in the message; empty when nobody is.
  ask          one short line stating what they need, in the recipient's words
               where possible; empty when needs_reply is false.
  reason       one sentence, citing the words that decided it.`

// inquiryVerdict is the model's structured answer on the inquiry contract,
// mirroring InquiryVerdictSchema field for field.
type inquiryVerdict struct {
	NeedsReply bool   `json:"needs_reply"`
	AskKind    string `json:"ask_kind"`
	Asker      string `json:"asker"`
	Ask        string `json:"ask"`
	Reason     string `json:"reason"`
}

// The three values of a verdict's thread_scope (criterion 13): WHICH claim a
// later outbound message on the verdict's thread can support.
//
//   - thread: the key names one thread — a gmail thread, a jira issue, a slack
//     message with a thread root. A later outbound there is a reply in it.
//   - conversation: an unthreaded slack channel or DM. There is no thread to
//     reply INTO yet, and a later outbound only means Salvador has spoken in the
//     conversation since.
//   - none: the message has no thread at all.
const (
	ScopeThread       = "thread"
	ScopeConversation = "conversation"
	ScopeNone         = "none"
)

// threadScopeOf decides a message's scope from the columns it was loaded with.
// Whether a slack key is rooted is asked of slackweb.IsRootedThreadKey, the ONE
// reading of the shape its normalizer builds (criterion 14) — this package never
// takes the key apart itself.
func threadScopeOf(m PendingMessage) string {
	switch {
	case m.ThreadID == 0:
		return ScopeNone
	case m.Channel == slackweb.Channel && !slackweb.IsRootedThreadKey(m.ThreadKey):
		return ScopeConversation
	default:
		return ScopeThread
	}
}

// inquiryFields is what an inquiry verdict records (criterion 13): the model's
// five, the bookkeeping every lane stores (sender, subject, channel, project —
// stored HERE so the report never joins back for a second copy of what was
// classified), and the thread identity a later drafting ticket aims with,
// re-deriving nothing. context_messages is recorded because without it "no
// context existed" and "the context was not loaded" are the same row.
//
// It records the thread; it aims nothing. Building a reply target is a later
// ticket's job, through the delivery tools.
func inquiryFields(m PendingMessage, v inquiryVerdict) map[string]any {
	return map[string]any{
		"needs_reply":           v.NeedsReply,
		"ask_kind":              v.AskKind,
		"asker":                 v.Asker,
		"ask":                   v.Ask,
		"reason":                v.Reason,
		"sender":                m.Sender,
		"subject":               m.Subject,
		"channel":               m.Channel,
		"project_id":            m.ProjectID,
		"project_slug":          m.ProjectSlug,
		"normalized_message_id": m.MessageID,
		"thread_id":             m.ThreadID,
		"thread_key":            m.ThreadKey,
		"thread_scope":          threadScopeOf(m),
		"external_message_id":   m.ExternalMessageID,
		"context_messages":      len(m.ThreadContext),
	}
}

// inquiryContextBodyMax bounds each PRIOR message in the transcript. The
// target keeps renderMessage's own cap; context is there to show whether the
// question was already answered, and six full-length bodies would multiply a
// prompt size nobody measured.
const inquiryContextBodyMax = 600

// renderInquiryUser builds the inquiry prompt's user half: the PRIOR transcript
// (oldest first, each line tagged from direction — "me:" for our own messages,
// "them:" for everyone else), then the target message.
//
// No numbered-link block (criterion 6): this contract has no link_index to
// answer with, so the list would be a question the model tries to answer into
// a field that does not exist.
func renderInquiryUser(m PendingMessage) string {
	var b strings.Builder
	if len(m.ThreadContext) > 0 {
		b.WriteString("Earlier in this conversation, oldest first (prior context only):\n")
		for _, c := range m.ThreadContext {
			tag := "them:"
			if c.Direction == "outbound" {
				tag = "me:"
			}
			body := strings.Join(strings.Fields(c.BodyText), " ")
			if len(body) > inquiryContextBodyMax {
				body = strings.ToValidUTF8(body[:inquiryContextBodyMax], "") + " …"
			}
			fmt.Fprintf(&b, "%s %s\n", tag, body)
		}
		b.WriteString("\nThe message to decide on:\n")
	}
	b.WriteString(renderMessage(m))
	return b.String()
}

// ThreadMessage is one prior message of the target's thread, as the context
// loader yields it.
type ThreadMessage struct {
	MessageID int64
	SentAt    time.Time
	Direction string // 'inbound' | 'outbound'
	BodyText  string
}

// InquiryContext is the PURE selection rule the store's loader applies: up to
// inquiryContextMax messages of the same thread that came STRICTLY BEFORE the
// target, oldest → newest.
//
// Prior only, and that is the whole design (D2): a message that arrived AFTER
// the target must never enter the prompt, or the verdict stops being a stable
// property of the target and a re-run stops being a re-run. Ordering is
// (sent_at, id) so a same-second pair is still deterministic.
func InquiryContext(target PendingMessage, thread []ThreadMessage) []ThreadMessage {
	prior := make([]ThreadMessage, 0, len(thread))
	for _, m := range thread {
		if m.MessageID == target.MessageID {
			continue
		}
		if m.SentAt.After(target.SentAt) {
			continue
		}
		if m.SentAt.Equal(target.SentAt) && m.MessageID >= target.MessageID {
			continue
		}
		prior = append(prior, m)
	}
	sort.Slice(prior, func(i, j int) bool {
		if prior[i].SentAt.Equal(prior[j].SentAt) {
			return prior[i].MessageID < prior[j].MessageID
		}
		return prior[i].SentAt.Before(prior[j].SentAt)
	})
	if len(prior) > inquiryContextMax {
		// Keep the NEWEST window: the messages nearest the target are the ones
		// that can show it was already answered.
		prior = prior[len(prior)-inquiryContextMax:]
	}
	return prior
}
