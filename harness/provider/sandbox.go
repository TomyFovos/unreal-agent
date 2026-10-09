package provider

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
)

// DockerSandboxCodex is an explicit host-owned OAuth route, not another token
// source. Every request rereads the kit's routing/marker, without importing,
// modifying or refreshing its auth file. Material carries only an owner marker.
func DockerSandboxCodex(source openaicodex.DockerSandboxSource) credential.Resolver {
	return credential.ResolverFunc(func(ctx context.Context, ref credential.Reference) (credential.Material, error) {
		if err := ctx.Err(); err != nil {
			return credential.Material{}, err
		}
		if ref != (credential.Reference{Provider: "openai-codex", Method: credential.DockerSandboxOAuth, ID: "docker-sandbox-oauth"}) {
			return credential.Material{}, &credential.Error{Code: "unsupported_external_source"}
		}
		if source.Validate() != nil {
			return credential.Material{}, &credential.Error{Code: "docker_sandbox_oauth_unavailable"}
		}
		return credential.Material{Owner: credential.DockerSandboxProxy}, nil
	})
}
