package agentrunner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestLanguageToolsLifetimeAndHistoryCodec(t *testing.T) {
	ctx := permission.WithPolicy(t.Context(), permission.Unrestricted())
	for _, disabled := range []bool{false, true} {
		owner, err := NewLanguageTools(ctx, t.TempDir(), []lsp.ServerConfig{{Language: "go", Path: "/unavailable/gopls"}})
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		if owner.Identity() == "" || strings.Contains(owner.Identity(), "gopls") {
			t.Fatal("missing configuration fingerprint")
		}
		var closed atomic.Int32
		base := func(context.Context, ToolConfig) (Tools, error) {
			return Tools{Registry: tool.NewRegistry(tool.StaticTranslators{}), Close: func() error { closed.Add(1); return nil }}, nil
		}
		var disallowed []string
		if disabled {
			disallowed = []string{"LSP"}
		}
		first, err := owner.Wrap(base, disallowed)(t.Context(), ToolConfig{SessionID: "first"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := owner.Wrap(base, disallowed)(t.Context(), ToolConfig{SessionID: "second"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := first.Registry.Resolve("LSP"); ok == disabled {
			t.Fatalf("disabled=%v resolves=%v", disabled, ok)
		}
		if _, ok := tool.ResolveHistory(first.Registry, "LSP"); !ok {
			t.Fatal("disabled history codec unavailable")
		}
		if err = first.Close(); err != nil {
			t.Fatal(err)
		}
		// The sibling's resource owner remains available after one session closes.
		result := owner.manager.Execute(ctx, "second/missing", lsp.Request{Action: "workspace_symbols", Language: "missing"})
		if result.Code == "expired" {
			t.Fatal("session closed shared owner")
		}
		if err = second.Close(); err != nil {
			t.Fatal(err)
		}
		if closed.Load() != 2 {
			t.Fatal(closed.Load())
		}
		if err = owner.Close(); err != nil {
			t.Fatal(err)
		}
		if result = owner.manager.Execute(ctx, "late", lsp.Request{Action: "workspace_symbols", Language: "missing"}); result.Code != "expired" && result.Code != "canceled" {
			t.Fatal(result)
		}
	}
}
func TestLanguageServerConfiguration(t *testing.T) {
	servers, err := LanguageServerConfiguration(func(string) string {
		return `[{"language":"go","path":"/opt/gopls","arguments":["serve"],"extensions":[".go"]}]`
	})
	if err != nil || len(servers) != 1 || servers[0].Language != "go" {
		t.Fatal(servers, err)
	}
	for _, raw := range []string{`[{"unknown":"secret"}]`, strings.Repeat("x", 65537)} {
		if _, err := LanguageServerConfiguration(func(string) string { return raw }); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	owner, err := NewLanguageTools(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.Identity() != "" {
		t.Fatal("empty config changes legacy identity")
	}
}
