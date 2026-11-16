// Package opencodego connects to the OpenCode Go subscription through its
// Responses endpoint (docs/opencode-go.md). Pay-as-you-go OpenCode Zen is not
// a destination.
package opencodego

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

const (
	ProviderID = "opencode-go"
	BaseURL    = "https://opencode.ai/inference/go/openai/v1"
	// ModelsURL is the public Go model list: it needs no key and uses no
	// allowance.
	ModelsURL = "https://opencode.ai/inference/go/v1/models"
	// basePath bounds every endpoint, including loopback test servers.
	basePath = "/inference/go/"
)

// OfferedModels are the models a user can select: Flash, then the 23 models
// that each passed every scenario of the live probe (docs/opencode-go.md).
// Excluded: mimo-v2.6-flash (did not call the tool), the two
// muse-spark-*-contributor models (prompts are used for training) and the Free
// models. The endpoint, headers, errors and limit classification are the same
// for every model.
func OfferedModels() []string {
	if !buildfeatures.OpenCodeGoEnabled {
		return nil
	}
	return []string{
		"deepseek-v4.1-flash",
		"deepseek-v4-pro",
		"glm-5.2",
		"glm-5.3",
		"grok-4.7",
		"glm-5.3-flash",
		"gpt-5.6-luna",
		"gpt-6-luna",
		"claude-haiku-5-5",
		"hy3",
		"hy4-preview",
		"kimi-k2.7-code",
		"kimi-k3",
		"mimo-v2.6-pro",
		"minimax-m3",
		"minimax-m3.1-flash",
		"longcat-2.0",
		"step-5-preview",
		"qwen3.8-max",
		"qwen3.8-flash",
		"qwen3.7-plus",
		"kimi-k3.1-pro",
		"gemini-4-argon",
		"hy4",
	}
}

// The Codex retry counts, which include the first attempt.
const (
	DefaultMaxAttempts       = 5
	DefaultMaxStreamAttempts = 6
)

type Config struct {
	HTTPClient        *http.Client
	APIKey            string
	BaseURL           string
	MaxAttempts       *int
	MaxStreamAttempts *int
	Trace             func(responsesapi.Exchange)
}

type Client struct {
	adapter llm.Adapter
	remote  *primitives.RemoteClient
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	if !buildfeatures.OpenCodeGoEnabled {
		return nil, buildfeatures.ErrOpenCodeGoDisabled
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("OpenCode Go API key must be set")
	}
	baseURL, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	// Never forward the key through redirects.
	httpClient := http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	if config.HTTPClient != nil {
		httpClient = *config.HTTPClient
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	remote := primitives.NewRemoteClientWithHTTPClient(&httpClient)
	attempts, streamAttempts := config.MaxAttempts, config.MaxStreamAttempts
	if attempts == nil {
		attempts = new(DefaultMaxAttempts)
		if streamAttempts == nil {
			streamAttempts = new(DefaultMaxStreamAttempts)
		}
	}
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: baseURL + "/responses",
		Headers: map[string][]string{
			"Authorization": {"Bearer " + config.APIKey},
			"Content-Type":  {"application/json"},
			"User-Agent":    {"unreal-agent"},
		},
		// A stable value per Session, derived from the cache key; never the raw key.
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{Header: "x-opencode-session"},
		MaxAttempts:       attempts,
		MaxStreamAttempts: streamAttempts,
		Trace:             config.Trace,
		ClassifyError:     classifyError,
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{adapter: adapter, remote: remote}, nil
}

func (client *Client) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	// Legacy ultra is sent as max, which DeepSeek V4.1 Flash accepts.
	request.Model.ReasoningEffort = request.Model.ReasoningEffort.Normalize()
	return client.adapter.Respond(ctx, request, options)
}

func (client *Client) Close() error { return client.remote.Close() }

// classifyError names an exhausted allowance usage_limit_reached, which the
// shared policy never retries, so a child stops and reports instead of
// retrying until the allowance renews. OpenCode Go's own exhaustion format is
// unconfirmed (docs/opencode-go.md, 未確認事項); until it is known, a 429 whose
// Retry-After exceeds the shared backoff cap counts as exhaustion. Shorter
// limits stay temporary and are retried as for Codex.
func classifyError(failure *responsesapi.APIError, headers http.Header) *responsesapi.APIError {
	if failure.StatusCode != http.StatusTooManyRequests || retryAfter(headers.Get("Retry-After"), time.Now()) <= maxBackoff {
		return failure
	}
	exhausted := *failure
	exhausted.Code = "usage_limit_reached"
	return &exhausted
}

var maxBackoff = primitives.DefaultRemoteRequest("", "", "").RetryPolicy.MaxBackoff

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 63); err == nil {
		return time.Duration(min(seconds, uint64((1<<63-1)/time.Second))) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return deadline.Sub(now)
	}
	return 0
}

// validateBaseURL accepts OpenCode Go itself and, for tests, an explicit
// loopback IP endpoint. Both stay under /inference/go/.
func validateBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" || value == BaseURL {
		return BaseURL, nil
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") && strings.HasPrefix(parsed.Path+"/", basePath) && !strings.Contains(parsed.Path, "..") {
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsLoopback() {
			return value, nil
		}
	}
	return "", errors.New("OpenCode Go base URL must be " + BaseURL + " or an explicit loopback IP endpoint under " + basePath)
}

// ModelsURLFor is the model list of the server that base names: ModelsURL for
// OpenCode Go itself, the same path on a loopback test server.
func ModelsURLFor(base string) (string, error) {
	base, err := validateBaseURL(base)
	if err != nil {
		return "", err
	}
	if base == BaseURL {
		return ModelsURL, nil
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	return parsed.Scheme + "://" + parsed.Host + basePath + "v1/models", nil
}
