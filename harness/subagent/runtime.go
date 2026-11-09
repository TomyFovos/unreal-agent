package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// RuntimeRequest contains no credentials, process flags or permission settings.
// Complete provider selections go through the operator's registered resolver.
// Keep legacy aliases intact here: templates enter immutable Session identity.
// An omitted Provider requests only Model/Effort overrides of the template;
// omitted override fields keep the template's values (the CLI start path).
type RuntimeRequest struct {
	Provider string              `json:"provider"`
	Model    string              `json:"model"`
	Effort   llm.ReasoningEffort `json:"effort"`
}

type RuntimeError struct{ Code, Hint string }

func (e *RuntimeError) Error() string {
	if e.Hint != "" {
		return "child runtime: " + e.Code + ": " + e.Hint
	}
	return "child runtime: " + e.Code
}

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
	var override *RuntimeRequest
	if requested != nil && requested.Provider == "" {
		copy := *requested
		copy.Effort = copy.Effort.Normalize()
		override = &copy
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

		if override != nil || plan.RuntimeOverride != nil {
			if override == nil || plan.RuntimeOverride == nil || *override != *plan.RuntimeOverride {
				return host.Receipt{}, host.ErrConflict
			}
		} else if requested != nil {
			s := sessionstore.SelectionFromConfiguration(plan.Configuration.Runtime)
			if s == nil || s.Provider != requested.Provider || s.Model != requested.Model || s.Effort != requested.Effort.Normalize() {
				return host.Receipt{}, host.ErrConflict
			}
		}
		return owner.SubmitOperation(ctx, generation, input, operation.ID(old.OperationID), old.ToolName, spec)
	}
	bound := false
	if override != nil {
		var e error
		configured, e = overrideRuntime(configured, *override)
		if e != nil {
			return host.Receipt{}, e
		}
	} else if resolve != nil {
		var e error
		configured, e = resolve(ctx, owner, configured, requested)
		if e != nil {
			return host.Receipt{}, e
		}
		bound = true
	} else if requested != nil {
		return host.Receipt{}, &RuntimeError{Code: "selection_unsupported"}
	}
	spec, e := NewSpec(Plan{Version: 1, Action: "start", ParentID: owner.ID, Template: name, Text: task, Configuration: &configured, RuntimeBound: bound, RuntimeOverride: override, Notify: true})
	if e != nil {
		return host.Receipt{}, e
	}
	return owner.SubmitOperation(ctx, generation, input, StartOperationID(owner.ID, input), "SubagentStart", spec)
}

// overrideRuntime changes only the two explicitly requested fields. Preserve
// every other runtime member, including ones this package does not interpret.
// Catalog availability is not required for a template override.
func overrideRuntime(template Template, requested RuntimeRequest) (Template, error) {
	fail := func(code string) (Template, error) { return template, &RuntimeError{Code: code} }
	if requested.Provider != "" {
		return fail("invalid_provider")
	}
	if requested.Effort != "" && !requested.Effort.Valid() {
		return fail("invalid_effort")
	}
	var identity, provider, model map[string]jsontext.Value
	if json.Unmarshal(template.Runtime, &identity) != nil || identity == nil ||
		json.Unmarshal(identity["Provider"], &provider) != nil || provider == nil ||
		json.Unmarshal(provider["model"], &model) != nil || model == nil {
		return fail("invalid_template")
	}
	if requested.Model != "" {
		model["id"], _ = json.Marshal(requested.Model)
		provider["model"], _ = json.Marshal(model)
		identity["Provider"], _ = json.Marshal(provider)
	}
	if requested.Effort != "" {
		identity["ReasoningEffort"], _ = json.Marshal(requested.Effort.Normalize())
	}
	raw, err := json.Marshal(identity)
	if err != nil || sessionstore.SelectionFromConfiguration(raw) == nil {
		return fail("invalid_selection")
	}
	template.Runtime = raw
	return template, nil
}

// StartOperationID is the handle StartChild gives a child started with input.
func StartOperationID(parent session.ID, input inbox.ID) operation.ID {
	sum := sha256.Sum256([]byte(string(parent) + "\x00" + string(input)))
	return operation.ID("child-start-" + hex.EncodeToString(sum[:16]))
}
