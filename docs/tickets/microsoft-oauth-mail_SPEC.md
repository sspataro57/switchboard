> Jira: SWT-66
>
> **DECIDED.** Both open questions are answered (Salvador, 2026-09-17) and folded
> in below: Q1 the MSN mailbox rule is HIGH priority 95 — everything arriving at
> that address is `personal`, a client writing there included; Q2 the Azure
> registration is **personal Microsoft accounts only** (`consumers`), so a work
> account cannot complete the flow at all.

# microsoft-oauth-mail — ingest sspataro57@msn.com over IMAP with Microsoft OAuth

## Source

Salvador, 2026-09-17: *"how can we add sspataro57@msn.com as source for personal?"*
After being shown the options he chose to build Microsoft sign-in properly rather
than forward the mail into a Gmail account.

The forcing fact, measured this session: `outlook.office365.com:993` advertises,
before authentication,

```
* CAPABILITY IMAP4 IMAP4rev1 AUTH=XOAUTH2 LOGINDISABLED SASL-IR UIDPLUS MOVE ID UNSELECT CHILDREN IDLE NAMESPACE LITERAL+
```

`LOGINDISABLED`, with `AUTH=XOAUTH2` as the only mechanism. An onboarding attempt
through `google-auth add-app-password` failed with `Login is disabled in current
state` and stored nothing. No Outlook.com setting re-opens basic auth; the app
password he generated is irrelevant for IMAP. So the mailbox is reachable only by
teaching the existing IMAP client one new SASL mechanism and one new credential
kind.

## Goal

Give `source_accounts` a second credential kind — an OAuth refresh token used to
mint XOAUTH2 bearer tokens for IMAP — so the existing, already-generic IMAP
connector can ingest `sspataro57@msn.com` raw-first into the one funnel, with the
three live Gmail app-password mailboxes byte-identical.

**Usable alone means:** after this merges, Salvador does the Azure app
registration once, runs `google-auth add-microsoft sspataro57@msn.com`, and the
next `cmd/connectors/google` pass fills `raw_source_items` +
`normalized_messages` with that mailbox's INBOX and Sent mail. It is then visible
to `mail_search`, the dashboard funnel, classify and capture with no further code.
Attribution to `personal` is one `opsctl capture-rules add` away and is included
here as an operator step, not as new code.

## What is already true (verified in this worktree — do not re-derive)

- **The "google" connector is already a generic IMAP connector.**
  `IMAPClientSource` dials the per-account `imap_host`/`imap_port` columns
  (`imap.gmail.com` is only `DefaultIMAPHost`, imap.go:219), selects folders by
  RFC 6154 special-use flags (`SelectFolders`, imap.go:152) and threads on RFC822
  `References`/`In-Reply-To` (`threadRoot`, rfc822.go:154) — never on
  `X-GM-THRID`. The only Gmail-specific thing in the live path is the string
  `"gmail"` in names and keys.
- **The single choke point is one line.** `IMAPClientSource.connect` calls
  `conn.Login(s.Username, s.Password)` (imap.go:333). Everything else in the
  ingest, normalize, dedup, capture and delivery paths is credential-blind.
- **go-imap v1.2.1 already exposes `func (c *Client) Authenticate(auth sasl.Client)`**
  (`client/cmd_noauth.go:87`) and sends the initial response inline when the
  server advertises `SASL-IR` — Outlook does.
- **`golang.org/x/oauth2 v0.35.0` is already a direct dependency** and ships
  `deviceauth.go`: `(*Config).DeviceAuth` and `(*Config).DeviceAccessToken`.
- **`github.com/emersion/go-sasl` (pinned, indirect) has NO XOAUTH2 mechanism** —
  only `plain`, `login`, `external`, `anonymous`, `oauthbearer`. Neither the
  pinned version nor the newest one has it.
- **`source_accounts` already has every column needed**:
  `refresh_token_encrypted` (pgcrypto), `auth_type`, `imap_host/port`,
  `smtp_host/port`, `send_enabled`, `calendar_in_availability` (0014).
- **`send_enabled` is a real gate, not decoration**: `send_delivery` refuses
  `account %s is not send-enabled` at delivery.go:1228 and :1692.

## Decisions taken in this SPEC (rationale attached; flag in review if wrong)

### D1. Device code flow, not loopback

`google-auth add` uses a loopback listener (oauth.go `LoopbackFlow`). For
Microsoft we use the **device code flow** instead:

- No redirect URI to register, so the Azure app needs only "Allow public client
  flows = Yes" and nothing platform-shaped.
- No PKCE plumbing. Microsoft requires PKCE for public-client auth-code; device
  code has no code to protect.
- It works from a headless shell and over SSH — the browser step happens on
  whatever device he likes, the CLI polls. The loopback flow requires a browser on
  the same host as the binary, which is exactly the constraint that makes the
  in-cluster and second-workstation cases awkward.
- `x/oauth2` v0.35.0 implements it; we add no dependency.

The account is a **personal Microsoft account**, so the app registration is a
**public client** (no secret is issued and none is stored) and its audience must
include personal accounts. Q2 is answered: the audience is `consumers`
(personal Microsoft accounts ONLY), so a work/school account cannot complete the
device flow — the wrong-account mistake is refused at the identity provider
rather than caught afterwards by D4's claim check, which stays as the second net.

### D2. Scopes: IMAP read + offline_access + openid/email. NOT SMTP.Send

Requested scope string:

```
https://outlook.office.com/IMAP.AccessAsUser.All offline_access openid email
```

- `IMAP.AccessAsUser.All` is the minimum for IMAP read; the connector has no
  mailbox write verb by construction (imap.go's file comment plus
  `TestIMAPClientSource_UsesBodyPeekAndNoWriteVerbs`).
- `offline_access` is what makes a refresh token come back. Without it the account
  dies in an hour.
- `openid email` are reserved scopes, so they may be combined with a resource
  scope in one request, and they make the token response carry an `id_token` —
  which is how onboarding verifies the authorized identity (D4).
- **`SMTP.Send` is deliberately NOT requested.** He has not asked to send from
  this mailbox; `send_enabled` is false on insert and `send_delivery` enforces it;
  SMTP XOAUTH2 would need a second mechanism implementation
  (`net/smtp.Auth`, distinct from `sasl.Client`). Adding it later is incremental
  consent: extend the scope constant and re-run the subcommand. Until then the
  send path must refuse the account **by name** (D6), not fall through.

### D3. The row stays `provider='google'`; the new `auth_type` is `'xoauth2'`

This is the load-bearing decision and Salvador has already objected to the naming
("this is msn not gooble it's microsof"), so it is stated plainly rather than
buried.

**What `provider='microsoft'` would cost.** Every account read in the mail path
spells `provider='google'` in SQL, and three of them are correctness-critical:

| site | what breaks if the row is `microsoft` |
|---|---|
| `google/sink.go:214` `ownEmailSet` | the msn address is not in the own-address set, so **our own sent copies normalize as `inbound`** and are eligible for triage — a direct invariant-5 break |
| `google/sink.go:190` `PendingRaw` (`a.provider='google'`) | the account's raw rows are never normalized: ingest succeeds, the funnel stays empty, no error anywhere |
| `tools/delivery.go:441` | `draft_delivery` cannot resolve a from-account for a `gmail:` thread key |
| `google/mailsender.go:227` `accountSelect`, `:141` `loadMailAccount` | the account is invisible to every lister and to the send router |
| `availability/store.go:89,127,162` | it would ENTER the availability scope if `calendar_in_availability` were true, and `LoadBusy` refuses for *everyone* when any in-scope account has no fresh calendar sync (SWT-24 readiness contract) |
| `cmd/opsctl/mailrefetch.go:236` | `opsctl mail refetch` cannot open the mailbox |

A SEVENTH exists that this table missed, found in review: `sink.go:61`
`ListAccounts`, which feeds `ingest.Run` under `MAIL_SOURCE=gmail_api`. It would
hand the Microsoft refresh token to `google.TokenClient` and abort the WHOLE pass
(`ingest.go:390` returns rather than continuing per account). Latent only:
production is `MAIL_SOURCE=imap`, and gmail_api mode is already broken there
because all three google rows carry a NULL refresh token — but the MSN row is the
first `provider='google'` row ever to have a non-NULL one, so a future flip of
that env var would fail differently than it does today.

That is six production predicates, several of which mean subtly different things
("mailboxes we read", "mailboxes we may send from", "calendars in availability").
The repo's most expensive recurring defect is a half-restated shared predicate
(IK: SWT-18's constant discriminator, SWT-21's inert guard, the two upwork room
columns). Rewriting six of them to protect three live mailboxes, for a naming
gain, is the wrong trade.

**And re-keying threads would be far worse.** Thread keys are
`"gmail:" + accountEmail + ":" + threadRoot(...)` (rfc822.go:129). They are the
identity of every stored thread: `capture_rules.thread_key_prefix` /
`thread_key_contains` match on them (two live treetop rules do), `splitGmailThreadKey`
(delivery.go:708) parses them to resolve the sending mailbox, and ~16,500 stored
Google raw items sit behind them. Changing the prefix is a data migration plus a
rule rewrite plus a parser change, and buys nothing functional.

**Decision:** one row, `provider='google'`, `account_email='sspataro57@msn.com'`,
`auth_type='xoauth2'`, `imap_host='outlook.office365.com'`, `imap_port=993`,
`smtp_host='smtp-mail.outlook.com'`, `smtp_port=587`,
`calendar_in_availability=false`, `send_enabled=false`. Thread keys keep the
`gmail:` prefix. `normalized_messages.channel` stays `'gmail'`.

**The debt, stated honestly.** On prod today `provider='google'` does not mean
"Google the company": all three google rows have a NULL `refresh_token_encrypted`
and empty `scopes`, and all 16,490 Google raw items carry `source: "imap"`. The
value already means *"a mailbox switchboard reads over IMAP and sends over SMTP,
ingested by `cmd/connectors/google`"*. This ticket extends an existing misnomer
rather than creating one. The rename (`provider='mail'`, thread prefix `mail:`,
package rename) is a real piece of work with its own migration + predicate sweep +
rule rewrite; it goes in Future work, not here.

**Reversibility:** nothing this ticket writes encodes "google" beyond what already
existed. A later rename is an `UPDATE source_accounts SET provider=...` plus the
same six-predicate sweep, whether or not this row exists.

**Mandatory belt-and-braces (a criterion, not a suggestion):**
`calendar_in_availability` MUST be false on this row. `UpsertAppPasswordAccount`
defaults it to true, and a `provider='google'` row with that flag and no calendar
sync makes `propose_slots` refuse for every account, forever (SWT-24 readiness:
freshness of the SYNC, and an empty SCOPE refuses).

### D4. Identity is verified before anything is stored, twice

`add-app-password` verifies by connecting; `add` verifies by `users.getProfile`.
Both exist because picking the wrong account in a browser is the realistic
mistake, and a mislabelled row poisons the own-address set and every thread key.

For Microsoft:

1. **Claim check** — decode the `id_token`'s payload segment (base64url, middle
   JWT segment) and require `preferred_username` or `email` to equal the argument,
   case-insensitively. If neither claim is present, refuse and print which claims
   were found. The decode does NOT verify the signature; that is acceptable only
   because the token came straight from the token endpoint over TLS in this
   process, and the code comment must say exactly that and nothing stronger (IK:
   "a comment can be a defect").
2. **Live check** — open IMAP with the new access token, `LIST`, and require
   `SelectFolders` to return a non-empty set, exactly as `add-app-password` does
   (apppassword.go:90-101).

Only then is the refresh token encrypted and stored.

### D5. XOAUTH2 is implemented in-repo, not by bumping go-sasl

Neither the pinned nor the current go-sasl has the mechanism, so a bump buys
nothing. The mechanism is a single initial client response and about 30 lines
against `sasl.Client` (`Start() (mech string, ir []byte, err error)` /
`Next(challenge []byte)`). go-sasl moves from indirect to a direct `require` in
`go.mod` — no new module, no version change.

### D6. The send router refuses the new auth_type by name

`MailSender.Send` (mailsender.go:104) switches `case AuthTypeAppPassword: SMTP;
default: OAuth`. An `xoauth2` row would fall into `default` and hand a **Microsoft
refresh token to the Gmail-API sender**. That is a fail-loud error rather than a
wrong send, but it is a misroute with a confusing message, and the shape of the
switch is exactly the "new value lands in default" trap. The switch gains an
explicit `case AuthTypeXOAuth2` returning `SendRejectedError` naming the account
and the reason ("no SMTP XOAUTH2 transport is wired"), and the `default` branch
refuses unknown values instead of assuming OAuth.

### D7. One credential seam, one account-listing predicate

Today three callers independently do `ListAppPasswordAccounts` →
`DecryptAppPassword` → `NewIMAPClientSource`: `runIMAPIngest`
(cmd/connectors/google/mailsource.go:73-114), `idleOnce` (watch.go:222-226) and
`opsctl mail refetch` (mailrefetch.go:229). Three copies × two credential kinds is
six branches and a guaranteed divergence.

- `ListAppPasswordAccounts` is **renamed** `ListIMAPAccounts` with the predicate
  `auth_type = ANY('{app_password,xoauth2}')`. One spelling. (Renaming rather than
  adding a sibling is deliberate: two functions meaning "the IMAP account set"
  is how one caller silently keeps ingesting three mailboxes while the other does
  four.)
- A new exported seam `google.OpenIMAPSource(ctx, pool, acct, tokenKey) (*IMAPClientSource, error)`
  resolves the credential by `acct.AuthType` and returns a ready source. All three
  callers use it. It is the only place that branches on credential kind.

### D8. Token refresh happens per credential resolution and is never cached across passes

`OpenIMAPSource` mints a fresh access token every time it is called — i.e. once per
one-shot pass per account (≤10 min), once per IDLE cycle (≤25 min,
`defaultIdleRefresh`), once per refetch batch. Microsoft access tokens live ~1 h,
so no in-pass refresh is needed and none is implemented. Access tokens are NEVER
persisted (SPEC 07 criterion 3's rule, unchanged).

**Refresh-token rotation MUST be persisted.** Microsoft rotates the refresh token
on redemption for personal accounts; dropping the new one kills the account when
the old one ages out. The write is a **single-column** update
(`SaveRefreshToken(ctx, pool, accountID, token, key)` writing only
`refresh_token_encrypted`), not `UpsertGoogleAccount`, which also writes `scopes`
and `calendar_in_availability` and would resurrect stale values — the same
whole-blob-clobber lesson as `SaveCursorField` vs `SaveCursor` (SWT-24).

### D9. A credential failure is a LOUD, per-account `sync_runs` row

Today a credential failure in `runIMAPIngest` (`DecryptAppPassword` error) does
`continue` with no `sync_runs` row at all: if another account succeeds the pass
exits 0 and the failure exists only in stdout. For a token that has been revoked —
the expected long-run failure of this ticket — that is a mailbox that silently
stops ingesting.

`OpenIMAPSource` failures must therefore write one `sync_runs` row per account,
`phase='imap'`, `status='error'`, message naming the account and the cause class
(`refresh token rejected (invalid_grant) — re-run google-auth add-microsoft`,
`MS_OAUTH_CLIENT_ID is not set`, `no credential stored`). Precedent to copy:
`watchAccount`'s `imap_idle` error run (watch.go:200-202). This is a deliberate
behaviour change for the existing app-password accounts too — from silence to a
recorded error — and it is the right direction.

Error text may never contain the refresh token, the access token or the pgcrypto
key.

### D10. Config surface

- `MS_OAUTH_CLIENT_ID` — the Azure Application (client) ID. Not a secret
  (public client), but required by every binary that resolves an xoauth2
  credential: `google-auth`, `cmd/connectors/google` (one-shot AND watch),
  `opsctl mail refetch`. Absent ⇒ D9's loud per-account error, never a skip.
- `MS_OAUTH_AUTHORITY` — optional override of the authority base
  (`https://login.microsoftonline.com/{audience}`); the default is the audience
  chosen in Q2, i.e.
  `https://login.microsoftonline.com/consumers`. Deployment property, never a `source_accounts`
  column — the `CAL_SOURCE` / `MAIL_SOURCE` precedent.
- No client secret anywhere, no secret file.
- `OPS_TOKEN_KEY` is already required by all three binaries.

### D11. Backfill and folders take the existing defaults

`--backfill` default 90 d, `MAIL_FOLDERS` unset ⇒ `SelectFolders` picks INBOX plus
the `\Sent` special-use folder. Outlook advertises `\Sent`, so no override is
needed; if it turns out not to, `MAIL_FOLDERS` is the documented escape hatch and
the fallback constant `gmailSentFolder` must NOT grow an Outlook sibling in this
ticket.

Ingesting Sent is not optional: it is the own-message loop-closure surface
(invariant 5) and the reason the direction rule works for mail he sends from the
Outlook app.

### D12. The usage-string bug found on the way is fixed here

`cmd/google-auth/apppassword.go:66` prints
`usage: google-auth add-app-password <email> [--imap-host H] ...`, but Go's `flag`
package stops parsing at the first non-flag argument, so the documented order
silently produces a usage error. Salvador hit this. The message is corrected to put
the flags first, and the same shape is used for the new subcommand. Cheap, in the
same file family, and it is the sentence that misled him.

## Acceptance criteria

1. `go build ./...`, `go vet ./...` and `go test ./...` pass offline: the XOAUTH2
   mechanism, the device-flow client and the id_token claim reader are unit-tested
   with zero network and zero Postgres (httptest fakes and an in-process TLS
   listener only).
2. `google.XOAuth2Client(username, accessToken)` implements `sasl.Client`. `Start`
   returns mechanism `XOAUTH2` and an initial response that is the base64 of the
   byte sequence: `user=` + username, one 0x01 byte, `auth=Bearer ` + token, two
   0x01 bytes. A unit test pins the exact base64 for a fixed username/token pair.
3. On an auth failure the mechanism's `Next` returns an EMPTY response (not an
   error), so the exchange completes and the server's tagged `NO` arrives, and it
   records the server's base64 JSON challenge so `connect` can include its
   `status`/`scope` in the returned error. A failure whose message is only
   "AUTHENTICATE failed" does not satisfy this criterion.
4. `IMAPClientSource` authenticates by credential kind: with a password it calls
   `Login` exactly as today; with a token provider it calls `Authenticate`. Proven
   against an in-process TLS IMAP server (sibling harness: `smtp_test.go`'s
   `fakeSMTP`): a server advertising `LOGINDISABLED AUTH=XOAUTH2 SASL-IR` accepts
   the token path and rejects the password path, and a server advertising neither
   accepts the password path — and the recorded wire bytes show `AUTHENTICATE
   XOAUTH2 <ir>` with the criterion-2 initial response inline.
5. `google-auth add-microsoft <email>` runs the device flow (no --no-availability flag: calendar_in_availability is forced false for this row, and a flag that cannot change anything is a lie):
   prints the verification URL and user code, polls honouring
   `authorization_pending` and `slow_down`, and on success verifies identity by
   D4's two checks, then upserts ONE `source_accounts` row with
   `provider='google'`, `auth_type='xoauth2'`, the encrypted refresh token, the
   scope list, `send_enabled=false`, `calendar_in_availability=false`, and the
   Outlook IMAP/SMTP endpoints. A claim mismatch, a missing claim or a failed IMAP
   check stores NOTHING. Tested end-to-end against a fake device/token endpoint
   plus the fake IMAP server; the pgcrypto round trip is an integration test.
6. No secret reaches argv, an env var, a log line, an error string or
   `sync_runs.stats`: the refresh token, the access token and `OPS_TOKEN_KEY`
   appear in none of them. A unit test feeds a sentinel token through the failure
   paths and greps the rendered errors.
7. `ListIMAPAccounts` returns both `app_password` and `xoauth2` accounts and
   nothing else (an `oauth`-only row is excluded, as today). Proven by an
   INTEGRATION test whose discriminating values come from Postgres columns:
   mutating the SELECT to a literal `auth_type` makes it red (IK: "test the column,
   not the fixture"). All three call sites use it; a structure test fails on a
   second `auth_type IN (...)` spelling under `internal/` or `cmd/`.
8. `google.OpenIMAPSource` is the ONLY place that branches on credential kind: a
   structure test fails any file other than its own that names both
   `AuthTypeAppPassword` and `AuthTypeXOAuth2` in a credential decision.
9. A refresh-token rotation is persisted to `refresh_token_encrypted` alone: an
   integration test sets `scopes` and `calendar_in_availability` to sentinel
   values, forces a rotation through a fake token endpoint, and asserts both
   sentinels survive and the stored token decrypts to the new one.
10. A revoked/expired refresh token (`invalid_grant`), an unset
    `MS_OAUTH_CLIENT_ID` and a missing stored token each produce exactly ONE
    `sync_runs` row for that account with `status='error'` and a message naming the
    account and the cause, and the one-shot pass exits non-zero when no account
    succeeded. An integration test asserts the row exists (per D9 this is new
    behaviour for app-password accounts too).
11. **Invariant 5, the seam that matters:** with the msn row present,
    `ownEmailSet` contains `sspataro57@msn.com`, and a message in that mailbox's
    Sent folder whose `From` is that address normalizes `direction='outbound'` and
    is therefore invisible to the capture and triage inboxes. Integration test over
    real Postgres, using the production `provider` value with a test-scoped email.
12. **The three live Gmail mailboxes are untouched.** The existing suites
    (`imap_integration_test.go`, `loopclosure_integration_test.go`,
    `refetch_integration_test.go`, `calendarphase_integration_test.go`) stay green
    unchanged, `MAIL_SOURCE` dispatch is unchanged, `accountSelect`'s column list
    is unchanged, thread keys are unchanged, and `channel` is unchanged.
13. `MailSender.Send` refuses an `xoauth2` account with a `SendRejectedError`
    naming the account and the missing transport, and refuses an unknown
    `auth_type` instead of routing it to the OAuth sender. Unit test over the
    three known values plus one unknown.
14. Migration `0037_microsoft_oauth_mail.sql` applies cleanly twice, extends the
    `auth_type` CHECK to include `xoauth2`, and adds a CHECK that an `xoauth2` row
    carries a non-NULL `refresh_token_encrypted` (0014's `app_password_present`
    shape). The CHECK must fail CLOSED on NULL — spell it
    `auth_type IS NOT NULL AND auth_type = 'xoauth2'` inside the condition, per
    IK's "`col = 'x'` inside a CHECK passes on NULL".
15. Zero tasks, zero deliveries, zero new tables: no run of `add-microsoft`, the
    connector, or `opsctl mail refetch` changes `count(*)` of `tasks` or
    `deliveries` (scoped assertions, per the cross-suite pact).
16. `docs/runbooks/imap-mail-connector.md` gains a Microsoft section complete
    enough to execute without re-deriving: the Azure registration click path, what
    Salvador must produce (client id only), the env vars, the onboarding command,
    the rollout order (D-rollout below) and the revocation/re-consent procedure.

## Data model changes

`migrations/0037_microsoft_oauth_mail.sql` — forward-only, no new tables, no new
columns.

```sql
-- auth_type gains a third value. 'oauth' = Gmail API transport; 'app_password' =
-- IMAP/SMTP with a stored password; 'xoauth2' = IMAP with an OAuth bearer token
-- minted from the stored refresh token. The value names the CREDENTIAL+TRANSPORT
-- binding, which is why it is the SASL mechanism's name and not the vendor's.
ALTER TABLE source_accounts DROP CONSTRAINT IF EXISTS source_accounts_auth_type_check;
ALTER TABLE source_accounts
  ADD CONSTRAINT source_accounts_auth_type_check
  CHECK (auth_type IN ('oauth','app_password','xoauth2'));

-- An xoauth2 row with no refresh token is a mailbox that can never authenticate.
-- Spelled to fail closed on NULL.
ALTER TABLE source_accounts
  ADD CONSTRAINT source_accounts_xoauth2_token_present
  CHECK (auth_type IS NULL OR auth_type <> 'xoauth2'
         OR refresh_token_encrypted IS NOT NULL);
```

(Both wrapped in the `IF NOT EXISTS`-style `DO $$` blocks 0014 uses, so a re-apply
is a no-op.)

Rows written at runtime, not by migration — one `source_accounts` row per D3.
`sync_cursor` uses the existing `imap_folders` shape; no new cursor key.
`raw_source_items.external_id` keeps `imap:{folder}:{uidvalidity}:{uid}` and
`raw_json` keeps the `source:"imap"` envelope, so `google.RawMailHeaders`, the
attachment tools and the PR-review trust chain all keep working on this mailbox
exactly as on the Gmail ones.

## API / MCP tool changes

**None.** No tool is added, removed or re-schema'd.

Invariant 3 is satisfied the way SPEC 07 satisfied it: `google-auth` and the
connector are trusted spine that write `source_accounts` / `raw_source_items`
directly and are not agent-reachable; nothing new is registered on the executor,
and no `raw_sql`/`raw_api` surface appears.

The one executor-mediated action in this ticket is the routing rule, and it goes
through the existing humanOnly, off-MCP `capture_rule_add` tool via
`opsctl capture-rules add` — not an INSERT.

The only change to executor-reachable behaviour is D6: `send_delivery`'s
`GmailSender` seam gains an explicit refusal for the new `auth_type`. That is a
narrowing inside an existing handler, with no schema or policy change.

## MQTT topics

None touched. The connector's existing `pipeline.AnnounceCaptured` publish is
unchanged and fires only when a capture pass decides something.

## Files likely to touch

New:

- `migrations/0037_microsoft_oauth_mail.sql`
- `internal/connector/google/xoauth2.go` — the `sasl.Client` mechanism: pure
  initial-response builder + challenge capture. No I/O.
- `internal/connector/google/msoauth.go` — Microsoft endpoint construction from
  `MS_OAUTH_CLIENT_ID`/`MS_OAUTH_AUTHORITY`, `MicrosoftScopes`, the device flow
  (`DeviceFlow(ctx, cfg) (*oauth2.Token, error)`), the id_token claim reader, the
  per-pass token source with the rotation hook, `SaveRefreshToken`. Endpoints are
  injectable (baseURL parameter) exactly like `internal/provider/openai.go` and
  `NewGmailClient`.
- `internal/connector/google/credential.go` — `OpenIMAPSource` (D7).
- `internal/connector/google/xoauth2_test.go`, `msoauth_test.go`,
  `imapauth_test.go` (in-process TLS IMAP server), `credential_integration_test.go`.

Modified:

- `internal/connector/google/imap.go` — `IMAPClientSource` gains a token-provider
  field; `connect` branches Login vs Authenticate. **Careful:** this file is
  scanned by `TestIMAPClientSource_UsesBodyPeekAndNoWriteVerbs` for the substrings
  `expunge`, `.store(`, `.move(`, `.append(` — new code must not introduce them.
- `internal/connector/google/mailsender.go` — `AuthTypeXOAuth2` const;
  `ListAppPasswordAccounts` → `ListIMAPAccounts` + predicate; `MailSender.Send`
  switch (D6). `accountSelect` is NOT changed.
- `cmd/connectors/google/mailsource.go` — `runIMAPIngest` uses `OpenIMAPSource`
  and writes D9's error run; the "no accounts" message wording.
- `cmd/connectors/google/watch.go` — `idleOnce` uses `OpenIMAPSource`; the account
  list call renames.
- `cmd/opsctl/mailrefetch.go` — the list call renames; the refusal message at :236
  stops saying "app-password".
- `cmd/google-auth/main.go` — `add-microsoft` subcommand + usage block.
- `cmd/google-auth/apppassword.go` — usage-string fix (D12).
- `cmd/google-auth/msoauth.go` (new) — the subcommand body, reading nothing from
  argv but the email; no secret on stdin (there is none).
- `go.mod` — `github.com/emersion/go-sasl` moves from indirect to direct. No
  version change, no new module.
- `docs/runbooks/imap-mail-connector.md` — the Microsoft section (criterion 16).
- `.claude/INSTITUTIONAL_KNOWLEDGE.md` — after the manual smoke: the
  `LOGINDISABLED` fact, the provider-misnomer decision, the rollout barrier.
- `docs/runbooks/HANDOFF-kube-microsoft-oauth-mail.md` — the kube session's
  handoff (image bump + `MS_OAUTH_CLIENT_ID` on the connector CronJob and the
  watch Deployment). Manifests are NOT edited here.

## In scope

- Migration 0037; the XOAUTH2 mechanism; the device flow and token plumbing;
  `OpenIMAPSource` and the single account-listing predicate; `add-microsoft`;
  D6's send refusal; D9's loud credential failures; D12's usage fix; the runbook;
  the kube handoff doc.
- The operator steps (Azure registration, onboarding, the capture rule) documented
  and executed by Salvador — the code ships green without them.

## Out of scope (do not bundle)

- **Sending from the MSN mailbox.** No `SMTP.Send` scope, no SMTP XOAUTH2
  `net/smtp.Auth`, no `send_enabled=true`. D6 refuses it explicitly.
- **Calendar.** No Outlook calendar ingest, no availability participation. This
  row is `calendar_in_availability=false` and the calendar phase is
  credential-gated on `calendar.readonly` in `scopes`
  (`ListCalendarCredentialedAccounts`), which this consent never requests.
- **Renaming `provider='google'` / the `gmail:` thread prefix / the `google`
  package.** Future work, with its own migration and predicate sweep.
- **Microsoft Graph.** No Graph API client. IMAP is the transport.
- **Adding the mailbox to a classify or routing lane.** No
  `source_account_projects` row, no `route_after`, no `ai_inquiry` change. The
  `personal` project's existing lane settings apply as-is once mail is attributed
  there.
- **Re-pointing history.** Nothing backfills or re-decides already-stored
  messages; the capture rule's effect on history is the documented shadow
  `--all` re-pointing pass, run by hand if wanted.
- **A second Microsoft mailbox / work (Office 365) accounts.** The code path is
  generic, but nothing here onboards one, and the `consumers` audience chosen in
  Q2 means a work mailbox could not sign in without a second registration.
- **`opsctl` capture-rule changes.** The routing rule uses the existing tool.

## Invariants that apply

1. **Raw-first.** Unchanged and untouched: the write is still
   `writeBatch` → `sink.InsertRaw` in `imap_ingest.go:192`, before any normalize.
   This ticket changes only how the socket gets authenticated; no new code writes a
   normalized row, and `--normalize-only --all` still rebuilds from `raw_json`
   alone with no network and no credential (it must not even call
   `OpenIMAPSource` — assert it).
2. **One funnel.** No new tables, no new columns, one additional `source_accounts`
   row. Its mail becomes ordinary `normalized_messages` with `channel='gmail'` and
   ordinary `normalized_threads`, so every existing consumer (capture, classify,
   `mail_search`, dashboard funnel) sees it with no change.
3. **Everything through the executor.** No tool added. The connector and
   `google-auth` stay trusted spine, audited through `sync_runs` (SPEC 07's
   stance). The project attribution is created through `capture_rule_add`
   (humanOnly, off the MCP surface) — never by SQL. D6 narrows an existing
   executor-reachable handler and widens nothing.
4. **Nothing external without a delivery row.** No send path is added. Three
   independent gates keep it that way: the scope is not requested, `send_enabled`
   is false and `send_delivery` enforces it (delivery.go:1228, :1692), and
   `MailSender.Send` refuses `xoauth2` by name. The IMAP interface still has no
   write verb, and the structural grep over `imap.go` still enforces it.
5. **Own-message loop closure.** This is the invariant D3 is chosen to protect.
   Concretely: the row must be `provider='google'` so `ownEmailSet`
   (`sink.go:214`) contains `sspataro57@msn.com`; `SelectFolders` must pick the
   `\Sent` folder so his Outlook-app sends re-enter; `NormalizeRFC822`'s
   `isOwnAddress` then marks them `outbound`, which is what keeps the capture
   engine (`direction='inbound'`, rules_store.go:650) and triage from minting
   tasks from our own mail. Criterion 11 is the test.
6. **Stealth attribution.** No client-visible text is authored anywhere in this
   ticket. Nothing to enforce beyond the commit rule.
7. **Purity / testability.** The XOAUTH2 initial response and the id_token claim
   reader are pure functions. The device flow and the token source take injectable
   endpoints, so every test is offline. No LLM, no provider adapter, nothing
   imported into the orchestrator.

## Sibling patterns to copy

- **OAuth plumbing shape:** `internal/connector/google/oauth.go` —
  `LoadOAuthConfig` / `LoopbackFlow` / `DecryptRefreshToken` / `TokenClient` /
  `persistingTokenSource`. This is the *dormant* step-7 code (SPEC 07's OAuth was
  superseded by IMAP app passwords in migration 0014; all three google rows have a
  NULL refresh token). **Reusable:** the pgcrypto encrypt/decrypt idiom, the
  rotation-persisting `TokenSource` wrapper, the "verify identity before storing"
  discipline, the `oauth2.Config` shape. **Dead, do not copy:** `LoopbackFlow`
  (D1 chooses device code), the client-secret file loader (public client, no
  secret), `Scopes`/`ReadonlyScopes` (Google's), `TokenClient`'s `*http.Client`
  return (we need a bearer string for SASL, not an HTTP client), and
  `UpsertGoogleAccount` as a rotation sink (D8: single-column write).
- **Verify-then-store onboarding:** `cmd/google-auth/apppassword.go:88-101`.
  Its stdin discipline (`readAppPassword`, lines 17-52) is the rule for secrets
  even though this subcommand has no secret to read.
- **In-process TLS server test harness:** `internal/connector/google/smtp_test.go`
  `fakeSMTP` / `fakeSMTPCert` / `clientConfig()` — `IMAPClientSource.TLSConfig`
  exists for exactly this and is nil in production.
- **Offline `MailSource` fake:** `internal/connector/google/fake_imap_test.go` —
  unchanged; it fakes the interface, so it cannot see the auth branch. That is why
  criterion 4 needs a wire-level server.
- **Single-field cursor write (the clobber lesson):** `Sink.SaveCursorField`
  (ingest.go:164) — `SaveRefreshToken` is its analogue for D8.
- **Loud per-account failure row:** `watchAccount`'s `imap_idle` error run
  (watch.go:200-202).
- **HTTP client with an injectable base URL:** `internal/provider/openai.go` and
  `NewGmailClient(hc, baseURL, userID)`.
- **Integration-suite hygiene:** `internal/connector/google/imap_integration_test.go`
  (build tag, `DATABASE_URL` gate, FK-ordered rerunnable cleanup, test-scoped
  emails under the PRODUCTION provider value) and the cleanup pact in
  `internal/triage/integration_test.go:81-119` — a new `itest-msoauth-%` corpus
  must join it.

## Verification protocol

### Before commit (code half — no Azure account needed)

1. `go test ./...` — green offline.
2. Integration, in an **isolated scratch database** (IK 2026-09-12: the compose
   `ops` db is shared by every worktree and capture suites take a global advisory
   lock):

   ```bash
   psql 'postgres://ops:ops@localhost:5433/ops?sslmode=disable' -c 'CREATE DATABASE ops_msoauth'
   make migrate LOCAL_DB_URL='postgres://ops:ops@localhost:5433/ops_msoauth?sslmode=disable'
   DATABASE_URL='postgres://ops:ops@localhost:5433/ops_msoauth?sslmode=disable' go test -tags integration -p 1 ./...
   ```

   0037 applied twice, second run all-skips.
3. `go vet ./...`, and the structure tests of criteria 7 and 8.
4. Mutation checks, run and reported (the repo's standing rule that a guard
   nothing tests is decoration):
   - drop `auth_type` from `ListIMAPAccounts`' SELECT and replace it with a
     literal ⇒ criterion 7 goes red;
   - remove the `SaveRefreshToken` call ⇒ criterion 9 goes red;
   - make `MailSender.Send`'s new case fall through to `default` ⇒ criterion 13
     goes red.
5. Never point any test at `192.168.50.49`, the production broker, or the shared
   compose `ops` db.

### Operator steps (Salvador, once — the only manual part)

6. **Azure app registration** (his account; nobody else can do it). Entra ID →
   App registrations → New registration. Name `switchboard-mail`. Supported
   account types: **Personal Microsoft accounts only** (Q2). No redirect URI. Then Authentication →
   **Allow public client flows = Yes**. API permissions → delegated:
   `IMAP.AccessAsUser.All` (Office 365 Exchange Online), plus `offline_access`,
   `openid`, `email`. **What he hands switchboard: the Application (client) ID and
   nothing else** — no secret is created and none is stored.
7. `export MS_OAUTH_CLIENT_ID=<that id>` in `~/.bashrc` (same non-interactive
   grep/eval caveat as `OPS_TOKEN_KEY`).
8. Apply 0037 to prod:
   `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/tools/migrate --dir migrations`
   (twice; second all-skips). **Merging a migration is not applying it.**
9. **Rollout order (barrier, not a suggestion).** Apply 0037 → hand the kube
   session the image bump plus `MS_OAUTH_CLIENT_ID` on the connector CronJob and
   the watch Deployment → confirm with `kubectl -n ops get cronjob,deploy -o wide`
   → **only then** run the onboarding in step 10. The row is what makes the
   in-cluster passes try to resolve a Microsoft credential; onboarding first means
   every in-cluster pass logs a D9 error row for it until the deploy lands.
10. `DATABASE_URL="$OPS_DATABASE_URL" go run ./cmd/google-auth add-microsoft sspataro57@msn.com`
    — device code printed, sign in on any device, pick **sspataro57@msn.com**
    (a mismatch aborts and stores nothing). Then `google-auth list` shows
    `auth=xoauth2`, `send=false`, `availability=false`.

### Manual smoke — "usable alone"

11. One pass, scoped:
    ```bash
    DATABASE_URL="$OPS_DATABASE_URL" MAIL_SOURCE=imap \
      go run ./cmd/connectors/google --account sspataro57@msn.com --backfill 720h
    ```
    then psql:
    - latest `sync_runs` for the account: `status='ok'` with a plausible
      `raw_inserted`;
    - `SELECT count(*) FROM raw_source_items r JOIN source_accounts a ON a.id=r.source_account_id
       WHERE a.account_email='sspataro57@msn.com' AND r.raw_json->>'source'='imap'` > 0;
    - `SELECT count(*) FROM raw_source_items WHERE normalized_at IS NULL` → 0;
    - a Sent-folder message from that address has `direction='outbound'`
      (invariant 5, criterion 11 on real data);
    - `SELECT count(*) FROM tasks`, `FROM deliveries` unchanged.
12. Re-run immediately → `raw_inserted=0` (cursor advanced, no re-fetch).
13. **Prove the Gmail mailboxes are unharmed:** a full pass with no `--account`;
    the three app-password accounts each get a `status='ok'` `sync_runs` row, and
    `google-auth list` still shows them `auth=app_password`.
14. **Attribution to `personal`** — dry run FIRST, both candidate priorities, and
    save the pre-add output (after an add, `try` refuses the same triple by name):
    ```bash
    opsctl capture-rules try --project personal --type thread_key_prefix \
      --pattern 'gmail:sspataro57@msn.com:' --priority 95
    opsctl capture-rules add --project personal --type thread_key_prefix \
      --pattern 'gmail:sspataro57@msn.com:' --priority 95 \
      --note "microsoft-oauth-mail: mail arriving in the MSN mailbox"
    opsctl capture-rules list
    ```
    No `--external-system`, so the rule is **attribution only** — a project, never
    a task, in shadow or live.
15. `opsctl capture-rules report --since 168h` after the next pass: the rule has
    matched, and nothing moved out of a client project.

## Known interactions and accepted residuals

- **Cross-account Message-ID dedup can steal a message from the rule.** Migration
  0005's partial unique index is `normalized_messages (external_message_id) WHERE
  channel='gmail'` — GLOBAL across accounts. A message delivered to both a Gmail
  mailbox and the MSN one produces two raw rows and ONE normalized row, attached
  to whichever copy normalized first (`PendingRaw` orders by
  `r.external_id, r.id`, which across accounts is effectively arbitrary). So a
  `thread_key_prefix` rule on the MSN mailbox is "arrived in that mailbox **and
  won dedup**". This cuts both ways, and with Q1 answered HIGH it is the one
residual sharp edge: a mail delivered to BOTH a Gmail mailbox and the MSN one
keeps a single normalized row attached to whichever copy normalized first, so
a cross-delivered client mail is `personal` only about half the time. Run
`opsctl capture-rules try` before storing the rule to see the real corpus answer.
  Not fixed here; per-account threads and cross-account unification are already
  documented Future work from SPEC 07.
- **Capture only decides inbound messages** (`rules_store.go:650`), so nothing
  attributes his own MSN sends — by design (IK: absent-because-impossible).
- **`send_enabled=false` is enforced**, contrary to what a quick read of
  `add-app-password`'s closing line suggests it is only advisory. Recorded because
  the "defer sending" argument rests on it.
- **Google has no delivery reconciler**, so `confirmDeliveryByBodyPrefix`'s
  multi-match refusals stay silent — unchanged by this ticket, but this mailbox
  adds outbound mail to the corpus that matcher scans (it joins on
  `d.from_account_id`, and no delivery will ever name this account, so the
  practical exposure is nil).

## Future work (not this SPEC)

- Rename the transport family honestly: `provider='mail'`, thread prefix `mail:`,
  package `internal/connector/mail`, `channel='mail'`. Needs a data migration, a
  six-predicate sweep, a capture-rule rewrite and a `splitGmailThreadKey` change.
- SMTP XOAUTH2 (`SMTP.Send` scope + a `net/smtp.Auth` mechanism) if he ever wants
  to reply from the MSN address through switchboard.
- Outlook calendar (Graph or CalDAV) into the availability scope.
- A second Microsoft mailbox, and a work (Office 365) account — audience and
  admin-consent behaviour differ from a personal account.
- A google/mail delivery reconciler, so a confirmation refusal stops being silent.
- Cross-account thread unification, which would also make mailbox-scoped capture
  rules exact.
