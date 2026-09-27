//go:build !cgo

package ast

import (
	"testing"
)

func TestBackendUnavailableIsExplicit(t *testing.T) {
	_, err := analyze(t.Context(), "javascript", "(identifier) @match", []byte("name"), nil)
	if err != Failure("unsupported_backend_cgo_required") {
		t.Fatal(err)
	}
}
