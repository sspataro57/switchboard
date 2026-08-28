package drafts

// Criterion 26, second half: "no query in internal/drafts references
// client_person_id, people, or person_identities". A source scan, in the mold of
// internal/textmatch/callsites_test.go — plain unit test, no build tag, no db.
//
// Why mechanise it: migration 0015 DROPS projects.client_person_id, and a dropped
// column does not fail a query at compile time. The failure surfaces at RUNTIME,
// inside cmd/drafts, which is not deployed — so nothing would notice. The
// people/person_identities joins go for a second reason: §9 site C's sender
// parsing (`split_part(replace(replace(m.sender,'>',''),'<',''),' ',...)`) is an
// unshared canonicalization of an email address, the SWT-13 landmine's family, and
// the rework's whole point is that the resolved thread is the same object for
// every channel.
//
// GREENFIELD NOTE: FAILS today — store.go carries all three. That is the red state.

import (
	"os"
	"strings"
	"testing"
)

func TestDraftsStore_ResolvesWithoutPeople(t *testing.T) {
	banned := []struct{ token, why string }{
		{"client_person_id", "dropped by migration 0015; a query naming it fails at runtime, in a binary nothing watches"},
		{"person_identities", "§9 sites C and E: the thread comes from the task's external_refs row, not from an identity"},
		{"people", "§9 site A: ClientName is COALESCE(NULLIF(p.client,''), p.name) — the join is deleted"},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/drafts: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for _, b := range banned {
			for i, line := range strings.Split(string(src), "\n") {
				trimmed := strings.TrimSpace(line)
				// Prose may explain why the dependency is gone; code may not
				// carry it. Same convention as the SWT-19 key-spelling test.
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				if strings.Contains(line, b.token) {
					t.Errorf("%s:%d references %q — %s\n\t%s", name, i+1, b.token, b.why, trimmed)
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned no source files in internal/drafts; a scan with nothing to scan proves nothing")
	}
}
