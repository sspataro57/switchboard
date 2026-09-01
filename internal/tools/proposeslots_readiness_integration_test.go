//go:build integration

package tools_test

// Integration tests for propose_slots' refusal (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 8, 9 and 11).
// Build-tagged `integration` AND env-gated on DATABASE_URL. Every call goes
// through executor.Execute with the REAL policy Matrix — the only route to a
// handler (invariant 3) — so the audit row this ticket promises is the audit row
// the test reads. NO network, no LLM, no Google credential.
//
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run ProposeSlotsReadiness ./internal/tools/
//
// WHY NOT A UNIT TEST, given the SPEC says "a unit test asserts the refusal for
// the six actor shapes" (criterion 9): the refusal is decided from
// source_accounts and sync_runs, so the handler needs a pool — a nil-pool unit
// call panics before it can refuse. The actor sweep is therefore run here, at
// the executor, where it also proves the second half of the criterion: each
// refusal leaves audit_events.status='error' regardless of who asked.
//
// THE SIX ACTOR SHAPES ARE NOT DECORATION. IK, "An actor-prefix check is a
// transport label, not a trust boundary": SWT-19 shipped a go-live gate keyed on
// executor.ViaMCP and recorded that it "cannot be crossed by forgetting about
// it". It could — the drafts worker calls the executor directly as `drafts:gpt`
// and is on no MCP transport at all. Criterion 9 says there is NO override, so
// the sweep enumerates every actor shape the repo actually produces and asserts
// the identical refusal for each. If one of them answers, that one is the hole.
//
// Cross-suite discipline (SWT-6 mutual-cleanup pact): the accounts are
// 'itest-swt24-slots-%', the sync_runs rows carry stats->>'itest', and the audit
// rows are removed by id watermark, so nothing foreign is touched. Readiness
// scope is GLOBAL by design (criterion 3), so freshenForeignCalendars makes
// every account this suite does not own current before the "must answer" cases;
// it is the production precondition, set explicitly and removed after.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sspataro57/switchboard/internal/audit"
	"github.com/sspataro57/switchboard/internal/executor"
	"github.com/sspataro57/switchboard/internal/policy"
	"github.com/sspataro57/switchboard/internal/store"
	"github.com/sspataro57/switchboard/internal/tools"
)

const (
	slotsPrefix = "itest-swt24-slots-"
	slotsMarker = "swt24-slots"
)

// The actor shapes that exist in this repo (IK). Every one of them must be
// refused identically.
var slotsActors = []string{
	"dashboard:itest-swt24-slots",
	"opsctl:itest-swt24-slots",
	"mcp:worker:itest-swt24-slots",
	"mcp:manual:itest-swt24-slots",
	"drafts:gpt",
	"worker:itest-swt24-slots",
}

type slotsFixture struct {
	pool     *pgxpool.Pool
	ex       *executor.Executor
	auditMin int64
}

func newSlotsFixture(t *testing.T, ctx context.Context) *slotsFixture {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	if strings.Contains(os.Getenv("DATABASE_URL"), "192.168.50.49") {
		t.Fatal("integration tests must NEVER run against the real ops db; use the compose db on :5433")
	}
	pool, err := store.NewPool(ctx)
	if err != nil {
		t.Fatalf("store.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	reg := executor.NewRegistry()
	tools.Register(reg, pool)
	checker := policy.NewMatrix(policy.NewPGSnapshotLoader(pool), policy.NewStatic(reg.Names()...))

	f := &slotsFixture{pool: pool, ex: executor.New(reg, checker, audit.NewPGStore(pool))}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM audit_events`).Scan(&f.auditMin); err != nil {
		t.Fatalf("audit watermark: %v", err)
	}
	f.cleanup(t, ctx)
	t.Cleanup(func() { f.cleanup(t, context.Background()) })
	return f
}

func (f *slotsFixture) cleanup(t *testing.T, ctx context.Context) {
	t.Helper()
	owned := `(SELECT id FROM source_accounts WHERE account_email LIKE '` + slotsPrefix + `%')`
	stmts := []string{
		`DELETE FROM policy_decisions WHERE audit_event_id IN (SELECT id FROM audit_events WHERE tool='propose_slots' AND id > ` + slotsWatermark(f.auditMin) + `)`,
		`DELETE FROM audit_events WHERE tool='propose_slots' AND id > ` + slotsWatermark(f.auditMin),
		`DELETE FROM sync_runs WHERE stats->>'itest' = '` + slotsMarker + `'`,
		`DELETE FROM sync_runs WHERE source_account_id IN ` + owned,
		`DELETE FROM source_accounts WHERE account_email LIKE '` + slotsPrefix + `%'`,
	}
	for _, s := range stmts {
		if _, err := f.pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func slotsWatermark(n int64) string { return strconv.FormatInt(n, 10) }

// account inserts one provider='google' account in availability scope.
func (f *slotsFixture) account(t *testing.T, ctx context.Context, suffix string) (int64, string) {
	t.Helper()
	email := slotsPrefix + suffix + "@example.com"
	var id int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO source_accounts (provider, account_email, calendar_in_availability, send_enabled)
		 VALUES ('google', $1, true, false) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("insert account %s: %v", email, err)
	}
	return id, email
}

func (f *slotsFixture) calendarRun(t *testing.T, ctx context.Context, accountID int64, status string, ago time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 VALUES ($1, now() - $2::interval, now() - $2::interval, $3,
		         jsonb_build_object('phase','calendar','itest',$4::text))`,
		accountID, ago.String(), status, slotsMarker); err != nil {
		t.Fatalf("insert calendar run: %v", err)
	}
}

func (f *slotsFixture) freshenForeignCalendars(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO sync_runs (source_account_id, started_at, finished_at, status, stats)
		 SELECT id, now(), now(), 'ok', jsonb_build_object('phase','calendar','itest',$2::text)
		   FROM source_accounts
		  WHERE provider='google' AND calendar_in_availability AND account_email NOT LIKE $1`,
		slotsPrefix+"%", slotsMarker); err != nil {
		t.Fatalf("freshen foreign calendars: %v", err)
	}
}

func (f *slotsFixture) lastAudit(t *testing.T, ctx context.Context, actor string) (status, errText string) {
	t.Helper()
	if err := f.pool.QueryRow(ctx,
		`SELECT status, COALESCE(error,'') FROM audit_events
		  WHERE tool='propose_slots' AND actor=$1 ORDER BY id DESC LIMIT 1`, actor).Scan(&status, &errText); err != nil {
		t.Fatalf("read audit row for %s: %v", actor, err)
	}
	return status, errText
}

// ---------------------------------------------------------------------------
// Criteria 8 + 9: every actor is refused, and every refusal is audited as an
// error carrying the reason. The tool NEVER answers {"slots":[]} for a calendar
// it cannot vouch for — an empty array is indistinguishable from "fully
// booked", invisible in an audit row, and impossible to alert on.
// ---------------------------------------------------------------------------

func TestProposeSlotsReadiness_Integration_RefusesForEveryActorShape(t *testing.T) {
	ctx := context.Background()
	f := newSlotsFixture(t, ctx)
	f.freshenForeignCalendars(t, ctx)

	_, email := f.account(t, ctx, "never") // in scope, never synced

	for _, actor := range slotsActors {
		t.Run(actor, func(t *testing.T) {
			res, err := f.ex.Execute(ctx, executor.Call{
				Tool:  "propose_slots",
				Actor: actor,
				Args:  []byte(`{"duration_minutes":30}`),
			})
			if err == nil {
				var out struct {
					Slots []map[string]string `json:"slots"`
				}
				_ = json.Unmarshal(res.Output, &out)
				t.Fatalf("propose_slots ANSWERED for actor %q with an unsynced calendar (%d slots, output %s). "+
					"There is no override and no actor may bypass the refusal — an actor-prefix check is a "+
					"transport label, not a trust boundary (IK)", actor, len(out.Slots), res.Output)
			}
			if !strings.Contains(err.Error(), email) {
				t.Errorf("refusal does not name the account it has no data for (%s): %v", email, err)
			}

			status, errText := f.lastAudit(t, ctx, actor)
			if status != "error" {
				t.Errorf("audit_events.status = %q for actor %q, want \"error\" (a refusal travels the whole "+
					"executor path: validate -> policy -> audit start -> handler -> audit complete)", status, actor)
			}
			if !strings.Contains(errText, email) {
				t.Errorf("audit_events.error does not carry the reason for actor %q: %q", actor, errText)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 11: AVAIL_MAX_SYNC_AGE, default 1h, read only in the tool wiring.
// The default matters — four missed */15 polls before the service goes quiet —
// and a typo must not widen a safety window silently.
// ---------------------------------------------------------------------------

func TestProposeSlotsReadiness_Integration_MaxSyncAgeEnv(t *testing.T) {
	ctx := context.Background()
	f := newSlotsFixture(t, ctx)
	f.freshenForeignCalendars(t, ctx)

	id, email := f.account(t, ctx, "maxage")
	f.calendarRun(t, ctx, id, "ok", 90*time.Minute) // older than the 1h default, younger than 4h

	const actor = "opsctl:itest-swt24-slots"
	args := []byte(`{"duration_minutes":30}`)

	// Default (env unset): 90 minutes is stale, so this refuses.
	t.Setenv("AVAIL_MAX_SYNC_AGE", "")
	os.Unsetenv("AVAIL_MAX_SYNC_AGE")
	if _, err := f.ex.Execute(ctx, executor.Call{Tool: "propose_slots", Actor: actor, Args: args}); err == nil {
		t.Fatalf("propose_slots answered with a 90-minute-old calendar sync and AVAIL_MAX_SYNC_AGE unset; "+
			"the default is 1h (%s)", email)
	}

	// Explicitly widened: the same fixture now answers. Without this the case
	// above would pass against an implementation that refuses unconditionally.
	t.Setenv("AVAIL_MAX_SYNC_AGE", "4h")
	res, err := f.ex.Execute(ctx, executor.Call{Tool: "propose_slots", Actor: actor, Args: args})
	if err != nil {
		t.Fatalf("propose_slots refused with AVAIL_MAX_SYNC_AGE=4h and a 90-minute-old sync: %v", err)
	}
	var out struct {
		Slots []map[string]string `json:"slots"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("propose_slots output not {slots:[...]}: %v (%s)", err, res.Output)
	}
	if len(out.Slots) == 0 {
		t.Errorf("propose_slots returned zero slots over a week with no busy time; the shape of a successful " +
			"answer is unchanged by this ticket")
	}

	// An unparseable value is an ERROR returned to the caller, never a silent
	// fallback to the default: "720" is this repo's known trap spelling (the
	// capture-rules horizon), and a typo that quietly restores a 1h window is
	// indistinguishable from a working configuration.
	t.Setenv("AVAIL_MAX_SYNC_AGE", "720")
	_, err = f.ex.Execute(ctx, executor.Call{Tool: "propose_slots", Actor: actor, Args: args})
	if err == nil {
		t.Fatalf("propose_slots accepted AVAIL_MAX_SYNC_AGE=720 (not a Go duration) and answered anyway")
	}
	if !strings.Contains(err.Error(), "AVAIL_MAX_SYNC_AGE") {
		t.Errorf("the unparseable-duration error must name the variable: %v", err)
	}
	if status, _ := f.lastAudit(t, ctx, actor); status != "error" {
		t.Errorf("audit_events.status = %q after a bad AVAIL_MAX_SYNC_AGE, want \"error\"", status)
	}
}
