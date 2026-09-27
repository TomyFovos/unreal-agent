package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/viewimage"
)

const (
	defaultProvider           = "openai"
	defaultSessionDirectory   = "unreal-agent/sessions"
	llmAPIKeyEnvironment      = "UNREAL_HARNESS_LLM_API_KEY"
	llmBaseURLEnvironment     = "UNREAL_HARNESS_LLM_BASE_URL"
	llmModelEnvironment       = "UNREAL_HARNESS_LLM_MODEL"
	llmProviderEnvironment    = "UNREAL_HARNESS_LLM_PROVIDER"
	llmMaxAttemptsEnvironment = "UNREAL_HARNESS_LLM_MAX_ATTEMPTS"
)

const defaultSystemPrompt = `You are an AI agent running inside an isolated sandbox container.

## Guidelines
- Save output files to the workspace root.
- For large datasets, inspect a sample first before processing everything.
`

type Client interface {
	llm.Adapter
	Close() error
}

type Provider struct {
	Descriptor        *provider.Descriptor
	Name              string
	BaseURL           string
	DefaultModel      string
	APIKeyEnvironment string // Empty delegates authentication to NewClient.
	NewClient         func(apiKey, baseURL string, maxAttempts int, getenv func(string) string) (Client, error)
}

type Request struct {
	ProfileID      string `json:"profile_id"`
	ProfileVersion int    `json:"profile_version"`
	ModelFamily    string `json:"model_family"`

	Messages               []RequestMessage `json:"messages"`
	Prompt                 *string          `json:"prompt"`
	SystemPrompt           *string          `json:"system_prompt"`
	Model                  string           `json:"model"`
	MaxAttempts            *int             `json:"max_attempts"`
	SessionID              *string          `json:"session_id"`
	ThinkingLevel          string           `json:"thinking_level"`
	IncludePartialMessages *bool            `json:"include_partial_messages"`
	ExtraAllowedTools      []string         `json:"extra_allowed_tools"`
	DisallowedTools        []string         `json:"disallowed_tools"`
}

type RequestMessage struct {
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	MessageID *string `json:"message_id"`
}

type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type sessionObserver struct {
	sessionID session.ID
	output    io.Writer
	cancel    context.CancelFunc

	mu  sync.Mutex
	err error
}

func RunMain(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	environ func() []string,
	input io.Reader,
	output io.Writer,
	stderr io.Writer,
	config Config,
) int {
	err := Run(ctx, args, getenv, environ, input, output, stderr, config)
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if context.Cause(ctx) != nil {
		return 130
	}
	encoded, encodeErr := json.Marshal(errorEvent{Type: "error", Message: err.Error()})
	if encodeErr != nil {
		err = errors.Join(err, fmt.Errorf("encode error event: %w", encodeErr))
	} else if _, writeErr := fmt.Fprintf(output, "%s\n", encoded); writeErr != nil {
		err = errors.Join(err, fmt.Errorf("write error event: %w", writeErr))
	}
	prefix := ""
	if config.Name != "" {
		prefix = config.Name + ": "
	}
	if _, writeErr := fmt.Fprintf(stderr, "%s%v\n", prefix, err); writeErr != nil {
		return 1
	}
	return 1
}

func Run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	environ func() []string,
	input io.Reader,
	output io.Writer,
	flagOutput io.Writer,
	config Config,
) (runErr error) {
	if strings.TrimSpace(config.Name) == "" {
		return errors.New("runner name must be set")
	}
	if config.ParseRequest == nil {
		return errors.New("request parser must be set")
	}
	flags := flag.NewFlagSet(config.Name, flag.ContinueOnError)
	flags.SetOutput(flagOutput)
	var usageErr error
	flags.Usage = func() {
		usageErr = writeUsage(flags)
	}
	var prompt *string
	flags.Func("p", "send a request with the given `prompt` without reading stdin", func(value string) error {
		prompt = &value
		return nil
	})
	sessionDirectory := flags.String("session-directory", "", "directory containing session files; defaults to $XDG_STATE_HOME/unreal-agent/sessions, or $HOME/.local/state/unreal-agent/sessions")
	workspaceDirectory := flags.String("workspace", ".", "agent workspace and Bash working directory")
	logDirectory := flags.String("log-directory", "", "optional session JSONL log directory; unset writes only to stdout")
	toolHeartbeatInterval := flags.Duration("tool-heartbeat-interval", 10*time.Minute, "tool-wait heartbeat interval (0 disables)")
	if err := flags.Parse(args); err != nil {
		if usageErr != nil {
			if errors.Is(err, flag.ErrHelp) {
				return usageErr
			}
			return errors.Join(err, usageErr)
		}
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("expected at most one positional JSON request")
	}
	if prompt != nil && flags.NArg() != 0 {
		return errors.New("-p cannot be combined with a positional JSON request")
	}
	if *toolHeartbeatInterval < 0 {
		return errors.New("tool heartbeat interval must not be negative")
	}

	if prompt != nil {
		encoded, err := json.Marshal(struct {
			Prompt string `json:"prompt"`
		}{Prompt: *prompt})
		if err != nil {
			return fmt.Errorf("encode prompt request: %w", err)
		}
		input = bytes.NewReader(encoded)
	} else if flags.NArg() == 1 {
		input = strings.NewReader(flags.Arg(0))
	}
	parsed, newTools, err := config.ParseRequest(input)
	if err != nil {
		return err
	}
	if newTools == nil {
		return errors.New("request parser returned no tool factory")
	}
	messages, err := validateRequest(parsed)
	if err != nil {
		return err
	}
	workspace, err := filepath.Abs(strings.TrimSpace(*workspaceDirectory))
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("inspect workspace: %w", err)
	}
	if !workspaceInfo.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", workspace)
	}
	environment, err := loadDotEnv(filepath.Join(workspace, ".env"), getenv)
	if err != nil {
		return err
	}
	getenv = environment
	maxAttempts, err := resolveMaxAttempts(parsed.MaxAttempts, getenv)
	if err != nil {
		return err
	}
	providerName := strings.TrimSpace(getenv(llmProviderEnvironment))
	if providerName == "" {
		providerName = defaultProvider
	}
	selected, err := selectProvider(config.Providers, providerName)
	if err != nil {
		return err
	}
	configuredBaseURL := strings.TrimSpace(getenv(llmBaseURLEnvironment))
	if configuredBaseURL == "" {
		configuredBaseURL = selected.BaseURL
	}

	model := strings.TrimSpace(parsed.Model)
	if model == "" {
		model = strings.TrimSpace(getenv(llmModelEnvironment))
	}
	if model == "" {
		model = selected.DefaultModel
	}
	if model == "" {
		return fmt.Errorf("model must be set in the request or %s", llmModelEnvironment)
	}
	var apiKey string
	if selected.APIKeyEnvironment != "" {
		apiKey = getenv(llmAPIKeyEnvironment)
		if strings.TrimSpace(apiKey) == "" {
			apiKey = getenv(selected.APIKeyEnvironment)
		}
		if strings.TrimSpace(apiKey) == "" {
			return fmt.Errorf(
				"%s or %s must be set",
				llmAPIKeyEnvironment,
				selected.APIKeyEnvironment,
			)
		}
	}
	client, resolvedProvider, err := buildSessionClient(selected, model, apiKey, configuredBaseURL, maxAttempts, getenv)
	if err != nil {
		return fmt.Errorf("create %s client: %w", selected.Name, err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close %s client: %w", selected.Name, err))
		}
	}()

	storeDirectory, err := resolveSessionDirectory(*sessionDirectory, getenv)
	if err != nil {
		return fmt.Errorf("resolve session directory: %w", err)
	}
	sessionID := session.ID(uuid.New().String())
	if parsed.SessionID != nil {
		sessionID = session.ID(strings.TrimSpace(*parsed.SessionID))
	}
	observedOutput := output
	if directory := strings.TrimSpace(*logDirectory); directory != "" {
		logFile, err := openDatetimeLog(directory, time.Now())
		if err != nil {
			return err
		}
		defer func() {
			if err := logFile.Close(); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("close session log: %w", err))
			}
		}()
		observedOutput = io.MultiWriter(logFile, output)
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	// The legacy sandbox runner explicitly preserves its unrestricted execution contract.
	executionPolicy := permission.Unrestricted()
	runContext = permission.WithPolicy(runContext, executionPolicy)
	operationDirectory := filepath.Join(storeDirectory, "operations", string(sessionID))
	if err := os.MkdirAll(operationDirectory, 0o700); err != nil {
		return fmt.Errorf("create operation directory: %w", err)
	}
	shell := strings.TrimSpace(getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	skills, skillErrors := tool.DiscoverSkills(filepath.Join(workspace, ".harness", "skills"))
	names := []string{tool.BashName, tool.ViewImageName}
	if len(skills) != 0 {
		names = append(names, tool.SkillUseName)
	}
	toolConfig := ToolConfig{
		SessionID: sessionID, Getenv: getenv, Names: names, Directory: workspace,
		Translators: tool.StaticTranslators{
			Bash: bash.New(bash.Config{
				Shell:         shell,
				Directory:     workspace,
				BaseDirectory: operationDirectory,
			}),
			ViewImage: viewimage.New(viewimage.Config{Directory: workspace}),
		},
	}
	configuredTools, err := newTools(runContext, toolConfig)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		if configuredTools.Close != nil {
			runErr = errors.Join(runErr, configuredTools.Close())
		}
	}()
	registry := configuredTools.Registry
	if _, enabled := registry.Resolve(tool.SkillUseName); enabled {
		for _, skill := range skills {
			if _, err := registry.RegisterSkill(skill); err != nil {
				return fmt.Errorf("register skill %q: %w", skill.Path, err)
			}
		}
	}
	for _, skillErr := range skillErrors {
		if _, err := fmt.Fprintf(flagOutput, "skill error> %s\n", skillErr); err != nil {
			return fmt.Errorf("write skill error: %w", err)
		}
	}

	var initialInputs []inbox.Input
	settingsPayload, err := json.Marshal(inbox.ControlMessage{
		Mode: inbox.UpdateSettings,
		Parameters: inbox.Settings{
			ReasoningEffort: reasoningEffort(parsed.ThinkingLevel),
		},
	})
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	initialInputs = append(initialInputs, inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: settingsPayload})
	for index, message := range messages {
		payload, err := json.Marshal(message.Content)
		if err != nil {
			return fmt.Errorf("encode message %d: %w", index, err)
		}
		var messageID inbox.ID
		if message.MessageID != nil {
			messageID = inbox.ID(strings.TrimSpace(*message.MessageID))
		} else {
			messageID = inbox.ID(uuid.New().String())
		}
		initialInputs = append(initialInputs, inbox.Input{ID: messageID, Kind: inbox.InputExternal, Payload: payload})
	}

	stopPayload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.StopWhenIdle})
	if err != nil {
		return fmt.Errorf("encode stop request: %w", err)
	}
	initialInputs = append(initialInputs, inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: stopPayload})

	builder := contextbuilder.NewBuilder(registry.Skills()...)
	builder.SetModel(llm.Model{
		ID:              model,
		ReasoningEffort: reasoningEffort(parsed.ThinkingLevel),
	})
	systemPrompt := defaultSystemPrompt
	if parsed.SystemPrompt != nil {
		systemPrompt = *parsed.SystemPrompt
	}
	if resolvedProvider.Version == 0 {
		resolvedProvider = provider.Selection{Version: 1, Provider: selected.Name, Model: provider.Model{ID: model}, Endpoint: configuredBaseURL, MaxAttempts: maxAttempts, Source: "injected provider"}
	}
	resolvedProvider.Model.Family = parsed.ModelFamily
	resolvedProfile, err := resolveRequestProfile(parsed, resolvedProvider)
	if err != nil {
		return err
	}
	definitions := registry.StaticDefinitions()
	modelTools := make([]llm.Tool, 0, len(definitions))
	for _, definition := range definitions {
		modelTools = append(modelTools, definition.Tool)
	}
	systemPrompt, modelTools = resolvedProfile.Compose(systemPrompt, modelTools)
	builder.SetSystemPrompt(systemPrompt)
	for _, definition := range modelTools {
		builder.AddTool(definition)
	}

	currentHost, err := host.New(runContext, host.Config{Directory: storeDirectory, Build: func(sessionContext context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: builder, LLM: client, Tools: registry, Close: configuredTools.Close, Operations: operation.NewLocalOperationManager(sessionContext, configuredTools.RemoteJobs...)}, nil
	}})
	if err != nil {
		return err
	}
	defer currentHost.Close()
	identity, err := json.Marshal(struct {
		Version   int
		Provider  provider.Selection
		Profile   profile.Selection
		Workspace string
	}{1, resolvedProvider, resolvedProfile.Selection(), workspace})
	if err != nil {
		return err
	}
	current, err := currentHost.Open(runContext, host.Options{Policy: executionPolicy, Lifecycle: "one-shot", Configuration: identity, ID: sessionID, Initial: initialInputs, Heartbeat: *toolHeartbeatInterval})
	if err != nil {
		return err
	}
	if err = writeHostSession(runContext, current, observedOutput); err != nil {
		return err
	}
	if err = current.Wait(runContext); err != nil {
		return fmt.Errorf("run coordinator: %w", err)
	}
	return nil
}

// The one-shot client follows canonical history. A slow output can resync
// without blocking the session owner or holding its store lock.
func writeHostSession(ctx context.Context, current *host.Session, output io.Writer) error {
	after := current.StartAfter
	drain := func() error {
		for {
			view, err := current.Inspect(after, 256)
			if err != nil {
				return err
			}
			for _, item := range view.History.Items {
				if err = writeSessionItem(output, item); err != nil {
					return err
				}
				after = item.Sequence
			}
			if !view.History.More {
				return nil
			}
		}
	}
	for {
		sub, err := current.Subscribe(after, 256, 64)
		if err != nil {
			return err
		}
		if err = drain(); err != nil {
			sub.Cancel()
			return err
		}
		for event := range sub.Events {
			if err = drain(); err != nil {
				sub.Cancel()
				return err
			}
			if event.Kind == "gap" {
				break
			}
			if err = context.Cause(ctx); err != nil {
				sub.Cancel()
				return err
			}
		}
		sub.Cancel()
		if err = drain(); err != nil {
			return err
		}
		view, err := current.Inspect(after, 1)
		if err != nil {
			return err
		}
		if !view.Running {
			return nil
		}
	}
}

func resolveMaxAttempts(requested *int, getenv func(string) string) (int, error) {
	maxAttempts := responsesapi.DefaultMaxAttempts
	if requested != nil {
		maxAttempts = *requested
	} else if value := strings.TrimSpace(getenv(llmMaxAttemptsEnvironment)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", llmMaxAttemptsEnvironment, err)
		}
		maxAttempts = parsed
	}
	if maxAttempts <= 0 {
		return 0, errors.New("max attempts must be positive")
	}
	return maxAttempts, nil
}

func selectProvider(providers []Provider, name string) (Provider, error) {
	for _, provider := range providers {
		if provider.Name == name {
			if provider.NewClient == nil {
				return Provider{}, fmt.Errorf("provider %q has no client factory", name)
			}
			return provider, nil
		}
	}
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name)
	}
	return Provider{}, fmt.Errorf("unsupported provider %q; available providers: %s", name, strings.Join(names, ", "))
}

func DecodeRequest(input io.Reader, destination any) error {
	raw, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("empty input")
	}
	if err := json.Unmarshal(raw, destination, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func resolveSessionDirectory(configured string, getenv func(string) string) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		return filepath.Abs(configured)
	}
	stateHome := getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(stateHome) {
		userHome := getenv("HOME")
		if userHome == "" {
			var err error
			userHome, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("find home directory: %w; specify -session-directory to override", err)
			}
		}
		if !filepath.IsAbs(userHome) {
			return "", errors.New("set an absolute XDG_STATE_HOME or HOME, or specify -session-directory")
		}
		stateHome = filepath.Join(userHome, ".local", "state")
	}
	return filepath.Join(stateHome, defaultSessionDirectory), nil
}

func openDatetimeLog(directory string, now time.Time) (*os.File, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	path := filepath.Join(directory, now.UTC().Format("20060102-150405")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}
	return file, nil
}

// loadDotEnv returns a session-local overlay and never mutates the process.
// Explicit caller environment wins; sandbox proxy remains the documented file override.
func loadDotEnv(path string, parent func(string) string) (func(string) string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("read environment file failed")
	}
	values := map[string]string{}
	for line := range strings.Lines(string(encoded)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if ok && name != "" {
			values[name] = strings.TrimSpace(value)
		}
	}
	return func(name string) string {
		if value := parent(name); value != "" && name != "SANDBOX_EGRESS_PROXY" {
			return value
		}
		return values[name]
	}, nil
}

func validateRequest(parsed Request) ([]RequestMessage, error) {
	if parsed.SessionID != nil && strings.TrimSpace(*parsed.SessionID) == "" {
		return nil, errors.New("session_id must not be empty")
	}
	if parsed.ThinkingLevel != "" {
		switch parsed.ThinkingLevel {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return nil, errors.New("thinking_level must be one of: low, medium, high, xhigh, max")
		}
	}
	for _, name := range append(parsed.ExtraAllowedTools, parsed.DisallowedTools...) {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("tool names must not be empty")
		}
	}

	if parsed.Messages == nil {
		if parsed.Prompt == nil {
			return nil, errors.New("messages must be set")
		}
		return []RequestMessage{{Content: *parsed.Prompt}}, nil
	}
	if len(parsed.Messages) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	for index, message := range parsed.Messages {
		if message.Role != "" && message.Role != "user" {
			return nil, fmt.Errorf("messages[%d].role must be user", index)
		}
		if message.MessageID != nil && strings.TrimSpace(*message.MessageID) == "" {
			return nil, fmt.Errorf("messages[%d].message_id must not be empty", index)
		}
		if message.MessageID != nil {
			if _, err := uuid.Parse(strings.TrimSpace(*message.MessageID)); err != nil {
				return nil, fmt.Errorf("messages[%d].message_id must be a UUID", index)
			}
		}
	}
	return parsed.Messages, nil
}

func reasoningEffort(level string) llm.ReasoningEffort {
	switch level {
	case "low":
		return llm.ReasoningEffortLow
	case "medium":
		return llm.ReasoningEffortMedium
	case "xhigh":
		return llm.ReasoningEffortXHigh
	case "max":
		return llm.ReasoningEffortMax
	default:
		return llm.ReasoningEffortHigh
	}
}

func openSession(
	ctx context.Context,
	store *localfile.Store,
	requested *string,
) (session.ID, sessionstore.ResumeState, error) {
	id := session.ID(uuid.New().String())
	if requested != nil {
		id = session.ID(strings.TrimSpace(*requested))
	}
	restored, err := store.Resume(ctx, id)
	if err == nil {
		return id, restored, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", sessionstore.ResumeState{}, fmt.Errorf("open session %q: %w", id, err)
	}
	snapshot, err := store.Create(ctx, id)
	if err != nil {
		return "", sessionstore.ResumeState{}, fmt.Errorf("create session %q: %w", id, err)
	}
	return id, sessionstore.ResumeState{Snapshot: snapshot}, nil
}

func (observer *sessionObserver) Observe(sessionID session.ID, item sessionstore.Item) {
	if sessionID != observer.sessionID {
		return
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.err != nil {
		return
	}
	if err := writeSessionItem(observer.output, item); err != nil {
		observer.fail(err)
		return
	}
}

func writeSessionItem(output io.Writer, item sessionstore.Item) error {
	encoded, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode session item %d: %w", item.Sequence, err)
	}
	if _, err := fmt.Fprintf(output, "%s\n", encoded); err != nil {
		return fmt.Errorf("write session item %d: %w", item.Sequence, err)
	}
	return nil
}

func (observer *sessionObserver) fail(err error) {
	observer.err = err
	observer.cancel()
}

func (observer *sessionObserver) Err() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.err
}
