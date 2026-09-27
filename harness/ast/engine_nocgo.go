//go:build !cgo

package ast

import (
	"context"
)

func analyze(context.Context, string, string, []byte, *string) (analysis, error) {
	return analysis{}, Failure("unsupported_backend_cgo_required")
}
