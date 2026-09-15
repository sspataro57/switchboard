package capture

// REGRESSION — bug sana-email-not-captured (Jira SWT-58): the hot-loop guard,
// structurally. docs/bugs/sana-email-not-captured_DIAGNOSIS.md, "Proposed fix
// scope" 1 and "Risk assessment": the new unarmed-waiting count "must never add
// to Written, or the stage loop re-runs at once". ZERO I/O beyond this repo's
// source. The integration twin (route_unarmed_integration_test.go) is the
// behavioural proof; this keeps the wiring from drifting silently.
//
// Like every structure check in this package it first REQUIRES its subject: a
// scan for "the unarmed count never reaches Written" over a file with no
// unarmed count proves nothing. So it FAILS today, on that precondition. The
// accepted surfaces (the DIAGNOSIS leaves the name open) are a RouteStats field
// `Unarmed` or the Unrouted key "account_unarmed".

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestRegression_SanaEmailNotCaptured_UnarmedCountNeverFeedsWrittenInSource(t *testing.T) {
	src := mustReadRepoFile(t, routeFile)
	_, hasField := reflect.TypeOf(RouteStats{}).FieldByName("Unarmed")
	if !hasField && !strings.Contains(src, `"account_unarmed"`) {
		t.Fatalf("%s has no unarmed-waiting counter (neither RouteStats.Unarmed nor an Unrouted \"account_unarmed\" "+
			"key). SWT-58: routeInbox drops route_after-NULL accounts in SQL, so route_apply's log cannot tell "+
			"\"unarmed with verdicts waiting\" from \"nothing to do\"", routeFile)
	}
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")

	// Written moves in exactly one place: applyRoute's `case wrote:` branch.
	if n := len(regexp.MustCompile(`\.Written\s*\+\+`).FindAllString(code, -1)); n != 1 {
		t.Errorf("%s increments .Written %d time(s), want exactly 1 (a route row inserted). Written is pipelined's "+
			"processed: anything else that moves it re-runs the stage at once", routeFile, n)
	}
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`\.Written\s*(\+=|-=|=[^=])`), // assigned or accumulated
		regexp.MustCompile(`\bWritten\s*:[^=]`),          // set in a composite literal
	} {
		if m := bad.FindString(code); m != "" {
			t.Errorf("%s contains %q — Written counts inserted route rows only; the unarmed count must never feed it",
				routeFile, m)
		}
	}
	unarmed := regexp.MustCompile(`(?i)unarmed`)
	for i, ln := range strings.Split(code, "\n") {
		if strings.Contains(ln, "Written") && unarmed.MatchString(ln) {
			t.Errorf("%s:%d mixes Written and the unarmed count: %q", routeFile, i+1, strings.TrimSpace(ln))
		}
	}
}
