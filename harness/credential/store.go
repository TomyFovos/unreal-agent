package credential

import (
	"context"
	"fmt"
	"time"
)

type Metadata struct {
	// RefreshPending is decoded only for compatibility with older credential records.
	RefreshPending bool      `json:"refresh_pending,omitzero"`
	Reference      Reference `json:"reference"`
	Owner          Owner     `json:"owner"`
	ExpiresAt      time.Time `json:"expires_at,omitzero"`
	Revision       uint64    `json:"revision"`
}
type Record struct {
	Metadata Metadata
	Material Material
}

// Transaction is valid only during WithCredential. Backends must serialize it
// across processes sharing an account, reread under the lock, and commit atomically.
type Transaction interface {
	Read() (Record, error)
	Write(Record) error
	Remove() error
}
type Store interface {
	WithCredential(context.Context, Reference, func(Transaction) error) error
	List(context.Context) ([]Metadata, error)
}
type Manager struct {
	store Store
	now   func() time.Time
}

func NewManager(store Store) *Manager {
	return &Manager{store: store, now: time.Now}
}
func (m *Manager) Login(ctx context.Context, ref Reference, material Material) error {
	if ref.Method == DockerSandboxOAuth {
		return &Error{Code: "unsupported_managed_source"}
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := validateMaterial(ref, material); err != nil {
		return err
	}
	return m.store.WithCredential(ctx, ref, func(tx Transaction) error {
		return tx.Write(Record{Metadata: Metadata{Reference: ref, Owner: material.Owner, ExpiresAt: material.ExpiresAt}, Material: material})
	})
}
func (m *Manager) Logout(ctx context.Context, ref Reference) error {
	return m.store.WithCredential(ctx, ref, func(tx Transaction) error { return tx.Remove() })
}

func (m *Manager) List(ctx context.Context) ([]Metadata, error) { return m.store.List(ctx) }
func (m *Manager) Resolve(ctx context.Context, ref Reference) (Material, error) {
	if ref.Method == DockerSandboxOAuth {
		return Material{}, &Error{Code: "unsupported_managed_source"}
	}
	if err := ref.Validate(); err != nil {
		return Material{}, err
	}
	var result Material
	err := m.store.WithCredential(ctx, ref, func(tx Transaction) error {
		record, err := tx.Read()
		if err != nil {
			return err
		}
		current := record.Material
		if record.Metadata.Reference != ref {
			return &Error{Code: "reference_mismatch"}
		}
		if ref.Method == APIKey || current.ExpiresAt.IsZero() || m.now().Before(current.ExpiresAt) {
			result = current
			return nil
		}
		if current.Owner != Managed {
			return fmt.Errorf("credential expired: %w", &Error{Code: "external_reauth_required"})
		}
		return fmt.Errorf("credential expired: %w", &Error{Code: "reauth_required"})
	})
	return result, err
}
func validateMaterial(ref Reference, v Material) error {
	if v.Token.Reveal() == "" || (v.Owner != Managed && v.Owner != External) {
		return &Error{Code: "invalid_material"}
	}
	for _, r := range v.Token.Reveal() {
		if r <= ' ' || r > '~' {
			return &Error{Code: "invalid_material"}
		}
	}
	if ref.Method == APIKey && (v.RefreshToken.Reveal() != "" || !v.ExpiresAt.IsZero()) {
		return &Error{Code: "invalid_material"}
	}
	if ref.Method == OAuth && v.ExpiresAt.IsZero() {
		return &Error{Code: "expiry_required"}
	}
	return nil
}
