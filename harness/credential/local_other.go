//go:build !linux && !darwin

package credential

import "context"

type Local struct{}

func OpenLocal(string) (*Local, error) { return nil, &Error{Code: "unsupported_private_storage"} }
func (*Local) WithCredential(context.Context, Reference, func(Transaction) error) error {
	return &Error{Code: "unsupported_private_storage"}
}
func (*Local) List(context.Context) ([]Metadata, error) {
	return nil, &Error{Code: "unsupported_private_storage"}
}
