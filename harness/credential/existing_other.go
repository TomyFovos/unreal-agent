//go:build !linux

package credential

import (
	"context"
	"os"
)

// Descriptor-pinned existing-owner continuation is implemented only on Linux.
// Ordinary Darwin OpenLocal retains its existing implementation.
type existingLocal struct{}

func OpenExistingLocal(string) (*Local, error) {
	return nil, &Error{Code: "unsupported_private_storage"}
}

func OpenExistingLocalAt(*os.File, string) (*Local, error) {
	return nil, &Error{Code: "unsupported_private_storage"}
}

func (*Local) Close() error { return nil }

func (*existingLocal) withCredential(context.Context, *Local, Reference, func(Transaction) error) error {
	return &Error{Code: "unsupported_private_storage"}
}

func (*existingLocal) list(context.Context, *Local) ([]Metadata, error) {
	return nil, &Error{Code: "unsupported_private_storage"}
}
