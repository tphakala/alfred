package runner

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a thread-safe map of runner name to Runner.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]Runner
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{runners: map[string]Runner{}}
}

// Register adds a runner. Panics if the name is already registered.
func (r *Registry) Register(rn Runner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := rn.Name()
	if _, exists := r.runners[name]; exists {
		panic(fmt.Sprintf("runner %q already registered", name))
	}
	r.runners[name] = rn
}

// Get returns the runner registered for name and a bool indicating presence.
func (r *Registry) Get(name string) (Runner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rn, ok := r.runners[name]
	return rn, ok
}

// Names returns the sorted list of registered runner names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.runners))
	for n := range r.runners {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
