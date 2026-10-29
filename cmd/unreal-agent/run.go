package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type serveConfiguration struct {
	providerStatuses    []modelcatalog.Provider
	providerDiagnostics []string
	// Launcher is nonsecret local discovery policy, outside Session identity.
	Launcher        *normalLauncherConfig `json:",omitzero"`
	Context         contextengine.Config  `json:",omitzero"`
	Runtime         agentrunner.RuntimeIdentity
	Permissions     permission.Config
	PermissionModes *permission.ModesConfig `json:",omitzero"`
	LanguageServers []lsp.ServerConfig
	DebugAdapters   []dap.AdapterConfig
	Subagents       map[string]childTemplate
	// Additional registered backends. Runtime.Provider remains the initial
	// provider, preserving existing single-provider configuration identities.
	Providers map[string]agentrunner.ProviderRuntime `json:",omitzero"`
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: unreal-agent serve|attach (see docs/interactive.md)")
	}
	flags := flag.NewFlagSet("unreal-agent "+args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", "", "absolute private Unix socket path")
	switch args[0] {
	case "providers":
		return runProviderPreflight(ctx, args[1:], output)
	case "serve":
		normal := flags.Bool("normal-runtime", false, "enable normal launcher provider discovery (internal launcher option)")
		path := flags.String("config", "", "nonsecret runtime/policy JSON configuration")
		directory := flags.String("session-directory", "", "canonical session directory")
		credentials := flags.String("credential-directory", "", "private credential store; optional for unauthenticated providers")
		codexFile := flags.String("codex-auth-file", "", "existing private Codex auth file path; defaults to Codex environment discovery")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || *directory == "" || *socket == "" || flags.NArg() != 0 {
			return errors.New("serve requires --config, --session-directory and --socket")
		}
		config, err := readServeConfiguration(*path)
		if err != nil {
			return err
		}
		normalMode := *normal || config.Launcher != nil
		if normalMode {
			config, err = registerNormalProviders(ctx, config, *codexFile, os.Getenv)
			if err != nil {
				return err
			}
		}
		// Built-in subagents are derived per Host, not part of a Session's
		// recorded identity, so Sessions from before them still resume.
		builtinSubagents := false
		if len(config.Subagents) == 0 {
			if defaults := defaultSubagents(config); defaults != nil {
				maximum := withChildTools(*config.PermissionModes.Maximum)
				config.PermissionModes.Maximum = &maximum
				config.Subagents, builtinSubagents = defaults, true
			}
		}
		policy, err := permission.New(withClaudeCodeRecord(config.Permissions))
		if err != nil {
			return err
		}
		defer policy.Close()
		capabilityPolicy := policy
		var modeMaximum *permission.Policy
		if config.PermissionModes != nil {
			if err = config.PermissionModes.Validate(); err != nil {
				return err
			}
			if config.PermissionModes.Enabled {
				modeMaximum, err = permission.New(withClaudeCodeRecord(*config.PermissionModes.Maximum))
				if err != nil {
					return err
				}
				defer modeMaximum.Close()
				capabilityPolicy = modeMaximum
			}
		}
		var resolver credential.Resolver
		var auth *authflow.Service
		if *credentials != "" {
			store, e := credential.OpenLocal(*credentials)
			if e != nil {
				return e
			}
			manager := credential.NewManager(store)
			resolver = manager
			auth = authflow.New(manager)
		}
		var codexPath string
		if *codexFile != "" || needsExternalCodex(config) {
			codexPath, err = externalCodexAuthFile(*codexFile, os.Getenv)
			if err != nil {
				return err
			}
			resolver = withExternalCodexCredentials(resolver, codexPath)
		}
		if normalMode && codexPath == "" {
			resolver = withCodexDiscoveryCredentials(resolver, *codexFile, os.Getenv)
		}
		for _, diagnostic := range config.providerDiagnostics {
			fmt.Fprintln(output, diagnostic)
		}
		registry, err := registeredProviders(config.Runtime, config.Providers)
		if err != nil {
			return err
		}
		codexRead := codexCatalogReader(codexPath, isDockerSandboxCodex(config.Runtime.Provider.Auth) || isDockerSandboxCodex(config.Providers["openai-codex"].Provider.Auth), os.Getenv)
		health := map[string]func(context.Context) error{}
		var claudeAgent *claudeCatalog
		if binary, e := claudeBinary(config, os.Getenv); e == nil && (config.Runtime.Provider.Provider == claudeagent.ProviderID || config.Providers[claudeagent.ProviderID].Provider.Provider != "") {
			claudeAgent = &claudeCatalog{binary: binary}
			health[claudeagent.ProviderID] = claudeAgent.check
		}
		languageTools, err := agentrunner.NewLanguageTools(permission.WithPolicy(ctx, policy), config.Runtime.Workspace, config.LanguageServers)
		if err != nil {
			return err
		}
		defer languageTools.Close()
		catalogs := map[string]func(context.Context) (modelcatalog.Catalog, error){"openai-codex": func(readCtx context.Context) (modelcatalog.Catalog, error) {
			return codexRead(readCtx, false)
		}}
		if claudeAgent != nil {
			catalogs[claudeagent.ProviderID] = func(readCtx context.Context) (modelcatalog.Catalog, error) { return claudeAgent.read(readCtx, false) }
		}
		runtimeConfig, templates, err := withParentSubagents(agentrunner.RuntimeConfig{Context: config.Context, Identity: config.Runtime, SessionDirectory: *directory, Providers: registry, Credentials: resolver, LanguageTools: languageTools, DebugAdapters: config.DebugAdapters, Backends: config.Providers, Health: health, Catalogs: catalogs}, config.Subagents, capabilityPolicy, *credentials, codexPath)
		if err != nil {
			return err
		}
		if runtimeConfig.ClaudeSubagents, err = claudeSubagents(templates, *socket); err != nil {
			return err
		}
		if normalMode {
			runtimeConfig.ProviderProcess = true
			runtimeConfig.ToolFilter = func(id, name string) bool {
				if modeMaximum != nil {
					return capabilityPolicy.CheckTool(name) == nil
				}
				return true
			}
			available := runtimeConfig.CheckProvider(ctx, config.Runtime.Provider.Provider) == nil
			for id := range runtimeConfig.Backends {
				available = runtimeConfig.CheckProvider(ctx, id) == nil || available
			}
			if !available {
				return &normalRuntimeError{Code: "providers_unavailable"}
			}
		}
		if modeMaximum != nil {
			runtimeConfig.ProviderProcess = true
			runtimeConfig.ToolFilter = func(_, name string) bool { return capabilityPolicy.CheckTool(name) == nil }
		}
		var rediscover func(context.Context) error
		if normalMode {
			runtimeConfig.Discovered = &agentrunner.DiscoveredBackends{}
			rediscover = normalRediscovery(config, *codexFile, runtimeConfig.Discovered, os.Getenv, output)
		}
		factory, identity, err := agentrunner.NewRuntimeFactory(runtimeConfig)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(identity, &runtimeConfig.Identity); err != nil {
			return err
		}
		if !builtinSubagents {
			identity, err = withSubagentIdentity(identity, templates)
			if err != nil {
				return err
			}
		}
		var failureLog sync.Mutex
		owner, err := host.New(ctx, host.Config{Directory: *directory, Build: factory, PermissionMaximum: modeMaximum, OnFailure: func(id session.ID, cause error) {
			failureLog.Lock()
			defer failureLog.Unlock()
			fmt.Fprintln(output, gateway.SafeReason(fmt.Sprintf("session %s: %v", id, cause)))
		}})
		if err != nil {
			return err
		}
		defer owner.Close()
		fmt.Fprintln(output, "Local Host starting; attach clients may disconnect without stopping sessions.")
		workspace, err := filepath.Abs(config.Runtime.Workspace)
		if err != nil {
			return err
		}
		gatewayConfig := withViewer(gateway.Config{Host: owner, Policy: policy, Configuration: identity, Auth: auth, Workspace: workspace}, *directory)
		binding := launcherHostBinding{Configuration: absolutePath(*path), Sessions: absolutePath(*directory), Credentials: absolutePath(*credentials), CodexFile: absolutePath(*codexFile), Normal: *normal}
		models := modelUI{rediscover: rediscover, supported: config.providerStatuses, binding: &binding, owner: owner, runtime: runtimeConfig, templates: templates, policy: policy, codexRead: codexRead, claudeAgent: claudeAgent, next: gatewayConfig.Extension}
		gatewayConfig.Extension = models
		return gateway.ListenAndServe(ctx, *socket, gatewayConfig)
	case "attach":
		id := flags.String("session", "", "session ID; optional with --mode create")
		mode := flags.String("mode", "attach", "attach, create, or resume")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *socket == "" || flags.NArg() != 0 {
			return errors.New("attach requires --socket")
		}
		client := gateway.NewClient(*socket)
		defer client.Close()
		var view host.View
		var err error
		switch *mode {
		case "attach":
			if *id == "" {
				return errors.New("attach requires --session")
			}
			view, err = client.Inspect(ctx, session.ID(*id), 0, 128)
		case "create", "resume":
			if *mode == "resume" && *id == "" {
				return errors.New("resume requires --session")
			}
			view, err = client.Open(ctx, host.Mode(*mode), session.ID(*id))
		default:
			return errors.New("mode must be attach, create or resume")
		}
		if err != nil {
			return err
		}
		return attachViewer(ctx, client, view.Session.Session.ID)
	case "child":
		return runChild(ctx, args[1:], output)
	default:
		return errors.New("unsupported command: expected serve or attach")
	}
}

// withClaudeCodeRecord lets every policy, the Read Only baseline included, keep
// claude_code receipts. They only record calls Claude Code already ran after
// Unreal's mode approved them (U21) and grant nothing, so a policy saved
// before the route existed covers them too.
func withClaudeCodeRecord(c permission.Config) permission.Config {
	if !slices.Contains(c.Tools, tool.ClaudeCodeName) {
		c.Tools = append(slices.Clone(c.Tools), tool.ClaudeCodeName)
	}
	return c
}
