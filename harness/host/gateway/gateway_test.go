//go:build linux || darwin

package gateway

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type model struct{}

func (model) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "completed"}}}}, nil
}
func fixture(t *testing.T) (*host.Host, *Client, string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ua-gateway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	store := filepath.Join(dir, "sessions")
	owner, err := host.New(t.Context(), host.Config{Directory: store, Build: func(ctx context.Context, id session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: model{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	backend, err := credential.OpenLocal(filepath.Join(dir, "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	socket := filepath.Join(dir, "host.sock")
	go func() {
		done <- ListenAndServe(ctx, socket, Config{Host: owner, Policy: permission.Unrestricted(), Auth: authflow.New(credential.NewManager(backend, nil))})
	}()
	client := NewClient(socket)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		client.Close()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.Methods(t.Context()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return owner, client, socket, store
}
func prompt(id, text string) inbox.Input {
	payload, _ := json.Marshal(text)
	return inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload}
}
func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, c := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(c)
	return ctx
}
func TestGatewayClientProcess(t *testing.T) {
	if socket := os.Getenv("UA_TEST_GATEWAY"); socket != "" {
		c := NewClient(socket)
		defer c.Close()
		v, err := c.Inspect(bounded(t), "external", 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := c.Subscribe(bounded(t), "external", 0, 128, 1)
		if err != nil {
			t.Fatal(err)
		}
		sub.Cancel()
		if _, err = c.Submit(bounded(t), "external", v.Generation, prompt("child-prompt", "hello from another process")); err != nil {
			t.Fatal(err)
		}
		return
	}
	owner, client, socket, _ := fixture(t)
	v, err := client.Open(bounded(t), host.Create, "external")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(bounded(t), os.Args[0], "-test.run=^TestGatewayClientProcess$")
	cmd.Env = append(os.Environ(), "UA_TEST_GATEWAY="+socket)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("client process: %v %s", err, output)
	}
	current, _ := owner.Attach("external")
	select {
	case <-current.Done():
		t.Fatal("client disconnect stopped owner")
	default:
	}
	if _, err = client.Open(bounded(t), host.Resume, "external"); !errors.Is(err, localfile.ErrWriterOwned) {
		t.Fatalf("contention: %v", err)
	}
	if _, err = client.Submit(bounded(t), "external", "old", prompt("wrong", "no")); !errors.Is(err, host.ErrStaleGeneration) {
		t.Fatalf("stale: %v", err)
	}
	r1, err := client.Submit(bounded(t), "external", v.Generation, prompt("second", "second prompt"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := client.Submit(bounded(t), "external", v.Generation, prompt("second", "second prompt"))
	if err != nil || r1 != r2 {
		t.Fatal("duplicate receipt")
	}
	if _, err = client.Stop(bounded(t), "external", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	if err = current.Wait(bounded(t)); err != nil {
		t.Fatal(err)
	}
	resumed, err := client.Open(bounded(t), host.Resume, "external")
	if err != nil || resumed.Generation == v.Generation {
		t.Fatalf("resume: %v", err)
	}
	if _, err = client.Submit(bounded(t), "external", resumed.Generation, prompt("third", "after resume")); err != nil {
		t.Fatal(err)
	}
	final, err := client.Inspect(bounded(t), "external", 0, 256)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[inbox.ID]int{}
	for _, item := range final.History.Items {
		if item.Kind == sessionstore.ItemInput {
			seen[item.Data.(inbox.Input).ID]++
		}
	}
	if seen["second"] != 1 || seen["child-prompt"] != 1 || seen["third"] != 1 {
		t.Fatalf("history: %v", seen)
	}
}
func TestGatewayAuthNeverEntersHistory(t *testing.T) {
	_, client, _, store := fixture(t)
	client.Open(bounded(t), host.Create, "auth-test")
	ref := credential.Reference{Provider: "openai", Method: credential.APIKey, ID: "key"}
	secret := "private-ui-secret-must-never-appear"
	if _, err := client.Login(bounded(t), ref, credential.NewSecret(secret)); err != nil {
		t.Fatal(err)
	}
	entries, err := client.ListCredentials(bounded(t))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if _, err = client.Login(bounded(t), credential.Reference{Provider: "anthropic", Method: credential.OAuth, ID: "no"}, credential.NewSecret(secret)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("unsupported auth or secret exposure")
	}
	if err = client.Logout(bounded(t), ref); err != nil {
		t.Fatal(err)
	}
	filepath.WalkDir(store, func(path string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), secret) {
				t.Errorf("secret in session %s", path)
			}
		}
		return e
	})
}

type blockedWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(p)
}
func TestSlowGatewayWriterCannotBlockOwner(t *testing.T) {
	owner, client, _, _ := fixture(t)
	v, err := client.Open(bounded(t), host.Create, "slow")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(request{ID: "slow", Capacity: 1})
	req := httptest.NewRequest("POST", "http://localhost/v1/subscribe", strings.NewReader(string(body)))
	writer := &blockedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); Handler(Config{Host: owner}).ServeHTTP(writer, req) }()
	<-writer.entered
	if _, err = client.Submit(bounded(t), "slow", v.Generation, prompt("unblocked", "continue")); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Stop(bounded(t), "slow", v.Generation, inbox.StopWhenIdle, "stop"); err != nil {
		t.Fatal(err)
	}
	current, _ := owner.Attach("slow")
	if err = current.Wait(bounded(t)); err != nil {
		t.Fatal(err)
	}
	close(writer.release)
	select {
	case <-done:
	case <-bounded(t).Done():
		t.Fatal("subscriber did not terminate")
	}
	if !strings.Contains(writer.Body.String(), "gap") {
		t.Fatal("missing backpressure gap")
	}
}
func TestSocketOwnershipAndProtocolBounds(t *testing.T) {
	owner, client, socket, _ := fixture(t)
	if _, _, err := listen(socket); err == nil {
		t.Fatal("second socket owner accepted")
	}
	stat, err := os.Stat(socket)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal(stat, err)
	}
	if _, err = client.call(bounded(t), "inspect", request{ID: "x", Limit: 257}); err == nil {
		t.Fatal("unbounded request accepted")
	}
	body := strings.Repeat("x", MaxRequestBytes+1)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://localhost/v1/inspect", strings.NewReader(body))
	Handler(Config{Host: owner}).ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(b), "invalid_request") {
		t.Fatal("request bound ignored")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestBoundedExtensionAndLocalRequestBoundary(t *testing.T) {
	calls := 0
	handler := Handler(Config{Extension: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; body, _ := io.ReadAll(r.Body); w.Write(body) })})
	c := NewClient("unused")
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})
	value := jsontext.Value(`{"action":"inspect_child"}`)
	result, err := c.Extension(t.Context(), value)
	if err != nil || string(result) != string(value) {
		t.Fatal(string(result), err)
	}
	if _, err = c.Extension(t.Context(), jsontext.Value(`{"large":"`+strings.Repeat("x", MaxRequestBytes)+`"}`)); err == nil {
		t.Fatal("oversized extension accepted")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://localhost/v1/extension", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://untrusted.example")
	handler.ServeHTTP(w, req)
	if calls != 1 || w.Code != http.StatusBadRequest {
		t.Fatal("extension bypassed local origin validation")
	}
}
