package claudeagent

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAncestorInstructionsExcludeOnlyExistingFilesAboveWorkspace(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, ".claude", "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, dir := range []string{home, filepath.Dir(workspace), workspace} {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md", "CLAUDE.local.md"} {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("instructions"), 0600); err != nil {
				t.Fatal(err)
			}
			files = append(files, path)
		}
	}
	// A directory with an instruction filename is not an instruction file.
	if err := os.Remove(filepath.Join(home, "CLAUDE.local.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "CLAUDE.local.md"), 0700); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".claude", "CLAUDE.md")
	for _, root := range []string{workspace, workspace + string(filepath.Separator)} {
		settings := ancestorInstructionSettings(root, []string{"HOME=" + home})
		var decoded struct {
			Excludes []string `json:"claudeMdExcludes"`
		}
		if err := json.Unmarshal([]byte(settings), &decoded); err != nil {
			t.Fatal(err)
		}
		for _, path := range decoded.Excludes {
			info, err := os.Stat(path)
			if !filepath.IsAbs(path) || path == global || filepath.Dir(path) == workspace || err != nil || !info.Mode().IsRegular() {
				t.Fatal("invalid instruction exclusion", path, err)
			}
		}
		for _, path := range files {
			info, _ := os.Stat(path)
			want := filepath.Dir(path) != workspace && path != global && info.Mode().IsRegular()
			if slices.Contains(decoded.Excludes, path) != want {
				t.Fatalf("exclusion %s = %t, want %t", path, !want, want)
			}
		}
	}
	if got := ancestorInstructionSettings(string(filepath.Separator), nil); got != "" {
		t.Fatal("filesystem root has no ancestor instructions", got)
	}
}

func TestLaunchKeepsProjectInstructionsNativeAndEnablesOnlyMeasuredTools(t *testing.T) {
	h := newHarness(t, "echo")
	const marker = "PROJECT_INSTRUCTIONS_MUST_NOT_BE_APPENDED"
	if err := os.WriteFile(filepath.Join(h.workspace, "AGENTS.md"), []byte(strings.Repeat(marker, 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	parentInstructions := filepath.Join(filepath.Dir(h.workspace), "CLAUDE.local.md")
	if err := os.WriteFile(parentInstructions, []byte("parent instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.respond(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.args)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(data), "\n")
	for _, flag := range []string{"--tools", "--append-system-prompt", "--settings"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatal("missing launch argument", flag)
		}
		switch flag {
		case "--tools":
			if args[i+1] != enabledTools+",Agent" {
				t.Fatal(args[i+1])
			}
		case "--append-system-prompt":
			if strings.Contains(args[i+1], marker) {
				t.Fatal("duplicated project instructions")
			}
		case "--settings":
			var settings struct {
				Excludes []string `json:"claudeMdExcludes"`
			}
			if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil || !slices.Contains(settings.Excludes, parentInstructions) {
				t.Fatal(args[i+1], err)
			}
		}
	}
	if slices.Contains(args, "--disallowedTools") || slices.Contains(args, "--disable-slash-commands") {
		t.Fatal("redundant tool denylist or disabled /compact", args)
	}
	for _, disabled := range []string{"Agent", "Monitor", "AskUserQuestion"} {
		if slices.Contains(strings.Split(enabledTools, ","), disabled) {
			t.Fatal(disabled)
		}
	}
}

func TestLaunchToolsLeaveAgentToGPTChildren(t *testing.T) {
	if got := launchTools(SubagentCLI{Binary: "/opt/unreal/bin/unreal"}); got != enabledTools {
		t.Fatal(got)
	}
	if got := launchTools(SubagentCLI{}); got != enabledTools+",Agent" {
		t.Fatal(got)
	}
}

func TestAgentDelegationStaysInsideTheTurn(t *testing.T) {
	if env := launchEnv([]string{"HOME=/h"}, SubagentCLI{}); !slices.Contains(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") {
		t.Fatal("background tasks stay enabled without GPT children", env)
	}
	if env := launchEnv([]string{"HOME=/h"}, SubagentCLI{Binary: "/opt/unreal/bin/unreal"}); slices.Contains(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") {
		t.Fatal("GPT children configured, yet background tasks disabled", env)
	}
}

func TestLaunchDelegationTracksProviderAvailabilityEachTurn(t *testing.T) {
	h := newHarness(t, "echo")
	available := false
	h.client.config.Subagents = SubagentCLI{Binary: "/opt/unreal-agent", Socket: "/run/unreal.sock", Templates: []string{"lead"}, Available: func(context.Context) bool { return available }}
	for _, ready := range []bool{false, true, false} {
		available = ready
		if _, err := h.respond(t.Context()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(h.args)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(string(data), "\n")
		tools := args[slices.Index(args, "--tools")+1]
		prompt := string(data)
		if slices.Contains(strings.Split(tools, ","), "Agent") == ready || strings.Contains(prompt, "No GPT children are available") == ready || strings.Contains(prompt, "Templates: lead") != ready {
			t.Fatalf("available=%t tools=%s prompt=%s", ready, tools, prompt)
		}
	}
}
