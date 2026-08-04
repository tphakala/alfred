package agent

import (
	"context"
	"fmt"
)

// SafeExecute wraps Registry.Execute with a panic recovery so a single
// tool crash doesn't kill the entire agent run.
func SafeExecute(ctx context.Context, reg *Registry, name string, args map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool %q panicked: %v", name, r)
		}
	}()
	return reg.Execute(ctx, name, args)
}
