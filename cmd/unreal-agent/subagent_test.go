//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// Build and invoke the real CLI: serve -> RemoteJob -> child --stdio.
// Only the provider HTTP response is a fixture; processes/IPC/stores are real.
func TestCLIChildInheritsBoundInstructions(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, data)
	}
	for _, scenario := range []struct {
		present   bool
		control   string
		provider  string
		childOnly bool
	}{{present: true}, {}, {present: true, control: "steer"}, {control: "cancel"}, {present: true, provider: "openai-codex"}, {provider: "openai-codex", childOnly: true}, {present: true, provider: "openai"}} {
		present := scenario.present
		t.Run(fmt.Sprintf("present-%t-control-%s-provider-%s-childOnly-%t", present, scenario.control, scenario.provider, scenario.childOnly), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.Chmod(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(workspace, "sessions")
			const ruleA = "PROJECT_REVISION_A"
			const token, account = "child-external-token-sensitive", "child-external-account-sensitive"
			const managedKey = "child-managed-api-key-sensitive"
			authPath := filepath.Join(t.TempDir(), "auth file.json")
			if scenario.childOnly {
				authPath = filepath.Join(t.TempDir(), "auth.json")
			}
			var hostPID atomic.Int64
			var parentCalls, childCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var request struct {
					Model string `json:"model"`
				}
				if err = json.Unmarshal(data, &request); err != nil {
					t.Error(err)
					return
				}
				if scenario.provider == "openai-codex" && (!scenario.childOnly || request.Model == "child") {
					if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-ID") != account {
						t.Error("parent/child lost external Codex credentials")
					}
					if request.Model == "child" {
						assertChildCodexSource(t, binary, int(hostPID.Load()), authPath)
					}
				} else if scenario.provider == "openai" && r.Header.Get("Authorization") != "Bearer "+managedKey {
					t.Error("child lost managed API-key credentials")
				}
				assertNoCredentialLeak(t, string(data), token, account, managedKey, "unused-external-refresh-secret", "ambient-inline-token-sensitive", "ambient-inline-account-sensitive")
				var output []any
				switch request.Model {
				case "parent":
					if parentCalls.Add(1) == 1 {
						output = []any{map[string]any{"type": "function_call", "id": "start", "call_id": "start", "name": "SubagentStart", "arguments": `{"template":"worker","task":"delegated task only"}`}}
					}
				case "child":
					calls := childCalls.Add(1)
					body := string(data)
					if strings.Contains(body, "PARENT_CONVERSATION_ONLY") || strings.Contains(body, "PROJECT_REVISION_B") {
						t.Error("child copied parent conversation or rediscovered disk")
					}
					if strings.Contains(body, ruleA) != present {
						t.Error("child context lost bound snapshot")
					}
					if strings.Count(body, ruleA) > 1 {
						t.Error("instructions duplicated in delegated task")
					}
					output = []any{map[string]any{"type": "function_call", "id": "finish", "call_id": "finish", "name": "Finish", "arguments": `{"status":"completed","summary":"CLI child finished","changedFiles":[],"tests":["fixture"],"blockers":[]}`}}
					if scenario.control != "" && calls == 1 {
						output = []any{}
					} else if scenario.control == "steer" && !strings.Contains(body, "VIEWER_STEER_INPUT") {
						t.Error("viewer input did not reach child inbox")
					}
				default:
					t.Errorf("unexpected model %q", request.Model)
				}
				if output == nil {
					output = []any{}
				}
				event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", event)
			}))
			t.Cleanup(upstream.Close)
			identity := func(model string) agentrunner.RuntimeIdentity {
				selected, auth := "ollama", credential.Reference{Method: credential.None}
				if scenario.provider != "" && (!scenario.childOnly || model == "child") {
					selected = scenario.provider
					if selected == "openai-codex" {
						auth = codexReference()
					} else {
						auth = credential.Reference{Provider: selected, Method: credential.APIKey, ID: "primary"}
					}
				}
				return agentrunner.RuntimeIdentity{Version: 1, Workspace: workspace, ReasoningEffort: llm.ReasoningEffort("low"), Profile: profile.Default(), Provider: provider.Selection{Version: 1, Provider: selected, Model: provider.Model{ID: model}, Endpoint: upstream.URL, Auth: auth, Source: "test", MaxAttempts: 1}}
			}
			config := serveConfiguration{Runtime: identity("parent"), Permissions: permission.Config{Tools: []string{"SubagentStart", "SubagentSend", "SubagentCancel", "SendParent", "Finish"}, FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}, Subagents: map[string]childTemplate{"worker": {Runtime: identity("child"), Permissions: permission.Config{Tools: []string{"Finish", "SendParent"}, ReadRoots: []string{workspace}, NetworkOrigins: []string{upstream.URL}}}}}
			data, _ := json.Marshal(config)
			path := filepath.Join(workspace, "config.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if present {
				if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte(ruleA), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			t.Cleanup(cancel)
			// t.TempDir includes the test name; keep Unix socket paths short on macOS.
			socketDirectory, err := os.MkdirTemp("", "ua-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(socketDirectory); err != nil {
					t.Error(err)
				}
			})
			socket := filepath.Join(socketDirectory, "host.sock")
			cmd := exec.CommandContext(ctx, binary, "serve", "--config", path, "--session-directory", store, "--socket", socket)
			if scenario.provider == "openai-codex" {
				writeCodexAuth(t, authPath, token, account)
				cmd.Env = append(os.Environ(), "OPENAI_CODEX_ACCESS_TOKEN=ambient-inline-token-sensitive", "OPENAI_CODEX_ACCOUNT_ID=ambient-inline-account-sensitive", "OPENAI_API_KEY=sk-ambient-sensitive", "OPENAI_CODEX_AUTH_FILE=")
				if scenario.childOnly {
					cmd.Env = append(cmd.Env, "CODEX_HOME="+filepath.Dir(authPath))
				} else {
					cmd.Args = append(cmd.Args, "--codex-auth-file", authPath)
				}
			} else if scenario.provider == "openai" {
				credentialDirectory := filepath.Join(t.TempDir(), "credentials")
				backend, err := credential.OpenLocal(credentialDirectory)
				if err != nil {
					t.Fatal(err)
				}
				if err = credential.NewManager(backend, nil).Login(t.Context(), identity("parent").Provider.Auth, credential.Material{Token: credential.NewSecret(managedKey), Owner: credential.Managed}); err != nil {
					t.Fatal(err)
				}
				cmd.Args = append(cmd.Args, "--credential-directory", credentialDirectory)
			}
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			hostPID.Store(int64(cmd.Process.Pid))
			done := make(chan struct{})
			var processErr error
			go func() { processErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
					if processErr != nil {
						t.Errorf("CLI exit: %v\n%s", processErr, diagnostics.String())
					}
					assertNoCredentialLeak(t, diagnostics.String(), token, account, managedKey, "unused-external-refresh-secret", "ambient-inline-token-sensitive", "ambient-inline-account-sensitive", "sk-ambient-sensitive")
				case <-time.After(5 * time.Second):
					cancel()
					<-done
					t.Error("CLI did not drain")
				}
			})
			client := gateway.NewClient(socket)
			t.Cleanup(func() { client.Close() })
			for {
				if _, err := client.Methods(ctx); err == nil {
					break
				}
				select {
				case <-done:
					t.Fatalf("CLI startup: %v\n%s", processErr, diagnostics.String())
				default:
				}
				if ctx.Err() != nil {
					t.Fatal("CLI gateway did not start")
				}
				time.Sleep(time.Millisecond)
			}
			view, err := client.Open(ctx, host.Create, "parent")
			if err != nil {
				t.Fatal(err)
			}
			want := projectinstructions.None()
			if present {
				want, _ = projectinstructions.FromContent([]byte(ruleA))
			}
			if view.ProjectInstructions == nil || *view.ProjectInstructions != want.Metadata() {
				t.Fatal("parent binding missing")
			}
			// Change disk after parent creation and before the actual child spawn.
			if err = os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("PROJECT_REVISION_B"), 0600); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal("PARENT_CONVERSATION_ONLY")
			if _, err = client.Submit(ctx, "parent", view.Generation, inbox.Input{ID: "start", Kind: inbox.InputExternal, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			var completed operation.Operation
			remote := viewer.Remote{ParentID: "parent", Parent: client, Call: client.Extension}
			observer := viewer.NewClient(remote, viewer.New(viewer.SubagentOptions()), remote)
			controlled := false
			var receipt host.Receipt
			for {
				view, err = client.Inspect(ctx, "parent", 0, 256)
				if err != nil {
					t.Fatal(err)
				}
				if view.Failure != "" {
					t.Fatalf("parent failed: %s", view.Failure)
				}
				for _, op := range view.Operations {
					if op.ToolName == "SubagentStart" && (op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled) {
						completed = op
					}
				}
				if completed.ID != "" {
					break
				}
				if scenario.control != "" && !controlled && childCalls.Load() > 0 {
					if err = observer.Refresh(ctx, "parent"); err != nil {
						t.Fatal(err)
					}
					for _, op := range view.Operations {
						if op.ToolName != "SubagentStart" {
							continue
						}
						plan, e := subagent.DecodePlan(op)
						if e != nil {
							t.Fatal(e)
						}
						if e = observer.Refresh(ctx, plan.ChildID); e != nil {
							t.Fatal(e)
						}
						for range 2 {
							if e = observer.Resume(ctx, plan.ChildID, "viewer-resume"); e != nil {
								t.Fatal(e)
							}
						}
						if scenario.control == "steer" {
							receipt, e = observer.Steer(ctx, plan.ChildID, "viewer-steer", "VIEWER_STEER_INPUT")
						} else {
							receipt, e = observer.Cancel(ctx, plan.ChildID, "viewer-cancel", "stop")
						}
						if e != nil || receipt.Sequence == 0 {
							t.Fatalf("viewer control: %v %+v", e, receipt)
						}
						controlled = true
					}
				}
				if ctx.Err() != nil {
					t.Fatal("child did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			if scenario.control == "cancel" {
				if completed.Status != operation.StatusCanceled || !controlled {
					t.Fatalf("cancel outcome: %+v", completed)
				}
			} else if completed.Status != operation.StatusCompleted {
				t.Fatalf("child outcome: %+v", completed)
			}
			plan, err := subagent.DecodePlan(completed)
			if err != nil {
				t.Fatal(err)
			}
			child, err := subagent.ReadChild(ctx, store, plan.ChildID, 0, 256)
			if err != nil {
				t.Fatal(err)
			}
			if child.Configuration == nil || child.Configuration.ProjectInstructions == nil || *child.Configuration.ProjectInstructions != want {
				t.Fatal("child configuration snapshot differs")
			}
			if child.View.ProjectInstructions == nil || *child.View.ProjectInstructions != want.Metadata() {
				t.Fatal("child metadata differs")
			}
			public, err := json.Marshal(child.View)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(public), "PROJECT_REVISION_A") {
				t.Fatal("child view exposed project instructions")
			}
			canonicalStore, err := localfile.New(store)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := canonicalStore.Items(ctx, plan.ChildID, 0, 256)
			if err != nil {
				t.Fatal(err)
			}
			canonicalBindings := 0
			for _, item := range canonical.Items {
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == sessionstore.HostProjectInstructions {
					canonicalBindings++
					if record.ProjectInstructions == nil || *record.ProjectInstructions != want {
						t.Fatal("canonical child binding differs")
					}
				}
			}
			if canonicalBindings != 1 {
				t.Fatalf("canonical bindings=%d", canonicalBindings)
			}
			bindings, tasks, ready := 0, 0, 0
			for _, item := range child.View.History.Items {
				if record, ok := item.Data.(host.ProjectInstructionRecord); ok && record.Kind == sessionstore.HostProjectInstructions {
					if record.ProjectInstructions == nil || *record.ProjectInstructions != want.Metadata() {
						t.Fatal("child binding metadata differs")
					}
					bindings++
				}
				if in, ok := item.Data.(inbox.Input); ok && in.Kind == inbox.InputPeer {
					peer, _ := in.DecodePeerMessage()
					if in.ID == inbox.ID("task:"+string(completed.ID)) && peer.Text != "delegated task only" {
						t.Error("task payload includes extra context")
					}
					if in.ID == inbox.ID("task:"+string(completed.ID)) {
						tasks++
					}
				}
			}
			for _, item := range view.History.Items {
				if in, ok := item.Data.(inbox.Input); ok && in.Kind == inbox.InputPeer {
					peer, _ := in.DecodePeerMessage()
					if peer.Kind == "ready" {
						ready++
					}
					if strings.Contains(peer.Text, "CLI child finished") {
						t.Error("final result duplicated as peer")
					}
				}
			}
			wantCalls := int32(1)
			if scenario.control == "steer" {
				wantCalls = 2
			}
			if bindings != 1 || tasks != 1 || ready != 1 || childCalls.Load() != wantCalls {
				t.Fatalf("bindings=%d tasks=%d ready=%d childCalls=%d", bindings, tasks, ready, childCalls.Load())
			}
			if scenario.control == "cancel" {
				if child.Finish != nil {
					t.Fatal("cancel invented Finish")
				}
			} else if child.Finish == nil || child.Finish.Result.Summary != "CLI child finished" {
				t.Fatal("canonical Finish missing")
			}
			for range 2 {
				if err = observer.Refresh(ctx, "parent"); err != nil {
					t.Fatal(err)
				}
				if err = observer.Refresh(ctx, plan.ChildID); err != nil {
					t.Fatal(err)
				}
			}
			detail, ok := observer.Model.Detail(plan.ChildID, time.Now())
			if !ok || detail.Row.ProjectInstructions == nil || *detail.Row.ProjectInstructions != want.Metadata() || detail.Row.Runtime != viewer.RuntimeUnknown || detail.Row.Usage.Responses != int(wantCalls) || detail.Row.Usage.Input != int64(wantCalls)*10 {
				t.Fatalf("viewer projection: %+v", detail.Row)
			}
			if (detail.Row.Finish == nil) != (child.Finish == nil) {
				t.Fatal("viewer result provenance differs")
			}
			page, e := observer.History(ctx, plan.ChildID, 0, 2)
			if e != nil || len(page.Items) != 2 || !page.More {
				t.Fatalf("viewer page: %v %+v", e, page)
			}
			if scenario.control == "steer" {
				again, e := observer.Steer(ctx, plan.ChildID, "viewer-steer", "VIEWER_STEER_INPUT")
				if e != nil || again != receipt {
					t.Fatalf("terminal steer retry: %v", e)
				}
			}
			if scenario.control == "cancel" {
				again, e := observer.Cancel(ctx, plan.ChildID, "viewer-cancel", "stop")
				if e != nil || again != receipt {
					t.Fatalf("terminal cancel retry: %v", e)
				}
			}
			if _, e = observer.Steer(ctx, plan.ChildID, "new-control", "too late"); e == nil {
				t.Fatal("terminal child accepted new viewer input")
			}
			assertNoStoredCredentials(t, store, token, account, managedKey, "unused-external-refresh-secret", "ambient-inline-token-sensitive", "ambient-inline-account-sensitive", "sk-ambient-sensitive")
		})
	}
}

// Inspect the running child while its fake provider request is in flight. Linux
// exposes children per thread, so inspect every Host thread's children list.
func assertChildCodexSource(t *testing.T, binary string, parentPID int, authPath string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	lists, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", parentPID))
	if err != nil {
		t.Error(err)
		return
	}
	for _, list := range lists {
		data, err := os.ReadFile(list)
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(data)) {
			arguments, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
			if err != nil {
				continue
			}
			args := strings.Split(strings.TrimRight(string(arguments), "\x00"), "\x00")
			if len(args) < 2 || args[0] != binary || args[1] != "child" {
				continue
			}
			if len(args) != 5 || args[2] != "--stdio" || args[3] != "--codex-auth-file" || args[4] != authPath {
				t.Error("child did not receive only the explicit Codex auth file path")
			}
			environment, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
			if err != nil {
				t.Error("could not inspect child environment")
				return
			}
			entries := strings.Split(strings.TrimRight(string(environment), "\x00"), "\x00")
			if len(entries) != 2 {
				t.Error("child environment inherited ambient entries")
			}
			for _, entry := range entries {
				if entry != "PATH=/usr/bin:/bin" && entry != "LANG=C.UTF-8" {
					t.Error("child environment contains an unauthorized entry")
				}
			}
			return
		}
	}
	t.Error("could not find the running Codex child")
}
