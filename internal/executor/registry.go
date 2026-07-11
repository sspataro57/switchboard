package executor

import (
	"context"
	"sort"
)

// Tool is one registered internal tool. Handlers are wired in as closures at
// registration time and are never exported by their packages — the registry is
// the only route to them (invariant 3).
type Tool struct {
	Name     string
	Validate func(args []byte) error
	Handle   func(ctx context.Context, args []byte) ([]byte, error)
}

// Registry maps tool names to tools. It is populated at startup and read-only
// afterwards; no mutex needed.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name] = t
}

// Names returns the registered tool names, sorted — the static policy's allow
// set is built from this.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}
