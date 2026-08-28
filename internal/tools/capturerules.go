package tools

// SWT-17 capture-rules tools (SPEC "API / MCP tool changes"): capture_rule_add
// and capture_rule_set_enabled are the ONLY way a `capture_rules` row is
// created or disabled, so `audit_events` + `policy_decisions` answer "who
// redirected the funnel, when, and was it allowed" for every routing change.
// The evaluation pass itself needs no tool of its own — it reuses create_task,
// link_external_ref and task_append_log.
//
// Two deliberate omissions, both load-bearing:
//
//   - Neither tool is listed in `internal/mcpserver/schemas.go`. Routing IS the
//     funnel; an agent that could add a rule could point any client's traffic at
//     any project and then be handed the work. The SPEC states this outright
//     ("neither is MCP-listed — an agent must not be able to redirect the
//     funnel"), and it is also this repo's default for anything spine-facing.
//   - Both are in `policy.humanOnly`, so only dashboard:/opsctl:/manual: actors
//     pass. The tool list and the actor gate are independent defences: dropping
//     off the MCP list keeps a worker from seeing the tool, humanOnly keeps a
//     worker-shaped actor from calling it through any other surface.
//
// There is no capture_rule_delete. A misfiring rule is turned OFF, never
// removed — `capture_decisions.matched_rule_id` references it, and the shadow
// report is worthless if the rule a decision names can vanish.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// captureCriteriaTypes is `capture_rules.criteria_type`'s CHECK (SPEC §1),
// spelled here on the Go side so a bad kind is refused with a readable error
// before Postgres refuses it with a constraint name. The two lists must move
// together; there is no third copy.
var captureCriteriaTypes = []string{
	"body_regex", "sender", "thread_key_prefix", "thread_key_contains",
	"source_slack_workspace", "person",
}

// captureExternalSystems is `external_refs.system`'s CHECK as migration 0015
// extends it (slack and gmail join the original three, so a thread-keyed dedup
// ref is expressible without a new table).
//
// Kept in step with external_refs' CHECK (migration 0015) and with
// validateLinkExternalRef in prci.go, which SWT-17 widened to the same five.
// Three spellings of one enum, and a drift between them is a duplicate-task
// loop rather than an error — see prci.go for the mechanism.
var captureExternalSystems = []string{"jira", "github", "upwork_crm", "slack", "gmail"}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// ---- capture_rule_add ----------------------------------------------------------

type captureRuleAddArgs struct {
	Project        string `json:"project"` // project slug
	CriteriaType   string `json:"criteria_type"`
	Pattern        string `json:"pattern"`
	Subproject     string `json:"subproject,omitempty"`
	ExternalSystem string `json:"external_system,omitempty"`
	KeyRegex       string `json:"key_regex,omitempty"`
	URLTemplate    string `json:"url_template,omitempty"`
	Priority       *int   `json:"priority,omitempty"`
	Note           string `json:"note,omitempty"`
}

// parseCaptureRuleAdd unmarshals and applies every check that needs no
// database, so validate and the handler share ONE spelling of the rules
// (the handler re-runs it: a handler never trusts that Validate ran).
//
// Project-slug resolution is deliberately NOT here — `Validate` is
// `func([]byte) error` with no pool and no context, so the SPEC's "project slug
// resolves" check can only live in the handler, where it does.
func parseCaptureRuleAdd(args []byte) (captureRuleAddArgs, error) {
	var a captureRuleAddArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	if a.Project == "" {
		return a, errors.New("missing project")
	}
	if !oneOf(a.CriteriaType, captureCriteriaTypes) {
		return a, fmt.Errorf("criteria_type %q: must be one of %s",
			a.CriteriaType, strings.Join(captureCriteriaTypes, ", "))
	}
	if a.Pattern == "" {
		return a, errors.New("missing pattern")
	}

	// Regexes are compiled at INSERT time, never at run time: a pattern that
	// fails to compile inside a CronJob is a silent routing outage, and RE2
	// gives no way to discover it later. The field is named in the error
	// because a rule carries two regexes.
	if a.CriteriaType == "body_regex" {
		if _, err := regexp.Compile(a.Pattern); err != nil {
			return a, fmt.Errorf("pattern %q is not a valid Go regexp: %w", a.Pattern, err)
		}
	}
	if a.KeyRegex != "" {
		if _, err := regexp.Compile(a.KeyRegex); err != nil {
			return a, fmt.Errorf("key_regex %q is not a valid Go regexp: %w", a.KeyRegex, err)
		}
	}

	// A `person` pattern is a people.id rendered as text (SPEC §1); the
	// evaluator compares it against the thread's participant ids, so anything
	// non-numeric is a rule that is stored, listed, and matches nothing
	// forever — the failure mode this repo has already paid for three times.
	// Refusing it here is the only place it is detectable.
	if a.CriteriaType == "person" {
		if _, err := strconv.ParseInt(a.Pattern, 10, 64); err != nil {
			return a, fmt.Errorf("pattern %q: criteria_type person takes a people.id as text (e.g. \"4242\"), "+
				"and a non-numeric pattern can never match a participant id", a.Pattern)
		}
	}

	if a.ExternalSystem != "" && !oneOf(a.ExternalSystem, captureExternalSystems) {
		return a, fmt.Errorf("external_system %q: must be one of %s",
			a.ExternalSystem, strings.Join(captureExternalSystems, ", "))
	}
	if a.URLTemplate != "" && !strings.Contains(a.URLTemplate, "{key}") {
		return a, fmt.Errorf("url_template %q must contain the {key} placeholder", a.URLTemplate)
	}
	return a, nil
}

func validateCaptureRuleAdd(args []byte) error {
	_, err := parseCaptureRuleAdd(args)
	return err
}

// captureRuleAdd inserts one capture_rules row. Rules are seeded by runbook
// rather than by a data migration (SPEC "Decisions made unilaterally" 5), so
// this tool is their only writer and the audit row is their provenance.
func captureRuleAdd(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseCaptureRuleAdd(args)
	if err != nil {
		return nil, err
	}

	var projectID int64
	err = pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1`, a.Project).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("project %q not found", a.Project)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", a.Project, err)
	}

	priority := 0
	if a.Priority != nil {
		priority = *a.Priority
	}

	var ruleID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO capture_rules
		   (project_id, subproject, criteria_type, pattern, external_system, key_regex, url_template, priority, note)
		 VALUES ($1, NULLIF($2,''), $3, $4, NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8, NULLIF($9,''))
		 RETURNING id`,
		projectID, a.Subproject, a.CriteriaType, a.Pattern, a.ExternalSystem,
		a.KeyRegex, a.URLTemplate, priority, a.Note).Scan(&ruleID)
	if err != nil {
		return nil, fmt.Errorf("insert capture rule (one rule per project+criteria_type+pattern — "+
			"disable the existing one with capture_rule_set_enabled instead of re-adding?): %w", err)
	}
	return marshalResult(map[string]any{"rule_id": ruleID})
}

// ---- capture_rule_set_enabled --------------------------------------------------

type captureRuleSetEnabledArgs struct {
	RuleID int64 `json:"rule_id"`
	// Pointer so an omitted flag is an error rather than a silent disable —
	// same shape as set_sending_frozen's `frozen`.
	Enabled *bool `json:"enabled"`
}

func parseCaptureRuleSetEnabled(args []byte) (captureRuleSetEnabledArgs, error) {
	var a captureRuleSetEnabledArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("parse args: %w", err)
	}
	if a.RuleID == 0 {
		return a, errors.New("missing rule_id")
	}
	if a.Enabled == nil {
		return a, errors.New("missing enabled (explicit true/false required)")
	}
	return a, nil
}

func validateCaptureRuleSetEnabled(args []byte) error {
	_, err := parseCaptureRuleSetEnabled(args)
	return err
}

// captureRuleSetEnabled flips one rule's `enabled` flag. Turning a rule off is
// how a misfiring rule is retired: the row survives so the
// `capture_decisions.matched_rule_id` history it produced stays readable.
//
// An unknown rule_id is an ERROR, not `{"updated": false}` — this is routing
// configuration typed by a human, and a silent no-op on a mistyped id is
// indistinguishable from a rule that was turned off.
func captureRuleSetEnabled(ctx context.Context, pool *pgxpool.Pool, args []byte) ([]byte, error) {
	a, err := parseCaptureRuleSetEnabled(args)
	if err != nil {
		return nil, err
	}

	var id int64
	err = pool.QueryRow(ctx,
		`UPDATE capture_rules SET enabled = $2 WHERE id = $1 RETURNING id`,
		a.RuleID, *a.Enabled).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("capture rule %d not found", a.RuleID)
	}
	if err != nil {
		return nil, fmt.Errorf("set capture rule %d enabled=%t: %w", a.RuleID, *a.Enabled, err)
	}
	return marshalResult(map[string]any{"updated": true})
}
