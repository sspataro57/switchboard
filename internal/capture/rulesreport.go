package capture

// The capture-rules shadow report (SWT-17 acceptance criterion 14), in the mould
// of triage.Report: deterministic, no LLM, no network beyond Postgres, and driven
// by ONE table — capture_decisions. It enforces nothing and creates nothing; it is
// the diff surface the go-live decision is made from.
//
// It is also the ONLY diff surface for deterministically routed messages, because
// once this engine lands triage stops extracting for them (SPEC §8). Their volume
// shows up here or nowhere.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// reportListLimit bounds every leaderboard. A report that prints 16,000 unmatched
// senders is one nobody reads.
const reportListLimit = 20

// latestDecisions is the base of every query below: ONE row per message, the
// newest decision for it inside the window.
//
// Deduping matters because shadow is re-runnable by design — the pending filter
// is "no LIVE decision row", so every shadow pass over the same corpus writes
// another row. Counting rows instead of messages would make the report's numbers
// grow with the number of times someone ran it, which is exactly the kind of
// silent inflation that makes a diff untrustworthy. `id DESC` is the same
// "latest" triage's project lookup uses (SPEC §8a); the two must not drift.
const latestDecisions = `
WITH latest AS (
  SELECT DISTINCT ON (cd.message_id) cd.*
    FROM capture_decisions cd
   WHERE ($1::timestamptz IS NULL OR cd.created_at >= $1)
   ORDER BY cd.message_id, cd.id DESC
)`

// Report renders the shadow diff for decisions made at or after `since`; a zero
// `since` means the whole history. A non-empty `domain` appends the targeted
// domain-detail investigation (SWT-23 criterion 4) — a census that printed
// 14,737 subjects by default is one nobody reads, so the detail renders ONLY
// when asked for.
//
// Returns the rendered text rather than writing it, so a caller can print it,
// mail it, or assert on it.
func Report(ctx context.Context, pool *pgxpool.Pool, since time.Time, domain string) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("capture rules report: nil database pool")
	}
	var window *time.Time
	if !since.IsZero() {
		w := since
		window = &w
	}

	var b strings.Builder
	if window == nil {
		b.WriteString("Capture rules report (all recorded decisions)\n")
	} else {
		fmt.Fprintf(&b, "Capture rules report (decisions since %s)\n", window.UTC().Format(time.RFC3339))
	}
	b.WriteString("One row per message: the latest decision for it in the window.\n\n")

	for _, section := range []func(context.Context, *pgxpool.Pool, *time.Time, *strings.Builder) error{
		reportTotals,
		reportProjects,
		reportProposedTasks,
		reportAmbiguous,
		// SWT-23 criterion 1: the DOMAIN table renders before the full-From
		// table — it is the one a reader acts on, and the sender table is the
		// detail underneath it. Both stay.
		reportUnmatchedSenderDomains,
		reportUnmatchedChannels,
		reportUnmatchedSenders,
		reportUnmatchedThreadPrefixes,
	} {
		if err := section(ctx, pool, window, &b); err != nil {
			return "", err
		}
		b.WriteString("\n")
	}
	if domain != "" {
		if err := reportUnmatchedDomainDetail(ctx, pool, window, &b, domain); err != nil {
			return "", err
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// unmatchedSenderCounts returns every unmatched sender with its message count —
// unbounded, because the domain fold happens in Go and a SQL LIMIT would drop
// the long tail the cumulative column exists to measure.
func unmatchedSenderCounts(ctx context.Context, pool *pgxpool.Pool, window *time.Time) (map[string]int, error) {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT COALESCE(m.sender,''), count(*)
	    FROM latest l JOIN normalized_messages m ON m.id = l.message_id
	   WHERE l.action = 'unmatched'
	   GROUP BY 1`, window)
	if err != nil {
		return nil, fmt.Errorf("select unmatched sender counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var sender string
		var n int
		if err := rows.Scan(&sender, &n); err != nil {
			return nil, fmt.Errorf("scan unmatched sender count: %w", err)
		}
		out[sender] += n
	}
	return out, rows.Err()
}

// reportUnmatchedSenderDomains is SWT-23 criterion 1: the residue at the
// granularity a rule is actually written at — sender DOMAIN — with share and
// CUMULATIVE share. The cumulative column is the point: "the top 20 cover 43%"
// is the one number that decides whether rules or a classifier is the cheaper
// answer, and before this section it was not printable. The domain is parsed in
// GO (senderDomain), never in SQL: split_part(sender,'@',2) on `Name <a@b.com>`
// yields `b.com>`, which matches no rule and silently splits one domain into
// two rows.
func reportUnmatchedSenderDomains(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	senders, err := unmatchedSenderCounts(ctx, pool, window)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	total := 0
	for sender, n := range senders {
		counts[senderDomain(sender)] += n
		total += n
	}

	b.WriteString("TOP UNMATCHED SENDER DOMAINS\n")
	if total == 0 {
		b.WriteString("  (none — every evaluated message matched a rule)\n")
		return nil
	}
	cumulative := 0.0
	for _, e := range topCounts(counts, 40) {
		share := float64(e.count) * 100 / float64(total)
		cumulative += share
		fmt.Fprintf(b, "  %8d  %6.2f%%  %7.2f%%  %s\n", e.count, share, cumulative, e.key)
	}
	return nil
}

// reportUnmatchedChannels is SWT-23 criterion 3: the residue by channel, and
// within it the count of senders carrying NO '@' at all. That single column is
// the work-vs-noise discriminator: a bare display name is slack or upwork,
// never gmail — google writes the raw From header, which always carries an
// address (connector/google/rfc822.go, normalize.go), while slackweb writes
// message.Author and upworkcrm the CRM's sender column, both display names.
// Measured 2026-08-31: 1,287 address-less residue messages, every one of them
// channel='upwork' — work conversations sitting unmatched.
func reportUnmatchedChannels(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT COALESCE(m.channel,'(none)'), count(*),
	         count(*) FILTER (WHERE m.sender NOT LIKE '%@%')
	    FROM latest l JOIN normalized_messages m ON m.id = l.message_id
	   WHERE l.action = 'unmatched'
	   GROUP BY 1 ORDER BY 2 DESC, 1`, window)
	if err != nil {
		return fmt.Errorf("select unmatched channels: %w", err)
	}
	defer rows.Close()

	b.WriteString("UNMATCHED BY CHANNEL\n")
	b.WriteString("  channel        messages   senders with no '@'\n")
	any := false
	for rows.Next() {
		var channel string
		var n, noAt int
		if err := rows.Scan(&channel, &n, &noAt); err != nil {
			return fmt.Errorf("scan unmatched channel: %w", err)
		}
		fmt.Fprintf(b, "  %-12s %10d %14d\n", channel, n, noAt)
		any = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate unmatched channels: %w", err)
	}
	if !any {
		b.WriteString("  (none — every evaluated message matched a rule)\n")
	}
	return nil
}

// reportUnmatchedDomainDetail is SWT-23 criterion 4: the targeted
// investigation. The top 20 FULL sender addresses with counts, and the newest
// 10 subjects — the counts say how much, the subjects say what, and the claim
// gate of criterion 6 needs both. It is the query that turned sspataro.com
// (518) from a suspicion into test@sspataro.com session notifications, and
// upwork.com (106) into "Invitation to Interview".
func reportUnmatchedDomainDetail(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder, domain string) error {
	senders, err := unmatchedSenderCounts(ctx, pool, window)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for sender, n := range senders {
		if senderDomain(sender) == domain {
			counts[sender] += n
		}
	}

	fmt.Fprintf(b, "DOMAIN DETAIL %s\n", domain)
	if len(counts) == 0 {
		b.WriteString("  (no unmatched messages for this domain in the window)\n")
		return nil
	}
	b.WriteString("  senders:\n")
	for _, e := range topCounts(counts, reportListLimit) {
		fmt.Fprintf(b, "  %8d  %s\n", e.count, e.key)
	}

	// Newest subjects, streamed newest-first and cut off after ten matches —
	// the domain cannot be expressed in SQL without re-spelling the Go parse.
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT COALESCE(m.sender,''), COALESCE(m.subject,'')
	    FROM latest l JOIN normalized_messages m ON m.id = l.message_id
	   WHERE l.action = 'unmatched'
	   ORDER BY m.sent_at DESC, m.id DESC`, window)
	if err != nil {
		return fmt.Errorf("select unmatched subjects: %w", err)
	}
	defer rows.Close()

	b.WriteString("  newest subjects:\n")
	shown := 0
	for rows.Next() && shown < 10 {
		var sender, subject string
		if err := rows.Scan(&sender, &subject); err != nil {
			return fmt.Errorf("scan unmatched subject: %w", err)
		}
		if senderDomain(sender) != domain {
			continue
		}
		fmt.Fprintf(b, "    %s\n", subject)
		shown++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate unmatched subjects: %w", err)
	}
	if shown == 0 {
		b.WriteString("    (none)\n")
	}
	return nil
}

// reportTotals is the "did this run at all" line: a silent pass and a pass that
// never ran must not look the same.
func reportTotals(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT mode, action, count(*) FROM latest GROUP BY 1,2 ORDER BY 1,2`, window)
	if err != nil {
		return fmt.Errorf("select capture decision totals: %w", err)
	}
	defer rows.Close()

	b.WriteString("DECISIONS\n")
	total := 0
	any := false
	for rows.Next() {
		var mode, action string
		var n int
		if err := rows.Scan(&mode, &action, &n); err != nil {
			return fmt.Errorf("scan capture decision total: %w", err)
		}
		fmt.Fprintf(b, "  %-8s %-12s %6d\n", mode, action, n)
		total += n
		any = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate capture decision totals: %w", err)
	}
	if !any {
		b.WriteString("  (none — no message has been evaluated in this window)\n")
		return nil
	}
	fmt.Fprintf(b, "  %-8s %-12s %6d\n", "", "TOTAL", total)
	return reportUnapplied(ctx, pool, window, b)
}

// reportUnapplied surfaces the one failure this engine cannot retry: a LIVE
// decision that recorded action='task' and has no task.
//
// The live claim is one decision per message FOREVER, so a run that died between
// the claim and the create_task call leaves a message the pass will never look at
// again. Nothing else in the system notices — there is no task, no external ref,
// no event — so this line is the only place it surfaces. Zero is the normal
// reading and is therefore printed only when it is not zero.
func reportUnapplied(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	var n int
	if err := pool.QueryRow(ctx, latestDecisions+`
	  SELECT count(*) FROM latest
	   WHERE mode = 'live' AND action = 'task' AND task_id IS NULL`, window).Scan(&n); err != nil {
		return fmt.Errorf("count unapplied capture decisions: %w", err)
	}
	if n > 0 {
		fmt.Fprintf(b, "  WARNING: %d live decision(s) recorded action='task' with no task — the run died "+
			"between claiming the message and creating it, and the live claim is permanent.\n", n)
	}
	return nil
}

// reportProjects is criterion 14's per-project matched counts, broken down by
// what the decision was — attribution, a proposed task, or an append to one.
func reportProjects(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT p.slug, count(*),
	         count(*) FILTER (WHERE l.action = 'attributed'),
	         count(*) FILTER (WHERE l.action = 'task'),
	         count(*) FILTER (WHERE l.action = 'task_log')
	    FROM latest l JOIN projects p ON p.id = l.project_id
	   GROUP BY 1 ORDER BY 2 DESC, 1`, window)
	if err != nil {
		return fmt.Errorf("select capture decisions by project: %w", err)
	}
	defer rows.Close()

	fmt.Fprintf(b, "BY PROJECT\n  %-28s %8s %12s %8s %10s\n", "project", "messages", "attributed", "task", "task_log")
	any := false
	for rows.Next() {
		var slug string
		var total, attributed, task, taskLog int
		if err := rows.Scan(&slug, &total, &attributed, &task, &taskLog); err != nil {
			return fmt.Errorf("scan capture decisions by project: %w", err)
		}
		fmt.Fprintf(b, "  %-28s %8d %12d %8d %10d\n", slug, total, attributed, task, taskLog)
		any = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate capture decisions by project: %w", err)
	}
	if !any {
		b.WriteString("  (none — no message was attributed to a project)\n")
	}
	return nil
}

// reportProposedTasks is the number criterion 14 exists for: DISTINCT
// (external_system, external_key) against the message count, so 15 messages over
// 5 tickets reads as "5 tasks, 15 messages".
//
// Shadow proposes one task per MESSAGE (there are no external_refs rows to dedup
// against), so without this collapse the report would forecast 15 tasks for work
// that will produce 5.
func reportProposedTasks(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT p.slug, l.external_system,
	         count(DISTINCT l.external_key), count(*)
	    FROM latest l JOIN projects p ON p.id = l.project_id
	   WHERE l.action IN ('task','task_log')
	     AND l.external_key IS NOT NULL AND l.external_key <> ''
	   GROUP BY 1,2 ORDER BY 3 DESC, 1, 2`, window)
	if err != nil {
		return fmt.Errorf("select proposed capture tasks: %w", err)
	}
	defer rows.Close()

	fmt.Fprintf(b, "PROPOSED TASKS (distinct external keys)\n  %-28s %-12s %8s %10s\n",
		"project", "system", "tasks", "messages")
	any := false
	for rows.Next() {
		var slug, system string
		var keys, messages int
		if err := rows.Scan(&slug, &system, &keys, &messages); err != nil {
			return fmt.Errorf("scan proposed capture tasks: %w", err)
		}
		fmt.Fprintf(b, "  %-28s %-12s %8d %10d\n", slug, system, keys, messages)
		any = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate proposed capture tasks: %w", err)
	}
	if !any {
		b.WriteString("  (none — every matched rule was attribution-only)\n")
	}
	return nil
}

// reportAmbiguous lists the routing collisions: two matched rules naming DIFFERENT
// projects. The winner is recorded and honoured; this list is the only place the
// loser is visible, and the SPEC's own sanity check is that it should contain LHH
// mentions inside Collaboratory threads and little else.
func reportAmbiguous(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT l.message_id, p.slug, l.matched_rule_ids,
	         COALESCE(l.external_key,''), COALESCE(nt.thread_key,''), COALESCE(m.sender,'')
	    FROM latest l
	    JOIN projects p ON p.id = l.project_id
	    JOIN normalized_messages m ON m.id = l.message_id
	    LEFT JOIN normalized_threads nt ON nt.id = m.thread_id
	   WHERE l.ambiguous
	   ORDER BY l.id DESC LIMIT $2`, window, reportListLimit)
	if err != nil {
		return fmt.Errorf("select ambiguous capture decisions: %w", err)
	}
	defer rows.Close()

	b.WriteString("AMBIGUOUS (two matched rules, different projects — the winner is shown)\n")
	any := false
	for rows.Next() {
		var messageID int64
		var slug, key, threadKey, sender string
		var ruleIDs []int64
		if err := rows.Scan(&messageID, &slug, &ruleIDs, &key, &threadKey, &sender); err != nil {
			return fmt.Errorf("scan ambiguous capture decision: %w", err)
		}
		fmt.Fprintf(b, "  message %-8d -> %-24s rules %v  %s  %s  %s\n",
			messageID, slug, ruleIDs, ruleOrNone(key), ruleOrNone(threadKey), ruleOrNone(sender))
		any = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate ambiguous capture decisions: %w", err)
	}
	if !any {
		b.WriteString("  (none)\n")
	}
	return nil
}

// reportUnmatchedSenders is half of "what rule should exist next". `unmatched` is
// also triage's inbox (SPEC §8b), so this list is literally the model's workload.
func reportUnmatchedSenders(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT COALESCE(NULLIF(m.sender,''),'(none)'), count(*)
	    FROM latest l JOIN normalized_messages m ON m.id = l.message_id
	   WHERE l.action = 'unmatched'
	   GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $2`, window, reportListLimit)
	if err != nil {
		return fmt.Errorf("select unmatched senders: %w", err)
	}
	defer rows.Close()

	b.WriteString("TOP UNMATCHED SENDERS\n")
	any := false
	for rows.Next() {
		var sender string
		var n int
		if err := rows.Scan(&sender, &n); err != nil {
			return fmt.Errorf("scan unmatched sender: %w", err)
		}
		fmt.Fprintf(b, "  %8d  %s\n", n, sender)
		any = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate unmatched senders: %w", err)
	}
	if !any {
		b.WriteString("  (none — every evaluated message matched a rule)\n")
	}
	return nil
}

// reportUnmatchedThreadPrefixes is the other half.
//
// The prefix is folded in GO, from whole thread keys the query returns unchanged.
// SQL that took the key apart — a split_part or a || — would be a SECOND spelling
// of a key format Go already owns, which is the landmine this repo has paid for
// four times and which a structural test now enforces for upwork keys. A report is
// no exception: the day a connector changes its key shape, a report grouping
// silently into "(none)" is exactly as misleading as a matcher that stops matching.
func reportUnmatchedThreadPrefixes(ctx context.Context, pool *pgxpool.Pool, window *time.Time, b *strings.Builder) error {
	rows, err := pool.Query(ctx, latestDecisions+`
	  SELECT COALESCE(nt.thread_key,''), count(*)
	    FROM latest l
	    JOIN normalized_messages m ON m.id = l.message_id
	    LEFT JOIN normalized_threads nt ON nt.id = m.thread_id
	   WHERE l.action = 'unmatched'
	   GROUP BY 1`, window)
	if err != nil {
		return fmt.Errorf("select unmatched thread keys: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var threadKey string
		var n int
		if err := rows.Scan(&threadKey, &n); err != nil {
			return fmt.Errorf("scan unmatched thread key: %w", err)
		}
		counts[threadKeyPrefix(threadKey)] += n
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate unmatched thread keys: %w", err)
	}

	b.WriteString("TOP UNMATCHED THREAD-KEY PREFIXES\n")
	if len(counts) == 0 {
		b.WriteString("  (none — every evaluated message matched a rule)\n")
		return nil
	}
	for _, e := range topCounts(counts, reportListLimit) {
		fmt.Fprintf(b, "  %8d  %s\n", e.count, e.key)
	}
	return nil
}

// threadKeyPrefix keeps the first two colon-separated segments — provider and
// account/site/workspace — which is the granularity a new rule is written at
// (`thread_key_prefix jira:treetopllc.jira.com:WEB-` narrows further by hand).
func threadKeyPrefix(threadKey string) string {
	if strings.TrimSpace(threadKey) == "" {
		return "(no thread)"
	}
	parts := strings.SplitN(threadKey, ":", 3)
	if len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + ":" + parts[1]
}

type countedKey struct {
	key   string
	count int
}

// topCounts sorts by volume, then by key so two runs over the same data print the
// same report — a diff you have to re-sort by eye is one nobody diffs.
func topCounts(counts map[string]int, limit int) []countedKey {
	out := make([]countedKey, 0, len(counts))
	for k, n := range counts {
		out = append(out, countedKey{key: k, count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].key < out[j].key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
