//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Exercise the real headless attachment, Host, child processes, and both model
// transports. Completion reports wake idle parents immediately, including leads.
func TestHeadlessUnreportedChildCompletion(t *testing.T) {
	binary := testCLI(t, false)
	for _, route := range []string{"gpt", "claude"} {
		for _, nested := range []bool{false, true} {
			for _, stop := range []llm.StopReason{llm.StopMaxOutputTokens, llm.StopComplete} {
				t.Run(fmt.Sprintf("%s/grandchild=%v/%s", route, nested, stop), func(t *testing.T) {
					t.Parallel()
					workspace, directory := t.TempDir(), privateCLIDirectory(t)
					template := "collector"
					if nested {
						template = "lead"
					}
					var mu sync.Mutex
					calls := map[string]int{}
					function := func(name string, args any) map[string]any {
						data, _ := json.Marshal(args)
						return map[string]any{"type": "function_call", "id": "f-" + name, "call_id": "call-" + name, "name": name, "arguments": string(data)}
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/models") {
							fmt.Fprint(w, `{"data":[{"id":"gpt-main"},{"id":"gpt-child"},{"id":"gpt-6-luna"}]}`)
							return
						}
						var body map[string]any
						if err := json.UnmarshalRead(r.Body, &body); err != nil {
							t.Error(err)
							return
						}
						model, _ := body["model"].(string)
						mu.Lock()
						calls[model]++
						n := calls[model]
						mu.Unlock()
						output := map[string]any{"type": "message", "id": "m", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "partial work without Finish"}}}
						status := "completed"
						switch model {
						case "gpt-main":
							if n == 1 {
								output = function("SubagentStart", map[string]string{"template": template, "task": "collect evidence"})
							} else {
								data, _ := json.Marshal(body)
								if n != 2 || !strings.Contains(string(data), "stop="+string(stop)) {
									t.Error("root missed failure reason or repeated its final turn", n)
								}
							}
						case "gpt-child":
							if n == 1 {
								output = function("SubagentStart", map[string]string{"template": "collector", "task": "collect evidence"})
							} else {
								data, _ := json.Marshal(body)
								if n != 2 || !strings.Contains(string(data), "stop="+string(stop)) {
									t.Error("lead missed collector failure or retried it", n)
								}
								output = function("Finish", map[string]any{"status": "failed", "summary": "collector failed: stop=" + string(stop), "changedFiles": []string{}, "tests": []string{}, "blockers": []string{"collector ended without Finish"}})
							}
						case "gpt-6-luna":
							if n != 1 {
								t.Error("unreported child was retried", n)
							}
							if stop == llm.StopMaxOutputTokens {
								status = "incomplete"
								output = map[string]any{"type": "reasoning", "id": "reasoning-only", "summary": []any{}}
							}
						default:
							t.Error("unexpected model", model)
						}
						usage := map[string]any{"input_tokens": 11, "output_tokens": 7}
						if model == "gpt-6-luna" && stop == llm.StopMaxOutputTokens {
							usage["output_tokens"], usage["output_tokens_details"] = 128000, map[string]any{"reasoning_tokens": 119}
						}
						response := map[string]any{"id": "r", "status": status, "output": []any{output}, "usage": usage}
						if status == "incomplete" {
							response["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
						}
						event := "response.completed"
						if status == "incomplete" {
							event = "response.incomplete"
						}
						data, _ := json.Marshal(map[string]any{"type": event, "response": response})
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\n", data)
					}))
					defer server.Close()
					api, key := apiFixture(t, server.URL)
					api.Provider.Model.ID, api.ReasoningEffort = "gpt-main", "max"
					cfg := codexServeConfiguration(workspace, server.URL)
					cfg.Runtime.Provider, cfg.Runtime.ReasoningEffort = api.Provider, "max"
					maximum := withChildTools(permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted})
					cfg.Permissions, cfg.PermissionModes = maximum, &permission.ModesConfig{Enabled: true, Maximum: &maximum}
					no := false
					cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: &no, CodexDisabled: true, ClaudeDisabled: true, APIKeys: map[string]credential.Source{"openai": {File: key}}}
					cfg.SubagentDefaults = &subagentDefaults{Provider: "openai", Model: "gpt-child", Effort: "max"}
					var fixture *testclaude.Fixture
					if route == "claude" {
						fixture = testclaude.New(t)
						input, _ := json.Marshal(map[string]string{"command": binary + " subagent start --template " + template + " --task 'collect evidence'"})
						fixture.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentSteps: []testclaude.AgentStep{{Tool: "Bash", Input: input}}, AgentOnce: true, AgentFinal: "Claude final review"})
						anthropic := filepath.Join(directory, "anthropic.key")
						if err := os.WriteFile(anthropic, []byte("fake-anthropic-headless"), 0600); err != nil {
							t.Fatal(err)
						}
						cfg.Launcher.APIKeys[claudeagent.APIProviderID] = credential.Source{File: anthropic}
						cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai": api}
						cfg.Runtime.Provider.Provider, cfg.Runtime.Provider.Endpoint, cfg.Runtime.Provider.Model.ID = claudeagent.APIProviderID, "", "opus"
						cfg.Runtime.Provider.Auth = credential.Reference{Provider: claudeagent.APIProviderID, Method: credential.APIKey, ID: credential.ExplicitAPIKeyID}
						cfg.Runtime.Claude = &claudeagent.Config{Binary: fixture.Binary}
					}
					data, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					config, prompt, sessions := filepath.Join(directory, "config.json"), filepath.Join(directory, "prompt"), filepath.Join(directory, "sessions")
					if err = os.WriteFile(config, data, 0600); err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(prompt, []byte("Complete the benchmark task."), 0600); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, binary, "exec", "--config", config, "--session-directory", sessions, "--socket", filepath.Join(directory, "host.sock"), "--prompt-file", prompt, "--session", "benchmark")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("headless did not finish: %v\n%s", err, out)
					}
					mu.Lock()
					rootCalls, leadCalls, collectorCalls := calls["gpt-main"], calls["gpt-child"], calls["gpt-6-luna"]
					mu.Unlock()
					if route == "gpt" && rootCalls != 2 || collectorCalls != 1 || nested && leadCalls != 2 {
						t.Fatalf("missing final review or repeated inference: root=%d lead=%d collector=%d", rootCalls, leadCalls, collectorCalls)
					}
					store, err := localfile.New(sessions)
					if err != nil {
						t.Fatal(err)
					}
					infos, err := store.ListSessions(t.Context())
					want := 2
					if nested {
						want = 3
					}
					if err != nil || len(infos) != want {
						t.Fatal("session tree", len(infos), err)
					}
					for _, info := range infos {
						_, items, ops, err := store.ReadSnapshot(t.Context(), info.ID)
						if err != nil {
							t.Fatal(err)
						}
						for _, op := range ops {
							if p, err := subagent.DecodePlan(op); err == nil && p.Action == "start" && op.Status != operation.StatusFailed {
								t.Fatal("child not terminal failed", op.Status)
							}
						}
						if info.ID == session.ID("benchmark") {
							ended := 0
							for _, item := range items {
								if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputPeer {
									peer, _ := input.DecodePeerMessage()
									if peer.Kind == "ended" {
										ended++
									}
								}
							}
							if route == "claude" && ended != 1 {
								t.Fatal("missing or duplicate completion notice", ended)
							}
							continue
						}
						view, err := subagent.ReadChild(t.Context(), sessions, info.ID, 0, 256)
						if err != nil || view.Finish == nil || view.Finish.Result.Status != "failed" || !strings.Contains(view.Finish.Result.Summary, "stop="+string(stop)) {
							t.Fatalf("child result=%+v error=%v", view.Finish, err)
						}
						if len(ops) == 0 && (view.Finish.ModelTurnID == "" || len(modelResponses(items)) != 1) {
							t.Fatal("leaf lacks model-owned failed completion or was retried")
						}
					}
					if fixture != nil {
						turns := fixture.Calls(t)
						if len(turns) != 2 || !strings.Contains(turns[1].Input, "stop="+string(stop)) {
							t.Fatalf("Claude did not review child's failure: %+v", turns)
						}
					}
					assertNoStoredCredentials(t, sessions, "fake-selected-key", "fake-anthropic-headless")
				})
			}
		}
	}
}

func modelResponses(items []sessionstore.Item) []sessionstore.ModelResponse {
	var responses []sessionstore.ModelResponse
	for _, item := range items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok {
			responses = append(responses, r)
		}
	}
	return responses
}
