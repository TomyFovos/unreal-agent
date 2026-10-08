package sessionstore

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"strings"
	"testing"
)

func TestNoEffortRuntimeSelectionIsLimitedToClaude(t *testing.T) {
	for _, provider := range []string{"claude-code", "openai-codex", "openai", "ollama", "openrouter", "fireworks"} {
		s := RuntimeSelection{Version: 1, Provider: provider, Model: "configured-model"}
		if (s.Validate() == nil) != (provider == "claude-code") {
			t.Fatalf("provider %s accepted an unsupported empty effort", provider)
		}
		s.Effort = "invented"
		if s.Validate() == nil {
			t.Fatal("unknown effort became a no-effort selection")
		}
		s.Effort = llm.ReasoningEffortMedium
		if err := s.Validate(); err != nil {
			t.Fatal("existing valid effort changed", err)
		}
	}
}

func TestSelectionFromCanonicalCreationIdentityWrappers(t *testing.T) {
	base := jsontext.Value(`{"Provider":{"provider":"openai-codex","model":{"id":"model-a"},"auth":{"id":"credential-sensitive"}},"ReasoningEffort":"medium","SystemPrompt":"prompt-sensitive"}`)
	parent, err := json.Marshal(struct {
		Version   int
		Runtime   jsontext.Value
		Subagents map[string]string
	}{1, base, map[string]string{"task": "task-sensitive"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := json.Marshal(struct {
		ChildID string
		Runtime jsontext.Value
		Task    string
	}{"child", base, "child-sensitive"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []jsontext.Value{base, parent, child} {
		s := SelectionFromConfiguration(raw)
		if s == nil || s.Model != "model-a" || s.Effort != "medium" || s.Revision != 0 {
			t.Fatal("lost baseline selection", s)
		}
		b, err := json.Marshal(s)
		if err != nil || strings.Contains(string(b), "sensitive") {
			t.Fatal("projection retained non-model configuration", err)
		}
	}
	for _, raw := range []string{`{}`, `{"Provider":{"model":{"id":"x"}},"ReasoningEffort":"medium"}`, `{"Provider":{"provider":"openai-codex","model":{"id":"x"}},"ReasoningEffort":"invented"}`, `{"Runtime":{"Runtime":{"Runtime":{"Runtime":` + string(base) + `}}}}`} {
		if SelectionFromConfiguration([]byte(raw)) != nil {
			t.Fatal("invalid/unbounded identity accepted")
		}
	}
}

func TestManagedPolicyMetadataIsClosedAndNonsecret(t *testing.T) {
	for _, tc := range []struct{ mode, provider, want string }{
		{"", "claude-code", "reject"}, {"reject", "claude-code", "reject"},
		{"trust", "claude-code", "trust"}, {"private-mode-sensitive", "claude-code", ""},
		{"trust", "openai-codex", ""},
	} {
		base, err := json.Marshal(map[string]any{
			"Provider":        map[string]any{"provider": tc.provider, "model": map[string]string{"id": "configured-a"}, "auth": map[string]string{"token": "token-sensitive"}},
			"ReasoningEffort": "medium", "SystemPrompt": "prompt-sensitive",
			"ClaudeCode": map[string]any{"managedPolicyMode": tc.mode, "Binary": "path-sensitive", "env": map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "endpoint-sensitive"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		parent, err := json.Marshal(map[string]any{"Runtime": jsontext.Value(base), "task": "task-sensitive"})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []jsontext.Value{base, parent} {
			if got := ManagedPolicyModeFromConfiguration(raw); got != tc.want {
				t.Fatalf("policy boundary=%q, want %q", got, tc.want)
			}
			if selection := SelectionFromConfiguration(raw); selection == nil || selection.Model != "configured-a" {
				t.Fatal("policy metadata changed model projection")
			}
		}
	}
	for _, raw := range []string{`{}`, `{"ClaudeCode":{"managedPolicyMode":"trust"}}`, `{"Runtime":{"Runtime":{"Runtime":{"Provider":{"provider":"claude-code","model":{"id":"a"}},"ClaudeCode":{"managedPolicyMode":"trust"}}}}}`} {
		if ManagedPolicyModeFromConfiguration([]byte(raw)) != "" {
			t.Fatal("unknown/unbounded identity reported trusted")
		}
	}
	for _, claude := range []string{`"private-malformed-sensitive"`, `{"managedPolicyMode":123}`} {
		raw := jsontext.Value(`{"Provider":{"provider":"claude-code","model":{"id":"configured-a"}},"ReasoningEffort":"medium","ClaudeCode":` + claude + `}`)
		if ManagedPolicyModeFromConfiguration(raw) != "" || SelectionFromConfiguration(raw) == nil {
			t.Fatal("malformed policy metadata authorized trust or broke existing model projection")
		}
	}
}
