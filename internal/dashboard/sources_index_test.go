package dashboard

import (
	"os"
	"strings"
	"testing"
)

// swb 709: /sources' two raw_json counters are fast only while their
// predicates match migration 0046's partial indexes EXACTLY; any other spelling
// is not provable and falls back to de-TOASTing every raw item (22 s a page on
// 2026-09-25).
func TestSources_CountersMatchTheirIndexes(t *testing.T) {
	src, err := os.ReadFile("sources.go")
	if err != nil {
		t.Fatal(err)
	}
	mig, err := os.ReadFile("../../migrations/0046_raw_items_flag_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, pred := range []string{
		`raw_json->>'truncated' = 'true'`,
		`jsonb_array_length(COALESCE(raw_json->'parts','[]'::jsonb)) > 0`,
	} {
		if !strings.Contains(string(mig), "WHERE "+pred) {
			t.Errorf("migration 0046 no longer indexes WHERE %s", pred)
		}
		// sources.go aliases the table as ri.
		if want := strings.Replace(pred, "raw_json", "ri.raw_json", 1); !strings.Contains(string(src), want) {
			t.Errorf("sources.go no longer spells %q as its index does: the page falls back to a full "+
				"de-TOAST of raw_source_items", want)
		}
	}
}
