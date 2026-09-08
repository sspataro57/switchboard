> Jira: SWT-29

# funnel-view — open questions

**ANSWERED 2026-09-08 (Salvador): Q1 = (a), Q2 = (a).** Both are folded into
`docs/tickets/funnel-view_SPEC.md`, which is now settled. This file stays as the
record of the argument — each answer cost something, and the next reader should
be able to see what was traded away without re-deriving it.

One question. It is here rather than decided because the request was written on
the premise that the dashboard has no ingestion page, and it does — `/sources`
(`internal/dashboard/sources.go`, `templates/sources.html`, shipped SWT-11,
linked in every nav, named in `docs/runbooks/imap-mail-connector.md` and
`HANDOFF-kube-swt11.md` as *the* ingestion check). It renders per-account
lifetime totals (raw / normalized / pending / messages / in / out / newest /
truncated), a per-channel table, and a **Last run + Status** column built from
the newest `sync_runs` row of ANY phase and ANY status, with no freshness rule.

The overlap is exactly one fact — connector health — and shipping it twice with
two different rules is the repo's recurring defect.

---

## Q1. Where does the funnel page live, and what happens to `/sources`?

**(a) New `GET /funnel`, and `/sources` gives up its run columns.** The funnel
page owns connector health (last SUCCESSFUL run per phase + the real
`AVAIL_MAX_SYNC_AGE` verdict for calendar) and the three new time series;
`sources.go` loses its `LastRunAt` / `LastRunPhase` / `LastRunStatus` /
`LastRunError` / `RunsTotal` fields and its two table columns, gaining a "runs
and freshness → /funnel" line. Two pages split by question: `/sources` = "how
much is in there, per account", `/funnel` = "is it moving, and what happened to
it". Cost: an edit to a shipped page and its template, and a reader has to know
which of two ingestion pages to open.

**(b) `/sources` grows into the funnel page.** No new route. The account table's
run columns are upgraded in place (last successful, per phase, red when stale)
and the three new sections are appended below the existing Channels table;
heading becomes "Funnel". Cost: one long page (six tables), the URL keeps a name
that no longer describes it, and the diff lands entirely inside a shipped file
rather than beside it.

Everything else in the SPEC is identical under either answer; only criteria 1,
20 and the "files likely to touch" list move.

**Answer: (a).** New `GET /funnel`; `/sources` gives up its run columns and gains
the pointer line. Two pages split by question — `/sources` answers "how much is
in there", `/funnel` answers "is it moving". Accepted cost: an edit to a shipped
page and its template, and the deletion is part of THIS ticket, not a follow-up
(leaving both spellings live for one release is how the second one survives).

---

## Q2. Does the classify section belong on the same page at all?

Sections 1, 2 and 4 are ingestion facts about connectors and rules. Section 3 is
a model's output, windowed, two lanes, with a 50-row flag list — it is the
biggest block on the page and the one whose numbers get read against
`classify report`.

**(a) One page.** The funnel is the funnel; a verdict is the last stage of it,
and the value of the page is seeing "mail arrived → rules claimed 40% → the
classifier flagged 3" in one screen. Cost: the page is long and the classify
section will dominate it visually.

**(b) `/funnel` for 1/2/4, `/classify` for 3.** Two short pages; the classify
page can later grow the eval numbers, the model/prompt version and the skipped
detail without crowding anything. Cost: the cross-stage read that motivated the
ticket needs two tabs, and `classify.Summarize` is called from two routes
instead of one (harmless, same seam).

**Answer: (a).** One page. The cross-stage read in one screen IS the ticket;
splitting it costs the thing the ticket exists for. Accepted cost: the classify
block dominates visually — mitigated only by section order (health, intake,
capture, classify last, so the long block is at the bottom), not by trimming it.

---

Answered by Salvador, 2026-09-08. Folded into the SPEC the same day.
