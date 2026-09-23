> Jira: SWT-81

# imap-idle-missed-arrivals

## Report (verbatim, Salvador, 2026-09-23)

> if the imap subscription on? eamils comming instanly?

(After seeing the measurements below:)

> go with 1 specially with handsonconnect

"1" = investigate why IMAP IDLE misses some INBOX arrivals (rather than only shortening the reconcile sweep).

## Measurements that prompted it (production, read-only, last 24h, 2026-09-23 ~14:10Z)

Delay = `normalized_messages.created_at` − the server's `raw_json.internaldate` (receipt time):

| account | folder | n | median | worst |
|---|---|---|---|---|
| sspataro@gmail.com | INBOX | 96 | 66 s | 804 s |
| salvador@handsonconnect.org | INBOX | 25 | 179 s | 582 s |
| developer@sspataro.com | INBOX | 10 | 100 s | 618 s |
| sspataro57@msn.com | INBOX | 4 | 346 s | 433 s |
| (Gmail accounts) | [Gmail]/Sent Mail | 23 | 85–362 s | 619 s |

- `connector-google-watch` (0.7.48) runs `watch: mode=live … reconcile=10m idle_refresh=25m … accounts=4`
  and logs `wake <account>` lines; some messages are captured within ~20 s of receipt (e.g. Katie's Jira
  email to handsonconnect, received 13:16:25Z, captured 13:16:47Z), others wait for the 10-minute
  reconcile (e.g. handsonconnect INBOX received 13:34:21Z, stored ~13:44Z).
- Note: `raw_source_items.ingested_at` is re-bumped on content/flag changes, so it overstates first-arrival
  delay; use `normalized_messages.created_at` (first stored) instead.
- Sent Mail is swept by reconcile only (by design); the MSN account is on the Microsoft path.

## Decision (Salvador, 2026-09-23)

> log only schedulle a review in 1 week

So: no fix and no reconcile change yet. The watcher now logs every IDLE session's life
(`watch: idle <account> open / fired after / refresh after / skipped`) and the IMAP source logs how the IDLE
command ended (`imap: idle <login> INBOX ended before we stopped it …` — the server-dropped / dead-connection
case that used to look like a healthy quiet session). Review on/after 2026-09-30: swb task #561.

**Reading the lines at the review (go-reviewer):** `imap: idle … stop returned an error` lines are expected
noise — at each refresh or fire the caller's Close (LOGOUT) races the DONE (go-imap v1 pipelines commands).
Do NOT count an `imap: idle … ended before we stopped it` line that lands within milliseconds of a
`watch: idle … refresh after` / `fired after` line for the same account; that is the same race. The real
signal is an "ended before" line with no refresh/fire beside it (a BYE or a dead connection), followed by
silence until the next `open`. go-imap restarts IDLE itself every 25 min and re-issues a clean server end
silently, so a clean end is never logged — and is not a miss.
