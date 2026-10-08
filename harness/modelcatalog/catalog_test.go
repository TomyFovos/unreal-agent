package modelcatalog

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexCacheReadOnlyModelSpecificEfforts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	data := []byte(`{"identity":{"account_id":"never-return-account"},"models":[{"slug":"b","display_name":"B","visibility":"list","priority":2,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"high"}],"context_window":10000,"base_instructions":"never-return-instructions"},{"slug":"a","display_name":"A","visibility":"list","priority":0,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"ultra"}]},{"slug":"hidden","visibility":"hide","supported_reasoning_levels":[{"effort":"low"}]}]}`)
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
