package main

// Unit tests for the calendar phase's wiring in the connector main (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 18 and 20).
// ZERO network, ZERO Postgres — the mode selection is a pure function and the
// flag surface is read off the source, the shape mailsource_test.go established.
//
// GREENFIELD NOTE (historical): calendarPhaseRuns did not exist when this
// file was written (it compile-FAILed, the expected red) until the SWT-24
// implementation landed. Imposed surface (package-internal,
// cmd/connectors/google/calendarsource.go):
//
//	// calendarPhaseRuns reports whether THIS pass owns the calendar phase.
//	// Only the imap source does: bridge and gmail_api already ingest calendar
//	// inline (google.Run / RunBridge), so running it again would give one
//	// account two calendar passes in one invocation — two sync_runs rows, two
//	// token writes, and a race between them for the same cursor key.
//	func calendarPhaseRuns(source mailSource) bool

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestCalendarPhaseRuns_OnlyForTheIMAPSource(t *testing.T) {
	cases := []struct {
		name   string
		source mailSource
		want   bool
	}{
		{
			// The production configuration, and the whole reason this ticket
			// exists: runIMAPIngest does mail and nothing else, and the watch
			// loop contains the string "calendar" zero times, so restoring
			// tokens alone would still ingest no events (SPEC premise 3).
			name:   "imap owns the calendar phase",
			source: mailSourceIMAP,
			want:   true,
		},
		{
			// google.RunBridge already calls IngestCalendar inline
			// (bridge_ingest.go).
			name:   "bridge already ingests calendar inline",
			source: mailSourceBridge,
			want:   false,
		},
		{
			// google.Run's per-account loop is gmail phase then calendar phase.
			name:   "gmail_api already ingests calendar inline",
			source: mailSourceGmailAPI,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calendarPhaseRuns(tc.source); got != tc.want {
				t.Errorf("calendarPhaseRuns(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

// Criterion 20 + SPEC premise 11: the --full help string describes behaviour
// production cannot reach. "ignore gmail cursor, drop calendar sync token" is
// true of gmail_api mode only — under MAIL_SOURCE=imap the gmail half is
// unreachable (IMAP uses per-folder UID cursors) and the calendar half lived in
// code no imap pass ever called. A help string that describes a mode you are
// not in is worse than none: it is what an operator reads before deciding
// whether a re-sync is safe.
//
// Source scan rather than flag introspection because the flags are declared
// inside main() and cannot be enumerated without running it.
func TestConnectorFlags_FullHelpNamesItsModesAndCalendarOnlyExists(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	usage := func(flagName string) (string, bool) {
		re := regexp.MustCompile(`flag\.Bool\("` + regexp.QuoteMeta(flagName) + `",[^,]+,\s*"((?:[^"\\]|\\.)*)"\)`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			return "", false
		}
		return m[1], true
	}

	fullUsage, ok := usage("full")
	if !ok {
		t.Fatalf("no flag.Bool(\"full\", ...) found in main.go; the scan has stopped matching")
	}
	named := false
	for _, mode := range []string{"gmail_api", "imap", "bridge"} {
		if strings.Contains(fullUsage, mode) {
			named = true
		}
	}
	if !named {
		t.Errorf("--full usage is %q and names no mail source. Its clauses do not all apply in every mode: "+
			"the gmail-cursor clause is gmail_api only (IMAP keeps per-folder UID cursors) and the "+
			"calendar-sync-token clause now belongs to the calendar phase. Say which is which (criterion 20)",
			fullUsage)
	}

	calUsage, ok := usage("calendar-only")
	if !ok {
		t.Fatalf("no flag.Bool(\"calendar-only\", ...) in main.go. Criterion 20: --calendar-only runs the " +
			"calendar phase followed by Normalize and NOTHING else — no mail ingest, no ObserveOutbound, no " +
			"capture-rules pass; those belong to the mail funnel and the watch loop already runs them. It is " +
			"what a future CronJob calls to keep availability fresh without touching mail")
	}
	if !strings.Contains(strings.ToLower(calUsage), "calendar") {
		t.Errorf("--calendar-only usage %q does not describe the calendar phase", calUsage)
	}
}
