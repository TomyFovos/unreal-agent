package agentrunner

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestSelectionClientInjectsRouteCompactionSetting(t *testing.T) {
	selection := provider.Selection{Version: 1, Provider: claudeagent.ProviderID, Model: provider.Model{ID: "model"}, Auth: credential.Reference{Provider: claudeagent.ProviderID, Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 1, Source: "test"}
	for _, initial := range []bool{true, false} {
		var injected int64
		descriptors := provider.Defaults(map[string][]provider.Model{claudeagent.ProviderID: {selection.Model}})
		for i := range descriptors {
			if descriptors[i].ID == claudeagent.ProviderID {
				descriptors[i].New = func(build provider.BuildConfig, _ credential.Material) (provider.Client, error) {
					injected = build.ClaudeAgent.CompactThresholdTokens
					return unavailableRuntimeClient{}, nil
				}
			}
		}
		registry, err := provider.New(descriptors...)
		if err != nil {
			t.Fatal(err)
		}
		backend := ProviderRuntime{Provider: selection, ReasoningEffort: "medium", Claude: &claudeagent.Config{Binary: "/test/claude"}}
		config := RuntimeConfig{Providers: registry, Claude: ClaudeRouteConfig{CompactThresholdTokens: 123456}}
		if initial {
			config.Identity = RuntimeIdentity{Provider: backend.Provider, ReasoningEffort: backend.ReasoningEffort, Claude: backend.Claude}
		} else {
			config.Backends = map[string]ProviderRuntime{claudeagent.ProviderID: backend}
		}
		binding := backend.Binding()
		client, err := selectionClient(config, sessionstore.RuntimeSelection{Version: 1, Provider: selection.Provider, Model: "model", Effort: "medium", Binding: binding}, t.TempDir(), observed.NewResults())
		if err != nil {
			t.Fatal(err)
		}
		// The registry creates the fake route client lazily on Respond.
		_, _ = client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "model"}}, llm.RequestOptions{})
		client.Close()
		if injected != 123456 || backend.Binding() != binding {
			t.Fatal("route setting was not injected or changed immutable binding", injected)
		}
	}
}
