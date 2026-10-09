package credential

import "testing"

func TestDockerSandboxOAuthCannotEnterManagedStore(t *testing.T) {
	ref := Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: DockerSandboxOAuth}
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, nil) // A rejection must happen before any store access.
	if err := manager.Login(t.Context(), ref, Material{Owner: DockerSandboxProxy}); !IsCode(err, "unsupported_managed_source") {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(t.Context(), ref); !IsCode(err, "unsupported_managed_source") {
		t.Fatal(err)
	}
	for _, other := range []Reference{{Provider: "openai", ID: ref.ID, Method: ref.Method}, {Provider: ref.Provider, ID: "external-codex", Method: ref.Method}} {
		if other.Validate() == nil {
			t.Fatal("proxy reference allowed a different identity/provider")
		}
	}
}
