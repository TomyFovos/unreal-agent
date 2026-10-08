package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Offline SDK/MCP wire fixture. Tool steps send requests and inspect receipts;
// they never read/edit workspace files or run workspace commands themselves.
func bridge(dir, path string, args []string, c configuration) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 8192), 16<<20)
	read := func() map[string]any {
		if !scanner.Scan() {
			os.Exit(0)
		}
		var v map[string]any
		if json.Unmarshal(scanner.Bytes(), &v) != nil {
			os.Exit(110)
		}
		return v
	}
	emit := func(v any) {
		if json.MarshalWrite(os.Stdout, v) != nil {
			os.Exit(111)
		}
		fmt.Println()
	}
	reply := func(id string, data any) {
		emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": data}})
	}
	mcpEnvelope := func(id, rpcID, method string, params any) map[string]any {
		message := map[string]any{"jsonrpc": "2.0", "id": rpcID, "method": method}
		if params != nil {
			message["params"] = params
		}
		frame := map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "mcp_message", "server_name": "unreal", "message": message}}
		emit(frame)
		for {
			v := read()
			if v["type"] == "control_response" {
				return v
			}
			if v["type"] == "control_request" {
				request := v["request"].(map[string]any)
				if request["subtype"] != "mcp_status" {
					os.Exit(112)
				}
				reply(v["request_id"].(string), map[string]any{"mcpServers": []any{}})
			}
		}
	}
	mcp := func(id, method string, params any) map[string]any { return mcpEnvelope(id, id, method, params) }
	init := read()
	if init["type"] != "control_request" || init["request"].(map[string]any)["subtype"] != "initialize" {
		os.Exit(113)
	}
	_ = mcp("mcp-init", "initialize", map[string]any{"protocolVersion": "2025-11-25"})
	_ = mcp("mcp-notification", "notifications/initialized", nil)
	listed := mcp("mcp-list", "tools/list", nil)
	reply(init["request_id"].(string), map[string]any{"account": map[string]any{"token": "token-sensitive", "email": "account-sensitive"}})
	for {
		v := read()
		if v["type"] == "user" {
			init = v
			break
		}
		request, ok := v["request"].(map[string]any)
		if !ok {
			os.Exit(114)
		}
		if request["subtype"] == "list_permission_rules" {
			var rules []any = []any{}
			if c.BridgePermissionRules != "" {
				if json.Unmarshal([]byte(c.BridgePermissionRules), &rules) != nil {
					os.Exit(114)
				}
			} else if !c.BridgeManagedPermissionsOnly {
				for i, arg := range args {
					if arg == "--allowedTools" && i+1 < len(args) {
						for _, name := range strings.Split(args[i+1], ",") {
							if name != "" {
								rules = append(rules, map[string]any{"behavior": "allow", "source": "cliArg", "rule": name, "editability": "session"})
							}
						}
					}
				}
			}
			reply(v["request_id"].(string), map[string]any{"state": map[string]any{"rules": rules, "managedOnly": c.BridgeManagedPermissionsOnly}})
			continue
		}
		if request["subtype"] != "mcp_status" {
			os.Exit(114)
		}
		source := c.BridgeSource
		if source == "" {
			source = "sdk"
		}
		servers := []any{map[string]any{"name": "unreal", "status": "connected", "source": source}}
		if c.BridgeExtraServer {
			servers = append(servers, map[string]any{"name": "not-owned", "status": "connected", "source": "managed"})
		}
		reply(v["request_id"].(string), map[string]any{"mcpServers": servers})
	}
	entry := call{Arguments: args, Environment: os.Environ(), PID: os.Getpid()}
	entry.Directory, _ = os.Getwd()
	message := init["message"].(map[string]any)
	entry.Input, _ = message["content"].(string)
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
			os.Exit(115)
		}
		entry.ChildPID = child.Process.Pid
		defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	}
	log, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(116)
	}
	data, _ := json.Marshal(entry)
	_, _ = log.Write(append(data, '\n'))
	log.Close()
	// Persist only test-controlled protocol data, never an actual account/token.
	b, _ := json.Marshal(listed)
	_ = os.WriteFile(filepath.Join(dir, "bridge-list.json"), b, 0600)
	if c.Wait {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
		<-signals
		return
	}
	for c.Gate != "" {
		if _, err := os.Stat(c.Gate); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if c.BridgeEvent != "" {
		fmt.Println(c.BridgeEvent)
		return
	}
	model := "claude-configured-a"
	for i, arg := range args {
		if arg == "--model" && i+1 < len(args) {
			model = args[i+1]
		}
	}
	available := []string{}
	if strings.Contains(entry.System, "CHILD_GUIDANCE") {
		c.BridgeSteps = c.BridgeChildSteps
	} else if c.BridgeOnce && strings.Contains(entry.Input, "Bridge fixture completed") {
		c.BridgeSteps = nil
	}
	for _, s := range c.BridgeSteps {
		available = append(available, "mcp__unreal__unreal_"+s.Name)
	}
	emit(map[string]any{"type": "system", "subtype": "init", "model": model, "tools": available, "mcp_servers": []any{map[string]any{"name": "unreal", "status": "connected"}}, "permissionMode": "default", "plugins": []any{map[string]any{"name": "plugin-catalog-sensitive"}}})
	digest := ""
	for n, step := range c.BridgeSteps {
		arguments := strings.ReplaceAll(step.Arguments, "$REVISION", digest)
		id := fmt.Sprintf("tool-%d", n)
		name := "mcp__unreal__unreal_" + step.Name
		emit(map[string]any{"type": "assistant", "message": map[string]any{"id": fmt.Sprintf("msg-%d", n), "model": model, "content": []any{map[string]any{"type": "thinking", "thinking": "private-reasoning-sensitive"}, map[string]any{"type": "tool_use", "id": id, "name": name, "input": jsontext.Value(arguments)}}, "stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}})
		result := mcp(id, "tools/call", map[string]any{"name": "unreal_" + step.Name, "arguments": jsontext.Value(arguments)})
		if step.Duplicate {
			second := mcp(id, "tools/call", map[string]any{"name": "unreal_" + step.Name, "arguments": jsontext.Value(arguments)})
			b1, _ := json.Marshal(result, json.Deterministic(true))
			b2, _ := json.Marshal(second, json.Deterministic(true))
			if string(b1) != string(b2) {
				os.Exit(117)
			}
		}
		if step.DuplicateEnvelope {
			second := mcpEnvelope(id+"-retry", id, "tools/call", map[string]any{"name": "unreal_" + step.Name, "arguments": jsontext.Value(arguments)})
			b1, _ := json.Marshal(result["response"].(map[string]any)["response"], json.Deterministic(true))
			b2, _ := json.Marshal(second["response"].(map[string]any)["response"], json.Deterministic(true))
			if string(b1) != string(b2) {
				os.Exit(117)
			}
		}
		rpc := result["response"].(map[string]any)["response"].(map[string]any)["mcp_response"].(map[string]any)["result"].(map[string]any)
		if rpc["isError"] != step.Error {
			failure, _ := json.Marshal(map[string]any{"step": n, "reason": "unexpected_error_flag", "actual": rpc["isError"]})
			_ = os.WriteFile(filepath.Join(dir, "bridge-failure.json"), failure, 0600)
			os.Exit(118)
		}
		var text strings.Builder
		for _, v := range rpc["content"].([]any) {
			text.WriteString(v.(map[string]any)["text"].(string))
		}
		if !strings.Contains(text.String(), step.Contains) {
			failure, _ := json.Marshal(map[string]any{"step": n, "reason": "missing_expected_receipt"})
			_ = os.WriteFile(filepath.Join(dir, "bridge-failure.json"), failure, 0600)
			os.Exit(119)
		}
		var receipt struct {
			Revision struct {
				Digest string `json:"digest"`
			} `json:"revision"`
		}
		if json.Unmarshal([]byte(text.String()), &receipt) == nil && receipt.Revision.Digest != "" {
			digest = receipt.Revision.Digest
		}
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": rpc["content"]}}}, "tool_use_result": map[string]any{"private": "private-cli-result-sensitive"}})
	}
	emit(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg-final", "model": model, "content": []any{map[string]any{"type": "text", "text": "Bridge fixture completed"}}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}})
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Bridge fixture completed", "usage": map[string]any{"input_tokens": 10 * (len(c.BridgeSteps) + 1), "output_tokens": 2 * (len(c.BridgeSteps) + 1), "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, "session_id": "private-session-sensitive"})
	fmt.Fprintln(os.Stderr, "private-api-key-sensitive authorization: Bearer token-sensitive")
	os.Exit(c.Exit)
}
