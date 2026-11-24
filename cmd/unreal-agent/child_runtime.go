package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
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

// A lead keeps its own runtime, and each new worker uses its Session's
// current role choice. Provider registrations and catalog readers are
// process configuration, outside the lead's recorded runtime identity.
func configureWorkerModels(c *agentrunner.RuntimeConfig, modelFile string, sources map[string]credential.Source, parent subagent.ChildConfig) {
	if _, err := c.ProviderRuntime(opencodego.ProviderID); err == nil {
		models := &openCodeGoCatalog{url: openCodeGoModelsURL(serveConfiguration{Runtime: c.Identity, Providers: c.Backends})}
		c.Catalogs[opencodego.ProviderID] = func(ctx context.Context) (modelcatalog.Catalog, error) { return models.read(ctx, false) }
	}
	c.Health = map[string]func(context.Context) error{}
	if backend, err := c.ProviderRuntime(claudeagent.ProviderID); err == nil && backend.Claude != nil {
		models := &claudeCatalog{binary: backend.Claude.Binary}
		c.Health[claudeagent.ProviderID] = models.check
		c.Catalogs[claudeagent.ProviderID] = func(ctx context.Context) (modelcatalog.Catalog, error) { return models.read(ctx, false) }
	}
	backends := maps.Clone(c.Backends)
	if backends == nil {
		backends = map[string]agentrunner.ProviderRuntime{}
	}
	backends[c.Identity.Provider.Provider] = agentrunner.ProviderRuntime{Provider: c.Identity.Provider, ReasoningEffort: c.Identity.ReasoningEffort, Claude: c.Identity.Claude}
	for id, p := range backends {
		if b := apiBackend(p, sources[id]); b.Catalog != nil {
			c.Health[id] = b.Check
			c.Catalogs[id] = func(ctx context.Context) (modelcatalog.Catalog, error) { return b.Catalog(ctx, false) }
		}
	}
	parentLayer := parent.Layer
	if parentLayer == "" {
		parentLayer = subagent.RoleGroup(parent.Template)
	}
	models := subagentModelScope{defaultFile: modelFile, sessionDirectory: c.SessionDirectory, parent: parent.ParentID}
	resolve := childRuntimeResolverForLayer(*c, models, parentLayer, nil)
	c.ResolveChildRuntime = func(ctx context.Context, owner *host.Session, name string, template subagent.Template, requested *subagent.RuntimeRequest) (subagent.Template, error) {
		// Older leads recorded already bound templates with no Default. Supply
		// the built-in role default only while resolving, preserving their shape
		// for canonical replay and template validation.
		originalDefault := template.Default
		choice := subagent.RuntimeRequest{Provider: "openai-codex", Model: leadModel, Effort: llm.ReasoningEffort(strings.TrimPrefix(name, "worker-"))}
		if name == "collector" {
			choice.Model, choice.Effort = collectorModel, collectorEffort
		}
		template.Default = &choice
		chosen, err := resolve(ctx, owner, name, template, requested)
		chosen.Default = originalDefault
		return chosen, err
	}
	c.ValidateChildTemplate = childTemplateValidator(*c)
}

// childRuntimeResolver binds a child start to a registered backend. With
// role settings, a built-in template and its workers take their choices from
// the top-level Session that owns the start.
func childRuntimeResolver(c agentrunner.RuntimeConfig, modelFile string) subagent.RuntimeResolver {
	models := subagentModelScope{defaultFile: modelFile, sessionDirectory: c.SessionDirectory}
	return childRuntimeResolverForLayer(c, models, "main", nil)
}

func childRuntimeResolverForLayer(c agentrunner.RuntimeConfig, models subagentModelScope, parentLayer string, leadDefault *subagent.RuntimeRequest) subagent.RuntimeResolver {
	return func(ctx context.Context, owner *host.Session, name string, template subagent.Template, requested *subagent.RuntimeRequest) (subagent.Template, error) {
		template.Layer = subagent.ResolveLayer(name, parentLayer)
		roleName := name
		roleDefault := template.Default
		if parentLayer == "main" && template.Layer == "lead" && subagent.RoleGroup(name) != "lead" && (c.BuiltinSubagents || models.defaultFile != "") {
			roleName = "lead"
			if roleDefault != nil {
				choice := *roleDefault
				choice.Model, choice.Effort = leadModel, leadEffort
				if leadDefault != nil {
					choice = *leadDefault
				}
				roleDefault = &choice
			}
		}
		// The child follows the Session's mode; only its maximum bounds the start.
		ctx = owner.WithCapabilityPolicy(ctx)
		fail := func(code string) (subagent.Template, error) { return template, &subagent.RuntimeError{Code: code} }
		active, _ := owner.RuntimeSelection()
		if active == nil {
			return fail("selection_unavailable")
		}
		choice := *active
		if roleDefault != nil {
			choice = sessionstore.RuntimeSelection{Version: 1, Provider: roleDefault.Provider, Model: roleDefault.Model, Effort: roleDefault.Effort.Normalize()}
		}
		if c.BuiltinSubagents {
			choice.Provider = builtinGPTProvider(owner)
		}
		var saved subagentModel
		var roleChoice subagentModelChoice
		if models.defaultFile != "" && roleDefault != nil {
			m, e := models.read(ctx, owner.ID)
			if e != nil {
				return fail("subagent_model_unreadable")
			}
			saved, roleChoice = m, m.choice(roleName)
			if roleChoice.Provider != "" {
				choice.Provider = roleChoice.Provider
			}
			if roleChoice.Model != "" {
				choice.Model = roleChoice.Model
			}
		}
		choice.Fast = roleChoice.Fast
		chosen := roleChoice.Model != "" || roleChoice.Effort != "" || roleChoice.Fast
		id := choice.Provider
		if requested != nil && (requested.Provider != "" || requested.Model != "") {
			id = requested.Provider
		}
		if buildfeatures.Disabled(id) {
			return template, disabledChildError()
		}
		policy := permission.FromContext(ctx)
		if policy.CheckTool("SubagentStart") != nil || policy.CheckProcess() != nil {
			return fail("permission_denied")
		}
		for _, name := range template.Policy.Tools {
			if policy.CheckTool(name) != nil {
				return fail("permission_denied")
			}
		}
		backend, model, e := selectChildModel(ctx, c, &choice, requested, active)
		if e != nil {
			return template, e
		}
		if chosen && choice.Provider == roleChoice.Provider {
			if choice.Provider == opencodego.ProviderID {
				choice.Effort = subagentEffort(roleName, model)
			} else if roleChoice.Effort == "" && !model.AllowsEffort(choice.Effort) {
				choice.Effort = model.DefaultEffort
			}
			if roleChoice.Effort != "" {
				choice.Effort = roleChoice.Effort
			}
		}
		if requested != nil {
			if requested.Effort != "" {
				choice.Effort = requested.Effort.Normalize()
			} else if requested.Model != "" && !model.AllowsEffort(choice.Effort) {
				choice.Effort = model.DefaultEffort
			}
			choice.Fast = choice.Fast && model.AllowsFast(choice.Provider)
		}
		if choice.Validate() != nil {
			return fail("invalid_selection")
		}
		if !model.AllowsEffort(choice.Effort) {
			return fail("unsupported_effort")
		}
		if c.BuiltinSubagents {
			template, e = builtinGPTTemplate(c, template, choice.Provider)
			if e != nil {
				return template, e
			}
		}
		collector := subagentEffort("collector", model)
		retarget := chosen || c.BuiltinSubagents && requested != nil && (requested.Model != "" || requested.Provider != "")
		raw, e := boundChildRuntime(c, template.Runtime, backend, choice, retarget, collector)
		if e != nil {
			return template, e
		}
		template.Runtime = raw
		if (chosen || saved.Roles != nil || retarget) && len(template.Subagents) > 0 {
			workers := make(map[string]subagent.Template, len(template.Subagents))
			for worker, t := range template.Subagents {
				workers[worker] = t
				workerChoice := saved.choice(worker)
				id := workerChoice.Provider
				if id == "" {
					id = builtinGPTProvider(owner)
				}
				if buildfeatures.Disabled(id) {
					return template, disabledChildError()
				}
				workerBackend, err := c.ProviderRuntime(id)
				if err != nil {
					continue // report availability when this worker is requested
				}
				var identity agentrunner.RuntimeIdentity
				if json.Unmarshal(t.Runtime, &identity) != nil {
					return fail("invalid_template")
				}
				workerModel, workerEffort := identity.Provider.Model.ID, identity.ReasoningEffort.Normalize()
				if workerChoice.Model != "" {
					workerModel = workerChoice.Model
				}
				if id == opencodego.ProviderID || workerChoice.Model != "" || workerChoice.Effort != "" || workerChoice.Fast {
					catalog, err := c.Catalog(ctx, id)
					if err != nil || !catalog.Available {
						continue
					}
					m, err := catalog.FindSelection(workerModel)
					if err != nil {
						return fail("invalid_model")
					}
					if id == opencodego.ProviderID {
						workerEffort = subagentEffort(worker, m)
					} else if workerChoice.Effort == "" && !m.AllowsEffort(workerEffort) {
						workerEffort = m.DefaultEffort
					}
					if workerChoice.Effort != "" {
						workerEffort = workerChoice.Effort
					}
					if !m.AllowsEffort(workerEffort) {
						return fail("unsupported_effort")
					}
				}
				choice := sessionstore.RuntimeSelection{Model: workerModel, Effort: workerEffort, Fast: workerChoice.Fast}
				if t.Runtime, e = boundChildRuntime(c, t.Runtime, workerBackend, choice, false, ""); e != nil {
					return template, e
				}
				workers[worker] = t
			}
			template.Subagents = workers
			if saved.Roles != nil {
				var collectorRuntime, leadRuntime agentrunner.RuntimeIdentity
				if t, ok := workers["collector"]; ok && json.Unmarshal(t.Runtime, &collectorRuntime) == nil && json.Unmarshal(template.Runtime, &leadRuntime) == nil {
					leadRuntime.SystemPrompt = subagentCollectorPrompt(leadRuntime.SystemPrompt, collectorRuntime)
					template.Runtime, e = json.Marshal(leadRuntime)
					if e != nil {
						return template, e
					}
				}
			}
		}
		return template, nil
	}
}

// selectChildModel resolves only a model-only request across connections.
// An explicit or saved provider is checked directly.
func selectChildModel(ctx context.Context, c agentrunner.RuntimeConfig, choice *sessionstore.RuntimeSelection, requested *subagent.RuntimeRequest, active *sessionstore.RuntimeSelection) (agentrunner.ProviderRuntime, modelcatalog.Model, error) {
	originalProvider := choice.Provider
	if requested != nil {
		if requested.Provider != "" {
			choice.Provider = requested.Provider
		}
		if requested.Model != "" {
			choice.Model = requested.Model
			if requested.Provider == "" {
				id, model, err := childProviderForModel(ctx, c, choice.Model)
				if err != nil {
					return agentrunner.ProviderRuntime{}, model, err
				}
				backend, err := c.ProviderRuntime(id)
				if err != nil {
					return backend, model, err
				}
				if id != originalProvider {
					if backend.Provider.Auth.Method == credential.APIKey && id != opencodego.ProviderID {
						return backend, model, &subagent.RuntimeError{Code: "provider_required", Hint: fmt.Sprintf("select the API connection with --provider %s (SubagentStart provider)", id)}
					}
					choice.Provider, choice.Effort, choice.Fast = id, model.DefaultEffort, false
				}
				return backend, model, nil
			}
		}
	}
	backend, catalog, err := childProviderCatalog(ctx, c, choice.Provider)
	if err != nil {
		return backend, modelcatalog.Model{}, err
	}
	if requested != nil && choice.Provider != originalProvider && requested.Model == "" {
		choice.Model = backend.Provider.Model.ID
	}
	model, err := catalog.FindSelection(choice.Model)
	if err != nil && (requested == nil || requested.Model == "") && !catalog.Authoritative &&
		choice.Provider == active.Provider && choice.Model == active.Model && choice.Effort == active.Effort {
		model.ID, model.Name, model.Efforts = active.Model, active.Name, []llm.ReasoningEffort{active.Effort}
		err = nil
	}
	if err != nil {
		return backend, model, &subagent.RuntimeError{Code: "invalid_model", Hint: fmt.Sprintf("model %q is not in %s's catalog", choice.Model, choice.Provider)}
	}
	if choice.Provider != originalProvider {
		choice.Effort, choice.Fast = model.DefaultEffort, false
	}
	return backend, model, nil
}

func childProviderCatalog(ctx context.Context, c agentrunner.RuntimeConfig, id string) (agentrunner.ProviderRuntime, modelcatalog.Catalog, error) {
	backend, err := c.ProviderRuntime(id)
	if buildfeatures.Disabled(id) {
		return backend, modelcatalog.Catalog{}, disabledChildError()
	}
	if err != nil || c.CheckProvider(ctx, id) != nil {
		return backend, modelcatalog.Catalog{}, &subagent.RuntimeError{Code: "provider_unavailable", Hint: providerGuidance(id, modelcatalog.AuthRequired.ProviderHint(id), "。ログイン後は /model refresh を実行してください")}
	}
	catalog, err := c.Catalog(ctx, id)
	if err != nil || !catalog.Available {
		return backend, catalog, &subagent.RuntimeError{Code: "provider_unavailable", Hint: providerGuidance(id, modelcatalog.CatalogUnavailable.ProviderHint(id), "。/login でログイン手順を確認してください")}
	}
	return backend, catalog, nil
}

func childProviderForModel(ctx context.Context, c agentrunner.RuntimeConfig, id string) (string, modelcatalog.Model, error) {
	ids := append(slices.Collect(maps.Keys(c.Backends)), c.Discovered.IDs()...)
	ids = append(ids, c.Identity.Provider.Provider)
	slices.Sort(ids)
	var matches []string
	var model modelcatalog.Model
	for _, providerID := range slices.Compact(ids) {
		if providerID == loginProvider {
			continue
		}
		_, catalog, err := childProviderCatalog(ctx, c, providerID)
		if err != nil {
			continue
		}
		if found, err := catalog.FindSelection(id); err == nil {
			matches = append(matches, providerID)
			model = found
		}
	}
	switch len(matches) {
	case 0:
		return "", model, &subagent.RuntimeError{Code: "invalid_model", Hint: fmt.Sprintf("model %q is not in any available provider catalog", id)}
	case 1:
		return matches[0], model, nil
	default:
		return "", model, &subagent.RuntimeError{Code: "ambiguous_model", Hint: fmt.Sprintf("model %q is offered by %s; specify --provider ID (SubagentStart provider)", id, strings.Join(matches, ", "))}
	}
}

// boundChildRuntime is a template's runtime on a registered backend, model
// and effort; chosen gives a built-in prompt for that model, whose lead names
// the collector's effort.
func boundChildRuntime(c agentrunner.RuntimeConfig, runtime jsontext.Value, backend agentrunner.ProviderRuntime, choice sessionstore.RuntimeSelection, chosen bool, collector llm.ReasoningEffort) (jsontext.Value, error) {
	var identity agentrunner.RuntimeIdentity
	if json.Unmarshal(runtime, &identity) != nil {
		return nil, &subagent.RuntimeError{Code: "invalid_template"}
	}
	identity.Provider = backend.Provider
	// Keep the exact requested/applied selection. A catalog row may match a
	// resolved version as well as its alias; inheritance must not silently
	// replace a pinned version with the alias (or break stable-ID retries).
	identity.Provider.Model.ID = choice.Model
	identity.ReasoningEffort = choice.Effort
	identity.Fast = choice.Fast
	identity.Claude = backend.Claude
	if chosen {
		identity.SystemPrompt = subagentPromptFor(identity.SystemPrompt, backend.Provider.Provider, choice.Model, collector)
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
		var collectorRuntime agentrunner.RuntimeIdentity
		if worker, ok := chosen.Subagents["collector"]; ok {
			var identity agentrunner.RuntimeIdentity
			if json.Unmarshal(worker.Runtime, &identity) == nil {
				collector = identity.ReasoningEffort
				collectorRuntime = identity
			}
		}
		var baseIdentity agentrunner.RuntimeIdentity
		_ = json.Unmarshal(base.Runtime, &baseIdentity)
		if validChildRuntime(c, base.Runtime, chosen.Runtime, collector, subagentCollectorPrompt(baseIdentity.SystemPrompt, collectorRuntime)) != nil {
			return fail()
		}
		base.Runtime = chosen.Runtime
		// Layer is start metadata. Replay preserves the recorded resolution,
		// including legacy plans that predate explicit layer attribution.
		base.Layer = chosen.Layer
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
func validChildRuntime(c agentrunner.RuntimeConfig, base, recorded jsontext.Value, collector llm.ReasoningEffort, prompts ...string) error {
	mismatch := errors.New("child runtime mismatch")
	var a, b agentrunner.RuntimeIdentity
	if json.Unmarshal(base, &a) != nil || json.Unmarshal(recorded, &b) != nil {
		return mismatch
	}
	unchanged, expected := base.Clone(), recorded.Clone()
	if unchanged.Canonicalize() == nil && expected.Canonicalize() == nil && bytes.Equal(unchanged, expected) {
		return nil
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
	expected, _ = json.Marshal(registered)
	actual, _ := json.Marshal(b.Provider)
	if !bytes.Equal(expected, actual) {
		return mismatch
	}
	a.Provider = registered
	a.Claude = backend.Claude
	a.ReasoningEffort = b.ReasoningEffort
	a.Fast = b.Fast
	want := recorded.Clone()
	if want.Canonicalize() != nil {
		return mismatch
	}
	// The template's own prompt, or the built-in one for the sub-agents' model.
	for _, prompt := range append([]string{a.SystemPrompt, subagentPromptFor(a.SystemPrompt, registered.Provider, registered.Model.ID, collector)}, prompts...) {
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
