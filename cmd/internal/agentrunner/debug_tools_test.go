package agentrunner

import (
	"context"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestDebugToolsGenerationAndDisabledHistory(t *testing.T) {
	base := func(context.Context, ToolConfig) (Tools, error) {
		return Tools{Registry: tool.NewRegistry(tool.StaticTranslators{})}, nil
	}
	for _, disabled := range []bool{false, true} {
		var disallowed []string
		if disabled {
			disallowed = []string{"DAP"}
		}
		adapters := []dap.AdapterConfig{{ID: "test", Path: "/missing/adapter", Directory: "/workspace"}}
		factory, identity, err := DebugTools(base, adapters, disallowed)
		if err != nil || len(identity) != 64 {
			t.Fatal(identity, err)
		}
		adapters[0].Path = "changed"
		first, err := factory(t.Context(), ToolConfig{SessionID: "first"})
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		second, err := factory(t.Context(), ToolConfig{SessionID: "second"})
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		a := first.RemoteJobs[0].(*dap.Manager)
		b := second.RemoteJobs[0].(*dap.Manager)
		if a.Generation() == b.Generation() {
			t.Fatal("debugger generation reused")
		}
		if _, ok := first.Registry.Resolve("DAP"); ok == disabled {
			t.Fatal("incorrect tool availability")
		}
		if _, ok := tool.ResolveHistory(first.Registry, "DAP"); !ok {
			t.Fatal("history codec missing")
		}
		old := dap.Request{Version: 1, OwnerGeneration: a.Generation(), Handle: dap.Handle{ID: "debug", Generation: a.Generation()}, Command: "inspect"}
		result, err := b.Execute(t.Context(), old)
		if err == nil || result.Error != "expired" {
			t.Fatal("old runtime accepted", result, err)
		}
	}
}
func TestDebugAdapterConfiguration(t *testing.T) {
	configs, err := DebugAdapterConfiguration(func(string) string {
		return `[{"id":"test","path":"/adapter","directory":"/workspace","allowed_attach_pids":[123]}]`
	})
	if err != nil || len(configs) != 1 || len(configs[0].AllowedAttachPIDs) != 1 {
		t.Fatal(configs, err)
	}
	for _, raw := range []string{`[{"unknown":"secret"}]`, strings.Repeat("x", 65537)} {
		if _, err := DebugAdapterConfiguration(func(string) string { return raw }); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	base := func(context.Context, ToolConfig) (Tools, error) {
		return Tools{Registry: tool.NewRegistry(tool.StaticTranslators{})}, nil
	}
	if _, _, err := DebugTools(base, []dap.AdapterConfig{{ID: "test", Path: "relative", Directory: "/workspace"}}, nil); err == nil {
		t.Fatal("invalid adapter accepted")
	}
	factory, identity, err := DebugTools(base, nil, nil)
	if err != nil || identity != "" {
		t.Fatal(identity, err)
	}
	configured, err := factory(t.Context(), ToolConfig{SessionID: "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	defer configured.Close()
	if _, ok := configured.Registry.Resolve("DAP"); ok {
		t.Fatal("unconfigured debugger advertised")
	}
	if _, ok := tool.ResolveHistory(configured.Registry, "DAP"); !ok {
		t.Fatal("unconfigured history codec missing")
	}
}
