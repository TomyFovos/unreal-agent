package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
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
	// NativeGrant is an opt-in v2 extension owned by the dedicated Native
	// lifecycle. It is committed in the same atomic replacement as Material.
	// Ordinary providers retain the v1 encoding and Manager behavior.
	NativeGrant *NativeGrantEvidence
}

// NativeGrantEvidence records verified grant provenance, not permission to
// infer or a billing assertion. MaterialHash binds it to this exact rotation.
type NativeGrantEvidence struct {
	RouteDigest  string   `json:"route_digest"`
	AccountID    string   `json:"account_id"`
	TeamID       string   `json:"team_id"`
	TokenKind    string   `json:"token_kind"`
	ScopeSource  string   `json:"scope_source"`
	Scopes       []string `json:"scopes"`
	Generation   uint64   `json:"generation"`
	Revision     uint64   `json:"revision"`
	MaterialHash string   `json:"material_hash"`
	// Revoked retains the account/route binding after logout while all token
	// material is erased. It can never be used for refresh or dispatch.
	Revoked bool `json:"revoked,omitzero"`
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

// NativeLogout erases material while retaining the verified ordinary account
// binding. A later login cannot silently switch accounts under existing saved
// Session references. Legacy providers retain the existing Remove semantics.
func (m *Manager) NativeLogout(ctx context.Context, ref Reference) error {
	if ref.Provider != "claude-native" || ref.Method != OAuth {
		return &Error{Code: "invalid_reference"}
	}
	return m.store.WithCredential(ctx, ref, func(tx Transaction) error {
		r, err := tx.Read()
		if IsCode(err, "not_found") {
			return nil
		}
		if err != nil {
			return err
		}
		if r.NativeGrant == nil {
			return &Error{Code: "native_verified_grant_required"}
		}
		g := *r.NativeGrant
		g.Scopes = slices.Clone(g.Scopes)
		g.Revoked = true
		r.NativeGrant = &g
		r.Material.Token = Secret{}
		r.Material.RefreshToken = Secret{}
		r.Material.ExpiresAt = time.Time{}
		r.Metadata.RefreshPending = false
		g.MaterialHash = NativeMaterialBindingHash(ref, r)
		return tx.Write(r)
	})
}

// NativeMaterialBindingHash is a private-record digest, never a public grant.
// It binds the opt-in evidence to exactly the material atomically stored beside
// it; no secret is returned, logged, or added to canonical history.
func NativeMaterialBindingHash(ref Reference, r Record) string {
	h := sha256.New()
	for _, v := range []string{ref.Provider, ref.ID, string(ref.Method), r.Material.AccountID, r.Material.Token.Reveal(), r.Material.RefreshToken.Reveal(), r.Material.ExpiresAt.Format(time.RFC3339Nano)} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
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
		if record.NativeGrant != nil {
			return &Error{Code: "native_owner_required"}
		}
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
