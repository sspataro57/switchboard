//go:build integration

package capture_test

// SWT-23 criteria 1, 3 and 4 — the residue census, against a real database.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run CaptureCensus ./internal/capture/
//
// WHY INTEGRATION AND NOT UNIT. Every number here is produced by a GROUP BY over
// capture_decisions joined to normalized_messages. A fake that supplied the rows
// would be supplying the very values the section is supposed to compute — the
// 6th landmine in SWT-21's list, and the reason the rule in this repo is that a
// predicate (or an aggregate) whose input comes from a COLUMN gets its test here.
// The pure half — the sender parse — is unit-tested in senderdomain_test.go,
// where it belongs, because it takes a string and returns a string.
//
// IMPOSED SURFACE. The SPEC names the three sections but not how `--domain`
// reaches them, so this file fixes the smallest shape that works and that
// cmd/opsctl can call:
//
//	func Report(ctx context.Context, pool *pgxpool.Pool, since time.Time, domain string) (string, error)
//
// `domain == ""` renders today's report plus the two new always-on sections;
// a non-empty domain adds reportUnmatchedDomainDetail for that one domain.
// The existing three-argument call in rules_integration_test.go is updated in
// the same change. If the implementer prefers an options struct, change these
// call sites — the ASSERTIONS below are about the output, not the signature.
//
// EVERYTHING IS A DELTA. The report is global by design (it is the census of the
// whole residue), so other suites' leftovers legitimately appear in it. Each
// assertion below takes the report BEFORE this suite seeds and again after, and
// asserts the difference — an absolute count would be a flake, not a check, and
// the sibling suite's cleanup deletes `capture_decisions` wholesale.
//
// CROSS-SUITE DISCIPLINE: this suite owns provider 'itest-census-src', thread
// keys 'itest-census:%' and nothing else; cleanup runs at start AND end in FK
// order. It creates NO project — an unmatched decision cannot name one
// (0015:126-127, capture_decisions_unmatched_has_no_project).
//
// GREENFIELD NOTE: the three sections do not exist and Report takes three
// arguments today, so this file compile-FAILS. Expected red.

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/store"
)

const (
	ccProvider = "itest-census-src"
	ccAccount  = "itest-census@pg-main"

	// Two DIFFERENT full From headers at ONE domain. This is criterion 1's whole
	// argument in a fixture: `reportUnmatchedSenders` groups by the whole
	// `Name <addr>` string and prints these as two rows of 3 and 2, so the
	// operator never sees the 5 that decides whether a rule is worth writing.
	ccLinkedInA = "ITest LinkedIn <messages-noreply@itest-linkedin.example>"
	ccLinkedInB = "ITest LinkedIn Job Alerts <jobalerts-noreply@itest-linkedin.example>"
	ccLinkedIn  = "itest-linkedin.example"

	// Mixed case on purpose: a domain is case-insensitive and a rule is written
	// lower case, so two casings must fold to one row.
	ccMediumSender = "ITest Medium Daily <news@ITest-Medium.EXAMPLE>"
	ccMedium       = "itest-medium.example"

	// The address-less segment (premise 6): a bare display name is slack or
	// upwork, NEVER gmail. Measured 2026-08-31: 1,287 of the real residue carry
	// no `@` and every one of them is channel='upwork'.
	ccNameOnlyA  = "itest gil vazquez"
	ccNameOnlyB  = "itest mario cruz"
	ccUpworkAddr = "ITest Upwork <donotreply@itest-upwork.example>"
)

type ccSuite struct {
	pool *pgxpool.Pool
}

func newCCSuite(t *testing.T, ctx context.Context) *ccSuite {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db (cleanup deletes corpus rows); " +
			"use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &ccSuite{pool: pool}
	s.cleanup(t, ctx)
	t.Cleanup(func() { s.cleanup(t, ctx) })
	return s
}

func (s *ccSuite) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	const raws = `(SELECT id FROM raw_source_items WHERE source_account_id IN
	                (SELECT id FROM source_accounts WHERE provider='` + ccProvider + `'))`
	for _, q := range []string{
		// message_id is ON DELETE CASCADE, but the decisions are deleted
		// explicitly so a failure here reads as a cleanup failure rather than as
		// a mysterious FK error two statements later.
		`DELETE FROM capture_decisions WHERE message_id IN
		   (SELECT id FROM normalized_messages WHERE raw_source_item_id IN ` + raws + `)`,
		`DELETE FROM ai_extractions WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_messages WHERE raw_source_item_id IN ` + raws,
		`DELETE FROM normalized_threads WHERE thread_key LIKE 'itest-census:%'`,
		`DELETE FROM raw_source_items WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + ccProvider + `')`,
		`DELETE FROM sync_runs WHERE source_account_id IN
		   (SELECT id FROM source_accounts WHERE provider='` + ccProvider + `')`,
		`DELETE FROM source_accounts WHERE provider='` + ccProvider + `'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
}

// seed writes the residue fixture: every message inbound, every message with a
// LATEST decision of 'unmatched' except the two named controls.
func (s *ccSuite) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	ins := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", q, err)
		}
		return id
	}
	account := ins(`INSERT INTO source_accounts (provider, account_email, send_enabled)
	                VALUES ($1,$2,false) RETURNING id`, ccProvider, ccAccount)

	msg := func(label, sender, subject, channel string, minsAgo int) int64 {
		rawID := ins(`INSERT INTO raw_source_items (source_account_id, external_id, raw_json, content_hash, normalized_at)
		              VALUES ($1,$2,'{}',$3, now()) RETURNING id`,
			account, "itest-census-"+label, "itest-census-h-"+label)
		threadID := ins(`INSERT INTO normalized_threads (thread_key, subject, participants)
		                 VALUES ($1,$2,'[]') RETURNING id`, "itest-census:"+label, subject)
		return ins(`INSERT INTO normalized_messages
		              (raw_source_item_id, thread_id, direction, external_message_id, sent_at,
		               body_text, subject, sender, channel)
		            VALUES ($1,$2,'inbound',$3, now() - make_interval(mins => $4), 'body',$5,$6,$7) RETURNING id`,
			rawID, threadID, "itest-census-"+label, minsAgo, subject, sender, channel)
	}
	unmatched := func(messageID int64) {
		ins(`INSERT INTO capture_decisions (message_id, mode, action, project_id, reason)
		     VALUES ($1,'shadow','unmatched',NULL,'itest-census: no rule') RETURNING id`, messageID)
	}

	// itest-linkedin.example: 5 messages over TWO full From headers.
	for i, spec := range []struct {
		label, sender, subject string
		minsAgo                int
	}{
		{"li1", ccLinkedInA, ccSubjectNewest, 5},
		{"li2", ccLinkedInA, "itest-census You appeared in 9 searches", 20},
		{"li3", ccLinkedInA, "itest-census Your network is talking", 30},
		{"li4", ccLinkedInB, "itest-census 12 new jobs for you", 40},
		{"li5", ccLinkedInB, "itest-census Jobs you may be interested in", 50},
	} {
		id := msg(spec.label, spec.sender, spec.subject, "gmail", spec.minsAgo)
		unmatched(id)
		// Shadow is re-runnable by design and writes a decision per pass, so one
		// message carries TWO. Counting decision ROWS instead of MESSAGES would
		// make every number in the census grow with the number of times someone
		// ran the pass — the inflation latestDecisions' DISTINCT ON exists to
		// prevent, restated here because a new section is a new place to forget it.
		if i == 0 {
			unmatched(id)
		}
	}

	// One message at the SAME domain with NO decision row at all. UNSEEN is not
	// unmatched (capture-rules.md:8-18): the engine has not looked at it, and a
	// census that counts it reports a residue that includes mail the rules were
	// never run against.
	msg("li-unseen", ccLinkedInA, "itest-census not evaluated yet", "gmail", 10)

	// Mixed-case domain, one message.
	unmatched(msg("med1", ccMediumSender, "itest-census The daily digest", "gmail", 60))

	// The address-less segment, all on channel='upwork', plus ONE upwork message
	// that DOES carry an address — without it the "no @" count and the channel
	// count would be the same number and the column would prove nothing.
	unmatched(msg("up1", ccNameOnlyA, "itest-census can you take a look", "upwork", 15))
	unmatched(msg("up2", ccNameOnlyA, "itest-census following up", "upwork", 25))
	unmatched(msg("up3", ccNameOnlyB, "itest-census new milestone", "upwork", 35))
	unmatched(msg("up4", ccUpworkAddr, "itest-census invitation to interview", "upwork", 45))
}

const ccSubjectNewest = "itest-census Someone viewed your profile"

func ccReport(t *testing.T, ctx context.Context, s *ccSuite, domain string) string {
	t.Helper()
	rep, err := capture.Report(ctx, s.pool, time.Time{}, domain)
	if err != nil {
		t.Fatalf("capture.Report(domain=%q): %v", domain, err)
	}
	return rep
}

// ---- report parsing ----------------------------------------------------------

// ccSection returns the lines of the section whose HEADER matches re. A section
// runs from its header to the next line that is non-empty and unindented — the
// same shape reportSectionLine already assumes.
func ccSection(t *testing.T, report string, re *regexp.Regexp, why string) []string {
	t.Helper()
	lines := strings.Split(report, "\n")
	start := -1
	for i, line := range lines {
		if (line != "" && (line[0] == ' ' || line[0] == '\t')) || line == "" {
			continue
		}
		if re.MatchString(line) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("the report has no section header matching %s.\n%s\n\nreport:\n%s", re, why, report)
	}
	out := []string{}
	for _, line := range lines[start+1:] {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			break
		}
		out = append(out, line)
	}
	return out
}

// ccLine returns the first line of a section mentioning key, or "".
func ccLine(section []string, key string) string {
	for _, line := range section {
		if strings.Contains(line, key) {
			return line
		}
	}
	return ""
}

var ccNumRe = regexp.MustCompile(`\d+(?:\.\d+)?%?`)

// ccNumbers pulls the numeric columns out of one report line, with the key
// itself removed first so digits inside a domain or a subject cannot be read as
// a column.
func ccNumbers(line, key string) []string {
	return ccNumRe.FindAllString(strings.ReplaceAll(line, key, " "), -1)
}

// ccCount reads the message count from a line: the first bare integer.
func ccCount(t *testing.T, line, key string) int {
	t.Helper()
	for _, tok := range ccNumbers(line, key) {
		if !strings.ContainsAny(tok, ".%") {
			n, err := strconv.Atoi(tok)
			if err == nil {
				return n
			}
		}
	}
	t.Errorf("no integer count on the line %q (key %q)", line, key)
	return -1
}

// ccCountFor is the delta primitive: the count on the line mentioning key, or 0
// when the section has no such line.
func ccCountFor(t *testing.T, report string, re *regexp.Regexp, why, key string) int {
	t.Helper()
	line := ccLine(ccSection(t, report, re, why), key)
	if line == "" {
		return 0
	}
	return ccCount(t, line, key)
}

var (
	ccDomainsHeader = regexp.MustCompile(`(?i)unmatched sender domains`)
	ccChannelHeader = regexp.MustCompile(`(?i)unmatched.{0,12}channel`)
	ccDetailHeader  = regexp.MustCompile(`(?i)domain detail|detail for`)
)

const (
	ccDomainsWhy = `criterion 1 adds reportUnmatchedSenderDomains, printed BEFORE the existing ` +
		`TOP UNMATCHED SENDERS section. Suggested header: "TOP UNMATCHED SENDER DOMAINS"`
	ccChannelWhy = `criterion 3 adds reportUnmatchedChannels: the residue by ` +
		`normalized_messages.channel, and within that the count of senders carrying no "@" at all. ` +
		`Suggested header: "UNMATCHED BY CHANNEL", columns: channel, messages, senders with no address`
	ccDetailWhy = `criterion 4 adds reportUnmatchedDomainDetail, rendered only when --domain is passed: ` +
		`the top 20 FULL sender addresses with counts and the newest 10 subjects. ` +
		`Suggested header: "DOMAIN DETAIL <domain>"`
)

// ---- criterion 1: the domain census, with cumulative coverage -----------------

// "The top 20 cover 43%" is the ONE number that decides whether rules or a
// classifier is the cheaper answer for the residue, and today it is not
// printable: the existing section groups by the whole `Name <addr>` string and
// caps at 20 rows with no percentage.
func TestCaptureCensus_Integration_SenderDomainsWithCumulativeShare(t *testing.T) {
	ctx := context.Background()
	s := newCCSuite(t, ctx)

	before := ccReport(t, ctx, s, "")
	beforeLinkedIn := ccCountFor(t, before, ccDomainsHeader, ccDomainsWhy, ccLinkedIn)
	beforeMedium := ccCountFor(t, before, ccDomainsHeader, ccDomainsWhy, ccMedium)

	s.seed(t, ctx)
	after := ccReport(t, ctx, s, "")
	domains := ccSection(t, after, ccDomainsHeader, ccDomainsWhy)

	// (a) TWO full From headers, ONE domain row, and the count is 5.
	line := ccLine(domains, ccLinkedIn)
	if line == "" {
		t.Fatalf("no line for %q in the sender-domain section:\n%s\n\nfull report:\n%s",
			ccLinkedIn, strings.Join(domains, "\n"), after)
	}
	if got := ccCount(t, line, ccLinkedIn) - beforeLinkedIn; got != 5 {
		t.Errorf("%s counts %d new message(s), want 5 (3 from %q + 2 from %q).\nline: %q\n"+
			"If this is 6, the section is counting UNSEEN messages — a message with no decision row is not "+
			"unmatched, the engine has not looked at it. If this is 7, it is counting decision ROWS: shadow "+
			"writes one per pass, so the census must read the LATEST decision per message.",
			ccLinkedIn, got, ccLinkedInA, ccLinkedInB, line)
	}

	// The domain must be spelled as a rule would be written — no angle bracket.
	// split_part(sender,'@',2) on `Name <a@b.com>` yields `b.com>`; criterion 2
	// requires the parse in Go and this is where a SQL-side fold shows up.
	if strings.Contains(line, ccLinkedIn+">") || strings.Contains(line, "<") {
		t.Errorf("the domain row is %q — it carries an angle bracket, which is what "+
			"split_part(sender,'@',2) produces on a `Name <addr>` header. `capture.KindSender` is a "+
			"substring match on the raw From header, so a domain with a '>' in it matches nothing and "+
			"nothing errors", line)
	}

	// (b) The mixed-case domain folds into the lower-cased row.
	medium := ccLine(domains, ccMedium)
	if medium == "" {
		t.Errorf("no line for %q; the fixture's sender is %q, so the host must be LOWER-CASED before "+
			"grouping — two casings of one domain are two rows and each looks too small to earn a rule",
			ccMedium, ccMediumSender)
	} else if got := ccCount(t, medium, ccMedium) - beforeMedium; got != 1 {
		t.Errorf("%s counts %d new message(s), want 1.\nline: %q", ccMedium, got, medium)
	}

	// (c) The address-less senders are still counted, under themselves. The
	// fallback is the RAW STRING (criterion 2), never an empty key: 1,287
	// messages disappearing into one anonymous row is how a funnel finding gets
	// missed.
	if ccLine(domains, ccNameOnlyA) == "" {
		t.Errorf("the sender-domain section has no row for the address-less sender %q. Its 'domain' is the "+
			"raw string, because premise 6 says a bare display name is slack or upwork — WORK sitting "+
			"unmatched, and the census is where that gets seen", ccNameOnlyA)
	}

	// (d) CUMULATIVE SHARE — the column the whole section exists for.
	var shares, cums []float64
	for _, l := range domains {
		if strings.TrimSpace(l) == "" {
			continue
		}
		pcts := []float64{}
		for _, tok := range ccNumRe.FindAllString(l, -1) {
			if strings.ContainsAny(tok, ".%") {
				v, err := strconv.ParseFloat(strings.TrimSuffix(tok, "%"), 64)
				if err == nil {
					pcts = append(pcts, v)
				}
			}
		}
		if len(pcts) < 2 {
			continue
		}
		shares = append(shares, pcts[len(pcts)-2])
		cums = append(cums, pcts[len(pcts)-1])
	}
	if len(cums) < 3 {
		t.Fatalf("only %d row(s) in the sender-domain section carry both a share and a CUMULATIVE share.\n"+
			"Criterion 1: 'sender domain, message count, share of the residue, and cumulative share, top "+
			"40'. The cumulative column is the point — without it nobody can answer 'do the top 20 cover "+
			"enough of this pile to be worth writing rules for'.\nsection:\n%s",
			len(cums), strings.Join(domains, "\n"))
	}
	for i := range cums {
		if cums[i]+1e-9 < shares[i] {
			t.Errorf("row %d has cumulative share %.4f below its own share %.4f; the cumulative column is "+
				"the running total, so it is never smaller than the row it is on:\n%s",
				i, cums[i], shares[i], strings.Join(domains, "\n"))
		}
		if i > 0 && cums[i]+1e-9 < cums[i-1] {
			t.Errorf("cumulative share went DOWN at row %d (%.4f after %.4f). The section is ordered by "+
				"volume descending and the column accumulates down it:\n%s",
				i, cums[i], cums[i-1], strings.Join(domains, "\n"))
		}
	}

	// (e) Registered BEFORE the existing full-`From` section, which STAYS. Order
	// is the criterion's own word: the domain table is the one a reader acts on,
	// and the sender table is the detail underneath it.
	iDomains := ccDomainsHeader.FindStringIndex(after)
	sendersRe := regexp.MustCompile(`(?im)^\s*TOP UNMATCHED SENDERS\s*$`)
	iSenders := sendersRe.FindStringIndex(after)
	if iSenders == nil {
		t.Errorf("the TOP UNMATCHED SENDERS section is gone. Criterion 1: the domain section is added " +
			"BEFORE it and it STAYS — the full From header is what a --domain investigation starts from")
	} else if iDomains != nil && iDomains[0] > iSenders[0] {
		t.Errorf("the sender-DOMAIN section is printed after the sender section; criterion 1 registers it " +
			"before reportUnmatchedSenders in the section list")
	}

	// (f) And the sibling section still splits what this one joins — the fixture's
	// whole argument, asserted rather than assumed.
	senders := ccSection(t, after, sendersRe, "the pre-existing full-From section")
	if ccLine(senders, ccLinkedInA) == "" || ccLine(senders, ccLinkedInB) == "" {
		t.Errorf("TOP UNMATCHED SENDERS does not show both %q and %q as separate rows. If they have been "+
			"merged, the two sections now say the same thing and the domain section's argument (one rule "+
			"covers both) is no longer visible", ccLinkedInA, ccLinkedInB)
	}
}

// ---- criterion 3: the channel breakdown and the no-`@` count ------------------

// The single line that tells work from noise. A bare display name means slack or
// upwork, never gmail: google writes the raw From header
// (connector/google/rfc822.go:134, normalize.go:121) which always carries an
// address, while slackweb writes message.Author (normalize.go:88) and upworkcrm
// the CRM's `sender` column (normalize.go:148) — both display names. Measured on
// the real corpus 2026-08-31: 1,287 address-less residue messages, every one of
// them channel='upwork'.
func TestCaptureCensus_Integration_ChannelsAndTheAddresslessCount(t *testing.T) {
	ctx := context.Background()
	s := newCCSuite(t, ctx)

	before := ccReport(t, ctx, s, "")
	beforeChannels := ccSection(t, before, ccChannelHeader, ccChannelWhy)
	beforeUpwork := ccChannelNumbers(t, ccLine(beforeChannels, "upwork"), "upwork")
	beforeGmail := ccChannelNumbers(t, ccLine(beforeChannels, "gmail"), "gmail")

	s.seed(t, ctx)
	after := ccReport(t, ctx, s, "")
	channels := ccSection(t, after, ccChannelHeader, ccChannelWhy)

	upwork := ccChannelNumbers(t, ccLine(channels, "upwork"), "upwork")
	gmail := ccChannelNumbers(t, ccLine(channels, "gmail"), "gmail")

	if got := upwork.total - beforeUpwork.total; got != 4 {
		t.Errorf("channel 'upwork' gained %d unmatched message(s), want 4 (3 address-less + 1 with an "+
			"address).\nsection:\n%s", got, strings.Join(channels, "\n"))
	}
	if got := upwork.noAt - beforeUpwork.noAt; got != 3 {
		t.Errorf("channel 'upwork' gained %d sender(s) with no '@', want 3.\nsection:\n%s\n"+
			"This is the work-vs-noise discriminator (premise 6). If it reports 4, the count is counting "+
			"every message on the channel rather than the address-less ones, and the column says nothing.",
			got, strings.Join(channels, "\n"))
	}
	if got := gmail.total - beforeGmail.total; got != 6 {
		t.Errorf("channel 'gmail' gained %d unmatched message(s), want 6 (5 linkedin + 1 medium; the "+
			"6th linkedin fixture has NO decision row and must not be counted).\nsection:\n%s",
			got, strings.Join(channels, "\n"))
	}
	if got := gmail.noAt - beforeGmail.noAt; got != 0 {
		t.Errorf("channel 'gmail' gained %d address-less sender(s), want 0. Every google row carries a raw "+
			"From header with an address in it; a non-zero here means the '@' test is not testing the "+
			"sender.\nsection:\n%s", got, strings.Join(channels, "\n"))
	}
}

type ccChannelRow struct{ total, noAt int }

// ccChannelNumbers reads the two columns of a channel line. IMPOSED ORDER:
// messages first, senders-with-no-`@` second.
func ccChannelNumbers(t *testing.T, line, key string) ccChannelRow {
	t.Helper()
	if line == "" {
		return ccChannelRow{}
	}
	nums := []int{}
	for _, tok := range ccNumbers(line, key) {
		if strings.ContainsAny(tok, ".%") {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err == nil {
			nums = append(nums, n)
		}
	}
	if len(nums) < 2 {
		t.Fatalf("the channel line %q carries %d numeric column(s), want at least 2: the message count and "+
			"the count of senders with NO '@' at all (criterion 3). One number cannot answer the question "+
			"the section exists for", line, len(nums))
	}
	return ccChannelRow{total: nums[0], noAt: nums[1]}
}

// ---- criterion 4: --domain, the tool that answers "what IS sspataro.com" ------

// A Phase-0 BLOCKER: the rule set of criterion 5 may not be written until this
// has been run and its output recorded in docs/runbooks/capture-rules.md. It is
// the query that turned `sspataro.com` (518) from a suspicion into
// `test@sspataro.com` session notifications, and `upwork.com` (106) into
// "Invitation to Interview" — one goes to bulk, the other stays refused, and
// nothing but this output could tell them apart.
func TestCaptureCensus_Integration_DomainDetail(t *testing.T) {
	ctx := context.Background()
	s := newCCSuite(t, ctx)
	s.seed(t, ctx)

	// The control FIRST: the detail is rendered only when --domain is passed. The
	// subjects are the fixture's own strings and appear nowhere else in the
	// report, so their absence here is what makes their presence below mean
	// something.
	plain := ccReport(t, ctx, s, "")
	if strings.Contains(plain, ccSubjectNewest) {
		t.Errorf("a report with no --domain already prints message subjects. The domain detail is a "+
			"targeted investigation, not a default section: a census that prints 14,737 subjects is one "+
			"nobody reads.\nreport:\n%s", plain)
	}

	report := ccReport(t, ctx, s, ccLinkedIn)
	detail := ccSection(t, report, ccDetailHeader, ccDetailWhy)
	text := strings.Join(detail, "\n")

	// (a) FULL sender addresses with counts — the granularity the domain row
	// hides, and the one an investigation needs.
	for _, want := range []struct {
		sender string
		n      int
	}{{ccLinkedInA, 3}, {ccLinkedInB, 2}} {
		line := ccLine(detail, want.sender)
		if line == "" {
			t.Errorf("the domain detail for %s does not list the full sender %q. 'Top 20 FULL sender "+
				"addresses with counts' is what turns a domain into a decision — sspataro.com resolved to "+
				"test@sspataro.com (503, session notifications, bulk) and openproject@sspataro.com (14) "+
				"only because this listed the local parts.\nsection:\n%s", ccLinkedIn, want.sender, text)
			continue
		}
		if got := ccCount(t, line, want.sender); got != want.n {
			t.Errorf("the detail line for %q counts %d, want %d.\nline: %q", want.sender, got, want.n, line)
		}
	}

	// (b) The newest subjects, so a reader can see WHAT this sender sends.
	if !strings.Contains(text, ccSubjectNewest) {
		t.Errorf("the domain detail does not show the newest subject %q. Criterion 4 asks for the newest "+
			"10 — the counts say how much, the subjects say what, and the claim gate of criterion 6 needs "+
			"both.\nsection:\n%s", ccSubjectNewest, text)
	}

	// (c) SCOPED to the one domain. A detail section that leaks other domains is
	// the census again, not an investigation.
	for _, other := range []string{ccMediumSender, ccNameOnlyA, ccUpworkAddr} {
		if strings.Contains(text, other) {
			t.Errorf("the domain detail for %s also lists %q, which belongs to another domain.\nsection:\n%s",
				ccLinkedIn, other, text)
		}
	}

	// (d) UNMATCHED only, here too. The unseen fixture shares the sender and the
	// domain, so if it appears the detail is reading normalized_messages rather
	// than the residue.
	if strings.Contains(text, "itest-census not evaluated yet") {
		t.Errorf("the domain detail includes a message with NO capture_decisions row. Unseen is not "+
			"unmatched: the engine has not looked at it, and a claim gate sampled from that population is "+
			"sampling mail the rules were never run against.\nsection:\n%s", text)
	}
}

// ---- the sections must survive an empty residue -------------------------------

// An alarm that cannot tell "did not run" from "found nothing" is this repo's
// named failure class. A census over a residue that contains nothing of ours
// must still print its sections, with a stated "(none)" rather than a missing
// heading.
func TestCaptureCensus_Integration_SectionsRenderWithNoRowsOfOurs(t *testing.T) {
	ctx := context.Background()
	s := newCCSuite(t, ctx)

	// A window in the future contains no decision at all, this suite's or anyone
	// else's — the same shape TestCaptureRules_Integration_ReportSections uses.
	report, err := capture.Report(ctx, s.pool, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatalf("Report(future window): %v", err)
	}
	for _, re := range []*regexp.Regexp{ccDomainsHeader, ccChannelHeader} {
		if !re.MatchString(report) {
			t.Errorf("the report over an empty window has no section matching %s. A section that "+
				"disappears when it is empty makes 'nothing arrived' and 'the section was removed' the "+
				"same output.\nreport:\n%s", re, report)
		}
	}
	if strings.Contains(report, ccLinkedIn) {
		t.Fatalf("a report windowed to the future shows this suite's fixtures; --since is what makes "+
			"'what changed after I added that rule' answerable.\nreport:\n%s", report)
	}
}
