package agentrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const llmHeadersEnvironment = "UNREAL_HARNESS_LLM_HEADERS"

// Header material is transport-only: never put it in RuntimeIdentity or errors.
func runnerHTTPClient(getenv func(string) string, sessionID, version string) (*http.Client, error) {
	headers := http.Header{}
	set := func(name, value string) error {
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if name == "" {
			return errors.New("invalid LLM header name")
		}
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return errors.New("invalid LLM header name")
			}
		}
		for _, c := range value {
			if c < 32 && c != '\t' || c == 127 {
				return errors.New("invalid LLM header value")
			}
		}
		headers.Set(name, value)
		return nil
	}
	raw := strings.TrimSpace(getenv(llmHeadersEnvironment))
	if strings.HasPrefix(raw, "{") {
		var values map[string]string
		if json.Unmarshal([]byte(raw), &values) != nil {
			return nil, errors.New("invalid UNREAL_HARNESS_LLM_HEADERS JSON")
		}
		for name, value := range values {
			if err := set(name, value); err != nil {
				return nil, err
			}
		}
	} else if raw != "" {
		for _, entry := range strings.FieldsFunc(raw, func(c rune) bool { return c == ',' || c == '\n' }) {
			name, value, ok := strings.Cut(entry, "=")
			if !ok {
				name, value, ok = strings.Cut(entry, ":")
			}
			if !ok {
				return nil, errors.New("UNREAL_HARNESS_LLM_HEADERS entries must be Name=Value")
			}
			if err := set(name, value); err != nil {
				return nil, err
			}
		}
	}
	if _, ok := headers[http.CanonicalHeaderKey("x-opencode-session")]; !ok {
		// Stable across turns/resume, without exposing the transport ID in logs
		// that contain canonical parent/child Session IDs.
		id := sha256.Sum256([]byte("unreal-agent-runner/llm-session/" + sessionID))
		headers.Set("x-opencode-session", hex.EncodeToString(id[:]))
	}
	if _, ok := headers["User-Agent"]; !ok {
		if version == "" {
			version = "dev"
		}
		headers.Set("User-Agent", "unreal-agent-runner/"+version)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy := strings.TrimSpace(getenv("SANDBOX_EGRESS_PROXY")); proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" {
			return nil, errors.New("invalid sandbox proxy")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: headerTransport{base: transport, headers: headers}}, nil
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	for name, values := range t.headers {
		cloned.Header[name] = append([]string(nil), values...)
	}
	return t.base.RoundTrip(cloned)
}

func (t headerTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
