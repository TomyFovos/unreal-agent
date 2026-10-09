package authcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
)

func TestDockerSandboxCheckNoStoreOrInference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	for name, content := range map[string]string{"auth.json": `{"OPENAI_API_KEY":"proxy-managed"}`, "config.toml": `forced_login_method="api"
model_provider="sandboxd"
[model_providers.sandboxd]
base_url="https://chatgpt.com/backend-api/codex"
experimental_bearer_token="oai-oat01-proxy-managed"
requires_openai_auth=false
`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
	var output bytes.Buffer
	args := []string{"check-docker-sandbox-oauth", "--codex-auth-file", path}
	if err := Run(t.Context(), args, strings.NewReader(""), &output); err != nil || !strings.Contains(output.String(), "ready") || strings.Contains(output.String(), "oai-oat") {
		t.Fatal("preflight failed or printed sentinel", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatal("preflight created files")
	}
	t.Setenv("SBX_CRED_OPENAI_MODE", "apikey")
	output.Reset()
	if err := Run(t.Context(), args, strings.NewReader(""), &output); !credential.IsCode(err, "docker_sandbox_oauth_unavailable") || output.Len() != 0 {
		t.Fatal("wrong-mode preflight did not fail closed", err)
	}
	if err := Run(t.Context(), []string{"check-docker-sandbox-oauth"}, strings.NewReader(""), &output); !credential.IsCode(err, "invalid_arguments") {
		t.Fatal("missing source accepted")
	}
}
