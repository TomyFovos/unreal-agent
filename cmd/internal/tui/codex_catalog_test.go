package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestCodexCatalogPickerFailureReasonAndObservedEffort(t *testing.T) {
	current := &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "current", Name: "Current", Effort: "medium", Revision: 7}
	for _, code := range []string{"codex_cache_missing", "codex_cache_invalid", "codex_cli_missing", "codex_cli_protocol_invalid", "codex_cli_rejected", "codex_cli_timeout"} {
		p := modelPicker(modelcatalog.Catalog{Problem: "private-provider-text", ProblemCode: code}, current)
		if len(p.Options) != 1 || p.Catalog.Models[0].ID != "current" || p.effort(current).selectedEffort() != "medium" || !strings.Contains(p.Hint, modelcatalog.CodexProblemHint(code)) || strings.Contains(p.Hint, "private-provider-text") || current.Revision != 7 {
			t.Fatal("unavailable picker leaked or changed canonical selection")
		}
	}
	p := modelPicker(modelcatalog.Catalog{Available: true, Source: "Codex CLI model/list", Models: []modelcatalog.Model{{ID: "next", Name: "Next", Efforts: []llm.ReasoningEffort{"high", "xhigh"}, DefaultEffort: "high"}, {ID: "current", Name: "Current", Efforts: []llm.ReasoningEffort{"medium"}, DefaultEffort: "medium"}}}, current)
	p.Selection = 0
	e := p.effort(current)
	if !reflect.DeepEqual(e.Options, []string{"high", "xhigh"}) || !strings.Contains(p.Hint, "access unverified") || current.Model != "current" {
		t.Fatal("model-specific efforts were expanded or runtime changed")
	}
	p = modelPicker(modelcatalog.Catalog{ProblemCode: "secret-provider-text"}, current)
	if strings.Contains(p.Hint, "secret-provider-text") {
		t.Fatal("unknown problem category leaked")
	}
}
