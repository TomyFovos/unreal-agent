// Package provider composes existing LLM adapters without owning an agent loop.
package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

type Client interface {
	llm.Adapter
	Close() error
}
type Model struct {
	ID           string   `json:"id"`
	Family       string   `json:"family"`
	Capabilities []string `json:"capabilities"`
}

// Selection is an immutable-by-copy non-secret resume contract. Catalog must
// explicitly declare models at composition; no model-name guessing/fallback.
type Selection struct {
	Version     int                  `json:"version"`
	Provider    string               `json:"provider"`
	Model       Model                `json:"model"`
	Endpoint    string               `json:"endpoint"`
	Auth        credential.Reference `json:"auth"`
	MaxAttempts int                  `json:"max_attempts"`
	Source      string               `json:"source"`
}
type BuildConfig struct {
	Selection  Selection
	Resolver   credential.Resolver
	HTTPClient *http.Client
	ClaudeCode claudecode.Config
}
type Factory func(BuildConfig, credential.Material) (Client, error)
type Descriptor struct {
	ID           string
	Endpoint     string
	AuthMethods  []credential.Method
	Capabilities []string
	Models       []Model
	New          Factory
	// LocalProcess has no HTTP endpoint. ExternalAuth delegates authentication
	// to the installed executable without obtaining credential material.
	LocalProcess bool
	ExternalAuth bool
	// ToolCapability is an explicit runtime opt-in, independent of model names.
	// Static capabilities remain the backwards-compatible default.
	ToolCapability func(BuildConfig) bool
}
type Registry struct{ entries map[string]Descriptor }

// WithModel extends a registered adapter's read-only catalog while retaining
// its factory, authentication contract and capabilities. It registers no new
// provider; the caller must authorize discovery before constructing a request.
func (r *Registry) WithModel(s Selection) (*Registry, error) {
	d, ok := r.entries[s.Provider]
	if !ok {
		return nil, &Error{Code: "provider_unregistered"}
	}
	copy := &Registry{entries: make(map[string]Descriptor, len(r.entries))}
	for id, v := range r.entries {
		copy.entries[id] = v
	}
	d.Models = slices.Clone(d.Models)
	for _, m := range d.Models {
		if m.ID == s.Model.ID {
			return r, nil
		}
	}
	d.Models = append(d.Models, s.Model)
	copy.entries[s.Provider] = d
	return copy, nil
}

func New(descriptors ...Descriptor) (*Registry, error) {
	r := &Registry{entries: map[string]Descriptor{}}
	for _, d := range descriptors {
		if strings.TrimSpace(d.ID) == "" || d.New == nil {
			return nil, &Error{Code: "invalid_provider"}
		}
		if _, ok := r.entries[d.ID]; ok {
			return nil, &Error{Code: "duplicate_provider"}
		}
		seenModels := map[string]bool{}
		for _, m := range d.Models {
			if strings.TrimSpace(m.ID) == "" || seenModels[m.ID] {
				return nil, &Error{Code: "invalid_model_catalog"}
			}
			seenModels[m.ID] = true
			for _, c := range m.Capabilities {
				if !slices.Contains(d.Capabilities, c) {
					return nil, &Error{Code: "invalid_model_capability"}
				}
			}
		}
		d.AuthMethods = slices.Clone(d.AuthMethods)
		d.Capabilities = slices.Clone(d.Capabilities)
		d.Models = cloneModels(d.Models)
		r.entries[d.ID] = d
	}
	return r, nil
}
func cloneModels(ms []Model) []Model {
	out := slices.Clone(ms)
	for i := range out {
		out[i].Capabilities = slices.Clone(out[i].Capabilities)
	}
	return out
}
func (r *Registry) Resolve(s Selection) (Selection, error) {
	d, ok := r.entries[s.Provider]
	if !ok {
		return Selection{}, &Error{Code: "unsupported_provider"}
	}
	if s.Version != 1 {
		return Selection{}, &Error{Code: "unsupported_configuration_version"}
	}
	if s.MaxAttempts < 1 || s.MaxAttempts > 10 {
		return Selection{}, &Error{Code: "invalid_retry_limit"}
	}
	if strings.TrimSpace(s.Source) == "" {
		return Selection{}, &Error{Code: "selection_source_required"}
	}
	modelIndex := slices.IndexFunc(d.Models, func(m Model) bool { return m.ID == s.Model.ID })
	if modelIndex < 0 {
		return Selection{}, &Error{Code: "unsupported_model"}
	}
	m := d.Models[modelIndex]
	for _, c := range s.Model.Capabilities {
		if !slices.Contains(m.Capabilities, c) || !slices.Contains(d.Capabilities, c) {
			return Selection{}, &Error{Code: "unsupported_capability"}
		}
	}
	if s.Model.Family != "" && s.Model.Family != m.Family {
		return Selection{}, &Error{Code: "incompatible_model_family"}
	}
	s.Model = m
	s.Model.Capabilities = slices.Clone(m.Capabilities)
	if !slices.Contains(d.AuthMethods, s.Auth.Method) {
		return Selection{}, &Error{Code: "unsupported_auth_method"}
	}
	if s.Auth.Method != credential.None {
		if err := s.Auth.Validate(); err != nil {
			return Selection{}, err
		}
		if s.Auth.Provider != s.Provider {
			return Selection{}, &Error{Code: "credential_provider_mismatch"}
		}
	} else if s.Auth.ID != "" || s.Auth.Provider != "" {
		return Selection{}, &Error{Code: "unexpected_credential"}
	}
	if s.Endpoint == "" {
		s.Endpoint = d.Endpoint
	}
	if d.LocalProcess {
		if s.Endpoint != "" {
			return Selection{}, &Error{Code: "invalid_endpoint"}
		}
		if d.ExternalAuth && s.Auth.ID != "external-claude-code" {
			return Selection{}, &Error{Code: "unsupported_auth_reference"}
		}
		return s, nil
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Selection{}, &Error{Code: "invalid_endpoint"}
	}
	s.Endpoint = strings.TrimRight(s.Endpoint, "/")
	return s, nil
}
func ValidateResume(recorded, requested Selection) error {
	if !reflect.DeepEqual(recorded, requested) {
		return &Error{Code: "incompatible_resume_configuration"}
	}
	return nil
}
func (r *Registry) RequiresResolver(s Selection) bool {
	return s.Auth.Method != credential.None && !r.entries[s.Provider].ExternalAuth
}

// SupportsTools reports the adapter's tool capability for runtime composition.
// Model metadata can omit optional capabilities; that does not disable tools on
// an existing tool-capable adapter. Resolve validates model declarations first.
func (r *Registry) SupportsTools(s Selection) bool {
	d, ok := r.entries[s.Provider]
	return ok && slices.Contains(d.Capabilities, "tools")
}

func (r *Registry) SupportsRuntimeTools(c BuildConfig) bool {
	d, ok := r.entries[c.Selection.Provider]
	return ok && (r.SupportsTools(c.Selection) || d.ToolCapability != nil && d.ToolCapability(c))
}

func (r *Registry) Build(c BuildConfig) (Client, Selection, error) {
	s, err := r.Resolve(c.Selection)
	if err != nil {
		return nil, Selection{}, err
	}
	if r.RequiresResolver(s) && c.Resolver == nil {
		return nil, Selection{}, &Error{Code: "credential_resolver_required"}
	}
	c.Selection = s
	c.Selection.Model.Capabilities = slices.Clone(s.Model.Capabilities)
	// Credentials never follow redirects, even within the same origin. Embedders
	// may inject a policy transport; copy the client rather than mutating it.
	hc := http.Client{}
	if c.HTTPClient != nil {
		hc = *c.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.HTTPClient = &hc
	a := &adapter{config: c, factory: r.entries[s.Provider].New, externalAuth: r.entries[s.Provider].ExternalAuth}
	if r.entries[s.Provider].LocalProcess {
		a.processes = &processRequests{cancels: make(map[*context.CancelFunc]context.CancelFunc)}
	}
	return a, s, nil
}

type adapter struct {
	config       BuildConfig
	factory      Factory
	externalAuth bool
	processes    *processRequests
}

func (a *adapter) Close() error {
	if a.processes != nil {
		a.processes.close()
	}
	return nil
}
func (a *adapter) Respond(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	if a.processes != nil {
		requestCtx, release, err := a.processes.begin(ctx)
		if err != nil {
			return llm.Response{}, err
		}
		defer release()
		ctx = requestCtx
	}
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if req.Model.ID != a.config.Selection.Model.ID {
		return llm.Response{}, &Error{Code: "model_selection_mismatch"}
	}
	var material credential.Material
	if a.config.Selection.Auth.Method != credential.None && !a.externalAuth {
		var err error
		material, err = a.config.Resolver.Resolve(ctx, a.config.Selection.Auth)
		if err != nil {
			return llm.Response{}, normalize(err)
		}
	}
	client, err := a.factory(a.config, material)
	if err != nil {
		return llm.Response{}, normalize(err)
	}
	defer client.Close()
	response, err := client.Respond(ctx, req, opt)
	if err != nil {
		return llm.Response{}, normalize(err)
	}
	// Provider failure messages may echo credential/header/request data. Keep codes
	// from a closed vocabulary at the public adapter boundary.
	if response.Failure != nil {
		response.Failure = &llm.Failure{Code: "provider_failure", Message: "provider reported a failure"}
	}
	return response, nil
}

type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string { return "provider: " + e.Code }
func normalize(err error) error {
	if denied := permission.Failure(err); denied != nil {
		return denied
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var ce *credential.Error
	var cli *claudecode.Error
	if errors.As(err, &cli) {
		return cli
	}
	if errors.As(err, &ce) {
		return ce
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	var api *responsesapi.APIError
	if errors.As(err, &api) {
		code := "remote_failure"
		switch api.StatusCode {
		case 400, 404, 422:
			code = "invalid_request"
		case 401:
			code = "unauthenticated"
		case 403:
			code = "forbidden"
		case 429:
			code = "rate_limited"
		}
		return &Error{Code: code, Status: api.StatusCode}
	}
	return &Error{Code: "request_failed"}
}
