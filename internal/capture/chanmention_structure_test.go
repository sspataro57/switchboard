package capture

// slack-channel-mentions (Jira SWT-79, docs/tickets/slack-channel-mentions_SPEC.md),
// the STRUCTURAL half: criterion 13 (the migration file and both living
// ledgers), criterion 14 (bulk untouched) and criterion 3's purity. ZERO I/O
// beyond reading this repo's own files. The applied schema is
// TestSWT79_Migration0043_Integration_Applied.
//
// EXPECTED RED TODAY: migrations/0043_slack_channel_mentions.sql does not exist,
// neither ledger owns 43, and no pure channelUnmentioned predicate exists.
//
// MUTATIONS: drop the CHECK or its name → Migration0043Shape; leave ai_locality
// to its default (local_only) → Migration0043Shape; arm the project in the
// migration (inquiry_promote_after = now()) → Migration0043Shape; UPDATE bulk →
// Migration0043Shape.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const chanMentionMigration = "0043_slack_channel_mentions.sql"

// chmSplitTop splits a SQL list on commas at depth 0, outside single quotes.
func chmSplitTop(s string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// chmMatchParen returns the index of the ')' closing the '(' at open.
func chmMatchParen(s string, open int) int {
	depth, quoted := 0, false
	for i := open; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func TestMigration0043_SlackChannelMentionsShape(t *testing.T) {
	dir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(dir, "0042_slack_send_queue.sql")); err != nil {
		t.Fatalf("control: migrations/0042_slack_send_queue.sql is missing (%v); 0043 follows it", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "0043_*.sql"))
	if err != nil {
		t.Fatalf("glob migrations/0043_*.sql: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != chanMentionMigration {
		var names []string
		for _, m := range matches {
			names = append(names, filepath.Base(m))
		}
		t.Fatalf("migrations/0043_*.sql = %v, want exactly [%s] (the SPEC's \"Data model changes\": the column, "+
			"its named CHECK and the a-millon project, forward-only)", names, chanMentionMigration)
	}

	raw := mustReadRepoFile(t, "migrations/"+chanMentionMigration)
	var code []string
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	sql := strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(code, " "), " "))
	sql = strings.ReplaceAll(strings.ReplaceAll(sql, "( ", "("), " )", ")")
	sql = regexp.MustCompile(`\s*,\s*`).ReplaceAllString(sql, ",")

	for _, want := range []struct{ re, why string }{
		{`alter table (public\.)?capture_decisions\b`, "D2: the fact is a column on capture's own decision row"},
		{`add column (if not exists )?channel_unmentioned boolean not null default false`,
			"D2: BOOLEAN NOT NULL DEFAULT false — every writer that does not name it (gate, route, pre-deploy rows, " +
				"an old binary) records \"eligible, as today\", so the migration alone changes no behaviour"},
		{`constraint capture_decisions_channel_unmentioned_is_attributed check ?\(not channel_unmentioned or action ?= ?'attributed'\)`,
			"D2: the named CHECK (NOT channel_unmentioned OR action = 'attributed'), the 0034 resurface precedent"},
		{`insert into (public\.)?projects ?\(`, "D6: the a-millon project row, the 0016/0018 precedent"},
		{`on conflict ?\(slug\) do nothing`, "D6: ON CONFLICT (slug) DO NOTHING — a re-run never overwrites an " +
			"operator's later change"},
	} {
		if !regexp.MustCompile(want.re).MatchString(sql) {
			t.Errorf("%s does not match /%s/ — %s\n\ngot: %s", chanMentionMigration, want.re, want.why, sql)
		}
	}
	for _, banned := range []struct{ re, why string }{
		{`\bupdate\b`, "criteria 13/14: no UPDATE — bulk and every other project are untouched, and arming is the " +
			"D7 hand UPDATE at go-live, never the migration"},
		{`'bulk'`, "criterion 14: the migration does not name bulk"},
		{`delete from`, "nothing deleted"},
		{`\bdrop\b`, "forward-only"},
		{`create unique index`, "no unique index; the one allowed index is raw_source_items_ingested_at_idx " +
			"(review amendment: recheckEditedMentions starts from re-ingested raw items every pass)"},
		{`create table`, "no new table"},
		{`capture_rules`, "the rule swap is data through the audited tools (D8), not the migration"},
	} {
		if regexp.MustCompile(banned.re).MatchString(sql) {
			t.Errorf("%s matches /%s/ — %s", chanMentionMigration, banned.re, banned.why)
		}
	}
	if !strings.Contains(sql, "if not exists") && !strings.Contains(sql, "do $$") {
		t.Errorf("%s has neither IF NOT EXISTS nor a DO $$ guard; criterion 13: it applies twice cleanly", chanMentionMigration)
	}
	if !strings.Contains(strings.ToLower(raw), "before any image") {
		t.Errorf("%s's header does not say to apply it BEFORE any image built from this branch (the 0034 style; "+
			"a new capture binary writes the column and both inboxes select it)", chanMentionMigration)
	}

	// D6's table: every column named EXPLICITLY, with its value.
	i := regexp.MustCompile(`insert into (public\.)?projects ?\(`).FindStringIndex(sql)
	if i == nil {
		return
	}
	colsOpen := i[1] - 1
	colsClose := chmMatchParen(sql, colsOpen)
	vi := strings.Index(sql[colsClose:], "values")
	if colsClose < 0 || vi < 0 {
		t.Fatalf("cannot read the projects INSERT's column/VALUES lists:\n%s", sql[i[0]:])
	}
	valsOpen := colsClose + vi + strings.Index(sql[colsClose+vi:], "(")
	valsClose := chmMatchParen(sql, valsOpen)
	cols := chmSplitTop(sql[colsOpen+1 : colsClose])
	vals := chmSplitTop(sql[valsOpen+1 : valsClose])
	if len(cols) != len(vals) {
		t.Fatalf("the projects INSERT names %d columns and %d values:\n%v\n%v", len(cols), len(vals), cols, vals)
	}
	row := map[string]string{}
	for k, c := range cols {
		row[c] = vals[k]
	}
	for col, want := range map[string]struct {
		re, why string
	}{
		"name":             {`^'#a-millon \(avviato general\)'$`, "display only"},
		"slug":             {`^'a-millon'$`, "the channel's name (D6)"},
		"client":           {`^null$`, "load-bearing (0016): task_get_next's p.client = $1 keeps worker consoles off it"},
		"execution":        {`^'manual'$`, "the default, stated so it is reviewed"},
		"delivery":         {`^'dashboard'$`, "the default, stated so it is reviewed"},
		"ai_locality":      {`^'any'$`, "EXPLICIT 'any' — the default local_only would silently make the drafts lane skip it"},
		"ai_classify":      {`^false$`, "the personal lane is mail"},
		"ai_inquiry":       {`^true$`, "the workload flag (0024 precedent); inert until a rule attributes here"},
		"notifier_senders": {`^'\{\}'(::text\[\])?$`, "'{}' (pre-check 0h: no bots in #a-millon)"},
	} {
		got, ok := row[col]
		if !ok {
			t.Errorf("the a-millon INSERT does not name %s; D6 names every column explicitly (%s)", col, want.why)
			continue
		}
		if !regexp.MustCompile(want.re).MatchString(got) {
			t.Errorf("the a-millon INSERT sets %s = %s, want /%s/ — %s", col, got, want.re, want.why)
		}
	}
	if v, ok := row["inquiry_promote_after"]; ok && v != "null" {
		t.Errorf("the a-millon INSERT sets inquiry_promote_after = %s; D6/D7: NULL — armed by hand at go-live, and no "+
			"test DB is ever armed by a migration", v)
	}
}

// Both living ledgers must OWN 43, or `ls migrations/` grows a file no SPEC
// accounts for (the migrate runner keys on version with NO checksum). The
// classify note sits ABOVE the "34 is" line: internal/capture's 0034 guard
// reads only the 3000 chars above the marker (the 0042 note's reason).
func TestMigrationLedger_Learns0043(t *testing.T) {
	src := mustReadRepoFile(t, "internal/classify/structure_test.go")
	marker := strings.Index(src, "THIS LEDGER IS THE LIVING REGISTRY")
	if marker < 0 {
		t.Fatalf("the migration ledger marker is gone; REWRITE the guard, never delete it")
	}
	note := strings.Index(src, "// 43 is ")
	line34 := strings.Index(src, "// 34 is chat-on-closed-task")
	if line34 < 0 {
		t.Fatalf("the ledger's \"34 is chat-on-closed-task\" note is gone")
	}
	switch {
	case note < 0:
		t.Errorf("the ledger has no ownership note \"// 43 is …\" (slack-channel-mentions, SWT-79, %s)", chanMentionMigration)
	case note > line34:
		t.Errorf("the ledger's 43 note sits BELOW the \"34 is\" line; it belongs ABOVE it (the 0042 note's reason: " +
			"the 0034 guard reads only the 3000 chars above the marker)")
	case !strings.Contains(src[note:line34], "SWT-79"):
		t.Errorf("the ledger's 43 note does not name SWT-79")
	}
	pred := regexp.MustCompile(`if\s+n\s*>\s*17(\s*&&\s*n\s*!=\s*\d+)+`).FindString(src[marker:min(marker+2500, len(src))])
	if pred == "" {
		t.Fatalf("the ledger's `if n > 17 && n != …` predicate is gone from below the marker")
	}
	if !regexp.MustCompile(`n\s*!=\s*43\b`).MatchString(pred) {
		t.Errorf("the ledger predicate %q does not accept 43; migrations/%s would be flagged unowned", pred, chanMentionMigration)
	}

	sig := mustReadRepoFile(t, "internal/tools/signal_structure_test.go")
	if !regexp.MustCompile(`v\s*!=\s*43\b`).MatchString(sig) {
		t.Errorf("TestMigration0033_TaskWorkingStateShape's exemption list does not include 43; it flags every " +
			"migration above 33 that no ticket claims there")
	}
	if !strings.Contains(sig, "slack-channel-mentions") {
		t.Errorf("internal/tools/signal_structure_test.go's owner list does not name 43: slack-channel-mentions")
	}
}

// Criterion 3: the fact is a PURE, reason-bearing predicate beside direct.go
// (the SPEC's example name: channelUnmentioned(in) (bool, string)), with no
// I/O tokens in its file (invariant 7; the direct.go / resurface.go / comm.go
// precedent). Its behaviour is table-tested through decideMessage in
// chanmention_test.go, which compiles today.
func TestChannelUnmentionedPredicate_IsPure(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	decl := regexp.MustCompile(`func channelUnmentioned\([^)]*\)\s*\(bool,\s*string\)`)
	var file, src string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		if decl.Match(b) {
			file, src = n, string(b)
		}
	}
	if file == "" {
		t.Fatalf("no file in internal/capture declares `func channelUnmentioned(in …) (bool, string)`. Criterion 3: " +
			"a pure, reason-bearing predicate beside direct.go (e.g. internal/capture/mention.go)")
	}
	for _, banned := range []string{`"context"`, `"github.com/jackc/pgx`, `"os"`, `"time"`, `"net/`, "pgxpool"} {
		if strings.Contains(src, banned) {
			t.Errorf("%s (the channelUnmentioned predicate's file) contains %s; the predicate is pure — its inputs "+
				"arrive as values from rules_store.go (invariant 7)", file, banned)
		}
	}
}
