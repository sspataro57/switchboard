package availability_test

// Structural enforcement of the ONE-READER rule (SWT-24 /
// docs/tickets/calendar-availability_SPEC.md, acceptance criteria 1 and 2).
//
// Plain unit test on purpose — no build tag, no database, the shape of
// internal/textmatch/callsites_test.go and
// internal/connector/upworkcrm/keyspelling_test.go. It runs on every
// `go test ./...`.
//
// THE RULE. Free/busy has exactly one database-backed entry point, LoadBusy,
// and LoadBusy performs the readiness check BEFORE it reads a row of
// normalized_events. That guarantee is worth nothing if a second caller can
// query the table itself, or call the unsafe primitive: an empty result set
// from a direct read is indistinguishable from "no meetings", which is the
// precise fail-open this ticket exists to remove. So the primitive is
// unexported (criterion 1) and the table has one reader (criterion 2), and both
// are checked mechanically rather than documented — the repo has paid four
// times for a rule that lived only in prose (IK: "Exact text comparison across
// a provider round trip", the upwork thread-key spelling, the inert time floor,
// the guard whose column no query selected).
//
// The writer is exempt because it is the writer: sink.upsertEvent is the only
// code that INSERTs normalized_events, and SupersedeAbsentCalendar cancels them
// on a Calendar reset. Neither answers a free/busy question.
//
// NOTE ON THE RED (historical): TestNormalizedEventsHasExactlyOneReader was a
// pin from day one, while TestAvailability_ExportsNoUncheckedEventLoader was
// RED while this ticket was being built — store.go exported LoadEvents and
// LoadBusy did not exist. Both live in this file because they are one rule
// stated at two levels.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoPaths are the trees the SPEC scopes the ban to, relative to this package.
var repoPaths = []string{
	filepath.Join("..", "..", "internal"),
	filepath.Join("..", "..", "cmd"),
}

// allowedEventReaders are the two files permitted to name the table: the one
// SQL read behind LoadBusy, and the connector sink that WRITES the rows.
var allowedEventReaders = map[string]bool{
	filepath.Join("internal", "availability", "store.go"):       true,
	filepath.Join("internal", "connector", "google", "sink.go"): true,
}

func TestNormalizedEventsHasExactlyOneReader(t *testing.T) {
	seen := map[string]bool{}

	for _, root := range repoPaths {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !mentionsOutsideComments(string(src), "normalized_events") {
				return nil
			}
			// Normalize "../../internal/availability/store.go" to the repo-relative
			// key the allowlist is spelled in.
			rel := path
			if i := strings.Index(path, "internal"+string(filepath.Separator)); i >= 0 {
				rel = path[i:]
			} else if i := strings.Index(path, "cmd"+string(filepath.Separator)); i >= 0 {
				rel = path[i:]
			}
			seen[rel] = true
			if !allowedEventReaders[rel] {
				t.Errorf("%s names normalized_events in SQL. Free/busy has ONE database-backed entry "+
					"point (availability.LoadBusy) and it refuses before it reads the table; a second reader "+
					"gets an empty result set for a dead poller and cannot tell it apart from an empty "+
					"calendar — the exact fail-open SWT-24 removes. Route the read through LoadBusy, or add "+
					"the file to allowedEventReaders with a reason", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// Without this the scan silently passes if the SQL is re-spelled, the files
	// move, or the walk root stops matching — the failure mode of every
	// source-scanning test.
	for want := range allowedEventReaders {
		if !seen[want] {
			t.Errorf("expected %s to contain the table name and it does not; the scan has probably stopped "+
				"matching (check repoPaths and the allowlist spellings)", want)
		}
	}
}

// Criterion 1: the unsafe primitive is UNREACHABLE from outside the package,
// not merely discouraged. A source scan rather than a reflection test because
// the thing being banned is an exported identifier, which is a spelling.
func TestAvailability_ExportsNoUncheckedEventLoader(t *testing.T) {
	var srcs []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		srcs = append(srcs, string(b))
	}
	if len(srcs) == 0 {
		t.Fatalf("no non-test sources found in internal/availability; the scan is vacuous")
	}
	joined := strings.Join(srcs, "\n")

	if strings.Contains(joined, "func LoadEvents(") {
		t.Errorf("internal/availability still exports LoadEvents. It reads normalized_events with NO readiness " +
			"check, so any caller holding it can be told \"you are free all week\" by an empty table (SPEC " +
			"premise 1). Criterion 1: unexport it to loadEvents and make LoadBusy the one entry point")
	}
	if !strings.Contains(joined, "func LoadBusy(") {
		t.Errorf("internal/availability has no LoadBusy. Criterion 1 pins exactly one database-backed entry " +
			"point, LoadBusy(ctx, pool, req) ([]Interval, error), which runs the readiness check BEFORE it " +
			"reads a single row of normalized_events")
	}
}

// mentionsOutsideComments reports whether needle appears on a line that is not
// a whole-line // comment. Prose may explain why a rule exists and quote the
// thing it bans (the upworkcrm keyspelling test made the same allowance).
func mentionsOutsideComments(src, needle string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
