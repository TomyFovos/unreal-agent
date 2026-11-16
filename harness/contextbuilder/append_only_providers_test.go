package contextbuilder

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
)

func TestAppendOnlyContextKeepsExistingProviders(t *testing.T) {
	for _, provider := range []string{"openai-codex", "claude-code", "claude", "openai", "openrouter", "fireworks", "ollama"} {
		f := newContextFixture(t, provider, contextengine.Config{})
		if f.b.UsesAppendOnlyContext() != (provider == "openai-codex") {
			t.Fatal("append-only context changed", provider)
		}
	}
	if NewBuilder().(*builder).UsesAppendOnlyContext() {
		t.Fatal("append-only context without a context engine")
	}
}
