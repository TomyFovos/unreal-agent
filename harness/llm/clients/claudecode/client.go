package claudecode

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// Config contains only executable/catalog and trust-boundary metadata. Auth
// discovery is delegated to Claude with a restricted environment; no tokens
// enter this configuration.
type Config struct {
	Binary            string                                              `json:",omitzero"`
	Models            []modelcatalog.Model                                `json:",omitzero"`
	ManagedPolicyMode ManagedPolicyMode                                   `json:"managedPolicyMode,omitzero"`
	ToolBridge        ToolBridgeConfig                                    `json:",omitzero"`
	Getenv            func(string) string                                 `json:"-"`
	CatalogSource     func(context.Context) (modelcatalog.Catalog, error) `json:"-"`
	CurrentModel      llm.Model                                           `json:"-"`
	// AuthorizeProcess is supplied by a Host-authorized isolated provider or
	// bounded child transport. It grants no tool/process Operation capability.
	AuthorizeProcess func(context.Context) error `json:"-"`
}

type Client struct {
	config   Config
	catalog  modelcatalog.Catalog
	cacheMu  sync.Mutex
	cached   modelcatalog.Catalog
	cacheKey string
	expires  time.Time
}

func NewClient(c Config) (*Client, error) {
	if err := c.ToolBridge.Validate(); err != nil {
		return nil, err
	}
	if err := c.ManagedPolicyMode.Validate(); err != nil {
		return nil, err
	}
	catalog, err := modelcatalog.Configured(c.Models)
	if err != nil {
		return nil, &Error{Code: "invalid_model"}
	}
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	return &Client{config: c, catalog: catalog}, nil
}
func (c *Client) Close() error { return nil }

// Probe executes only version/help/auth status, never a model request. The same
// checks run for every Respond, including after a subscription login changes.
func (c *Client) Probe(ctx context.Context) error {
	_, _, err := c.prepare(ctx)
	return err
}

func (c *Client) environment() ([]string, error) {
	getenv := c.config.Getenv
	// An unrelated parent API key is dropped, not used and not logged. Explicit
	// routing/billing switches are an error instead of a silent mode change.
	for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS"} {
		v := strings.ToLower(strings.TrimSpace(getenv(key)))
		if v != "" && v != "0" && v != "false" {
			return nil, &Error{Code: "subscription_mode_conflict"}
		}
	}
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_AWS_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_CONFIG_DIR", "CLAUDE_CODE_CLIENT_DATA_URL"} {
		if getenv(key) != "" {
			return nil, &Error{Code: "subscription_mode_conflict"}
		}
	}
	home := getenv("HOME")
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") {
		return nil, &Error{Code: "subscription_unavailable"}
	}
	dir := getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\x00\r\n") {
		return nil, &Error{Code: "subscription_unavailable"}
	}
	if c.config.ManagedPolicyMode.Effective() == ManagedPolicyReject {
		if err := checkPolicySources(home, dir); err != nil {
			return nil, err
		}
	}
	// The CLI owns subscription authentication. In 2.1.285, setting
	// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST hides its stored login; this adapter
	// supplies no host credentials. Session/tool ownership is enforced by the
	// Unreal runtime and isolation flags, independently of that provider marker.
	env := []string{
		"HOME=" + home, "CLAUDE_CONFIG_DIR=" + dir, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TERM=dumb", "NO_COLOR=1",
		"CLAUDE_CODE_SAFE_MODE=1", "CLAUDE_CODE_SKIP_PROMPT_HISTORY=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_COMPACT=1", "DISABLE_AUTO_COMPACT=1", "CLAUDE_AGENT_SDK_DISABLE_BUILTIN_AGENTS=1", "CLAUDE_CODE_AUTO_CONNECT_IDE=false", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false",
	}
	if c.config.ManagedPolicyMode.Effective() == ManagedPolicyReject {
		env = append(env, "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	}
	// In trust mode Claude, not Unreal, applies organization telemetry/env.
	// Parent OTEL_*, routing and credential variables remain outside the whitelist.
	return env, nil
}

const settings = `{"disableAllHooks":true,"autoMemoryEnabled":false,"disableClaudeAiConnectors":true,"syncClaudeAiSkills":false,"syncClaudeAiPlugins":false}`

func isolationArgs() []string {
	return []string{"--safe-mode", "--restricted", "--setting-sources", "", "--settings", settings, "--tools", "", "--disallowedTools", "*", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands", "--no-chrome"}
}

func (c *Client) prepare(ctx context.Context) (string, []string, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	authorize := c.config.AuthorizeProcess
	if authorize == nil {
		authorize = func(ctx context.Context) error { return permission.FromContext(ctx).CheckProcess() }
	}
	if err := authorize(ctx); err != nil {
		return "", nil, err
	}
	env, err := c.environment()
	if err != nil {
		return "", nil, err
	}
	name := c.config.Binary
	if name == "" {
		name = "claude"
	}
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", nil, &Error{Code: "binary_not_found"}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", nil, &Error{Code: "binary_not_found"}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", nil, &Error{Code: "binary_not_found"}
	}
	// Version/help execute outside the workspace and with the same clean env.
	version, _, err := probeCommand(ctx, path, env, []string{"--version"})
	if err != nil {
		return "", nil, err
	}
	fields := strings.Fields(string(version))
	if len(fields) == 0 || !supportedVersion(fields[0]) {
		return "", nil, &Error{Code: "unsupported_version"}
	}
	help, _, err := probeCommand(ctx, path, env, []string{"--help"})
	if err != nil {
		return "", nil, err
	}
	for _, flag := range []string{"--safe-mode", "--restricted", "--setting-sources", "--settings", "--tools", "--disallowedTools", "--strict-mcp-config", "--mcp-config", "--disable-slash-commands", "--no-chrome", "--no-session-persistence", "--model", "--effort", "--input-format", "--output-format", "--verbose", "--include-partial-messages", "--permission-prompts"} {
		if !strings.Contains(string(help), flag) {
			return "", nil, &Error{Code: "unsupported_version"}
		}
	}
	if c.config.ToolBridge.Enabled && !strings.Contains(string(help), "--allowedTools") {
		return "", nil, &Error{Code: "unsupported_version"}
	}
	if c.config.ToolBridge.ResolvedMode() == BridgeModeStructured && !strings.Contains(string(help), "--json-schema") {
		return "", nil, &Error{Code: "structured_unavailable"}
	}
	// These official flags are hidden from some --help versions. Exercise the
	// installed parser using auth status, which performs no inference.
	args := append(isolationArgs(), "--system-prompt-file", os.DevNull, "--max-turns", "1", "auth", "status")
	if err := validateLaunchContract(args); err != nil {
		return "", nil, err
	}
	status, code, err := probeCommand(ctx, path, env, args)
	if err != nil {
		return "", nil, err
	}
	var auth struct {
		LoggedIn     bool   `json:"loggedIn"`
		Method       string `json:"authMethod"`
		Provider     string `json:"apiProvider"`
		Subscription string `json:"subscriptionType"`
	}
	if json.Unmarshal(status, &auth) != nil {
		return "", nil, &Error{Code: "subprocess_failure"}
	}
	// The verified CLI contract is exit 0 when logged in, 1 when not. A false
	// loggedIn field also fails closed even if a subprocess reports exit 0.
	if code == 1 || !auth.LoggedIn {
		return "", nil, &Error{Code: "external_reauth_required"}
	}
	if code != 0 {
		return "", nil, &Error{Code: "subprocess_failure"}
	}
	if auth.Method != "claude.ai" || auth.Provider != "firstParty" {
		return "", nil, &Error{Code: "subscription_unavailable"}
	}
	if err = checkSubscriptionPolicy(auth.Subscription, c.config.ManagedPolicyMode); err != nil {
		return "", nil, err
	}
	return path, env, nil
}

func supportedVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	var n [3]int
	for i, p := range parts {
		v, e := strconv.Atoi(p)
		if e != nil || v < 0 {
			return false
		}
		n[i] = v
	}
	// Contract verified against 2.1.285; later 2.1 patches must also pass the
	// required flag/parser probes. Unknown major/minor contracts fail closed.
	return n[0] == 2 && n[1] == 1 && n[2] >= 285
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:max(0, remaining)]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func probeCommand(ctx context.Context, path string, env, args []string) ([]byte, int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "unreal-claude-probe-")
	if err != nil {
		return nil, 0, &Error{Code: "subprocess_failure"}
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(probeCtx, path, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, env, io.Discard
	if err = isolateProcess(cmd); err != nil {
		return nil, 0, err
	}
	output := &boundedOutput{limit: 1 << 20}
	cmd.Stdout = output
	err = cmd.Run()
	if e := ctx.Err(); e != nil {
		return nil, 0, e
	}
	if probeCtx.Err() != nil {
		return nil, 0, &Error{Code: "subprocess_failure"}
	}
	if output.overflow {
		return nil, 0, &Error{Code: "subprocess_failure"}
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, 0, &Error{Code: "subprocess_failure"}
		}
		if slices.Contains(args, "auth") {
			return output.Bytes(), exit.ExitCode(), nil
		}
		return nil, exit.ExitCode(), &Error{Code: "unsupported_version"}
	}
	return output.Bytes(), 0, nil
}

func (c *Client) Respond(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	if c.config.ToolBridge.Enabled {
		if c.config.ToolBridge.ResolvedMode() == BridgeModeStructured {
			return c.respondStructured(ctx, req, opt)
		}
		return c.respondBridge(ctx, req, opt)
	}
	if len(req.Tools) != 0 {
		return llm.Response{}, &Error{Code: "tools_unsupported", toolReason: toolRequestSchemas}
	}
	catalog, err := c.Catalog(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	m, err := catalog.FindSelection(req.Model.ID)
	if err != nil && !catalog.Authoritative && req.Model.ID == c.config.CurrentModel.ID && req.Model.ReasoningEffort == c.config.CurrentModel.ReasoningEffort {
		m, err = modelcatalog.Model{ID: req.Model.ID, Efforts: []llm.ReasoningEffort{req.Model.ReasoningEffort}}, nil
	}
	if err != nil {
		return llm.Response{}, &Error{Code: "invalid_model"}
	}
	if !m.AllowsEffort(req.Model.ReasoningEffort) {
		return llm.Response{}, &Error{Code: "invalid_effort"}
	}
	if req.Model.MaxOutputTokens != nil {
		return llm.Response{}, &Error{Code: "unsupported_input"}
	}
	system, input, err := prompt(req.Input)
	if err != nil {
		return llm.Response{}, err
	}
	path, env, err := c.prepare(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	dir, err := os.MkdirTemp("", "unreal-claude-request-")
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer os.RemoveAll(dir)
	// Only a private temporary path appears in argv. Prompt contents are never
	// process arguments or part of a Claude-owned persisted session.
	systemPath := filepath.Join(dir, "system.txt")
	if err = os.WriteFile(systemPath, []byte(system), 0600); err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	args := append(isolationArgs(), "-p", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--no-session-persistence", "--permission-prompts", "none", "--max-turns", "1", "--system-prompt-file", systemPath, "--model", req.Model.ID)
	if req.Model.ReasoningEffort != "" {
		args = append(args, "--effort", string(req.Model.ReasoningEffort))
	}
	if err := validateLaunchContract(args); err != nil {
		return llm.Response{}, err
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, path, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stderr = strings.NewReader(input), io.Discard
	if err = isolateProcess(cmd); err != nil {
		return llm.Response{}, err
	}
	// An os.Pipe assigned directly to Stdout avoids exec's copier goroutines
	// and allows explicit close when an orphan holds the write descriptor.
	reader, writer, err := os.Pipe()
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer reader.Close()
	cmd.Stdout = writer
	if err = cmd.Start(); err != nil {
		writer.Close()
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	writer.Close()
	done := make(chan struct{})
	parsed := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		_ = killProcessGroup(cmd)
		select {
		case <-parsed:
		case <-processCtx.Done():
			reader.Close()
		case <-time.After(2 * time.Second):
			reader.Close()
		}
		close(done)
	}()
	if opt.Progress != nil {
		opt.Progress(llm.Progress{Attempt: 1, Reset: true})
	}
	response, streamErr := parseStream(reader, opt)
	close(parsed)
	if streamErr != nil {
		cancel()
	}
	<-done
	if err = ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if streamErr != nil {
		return llm.Response{}, streamErr
	}
	if waitErr != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	return response, nil
}

func prompt(items []llm.Item) (string, string, error) {
	var system []string
	var messages []llm.Message
	total := 0
	for _, item := range items {
		if item.Type != llm.ItemMessage {
			return "", "", &Error{Code: "unsupported_input"}
		}
		m, ok := item.Data.(llm.Message)
		if !ok {
			return "", "", &Error{Code: "unsupported_input"}
		}
		total += len(m.Text)
		if total > 8<<20 {
			return "", "", &Error{Code: "unsupported_input"}
		}
		switch m.Role {
		case llm.RoleSystem:
			system = append(system, m.Text)
		case llm.RoleUser, llm.RoleAssistant:
			messages = append(messages, m)
		default:
			return "", "", &Error{Code: "unsupported_input"}
		}
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != llm.RoleUser {
		return "", "", &Error{Code: "unsupported_input"}
	}
	if len(messages) == 1 {
		return strings.Join(system, "\n\n"), messages[0].Text, nil
	}
	// The CLI accepts one user input, not an externally restored role history.
	// Serialize the Context Builder's text messages as data in that single input;
	// this is not a tool protocol and no tool-shaped output is interpreted.
	data, err := json.Marshal(messages)
	if err != nil {
		return "", "", &Error{Code: "unsupported_input"}
	}
	system = append(system, "Unreal Agent supplies the canonical conversation as a JSON array of Role/Text messages in the user input. Use it as conversation context and reply to the final user message. Tools are unavailable.")
	return strings.Join(system, "\n\n"), string(data), nil
}
