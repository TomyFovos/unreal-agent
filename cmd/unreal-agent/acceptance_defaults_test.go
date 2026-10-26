//go:build linux || darwin

package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestAcceptanceNormalBootstrapDefaultToolsReadContinuation(t *testing.T) {
	testAcceptanceNormalBootstrap(t, false)
}

func TestAcceptanceNormalBootstrapFullNativeWorkDefaultBudget(t *testing.T) {
	testAcceptanceNormalBootstrap(t, true)
}

// The normal first run registers the Claude Code route (U21). Claude Code runs
// Read, Edit and Bash itself; Unreal approves them under Full Access, keeps
// every call and result as a canonical receipt, and gives Claude Code the
// subscription login only (U5).
func testAcceptanceNormalBootstrap(t *testing.T, full bool) {
	_, f := normalFixture(t, false, true)
	const verify = "set -eu\n[ \"$(wc -l < sample.txt)\" -eq 200 ]\ngrep -q '^UNREAL_REPAIR_AFTER$' sample.txt\n! grep -q '^UNREAL_REPAIR_BEFORE$' sample.txt\nprintf 'UNREAL_ACCEPTANCE_OK\\n'"
	steps := []testclaude.AgentStep{{Tool: "Read", Input: jsontext.Value(`{"file_path":"sample.txt"}`)}}
	tools := []string{"Read"}
	if full {
		verifyInput, err := json.Marshal(map[string]any{"command": verify})
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps,
			testclaude.AgentStep{Tool: "Edit", Input: jsontext.Value(`{"file_path":"sample.txt","old_string":"UNREAL_REPAIR_BEFORE","new_string":"UNREAL_REPAIR_AFTER"}`)},
			testclaude.AgentStep{Tool: "Read", Input: jsontext.Value(`{"file_path":"sample.txt"}`)},
			testclaude.AgentStep{Tool: "Bash", Input: verifyInput})
		tools = append(tools, "Edit", "Read", "Bash")
	}
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, AgentSteps: steps, AgentFinal: "Claude Code fixture completed"})
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-api-key-never-passed")
	o, err := parseLaunchOptions([]string{"--bootstrap-claude-binary", f.Binary}, io.Discard, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := bootstrapRuntime(t.Context(), o, nil, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Provider.Provider != claudeagent.ProviderID {
		t.Fatal("first run did not register the Claude Code route", cfg.Runtime.Provider.Provider)
	}
	body := strings.Repeat("synthetic continuation line\n", 99) + "UNREAL_REPAIR_BEFORE\n" + strings.Repeat("synthetic continuation line\n", 100)
	for name, value := range map[string]string{"sample.txt": body, "AGENTS.md": "Read before editing; never commit or push.\n"} {
		if os.WriteFile(filepath.Join(cfg.Runtime.Workspace, name), []byte(value), 0600) != nil {
			t.Fatal("fixture write failed")
		}
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "sample.txt"}} {
		command := exec.Command("git", args...)
		command.Dir = cfg.Runtime.Workspace
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatal("synthetic git setup failed", err, string(out))
		}
	}
	var client *gateway.Client
	if binary := os.Getenv("UNREAL_COMMON_BIN_DIR"); binary != "" {
		client = startPackagedInteractiveHost(t, cfg, privateCLIDirectory(t), binary)
	} else {
		client, _, _ = startInteractiveHost(t, cfg)
	}
	v, err := client.Open(t.Context(), host.Create, "acceptance-defaults")
	if err != nil {
		t.Fatal(err)
	}
	control, err := client.WatchPermissions(t.Context(), "acceptance-defaults", v.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Cancel()
	setGatewayHumanMode(t, control, permission.FullAccess)
	submitInteractive(t, client, v, "acceptance-task", "sample.txtの中央行UNREAL_REPAIR_BEFOREをUNREAL_REPAIR_AFTERに直し、Bashで検証してください。commitとpushは禁止です。")
	end := inspectInteractive(t, client, "acceptance-defaults", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal("default normal Host turn failed", v.Failure)
		}
		return hasFinalText(v, "Claude Code fixture completed")
	})
	if len(end.Operations) != len(tools) || len(f.Calls(t)) != 1 {
		t.Fatal("expected one recorded receipt per Claude Code tool call in one Claude Code turn", len(end.Operations), len(f.Calls(t)))
	}
	recorded := map[string]int{}
	sawBody, sawVerified := false, false
	for _, op := range end.Operations {
		value, err := operation.DecodeValue(op)
		var result observed.Result
		if err != nil || json.Unmarshal(value, &result) != nil || op.ToolName != tool.ClaudeCodeName || op.Status != operation.StatusCompleted || op.Denial != nil || result.IsError {
			t.Fatal("Claude Code call was not recorded as its completed receipt", op.ToolName, op.Status, result.Tool)
		}
		recorded[result.Tool]++
		sawBody = sawBody || result.Tool == "Read" && result.Content == body
		sawVerified = sawVerified || result.Tool == "Bash" && strings.Contains(result.Content, "UNREAL_ACCEPTANCE_OK")
	}
	want := map[string]int{}
	for _, name := range tools {
		want[name]++
	}
	if !reflect.DeepEqual(recorded, want) || !sawBody || full && !sawVerified {
		t.Fatal("receipts differ from what Claude Code ran", recorded, sawBody, sawVerified)
	}
	if full {
		got, err := os.ReadFile(filepath.Join(cfg.Runtime.Workspace, "sample.txt"))
		if err != nil || string(got) != strings.Replace(body, "UNREAL_REPAIR_BEFORE", "UNREAL_REPAIR_AFTER", 1) {
			t.Fatal("the approved edit was not applied exactly")
		}
	}
	call := f.Calls(t)[0]
	cliArgument(t, call, "--permission-prompt-tool", "stdio")
	cliArgument(t, call, "--model", cfg.Runtime.Provider.Model.ID)
	if call.Directory != cfg.Runtime.Workspace {
		t.Fatal("Claude Code did not run in the workspace", call.Directory)
	}
	for i, arg := range call.Arguments {
		if arg == "--append-system-prompt" && (i+1 >= len(call.Arguments) || !strings.Contains(call.Arguments[i+1], "Full Access")) {
			t.Fatal("Claude was not told Unreal's permission mode")
		}
	}
	for _, kv := range call.Environment {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatal("an API key reached Claude Code; only the subscription login may be used")
		}
	}
}

func hasFinalText(v host.View, text string) bool {
	for _, item := range v.History.Items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok {
			for _, output := range r.Response.Output {
				if m, ok := output.Data.(llm.Message); ok && strings.Contains(m.Text, text) {
					return true
				}
			}
		}
	}
	return false
}
