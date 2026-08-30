package classify

import "encoding/json"

// PromptVersion stamps every ai_runs.input, triage's convention. It is the only
// thing that makes a prompt change re-sweepable later: without it, two runs that
// disagree are indistinguishable from a model that drifted.
const PromptVersion = "classify-v1"

// SchemaName is the structured-output name. The adapter forwards it; ollama's
// native API does not use it, but keeping it means the Request is complete for
// either lane.
const SchemaName = "classify_verdict"

// VerdictSchema is the output contract: four fields, nothing else.
//
// NOTE WHAT IS ABSENT: there is no `confidence` field, and that is deliberate
// rather than an oversight. Measured on the real corpus, qwen3:8b returns
// exactly 0.95 for everything it flags — 27 true positives and 17 false
// positives, identical. A confidence column built on that would look like a dial
// and be a constant, which is this repo's recurring landmine wearing a model's
// clothes. To trade precision back, use a second pass with a DIFFERENT model
// over the flagged subset (Future work); do not fake a threshold.
//
// `kind` is an ENUM, not a free string, and that was measured too: left
// unconstrained the model returns the same concept in three casings in a single
// run — "payment due", "violation_to_cure", "statement-availabl…",
// "transaction_notifi…" — producing a report column nothing can GROUP BY.
// Constrained, every response landed inside the enum.
//
// additionalProperties:false so a model that invents a field has it rejected
// rather than stored unchallenged.
var VerdictSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["actionable", "kind", "title", "reason"],
  "properties": {
    "actionable": {"type": "boolean"},
    "kind": {
      "type": "string",
      "enum": ["payment_due", "deadline", "appointment", "action_required", "informational"]
    },
    "title": {"type": "string"},
    "reason": {"type": "string"}
  }
}`)

// SystemPrompt is ONE prompt for every sender.
//
// Per-sender prompts are rules in a costume: unbounded maintenance, untestable
// in aggregate, and unattributable when they misfire. What the HOA case actually
// shows is a missing CONTEXT FACT — that this sender emits both routine
// announcements and fine notices — and a fact belongs in a column, editable and
// visible in the report, never in a branch.
//
// The near-miss clause below is the one part whose removal was MEASURED: a
// shortened prompt, identical except that it stopped naming statement-available,
// balance notices and card-was-used as informational, flipped "your statement is
// available" to actionable — on the single most common message shape in the
// corpus (883 Bank of America messages). Do not trim it for brevity.
//
// The prompt is language-neutral on purpose. 51 of the 1,609 personal messages
// are Spanish (Bank of America duplicates its alerts), and the originals are
// kept rather than translated: the model parses them, and a translation pass
// would be a second inference plus a new failure mode that silently changes an
// answer with nothing left to compare against.
const SystemPrompt = `You classify one personal (non-work) email for ACTIONABILITY.

Answer only about the message you are given. Messages may be in English or
Spanish; treat both identically.

actionable = true when the message requires the recipient to DO something, and
there is a consequence for not doing it:
  - a payment or bill that is due, or a balance that must be paid
  - a fine, a violation to cure, or a compliance deadline
  - an appointment to confirm, reschedule or attend
  - a document, form or signature that must be returned
  - anything with a stated deadline that has not passed

actionable = false for pure information, even when it sounds urgent:
  - "your statement is available", "your statement is ready"
  - balance notices, low-balance alerts, "your available balance"
  - "your card was used", transaction and deposit notifications
  - receipts and confirmations for something already done
  - marketing, offers, newsletters, community announcements, event invitations

RECALL IS THE OBJECTIVE. A missed payment or fine notice costs a late fee; a
false alarm costs one second to dismiss. When you are genuinely torn, answer
true.

Some notices defer their detail to an attachment or a portal that you cannot
read. Say so plainly in the title — for example "HOA violation notice — open the
attachment" — and never guess an amount or a date that is not in the text you
were given.

Fields:
  actionable  true or false, as above.
  kind        payment_due | deadline | appointment | action_required | informational
  title       one short line a human can act on, naming the amount and date ONLY
              if they appear in the message.
  reason      one sentence, citing the words that decided it.`
