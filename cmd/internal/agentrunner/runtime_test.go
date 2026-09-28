package agentrunner

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestRuntimeFactoryOwnsFreshSessionComponentsAndIdentity(t *testing.T) {
	catalog := map[string][]provider.Model{"ollama": {{ID: "explicit-model", Family: "custom"}}}
	registry, err := provider.New(provider.Defaults(catalog)...)
	if err != nil {
		t.Fatal(err)
	}
	identity := RuntimeIdentity{Version: 1, Workspace: t.TempDir(), SystemPrompt: "explicit system", Provider: provider.Selection{Version: 1, Provider: "ollama", Model: catalog["ollama"][0], Auth: credential.Reference{Method: credential.None}, Source: "test", MaxAttempts: 1}, Profile: profile.Default(), ReasoningEffort: llm.ReasoningEffort("low"), DisallowedTools: []string{tool.BashName}}
	c := RuntimeConfig{Identity: identity, SessionDirectory: filepath.Join(t.TempDir(), "sessions"), Providers: registry}
	factory, raw, err := NewRuntimeFactory(c)
	if err != nil {
		t.Fatal(err)
	}
	identity.DisallowedTools[0] = tool.ReadName
	var saved RuntimeIdentity
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DisallowedTools[0] != tool.BashName {
		t.Fatal("identity alias")
	}
	ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), permission.Unrestricted()))
	first, err := factory(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if first.Builder == second.Builder || first.Tools == second.Tools || first.Operations == second.Operations {
		t.Fatal("shared mutable session components")
	}
	if _, ok := first.Tools.Resolve(tool.BashName); ok {
		t.Fatal("mutated caller tool list affected runtime")
	}
	if _, ok := first.Tools.Resolve(tool.ReadName); !ok {
		t.Fatal("default native tools absent")
	}
	cancel()
	first.Close()
	second.Close()
	owner, err := host.New(t.Context(), host.Config{Directory: c.SessionDirectory, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// Configuration mismatch is checked before building a model or acquiring credentials.
	s, err := owner.Create(t.Context(), host.Options{ID: "identity", Configuration: raw})
	if err != nil {
		t.Fatal(err)
	}
	owner.Close()
	<-s.Done()
	saved.SystemPrompt = "changed"
	changed, _ := json.Marshal(saved)
	replacement, err := host.New(t.Context(), host.Config{Directory: c.SessionDirectory, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err = replacement.Resume(t.Context(), host.Options{ID: "identity", Configuration: changed}); err == nil {
		t.Fatal("prompt drift allowed on resume")
	}
}
