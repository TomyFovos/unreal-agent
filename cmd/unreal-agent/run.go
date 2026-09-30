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
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type serveConfiguration struct {
	Runtime         agentrunner.RuntimeIdentity
	Permissions     permission.Config
	LanguageServers []lsp.ServerConfig
	DebugAdapters   []dap.AdapterConfig
	Subagents       map[string]childTemplate
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
		path := flags.String("config", "", "nonsecret runtime/policy JSON configuration")
		directory := flags.String("session-directory", "", "canonical session directory")
		credentials := flags.String("credential-directory", "", "private credential store; optional for unauthenticated providers")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || *directory == "" || *socket == "" || flags.NArg() != 0 {
			return errors.New("serve requires --config, --session-directory and --socket")
		}
		file, err := os.Open(*path)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
		file.Close()
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return errors.New("configuration exceeds 1 MiB")
		}
		var config serveConfiguration
		if err = json.Unmarshal(data, &config, json.RejectUnknownMembers(true)); err != nil {
			return errors.New("invalid runtime configuration JSON")
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
		selected := config.Runtime.Provider
		registry, err := provider.New(provider.Defaults(map[string][]provider.Model{selected.Provider: {selected.Model}})...)
		if err != nil {
			return err
		}
		languageTools, err := agentrunner.NewLanguageTools(permission.WithPolicy(ctx, policy), config.Runtime.Workspace, config.LanguageServers)
		if err != nil {
			return err
		}
		defer languageTools.Close()
		runtimeConfig, templates, err := withParentSubagents(agentrunner.RuntimeConfig{Identity: config.Runtime, SessionDirectory: *directory, Providers: registry, Credentials: resolver, LanguageTools: languageTools, DebugAdapters: config.DebugAdapters}, config.Subagents, policy, *credentials)
		if err != nil {
			return err
		}
		factory, identity, err := agentrunner.NewRuntimeFactory(runtimeConfig)
		if err != nil {
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
		return gateway.ListenAndServe(ctx, *socket, gateway.Config{Host: owner, Policy: policy, Configuration: identity, Auth: auth, Workspace: workspace})
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
		return tui.Terminal(ctx, tui.Config{Client: client, ID: view.Session.Session.ID})
	case "child":
		return runChild(ctx, args[1:], output)
	default:
		return errors.New("unsupported command: expected serve or attach")
	}
}
