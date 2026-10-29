package agentrunner

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/harness/tool/viewimage"
	"net/http"
	"os"
	"path/filepath"
	"slices"
)

// RuntimeIdentity is nonsecret and serializable. It captures every prompt/tool
// selection that must remain stable on resume; credentials resolve per request.
type RuntimeIdentity struct {
	Version                 int
	Provider                provider.Selection
	Profile                 profile.Selection
	Workspace, SystemPrompt string
	ReasoningEffort         llm.ReasoningEffort
	DisallowedTools         []string
	LanguageServers         string              `json:",omitempty"`
	DebugAdapters           string              `json:",omitempty"`
	Claude                  *claudeagent.Config `json:",omitzero"`
}
type RuntimeConfig struct {
	// Context is a versioned, disposable projection strategy. It is deliberately
	// outside immutable runtime/auth identity, so old histories need no migration.
	Context contextengine.Config
	// ClaudeSubagents lets the Claude route drive this Host's child templates
	// through Bash (U21); zero when there are none.
	ClaudeSubagents  claudeagent.SubagentCLI
	TaskInput        inbox.ID
	Identity         RuntimeIdentity
	SessionDirectory string
	Providers        *provider.Registry
	Credentials      credential.Resolver
	HTTPClient       *http.Client
	// Borrowed read-only discovery/cache; never part of RuntimeIdentity.
	ModelCatalog func(context.Context) (modelcatalog.Catalog, error)
	Discovered   *DiscoveredBackends
	Backends     map[string]ProviderRuntime
	Catalogs     map[string]func(context.Context) (modelcatalog.Catalog, error)
	// Health performs non-inference availability checks independently of a
	// fallback catalog. Health and credential material are never canonical state.
	Health map[string]func(context.Context) error
	// Capabilities are startup observations, never immutable creation identity.
	// A requested bridge can be blocked while its provider remains text-capable.
	Capabilities map[string]modelcatalog.Capabilities
	// ToolFilter narrows normal bridge exposure to the configured tool policy.
	// It grants nothing; translators/executors still enforce that same policy.
	ToolFilter func(provider, tool string) bool
	// ToolCatalogOnly is used solely by disposable registration composition.
	// It never enters Session identity or a model request.
	ToolCatalogOnly bool
	// ControlTools composes Unreal-owned child controls even for text-only
	// parents. These are never advertised to a text-only model.
	ControlTools bool
	// ProviderProcess authorizes the isolated CLI provider transport for a
	// normal Host or bounded child. It never changes tool execution policy.
	ProviderProcess bool
	// LanguageTools is borrowed from the Host owner, which closes it after sessions drain.
	LanguageTools *LanguageTools
	DebugAdapters []dap.AdapterConfig
	// NewTools optionally composes additional protocol/subagent handlers. It is
	// called per session with that session's owner context and explicit workspace.
	NewTools ToolFactory
}

// NewRuntimeFactory validates/fixes configuration once and builds a fresh
// builder, client, handler set and Operation Manager for each Host-owned session.
func NewRuntimeFactory(c RuntimeConfig) (host.Factory, jsontext.Value, error) {
	if c.Providers == nil {
		return nil, nil, errors.New("runtime: provider registry required")
	}
	if c.Identity.Version != 1 {
		return nil, nil, errors.New("runtime: unsupported identity version")
	}
	workspace, err := filepath.Abs(c.Identity.Workspace)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, errors.New("runtime: workspace is not a directory")
	}
	if c.SessionDirectory == "" {
		return nil, nil, errors.New("runtime: session directory required")
	}
	c.SessionDirectory, err = filepath.Abs(c.SessionDirectory)
	if err != nil {
		return nil, nil, err
	}
	c.Context, err = c.Context.Resolve()
	if err != nil {
		return nil, nil, err
	}
	c.Identity.Workspace = workspace
	c.Identity.DisallowedTools = slices.Clone(c.Identity.DisallowedTools)
	c.Identity.Provider, err = c.Providers.Resolve(c.Identity.Provider)
	if err != nil {
		return nil, nil, err
	}
	supportsTools := c.ProviderSupportsTools(c.Identity.Provider.Provider)
	if c.Providers.RequiresResolver(c.Identity.Provider) && c.Credentials == nil {
		return nil, nil, errors.New("runtime: credential resolver required")
	}
	if !c.Identity.ReasoningEffort.Valid() && !(c.Identity.Provider.Provider == claudeagent.ProviderID && c.Identity.ReasoningEffort == "") {
		return nil, nil, errors.New("runtime: invalid reasoning effort")
	}
	if c.Identity.Provider.Provider == claudeagent.ProviderID && (c.Identity.Claude == nil || !filepath.IsAbs(c.Identity.Claude.Binary)) {
		return nil, nil, errors.New("runtime: Claude executable configuration required")
	}
	toolResources := supportsTools
	for id, p := range c.Backends {
		if id != p.Provider.Provider || id == c.Identity.Provider.Provider {
			return nil, nil, errors.New("runtime: invalid backend registration")
		}
		p.Provider, err = c.Providers.Resolve(p.Provider)
		if err != nil {
			return nil, nil, err
		}
		if _, err = profile.Resolve(c.Identity.Profile, p.Provider); err != nil {
			return nil, nil, err
		}
		if p.Provider.Provider == claudeagent.ProviderID && (p.Claude == nil || !filepath.IsAbs(p.Claude.Binary)) {
			return nil, nil, errors.New("runtime: Claude executable configuration required")
		}
		if !p.ReasoningEffort.Valid() && !(p.Provider.Provider == claudeagent.ProviderID && p.ReasoningEffort == "") {
			return nil, nil, errors.New("runtime: invalid backend effort")
		}
		c.Backends[id] = p
		toolResources = toolResources || c.ProviderSupportsTools(id)
	}
	if !toolResources && !c.ControlTools {
		if c.NewTools != nil || c.LanguageTools != nil && c.LanguageTools.Identity() != "" || len(c.DebugAdapters) > 0 {
			return nil, nil, errors.New("runtime: selected provider is text-only; tools unsupported")
		}
		// Capability selection owns both the model catalog and executable
		// Translators. Permissions remain a separate execution authorization.
		c.NewTools = func(context.Context, ToolConfig) (Tools, error) {
			return Tools{Registry: tool.NewRegistry(tool.StaticTranslators{})}, nil
		}
		c.LanguageTools = nil
	}
	_, err = profile.Resolve(c.Identity.Profile, c.Identity.Provider)
	if err != nil {
		return nil, nil, err
	}
	if c.NewTools == nil {
		c.NewTools = func(ctx context.Context, t ToolConfig) (Tools, error) {
			return DefaultTools(ctx, t, c.Identity.DisallowedTools)
		}
	}
	c.Identity.LanguageServers = ""
	if c.LanguageTools != nil {
		c.NewTools = c.LanguageTools.Wrap(c.NewTools, c.Identity.DisallowedTools)
		c.Identity.LanguageServers = c.LanguageTools.Identity()
	}
	if toolResources {
		c.NewTools, c.Identity.DebugAdapters, err = DebugTools(c.NewTools, c.DebugAdapters, c.Identity.DisallowedTools)
		if err != nil {
			return nil, nil, err
		}
	}
	identity, err := json.Marshal(c.Identity)
	if err != nil {
		return nil, nil, err
	}
	factory := func(ctx context.Context, id session.ID) (host.Runtime, error) {
		base := sessionstore.RuntimeSelection{Version: 1, Provider: c.Identity.Provider.Provider, Model: c.Identity.Provider.Model.ID, Name: c.Identity.Provider.Model.ID, Effort: c.Identity.ReasoningEffort}
		active := base
		if prior := host.ActiveRuntimeSelection(ctx); prior != nil {
			active = *prior
		}
		if (host.IsNewSession(ctx) || host.ActiveRuntimeSelection(ctx) == nil) && active.Revision == 0 {
			if catalog, err := c.Catalog(ctx, base.Provider); err == nil && catalog.Available {
				// Initial selection uses the same CLI-owned metadata as /model.
				// Unknown catalogs retain the conservative fallback; canonical resume
				// selections are never rewritten from newly discovered metadata.
				if model, err := catalog.FindSelection(base.Model); err == nil && model.ContextWindow > 0 && model.ContextWindow <= 16<<20 {
					base.ContextWindow = model.ContextWindow
					active = base
				}
			}
		}
		// Results carries what Claude Code reported for its own tool calls to the
		// claude_code receipts of this Session.
		results := observed.NewResults()
		initialClient, err := selectionClient(c, active, workspace, results)
		if err != nil {
			return host.Runtime{}, err
		}
		client := &selectedAdapter{client: initialClient}
		operationDirectory := filepath.Join(c.SessionDirectory, "operations", string(id))
		if err = os.MkdirAll(operationDirectory, 0700); err != nil {
			client.Close()
			return host.Runtime{}, err
		}
		var skills []tool.Skill
		var skillErrors []error
		if toolResources {
			skills, skillErrors = tool.DiscoverSkills(filepath.Join(workspace, ".harness", "skills"))
		}
		if len(skillErrors) > 0 {
			client.Close()
			return host.Runtime{}, errors.Join(skillErrors...)
		}
		tools := ToolConfig{SessionID: id, Directory: workspace, Getenv: os.Getenv, CatalogOnly: c.ToolCatalogOnly}
		if toolResources {
			tools.Names = []string{tool.BashName, tool.ViewImageName, tool.ClaudeCodeName}
			if len(skills) > 0 {
				tools.Names = append(tools.Names, tool.SkillUseName)
			}
			tools.Translators = tool.StaticTranslators{Bash: bash.New(bash.Config{Shell: "/bin/sh", Directory: workspace, BaseDirectory: operationDirectory}), ViewImage: viewimage.New(viewimage.Config{Directory: workspace}), ClaudeCode: observed.New(results)}
		}
		configured, err := c.NewTools(ctx, tools)
		if err != nil {
			client.Close()
			return host.Runtime{}, err
		}
		if configured.Registry == nil {
			if configured.Close != nil {
				configured.Close()
			}
			client.Close()
			return host.Runtime{}, errors.New("runtime: tool registry required")
		}
		closeRuntime := func() error {
			var e error
			if configured.Close != nil {
				e = configured.Close()
			}
			return errors.Join(e, client.Close())
		}
		if _, enabled := configured.Registry.Resolve(tool.SkillUseName); enabled {
			for _, skill := range skills {
				if _, err = configured.Registry.RegisterSkill(skill); err != nil {
					closeRuntime()
					return host.Runtime{}, err
				}
			}
		}

		builder := contextbuilder.NewBuilder()
		constraints := contextengine.Constraints{ToolPolicy: "enforced-by-Unreal", Filesystem: "enforced-by-Unreal", Network: "enforced-by-Unreal", Process: "denied"}
		policy := permission.FromContext(ctx)
		for _, d := range configured.Registry.StaticDefinitions() {
			if policy.CheckTool(d.Tool.Name) == nil {
				constraints.AllowedTools = append(constraints.AllowedTools, d.Tool.Name)
			}
		}
		slices.Sort(constraints.AllowedTools)
		if policy.CheckProcess() == nil {
			constraints.Process = "authorized-transport-or-process"
		}
		engine, e := contextbuilder.EnableCompaction(builder, c.Context, constraints)
		if e != nil {
			closeRuntime()
			return host.Runtime{}, e
		}
		registry := &selectedTools{Registry: configured.Registry}
		refreshPermission := func(context.Context) error {
			updated := constraints
			prompt := ""
			if controller := permission.ControllerFromContext(ctx); controller != nil {
				snapshot := controller.Snapshot()
				mode := snapshot.Mode
				prompt = "Unreal permission mode: " + mode.Label() + ". Tool availability is not authorization. Unreal owns human approval per Operation; never claim to approve or change permissions. Denials are observations. Full Access Bash is ambient access, not an OS sandbox."
				updated.ToolPolicy = "human-controlled-by-Unreal: " + string(mode)
				updated.Process = "denied until human authorization; ambient access, no OS sandbox"
				if mode == permission.FullAccess {
					updated.Process = "human-approved ambient access; operator maximum applies"
				}
				updated.AllowedTools = nil
				for _, d := range registry.StaticDefinitions() {
					if policy.CheckTool(d.Tool.Name) == nil {
						updated.AllowedTools = append(updated.AllowedTools, d.Tool.Name)
					}
				}
				slices.Sort(updated.AllowedTools)
			}
			builder.(interface{ SetPermissionPrompt(string) }).SetPermissionPrompt(prompt)
			builder.(interface {
				SetContextConstraints(contextengine.Constraints)
			}).SetContextConstraints(updated)
			return nil
		}
		configure := func(choice sessionstore.RuntimeSelection) error {
			backend, e := c.ProviderRuntime(choice.Provider)
			if e != nil {
				return e
			}
			selected := backend.Provider
			selected.Model.ID = choice.Model
			composed, e := profile.Resolve(c.Identity.Profile, selected)
			if e != nil {
				return e
			}
			enabled := c.ProviderSupportsTools(choice.Provider)
			allowed := func(name string) bool {
				// Claude Code runs its own tools: the Claude route only records
				// them and hands refused commands to Bash (U21, U22). No other
				// route sees the claude_code record tool.
				if choice.Provider == claudeagent.ProviderID {
					if name == tool.ClaudeCodeName {
						return true
					}
					if name != tool.BashName {
						return false
					}
				} else if name == tool.ClaudeCodeName {
					return false
				}
				return c.ToolFilter == nil || c.ToolFilter(choice.Provider, name)
			}
			registry.set(enabled, allowed)
			var definitions []llm.Tool
			if enabled {
				for _, d := range registry.StaticDefinitions() {
					definitions = append(definitions, d.Tool)
				}
			}
			prompt, definitions := composed.Compose(c.Identity.SystemPrompt, definitions)
			builder.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(choice.Provider)
			b := builder.(interface {
				ConfigureRuntime(llm.Model, string, []llm.Tool, []tool.Skill, bool, bool)
			})
			b.ConfigureRuntime(llm.Model{ID: choice.Model, ReasoningEffort: choice.Effort}, prompt, definitions, registry.Skills(), !enabled, len(c.Backends) > 0 || c.Discovered != nil || !enabled)
			builder.(interface {
				SetContextRuntime(sessionstore.RuntimeSelection, bool)
			}).SetContextRuntime(choice, enabled)
			return refreshPermission(ctx)
		}
		if err = configure(active); err != nil {
			closeRuntime()
			return host.Runtime{}, err
		}
		validate := func(choice sessionstore.RuntimeSelection) error {
			if choice.Provider != base.Provider && choice.Binding == "" {
				return errors.New("runtime provider binding required")
			}
			_, e := c.BindSelection(choice)
			return e
		}
		apply := func(choice sessionstore.RuntimeSelection) error {
			if err := validate(choice); err != nil {
				return err
			}
			if permission.ControllerFromContext(ctx) != nil && choice == active {
				if err := configure(choice); err != nil {
					return err
				}
			}
			if choice != active {
				next, e := selectionClient(c, choice, workspace, results)
				if e != nil {
					return e
				}
				if e = configure(choice); e != nil {
					next.Close()
					return e
				}
				client.mu.Lock()
				old := client.client
				client.client = next
				client.mu.Unlock()
				active = choice
				if e = old.Close(); e != nil {
					return e
				}
			}
			builder.SetModel(llm.Model{ID: choice.Model, ReasoningEffort: choice.Effort})
			return nil
		}
		return host.Runtime{InterruptedInput: configured.InterruptedInput, ContextEngine: engine, Selection: &base, ValidateSelection: validate, ApplySelection: apply, BeforeGeneration: refreshPermission, Builder: builder, LLM: client, Tools: registry, Operations: operation.NewLocalOperationManager(ctx, configured.RemoteJobs...), Close: closeRuntime}, nil

	}
	return factory, identity, nil
}
