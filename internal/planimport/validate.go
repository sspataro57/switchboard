// Package planimport is the one-way plan funnel (SPEC 10-plan-import): a .md
// plan file is captured raw-first, parsed by a GPT provider call into a
// strictly-schema'd task tree, deterministically validated (Go, not the
// model), and — only after dashboard approval — materialized as tasks through
// the executor apply tool. The package has NO task-write surface (invariant 2:
// nothing is disposed before approval).
package planimport

import (
	"fmt"
	"sort"
	"strings"
)

// MaxTasks is the hard cap on one plan tree (criterion 4).
const MaxTasks = 200

// Node is one proposed task as the model emits it (the plan_tree schema
// shape). plan_order is deliberately ABSENT — array position is authoritative
// and Go assigns the number.
type Node struct {
	Ref           string   `json:"ref"`
	ParentRef     *string  `json:"parent_ref"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	AssigneeType  string   `json:"assignee_type"` // human | claude
	Subproject    *string  `json:"subproject"`
	WorkerType    *string  `json:"worker_type"`
	Priority      int      `json:"priority"`
	DependsOnRefs []string `json:"depends_on_refs"`
	Confidence    float64  `json:"confidence"`
	Notes         string   `json:"notes"`
}

// Tree is the full plan_tree document.
type Tree struct {
	Summary string `json:"summary"`
	Tasks   []Node `json:"tasks"`
}

// ValidatedNode is a Node after validation, with the Go-assigned plan_order.
type ValidatedNode struct {
	Ref           string   `json:"ref"`
	ParentRef     *string  `json:"parent_ref"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	AssigneeType  string   `json:"assignee_type"`
	Subproject    *string  `json:"subproject"`
	WorkerType    *string  `json:"worker_type"`
	Priority      int      `json:"priority"`
	DependsOnRefs []string `json:"depends_on_refs"`
	Confidence    float64  `json:"confidence"`
	Notes         string   `json:"notes"`
	PlanOrder     int      `json:"plan_order"` // 1-based sibling array position
}

// Result is Validate's output for a structurally-valid tree. Tasks are in
// topological (parents-before-children) order — the order apply inserts them.
type Result struct {
	Summary    string          `json:"summary"`
	Tasks      []ValidatedNode `json:"tasks"`
	Validation []string        `json:"validation,omitempty"` // soft clamps/corrections
}

// Validate is criterion 4 — determinism at the spine. Hard-invalid trees
// (duplicate/empty ref, empty title, unresolved parent_ref/depends_on_refs,
// parent or dependency cycle, bad assignee_type, > MaxTasks) return a non-nil
// error naming every reason; soft issues (out-of-range confidence) are clamped
// and recorded in Result.Validation. plan_order is assigned here as the
// 1-based position among siblings — never model-chosen.
func Validate(tree Tree) (Result, error) {
	var hard []string
	if len(tree.Tasks) > MaxTasks {
		hard = append(hard, fmt.Sprintf("plan has %d tasks; the cap is %d (200)", len(tree.Tasks), MaxTasks))
	}

	byRef := map[string]int{}
	for i, n := range tree.Tasks {
		if strings.TrimSpace(n.Ref) == "" {
			hard = append(hard, fmt.Sprintf("task %d: empty ref", i+1))
			continue
		}
		if _, dup := byRef[n.Ref]; dup {
			hard = append(hard, fmt.Sprintf("duplicate ref %q", n.Ref))
			continue
		}
		byRef[n.Ref] = i
	}

	for _, n := range tree.Tasks {
		if strings.TrimSpace(n.Title) == "" {
			hard = append(hard, fmt.Sprintf("task %q: empty title", n.Ref))
		}
		if n.AssigneeType != "human" && n.AssigneeType != "claude" {
			hard = append(hard, fmt.Sprintf("task %q: assignee_type %q must be human or claude", n.Ref, n.AssigneeType))
		}
		if n.ParentRef != nil {
			if _, ok := byRef[*n.ParentRef]; !ok {
				hard = append(hard, fmt.Sprintf("task %q: parent_ref %q does not resolve", n.Ref, *n.ParentRef))
			}
		}
		for _, d := range n.DependsOnRefs {
			if _, ok := byRef[d]; !ok {
				hard = append(hard, fmt.Sprintf("task %q: depends_on ref %q does not resolve", n.Ref, d))
			}
		}
	}

	// Parent cycles (only meaningful once refs resolve).
	if len(hard) == 0 {
		for _, n := range tree.Tasks {
			seen := map[string]bool{n.Ref: true}
			cur := n.ParentRef
			for cur != nil {
				if seen[*cur] {
					hard = append(hard, fmt.Sprintf("parent cycle through %q", n.Ref))
					break
				}
				seen[*cur] = true
				cur = tree.Tasks[byRef[*cur]].ParentRef
			}
		}
		// Dependency cycles: Kahn over depends_on edges.
		indeg := map[string]int{}
		for _, n := range tree.Tasks {
			indeg[n.Ref] += 0
			for range n.DependsOnRefs {
				indeg[n.Ref]++
			}
		}
		queue := []string{}
		for ref, d := range indeg {
			if d == 0 {
				queue = append(queue, ref)
			}
		}
		dependents := map[string][]string{}
		for _, n := range tree.Tasks {
			for _, d := range n.DependsOnRefs {
				dependents[d] = append(dependents[d], n.Ref)
			}
		}
		resolved := 0
		for len(queue) > 0 {
			ref := queue[0]
			queue = queue[1:]
			resolved++
			for _, dep := range dependents[ref] {
				indeg[dep]--
				if indeg[dep] == 0 {
					queue = append(queue, dep)
				}
			}
		}
		if resolved != len(tree.Tasks) {
			var stuck []string
			for ref, d := range indeg {
				if d > 0 {
					stuck = append(stuck, ref)
				}
			}
			sort.Strings(stuck)
			hard = append(hard, fmt.Sprintf("dependency cycle through %s", strings.Join(stuck, ", ")))
		}
	}

	if len(hard) > 0 {
		return Result{}, fmt.Errorf("invalid plan tree: %s", strings.Join(hard, "; "))
	}

	res := Result{Summary: tree.Summary}

	// plan_order: 1-based position among siblings, in array order.
	siblingCount := map[string]int{} // key "" for roots, else parent ref
	orderOf := make([]int, len(tree.Tasks))
	for i, n := range tree.Tasks {
		key := ""
		if n.ParentRef != nil {
			key = *n.ParentRef
		}
		siblingCount[key]++
		orderOf[i] = siblingCount[key]
	}

	// Topological (parents-first) output order, stable within the array.
	emitted := map[string]bool{}
	var emit func(i int)
	emit = func(i int) {
		n := tree.Tasks[i]
		if emitted[n.Ref] {
			return
		}
		if n.ParentRef != nil && !emitted[*n.ParentRef] {
			emit(byRef[*n.ParentRef])
		}
		emitted[n.Ref] = true
		v := ValidatedNode{
			Ref: n.Ref, ParentRef: n.ParentRef, Title: n.Title, Body: n.Body,
			AssigneeType: n.AssigneeType, Subproject: n.Subproject, WorkerType: n.WorkerType,
			Priority: n.Priority, DependsOnRefs: n.DependsOnRefs,
			Confidence: n.Confidence, Notes: n.Notes, PlanOrder: orderOf[i],
		}
		if v.Confidence < 0 || v.Confidence > 1 {
			clamped := v.Confidence
			if clamped < 0 {
				clamped = 0
			}
			if clamped > 1 {
				clamped = 1
			}
			res.Validation = append(res.Validation,
				fmt.Sprintf("task %q: confidence %v clamped to %v", v.Ref, v.Confidence, clamped))
			v.Confidence = clamped
		}
		res.Tasks = append(res.Tasks, v)
	}
	for i := range tree.Tasks {
		emit(i)
	}
	return res, nil
}
