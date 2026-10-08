// A native offline subprocess fixture. It never performs a network request.
package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

type configuration struct {
	Version, MissingFlag, AuthMethod, APIProvider, Subscription, Stream string
	SignedOut                                                           bool
	Exit                                                                int
	Wait, Child, IgnoreTermination                                      bool
	Gate                                                                string
	Doctor                                                              string
	ManagedTelemetry                                                    bool
	AuthStatus                                                          string
	Catalog, CatalogStream                                              string
	CatalogWait, CatalogChild, InitializeModelsOnly                     bool
	BridgeSteps                                                         []bridgeStep
	BridgeChildSteps                                                    []bridgeStep
	BridgeSource, BridgeEvent                                           string
	BridgeExtraServer                                                   bool
	BridgeOnce                                                          bool
	BridgeManagedPermissionsOnly                                        bool
	BridgePermissionRules                                               string
	StructuredResponses                                                 []string
	StructuredHelperInputs                                              []string
	StructuredAssistantFrames                                           []string
	StructuredInitializeError                                           bool
	StructuredSerializerFailures                                        int
	StructuredEnforcementReminder                                       bool
}
type bridgeStep struct {
	Name, Arguments, Contains           string
	Error, Duplicate, DuplicateEnvelope bool
}
type call struct {
	Arguments, Environment   []string
	Input, System, Directory string
	PID, ChildPID            int
	Schema                   string
	Initialize, UserFrame    string `json:",omitempty"`
}

const help = "--safe-mode --restricted --setting-sources --settings --tools --disallowedTools --allowedTools --strict-mcp-config --mcp-config --disable-slash-commands --no-chrome --no-session-persistence --model --effort --input-format --output-format --verbose --include-partial-messages --permission-prompts --json-schema"
const stream = `{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"permissionMode":"default","mcp_servers":[],"agents":["synthetic-catalog-agent"],"skills":["synthetic-catalog-skill"],"plugins":[{"name":"plugin-catalog-sensitive","path":"/plugin-catalog-sensitive","version":"version-catalog-sensitive"},{"name":"plugin-2-catalog-sensitive","path":"/plugin-2-catalog-sensitive"}]}
{"type":"system","subtype":"status","status":"requesting","permissionMode":"default"}
{"type":"system","subtype":"session_state_changed","state":"running","uuid":"lifecycle-uuid-sensitive","session_id":"lifecycle-session-sensitive"}
{"type":"system","subtype":"thinking_tokens","estimated_tokens":7,"estimated_tokens_delta":7,"uuid":"thinking-uuid-sensitive","session_id":"thinking-session-sensitive"}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"fake reply"}}}
{"type":"assistant","message":{"id":"msg_fake","model":"claude-configured-a","content":[{"type":"text","text":"fake reply"},{"type":"thinking","thinking":"private-reasoning-sensitive","signature":"private-signature-sensitive"}],"stop_reason":"end_turn"}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}
{"type":"result","subtype":"success","is_error":false,"result":"fake reply","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":40},"account_id":"account-sensitive","session_id":"claude-internal-session-sensitive","total_cost_usd":100}
`

func main() {
	path, _ := os.Executable()
	dir := filepath.Dir(path)
	b, _ := os.ReadFile(filepath.Join(dir, "fake.json"))
	var c configuration
	if json.Unmarshal(b, &c) != nil {
		os.Exit(91)
	}
	if slices.Contains(os.Args, "--fake-child") {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		<-ch
		return
	}
	args := os.Args[1:]
	switch {
	case slices.Contains(args, "--version"):
		if c.Version == "" {
			c.Version = "2.1.285"
		}
		fmt.Println(c.Version + " (Claude Code)")
		return
	case slices.Contains(args, "--help"):
		fmt.Println(strings.ReplaceAll(help, c.MissingFlag, ""))
		return
	case slices.Contains(args, "doctor"):
		// A diagnostic may initialize policy/helpers before printing its result.
		// Tests must verify that an unproven diagnostic is never auto-launched.
		if os.WriteFile(filepath.Join(dir, "doctor-called"), []byte("called"), 0600) != nil {
			os.Exit(95)
		}
		fmt.Println(c.Doctor)
		return
	case slices.Contains(args, "auth"):
		entry := call{Arguments: args, Environment: os.Environ(), PID: os.Getpid()}
		entry.Directory, _ = os.Getwd()
		log, err := os.OpenFile(filepath.Join(dir, "auth-calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(94)
		}
		encoded, _ := json.Marshal(entry)
		_, err = log.Write(append(encoded, '\n'))
		log.Close()
		if err != nil {
			os.Exit(94)
		}
		if c.AuthStatus != "" {
			fmt.Print(c.AuthStatus)
			return
		}
		if c.AuthMethod == "" {
			c.AuthMethod = "claude.ai"
		}
		if c.APIProvider == "" {
			c.APIProvider = "firstParty"
		}
		if c.Subscription == "" {
			c.Subscription = "pro"
		}
		// Native CLI 2.1.285 hides stored subscription authentication in host-
		// managed provider or simple/bare mode. No credentials are involved.
		hostManaged := os.Getenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST")
		simple := os.Getenv("CLAUDE_CODE_SIMPLE")
		signedOut := c.SignedOut || hostManaged == "1" || strings.EqualFold(hostManaged, "true") || simple == "1" || strings.EqualFold(simple, "true") || slices.Contains(args, "--bare")
		var subscription any = c.Subscription
		if signedOut {
			c.AuthMethod, subscription = "none", nil
		}
		configDir := os.Getenv("CLAUDE_CONFIG_DIR")
		if configDir == "" {
			configDir = filepath.Join(os.Getenv("HOME"), ".claude")
		}
		_ = json.MarshalWrite(os.Stdout, map[string]any{
			"loggedIn": !signedOut, "authMethod": c.AuthMethod,
			"apiProvider": c.APIProvider, "subscriptionType": subscription,
			"analyticsDisabled": false, "projectsDirectory": filepath.Join(configDir, "projects"), "configDirectory": configDir,
		})
		if signedOut {
			os.Exit(1)
		}
		return
	}
	if slices.Contains(args, `{"mcpServers":{"unreal":{"type":"sdk","name":"unreal"}}}`) {
		bridge(dir, path, args, c)
		return
	}
	for i, arg := range args {
		if arg == "--allowedTools" && i+1 < len(args) && args[i+1] == "StructuredOutput" {
			structured(dir, path, args, c)
			return
		}
	}
	if !slices.Contains(args, "-p") && slices.Contains(args, "stream-json") {
		catalog(dir, path, args, c)
		return
	}
	if !slices.Contains(args, "-p") {
		os.Exit(92)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	if c.ManagedTelemetry {
		// Simulate the CLI applying organization policy after startup. These
		// values are deliberately not supplied by the parent environment.
		_ = os.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
		_ = os.Setenv("OTEL_LOGS_EXPORTER", "otlp")
		_ = os.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
		_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://managed-telemetry-sensitive.invalid")
		_ = os.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=managed-telemetry-credential-sensitive")
		_ = os.Setenv("OTEL_LOG_TOOL_DETAILS", "1")
	}
	entry := call{Arguments: args, Environment: os.Environ(), PID: os.Getpid()}
	entry.Directory, _ = os.Getwd()
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, 8<<20+1))
	entry.Input = string(input)
	for i, arg := range args {
		if arg == "--system-prompt-file" && i+1 < len(args) {
			system, _ := os.ReadFile(args[i+1])
			entry.System = string(system)
		}
	}
	var child *exec.Cmd
	if c.Child {
		child = exec.Command(path, "--fake-child")
		child.Stdout = os.Stdout
		if child.Start() != nil {
			os.Exit(93)
		}
		entry.ChildPID = child.Process.Pid
	}
	log, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(94)
	}
	encoded, _ := json.Marshal(entry)
	encoded = append(encoded, '\n')
	_, _ = log.Write(encoded)
	log.Close()
	if c.Wait || c.Gate != "" {
		for {
			if c.Gate != "" {
				if _, e := os.Stat(c.Gate); e == nil {
					break
				}
			}
			select {
			case <-ch:
				if c.IgnoreTermination {
					continue
				}
				if child != nil {
					_ = child.Wait()
				}
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	if child != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
	}
	fmt.Fprintln(os.Stderr, "stderr-sensitive authorization: Bearer token-sensitive private-api-key-sensitive")
	if c.Stream == "" {
		c.Stream = stream
		for i, arg := range args {
			if arg == "--model" && i+1 < len(args) {
				c.Stream = strings.ReplaceAll(c.Stream, "claude-configured-a", args[i+1])
			}
		}
	}
	fmt.Print(c.Stream)
	os.Exit(c.Exit)
}

func catalog(dir, path string, args []string, c configuration) {
	entry := call{Arguments: args, Environment: os.Environ(), PID: os.Getpid()}
	entry.Directory, _ = os.Getwd()
	var child *exec.Cmd
	if c.CatalogChild {
		child = exec.Command(path, "--fake-child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(96)
		}
		entry.ChildPID = child.Process.Pid
		defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return
	}
	entry.Input = scanner.Text() + "\n"
	log := func() {
		f, err := os.OpenFile(filepath.Join(dir, "catalog-calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(94)
		}
		data, _ := json.Marshal(entry)
		_, err = f.Write(append(data, '\n'))
		f.Close()
		if err != nil {
			os.Exit(94)
		}
	}
	if c.CatalogWait {
		log()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		<-ch
		return
	}
	if c.CatalogStream != "" {
		log()
		fmt.Print(c.CatalogStream)
		return
	}
	var req struct {
		ID      string `json:"request_id"`
		Request struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if json.Unmarshal(scanner.Bytes(), &req) != nil || req.Request.Subtype != "initialize" {
		log()
		return
	}
	respond := func(id string, models jsontext.Value, success bool) {
		response := map[string]any{"request_id": id, "subtype": "error", "error": "unsupported-control-sensitive"}
		if success {
			response = map[string]any{"request_id": id, "subtype": "success", "response": map[string]any{"models": models, "account": map[string]any{"email": "account-sensitive", "token": "token-sensitive"}}}
		}
		_ = json.MarshalWrite(os.Stdout, map[string]any{"type": "control_response", "response": response})
		fmt.Println()
	}
	if c.Catalog == "" {
		log()
		respond(req.ID, nil, false)
		return
	}
	respond(req.ID, jsontext.Value(c.Catalog), true)
	if !scanner.Scan() {
		log()
		return
	}
	entry.Input += scanner.Text() + "\n"
	if json.Unmarshal(scanner.Bytes(), &req) != nil || req.Request.Subtype != "list_models" {
		log()
		return
	}
	log()
	respond(req.ID, jsontext.Value(c.Catalog), !c.InitializeModelsOnly)
}
