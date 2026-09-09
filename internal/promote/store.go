package promote

// The driver: advisory lock, inbox query, per-verdict claim, executor calls —
// capture.EvaluateRules with a simpler decision (the SPEC names it as the
// sibling to copy). The claim ordering is criterion 12: the classify_promotions
// row is inserted BEFORE the executor call it describes, then updated with the
// resulting task_id, so a crash between the two leaves a visible artifact
// (task_id NULL) instead of a task nothing remembers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/textmatch"
)

// AdvisoryLockKey serialises promotion passes (criterion 17). Free — verified
// against every key in the repo: 0005 orchestrator, 0006 triage, 0015 capture,
// classify's own, and SWT-28's calendar booking.
const AdvisoryLockKey = int64(0x5157_0021)

// titleLimit is create_task's title budget, cut with the ONE spelling
// (textmatch.NormalizedPrefix, criterion 15).
const titleLimit = 120

// Config drives one pass.
type Config struct {
	// DryRun reads the same rows and takes the same decisions as the real path,
	// performs no writes of any kind (no promotion rows either), and prints one
	// line per decision (criterion 14).
	DryRun bool
	// Limit caps how many verdicts this pass considers (0 = all).
	Limit int
}

// Stats summarises one pass. The printed plan is the CLI's contract; these
// fields are informational.
type Stats struct {
	Considered int // verdicts the inbox produced
	Created    int // live tasks (action=task)
	Review     int // holding tasks (action=review)
	Attached   int // log appends onto an open task
	Lost       int // claims lost to a concurrent or earlier row
}

// verdictRow is Verdict plus what the executor calls need but Decide does not.
type verdictRow struct {
	v Verdict
}

// Run is one promotion pass.
//
// Losing the advisory lock is an ERROR and a non-zero exit — classify's lock
// POLICY, deliberately not capture's (which logs and returns nil): this is a
// solo pass, and a run that silently no-ops looks exactly like an empty inbox.
// The lock SHAPE is capture's and classify's both: a dedicated connection with
// an explicit unlock before release, because a session-level advisory lock
// outlives a returned pooled connection.
func Run(ctx context.Context, pool *pgxpool.Pool, ex *executor.Executor, cfg Config) (Stats, error) {
	var stats Stats
	if pool == nil {
		return stats, errors.New("promote: nil database pool")
	}
	if !cfg.DryRun && ex == nil {
		// Invariant 3: the live path reaches tasks and task_events only through
		// the executor, so without one there is no legal way to act.
		return stats, errors.New("promote: live mode requires an executor")
	}

	release, held, err := tryLock(ctx, pool)
	if err != nil {
		return stats, err
	}
	if !held {
		return stats, fmt.Errorf("promote: another pass holds advisory lock 0x%X; refusing to run", AdvisoryLockKey)
	}
	defer release()

	// Criterion 5: promotion is OFF until a human sets a cutover. Checked
	// explicitly so the answer is a sentence rather than a silent empty inbox.
	var armed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE classify_promote_after IS NOT NULL`).Scan(&armed); err != nil {
		return stats, fmt.Errorf("promote: count armed projects: %w", err)
	}
	if armed == 0 {
		slog.Info("promote: no project has a cutover set; promotion is off everywhere " +
			"(UPDATE projects SET classify_promote_after = ... to arm one — see docs/runbooks/local-classifier.md)")
		return stats, nil
	}

	rows, err := inbox(ctx, pool, cfg.Limit)
	if err != nil {
		return stats, err
	}

	for _, row := range rows {
		v := row.v
		existing, finished, err := threadTask(ctx, pool, v.ThreadID, v.ProjectID)
		if err != nil {
			return stats, err
		}
		d := Decide(v, existing)
		reason := decisionReason(v, existing, finished)

		if cfg.DryRun {
			stats.Considered++
			slog.Info("promote dry-run", "message", v.MessageID, "kind", v.Kind,
				"action", d.Action, "status", d.Status, "attach_task", d.TaskID,
				"project", v.ProjectSlug, "title", v.Title, "reason", reason)
			count(&stats, d)
			continue
		}

		promoID, claimed, err := claim(ctx, pool, v, d, reason)
		if err != nil {
			return stats, err
		}
		if !claimed {
			// Whoever won the claim owns the action — a concurrent pass, or a
			// second eligible verdict for the same message inside this one. One
			// row per message, forever (criterion 11).
			stats.Lost++
			continue
		}
		stats.Considered++

		switch d.Action {
		case "attached":
			if err := appendVerdictLog(ctx, ex, v, d.TaskID); err != nil {
				return stats, err
			}
			if err := recordTask(ctx, pool, promoID, d.TaskID); err != nil {
				return stats, err
			}
		default: // "task" | "review"
			taskID, err := createVerdictTask(ctx, ex, v, d)
			if err != nil {
				return stats, err
			}
			// Record the task on its promotion row BEFORE provenance —
			// capture.EvaluateRules' ordering, for the same reason: the claim is
			// spent, so a later failure must not also lose the pointer to the
			// task that was created.
			if err := recordTask(ctx, pool, promoID, taskID); err != nil {
				return stats, err
			}
			if err := setProvenance(ctx, ex, v, taskID); err != nil {
				return stats, err
			}
		}
		count(&stats, d)
	}
	return stats, nil
}

func count(stats *Stats, d Decision) {
	switch d.Action {
	case "task":
		stats.Created++
	case "review":
		stats.Review++
	case "attached":
		stats.Attached++
	}
}

// tryLock is classify's TryLock shape on this package's key.
func tryLock(ctx context.Context, pool *pgxpool.Pool) (func(), bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("promote: acquire lock conn: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, AdvisoryLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("promote: pg_try_advisory_lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// context.Background() deliberately: the unlock must happen even when
		// the run was cancelled — that is when the lock most needs releasing.
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, AdvisoryLockKey); err != nil {
			slog.Warn("promote: releasing advisory lock", "key", fmt.Sprintf("0x%X", AdvisoryLockKey), "err", err)
		}
		conn.Release()
	}, true, nil
}

// inbox is criterion 2, verbatim: ok personal-lane verdicts over actionable
// extractions, joined through the message to its LATEST capture decision and
// that decision's project, where the project classifies AND has a cutover AND
// the verdict was recorded after it, and no promotion row claims the message.
// Oldest verdict first — the ordering criterion 9's one-pass attach depends on.
//
// The residue lane is excluded twice over (criterion 3): worker_type='classify'
// by name, and structurally by the INNER join — an unmatched decision has
// project_id NULL by 0015's CHECK, so it joins to no project at all.
func inbox(ctx context.Context, pool *pgxpool.Pool, limit int) ([]verdictRow, error) {
	q := `
	SELECT e.id, e.raw_source_item_id, e.fields,
	       nm.id, nm.thread_id, nm.sent_at, r.created_at,
	       p.id, p.slug
	  FROM ai_extractions e
	  JOIN ai_runs r ON r.id = e.ai_run_id
	       AND r.worker_type = 'classify' AND r.status = 'ok'
	  JOIN normalized_messages nm ON nm.raw_source_item_id = e.raw_source_item_id
	  JOIN LATERAL (SELECT cd.project_id FROM capture_decisions cd
	                 WHERE cd.message_id = nm.id
	                 ORDER BY cd.id DESC LIMIT 1) latest ON true
	  JOIN projects p ON p.id = latest.project_id
	       AND p.ai_classify
	       AND p.classify_promote_after IS NOT NULL
	       AND r.created_at >= p.classify_promote_after
	 WHERE e.fields->>'actionable' = 'true'
	   AND NOT EXISTS (SELECT 1 FROM classify_promotions cp
	                    WHERE cp.normalized_message_id = nm.id)
	 ORDER BY r.created_at ASC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT $1`
		args = append(args, limit)
	}

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("promote: select inbox: %w", err)
	}
	defer rows.Close()

	var out []verdictRow
	for rows.Next() {
		var (
			v       Verdict
			raw     []byte
			rawItem *int64
			sentAt  *time.Time
		)
		if err := rows.Scan(&v.ExtractionID, &rawItem, &raw,
			&v.MessageID, &v.ThreadID, &sentAt, &v.RunAt,
			&v.ProjectID, &v.ProjectSlug); err != nil {
			return nil, fmt.Errorf("promote: scan inbox row: %w", err)
		}
		v.RawItemID = rawItem
		v.SentAt = sentAt

		var f struct {
			Kind      string  `json:"kind"`
			Title     string  `json:"title"`
			Reason    string  `json:"reason"`
			Sender    string  `json:"sender"`
			Subject   string  `json:"subject"`
			ProjectID int64   `json:"project_id"`
			LinkURL   *string `json:"link_url"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			// A verdict the store accepted but this pass cannot read is a real
			// error, not a skip: the claim would otherwise never be taken and
			// the message would re-surface every pass forever. KNOWN BLAST
			// RADIUS (go-reviewer, 2026-09-09): because this aborts the pass,
			// one malformed row stalls the WHOLE lane until it is fixed by
			// hand — chosen over a silent skip on purpose (classify writes
			// this shape; a stall is visible, a skip is forever).
			return nil, fmt.Errorf("promote: parse verdict fields for extraction %d: %w", v.ExtractionID, err)
		}
		v.Kind, v.Title, v.Reason = f.Kind, f.Title, f.Reason
		v.Sender, v.Subject = f.Sender, f.Subject
		v.StoredProjectID = f.ProjectID
		if f.LinkURL != nil {
			v.LinkURL = *f.LinkURL
		}
		out = append(out, verdictRow{v: v})
	}
	return out, rows.Err()
}

// threadTask finds the thread's oldest OPEN task IN THE VERDICT'S PROJECT (the
// attach target), and — when there is none — the oldest finished one, so the
// Q3 fall-through can name it in the promotion row's reason. A nil thread
// means neither.
//
// Project-scoped ON PURPOSE (go-reviewer, 2026-09-09; a SPEC amendment):
// task_set_source_thread's other caller is the capture engine, which creates
// tasks in CLIENT projects. Without the project clause, a thread carrying a
// client task could absorb a personal verdict's log line — kind, sender,
// model-authored title — into a non-local_only project, exactly the boundary
// the institutional-knowledge entry says holds. Latent today (no prod task
// carries source_thread_id yet) but the clause is one line and the leak is
// silent.
func threadTask(ctx context.Context, pool *pgxpool.Pool, threadID *int64, projectID int64) (openTask, finished *ExistingTask, err error) {
	if threadID == nil {
		return nil, nil, nil
	}
	var t ExistingTask
	err = pool.QueryRow(ctx,
		`SELECT id, status FROM tasks
		  WHERE source_thread_id = $1 AND project_id = $2 AND status NOT IN ('closed','delivered')
		  ORDER BY id LIMIT 1`, *threadID, projectID).Scan(&t.ID, &t.Status)
	switch {
	case err == nil:
		return &t, nil, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to the finished lookup
	default:
		return nil, nil, fmt.Errorf("promote: find open task for thread %d: %w", *threadID, err)
	}

	var f ExistingTask
	err = pool.QueryRow(ctx,
		`SELECT id, status FROM tasks WHERE source_thread_id = $1 AND project_id = $2 ORDER BY id LIMIT 1`,
		*threadID, projectID).Scan(&f.ID, &f.Status)
	switch {
	case err == nil:
		return nil, &f, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("promote: find finished task for thread %d: %w", *threadID, err)
	}
}

// decisionReason composes the promotion row's reason. Two facts are recorded
// when present: a re-attribution (criterion 16 — BOTH project ids, because the
// disagreement is the interesting fact) and a Q3 fall-through past a finished
// task (the closed task's id, because "why is there a second task on this
// thread" must be answerable from the row that made the decision).
func decisionReason(v Verdict, openTask, finished *ExistingTask) string {
	var parts []string
	if v.StoredProjectID != 0 && v.StoredProjectID != v.ProjectID {
		parts = append(parts, fmt.Sprintf(
			"re-attributed after classification: verdict stored project_id %d, current attribution %d",
			v.StoredProjectID, v.ProjectID))
	}
	if openTask == nil && finished != nil {
		parts = append(parts, fmt.Sprintf(
			"thread's task %d is %s; created a new task (Q3: a thread yields at most one open task)",
			finished.ID, finished.Status))
	}
	return strings.Join(parts, "; ")
}

// claim inserts the promotion row — the claim-before-act half of criterion 12.
// A conflict means someone (or an earlier verdict in this pass) already owns
// the message: the caller must NOT act (criterion 11).
func claim(ctx context.Context, pool *pgxpool.Pool, v Verdict, d Decision, reason string) (int64, bool, error) {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO classify_promotions
		   (normalized_message_id, raw_source_item_id, ai_extraction_id, project_id, kind, action, reason)
		 VALUES ($1,$2,$3,$4,$5,$6, NULLIF($7,''))
		 ON CONFLICT (normalized_message_id) DO NOTHING
		 RETURNING id`,
		v.MessageID, v.RawItemID, v.ExtractionID, v.ProjectID, v.Kind, d.Action, reason).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("promote: claim message %d: %w", v.MessageID, err)
	}
	return id, true, nil
}

// recordTask completes the claim with the task the executor produced (or the
// attach target). The ONLY other direct write this package performs.
func recordTask(ctx context.Context, pool *pgxpool.Pool, promoID, taskID int64) error {
	if _, err := pool.Exec(ctx,
		`UPDATE classify_promotions SET task_id = $2 WHERE id = $1`, promoID, taskID); err != nil {
		return fmt.Errorf("promote: record task %d on promotion %d: %w", taskID, promoID, err)
	}
	return nil
}

// createVerdictTask is criterion 15: every value COPIED from the stored
// verdict, never generated — there is no model call anywhere in this package.
func createVerdictTask(ctx context.Context, ex *executor.Executor, v Verdict, d Decision) (int64, error) {
	args, err := json.Marshal(map[string]any{
		"project":       v.ProjectSlug,
		"title":         taskTitle(v),
		"body":          taskBody(v),
		"assignee_type": "human", // D6: personal has client NULL, so no worker queue can see it anyway
		"priority":      0,
		"status":        d.Status,
	})
	if err != nil {
		return 0, fmt.Errorf("promote: marshal create_task args for message %d: %w", v.MessageID, err)
	}
	res, err := ex.Execute(ctx, executor.Call{Tool: "create_task", Actor: Actor, Args: args})
	if err != nil {
		return 0, fmt.Errorf("promote: create task for message %d: %w", v.MessageID, err)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return 0, fmt.Errorf("promote: parse create_task result for message %d: %w", v.MessageID, err)
	}
	if out.TaskID == 0 {
		return 0, fmt.Errorf("promote: create_task returned no task id for message %d", v.MessageID)
	}
	return out.TaskID, nil
}

// appendVerdictLog attaches a follow-up verdict to the thread's open task: ONE
// task_append_log through the executor, nothing else (criterion 9 — "no second
// task, no status change").
func appendVerdictLog(ctx context.Context, ex *executor.Executor, v Verdict, taskID int64) error {
	args, err := json.Marshal(map[string]any{
		"task_id": taskID,
		"kind":    "log",
		"message": fmt.Sprintf("promote: %s — message %d from %s: %s",
			v.Kind, v.MessageID, orNone(v.Sender), textmatch.NormalizedPrefix(v.Title, titleLimit)),
	})
	if err != nil {
		return fmt.Errorf("promote: marshal task_append_log args for task %d: %w", taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_append_log", Actor: Actor, Args: args, TaskID: &taskID,
	}); err != nil {
		return fmt.Errorf("promote: append log to task %d (message %d): %w", taskID, v.MessageID, err)
	}
	return nil
}

// setProvenance records which conversation raised the task — what makes the
// NEXT message on the thread attach instead of duplicating (criterion 9's
// lookup is inert without it). Skipped when the message has no thread:
// provenance is an observation, never an invention.
func setProvenance(ctx context.Context, ex *executor.Executor, v Verdict, taskID int64) error {
	if v.ThreadID == nil {
		return nil
	}
	args, err := json.Marshal(map[string]any{"task_id": taskID, "thread_id": *v.ThreadID})
	if err != nil {
		return fmt.Errorf("promote: marshal task_set_source_thread args for task %d: %w", taskID, err)
	}
	if _, err := ex.Execute(ctx, executor.Call{
		Tool: "task_set_source_thread", Actor: Actor, Args: args, TaskID: &taskID,
	}); err != nil {
		return fmt.Errorf("promote: set source thread on task %d: %w", taskID, err)
	}
	return nil
}

// taskTitle is the verdict's stored title, truncated with the ONE spelling.
// create_task refuses an empty title, so the fallbacks are the subject and then
// the kind — still copies, never compositions.
func taskTitle(v Verdict) string {
	if t := textmatch.NormalizedPrefix(v.Title, titleLimit); t != "" {
		return t
	}
	if s := textmatch.NormalizedPrefix(v.Subject, titleLimit); s != "" {
		return s
	}
	return "classify: " + v.Kind
}

// taskBody is criterion 15's deterministic block: kind, sender, subject,
// sent_at, the ids, the verdict reason, and the resolved link when the verdict
// carries one.
func taskBody(v Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kind: %s\n", v.Kind)
	fmt.Fprintf(&b, "sender: %s\n", orNone(v.Sender))
	fmt.Fprintf(&b, "subject: %s\n", orNone(v.Subject))
	if v.SentAt != nil {
		fmt.Fprintf(&b, "sent_at: %s\n", v.SentAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "normalized_message_id: %d\n", v.MessageID)
	fmt.Fprintf(&b, "ai_extraction_id: %d\n", v.ExtractionID)
	fmt.Fprintf(&b, "verdict: %s\n", orNone(v.Reason))
	if v.LinkURL != "" {
		fmt.Fprintf(&b, "link: %s\n", v.LinkURL)
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
