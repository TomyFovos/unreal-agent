//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

// Real native executors behind a loopback TLS CONNECT fixture. The proxy never
// receives real credentials. Model request failures cannot undo, obscure or
// reexecute the already committed mutations on explicit Session resume.
func TestDockerOAuthMultiToolContinuationFailureAndResume(t *testing.T) {
	for _, tc := range []struct {
		failure string
		size    int
		partial bool
	}{{"none", 128, false}, {"none", 4096, false}, {"none", 8192, false}, {"none", 8192, true}, {"http-502", 4096, false}, {"stream-server-error", 4096, false}, {"stream-unknown", 4096, false}} {
		t.Run(fmt.Sprintf("%s/%d/partial=%t", tc.failure, tc.size, tc.partial), func(t *testing.T) {
			failure := tc.failure
			t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
			t.Setenv("HTTPS_PROXY", testcodex.DockerProxyURL)
			t.Setenv("https_proxy", "")
			auth := sandboxCodexFixture(t)
			originalAuth, _ := os.ReadFile(auth)
			originalInfo, _ := os.Stat(auth)
			initial := "before\n" + strings.Repeat("fixture line\n", tc.size/13)
			write, _ := json.Marshal(map[string]any{"path": "unreal-gate-e-asp/sample.txt", "content": initial, "expected": mutation.Revision{}})
			read := `{"path":"unreal-gate-e-asp/sample.txt"}`
			if tc.partial {
				read = `{"path":"unreal-gate-e-asp/sample.txt","start_line":1,"lines":2}`
			}
			steps := []struct{ tool, args, result string }{
				{"Bash", `{"command":"mkdir unreal-gate-e-asp && printf 'CREATED\\n'"}`, "CREATED"},
				{"write", string(write), "applied"},
				{"read", read, "before"},
				{"edit", "", "applied"},
				{"read", read, "after"},
				{"Bash", `{"command":"test \"$(head -n 1 unreal-gate-e-asp/sample.txt)\" = after && printf 'FILE_VERIFIED\\n'"}`, "FILE_VERIFIED"},
			}
			var calls atomic.Int32
			var resumed atomic.Bool
			proxy := testcodex.NewOAuthProxyWithHandler(t, openaicodex.SandboxAccessSentinel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req multiWire
				if err := json.UnmarshalRead(r.Body, &req); err != nil {
					t.Error("invalid synthetic continuation request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				n := int(calls.Add(1)) - 1
				completed := min(n, len(steps))
				callCount, resultCount := map[string]int{}, map[string]int{}
				for _, item := range req.Input {
					m, ok := item.(map[string]any)
					if !ok {
						continue
					}
					id, _ := m["call_id"].(string)
					switch m["type"] {
					case "function_call":
						callCount[id]++
					case "function_call_output":
						resultCount[id]++
					}
				}
				for i := range completed {
					id := fmt.Sprint("fixture-call-", i)
					// Consumed terminal groups are optional bounded history, but
					// the fresh result is mandatory and every retained native pair
					// must be structurally complete. Canonical receipts are checked
					// independently below, including after failure/resume.
					if callCount[id] > 1 || callCount[id] != resultCount[id] || (i == completed-1 && !resumed.Load() && callCount[id] != 1) {
						t.Error("continuation lost or duplicated a canonical call/result pair")
					}
				}
				input, _ := json.Marshal(req.Input)
				if completed > 0 && !strings.Contains(string(input), steps[completed-1].result) {
					t.Error("latest canonical receipt absent from continuation")
				}
				if n == len(steps) && failure != "none" && !resumed.Load() {
					if failure == "http-502" {
						w.WriteHeader(http.StatusBadGateway)
						io.WriteString(w, `{"error":{"code":"server_error","message":"private-fixture-body","param":"private-fixture-param"}}`)
					} else {
						code := "server_error"
						if failure == "stream-unknown" {
							code = "private-fixture-code"
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"type\":\"error\",\"code\":%q,\"message\":\"private-fixture-body\",\"param\":\"private-fixture-param\"}\n\n", code)
					}
					return
				}
				var output []any
				if n < len(steps) {
					if n == 3 {
						// The fake model may use only what the actual native read sent
						// on this wire. No separately calculated RevisionOf shortcut.
						var observed files.Result
						for _, item := range req.Input {
							m, ok := item.(map[string]any)
							if !ok || m["type"] != "function_call_output" || m["call_id"] != "fixture-call-2" {
								continue
							}
							parts, _ := m["output"].([]any)
							for _, part := range parts {
								p, _ := part.(map[string]any)
								value, _ := p["text"].(string)
								if json.Unmarshal([]byte(value), &observed) != nil {
									t.Error("read wire body was not a typed native result")
								}
							}
						}
						expectedBody := initial
						if tc.partial {
							expectedBody = "before\nfixture line\n"
						}
						if observed.Code != "ok" || observed.Revision == nil || observed.Content != expectedBody || observed.Truncated {
							t.Error("received read lost exact body, range, revision or truncation metadata")
							w.WriteHeader(400)
							return
						}
						edit, _ := json.Marshal(map[string]any{"path": "unreal-gate-e-asp/sample.txt", "expected": observed.Revision, "old_text": strings.SplitN(observed.Content, "\n", 2)[0], "new_text": "after"})
						steps[n].args = string(edit)
					}
					id := fmt.Sprint("fixture-call-", n)
					output = []any{map[string]any{"type": "function_call", "id": id, "call_id": id, "name": steps[n].tool, "arguments": steps[n].args}}
				} else {
					output = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "CONTINUATION_OK"}}}}
				}
				data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("fixture-response-", n), "status": "completed", "output": output}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", data)
			}))
			cfg := humanModeConfiguration(codexServeConfiguration(t.TempDir(), openaicodex.BaseURL))
			cfg.Runtime.Provider.Auth = credential.Reference{Provider: "openai-codex", ID: "docker-sandbox-oauth", Method: credential.DockerSandboxOAuth}
			cfg.Runtime.ReasoningEffort = llm.ReasoningEffortMedium
			cfg.Permissions.NetworkOrigins = []string{"https://chatgpt.com"}
			registry, err := registeredProviders(cfg.Runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(t.TempDir(), "sessions")
			factory, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: cfg.Runtime, SessionDirectory: store, Providers: registry, Credentials: withExternalCodexCredentials(nil, auth), HTTPClient: &http.Client{Transport: proxy.Transport}})
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := permission.New(cfg.Permissions)
			if err != nil {
				t.Fatal(err)
			}
			defer baseline.Close()
			maximum, err := permission.New(*cfg.PermissionModes.Maximum)
			if err != nil {
				t.Fatal(err)
			}
			defer maximum.Close()
			owner, err := host.New(t.Context(), host.Config{Directory: store, Build: factory, PermissionMaximum: maximum})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			options := host.Options{ID: "continuation", Lifecycle: "interactive", Policy: baseline, Workspace: cfg.Runtime.Workspace, Configuration: identity}
			s, err := owner.Create(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			control, err := s.PermissionControl(s.Generation)
			if err != nil {
				t.Fatal(err)
			}
			key, detach := control.AcquireControl()
			rev := control.Snapshot().Revision
			challenge, err := control.PrepareFull(key, rev)
			if err != nil {
				t.Fatal(err)
			}
			if err = control.SetMode(key, rev, permission.FullAccess, challenge); err != nil {
				t.Fatal(err)
			}
			detach()
			input := inbox.Input{ID: "one-submission", Kind: inbox.InputExternal, Payload: []byte(`"Create, write, observe, edit, observe and verify the temporary fixture."`)}
			if _, err = s.Submit(t.Context(), s.Generation, input); err != nil {
				t.Fatal(err)
			}
			var before host.View
			waitInteractive(t, func() bool {
				before, err = s.Inspect(0, 4096)
				if err != nil {
					t.Fatal(err)
				}
				return !before.Running || responseCount(before) == len(steps)+1
			})
			if failure != "none" {
				stage, reason, status := "stream_error", "server_error", 200
				if failure == "http-502" {
					stage, status = "http_status", 502
				}
				if failure == "stream-unknown" {
					reason = "unknown"
				}
				for _, expected := range []string{"provider: remote_failure", "stage=" + stage, fmt.Sprint("status=", status), "reason=" + reason, "retryable=true"} {
					if !strings.Contains(before.Failure, expected) {
						t.Fatalf("missing safe failure %s: %s", expected, before.Failure)
					}
				}
				if responseCount(before) != len(steps) {
					t.Fatal("failed generation invented a model response")
				}
			} else if before.Failure != "" {
				t.Fatal(before.Failure)
			}
			if calls.Load() != int32(len(steps)+1) {
				t.Fatal("unexpected automatic model retry")
			}
			if len(before.Operations) != len(steps) {
				t.Fatal("canonical Operation count disagrees with tool chain")
			}
			ops := map[operation.ID]string{}
			for _, op := range before.Operations {
				if op.Status != operation.StatusCompleted || op.Denial != nil {
					t.Fatal("actual executor did not complete", op.ToolName)
				}
				if op.ToolName == "Bash" {
					var state operation.ShellState
					if json.Unmarshal(op.State, &state) != nil || state.Result == nil || state.Result.ExitCode != 0 {
						t.Fatal("Bash receipt disagrees with file")
					}
				} else {
					state, err := operation.DecodeRemoteJobState(op)
					if err != nil {
						t.Fatal(err)
					}
					var result files.Result
					if json.Unmarshal(state.Handle, &result) != nil || result.Code != "ok" && result.Code != "applied" {
						t.Fatal("native receipt not completed", result.Code)
					}
				}
				data, _ := json.Marshal(op)
				if _, exists := ops[op.ID]; exists {
					t.Fatal("duplicate Operation identity")
				}
				ops[op.ID] = string(data)
			}
			file := filepath.Join(cfg.Runtime.Workspace, "unreal-gate-e-asp/sample.txt")
			data, err := os.ReadFile(file)
			if err != nil || string(data) != strings.Replace(initial, "before", "after", 1) {
				t.Fatal("receipts disagree with actual file", err)
			}
			if err = owner.Close(); err != nil {
				t.Fatal(err)
			}
			resumed.Store(true)
			replacement, err := host.New(t.Context(), host.Config{Directory: store, Build: factory, PermissionMaximum: maximum})
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			s, err = replacement.Resume(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			var after host.View
			waitInteractive(t, func() bool {
				after, err = s.Inspect(0, 4096)
				if err != nil || after.Failure != "" {
					t.Fatal("resume failed", err, after.Failure)
				}
				return responseCount(after) == len(steps)+1
			})
			if after.Permissions.Mode != permission.ReadOnly || len(after.Operations) != len(ops) {
				t.Fatal("resume retained escalation or replayed operations")
			}
			for _, op := range after.Operations {
				data, _ := json.Marshal(op)
				if ops[op.ID] != string(data) {
					t.Fatal("completed receipt changed on resume")
				}
			}
			for i, item := range before.History.Items {
				x, _ := json.Marshal(item)
				y, _ := json.Marshal(after.History.Items[i])
				if string(x) != string(y) {
					t.Fatal("canonical history prefix changed")
				}
			}
			expectedRequests := int32(len(steps) + 1)
			if failure != "none" {
				expectedRequests++
			}
			if _, err = s.Submit(t.Context(), s.Generation, input); err != nil {
				t.Fatal(err)
			}
			if proxy.Requests.Load() != expectedRequests || proxy.DirectRequests.Load() != 0 || !proxy.RequestOK.Load() {
				t.Fatal("resume used fallback or repeated generation")
			}
			data, _ = os.ReadFile(file)
			if string(data) != strings.Replace(initial, "before", "after", 1) {
				t.Fatal("resume repeated a side effect")
			}
			encoded, _ := json.Marshal(after)
			assertNoCredentialLeak(t, string(encoded), openaicodex.SandboxAccessSentinel, "private-fixture-body", "private-fixture-param", "private-fixture-code")
			assertNoStoredCredentials(t, store, openaicodex.SandboxAccessSentinel, "private-fixture-body", "private-fixture-param", "private-fixture-code")
			currentAuth, _ := os.ReadFile(auth)
			currentInfo, _ := os.Stat(auth)
			if string(currentAuth) != string(originalAuth) || !currentInfo.ModTime().Equal(originalInfo.ModTime()) {
				t.Fatal("existing Codex auth changed")
			}
		})
	}
}
