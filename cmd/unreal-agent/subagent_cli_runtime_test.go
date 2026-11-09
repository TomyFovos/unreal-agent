//go:build linux || darwin

package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestSubagentCommandRuntimeOverrides(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	const prompt = "Runtime override test worker."
	type selection struct{ model, effort string }
	requests := make(chan selection, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model        string         `json:"model"`
			Instructions string         `json:"instructions"`
			Input        jsontext.Value `json:"input"`
			Reasoning    struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		output := []any{}
		if strings.Contains(request.Instructions, prompt) || strings.Contains(string(request.Input), prompt) {
			requests <- selection{request.Model, request.Reasoning.Effort}
			output = []any{map[string]any{"type": "function_call", "id": "finish", "call_id": "finish", "name": "Finish", "arguments": `{"status":"completed","summary":"review complete","changedFiles":[],"tests":[],"blockers":[]}`}}
		}
		event, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	defer server.Close()
	auth := t.TempDir()
	writeCodexAuth(t, filepath.Join(auth, "auth.json"), "runtime-test-token", "runtime-test-account")
	t.Setenv("CODEX_HOME", auth)
	t.Setenv("OPENAI_CODEX_AUTH_FILE", "")
	cfg := codexServeConfiguration(t.TempDir(), server.URL)
	cfg.Runtime.Provider.Model.ID = "parent-model"
	cfg.Runtime.ReasoningEffort = llm.ReasoningEffortMedium
	cfg.Permissions = permission.Config{Tools: []string{"SubagentStart", "Finish", "SendParent"}, FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	childRuntime := cfg.Runtime
	childRuntime.Provider.Model.ID = "template-model"
	childRuntime.ReasoningEffort = llm.ReasoningEffortHigh
	childRuntime.SystemPrompt = prompt
	cfg.Subagents = map[string]childTemplate{"review": {Runtime: childRuntime, Pinned: true, Permissions: permission.Config{Tools: []string{"Finish"}, NetworkOrigins: []string{server.URL}}}}
	directory := privateCLIDirectory(t)
	client, _ := startMultiChildCLI(t, binary, cfg, directory, 2*time.Minute)
	if _, err := client.Open(t.Context(), host.Create, "parent"); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), binary, append([]string{"subagent"}, args...)...)
		cmd.Env = append(os.Environ(), subagentSocketEnv+"="+filepath.Join(directory, "host.sock"), subagentSessionEnv+"=parent")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, tc := range []struct {
		name  string
		flags []string
		want  selection
	}{
		{"template", nil, selection{"template-model", "high"}},
		{"model", []string{"--model", "gpt-6-astra"}, selection{"gpt-6-astra", "high"}},
		{"effort", []string{"-effort", "low"}, selection{"template-model", "low"}},
		{"both", []string{"-model", "gpt-6-astra", "--effort", "max"}, selection{"gpt-6-astra", "max"}},
		{"legacy effort", []string{"--effort", "ultra"}, selection{"template-model", "max"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"start", "--template", "review", "--task", "Review the change."}, tc.flags...)
			started, err := cli(args...)
			if err != nil {
				t.Fatal(started, err)
			}
			handle := regexp.MustCompile(`as (child-start-[0-9a-f]+)\.`).FindStringSubmatch(started)
			if handle == nil {
				t.Fatal("missing child handle", started)
			}
			waited, err := cli("wait", handle[1], "--timeout", "20s")
			if err != nil || !strings.Contains(waited, "result: completed") {
				t.Fatal("child did not finish", waited, err)
			}
			select {
			case got := <-requests:
				if got != tc.want {
					t.Fatalf("inference runtime = %+v; want %+v", got, tc.want)
				}
			default:
				t.Fatal("child made no inference request")
			}
			label := "review (" + tc.want.model + " " + tc.want.effort + ")"
			for _, args := range [][]string{{"list", "--all"}, {"show", handle[1]}} {
				out, err := cli(args...)
				if err != nil || !strings.Contains(out, handle[1]+"  "+label) {
					t.Fatal("displayed runtime", out, err)
				}
			}
			view, err := client.Inspect(t.Context(), "parent", 0, 256)
			if err != nil {
				t.Fatal(err)
			}
			var plan *subagent.Plan
			for _, op := range view.Operations {
				if string(op.ID) == handle[1] {
					p, err := subagent.DecodePlan(op)
					if err != nil || op.Status != operation.StatusCompleted {
						t.Fatal("recorded plan", p, err, op.Status)
					}
					plan = &p
				}
			}
			if plan == nil {
				t.Fatal("missing recorded plan")
			}
			child, err := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), plan.ChildID, 0, 256)
			if err != nil || child.Configuration == nil {
				t.Fatal("recorded child", child, err)
			}
			if s := sessionstore.SelectionFromConfiguration(child.Configuration.Runtime); s == nil || s.Provider != "openai-codex" || s.Model != tc.want.model || string(s.Effort) != tc.want.effort {
				t.Fatal("recorded runtime selection", s)
			}
		})
	}
	if out, err := cli("start", "--template", "review", "--effort", "invented", "--task", "Review."); err == nil || !strings.Contains(out, `invalid effort "invented"`) {
		t.Fatal("invalid effort was not explained", out, err)
	}
}
