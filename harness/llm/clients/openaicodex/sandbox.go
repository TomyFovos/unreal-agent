package openaicodex

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Public protocol placeholders, never bearer credentials from an account.
// Source: docker/sbx-kits-contrib codex/spec.yaml, eeaf96617172b3544e846378e6f7dca73c9db6c8.
const SandboxAccessSentinel = "oai-oat01-proxy-managed"

// SandboxProxyError contains no environment value, URL, or credential. A
// missing/unrecognized forward proxy must not cause a direct sentinel request.
type SandboxProxyError struct{}

func (*SandboxProxyError) Error() string { return "codex Docker Sandbox forward proxy unavailable" }

// DockerSandboxProxyURL accepts only Docker's host/gateway proxy listener.
// These are public routing metadata, not credentials. HTTPS_PROXY is the
// runtime-provided route for the fixed HTTPS backend; NO_PROXY and ALL_PROXY
// cannot bypass credential injection or select an arbitrary relay. Native
// ExternalCodex never calls this function.
func DockerSandboxProxyURL(getenv func(string) string) (*url.URL, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	upper, lower := getenv("HTTPS_PROXY"), getenv("https_proxy")
	if upper != "" && lower != "" && upper != lower {
		return nil, &SandboxProxyError{}
	}
	value := upper
	if value == "" {
		value = lower
	}
	switch value {
	case "http://host.docker.internal:3128", "http://gateway.docker.internal:3128":
		return url.Parse(value)
	default:
		return nil, &SandboxProxyError{}
	}
}

// DockerSandboxSource validates only the selected kit's authentication routing.
// It never imports permissions, MCP servers, CLI instructions or credentials.
type DockerSandboxSource struct {
	AuthFile string
	Getenv   func(string) string
}

func (source DockerSandboxSource) Validate() error {
	fail := func() error { return errors.New("codex Docker Sandbox OAuth source unavailable or inconsistent") }
	getenv := source.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("SBX_CRED_OPENAI_MODE") != "oauth" || source.AuthFile == "" {
		return fail()
	}
	data, err := sandboxFile(source.AuthFile, 4096)
	if err != nil {
		return fail()
	}
	var auth struct {
		Marker string `json:"OPENAI_API_KEY"`
	}
	// Both api-key and OAuth kits use this nonsecret marker. It is insufficient
	// without the independent, exact mode and TOML routing checks below.
	if json.Unmarshal(data, &auth, json.RejectUnknownMembers(true)) != nil || auth.Marker != "proxy-managed" {
		return fail()
	}
	data, err = sandboxFile(filepath.Join(filepath.Dir(source.AuthFile), "config.toml"), 256<<10)
	if err != nil {
		return fail()
	}
	var config map[string]any
	// A real TOML decoder rejects duplicate keys, tables and ambiguous syntax.
	// Unrelated CLI settings are accepted but are never imported into Unreal.
	if toml.Unmarshal(data, &config) != nil || config["forced_login_method"] != "api" || config["model_provider"] != "sandboxd" {
		return fail()
	}
	providers, ok := config["model_providers"].(map[string]any)
	if !ok {
		return fail()
	}
	proxy, ok := providers["sandboxd"].(map[string]any)
	if !ok || proxy["base_url"] != BaseURL || proxy["experimental_bearer_token"] != SandboxAccessSentinel || proxy["requires_openai_auth"] != false {
		return fail()
	}
	return nil
}

func sandboxFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > limit {
		return nil, errors.New("invalid Sandbox source file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0022 != 0 {
		return nil, errors.New("invalid Sandbox source file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("invalid Sandbox source file")
	}
	return data, nil
}

func reservedProxyToken(token string) bool {
	return token == SandboxAccessSentinel || token == "oai-ort01-proxy-managed" || token == "proxy-managed"
}

// Restrict every exchange, including exchanges produced by a future adapter,
// before passing it to a trusted, injectable host transport. Standard network
// transports use only the validated Docker forward proxy; arbitrary ambient
// proxies remain unavailable. Permission still checks the backend on each call.
type sandboxTransport struct{ base http.RoundTripper }

func (t sandboxTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL.String() != BaseURL+"/responses" || request.Host != "" && request.Host != "chatgpt.com" {
		return nil, errors.New("codex Sandbox endpoint rejected")
	}
	if values := request.Header.Values("Authorization"); len(values) != 1 || values[0] != "Bearer "+SandboxAccessSentinel {
		return nil, errors.New("codex Sandbox authorization rejected")
	}
	for key := range request.Header {
		switch http.CanonicalHeaderKey(key) {
		case "Authorization", "Content-Type", "Accept", "Originator", "User-Agent", "Session-Id":
		default:
			return nil, errors.New("codex Sandbox header rejected")
		}
	}
	return t.base.RoundTrip(request)
}

func (t sandboxTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
