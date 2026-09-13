package capture

// SWT-40 Part B structural checks (docs/tickets/inquiry-promote_SPEC.md,
// B-D5, B-D6, B-D7, B-D1, B5, B11, the data-model section and "Invariants that
// apply"). ZERO I/O beyond reading this repo's source and docs. Each check first
// REQUIRES its subject to exist: a scan with nothing to scan proves nothing.
//
//   - B-D6 / B5 / invariant 3: route application writes capture's own log and
//     calls NO tool — no executor, no task table, no delivery.
//   - B-D6 / invariant 7: DecideRoute's body does no I/O and reads no clock; the
//     apply step never reaches a model or a provider adapter.
//   - B5 / V3: the route row's ON CONFLICT restates WHERE mode = 'route'.
//   - E-D4: route_apply serializes on capture's lock 0x5157_0015.
//   - D-D2 / B6: pendingMessages' shadow-overwrite guard covers route rows.
//   - The migration's shape (numbered 0032 on this branch; the SPEC says 0029).
//   - B-D1 / invariant 3: source_account_projects is written only by the
//     humanOnly executor tools; route_after is armed by hand, never by code.
//   - B11: the runbook's Routing lane section and the IK entry.
//   - Invariant 1: route.go never reads raw_json.
//
// GREENFIELD NOTE — EXPECTED RED: internal/capture/route.go,
// migrations/0032_route_tier.sql, internal/tools/routecandidates.go and the doc
// sections do not exist.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	routeFile      = "internal/capture/route.go"
	routeMigration = "migrations/0032_route_tier.sql"
)

func TestCaptureRoute_WritesOnlyCapturesOwnLogAndCallsNoTool(t *testing.T) {
	src := mustReadRepoFile(t, routeFile)
	for _, b := range []struct{ re, why string }{
		{`(?is)insert\s+into\s+(tasks|external_refs|task_events|task_dismissals|deliveries|classify_promotions)\b`,
			"B5: no task, no ref, no event, no delivery — a route row is an attribution, nothing else"},
		{`(?is)update\s+(tasks|external_refs|task_events|task_dismissals|deliveries|source_accounts|source_account_projects)\b`,
			"route application mutates nothing but its own decision rows"},
		{`(?is)delete\s+from\s+capture_decisions`, "capture_decisions is an append-only log"},
		{`internal/executor"`, "B-D6: the driver calls no tool (\"API / MCP tool changes\": no executor call from route application)"},
		{`\.Execute\(`, "no executor call"},
		{`internal/provider"`, "invariant 7: the apply step never reaches a model; the LLM stage is the route LANE, a leaf consumer"},
		{`internal/connector/`, "no provider adapter"},
		{`"net/http"`, "no network"},
		{`raw_json`, "invariant 1: the account join selects source_account_id; the provider payload is never read here"},
		{`(?i)send_delivery|draft_delivery|smtp`, "invariant 4: nothing sends"},
	} {
		if m := regexp.MustCompile(b.re).FindString(src); m != "" {
			t.Errorf("%s contains %q — %s", routeFile, m, b.why)
		}
	}
	if !regexp.MustCompile(`(?is)insert\s+into\s+capture_decisions`).MatchString(src) {
		t.Errorf("%s never inserts into capture_decisions; B-D5: the applied decision IS a mode='route' row", routeFile)
	}
	if !regexp.MustCompile(`(?is)on\s+conflict\s*\(\s*message_id\s*\)\s*where\s+mode\s*=\s*'route'\s*do\s+nothing`).MatchString(src) {
		t.Errorf("%s has no `ON CONFLICT (message_id) WHERE mode = 'route' DO NOTHING`. B5: one route per message, "+
			"forever, against the PARTIAL capture_decisions_route_uniq — the predicate must be restated or Postgres "+
			"raises at runtime inside a stage nobody is watching", routeFile)
	}
	if !regexp.MustCompile(`(?i)0x5157_?0015|RulesAdvisoryLockKey|tryRulesLock\(`).MatchString(src) {
		t.Errorf("%s does not take capture's advisory lock 0x5157_0015 (E-D4/B-D6): route_apply writes "+
			"capture_decisions and must serialize with every connector's capture pass", routeFile)
	}
	if !strings.Contains(src, "ErrRouteLockHeld") {
		t.Errorf("%s does not declare ErrRouteLockHeld; pipelined maps it to pipeline.ErrLockHeld", routeFile)
	}
	if !strings.Contains(src, "route_after") {
		t.Errorf("%s never reads source_accounts.route_after; B-D7: arming is per account, and an unarmed account "+
			"writes nothing", routeFile)
	}
}

func TestDecideRoute_BodyIsPure(t *testing.T) {
	src := mustReadRepoFile(t, routeFile)
	i := strings.Index(src, "func DecideRoute(")
	if i < 0 {
		t.Fatalf("%s does not declare DecideRoute (B-D6 names it there)", routeFile)
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	for _, b := range []string{"ctx", "pool", ".Query", ".Exec", "Execute(", "time.Now", "time.Since", "os.Getenv"} {
		if strings.Contains(body, b) {
			t.Errorf("DecideRoute's body contains %q — B4/invariant 7: a pure function of (facts, candidates, verdict); "+
				"the arming and verdict instants arrive as values.\nbody:\n%s", b, body)
		}
	}
}

// D-D2's shadow-overwrite guard, shared with D: pendingMessages excludes a
// message carrying a route row in EVERY mode. The integration test is the
// mutation that bites; this keeps the clause from being dropped silently.
func TestPendingMessages_ExcludesRouteRowsInEveryMode(t *testing.T) {
	src := mustReadRepoFile(t, "internal/capture/rules_store.go")
	i := strings.Index(src, "func pendingMessages(")
	if i < 0 {
		t.Fatalf("rules_store.go has no pendingMessages")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, "'gate'") {
		t.Fatalf("POSITIVE CONTROL FAILED: pendingMessages no longer excludes gate rows")
	}
	if !strings.Contains(body, "'route'") {
		t.Errorf("pendingMessages does not exclude messages carrying a mode='route' row. D-D2/B6: otherwise the " +
			"documented shadow --all re-pointing pass writes newer rows that bury the route for every " +
			"ORDER BY id DESC reader")
	}
}

func TestMigration0032_RouteTierShape(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "0032_*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != filepath.Base(routeMigration) {
		t.Fatalf("migrations/0032_*.sql = %v, want exactly %s (the SPEC's 0029_route_tier.sql; 0029-0031 are taken)",
			matches, routeMigration)
	}
	sql := strings.ToLower(mustReadRepoFile(t, routeMigration))
	code := regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(sql, "")
	norm := strings.Join(strings.Fields(code), " ")

	for _, want := range []struct{ re, why string }{
		{`drop constraint capture_decisions_mode_check`, "the mode CHECK Part D widened"},
		{`add constraint capture_decisions_mode_check check \( ?mode in \( ?'shadow', ?'live', ?'gate', ?'route' ?\) ?\)`,
			"mode widened by exactly 'route', 'gate' kept"},
		{`add column route_step text check \( ?route_step in \( ?'thread', ?'single', ?'model', ?'default' ?\) ?\)`,
			"the four steps of B-D2, typed"},
		{`add column ai_extraction_id bigint references ai_extractions ?\( ?id ?\)`, "a model row names its verdict"},
		{`add constraint capture_decisions_route_shape check`, "B-D5's shape CHECK"},
		{`\( ?mode = 'route' ?\) = \( ?route_step is not null ?\)`, "route_step iff a route row"},
		{`mode <> 'route' or \( ?action = 'attributed' and matched_rule_id is null and task_id is null ?\)`,
			"a route row is an attribution: no rule, no task"},
		{`\( ?route_step = 'model' ?\) = \( ?ai_extraction_id is not null ?\)`, "an extraction iff step model"},
		{`create unique index capture_decisions_route_uniq on capture_decisions \( ?message_id ?\) where mode = 'route'`,
			"one route per message, forever — PARTIAL"},
		{`create table source_account_projects`, "B-D1's config table"},
		{`source_account_id bigint not null references source_accounts ?\( ?id ?\)`, "per source account"},
		{`project_id bigint not null references projects ?\( ?id ?\)`, "a real project"},
		{`is_default boolean not null default false`, "a default is opted into, never assumed"},
		{`description text not null`, "the row's description reaches the prompt (B-D4)"},
		{`unique ?\( ?source_account_id, ?project_id ?\)`, "a project is a candidate once per account"},
		{`create unique index source_account_projects_default_uniq on source_account_projects \( ?source_account_id ?\) where is_default`,
			"at most one default per account"},
		{`alter table source_accounts add column route_after timestamptz`, "B-D7's arming column"},
	} {
		if !regexp.MustCompile(want.re).MatchString(norm) {
			t.Errorf("%s does not match /%s/ — %s", routeMigration, want.re, want.why)
		}
	}
	for _, bad := range []struct{ re, why string }{
		{`capture_decisions_live_uniq`, "the live index is untouched (every deployed binary's ON CONFLICT … WHERE mode='live')"},
		{`capture_decisions_gate_uniq`, "Part D's index is untouched"},
		{`drop\s+index`, "no index is dropped"},
		{`insert\s+into`, "no seeding anywhere: config goes in through route_candidate_add"},
		{`update\s+source_accounts`, "the migration arms nothing: route_after is a hand-run UPDATE after the shadow period"},
		{`route_after timestamptz (not null|default)`, "NULL = routing off, the fail-closed side"},
		{`drop\s+column`, "forward-only"},
	} {
		if regexp.MustCompile(bad.re).MatchString(norm) {
			t.Errorf("%s matches /%s/ — %s", routeMigration, bad.re, bad.why)
		}
	}
}

// B-D1 / invariant 3: the candidate table is written ONLY through the humanOnly
// executor tools (route_candidate_add/remove, internal/tools/routecandidates.go).
// And route_after is armed only by a hand-run UPDATE (B-D7): no Go code sets it.
func TestRouteConfig_OnlyTheToolsWriteCandidatesAndNothingArms(t *testing.T) {
	tools := mustReadRepoFile(t, "internal/tools/routecandidates.go")
	if !regexp.MustCompile(`(?is)insert\s+into\s+source_account_projects`).MatchString(tools) ||
		!regexp.MustCompile(`(?is)delete\s+from\s+source_account_projects`).MatchString(tools) {
		t.Errorf("internal/tools/routecandidates.go does not INSERT into and DELETE from source_account_projects; " +
			"B8's two tools are the only writers")
	}
	write := regexp.MustCompile(`(?is)(insert\s+into|update|delete\s+from)\s+source_account_projects\b`)
	arm := regexp.MustCompile(`(?i)\broute_after\s*=\s*(now|clock_timestamp|current_timestamp|\$\d|'|null)`)
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
			rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
			if m := write.FindString(src); m != "" && filepath.ToSlash(rel) != "internal/tools/routecandidates.go" {
				t.Errorf("%s contains %q — invariant 3: candidate config is written only through the humanOnly "+
					"route_candidate_add / route_candidate_remove tools (validate → policy → audit)", rel, m)
			}
			if m := arm.FindString(src); m != "" {
				t.Errorf("%s contains %q — B-D7: arming is `UPDATE source_accounts SET route_after = now()` by hand, "+
					"per account, after the shadow reads; no code path may arm or disarm routing", rel, m)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
}

func TestDocs_RouteTier(t *testing.T) {
	rb := mustReadRepoFile(t, "docs/runbooks/local-classifier.md")
	if !regexp.MustCompile(`(?im)^#+ .*routing lane`).MatchString(rb) {
		t.Errorf("docs/runbooks/local-classifier.md has no \"Routing lane\" section (B11)")
	}
	for _, want := range []string{"route_after", "route-candidates", "pending_verdict", "classify_route"} {
		if !strings.Contains(rb, want) {
			t.Errorf("the runbook's routing section never mentions %q (B11: arming, candidates, the report's states)", want)
		}
	}
	pl := mustReadRepoFile(t, "docs/runbooks/pipeline.md")
	for _, row := range []string{"| `route` |", "| `route_apply` |"} {
		if !strings.Contains(pl, row) {
			t.Errorf("docs/runbooks/pipeline.md has no stage row %q (B11: the two stages, their locks and wakes)", row)
		}
	}
	ik := mustReadRepoFile(t, ".claude/INSTITUTIONAL_KNOWLEDGE.md")
	for _, want := range []string{"capture_decisions_route_uniq", "mode='route'", "source_account_projects"} {
		if !strings.Contains(ik, want) {
			t.Errorf(".claude/INSTITUTIONAL_KNOWLEDGE.md never mentions %q (B11: an IK entry on the route mode — the "+
				"FOURTH partial unique index on capture_decisions.message_id)", want)
		}
	}
}
