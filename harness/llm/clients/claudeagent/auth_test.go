package claudeagent

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
)

func TestExplicitAPIKeyEnvironmentAndErrors(t *testing.T) {
	key := credential.NewSecret("chosen-api-test-key")
	ambient := []string{"HOME=/fake", "ANTHROPIC_API_KEY=ambient", "ANTHROPIC_AUTH_TOKEN=ambient-auth", "CLAUDE_CODE_USE_BEDROCK=1"}
	for _, api := range []bool{false, true} {
		selected := credential.Secret{}
		if api {
			selected = key
		}
		env := authenticatedEnvironment(ambient, selected)
		if slices.Contains(env, "ANTHROPIC_API_KEY="+key.Reveal()) != api {
			t.Fatal("API key environment did not follow explicit selection")
		}
		for _, entry := range env {
			if strings.Contains(entry, "ambient") || strings.Contains(entry, "BEDROCK") {
				t.Fatal("ambient billing source retained")
			}
		}
	}
	for _, scenario := range []string{"api_echo", "api_error", "api_stderr"} {
		h := newHarness(t, scenario)
		client, err := NewAPIClient(h.client.config, credential.Material{Token: key})
		if err != nil {
			t.Fatal("API client failed")
		}
		h.client = client
		response, err := h.respond(t.Context())
		encoded, _ := json.Marshal(response)
		text := string(encoded) + h.progress.String() + fmt.Sprintf("%v %#v %v %#v", client.config, client.config, client, client)
		if err != nil {
			text += err.Error()
		}
		if strings.Contains(text, key.Reveal()) {
			t.Fatal("Claude API key exposed")
		}
		if scenario == "api_echo" && err != nil || scenario != "api_echo" && err == nil {
			t.Fatal("unexpected fake API outcome")
		}
	}
	if _, err := NewAPIClient(newHarness(t, "echo").client.config, credential.Material{}); err == nil {
		t.Fatal("API client allowed missing key and subscription fallback")
	}
}

func TestExplicitClaudeAPIKeyCatalog(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("chosen-api-test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environ := func() []string { return []string{"CLAUDEAGENT_FAKE=api_catalog"} }
	c, account, err := DiscoverAPI(t.Context(), binary, environ, credential.Source{File: key})
	if err != nil || !c.Available || account.Subscription() {
		t.Fatal("API catalog incorrectly required subscription")
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DiscoverAPI(t.Context(), binary, environ, credential.Source{File: key}); !credential.IsCode(err, "api_key_file_requires_0600") {
		t.Fatal("API discovery accepted insecure key source")
	}
	_, _, err = DiscoverAPI(t.Context(), binary, func() []string { return []string{"CLAUDEAGENT_FAKE=api_catalog", "CHOSEN_KEY=chosen-api-test-key"} }, credential.Source{Env: "CHOSEN_KEY"})
	if err != nil {
		t.Fatal("explicit catalog environment source failed")
	}
}

func TestExplicitClaudeAPIKeySourceReadBeforeDeferredProcess(t *testing.T) {
	h := newHarness(t, "api_echo")
	path := filepath.Join(t.TempDir(), "api.key")
	if err := os.WriteFile(path, []byte("chosen-api-test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewAPIClient(h.client.config, credential.Material{Token: credential.NewSecret("old-fake-key"), Source: credential.Source{File: path}})
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	if _, err := h.respond(t.Context()); err != nil {
		t.Fatal("deferred process did not read current source")
	}
	before, err := os.Stat(h.args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := h.respond(t.Context()); !credential.IsCode(err, "api_key_file_unreadable") {
		t.Fatal("deferred process used cached key after source removal")
	}
	after, err := os.Stat(h.args)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("missing key launched a process")
	}
}
