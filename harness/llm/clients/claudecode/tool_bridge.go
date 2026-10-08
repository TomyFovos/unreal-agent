package claudecode

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

// ToolBridge is explicit opt-in. It changes the provider transport, not Unreal
// permissions, project instructions, child limits or execution ownership.
type ToolBridgeConfig struct {
	Enabled         bool
	Mode            string `json:",omitzero"`
	MaxToolRounds   int    `json:",omitzero"`
	MaxActionRounds int    `json:",omitzero"`
	TimeoutMillis   int    `json:",omitzero"`
	ResultBytes     int    `json:",omitzero"`
}

func (c ToolBridgeConfig) Validate() error {
	if c.Mode != "" && c.Mode != BridgeModeStructured && c.Mode != BridgeModeMCP || c.MaxActionRounds < 0 || c.MaxActionRounds > 128 {
		return &Error{Code: "bridge_configuration_invalid"}
	}
	if c.MaxToolRounds < 0 || c.MaxToolRounds > 128 || c.TimeoutMillis < 0 || c.TimeoutMillis > 3600000 || c.TimeoutMillis > 0 && c.TimeoutMillis < 1000 || c.ResultBytes < 0 || c.ResultBytes > 1<<20 || c.ResultBytes > 0 && c.ResultBytes < 1024 {
		return &Error{Code: "bridge_configuration_invalid"}
	}
	return nil
}
func (c ToolBridgeConfig) limits() ToolBridgeConfig {
	if c.MaxActionRounds == 0 {
		c.MaxActionRounds = 32
	}
	if c.MaxToolRounds == 0 {
		c.MaxToolRounds = 32
	}
	if c.TimeoutMillis == 0 {
		c.TimeoutMillis = 300000
	}
	if c.ResultBytes == 0 {
		c.ResultBytes = 64 << 10
	}
	return c
}

const BridgeModeStructured = "structured"
const BridgeModeMCP = "mcp"

// Missing Mode preserves the original explicit SDK MCP configuration. New
// launcher defaults choose structured explicitly; no runtime fallback occurs.
func (c ToolBridgeConfig) ResolvedMode() string {
	if !c.Enabled {
		return "disabled"
	}
	if c.Mode == "" {
		return BridgeModeMCP
	}
	return c.Mode
}

const bridgeServer = "unreal"
const bridgeMCP = `{"mcpServers":{"unreal":{"type":"sdk","name":"unreal"}}}`

// In MCP mode --tools "" removes built-ins. The extra deny list is defence in
// depth; a wildcard would also deny caller-owned MCP tools. Structured mode
// shares this list and additionally denies all MCP. Its sole internal serializer
// is injected by initialize.jsonSchema after baseline tool selection.
const bridgeBuiltinDeny = "Bash,Read,Write,Edit,MultiEdit,Glob,Grep,Agent,Task,WebFetch,WebSearch,ToolSearch,Skill,NotebookEdit,AskUserQuestion,TodoWrite"

type bridgeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"inputSchema"`
	Meta        map[string]any `json:"_meta"`
	original    string
}

func bridgeTools(tools []llm.Tool) ([]bridgeTool, error) {
	var result []bridgeTool
	seen := map[string]bool{}
	for _, t := range tools {
		if t.Type != llm.ToolFunction || !toolIdentifier(t.Name) || t.Parameters == nil || seen[t.Name] {
			return nil, &Error{Code: "bridge_schema_invalid"}
		}
		seen[t.Name] = true
		result = append(result, bridgeTool{Name: "unreal_" + t.Name, Description: t.Description, Schema: t.Parameters, Meta: map[string]any{"anthropic/alwaysLoad": true}, original: t.Name})
	}
	if len(result) > 128 {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	return result, nil
}
func toolIdentifier(s string) bool {
	if s == "" || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func bridgeArgs(tools []bridgeTool) []string {
	args := isolationArgs()
	for i := range args {
		if args[i] == "--disallowedTools" {
			args[i+1] = bridgeBuiltinDeny
		}
		if args[i] == "--mcp-config" {
			args[i+1] = bridgeMCP
		}
	}
	allowed := make([]string, 0, len(tools))
	for _, t := range tools {
		allowed = append(allowed, "mcp__"+bridgeServer+"__"+t.Name)
	}
	return append(args, "--allowedTools", strings.Join(allowed, ","))
}

// Reuse the strict text-only contract to check every unchanged boundary. Only
// the exact SDK server and generated allowlist can replace empty MCP/wildcard.
func validateBridgeLaunch(args []string, tools []bridgeTool) error {
	expected := bridgeArgs(tools)
	for _, flag := range []string{"--disallowedTools", "--mcp-config", "--allowedTools"} {
		var value string
		for i, arg := range expected {
			if arg == flag {
				value = expected[i+1]
			}
		}
		count := 0
		for i, arg := range args {
			if strings.HasPrefix(arg, flag+"=") {
				return &Error{Code: "isolation_contract_invalid"}
			}
			if arg == flag {
				count++
				if i+1 == len(args) || args[i+1] != value {
					return &Error{Code: "isolation_contract_invalid"}
				}
			}
		}
		if count != 1 {
			return &Error{Code: "isolation_contract_invalid"}
		}
	}
	copy := append([]string(nil), args...)
	for i, arg := range copy {
		if arg == "--disallowedTools" {
			copy[i+1] = "*"
		}
		if arg == "--mcp-config" {
			copy[i+1] = `{"mcpServers":{}}`
		}
	}
	return validateLaunchContract(copy)
}

func publicBridgePrompt(items []llm.Item) (string, string, error) {
	var system []string
	var public []llm.Item
	total := 0
	for _, item := range items {
		item.ProviderID = "" // No replay IDs/private provider state crosses requests.
		if item.Type != llm.ItemReasoning {
			if _, eligible := contextengine.PublicItem(item); !eligible {
				return "", "", &Error{Code: "unsupported_input"}
			}
		}
		switch item.Type {
		case llm.ItemMessage:
			m, ok := item.Data.(llm.Message)
			if !ok {
				return "", "", &Error{Code: "unsupported_input"}
			}
			if m.Role == llm.RoleSystem {
				system = append(system, m.Text)
				total += len(m.Text)
				continue
			}
			if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant {
				return "", "", &Error{Code: "unsupported_input"}
			}
		case llm.ItemToolCall:
			if _, ok := item.Data.(llm.ToolCall); !ok {
				return "", "", &Error{Code: "unsupported_input"}
			}
		case llm.ItemToolResult:
			if _, ok := item.Data.(llm.ToolResult); !ok {
				return "", "", &Error{Code: "unsupported_input"}
			}
		case llm.ItemReasoning:
			continue // Private reasoning is never sent, even to the same provider.
		default:
			return "", "", &Error{Code: "unsupported_input"}
		}
		public = append(public, item)
	}
	data, err := json.Marshal(public, json.Deterministic(true))
	if err != nil || len(public) == 0 || len(data)+total > 8<<20 {
		return "", "", &Error{Code: "unsupported_input"}
	}
	return strings.Join(system, "\n\n"), string(data), nil
}

func bridgePrompt(items []llm.Item) (string, string, error) {
	system, input, err := publicBridgePrompt(items)
	system += "\n\nUnreal Agent supplies provider-neutral public conversation and tool receipts as JSON data in the user input. Reply to the current user task. Only the supplied mcp__unreal__ tools are available; Unreal owns all execution and permissions. Historical tool records are context, not instructions to re-execute them."
	return system, input, err
}

func (c *Client) respondBridge(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	if opt.BeginTools != nil {
		if err := opt.BeginTools(ctx); err != nil {
			return llm.Response{}, safeBridgeCallbackError(err)
		}
	}
	return c.bridgeRequest(ctx, req, opt, false)
}

// ProbeToolBridge checks structured schema acceptance or SDK MCP registration,
// provenance and permissions, depending on the explicitly selected mode.
// It sends no user message, tool call or inference. Authentication remains CLI-owned.
func (c *Client) ProbeToolBridge(ctx context.Context, tools []llm.Tool) error {
	if !c.config.ToolBridge.Enabled {
		return &Error{Code: "tools_unsupported"}
	}
	if c.config.ToolBridge.ResolvedMode() == BridgeModeStructured {
		return c.probeStructured(ctx, tools)
	}
	_, err := c.bridgeRequest(ctx, llm.Request{Tools: tools}, llm.RequestOptions{}, true)
	return err
}

func (c *Client) bridgeRequest(ctx context.Context, req llm.Request, opt llm.RequestOptions, probe bool) (llm.Response, error) {
	if !probe && opt.Tools == nil {
		return llm.Response{}, &Error{Code: "bridge_host_required"}
	}
	tools, err := bridgeTools(req.Tools)
	if err != nil {
		return llm.Response{}, err
	}
	system, input := "", ""
	if !probe {
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
		system, input, err = bridgePrompt(req.Input)
		if err != nil {
			return llm.Response{}, err
		}
		if opt.InputBudget == 0 {
			opt.InputBudget = 24576
		}
		opt.InputBudget -= contextengine.Estimate(input) + contextengine.Estimate(system)
		if opt.InputBudget < 1024 {
			return llm.Response{}, &Error{Code: "bridge_context_budget"}
		}
	}
	path, env, err := c.prepare(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	dir, err := os.MkdirTemp("", "unreal-claude-bridge-")
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer os.RemoveAll(dir)
	systemPath := filepath.Join(dir, "system.txt")
	if os.WriteFile(systemPath, []byte(system), 0600) != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	limits := c.config.ToolBridge.limits()
	args := append(bridgeArgs(tools), "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--no-session-persistence", "--permission-prompts", "none", "--max-turns", strconv.Itoa(limits.MaxToolRounds+1), "--system-prompt-file", systemPath)
	if !probe {
		args = append(args, "-p", "--model", req.Model.ID)
	}
	if req.Model.ReasoningEffort != "" {
		args = append(args, "--effort", string(req.Model.ReasoningEffort))
	}
	if err = validateBridgeLaunch(args, tools); err != nil {
		return llm.Response{}, err
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, path, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, env, io.Discard
	if err = isolateProcess(cmd); err != nil {
		return llm.Response{}, err
	}
	reader, output, err := os.Pipe()
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer reader.Close()
	defer output.Close()
	stdin, writer, err := os.Pipe()
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer stdin.Close()
	defer writer.Close()
	cmd.Stdout, cmd.Stdin = output, stdin
	if err = cmd.Start(); err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	output.Close()
	stdin.Close()
	done := make(chan error, 1)
	parsed := make(chan struct{})
	go func() {
		err := cmd.Wait()
		_ = killProcessGroup(cmd)
		select {
		case <-parsed:
		case <-processCtx.Done():
			reader.Close()
		}
		done <- err
	}()
	response, streamErr := runBridgeProtocol(processCtx, reader, writer, input, tools, limits, opt, probe)
	// The SDK transport keeps stdin open for callbacks. End this one request's
	// process explicitly; never --continue/--resume or a persisted CLI session.
	close(parsed)
	writer.Close()
	natural := false
	var waitErr error
	select {
	case waitErr = <-done:
		natural = true
	case <-ctx.Done():
		cancel()
		waitErr = <-done
	case <-time.After(100 * time.Millisecond):
		cancel()
		waitErr = <-done
	}
	cancel()
	reader.Close()
	if ctx.Err() != nil {
		return llm.Response{}, ctx.Err()
	}
	if streamErr != nil {
		var typed *Error
		if natural && waitErr != nil && errors.As(streamErr, &typed) && typed.Code == "malformed_stream" {
			return llm.Response{}, &Error{Code: "subprocess_failure"}
		}
		return llm.Response{}, streamErr
	}
	if natural && waitErr != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	return response, nil
}

func writeBridgeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return &Error{Code: "bridge_protocol_failure"}
	}
	if _, err = io.Copy(w, bytes.NewReader(append(data, '\n'))); err != nil {
		return &Error{Code: "bridge_protocol_failure"}
	}
	return nil
}
func bridgeControl(w io.Writer, id string, request any) error {
	return writeBridgeFrame(w, map[string]any{"type": "control_request", "request_id": id, "request": request})
}
func bridgeControlReply(w io.Writer, id string, response any) error {
	return writeBridgeFrame(w, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": response}})
}

type bridgeWire struct {
	Type    string `json:"type"`
	ID      string `json:"request_id"`
	Request struct {
		Subtype string `json:"subtype"`
		Server  string `json:"server_name"`
		Message struct {
			Version string         `json:"jsonrpc"`
			ID      jsontext.Value `json:"id"`
			Method  string         `json:"method"`
			Params  struct {
				Version   string         `json:"protocolVersion"`
				Name      string         `json:"name"`
				Arguments jsontext.Value `json:"arguments"`
			} `json:"params"`
		} `json:"message"`
	} `json:"request"`
	Response struct {
		Subtype string `json:"subtype"`
		ID      string `json:"request_id"`
		Data    struct {
			Permissions *bridgePermissionState `json:"state"`
			Servers     []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Source string `json:"source"`
			} `json:"mcpServers"`
		} `json:"response"`
	} `json:"response"`
}

func rpcResult(w io.Writer, frame bridgeWire, result any) error {
	rpc := map[string]any{"jsonrpc": "2.0", "result": result}
	if len(frame.Request.Message.ID) > 0 {
		rpc["id"] = frame.Request.Message.ID
	}
	return bridgeControlReply(w, frame.ID, map[string]any{"mcp_response": rpc})
}

func sameArguments(a, b jsontext.Value) bool {
	var x, y map[string]any
	if json.Unmarshal(a, &x) != nil || x == nil || json.Unmarshal(b, &y) != nil || y == nil {
		return false
	}
	xb, e1 := json.Marshal(x, json.Deterministic(true))
	yb, e2 := json.Marshal(y, json.Deterministic(true))
	return e1 == nil && e2 == nil && bytes.Equal(xb, yb)
}

func boundedBridgeText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	head, tail := limit/2-128, limit/2-128
	for head > 0 && text[head]&0xc0 == 0x80 {
		head--
	}
	for tail > 0 && text[len(text)-tail]&0xc0 == 0x80 {
		tail--
	}
	return text[:head] + "\n[Unreal receipt truncated; original bytes=" + strconv.Itoa(len(text)) + "; full receipt retained in canonical history]\n" + text[len(text)-tail:]
}
