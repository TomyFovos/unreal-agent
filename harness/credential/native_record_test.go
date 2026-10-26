//go:build linux || darwin

package credential

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeRecordFixture() Record {
	ref := Reference{Provider: "claude-native", ID: "normal-synthetic", Method: OAuth}
	r := Record{Metadata: Metadata{Reference: ref}, Material: Material{Token: NewSecret("SYNTHETIC_ACCESS"), RefreshToken: NewSecret("SYNTHETIC_REFRESH"), AccountID: "SYNTHETIC_ACCOUNT", Owner: Managed, ExpiresAt: time.Now().Add(time.Hour)}, NativeGrant: &NativeGrantEvidence{RouteDigest: strings.Repeat("a", 64), AccountID: "SYNTHETIC_ACCOUNT", TeamID: "SYNTHETIC_TEAM", TokenKind: "OAUTH_BEARER", ScopeSource: "BOUND_TOKEN_RESPONSE", Scopes: []string{"user:profile", "user:inference"}, Generation: 1}}
	r.NativeGrant.MaterialHash = NativeMaterialBindingHash(ref, r)
	return r
}
func TestNativeV2AtomicRecordAndV1Compatibility(t *testing.T) {
	s := testLocal(t)
	r := nativeRecordFixture()
	if err := s.WithCredential(t.Context(), r.Metadata.Reference, func(tx Transaction) error { return tx.Write(r) }); err != nil {
		t.Fatal(err)
	}
	if err := s.WithCredential(t.Context(), r.Metadata.Reference, func(tx Transaction) error {
		got, err := tx.Read()
		if err != nil {
			return err
		}
		if got.NativeGrant == nil || got.NativeGrant.Revision != got.Metadata.Revision || got.NativeGrant.MaterialHash != NativeMaterialBindingHash(got.Metadata.Reference, got) {
			t.Fatal("grant/material not coherent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.directory, s.key(r.Metadata.Reference)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var d diskRecord
	if err := json.Unmarshal(data, &d); err != nil || d.Version != 2 {
		t.Fatal("native v2 missing", err)
	}
	oldRef := Reference{Provider: "openai", ID: "existing", Method: APIKey}
	m := NewManager(s, nil)
	if err := m.Login(t.Context(), oldRef, Material{Token: NewSecret("SYNTHETIC_KEY"), Owner: Managed}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(s.directory, s.key(oldRef)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	d = diskRecord{}
	if err := json.Unmarshal(data, &d); err != nil || d.Version != 1 || d.NativeGrant != nil {
		t.Fatal("ordinary provider changed encoding", err)
	}
	entries, err := m.List(t.Context())
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	metadata, _ := json.Marshal(entries)
	if strings.Contains(string(metadata), "SYNTHETIC_ACCESS") || strings.Contains(string(metadata), "native_grant") {
		t.Fatal("private grant escaped metadata list")
	}
}
func TestNativeLogoutRetainsBindingAndErasesMaterial(t *testing.T) {
	s := testLocal(t)
	r := nativeRecordFixture()
	ref := r.Metadata.Reference
	if err := s.WithCredential(t.Context(), ref, func(tx Transaction) error { return tx.Write(r) }); err != nil {
		t.Fatal(err)
	}
	m := NewManager(s, nil)
	if err := m.NativeLogout(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := s.WithCredential(t.Context(), ref, func(tx Transaction) error {
		got, err := tx.Read()
		if err != nil {
			return err
		}
		if !got.NativeGrant.Revoked || got.Material.Token.Reveal() != "" || got.Material.RefreshToken.Reveal() != "" || !got.Material.ExpiresAt.IsZero() || got.NativeGrant.AccountID != r.Material.AccountID || got.NativeGrant.MaterialHash != NativeMaterialBindingHash(ref, got) {
			t.Fatal("invalid revocation tombstone")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Logout(t.Context(), ref); !IsCode(err, "native_owner_required") {
		t.Fatal("generic logout erased binding", err)
	}
	if err := m.Login(t.Context(), ref, r.Material); !IsCode(err, "native_owner_required") {
		t.Fatal("generic login downgraded v2", err)
	}
	data, err := os.ReadFile(filepath.Join(s.directory, s.key(ref)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SYNTHETIC_ACCESS") || strings.Contains(string(data), "SYNTHETIC_REFRESH") {
		t.Fatal("logout retained material")
	}
}
func TestNativeV2RejectsCrossProviderAndMalformedEvidence(t *testing.T) {
	for _, spec := range []struct {
		name   string
		mutate func(*Record)
	}{
		{"other_provider", func(r *Record) { r.Metadata.Reference.Provider = "openai" }},
		{"missing_scope", func(r *Record) { r.NativeGrant.Scopes = nil }},
		{"duplicate_scope", func(r *Record) { r.NativeGrant.Scopes = []string{"user:profile", "user:profile"} }},
		{"invalid_scope_source", func(r *Record) { r.NativeGrant.ScopeSource = "REQUESTED" }},
		{"zero_generation", func(r *Record) { r.NativeGrant.Generation = 0 }},
		{"invalid_hash", func(r *Record) { r.NativeGrant.MaterialHash = "bad" }},
		{"mismatched_hash", func(r *Record) { r.NativeGrant.MaterialHash = strings.Repeat("0", 64) }},
		{"revoked_with_material", func(r *Record) { r.NativeGrant.Revoked = true }},
	} {
		t.Run(spec.name, func(t *testing.T) {
			s := testLocal(t)
			r := nativeRecordFixture()
			spec.mutate(&r)
			if err := s.WithCredential(t.Context(), r.Metadata.Reference, func(tx Transaction) error { return tx.Write(r) }); !IsCode(err, "invalid_record") {
				t.Fatal(err)
			}
		})
	}
}
