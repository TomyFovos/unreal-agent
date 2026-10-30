package openaicodex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

const BaseURL = "https://chatgpt.com/backend-api/codex"

// Codex CLI defaults: request_max_retries=4, stream_max_retries=5.
// These constants include the first attempt.
// https://github.com/openai/codex/blob/main/codex-rs/model-provider-info/src/lib.rs
const (
	DefaultMaxAttempts       = 5
	DefaultMaxStreamAttempts = 6
)

type Config struct {
	HTTPClient  *http.Client
	AccessToken string
	AccountID   string
	// Read once at construction; exclusive with inline credentials.
	AuthFile          string
	BaseURL           string
	MaxAttempts       *int
	MaxStreamAttempts *int
	Trace             func(responsesapi.Exchange)
	// Requires the separately validated host-owned Sandbox OAuth route. Never
	// combine with token, account ID or AuthFile, and never infer from a token.
	DockerSandboxOAuth bool
	// RequestEffort turns a selected effort into the one sent, from the
	// model catalog; nil sends max for ultra (see modelcatalog.RequestEffort).
	RequestEffort func(ctx context.Context, model string, selected llm.ReasoningEffort) llm.ReasoningEffort
}

type Client struct {
	adapter       llm.Adapter
	remote        *primitives.RemoteClient
	requestEffort func(context.Context, string, llm.ReasoningEffort) llm.ReasoningEffort
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	baseURL, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if config.MaxAttempts != nil && *config.MaxAttempts <= 0 {
		return nil, errors.New("max attempts must be positive")
	}
	var credentials credentials
	if config.DockerSandboxOAuth {
		if config.BaseURL != "" && config.BaseURL != BaseURL || config.AccessToken != "" || config.AccountID != "" || config.AuthFile != "" {
			return nil, errors.New("invalid codex Docker Sandbox OAuth configuration")
		}
	} else {
		credentials, err = config.credentials()
		if err != nil {
			return nil, err
		}
	}
	// Never forward subscription credentials through redirects.
	httpClient := http.Client{
		Transport:     http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if config.HTTPClient != nil {
		httpClient = *config.HTTPClient
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	headers := map[string][]string{
		"Authorization":      {"Bearer " + credentials.accessToken},
		"ChatGPT-Account-ID": {credentials.accountID},
		"Content-Type":       {"application/json"}, "originator": {"unreal-agent"}, "User-Agent": {"unreal-agent"},
	}
	if config.DockerSandboxOAuth {
		delete(headers, "ChatGPT-Account-ID")
		headers["Authorization"] = []string{"Bearer " + SandboxAccessSentinel}
		base := httpClient.Transport
		if config.HTTPClient == nil || base == nil {
			base = http.DefaultTransport.(*http.Transport)
		}
		if transport, ok := base.(*http.Transport); ok {
			proxy, err := DockerSandboxProxyURL(nil)
			if err != nil {
				return nil, err
			}
			clone := transport.Clone()
			clone.Proxy = http.ProxyURL(proxy)
			// No proxy credentials or caller-supplied CONNECT headers. Only the
			// fixed HTTPS authority crosses this public, runtime-owned relay.
			clone.ProxyConnectHeader = nil
			clone.GetProxyConnectHeader = nil
			base = clone
		}
		httpClient.Transport = sandboxTransport{base: base}
		httpClient.Jar = nil
	}
	remote := primitives.NewRemoteClientWithHTTPClient(&httpClient)
	attempts, streamAttempts := config.MaxAttempts, config.MaxStreamAttempts
	if attempts == nil {
		attempts = new(DefaultMaxAttempts)
		if streamAttempts == nil {
			streamAttempts = new(DefaultMaxStreamAttempts)
		}
	}
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint:          baseURL + "/responses",
		Headers:           headers,
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{UsePromptCacheKeyField: true, Header: "session-id"},
		MaxAttempts:       attempts,
		MaxStreamAttempts: streamAttempts,
		Trace:             config.Trace,
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{adapter: adapter, remote: remote, requestEffort: config.RequestEffort}, nil
}

func (client *Client) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if request.Model.MaxOutputTokens != nil {
		return llm.Response{}, errors.New("codex does not support max_output_tokens")
	}
	// Legacy ultra and its max alias use the highest effort this model supports.
	if selected := request.Model.ReasoningEffort; selected.Normalize() == llm.ReasoningEffortMax {
		request.Model.ReasoningEffort = selected.Normalize()
		if client.requestEffort != nil {
			request.Model.ReasoningEffort = client.requestEffort(ctx, request.Model.ID, selected).Normalize()
		}
	}
	response, err := client.adapter.Respond(ctx, request, options)
	var apiErr *responsesapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		return llm.Response{}, fmt.Errorf("codex credentials rejected; renew them externally and recreate the client: %w", err)
	}
	return response, err
}

func (client *Client) Close() error { return client.remote.Close() }

func validateBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" || value == BaseURL {
		return BaseURL, nil
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") {
		ip := net.ParseIP(parsed.Hostname())
		if ip != nil && ip.IsLoopback() {
			return value, nil
		}
	}
	return "", errors.New("codex base URL must be https://chatgpt.com/backend-api/codex or an explicit loopback IP endpoint")
}
