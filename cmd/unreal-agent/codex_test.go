//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type credentialOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *credentialOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *credentialOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func waitInteractive(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("interactive condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func privateCLIDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ua-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func codexServeConfiguration(workspace, endpoint string) serveConfiguration {
	return serveConfiguration{
		Runtime:     agentrunner.RuntimeIdentity{Version: 1, Workspace: workspace, SystemPrompt: "Help with this workspace.", ReasoningEffort: llm.ReasoningEffort("low"), Profile: profile.Default(), Provider: provider.Selection{Version: 1, Provider: "openai-codex", Model: provider.Model{ID: "gpt-6.1-sol", Family: "openai-reasoning", Capabilities: []string{"tools", "reasoning"}}, Endpoint: endpoint, Auth: codexReference(), Source: "operator configuration", MaxAttempts: 1}},
		Permissions: permission.Config{NetworkOrigins: []string{endpoint}},
	}
}

func startInteractiveHost(t *testing.T, config serveConfiguration, options ...string) (*gateway.Client, string, *credentialOutput) {
	t.Helper()
	dir := privateCLIDirectory(t)
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "sessions")
	socket := filepath.Join(dir, "host.sock")
	args := append([]string{"serve", "--config", path, "--session-directory", sessions, "--socket", socket}, options...)
	ctx, cancel := context.WithCancel(t.Context())
	output := &credentialOutput{}
	done := make(chan struct{})
	var serveErr error
	go func() { serveErr = run(ctx, args, output); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if serveErr != nil {
				t.Errorf("serve failed: %v", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not drain")
		}
	})
	client := gateway.NewClient(socket)
	t.Cleanup(func() { _ = client.Close() })
	waitInteractive(t, func() bool {
		select {
		case <-done:
			t.Fatalf("serve startup failed: %v", serveErr)
		default:
		}
		_, err := client.Methods(t.Context())
		return err == nil
	})
	return client, sessions, output
}

func assertNoCredentialLeak(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatal("credential material leaked")
		}
	}
}

func assertNoStoredCredentials(t *testing.T, directory string, secrets ...string) {
	t.Helper()
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil {
			assertNoCredentialLeak(t, string(data), secrets...)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func submitInteractive(t *testing.T, client *gateway.Client, view host.View, id, text string) {
	t.Helper()
	payload, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Submit(t.Context(), view.Session.Session.ID, view.Generation, inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func inspectInteractive(t *testing.T, client *gateway.Client, id session.ID, ready func(host.View) bool) host.View {
	t.Helper()
	var view host.View
	waitInteractive(t, func() bool {
		var err error
		view, err = client.Inspect(t.Context(), id, 0, 256)
		if err != nil {
			t.Fatal(err)
		}
		return ready(view)
	})
	return view
}

func responseCount(view host.View) int {
	n := 0
	for _, item := range view.History.Items {
		if item.Kind == sessionstore.ItemModelResponse {
			n++
		}
	}
	return n
}

func TestServeExternalCodexTUIAndRotation(t *testing.T) {
	tokens := []string{"first-codex-token-sensitive", "rotated-codex-token-sensitive"}
	accounts := []string{"first-account-sensitive", "rotated-account-sensitive"}
	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodexAuth(t, path, tokens[0], accounts[0])
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		index := min(n-1, 1)
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+tokens[index] || r.Header.Get("ChatGPT-Account-ID") != accounts[index] {
			t.Error("incorrect hosted endpoint or external credential headers")
		}
		var request struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			Store     *bool  `json:"store"`
			MaxOutput *int   `json:"max_output_tokens"`
			Tools     []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil || request.Model != "gpt-6.1-sol" || len(request.Input) == 0 || request.Input[0].Role != "system" || !request.Stream || request.Store == nil || *request.Store || request.MaxOutput != nil {
			t.Error("request is not hosted Codex compatible")
		}
		if len(request.Input) > 0 {
			if instructions, ok := request.Input[0].Content.(string); !ok || instructions == "" {
				t.Error("request lost system instructions")
			}
		}
		for _, name := range []string{"Bash", "ViewImage", "read", "write", "edit", "grep", "glob", "ast_grep", "ast_edit"} {
			found := false
			for _, definition := range request.Tools {
				found = found || definition.Name == name
			}
			if !found {
				t.Errorf("tool-capable Codex request lost existing tool %s", name)
			}
		}
		if n == 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.MarshalWrite(w, map[string]any{"error": map[string]string{"message": tokens[index] + " " + accounts[index]}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r%d\",\"status\":\"completed\",\"output\":[{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Codex response %d\"}]}]}}\n\n", n, n)
	}))
	defer upstream.Close()
	t.Setenv("OPENAI_API_KEY", "sk-unused-api-key-sensitive")
	t.Setenv("OPENAI_CODEX_ACCESS_TOKEN", "unused-inline-token-sensitive")
	t.Setenv("OPENAI_CODEX_ACCOUNT_ID", "unused-inline-account-sensitive")
	client, sessions, diagnostics := startInteractiveHost(t, codexServeConfiguration(t.TempDir(), upstream.URL), "--codex-auth-file", path)
	view, err := client.Open(t.Context(), host.Create, "codex-tui")
	if err != nil {
		t.Fatal(err)
	}
	keys := make(chan tui.Key, 4)
	output := &credentialOutput{}
	tuiCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- tui.Run(tuiCtx, tui.Config{Client: client, ID: "codex-tui", Keys: keys, Output: output, Size: func() (int, int) { return 120, 40 }})
	}()
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if t.Failed() {
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			v, inspectErr := client.Inspect(ctx, "codex-tui", 0, 256)
			text := output.String()
			for _, secret := range append(append(tokens, accounts...), "unused-external-refresh-secret", "sk-unused-api-key-sensitive", "unused-inline-token-sensitive", "unused-inline-account-sensitive") {
				text = strings.ReplaceAll(text, secret, "[redacted]")
			}
			t.Logf("hosted calls=%d responses=%d running=%t inspect error=%v; terminal=%q", calls.Load(), responseCount(v), v.Running, inspectErr, text)
		}
	})
	waitInteractive(t, func() bool { return strings.Contains(output.String(), "⇄ connected  running") })
	keys <- tui.Key{Text: "Say hello."}
	keys <- tui.Key{Name: "enter"}
	waitInteractive(t, func() bool { return strings.Contains(output.String(), "Codex response 1") })
	keys <- tui.Key{Name: "detach"}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI detach did not drain")
	}
	view = inspectInteractive(t, client, "codex-tui", func(v host.View) bool { return responseCount(v) == 1 })
	if !view.Running {
		t.Fatal("TUI detach stopped the Host session")
	}
	// An atomic replacement models Codex CLI updating its externally owned file.
	replacement := path + ".next"
	wantAuth := writeCodexAuth(t, replacement, tokens[1], accounts[1])
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, client, view, "second", "Say hello again.")
	view = inspectInteractive(t, client, "codex-tui", func(v host.View) bool { return responseCount(v) == 2 })
	submitInteractive(t, client, view, "third", "Exercise a rejected credential response.")
	view = inspectInteractive(t, client, "codex-tui", func(v host.View) bool { return v.Failure != "" })
	if !strings.HasPrefix(view.Failure, "call model for turn \"") || !strings.HasSuffix(view.Failure, ": provider: unauthenticated") || calls.Load() != 3 {
		t.Fatalf("unexpected normalized failure or request count: %s, %d", view.Failure, calls.Load())
	}
	secrets := append(append(tokens, accounts...), "unused-external-refresh-secret", "sk-unused-api-key-sensitive", "unused-inline-token-sensitive", "unused-inline-account-sensitive")
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCredentialLeak(t, string(data)+output.String()+diagnostics.String(), secrets...)
	assertNoStoredCredentials(t, sessions, secrets...)
	gotAuth, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(gotAuth, wantAuth) {
		t.Fatal("Host modified externally owned credentials")
	}
}

func TestServeExternalCodexDefaultAuthFile(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	const token, account = "default-source-token-sensitive", "default-source-account-sensitive"
	writeCodexAuth(t, filepath.Join(home, ".codex", "auth.json"), token, account)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("OPENAI_CODEX_AUTH_FILE", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-ID") != account {
			t.Error("Host did not discover the default external auth file")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	client, sessions, output := startInteractiveHost(t, codexServeConfiguration(t.TempDir(), upstream.URL))
	view, err := client.Open(t.Context(), host.Create, "default-source")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, client, view, "prompt", "Use the existing Codex file.")
	inspectInteractive(t, client, "default-source", func(v host.View) bool { return responseCount(v) == 1 })
	assertNoCredentialLeak(t, output.String(), token, account)
	assertNoStoredCredentials(t, sessions, token, account, "unused-external-refresh-secret")
}

func TestServeExternalCodexRejectedFiles(t *testing.T) {
	const token, account = "rejected-token-sensitive", "rejected-account-sensitive"
	for _, kind := range []string{"missing", "insecure", "invalid", "expired", "nonregular"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			rejectedToken := token
			switch kind {
			case "insecure":
				writeCodexAuth(t, path, token, account)
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(path, []byte("{"+token+account), 0600); err != nil {
					t.Fatal(err)
				}
			case "expired":
				payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
				rejectedToken = "e30." + payload + ".signature"
				writeCodexAuth(t, path, rejectedToken, account)
			case "nonregular":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := withExternalCodexCredentials(nil, path).Resolve(t.Context(), codexReference())
			var external *credential.Error
			if !errors.As(err, &external) || external.Code != "external_reauth_required" {
				t.Fatalf("lost typed external reauth failure: %v", err)
			}
			assertNoCredentialLeak(t, err.Error(), rejectedToken, account)
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rejected credentials reached provider") }))
			defer upstream.Close()
			t.Setenv("OPENAI_API_KEY", "sk-no-fallback")
			client, sessions, output := startInteractiveHost(t, codexServeConfiguration(t.TempDir(), upstream.URL), "--codex-auth-file", path)
			view, err := client.Open(t.Context(), host.Create, "rejected")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, client, view, "prompt", "Test invalid external credentials.")
			view = inspectInteractive(t, client, "rejected", func(v host.View) bool { return v.Failure != "" })
			if !strings.HasPrefix(view.Failure, "call model for turn \"") || !strings.HasSuffix(view.Failure, ": credential: external_reauth_required") {
				t.Fatalf("external failure changed: %s", view.Failure)
			}
			data, _ := json.Marshal(view)
			assertNoCredentialLeak(t, string(data)+output.String(), rejectedToken, account, "sk-no-fallback")
			assertNoStoredCredentials(t, sessions, rejectedToken, account, "sk-no-fallback", "unused-external-refresh-secret")
		})
	}
}

func TestServeManagedAPIKeysWithExternalCodexConfigured(t *testing.T) {
	for _, id := range []string{"openai", "openrouter", "fireworks"} {
		t.Run(id, func(t *testing.T) {
			const key = "managed-api-key-sensitive"
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("ChatGPT-Account-ID") != "" {
					t.Error("managed API-key authentication changed")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer upstream.Close()
			config := codexServeConfiguration(t.TempDir(), upstream.URL)
			ref := credential.Reference{Provider: id, Method: credential.APIKey, ID: "primary"}
			config.Runtime.Provider.Provider, config.Runtime.Provider.Auth = id, ref
			directory := filepath.Join(t.TempDir(), "credentials")
			if id != "openai" {
				store, err := credential.OpenLocal(directory)
				if err != nil {
					t.Fatal(err)
				}
				if err = credential.NewManager(store, nil).Login(t.Context(), ref, credential.Material{Token: credential.NewSecret(key), Owner: credential.Managed}); err != nil {
					t.Fatal(err)
				}
			}
			client, sessions, output := startInteractiveHost(t, config, "--credential-directory", directory, "--codex-auth-file", filepath.Join(t.TempDir(), "unused-auth.json"))
			if id == "openai" {
				if _, err := client.Login(t.Context(), ref, credential.NewSecret(key)); err != nil {
					t.Fatal(err)
				}
			}
			view, err := client.Open(t.Context(), host.Create, "managed")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, client, view, "prompt", "Use the managed key.")
			view = inspectInteractive(t, client, "managed", func(v host.View) bool { return responseCount(v) == 1 })
			if calls.Load() != 1 || view.Failure != "" {
				t.Fatal("managed provider request failed")
			}
			assertNoCredentialLeak(t, output.String(), key)
			assertNoStoredCredentials(t, sessions, key)
		})
	}
}
