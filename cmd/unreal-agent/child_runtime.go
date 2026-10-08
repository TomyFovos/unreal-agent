package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"os/exec"
	"path/filepath"
)

func childRuntimeResolver(c agentrunner.RuntimeConfig) subagent.RuntimeResolver {
	return func(ctx context.Context, owner *host.Session, template subagent.Template, requested *subagent.RuntimeRequest) (subagent.Template, error) {
		ctx = owner.WithExecutionPolicy(ctx)
		fail := func(code string) (subagent.Template, error) { return template, &subagent.RuntimeError{Code: code} }
		if e := permission.FromContext(ctx).CheckTool("SubagentStart"); e != nil {
			return fail("permission_denied")
		}
		active, _ := owner.RuntimeSelection()
		if active == nil {
			return fail("selection_unavailable")
		}
		choice := *active
		if requested != nil {
			choice = sessionstore.RuntimeSelection{Version: 1, Provider: requested.Provider, Model: requested.Model, Effort: requested.Effort}
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
			return fail("provider_unavailable")
		}
		catalog, e := c.Catalog(ctx, choice.Provider)
		if e != nil {
			return fail("provider_unavailable")
		}
		model, e := catalog.FindSelection(choice.Model)
		if e != nil && !catalog.Authoritative && choice.Provider == active.Provider && choice.Model == active.Model && choice.Effort == active.Effort {
			model.ID, model.Name, model.Efforts = active.Model, active.Name, []llm.ReasoningEffort{active.Effort}
			e = nil
		}
		if e != nil {
			return fail("invalid_model")
		}
		if !model.AllowsEffort(choice.Effort) {
			return fail("unsupported_effort")
		}
		var identity agentrunner.RuntimeIdentity
		if json.Unmarshal(template.Runtime, &identity) != nil {
			return fail("invalid_template")
		}
		identity.Provider = backend.Provider
		// Keep the exact requested/applied selection. A catalog row may match a
		// resolved version as well as its alias; inheritance must not silently
		// replace a pinned version with the alias (or break stable-ID retries).
		identity.Provider.Model.ID = choice.Model
		identity.ReasoningEffort = choice.Effort
		identity.ClaudeCode = backend.ClaudeCode
		if identity.ClaudeCode != nil {
			copy := *identity.ClaudeCode
			name := copy.Binary
			if name == "" {
				name = "claude"
			}
			path, e := exec.LookPath(name)
			if e != nil {
				return fail("provider_unavailable")
			}
			copy.Binary, _ = filepath.Abs(path)
			identity.ClaudeCode = &copy
		}
		registry, e := c.Providers.WithModel(identity.Provider)
		if e != nil {
			return fail("invalid_model")
		}
		_, raw, e := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: identity, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: c.Credentials})
		if e != nil {
			return fail("incompatible_runtime")
		}
		template.Runtime = raw
		check := subagent.ChildConfig{Version: 1, ParentID: "validation", OperationID: "validation", ChildID: subagent.ChildID("validation", "validation"), ReadyID: "ready:validation", Task: "validation", SessionDirectory: c.SessionDirectory, Workspace: template.Workspace, Runtime: raw, Policy: template.Policy}
		if choice.Provider == "claude-code" {
			check.TextOnly = !sessionstore.ToolBridgeEnabledFromConfiguration(raw)
			check.ProviderProcess = true
		}
		if check.Validate() != nil {
			return fail("incompatible_capability")
		}
		return template, nil
	}
}

// Recovery validates the recorded configuration, never re-inherits the parent's
// current selection or replaces the child's catalog choice.
func childTemplateValidator(c agentrunner.RuntimeConfig) func(subagent.Template, subagent.Template) error {
	return func(base, chosen subagent.Template) error {
		fail := func() error { return &subagent.RuntimeError{Code: "configuration_mismatch"} }
		var a, b agentrunner.RuntimeIdentity
		if json.Unmarshal(base.Runtime, &a) != nil || json.Unmarshal(chosen.Runtime, &b) != nil {
			return fail()
		}
		backend, e := c.ProviderRuntime(b.Provider.Provider)
		if e != nil {
			return fail()
		}
		registered := backend.Provider
		registered.Model.ID = b.Provider.Model.ID
		registry, e := c.Providers.WithModel(registered)
		if e != nil {
			return fail()
		}
		registered, e = registry.Resolve(registered)
		if e != nil {
			return fail()
		}
		expected, _ := json.Marshal(registered)
		actual, _ := json.Marshal(b.Provider)
		if !bytes.Equal(expected, actual) {
			return fail()
		}
		cfg := backend.ClaudeCode
		if cfg != nil {
			copy := *cfg
			name := copy.Binary
			if name == "" {
				name = "claude"
			}
			path, e := exec.LookPath(name)
			if e != nil {
				return fail()
			}
			copy.Binary, _ = filepath.Abs(path)
			cfg = &copy
		}
		a.Provider = registered
		a.ClaudeCode = cfg
		a.ReasoningEffort = b.ReasoningEffort
		// The same factory normalization used at creation fills catalog display
		// names and resolved operator defaults. It performs no auth/inference and
		// cannot add a permission or replace the recorded model on recovery.
		_, expectedRuntime, e := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: a, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: c.Credentials})
		recorded := chosen.Runtime.Clone()
		if e != nil || expectedRuntime.Canonicalize() != nil || recorded.Canonicalize() != nil || !bytes.Equal(expectedRuntime, recorded) {
			return fail()
		}
		base.Runtime = chosen.Runtime
		old, _ := json.Marshal(base)
		now, _ := json.Marshal(chosen)
		if !bytes.Equal(old, now) {
			return fail()
		}
		return nil
	}
}
