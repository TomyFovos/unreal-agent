package operation

import (
	"encoding/json/v2"
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
	case TypeRemoteJob:
		state, err := DecodeRemoteJobState(op)
		if err == nil && state.Plan.Type == "native_file" {
			var plan struct {
				Action string `json:"action"`
			}
			if json.Unmarshal(state.Plan.Data, &plan) == nil && (plan.Action == "write" || plan.Action == "edit") {
				r.Tool = plan.Action
				r.Scope = "native workspace file mutation"
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
