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
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/internal/privateexport"
	"golang.org/x/term"
)

// Normal discovery adds registrations, never replaces Runtime or permissions.
// Team/Enterprise trust must be explicitly configured, even on first run.
type normalLauncherConfig struct {
	Version           int
	DiscoverProviders *bool              `json:",omitzero"`
	ClaudeCode        *claudecode.Config `json:",omitzero"`
}

type normalRuntimeError struct{ Code string }

func (e *normalRuntimeError) Error() string {
	switch e.Code {
	case "providers_unavailable":
		return "no external provider is available; sign in with codex login or claude auth login; Team/Enterprise also requires explicit managedPolicyMode=trust in runtime.json"
	case "initial_provider_required":
		return "first run needs an initial provider selection; run unreal in a terminal or set the existing UNREAL_HARNESS_LLM_PROVIDER override"
	case "initial_provider_unavailable":
		return "configured initial provider is unavailable; no automatic provider fallback"
	case "catalog_unavailable":
		return "initial model catalog is unavailable; refresh the external CLI catalog or configure an explicit runtime/model"
	default:
		return "normal runtime: " + e.Code
	}
}

func normalClaudeConfig(config serveConfiguration) claudecode.Config {
	if config.Runtime.Provider.Provider == "claude-code" && config.Runtime.ClaudeCode != nil {
		return *config.Runtime.ClaudeCode
	}
	if p, ok := config.Providers["claude-code"]; ok && p.ClaudeCode != nil {
		return *p.ClaudeCode
	}
	if config.Launcher != nil && config.Launcher.ClaudeCode != nil {
		return *config.Launcher.ClaudeCode
	}
	// Opt in to checking Unreal-owned tools, not permissions or managed trust.
	return claudecode.Config{ManagedPolicyMode: claudecode.ManagedPolicyReject, ToolBridge: claudecode.ToolBridgeConfig{Enabled: true, Mode: claudecode.BridgeModeStructured}}
}

func claudeTransportConfig(c claudecode.Config, getenv func(string) string) claudecode.Config {
	c.Getenv = getenv
	// This authorizes only the isolated provider protocol. Tool subprocesses
	// still go through the unmodified execution permission policy.
	c.AuthorizeProcess = func(context.Context) error { return nil }
	return c
}

func discoverCodex(ctx context.Context, option string, getenv func(string) string) (agentrunner.ProviderRuntime, error) {
	path, err := externalCodexAuthFile(option, getenv)
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return agentrunner.ProviderRuntime{}, &credential.Error{Code: "external_reauth_required"}
	}
	ref := credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}
	// Resolve with the existing external owner, then discard the material. It is
	// never imported, copied to config, logged or retained by discovery.
	if _, err = provider.ExternalCodex(openaicodex.Config{AuthFile: path}).Resolve(ctx, ref); err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	catalog := modelcatalog.ReadCodexCache(codexModelCache(path))
	model, err := initialCatalogModel(catalog, "")
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	return agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: "openai-codex", Model: provider.Model{ID: model.ID}, Endpoint: openaicodex.BaseURL, Auth: ref, MaxAttempts: 2, Source: "external CLI catalog / local bootstrap"}, ReasoningEffort: model.DefaultEffort}, nil
}

func discoverClaude(ctx context.Context, c claudecode.Config, getenv func(string) string) (agentrunner.ProviderRuntime, error) {
	cli, err := claudecode.NewClient(claudeTransportConfig(c, getenv))
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	defer cli.Close()
	if err = cli.Probe(ctx); err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	catalog, err := cli.Catalog(ctx)
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	model, err := initialCatalogModel(catalog, "")
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	return agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: "claude-code", Model: provider.Model{ID: model.ID}, Auth: credential.Reference{Provider: "claude-code", Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 2, Source: "external CLI catalog / local bootstrap"}, ReasoningEffort: model.DefaultEffort, ClaudeCode: &c}, nil
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
	if config.Launcher != nil {
		if config.Launcher.Version != 1 {
			return config, &normalRuntimeError{Code: "unsupported_launcher_version"}
		}
		if config.Launcher.ClaudeCode != nil {
			if _, err := claudecode.NewClient(*config.Launcher.ClaudeCode); err != nil {
				return config, err
			}
		}
		if config.Launcher.DiscoverProviders != nil && !*config.Launcher.DiscoverProviders {
			return config, nil
		}
	}
	config.Providers = cloneBackends(config.Providers)
	catalogUnavailable := false
	// Fixed traversal only controls registration order, never initial selection.
	for _, id := range []string{"openai-codex", "claude-code"} {
		if config.Runtime.Provider.Provider == id || config.Providers[id].Provider.Provider != "" {
			continue
		}
		var p agentrunner.ProviderRuntime
		var err error
		if id == "openai-codex" {
			p, err = discoverCodex(ctx, codexFile, getenv)
		} else {
			p, err = discoverClaude(ctx, normalClaudeConfig(config), getenv)
		}
		if ctx.Err() != nil {
			return config, ctx.Err()
		}
		if err == nil {
			config.Providers[id] = p
		} else {
			var failure *normalRuntimeError
			catalogUnavailable = catalogUnavailable || errors.As(err, &failure) && failure.Code == "catalog_unavailable"
		}
	}
	if config.Runtime.Provider.Provider == "" && len(config.Providers) == 0 && catalogUnavailable {
		return config, &normalRuntimeError{Code: "catalog_unavailable"}
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

func chooseInitialProvider(out io.Writer) initialProviderChooser {
	return func(ids []string) (string, error) {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", &normalRuntimeError{Code: "initial_provider_required"}
		}
		fmt.Fprintln(out, "First run: choose the initial provider (saved in runtime.json).")
		for i, id := range ids {
			fmt.Fprintf(out, "%d. %s\n", i+1, modelcatalog.ProviderName(id))
		}
		fmt.Fprint(out, "Provider number: ")
		var number int
		if _, err := fmt.Fscanln(os.Stdin, &number); err != nil || number < 1 || number > len(ids) {
			return "", &normalRuntimeError{Code: "initial_provider_required"}
		}
		return ids[number-1], nil
	}
}

func bootstrapRuntime(ctx context.Context, o launchOptions, choose initialProviderChooser, getenv func(string) string) (serveConfiguration, error) {
	var config serveConfiguration
	config.Launcher = &normalLauncherConfig{Version: 1}
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
		return config, &normalRuntimeError{Code: "providers_unavailable"}
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
		if id == "openai-codex" {
			path, _ := externalCodexAuthFile(o.codexFile, getenv)
			catalog = modelcatalog.ReadCodexCache(codexModelCache(path))
		} else {
			cli, e := claudecode.NewClient(claudeTransportConfig(*p.ClaudeCode, getenv))
			if e != nil {
				return config, e
			}
			catalog, err = cli.Catalog(ctx)
			cli.Close()
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
	config.Runtime = agentrunner.RuntimeIdentity{Version: 1, Provider: p.Provider, Profile: profile.Default(), Workspace: workspace, SystemPrompt: "Help with this workspace. Use observed evidence, respect the configured permissions, and report uncertainty explicitly.", ReasoningEffort: p.ReasoningEffort, DisallowedTools: []string{"Bash"}, ClaudeCode: p.ClaudeCode}
	delete(config.Providers, id)
	// The conservative normal profile permits only native workspace inspection.
	// No shell sandbox is supplied by the existing permission engine.
	config.Permissions = permission.Config{}
	if bootstrapWorkspaceReadable(workspace, o, getenv) {
		config.Permissions.Tools = []string{"read", "grep", "glob"}
		config.Permissions.ReadRoots = []string{workspace}
	}
	if u, e := url.Parse(openaicodex.BaseURL); e == nil {
		config.Permissions.NetworkOrigins = []string{u.Scheme + "://" + u.Host}
	}
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

// Evaluate the exact generated registry at registration without a Session,
// model request, user frame, tool call or canonical Operation.
func probeNormalBridge(ctx context.Context, c agentrunner.RuntimeConfig, cli *claudecode.Client) modelcatalog.Capabilities {
	capability := modelcatalog.Capabilities{ToolBridge: "unknown"}
	dir, err := os.MkdirTemp("", "unreal-provider-registration-")
	if err != nil {
		return capability
	}
	defer os.RemoveAll(dir)
	c.SessionDirectory, c.Backends = dir, cloneBackends(c.Backends)
	c.ToolCatalogOnly = true
	// Build in the Claude namespace even if Codex is the initial provider.
	p, err := c.ProviderRuntime("claude-code")
	if err != nil {
		return capability
	}
	if c.Identity.Provider.Provider != "claude-code" {
		c.Backends[c.Identity.Provider.Provider] = agentrunner.ProviderRuntime{Provider: c.Identity.Provider, ReasoningEffort: c.Identity.ReasoningEffort, ClaudeCode: c.Identity.ClaudeCode}
		delete(c.Backends, "claude-code")
	}
	c.Identity.Provider, c.Identity.ReasoningEffort, c.Identity.ClaudeCode = p.Provider, p.ReasoningEffort, p.ClaudeCode
	build, _, err := agentrunner.NewRuntimeFactory(c)
	if err != nil {
		return capability
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runtime, err := build(probeCtx, "provider-registration")
	if err != nil {
		return capability
	}
	defer func() {
		cancel()
		if runtime.Close != nil {
			runtime.Close()
		}
		if m, ok := runtime.Operations.(interface{ Done() <-chan struct{} }); ok {
			<-m.Done()
		}
	}()
	var definitions []llm.Tool
	for _, d := range runtime.Tools.StaticDefinitions() {
		definitions = append(definitions, d.Tool)
	}
	if len(definitions) == 0 {
		return modelcatalog.Capabilities{ToolBridge: "disabled"}
	}
	if err = cli.ProbeToolBridge(ctx, definitions); err == nil {
		return modelcatalog.Capabilities{Tools: true, ToolBridge: "available"}
	}
	var failure *claudecode.Error
	if errors.As(err, &failure) && (failure.Code == "bridge_allowlist_inactive" || failure.Code == "bridge_permission_denied" || failure.Code == "permission_denied" || failure.Code == "structured_unavailable") {
		capability.ToolBridge = "blocked_by_policy"
	}
	return capability
}
