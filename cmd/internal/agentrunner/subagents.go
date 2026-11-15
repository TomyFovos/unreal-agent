package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	subtool "github.com/unreallabsai/unreal-agent/harness/tool/subagent"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ChildApprovalTimeout keeps requests pending until the parent answers (U17).
const ChildApprovalTimeout = 1000 * time.Hour

// WithSubagentTools binds the existing child manager to its owning Host Session.
func WithSubagentTools(base ToolFactory, configuration subagent.Config, disabled []string) ToolFactory {
	disabled = slices.Clone(disabled)
	if base == nil {
		base = func(ctx context.Context, c ToolConfig) (Tools, error) {
			return DefaultTools(ctx, c, disabled)
		}
	}
	return func(ctx context.Context, c ToolConfig) (Tools, error) {
		owner, ok := host.SessionFromContext(ctx)
		if !ok && !c.CatalogOnly {
			return Tools{}, errors.New("subagent: Host owner context required")
		}
		configured, err := base(ctx, c)
		if err != nil {
			return Tools{}, err
		}
		closeBase := configured.Close
		fail := func(err error) (Tools, error) {
			if closeBase != nil {
				err = errors.Join(err, closeBase())
			}
			return Tools{}, err
		}
		extensions, err := subtool.Extensions(c.SessionID, configuration.Templates, configuration.Child != nil)
		if err != nil {
			return fail(err)
		}
		subtool.BindRuntime(extensions, ctx, owner, configuration.ResolveRuntime)
		for i := range extensions {
			name := extensions[i].Definition.Tool.Name
			if slices.Contains(disabled, name) || (name == "SubagentStart" && len(configuration.Templates) == 0) {
				extensions[i].Enabled = false
			}
		}
		configured.Registry, err = tool.WithExtensions(configured.Registry, extensions)
		if err != nil {
			return fail(err)
		}
		if c.CatalogOnly {
			return configured, nil // same schemas, no owner or child manager
		}
		execution := configuration
		execution.Owner = owner
		manager, err := subagent.NewManager(ctx, execution)
		if err != nil {
			return fail(err)
		}
		configured.RemoteJobs = append(configured.RemoteJobs, manager)
		configured.InterruptedInput = manager.InterruptedInput
		configured.CompletedInput = manager.CompletedInput
		configured.Close = func() error {
			var err error
			err = manager.Close()
			if closeBase != nil {
				err = errors.Join(err, closeBase())
			}
			return err
		}
		return configured, nil
	}
}

// NewChildFactory shares child ownership, recovery and shutdown with the TUI Host.
func NewChildFactory(base RuntimeConfig, configure func(subagent.ChildConfig, *RuntimeConfig) error) subagent.ChildFactory {
	return func(ctx context.Context, c subagent.ChildConfig, link subagent.ChildLink) (*host.Session, io.Closer, error) {
		var identity RuntimeIdentity
		if err := json.Unmarshal(c.Runtime, &identity, json.RejectUnknownMembers(true)); err != nil {
			return nil, nil, errors.New("child runtime configuration is invalid")
		}
		if !filepath.IsAbs(identity.Workspace) || filepath.Clean(identity.Workspace) != filepath.Clean(c.Workspace) {
			return nil, nil, errors.New("child runtime workspace mismatch")
		}
		if slices.Contains(identity.DisallowedTools, "Finish") {
			return nil, nil, errors.New("child runtime disables Finish")
		}
		registry := base.Providers
		var err error
		if registry == nil {
			registry, err = provider.New(provider.Defaults(map[string][]provider.Model{identity.Provider.Provider: {identity.Provider.Model}})...)
		}
		if err != nil {
			return nil, nil, err
		}
		var tools ToolFactory
		if !c.TextOnly {
			execution := subagent.Config{Child: &c, SendParent: link.Send}
			if len(c.Subagents) > 0 {
				// A lead starts its workers with this same child process command.
				binary, err := os.Executable()
				if err != nil {
					return nil, nil, err
				}
				execution.Directory, execution.Binary, execution.Arguments = c.SessionDirectory, binary, os.Args[1:]
				execution.Environment, execution.Templates = os.Environ(), c.Subagents
			}
			tools = WithSubagentTools(func(ctx context.Context, tools ToolConfig) (Tools, error) {
				tools.MutationStateDirectory = c.MutationStateDirectory
				return DefaultTools(ctx, tools, identity.DisallowedTools)
			}, execution, identity.DisallowedTools)
		}
		runtime := base
		runtime.Identity, runtime.SessionDirectory, runtime.Providers = identity, c.SessionDirectory, registry
		runtime.NewTools, runtime.ProviderProcess, runtime.TaskInput = tools, c.ProviderProcess, c.InitialInput().ID
		if configure != nil {
			if err := configure(c, &runtime); err != nil {
				return nil, nil, err
			}
		}
		factory, resolved, err := NewRuntimeFactory(runtime)
		if err != nil {
			return nil, nil, err
		}
		old := c.Runtime.Clone()
		if err = old.Canonicalize(); err != nil {
			return nil, nil, err
		}
		if err = resolved.Canonicalize(); err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(old, resolved) {
			return nil, nil, errors.New("child runtime resolution differs from recorded configuration")
		}
		// A parent with permission modes makes this child follow its mode: the
		// template's Policy is the ceiling, its Baseline what Read Only and
		// Ask allow without approval. Otherwise Policy is fixed.
		hostConfig := host.Config{Directory: c.SessionDirectory, Build: factory}
		baseline, maximum := c.Policy, (*permission.Policy)(nil)
		if link.Mode != "" {
			if c.Baseline != nil {
				baseline = *c.Baseline
			}
			if maximum, err = permission.New(c.Policy); err != nil {
				return nil, nil, err
			}
			hostConfig.PermissionMaximum, hostConfig.ApprovalTimeout = maximum, ChildApprovalTimeout
		}
		policy, err := permission.New(baseline)
		if err != nil {
			if maximum != nil {
				maximum.Close()
			}
			return nil, nil, err
		}
		owner, err := host.New(ctx, hostConfig)
		if err != nil {
			policy.Close()
			if maximum != nil {
				maximum.Close()
			}
			return nil, nil, err
		}
		closeOwner := childCloseFunc(func() error {
			err := errors.Join(owner.Close(), policy.Close())
			if maximum != nil {
				err = errors.Join(err, maximum.Close())
			}
			return err
		})
		configuration, err := json.Marshal(c)
		if err != nil {
			closeOwner.Close()
			return nil, nil, err
		}
		snapshot := projectinstructions.None()
		if c.ProjectInstructions != nil {
			snapshot = *c.ProjectInstructions
		}
		current, err := owner.Open(ctx, host.Options{TextOnlyChild: c.TextOnly, ProjectInstructions: &snapshot, Workspace: c.Workspace, ID: c.ChildID, Lifecycle: "child", Policy: policy, Configuration: configuration, Initial: []inbox.Input{c.InitialInput()}, Heartbeat: 0, PermissionMode: link.Mode})
		if err != nil {
			closeOwner.Close()
			return nil, nil, err
		}
		return current, closeOwner, nil
	}
}

type childCloseFunc func() error

func (f childCloseFunc) Close() error { return f() }
