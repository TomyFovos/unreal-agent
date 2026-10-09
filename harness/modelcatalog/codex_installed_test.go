package modelcatalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Explicit non-inference contract check with the installed CLI. A synthetic
// config points at a fixture server; no real credential/provider is involved.
func TestCodexInstalledCLIModelListOnly(t *testing.T) {
	binary := os.Getenv("UNREAL_CODEX_CATALOG_BINARY")
	if binary == "" {
		t.Skip("set UNREAL_CODEX_CATALOG_BINARY for installed metadata-only CLI check")
	}
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "fixture: no inference or external network permitted", http.StatusForbidden)
	}))
	defer server.Close()
	config := fmt.Sprintf("forced_login_method=\"api\"\nmodel_provider=\"sandboxd\"\n[model_providers.sandboxd]\nname=\"Sandbox Proxy\"\nbase_url=%q\nexperimental_bearer_token=\"public-fixture-sentinel\"\nrequires_openai_auth=false\n", server.URL)
	marker := filepath.Join(home, "mcp-must-not-start")
	config += fmt.Sprintf("[mcp_servers.forbidden_fixture]\ncommand=\"/usr/bin/touch\"\nargs=[%q]\n", marker)
	files := map[string]string{"auth.json": `{"OPENAI_API_KEY":"proxy-managed"}`, "config.toml": config}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(codexHome, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cli := &CodexCLI{Home: codexHome, Binary: binary, Getenv: func(key string) string {
		if key == "HOME" {
			return home
		}
		if key == "PATH" {
			return os.Getenv("PATH")
		}
		return ""
	}, Validate: func(context.Context) error { return nil }}
	c := cli.Catalog(t.Context(), true)
	if !c.Available || len(c.Models) < 2 {
		t.Fatal("installed model/list unavailable", c.ProblemCode)
	}
	if calls.Load() != 0 {
		t.Fatal("metadata-only CLI contacted provider", calls.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("metadata reader started an MCP command")
	}
	for name, value := range files {
		b, err := os.ReadFile(filepath.Join(codexHome, name))
		if err != nil || string(b) != value {
			t.Fatal("CLI changed authentication/config", name)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(codexHome, "sessions", "*")); len(entries) > 0 {
		t.Fatal("CLI created a thread")
	}
	t.Logf("installed CLI: %d models; provider requests=0; threads=0; no real authentication", len(c.Models))
}
