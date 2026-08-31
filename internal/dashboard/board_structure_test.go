package dashboard

import (
	"strings"
	"testing"
)

// The project select applies itself on change (SWT-26). The board is a plain
// GET form — the attribute is the whole feature, and it is exactly the kind of
// "unused attribute" a template cleanup would strip with every test staying
// green. Assert it, and assert the text inputs did NOT grow the same behaviour:
// submitting per keystroke would make status/assignee_type/subproject untypable.
func TestBoardTemplate_ProjectFilterAutoSubmits(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/tasks.html")
	if err != nil {
		t.Fatalf("read embedded tasks.html: %v", err)
	}
	s := string(raw)

	if !strings.Contains(s, `<select name="project" onchange="this.form.submit()">`) {
		t.Fatalf("the project select no longer auto-submits on change; switching project must apply the filter without a second click (SWT-26)")
	}
	if n := strings.Count(s, "onchange"); n != 1 {
		t.Fatalf("expected exactly one onchange in tasks.html (the project select), found %d — text inputs must not submit while being typed in", n)
	}
}
