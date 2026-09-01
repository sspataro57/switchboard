package availability_test

// Pure unit tests for the fail-closed readiness decision (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 4, 5 and 10).
// ZERO network, ZERO Postgres, ZERO env, ZERO clock reads — the package doc
// pins that ("pure functions over calendar intervals — no LLM, no network, no
// clock reads") and criterion 10 makes the readiness decision obey it too:
// NotReady takes (states, now, maxAge) and returns a slice.
//
// WHY THE DECISION IS SPLIT OUT AS A PURE FUNCTION AT ALL. The bug this ticket
// closes is that LoadEvents reads a table nothing writes, Busy(nil) is nil,
// ProposeSlots(nil, cfg) proposes EVERY aligned working-hours slot, and
// propose_slots answers "you are free all week" with total confidence. The fix
// is a refusal, and a refusal that only exists inside a SQL-shaped function is
// a refusal nobody can enumerate the cases of. Everything decidable without a
// database is decided here; store.go only supplies the rows.
//
// GREENFIELD NOTE (historical): none of the surface below existed when this
// file was written, so it compile-FAILed under `go test ./...` — the expected
// red for a SPEC-named greenfield surface — until the SWT-24 implementation
// landed. For greenfield code the SPEC's contract IS the signature. Imposed
// exported surface (SPEC "Files likely to touch": internal/availability/
// availability.go), with the field names chosen here because the SPEC names
// only the function:
//
//	// AccountState is one in-scope calendar account and the last time a
//	// calendar sync SUCCEEDED for it. Scope is source_accounts rows with
//	// provider='google' AND calendar_in_availability (criterion 3); the SQL
//	// that produces these lives in store.go (LoadAccountStates).
//	type AccountState struct {
//	    AccountID        int64
//	    Email            string
//	    LastCalendarSync time.Time // zero value => never synced successfully
//	}
//
//	// NotReady returns the in-scope accounts whose last successful calendar
//	// sync is missing or older than maxAge, in input order. Pure: no clock
//	// read, no env read, no pool (criterion 10).
//	func NotReady(states []AccountState, now time.Time, maxAge time.Duration) []AccountState
//
//	// NotReadyError is the typed, errors.As-able refusal (criterion 5). Its
//	// text names each offending account email and its last successful calendar
//	// sync, or the word "never".
//	type NotReadyError struct {
//	    Accounts []AccountState
//	    MaxAge   time.Duration
//	}
//	func (e *NotReadyError) Error() string
//
// Deliberately NOT tested here: the scope-empty refusal (criterion 5b) and the
// window-coverage refusal (criterion 7). Both are LoadBusy's job — NotReady
// over an empty slice honestly has nothing to report, and pretending otherwise
// would put a refusal in the one place that cannot know whether the SELECT
// returned nothing because no calendar is in scope or because it was never
// asked. See readiness_integration_test.go.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/availability"
)

// now is the injected clock for every case below. It is a plausible wall-clock
// instant on purpose: a NotReady that secretly read time.Now() would still pass
// a test whose "now" was today, so the purity case at the bottom moves it.
var readinessNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func state(id int64, email string, lastSync time.Time) availability.AccountState {
	return availability.AccountState{AccountID: id, Email: email, LastCalendarSync: lastSync}
}

func emails(states []availability.AccountState) []string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, s.Email)
	}
	return out
}

// Criterion 4, as a pure function: READY iff a successful calendar sync landed
// at or after now-maxAge. The three unready shapes are distinct failure modes
// and the error text has to tell them apart, so they are distinct cases.
func TestNotReady_FreshnessRule(t *testing.T) {
	maxAge := time.Hour

	cases := []struct {
		name   string
		states []availability.AccountState
		want   []string
	}{
		{
			name:   "fresh sync is ready",
			states: []availability.AccountState{state(1, "a@example.com", readinessNow.Add(-10*time.Minute))},
			want:   nil,
		},
		{
			name:   "never synced is NOT ready",
			states: []availability.AccountState{state(1, "a@example.com", time.Time{})},
			want:   []string{"a@example.com"},
		},
		{
			name:   "stale sync is NOT ready",
			states: []availability.AccountState{state(1, "a@example.com", readinessNow.Add(-3*time.Hour))},
			want:   []string{"a@example.com"},
		},
		{
			name:   "exactly maxAge old is ready (finished_at >= now - maxSyncAge)",
			states: []availability.AccountState{state(1, "a@example.com", readinessNow.Add(-time.Hour))},
			want:   nil,
		},
		{
			name:   "one tick past maxAge is NOT ready",
			states: []availability.AccountState{state(1, "a@example.com", readinessNow.Add(-time.Hour-time.Nanosecond))},
			want:   []string{"a@example.com"},
		},
		{
			name: "a future-dated sync (clock skew between us and Postgres) is ready, not an error",
			// Refusing here would turn a second of skew into an outage, and the
			// direction of the skew says nothing about whether we hold data.
			states: []availability.AccountState{state(1, "a@example.com", readinessNow.Add(5*time.Minute))},
			want:   nil,
		},
		{
			name: "mixed scope reports ONLY the offenders, in input order",
			states: []availability.AccountState{
				state(1, "fresh@example.com", readinessNow.Add(-time.Minute)),
				state(2, "stale@example.com", readinessNow.Add(-25*time.Hour)),
				state(3, "never@example.com", time.Time{}),
				state(4, "alsofresh@example.com", readinessNow.Add(-30*time.Minute)),
			},
			want: []string{"stale@example.com", "never@example.com"},
		},
		{
			name: "every account fresh => nothing to refuse",
			states: []availability.AccountState{
				state(1, "a@example.com", readinessNow.Add(-time.Minute)),
				state(2, "b@example.com", readinessNow.Add(-59*time.Minute)),
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := emails(availability.NotReady(tc.states, readinessNow, maxAge))
			if len(got) != len(tc.want) {
				t.Fatalf("NotReady = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NotReady = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// An empty scope is NOT this function's refusal to make. NotReady over no rows
// has nothing to report; LoadBusy is the one that knows the difference between
// "no calendar is in availability scope" and "you have no meetings" and refuses
// on purpose (criterion 5b, readiness_integration_test.go). Pinning it here so
// nobody later "fixes" NotReady into inventing an offender out of nothing.
func TestNotReady_EmptyScopeIsNotThisFunctionsRefusal(t *testing.T) {
	if got := availability.NotReady(nil, readinessNow, time.Hour); len(got) != 0 {
		t.Errorf("NotReady(nil) = %v, want empty; the scope-empty refusal belongs to LoadBusy", emails(got))
	}
	if got := availability.NotReady([]availability.AccountState{}, readinessNow, time.Hour); len(got) != 0 {
		t.Errorf("NotReady([]) = %v, want empty", emails(got))
	}
}

// Criterion 10, stated as a property rather than a comment: the verdict follows
// the `now` ARGUMENT. A state synced at real-world time.Now() is ready when now
// is that instant and unready ten hours later, and the function is called twice
// with identical inputs to pin determinism. An implementation that read the
// wall clock would pass the first half and fail the second.
func TestNotReady_ReadsTheClockArgumentAndNothingElse(t *testing.T) {
	synced := time.Now()
	states := []availability.AccountState{state(7, "clock@example.com", synced)}

	if got := availability.NotReady(states, synced.Add(time.Minute), time.Hour); len(got) != 0 {
		t.Errorf("NotReady one minute after the sync = %v, want ready", emails(got))
	}
	if got := availability.NotReady(states, synced.Add(10*time.Hour), time.Hour); len(got) != 1 {
		t.Errorf("NotReady ten hours after the sync = %v, want the account refused — "+
			"the decision must follow the now ARGUMENT, not a wall-clock read", emails(got))
	}
	first := availability.NotReady(states, synced.Add(10*time.Hour), time.Hour)
	second := availability.NotReady(states, synced.Add(10*time.Hour), time.Hour)
	if len(first) != len(second) {
		t.Errorf("NotReady is not deterministic: %v then %v", emails(first), emails(second))
	}
}

// Criterion 5: the refusal is a typed error carrying the offenders, and its
// text NAMES each account and either its last successful calendar sync or the
// word "never". The audit row stores this string (criterion 8), so it is the
// only thing an operator reading audit_events.error will ever see — a message
// that says "calendar not ready" and stops has told them nothing actionable.
func TestNotReadyError_NamesEachAccountAndItsLastSync(t *testing.T) {
	last := time.Date(2026, 8, 29, 4, 11, 7, 0, time.UTC)
	err := error(&availability.NotReadyError{
		Accounts: []availability.AccountState{
			state(1, "sspataro@gmail.com", time.Time{}),
			state(2, "salvador@org.example", last),
		},
		MaxAge: time.Hour,
	})

	var nre *availability.NotReadyError
	if !errors.As(err, &nre) {
		t.Fatalf("NotReadyError is not errors.As-able as *availability.NotReadyError")
	}
	if len(nre.Accounts) != 2 {
		t.Errorf("NotReadyError.Accounts = %d, want 2", len(nre.Accounts))
	}

	msg := err.Error()
	for _, want := range []string{"sspataro@gmail.com", "salvador@org.example"} {
		if !strings.Contains(msg, want) {
			t.Errorf("NotReadyError text does not name %q: %q", want, msg)
		}
	}
	if !strings.Contains(msg, "never") {
		t.Errorf("NotReadyError text must say %q for an account that has never synced: %q", "never", msg)
	}
	// The timestamp of the stale account: any rendering that contains the date
	// is fine, the point is that "which one is stale and how stale" survives
	// into audit_events.error.
	if !strings.Contains(msg, "2026-08-29") {
		t.Errorf("NotReadyError text does not carry the stale account's last successful sync (2026-08-29...): %q", msg)
	}
}
