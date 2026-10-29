package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeRunningSubagentWaitYieldsToUserInput(t *testing.T) {
	for _, form := range []string{"literal", "variable", "timeout", "pipe"} {
		t.Run(form, func(t *testing.T) { testClaudeRunningSubagentWait(t, form) })
	}
}

func testClaudeRunningSubagentWait(t *testing.T, form string) {
	h := startGPTHost(t, false)
	hold := make(chan struct{})
	defer close(hold)
	h.gpt.mu.Lock()
	h.gpt.leadHold = hold
	h.gpt.mu.Unlock()
	wait := h.binary + " subagent wait --timeout 60s"
	switch form {
	case "variable":
		wait = "U=$(printf '%s' '" + filepath.Dir(h.binary) + "/')unreal-agent; timeout 560 $U subagent wait"
	case "timeout":
		wait = "PATH='" + filepath.Dir(h.binary) + "':$PATH timeout 9m unreal-agent subagent wait"
	case "pipe":
		wait = h.binary + " subagent wait | cat"
	}
	var steps []testclaude.AgentStep
	for _, command := range []string{
		h.binary + " subagent start --template lead --task 'Implement the feature.'",
		wait,
	} {
		input, _ := json.Marshal(map[string]string{"command": command})
		steps = append(steps, testclaude.AgentStep{Tool: "Bash", Input: input})
	}
	h.claude.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentSteps: steps, AgentOnce: true, AgentFinal: "user input answered"})
	view := h.designer(t)
	if form != "literal" {
		control, err := h.client.WatchPermissions(t.Context(), "designer", view.Generation)
		if err != nil {
			t.Fatal(err)
		}
		defer control.Cancel()
		setGatewayHumanMode(t, control, permission.FullAccess)
	}
	submitInteractive(t, h.client, view, "delegate", "Delegate the implementation.")
	// Observe the actual wait process among this fixture's descendants, after
	// its PreToolUse handshake, before sending the user input.
	waitInteractiveContext(t, func(context.Context) bool {
		calls := h.claude.Calls(t)
		if len(calls) == 0 {
			return false
		}
		commandName := h.binary
		if form == "timeout" {
			commandName = "unreal-agent"
		}
		return descendantCommand(calls[0].PID, commandName+"\x00subagent\x00wait\x00")
	})
	submitInteractive(t, h.client, view, "user-input", "Answer this user input first.")
	inspectInteractive(t, h.client, "designer", func(v host.View) bool {
		data, _ := json.Marshal(v.History.Items)
		return strings.Contains(string(data), "user input answered")
	})
	calls := h.claude.Calls(t)
	if len(calls) != 2 || !strings.Contains(calls[1].Input, "Answer this user input first.") {
		t.Fatalf("user turn did not replace the running wait: calls=%d", len(calls))
	}
}

func descendantCommand(pid int, command string) bool {
	// A multithreaded parent lists each child under the thread that forked it.
	var children []string
	tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	for _, task := range tasks {
		data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, task.Name()))
		children = append(children, strings.Fields(string(data))...)
	}
	for _, raw := range children {
		child, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		args, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", child))
		if strings.HasPrefix(string(args), command) || descendantCommand(child, command) {
			return true
		}
	}
	return false
}
