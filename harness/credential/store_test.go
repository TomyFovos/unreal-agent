//go:build linux || darwin

package credential

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	m := NewManager(s, nil)
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
func TestRefreshLogoutAndNoResurrection(t *testing.T) {
	s := testLocal(t)
	ref := testRef()
	started := make(chan struct{})
	release := make(chan struct{})
	m := NewManager(s, map[string]RefreshFunc{"synthetic": func(ctx context.Context, r Reference, old Material) (Material, error) {
		close(started)
		<-release
		old.Token = NewSecret(rand.Text())
		old.RefreshToken = NewSecret(rand.Text())
		old.ExpiresAt = time.Now().Add(time.Hour)
		return old, nil
	}})
	if err := m.Login(t.Context(), ref, testMaterial()); err != nil {
		t.Fatal(err)
	}
	resolved := make(chan error, 1)
	go func() { _, err := m.Resolve(t.Context(), ref); resolved <- err }()
	<-started
	loggedOut := make(chan error, 1)
	go func() { loggedOut <- m.Logout(t.Context(), ref) }()
	close(release)
	if err := <-resolved; err != nil {
		t.Fatal(err)
	}
	if err := <-loggedOut; err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), ref); !IsCode(err, "not_found") {
		t.Fatalf("resurrected after logout: %v", err)
	}
}
func TestRefreshFailureIsBoundedAndExternalOwnership(t *testing.T) {
	s := testLocal(t)
	ref := testRef()
	var calls atomic.Int64
	m := NewManager(s, map[string]RefreshFunc{"synthetic": func(context.Context, Reference, Material) (Material, error) {
		calls.Add(1)
		return Material{}, errors.New(rand.Text())
	}})
	material := testMaterial()
	if err := m.Login(t.Context(), ref, material); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), ref); !IsCode(err, "refresh_failed") {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), ref); !IsCode(err, "reauth_required") {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("ambiguous refresh was retried")
	}
	material.Owner = External
	if err := m.Login(t.Context(), ref, material); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(t.Context(), ref); !IsCode(err, "external_reauth_required") {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("refreshed externally owned credential")
	}
}

type failingStore struct {
	Store
	writes int
}
type failingTx struct {
	Transaction
	parent *failingStore
}

func (s *failingStore) WithCredential(ctx context.Context, r Reference, fn func(Transaction) error) error {
	return s.Store.WithCredential(ctx, r, func(tx Transaction) error { return fn(&failingTx{tx, s}) })
}
func (t *failingTx) Write(r Record) error {
	t.parent.writes++
	if t.parent.writes == 2 {
		return errors.New("injected commit failure")
	}
	return t.Transaction.Write(r)
}
func TestRefreshPersistenceFailureRemainsIndeterminate(t *testing.T) {
	s := testLocal(t)
	ref := testRef()
	m := NewManager(s, nil)
	if err := m.Login(t.Context(), ref, testMaterial()); err != nil {
		t.Fatal(err)
	}
	f := &failingStore{Store: s}
	var calls int
	refresh := func(context.Context, Reference, Material) (Material, error) {
		calls++
		v := testMaterial()
		v.ExpiresAt = time.Now().Add(time.Hour)
		return v, nil
	}
	manager := NewManager(f, map[string]RefreshFunc{"synthetic": refresh})
	if _, err := manager.Resolve(t.Context(), ref); !IsCode(err, "refresh_persist_failed") {
		t.Fatal(err)
	}
	if _, err := NewManager(s, map[string]RefreshFunc{"synthetic": refresh}).Resolve(t.Context(), ref); !IsCode(err, "reauth_required") {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("replayed consumed refresh token")
	}
}
func TestCredentialProcessHelper(t *testing.T) {
	dir := os.Getenv("UNREAL_CREDENTIAL_TEST_DIR")
	if dir == "" {
		return
	}
	s, err := OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(s, map[string]RefreshFunc{"synthetic": func(ctx context.Context, ref Reference, current Material) (Material, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", os.Getenv("UNREAL_CREDENTIAL_TEST_URL"), nil)
		if err != nil {
			return Material{}, err
		}
		req.Header.Set("Authorization", "Bearer "+current.RefreshToken.Reveal())
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return Material{}, err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return Material{}, errors.New("refresh rejected")
		}
		var next struct {
			Access  string
			Refresh string
		}
		if err := json.UnmarshalRead(response.Body, &next); err != nil {
			return Material{}, err
		}
		return Material{Token: NewSecret(next.Access), RefreshToken: NewSecret(next.Refresh), Owner: Managed, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}})
	result, err := manager.Resolve(t.Context(), testRef())
	if err != nil {
		t.Fatal(err)
	}
	// Server verifies every child used the newly committed access token, not merely
	// that only one refresh happened.
	req, _ := http.NewRequestWithContext(t.Context(), "GET", os.Getenv("UNREAL_CREDENTIAL_TEST_URL"), nil)
	req.Header.Set("Authorization", "Bearer "+result.Token.Reveal())
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("stale access credential")
	}
}
func TestSeparateProcessesSerializeRotatingRefresh(t *testing.T) {
	s := testLocal(t)
	old := testMaterial()
	m := NewManager(s, nil)
	if err := m.Login(t.Context(), testRef(), old); err != nil {
		t.Fatal(err)
	}
	nextAccess, nextRefresh := rand.Text(), rand.Text()
	var refreshes, uses atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if r.Header.Get("Authorization") != "Bearer "+old.RefreshToken.Reveal() || refreshes.Add(1) != 1 {
				w.WriteHeader(401)
				return
			}
			_ = json.MarshalWrite(w, struct {
				Access  string
				Refresh string
			}{nextAccess, nextRefresh})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+nextAccess {
			w.WriteHeader(401)
			return
		}
		uses.Add(1)
		w.WriteHeader(200)
	}))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const children = 5
	cmds := make([]*exec.Cmd, children)
	for i := range cmds {
		cmds[i] = exec.CommandContext(t.Context(), executable, "-test.run=^TestCredentialProcessHelper$")
		cmds[i].Env = append(os.Environ(), "UNREAL_CREDENTIAL_TEST_DIR="+s.directory, "UNREAL_CREDENTIAL_TEST_URL="+server.URL)
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, cmd := range cmds {
		wg.Go(func() {
			if err := cmd.Wait(); err != nil {
				t.Errorf("child failed: %v", err)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 1 || uses.Load() != children {
		t.Fatalf("refresh=%d use=%d", refreshes.Load(), uses.Load())
	}
}
func TestPrivateFileAndLockCancellation(t *testing.T) {
	s := testLocal(t)
	ref := testRef()
	m := NewManager(s, nil)
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
