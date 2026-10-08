package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// RuntimeRequest contains no credentials, process flags or permission settings.
// It is a request; the operator's registered resolver has final authority.
type RuntimeRequest struct {
	Provider string              `json:"provider"`
	Model    string              `json:"model"`
	Effort   llm.ReasoningEffort `json:"effort"`
}
type RuntimeError struct{ Code string }

func (e *RuntimeError) Error() string { return "child runtime: " + e.Code }

type RuntimeResolver func(context.Context, *host.Session, Template, *RuntimeRequest) (Template, error)

// StartChild uses the same canonical SubagentStart Operation as model calls.
// The stable input ID also gives operator starts their existing retry semantics.
func StartChild(ctx context.Context, owner *host.Session, generation string, input inbox.ID, name, task string, templates map[string]Template, requested *RuntimeRequest, resolve RuntimeResolver) (host.Receipt, error) {
	if owner == nil || generation != owner.Generation {
		return host.Receipt{}, host.ErrStaleGeneration
	}
	configured, ok := templates[name]
	if !ok {
		return host.Receipt{}, &RuntimeError{Code: "invalid_template"}
	}
	if input == "" {
		return host.Receipt{}, &RuntimeError{Code: "invalid_request"}
	}
	if old, ok := owner.OperationIntent(input); ok {
		var spec operation.Spec
		if old.ToolName != "SubagentStart" || json.Unmarshal(old.Spec, &spec) != nil {
			return host.Receipt{}, host.ErrConflict
		}
		var state operation.RemoteJobState
		var plan Plan
		if spec.Type != operation.TypeRemoteJob || spec.Version != operation.VersionRemoteJob || json.Unmarshal(spec.State, &state) != nil || state.Plan.Type != PlanType || state.Plan.Version != PlanVersion || json.Unmarshal(state.Plan.Data, &plan) != nil || plan.Validate() != nil || plan.Action != "start" || plan.Template != name || plan.Text != task {
			return host.Receipt{}, host.ErrConflict
		}

		if requested != nil {
			s := sessionstore.SelectionFromConfiguration(plan.Configuration.Runtime)
			if s == nil || s.Provider != requested.Provider || s.Model != requested.Model || s.Effort != requested.Effort {
				return host.Receipt{}, host.ErrConflict
			}
		}
		return owner.SubmitOperation(ctx, generation, input, operation.ID(old.OperationID), old.ToolName, spec)
	}
	bound := false
	if resolve != nil {
		var e error
		configured, e = resolve(ctx, owner, configured, requested)
		if e != nil {
			return host.Receipt{}, e
		}
		bound = true
	} else if requested != nil {
		return host.Receipt{}, &RuntimeError{Code: "selection_unsupported"}
	}
	spec, e := NewSpec(Plan{Version: 1, Action: "start", ParentID: owner.ID, Template: name, Text: task, Configuration: &configured, RuntimeBound: bound})
	if e != nil {
		return host.Receipt{}, e
	}
	sum := sha256.Sum256([]byte(string(owner.ID) + "\x00" + string(input)))
	return owner.SubmitOperation(ctx, generation, input, operation.ID("child-start-"+hex.EncodeToString(sum[:16])), "SubagentStart", spec)
}
