//go:build linux || darwin

package credential

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRef() Reference { return Reference{Provider: "synthetic", ID: "account", Method: OAuth} }
func testMaterial() Material {
	return Material{Token: NewSecret(rand.Text()), RefreshToken: NewSecret(rand.Text()), Owner: Managed, ExpiresAt: time.Now().Add(-time.Hour)}
}
func testLocal(t *testing.T) *Local {
	t.Helper()
	s, err := OpenLocal(filepath.Join(t.TempDir(), "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestLocalPermissionsRedactionAndRoundtrip(t *testing.T) {
	s := testLocal(t)
	m := NewManager(s)
	ref := Reference{Provider: "openai", ID: "account", Method: APIKey}
	material := Material{Token: NewSecret(rand.Text()), Owner: Managed}
	if err := m.Login(t.Context(), ref, material); err != nil {
		t.Fatal(err)
	}
	got, err := m.Resolve(t.Context(), ref)
	if err != nil || got.Token.Reveal() != material.Token.Reveal() {
		t.Fatal("roundtrip failed", err)
	}
	meta, err := m.List(t.Context())
	if err != nil || len(meta) != 1 {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), material.Token.Reveal()) {
		t.Fatal("secret in metadata")
	}
	if err := m.Logout(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), ref); !IsCode(err, "not_found") {
		t.Fatal(err)
	}
	if err := os.Chmod(s.directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(s.directory); !IsCode(err, "insecure_storage") {
		t.Fatal(err)
	}
}
func TestExpiredCredentialDoesNotChangeRecord(t *testing.T) {
	s := testLocal(t)
	m := NewManager(s)
	ref := testRef()
	for _, owner := range []Owner{Managed, External} {
		material := testMaterial()
		material.Owner = owner
		if err := m.Login(t.Context(), ref, material); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.directory, s.key(ref)+".json")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code := "reauth_required"
		if owner == External {
			code = "external_reauth_required"
		}
		for range 2 {
			if _, err := m.Resolve(t.Context(), ref); !IsCode(err, code) {
				t.Fatal(err)
			}
		}
		after, err := os.ReadFile(path)
		if err != nil || string(before) != string(after) {
			t.Fatal("expiry changed credential", err)
		}
		// A token remains usable until its actual expiry, with no 30-second margin.
		material.ExpiresAt = time.Now().Add(20 * time.Second)
		if err := m.Login(t.Context(), ref, material); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Resolve(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogoutRemovesMalformedCredential(t *testing.T) {
	s := testLocal(t)
	path := filepath.Join(s.directory, s.key(testRef())+".json")
	if err := os.WriteFile(path, []byte("broken JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(s)
	if err := m.Logout(t.Context(), testRef()); err != nil {
		t.Fatal(err)
	}
	if err := m.Logout(t.Context(), testRef()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestPrivateFileAndLockCancellation(t *testing.T) {
	s := testLocal(t)
	ref := testRef()
	m := NewManager(s)
	if err := m.Login(t.Context(), ref, testMaterial()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.directory, s.key(ref)+".json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), ref); !IsCode(err, "insecure_storage") {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithCredential(t.Context(), ref, func(Transaction) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Logout(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("%+v", testMaterial())
	if strings.Contains(text, "value:") {
		t.Fatal("raw material formatting")
	}
}
