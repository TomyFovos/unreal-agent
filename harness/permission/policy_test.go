package permission

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newPolicy(t *testing.T, config Config) *Policy {
	t.Helper()
	p, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func assertDenied(t *testing.T, err error, code Code) {
	t.Helper()
	failure := Failure(err)
	if failure == nil || failure.Code != code {
		t.Fatalf("failure = %v, want %s", err, code)
	}
}

func TestPolicyDefaultsAndChildIntersection(t *testing.T) {
	root := t.TempDir()
	config := Config{Tools: []string{"ViewImage"}, ReadRoots: []string{root}, NetworkOrigins: []string{"https://example.com:443"}}
	parent := newPolicy(t, config)
	config.Tools[0] = "Bash"
	config.ReadRoots[0] = "/"
	config.NetworkOrigins[0] = "https://other.example"
	ctx := WithPolicy(WithPolicy(t.Context(), parent), Unrestricted())
	child := FromContext(ctx)
	if err := child.CheckTool("ViewImage"); err != nil {
		t.Fatal(err)
	}
	assertDenied(t, child.CheckTool("Bash"), Denied)
	assertDenied(t, child.CheckProcess(), Denied)
	assertDenied(t, child.CheckPath(filepath.Join(root, "file"), true), Denied)
	assertDenied(t, FromContext(WithPolicy(t.Context(), nil)).CheckTool("ViewImage"), Denied)
	parsed, _ := url.Parse("https://example.com/path")
	if err := child.CheckURL(parsed); err != nil {
		t.Fatal(err)
	}
	parsed, _ = url.Parse("https://other.example")
	assertDenied(t, child.CheckURL(parsed), Denied)
	if !Configured(ctx) || Configured(t.Context()) {
		t.Fatal("explicit scope is not distinguished")
	}
	if err := Unrestricted().CheckProcess(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessRestrictionsFailClosed(t *testing.T) {
	for _, config := range []Config{
		{ProcessMode: ProcessSandboxed},
		{ProcessMode: ProcessUnrestricted, FilesystemUnrestricted: true},
		{ProcessMode: ProcessUnrestricted, NetworkUnrestricted: true},
	} {
		assertDenied(t, newPolicy(t, config).CheckProcess(), Unsupported)
	}
	p := newPolicy(t, Config{ProcessMode: ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true})
	if err := p.CheckProcess(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{ProcessMode: "unknown"}); err == nil {
		t.Fatal("accepted invalid mode")
	}
}

func TestRootedFileAccessAndSymlinkEscape(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(allowed, "inside")
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(inside, []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(allowed, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside", filepath.Join(allowed, "alias")); err != nil {
		t.Fatal(err)
	}
	p := newPolicy(t, Config{ReadRoots: []string{allowed}, WriteRoots: []string{allowed}})
	for _, path := range []string{secret, filepath.Join(allowed, "escape", "secret"), allowed + "/../secret"} {
		_, err := p.OpenFile(path, os.O_RDONLY, 0)
		assertDenied(t, err, Denied)
	}
	file, err := p.OpenFile(filepath.Join(allowed, "alias"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "allowed" {
		t.Fatalf("data=%s err=%v", data, err)
	}
	created := filepath.Join(allowed, "new")
	file, err = p.OpenFile(created, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := os.Stat(created); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedRootAndNarrowerChild(t *testing.T) {
	base := t.TempDir()
	broad, narrow := filepath.Join(base, "broad"), filepath.Join(base, "broad", "child")
	if err := os.MkdirAll(narrow, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(narrow, "data"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	parent := newPolicy(t, Config{ReadRoots: []string{broad}})
	child := newPolicy(t, Config{ReadRoots: []string{narrow}})
	p := parent.Intersect(child)
	assertDenied(t, p.CheckPath(filepath.Join(broad, "outside"), false), Denied)
	// A newly created directory at the same lexical root must not replace the
	// originally authorized directory identity.
	moved := filepath.Join(base, "moved")
	if err := os.Rename(broad, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(narrow, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(narrow, "data"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := p.OpenFile(filepath.Join(narrow, "data"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "original" {
		t.Fatalf("root identity changed: %q %v", data, err)
	}
}

func TestNetworkOriginsRedirectsAndSecretFreeErrors(t *testing.T) {
	var forbidden atomic.Int32
	blocked := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forbidden.Add(1) }))
	defer blocked.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL+"/secret?token=DO_NOT_LOG", http.StatusFound)
	}))
	defer allowed.Close()
	p := newPolicy(t, Config{NetworkOrigins: []string{allowed.URL}})
	client := &http.Client{Transport: p.RoundTripper(nil)}
	defer client.CloseIdleConnections()
	_, err := client.Get(allowed.URL)
	assertDenied(t, err, Denied)
	if forbidden.Load() != 0 {
		t.Fatal("redirect bypassed destination policy")
	}
	if strings.Contains(Failure(err).Error(), "DO_NOT_LOG") || strings.Contains(Failure(err).Error(), blocked.URL) {
		t.Fatal("denial leaked request data")
	}
	request, _ := http.NewRequest("GET", allowed.URL, nil)
	request = request.WithContext(WithPolicy(t.Context(), DenyAll()))
	_, err = client.Do(request)
	assertDenied(t, err, Denied)
	u, _ := url.Parse("https://user:secret@example.com")
	assertDenied(t, Unrestricted().CheckURL(u), Denied)
	for _, origin := range []string{"https://u:secret@example.com", "https://example.com/path", "file:///tmp/file", "https://example.com?token=secret"} {
		if _, err := New(Config{NetworkOrigins: []string{origin}}); err == nil {
			t.Fatal("accepted invalid origin")
		}
	}
}

func TestRestrictedTransportDoesNotUseAmbientProxy(t *testing.T) {
	p := newPolicy(t, Config{})
	guarded := p.RoundTripper(nil).(*transport)
	base := guarded.base.(*http.Transport)
	if base.Proxy != nil {
		t.Fatal("restricted transport inherits ambient proxy")
	}
	guarded.CloseIdleConnections()
	if !errors.As(&Error{Code: Denied}, new(*Error)) {
		t.Fatal("typed failure")
	}
}

func TestReadWriteOpenRequiresBothGrants(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	p := newPolicy(t, Config{WriteRoots: []string{root}})
	_, err := p.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0)
	assertDenied(t, err, Denied)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "untouched" {
		t.Fatal("read-write denial truncated the file")
	}
}

func TestRootedOpenCannotFollowRacingSymlinkOutsideRoot(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(allowed, "inside"), []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	p := newPolicy(t, Config{ReadRoots: []string{allowed}})
	link := filepath.Join(allowed, "link")
	if err := os.Symlink("inside", link); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		temp := filepath.Join(allowed, "next")
		for range 150 {
			_ = os.Symlink(secret, temp)
			_ = os.Rename(temp, link)
			_ = os.Symlink("inside", temp)
			_ = os.Rename(temp, link)
		}
	}()
	for range 300 {
		file, err := p.OpenFile(link, os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "allowed" {
			t.Errorf("rooted access escaped: %q", data)
			break
		}
	}
	<-done
}
