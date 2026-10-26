//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// TestLiveClaudeReadEditBash drives the official Claude Code CLI through the
// normal Host (U21): Claude Code runs Read, Edit and Bash, Unreal approves and
// records them, and the second turn resumes the same Claude Code conversation.
// It uses the signed-in user's subscription, so it is skipped unless
// UNREAL_LIVE_CLAUDE=1. UNREAL_LIVE_CLAUDE_MODEL picks the model (default
// haiku, the smallest use of the plan) and UNREAL_LIVE_CLAUDE_EFFORT its effort.
func TestLiveClaudeReadEditBash(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_CLAUDE") != "1" {
		t.Skip("set UNREAL_LIVE_CLAUDE=1 to use the signed-in Claude Code subscription")
	}
	backend, err := discoverClaudeAgent(t.Context(), serveConfiguration{}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	backend.Provider.Model.ID = "haiku"
	if model := os.Getenv("UNREAL_LIVE_CLAUDE_MODEL"); model != "" {
		backend.Provider.Model.ID = model
	}
	if effort := os.Getenv("UNREAL_LIVE_CLAUDE_EFFORT"); effort != "" {
		backend.ReasoningEffort = llm.ReasoningEffort(effort)
	}
	workspace := t.TempDir()
	if out, err := exec.Command("git", "-C", workspace, "init", "-q").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	sample := filepath.Join(workspace, "sample.txt")
	if err := os.WriteFile(sample, []byte("line one\nBEFORE\nline three\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := serveConfiguration{
		Launcher: &normalLauncherConfig{Version: 1, CodexDisabled: true, ClaudeDisabled: true},
		Runtime: agentrunner.RuntimeIdentity{Version: 1, Provider: backend.Provider, Claude: backend.Claude, Profile: profile.Default(), Workspace: workspace,
			SystemPrompt:    "You are a coding agent working in this workspace.",
			ReasoningEffort: backend.ReasoningEffort},
		Permissions: permission.Config{Tools: []string{"read", "write", "edit", "grep", "glob", "Bash", tool.ClaudeCodeName}, FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted},
	}
	c, _, output := startInteractiveHost(t, cfg, "--normal-runtime")
	v, err := c.Open(t.Context(), host.Create, "live-claude")
	if err != nil {
		output.mu.Lock()
		t.Fatal(err, "\n", output.buf.String())
	}
	started := time.Now()
	submitInteractive(t, c, v, "live-task", "Read sample.txt. Then use the Edit tool to replace the line BEFORE with AFTER. Then run `grep -c AFTER sample.txt` with the Bash tool. Finally reply with only the number that grep printed.")
	end, final := liveAwait(t, c, "live-claude", 10*time.Minute)
	t.Logf("model %s finished in %s: %d operations, %d responses, final %q", backend.Provider.Model.ID, time.Since(started).Round(time.Second), len(end.Operations), responseCount(end), final)
	if end.Failure != "" {
		t.Fatal("live session failed:", end.Failure)
	}
	data, err := os.ReadFile(sample)
	if err != nil || string(data) != "line one\nAFTER\nline three\n" {
		t.Fatalf("sample.txt was not edited as asked: %q %v", data, err)
	}
	recorded := map[string]bool{}
	for _, item := range end.History.Items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok {
			for _, output := range r.Response.Output {
				if call, ok := output.Data.(llm.ToolCall); ok && call.Name == tool.ClaudeCodeName {
					for _, name := range []string{"Read", "Edit", "Bash"} {
						if strings.Contains(call.Arguments, `"tool":"`+name+`"`) {
							recorded[name] = true
						}
					}
				}
			}
		}
	}
	if !recorded["Edit"] || !recorded["Bash"] || !strings.Contains(final, "1") {
		t.Fatalf("expected recorded Edit and Bash calls and the grep count in the answer; recorded %v, final %q", recorded, final)
	}

	// The next turn continues the same Claude Code conversation.
	submitInteractive(t, c, v, "live-follow-up", "Which line did you replace, and with what? Reply in one short sentence.")
	deadline := time.Now().Add(5 * time.Minute)
	for {
		next, text := liveAwait(t, c, "live-claude", time.Until(deadline))
		if next.Failure != "" {
			t.Fatal("follow-up failed:", next.Failure)
		}
		if responseCount(next) > responseCount(end) && strings.Contains(text, "AFTER") {
			t.Logf("follow-up: %q", text)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow-up did not answer from the resumed conversation: %q", text)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestLiveClaudeAskApproval checks the human approval path with the real CLI:
// in Ask, Claude Code's edit waits for the human; a denial leaves the file
// unchanged and an approval applies it. Skipped unless UNREAL_LIVE_CLAUDE=1.
func TestLiveClaudeAskApproval(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_CLAUDE") != "1" {
		t.Skip("set UNREAL_LIVE_CLAUDE=1 to use the signed-in Claude Code subscription")
	}
	backend, err := discoverClaudeAgent(t.Context(), serveConfiguration{}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	backend.Provider.Model.ID = "haiku"
	if model := os.Getenv("UNREAL_LIVE_CLAUDE_MODEL"); model != "" {
		backend.Provider.Model.ID = model
	}
	workspace := t.TempDir()
	sample := filepath.Join(workspace, "sample.txt")
	if err := os.WriteFile(sample, []byte("BEFORE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := serveConfiguration{
		Runtime: agentrunner.RuntimeIdentity{Version: 1, Provider: backend.Provider, Claude: backend.Claude, Profile: profile.Default(), Workspace: workspace,
			SystemPrompt: "You are a coding agent working in this workspace.", ReasoningEffort: backend.ReasoningEffort},
		Permissions:     permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{workspace}},
		PermissionModes: &permission.ModesConfig{Enabled: true, Maximum: &permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}},
	}
	c, _, _ := startInteractiveHost(t, cfg)
	v, err := c.Open(t.Context(), host.Create, "live-claude-ask")
	if err != nil {
		t.Fatal(err)
	}
	w, err := c.WatchPermissions(t.Context(), "live-claude-ask", v.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cancel()
	setGatewayHumanMode(t, w, permission.Ask)
	for i, allow := range []bool{false, true} {
		before := responseCount(func() host.View { v, _ := c.Inspect(t.Context(), "live-claude-ask", 0, 256); return v }())
		submitInteractive(t, c, v, "live-ask-"+string(rune('a'+i)), "Use the Edit tool to replace BEFORE with AFTER in sample.txt. If the edit is denied, do not try another way; say that it was denied.")
		var snapshot permission.ModeSnapshot
		var p permission.Pending
		wait := time.After(3 * time.Minute)
		for p.ID == "" {
			select {
			case s, ok := <-w.Updates:
				if !ok {
					t.Fatal("the permission stream ended")
				}
				if len(s.Pending) == 1 {
					snapshot, p = s, s.Pending[0]
				}
			case <-time.After(5 * time.Second):
				if now, err := c.Inspect(t.Context(), "live-claude-ask", 0, 256); err == nil && responseCount(now) > before {
					if text, done := liveFinal(now); done {
						t.Fatalf("Claude answered without asking to edit: %q", text)
					}
				}
			case <-wait:
				t.Fatal("no approval request reached the human")
			}
		}
		if p.Tool != tool.EditName || !strings.Contains(p.Scope, "sample.txt") {
			t.Fatal("the human was not shown Claude Code's edit", p.Tool, p.Scope)
		}
		if _, err := w.Request(t.Context(), permission.ControlRequest{Action: "decide", Expected: snapshot.Revision, Operation: p.ID, Allow: allow}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Minute)
		for {
			end, text := liveAwait(t, c, "live-claude-ask", time.Until(deadline))
			if end.Failure != "" {
				t.Fatal("live session failed:", end.Failure)
			}
			if responseCount(end) > before && text != "" {
				t.Logf("allow=%v: %q", allow, text)
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no answer after the decision")
			}
			time.Sleep(2 * time.Second)
		}
		data, _ := os.ReadFile(sample)
		if want := map[bool]string{false: "BEFORE\n", true: "AFTER\n"}[allow]; string(data) != want {
			t.Fatalf("allow=%v: sample.txt = %q, want %q", allow, data, want)
		}
	}
}
