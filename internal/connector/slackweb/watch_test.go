package slackweb

// slack-watch-sweep (SWT-75, docs/tickets/slack-watch-sweep_SPEC.md) Part 3 and
// Part 4: the resident loop's two cadences, its pass bound, and the rule that
// makes it survivable — "any bridge error — 503, 500, EOF, a killed process —
// is a skipped pass, never a failed process" (D10).
//
// ZERO I/O: a fake clock whose Sleep advances it, a fake pass function, a fake
// Source and a fake Sink. No browser, no database, no broker, no goroutines but
// the test's own.
//
// IMPOSED SURFACE (D3, D5, D10 and criteria 9-14 fix the behaviour; the
// identifiers are chosen here, because the SPEC names only the file —
// internal/connector/slackweb/watch.go, "the loop: pass scheduling, the two
// pass kinds, backoff, bridge-busy handling, the counter line"):
//
//	type PassKind string
//	const (
//		PassTargeted PassKind = "targeted"   // the counter line's pass= value
//		PassRotation PassKind = "rotation"
//	)
//
//	type WatchConfig struct {
//		Interval         time.Duration // SLACK_WATCH_INTERVAL,       default 60s
//		RotationInterval time.Duration // SLACK_ROTATION_INTERVAL,    default 30m
//		BudgetMS         int           // SLACK_WATCH_BUDGET_MS,      default 150000
//		BridgeGrace      time.Duration // SLACK_BRIDGE_GRACE,         default 120s
//		HealthAddr       string        // SLACK_WATCH_HEALTH_ADDR,    default ":8093"
//	}
//	func WatchConfigFromEnv() WatchConfig          // positiveEnv discipline throughout
//	func (c WatchConfig) PassTimeout() time.Duration // budget + grace (D5)
//
//	type WatchDeps struct {
//		Now   func() time.Time
//		Sleep func(ctx context.Context, d time.Duration) error
//		Pass  func(ctx context.Context, kind PassKind) error
//	}
//	type Watcher struct{ ... }
//	func NewWatcher(cfg WatchConfig, deps WatchDeps) *Watcher
//	func (w *Watcher) Run(ctx context.Context) error // nil when ctx ends (criterion 25)
//	func (w *Watcher) LastPassAt() time.Time         // last COMPLETED pass — /healthz's input
//	func (w *Watcher) Skipped() int                  // passes skipped by a bridge failure
//
//	// Schedule, pinned by TestWatcher_TwoCadencesNeverOverlap: the first targeted
//	// pass runs at startup; the first rotation one RotationInterval later.
//	// Passes are strictly sequential — concurrency here means 503 (D3).
//
//	// http_bridge.go: the leaf's /export 503 is the browser queue refusing a
//	// second sweep (job-queue.ts:144-152). It is a SKIP, with the leaf's own
//	// Retry-After.
//	var ErrBridgeBusy error
//	type BridgeBusyError struct {
//		RetryAfter time.Duration
//		Body       string
//	}   // errors.Is(err, ErrBridgeBusy) is true
//
//	// One targeted pass, end to end. Refuses an empty target set WITHOUT
//	// touching the bridge (criterion 6), refuses a response that is not
//	// coverage.mode "targeted" (criterion 7), and otherwise ingests raw-first
//	// through the SAME upsertObservation path under PhaseSlackWebWatch.
//	var ErrNoTargets error
//	func RunTargeted(ctx context.Context, source Source, sink Sink, rows []WatchRow, cfg WatchConfig) (Stats, error)
//
//	// D6's volume discipline, as a pure decision: a quiet targeted pass writes
//	// NO sync_runs row. prevStatus is the account+phase's latest run status
//	// ("" when there is none).
//	func WriteRunRow(stats Stats, passErr error, prevStatus string) bool
//
//	// ingest.go: the request and the phase become inputs.
//	type IngestOptions struct {
//		Request ExportRequest
//		Phase   string
//		Quiet   bool // D6: write a run row only when something changed or failed
//	}
//	func IngestWith(ctx context.Context, source Source, sink Sink, opts IngestOptions) (Stats, error)
//	// Sink.StartRun gains the phase: StartRun(ctx, accountID, startedAt, phase)
//
// GREENFIELD NOTE, EXPECTED RED: none of this exists, so the file compile-FAILs
// ("undefined: NewWatcher", "undefined: RunTargeted", ...). The fake sink below
// implements the NEW four-argument StartRun, so it will not satisfy today's
// Sink interface either — that mismatch is the point: ingest_test.go's and
// coverage_test.go's fakes gain the same parameter when the interface changes.
//
// MUTATIONS THAT MUST TURN THIS FILE RED (SPEC "Mutations"):
//   - start a pass while one is in flight            -> criterion 9
//   - make the Go context shorter than budget_ms     -> criterion 11
//   - return an error instead of skipping on a 503   -> criterion 14
//   - write a sync_runs row on every targeted pass   -> criterion 20

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// ---- fake clock ----------------------------------------------------------------

// wtClock is a deterministic clock: Sleep ADVANCES it. The loop therefore runs
// as fast as the CPU allows while the schedule it sees is exact.
type wtClock struct {
	now    time.Time
	sleeps []time.Duration
}

func newWTClock() *wtClock {
	// Far from wall time on purpose: a loop that consults time.Now() instead of
	// the injected clock sees every pass as decades stale and fails the schedule.
	return &wtClock{now: time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)}
}

func (c *wtClock) Now() time.Time { return c.now }

func (c *wtClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d < 0 {
		return fmt.Errorf("negative sleep %s: the loop computed a due time in the past", d)
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

type wtPass struct {
	kind PassKind
	at   time.Time
}

// wtRecorder records passes, detects re-entrancy, and cancels the context after
// `stop` passes so Run returns.
type wtRecorder struct {
	clock    *wtClock
	dur      time.Duration
	stop     int
	cancel   context.CancelFunc
	inFlight bool
	passes   []wtPass
	errs     map[int]error // pass index -> error to return
	t        *testing.T
}

func (r *wtRecorder) Pass(ctx context.Context, kind PassKind) error {
	if r.inFlight {
		r.t.Fatalf("a %s pass started while another was in flight. Criterion 9 / D3: the Mac mini has ONE "+
			"browser and ONE queue, so switchboard must present it with ONE caller — concurrency here means 503",
			kind)
	}
	r.inFlight = true
	defer func() { r.inFlight = false }()
	idx := len(r.passes)
	r.passes = append(r.passes, wtPass{kind: kind, at: r.clock.Now()})
	r.clock.now = r.clock.now.Add(r.dur)
	if len(r.passes) >= r.stop && r.cancel != nil {
		r.cancel()
	}
	return r.errs[idx]
}

func (r *wtRecorder) kinds() []PassKind {
	out := make([]PassKind, 0, len(r.passes))
	for _, p := range r.passes {
		out = append(out, p.kind)
	}
	return out
}

func runWatcher(t *testing.T, cfg WatchConfig, rec *wtRecorder) *Watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec.t = t
	rec.cancel = cancel
	w := NewWatcher(cfg, WatchDeps{Now: rec.clock.Now, Sleep: rec.clock.Sleep, Pass: rec.Pass})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Watcher.Run = %v, want nil when the context ends (criterion 25)", err)
	}
	return w
}

func wtConfig() WatchConfig {
	return WatchConfig{
		Interval:         60 * time.Second,
		RotationInterval: 5 * time.Minute, // short, so the table stays readable
		BudgetMS:         150000,
		BridgeGrace:      120 * time.Second,
		HealthAddr:       ":8093",
	}
}

// ---- criterion 9: the two cadences, never overlapping ---------------------------

// The pass sequence over a fake clock. With instant passes the schedule is
// exact: a targeted pass at startup and every Interval, a rotation every
// RotationInterval, and at an instant when BOTH are due the ROTATION runs —
// it subsumes the targeted read (its export covers every conversation) and
// running both would be two callers at one browser.
func TestWatcher_TwoCadencesNeverOverlap(t *testing.T) {
	cfg := wtConfig()
	clock := newWTClock()
	start := clock.Now()
	rec := &wtRecorder{clock: clock, stop: 12, errs: map[int]error{}}
	runWatcher(t, cfg, rec)

	want := []struct {
		kind  PassKind
		after time.Duration
	}{
		{PassTargeted, 0},
		{PassTargeted, 1 * time.Minute},
		{PassTargeted, 2 * time.Minute},
		{PassTargeted, 3 * time.Minute},
		{PassTargeted, 4 * time.Minute},
		{PassRotation, 5 * time.Minute}, // both due: the rotation wins
		{PassTargeted, 6 * time.Minute},
		{PassTargeted, 7 * time.Minute},
		{PassTargeted, 8 * time.Minute},
		{PassTargeted, 9 * time.Minute},
		{PassRotation, 10 * time.Minute},
		{PassTargeted, 11 * time.Minute},
	}
	if len(rec.passes) != len(want) {
		t.Fatalf("ran %d passes (%v), want %d", len(rec.passes), rec.kinds(), len(want))
	}
	for i, w := range want {
		got := rec.passes[i]
		if got.kind != w.kind || !got.at.Equal(start.Add(w.after)) {
			t.Errorf("pass %d = %s at +%s, want %s at +%s. Criterion 9: one targeted pass per "+
				"SLACK_WATCH_INTERVAL, one rotation per SLACK_ROTATION_INTERVAL, and the first targeted pass "+
				"runs at STARTUP (a watcher that began with a 12-minute rotation would black-hole the watched "+
				"DMs for its first quarter of an hour, which is the thing this ticket exists to stop)",
				i, got.kind, got.at.Sub(start), w.kind, w.after)
		}
	}
}

// A pass that outlives its own interval must not produce a burst of catch-up
// passes: the loop runs the next DUE pass once, it does not replay the ones it
// missed. Two back-to-back targeted passes with nothing between them is the
// browser being hammered.
func TestWatcher_ALongPassDoesNotQueueCatchUpPasses(t *testing.T) {
	cfg := wtConfig()
	clock := newWTClock()
	rec := &wtRecorder{clock: clock, dur: 150 * time.Second, stop: 6, errs: map[int]error{}}
	runWatcher(t, cfg, rec)

	for i := 1; i < len(rec.passes); i++ {
		gap := rec.passes[i].at.Sub(rec.passes[i-1].at)
		if gap < rec.dur {
			t.Fatalf("pass %d started %s after pass %d, which is less than the %s a pass takes — the passes "+
				"overlapped (criterion 9)", i, gap, i-1, rec.dur)
		}
		if rec.passes[i].kind == PassTargeted && rec.passes[i-1].kind == PassTargeted && gap < cfg.Interval {
			t.Errorf("two targeted passes %s apart (< SLACK_WATCH_INTERVAL %s): an overrunning pass must not "+
				"be followed by the passes it missed. 2-4 conversations a minute is already ~3,000-5,700 extra "+
				"navigations a day, and navigation is the documented cause of the renderer wedges (D10)",
				gap, cfg.Interval)
		}
	}
}

// Criterion 25: SIGTERM (an already-cancelled context) starts no pass and exits
// cleanly. The kube session's terminationGracePeriodSeconds is 120 precisely
// because an in-flight pass is allowed to finish; a NEW one is not.
func TestWatcher_CancelledContextStartsNoPass(t *testing.T) {
	clock := newWTClock()
	rec := &wtRecorder{clock: clock, stop: 1000, errs: map[int]error{}, t: t}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := NewWatcher(wtConfig(), WatchDeps{Now: clock.Now, Sleep: clock.Sleep, Pass: rec.Pass})
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run(cancelled ctx) = %v, want nil — criterion 25: SIGTERM is exit code 0", err)
	}
	if len(rec.passes) != 0 {
		t.Errorf("Run(cancelled ctx) ran %v; no pass starts after cancellation (criterion 25)", rec.kinds())
	}
}

// ---- criterion 14: failure is a skipped pass, never a dead process --------------

// Each of the leaf's real failure shapes. Every one leaves the loop running,
// counts a skip, and does NOT refresh the pass-completion window /healthz reads
// (criterion 23: 200 iff a pass COMPLETED).
func TestWatcher_EveryBridgeFailureIsASkippedPass(t *testing.T) {
	cases := map[string]error{
		"503 with Retry-After": &BridgeBusyError{RetryAfter: 30 * time.Second, Body: "queue busy"},
		"500 from the leaf":    errors.New("Slack bridge /export returned 500: SLACK_UI_CHANGED"),
		"EOF mid-response":     fmt.Errorf("read Slack bridge /export: %w", io.ErrUnexpectedEOF),
		"dial failure":         fmt.Errorf("call Slack bridge /export: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}),
		"unparseable body":     fmt.Errorf("parse Slack bridge export: %w", &json.SyntaxError{}),
		"not targeted":         fmt.Errorf("workspace T0360B84U: %w", ErrNotTargeted),
	}
	for name, passErr := range cases {
		passErr := passErr
		t.Run(name, func(t *testing.T) {
			clock := newWTClock()
			start := clock.Now()
			// The first two passes fail; the loop must reach a third.
			rec := &wtRecorder{clock: clock, stop: 3, errs: map[int]error{0: passErr, 1: passErr}}
			w := runWatcher(t, wtConfig(), rec)

			if len(rec.passes) != 3 {
				t.Fatalf("the loop stopped after %d passes (%v); D10/criterion 14: any bridge error is a "+
					"SKIPPED PASS, never a failed process — restarting the pod cannot fix Chrome", len(rec.passes), rec.kinds())
			}
			if w.Skipped() != 2 {
				t.Errorf("Skipped() = %d after two failing passes, want 2: the skip is COUNTED, so a mini that "+
					"is wedged all day is visible in the logs rather than silent", w.Skipped())
			}
			if !w.LastPassAt().Equal(rec.passes[2].at) {
				t.Errorf("LastPassAt() = %s after two failures and one success, want the SUCCESS at %s. "+
					"Criterion 23 reads this as \"a pass COMPLETED\"; a failed pass that refreshed it would "+
					"make /healthz green on a bridge that has answered nothing for hours",
					w.LastPassAt().Sub(start), rec.passes[2].at.Sub(start))
			}
		})
	}
}

// Criterion 14's last sentence: "A 503 additionally sleeps until
// min(Retry-After, interval)". Longer than the interval would idle the watch
// list for no reason; shorter would hammer a queue that told us when to return.
func TestWatcher_A503SleepsUntilMinOfRetryAfterAndInterval(t *testing.T) {
	cfg := wtConfig() // Interval 60s
	for _, tc := range []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"leaf asks for longer than the interval", 9 * time.Minute, 60 * time.Second},
		{"leaf asks for less", 5 * time.Second, 5 * time.Second},
		{"leaf sent no Retry-After", 0, 60 * time.Second},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			clock := newWTClock()
			rec := &wtRecorder{clock: clock, stop: 2, errs: map[int]error{
				0: &BridgeBusyError{RetryAfter: tc.retryAfter, Body: "sweep already running"},
			}}
			runWatcher(t, cfg, rec)

			if len(rec.passes) != 2 {
				t.Fatalf("passes = %v, want two", rec.kinds())
			}
			if got := rec.passes[1].at.Sub(rec.passes[0].at); got != tc.want {
				t.Errorf("after a 503 with Retry-After %s the next pass came %s later, want %s = "+
					"min(Retry-After, SLACK_WATCH_INTERVAL) (criterion 14)", tc.retryAfter, got, tc.want)
			}
		})
	}
}

// A 503 from /export is ErrBridgeBusy, so the loop can tell "the browser is
// busy" from "the leaf broke" without reading a status code out of a string.
func TestBridgeBusyError_IsErrBridgeBusy(t *testing.T) {
	err := error(&BridgeBusyError{RetryAfter: 42 * time.Second, Body: "busy"})
	if !errors.Is(err, ErrBridgeBusy) {
		t.Errorf("errors.Is(%v, ErrBridgeBusy) = false; the sentinel is what a caller that only wants \"skip "+
			"this pass\" matches on", err)
	}
	var busy *BridgeBusyError
	if !errors.As(err, &busy) || busy.RetryAfter != 42*time.Second {
		t.Errorf("errors.As lost the Retry-After: %v", err)
	}
}

// ---- criterion 11: the pass bound ----------------------------------------------

// D5: the leaf-side bound is budget_ms — "the only bound that stops browser
// work cleanly" — and the Go context must be able to fire ONLY after the leaf
// has already given up. monitorAbandonedExport (export-lifecycle.ts:15-29)
// TERMINATES the bridge process when an /export caller disconnects, so a short
// client deadline on a per-minute pass is a per-minute bridge suicide.
func TestWatchConfig_PassTimeoutIsNeverShorterThanTheLeafsBudget(t *testing.T) {
	for _, tc := range []struct {
		budgetMS int
		grace    time.Duration
	}{
		{150000, 120 * time.Second},
		{150000, 0},
		{900000, 120 * time.Second},
		{1000, time.Second},
	} {
		cfg := WatchConfig{BudgetMS: tc.budgetMS, BridgeGrace: tc.grace}
		budget := time.Duration(tc.budgetMS) * time.Millisecond
		got := cfg.PassTimeout()
		if got < budget {
			t.Errorf("PassTimeout() = %s for budget_ms=%d: the Go context would cut the HTTP connection while "+
				"the leaf is still working, and monitorAbandonedExport then EXITS the bridge process — every "+
				"minute (D5, criterion 11)", got, tc.budgetMS)
		}
		if want := budget + tc.grace; got != want {
			t.Errorf("PassTimeout() = %s, want budget + SLACK_BRIDGE_GRACE = %s (criterion 11)", got, want)
		}
	}
}

// ---- criterion 13's numbers: the env knobs -------------------------------------

func TestWatchConfigFromEnv_DefaultsAreSalvadorsNumbers(t *testing.T) {
	for _, k := range []string{"SLACK_WATCH_INTERVAL", "SLACK_ROTATION_INTERVAL", "SLACK_WATCH_BUDGET_MS",
		"SLACK_BRIDGE_GRACE", "SLACK_WATCH_HEALTH_ADDR"} {
		t.Setenv(k, "")
	}
	cfg := WatchConfigFromEnv()
	if cfg.Interval != 60*time.Second {
		t.Errorf("Interval = %s, want 60s (\"I want to constantly sweep jose an katie like every minute\")", cfg.Interval)
	}
	if cfg.RotationInterval != 30*time.Minute {
		t.Errorf("RotationInterval = %s, want 30m (\"the full export can then be half hour or so\")", cfg.RotationInterval)
	}
	if cfg.BudgetMS != 150000 {
		t.Errorf("BudgetMS = %d, want 150000 (D3's SLACK_WATCH_BUDGET_MS default)", cfg.BudgetMS)
	}
	if cfg.BridgeGrace != 120*time.Second {
		t.Errorf("BridgeGrace = %s, want 120s (D5)", cfg.BridgeGrace)
	}
	if cfg.HealthAddr != ":8093" {
		t.Errorf("HealthAddr = %q, want \":8093\" — hooksd owns :8090, orchestratord :8091 and SWT-73's mail "+
			"watcher :8092 (D9)", cfg.HealthAddr)
	}
}

// export_request.go:69-82's rule, one level up: "an unparseable or non-positive
// value falls back, never produces a zero the leaf would reject". A zero
// budget_ms 500s the export; a zero interval is a loop that never idles.
func TestWatchConfigFromEnv_JunkFallsBackToTheDefaults(t *testing.T) {
	for _, junk := range []string{"720", "0", "-5s", "", "soon"} {
		junk := junk
		t.Run(junk, func(t *testing.T) {
			t.Setenv("SLACK_WATCH_INTERVAL", junk)
			t.Setenv("SLACK_WATCH_BUDGET_MS", junk)
			t.Setenv("SLACK_BRIDGE_GRACE", junk)
			cfg := WatchConfigFromEnv()
			if cfg.Interval <= 0 || cfg.BudgetMS <= 0 || cfg.BridgeGrace <= 0 {
				t.Errorf("SLACK_WATCH_* = %q gave Interval=%s BudgetMS=%d Grace=%s; a non-positive value must "+
					"fall back, never reach the leaf (\"720\" is the realistic typo — it is not a Go duration)",
					junk, cfg.Interval, cfg.BudgetMS, cfg.BridgeGrace)
			}
		})
	}
}

// D10's last escape hatch, and the milder rollback the SPEC names first:
// SLACK_WATCH_INTERVAL=0 turns targeted passes OFF and leaves the rotation
// running, with no image roll.
func TestWatchConfigFromEnv_ZeroIntervalIsNotAZeroSleep(t *testing.T) {
	t.Setenv("SLACK_WATCH_INTERVAL", "0")
	if cfg := WatchConfigFromEnv(); cfg.Interval == 0 {
		t.Errorf("Interval = 0 would make the loop spin. D10's rollback (\"SLACK_WATCH_INTERVAL=0, targeted " +
			"passes off, rotation only\") must be expressed as a DISABLED cadence, never as a zero sleep")
	}
}

// ---- criterion 6 and 7 at the pass level ---------------------------------------

type wtSource struct {
	calls    int
	lastReq  ExportRequest
	response Export
	err      error
}

func (s *wtSource) Export(_ context.Context, req ExportRequest) (Export, error) {
	s.calls++
	s.lastReq = req
	return s.response, s.err
}

// wtSink implements the NEW Sink: StartRun takes the phase (D6).
type wtSink struct {
	phases   []string
	statuses []string
	inserts  int
	updates  int
	runs     int
}

func (s *wtSink) EnsureAccount(context.Context, Workspace) (int64, error) { return 11, nil }
func (s *wtSink) StartRun(_ context.Context, _ int64, _ time.Time, phase string) (int64, error) {
	s.phases = append(s.phases, phase)
	s.runs++
	return 21, nil
}
func (s *wtSink) KnownConversations(context.Context) ([]KnownConversationRow, error) { return nil, nil }
func (s *wtSink) WatchTargets(context.Context) ([]WatchRow, error)                   { return nil, nil }
func (s *wtSink) RawHash(context.Context, int64, string) (string, bool, error)       { return "", false, nil }
func (s *wtSink) InsertRaw(context.Context, int64, string, json.RawMessage, string) error {
	s.inserts++
	return nil
}
func (s *wtSink) UpdateRaw(context.Context, int64, string, json.RawMessage, string) error {
	s.updates++
	return nil
}
func (s *wtSink) FinishRun(_ context.Context, _ int64, status string, _ Stats, _ string) error {
	s.statuses = append(s.statuses, status)
	return nil
}

func wtTargetedExport(mode string) Export {
	return Export{SchemaVersion: SchemaVersion, Workspaces: []Workspace{{
		ID: "T0360B84U", Name: "Avviato", URL: "https://app.slack.com/client/T0360B84U", OwnUserID: "UOWN0001",
		Conversations: []Conversation{{
			ID: "DSAV4HJ2F", Name: "jose", Type: "dm", URL: "https://app.slack.com/client/T0360B84U/DSAV4HJ2F",
			Messages: []Message{{ID: "p1789000000000001", Timestamp: "2026-09-22T09:00:00Z",
				Author: "José", AuthorID: "UJOSE001", Text: "can you look at the invoice"}},
		}},
		Read:     []string{"DSAV4HJ2F"},
		Coverage: &Coverage{EnumeratedCount: 1, ReadCount: 1, ElapsedMS: 18000, Mode: mode},
	}}}
}

// Criterion 6: "an empty enabled set means no targeted pass is issued at all
// (no HTTP call)". The browser is the scarcest resource in this system; asking
// it for nothing still costs a queue slot and a navigation.
func TestRunTargeted_EmptyWatchListNeverTouchesTheBridge(t *testing.T) {
	src := &wtSource{response: wtTargetedExport(CoverageModeTargeted)}
	sink := &wtSink{}
	_, err := RunTargeted(context.Background(), src, sink, nil, wtConfig())
	if !errors.Is(err, ErrNoTargets) {
		t.Errorf("RunTargeted(no rows) = %v, want ErrNoTargets (criterion 6)", err)
	}
	if src.calls != 0 {
		t.Errorf("RunTargeted(no rows) called /export %d times; an empty watch list must not reach the bridge "+
			"at all (criterion 6)", src.calls)
	}
	if sink.runs != 0 {
		t.Errorf("RunTargeted(no rows) opened %d sync_runs rows; a pass that never happened writes nothing "+
			"(D6 volume discipline)", sink.runs)
	}
}

// Criterion 7: a response that does not report coverage.mode "targeted" is
// REFUSED — nothing is ingested from it. MUTATION: make the fake leaf answer
// "full" and this must be the test that goes red.
func TestRunTargeted_RefusesAResponseThatIsNotTargeted(t *testing.T) {
	rows := []WatchRow{watchRow("T0360B84U", "DSAV4HJ2F", true)}
	for _, mode := range []string{CoverageModeFull, ""} {
		mode := mode
		t.Run("mode="+mode, func(t *testing.T) {
			src := &wtSource{response: wtTargetedExport(mode)}
			sink := &wtSink{}
			_, err := RunTargeted(context.Background(), src, sink, rows, wtConfig())
			if err == nil || !errors.Is(err, ErrNotTargeted) {
				t.Fatalf("RunTargeted against a leaf answering mode=%q = %v, want ErrNotTargeted: an old leaf "+
					"silently ignores unknown keys and would answer every minute with a 15-minute FULL export "+
					"(criterion 7, D8)", mode, err)
			}
			if sink.inserts != 0 || sink.updates != 0 {
				t.Errorf("a refused response still wrote %d raw rows; criterion 7 says nothing is ingested from it",
					sink.inserts+sink.updates)
			}
		})
	}
}

// The happy path: the SAME raw-first path, under the watch phase (D6, invariant 1).
func TestRunTargeted_IngestsRawFirstUnderTheWatchPhase(t *testing.T) {
	rows := []WatchRow{watchRow("T0360B84U", "DSAV4HJ2F", true)}
	src := &wtSource{response: wtTargetedExport(CoverageModeTargeted)}
	sink := &wtSink{}
	stats, err := RunTargeted(context.Background(), src, sink, rows, wtConfig())
	if err != nil {
		t.Fatalf("RunTargeted = %v, want nil", err)
	}
	if src.lastReq.Targets == nil || len(src.lastReq.Known) != 0 {
		t.Errorf("the pass asked for %+v; criterion 5: targets, never known", src.lastReq)
	}
	if src.lastReq.BudgetMS != 150000 {
		t.Errorf("budget_ms = %d, want SLACK_WATCH_BUDGET_MS (150000): the leaf-side budget is the only bound "+
			"that stops browser work cleanly (D5)", src.lastReq.BudgetMS)
	}
	if stats.RawInserted != 2 { // the conversation row and its one message
		t.Errorf("RawInserted = %d, want 2 (the conversation observation and the message). Invariant 1: a "+
			"targeted pass writes through the SAME upsertObservation path, raw JSON plus content hash, BEFORE "+
			"normalize", stats.RawInserted)
	}
	if len(sink.phases) != 1 || sink.phases[0] != PhaseSlackWebWatch {
		t.Errorf("StartRun phases = %v, want exactly [%s] (criterion 17 / D6: the watch phase is invisible to "+
			"ReconcileUnconfirmed and KnownConversations)", sink.phases, PhaseSlackWebWatch)
	}
}

// Criterion 15: a conversation the leaf reports `unreadable` does not fail the
// pass. What WAS read is ingested raw-first and the run is `partial`
// (ingest.go:104-108, unchanged by this ticket — and pre-check 0c confirmed
// `partial` is HONEST today: every run defers 14-24 conversations with
// unreadable_count 0).
func TestRunTargeted_AnUnreadableConversationDoesNotFailThePass(t *testing.T) {
	export := wtTargetedExport(CoverageModeTargeted)
	export.Workspaces[0].Unreadable = []UnreadableConversation{
		{ID: "D04F7LXRB8B", Name: "katie", Code: "SLACK_READ_FAILED", Reason: "the conversation did not render"},
	}
	export.Workspaces[0].Coverage.UnreadableCount = 1

	src := &wtSource{response: export}
	sink := &wtSink{}
	stats, err := RunTargeted(context.Background(), src, sink,
		[]WatchRow{watchRow("T0360B84U", "DSAV4HJ2F", true), watchRow("T0360B84U", "D04F7LXRB8B", true)}, wtConfig())
	if err != nil {
		t.Fatalf("RunTargeted with one unreadable conversation = %v, want nil: one conversation the browser "+
			"could not open must not cost the other its ingest (criterion 15)", err)
	}
	if stats.RawInserted == 0 {
		t.Errorf("nothing was ingested from a pass that DID read one conversation (criterion 15, invariant 1)")
	}
	if len(sink.statuses) != 1 || sink.statuses[0] != "partial" {
		t.Errorf("FinishRun statuses = %v, want [partial] — partialCoverage() holds whenever anything is "+
			"deferred or unreadable, and both consumers already treat partial as success "+
			"(reconcile.go:121, /funnel's last_ok)", sink.statuses)
	}
}

// ---- criterion 20: the run-row volume discipline -------------------------------

// D6: "a targeted pass writes a sync_runs row ONLY when it inserted or updated
// raw rows, when it failed, or on the first success after a failure. A quiet
// watcher writes nothing — 1,440 rows/day/workspace for nothing is worse than
// the 230/day that decision already refused."
func TestWriteRunRow_QuietPassesWriteNothing(t *testing.T) {
	quiet := Stats{WorkspacesSeen: 1, ConversationsSeen: 2, RawUnchanged: 9}
	ingested := Stats{WorkspacesSeen: 1, RawInserted: 1}
	updated := Stats{WorkspacesSeen: 1, RawUpdated: 3}
	boom := errors.New("Slack bridge /export returned 500")

	for _, tc := range []struct {
		name       string
		stats      Stats
		err        error
		prevStatus string
		want       bool
		why        string
	}{
		{"quiet pass after a quiet pass", quiet, nil, "ok", false,
			"nothing changed and nothing failed: 1,440 rows a day for nothing"},
		{"quiet pass, no previous run at all", quiet, nil, "", false,
			"a watcher that has never had anything to say still says nothing"},
		{"a pass that inserted", ingested, nil, "ok", true, "raw rows moved"},
		{"a pass that updated", updated, nil, "ok", true, "raw rows moved"},
		{"a failure", quiet, boom, "ok", true, "a failure always leaves a row, with its error"},
		{"the first success after a failure", quiet, nil, "error", true,
			"the recovery is the one quiet pass worth recording"},
		{"the second success after a failure", quiet, nil, "ok", false,
			"the recovery was already recorded"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := WriteRunRow(tc.stats, tc.err, tc.prevStatus); got != tc.want {
				t.Errorf("WriteRunRow(%s) = %v, want %v — %s (criterion 20, D6)", tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// ---- review follow-ups (2026-09-22) ------------------------------------------------

// A busy ROTATION must not starve the targeted cadence. Before the fix, a
// rotation 503 collapsed both due instants onto the same tick and the
// rotation-wins tie-break then retried the rotation every minute while the
// targeted pass never ran.
func TestWatcher_ABusyRotationDoesNotStarveTargetedPasses(t *testing.T) {
	cfg := wtConfig() // interval 60s, rotation 5m
	clock := newWTClock()
	rec := &wtRecorder{clock: clock, stop: 9, errs: map[int]error{
		5: &BridgeBusyError{RetryAfter: 9 * time.Minute, Body: "sweep already running"}, // the first rotation, at +5m
	}}
	runWatcher(t, cfg, rec)
	kinds := rec.kinds()
	if kinds[5] != PassRotation {
		t.Fatalf("pass 5 = %s, want the rotation at +5m: %v", kinds[5], kinds)
	}
	if kinds[6] != PassTargeted || kinds[7] != PassTargeted {
		t.Errorf("after a busy rotation the next passes were %v; want targeted passes to keep their cadence "+
			"while the rotation retries later (review: rotation-busy starvation)", kinds[6:])
	}
	if got := rec.passes[6].at.Sub(rec.passes[5].at); got != time.Minute {
		t.Errorf("the targeted pass after a busy rotation came %s later, want 1m (its own cadence)", got)
	}
}

// SLACK_WATCH_INTERVAL=0 runs NO targeted pass, not even one at startup.
func TestWatcher_DisabledIntervalRunsNoTargetedPass(t *testing.T) {
	cfg := wtConfig()
	cfg.Interval = disabledTargeted
	clock := newWTClock()
	rec := &wtRecorder{clock: clock, stop: 2, errs: map[int]error{}}
	runWatcher(t, cfg, rec)
	for _, k := range rec.kinds() {
		if k != PassRotation {
			t.Fatalf("with targeted passes disabled the loop ran %v; want rotations only (D10's lever)", rec.kinds())
		}
	}
}

// D8 / criterion 7: a non-targeted answer stands targeted passes down until
// the next COMPLETED rotation, then they resume.
func TestWatcher_NotTargetedStandsDownUntilTheNextRotation(t *testing.T) {
	cfg := wtConfig() // rotation at +5m
	clock := newWTClock()
	rec := &wtRecorder{clock: clock, stop: 4, errs: map[int]error{
		1: fmt.Errorf("targeted pass: %w", ErrNotTargeted), // +1m
	}}
	w := runWatcher(t, cfg, rec)
	kinds := rec.kinds()
	want := []PassKind{PassTargeted, PassTargeted, PassRotation, PassTargeted}
	if len(kinds) != len(want) {
		t.Fatalf("passes = %v, want %v: after the refusal no targeted pass until the rotation at +5m, then one "+
			"at +6m", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("passes = %v, want %v", kinds, want)
		}
	}
	if w.StoodDown() {
		t.Errorf("StoodDown() still true after a completed rotation; the rotation is the probe that lifts it")
	}
}
