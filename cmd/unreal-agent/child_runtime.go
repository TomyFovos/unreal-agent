package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

// Only built-in GPT defaults follow the parent's explicit connection choice.
// Availability never selects a different connection. Explicit operator
// templates and recorded children retain their own selections.
func builtinGPTTemplate(c agentrunner.RuntimeConfig, template subagent.Template, id string) (subagent.Template, error) {
	if buildfeatures.Disabled(id) {
		return template, disabledChildError()
	}
	backend, err := c.ProviderRuntime(id)
	if err != nil {
		return template, &subagent.RuntimeError{Code: "provider_unavailable", Hint: modelcatalog.AuthRequired.ProviderHint(id)}
	}
	var identity agentrunner.RuntimeIdentity
	if err = json.Unmarshal(template.Runtime, &identity); err != nil {
		return template, err
	}
	model := identity.Provider.Model.ID
	identity.Provider = backend.Provider
	identity.Provider.Model.ID = model
	identity.Claude = backend.Claude
	registry, err := c.Providers.WithModel(identity.Provider)
	if err != nil {
		return template, err
	}
	_, raw, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: identity, Providers: registry, Credentials: c.Credentials, SessionDirectory: c.SessionDirectory})
	if err != nil {
		return template, err
	}
	template.Runtime = raw
	if template.Default != nil {
		choice := *template.Default
		choice.Provider = id
		template.Default = &choice
	}
	if len(template.Subagents) > 0 {
		workers := make(map[string]subagent.Template, len(template.Subagents))
		for name, worker := range template.Subagents {
			workers[name], err = builtinGPTTemplate(c, worker, id)
			if err != nil {
				return template, err
			}
		}
		template.Subagents = workers
	}
	return template, nil
}

func builtinGPTProvider(owner *host.Session) string {
	active, _ := owner.RuntimeSelection()
	if active != nil && active.Provider == "openai" {
		return "openai"
	}
	return "openai-codex"
}

// claudeChildrenRefusal says what to do while /model-sub chose Claude: Claude's
// own Agent tool runs the sub-agents, so a built-in child start is refused.
const claudeChildrenRefusal = "/model-sub で Claude を選んでいる間は子を起動できません。子を使うには /model-sub で OpenAI Codex か OpenCode Go を選んでください"

// childRuntimeResolver binds a child start to a registered backend. With
// modelFile, a start of a built-in template that names no runtime runs the
// sub-agents' model chosen with /model-sub, and so do a lead's workers;
// a choice that cannot run fails the start, nothing falls back to Codex.
func childRuntimeResolver(c agentrunner.RuntimeConfig, modelFile string) subagent.RuntimeResolver {
	return func(ctx context.Context, owner *host.Session, name string, template subagent.Template, requested *subagent.RuntimeRequest) (subagent.Template, error) {
		// The child follows the Session's mode; only its maximum bounds the start.
		ctx = owner.WithCapabilityPolicy(ctx)
		fail := func(code string) (subagent.Template, error) { return template, &subagent.RuntimeError{Code: code} }
		chosen := false
		explicitCodex := false
		if requested == nil && modelFile != "" && template.Default != nil {
			m, e := readSubagentModel(modelFile)
			if e != nil {
				return fail("subagent_model_unreadable")
			}
			if buildfeatures.Disabled(m.Provider) {
				return template, disabledChildError()
			}
			explicitCodex = m.Provider == "openai-codex"
			if m.Provider == claudeagent.ProviderID {
				return template, &subagent.RuntimeError{Code: "provider_unavailable", Hint: claudeChildrenRefusal}
			}
			if m.Provider == opencodego.ProviderID {
				// The effort is the role's on this model, bound below.
				requested, chosen = &subagent.RuntimeRequest{Provider: m.Provider, Model: m.Model, Effort: template.Default.Effort}, true
			} else if template.Default.Provider != "openai-codex" && !c.BuiltinSubagents {
				return fail("provider_unavailable")
			}
		}
		if requested != nil && buildfeatures.Disabled(requested.Provider) {
			return template, disabledChildError()
		}
		if c.BuiltinSubagents {
			id := builtinGPTProvider(owner)
			if explicitCodex {
				id = "openai-codex"
			}
			if requested != nil && requested.Provider != "" {
				id = requested.Provider
			}
			var err error
			template, err = builtinGPTTemplate(c, template, id)
			if err != nil {
				return template, err
			}
			if requested != nil && requested.Provider == "" {
				choice := *template.Default
				if requested.Model != "" {
					choice.Model = requested.Model
				}
				if requested.Effort != "" {
					choice.Effort = requested.Effort
				}
				requested = &choice
			}
		}
		if requested == nil {
			requested = template.Default
		}
		if e := permission.FromContext(ctx).CheckTool("SubagentStart"); e != nil {
			return fail("permission_denied")
		}
		active, _ := owner.RuntimeSelection()
		if active == nil {
			return fail("selection_unavailable")
		}
		choice := *active
		if requested != nil {
			choice = sessionstore.RuntimeSelection{Version: 1, Provider: requested.Provider, Model: requested.Model, Effort: requested.Effort.Normalize()}
		}
		if buildfeatures.Disabled(choice.Provider) {
			return template, disabledChildError()
		}
		backend, e := c.ProviderRuntime(choice.Provider)
		if e != nil {
			return fail("invalid_provider")
		}
		if choice.Validate() != nil {
			return fail("invalid_selection")
		}
		// Authorize the owning Session before any CLI health/catalog process.
		if e = permission.FromContext(ctx).CheckProcess(); e != nil {
			return fail("permission_denied")
		}
		for _, name := range template.Policy.Tools {
			if e = permission.FromContext(ctx).CheckTool(name); e != nil {
				return fail("permission_denied")
			}
		}
		if c.CheckProvider(ctx, choice.Provider) != nil {
			return template, &subagent.RuntimeError{Code: "provider_unavailable", Hint: providerGuidance(choice.Provider, modelcatalog.AuthRequired.ProviderHint(choice.Provider), "。ログイン後は /model refresh を実行してください")}
		}
		catalog, e := c.Catalog(ctx, choice.Provider)
		if e != nil {
			return template, &subagent.RuntimeError{Code: "provider_unavailable", Hint: providerGuidance(choice.Provider, modelcatalog.CatalogUnavailable.ProviderHint(choice.Provider), "。/login でログイン手順を確認してください")}
		}
		model, e := catalog.FindSelection(choice.Model)
		if e != nil && !catalog.Authoritative && choice.Provider == active.Provider && choice.Model == active.Model && choice.Effort == active.Effort {
			model.ID, model.Name, model.Efforts = active.Model, active.Name, []llm.ReasoningEffort{active.Effort}
			e = nil
		}
		if e != nil {
			return fail("invalid_model")
		}
		if chosen {
			choice.Effort = subagentEffort(name, model)
		}
		if !model.AllowsEffort(choice.Effort) {
			return fail("unsupported_effort")
		}
		collector := subagentEffort("collector", model)
		raw, e := boundChildRuntime(c, template.Runtime, backend, choice.Model, choice.Effort, chosen, collector)
		if e != nil {
			return template, e
		}
		template.Runtime = raw
		if chosen && len(template.Subagents) > 0 {
			workers := make(map[string]subagent.Template, len(template.Subagents))
			for worker, t := range template.Subagents {
				if t.Runtime, e = boundChildRuntime(c, t.Runtime, backend, choice.Model, subagentEffort(worker, model), true, collector); e != nil {
					return template, e
				}
				workers[worker] = t
			}
			template.Subagents = workers
		}
		return template, nil
	}
}

// boundChildRuntime is a template's runtime on a registered backend, model
// and effort; chosen gives a built-in prompt for that model, whose lead names
// the collector's effort.
func boundChildRuntime(c agentrunner.RuntimeConfig, runtime jsontext.Value, backend agentrunner.ProviderRuntime, model string, effort llm.ReasoningEffort, chosen bool, collector llm.ReasoningEffort) (jsontext.Value, error) {
	var identity agentrunner.RuntimeIdentity
	if json.Unmarshal(runtime, &identity) != nil {
		return nil, &subagent.RuntimeError{Code: "invalid_template"}
	}
	identity.Provider = backend.Provider
	// Keep the exact requested/applied selection. A catalog row may match a
	// resolved version as well as its alias; inheritance must not silently
	// replace a pinned version with the alias (or break stable-ID retries).
	identity.Provider.Model.ID = model
	identity.ReasoningEffort = effort
	identity.Claude = backend.Claude
	if chosen {
		identity.SystemPrompt = subagentPromptFor(identity.SystemPrompt, backend.Provider.Provider, model, collector)
	}
	registry, e := c.Providers.WithModel(identity.Provider)
	if e != nil {
		return nil, &subagent.RuntimeError{Code: "invalid_model"}
	}
	_, raw, e := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: identity, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: c.Credentials})
	if e != nil {
		return nil, &subagent.RuntimeError{Code: "incompatible_runtime"}
	}
	return raw, nil
}

// Recovery validates the recorded configuration, never re-inherits the parent's
// current selection or replaces the child's catalog choice.
func childTemplateValidator(c agentrunner.RuntimeConfig) func(subagent.Template, subagent.Template) error {
	return func(base, chosen subagent.Template) error {
		fail := func() error { return &subagent.RuntimeError{Code: "configuration_mismatch"} }
		var recorded agentrunner.RuntimeIdentity
		if json.Unmarshal(chosen.Runtime, &recorded) != nil {
			return fail()
		}
		if buildfeatures.Disabled(recorded.Provider.Provider) {
			return disabledChildError()
		}
		if c.BuiltinSubagents {
			var err error
			base, err = builtinGPTTemplate(c, base, recorded.Provider.Provider)
			if err != nil {
				return fail()
			}
		}
		// The lead's prompt names the collector's effort, recorded with it.
		var collector llm.ReasoningEffort
		if worker, ok := chosen.Subagents["collector"]; ok {
			var identity agentrunner.RuntimeIdentity
			if json.Unmarshal(worker.Runtime, &identity) == nil {
				collector = identity.ReasoningEffort
			}
		}
		if validChildRuntime(c, base.Runtime, chosen.Runtime, collector) != nil {
			return fail()
		}
		base.Runtime = chosen.Runtime
		// A start on the sub-agents' model binds the lead's workers too.
		if len(base.Subagents) > 0 && len(chosen.Subagents) == len(base.Subagents) {
			workers := maps.Clone(base.Subagents)
			for name, worker := range chosen.Subagents {
				if t, ok := workers[name]; ok && validChildRuntime(c, t.Runtime, worker.Runtime, collector) == nil {
					t.Runtime = worker.Runtime
					workers[name] = t
				}
			}
			base.Subagents = workers
		}
		// Stored plans come back canonicalized, nested worker runtimes too.
		old, _ := json.Marshal(base)
		now, _ := json.Marshal(chosen)
		was, is := jsontext.Value(old), jsontext.Value(now)
		if was.Canonicalize() != nil || is.Canonicalize() != nil || !bytes.Equal(was, is) {
			return fail()
		}
		return nil
	}
}

// validChildRuntime reports whether recorded is base bound to a registered
// backend, as the resolver binds it.
func validChildRuntime(c agentrunner.RuntimeConfig, base, recorded jsontext.Value, collector llm.ReasoningEffort) error {
	mismatch := errors.New("child runtime mismatch")
	var a, b agentrunner.RuntimeIdentity
	if json.Unmarshal(base, &a) != nil || json.Unmarshal(recorded, &b) != nil {
		return mismatch
	}
	backend, e := c.ProviderRuntime(b.Provider.Provider)
	if e != nil {
		return mismatch
	}
	registered := backend.Provider
	registered.Model.ID = b.Provider.Model.ID
	registry, e := c.Providers.WithModel(registered)
	if e != nil {
		return mismatch
	}
	registered, e = registry.Resolve(registered)
	if e != nil {
		return mismatch
	}
	expected, _ := json.Marshal(registered)
	actual, _ := json.Marshal(b.Provider)
	if !bytes.Equal(expected, actual) {
		return mismatch
	}
	a.Provider = registered
	a.ReasoningEffort = b.ReasoningEffort
	want := recorded.Clone()
	if want.Canonicalize() != nil {
		return mismatch
	}
	// The template's own prompt, or the built-in one for the sub-agents' model.
	for _, prompt := range []string{a.SystemPrompt, subagentPromptFor(a.SystemPrompt, registered.Provider, registered.Model.ID, collector)} {
		a.SystemPrompt = prompt
		// The same factory normalization used at creation fills catalog display
		// names and resolved operator defaults. It performs no auth/inference and
		// cannot add a permission or replace the recorded model on recovery.
		_, expectedRuntime, e := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: a, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: c.Credentials})
		if e == nil && expectedRuntime.Canonicalize() == nil && bytes.Equal(expectedRuntime, want) {
			return nil
		}
	}
	return mismatch
}

func disabledChildError() error {
	return &subagent.RuntimeError{Code: buildfeatures.OpenCodeGoDisabledCode, Hint: buildfeatures.OpenCodeGoChildDisabledMessage}
}
