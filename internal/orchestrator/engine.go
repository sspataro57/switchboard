package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/fleet"
)

// Actor is the executor identity of every orchestrator action.
const Actor = "orchestrator"

// AdvisoryLockKey guards single-instance operation (pg_try_advisory_lock).
const AdvisoryLockKey = 0x5157_0005 // "switchboard step 5"

// Publisher is the fleet-command surface the applier needs. *fleet.Client
// (via fleet.NewSpineClient) satisfies it.
type Publisher interface {
	PublishCommand(workerID string, cmd fleet.Cmd) error
}

// Engine drains task_events past the cursor, evaluates the pure rules, and
// applies actions through the executor + publisher. NOTIFY is a wake-up only;
// the drain is the sole delivery path.
type Engine struct {
	pool *pgxpool.Pool
	ex   *executor.Executor
	pub  Publisher
	cfg  Config
}

func NewEngine(pool *pgxpool.Pool, ex *executor.Executor, pub Publisher, cfg Config) *Engine {
	return &Engine{pool: pool, ex: ex, pub: pub, cfg: cfg}
}

// DrainOnce processes all events past the cursor, advancing it per event.
// At-least-once: a crash mid-event replays it; the rules' dedup facts make
// replays no-ops.
func (e *Engine) DrainOnce(ctx context.Context) (int, error) {
	processed := 0
	for {
		var cursor int64
		if err := e.pool.QueryRow(ctx,
			`SELECT last_event_id FROM orchestrator_cursor WHERE name='orchestrator'`).Scan(&cursor); err != nil {
			return processed, fmt.Errorf("read cursor: %w", err)
		}

		rows, err := e.pool.Query(ctx,
			`SELECT id, task_id, event_type, payload FROM task_events
			 WHERE id > $1 ORDER BY id LIMIT 200`, cursor)
		if err != nil {
			return processed, fmt.Errorf("select events: %w", err)
		}
		batch := []Event{}
		for rows.Next() {
			var ev Event
			var raw []byte
			if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.Type, &raw); err != nil {
				rows.Close()
				return processed, fmt.Errorf("scan event: %w", err)
			}
			if err := json.Unmarshal(raw, &ev.Payload); err != nil {
				ev.Payload = map[string]any{}
			}
			batch = append(batch, ev)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return processed, fmt.Errorf("iterate events: %w", err)
		}
		if len(batch) == 0 {
			return processed, nil
		}

		for _, ev := range batch {
			ev.Now = time.Now()
			facts, err := e.loadEventFacts(ctx, ev)
			if err != nil {
				return processed, fmt.Errorf("load facts for event %d: %w", ev.ID, err)
			}
			actions := Evaluate(ev, facts, e.cfg)
			e.apply(ctx, ev, actions)
			if _, err := e.pool.Exec(ctx,
				`UPDATE orchestrator_cursor SET last_event_id=$1, updated_at=now()
				 WHERE name='orchestrator' AND last_event_id < $1`, ev.ID); err != nil {
				return processed, fmt.Errorf("advance cursor: %w", err)
			}
			processed++
		}
	}
}

// TickOnce runs the time-based rules (R6 expiry, R7 brief).
func (e *Engine) TickOnce(ctx context.Context, now time.Time) error {
	facts, err := e.loadTickFacts(ctx, now)
	if err != nil {
		return fmt.Errorf("load tick facts: %w", err)
	}
	ev := Event{Type: EventTick, Now: now}
	e.apply(ctx, ev, Evaluate(ev, facts, e.cfg))
	return nil
}

// apply executes actions in order. A failed action is logged (its executor
// audit row records the error) and the drain continues — a single bad event
// must not stall the spine; dedup guards keep any replay safe. One exception:
// a record_orchestration that follows a FAILED create_task is skipped —
// writing the dedup key for a task that was never created would permanently
// suppress that lifecycle task on replay.
func (e *Engine) apply(ctx context.Context, ev Event, actions []Action) {
	var lastCreatedTaskID int64
	createFailed := false
	for _, a := range actions {
		switch a.Kind {
		case ActionExecute:
			if a.Tool == "record_orchestration" && createFailed {
				slog.Warn("skipping decision record after failed create_task; event will replay", "event", ev.ID)
				continue
			}
			args := a.Args
			if a.Tool == "record_orchestration" && lastCreatedTaskID != 0 {
				args = withCreatedTaskID(args, lastCreatedTaskID)
			}
			raw, err := json.Marshal(args)
			if err != nil {
				slog.Error("marshal action args", "tool", a.Tool, "err", err)
				continue
			}
			res, err := e.ex.Execute(ctx, executor.Call{Tool: a.Tool, Actor: Actor, Args: raw})
			if err != nil {
				slog.Error("orchestrator action failed", "tool", a.Tool, "event", ev.ID, "err", err)
				if a.Tool == "create_task" {
					createFailed = true
				}
				continue
			}
			if a.Tool == "create_task" {
				var out struct {
					TaskID int64 `json:"task_id"`
				}
				if err := json.Unmarshal(res.Output, &out); err == nil {
					lastCreatedTaskID = out.TaskID
				}
			}
		case ActionPublish:
			raw, err := json.Marshal(a.PublishArgs)
			if err != nil {
				slog.Error("marshal publish args", "err", err)
				continue
			}
			if err := e.pub.PublishCommand(a.WorkerID, fleet.Cmd{Action: a.PublishVerb, Args: raw}); err != nil {
				slog.Error("orchestrator publish failed", "worker", a.WorkerID, "verb", a.PublishVerb, "err", err)
			}
		}
	}
}

// withCreatedTaskID injects the runtime-created task id into the decision
// record's payload (the pure layer cannot know it).
func withCreatedTaskID(args map[string]any, id int64) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	payload := map[string]any{}
	if p, ok := args["payload"].(map[string]any); ok {
		for k, v := range p {
			payload[k] = v
		}
	}
	payload["created_task_id"] = id
	out["payload"] = payload
	return out
}

// TryAdvisoryLock takes the single-instance lock on a dedicated connection.
// The returned release func must be called on shutdown (or the conn held for
// process lifetime). ok=false means another orchestratord holds it.
func TryAdvisoryLock(ctx context.Context, pool *pgxpool.Pool) (ok bool, release func(), err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("acquire lock conn: %w", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, AdvisoryLockKey).Scan(&ok); err != nil {
		conn.Release()
		return false, nil, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !ok {
		conn.Release()
		return false, nil, nil
	}
	return true, conn.Release, nil
}

// Listen blocks on LISTEN task_events, invoking wake on every notification.
// It returns on ctx cancellation. Errors are returned so the caller can
// reconnect (the drain loop tolerates missed notifications by design).
func (e *Engine) Listen(ctx context.Context, wake func()) error {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listen conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN task_events"); err != nil {
		return fmt.Errorf("LISTEN task_events: %w", err)
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait for notification: %w", err)
		}
		wake()
	}
}
