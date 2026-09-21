> Jira: SWT-70

# inquiry-reads-quoted-history

swb task #448. Reported by Salvador, 2026-09-21, in the switchboard session.

## The report, verbatim

> there is an email from mike and I don't se in incoming

and, after the miss was explained:

> after cc is done fix #448

## Observed (ops db, 2026-09-21)

- `normalized_messages.id = 385428`: inbound gmail, from a University of Rochester address, thread
  327208 ("RE: [EXT] Re: Questions About Collaboratory Activities Integration"), sent 2026-09-21 13:57Z,
  mailbox salvador@handsonconnect.org.
- Capture: `live | attributed | collaboratory` (rule 60). Correct.
- Inquiry lane (`ai_runs.worker_type = 'classify_inquiry'`, 14:01Z): `needs_reply=false`,
  `ask_kind="fyi"`, `ask=""`, `context_messages=2`. Its reason: the message is "confirming that they
  will send the custom fields along tomorrow. It does not ask for a reply".
- The NEW text of the message, above its `-----Original Message-----` quote, does the opposite: it
  apologises for the delay and DELIVERS the list of custom fields the sender needs (two on Course, two
  on Class). "Will send the custom fields" is what the PREVIOUS message in the quoted chain said.
- No `classify_promotions` row, no task. Nothing appeared in the board's incoming section. A task
  (#447, collaboratory) was created by hand.

## Expected

A reply whose new text delivers requested material or asks for something is judged on its NEW text.
Quoted reply history under it ("-----Original Message-----", "On … wrote:", `>` lines) must not decide
the verdict. This message should have produced an inquiry verdict that needs a reply, and a task.

## Reproduction material

Message 385428 and its stored run (`ai_runs.input->>'user_prompt'`, `ai_extractions.fields`); the two
context messages of thread 327208; the inquiry lane's prompt and eval set.
