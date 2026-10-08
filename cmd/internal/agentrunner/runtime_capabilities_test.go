package agentrunner

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func TestRuntimeFactoryProviderToolCapabilities(t *testing.T) {
	for _, name := range []string{"claude-code", "text-only", "openai-codex", "openai", "ollama", "openrouter", "fireworks"} {
		t.Run(name, func(t *testing.T) {
			model := provider.Model{ID: "configured-model"}
			var descriptor provider.Descriptor
			for _, d := range provider.Defaults(map[string][]provider.Model{name: {model}}) {
				if d.ID == name {
					descriptor = d
				}
			}
			if name == "text-only" {
				descriptor = provider.Descriptor{ID: name, Models: []provider.Model{model}, AuthMethods: []credential.Method{credential.None}, LocalProcess: true}
			}
			descriptor.New = func(provider.BuildConfig, credential.Material) (provider.Client, error) {
				return capabilityClient{}, nil
			}
			registry, err := provider.New(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			identity := RuntimeIdentity{Version: 1, Workspace: t.TempDir(), SystemPrompt: "Answer the user.", Profile: profile.Default(), ReasoningEffort: "medium", Provider: provider.Selection{Version: 1, Provider: name, Model: model, Auth: credential.Reference{Method: descriptor.AuthMethods[0]}, Source: "test", MaxAttempts: 1}}
			if identity.Provider.Auth.Method != credential.None {
				identity.Provider.Auth.Provider = name
				identity.Provider.Auth.ID = "test"
			}
			if name == "claude-code" {
				identity.Provider.Auth.ID = "external-claude-code"
				identity.ClaudeCode = &claudecode.Config{ManagedPolicyMode: claudecode.ManagedPolicyTrust, Models: []modelcatalog.Model{{ID: model.ID, Name: "Configured model", Efforts: []llm.ReasoningEffort{"medium"}, DefaultEffort: "medium"}}}
			}
			factory, _, err := NewRuntimeFactory(RuntimeConfig{Identity: identity, Providers: registry, SessionDirectory: filepath.Join(t.TempDir(), "sessions"), Credentials: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
				return credential.Material{}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			// An empty permission allowlist is independent of provider capability.
			policy, err := permission.New(permission.Config{ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true, Tools: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			runtime, err := factory(permission.WithPolicy(t.Context(), policy), "capabilities")
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			built, err := runtime.Builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			if name == "claude-code" || name == "text-only" {
				if len(built.Request.Tools) != 0 || len(runtime.Tools.StaticDefinitions()) != 0 {
					t.Fatal("text-only runtime advertised tool schemas")
				}
				prompt := built.Request.Input[0].Data.(llm.Message).Text
				for _, unavailable := range []string{"prefer to go wider with tool calls", "issue them as separate tool calls", "Tool calls are asynchronous"} {
					if strings.Contains(prompt, unavailable) {
						t.Fatal("text-only runtime requested unavailable tools")
					}
				}
				for _, tool := range []string{"Bash", "read", "SubagentStart", "Finish"} {
					if _, ok := runtime.Tools.Resolve(tool); ok {
						t.Fatal("text-only runtime enabled a Translator", tool)
					}
				}
				if len(runtime.Tools.Skills()) != 0 {
					t.Fatal("text-only runtime advertised skills")
				}
			} else {
				definitions := runtime.Tools.StaticDefinitions()
				want := make([]llm.Tool, len(definitions))
				for i, definition := range definitions {
					want[i] = definition.Tool
				}
				if len(want) == 0 || !reflect.DeepEqual(built.Request.Tools, want) {
					t.Fatal("tool-capable provider lost or changed its existing catalog")
				}
			}
		})
	}
}

type capabilityClient struct{}

func (capabilityClient) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, nil
}
func (capabilityClient) Close() error { return nil }
