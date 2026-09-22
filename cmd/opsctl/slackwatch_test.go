package main

// slack-watch-sweep (SWT-75) criterion 4's CLI half: `opsctl slack-watch
// add|list|disable` drives exactly the three executor tools and prints the row.
// The writes go through slack_watch_add / slack_watch_set_enabled (humanOnly,
// audited as opsctl:$USER) — never a direct INSERT or UPDATE here, which is
// invariant 3 and also the only reason `audit_events` can answer "who pointed
// the browser at this conversation".
//
// A source scan: opsctl has no other test harness (the route-candidates and
// pr-review flag tests are the same shape).
//
// This is the seeding path the SPEC's "Usable alone means" section uses, so it
// is on the critical path of the whole ticket — the migration seeds NO rows on
// purpose (production ids are not frozen literals, IK), and Salvador names the
// conversations with these commands after pre-check 0a.
//
// GREENFIELD NOTE — EXPECTED RED: main.go has no slack-watch command.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func opsctlSources(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(raw)
	}
	return b.String()
}

func TestOpsctl_SlackWatchCommand(t *testing.T) {
	src := opsctlSources(t)
	if !strings.Contains(src, `"capture-rules"`) {
		t.Fatalf("POSITIVE CONTROL FAILED: the opsctl sources no longer dispatch capture-rules, so this scan " +
			"is not reading the command table")
	}
	if !strings.Contains(src, `"slack-watch"`) {
		t.Errorf("opsctl does not dispatch a \"slack-watch\" command (criterion 4). It mirrors `opsctl " +
			"capture-rules`, which is the SPEC's named precedent for configuration that must stay off the " +
			"agent surface")
	}
	if !regexp.MustCompile(`usage: opsctl <[^>]*slack-watch`).MatchString(src) {
		t.Errorf("opsctl's top-level usage line does not list slack-watch")
	}
	if !regexp.MustCompile(`usage: opsctl slack-watch <add\|list\|disable>`).MatchString(src) {
		t.Errorf("opsctl has no `usage: opsctl slack-watch <add|list|disable>` line — criterion 4 names " +
			"exactly those three subcommands (there is no `remove`: a watch row is turned OFF, never deleted)")
	}
	for _, tool := range []string{"slack_watch_add", "slack_watch_set_enabled", "slack_watch_list"} {
		if !strings.Contains(src, `"`+tool+`"`) {
			t.Errorf("opsctl never calls the %s tool; criterion 4's writes go through the executor "+
				"(validate -> policy -> audit start -> handler -> audit complete), so a seeding command is "+
				"attributable afterwards", tool)
		}
	}
	for _, banned := range []string{"INSERT INTO slack_watch", "UPDATE slack_watch", "DELETE FROM slack_watch"} {
		if strings.Contains(src, banned) {
			t.Errorf("opsctl spells %q itself. Invariant 3: the three tools are the table's only writers, and "+
				"a side door here would leave a watch row with no audit_events provenance", banned)
		}
	}
	// The flags the seeding commands in "Usable alone means" pass.
	for _, flag := range []string{"workspace", "conversation", "label"} {
		if !strings.Contains(src, flag) {
			t.Errorf("opsctl slack-watch has no %q flag; the SPEC's own seeding call carries "+
				"{workspace_id, conversation_id, label}", flag)
		}
	}
}
