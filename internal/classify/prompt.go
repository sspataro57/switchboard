package classify

import "encoding/json"

// PromptVersion stamps every ai_runs.input, triage's convention. It is the only
// thing that makes a prompt change re-sweepable later: without it, two runs that
// disagree are indistinguishable from a model that drifted.
const PromptVersion = "classify-v2"

// SchemaName is the structured-output name. The adapter forwards it; ollama's
// native API does not use it, but keeping it means the Request is complete for
// either lane.
const SchemaName = "classify_verdict"

// VerdictSchema is the output contract: five fields, nothing else.
//
// `link_index` (SWT-25) is an INDEX into normalized_messages.links — 1-based,
// integer or null — and never a URL. The application resolves it via
// ResolveLink; the model is never given a string field to author a link into,
// which is what makes "the model never authors a URL" structural rather than a
// matter of prompt discipline.
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
  "required": ["actionable", "kind", "title", "reason", "link_index"],
  "properties": {
    "actionable": {"type": "boolean"},
    "kind": {
      "type": "string",
      "enum": ["payment_due", "deadline", "appointment", "action_required", "informational"]
    },
    "title": {"type": "string"},
    "reason": {"type": "string"},
    "link_index": {"type": ["integer", "null"]}
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
// classify-v2 (SWT-68, receipts-become-tasks): the money clauses. Under v1 every
// one of 19 receipts, autopay notices and refunds came back payment_due +
// actionable — one with the reason "there is nothing the recipient needs to
// do". The enum has no receipt kind, the first `true` bullet said "a payment or
// bill that is due", and the recall tie-break did the rest. v2 says what
// payment_due MEANS (a bill the recipient still has to pay), names the money
// shapes that are false (already paid or authorized, collected automatically,
// money arriving) and takes them out of the tie-break. A
// stricter draft ("must pay BY HAND" plus a consistency rule) was MEASURED and
// rejected: it lost three real "minimum payment due" bills. Measured result of
// this wording: 35 of 37 real asks caught (the same two misses as v1), 15 of
// the 18 receipt rows no longer flagged (docs/runbooks/local-classifier.md,
// 2026-09-19). The enum is NOT widened: it is
// shared with the residue lane through ActionabilityContract, and a new kind
// would stale that lane's 874-label baseline for no gain — actionable=false
// already keeps a message out of the promoter's inbox.
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
  - a payment or bill the recipient STILL HAS TO PAY: a bill, a statement with a
    minimum payment due, a balance that must be paid — whether the due date is
    weeks away or has ALREADY PASSED (an overdue, past-due or final notice is
    always actionable). Unless the message itself says the
    amount was already paid or authorized, or will be charged automatically,
    assume the recipient must pay it
  - a fine, a violation to cure, or a compliance deadline
  - an appointment to confirm, reschedule or attend
  - a document, form or signature that must be returned
  - anything with a stated deadline that has not passed

actionable = false for pure information, even when it sounds urgent:
  - "your statement is available", "your statement is ready"
  - balance notices, low-balance alerts, "your available balance"
  - "your card was used", transaction and deposit notifications
  - receipts and confirmations for something already done
  - money that has already moved: "you paid", "you sent a payment", "payment
    received", order and subscription receipts. "You authorized a payment" and
    "payment authorized" mean the recipient ALREADY approved it at checkout and
    the purchase is complete: it is a receipt. Nothing is pending and nothing is
    left to confirm, complete or process
  - an invoice or statement copy that was already charged to a card or account
    on file, or that gives no way to pay and asks for nothing
  - money that will move by itself: autopay and automatic-charge notices, "we'll
    automatically charge", "nothing you need to do". An installment plan ("pay
    in 4", "your plan", "upcoming payment", "due today" inside a schedule) is
    charged automatically to the saved card or account on each date
  - refunds and money arriving
  - marketing, offers, newsletters, community announcements, event invitations

RECALL IS THE OBJECTIVE. A missed payment or fine notice costs a late fee; a
false alarm costs one second to dismiss. When you are genuinely torn, answer
true.

That tie-break does NOT cover money that has already moved or will move by
itself. A receipt, an automatic charge or a refund names an amount and often a
date, and is still false: nothing is left for the recipient to do. Ask "has this
money already moved, or does the message say it will move automatically?" Only
then is it false. A bill or a minimum payment with a due date is payment_due.

Be consistent: when your reason says that no action is required, actionable is
false.

Some notices defer their detail to an attachment or a portal that you cannot
read. Say so plainly in the title — for example "HOA violation notice — open the
attachment" — and never guess an amount or a date that is not in the text you
were given.

When a numbered list of links is shown after the message, it is the complete
set of links available. Answer link_index with the number of the one link a
person would open to act on this message. Answer null when none of them is that
link, or when no list is shown at all — null is a normal answer, not a failure.
Never invent a number that is not in the list.

Fields:
  actionable  true or false, as above.
  kind        payment_due | deadline | appointment | action_required | informational
              payment_due means a bill the recipient still has to pay. A receipt,
              an automatic charge or a refund is informational.
  title       one short line a human can act on, naming the amount and date ONLY
              if they appear in the message.
  reason      one sentence, citing the words that decided it.
  link_index  the number of the chosen link from the list, or null.`
