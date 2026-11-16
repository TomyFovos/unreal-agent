//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestAPIKeyBuiltInLeadAndGrandchildProcess(t *testing.T) {
	workspace := t.TempDir()
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "invalid-unselected-codex-source")
	t.Setenv("OPENAI_CODEX_AUTH_FILE", "")
	gpt := &fakeGPT{t: t, leadMarker: filepath.Join(workspace, "lead"), workerMarker: filepath.Join(workspace, "worker"), leadRuns: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-selected-key" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("API child used another connection")
		}
		if r.URL.Path == "/models" {
			io.WriteString(w, `{"data":[{"id":"gpt-6.1-sol"},{"id":"gpt-6-luna"}]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if json.Unmarshal(body, &request) != nil || (request.Reasoning.Effort != "max" && request.Reasoning.Effort != "high") {
			t.Error("API child did not preserve its template effort")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		gpt.ServeHTTP(w, r)
	}))
	defer server.Close()
	p, key := apiFixture(t, server.URL)
	cfg := codexServeConfiguration(workspace, server.URL)
	cfg.Runtime.Provider, cfg.Runtime.ReasoningEffort = p.Provider, ""
	no := false
	cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: &no, APIKeys: map[string]credential.Source{"openai": {File: key}}}
	maximum := permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	cfg.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	directory := privateCLIDirectory(t)
	modelFile := filepath.Join(directory, "subagent-model.json")
	client, _ := startMultiChildCLIWith(t, binary, cfg, directory, 2*time.Minute, "--subagent-model-file", modelFile)
	v, err := client.Open(t.Context(), host.Create, "api-parent")
	if err != nil {
		t.Fatal(err)
	}
	control, err := client.WatchPermissions(t.Context(), "api-parent", v.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Cancel()
	setGatewayHumanMode(t, control, permission.FullAccess)
	started, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "child.start", ID: "api-parent", Generation: v.Generation, InputID: "start-api-lead", Template: "lead", Task: "Start the worker and finish."})
	if err != nil || started.Receipt == nil {
		t.Fatal("API child start failed", err)
	}
	handle := subagent.StartOperationID("api-parent", "start-api-lead")
	// The existing peer-report batch waits 45 seconds before resuming a lead.
	// Match the normal lead/worker fixture's 120-second completion budget.
	command := exec.CommandContext(t.Context(), binary, "subagent", "wait", string(handle), "--timeout", "120s")
	command.Env = append(os.Environ(), subagentSocketEnv+"="+filepath.Join(directory, "host.sock"), subagentSessionEnv+"=api-parent")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "result: completed") {
		gpt.mu.Lock()
		t.Logf("fake model calls: lead=%d worker=%d", len(gpt.leadEfforts), len(gpt.workerEffort))
		gpt.mu.Unlock()
		parent, e := client.Inspect(t.Context(), "api-parent", 0, 256)
		if e == nil {
			for _, op := range parent.Operations {
				plan, e := subagent.DecodePlan(op)
				if e != nil || plan.ChildID == "" {
					continue
				}
				child, e := subagent.ReadChild(t.Context(), filepath.Join(directory, "sessions"), plan.ChildID, 0, 256)
				if e != nil {
					continue
				}
				t.Logf("child: responses=%d operations=%d", responseCount(child.View), len(child.View.Operations))
				for _, op := range child.View.Operations {
					t.Logf("child operation: tool=%s status=%s", op.ToolName, op.Status)
				}
				for _, item := range child.View.History.Items {
					if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
						t.Logf("child tool status: %s", strings.ReplaceAll(status.Status.Error, "fake-selected-key", "[redacted]"))
					}
				}
			}
		}
		t.Fatalf("API lead did not finish: %v %s", err, output)
	}
	for _, marker := range []string{gpt.leadMarker, gpt.workerMarker} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("API lead or grandchild failed to execute its task")
		}
	}
	gpt.mu.Lock()
	leadCalls, workerCalls := len(gpt.leadEfforts), len(gpt.workerEffort)
	gpt.mu.Unlock()
	if leadCalls == 0 || workerCalls == 0 {
		t.Fatal("API lead and grandchild did not both use the API")
	}
	// An explicit /model-sub choice takes priority over the API parent's
	// connection. Neither unavailable provider may fall back to that API.
	for _, chosen := range []subagentModel{{Version: 1, Provider: "openai-codex"}, {Version: 1, Provider: "opencode-go", Model: "deepseek-v4.1-flash"}} {
		if err := writeSubagentModel(modelFile, chosen); err != nil {
			t.Fatal(err)
		}
		_, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "child.start", ID: "api-parent", Generation: v.Generation, InputID: "unavailable-" + inbox.ID(chosen.Provider), Template: "collector", Task: "This selected provider is unavailable."})
		if err == nil || !strings.Contains(err.Error(), "provider_unavailable") {
			t.Fatal("an unavailable /model-sub choice did not report its own failure", chosen.Provider, err)
		}
	}
	gpt.mu.Lock()
	if len(gpt.leadEfforts) != leadCalls || len(gpt.workerEffort) != workerCalls {
		t.Error("an unavailable /model-sub choice used the parent's API connection")
	}
	gpt.mu.Unlock()
	assertNoStoredCredentials(t, filepath.Join(directory, "sessions"), "fake-selected-key")
}
