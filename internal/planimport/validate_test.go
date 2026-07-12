package planimport_test

// Unit tests for the deterministic plan-tree validator (SPEC 10-plan-import,
// acceptance criterion 4). Go — NOT the model — validates and normalizes the
// tree before anything is proposed: refs unique/non-empty, parent_ref resolves,
// no parent cycles, depends_on_refs resolve with no dependency cycle
// (topological), assignee_type ∈ {human,claude}, titles non-empty, ≤ 200 tasks,
// and plan_order is assigned by Go as the 1-based sibling array position.
// Soft clamps/corrections are recorded in the validation slice (triage
// buildFields idiom); hard-invalid trees return an error whose message carries
// the reasons. Everything here is a pure function — ZERO network, ZERO Postgres.
//
// GREENFIELD NOTE: package internal/planimport does not exist yet; this file
// compile-FAILs under `go test ./...` until it is implemented — the expected
// failure mode. For greenfield code the SPEC's contract IS the signature.
// Imposed exported surface (validate.go / prompt.go), followed by the
// implementer:
//
//   // Node is one proposed task as the model emits it (the plan_tree schema
//   // shape). plan_order is deliberately ABSENT — array position is
//   // authoritative and Go assigns the number.
//   type Node struct {
//       Ref           string   `json:"ref"`
//       ParentRef     *string  `json:"parent_ref"`
//       Title         string   `json:"title"`
//       Body          string   `json:"body"`
//       AssigneeType  string   `json:"assignee_type"` // human | claude
//       Subproject    *string  `json:"subproject"`
//       WorkerType    *string  `json:"worker_type"`
//       Priority      int      `json:"priority"`
//       DependsOnRefs []string `json:"depends_on_refs"`
//       Confidence    float64  `json:"confidence"`
//       Notes         string   `json:"notes"`
//   }
//   type Tree struct {
//       Summary string `json:"summary"`
//       Tasks   []Node `json:"tasks"`
//   }
//
//   // ValidatedNode is a Node after validation, with the Go-assigned plan_order.
//   type ValidatedNode struct {
//       Ref           string
//       ParentRef     *string
//       Title         string
//       Body          string
//       AssigneeType  string
//       Subproject    *string
//       WorkerType    *string
//       Priority      int
//       DependsOnRefs []string
//       Confidence    float64
//       Notes         string
//       PlanOrder     int // 1-based sibling array position, assigned by Go
//   }
//
//   // Result is Validate's output for a structurally-valid tree. Tasks are in
//   // topological (parents-before-children) order — the order apply inserts them.
//   type Result struct {
//       Summary    string
//       Tasks      []ValidatedNode
//       Validation []string // soft clamps/corrections, triage buildFields idiom
//   }
//
//   // Validate is criterion 4: assigns plan_order, records soft clamps in
//   // Result.Validation, and returns a non-nil error (reasons in the message)
//   // for a HARD-invalid tree — a cycle, an unresolved ref, a duplicate/empty
//   // ref, an empty title, a bad assignee_type, or > 200 tasks. A hard-invalid
//   // tree yields NO plan_imports row upstream.
//   func Validate(tree Tree) (Result, error)
//
//   const MaxTasks = 200

import (
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/planimport"
)

// ---- fixtures --------------------------------------------------------------

func sp(s string) *string { return &s }

// node builds a Node with sane defaults; overrides via the mutator.
func node(ref, title string, mut func(*planimport.Node)) planimport.Node {
	n := planimport.Node{
		Ref:           ref,
		Title:         title,
		Body:          "body of " + ref,
		AssigneeType:  "claude",
		Priority:      0,
		DependsOnRefs: []string{},
		Confidence:    0.9,
	}
	if mut != nil {
		mut(&n)
	}
	return n
}

func nodeByRef(t *testing.T, r planimport.Result, ref string) planimport.ValidatedNode {
	t.Helper()
	for _, v := range r.Tasks {
		if v.Ref == ref {
			return v
		}
	}
	t.Fatalf("ref %q not present in Result.Tasks", ref)
	return planimport.ValidatedNode{}
}

// ---- hard-invalid trees (return an error, no proposal) ---------------------

// TestValidate_HardInvalid table-drives every structural rejection criterion 4
// pins. Each case must return a non-nil error; the message is expected to name
// the offending category (lenient substring, lowercased).
func TestValidate_HardInvalid(t *testing.T) {
	tests := []struct {
		name    string
		tree    planimport.Tree
		wantSub string // lowercased substring the reason should mention
	}{
		{
			name: "duplicate ref",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("dup", "A", nil),
				node("dup", "B", nil),
			}},
			wantSub: "dup",
		},
		{
			name: "empty ref",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("", "A", nil),
			}},
			wantSub: "ref",
		},
		{
			name: "empty title",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("a", "", nil),
			}},
			wantSub: "title",
		},
		{
			name: "unresolved parent_ref",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("a", "A", func(n *planimport.Node) { n.ParentRef = sp("ghost") }),
			}},
			wantSub: "parent",
		},
		{
			name: "parent cycle",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("a", "A", func(n *planimport.Node) { n.ParentRef = sp("b") }),
				node("b", "B", func(n *planimport.Node) { n.ParentRef = sp("a") }),
			}},
			wantSub: "cycle",
		},
		{
			name: "unresolved depends_on ref",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("a", "A", func(n *planimport.Node) { n.DependsOnRefs = []string{"ghost"} }),
			}},
			wantSub: "depend",
		},
		{
			name: "dependency cycle (topological)",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("a", "A", func(n *planimport.Node) { n.DependsOnRefs = []string{"b"} }),
				node("b", "B", func(n *planimport.Node) { n.DependsOnRefs = []string{"a"} }),
			}},
			wantSub: "cycle",
		},
		{
			name: "bad assignee_type",
			tree: planimport.Tree{Tasks: []planimport.Node{
				node("a", "A", func(n *planimport.Node) { n.AssigneeType = "robot" }),
			}},
			wantSub: "assignee_type",
		},
		{
			name: "over the 200-task cap",
			tree: planimport.Tree{Tasks: overCap(201)},
			wantSub: "200",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planimport.Validate(tc.tree)
			if err == nil {
				t.Fatalf("Validate(%s) = nil error, want a rejection", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantSub) {
				t.Errorf("Validate(%s) error = %q, want it to mention %q", tc.name, err, tc.wantSub)
			}
		})
	}
}

func overCap(n int) []planimport.Node {
	out := make([]planimport.Node, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, node(refN(i), "T", nil))
	}
	return out
}

func refN(i int) string {
	return "r" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

// TestValidate_AtCapAllowed: exactly 200 tasks is allowed (boundary).
func TestValidate_AtCapAllowed(t *testing.T) {
	if _, err := planimport.Validate(planimport.Tree{Tasks: overCap(planimport.MaxTasks)}); err != nil {
		t.Fatalf("Validate(200 tasks) = %v, want ok (200 is the cap, not over it)", err)
	}
}

// ---- plan_order assignment (Go, 1-based sibling index) ---------------------

// TestValidate_PlanOrderIsSiblingIndex pins criterion 4 / decision 5: plan_order
// is the 1-based position of a node among its SIBLINGS (roots among roots,
// children among that parent's children), assigned by Go — never model-chosen.
func TestValidate_PlanOrderIsSiblingIndex(t *testing.T) {
	tree := planimport.Tree{Tasks: []planimport.Node{
		node("root-a", "Root A", nil),
		node("child-a1", "Child A1", func(n *planimport.Node) { n.ParentRef = sp("root-a") }),
		node("root-b", "Root B", nil),
		node("child-a2", "Child A2", func(n *planimport.Node) { n.ParentRef = sp("root-a") }),
		node("child-b1", "Child B1", func(n *planimport.Node) { n.ParentRef = sp("root-b") }),
	}}

	res, err := planimport.Validate(tree)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := map[string]int{
		"root-a":   1, // first root
		"root-b":   2, // second root
		"child-a1": 1, // first child of root-a
		"child-a2": 2, // second child of root-a
		"child-b1": 1, // first child of root-b
	}
	for ref, wantOrder := range want {
		if got := nodeByRef(t, res, ref).PlanOrder; got != wantOrder {
			t.Errorf("plan_order[%s] = %d, want %d (1-based sibling array position)", ref, got, wantOrder)
		}
	}
}

// TestValidate_TopologicalParentsFirst: the returned tasks are ordered so every
// child appears AFTER its parent — the order apply inserts them so parent_id
// always resolves (criterion 7).
func TestValidate_TopologicalParentsFirst(t *testing.T) {
	tree := planimport.Tree{Tasks: []planimport.Node{
		node("child", "Child", func(n *planimport.Node) { n.ParentRef = sp("parent") }),
		node("parent", "Parent", nil),
	}}
	res, err := planimport.Validate(tree)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	pos := map[string]int{}
	for i, v := range res.Tasks {
		pos[v.Ref] = i
	}
	if pos["parent"] >= pos["child"] {
		t.Errorf("parent at index %d, child at %d — parent must come first (parents-first insert order)", pos["parent"], pos["child"])
	}
}

// ---- soft clamps recorded in validation ------------------------------------

// TestValidate_ClampsRecorded: out-of-range confidence is clamped into [0,1] and
// the correction is recorded in Result.Validation (triage buildFields idiom).
// The tree is otherwise valid, so it is NOT rejected — a clamp is soft.
func TestValidate_ClampsRecorded(t *testing.T) {
	tree := planimport.Tree{Tasks: []planimport.Node{
		node("hi", "High", func(n *planimport.Node) { n.Confidence = 1.5 }),
		node("lo", "Low", func(n *planimport.Node) { n.Confidence = -0.2 }),
	}}
	res, err := planimport.Validate(tree)
	if err != nil {
		t.Fatalf("Validate (clamp is soft, must not reject): %v", err)
	}
	if len(res.Validation) == 0 {
		t.Errorf("Result.Validation empty; out-of-range confidence clamps must be recorded")
	}
	for _, ref := range []string{"hi", "lo"} {
		c := nodeByRef(t, res, ref).Confidence
		if c < 0 || c > 1 {
			t.Errorf("confidence[%s] = %v, want clamped into [0,1]", ref, c)
		}
	}
}

// TestValidate_ValidTreeNoValidationNoise: a clean tree produces no spurious
// validation notes (so the dashboard's "validation notes" surface stays a
// signal, not noise).
func TestValidate_ValidTreeNoValidationNoise(t *testing.T) {
	tree := planimport.Tree{Summary: "clean", Tasks: []planimport.Node{
		node("a", "A", nil),
		node("b", "B", func(n *planimport.Node) { n.DependsOnRefs = []string{"a"} }),
	}}
	res, err := planimport.Validate(tree)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(res.Validation) != 0 {
		t.Errorf("Result.Validation = %v, want empty for a clean tree", res.Validation)
	}
	if res.Summary != "clean" {
		t.Errorf("Result.Summary = %q, want it carried through from the tree", res.Summary)
	}
}
