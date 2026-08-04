package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tphakala/alfred/internal/llm"
)

// ErrToolNotFound is returned by Registry.Execute when the requested tool
// name isn't registered. The error is surfaced to the model as the result
// of the function call so it can recover.
var ErrToolNotFound = errors.New("agent: tool not found")

// Tool is implemented by concrete tool packages. Tools are stateless and
// safe for concurrent calls — the registry dispatches without synchronization.
type Tool interface {
	// Name is the tool name used in function declarations.
	Name() string

	// Declaration returns the provider-neutral function declaration
	// used in StreamRequest.Tools.
	Declaration() *llm.FunctionDeclaration

	// Execute runs the tool with the given arguments. The return value
	// must be JSON-marshalable. Errors are wrapped into a FunctionResponse
	// with an "error" key by the agent loop.
	Execute(ctx context.Context, args map[string]any) (any, error)

	// Idempotent reports whether repeated execution of this tool with the
	// same arguments produces the same outcome. The workflow uses this to
	// select a retry policy: idempotent tools get 3 retries; non-idempotent
	// tools get MaxAttempts=1.
	Idempotent() bool
}

// Registry owns a lookup table of tools. Registered at server startup.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry builds a Registry from the given tools. Duplicate names panic.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		if _, exists := r.tools[t.Name()]; exists {
			panic(fmt.Sprintf("agent: duplicate tool name %q", t.Name()))
		}
		r.tools[t.Name()] = t
	}
	return r
}

// Lookup returns the named tool and ok=true, or nil and false.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Declarations returns FunctionDeclarations for all registered tools,
// sorted by name for deterministic iteration.
func (r *Registry) Declarations() []*llm.FunctionDeclaration {
	decls := make([]*llm.FunctionDeclaration, 0, len(r.tools))
	for _, t := range r.tools {
		decls = append(decls, t.Declaration())
	}
	slices.SortFunc(decls, func(a, b *llm.FunctionDeclaration) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return decls
}

// IsIdempotent reports whether the named tool is idempotent. Returns false
// for unknown tools, making the safe default to treat them as non-idempotent.
func (r *Registry) IsIdempotent(name string) bool {
	t, ok := r.tools[name]
	if !ok {
		return false
	}
	return t.Idempotent()
}

// Execute looks up the tool and dispatches. Returns ErrToolNotFound if
// the name isn't registered.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (any, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrToolNotFound, name)
	}
	return t.Execute(ctx, args)
}
