package capture

// SWT-40 Part D structural checks (docs/tickets/inquiry-promote_SPEC.md,
// criteria D3/D6, D-D2/D-D4, the 0027→0029 data-model section and V3). ZERO
// I/O beyond reading this repo's source. Each check first REQUIRES its subject
// to exist: a scan with nothing to scan proves nothing.
//
//   - D6: the direct-write scan holds for the gate's file. The existing
//     TestCaptureRules_NeverWritesToolActionTablesDirectly only scans rules*.go,
//     so gate.go needs its own pass of the same bans (and the existing test is
//     left unmodified).
//   - D-D4: the gate never touches the jira HTTP client; it goes through
//     ticketstatus.EnsureSnapshots and ticketstatus.Warranted, and re-spells
//     neither the TTL nor the warranted predicate.
//   - D3: DecideGate's body does no I/O and reads no clock.
//   - D-D2 / V3: every ON CONFLICT against capture_decisions restates its
//     partial-index predicate, and the gate row's is `WHERE mode = 'gate'`.
//   - The migration's shape (numbered 0029 on this branch; SPEC text says 0027).
//
// GREENFIELD NOTE — EXPECTED RED: internal/capture/gate.go and
// migrations/0029_capture_ticket_gate.sql do not exist.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const gateMigration = "migrations/0029_capture_ticket_gate.sql"

func TestCaptureGate_NeverWritesToolActionTablesDirectly(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/gate.go")
	banned := regexp.MustCompile(`(?is)insert\s+into\s+(tasks|external_refs|task_events|task_dismissals)\b`)
	updateBanned := regexp.MustCompile(`(?is)update\s+(tasks|external_refs|task_events|task_dismissals)\b`)
	if m := banned.FindString(src); m != "" {
		t.Errorf("internal/capture/gate.go contains %q — invariant 3: the gate reaches tasks/external_refs/"+
			"task_events/task_dismissals ONLY through create_task, link_external_ref, task_set_source_thread, "+
			"task_append_log and task_reopen on the executor, as capture:gate", m)
	}
	if m := updateBanned.FindString(src); m != "" {
		t.Errorf("internal/capture/gate.go contains %q — same rule", m)
	}
	// Control: the gate DOES write capture's own log, or it resolves nothing.
	if !regexp.MustCompile(`(?is)insert\s+into\s+capture_decisions`).MatchString(src) {
		t.Errorf("internal/capture/gate.go never inserts into capture_decisions; the resolution IS a " +
			"mode='gate' decision row (D-D2), so a gate that writes none is not the gate")
	}
}

func TestCaptureGate_ReachesJiraOnlyThroughTicketstatus(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/gate.go")
	for _, b := range []struct{ token, why string }{
		{`"net/http"`, "capture never calls Jira itself (D-D1); the lookup is ticketstatus.EnsureSnapshots"},
		{"LookupIssues(", "the fetch is EnsureSnapshots' job — a second fetch path re-spells routing and the TTL"},
		{"NewClient(", "the client is built by the injected factory pipelined holds, never here"},
		{"GetIssue(", "no direct GET"},
		{"Myself(", "no direct /myself"},
		{"RouteLookup(", "routing is inside EnsureSnapshots; a second call site is a second spelling"},
		{"LookupTTL(", "freshness is inside EnsureSnapshots (D-D5: the cache IS the reconciler's stored snapshot)"},
		{"IsDeliveredStatus(", "warranted has ONE spelling, ticketstatus.Warranted (D-D3)"},
		{`"ticket_done"`, "the drop reason comes from Warranted, never re-derived here"},
	} {
		if strings.Contains(src, b.token) {
			t.Errorf("internal/capture/gate.go contains %q — %s", b.token, b.why)
		}
	}
	for _, want := range []string{"ticketstatus.EnsureSnapshots(", "ticketstatus.Warranted("} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/capture/gate.go does not call %s — D-D3/D-D4: the gate shares the reconciler's "+
				"snapshot half and its predicate, so it never creates a task the reconciler closes 15 minutes later", want)
		}
	}
	if !regexp.MustCompile(`(?i)0x5157_?0015|RulesAdvisoryLockKey|tryRulesLock\(`).MatchString(src) {
		t.Errorf("internal/capture/gate.go does not take capture's advisory lock 0x5157_0015 (E-D4/D-D4): the " +
			"gate writes capture_decisions and must serialize with every connector's capture pass")
	}
}

func TestDecideGate_BodyIsPure(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/gate.go")
	i := strings.Index(src, "func DecideGate(")
	if i < 0 {
		t.Fatalf("internal/capture/gate.go does not declare DecideGate (D-D3 names it there)")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	for _, b := range []string{"ctx", "pool", ".Query", ".Exec", "Execute(", "time.Now", "time.Since", "os.Getenv"} {
		if strings.Contains(body, b) {
			t.Errorf("DecideGate's body contains %q — criterion D3: it is a pure function of (observation, "+
				"readable, ref); the hold's age arrives as a value, never a clock read.\nbody:\n%s", b, body)
		}
	}
}

// Every ON CONFLICT (message_id) in a file that writes capture_decisions must
// restate `WHERE mode = '…'`: both unique indexes on message_id are PARTIAL
// (live since 0015, gate since this ticket), and arbiter inference matches a
// partial index only when the predicate is repeated — omitting it raises "no
// unique or exclusion constraint matching the ON CONFLICT specification" at
// runtime, inside a stage nobody is watching. V3's mutation (drop the gate row's
// restated predicate) turns this red, and the integration suite red at runtime.
func TestCaptureDecisions_EveryOnConflictRestatesItsPartialPredicate(t *testing.T) {
	conflict := regexp.MustCompile(`(?is)on\s+conflict\s*\(\s*message_id\s*\)`)
	restated := regexp.MustCompile(`(?is)^\s*where\s+mode\s*=\s*'(\w+)'`)
	modes := map[string]bool{}
	found := 0
	for _, top := range []string{"internal", "cmd"} {
		root := filepath.Join("..", "..", top)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(b)
			if !strings.Contains(src, "capture_decisions") {
				return nil
			}
			for _, loc := range conflict.FindAllStringIndex(src, -1) {
				found++
				m := restated.FindStringSubmatch(src[loc[1]:])
				if m == nil {
					t.Errorf("%s: ON CONFLICT (message_id) without a restated `WHERE mode = '…'` — both "+
						"capture_decisions unique indexes on message_id are partial", path)
					continue
				}
				modes[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if found == 0 {
		t.Fatalf("found no ON CONFLICT (message_id) at all; the scan is blind (insertDecision has one)")
	}
	if !modes["live"] {
		t.Errorf("no ON CONFLICT … WHERE mode = 'live' found; the scan is not seeing insertDecision")
	}
	if !modes["gate"] {
		t.Errorf("no ON CONFLICT (message_id) WHERE mode = 'gate' found — D-D2: the gate row is inserted BEFORE "+
			"the executor calls (claim-before-act) against capture_decisions_gate_uniq, and the predicate must be "+
			"restated. modes seen: %v", modes)
	}
}

func TestMigration0029_CaptureTicketGateShape(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0029_*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != filepath.Base(gateMigration) {
		t.Fatalf("migrations/0029_*.sql = %v, want exactly %s (0027 is SWT-39's, 0028 is claimed by SWT-43; "+
			"whichever branch merges second renumbers)", matches, gateMigration)
	}
	sql := strings.ToLower(mustReadRepoFile(t, gateMigration))
	code := regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(sql, "")
	norm := strings.Join(strings.Fields(code), " ")

	for _, want := range []struct{ re, why string }{
		{`drop constraint capture_decisions_action_check`, "the inline CHECK name 0015 created (confirm against prod)"},
		{`add constraint capture_decisions_action_check check \( ?action in \( ?'unmatched', ?'attributed', ?'task', ?'task_log', ?'held' ?\) ?\)`,
			"action widened by exactly 'held'"},
		{`drop constraint capture_decisions_mode_check`, "the inline mode CHECK name"},
		{`add constraint capture_decisions_mode_check check \( ?mode in \( ?'shadow', ?'live', ?'gate' ?\) ?\)`,
			"mode widened by exactly 'gate'"},
		{`add constraint capture_decisions_gate_shape check`, "mode='gate' ⇒ action/rule/system/key shape"},
		{`mode <> 'gate' or \( ?action in \( ?'task', ?'task_log', ?'attributed' ?\)`, "a gate row is task | task_log | attributed"},
		{`matched_rule_id is not null`, "gate rows name their rule"},
		{`external_system is not null`, "gate rows name their system"},
		{`external_key is not null`, "gate rows name their key"},
		{`add constraint capture_decisions_held_is_not_gate check \( ?action <> 'held' or mode in \( ?'shadow', ?'live' ?\) ?\)`,
			"held is a capture action (shadow or live), never a resolution"},
		{`add constraint capture_decisions_gate_task_pin check \( ?mode <> 'gate' or \( ?\( ?action <> 'attributed' or task_id is null ?\) ?and \( ?action <> 'task_log' or task_id is not null ?\) ?\) ?\)`,
			"review fix 7: a gate attributed row names no task, a gate task_log row names its task; a gate task " +
				"row is claimed before its task exists, so it is left free"},
		{`create unique index capture_decisions_gate_uniq on capture_decisions \( ?message_id ?\) where mode = 'gate'`,
			"one resolution per message, forever — PARTIAL"},
	} {
		if !regexp.MustCompile(want.re).MatchString(norm) {
			t.Errorf("%s does not match /%s/ — %s", gateMigration, want.re, want.why)
		}
	}
	for _, bad := range []struct{ re, why string }{
		{`capture_decisions_live_uniq`, "the live index is untouched (old binaries' ON CONFLICT … WHERE mode='live' keeps inferring it)"},
		{`insert\s+into`, "no seeding: rules go through capture_rule_add, arming is a hand-run UPDATE"},
		{`update\s+projects`, "the migration arms nothing; ticket_assignee_gate is set per project by hand"},
		{`drop\s+column`, "forward-only"},
	} {
		if regexp.MustCompile(bad.re).MatchString(norm) {
			t.Errorf("%s matches /%s/ — %s", gateMigration, bad.re, bad.why)
		}
	}
}
