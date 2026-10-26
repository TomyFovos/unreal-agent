//go:build linux || darwin

package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// claudeRouteConfiguration runs the Claude Code route (U21) against the
// synthetic CLI, with human-controlled permission modes as in normal use.
func claudeRouteConfiguration(t *testing.T, f *testclaude.Fixture) serveConfiguration {
	t.Helper()
	workspace := t.TempDir()
	selection := provider.Selection{Version: 1, Provider: claudeagent.ProviderID, Model: provider.Model{ID: "claude-configured-a"}, Auth: credential.Reference{Provider: claudeagent.ProviderID, Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 2, Source: "synthetic Claude Code"}
	return serveConfiguration{
		Runtime: agentrunner.RuntimeIdentity{Version: 1, Provider: selection, Claude: &claudeagent.Config{Binary: f.Binary}, Profile: profile.Default(), Workspace: workspace,
			SystemPrompt: "Work in this workspace.", ReasoningEffort: "low"},
		Permissions:     permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{workspace}},
		PermissionModes: &permission.ModesConfig{Enabled: true, Maximum: &permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}},
	}
}

// Claude Code asks Unreal before it writes; Unreal's mode decides, and the
// human decides in Ask. Every call is kept as a canonical receipt.
func TestClaudeRouteHumanPermissions(t *testing.T) {
	for _, mode := range []string{"readonly", "ask-allow", "ask-deny", "full"} {
		t.Run(mode, func(t *testing.T) {
			f := testclaude.New(t)
			f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentSteps: []testclaude.AgentStep{{Tool: "Write", Input: jsontext.Value(`{"file_path":"human.txt","content":"human-approved\n"}`)}}, AgentFinal: "Claude Code fixture completed"})
			cfg := claudeRouteConfiguration(t, f)
			c, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
			v, err := c.Open(t.Context(), host.Create, "claude-route")
			if err != nil {
				t.Fatal(err)
			}
			w, err := c.WatchPermissions(t.Context(), "claude-route", v.Generation)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Cancel()
			switch mode {
			case "ask-allow", "ask-deny":
				setGatewayHumanMode(t, w, permission.Ask)
			case "full":
				setGatewayHumanMode(t, w, permission.FullAccess)
			}
			submitInteractive(t, c, v, "task", "Write human.txt.")
			path := filepath.Join(cfg.Runtime.Workspace, "human.txt")
			if strings.HasPrefix(mode, "ask") {
				snapshot, p := awaitPending(t, w)
				if p.Tool != tool.WriteName || !strings.Contains(p.Scope, "Claude Code Write: human.txt") {
					t.Fatal("the human was not shown what Claude Code will write", p.Tool, p.Scope)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("effect before human approval")
				}
				if _, err := w.Request(t.Context(), permission.ControlRequest{Action: "decide", Expected: snapshot.Revision, Operation: p.ID, Allow: mode == "ask-allow"}); err != nil {
					t.Fatal(err)
				}
			}
			end := inspectInteractive(t, c, "claude-route", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal(v.Failure)
				}
				return hasFinalText(v, "Claude Code fixture completed")
			})
			allowed := mode == "ask-allow" || mode == "full"
			data, err := os.ReadFile(path)
			if allowed != (err == nil && string(data) == "human-approved\n") {
				t.Fatal("the write does not follow Unreal's decision", mode, string(data), err)
			}
			// An allowed call is a completed receipt; a denied one is a failed
			// receipt that carries the reason Claude read.
			status := claudeCodeStatus(t, end)
			if allowed {
				if len(end.Operations) != 1 || end.Operations[0].Status != operation.StatusCompleted || end.Operations[0].Denial != nil {
					t.Fatal("expected the call's completed canonical receipt", len(end.Operations))
				}
				value, err := operation.DecodeValue(end.Operations[0])
				var result observed.Result
				if err != nil || json.Unmarshal(value, &result) != nil || result.Tool != "Write" || result.IsError {
					t.Fatal("the receipt does not record what happened", result)
				}
			} else if len(end.Operations) != 0 || status.Error == "" {
				t.Fatal("a denied call was not a failed receipt", len(end.Operations), status.Error)
			}
			if mode == "readonly" && !strings.Contains(status.Error, "Read Only") {
				t.Fatal("Read Only did not refuse the write before Claude Code's own rules", status.Error)
			}
		})
	}
}

// A command Claude Code's organization policy refuses is run by Unreal's own
// Bash under Unreal's mode (U22), and its result is handed back to Claude.
func TestClaudeRouteRunsRefusedCommandsInUnrealShell(t *testing.T) {
	for _, mode := range []string{"full", "ask-allow", "ask-deny", "readonly"} {
		t.Run(mode, func(t *testing.T) {
			f := testclaude.New(t)
			f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentSteps: []testclaude.AgentStep{{Tool: "Bash", Input: jsontext.Value(`{"command":"printf refused-command-ran > ran.txt; printf 'unreal-%s' output"}`), Refused: true}}, AgentFinal: "Claude Code fixture completed"})
			cfg := claudeRouteConfiguration(t, f)
			c, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
			v, err := c.Open(t.Context(), host.Create, "claude-refused")
			if err != nil {
				t.Fatal(err)
			}
			w, err := c.WatchPermissions(t.Context(), "claude-refused", v.Generation)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Cancel()
			switch mode {
			case "ask-allow", "ask-deny":
				setGatewayHumanMode(t, w, permission.Ask)
			case "full":
				setGatewayHumanMode(t, w, permission.FullAccess)
			}
			submitInteractive(t, c, v, "task", "Run the command.")
			if strings.HasPrefix(mode, "ask") {
				snapshot, p := awaitPending(t, w)
				if p.Tool != tool.BashName {
					t.Fatal("the refused command did not reach Unreal's own approval", p.Tool)
				}
				if _, err := w.Request(t.Context(), permission.ControlRequest{Action: "decide", Expected: snapshot.Revision, Operation: p.ID, Allow: mode == "ask-allow"}); err != nil {
					t.Fatal(err)
				}
			}
			end := inspectInteractive(t, c, "claude-refused", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal(v.Failure)
				}
				return hasFinalText(v, "Claude Code fixture completed")
			})
			ran := mode == "full" || mode == "ask-allow"
			data, err := os.ReadFile(filepath.Join(cfg.Runtime.Workspace, "ran.txt"))
			if ran != (err == nil && string(data) == "refused-command-ran") {
				t.Fatal("Unreal's shell did not follow Unreal's decision", mode, string(data), err)
			}
			if len(end.Operations) != 1 || end.Operations[0].Type != operation.TypeShell || end.Operations[0].ToolName != tool.BashName {
				t.Fatal("the refused command was not one Unreal Bash Operation", end.Operations)
			}
			// The command text is not its output, so only a run prints unreal-output.
			if !hasFinalText(end, "Unreal Agent handled `printf refused-command-ran > ran.txt; printf 'unreal-%s' output`") || ran != hasFinalText(end, "unreal-output") {
				t.Fatal("Claude did not receive Unreal's result for the refused command")
			}
		})
	}
}

// awaitPending waits on the human's permission stream, as the TUI does, for
// the one approval request Unreal shows.
func awaitPending(t *testing.T, w permission.ControlConnection) (permission.ModeSnapshot, permission.Pending) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case s, ok := <-w.Updates:
			if !ok {
				t.Fatal("the permission stream ended")
			}
			if len(s.Pending) == 1 {
				return s, s.Pending[0]
			}
		case <-timeout:
			t.Fatal("no approval request reached the human")
		}
	}
}

// claudeCodeStatus is the recorded status of the Session's one claude_code call.
func claudeCodeStatus(t *testing.T, v host.View) tool.CallStatus {
	t.Helper()
	calls := map[string]bool{}
	var status tool.CallStatus
	for _, item := range v.History.Items {
		switch data := item.Data.(type) {
		case sessionstore.ModelResponse:
			for _, output := range data.Response.Output {
				if call, ok := output.Data.(llm.ToolCall); ok && call.Name == tool.ClaudeCodeName {
					calls[call.CallID] = true
				}
			}
		case sessionstore.ToolCallStatus:
			if calls[data.CallID] {
				status = data.Status
			}
		}
	}
	if len(calls) != 1 {
		t.Fatal("expected one recorded claude_code call", len(calls))
	}
	return status
}
