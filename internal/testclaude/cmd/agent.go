package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// agentStep is one tool call the synthetic Claude Code runs itself, as the
// real CLI does for the Claude route (U21).
type agentStep struct {
	Tool  string         // Claude Code tool name: Read, Edit, Write or Bash
	Input jsontext.Value // the tool input
	// Refused simulates Claude Code declining a command itself:
	// no approval request, a permission_denied frame, then the batch hook.
	Refused bool
}

// agent speaks the stream-json control protocol of the Claude route: it answers
// initialize with models and the login, then runs the configured steps,
// asking the host to approve each one and reporting each batch.
func agent(dir string, args []string, c configuration) {
	in := bufio.NewReader(os.Stdin)
	out := encoder{}
	next := func() map[string]any {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return nil
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		return m
	}
	ask := func(id string, request map[string]any) map[string]any {
		_ = out.Encode(map[string]any{"type": "control_request", "request_id": id, "request": request})
		for {
			m := next()
			if m == nil {
				os.Exit(0)
			}
			if response, ok := m["response"].(map[string]any); ok && m["type"] == "control_response" && response["request_id"] == id {
				answer, _ := response["response"].(map[string]any)
				return answer
			}
		}
	}
	initialize := next()
	if initialize == nil {
		return
	}
	request, _ := initialize["request"].(map[string]any)
	hooks, _ := request["hooks"].(map[string]any)
	callback := func(event string) string {
		matchers, _ := hooks[event].([]any)
		for _, m := range matchers {
			ids, _ := m.(map[string]any)["hookCallbackIds"].([]any)
			if len(ids) > 0 {
				id, _ := ids[0].(string)
				return id
			}
		}
		return ""
	}
	account := map[string]any{"email": "account-sensitive", "organization": "org-sensitive", "apiProvider": "firstParty"}
	if !c.SignedOut {
		subscription := c.Subscription
		if subscription == "" {
			subscription = "pro"
		}
		account["subscriptionType"] = subscription
	}
	var models any = []any{}
	if c.Catalog != "" {
		models = jsontext.Value(c.Catalog)
	}
	_ = out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": initialize["request_id"], "response": map[string]any{"models": models, "account": account}}})
	user := next()
	if user == nil {
		return // discovery: no conversation, no inference
	}
	message, _ := user["message"].(map[string]any)
	prompt, _ := message["content"].(string)
	entry := call{Arguments: args, Environment: os.Environ(), PID: os.Getpid(), Input: prompt, InputAt: time.Now()}
	entry.Directory, _ = os.Getwd()
	steps := c.AgentSteps
	if previous, err := os.ReadFile(filepath.Join(dir, "calls.jsonl")); err == nil {
		if len(previous) > 0 && c.AgentOnce {
			steps = nil
		}
		conversation := func(args []string) string {
			for i, arg := range args {
				if (arg == "--resume" || arg == "--session-id") && i+1 < len(args) {
					return args[i+1]
				}
			}
			return ""
		}
		for _, line := range strings.Split(strings.TrimSpace(string(previous)), "\n") {
			var old call
			if json.Unmarshal([]byte(line), &old) == nil && old.PID > 0 && conversation(old.Arguments) == conversation(args) && syscall.Kill(old.PID, 0) == nil {
				entry.OverlappingPID = old.PID
			}
		}
	}
	if log, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
		encoded, _ := json.Marshal(entry)
		_, _ = log.Write(append(encoded, '\n'))
		log.Close()
	}
	model := "claude-configured-a"
	for i, arg := range args {
		if arg == "--model" && i+1 < len(args) {
			model = args[i+1]
		}
	}
	_ = out.Encode(map[string]any{"type": "system", "subtype": "init", "model": model, "apiKeySource": "none", "permissionMode": "default"})
	if slices.Contains(args, "--replay-user-messages") {
		_ = out.Encode(map[string]any{"type": "user", "isReplay": true, "parent_tool_use_id": nil, "message": message})
	}
	if strings.HasPrefix(prompt, "/compact") {
		_ = out.Encode(map[string]any{"type": "system", "subtype": "status", "status": "compacting"})
		time.Sleep(time.Duration(c.CompactDelayMillis) * time.Millisecond)
		if c.CompactCrash {
			fmt.Fprintln(os.Stderr, "synthetic compact process failure")
			os.Exit(9)
		}
		if c.CompactFailure != "" {
			_ = out.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": c.CompactFailure})
		} else {
			before, after := c.CompactPreTokens, c.CompactPostTokens
			if before == 0 {
				before = 250_000
			}
			if after == 0 {
				after = 12_000
			}
			metadata := map[string]any{"trigger": "manual", "pre_tokens": before}
			result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "compact complete"}
			if !c.CompactUnknownAfter {
				metadata["post_tokens"] = after
				result["usage"] = map[string]any{"input_tokens": after}
			}
			_ = out.Encode(map[string]any{"type": "system", "subtype": "compact_boundary", "compact_metadata": metadata})
			_ = out.Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "synthetic conversation summary"}})
			_ = out.Encode(result)
		}
		for next() != nil {
		}
		return
	}
	cwd, _ := os.Getwd()
	var notes []string
	for i, step := range steps {
		if c.AgentStepInputTokens > 0 {
			_ = out.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": c.AgentStepInputTokens}}}})
			_ = out.Encode(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_stop"}})
		}
		id := fmt.Sprintf("toolu_fake_%d", i+1)
		var input map[string]any
		_ = json.Unmarshal(step.Input, &input)
		_ = out.Encode(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": fmt.Sprintf("msg_fake_%d", i+1), "content": []any{map[string]any{"type": "tool_use", "id": id, "name": step.Tool, "input": input}}}})
		content, isError := "", false
		switch {
		case step.Refused:
			content, isError = "Permission to use Bash with command "+str(input["command"])+" has been denied.", true
			_ = out.Encode(map[string]any{"type": "system", "subtype": "permission_denied", "tool_name": step.Tool, "tool_use_id": id, "decision_reason_type": "rule", "message": content})
		case step.Tool == "Read":
			data, err := os.ReadFile(resolve(cwd, str(input["file_path"])))
			content, isError = string(data), err != nil
			if err != nil {
				content = err.Error()
			}
		default:
			if gate := callback("PreToolUse"); gate != "" {
				answer := ask("gate-"+id, map[string]any{"subtype": "hook_callback", "callback_id": gate, "input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": step.Tool, "tool_input": input, "tool_use_id": id, "cwd": cwd}})
				if output, _ := answer["hookSpecificOutput"].(map[string]any); output["permissionDecision"] == "deny" {
					content, isError = "PreToolUse:"+step.Tool+" hook error: "+str(output["permissionDecisionReason"]), true
					break
				}
			}
			decision := ask("perm-"+id, map[string]any{"subtype": "can_use_tool", "tool_name": step.Tool, "input": input, "tool_use_id": id})
			if decision["behavior"] != "allow" {
				content, isError = str(decision["message"]), true
				break
			}
			content, isError = run(cwd, step.Tool, input)
		}
		_ = out.Encode(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError}}}})
		if batch := callback("PostToolBatch"); batch != "" {
			answer := ask("batch-"+id, map[string]any{"subtype": "hook_callback", "callback_id": batch, "input": map[string]any{"hook_event_name": "PostToolBatch", "cwd": cwd, "tool_calls": []any{map[string]any{"tool_name": step.Tool, "tool_input": input, "tool_use_id": id, "tool_response": content}}}})
			if output, _ := answer["hookSpecificOutput"].(map[string]any); output != nil {
				if note := str(output["additionalContext"]); note != "" {
					notes = append(notes, note)
				}
			}
			if log, err := os.OpenFile(filepath.Join(dir, "batches.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				data, _ := json.Marshal(answer)
				_, _ = log.Write(append(data, '\n'))
				_ = log.Close()
			}
		}
	}
	final := c.AgentFinal
	if final == "" {
		final = "synthetic agent done"
	}
	if len(notes) > 0 {
		final += "\n" + strings.Join(notes, "\n")
	}
	_ = out.Encode(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "msg_fake_final", "content": []any{map[string]any{"type": "text", "text": final}}}})
	inputTokens := c.AgentInputTokens
	if inputTokens == 0 {
		inputTokens = 10
	}
	_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": final, "usage": map[string]any{"input_tokens": inputTokens, "cache_read_input_tokens": 20, "cache_creation_input_tokens": 30, "output_tokens": 40}})
	for next() != nil {
	}
}

type encoder struct{}

func (encoder) Encode(v any) error {
	data, err := json.Marshal(v)
	if err == nil {
		_, err = os.Stdout.Write(append(data, '\n'))
	}
	return err
}

// run performs an approved Edit, Write or Bash the way Claude Code would.
func run(cwd, tool string, input map[string]any) (string, bool) {
	switch tool {
	case "Edit":
		path := resolve(cwd, str(input["file_path"]))
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), str(input["old_string"])) {
			return "String to replace not found in file.", true
		}
		if err = os.WriteFile(path, []byte(strings.Replace(string(data), str(input["old_string"]), str(input["new_string"]), 1)), 0600); err != nil {
			return err.Error(), true
		}
		return "The file " + path + " has been updated successfully.", false
	case "Write":
		path := resolve(cwd, str(input["file_path"]))
		if err := os.WriteFile(path, []byte(str(input["content"])), 0600); err != nil {
			return err.Error(), true
		}
		return "File created successfully at: " + path, false
	case "Bash":
		output, err := exec.Command("/bin/sh", "-c", str(input["command"])).CombinedOutput()
		return string(output), err != nil
	}
	return "unsupported synthetic tool", true
}

func resolve(cwd, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(cwd, path)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
