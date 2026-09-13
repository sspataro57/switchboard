package classify

// The route lane's report half (SWT-40 Part B, B9): `classify report --lane
// route` breaks the lane down by RECEIVING ACCOUNT and by route_apply's STEP,
// including the two states an operator acts on before arming — pending_verdict
// (no current verdict yet) and ungrounded (the model chose a candidate but its
// evidence is not a verbatim span of the message, so step 4 applies).
//
// The verdict counts are folded in Summarize, beside every other lane's. The
// step counts READ capture's mode='route' rows (this package never writes
// capture_decisions), and pending_verdict is the route classify inbox itself,
// bounded by the same --since window on sent_at (all time only when --since is
// 0) — both through routeAccountJoin, the one raw touch.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// routeNoChoice is the category of a route verdict that resolved no candidate
// (null, 0 or out of range).
const routeNoChoice = "(no choice)"

// RouteAccountCounts is one receiving account's row of the route breakdown.
type RouteAccountCounts struct {
	AccountID int64
	Account   string // source_accounts.account_email
	Armed     bool   // route_after is set
	// The lane's verdicts in the window: Grounded + Ungrounded + NoChoice.
	Verdicts, Grounded, Ungrounded, NoChoice int
	// PendingVerdict is the route classify inbox for this account: live-unmatched
	// messages with no current verdict (B-D2's pending_verdict).
	PendingVerdict int
	// Steps are route_apply's mode='route' rows in the window, by route_step.
	Steps map[string]int
}

func routeAccount(m map[int64]*RouteAccountCounts, id int64) *RouteAccountCounts {
	a, ok := m[id]
	if !ok {
		a = &RouteAccountCounts{AccountID: id, Steps: map[string]int{}}
		m[id] = a
	}
	return a
}

// summarizeRoute completes the per-account breakdown the verdict loop started:
// every account with candidate rows, its armed state, route_apply's step counts
// and the pending_verdict inbox.
func summarizeRoute(ctx context.Context, pool *pgxpool.Pool, since time.Duration,
	acc map[int64]*RouteAccountCounts, s *Summary) error {
	var window any
	if since > 0 {
		window = since.String()
	}

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT sap.source_account_id FROM source_account_projects sap`)
	if err != nil {
		return fmt.Errorf("select route candidate accounts: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan route candidate account: %w", err)
		}
		routeAccount(acc, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate route candidate accounts: %w", err)
	}

	rows, err = pool.Query(ctx, `
		SELECT acct.source_account_id, cd.route_step, count(*)
		  FROM capture_decisions cd
		  JOIN normalized_messages nm ON nm.id = cd.message_id`+routeAccountJoin+`
		 WHERE cd.mode = 'route'
		   AND ($1::interval IS NULL OR cd.created_at >= now() - $1::interval)
		 GROUP BY 1, 2`, window)
	if err != nil {
		return fmt.Errorf("select route rows by account and step: %w", err)
	}
	for rows.Next() {
		var id int64
		var step string
		var n int
		if err := rows.Scan(&id, &step, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan route rows: %w", err)
		}
		routeAccount(acc, id).Steps[step] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate route rows: %w", err)
	}

	rows, err = pool.Query(ctx, `SELECT acct.source_account_id, count(*)`+inboxWhereRoute+`
		   AND ($1::interval IS NULL OR nm.sent_at >= now() - $1::interval)
		 GROUP BY 1`, window)
	if err != nil {
		return fmt.Errorf("select pending route verdicts: %w", err)
	}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return fmt.Errorf("scan pending route verdicts: %w", err)
		}
		routeAccount(acc, id).PendingVerdict += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pending route verdicts: %w", err)
	}

	ids := make([]int64, 0, len(acc))
	for id := range acc {
		ids = append(ids, id)
	}
	rows, err = pool.Query(ctx,
		`SELECT id, account_email, route_after IS NOT NULL FROM source_accounts WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("select route accounts: %w", err)
	}
	for rows.Next() {
		var id int64
		var email string
		var armed bool
		if err := rows.Scan(&id, &email, &armed); err != nil {
			rows.Close()
			return fmt.Errorf("scan route account: %w", err)
		}
		a := routeAccount(acc, id)
		a.Account, a.Armed = email, armed
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate route accounts: %w", err)
	}

	for _, a := range acc {
		if a.Account == "" {
			a.Account = fmt.Sprintf("(account %d)", a.AccountID)
		}
		s.RouteAccounts = append(s.RouteAccounts, *a)
	}
	sort.Slice(s.RouteAccounts, func(i, j int) bool {
		if s.RouteAccounts[i].Account != s.RouteAccounts[j].Account {
			return s.RouteAccounts[i].Account < s.RouteAccounts[j].Account
		}
		return s.RouteAccounts[i].AccountID < s.RouteAccounts[j].AccountID
	})
	return nil
}

// renderRouteReport prints the route lane (B9). Every number comes from
// Summarize; the /funnel block renders the same Summary.
func renderRouteReport(w io.Writer, s Summary, since time.Duration) {
	var grounded, ungrounded, noChoice int
	for _, a := range s.RouteAccounts {
		grounded += a.Grounded
		ungrounded += a.Ungrounded
		noChoice += a.NoChoice
	}
	fmt.Fprintln(w, "Classify shadow report (route — which candidate project does an unmatched message belong to?)")
	fmt.Fprintf(w, "  classified: %d  grounded: %d  ungrounded: %d  no choice: %d\n",
		s.Classified, grounded, ungrounded, noChoice)
	for _, k := range sortedKeys(s.ByKind) {
		fmt.Fprintf(w, "    by chosen project  %-24s %d\n", k, s.ByKind[k])
	}
	fmt.Fprintln(w, "  by account        armed    verdicts grounded ungrounded no_choice pending_verdict   thread single  model default")
	for _, a := range s.RouteAccounts {
		armed := "SHADOW"
		if a.Armed {
			armed = "armed"
		}
		fmt.Fprintf(w, "    %-32s %-7s %8d %8d %10d %9d %15d %8d %6d %6d %7d\n", trunc(a.Account, 32), armed,
			a.Verdicts, a.Grounded, a.Ungrounded, a.NoChoice, a.PendingVerdict,
			a.Steps["thread"], a.Steps["single"], a.Steps["model"], a.Steps["default"])
	}
	if len(s.RouteAccounts) == 0 {
		fmt.Fprintln(w, "    (no account has route candidates; add them with `opsctl route-candidates add`)")
	}
	fmt.Fprintf(w, "  grounded = the evidence is a verbatim span of the subject or body, at least %d words and %d characters:\n",
		GroundMinWords, GroundMinChars)
	fmt.Fprintln(w, "  route_apply's step 3.")
	fmt.Fprintln(w, "  ungrounded = a candidate was chosen but not quoted: step 4, the account's default.")
	window := "all time"
	if since > 0 {
		window = "sent within " + since.String()
	}
	fmt.Fprintf(w, "  pending_verdict = live-unmatched on a candidate account with no current verdict (%s);\n", window)
	fmt.Fprintln(w, "  a missing verdict never falls to the default. Steps are route_apply's mode='route' rows; a SHADOW")
	fmt.Fprintln(w, "  account writes none.")
	fmt.Fprintln(w)

	if s.Skipped > 0 {
		renderSkipCounts(w, s)
		fmt.Fprintln(w, "    NOTE: an all-skipped pass is EXPECTED when the local model is not running.")
		fmt.Fprintln(w, "          An unmatched message is restricted, so it is only ever classified locally;")
		fmt.Fprintln(w, "          falling back to a hosted provider is never the fix. See")
		fmt.Fprintln(w, "          docs/runbooks/local-classifier.md (the routing lane).")
		fmt.Fprintln(w)
	}

	if len(s.allFlags) > 0 {
		fmt.Fprintln(w, "grounded verdicts, newest first:")
		for _, f := range s.allFlags {
			fmt.Fprintf(w, "  %s  #%-7d %-24s %-28s %-34s evidence: %s\n",
				f.At.Format("2006-01-02 15:04"), f.MessageID, trunc(f.Kind, 24),
				trunc(f.Sender, 28), trunc(f.Subject, 34), f.Title)
		}
		fmt.Fprintln(w)
	}
}
