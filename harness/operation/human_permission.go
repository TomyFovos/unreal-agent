package operation

import (
	"encoding/json/v2"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// Inspect the canonical plan, not a provider-supplied tool label. Unknown/custom
// plans are privileged by default. Classifying as read-only grants no resource
// beyond the immutable read-only baseline.
func humanPermissionRequest(op Operation) permission.Request {
	r := permission.Request{ID: string(op.ID), Tool: op.ToolName, Privileged: true, Scope: "operation resources (operator maximum policy)"}
	if r.Tool == "" {
		r.Tool = string(op.Type)
	}
	switch op.Type {
	case TypeViewImage, TypeSkillUse:
		r.Privileged = false
		r.Scope = "read-only files"
	case TypeValue:
		r.Privileged = false
		r.Scope = "value only"
	case TypeShell:
		r.Tool = "Bash"
		r.Scope = "AMBIENT filesystem, network, environment, credentials and shared mounts; no OS sandbox"
		// Show the human the command being approved. Scope is display text
		// only; approval is still per Operation.
		if state, err := DecodeShellState(op); err == nil && strings.TrimSpace(state.Input.Command) != "" {
			r.Scope = shellScope(state.Input.Command)
			if permission.ReadOnlyCommand(state.Input.Command) {
				r.Privileged, r.ReadOnlyCommand = false, true
			}
		}
	case TypeRemoteJob:
		state, err := DecodeRemoteJobState(op)
		if err == nil && state.Plan.Type == "subagent" {
			// A child follows this Session's mode and its parent answers its
			// requests (D1, U17), so orchestration itself needs no approval
			// and a running child outlives mode changes.
			var plan struct {
				Action, Template, Request string
			}
			_ = json.Unmarshal(state.Plan.Data, &plan)
			r.Privileged, r.Orchestration = false, true
			r.Scope = "subagent " + plan.Action
			if plan.Template != "" {
				r.Scope += " " + plan.Template
			}
		}
		if err == nil && state.Plan.Type == "native_file" {
			var plan struct {
				Action string `json:"action"`
				Path   string `json:"path"`
			}
			if json.Unmarshal(state.Plan.Data, &plan) == nil && (plan.Action == "write" || plan.Action == "edit") {
				r.Tool = plan.Action
				r.Scope = "native workspace file mutation"
				// Name the file, as the shell names its command.
				if path := strings.TrimSpace(plan.Path); path != "" {
					r.Scope = clipScope(plan.Action + " " + path)
				}
			}
			if json.Unmarshal(state.Plan.Data, &plan) == nil && (plan.Action == "read" || plan.Action == "grep" || plan.Action == "glob") {
				r.Privileged = false
				r.Tool = plan.Action
				r.Scope = "read-only workspace files"
			}
		}
		if err == nil && state.Plan.Type == "structural_code" {
			var plan struct {
				Apply bool `json:"apply"`
			}
			if json.Unmarshal(state.Plan.Data, &plan) == nil && !plan.Apply && op.ToolName == "ast_grep" {
				r.Privileged = false
				r.Scope = "read-only workspace AST"
			}
		}
	}
	return r
}

// shellScope shows a command on one line within the 512-byte Scope.
func shellScope(command string) string {
	return clipScope("$ " + command)
}

func clipScope(text string) string {
	text = strings.Join(strings.Fields(strings.ToValidUTF8(text, "\uFFFD")), " ")
	if len(text) <= 512 {
		return text
	}
	cut := 512 - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
