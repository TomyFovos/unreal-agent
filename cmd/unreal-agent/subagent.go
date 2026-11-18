package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
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

// withParentSubagents gives the Host's Sessions the templates. Children read
// the same credential sources as the Host; defaultCredentialDirectory is the
// launcher's store for OpenCode Go's key. subagentModelFile is subagent model's
// choice for built-in templates, empty for an operator's.
func withParentSubagents(c agentrunner.RuntimeConfig, configured map[string]childTemplate, parentPolicy *permission.Policy, credentialDirectory, defaultCredentialDirectory, codexAuthFile, subagentModelFile string) (agentrunner.RuntimeConfig, map[string]subagent.Template, error) {
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
			// Nested templates retain their recorded shape. Built-in workers
			// resolve saved role choices separately at each new start.
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
	if c.BuiltinSubagents {
		arguments = append(arguments, "--builtin-subagents")
		if subagentModelFile != "" {
			modelFile, err := filepath.Abs(subagentModelFile)
			if err != nil {
				return c, nil, err
			}
			// Registrations are nonsecret connection references. A running lead
			// needs the same routes to bind a later saved worker selection.
			backends := maps.Clone(c.Backends)
			if backends == nil {
				backends = map[string]agentrunner.ProviderRuntime{}
			}
			backends[c.Identity.Provider.Provider] = agentrunner.ProviderRuntime{Provider: c.Identity.Provider, ReasoningEffort: c.Identity.ReasoningEffort, Claude: c.Identity.Claude}
			for _, id := range c.Discovered.IDs() {
				if b, ok := c.Discovered.Lookup(id); ok {
					backends[id] = b.Runtime
				}
			}
			raw, err := json.Marshal(backends)
			if err != nil {
				return c, nil, err
			}
			arguments = append(arguments, "--subagent-model-file", modelFile, "--subagent-backends", string(raw))
		}
	}
	arguments = append(arguments, apiKeyArguments(c.CredentialSources)...)
	if credentialDirectory != "" {
		credentialDirectory, err = filepath.Abs(credentialDirectory)
		if err != nil {
			return c, nil, err
		}
		arguments = append(arguments, "--credential-directory", credentialDirectory)
	} else if defaultCredentialDirectory != "" {
		defaultCredentialDirectory, err = filepath.Abs(defaultCredentialDirectory)
		if err != nil {
			return c, nil, err
		}
		arguments = append(arguments, "--default-credential-directory", defaultCredentialDirectory)
	}
	if codexAuthFile != "" {
		arguments = append(arguments, "--codex-auth-file", codexAuthFile)
	}
	// An explicit nonempty, nonsecret environment prevents os/exec's nil-env
	// inheritance, including provider tokens, .env overlays and proxy credentials.
	execution := subagent.Config{Directory: directory, Binary: binary, Arguments: arguments, Environment: childEnvironment(c), Templates: templates, ResolveRuntime: childRuntimeResolverForLayer(c, subagentModelFile, "main", templates["lead"].Default), ValidateTemplate: childTemplateValidator(c)}
	if len(c.Backends) == 0 && c.Discovered == nil {
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
		// Preserve creation bytes for old pinned templates; the resolver
		// normalizes the effective effort when starting a child.
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
	return agentrunner.WithSubagentTools(base, configuration, disabled)
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
	builtin := flags.Bool("builtin-subagents", false, "validate role selections of built-in worker templates (internal)")
	modelFile := flags.String("subagent-model-file", "", "saved role selections read at each worker start (internal)")
	backendsJSON := flags.String("subagent-backends", "", "nonsecret registered connections for worker starts (internal)")
	directory := flags.String("credential-directory", "", "explicit managed credential store, never credential material")
	defaultDirectory := flags.String("default-credential-directory", "", "the launcher's store OpenCode Go's key is read from, when it exists; never created")
	codexFile := flags.String("codex-auth-file", "", "explicit existing private Codex auth file path, never credential material")
	var apiOptions launchOptions
	apiKeyFlags(flags, &apiOptions)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*stdio || flags.NArg() != 0 {
		return errors.New("child requires --stdio")
	}
	sources, err := apiOptions.apiKeySources()
	if err != nil {
		return err
	}
	resolver, err := childCredentials(*directory, *defaultDirectory, *codexFile)
	if err != nil {
		return err
	}
	var backends map[string]agentrunner.ProviderRuntime
	if *backendsJSON != "" {
		if err := json.Unmarshal([]byte(*backendsJSON), &backends, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
	}
	return subagent.Serve(ctx, os.Stdin, os.Stdout, childFactory(credential.WithSources(resolver, sources, os.Getenv), *codexFile, *builtin, *modelFile, backends, sources))
}

// childCredentials resolves a child's keys from the sources its Host passed.
func childCredentials(directory, defaultDirectory, codexFile string) (credential.Resolver, error) {
	var resolver credential.Resolver
	for _, path := range []string{directory, defaultDirectory} {
		if path != "" && !filepath.IsAbs(path) {
			return nil, errors.New("child credential directory must be absolute")
		}
	}
	if directory != "" {
		store, err := credential.OpenLocal(directory)
		if err != nil {
			return nil, err
		}
		resolver = credential.NewManager(store)
	} else if defaultDirectory != "" {
		resolver = withDefaultOpenCodeGoStore(nil, defaultDirectory)
	}
	if codexFile != "" {
		if !filepath.IsAbs(codexFile) {
			return nil, errors.New("child Codex auth file path must be absolute")
		}
		resolver = withExternalCodexCredentials(resolver, codexFile)
	}
	return resolver, nil
}

// childApprovalTimeout keeps a child's request open until its parent answers
// (U17); a parent that stops ends it instead.
const childApprovalTimeout = agentrunner.ChildApprovalTimeout

func childFactory(resolver credential.Resolver, codexFile string, builtin bool, modelFile string, backends map[string]agentrunner.ProviderRuntime, sources map[string]credential.Source) subagent.ChildFactory {
	base := agentrunner.RuntimeConfig{Credentials: resolver}
	if builtin {
		base.ResolveChildRuntime = boundWorkerRuntime
	}
	return agentrunner.NewChildFactory(base, func(c subagent.ChildConfig, runtime *agentrunner.RuntimeConfig) error {
		if builtin && modelFile != "" {
			runtime.BuiltinSubagents = true
			runtime.Backends = maps.Clone(backends)
			delete(runtime.Backends, runtime.Identity.Provider.Provider)
			var err error
			runtime.Providers, err = registeredProviders(runtime.Identity, runtime.Backends)
			if err != nil {
				return err
			}
		}
		runtime.Catalogs = map[string]func(context.Context) (modelcatalog.Catalog, error){}
		if codexFile != "" {
			read := codexCatalogReader(codexFile, isDockerSandboxCodex(runtime.Identity.Provider.Auth) || isDockerSandboxCodex(runtime.Backends["openai-codex"].Provider.Auth), os.Getenv)
			runtime.Catalogs["openai-codex"] = func(ctx context.Context) (modelcatalog.Catalog, error) { return read(ctx, false) }
		}
		if builtin && modelFile != "" {
			configureWorkerModels(runtime, modelFile, sources, c)
		}
		return nil
	})
}

// Legacy/operator children without a shared role-model file keep their bound
// templates. A Claude Agent role still cannot become an Unreal child.
func boundWorkerRuntime(_ context.Context, _ *host.Session, _ string, template subagent.Template, requested *subagent.RuntimeRequest) (subagent.Template, error) {
	if requested != nil {
		return template, &subagent.RuntimeError{Code: "selection_unsupported"}
	}
	choice := sessionstore.SelectionFromConfiguration(template.Runtime)
	if choice == nil {
		return template, &subagent.RuntimeError{Code: "invalid_template"}
	}
	if buildfeatures.Disabled(choice.Provider) {
		return template, disabledChildError()
	}
	if choice.Provider == claudeagent.ProviderID {
		return template, &subagent.RuntimeError{Code: "provider_unavailable", Hint: claudeChildrenRefusal}
	}
	return template, nil
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

// childEnvironment gives a child the Host's environment, so its commands find
// the same tools (PATH, HOME, GOPATH ...) as the parent's (D1). Each provider
// route authenticates only with its explicitly selected reference; Codex token
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
