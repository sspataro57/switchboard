package classify_test

// SWT-33 — the criteria this SPEC asks to be enforced mechanically rather than
// by review: 10 (the missing ai_locality clause and its stated reason), 11's
// honesty-label extension, 14 (ONE spelling of the thread-scope rule),
// "the sanctioned carrier is tasks.source_thread_id" (no second provenance
// store), 26 (shadow is structural), 27 (migration 0024's shape and the ledger),
// 28 (the runbook's Inquiry-lane section) and 29 (the institutional-knowledge
// entry, INCLUDING its dated correction of the stale SWT-12 line).
//
// ZERO I/O beyond reading this repo's own source and docs. Same shape as
// internal/classify/structure_test.go, internal/tools/dismiss_structure_test.go
// and internal/ticketstatus/structure_test.go, and the same rule applies: every
// assertion first REQUIRES its subject to exist, because a source scan that
// passes because there was nothing to scan is the "fixture that proves nothing"
// landmine wearing a lab coat.
//
// GREENFIELD NOTE — EXPECTED RED. migrations/0024_project_ai_inquiry.sql,
// internal/classify/inquiry.go, the Inquiry-lane runbook section and the
// INSTITUTIONAL_KNOWLEDGE entry do not exist, so every guard below fails on a
// missing subject — and the file compile-fails alongside inquiry_test.go on
// classify.LaneInquiry. Two assertions here are GREEN today and are guards
// rather than discoveries: the no-second-provenance-store scan and the
// no-SQL-spells-a-slack-key scan. Their job is to STAY green.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/classify"
)

// ---- criterion 27: migration 0024, its two statements and their order --------

func TestMigration0024_IsTheOnlyOneThisTicketAdds(t *testing.T) {
	// Control first: 0023 must still be there, or a glob returning nothing below
	// would prove only that the directory moved.
	if _, err := os.Stat(filepath.Join("..", "..", "migrations", "0023_ticket_status_sync.sql")); err != nil {
		t.Fatalf("migrations/0023_ticket_status_sync.sql is missing: %v — SWT-32 owns it and this "+
			"ticket's precondition is that it merged first", err)
	}

	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0024_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0024_*.sql: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d migrations/0024_*.sql file(s), want exactly 1 (the SPEC names "+
			"0024_project_ai_inquiry.sql). 0023 is the current highest, and this ticket's data-model "+
			"section is ONE migration: the ai_inquiry column, then the UPDATE that arms collaboratory. "+
			"Merging a migration is not applying it — check `SELECT max(version) FROM schema_migrations` "+
			"before deploying", len(matches))
	}
	// The LIVING registry of owned migration numbers is the ledger in
	// internal/classify/structure_test.go; this guard keeps only its own
	// ticket's claim, exactly one 0024 (the SWT-30/31/32 amendment shape). When
	// a later ticket owns 0025, amend the LEDGER, not this test.

	rel := filepath.Join("migrations", filepath.Base(matches[0]))
	sql := strings.ToLower(csRepoFile(t, rel))

	// (1) the ALTER — a typed COLUMN, fail-closed.
	alter := regexp.MustCompile(`alter\s+table\s+projects\s+add\s+column\s+ai_inquiry\s+boolean\s+not\s+null\s+default\s+false`)
	if !alter.MatchString(sql) {
		t.Errorf("%s does not `ALTER TABLE projects ADD COLUMN ai_inquiry BOOLEAN NOT NULL DEFAULT "+
			"false`. D3: a typed column, not a projects.policies jsonb key — 0016 (ai_locality), 0018 "+
			"(ai_classify) and 0023 (ticket_assignee_gate) are three consecutive precedents and the "+
			"recorded argument against jsonb predicates. DEFAULT FALSE is 0018's polarity and its "+
			"reasoning: a stall is one UPDATE, a leak is irreversible. NOT NULL because a nullable "+
			"boolean makes `AND p.ai_inquiry` silently exclude every row nobody set", rel)
	}

	// (2) the UPDATE — D4: this migration ARMS collaboratory, unlike 0021's
	// classify_promote_after which was left NULL because it EXITS shadow.
	update := regexp.MustCompile(`(?s)update\s+projects\s+set\s+ai_inquiry\s*=\s*true.{0,80}'collaboratory'`)
	if !update.MatchString(sql) {
		t.Errorf("%s does not `UPDATE projects SET ai_inquiry = true WHERE slug = 'collaboratory'`. D4: "+
			"this lane CREATES NOTHING, so arming it is reversible with one UPDATE and is what makes the "+
			"ticket verifiable at all — an unarmed lane reports processed:0, which is indistinguishable "+
			"from an empty inbox or a dead poller", rel)
	}

	// STATEMENT ORDER IS LOAD-BEARING (0016's lesson, restated by every
	// migration guard since): ALTER, then UPDATE. Reordered, the UPDATE runs
	// before the column exists.
	iAlter := alter.FindStringIndex(sql)
	iUpdate := update.FindStringIndex(sql)
	if iAlter != nil && iUpdate != nil && iAlter[0] > iUpdate[0] {
		t.Errorf("%s runs its UPDATE (at %d) before its ALTER (at %d). The order is ALTER (fail-closed "+
			"default) -> UPDATE (arm collaboratory), and reordered the UPDATE names a column that does "+
			"not exist yet", rel, iUpdate[0], iAlter[0])
	}

	// (3) what it must NOT do. Each of these is a thing a session would add.
	if strings.Contains(sql, "drop column") || regexp.MustCompile(`(?s)--\s*down`).MatchString(sql) {
		t.Errorf("%s looks like it carries a down migration. Migrations here are FORWARD-ONLY, no "+
			"exceptions", rel)
	}
	if regexp.MustCompile(`create\s+index`).MatchString(sql) {
		t.Errorf("%s creates an index. NO INDEX, deliberately: `projects` holds tens of rows and every "+
			"read reaches it by primary key. 0016/0018/0023 record the argument — an index nothing uses "+
			"is a permanent claim that some query needs it, which the next reader has to disprove", rel)
	}
	if regexp.MustCompile(`insert\s+into\s+capture_rules`).MatchString(sql) {
		t.Errorf("%s inserts into capture_rules. Out of scope 3: rules 8/9 stay ATTRIBUTION-ONLY and this "+
			"lane does not touch them. A rule inserted around the `capture_rule_add` executor tool has no "+
			"audit row and no pattern validation (invariant 3)", rel)
	}
	if regexp.MustCompile(`create\s+table`).MatchString(sql) {
		t.Errorf("%s creates a table. Invariant 2: NO new tables — verdicts are ai_runs + ai_extractions "+
			"rows, exactly as the other two lanes, and the review surface is a filter over an existing "+
			"fold (/funnel), not a new store", rel)
	}
	if strings.Contains(sql, "ai_classify") {
		t.Errorf("%s mentions ai_classify. D3: ai_inquiry is its OWN column. ai_classify means 'mail "+
			"attributed here gets an actionability verdict from the personal lane' (0018's own words) and "+
			"`personal` is the only project carrying it — reusing it would drag personal mail into a lane "+
			"that asks a client-conversation question", rel)
	}
}

// The LEDGER — internal/classify/structure_test.go's living registry of owned
// migration numbers — must learn 24. Criterion 27 says rewrite the guard, never
// delete it: the migrate runner keys on schema_migrations.version with NO
// checksum, so a stray or edited file is skipped SILENTLY and the schema
// diverges with no error anywhere.
func TestMigrationLedger_Learns0024(t *testing.T) {
	const rel = "internal/classify/structure_test.go"
	src := csRepoFile(t, rel)

	// Control: the ledger is still there. If the test that owns it was deleted
	// rather than rewritten, the assertion below would pass vacuously on some
	// other mention of 24.
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("%s no longer carries the migration ledger (\"THIS LEDGER IS THE LIVING REGISTRY\"). "+
			"Criterion 27: REWRITE the guard to the new truth, do NOT delete it. A guard that becomes "+
			"wrong and gets deleted is how the next ticket adds a migration nobody notices", rel)
	}
	// The window opens BEFORE the marker as well as after it: the per-number
	// ownership list is the comment ABOVE the sentinel and the predicate is
	// below it, and a window anchored only forwards would report "0024 is
	// unowned" while its owner sat three lines up.
	start := i - 3000
	if start < 0 {
		start = 0
	}
	end := i + 1800
	if end > len(src) {
		end = len(src)
	}
	ledger := src[start:end]

	if !regexp.MustCompile(`n\s*!=\s*24|24\s*&&|!=\s*24`).MatchString(ledger) {
		t.Errorf("the ledger in %s does not account for migration 0024:\n%s\n"+
			"SWT-33 (inquiry-classify) owns 0024_project_ai_inquiry.sql, named by its ticket's \"Data "+
			"model changes\" section. Until the ledger learns it, `ls migrations/` shows a file no SPEC "+
			"accounts for and the guard cries wolf on a legitimate migration.", rel, ledger[:min(len(ledger), 1200)])
	}
	if !strings.Contains(ledger, "SWT-33") {
		t.Errorf("the ledger in %s adds 0024 without naming the ticket that owns it. The registry's value "+
			"is the OWNERSHIP, not the number: an unowned entry is exactly what it exists to catch", rel)
	}
}

// ---- criterion 14: ONE spelling of the thread-scope rule ---------------------

// SWT-19's landmine, generalised. Whether a Slack thread key is ROOTED
// (slack:{ws}:{conv}:{root}) or conversation-level (slack:{ws}:{conv}) is
// decided by ONE exported helper in internal/connector/slackweb, beside
// channelThreadKey which BUILDS it. internal/classify must not re-spell the
// rule, and no SQL anywhere may take the key apart.
//
// This is the classify half; the repo-wide SQL scan lives beside the helper, in
// internal/connector/slackweb/threadscope_test.go, in the shape of
// upworkcrm/keyspelling_test.go.
func TestClassify_DoesNotRespellTheSlackThreadKeyRule(t *testing.T) {
	// Control: the package must actually decide a thread scope, or the scan
	// below is checking a rule nobody applies.
	var mentionsScope bool
	for _, rel := range csSources(t, "internal/classify") {
		if strings.Contains(csGoCode(t, rel), "thread_scope") {
			mentionsScope = true
		}
	}
	if !mentionsScope {
		t.Fatalf("nothing in internal/classify mentions thread_scope. Criterion 13 records it on EVERY " +
			"verdict, and criterion 15 pins its four values against Postgres; a scan for a second " +
			"spelling of a rule nobody applies proves nothing")
	}

	surgery := regexp.MustCompile(`(?i)\bLIKE\b|split_part|strings\.Split|strings\.Count|strings\.LastIndex|strings\.HasSuffix|\bstrings\.Contains\b`)
	for _, rel := range csSources(t, "internal/classify") {
		code := csGoCode(t, rel)
		for _, line := range strings.Split(code, "\n") {
			if !strings.Contains(line, "slack:") {
				continue
			}
			if surgery.MatchString(line) {
				t.Errorf("%s picks apart a slack thread key:\n\t%s\n"+
					"Criterion 14: the rooted/unrooted decision has ONE spelling, an exported helper in "+
					"internal/connector/slackweb beside channelThreadKey which builds it. A second "+
					"spelling here drifts from the builder with no error anywhere — SWT-19's landmine, "+
					"and the reason the upwork key parse lives in exactly one file",
					rel, strings.TrimSpace(line))
			}
		}
	}
}

// ---- the slackweb import reaches exactly two symbols -------------------------

// Added by the SWT-33 re-review. internal/classify imports
// internal/connector/slackweb for ONE reason — criterion 14's single spelling
// of the rooted/unrooted rule — and that package also carries the bridge that
// drives the live Slack composer (net/http, os/exec). The token scan in
// TestClassifyPackage_FetchesNothingAndDecodesNoMIME only sees classify's own
// import strings, so a call to slackweb.CommandBridge from here would pass it.
// This allowlist is what stops the key helper's import from becoming a side
// door to a send path (invariant 4).
func TestClassify_UsesOnlyTheSlackKeyHelpersFromSlackweb(t *testing.T) {
	allowed := map[string]bool{"IsRootedThreadKey": true, "Channel": true}
	use := regexp.MustCompile(`\bslackweb\.([A-Za-z_]\w*)`)
	seen := map[string]bool{}
	for _, rel := range csSources(t, "internal/classify") {
		for _, m := range use.FindAllStringSubmatch(csGoCode(t, rel), -1) {
			seen[m[1]] = true
			if !allowed[m[1]] {
				t.Errorf("%s uses slackweb.%s. internal/classify may use exactly slackweb.IsRootedThreadKey and "+
					"slackweb.Channel — the key-shape rule of criterion 14. Anything else in that package reaches "+
					"the Slack bridge, and this lane creates nothing and sends nothing", rel, m[1])
			}
		}
	}
	// The IMPORT is pinned too (second re-review): the selector scan above keys
	// on the identifier `slackweb.`, so an aliased or dot import would walk past
	// it. Exactly one file imports the package, unaliased.
	const importPath = `"github.com/sspataro57/switchboard/internal/connector/slackweb"`
	importers := 0
	for _, rel := range csSources(t, "internal/classify") {
		for _, line := range strings.Split(csRepoFile(t, rel), "\n") {
			tl := strings.TrimSpace(line)
			if !strings.HasSuffix(tl, importPath) {
				continue
			}
			importers++
			if tl != importPath {
				t.Errorf("%s imports slackweb under an alias (%q); the allowlist reads `slackweb.` selectors, "+
					"so an alias would bypass it", rel, tl)
			}
		}
	}
	if importers != 1 {
		t.Errorf("internal/classify imports internal/connector/slackweb from %d file(s), want exactly 1 "+
			"(inquiry.go, for the thread-scope rule)", importers)
	}
	// Positive control: the helper really is used, or the scan checked nothing.
	if !seen["IsRootedThreadKey"] {
		t.Fatalf("no internal/classify source calls slackweb.IsRootedThreadKey; criterion 14's thread_scope " +
			"decision must go through it, and an allowlist over zero uses proves nothing")
	}
}

// ---- the sanctioned carrier: tasks.source_thread_id, and nothing else --------

// The SPEC is explicit and names the alternative it rejects: "Do not invent a
// second provenance store, and do not reach for external_refs" — SWT-20
// rejected external_refs for exactly this, because it is agent-facing free
// text, its join key is mutable, and `UNIQUE (system, external_key)` allows one
// task per conversation FOREVER.
//
// GREEN TODAY, and its job is to stay green: internal/classify has never
// written a task-shaped row. The scan is here because this is the ticket that
// makes someone want to.
func TestClassify_RecordsProvenanceAndCarriesItNowhere(t *testing.T) {
	banned := []struct{ token, why string }{
		{"external_refs", "SWT-20 rejected it as the carrier: agent-facing free text, a mutable join key, " +
			"and UNIQUE (system, external_key) allows one task per conversation forever. The sanctioned " +
			"carrier is tasks.source_thread_id, written only by the task_set_source_thread spine tool"},
		{"classify_promotions", "invariant 2 and criterion 26: this lane creates nothing and claims nothing"},
		{"task_events", "criterion 26: classify.Store gains no task-write method"},
		{"deliveries", "invariant 4 and out-of-scope 2: no delivery row, no draft_delivery, no target_ref " +
			"construction. This ticket records the thread so a later one can aim; it aims nothing"},
		{"target_ref", "the SPEC forbids constructing a target_ref here — the thread capture is " +
			"PROVENANCE, not a target"},
	}
	for _, rel := range csSources(t, "internal/classify") {
		code := csGoCode(t, rel)
		for _, b := range banned {
			if strings.Contains(code, b.token) {
				t.Errorf("%s mentions %q in code (comments stripped) — %s", rel, b.token, b.why)
			}
		}
		// `tasks` is scanned as a SQL token rather than as a substring: "tasks"
		// appears inside plenty of harmless English, and the thing that matters
		// is a query against the table.
		if regexp.MustCompile(`(?i)(from|into|update|join)\s+tasks\b`).MatchString(code) {
			t.Errorf("%s queries the `tasks` table. Shadow is STRUCTURAL (criterion 26): going live ADDS "+
				"an executor create_task call in internal/promote, it does not remove a guard here", rel)
		}
	}
}

// ---- criterion 10: the filter carries NO ai_locality clause, and says why ----

func TestInquiryFilter_HasNoLocalityClauseAndTheReasonIsInTheComment(t *testing.T) {
	const rel = "internal/classify/store.go"
	src := csRepoFile(t, rel)

	i := strings.Index(src, "inboxWhereInquiry")
	if i < 0 {
		t.Fatalf("%s declares no inboxWhereInquiry. Criterion 9's filter is a named constant beside "+
			"inboxWhere and inboxWhereResidue, in the same comment-per-clause style that says what each "+
			"clause actually excludes", rel)
	}
	// TWO subjects, read separately. AMENDED during implementation (2026-09-10):
	// the first cut ran both checks below over one 2,600-byte window starting
	// at the doc comment, which made them contradict each other — the comment
	// that EXPLAINS the absent ai_locality clause necessarily names it, so the
	// "carries a clause" check fired on the explanation the "reason" check
	// requires. The clause is judged on the SQL literal; the reason on the doc
	// comment above the declaration.
	decl := strings.Index(src, "const inboxWhereInquiry")
	if decl < 0 {
		t.Fatalf("%s mentions inboxWhereInquiry but never declares `const inboxWhereInquiry`", rel)
	}
	open := strings.Index(src[decl:], "`")
	if open < 0 {
		t.Fatalf("%s's inboxWhereInquiry is not a raw string literal", rel)
	}
	litStart := decl + open + 1
	closing := strings.Index(src[litStart:], "`")
	if closing < 0 {
		t.Fatalf("%s's inboxWhereInquiry literal is unterminated", rel)
	}
	literal := src[litStart : litStart+closing]
	comment := src[i:decl]

	if !strings.Contains(literal, "ai_inquiry") {
		t.Errorf("%s's inboxWhereInquiry does not test p.ai_inquiry:\n%s", rel, literal)
	}
	if strings.Contains(literal, "ai_locality") {
		t.Errorf("%s's inboxWhereInquiry carries an ai_locality clause:\n%s\n"+
			"Criterion 10: it deliberately does NOT. `collaboratory` is ai_locality='any', so an "+
			"ai_locality='local_only' clause would return ZERO rows and the lane would be silently inert. "+
			"This lane's containment is cmd/classify's buildRouter passing general = nil — there is "+
			"nothing to fall back TO — plus criterion 11's pinned class, not the project column.",
			rel, literal)
	}
	// The REASON has to be in the comment, or the next reader "fixes" the
	// missing clause back in and empties the lane.
	lowered := strings.ToLower(comment)
	if !strings.Contains(lowered, "ai_locality") {
		t.Errorf("%s's inboxWhereInquiry comment never explains the ABSENT ai_locality clause. Criterion "+
			"10 puts the reason above the filter precisely because its absence looks like an omission: "+
			"the containment is the nil general lane plus the pinned class, and adding the clause back "+
			"would empty the lane on the one armed project", rel)
	}
	if !regexp.MustCompile(`buildrouter|general\s*=\s*nil|nil general`).MatchString(lowered) {
		t.Errorf("%s's inboxWhereInquiry comment does not name what DOES contain this lane (cmd/classify's "+
			"buildRouter passing general = nil, plus criterion 11). A comment that only says 'no locality "+
			"clause here' reads as an oversight", rel)
	}
}

// ---- criterion 11: the honesty label gains its THIRD reason -------------------

// SWT-22 criterion 13 wrote the label, SWT-23 criterion 15 re-stated it for two
// lanes, and this ticket adds the third. The mechanism is DIFFERENT again: the
// personal lane is restricted through the project column, the residue through
// ClassOf's non-AttrProject branch, and the inquiry lane because the LANE PINS
// the class — its messages would otherwise be ClassGeneral, which is precisely
// why the pin exists.
//
// Scanned over COMMENT TEXT ONLY, for the reason the SWT-23 half records: over
// whole files, `inquiry` is satisfied by the identifier LaneInquiry, so the
// label would read as present the moment the lane exists. The label is prose or
// it is nothing.
func TestPackage_HonestyLabelGainsTheInquiryReason(t *testing.T) {
	comments := strings.ToLower(csComments(t, "internal/classify"))

	if !strings.Contains(comments, "inquiry") {
		t.Errorf("no comment in internal/classify mentions the INQUIRY lane. Criterion 11: the honesty " +
			"label gains a THIRD reason, stated in prose. Left as it is, the label gives two mechanisms " +
			"for three lanes, and the one it omits is the only one that is a deliberate PIN rather than " +
			"an inherited property")
	}
	if !regexp.MustCompile(`pin|pinned`).MatchString(comments) {
		t.Errorf("the honesty label does not say the inquiry lane's class is PINNED. That word is the " +
			"whole distinction: the other two lanes are restricted by what their messages ARE, and this " +
			"one is restricted because the lane refuses to widen")
	}
	if !regexp.MustCompile(`neighbour|neighbor`).MatchString(comments) {
		t.Errorf("the honesty label does not carry criterion 11's neighbour-bodies sentence. The pin is " +
			"\"required twice over, because the prompt carries thread-NEIGHBOUR bodies as well as the " +
			"target's\" — a reader who does not know that sees a lane sending one client message to a " +
			"local model and wonders why the hosted lane is refused")
	}
	if !regexp.MustCompile(`classgeneral|ai_locality\s*=?\s*'?any`).MatchString(comments) {
		t.Errorf("the honesty label does not name what the messages' OWN class is (ClassGeneral, because " +
			"the armed project is ai_locality='any'). Without that sentence the pin reads as a downgrade " +
			"of something, and the next reader removes it")
	}
}

// ---- criterion 28: the runbook's Inquiry lane section ------------------------

func TestRunbook_InquiryLaneSection(t *testing.T) {
	const rel = "docs/runbooks/local-classifier.md"
	doc := csRepoFile(t, rel)
	lower := strings.ToLower(doc)

	// Control: the two shipped lanes' sections must still be there. A rewritten
	// runbook would satisfy every "mentions X" below while deleting what an
	// operator of the other two lanes reads.
	for _, keep := range []string{"--lane residue", "7.2"} {
		if !strings.Contains(lower, keep) {
			t.Fatalf("POSITIVE CONTROL FAILED: %s no longer contains %q. The Inquiry section is ADDED "+
				"beside the others; the residue section and its measured 7.2 s median stay", rel, keep)
		}
	}

	// The opening sentence learns there are three lanes (criterion 28's last
	// line; it currently says "TWO lanes since SWT-23" in the first paragraph,
	// which is the first thing an operator reads and is now false).
	head := doc
	if len(head) > 900 {
		head = head[:900]
	}
	if !strings.Contains(strings.ToLower(head), "inquiry") {
		t.Errorf("%s opens without mentioning the inquiry lane:\n%s\n"+
			"Criterion 28: the opening sentence learns there are THREE lanes. It currently says "+
			"\"`classify` runs TWO lanes since SWT-23\", which is the first thing an operator reads and "+
			"is false the moment this ticket merges", rel, head[:min(len(head), 400)])
	}

	for _, want := range []struct{ token, why string }{
		{"--lane inquiry", "the command itself; without it the section is prose about a flag nobody can spell"},
		{"classify_inquiry", "the worker_type — the value every psql spot-check and every NOT EXISTS keys on"},
		{"ai_inquiry", "the flag that arms a project, and how to disarm it (one UPDATE)"},
		{"thread_scope", "what the three values mean"},
		{"conversation", "and WHY `conversation` is a WEAKER claim than `thread`: a later outbound there " +
			"only means Salvador has spoken in the channel since"},
		{"by channel", "the by-channel breakdown (criterion 30) — the diagnostic that replaces the " +
			"rejected --channel flag"},
		{"t0hpr78rx", "the measured per-workspace outbound counts (criterion 12), which is what says the " +
			"fold's discriminating column is not a production constant"},
		{"2026-09-10", "…WITH the date they were measured on. An undated count is a count nobody can " +
			"re-derive, and this corpus is live"},
		{"base rate", "the number that decides this lane's future"},
		{"subject_sha256", "the Slack subject-hash limitation of criterion 25: slackweb sets a message's " +
			"subject to the CONVERSATION NAME, so every message in a channel shares a hash"},
	} {
		if !strings.Contains(lower, strings.ToLower(want.token)) {
			t.Errorf("%s never mentions %q — %s (criterion 28)", rel, want.token, want.why)
		}
	}

	// The required --since rule WITH its arithmetic (D6). A rule stated without
	// its number is a rule the next operator argues with.
	since := regexp.MustCompile(`(?s)--since.{0,400}(required|mandatory)|(required|mandatory).{0,400}--since`)
	if !since.MatchString(lower) {
		t.Errorf("%s does not state that --since is REQUIRED (criterion 28 + D6). An inquiry has a shelf "+
			"life of days, so an unbounded pass is GPU spent on messages nobody will answer", rel)
	}

	// Criterion 31(c): the eval row carries the MARKER, and it is the same
	// constant the binary prints.
	if !strings.Contains(doc, classify.EvalIndicativeMarker) {
		t.Errorf("%s's score table does not carry classify.EvalIndicativeMarker (%q) on the inquiry row. "+
			"Criterion 31(c): the marker goes in the runbook row and in any Jira comment, in the SAME "+
			"spelling the binary prints — a row of counts with no marker beside it is exactly the shape "+
			"that got quoted out of context before", rel, classify.EvalIndicativeMarker)
	}

	// Criterion 32: the labelling protocol, and it NAMES WHO LABELS.
	protocol := strings.ToLower(doc)
	for _, want := range []struct{ re, why string }{
		{`uniform`, "the uniform half of the remaining 80 — NOT OPTIONAL, because a set built only from " +
			"flagged output can measure precision and can never measure recall"},
		{`14 days|fortnight|two weeks`, "the starter 40 come from messages recent enough to confirm from " +
			"memory in seconds"},
		{`salvador|he |his `, "the protocol NAMES WHO LABELS: the judgement 'does Salvador need to answer " +
			"this' is HIS and cannot be delegated to an agent or inferred from a heuristic"},
	} {
		if !regexp.MustCompile(want.re).MatchString(protocol) {
			t.Errorf("%s's labelling protocol does not match /%s/ — %s (criterion 32)", rel, want.re, want.why)
		}
	}

	// The dated commitment to 120, spelled from the constant so the runbook and
	// the binary cannot disagree.
	if !strings.Contains(doc, strconv.Itoa(classify.EvalResultThreshold)) {
		t.Errorf("%s does not carry the dated commitment to %d stratified labels (uniform >= 80 + "+
			"enriched >= 40). Q3 shipped 40 ON THAT CONDITION; a commitment nobody wrote down is a "+
			"commitment nobody keeps", rel, classify.EvalResultThreshold)
	}
}

// ---- criterion 29: the institutional-knowledge entry, and the DATED fix ------

// Two halves, and the second is the one that cost a measurement. IK's SWT-12
// section says `T0HPR78RX` (Collaboratory/LlamaSite) has no OWN_USER_IDS entry
// and that connector-slackweb is suspended. That sentence was TRUE when written
// and is false as of 2026-09-10 — the workspace carries 2,155 inbound and 2,393
// outbound rows, latest outbound 2026-09-09. It is what produced the doubt that
// blocked Q1, and left standing it will produce it again in the next session
// that reads the replied-since fold.
func TestInstitutionalKnowledge_RecordsTheInquiryLaneAndCorrectsSWT12(t *testing.T) {
	const rel = ".claude/INSTITUTIONAL_KNOWLEDGE.md"
	doc := csRepoFile(t, rel)
	lower := strings.ToLower(doc)

	if !strings.Contains(doc, "SWT-33") {
		t.Fatalf("%s has no SWT-33 entry. Criterion 29 asks for a short 'Inquiry lane' section: agents "+
			"read this file at session start instead of re-deriving what it holds", rel)
	}
	for _, want := range []struct{ token, why string }{
		{"classify_inquiry", "the three lanes and their worker_types"},
		{"ai_inquiry", "ai_inquiry vs ai_classify vs ai_locality — a workload flag, a workload flag and " +
			"the boundary, and the SPEC spends a paragraph on why they are not one column"},
		{"classgeneral", "the pinned restricted class and WHY: ClassGeneral + a nil general client = skip " +
			"EVERY message, a no-op that exits 0 and reads as an empty inbox"},
		{"thread_scope", "the thread-scope split"},
		{"113", "…with the 113 thread-exact / 83 conversation-level measurement, DATED. One " +
			"conversation-level key holds 9,704 messages, which is why thread-level classification was " +
			"rejected"},
		{"advisory", "the SHARED advisory lock and its consequence for cadence: two lanes must never be " +
			"scheduled at the same minute, because runCmd treats losing the lock as an ERROR"},
	} {
		if !strings.Contains(lower, strings.ToLower(want.token)) {
			t.Errorf("the SWT-33 entry does not mention %q — %s (criterion 29)", want.token, want.why)
		}
	}

	// ---- the dated in-place correction of the stale SWT-12 line -------------

	// Control: the original sentence must STILL BE THERE. Criterion 29 says
	// correct it in place, do NOT delete it — the sentence was true when
	// written, and the record of when it stopped being true is the useful
	// artefact. A deletion would satisfy every "no longer says X" test while
	// destroying exactly that.
	if !strings.Contains(doc, "T0HPR78RX") {
		t.Fatalf("%s no longer mentions T0HPR78RX at all. Criterion 29: the stale line is CORRECTED IN "+
			"PLACE with a date, not deleted", rel)
	}
	i := strings.Index(doc, "T0HPR78RX")
	end := i + 1400
	if end > len(doc) {
		end = len(doc)
	}
	window := doc[i:end]
	// The window starts at the SWT-12 occurrence (the file's first), which is
	// the sentence under correction.
	if !strings.Contains(window, "2026-09-10") {
		t.Errorf("%s's SWT-12 line about T0HPR78RX carries no 2026-09-10 correction:\n%s\n"+
			"Criterion 29: \"`T0HPR78RX` has no `OWN_USER_IDS` entry\" was true when written and is FALSE "+
			"as of 2026-09-10 (inbound 2,155 / outbound 2,393, latest outbound 2026-09-09). An undated "+
			"amendment cannot be ordered against the sentence it corrects, and this exact line is what "+
			"produced the doubt that made Q1 blocking.", rel, window[:min(len(window), 700)])
	}
	if !regexp.MustCompile(`(?i)outbound`).MatchString(window) {
		t.Errorf("%s's correction does not say what the new fact IS (the workspace carries outbound rows). "+
			"A correction that only says 'this is out of date' leaves the reader exactly where the stale "+
			"sentence did", rel)
	}
	// And WHAT the correction is load-bearing FOR, so the next reader knows why
	// somebody bothered.
	if !regexp.MustCompile(`(?i)inquiry|replied[- ]since|fold`).MatchString(window) {
		t.Errorf("%s's correction does not say what it is load bearing FOR (the inquiry lane's "+
			"replied-since fold, which rests entirely on direction='outbound' existing for this "+
			"workspace). Criterion 29 asks for that sentence by name", rel)
	}
}
