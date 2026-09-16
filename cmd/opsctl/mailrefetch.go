package main

// `opsctl mail refetch` — the hand-run recovery for attachments the fetch-time
// cap dropped (SWT-64). Flags, pool and printing only; the pass itself lives in
// internal/connector/google/refetch.go, where the envelope and external-id
// spellings already are.
//
// The bounds below are the safety property. The pass overwrites raw_json in
// place, raw_source_items keeps no version history, and there is no undo — so
// "it defaulted to everything" must not be reachable by forgetting a flag.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

// mailRefetchTimeout bounds one hand-run pass. Generous next to the 30s tool
// deadline: a 100 MiB fetch over IMAP is slow by nature, and this is a human
// waiting at a terminal, not a CronJob.
const mailRefetchTimeout = 20 * time.Minute

type mailRefetchOpts struct {
	query    google.RefetchQuery
	maxBytes int
	dryRun   bool
}

// parseMailRefetch parses and REFUSES: every bound is checked here so the run
// path never has to decide whether it was given a sane selection.
func parseMailRefetch(argv []string) (mailRefetchOpts, error) {
	fs := flag.NewFlagSet("mail refetch", flag.ContinueOnError)
	from := fs.String("from", "", "sender substring (the finder family)")
	rawIDs := fs.String("raw-id", "", "comma-separated raw_source_items ids (the explicit family)")
	account := fs.String("account", "", "narrow to one source_accounts.account_email")
	since := fs.String("since", "", "Go duration (720h) or RFC3339: only mail at or after this")
	until := fs.String("until", "", "Go duration or RFC3339: only mail at or before this")
	limit := fs.Int("limit", 0, "REQUIRED: stop after N messages, oldest first")
	maxBytes := fs.Int("max-bytes", google.RefetchMaxMessageBytes, "fetch cap in bytes")
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing")
	if err := fs.Parse(argv); err != nil {
		return mailRefetchOpts{}, err
	}

	// Limit first, so the commonest mistake — a selector and no bound — names the
	// bound rather than something else.
	if *limit <= 0 {
		return mailRefetchOpts{}, fmt.Errorf("--limit is required and must be >= 1: this tool never runs unbounded, " +
			"because it overwrites stored mail in place with no version history")
	}
	fromSet := strings.TrimSpace(*from) != ""
	rawSet := strings.TrimSpace(*rawIDs) != ""
	if fromSet == rawSet {
		return mailRefetchOpts{}, fmt.Errorf("give exactly one selector family: --from <sender substring> " +
			"(optionally with --since/--until) or --raw-id <id[,id...]>")
	}
	if *maxBytes <= 0 {
		return mailRefetchOpts{}, fmt.Errorf("--max-bytes must be positive (got %d); a zero cap would store "+
			"headers only, which is the defect this tool exists to repair", *maxBytes)
	}

	opts := mailRefetchOpts{
		query:    google.RefetchQuery{AccountEmail: strings.TrimSpace(*account), Limit: *limit},
		maxBytes: *maxBytes,
		dryRun:   *dryRun,
	}
	if fromSet {
		opts.query.From = strings.TrimSpace(*from)
	}
	if rawSet {
		seen := map[int64]bool{}
		for _, part := range strings.Split(*rawIDs, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return mailRefetchOpts{}, fmt.Errorf("--raw-id %q is not a number", part)
			}
			// Deduped: a repeated id would otherwise make the selection return
			// fewer rows than ids and be reported as "does not exist".
			if !seen[id] {
				seen[id] = true
				opts.query.RawIDs = append(opts.query.RawIDs, id)
			}
		}
		if len(opts.query.RawIDs) == 0 {
			return mailRefetchOpts{}, fmt.Errorf("--raw-id named no ids")
		}
		// Refused here, before the pool is opened, because the alternative is a
		// LIMIT silently cutting ids the operator named and the selection then
		// reporting them as nonexistent.
		if len(opts.query.RawIDs) > opts.query.Limit {
			return mailRefetchOpts{}, fmt.Errorf("--raw-id names %d ids but --limit is %d: raise --limit, "+
				"or the ids past the limit would be cut and reported as missing", len(opts.query.RawIDs), opts.query.Limit)
		}
	}

	var err error
	if opts.query.Since, err = parseRefetchTime(*since); err != nil {
		return mailRefetchOpts{}, fmt.Errorf("--since: %w", err)
	}
	if opts.query.Until, err = parseRefetchTime(*until); err != nil {
		return mailRefetchOpts{}, fmt.Errorf("--until: %w", err)
	}
	return opts, nil
}

// parseRefetchTime accepts a Go duration (relative to now) or an RFC3339 instant.
// Empty is the zero time, meaning unbounded on that side.
func parseRefetchTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("duration %q must be positive", raw)
		}
		return time.Now().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither a Go duration (720h) nor an RFC3339 instant", raw)
	}
	return t, nil
}

func runMailRefetch(argv []string) error {
	opts, err := parseMailRefetch(argv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mailRefetchTimeout)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	targets, err := google.SelectRefetchTargets(ctx, pool, opts.query)
	if err != nil {
		return err
	}
	// Printed even at zero, and before anything else: a selection that matched
	// nothing and a pass that never ran must not look the same.
	fmt.Printf("mail refetch: selected %d target(s), limit %d, max_bytes %d, dry_run %t\n",
		len(targets), opts.query.Limit, opts.maxBytes, opts.dryRun)
	if len(targets) == 0 {
		return nil
	}
	if len(targets) == opts.query.Limit {
		fmt.Printf("mail refetch: the selection filled the limit exactly; there may be more — re-run after this batch\n")
	}

	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return fmt.Errorf("OPS_TOKEN_KEY is not set (required to decrypt the mailbox app password)")
	}
	sink := google.NewPGSink(pool)

	// Grouped by account: each mailbox is its own IMAP connection, its own lock
	// and its own sync_runs row.
	var firstErr error
	for _, batch := range groupTargetsByAccount(targets) {
		if err := refetchOneAccount(ctx, pool, sink, key, batch, opts); err != nil {
			fmt.Fprintf(os.Stderr, "mail refetch: account %s failed: %v\n", batch.email, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// accountBatch is one mailbox's targets: its own IMAP connection, its own lock
// and its own sync_runs row.
type accountBatch struct {
	accountID int64
	email     string
	targets   []google.RefetchTarget
}

// groupTargetsByAccount groups by ACCOUNT ID, not by email.
//
// source_accounts is unique only on (provider, account_email), so case-distinct
// rows are legal, and ListAppPasswordAccounts matches lower(account_email).
// Grouping by the email string and taking accounts[0] could hand a batch to the
// wrong row's id — which the pass then refuses target by target (wrong_account),
// correct but useless. The id is already on every target, so the case just works.
//
// Pure and deterministic (sorted by account id) so it is table-testable without
// a pool.
func groupTargetsByAccount(targets []google.RefetchTarget) []accountBatch {
	order := []int64{}
	byID := map[int64]*accountBatch{}
	for _, t := range targets {
		b, ok := byID[t.AccountID]
		if !ok {
			b = &accountBatch{accountID: t.AccountID, email: t.AccountEmail}
			byID[t.AccountID] = b
			order = append(order, t.AccountID)
		}
		b.targets = append(b.targets, t)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]accountBatch, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func refetchOneAccount(ctx context.Context, pool *pgxpool.Pool, sink *google.PGSink, key string,
	batch accountBatch, opts mailRefetchOpts) error {
	accounts, err := google.ListAppPasswordAccounts(ctx, pool, batch.email)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		// Named, never a silent skip: "nothing to do" and "this mailbox cannot be
		// opened" are different answers.
		return fmt.Errorf("no provider='google' app-password account for %s; this tool refetches only mailboxes it can open", batch.email)
	}
	// Matched by ID, not position: the lookup is case-insensitive while the table
	// is not, so accounts[0] can be a different mailbox that merely shares the
	// spelling. The targets carry the id they were selected under.
	// Deliberately NOT accounts[0]: taking the first row was the case-variant bug
	// this loop exists to prevent, and leaving it as an initialiser would invite
	// the next reader to trust it.
	var acct google.Account
	found := false
	for _, a := range accounts {
		if a.ID == batch.accountID {
			acct, found = a, true
			break
		}
	}
	if !found {
		return fmt.Errorf("account %d (%s) is not an app-password google account any more: it was when the "+
			"targets were selected, so re-run the selection", batch.accountID, batch.email)
	}

	// A live run takes the same per-account advisory lock the connector takes, so
	// it cannot interleave with an ingest pass writing the same rows. A dry run
	// writes nothing and takes none.
	release := func() {}
	if !opts.dryRun {
		rel, ok, err := sink.LockAccount(ctx, acct.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("a google connector pass holds account %d (%s); retry shortly", acct.ID, batch.email)
		}
		release = rel
	}
	defer release()

	password, err := google.DecryptAppPassword(ctx, pool, acct.ID, key)
	if err != nil {
		return fmt.Errorf("decrypt app password for %s: %w", batch.email, err)
	}
	src := google.NewIMAPClientSource(acct.Hosts(), acct.Email, password)
	defer func() { _ = src.Close() }()

	stats, err := google.RefetchMessages(ctx, src, sink, acct, batch.targets, google.RefetchConfig{
		MaxBytes: opts.maxBytes,
		DryRun:   opts.dryRun,
		Out:      os.Stdout,
	})
	printRefetchStats(batch.email, stats)
	return err
}

// printRefetchStats prints every counter, zeros included, then each refusal with
// its raw id — the id is what the operator needs to look a refusal up.
func printRefetchStats(email string, s google.RefetchStats) {
	fmt.Printf("mail refetch %s: {\"planned\":%d,\"imap_fetched\":%d,\"raw_inserted\":%d,\"raw_updated\":%d,"+
		"\"raw_unchanged\":%d,\"still_truncated\":%d,\"uidvalidity_changed\":%d,\"folder_not_selectable\":%d,"+
		"\"envelope_mismatch\":%d,\"gone\":%d,\"wrong_account\":%d,\"row_vanished\":%d,"+
		"\"would_downgrade\":%d,\"would_shrink\":%d}\n",
		email, s.Planned, s.IMAPFetched, s.RawInserted, s.RawUpdated, s.RawUnchanged, s.StillTruncated,
		s.UIDValidityChanged, s.FolderNotSelectable, s.EnvelopeMismatch, s.Gone, s.WrongAccount, s.RowVanished,
		s.WouldDowngrade, s.WouldShrink)
	if s.StillTruncated > 0 {
		// Said in words, not only as a counter: every other number reads as a
		// successful repair, and this one means the bytes are still not stored.
		fmt.Printf("mail refetch %s: %d message(s) came back STILL TRUNCATED — they are over this pass's cap "+
			"too; re-run with a larger --max-bytes or fetch them another way\n", email, s.StillTruncated)
	}
	for _, r := range s.Refusals {
		fmt.Printf("mail refetch %s: raw %d REFUSED: %s\n", email, r.RawID, r.Reason)
	}
}
