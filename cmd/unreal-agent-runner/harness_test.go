package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
)

// The test executable serves the production child entrypoint, so these tests
// exercise real parent/lead/worker processes against a local fake provider.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		os.Exit(run(os.Stdin))
	}
	os.Exit(m.Run())
}

type harnessEvent struct {
	Sequence  uint64
	Kind      string
	Data      jsontext.Value
	Synthetic bool
	ChildID   string
	Type      string `json:"type"`
	IsError   bool   `json:"is_error"`
}

func harnessEvents(t *testing.T, raw []byte) []harnessEvent {
	t.Helper()
	var events []harnessEvent
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var event harnessEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestHarnessRunnerNestedChildren(t *testing.T) {
	for _, thinking := range []string{"high", "none"} {
		t.Run(thinking, func(t *testing.T) {
			t.Parallel()
			workspace, sessions, logs := t.TempDir(), t.TempDir(), t.TempDir()
			const source = "package p\nfunc old_name() {}\n"
			if err := os.WriteFile(filepath.Join(workspace, "sample.go"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			calls := map[string]int{}
			headerIDs := map[string]string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var request struct {
					Model     string         `json:"model"`
					Reasoning jsontext.Value `json:"reasoning"`
					Tools     []struct {
						Name string `json:"name"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Error(err)
					return
				}
				if request.Model != "fake-model" {
					t.Error("child changed the measurement model")
				}
				if thinking == "none" && len(request.Reasoning) != 0 {
					t.Error("reasoning was sent despite thinking_level:none")
				}
				role := "parent"
				switch {
				case bytes.Contains(raw, []byte("You are the implementation lead;")):
					role = "lead"
				case bytes.Contains(raw, []byte("You are a worker.")):
					role = "worker"
				case bytes.Contains(raw, []byte("You are a collector for low-difficulty questions:")):
					role = "collector"
				}
				mu.Lock()
				calls[role]++
				n := calls[role]
				header := r.Header.Get("x-opencode-session")
				if prior := headerIDs[role]; prior != "" && prior != header {
					t.Error("conversation header changed between turns")
				}
				headerIDs[role] = header
				mu.Unlock()
				if header == "" || r.Header.Get("User-Agent") == "" || r.Header.Get("X-Harness") != "private-value" {
					t.Error("child lost transport headers")
				}
				if n == 1 {
					var names []string
					for _, tool := range request.Tools {
						names = append(names, tool.Name)
					}
					for _, name := range []string{"read", "write", "edit", "grep", "glob", "ast_grep", "ast_edit", "Bash", "ViewImage"} {
						if !slices.Contains(names, name) {
							t.Errorf("%s missing %s", role, name)
						}
					}
					if (role == "parent" || role == "lead") && !slices.Contains(names, "SubagentStart") {
						t.Error("parent cannot start children")
					}
				}
				function := func(name string, args any) map[string]any {
					encoded, _ := json.Marshal(args)
					id := fmt.Sprintf("%s-%d-%s", role, n, name)
					if name == "SubagentStart" {
						id += "-" + args.(map[string]string)["template"]
					}
					return map[string]any{"type": "function_call", "id": id, "call_id": id, "name": name, "arguments": string(encoded), "status": "completed"}
				}
				finish := func(summary string) map[string]any {
					return function("Finish", map[string]any{"status": "completed", "summary": summary, "changedFiles": []string{}, "tests": []string{}, "blockers": []string{}})
				}
				var output []map[string]any
				switch fmt.Sprintf("%s/%d", role, n) {
				case "parent/1":
					output = []map[string]any{function("SubagentStart", map[string]string{"template": "lead", "task": "implement the fixture"})}
				case "parent/2":
					output = []map[string]any{function("edit", map[string]any{"path": filepath.Join(workspace, "shared.txt"), "expected": mutation.RevisionOf([]byte("worker\n")), "old_text": "worker", "new_text": "parent"})}
				case "parent/3":
					output = []map[string]any{{"type": "message", "id": "final", "role": "assistant", "status": "completed", "content": []map[string]string{{"type": "output_text", "text": "PARENT FINAL"}}}}
				case "lead/1":
					output = []map[string]any{function("SubagentStart", map[string]string{"template": "worker-high", "task": "write shared fixture"}), function("SubagentStart", map[string]string{"template": "collector", "task": "collect fixture status"})}
				case "lead/2":
					output = []map[string]any{function("Bash", map[string]string{"command": "printf 'local fake test'"}), function("ast_grep", map[string]any{"language": "go", "query": "(identifier) @match", "targets": []map[string]string{{"path": "sample.go"}}})}
				case "lead/3":
					output = []map[string]any{function("ast_edit", map[string]any{"language": "go", "query": "((identifier) @match (#eq? @match \"old_name\"))", "targets": []map[string]any{{"path": "sample.go", "expected": mutation.RevisionOf([]byte(source))}}, "replacement": "new_name", "expected_matches": 1, "apply": true})}
				case "lead/4":
					output = []map[string]any{finish("LEAD FINAL")}
				case "worker/1":
					output = []map[string]any{function("write", map[string]any{"path": "./shared.txt", "expected": mutation.Revision{}, "content": "worker\n"})}
				case "worker/2":
					output = []map[string]any{finish("WORKER FINAL")}
				case "collector/1":
					output = []map[string]any{finish("COLLECTOR FINAL")}
				default:
					t.Errorf("unexpected generation %s/%d", role, n)
					w.WriteHeader(500)
					return
				}
				frame, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("%s-%d", role, n), "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "input_tokens_details": map[string]int{"cached_tokens": 2}, "output_tokens_details": map[string]int{"reasoning_tokens": 1}}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", frame)
			}))
			defer server.Close()
			environment := map[string]string{"PATH": os.Getenv("PATH"), "HOME": t.TempDir(), "XDG_CACHE_HOME": t.TempDir(), "UNREAL_HARNESS_LLM_PROVIDER": "openai", "UNREAL_HARNESS_LLM_BASE_URL": server.URL, "UNREAL_HARNESS_LLM_API_KEY": "private-api-key", "UNREAL_HARNESS_LLM_HEADERS": `{"X-Harness":"private-value"}`}
			var output, diagnostics bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()
			request := fmt.Sprintf(`{"prompt":"do the fixture","profile":"development","model":"fake-model","max_attempts":1,"thinking_level":%q}`, thinking)
			code := agentrunner.RunMain(ctx, []string{"-workspace", workspace, "-session-directory", sessions, "-log-directory", logs}, func(name string) string { return environment[name] }, func() []string {
				var entries []string
				for k, v := range environment {
					entries = append(entries, k+"="+v)
				}
				return entries
			}, strings.NewReader(request), &output, &diagnostics, agentrunner.Config{Name: "runner", Version: "test-rev", ParseRequest: parseRequest, Providers: agentrunner.DefaultProviders()})
			if code != 0 {
				t.Fatalf("code=%d diagnostics=%s output=%s", code, diagnostics.String(), output.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if calls["parent"] != 3 || calls["lead"] != 4 || calls["worker"] != 2 || calls["collector"] != 1 {
				t.Fatalf("calls=%v", calls)
			}
			for role, id := range headerIDs {
				if bytes.Contains(output.Bytes(), []byte(id)) || id == "" {
					t.Fatalf("%s header exposed or absent", role)
				}
			}
			if strings.Contains(output.String()+diagnostics.String(), "private-value") || strings.Contains(output.String()+diagnostics.String(), "private-api-key") {
				t.Fatal("transport value leaked")
			}
			events := harnessEvents(t, output.Bytes())
			starts, ends, files := map[string]int{}, map[string]int{}, map[string]int{}
			var inputTokens, outputTokens int64
			var toolCalls, childResponses int
			var lastAssistant string
			for i, event := range events {
				if event.Sequence != uint64(i+1) {
					t.Fatal("output sequence is not monotonic")
				}
				switch event.Kind {
				case "subagent_start", "subagent_end":
					var data struct {
						Role   string
						Status string
					}
					json.Unmarshal(event.Data, &data)
					if event.Kind == "subagent_start" {
						starts[data.Role]++
					} else {
						ends[data.Role]++
						if data.Status != "completed" {
							t.Error("child failed", data)
						}
					}
				case "file_change":
					var data struct{ Path string }
					json.Unmarshal(event.Data, &data)
					files[data.Path]++
				case "model_response":
					var data struct{ Response llm.Response }
					if err := json.Unmarshal(event.Data, &data); err != nil {
						t.Fatal(err)
					}
					inputTokens += data.Response.Usage.InputTokens
					outputTokens += data.Response.Usage.OutputTokens
					for _, entry := range data.Response.Output {
						if entry.Type == llm.ItemToolCall {
							toolCalls++
						}
						if message, ok := entry.Data.(llm.Message); ok && message.Role == llm.RoleAssistant {
							lastAssistant = message.Text
							if event.ChildID != "" {
								t.Error("child final answer leaked")
							}
						}
					}
					if event.ChildID != "" {
						childResponses++
					}
				}
			}
			if starts["lead"] != 1 || starts["worker-high"] != 1 || starts["collector"] != 1 || fmt.Sprint(starts) != fmt.Sprint(ends) {
				t.Fatalf("starts=%v ends=%v", starts, ends)
			}
			if len(files) != 2 || files["shared.txt"] != 1 || files["sample.go"] != 1 {
				t.Fatalf("unique changes=%v", files)
			}
			if inputTokens != 110 || outputTokens != 30 || childResponses != 7 || toolCalls != 11 || lastAssistant != "PARENT FINAL" {
				t.Fatalf("usage=%d/%d children=%d tools=%d final=%q", inputTokens, outputTokens, childResponses, toolCalls, lastAssistant)
			}
			last := events[len(events)-1]
			var summary struct {
				IsError      bool
				Usage        llm.Usage
				ToolCalls    int
				ChangedFiles []string
			}
			if last.Kind != "session_end" || json.Unmarshal(last.Data, &summary) != nil || summary.IsError || summary.Usage.InputTokens != 110 || summary.ToolCalls != 11 || !slices.Equal(summary.ChangedFiles, []string{"sample.go", "shared.txt"}) {
				t.Fatal("invalid final summary", string(last.Data))
			}
			for name, want := range map[string]string{"shared.txt": "parent\n", "sample.go": "package p\nfunc new_name() {}\n"} {
				raw, err := os.ReadFile(filepath.Join(workspace, name))
				if err != nil || string(raw) != want {
					t.Fatalf("%s=%q err=%v", name, raw, err)
				}
			}
			paths, _ := filepath.Glob(filepath.Join(logs, "*.jsonl"))
			if len(paths) != 1 {
				t.Fatal("missing output log")
			}
			logged, err := os.ReadFile(paths[0])
			if err != nil || !bytes.Equal(logged, output.Bytes()) {
				t.Fatal("JSONL log differs from stdout")
			}
		})
	}
}

func TestHarnessRunnerCanceledChildHasEndEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("You are a collector for low-difficulty questions:")) {
			cancel()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"start\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc\",\"call_id\":\"fc\",\"name\":\"SubagentStart\",\"arguments\":\"{\\\"template\\\":\\\"collector\\\",\\\"task\\\":\\\"wait\\\"}\"}]}}\n\n")
	}))
	defer endpoint.Close()
	environment := map[string]string{"PATH": os.Getenv("PATH"), "HOME": t.TempDir(), "UNREAL_HARNESS_LLM_PROVIDER": "openai", "UNREAL_HARNESS_LLM_BASE_URL": endpoint.URL, "UNREAL_HARNESS_LLM_API_KEY": "synthetic-key"}
	var output, diagnostics bytes.Buffer
	code := agentrunner.RunMain(ctx, []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string { return environment[name] }, func() []string { return []string{"PATH=" + os.Getenv("PATH")} }, strings.NewReader(`{"prompt":"delegate","profile":"development","model":"fake","max_attempts":1}`), &output, &diagnostics, agentrunner.Config{Name: "runner", ParseRequest: parseRequest, Providers: agentrunner.DefaultProviders()})
	events := harnessEvents(t, output.Bytes())
	starts, ends := 0, 0
	for _, event := range events {
		if event.Kind == "subagent_start" {
			starts++
		}
		if event.Kind == "subagent_end" {
			ends++
		}
	}
	last := events[len(events)-1]
	if code != 130 || starts != 1 || ends != 1 || last.Type != "session_end" || !last.IsError {
		t.Fatalf("code=%d starts=%d ends=%d diagnostics=%s output=%s", code, starts, ends, diagnostics.String(), output.String())
	}
}

func TestHarnessRunnerProfileFlagAndPromptProfile(t *testing.T) {
	for _, tc := range []struct {
		name, flag, fields       string
		subagents, promptProfile bool
	}{
		{"default", "", "", false, false},
		{"flag", "development", "", true, false},
		{"JSON overrides flag", "development", `,"profile":"conservative"`, false, false},
		{"prompt profile", "", `,"profile":"development","profile_id":"openai-reasoning","profile_version":1,"model_family":"openai-reasoning"`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				var names []string
				for _, tool := range body.Tools {
					names = append(names, tool.Name)
				}
				if slices.Contains(names, "SubagentStart") != tc.subagents || slices.Contains(names, "write") {
					t.Error("composition or disallowed_tools ignored")
				}
				if tc.promptProfile && !bytes.Contains(raw, []byte("Inspect relevant code before editing.")) {
					t.Error("versioned request profile was lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer endpoint.Close()
			environment := map[string]string{"UNREAL_HARNESS_LLM_BASE_URL": endpoint.URL, "UNREAL_HARNESS_LLM_API_KEY": "synthetic-key"}
			args := []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}
			if tc.flag != "" {
				args = append(args, "-profile", tc.flag)
			}
			var output, diagnostics bytes.Buffer
			request := `{"prompt":"test","model":"fake","max_attempts":1,"disallowed_tools":["write"]` + tc.fields + `}`
			code := agentrunner.RunMain(t.Context(), args, func(name string) string { return environment[name] }, func() []string { return nil }, strings.NewReader(request), &output, &diagnostics, agentrunner.Config{Name: "runner", Providers: agentrunner.DefaultProviders(), ParseRequest: parseRequest})
			if code != 0 {
				t.Fatalf("code=%d diagnostics=%s output=%s", code, diagnostics.String(), output.String())
			}
		})
	}
}
