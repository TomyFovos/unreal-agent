package tool

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"testing"
)

func TestExtensionsSeparateExecutionHistoryAndCopy(t *testing.T) {
	base := NewRegistry(StaticTranslators{})
	translator := &fixedTranslator{}
	definition := Definition{Tool: llm.Tool{Name: "Custom", Parameters: map[string]any{"description": "old"}}}
	r, err := WithExtensions(base, []Extension{{Definition: definition, Translator: translator, Enabled: true}, {Definition: Definition{Tool: llm.Tool{Name: "Hidden"}}, Translator: translator}})
	if err != nil {
		t.Fatal(err)
	}
	definition.Tool.Parameters["description"] = "mutated"
	if defs := r.StaticDefinitions(); len(defs) != 1 || defs[0].Tool.Parameters["description"] != "old" {
		t.Fatal(defs)
	}
	defs := r.StaticDefinitions()
	defs[0].Tool.Parameters["description"] = "second mutation"
	if r.StaticDefinitions()[0].Tool.Parameters["description"] != "old" {
		t.Fatal("mutable registry")
	}
	if _, ok := r.Resolve("Hidden"); ok {
		t.Fatal("disabled tool executable")
	}
	if got, ok := ResolveHistory(r, "Hidden"); !ok || got != translator {
		t.Fatal("history codec missing")
	}
	if _, err = WithExtensions(r, []Extension{{Definition: definition, Translator: translator}}); err == nil {
		t.Fatal("duplicate accepted")
	}
	for _, name := range []string{"Bash", "SkillUse"} {
		if _, err = WithExtensions(base, []Extension{{Definition: Definition{Tool: llm.Tool{Name: name}}, Translator: translator}}); err == nil {
			t.Fatal("base collision accepted", name)
		}
	}
}
