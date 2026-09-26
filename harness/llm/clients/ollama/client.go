package ollama

import (
	"net/http"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

const BaseURL = "http://localhost:11434/v1"

type Config struct {
	HTTPClient  *http.Client
	BaseURL     string
	MaxAttempts *int
}

type Client struct {
	llm.Adapter
	remote *primitives.RemoteClient
}

func NewClient(config Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = BaseURL
	}
	remote := primitives.NewRemoteClient()
	if config.HTTPClient != nil {
		_ = remote.Close()
		remote = primitives.NewRemoteClientWithHTTPClient(config.HTTPClient)
	}
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint:    baseURL + "/responses",
		Headers:     map[string][]string{"Content-Type": {"application/json"}},
		MaxAttempts: config.MaxAttempts,
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{Adapter: adapter, remote: remote}, nil
}

func (client *Client) Close() error {
	return client.remote.Close()
}
