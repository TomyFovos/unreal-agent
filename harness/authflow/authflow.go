// Package authflow exposes UI-neutral, provider-supported authentication.
// Secrets enter this interface directly, never through Inbox/Session/Operations.
package authflow

import (
	"context"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/credential"
)

type Support struct {
	Provider  string            `json:"provider"`
	Method    credential.Method `json:"method"`
	Status    string            `json:"status"`
	Owner     credential.Owner  `json:"owner"`
	Inference string            `json:"inference"`
	Reason    string            `json:"reason"`
	Reviewed  string            `json:"reviewed"`
	Source    string            `json:"source"`
}

func Matrix() []Support {
	return []Support{
		{Provider: "openai", Method: credential.APIKey, Status: "supported", Owner: credential.Managed, Inference: "existing Responses adapter", Reviewed: "2026-09-27", Source: "https://developers.openai.com/api/docs/quickstart"},
		{Provider: "anthropic", Method: credential.APIKey, Status: "configuration_only", Owner: credential.Managed, Inference: "requires an explicitly installed Anthropic adapter", Reviewed: "2026-09-27", Source: "https://platform.claude.com/docs/en/api/overview", Reason: "API-key storage is supported; this fork currently ships Responses adapters"},
		{Provider: "anthropic", Method: credential.OAuth, Status: "unsupported", Owner: credential.External, Reviewed: "2026-09-27", Source: "https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use", Reason: "Claude.ai subscription credential mediation is not permitted"},
		{Provider: "openai-codex", Method: credential.OAuth, Status: "deferred", Owner: credential.External, Inference: "existing external read-only token source", Reviewed: "2026-09-27", Source: "https://learn.chatgpt.com/docs/auth", Reason: "no third-party direct OAuth client/exchange/refresh contract established; use explicitly owned external source"},
		{Provider: "github-copilot", Method: credential.OAuth, Status: "deferred", Owner: credential.External, Reviewed: "2026-09-27", Source: "https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/authenticate", Reason: "OAuth application authentication documented; inference adapter without competing runtime ownership not established"},
	}
}

type Service struct{ manager *credential.Manager }

func New(manager *credential.Manager) *Service { return &Service{manager: manager} }

type LoginRequest struct {
	Reference credential.Reference
	Secret    credential.Secret
}

// Login installs a key supplied through a private UI channel. It performs no
// speculative model request or browser flow. Inference authenticates the key;
// local installation alone makes no entitlement claim.
func (s *Service) Login(ctx context.Context, request LoginRequest) (credential.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return credential.Metadata{}, err
	}
	if err := ValidateMethod(request.Reference); err != nil {
		return credential.Metadata{}, err
	}
	material := credential.Material{Token: credential.NewSecret(strings.TrimSpace(request.Secret.Reveal())), Owner: credential.Managed}
	if err := s.manager.Login(ctx, request.Reference, material); err != nil {
		return credential.Metadata{}, err
	}
	entries, err := s.manager.List(ctx)
	if err != nil {
		return credential.Metadata{}, err
	}
	for _, entry := range entries {
		if entry.Reference == request.Reference {
			return entry, nil
		}
	}
	return credential.Metadata{}, &credential.Error{Code: "credential_changed"}
}
func (s *Service) Logout(ctx context.Context, reference credential.Reference) error {
	return s.manager.Logout(ctx, reference)
}
func (s *Service) List(ctx context.Context) ([]credential.Metadata, error) {
	return s.manager.List(ctx)
}

// ValidateMethod rejects unavailable flows before a UI requests a secret.
func ValidateMethod(ref credential.Reference) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	allowed := false
	for _, support := range Matrix() {
		if support.Provider == ref.Provider && support.Method == ref.Method && (support.Status == "supported" || support.Status == "configuration_only") {
			allowed = true
			break
		}
	}
	if !allowed {
		return &credential.Error{Code: "unsupported_auth_flow"}
	}
	return nil
}
