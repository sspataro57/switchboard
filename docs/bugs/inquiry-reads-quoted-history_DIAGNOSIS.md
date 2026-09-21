# inquiry-reads-quoted-history — diagnosis (SWT-70)

## Root cause

`classify.renderInquiryUser` (internal/classify/inquiry.go) hands the model the target message's
`body_text` verbatim under "The message to decide on", and each prior message's body verbatim
(flattened, first 600 characters) in the transcript block. A reply's `body_text` carries the whole
earlier conversation under its new text. For message 385428 the new text was 6% of the target body;
the other 94% was the quoted chain, unfenced, and the inbound context line carried a second copy of
it. The model summarised the quoted PREVIOUS message ("will send the custom fields tomorrow") and
answered `needs_reply=false, ask_kind=fyi`.

Verified, not inferred: replaying the stored prompt reproduces the stored verdict 5 of 5 with a
byte-identical reason; cutting the target body at its first quote separator gives
`needs_reply=true, ask_kind=request` 5 of 5 (REPRO.md). Capture, attribution and the promoter all
behaved correctly; nothing downstream of the verdict is involved.

Not a regression: the renderer has passed bodies whole since the lane shipped (SWT-33).

## Fix

`classify.StripQuotedHistory` / `StripQuotedHistoryOf` (internal/classify/quotedhistory.go), pure:
cut at the first quoted-history separator — Outlook's dashed "Original Message" line, Gmail's
"On … wrote:" (wrapped or not), a bare From:/Sent: header block, an underscore rule, their Spanish
forms, or a block of three or more `>` lines. Two guards keep it from throwing a message away:
fewer than 20 characters of new text above the separator means an inline reply, so the body is kept
whole; and a FORWARD (a "Forwarded message" marker, or a Fwd:/FW:/RV: subject) is never cut,
because what sits under a forward's header is the payload, not history.

Applied in `renderInquiryUser` to the target and to each context message. The earlier messages
still reach the model, once each, through the transcript block. The system prompt is unchanged;
`InquiryPromptVersion` moves to `inquiry-v2` because the same message now renders a different
prompt.

Scope: the inquiry lane only. The personal and residue lanes judge mostly non-conversational mail
and have their own measured baselines; not touched.

## Measurement (2026-09-21, qwen3:8b, inquiry eval set + message 385428 as `needs_reply`)

| renderer | labelled needs_reply caught | uniform flagged (of which right) | blanket `not` flagged |
|---|---|---|---|
| old | 2 of 3 (385428 missed) | 5 (2) | 12 |
| new | 3 of 3 | 5 (2) | 12 |

Nothing else moved. 20 of the file's 62 labels are excluded by the harness for subject-hash drift;
that predates this change.

The other six quoted client replies that were judged no-reply in the last 30 days stay no-reply
under the new renderer, and by eye that is right: five are a colleague answering the client on a
Cc'd thread, one is the client's "will send it tomorrow". So 385428 is the only ask this bug hid
in that window.

## Blast radius

Only `renderInquiryUser` calls the stripper. The roll re-classifies nothing (a message with an
inquiry verdict keeps it), so message 385428 keeps its v1 verdict; its task was created by hand
(#447).

## Residuals

- A reply typed BELOW the quote with a long greeting above it would be cut to the greeting. The
  20-character floor covers the common inline shape, not that one.
  Measured in review against production (the shipped function run over the bodies): of 8,097 inbound
  gmail bodies in 60 days the stripper cuts 118 (on-wrote 56, From:/Sent: 33, quoted block 15,
  underscore rule 10, dashed 4); none is a bottom-posted reply — the 4 with prose after the last quoted
  line are signatures or disclaimers, and the 9 cut to under 60 characters are genuine one-line acks.
  No missed reply separator was found; every uncut body carrying one is a forward or under the floor.
  Slack and Jira bodies (1,302): 0 cuts. Cost: 3 ms per 40 KB body.
- A forward marker ANYWHERE in the body keeps it whole, so a reply whose quoted chain contains an old
  forward is not stripped; `[EXT] FW:` / `Re: FW:` subjects are not seen as forwards (0 in 60 days).
  Both fail safe: the body is kept whole, which is the old behaviour.
- Signatures and legal disclaimers above the separator still reach the model.
- The eval set is small (3 positives). It guards against a regression, it does not measure recall.
