//go:build tools

package apiv1gen

import (
	// Ensure oapi-codegen is tracked as a module dependency.
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
	// Runtime types used by generated code.
	_ "github.com/oapi-codegen/runtime/types"
)
