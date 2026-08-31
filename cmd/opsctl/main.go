// opsctl is a minimal CLI client of the executor — it never writes tool-action
// tables directly (invariant 3). Usage:
//
//	opsctl create-task --project <slug> --title "..." [--body ... --assignee human|claude --priority N --subproject X]
//	opsctl call --tool <name> [--args '<json>']   (raw executor call; used by the negative smoke)
//	opsctl fleet
//	opsctl answer-feedback --id N --answer "..." [--resume]
//	opsctl capture-rules <list|add|run|report> [flags]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/capture"
	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/connector/jira"
	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: opsctl <create-task|call|fleet|answer-feedback|capture-rules> [flags]")
		os.Exit(2)
	}

	var toolName string
	var args json.RawMessage
	var err error

	switch os.Args[1] {
	case "create-task":
		toolName, args, err = parseCreateTask(os.Args[2:])
	case "call":
		toolName, args, err = parseCall(os.Args[2:])
	case "fleet":
		if err := runFleet(); err != nil {
			fmt.Fprintln(os.Stderr, "opsctl:", err)
			os.Exit(1)
		}
		return
	case "answer-feedback":
		if err := runAnswerFeedback(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "opsctl:", err)
			os.Exit(1)
		}
		return
	case "capture-rules":
		// `add` mutates routing, so it is a tool call and falls through to the
		// executor path below like create-task. list/run/report are their own
		// paths: two are reads and `run` needs a deadline the 30s one cannot give.
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: opsctl capture-rules <list|add|run|report> [flags]")
			break
		}
		if os.Args[2] == "add" {
			toolName, args, err = parseCaptureRuleAdd(os.Args[3:])
			break
		}
		if err := runCaptureRules(os.Args[2], os.Args[3:]); err != nil {
			fmt.Fprintln(os.Stderr, "opsctl:", err)
			os.Exit(1)
		}
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "opsctl:", err)
		os.Exit(2)
	}

	if err := run(toolName, args); err != nil {
		fmt.Fprintln(os.Stderr, "opsctl:", err)
		os.Exit(1)
	}
}

func parseCreateTask(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("create-task", flag.ContinueOnError)
	project := fs.String("project", "", "project slug (required)")
	title := fs.String("title", "", "task title (required)")
	body := fs.String("body", "", "task body")
	assignee := fs.String("assignee", "", "assignee_type: human (default) | claude")
	priority := fs.Int("priority", 0, "priority")
	subproject := fs.String("subproject", "", "subproject")
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}

	payload := map[string]any{"project": *project, "title": *title}
	if *body != "" {
		payload["body"] = *body
	}
	if *assignee != "" {
		payload["assignee_type"] = *assignee
	}
	if *priority != 0 {
		payload["priority"] = *priority
	}
	if *subproject != "" {
		payload["subproject"] = *subproject
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("marshal args: %w", err)
	}
	return "create_task", raw, nil
}

func parseCall(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	tool := fs.String("tool", "", "tool name (required)")
	rawArgs := fs.String("args", "{}", "tool args as JSON")
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	if *tool == "" {
		return "", nil, fmt.Errorf("--tool is required")
	}
	if !json.Valid([]byte(*rawArgs)) {
		return "", nil, fmt.Errorf("--args is not valid JSON")
	}
	return *tool, json.RawMessage(*rawArgs), nil
}

func run(toolName string, args json.RawMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))
	if key := os.Getenv("OPS_TOKEN_KEY"); key != "" {
		tools.SetJiraSender(&jira.AccountSender{Pool: pool, TokenKey: key})
	}
	// Routes per account auth_type (SWT-11 criterion 13): app-password mailboxes
	// over SMTP, OAuth mailboxes over the bridge/direct path as before.
	sender, _, err := google.WireMailSender(pool)
	if err != nil {
		return err
	}
	if sender != nil {
		tools.SetGmailSender(sender)
	}
	// One bridge serves both seams: prefill_delivery drafts through it and
	// send_delivery sends through it (SWT-12 criterion 15). HTTP is preferred so
	// this works from anywhere the mini is reachable, not only on the mini.
	if bridge, err := slackweb.NewDeliveryBridgeFromEnv(); err != nil {
		return fmt.Errorf("configure Slack bridge: %w", err)
	} else if bridge != nil {
		tools.SetSlackDrafter(bridge)
		tools.SetSlackSender(bridge)
	}

	res, err := ex.Execute(ctx, executor.Call{Tool: toolName, Actor: actor(), Args: args})
	if err != nil {
		return err
	}
	fmt.Println(string(res.Output))
	return nil
}

func actor() string {
	if u := os.Getenv("USER"); u != "" {
		return "opsctl:" + u
	}
	return "opsctl"
}

// runAnswerFeedback answers an open feedback request through the executor and
// optionally publishes the fleet resume command to the task's worker.
func runAnswerFeedback(argv []string) error {
	fs := flag.NewFlagSet("answer-feedback", flag.ContinueOnError)
	id := fs.Int64("id", 0, "feedback_request_id (required)")
	answer := fs.String("answer", "", "answer text (required)")
	resume := fs.Bool("resume", false, "publish the fleet resume cmd to the worker")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *id == 0 || *answer == "" {
		return fmt.Errorf("--id and --answer are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	answerJSON, err := json.Marshal(*answer)
	if err != nil {
		return fmt.Errorf("marshal answer: %w", err)
	}
	res, err := ex.Execute(ctx, executor.Call{
		Tool:  "answer_feedback",
		Actor: actor(),
		Args:  []byte(fmt.Sprintf(`{"feedback_request_id":%d,"answer":%s}`, *id, answerJSON)),
	})
	if err != nil {
		return err
	}
	fmt.Println(string(res.Output))

	if !*resume {
		return nil
	}
	var out struct {
		TaskID int64  `json:"task_id"`
		Client string `json:"client"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return fmt.Errorf("parse answer_feedback output: %w", err)
	}
	broker := os.Getenv("MQTT_BROKER")
	if broker == "" {
		return fmt.Errorf("MQTT_BROKER is not set (required for --resume)")
	}
	fc, err := fleet.NewMirrorClient(ctx, broker)
	if err != nil {
		return fmt.Errorf("connect broker: %w", err)
	}
	defer fc.Disconnect()
	args, err := json.Marshal(map[string]int64{"task_id": out.TaskID, "feedback_request_id": *id})
	if err != nil {
		return fmt.Errorf("marshal resume args: %w", err)
	}
	if err := fc.PublishCommand(out.Client, fleet.Cmd{Action: fleet.ActionResume, Args: args}); err != nil {
		return err
	}
	fmt.Printf("resume cmd published to %s\n", out.Client)
	return nil
}

// runFleet prints the fleet view: one line per worker_heartbeats row. It is a
// read-only SELECT (a view, not an action — no executor involvement); rows
// older than 3x the heartbeat interval and not already dead are marked stale.
func runFleet() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT worker_id, COALESCE(client,''), state, task_id, last_seen
		 FROM worker_heartbeats ORDER BY worker_id`)
	if err != nil {
		return fmt.Errorf("select heartbeats: %w", err)
	}
	defer rows.Close()

	staleAfter := 3 * fleet.HeartbeatInterval
	n := 0
	for rows.Next() {
		var workerID, client, state string
		var taskID *int64
		var lastSeen time.Time
		if err := rows.Scan(&workerID, &client, &state, &taskID, &lastSeen); err != nil {
			return fmt.Errorf("scan heartbeat: %w", err)
		}
		n++
		age := time.Since(lastSeen).Round(time.Second)
		task := "-"
		if taskID != nil {
			task = fmt.Sprintf("#%d", *taskID)
		}
		flag := ""
		if state != fleet.StateDead && age > staleAfter {
			flag = "  STALE"
		}
		fmt.Printf("%-24s %-12s %-16s %-8s last_seen %s ago%s\n", workerID, client, state, task, age, flag)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate heartbeats: %w", err)
	}
	if n == 0 {
		fmt.Println("no workers")
	}
	return nil
}

// ---- capture-rules (SWT-17) ----------------------------------------------------
//
// Four verbs, three shapes. `add` is a routing MUTATION and therefore an executor
// tool call (capture_rule_add, humanOnly) reached through the same path as
// create-task — "who changed the routing and when" is answerable from
// audit_events. `list` and `report` are reads, so they are plain SELECTs, the
// same standing runFleet has. `run` drives the pass itself.
//
// There is deliberately no `set-enabled` verb: the SPEC's CLI surface is
// list|add|run|report, and disabling a rule is already reachable as
//
//	opsctl call --tool capture_rule_set_enabled --args '{"rule_id":3,"enabled":false}'

const (
	// Generous on purpose. `run` walks the message corpus; SWT-13 already paid
	// for an opsctl deadline (30s) that was sized for a tool call and then cut a
	// long operation in half.
	captureRulesRunTimeout    = 15 * time.Minute
	captureRulesReportTimeout = 5 * time.Minute
	captureRulesListTimeout   = 30 * time.Second
)

func runCaptureRules(sub string, argv []string) error {
	switch sub {
	case "list":
		return runCaptureRulesList(argv)
	case "run":
		return runCaptureRulesRun(argv)
	case "report":
		return runCaptureRulesReport(argv)
	default:
		return fmt.Errorf("unknown capture-rules command %q (want list|add|run|report)", sub)
	}
}

// parseCaptureRuleAdd builds the capture_rule_add call. It validates only
// presence: `pattern` and `key_regex` are compiled by the TOOL (criterion 5), and
// a second regexp.Compile here would be a second spelling of the same rule that
// could disagree with the one that actually gates the insert.
func parseCaptureRuleAdd(argv []string) (string, json.RawMessage, error) {
	fs := flag.NewFlagSet("capture-rules add", flag.ContinueOnError)
	project := fs.String("project", "", "project slug (required)")
	criteria := fs.String("type", "", "criteria_type: body_regex|sender|thread_key_prefix|thread_key_contains|source_slack_workspace|person (required)")
	pattern := fs.String("pattern", "", "the criterion's pattern (required)")
	subproject := fs.String("subproject", "", "subproject for tasks this rule creates")
	externalSystem := fs.String("external-system", "", "jira|github|upwork_crm|slack|gmail; empty means attribution only, no task")
	keyRegex := fs.String("key-regex", "", "external key extractor; empty reuses --pattern for body_regex, else the thread_key")
	urlTemplate := fs.String("url-template", "", "external_url builder; must contain {key}")
	priority := fs.Int("priority", 0, "evaluation priority; evaluation order is priority DESC, id ASC and first match wins")
	note := fs.String("note", "", "why this rule exists")
	if err := fs.Parse(argv); err != nil {
		return "", nil, err
	}
	if *project == "" || *criteria == "" || *pattern == "" {
		return "", nil, fmt.Errorf("--project, --type and --pattern are required")
	}

	// priority is always sent, including 0: it is the load-bearing field (SPEC
	// §2), and omitting it to mean "default" hides the one number a reader of the
	// audit row needs.
	payload := map[string]any{
		"project":       *project,
		"criteria_type": *criteria,
		"pattern":       *pattern,
		"priority":      *priority,
	}
	optional := []struct {
		key, value string
	}{
		{"subproject", *subproject},
		{"external_system", *externalSystem},
		{"key_regex", *keyRegex},
		{"url_template", *urlTemplate},
		{"note", *note},
	}
	for _, o := range optional {
		if o.value != "" {
			payload[o.key] = o.value
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("marshal args: %w", err)
	}
	return "capture_rule_add", raw, nil
}

// runCaptureRulesList prints every rule in EVALUATION order (priority DESC, id
// ASC), enabled and disabled alike.
//
// Both choices are deliberate. Printing in evaluation order means the listing
// answers the question the SPEC calls load-bearing — which rule claims a message
// first — by reading top to bottom. Printing disabled rules with an on/off column
// rather than hiding them means a rule that stopped matching because someone
// disabled it is visible, instead of looking exactly like a rule that was never
// added.
func runCaptureRulesList(argv []string) error {
	fs := flag.NewFlagSet("capture-rules list", flag.ContinueOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), captureRulesListTimeout)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx,
		`SELECT r.id, p.slug, COALESCE(r.subproject,''), r.criteria_type, r.pattern,
		        COALESCE(r.external_system,''), COALESCE(r.key_regex,''), COALESCE(r.url_template,''),
		        r.priority, r.enabled, COALESCE(r.note,'')
		   FROM capture_rules r JOIN projects p ON p.id = r.project_id
		  ORDER BY r.priority DESC, r.id`)
	if err != nil {
		return fmt.Errorf("select capture_rules: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var id int64
		var slug, subproject, criteria, pattern, extSystem, keyRegex, urlTemplate, note string
		var priority int
		var enabled bool
		if err := rows.Scan(&id, &slug, &subproject, &criteria, &pattern,
			&extSystem, &keyRegex, &urlTemplate, &priority, &enabled, &note); err != nil {
			return fmt.Errorf("scan capture_rule: %w", err)
		}
		n++
		state := "off"
		if enabled {
			state = "on"
		}
		project := slug
		if subproject != "" {
			project = slug + "/" + subproject
		}
		fmt.Printf("%-5d prio %-4d %-3s %-22s %-22s %s\n", id, priority, state, project, criteria, pattern)
		// The key derivation on its own line: it is what turns a match into a
		// task, and "attribution only, no task" is the difference between a rule
		// that files tickets and one that only labels a message (SPEC §3).
		if extSystem == "" {
			fmt.Printf("      -> attribution only (no external_system, so no task)\n")
		} else {
			detail := fmt.Sprintf("      -> %s key", extSystem)
			if keyRegex != "" {
				detail += fmt.Sprintf(" %s", keyRegex)
			}
			if urlTemplate != "" {
				detail += fmt.Sprintf(" url %s", urlTemplate)
			}
			fmt.Println(detail)
		}
		if note != "" {
			fmt.Printf("      note: %s\n", note)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate capture_rules: %w", err)
	}
	if n == 0 {
		fmt.Println("no capture rules")
	}
	return nil
}

// runCaptureRulesRun drives one pass.
func runCaptureRulesRun(argv []string) error {
	fs := flag.NewFlagSet("capture-rules run", flag.ContinueOnError)
	live := fs.Bool("live", false, "act: create tasks and append logs. Default is shadow, which records decisions and creates nothing")
	since := fs.Duration("since", 0, "bound sent_at to the last N (Go duration); 0 keeps the mode's default")
	limit := fs.Int("limit", 0, "stop after N messages; 0 means no limit")
	all := fs.Bool("all", false, "re-evaluate messages that already carry a live decision row; SHADOW-ONLY, refused in live mode")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), captureRulesRunTimeout)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// The same four-line block every connector main now carries
	// (cmd/connectors/github/main.go is the precedent): the pass reaches tasks,
	// external_refs and task_events only through create_task, link_external_ref
	// and task_append_log.
	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))
	ex := executor.New(reg, checker, audit.NewPGStore(pool))

	cfg := captureRulesConfig(*live, *since, *limit, *all)
	stats, err := capture.EvaluateRules(ctx, pool, ex, cfg)
	// Printed unconditionally, zeros included, and before the error check: a pass
	// that matched nothing and a pass that never ran must not look the same.
	fmt.Printf("capture_rules: {\"mode\":%q,\"considered\":%d,\"matched\":%d,\"unmatched\":%d,"+
		"\"tasks_created\":%d,\"appended\":%d}\n",
		cfg.Mode, stats.Considered, stats.Matched, stats.Unmatched, stats.TasksCreated, stats.Appended)
	if err != nil {
		return fmt.Errorf("capture rules: %w", err)
	}
	return nil
}

// captureRulesConfig resolves the pass's knobs: environment first, because that
// is how a CronJob is configured, then flags on top, because that is how a human
// runs it.
//
// Mode and horizon come from capture's own readers rather than a second os.Getenv
// here — the "720 is not a Go duration" defence and the shadow-unbounded /
// live-bounded split each have exactly ONE spelling, and opsctl disagreeing with
// the CronJobs about what CAPTURE_RULES_SINCE means is a routing outage nobody
// would see. --live can only turn the mode ON: a human asking for live while the
// environment says shadow gets live, and there is deliberately no --shadow to
// argue with a manifest that says live.
//
// --all is passed through and refused by EvaluateRules in live mode (criterion
// 10); the refusal lives there so it also binds a caller that is not this CLI.
func captureRulesConfig(live bool, since time.Duration, limit int, all bool) capture.RulesConfig {
	mode := capture.RulesMode()
	if live {
		mode = capture.RulesModeLive
	}
	cfg := capture.RulesConfig{
		Mode:    mode,
		Horizon: capture.RulesHorizon(mode),
		Limit:   limit,
		Actor:   "capture:opsctl",
		All:     all,
	}
	if since > 0 {
		cfg.Horizon = since
	}
	return cfg
}

// runCaptureRulesReport prints the shadow-diff report from capture_decisions.
//
// --since defaults to unbounded (the zero time), not to the pass's horizon: the
// report's job is to show what the rules have decided so far, and silently
// hiding older decisions would make a rule that stopped firing indistinguishable
// from one that never fired.
func runCaptureRulesReport(argv []string) error {
	fs := flag.NewFlagSet("capture-rules report", flag.ContinueOnError)
	since := fs.Duration("since", 0, "report on decisions from the last N (Go duration); 0 means all of them")
	domain := fs.String("domain", "", "append the domain-detail investigation for one sender domain (SWT-23)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), captureRulesReportTimeout)
	defer cancel()

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	var from time.Time
	if *since > 0 {
		from = time.Now().Add(-*since)
	}
	out, err := capture.Report(ctx, pool, from, *domain)
	if err != nil {
		return fmt.Errorf("capture rules report: %w", err)
	}
	fmt.Print(out)
	return nil
}
