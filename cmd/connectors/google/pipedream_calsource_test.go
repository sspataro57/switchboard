package main

// Unit tests for the calendar TRANSPORT selection (pipedream-calendar /
// docs/tickets/pipedream-calendar_SPEC.md, acceptance criteria 1, 2 and 19).
// ZERO network, ZERO Postgres, ZERO Pipedream credentials: the selection is a
// pure function and the dispatch is read off the source, the shape
// mailsource_test.go and calendarsource_test.go established.
//
// DELIBERATELY A NEW FILE. Criterion 2 says "the existing calendarsource_test.go
// and calendarsource_integration_test.go pass unedited" — the cheapest way to
// make that literally true is not to touch them.
//
// GREENFIELD NOTE: selectCalendarSource and the calendarSource type do not
// exist yet, so this file compile-FAILS — the expected red. Imposed contract
// (cmd/connectors/google/calendarsource.go), the selectMailSource shape
// (mailsource.go:37-56) and its argument verbatim:
//
//	type calendarSource string
//	const (
//	    calendarSourceOAuth     calendarSource = "oauth"
//	    calendarSourcePipedream calendarSource = "pipedream"
//	)
//	func selectCalendarSource(calSourceEnv string) (calendarSource, error)
//
//	// runCalendarPhase is the ONE dispatch both call sites go through.
//	func runCalendarPhase(ctx context.Context, pool *pgxpool.Pool, sink *google.PGSink,
//	    source calendarSource, cfg google.Config) (google.Stats, error)

import (
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Criterion 1: four cases, and the unknown value is an ERROR.
//
// The argument is selectMailSource's, verbatim: a fallback would turn
// CAL_SOURCE=pipdream into a connector that silently keeps doing the old thing
// and reports success — three accounts with no OAuth credential, the SWT-24
// "no credentialed accounts" line, exit 0, and a CronJob that looks healthy
// while availability never refreshes.
// ---------------------------------------------------------------------------

func TestSelectCalendarSource_ResolvesTheFourCases(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    calendarSource
		wantErr bool
	}{
		{
			// Unset is byte-for-byte SWT-24 (criterion 2). Nothing about the
			// existing deployment changes until someone sets CAL_SOURCE
			// deliberately, and no Pipedream code runs: no env read beyond
			// CAL_SOURCE, no HTTP client constructed.
			name: "unset keeps the SWT-24 OAuth path", env: "", want: calendarSourceOAuth,
		},
		{
			// The way back, with no code change (criterion 23).
			name: "explicit oauth", env: "oauth", want: calendarSourceOAuth,
		},
		{
			name: "pipedream", env: "pipedream", want: calendarSourcePipedream,
		},
		{
			name: "a typo is an error, never a fallback", env: "pipdream", wantErr: true,
		},
		{
			name: "an unrelated value is an error", env: "gmail_api", wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectCalendarSource(tc.env)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectCalendarSource(%q) = %q with no error. Criterion 1: any other value is "+
						"an error NAMING the accepted values — a fallback turns a typo into a connector "+
						"that quietly keeps doing the old thing and reports success", tc.env, got)
				}
				for _, want := range []string{"oauth", "pipedream"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name the accepted value %q; the operator reading a "+
							"CronJob log has nothing else to go on", err.Error(), want)
					}
				}
				if !strings.Contains(err.Error(), "CAL_SOURCE") {
					t.Errorf("error %q does not name the variable CAL_SOURCE", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("selectCalendarSource(%q): %v", tc.env, err)
			}
			if got != tc.want {
				t.Errorf("selectCalendarSource(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// Criterion 2, stated as a spelling: the default is the OAuth transport and the
// two constants keep the names CAL_SOURCE accepts, so the manifest value and
// the code path are the same word. (mailsource.go makes the same argument
// against reading the source off a database row: an env var is visible in the
// Deployment, greppable in the repo, and testable without a database.)
func TestCalendarSourceConstantsSpellTheEnvValues(t *testing.T) {
	if string(calendarSourceOAuth) != "oauth" {
		t.Errorf("calendarSourceOAuth = %q, want \"oauth\"", calendarSourceOAuth)
	}
	if string(calendarSourcePipedream) != "pipedream" {
		t.Errorf("calendarSourcePipedream = %q, want \"pipedream\"", calendarSourcePipedream)
	}
}

// ---------------------------------------------------------------------------
// Criterion 19: the two calendar call sites dispatch through the SAME switch,
// so they can never disagree about which transport ran.
//
// A source scan rather than behaviour because the thing being pinned is a
// call-site shape inside main(), which cannot be observed without running it —
// the same reason calendarsource_test.go scans main.go for its flag usage
// strings. If both sites re-implemented the switch, a divergence would be
// invisible until production ran the mail pass and the CronJob against
// different transports and produced two calendar runs per account.
// ---------------------------------------------------------------------------

func TestCalendarCallSitesDispatchThroughOneSwitch(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	dispatches := strings.Count(body, "runCalendarPhase(")
	if dispatches != 2 {
		t.Errorf("main.go calls runCalendarPhase( %d times, want exactly 2: the --calendar-only site and "+
			"the in-mail-pass site (main.go:100-112 and :174-178). Criterion 19 routes both through one "+
			"dispatch so they can never disagree about which transport ran", dispatches)
	}
	for _, banned := range []string{"runCalendarIngest(", "runPipedreamCalendarIngest("} {
		if strings.Contains(body, banned) {
			t.Errorf("main.go calls %s directly. Both calendar call sites must go through runCalendarPhase, "+
				"or a future edit teaches one site about a transport and leaves the other behind — two "+
				"calendar passes per invocation, two sync_runs rows, and \"which one ran\" becomes "+
				"unanswerable (criterion 19)", banned)
		}
	}
	// Criterion 19's other half: calendarPhaseRuns itself is UNCHANGED — the
	// in-mail-pass site is still imap-only, because bridge and gmail_api ingest
	// calendar inline and two passes race the same cursor key.
	if !strings.Contains(body, "calendarPhaseRuns(") {
		t.Error("main.go no longer gates the in-mail-pass calendar site on calendarPhaseRuns(source). " +
			"Criterion 19 says that gate is unchanged: under bridge or gmail_api the inline calendar " +
			"ingest already ran, and a second pass races it for calendar_sync_token")
	}
}
