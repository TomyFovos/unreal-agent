package credential

import (
	"context"
	"time"
)

type Metadata struct {
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
type RefreshFunc func(context.Context, Reference, Material) (Material, error)
type Manager struct {
	store      Store
	refreshers map[string]RefreshFunc
	now        func() time.Time
	skew       time.Duration
}

func NewManager(store Store, refreshers map[string]RefreshFunc) *Manager {
	copied := map[string]RefreshFunc{}
	for k, v := range refreshers {
		copied[k] = v
	}
	return &Manager{store: store, refreshers: copied, now: time.Now, skew: 30 * time.Second}
}
func (m *Manager) Login(ctx context.Context, ref Reference, material Material) error {
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
		if record.Metadata.RefreshPending {
			return &Error{Code: "reauth_required"}
		}
		if record.Metadata.Reference != ref {
			return &Error{Code: "reference_mismatch"}
		}
		if ref.Method == APIKey || current.ExpiresAt.IsZero() || m.now().Add(m.skew).Before(current.ExpiresAt) {
			result = current
			return nil
		}
		if current.Owner != Managed {
			return &Error{Code: "external_reauth_required"}
		}
		refresh := m.refreshers[ref.Provider]
		if refresh == nil || current.RefreshToken.Reveal() == "" {
			return &Error{Code: "reauth_required"}
		}
		// The old rotating refresh token is used once, while the account lock is held.
		// No automatic retry: the server may have consumed it even on transport failure.
		// Persist uncertainty before exchange. A crash or consumed-token response
		// followed by a failed save cannot cause a second process to reuse that token.
		record.Metadata.RefreshPending = true
		if err := tx.Write(record); err != nil {
			return &Error{Code: "refresh_persist_failed"}
		}
		next, err := refresh(ctx, ref, current)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &Error{Code: "refresh_failed"}
		}
		if next.Owner != Managed || validateMaterial(ref, next) != nil || !next.ExpiresAt.After(m.now().Add(m.skew)) {
			return &Error{Code: "invalid_refresh_result"}
		}
		record.Material = next
		record.Metadata.RefreshPending = false
		record.Metadata.ExpiresAt = next.ExpiresAt
		if err := tx.Write(record); err != nil {
			return &Error{Code: "refresh_persist_failed"}
		}
		result = next
		return nil
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
