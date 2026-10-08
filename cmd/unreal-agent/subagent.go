package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	subtool "github.com/unreallabsai/unreal-agent/harness/tool/subagent"
)

// childTemplate is operator configuration. Model arguments contain only a
// template name and task, never process arguments, runtime JSON or capabilities.
type childTemplate struct {
	Runtime     agentrunner.RuntimeIdentity
	Permissions permission.Config
}

func runtimeProviders(identity agentrunner.RuntimeIdentity) (*provider.Registry, error) {
	selected := identity.Provider
	models := []provider.Model{selected.Model}
	if selected.Provider == "claude-code" && identity.ClaudeCode != nil {
		for _, m := range identity.ClaudeCode.Models {
			if m.ID == selected.Model.ID {
				continue
			}
			pm := selected.Model
			pm.ID = m.ID
			models = append(models, pm)
		}
	}
	return provider.New(provider.Defaults(map[string][]provider.Model{selected.Provider: models})...)
}
func withParentSubagents(c agentrunner.RuntimeConfig, configured map[string]childTemplate, parentPolicy *permission.Policy, credentialDirectory, codexAuthFile string) (agentrunner.RuntimeConfig, map[string]subagent.Template, error) {
	if len(configured) == 0 {
		return c, nil, nil
	}
	if len(c.Backends) == 0 && c.Identity.Provider.Provider == "claude-code" && !c.ProviderSupportsTools("claude-code") {
		return c, nil, errors.New("subagent: claude-code is text-only; tools unsupported without registered child runtimes")
	}
	// Canonical templates stay unchanged; additional provider bindings live in
	// selected child configurations, so old children retain their exact identity.
	c.Identity.Provider, _ = c.Providers.Resolve(c.Identity.Provider)
	for id, p := range c.Backends {
		var e error
		p.Provider, e = c.Providers.Resolve(p.Provider)
		if e != nil {
			return c, nil, e
		}
		c.Backends[id] = p
	}
	if len(configured) > 32 {
		return c, nil, errors.New("subagent: at most 32 templates are supported")
	}
	templates := map[string]subagent.Template{}
	for name, source := range configured {
		if len(c.Backends) == 0 && source.Runtime.Provider.Provider == "claude-code" && (source.Runtime.ClaudeCode == nil || !source.Runtime.ClaudeCode.ToolBridge.Enabled) {
			return c, nil, errors.New("subagent: Claude text-only children require registered runtimes and response-based Finish")
		}

		if strings.TrimSpace(name) == "" || len(name) > 128 {
			return c, nil, errors.New("subagent: invalid template name")
		}
		if err := parentPolicy.CheckProcess(); err != nil {
			return c, nil, err
		}
		if source.Runtime.LanguageServers != "" || source.Runtime.DebugAdapters != "" {
			return c, nil, errors.New("subagent: external language/debug runtime is unsupported by bounded child policy")
		}
		registry, err := runtimeProviders(source.Runtime)
		if err != nil {
			return c, nil, err
		}
		_, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: source.Runtime, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: c.Credentials})
		if err != nil {
			return c, nil, fmt.Errorf("subagent template %q: %w", name, err)
		}
		var resolved agentrunner.RuntimeIdentity
		if err = json.Unmarshal(identity, &resolved); err != nil {
			return c, nil, err
		}
		if slices.Contains(resolved.DisallowedTools, "Finish") {
			return c, nil, errors.New("subagent: Finish cannot be disabled in a child")
		}
		files, err := mutation.New(mutation.Config{Root: resolved.Workspace, Authorize: func(ctx context.Context, path string, write bool) error {
			return permission.FromContext(ctx).CheckPath(path, write)
		}})
		if err != nil {
			return c, nil, err
		}
		template := subagent.Template{Workspace: resolved.Workspace, Runtime: identity, Policy: source.Permissions, MutationStateDirectory: files.StateDirectory()}
		// Reuse the protocol validator for bounded child capabilities, including
		// required Finish; this creates no Session or process.
		check := subagent.ChildConfig{Version: 1, ParentID: "validation", OperationID: "validation", ChildID: subagent.ChildID("validation", "validation"), ReadyID: "ready:validation", Task: "validation", Workspace: template.Workspace, SessionDirectory: template.Workspace, Runtime: template.Runtime, Policy: template.Policy}
		if resolved.Provider.Provider == "claude-code" {
			check.TextOnly = !sessionstore.ToolBridgeEnabledFromConfiguration(identity)
			check.ProviderProcess = true
		}
		if err = check.Validate(); err != nil {
			return c, nil, err
		}
		policy, err := permission.New(template.Policy)
		if err != nil {
			return c, nil, err
		}
		policy.Close()
		for _, capability := range template.Policy.Tools {
			if err = parentPolicy.CheckTool(capability); err != nil {
				return c, nil, err
			}
		}
		for _, root := range template.Policy.ReadRoots {
			if err = parentPolicy.CheckPath(root, false); err != nil {
				return c, nil, err
			}
		}
		for _, root := range template.Policy.WriteRoots {
			if err = parentPolicy.CheckPath(root, true); err != nil {
				return c, nil, err
			}
		}
		for _, origin := range template.Policy.NetworkOrigins {
			u, e := url.Parse(origin)
			if e != nil {
				return c, nil, e
			}
			if err = parentPolicy.CheckURL(u); err != nil {
				return c, nil, err
			}
		}
		templates[name] = template
	}
	directory, err := filepath.Abs(c.SessionDirectory)
	if err != nil {
		return c, nil, err
	}
	binary, err := os.Executable()
	if err != nil {
		return c, nil, err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return c, nil, err
	}
	arguments := []string{"child", "--stdio"}
	if credentialDirectory != "" {
		credentialDirectory, err = filepath.Abs(credentialDirectory)
		if err != nil {
			return c, nil, err
		}
		arguments = append(arguments, "--credential-directory", credentialDirectory)
	}
	if codexAuthFile != "" {
		arguments = append(arguments, "--codex-auth-file", codexAuthFile)
	}
	// An explicit nonempty, nonsecret environment prevents os/exec's nil-env
	// inheritance, including provider tokens, .env overlays and proxy credentials.
	execution := subagent.Config{Directory: directory, Binary: binary, Arguments: arguments, Environment: childEnvironment(c), Templates: templates, ResolveRuntime: childRuntimeResolver(c), ValidateTemplate: childTemplateValidator(c)}
	if len(c.Backends) == 0 {
		execution.ResolveRuntime = nil
		execution.ValidateTemplate = nil
	}
	c.ControlTools = true
	c.NewTools = withSubagentTools(c.NewTools, execution, c.Identity.DisallowedTools)
	return c, templates, nil
}
func withSubagentTools(base agentrunner.ToolFactory, configuration subagent.Config, disabled []string) agentrunner.ToolFactory {
	disabled = slices.Clone(disabled)
	if base == nil {
		base = func(ctx context.Context, c agentrunner.ToolConfig) (agentrunner.Tools, error) {
			return agentrunner.DefaultTools(ctx, c, disabled)
		}
	}
	return func(ctx context.Context, c agentrunner.ToolConfig) (agentrunner.Tools, error) {
		owner, ok := host.SessionFromContext(ctx)
		if !ok && !c.CatalogOnly {
			return agentrunner.Tools{}, errors.New("subagent: Host owner context required")
		}
		configured, err := base(ctx, c)
		if err != nil {
			return agentrunner.Tools{}, err
		}
		closeBase := configured.Close
		fail := func(err error) (agentrunner.Tools, error) {
			if closeBase != nil {
				err = errors.Join(err, closeBase())
			}
			return agentrunner.Tools{}, err
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
func withSubagentIdentity(runtime jsontext.Value, templates map[string]subagent.Template) (jsontext.Value, error) {
	if len(templates) == 0 {
		return runtime, nil
	}
	return json.Marshal(struct {
		Version   int
		Runtime   jsontext.Value
		Subagents map[string]subagent.Template
	}{1, runtime, templates})
}
func runChild(ctx context.Context, args []string, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("unreal-agent child", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	stdio := flags.Bool("stdio", false, "serve one parent-bound child protocol connection")
	directory := flags.String("credential-directory", "", "explicit managed credential store, never credential material")
	codexFile := flags.String("codex-auth-file", "", "explicit existing private Codex auth file path, never credential material")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*stdio || flags.NArg() != 0 {
		return errors.New("child requires --stdio")
	}
	var resolver credential.Resolver
	if *directory != "" {
		if !filepath.IsAbs(*directory) {
			return errors.New("child credential directory must be absolute")
		}
		store, err := credential.OpenLocal(*directory)
		if err != nil {
			return err
		}
		resolver = credential.NewManager(store, nil)
	}
	if *codexFile != "" {
		if !filepath.IsAbs(*codexFile) {
			return errors.New("child Codex auth file path must be absolute")
		}
		resolver = withExternalCodexCredentials(resolver, *codexFile)
	}
	return subagent.Serve(ctx, os.Stdin, os.Stdout, childFactory(resolver))
}
func childFactory(resolver credential.Resolver) subagent.ChildFactory {
	return func(ctx context.Context, c subagent.ChildConfig, send subagent.Sender) (*host.Session, io.Closer, error) {
		if err := c.Validate(); err != nil {
			return nil, nil, err
		}
		var identity agentrunner.RuntimeIdentity
		if err := json.Unmarshal(c.Runtime, &identity, json.RejectUnknownMembers(true)); err != nil {
			return nil, nil, errors.New("child runtime configuration is invalid")
		}
		if !filepath.IsAbs(identity.Workspace) || filepath.Clean(identity.Workspace) != filepath.Clean(c.Workspace) {
			return nil, nil, errors.New("child runtime workspace mismatch")
		}
		if slices.Contains(identity.DisallowedTools, "Finish") {
			return nil, nil, errors.New("child runtime disables Finish")
		}
		registry, err := runtimeProviders(identity)
		if err != nil {
			return nil, nil, err
		}
		var tools agentrunner.ToolFactory
		if !c.TextOnly {
			tools = withSubagentTools(func(ctx context.Context, tools agentrunner.ToolConfig) (agentrunner.Tools, error) {
				tools.MutationStateDirectory = c.MutationStateDirectory
				return agentrunner.DefaultTools(ctx, tools, identity.DisallowedTools)
			}, subagent.Config{Child: &c, SendParent: send}, identity.DisallowedTools)
		}
		factory, resolved, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: identity, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: resolver, NewTools: tools, ProviderProcess: c.ProviderProcess, TaskInput: c.InitialInput().ID})
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
		policy, err := permission.New(c.Policy)
		if err != nil {
			return nil, nil, err
		}
		owner, err := host.New(ctx, host.Config{Directory: c.SessionDirectory, Build: factory})
		if err != nil {
			policy.Close()
			return nil, nil, err
		}
		closeOwner := closeFunc(func() error { return errors.Join(owner.Close(), policy.Close()) })
		configuration, err := json.Marshal(c)
		if err != nil {
			closeOwner.Close()
			return nil, nil, err
		}
		snapshot := projectinstructions.None()
		if c.ProjectInstructions != nil {
			snapshot = *c.ProjectInstructions
		}
		current, err := owner.Open(ctx, host.Options{TextOnlyChild: c.TextOnly, ProjectInstructions: &snapshot, Workspace: c.Workspace, ID: c.ChildID, Lifecycle: "child", Policy: policy, Configuration: configuration, Initial: []inbox.Input{c.InitialInput()}, Heartbeat: 0})
		if err != nil {
			closeOwner.Close()
			return nil, nil, err
		}
		return current, closeOwner, nil
	}
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

// Only nonsecret account source locations cross the restricted process boundary.
// API keys, OAuth tokens, proxy credentials and parent OTEL settings do not.
func childEnvironment(c agentrunner.RuntimeConfig) []string {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if c.Identity.Provider.Provider != "claude-code" && c.Backends["claude-code"].Provider.Provider == "" {
		return env
	}
	if home, e := os.UserHomeDir(); e == nil && filepath.IsAbs(home) && !strings.ContainsAny(home, "\x00\r\n") {
		env = append(env, "HOME="+home)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(dir) && !strings.ContainsAny(dir, "\x00\r\n") {
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}
	return env
}
