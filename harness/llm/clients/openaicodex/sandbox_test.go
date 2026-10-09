package openaicodex

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const sandboxTOML = `forced_login_method = "api"
model_provider = "sandboxd"
approval_policy = "never"
sandbox_mode = "danger-full-access"
[model_providers.sandboxd]
name = "Sandbox Proxy"
base_url = "https://chatgpt.com/backend-api/codex"
experimental_bearer_token = "oai-oat01-proxy-managed"
requires_openai_auth = false
[mcp_servers.foreign]
url = "http://not-used.invalid"
`

func TestDockerSandboxSource(t *testing.T) {
	for _, test := range []struct {
		name, mode, auth, config string
		ok                       bool
	}{
		{"kit", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, sandboxTOML, true},
		{"api-mode", "apikey", `{"OPENAI_API_KEY":"proxy-managed"}`, sandboxTOML, false},
		{"absent-mode", "", `{"OPENAI_API_KEY":"proxy-managed"}`, sandboxTOML, false},
		{"native-auth", "oauth", `{"tokens":{"access_token":"dummy","account_id":"dummy"}}`, sandboxTOML, false},
		{"api-key", "oauth", `{"OPENAI_API_KEY":"sk-dummy"}`, sandboxTOML, false},
		{"mixed-auth", "oauth", `{"OPENAI_API_KEY":"proxy-managed","tokens":{}}`, sandboxTOML, false},
		{"duplicate-json", "oauth", `{"OPENAI_API_KEY":"proxy-managed","OPENAI_API_KEY":"proxy-managed"}`, sandboxTOML, false},
		{"duplicate-toml", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, "model_provider='evil'\n" + sandboxTOML, false},
		{"wrong-provider", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, strings.Replace(sandboxTOML, `model_provider = "sandboxd"`, `model_provider = "other"`, 1), false},
		{"real-token", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, strings.Replace(sandboxTOML, SandboxAccessSentinel, "private-dummy", 1), false},
		{"account-required", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, strings.Replace(sandboxTOML, "requires_openai_auth = false", "requires_openai_auth = true", 1), false},
		{"extra-header", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, strings.Replace(sandboxTOML, "requires_openai_auth = false", "requires_openai_auth = false\nhttp_headers={Authorization='private-dummy'}", 1), false},
		{"ambiguous-type", "oauth", `{"OPENAI_API_KEY":"proxy-managed"}`, strings.Replace(sandboxTOML, "requires_openai_auth = false", "requires_openai_auth = 'false'", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "auth.json")
			for name, content := range map[string]string{"auth.json": test.auth, "config.toml": test.config} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			source := DockerSandboxSource{AuthFile: path, Getenv: func(string) string { return test.mode }}
			err := source.Validate()
			if (err == nil) != test.ok {
				t.Fatalf("source accepted=%v, expected=%v", err == nil, test.ok)
			}
			if err != nil && strings.Contains(err.Error(), "private-dummy") {
				t.Fatal("raw details leaked")
			}
			for name, content := range map[string]string{"auth.json": test.auth, "config.toml": test.config} {
				data, _ := os.ReadFile(filepath.Join(directory, name))
				if string(data) != content {
					t.Fatal("source modified")
				}
			}
		})
	}
}

func TestDockerSandboxSourceFileAndEndpointBoundary(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	config := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, []byte(`{"OPENAI_API_KEY":"proxy-managed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	source := DockerSandboxSource{AuthFile: path, Getenv: func(string) string { return "oauth" }}
	for _, endpoint := range []string{"http://chatgpt.com/backend-api/codex", BaseURL + "/", BaseURL + "?x=y", BaseURL + "#x", "https://chatgpt.com:443/backend-api/codex", "https://chatgpt.com./backend-api/codex", "https://user@chatgpt.com/backend-api/codex", "https://CHATGPT.COM/backend-api/codex", "http://127.0.0.1:1234", "https://api.openai.com/v1"} {
		os.WriteFile(config, []byte(strings.Replace(sandboxTOML, BaseURL, endpoint, 1)), 0600)
		if source.Validate() == nil {
			t.Errorf("accepted alternate endpoint %q", endpoint)
		}
	}
	os.WriteFile(config, []byte(sandboxTOML), 0600)
	os.Chmod(path, 0666)
	if source.Validate() == nil {
		t.Fatal("accepted writable auth")
	}
	os.Chmod(path, 0600)
	redirect := filepath.Join(directory, "redirect.json")
	if err := os.Symlink(path, redirect); err != nil {
		t.Fatal(err)
	}
	if (DockerSandboxSource{AuthFile: redirect, Getenv: source.Getenv}).Validate() == nil {
		t.Fatal("accepted auth symlink")
	}
	os.WriteFile(config, []byte(strings.Repeat("#bounded\n", 40000)), 0600)
	if source.Validate() == nil {
		t.Fatal("accepted oversized config")
	}
}

type sandboxTestTransport func(*http.Request) (*http.Response, error)

func (f sandboxTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDockerSandboxClientProtocol(t *testing.T) {
	count := 0
	transport := sandboxTestTransport(func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.String() != BaseURL+"/responses" || r.Header.Get("Authorization") != "Bearer "+SandboxAccessSentinel || len(r.Header.Values("ChatGPT-Account-ID")) != 0 {
			t.Fatal("incorrect proxy wire auth")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"dummy\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	})
	client, err := NewClient(Config{DockerSandboxOAuth: true, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{}); err != nil || count != 1 {
		t.Fatalf("wire exchange failed: %v count=%d", err, count)
	}
	for _, config := range []Config{{DockerSandboxOAuth: true, AccountID: "invented"}, {DockerSandboxOAuth: true, AccessToken: SandboxAccessSentinel}, {DockerSandboxOAuth: true, AuthFile: "dummy"}, {DockerSandboxOAuth: true, BaseURL: "http://127.0.0.1:1234"}, {AccessToken: SandboxAccessSentinel, AccountID: "invented"}, {AccessToken: "proxy-managed", AccountID: "invented"}} {
		if _, err := NewClient(config); err == nil {
			t.Fatal("mixed authentication accepted")
		}
	}
	guard := sandboxTransport{base: transport}
	for _, target := range []string{BaseURL + "/responses?x=y", BaseURL + "/responses/", BaseURL + "/models", "http://chatgpt.com/backend-api/codex/responses"} {
		u, _ := url.Parse(target)
		_, err := guard.RoundTrip(&http.Request{Method: "POST", URL: u, Header: http.Header{"Authorization": {"Bearer " + SandboxAccessSentinel}}})
		if err == nil {
			t.Fatal("guard allowed a different URL")
		}
	}
	u, _ := url.Parse(BaseURL + "/responses")
	for _, key := range []string{"ChatGPT-Account-ID", "Cookie", "Proxy-Authorization", "X-Api-Key", "X-Unknown"} {
		if _, err := guard.RoundTrip(&http.Request{Method: "POST", URL: u, Header: http.Header{"Authorization": {"Bearer " + SandboxAccessSentinel}, key: {"dummy"}}}); err == nil {
			t.Fatal("guard allowed an extra header")
		}
	}
	if count != 1 {
		t.Fatal("guard failure reached the proxy")
	}
}
