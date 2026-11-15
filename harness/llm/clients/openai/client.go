package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

type Config struct {
	HTTPClient  *http.Client
	APIKey      string
	BaseURL     string
	MaxAttempts *int
	Trace       func(Exchange)
}

type Exchange = responsesapi.Exchange

type Client struct {
	llm.Adapter
	remote *primitives.RemoteClient
	key    string
}

// EffortError preserves the API's rejection of an explicitly chosen effort.
// It contains no request headers, key material or arbitrary response bodies.
type EffortError struct {
	Model, Effort, Param, Code, Message string
	cause                               error
}

func (e *EffortError) Error() string {
	return fmt.Sprintf("OpenAI API rejected model=%s reasoning.effort=%s param=%s code=%s: %s", e.Model, e.Effort, e.Param, e.Code, e.Message)
}
func (e *EffortError) Unwrap() error { return e.cause }

func (client *Client) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	response, err := client.Adapter.Respond(ctx, request, options)
	var api *responsesapi.APIError
	if request.Model.ReasoningEffort != "" && errors.As(err, &api) && (api.StatusCode == 400 || api.StatusCode == 422) &&
		(api.Param == "reasoning" || api.Param == "reasoning.effort" || strings.Contains(strings.ToLower(api.Message), "effort")) &&
		api.Code != "context_length_exceeded" && api.Code != "context_window_exceeded" {
		safe := func(s string) string { return secretguard.Redact(strings.ReplaceAll(s, client.key, "[redacted]")) }
		return response, &EffortError{Model: safe(request.Model.ID), Effort: string(request.Model.ReasoningEffort), Param: safe(api.Param), Code: safe(api.Code), Message: safe(api.Message), cause: err}
	}
	return response, err
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("OpenAI API key must be set")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("OpenAI base URL must be set")
	}

	remote := primitives.NewRemoteClient()
	if config.HTTPClient != nil {
		_ = remote.Close()
		remote = primitives.NewRemoteClientWithHTTPClient(config.HTTPClient)
	}
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: baseURL + "/responses",
		Headers: map[string][]string{
			"Authorization": {"Bearer " + config.APIKey},
			"Content-Type":  {"application/json"},
		},
		Trace:             config.Trace,
		MaxAttempts:       config.MaxAttempts,
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{UsePromptCacheKeyField: true},
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{Adapter: adapter, remote: remote, key: config.APIKey}, nil
}

func (client *Client) Close() error {
	return client.remote.Close()
}
