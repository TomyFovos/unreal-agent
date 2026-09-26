package provider

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
)

// ExternalCodex is read-only and rereads the selected official auth file on every
// turn. Expired/unavailable credentials require external renewal, never import.
func ExternalCodex(config openaicodex.Config) credential.Resolver {
	return credential.ResolverFunc(func(ctx context.Context, ref credential.Reference) (credential.Material, error) {
		if err := ctx.Err(); err != nil {
			return credential.Material{}, err
		}
		if ref.Provider != "openai-codex" || ref.Method != credential.OAuth {
			return credential.Material{}, &credential.Error{Code: "unsupported_external_source"}
		}
		token, account, err := config.CurrentCredentials()
		if err != nil {
			return credential.Material{}, &credential.Error{Code: "external_reauth_required"}
		}
		return credential.Material{Token: credential.NewSecret(token), AccountID: account, Owner: credential.External}, nil
	})
}
