package modelcatalog

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCodexCacheReadOnlyModelSpecificEfforts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	data := []byte(`{"identity":{"account_id":"never-return-account"},"models":[{"slug":"b","display_name":"B","visibility":"list","priority":2,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"high"}],"context_window":10000,"base_instructions":"never-return-instructions"},{"slug":"a","display_name":"A","visibility":"list","priority":0,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"hyper"}]},{"slug":"hidden","visibility":"hide","supported_reasoning_levels":[{"effort":"low"}]}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := ReadCodexCache(path)
	if !c.Available || len(c.Models) != 2 || c.Models[0].ID != "a" || len(c.Models[0].Efforts) != 2 || c.Models[1].Efforts[0] != llm.ReasoningEffortHigh || c.Models[1].ContextWindow != 10000 {
		t.Fatal(c)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("cache was changed", err)
	}
	if c := ReadCodexCache(path + ".missing"); c.Available || len(c.Models) != 0 || c.Problem != "catalog unavailable" {
		t.Fatal(c)
	}
	if err := os.WriteFile(path, []byte(`{"models":[{"slug":"bad","visibility":"list"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if ReadCodexCache(path).Available {
		t.Fatal("invented efforts for incomplete metadata")
	}
	if ReadCodexCache(filepath.Dir(path)).Available {
		t.Fatal("directory cache accepted")
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if ReadCodexCache(link).Available {
		t.Fatal("nonregular cache source accepted")
	}
}

// Codex can advertise ultra, but Unreal only offers low through max.
func TestCodexCacheOmitsUltraEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	data := []byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := ReadCodexCache(path)
	if !c.Available || len(c.Models) != 1 || c.Models[0].UnsupportedEfforts || !slices.Equal(c.Models[0].Efforts, []llm.ReasoningEffort{"low", "medium", "high", "xhigh", "max"}) || c.Models[0].AllowsEffort(llm.ReasoningEffortUltra) || !c.Models[0].AllowsEffort(llm.ReasoningEffortMax) {
		t.Fatal(c)
	}
	if _, err := Configured(c.Models); err != nil {
		t.Fatal("filtered model is not a valid configured catalog", err)
	}
}

// Legacy ultra uses max, ignoring Codex's multi-agent delegation metadata.
func TestRequestEffortResolvesUltraToHighestSupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	data := []byte(`{"models":[{"slug":"gpt-6.1-sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}],"multi_agent_reasoning_effort":"xhigh"}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := ReadCodexCache(path)
	if !c.Available || c.Models[0].MultiAgentEffort != llm.ReasoningEffortXHigh {
		t.Fatal("multi-agent effort not read", c)
	}
	efforts := func(names ...llm.ReasoningEffort) []llm.ReasoningEffort { return names }
	for _, tc := range []struct {
		model    Model
		selected llm.ReasoningEffort
		want     llm.ReasoningEffort
	}{
		{c.Models[0], llm.ReasoningEffortUltra, llm.ReasoningEffortMax},
		{c.Models[0], llm.ReasoningEffortMax, llm.ReasoningEffortMax},
		{c.Models[0], llm.ReasoningEffortHigh, llm.ReasoningEffortHigh},
		{Model{Efforts: efforts("low", "high", "max", "ultra")}, llm.ReasoningEffortUltra, llm.ReasoningEffortMax},
		{Model{Efforts: efforts("low", "high", "ultra")}, llm.ReasoningEffortUltra, llm.ReasoningEffortHigh},
		{Model{Efforts: efforts("xhigh", "low", "ultra", "high")}, llm.ReasoningEffortUltra, llm.ReasoningEffortXHigh},
		{Model{Efforts: efforts("high", "low")}, llm.ReasoningEffortMax, llm.ReasoningEffortHigh},
		{Model{Efforts: efforts("medium", "low")}, llm.ReasoningEffortUltra, llm.ReasoningEffortMedium},
		{Model{Efforts: efforts("low")}, llm.ReasoningEffortUltra, llm.ReasoningEffortLow},
		{Model{}, llm.ReasoningEffortUltra, llm.ReasoningEffortMax},
	} {
		if got := tc.model.RequestEffort(tc.selected); got != tc.want {
			t.Errorf("%v %s: got %s, want %s", tc.model.Efforts, tc.selected, got, tc.want)
		}
	}
}
