//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestHeadlessHostRunsProductRuntimeAndChildren(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-p", "1", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, configuration := range []string{"gpt", "claude", "claude-gpt"} {
		t.Run(configuration, func(t *testing.T) {
			workspace, directory := t.TempDir(), privateCLIDirectory(t)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fake-selected-key" {
					t.Error("unselected connection")
				}
				if strings.HasSuffix(r.URL.Path, "/models") {
					w.Write([]byte(`{"data":[{"id":"gpt-test"},{"id":"gpt-6-luna"}]}`))
					return
				}
				calls.Add(1)
				var body struct {
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if json.UnmarshalRead(r.Body, &body) != nil || body.Reasoning.Effort != "max" {
					t.Error("headless effort changed")
				}
				output := `{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"task complete"}]}`
				if configuration == "claude-gpt" {
					output = `{"type":"function_call","id":"f","call_id":"finish","name":"Finish","arguments":"{\"status\":\"completed\",\"summary\":\"GPT worker completed\",\"changedFiles\":[],\"tests\":[],\"blockers\":[]}"}`
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[" + output + "],\"usage\":{\"input_tokens\":11,\"output_tokens\":7}}}\n\n"))
			}))
			defer server.Close()
			api, key := apiFixture(t, server.URL)
			api.Provider.Model.ID = "gpt-test"
			api.ReasoningEffort = "max"
			cfg := codexServeConfiguration(workspace, server.URL)
			cfg.Runtime.Provider, cfg.Runtime.ReasoningEffort = api.Provider, "max"
			maximum := withChildTools(permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted})
			cfg.Permissions = maximum
			cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
			no := false
			cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: &no, CodexDisabled: true, ClaudeDisabled: true, APIKeys: map[string]credential.Source{"openai": {File: key}}}
			cfg.SubagentDefaults = &subagentDefaults{Provider: "openai", Model: "gpt-test", Effort: "max"}
			if configuration != "gpt" {
				fixture := testclaude.New(t)
				steps := []testclaude.AgentStep{}
				if configuration == "claude-gpt" {
					input, _ := json.Marshal(map[string]string{"command": binary + " subagent start --template worker-max --task 'Complete the task'"})
					steps = append(steps, testclaude.AgentStep{Tool: "Bash", Input: input})
				}
				fixture.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentSteps: steps, AgentOnce: true, AgentFinal: "Claude task complete", AgentInputTokens: 13})
				anthropic := filepath.Join(t.TempDir(), "anthropic.key")
				os.WriteFile(anthropic, []byte("fake-anthropic-headless"), 0600)
				cfg.Launcher.APIKeys[claudeagent.APIProviderID] = credential.Source{File: anthropic}
				cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai": api}
				cfg.Runtime.Provider = api.Provider
				cfg.Runtime.Provider.Provider = claudeagent.APIProviderID
				cfg.Runtime.Provider.Endpoint = ""
				cfg.Runtime.Provider.Model.ID = "opus"
				cfg.Runtime.Provider.Auth = credential.Reference{Provider: claudeagent.APIProviderID, Method: credential.APIKey, ID: credential.ExplicitAPIKeyID}
				cfg.Runtime.Claude = &claudeagent.Config{Binary: fixture.Binary}
				if configuration == "claude" {
					cfg.DisableSubagents = true
					cfg.SubagentDefaults = nil
					delete(cfg.Launcher.APIKeys, "openai")
					cfg.Providers = nil
				}
			}
			path := filepath.Join(directory, "config.json")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(path, data, 0600)
			prompt := filepath.Join(directory, "prompt")
			os.WriteFile(prompt, []byte("Complete the benchmark task."), 0600)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "exec", "--config", path, "--session-directory", filepath.Join(directory, "sessions"), "--socket", filepath.Join(directory, "host.sock"), "--prompt-file", prompt, "--session", "benchmark")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("headless: %v %s", err, out)
			}
			sessions, err := filepath.Glob(filepath.Join(directory, "sessions", "*.session.jsonl"))
			if err != nil || len(sessions) == 0 {
				t.Fatal("missing canonical session", err)
			}
			if configuration == "claude-gpt" && (len(sessions) != 2 || calls.Load() != 1) {
				t.Fatalf("product child did not finish: sessions=%d calls=%d", len(sessions), calls.Load())
			}
			if configuration == "claude" && calls.Load() != 0 {
				t.Fatal("Claude-only accessed OpenAI")
			}
			if configuration == "gpt" && calls.Load() != 1 {
				t.Fatal("GPT task incomplete")
			}
			assertNoStoredCredentials(t, filepath.Join(directory, "sessions"), "fake-selected-key", "fake-anthropic-headless")
		})
	}
}
