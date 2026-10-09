package modelcatalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexCacheFailureBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	if c := ReadCodexCache(path); c.Available || c.Problem == "" || c.ProblemCode != "codex_cache_missing" {
		t.Fatal("missing CLI cache must retain the unavailable selection boundary")
	}
	// Metadata has no credentials and is not necessarily created for custom
	// model_provider=sandboxd. The old parser made this indistinguishable from
	// a changed wire shape, so the picker could not explain the failure.
	for _, data := range []string{`{"models":null}`, `{"models":[{"id":"wire-model","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if c := ReadCodexCache(path); c.Available || len(c.Models) != 0 {
			t.Fatal("incompatible cache must never manufacture a catalog")
		}
	}
}
