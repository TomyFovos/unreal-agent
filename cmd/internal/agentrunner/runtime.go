package agentrunner

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
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
	LanguageServers         string `json:",omitempty"`
	DebugAdapters           string `json:",omitempty"`
}
type RuntimeConfig struct {
	Identity         RuntimeIdentity
	SessionDirectory string
	Providers        *provider.Registry
	Credentials      credential.Resolver
	HTTPClient       *http.Client
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
	c.Identity.Workspace = workspace
	c.Identity.DisallowedTools = slices.Clone(c.Identity.DisallowedTools)
	c.Identity.Provider, err = c.Providers.Resolve(c.Identity.Provider)
	if err != nil {
		return nil, nil, err
	}
	if c.Identity.Provider.Auth.Method != credential.None && c.Credentials == nil {
		return nil, nil, errors.New("runtime: credential resolver required")
	}
	if !c.Identity.ReasoningEffort.Valid() {
		return nil, nil, errors.New("runtime: invalid reasoning effort")
	}
	resolved, err := profile.Resolve(c.Identity.Profile, c.Identity.Provider)
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
	c.NewTools, c.Identity.DebugAdapters, err = DebugTools(c.NewTools, c.DebugAdapters, c.Identity.DisallowedTools)
	if err != nil {
		return nil, nil, err
	}
	identity, err := json.Marshal(c.Identity)
	if err != nil {
		return nil, nil, err
	}
	factory := func(ctx context.Context, id session.ID) (host.Runtime, error) {
		client, _, err := c.Providers.Build(provider.BuildConfig{Selection: c.Identity.Provider, Resolver: c.Credentials, HTTPClient: c.HTTPClient})
		if err != nil {
			return host.Runtime{}, err
		}
		operationDirectory := filepath.Join(c.SessionDirectory, "operations", string(id))
		if err = os.MkdirAll(operationDirectory, 0700); err != nil {
			client.Close()
			return host.Runtime{}, err
		}
		skills, skillErrors := tool.DiscoverSkills(filepath.Join(workspace, ".harness", "skills"))
		if len(skillErrors) > 0 {
			client.Close()
			return host.Runtime{}, errors.Join(skillErrors...)
		}
		names := []string{tool.BashName, tool.ViewImageName}
		if len(skills) > 0 {
			names = append(names, tool.SkillUseName)
		}
		configured, err := c.NewTools(ctx, ToolConfig{SessionID: id, Directory: workspace, Names: names, Getenv: os.Getenv, Translators: tool.StaticTranslators{Bash: bash.New(bash.Config{Shell: "/bin/sh", Directory: workspace, BaseDirectory: operationDirectory}), ViewImage: viewimage.New(viewimage.Config{Directory: workspace})}})
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
		builder := contextbuilder.NewBuilder(configured.Registry.Skills()...)
		builder.SetModel(llm.Model{ID: c.Identity.Provider.Model.ID, ReasoningEffort: c.Identity.ReasoningEffort})
		var definitions []llm.Tool
		for _, definition := range configured.Registry.StaticDefinitions() {
			definitions = append(definitions, definition.Tool)
		}
		prompt, definitions := resolved.Compose(c.Identity.SystemPrompt, definitions)
		builder.SetSystemPrompt(prompt)
		for _, definition := range definitions {
			builder.AddTool(definition)
		}
		return host.Runtime{Builder: builder, LLM: client, Tools: configured.Registry, Operations: operation.NewLocalOperationManager(ctx, configured.RemoteJobs...), Close: closeRuntime}, nil
	}
	return factory, identity, nil
}
