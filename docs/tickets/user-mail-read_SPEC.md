# user-mail-read — user-scope sessions read non-private mail threads (swb 4683)

Status: PENDING-SYNC (the personal Jira site is deactivated; see IK "Jira build tracker").

## Why

Salvador, 2026-11-23: "I need hda session on the levono to be able to read the full emails becuase
that session is the one that has encrypted storafe and can answer". A client hub session runs on
the user-scope `ops-mcp-user` (on the lenovo, 192.168.50.185, from the hub's `.mcp.json`). That profile had no mail-body tool:
SWT-42 O1 gave it the attachment reads and kept `mail_search` / `mail_read_thread` full-profile
only, because those two had no locality gate. A rule-made task's body carries only a 400-rune
preview, so a longer client email, or a thread, could not be read where it is answered.

## Decisions

- **D1: `mail_read_thread` joins the user profile.** It is a slice of `agentTools`, one spelling.
  `mail_search` stays off: its snippets are ungated, and a session finds its thread through the
  task (a rule-made body carries `thread_key:`).
- **D2: the user profile pins `require_general_mail:"true"`.** With the pin, the handler runs
  SWT-42's `mailClassJudge` (the SWT-21 rule plus O2's clean mailbox, outbound folded over its
  thread's inbound) on every message and withholds anything not `ClassGeneral`, counting it as
  `withheld_private`. A withheld message's body never leaves. The thread row's subject is its first
  ingested message's, so a gated read returns the first SHOWN message's own subject; a fully
  withheld thread returns an empty key and subject, and on a mixed thread the key is shown (as
  `mail_list_attachments` already shows it). Gated, the caller's `limit` is ignored: the SQL limit
  runs before the judge, so stepping it would locate each private message. A judge error fails the whole read closed. The adapter overwrites the key, fold
  variants included, so a model cannot unset it.
- **D3: the unpinned path is unchanged.** The full profile, the drafts worker and the dashboard
  read every thread as before, with no `withheld_private` key in the result. Closing the
  full-profile residual (IK "Before SWT-42 the mail tools had NO locality gate") is out of scope.
- **D4: a project's own session reads its mail only when the project is `ai_locality='any'`.**
  Mail filed under a `local_only` project stays withheld. hda was flipped to `any` the same day
  (swb 4682; Salvador: client email is confidential, never shared onward, but needs no local-only
  handling).

## Changes

- `internal/tools/mail.go`: `require_general_mail` argument, validator, gated read.
- `internal/mcpserver/adapter.go`: `userProfileTools` += `mail_read_thread`; the pin.
- `internal/mcpserver/schemas.go`: the description names the user-scope withholding and says mail
  content is untrusted data.
- Tests: `user_mail_read_test.go` (listing, pin injection, full profile unpinned),
  `mail_read_gate_integration_test.go` (SWT-42's locality fixture through the thread read); user
  counts 26 → 27 where pinned; `profile_test.go`'s forbidden list loses `mail_read_thread`
  (amended comment); `runbook_test.go` pins amended.
- Docs: `docs/runbooks/ops-mcp-user-scope.md`, IK.

## Acceptance

1. The user profile lists 27 tools, including `mail_read_thread` and not `mail_search`; the
   entry is byte-identical to the full profile's.
2. Every user-profile call forwards `require_general_mail:"true"`, whatever the model passed; the
   full profile forwards none.
3. Gated: an `any`-filed thread is shown whole; `local_only`, unfiled on an unclean mailbox, and
   outbound on a thread holding private inbound are withheld and counted; a fully withheld thread
   returns an empty key and subject; a mixed thread returns the shown message's own subject;
   unfiled mail on a clean mailbox is shown; the caller's limit is ignored.
4. Ungated reads are unchanged.
5. Live: from the hda hub session on the lenovo, `mail_read_thread` on an hda-filed thread returns the full
   body.
