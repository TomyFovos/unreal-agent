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

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type serveConfiguration struct {
	// Launcher is nonsecret local discovery policy, outside Session identity.
	Launcher        *normalLauncherConfig `json:",omitzero"`
	Context         contextengine.Config  `json:",omitzero"`
	Runtime         agentrunner.RuntimeIdentity
	Permissions     permission.Config
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
		policy, err := permission.New(config.Permissions)
		if err != nil {
			return err
		}
		defer policy.Close()
		var resolver credential.Resolver
		var auth *authflow.Service
		if *credentials != "" {
			store, e := credential.OpenLocal(*credentials)
			if e != nil {
				return e
			}
			manager := credential.NewManager(store, nil)
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
		registry, err := registeredProviders(config.Runtime, config.Providers)
		if err != nil {
			return err
		}
		var claudeClient *claudecode.Client
		var readClaudeCatalog func(context.Context) (modelcatalog.Catalog, error)
		health := map[string]func(context.Context) error{}
		claudeConfig := config.Runtime.ClaudeCode
		if p, ok := config.Providers["claude-code"]; ok {
			claudeConfig = p.ClaudeCode
		}
		if config.Runtime.Provider.Provider == "claude-code" || config.Providers["claude-code"].Provider.Provider != "" {
			if claudeConfig == nil {
				return errors.New("claude-code requires Runtime.ClaudeCode executable/catalog configuration")
			}
			transportConfig := *claudeConfig
			if normalMode {
				transportConfig = claudeTransportConfig(transportConfig, os.Getenv)
			}
			cli, e := claudecode.NewClient(transportConfig)
			if e != nil {
				return e
			}
			probeErr := cli.Probe(permission.WithPolicy(ctx, policy))
			if probeErr != nil && len(config.Providers) == 0 && !normalMode {
				return probeErr
			}
			claudeClient = cli
			defer cli.Close()
			health["claude-code"] = func(checkCtx context.Context) error {
				if normalMode {
					return probeErr // registration result; not a heavy probe per picker/turn
				}
				checkCtx, cancelCheck := context.WithCancel(checkCtx)
				stop := context.AfterFunc(ctx, cancelCheck)
				defer stop()
				defer cancelCheck()
				return cli.Probe(permission.WithPolicy(checkCtx, policy))
			}
			readClaudeCatalog = func(readCtx context.Context) (modelcatalog.Catalog, error) {
				readCtx, cancelRead := context.WithCancel(readCtx)
				stop := context.AfterFunc(ctx, cancelRead)
				defer stop()
				defer cancelRead()
				return cli.Catalog(permission.WithPolicy(readCtx, policy))
			}
		}
		languageTools, err := agentrunner.NewLanguageTools(permission.WithPolicy(ctx, policy), config.Runtime.Workspace, config.LanguageServers)
		if err != nil {
			return err
		}
		defer languageTools.Close()
		runtimeConfig, templates, err := withParentSubagents(agentrunner.RuntimeConfig{Context: config.Context, Identity: config.Runtime, SessionDirectory: *directory, Providers: registry, Credentials: resolver, LanguageTools: languageTools, DebugAdapters: config.DebugAdapters, Backends: config.Providers, Health: health, Catalogs: map[string]func(context.Context) (modelcatalog.Catalog, error){"claude-code": readClaudeCatalog, "openai-codex": func(context.Context) (modelcatalog.Catalog, error) {
			return modelcatalog.ReadCodexCache(codexModelCache(codexPath)), nil
		}}}, config.Subagents, policy, *credentials, codexPath)
		if err != nil {
			return err
		}
		if normalMode {
			runtimeConfig.ProviderProcess = true
			runtimeConfig.ToolFilter = func(id, name string) bool {
				return id != "claude-code" || policy.CheckTool(name) == nil
			}
			available := runtimeConfig.CheckProvider(ctx, config.Runtime.Provider.Provider) == nil
			for id := range runtimeConfig.Backends {
				available = runtimeConfig.CheckProvider(ctx, id) == nil || available
			}
			if !available {
				return &normalRuntimeError{Code: "providers_unavailable"}
			}
			if claudeConfig != nil {
				capability := modelcatalog.Capabilities{ToolBridge: "disabled"}
				if claudeConfig.ToolBridge.Enabled {
					capability.ToolBridge = "unknown"
					if runtimeConfig.CheckProvider(ctx, "claude-code") == nil {
						capability = probeNormalBridge(permission.WithPolicy(ctx, policy), runtimeConfig, claudeClient)
					}
				}
				runtimeConfig.Capabilities = map[string]modelcatalog.Capabilities{"claude-code": capability}
			}
		}
		factory, identity, err := agentrunner.NewRuntimeFactory(runtimeConfig)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(identity, &runtimeConfig.Identity); err != nil {
			return err
		}
		identity, err = withSubagentIdentity(identity, templates)
		if err != nil {
			return err
		}
		owner, err := host.New(ctx, host.Config{Directory: *directory, Build: factory})
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
		var claudeCatalog modelcatalog.Catalog
		if claudeConfig != nil {
			claudeCatalog, err = modelcatalog.Configured(claudeConfig.Models)
			if err != nil {
				return err
			}
		}
		binding := launcherHostBinding{Configuration: absolutePath(*path), Sessions: absolutePath(*directory), Credentials: absolutePath(*credentials), CodexFile: absolutePath(*codexFile), Normal: *normal}
		models := modelUI{binding: &binding, owner: owner, runtime: runtimeConfig, templates: templates, policy: policy, cache: codexModelCache(codexPath), claude: claudeCatalog, next: gatewayConfig.Extension}
		if claudeClient != nil {
			models.claudeRead = func(readCtx context.Context, refresh bool) (modelcatalog.Catalog, error) {
				if refresh {
					claudeClient.InvalidateCatalog()
				}
				return readClaudeCatalog(readCtx)
			}
		}
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
