package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/privateexport"
)

// Normal discovery adds registrations, never replaces Runtime or permissions.
type normalLauncherConfig struct {
	Version           int
	DiscoverProviders *bool `json:",omitzero"`
	// ClaudeBinary is an explicit Claude Code executable (--bootstrap-claude-binary).
	ClaudeBinary string `json:",omitzero"`
	// Native auto detection validates routing and the source; environment labels
	// or proxy strings alone cannot authorize Docker or credential fallback.
	CodexDisabled           bool `json:",omitzero"`
	ClaudeDisabled          bool `json:",omitzero"`
	CodexDockerSandboxOAuth bool `json:",omitzero"`
	CodexAuto               bool `json:",omitzero"`
}

type normalRuntimeError struct {
	Code      string
	Providers []modelcatalog.Provider
}

func (e *normalRuntimeError) Error() string {
	if len(e.Providers) > 0 {
		text := "no external provider is available"
		for _, p := range e.Providers {
			text += "; " + p.ID + ": " + string(p.Reason) + " (" + p.Reason.Hint() + ")"
		}
		return text
	}
	switch e.Code {
	case "providers_unavailable":
		return "no external provider is available; sign in with codex login, or run claude and /login"
	case "initial_provider_unavailable":
		return "configured initial provider is unavailable; no automatic provider fallback"
	case "catalog_unavailable":
		return "initial model catalog is unavailable; refresh the external CLI catalog or configure an explicit runtime/model"
	default:
		return "normal runtime: " + e.Code
	}
}

func discoverCodex(ctx context.Context, option string, getenv func(string) string) (agentrunner.ProviderRuntime, error) {
	return discoverCodexSource(ctx, option, getenv, false)
}

func discoverCodexSource(ctx context.Context, option string, getenv func(string) string, sandbox bool) (agentrunner.ProviderRuntime, error) {
	path, err := externalCodexAuthFile(option, getenv)
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	ref := credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}
	var resolver credential.Resolver = provider.ExternalCodex(openaicodex.Config{AuthFile: path})
	if sandbox {
		ref = credential.Reference{Provider: "openai-codex", Method: credential.DockerSandboxOAuth, ID: "docker-sandbox-oauth"}
		resolver = provider.DockerSandboxCodex(openaicodex.DockerSandboxSource{AuthFile: path, Getenv: getenv})
	} else {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return agentrunner.ProviderRuntime{}, &credential.Error{Code: "external_reauth_required"}
		}
	}
	// Resolve with the existing external owner, then discard the material. It is
	// never imported, copied to config, logged or retained by discovery.
	if _, err = resolver.Resolve(ctx, ref); err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	if sandbox {
		if _, err = openaicodex.DockerSandboxProxyURL(getenv); err != nil {
			return agentrunner.ProviderRuntime{}, err
		}
	}
	catalog, _ := codexCatalogReader(path, sandbox, getenv)(ctx, false)
	model, err := initialCatalogModel(catalog, "")
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	return agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: "openai-codex", Model: provider.Model{ID: model.ID}, Endpoint: openaicodex.BaseURL, Auth: ref, MaxAttempts: 2, Source: "external CLI catalog / local bootstrap"}, ReasoningEffort: model.DefaultEffort}, nil
}

func discoverCodexAuto(ctx context.Context, option string, getenv func(string) string) (agentrunner.ProviderRuntime, error) {
	path, err := externalCodexAuthFile(option, getenv)
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	sandbox, err := (openaicodex.DockerSandboxSource{AuthFile: path, Getenv: getenv}).DetectRoute()
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	return discoverCodexSource(ctx, option, getenv, sandbox)
}

func initialCatalogModel(c modelcatalog.Catalog, id string) (modelcatalog.Model, error) {
	for _, m := range c.Models {
		if id != "" && !m.Matches(id) {
			continue
		}
		if m.SupportsEffort != nil && !*m.SupportsEffort {
			m.DefaultEffort = ""
			return m, nil
		}
		if m.AllowsEffort(m.DefaultEffort) {
			return m, nil
		}
		if len(m.Efforts) > 0 {
			m.DefaultEffort = m.Efforts[0]
			return m, nil
		}
	}
	return modelcatalog.Model{}, &normalRuntimeError{Code: "catalog_unavailable"}
}

func registerNormalProviders(ctx context.Context, config serveConfiguration, codexFile string, getenv func(string) string) (serveConfiguration, error) {
	discover := true
	if config.Launcher != nil {
		if config.Launcher.Version != 1 {
			return config, &normalRuntimeError{Code: "unsupported_launcher_version"}
		}
		if config.Launcher.CodexAuto && (config.Launcher.CodexDockerSandboxOAuth || config.Launcher.CodexDisabled) {
			return config, &normalRuntimeError{Code: "conflicting_codex_route"}
		}
		if config.Launcher.DiscoverProviders != nil && !*config.Launcher.DiscoverProviders {
			discover = false
		}
	}
	config.Providers = cloneBackends(config.Providers)
	config.providerStatuses = nil

	if !discover {
		return config, nil
	}
	// Fixed traversal only controls registration order, never initial selection.
	// Claude is the Claude Code route (U21); the older claude-code route is
	// no longer discovered.
	for _, id := range []string{"openai-codex", claudeagent.ProviderID} {
		if config.Runtime.Provider.Provider == id || config.Providers[id].Provider.Provider != "" {
			config.providerStatuses = append(config.providerStatuses, modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: "available"})
			continue
		}
		if config.Launcher != nil && (id == "openai-codex" && config.Launcher.CodexDisabled || id == claudeagent.ProviderID && config.Launcher.ClaudeDisabled) {
			config.providerStatuses = append(config.providerStatuses, unavailableProvider(id, modelcatalog.ProviderDisabled))
			continue
		}
		var p agentrunner.ProviderRuntime
		var err error
		if id == "openai-codex" {
			if config.Launcher != nil && config.Launcher.CodexAuto {
				p, err = discoverCodexAuto(ctx, codexFile, getenv)
			} else {
				p, err = discoverCodexSource(ctx, codexFile, getenv, config.Launcher != nil && config.Launcher.CodexDockerSandboxOAuth)
			}
		} else {
			p, err = discoverClaudeAgent(ctx, config, getenv)
		}
		if ctx.Err() != nil {
			return config, ctx.Err()
		}
		if err == nil {
			config.Providers[id] = p
			config.providerStatuses = append(config.providerStatuses, modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: "available"})
		} else {
			config.providerStatuses = append(config.providerStatuses, unavailableProvider(id, providerFailureReason(err, id == "openai-codex" && config.Launcher != nil && config.Launcher.CodexDockerSandboxOAuth)))
		}
	}
	return config, nil
}

func cloneBackends(in map[string]agentrunner.ProviderRuntime) map[string]agentrunner.ProviderRuntime {
	out := make(map[string]agentrunner.ProviderRuntime, len(in))
	for id, p := range in {
		out[id] = p
	}
	return out
}

type initialProviderChooser func([]string) (string, error)

// chooseInitialProvider starts a new instance with Claude, the designer (U25),
// when it is available. /model switches at any time, so nothing is asked (U8).
func chooseInitialProvider(out io.Writer) initialProviderChooser {
	return func(ids []string) (string, error) {
		id := ids[0]
		if slices.Contains(ids, claudeagent.ProviderID) {
			id = claudeagent.ProviderID
		}
		fmt.Fprintf(out, "Starting with %s; /model switches provider, model and effort.\n", modelcatalog.ProviderName(id))
		return id, nil
	}
}

func bootstrapRuntime(ctx context.Context, o launchOptions, choose initialProviderChooser, getenv func(string) string) (serveConfiguration, error) {
	config := bootstrapDiscovery(o)
	config, err := registerNormalProviders(ctx, config, o.codexFile, getenv)
	if err != nil {
		return config, err
	}
	ids := make([]string, 0, len(config.Providers))
	for id := range config.Providers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		for _, status := range config.providerStatuses {
			if status.Reason == modelcatalog.CatalogUnavailable {
				return config, &normalRuntimeError{Code: "catalog_unavailable", Providers: config.providerStatuses}
			}
		}
		return config, &normalRuntimeError{Code: "providers_unavailable", Providers: config.providerStatuses}
	}
	id := getenv("UNREAL_HARNESS_LLM_PROVIDER")
	if id == "" {
		if len(ids) == 1 {
			id = ids[0]
		} else {
			id, err = choose(ids)
			if err != nil {
				return config, err
			}
		}
	}
	p, ok := config.Providers[id]
	if !ok {
		return config, &normalRuntimeError{Code: "initial_provider_unavailable"}
	}
	if requested := getenv("UNREAL_HARNESS_LLM_MODEL"); requested != "" {
		var catalog modelcatalog.Catalog
		if id == claudeagent.ProviderID {
			catalog, _, err = claudeagent.Discover(ctx, p.Claude.Binary, nil)
		} else if id == "openai-codex" {
			path, _ := externalCodexAuthFile(o.codexFile, getenv)
			catalog, err = codexCatalogReader(path, isDockerSandboxCodex(p.Provider.Auth), getenv)(ctx, false)
		}
		m, e := initialCatalogModel(catalog, requested)
		if err != nil || e != nil {
			return config, &normalRuntimeError{Code: "catalog_unavailable"}
		}
		p.Provider.Model.ID, p.ReasoningEffort = m.ID, m.DefaultEffort
	}
	workspace, err := os.Getwd()
	if err != nil {
		return config, err
	}
	config.Runtime = agentrunner.RuntimeIdentity{Version: 1, Provider: p.Provider, Profile: profile.Default(), Workspace: workspace, SystemPrompt: "Help with this workspace. Use observed evidence, respect the configured permissions, and report uncertainty explicitly.", ReasoningEffort: p.ReasoningEffort, Claude: p.Claude}
	delete(config.Providers, id)
	// The conservative normal profile permits only native workspace inspection.
	// No shell sandbox is supplied by the existing permission engine.
	nativeOrigins := append([]string(nil), config.Permissions.NetworkOrigins...)
	config.Permissions = permission.Config{NetworkOrigins: nativeOrigins}
	if bootstrapWorkspaceReadable(workspace, o, getenv) {
		config.Permissions.Tools = []string{"read", "grep", "glob"}
		config.Permissions.ReadRoots = []string{workspace}
	}
	if u, e := url.Parse(openaicodex.BaseURL); e == nil {
		config.Permissions.NetworkOrigins = append(config.Permissions.NetworkOrigins, u.Scheme+"://"+u.Host)
	}
	maximum := permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	config.PermissionModes = &permission.ModesConfig{Enabled: true, Maximum: &maximum}
	return config, nil
}

// Automatic inspection never covers the user's home or known private provider/
// Host storage. Operators can review explicit policies; bootstrap grants none
// when a workspace overlaps these sources. This reads path metadata only.
func bootstrapWorkspaceReadable(workspace string, o launchOptions, getenv func(string) string) bool {
	canonical := func(path string) string {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			return real
		}
		return filepath.Clean(path)
	}
	contains := func(root, path string) bool {
		rel, err := filepath.Rel(canonical(root), canonical(path))
		return err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return false
		}
	}
	if contains(workspace, home) {
		return false
	}
	protected := []string{filepath.Join(home, ".claude"), o.state, filepath.Dir(o.config), o.sessions, o.credentials, getenv("CLAUDE_CONFIG_DIR")}
	if codex, err := externalCodexAuthFile(o.codexFile, getenv); err == nil {
		protected = append(protected, filepath.Dir(codex))
	} else {
		return false
	}
	for _, path := range protected {
		if path != "" && (contains(workspace, path) || contains(path, workspace)) {
			return false
		}
	}
	return true
}

func resolveLauncherConfig(ctx context.Context, o launchOptions, choose initialProviderChooser) (serveConfiguration, error) {
	config, err := readServeConfiguration(o.config)
	if !errors.Is(err, fs.ErrNotExist) || !o.normal {
		return config, err
	}
	if choose == nil {
		choose = chooseInitialProvider(io.Discard)
	}
	config, err = bootstrapRuntime(ctx, o, choose, os.Getenv)
	if err != nil {
		return config, err
	}
	data, err := json.Marshal(config, jsontext.WithIndent("  "))
	if err != nil {
		return config, err
	}
	err = privateexport.CreateRuntimeConfig(ctx, o.config, append(data, '\n'))
	if errors.Is(err, fs.ErrExist) {
		// Another launcher/config editor won. Consume its configuration unchanged.
		return readServeConfiguration(o.config)
	}
	return config, err
}
