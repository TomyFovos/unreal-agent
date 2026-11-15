package agentrunner

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func runDevelopment(ctx context.Context, parsed Request, messages []RequestMessage, newTools ToolFactory, selected Provider, model, apiKey, endpoint string, attempts int, httpClient *http.Client, getenv func(string) string, environ func() []string, config Config, directory, workspace string, id session.ID, output io.Writer, heartbeat time.Duration) error {
	c, err := sessionProviderConfig(selected, provider.Model{ID: model, Family: parsed.ModelFamily}, apiKey, endpoint, attempts, getenv)
	if err != nil {
		return err
	}
	c.Identity.Provider.Model.Family = parsed.ModelFamily
	resolved, err := resolveRequestProfile(parsed, c.Identity.Provider)
	if err != nil {
		return err
	}
	c.Identity.Version, c.Identity.Profile = 1, resolved.Selection()
	c.Identity.Workspace, c.Identity.SystemPrompt = workspace, defaultSystemPrompt
	if parsed.SystemPrompt != nil {
		c.Identity.SystemPrompt = *parsed.SystemPrompt
	}
	c.Identity.ReasoningEffort, c.Identity.DisallowedTools = reasoningEffort(parsed.ThinkingLevel), parsed.DisallowedTools
	c.SessionDirectory, c.Context, c.HTTPClient = directory, config.Context, httpClient
	c.NewTools, c.BuiltinSubagents = newTools, true
	servers, err := LanguageServerConfiguration(getenv)
	if err != nil {
		return err
	}
	c.LanguageTools, err = NewLanguageTools(ctx, workspace, servers)
	if err != nil {
		return err
	}
	defer c.LanguageTools.Close()
	c.DebugAdapters, err = DebugAdapterConfiguration(getenv)
	if err != nil {
		return err
	}
	templates, err := runnerSubagents(c)
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	execution := subagent.Config{Directory: directory, Binary: binary, Arguments: []string{"child", "--stdio"}, Environment: runnerEnvironment(environ(), getenv), Templates: templates}
	c.ControlTools = true
	c.NewTools = WithSubagentTools(c.NewTools, execution, parsed.DisallowedTools)
	factory, identity, err := NewRuntimeFactory(c)
	if err != nil {
		return err
	}
	identity, err = json.Marshal(struct {
		Version   int
		Runtime   jsontext.Value
		Subagents map[string]subagent.Template
	}{1, identity, templates})
	if err != nil {
		return err
	}
	owner, err := host.New(ctx, host.Config{Directory: directory, Build: factory})
	if err != nil {
		return err
	}
	defer owner.Close()
	initial, err := requestInputs(parsed, messages)
	if err != nil {
		return err
	}
	current, err := owner.Open(ctx, host.Options{Policy: permission.FromContext(ctx), Lifecycle: "one-shot", Configuration: identity, ID: id, Initial: initial, Heartbeat: heartbeat, Workspace: workspace})
	if err != nil {
		return err
	}
	if err = writeHostSession(ctx, current, output); err != nil {
		return err
	}
	return current.Wait(ctx)
}

// Reuse the product role prompts and worker efforts. A measurement pins every
// role to the requested model and connection, rather than selecting other models.
func runnerSubagents(c RuntimeConfig) (map[string]subagent.Template, error) {
	files, err := mutation.New(mutation.Config{Root: c.Identity.Workspace, Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		return nil, err
	}
	makeTemplate := func(effort llm.ReasoningEffort, prompt string, lead bool) (subagent.Template, error) {
		identity := c.Identity
		identity.SystemPrompt, identity.Profile, identity.ReasoningEffort = prompt, profile.Default(), effort
		if c.Identity.ReasoningEffort == "" {
			identity.ReasoningEffort = ""
		}
		child := c
		child.Identity, child.NewTools, child.LanguageTools, child.DebugAdapters = identity, nil, nil, nil
		_, raw, err := NewRuntimeFactory(child)
		if err != nil {
			return subagent.Template{}, err
		}
		names := append(tool.StaticNames(), "Finish", "SendParent")
		if lead {
			names = append(names, ChildToolNames[2:]...)
		}
		names = (Request{DisallowedTools: identity.DisallowedTools}).EnabledTools(names...)
		return subagent.Template{Workspace: identity.Workspace, MutationStateDirectory: files.StateDirectory(), Runtime: raw, Policy: permission.Config{Tools: names, FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}}, nil
	}
	workers := map[string]subagent.Template{}
	for _, effort := range WorkerEfforts {
		workers["worker-"+string(effort)], err = makeTemplate(effort, WorkerPrompt, false)
		if err != nil {
			return nil, err
		}
	}
	workers["collector"], err = makeTemplate(CollectorEffort, CollectorPrompt, false)
	if err != nil {
		return nil, err
	}
	lead, err := makeTemplate(LeadEffort, strings.NewReplacer(LeadModel, c.Identity.Provider.Model.ID, CollectorModel, c.Identity.Provider.Model.ID).Replace(LeadPrompt), true)
	if err != nil {
		return nil, err
	}
	lead.Subagents = workers
	templates := map[string]subagent.Template{"lead": lead}
	for name, worker := range workers {
		templates[name] = worker
	}
	return templates, nil
}

// The effective .env overlay is passed only through the process environment;
// neither credentials nor header values enter the durable child handshake.
func runnerEnvironment(environment []string, getenv func(string) string) []string {
	names := []string{llmAPIKeyEnvironment, llmBaseURLEnvironment, llmProviderEnvironment, llmModelEnvironment, llmHeadersEnvironment, "SANDBOX_EGRESS_PROXY", "OPENAI_CODEX_ACCESS_TOKEN", "OPENAI_CODEX_ACCOUNT_ID", "OPENAI_CODEX_AUTH_FILE", "CODEX_HOME", "SBX_CRED_OPENAI_MODE", "UNREAL_HARNESS_CREDENTIAL_ID", "UNREAL_HARNESS_CREDENTIAL_DIRECTORY", "SHELL", "HOME", "XDG_STATE_HOME"}
	for _, p := range DefaultProviders() {
		if p.APIKeyEnvironment != "" {
			names = append(names, p.APIKeyEnvironment)
		}
	}
	result := slices.Clone(environment)
	for _, name := range names {
		result = slices.DeleteFunc(result, func(entry string) bool { return strings.HasPrefix(entry, name+"=") })
		result = append(result, name+"="+getenv(name))
	}
	return result
}

// RunChild serves the existing parent-bound protocol; it is not a JSONL run.
func RunChild(ctx context.Context, args []string, input io.ReadCloser, output io.WriteCloser, diagnostics io.Writer, version string) error {
	flags := flag.NewFlagSet("unreal-agent-runner child", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	stdio := flags.Bool("stdio", false, "serve a parent-bound child")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*stdio || flags.NArg() != 0 {
		return errors.New("child requires --stdio")
	}
	factory := NewChildFactory(RuntimeConfig{}, func(child subagent.ChildConfig, runtime *RuntimeConfig) error {
		selected, err := selectProvider(DefaultProviders(), runtime.Identity.Provider.Provider)
		if err != nil {
			return err
		}
		key := os.Getenv(llmAPIKeyEnvironment)
		if key == "" {
			key = os.Getenv(selected.APIKeyEnvironment)
		}
		configured, err := sessionProviderConfig(selected, runtime.Identity.Provider.Model, key, runtime.Identity.Provider.Endpoint, runtime.Identity.Provider.MaxAttempts, os.Getenv)
		if err != nil {
			return err
		}
		runtime.Credentials = configured.Credentials
		runtime.Providers = configured.Providers
		runtime.HTTPClient, err = runnerHTTPClient(os.Getenv, string(child.ChildID), version)
		return err
	})
	return subagent.Serve(ctx, input, output, factory)
}
