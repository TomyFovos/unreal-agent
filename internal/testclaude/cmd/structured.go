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
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Offline structured-output fixture. Every generation is a separate process.
// It inspects only supplied context/receipts, never the workspace or commands.
func structured(dir, path string, args []string, c configuration) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 8192), 16<<20)
	read := func() map[string]any {
		if !scanner.Scan() {
			os.Exit(0)
		}
		var v map[string]any
		if json.Unmarshal(scanner.Bytes(), &v) != nil {
			os.Exit(130)
		}
		return v
	}
	emit := func(v any) {
		if json.MarshalWrite(os.Stdout, v) != nil {
			os.Exit(131)
		}
		fmt.Println()
	}
	init := read()
	initializeFrame := string(scanner.Bytes())
	request, _ := init["request"].(map[string]any)
	if init["type"] != "control_request" || request["subtype"] != "initialize" || request["jsonSchema"] == nil {
		os.Exit(132)
	}
	encoded, _ := json.Marshal(request["jsonSchema"])
	entry := call{Arguments: args, Environment: os.Environ(), PID: os.Getpid(), Schema: string(encoded), Initialize: initializeFrame}
	entry.Directory, _ = os.Getwd()
	log := func(name string) {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(133)
		}
		b, _ := json.Marshal(entry)
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
	subtype := "success"
	if c.StructuredInitializeError {
		subtype = "error"
	}
	emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": subtype, "request_id": init["request_id"], "response": map[string]any{"account": "account-sensitive"}}})
	if !slices.Contains(args, "-p") {
		log("structured-probes.jsonl")
		return
	}
	if c.StructuredInitializeError {
		return
	}
	user := read()
	entry.UserFrame = string(scanner.Bytes())
	if user["type"] != "user" {
		os.Exit(134)
	}
	entry.Input, _ = user["message"].(map[string]any)["content"].(string)
	model := "claude-configured-a"
	for i, arg := range args {
		if i+1 < len(args) {
			switch arg {
			case "--system-prompt-file":
				b, _ := os.ReadFile(args[i+1])
				entry.System = string(b)
			case "--model":
				model = args[i+1]
			}
		}
	}
	var child *exec.Cmd
	if c.Child {
		child = exec.Command(path, "--fake-child")
		child.Stdout = os.Stdout
		if child.Start() != nil {
			os.Exit(135)
		}
		entry.ChildPID = child.Process.Pid
		defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	}
	log("calls.jsonl")
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
	if c.Stream != "" {
		fmt.Print(c.Stream)
		return
	}
	if strings.Contains(entry.System, "CHILD_GUIDANCE") {
		c.BridgeSteps = c.BridgeChildSteps
	} else if c.BridgeOnce && strings.Contains(entry.Input, "Bridge fixture completed") {
		c.BridgeSteps = nil
	}
	generation := 0
	if start := strings.LastIndex(entry.System, "Unreal structured generation: "); start >= 0 {
		_, _ = fmt.Sscanf(entry.System[start:], "Unreal structured generation: %d.", &generation)
	}
	type step struct {
		bridgeStep
		id     int
		replay bool
	}
	var script []step
	for i, s := range c.BridgeSteps {
		script = append(script, step{s, i, false})
		if s.Duplicate {
			script = append(script, step{s, i, true})
		}
	}
	var last struct {
		Result struct {
			CallID string
			Output []struct{ Kind, Value string }
		}
		Failed bool
	}
	const label = "\n\nLast Unreal Action Result (machine-owned canonical receipt; Failed=true means failure):\n"
	if index := strings.LastIndex(entry.Input, label); index >= 0 {
		if json.Unmarshal([]byte(entry.Input[index+len(label):]), &last) != nil {
			os.Exit(136)
		}
	}
	if generation > 0 && generation-1 < len(script) {
		previous := script[generation-1]
		var text strings.Builder
		for _, o := range last.Result.Output {
			text.WriteString(o.Value)
		}
		if last.Failed != previous.Error || !strings.Contains(text.String(), previous.Contains) {
			b, _ := json.Marshal(map[string]any{"generation": generation, "reason": "missing_or_incorrect_canonical_receipt", "failed": last.Failed})
			_ = os.WriteFile(filepath.Join(dir, "bridge-failure.json"), b, 0600)
			os.Exit(137)
		}
	}
	output := any(map[string]any{"type": "final", "final": map[string]any{"message": "Bridge fixture completed"}})
	if generation < len(script) {
		s := script[generation]
		digest, oldArgs := structuredEvidence(entry.Input, fmt.Sprintf("-tool-%d", s.id))
		arguments := strings.ReplaceAll(s.Arguments, "$REVISION", digest)
		if s.replay && oldArgs != "" {
			arguments = oldArgs
		}
		output = map[string]any{"type": "action", "action": map[string]any{"id": fmt.Sprintf("tool-%d", s.id), "tool": s.Name, "arguments": jsontext.Value(arguments)}}
	}
	if generation < len(c.StructuredResponses) {
		output = jsontext.Value(c.StructuredResponses[generation])
	}
	emit(map[string]any{"type": "system", "subtype": "init", "model": model, "tools": []string{"StructuredOutput"}, "mcp_servers": []any{}, "permissionMode": "default", "plugins": []any{map[string]any{"name": "catalog-sensitive"}}})
	if c.StructuredEnforcementReminder {
		emit(map[string]any{"type": "assistant", "message": map[string]any{"id": "private-prose-id", "model": model, "content": []any{map[string]any{"type": "text", "text": "sensitive-prose"}}, "stop_reason": "end_turn"}})
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[structured-output-enforce] You MUST call the StructuredOutput tool to complete this request. Call this tool now."}}}, "parent_tool_use_id": nil, "isSynthetic": true, "timestamp": "2026-01-01T00:00:00Z", "uuid": "private-helper-id", "session_id": "private-session-id"})
	}
	// Model a serializer validation failure followed by its correction within
	// the same CLI process. Neither helper input nor error is a host Action.
	turns := 0
	for i, arg := range args {
		if arg == "--max-turns" && i+1 < len(args) {
			_, _ = fmt.Sscan(args[i+1], &turns)
		}
	}
	for attempt := 0; attempt < min(c.StructuredSerializerFailures, turns); attempt++ {
		id := fmt.Sprintf("private-serializer-retry-%d", attempt)
		emit(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "model": model, "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "StructuredOutput", "input": map[string]any{"private-invalid-proposal": "sensitive-argument"}}}, "stop_reason": "tool_use"}})
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": true, "content": "private-sensitive validation feedback"}}}})
	}
	if c.StructuredSerializerFailures >= turns && c.StructuredSerializerFailures > 0 {
		emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "errors": []string{"private-sensitive"}})
		return
	}
	if raw, ok := output.(jsontext.Value); ok && !raw.IsValid() {
		fmt.Printf("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":%s}\n", raw)
		return
	}
	helperInput := output
	if generation < len(c.StructuredHelperInputs) {
		helperInput = jsontext.Value(c.StructuredHelperInputs[generation])
	}
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "private-helper-message", "model": model, "content": []any{}}}})
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": "StructuredOutput", "id": "serializer-only"}}})
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}}})
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_stop", "index": 0}})
	// Official partial stream order: message_delta/message_stop precede the
	// complete assistant frame. A serializer stop is correlated by its start.
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}}})
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_stop"}})
	if generation < len(c.StructuredAssistantFrames) {
		emit(jsontext.Value(c.StructuredAssistantFrames[generation]))
	} else {
		emit(map[string]any{"type": "assistant", "message": map[string]any{"id": "private-helper-message", "model": model, "content": []any{map[string]any{"type": "thinking", "thinking": "private-reasoning-sensitive"}, map[string]any{"type": "tool_use", "id": "serializer-only", "name": "StructuredOutput", "input": helperInput}}, "stop_reason": "tool_use"}})
	}
	emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "serializer-only", "content": "Structured output provided successfully"}}}, "tool_use_result": map[string]any{"structured_output": output}})
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": output, "result": "private-helper-result", "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, "session_id": "private-session-sensitive"})
	fmt.Fprintln(os.Stderr, "private-token-sensitive")
	os.Exit(c.Exit)
}

func structuredEvidence(input, suffix string) (digest, arguments string) {
	var visit func(any, int)
	visit = func(value any, depth int) {
		if depth > 24 {
			return
		}
		switch v := value.(type) {
		case []any:
			for _, x := range v {
				visit(x, depth+1)
			}
		case map[string]any:
			if r, ok := v["revision"].(map[string]any); ok {
				if d, ok := r["digest"].(string); ok {
					digest = d
				}
			}
			if id, ok := v["CallID"].(string); ok && strings.HasSuffix(id, suffix) {
				if a, ok := v["Arguments"].(string); ok {
					arguments = a
				}
			}
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				visit(v[k], depth+1)
			}
		case string:
			var nested any
			if json.Unmarshal([]byte(v), &nested) == nil {
				visit(nested, depth+1)
			}
		}
	}
	parts := strings.SplitN(input, "\n\nLast Unreal Action Result", 2)
	var v any
	if json.Unmarshal([]byte(parts[0]), &v) == nil {
		visit(v, 0)
	}
	if len(parts) == 2 {
		index := strings.Index(parts[1], "\n")
		if index >= 0 && json.Unmarshal([]byte(parts[1][index+1:]), &v) == nil {
			visit(v, 0)
		}
	}
	return
}
