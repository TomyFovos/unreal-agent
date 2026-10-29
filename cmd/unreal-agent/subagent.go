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
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	subtool "github.com/unreallabsai/unreal-agent/harness/tool/subagent"
)

// childTemplate is operator configuration. Model arguments contain only a
// template name and task, never process arguments, runtime JSON or capabilities.
type childTemplate struct {
	Runtime     agentrunner.RuntimeIdentity
	Permissions permission.Config
	// Baseline is what the child may do in Read Only and without approval in
	// Ask; Permissions is its ceiling (see subagent.Template).
	Baseline *permission.Config `json:",omitzero"`
	// Pinned starts the child with Runtime's own selection when the start
	// names none, instead of the parent's current one.
	Pinned bool `json:",omitzero"`
	// Workers are the templates this child may start itself.
	Workers map[string]childTemplate `json:",omitzero"`
}

func runtimeProviders(identity agentrunner.RuntimeIdentity) (*provider.Registry, error) {
	selected := identity.Provider
	models := []provider.Model{selected.Model}
	return provider.New(provider.Defaults(map[string][]provider.Model{selected.Provider: models})...)
}
func withParentSubagents(c agentrunner.RuntimeConfig, configured map[string]childTemplate, parentPolicy *permission.Policy, credentialDirectory, codexAuthFile string) (agentrunner.RuntimeConfig, map[string]subagent.Template, error) {
	if len(configured) == 0 {
		return c, nil, nil
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
	templates := map[string]subagent.Template{}
	for name, source := range configured {
		workers := map[string]subagent.Template{}
		for worker, nested := range source.Workers {
			if len(nested.Workers) != 0 {
				return c, nil, fmt.Errorf("subagent template %q: workers cannot start children", name)
			}
			// A worker runs in its lead's process, which has no runtime
			// choice: its template is fixed.
			nested.Pinned = false
			converted, err := childTemplateFor(c, worker, nested, parentPolicy)
			if err != nil {
				return c, nil, err
			}
			workers[worker] = converted
		}
		template, err := childTemplateFor(c, name, source, parentPolicy)
		if err != nil {
			return c, nil, err
		}
		if len(workers) > 0 {
			template.Subagents = workers
			if err = subagent.ValidateWorkers(workers); err != nil {
				return c, nil, fmt.Errorf("subagent template %q: %w", name, err)
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

// childTemplateFor resolves one operator template for the parent's checks.
func childTemplateFor(c agentrunner.RuntimeConfig, name string, source childTemplate, parentPolicy *permission.Policy) (subagent.Template, error) {
	var template subagent.Template
	if strings.TrimSpace(name) == "" || len(name) > 128 {
		return template, errors.New("subagent: invalid template name")
	}
	if err := parentPolicy.CheckProcess(); err != nil {
		return template, err
	}
	if source.Runtime.LanguageServers != "" || source.Runtime.DebugAdapters != "" {
		return template, errors.New("subagent: external language/debug runtime is unsupported by bounded child policy")
	}
	registry, err := runtimeProviders(source.Runtime)
	if err != nil {
		return template, err
	}
	_, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: source.Runtime, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: c.Credentials})
	if err != nil {
		return template, fmt.Errorf("subagent template %q: %w", name, err)
	}
	var resolved agentrunner.RuntimeIdentity
	if err = json.Unmarshal(identity, &resolved); err != nil {
		return template, err
	}
	if slices.Contains(resolved.DisallowedTools, "Finish") {
		return template, errors.New("subagent: Finish cannot be disabled in a child")
	}
	files, err := mutation.New(mutation.Config{Root: resolved.Workspace, Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		return template, err
	}
	template = subagent.Template{Workspace: resolved.Workspace, Runtime: identity, Policy: source.Permissions, Baseline: source.Baseline, MutationStateDirectory: files.StateDirectory()}
	if source.Pinned {
		template.Default = &subagent.RuntimeRequest{Provider: resolved.Provider.Provider, Model: resolved.Provider.Model.ID, Effort: resolved.ReasoningEffort}
	}
	for _, config := range []*permission.Config{&template.Policy, template.Baseline} {
		if config == nil {
			continue
		}
		policy, err := permission.New(*config)
		if err != nil {
			return template, err
		}
		policy.Close()
	}
	for _, capability := range template.Policy.Tools {
		if err = parentPolicy.CheckTool(capability); err != nil {
			return template, err
		}
	}
	for _, root := range template.Policy.ReadRoots {
		if err = parentPolicy.CheckPath(root, false); err != nil {
			return template, err
		}
	}
	for _, root := range template.Policy.WriteRoots {
		if err = parentPolicy.CheckPath(root, true); err != nil {
			return template, err
		}
	}
	for _, origin := range template.Policy.NetworkOrigins {
		u, e := url.Parse(origin)
		if e != nil {
			return template, e
		}
		if err = parentPolicy.CheckURL(u); err != nil {
			return template, err
		}
	}
	return template, nil
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
		configured.InterruptedInput = manager.InterruptedInput
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
		resolver = credential.NewManager(store)
	}
	if *codexFile != "" {
		if !filepath.IsAbs(*codexFile) {
			return errors.New("child Codex auth file path must be absolute")
		}
		resolver = withExternalCodexCredentials(resolver, *codexFile)
	}
	return subagent.Serve(ctx, os.Stdin, os.Stdout, childFactory(resolver, *codexFile))
}

// childApprovalTimeout keeps a child's request open until its parent answers
// (U17); a parent that stops ends it instead.
const childApprovalTimeout = 1000 * time.Hour

func childFactory(resolver credential.Resolver, codexFile string) subagent.ChildFactory {
	return func(ctx context.Context, c subagent.ChildConfig, link subagent.ChildLink) (*host.Session, io.Closer, error) {
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
			tools = withSubagentTools(func(ctx context.Context, tools agentrunner.ToolConfig) (agentrunner.Tools, error) {
				tools.MutationStateDirectory = c.MutationStateDirectory
				return agentrunner.DefaultTools(ctx, tools, identity.DisallowedTools)
			}, execution, identity.DisallowedTools)
		}
		runtime := agentrunner.RuntimeConfig{Identity: identity, SessionDirectory: c.SessionDirectory, Providers: registry, Credentials: resolver, NewTools: tools, ProviderProcess: c.ProviderProcess, TaskInput: c.InitialInput().ID}
		if codexFile != "" {
			// Codex sends a selected ultra as the model's multi-agent effort,
			// which this catalog names.
			read := codexCatalogReader(codexFile, isDockerSandboxCodex(identity.Provider.Auth), os.Getenv)
			runtime.Catalogs = map[string]func(context.Context) (modelcatalog.Catalog, error){"openai-codex": func(ctx context.Context) (modelcatalog.Catalog, error) { return read(ctx, false) }}
		}
		factory, resolved, err := agentrunner.NewRuntimeFactory(runtime)
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
			hostConfig.PermissionMaximum, hostConfig.ApprovalTimeout = maximum, childApprovalTimeout
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
		closeOwner := closeFunc(func() error {
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

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

// childEnvironment gives a child the Host's environment, so its commands find
// the same tools (PATH, HOME, GOPATH ...) as the parent's (D1). Each provider
// route still authenticates only with the subscription (U5); Codex token
// variables stay behind as the launcher's Host environment drops them.
func childEnvironment(agentrunner.RuntimeConfig) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key != "OPENAI_CODEX_ACCESS_TOKEN" && key != "OPENAI_CODEX_ACCOUNT_ID" {
			env = append(env, kv)
		}
	}
	return env
}
