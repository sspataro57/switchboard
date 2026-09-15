package main

// REGRESSION — bug sana-email-not-captured (Jira SWT-58): route_apply's pass log
// line and its processed count. docs/bugs/sana-email-not-captured_DIAGNOSIS.md,
// "Proposed fix scope" 1:
//
//   - cmd/pipelined/route.go logs the new unarmed-waiting counter
//     UNCONDITIONALLY, zeros included (the comment already above that slog call:
//     "'found nothing' and 'never ran' are different lines");
//   - routeApplyPass's processed stays route rows WRITTEN. The unarmed count
//     never reaches it: processed >= 1 publishes `routed` (buildPass), and a
//     pass with processed >= Limit is repeated at once (pipeline.StageLoop).
//
// Pure: no database, no broker. routeApplyPass(nil) runs capture.RunRouteApply
// with a nil pool, which refuses before any I/O with zeroed stats, and the pass
// still prints its counter line (it does so today, for every counter). The key
// is matched loosely, any slog key containing "unarmed", because the DIAGNOSIS
// leaves the name to the implementer.
//
// Both tests FAIL today: the line has no unarmed key, and the source scan
// requires the counter to exist before it can guard it.
//
// captureSlog / slogRecord / unarmedAttrs are shared with the integration
// regression in sana_repro_integration_test.go (same package).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

// captureSlog runs fn with the default slog logger writing JSON into a buffer,
// restores the logger, echoes the lines to the test log and returns the
// decoded records.
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	func() {
		defer slog.SetDefault(old)
		fn()
	}()
	var recs []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		t.Logf("slog: %s", ln)
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("decode slog line %q: %v", ln, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// slogRecord returns the first record whose msg is msg.
func slogRecord(recs []map[string]any, msg string) (map[string]any, bool) {
	for _, r := range recs {
		if r["msg"] == msg {
			return r, true
		}
	}
	return nil, false
}

// unarmedAttrs returns every attribute of rec whose key contains "unarmed"
// (case-insensitive), with its value; a non-numeric value is returned as -1.
func unarmedAttrs(rec map[string]any) map[string]float64 {
	out := map[string]float64{}
	for k, v := range rec {
		if !strings.Contains(strings.ToLower(k), "unarmed") {
			continue
		}
		if n, ok := v.(float64); ok {
			out[k] = n
		} else {
			out[k] = -1
		}
	}
	return out
}

func TestRegression_SanaEmailNotCaptured_RouteApplyLogPrintsUnarmedEveryPass(t *testing.T) {
	recs := captureSlog(t, func() { _, _ = routeApplyPass(nil)(context.Background()) })
	rec, ok := slogRecord(recs, "route_apply pass")
	if !ok {
		t.Fatalf("routeApplyPass printed no \"route_apply pass\" line (records: %v); the counters are printed every pass", recs)
	}
	got := unarmedAttrs(rec)
	if len(got) == 0 {
		t.Fatalf("the route_apply pass line has no unarmed counter: %v. SWT-58: `written=0` with every reason at 0 "+
			"read the same as an empty inbox while client mail waited on an account with route_after NULL; the line "+
			"must print the count of messages waiting on an unarmed account EVERY pass, zeros included", rec)
	}
	for k, v := range got {
		if v != 0 {
			t.Errorf("route_apply pass %s = %v on a pass that read nothing; want the numeric 0, printed (zeros included)", k, v)
		}
	}
}

// processed = route rows written, and nothing else. REQUIRES the counter in the
// pass first (see the header): a guard over a pass with no counter proves nothing.
func TestRegression_SanaEmailNotCaptured_RouteApplyProcessedIsWrittenOnly(t *testing.T) {
	b, err := os.ReadFile("route.go")
	if err != nil {
		t.Fatalf("read cmd/pipelined/route.go: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "func routeApplyPass(")
	if i < 0 {
		t.Fatalf("cmd/pipelined/route.go declares no routeApplyPass")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(body, "")
	if !regexp.MustCompile(`(?i)unarmed`).MatchString(code) {
		t.Fatalf("routeApplyPass never mentions the unarmed-waiting counter, so it cannot log it (SWT-58); this guard "+
			"needs it present.\nbody:\n%s", body)
	}
	returns := regexp.MustCompile(`(?m)^\s*return\s+([^,\n]+),`).FindAllStringSubmatch(code, -1)
	written := regexp.MustCompile(`^\w+\.Written$`)
	sawWritten := false
	for _, r := range returns {
		expr := strings.TrimSpace(r[1])
		switch {
		// Amended while fixing SWT-58: routeApplyPass's own `return func(ctx …) (int, error) {`
		// returns the PassFunc closure, not a processed value; the regex's first
		// comma cuts it to "func(ctx context.Context) (int". Only the closure's
		// returns are processed values.
		case strings.HasPrefix(expr, "func("):
		case expr == "0":
		case written.MatchString(expr):
			sawWritten = true
		default:
			t.Errorf("routeApplyPass returns processed = %q; want the stats' Written (or 0). The unarmed count must "+
				"never be processed: >= 1 publishes `routed`, and >= the limit re-runs the pass at once", expr)
		}
	}
	if !sawWritten {
		t.Errorf("routeApplyPass never returns <stats>.Written as processed; returns = %v", returns)
	}
}
