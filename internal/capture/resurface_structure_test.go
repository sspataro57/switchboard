package capture

// Structural tests for chat-on-closed-task (SWT-53,
// docs/tickets/chat-on-closed-task_SPEC.md) on the capture side:
//
//   - criterion 1: migration 0034 is this ticket's only migration, and the
//     living ledger in internal/classify/structure_test.go accepts 34;
//   - criterion 2: 0034's shape (two columns, the named CHECK, no index, no
//     backfill, no arming UPDATE; the comment names CC3, CC4 and CC10);
//   - criterion 3: resurface.go is pure (the rules_structure_test.go shape);
//   - criterion 4: senderAddress lives beside senderDomain, which uses it;
//   - criterion 5's anchors: loadRules selects p.notifier_senders and
//     insertDecision writes resurface;
//   - criterion 9: RulesStats.Resurfaced exists and every capture counter line
//     prints "resurfaced" (opsctl's gate line as a constant 0);
//   - criterion 10: projects.notifier_senders is read by rules_store.go only;
//   - criteria 17, 18, 19: the capture-rules runbook section, the kube handoff
//     and the IK entry.
//
// ZERO I/O beyond reading this repo. Reuses mustReadRepoFile
// (rules_structure_test.go) and rvFuncSrc (revive_structure_test.go).
//
// RED TODAY: the package's unit binary does not compile until resurface.go
// exists (resurface_test.go). Once it compiles, each guard below fails on its
// own sentence until the matching file or text lands.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---- criterion 3: resurface.go is pure -------------------------------------------

func TestResurfaceGo_IsPure(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/resurface.go")
	for _, fn := range []string{"func resurfaces(", "func notifierSender("} {
		if !strings.Contains(src, fn) {
			t.Errorf("internal/capture/resurface.go does not declare %s — criteria 3 and 4 pin both pure "+
				"functions to this file, because the file is what makes the purity checkable", strings.TrimSuffix(fn, "("))
		}
	}
	for _, b := range []struct{ token, why string }{
		{`"context"`, "a pure decision takes no context"},
		{"pgx", "no database: invariant 7; the status, dismissal and list arrive as values"},
		{"internal/connector/", "the connector-copy fact is computed in rules_store.go and passed in as a bool (CC3)"},
		{"internal/provider", "no model"},
		{"net/http", "no network"},
		{`"net/mail"`, "the From parse lives in senderdomain.go (senderAddress), the one net/mail spelling (CC4)"},
		{"os.Getenv", "the notifier list is a COLUMN (projects.notifier_senders), never the environment"},
		{"regexp", "equality, never a pattern: a regex over the sender is the substring match CC4 refuses"},
	} {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/capture/resurface.go mentions %q — %s", b.token, b.why)
		}
	}
}

// ---- criterion 4: one From parser -------------------------------------------------

func TestSenderAddress_LivesBesideSenderDomainAndIsItsParser(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/senderdomain.go")
	if !strings.Contains(src, "func senderAddress(") {
		t.Fatalf("internal/capture/senderdomain.go does not declare senderAddress. CC4: the address is parsed " +
			"by a new helper BESIDE senderDomain, one net/mail spelling")
	}
	body := rvFuncSrc(src, "senderDomain")
	if body == "" {
		t.Fatalf("internal/capture/senderdomain.go no longer declares senderDomain")
	}
	if !strings.Contains(body, "senderAddress(") {
		t.Errorf("senderDomain does not call senderAddress. CC4: senderDomain is refactored ONTO it, so the two "+
			"cannot parse the same From two ways.\nbody:\n%s", body)
	}
	// And no other file in the package PARSES a From with net/mail.
	//
	// AMENDED at the SWT-53/SWT-54 merge, not deleted: this first banned the
	// net/mail IMPORT outright. SWT-54 (treetop-pr-review-tasks) legitimately
	// imports it in prreview.go / prreview_store.go for the mail.Header TYPE (the
	// stored RFC822 header map its origin check reads), which parses no From.
	// K6's rule is "one Go-side From parser", so the scan now bans the From
	// parsers themselves (mail.ParseAddress, mail.ParseAddressList,
	// mail.AddressParser) in any net/mail importer other than senderdomain.go.
	fromParsers := []string{"mail.ParseAddress", "mail.AddressParser"} // ParseAddress also prefixes ParseAddressList
	// CONTROL: the token list names what senderdomain.go really calls, so the
	// scan below cannot pass vacuously on a misspelled token.
	hit := false
	for _, tok := range fromParsers {
		hit = hit || strings.Contains(src, tok)
	}
	if !hit {
		t.Fatalf("CONTROL: senderdomain.go calls none of %v; the From-parser scan below would pass vacuously", fromParsers)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/capture: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "senderdomain.go" {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), n, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, spec := range f.Imports {
			if p, _ := strconv.Unquote(spec.Path.Value); p != "net/mail" {
				continue
			}
			fileSrc := mustReadRepoFile(t, "internal/capture/"+n)
			for _, tok := range fromParsers {
				if strings.Contains(fileSrc, tok) {
					t.Errorf("internal/capture/%s calls %s. K6: senderdomain.go is the one Go-side From parser "+
						"(senderAddress / senderDomain); call those instead", n, tok)
				}
			}
		}
	}
}

// ---- criterion 5's anchors: the column is selected and the flag is written ---------

func TestCaptureRules_LoadRulesSelectsTheNotifierColumnAndInsertDecisionWritesResurface(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	load := rvFuncSrc(src, "loadRules")
	if load == "" {
		t.Fatalf("rules_store.go no longer declares loadRules")
	}
	if !strings.Contains(load, "p.notifier_senders") {
		t.Errorf("loadRules does not select p.notifier_senders. CC4: the list is read ONLY by loadRules, joined " +
			"with the rules like ticket_assignee_gate, into storedRule.notifiers")
	}
	ins := rvFuncSrc(src, "insertDecision")
	if ins == "" {
		t.Fatalf("rules_store.go no longer declares insertDecision")
	}
	if !regexp.MustCompile(`\bresurface\b`).MatchString(ins) {
		t.Errorf("insertDecision does not write capture_decisions.resurface. CC3: capture RECORDS the fact; the " +
			"lanes only read it")
	}
}

// ---- criterion 9: the counter --------------------------------------------------------

func TestRulesStats_HasResurfaced(t *testing.T) {
	f, ok := reflect.TypeOf(RulesStats{}).FieldByName("Resurfaced")
	if !ok {
		t.Fatalf("RulesStats has no Resurfaced field. CC7: it counts decisions written with resurface=true, in " +
			"BOTH modes (the Deferred/Blind precedent: a recorded fact, not an action)")
	}
	if f.Type.Kind() != reflect.Int {
		t.Errorf("RulesStats.Resurfaced is %s, want int (every RulesStats counter is an int)", f.Type)
	}
}

const (
	rsfAppendedFmt   = `\"appended\":%d`
	rsfResurfacedFmt = `\"resurfaced\":%d`
)

// captureCounterFormats returns the text of every capture counter FORMAT in
// src: from the Printf( that opens it to the `}\n"` that ends it, for each
// occurrence of "appended":%d. Scoped to the format itself, never the file:
// cmd/connectors/jira/main.go also prints the RECONCILER's line, which carries
// its own "resurfaced" counter (SWT-45's last_action), and a whole-file check
// would pass there without the capture line printing it.
func captureCounterFormats(src string) []string {
	var out []string
	from := 0
	for {
		i := strings.Index(src[from:], rsfAppendedFmt)
		if i < 0 {
			return out
		}
		at := from + i
		start := strings.LastIndex(src[:at], "Printf(")
		if start < 0 {
			start = at
		}
		end := strings.Index(src[at:], `}\n"`)
		if end < 0 {
			end = len(src) - at
		}
		out = append(out, src[start:at+end])
		from = at + len(rsfAppendedFmt)
	}
}

// "Every capture counter line prints "resurfaced": cmd/connectors/{jira,
// slackweb,upworkcrm,google}/main.go, cmd/connectors/google/watch.go, and
// cmd/opsctl/main.go's capture-rules run. cmd/opsctl/gate.go prints it as
// constant 0. The structural counter scan fails a main without it." The
// printers are found by what they already print ("appended"), so a new one is
// held to the same line; watch.go prints through google/main.go's
// printCaptureRules and is held to calling it.
func TestCaptureCounterLines_PrintResurfaced(t *testing.T) {
	// Control for the scoping: a file whose capture line lacks the counter but
	// whose reconciler line carries one must still be flagged.
	probe := "fmt.Printf(\"capture_rules: {" + rsfAppendedFmt + `,\"blind\":%d}\n"` + ", a, b)\n" +
		"fmt.Printf(\"ticket_status: {" + rsfResurfacedFmt + `}\n"` + ", c)\n"
	if f := captureCounterFormats(probe); len(f) != 1 || strings.Contains(f[0], rsfResurfacedFmt) {
		t.Fatalf("CONTROL: captureCounterFormats(%q) = %q; it must return the capture line alone, without the "+
			"reconciler line's \"resurfaced\"", probe, f)
	}

	var printers []string
	err := filepath.Walk(filepath.Join("..", "..", "cmd"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		formats := captureCounterFormats(string(b))
		if len(formats) == 0 {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		printers = append(printers, rel)
		for _, f := range formats {
			if !strings.Contains(f, rsfResurfacedFmt) {
				t.Errorf("%s prints the capture counters without \\\"resurfaced\\\":%%d in the SAME format. "+
					"Criterion 9 / CC7: zeros included — Verification 4 reads \"resurfaced\":N on every "+
					"capture_rules: line", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	for _, want := range []string{
		"cmd/connectors/jira/main.go", "cmd/connectors/slackweb/main.go", "cmd/connectors/upworkcrm/main.go",
		"cmd/connectors/google/main.go", "cmd/opsctl/main.go", "cmd/opsctl/gate.go",
	} {
		found := false
		for _, p := range printers {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s no longer prints the capture counter line (\"appended\":%%d); the scan cannot hold it to "+
				"criterion 9. Found printers: %v", want, printers)
		}
	}

	// watch.go prints through google/main.go's printer, so it carries the same line.
	watch := mustReadRepoFile(t, "cmd/connectors/google/watch.go")
	if !strings.Contains(watch, "printCaptureRules(") {
		t.Errorf("cmd/connectors/google/watch.go no longer calls printCaptureRules; it must print the SAME capture " +
			"counter line as google/main.go (criterion 9 names watch.go)")
	}

	// The gate line prints it as a CONSTANT 0: the gate path never resurfaces (CC8).
	gate := mustReadRepoFile(t, "cmd/opsctl/gate.go")
	if strings.Contains(gate, ".Resurfaced") {
		t.Errorf("cmd/opsctl/gate.go reads a Resurfaced counter. CC8: the gate path writes resurface=false, so " +
			"its line prints \"resurfaced\" as constant 0, like revived/surfaced_created")
	}
	constZero := regexp.MustCompile(`(?m)^\s*const\s+([^=\n]*\bresurfaced\b[^=\n]*)=\s*([0,\s]+)$`)
	if !constZero.MatchString(gate) {
		t.Errorf("cmd/opsctl/gate.go declares no `const ... resurfaced ... = 0` (the revived/surfacedCreated/" +
			"deferred/blind precedent). Criterion 9: the gate prints it as constant 0")
	}
}

// ---- criterion 10: projects.notifier_senders has ONE reader ------------------------

func TestNotifierSenders_IsReadByCaptureRulesStoreOnly(t *testing.T) {
	const column = "notifier_senders"
	const allowed = "internal/capture/rules_store.go"
	found := false
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join("..", "..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !strings.Contains(string(b), column) {
				return nil
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if rel != allowed {
				t.Errorf("%s names %s. Criterion 10: projects.notifier_senders is read by NO non-test Go file "+
					"except %s (loadRules). A second reader is a second, divergent answer to 'is this sender a "+
					"notifier' (the SWT-34 criterion 22 precedent)", rel, column, allowed)
			} else {
				found = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", root, err)
		}
	}
	if !found {
		t.Errorf("%s does not read %s — CC4: loadRules selects it into storedRule.notifiers", allowed, column)
	}
}

// ---- criteria 1 and 2: migration 0034 ------------------------------------------------

// Its own guard, because letting 34 into the ledger cannot fail before the
// file exists. It asserts "exactly one 0034" and deliberately NOT "nothing
// above 0034": SWT-54 (treetop-pr-review-tasks) takes 0035 on another branch.
func TestMigration0034_ChatOnClosedTaskShape(t *testing.T) {
	dir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(dir, "0033_task_working_state.sql")); err != nil {
		t.Fatalf("control: migrations/0033_task_working_state.sql is missing (%v); 0034 follows it", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "0034_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0034_*.sql: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != "0034_chat_on_closed_task.sql" {
		var names []string
		for _, m := range matches {
			names = append(names, filepath.Base(m))
		}
		t.Fatalf("migrations/0034_*.sql = %v, want exactly [0034_chat_on_closed_task.sql]. Criterion 1: this "+
			"ticket's ONLY migration (SWT-54 takes 0035)", names)
	}
	raw := mustReadRepoFile(t, "migrations/0034_chat_on_closed_task.sql")

	for _, cc := range []string{"CC3", "CC4", "CC10"} {
		if !regexp.MustCompile(`\b` + cc + `\b`).MatchString(raw) {
			t.Errorf("0034's comment does not name %s. Criterion 2: the comment names CC3 (capture records the "+
				"fact), CC4 (the notifier list, equality) and CC10 (0034 BEFORE any image from this branch)", cc)
		}
	}

	var code []string
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	sql := strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(code, " "), " "))
	sql = strings.ReplaceAll(strings.ReplaceAll(sql, "( ", "("), " )", ")")
	sql = regexp.MustCompile(`\s*=\s*`).ReplaceAllString(sql, " = ")
	sql = regexp.MustCompile(`\s*\[\s*\]`).ReplaceAllString(sql, "[]")

	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?projects\b[^;]*add column (if not exists )?notifier_senders text\[\] not null default '\{\}'`,
			"projects.notifier_senders TEXT[] NOT NULL DEFAULT '{}' (CC4: the 0025 shape; '{}' is today's behaviour, " +
				"NOT NULL because of 0018's nullable trap)"},
		{`alter table (public\.)?capture_decisions\b[^;]*add column (if not exists )?resurface boolean not null default false`,
			"capture_decisions.resurface BOOLEAN NOT NULL DEFAULT false (CC3)"},
		{`constraint capture_decisions_resurface_is_task_log check \(+\s*not resurface or action = 'task_log'\s*\)+`,
			"the NAMED CHECK capture_decisions_resurface_is_task_log (NOT resurface OR action = 'task_log')"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("0034 does not match /%s/ — %s", want.re, want.why)
		}
	}
	for _, banned := range []struct{ re, why string }{
		{`create (unique )?index`, "no index: projects is tens of rows by primary key; resurface is read per latest row"},
		{`\bupdate\b`, "no backfill (CC9) and no arming UPDATE: seeding notifier_senders is an operator act (CC10)"},
		{`insert into`, "no seeding"},
		{`delete from`, "nothing deleted"},
		{`create table`, "no new table (invariant 2)"},
		{`\bdrop\b`, "forward-only"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("0034 matches /%s/ — %s", banned.re, banned.why)
		}
	}
}

// The ledger (internal/classify/structure_test.go) must own 34 with the note
// "34 is chat-on-closed-task" ABOVE the marker. GREEN guard: the ledger was
// taught 34 in the same change as this file. It does not refuse 35: SWT-54
// adds its own line when it lands, whichever branch merges second keeps both.
func TestMigrationLedger_Learns0034(t *testing.T) {
	src := mustReadRepoFile(t, "internal/classify/structure_test.go")
	i := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if i < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	above := src[max(i-3000, 0):i]
	if !strings.Contains(above, "34 is chat-on-closed-task") {
		t.Errorf("the ledger has no ownership note \"34 is chat-on-closed-task\" ABOVE the marker (criterion 1)")
	}
	pred := regexp.MustCompile(`if\s+n\s*>\s*17(\s*&&\s*n\s*!=\s*\d+)+`).FindString(src[i:min(i+1800, len(src))])
	if pred == "" {
		t.Fatalf("the ledger's `if n > 17 && n != …` predicate is gone from below the marker")
	}
	if !regexp.MustCompile(`n\s*!=\s*34\b`).MatchString(pred) {
		t.Errorf("the ledger predicate %q does not accept 34; 0034_chat_on_closed_task.sql would be flagged unowned", pred)
	}
}

// ---- criterion 17: the capture-rules runbook -------------------------------------------

func TestRunbook_DocumentsClosedTaskChatsResurface(t *testing.T) {
	doc := strings.ToLower(mustReadRepoFile(t, "docs/runbooks/capture-rules.md"))
	i := strings.Index(doc, "closed-task chats resurface (chat-on-closed-task)")
	if i < 0 {
		t.Fatalf("docs/runbooks/capture-rules.md has no \"Closed-task chats resurface (chat-on-closed-task)\" " +
			"section (criterion 17)")
	}
	section := doc[i:]
	if j := strings.Index(section[1:], "\n## "); j > 0 {
		section = section[:j+1]
	}
	for _, want := range []struct{ frag, why string }{
		{"resurface", "CC3: capture_decisions.resurface, capture's recorded fact"},
		{"task_log", "the case: a task_log onto a CLOSED task"},
		{"closed", "only a closed task"},
		{"revive", "the exclusions: an activity (revive) match stays SWT-45's"},
		{"dismiss", "the exclusions: an open dismissal stays SWT-36's"},
		{"notifier_senders", "CC4: the notifier list"},
		{"equal", "CC4: the match is EQUALITY"},
		{"substring", "CC4: never a substring (the sender criterion is one; the difference is the point)"},
		{"update projects set notifier_senders", "the seed SQL"},
		{"select", "the read SQL"},
		{"notifier_senders = '{}'", "the disarm SQL (and why it is not a rollback)"},
		{"0034", "CC10: 0034 BEFORE any image from this branch"},
		{"image", "CC10: the image roll comes after the migration and the seed"},
		{"before", "CC10's order is an order"},
		{"gate", "CC8: the gate path's residual (resurface=false)"},
	} {
		if !strings.Contains(section, want.frag) {
			t.Errorf("the runbook's chat-on-closed-task section never mentions %q — %s", want.frag, want.why)
		}
	}
}

// ---- criterion 18: the kube handoff, in order -----------------------------------------

func TestHandoff_ChatOnClosedTaskStatesTheOrder(t *testing.T) {
	const rel = "docs/runbooks/HANDOFF-kube-chat-on-closed-task.md"
	doc := strings.ToLower(mustReadRepoFile(t, rel))
	steps := []struct{ frag, why string }{
		{"0034_chat_on_closed_task.sql", "1. the 0034 migrate Job FIRST (CC10: new capture binaries select " +
			"p.notifier_senders and write resurface; on a db without 0034 every capture pass fails)"},
		{"update projects set notifier_senders", "2. the seed UPDATE, after 0034 and BEFORE the images, so the " +
			"first new pass already excludes the Jira app"},
		{"pipelined", "3. one image tag to every connector CronJob and pipelined"},
		{"go install ./cmd/opsctl", "4. reinstall opsctl wherever it runs hand passes"},
	}
	at := 0
	for _, s := range steps {
		j := strings.Index(doc[at:], s.frag)
		if j < 0 {
			t.Errorf("%s does not mention %q after the previous step — %s", rel, s.frag, s.why)
			continue
		}
		at += j + len(s.frag)
	}
	if !strings.Contains(doc, "cronjob") {
		t.Errorf("%s never names the connector CronJobs (every one of them runs capture)", rel)
	}
}

// ---- criterion 19: the IK entry -----------------------------------------------------------

func TestInstitutionalKnowledge_RecordsChatOnClosedTask(t *testing.T) {
	doc := mustReadRepoFile(t, ".claude/INSTITUTIONAL_KNOWLEDGE.md")
	i := strings.Index(strings.ToLower(doc), "chat-on-closed-task")
	if i < 0 {
		t.Fatalf("the IK has no chat-on-closed-task entry (criterion 19)")
	}
	entry := strings.ToLower(doc[i:min(i+6000, len(doc))])
	for _, want := range []struct{ frag, why string }{
		{"resurface", "resurface is capture's recorded fact, and the lanes only read it"},
		{"notifier_senders", "the notifier list"},
		{"equality", "notifier matching is EQUALITY"},
		{"substring", "... never substring"},
		{"0034", "the 0034-before-image landmine"},
		{"image", "the 0034-before-image landmine"},
		{"gate", "the gate residual (CC8)"},
		{"prefix", "rule 10's prefix keying (K3/K4) is why a reopen is wrong"},
		{"reopen", "... why a reopen is wrong"},
	} {
		if !strings.Contains(entry, want.frag) {
			t.Errorf("the IK chat-on-closed-task entry never mentions %q — %s", want.frag, want.why)
		}
	}
}
