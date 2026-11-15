package agentrunner

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunnerHeadersAndProxy(t *testing.T) {
	for _, raw := range []string{`{"x-opencode-session":"private-conversation","User-Agent":"custom-runner","X-Extra":"private-header"}`, "x-opencode-session=private-conversation,User-Agent=custom-runner\nX-Extra=private-header"} {
		t.Run(raw[:1], func(t *testing.T) {
			calls := 0
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-Opencode-Session") != "private-conversation" || r.Header.Get("User-Agent") != "custom-runner" || r.Header.Get("X-Extra") != "private-header" {
					t.Error("missing transport headers")
				}
				if r.URL.Host != "runner-target.invalid" {
					t.Error("request did not traverse proxy")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer proxy.Close()
			var output, diagnostics bytes.Buffer
			environment := map[string]string{llmHeadersEnvironment: raw, "SANDBOX_EGRESS_PROXY": proxy.URL, llmAPIKeyEnvironment: "private-api-key", llmBaseURLEnvironment: "http://runner-target.invalid"}
			workspace, sessions := t.TempDir(), t.TempDir()
			code := RunMain(t.Context(), []string{"-workspace", workspace, "-session-directory", sessions}, func(name string) string { return environment[name] }, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","model":"fake"}`), &output, &diagnostics, Config{Name: "runner", Version: "rev-test", Providers: DefaultProviders(), ParseRequest: parseTestRequest})
			if code != 0 || calls != 1 {
				t.Fatalf("code=%d calls=%d diagnostics=%s", code, calls, diagnostics.String())
			}
			for _, secret := range []string{"private-conversation", "private-header", "private-api-key", "custom-runner"} {
				if strings.Contains(output.String()+diagnostics.String(), secret) {
					t.Fatal("transport value leaked into output")
				}
				filepath.WalkDir(sessions, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						t.Fatal(err)
					}
					if !d.IsDir() {
						raw, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if bytes.Contains(raw, []byte(secret)) {
							t.Fatal("transport value persisted in session")
						}
					}
					return nil
				})
			}
		})
	}
}

func TestRunnerDefaultHeadersStableAndPrivate(t *testing.T) {
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("x-opencode-session"))
		if r.Header.Get("User-Agent") != "unreal-agent-runner/rev-test" {
			t.Error("missing revision User-Agent")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	for _, id := range []string{"session-a", "session-a", "session-b"} {
		client, err := runnerHTTPClient(func(string) string { return "" }, id, "rev-test")
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL, nil)
		request.Header.Set("User-Agent", "original")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if request.Header.Get("User-Agent") != "original" || request.Header.Get("x-opencode-session") != "" {
			t.Fatal("transport mutated caller's request")
		}
	}
	if ids[0] == "" || ids[0] != ids[1] || ids[0] == ids[2] || ids[0] == "session-a" {
		t.Fatal("conversation ID is not stable and private")
	}
}

func TestRunnerHeaderErrorsDoNotEchoValues(t *testing.T) {
	for _, raw := range []string{`{"X-Test":123,"private-secret":"value"}`, `{"bad name":"private-secret"}`, `{"X-Test":"private-secret\r\ninjected"}`, "private-secret"} {
		_, err := runnerHTTPClient(func(name string) string {
			if name == llmHeadersEnvironment {
				return raw
			}
			return ""
		}, "id", "rev")
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid header did not produce a private error")
		}
	}
}

func TestRunnerDotEnvProxyPrecedence(t *testing.T) {
	for _, contents := range []string{"", "UNRELATED=value\n", "SANDBOX_EGRESS_PROXY=http://file.invalid\n", "SANDBOX_EGRESS_PROXY=\n"} {
		path := filepath.Join(t.TempDir(), ".env")
		if contents != "" {
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
		}
		getenv, err := loadDotEnv(path, func(string) string { return "http://caller.invalid" })
		if err != nil {
			t.Fatal(err)
		}
		want := "http://caller.invalid"
		if strings.HasPrefix(contents, "SANDBOX_EGRESS_PROXY=") {
			want = strings.TrimSpace(strings.TrimPrefix(contents, "SANDBOX_EGRESS_PROXY="))
		}
		if getenv("SANDBOX_EGRESS_PROXY") != want {
			t.Fatal("proxy precedence changed")
		}
	}
}

func TestRunnerRetainsSupportedProxySchemes(t *testing.T) {
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		if _, err := runnerHTTPClient(func(name string) string {
			if name == "SANDBOX_EGRESS_PROXY" {
				return scheme + "://127.0.0.1:1080"
			}
			return ""
		}, "session", "rev"); err != nil {
			t.Fatalf("existing proxy scheme %s rejected: %v", scheme, err)
		}
	}
}

func TestRunnerHeaderTransportClosesIdleConnections(t *testing.T) {
	idle, closed := make(chan struct{}), make(chan struct{})
	var idleOnce, closedOnce sync.Once
	endpoint := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	endpoint.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateIdle {
			idleOnce.Do(func() { close(idle) })
		}
		if state == http.StateClosed {
			closedOnce.Do(func() { close(closed) })
		}
	}
	endpoint.Start()
	defer endpoint.Close()
	client, err := runnerHTTPClient(func(string) string { return "" }, "session", "rev")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(endpoint.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("connection did not become idle")
	}
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("header wrapper prevented idle connection cleanup")
	}
}

func TestRunnerMasksGatewayHeaderEchoInEffortError(t *testing.T) {
	for _, composition := range []string{"conservative", "development"} {
		t.Run(composition, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"unsupported effort; echo private-conversation private-agent private-api-key","param":"reasoning.effort","code":"unsupported_value"}}`)
			}))
			defer endpoint.Close()
			environment := map[string]string{llmBaseURLEnvironment: endpoint.URL, llmAPIKeyEnvironment: "private-api-key", llmHeadersEnvironment: `{"x-opencode-session":"private-conversation","User-Agent":"private-agent"}`}
			var output, diagnostics bytes.Buffer
			code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string { return environment[name] }, func() []string { return nil }, strings.NewReader(`{"prompt":"hello","model":"fake","profile":"`+composition+`","max_attempts":1}`), &output, &diagnostics, Config{Name: "runner", Providers: DefaultProviders(), ParseRequest: parseTestRequest})
			if code != 1 {
				t.Fatal("provider rejection became success")
			}
			text := output.String() + diagnostics.String()
			for _, value := range []string{"private-conversation", "private-agent", "private-api-key"} {
				if strings.Contains(text, value) {
					t.Fatal("gateway header echo exposed")
				}
			}
			if !strings.Contains(text, "unsupported effort") {
				t.Fatal("useful rejection reason was lost")
			}
		})
	}
}
