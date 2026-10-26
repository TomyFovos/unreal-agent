package claudeagent

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a scripted stand-in for the claude CLI: a test points
// Config.Binary at this test binary and names the scenario in the environment.
func TestMain(m *testing.M) {
	if scenario := os.Getenv("CLAUDEAGENT_FAKE"); scenario != "" {
		os.Exit(fakeCLI(scenario, os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fake struct {
	in *bufio.Reader
}

func (f *fake) emit(v any) {
	data, _ := json.Marshal(v)
	os.Stdout.Write(append(data, '\n'))
}

func (f *fake) next() map[string]any {
	line, err := f.in.ReadBytes('\n')
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(line, &m)
	return m
}

// ask sends a control request to the host and waits for its answer.
func (f *fake) ask(id string, request map[string]any) map[string]any {
	f.emit(map[string]any{"type": "control_request", "request_id": id, "request": request})
	for {
		m := f.next()
		if m == nil {
			return nil
		}
		if response, ok := m["response"].(map[string]any); ok && m["type"] == "control_response" && response["request_id"] == id {
			answer, _ := response["response"].(map[string]any)
			return answer
		}
	}
}

func (f *fake) text(id, text string) {
	f.emit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": id, "content": []any{map[string]any{"type": "text", "text": text}}}})
}

func (f *fake) toolUse(id, name string, input map[string]any) {
	f.emit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "msg_" + id, "content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
}

func (f *fake) toolResult(id, content string, isError bool) {
	f.emit(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError}}}})
}

// step streams one model step's events: its input usage, a pause standing in
// for generation time, and its output usage.
func (f *fake) step(input, cached, written, output int, text string) {
	event := func(v map[string]any) {
		f.emit(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": v})
	}
	event(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": input, "cache_read_input_tokens": cached, "cache_creation_input_tokens": written}}})
	if text != "" {
		event(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}})
	}
	time.Sleep(20 * time.Millisecond)
	event(map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": output}})
	event(map[string]any{"type": "message_stop"})
}

func (f *fake) result(text string) {
	f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "usage": map[string]any{"input_tokens": 10, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 5, "output_tokens": 7}})
}

func (f *fake) batch(id, cwd string, calls ...map[string]any) map[string]any {
	return f.ask(id, map[string]any{"subtype": "hook_callback", "callback_id": batchHook, "input": map[string]any{"hook_event_name": "PostToolBatch", "cwd": cwd, "tool_calls": calls}})
}

func hookOutput(answer map[string]any, field string) string {
	output, _ := answer["hookSpecificOutput"].(map[string]any)
	value, _ := output[field].(string)
	return value
}

func fakeCLI(scenario string, args []string) int {
	if path := os.Getenv("CLAUDEAGENT_ARGS"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(args, "\n")), 0o600)
	}
	resume := slices.Contains(args, "--resume")
	if scenario == "in_use" && !resume {
		fmt.Fprintln(os.Stderr, "Error: Session ID 0000 is already in use.")
		return 1
	}
	f := &fake{in: bufio.NewReader(os.Stdin)}
	initialize := f.next()
	answer := map[string]any{}
	if scenario == "catalog" {
		answer = map[string]any{
			"models": []any{
				map[string]any{"value": "default", "resolvedModel": "claude-opus-5-5", "displayName": "Default (recommended)", "supportsEffort": true, "supportedEffortLevels": []any{"low", "high"}},
				map[string]any{"value": "opus", "resolvedModel": "claude-opus-5-5", "displayName": "Opus 5.5", "supportsEffort": true, "supportedEffortLevels": []any{"low", "medium", "high", "xhigh", "max"}},
				map[string]any{"value": "claude-haiku-4-5-20251001", "displayName": "Haiku 4.5"},
			},
			"account": map[string]any{"email": "user@example.test", "organization": "example", "subscriptionType": "Claude Team", "apiProvider": "firstParty"},
		}
	}
	f.emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": initialize["request_id"], "response": answer}})
	if scenario == "catalog" {
		f.next()
		return 0
	}
	if scenario == "missing" && resume {
		f.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{"No conversation found with session ID: 0000"}})
		return 1
	}
	user := f.next()
	prompt, _ := user["message"].(map[string]any)["content"].(string)
	f.emit(map[string]any{"type": "system", "subtype": "init", "model": "claude-opus-5-5", "apiKeySource": "none", "session_id": "0000"})
	if scenario == "stale" {
		// Work Claude Code queued itself ends before it takes up the message.
		f.emit(map[string]any{"type": "result", "subtype": "success", "result": ""})
	}
	f.emit(map[string]any{"type": "user", "isReplay": true, "parent_tool_use_id": nil, "message": user["message"]})
	cwd := os.Getenv("CLAUDEAGENT_CWD")
	switch scenario {
	case "interrupt":
		// A long command runs until Unreal asks Claude Code to stop.
		for m := f.next(); m != nil; m = f.next() {
			if request, ok := m["request"].(map[string]any); ok && request["subtype"] == "interrupt" {
				_ = os.WriteFile(os.Getenv("CLAUDEAGENT_ARGS")+".interrupt", nil, 0o600)
				f.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{"interrupted"}})
				break
			}
		}
	case "missing", "in_use", "echo", "stale":
		mode := "fresh"
		if resume {
			mode = "resumed"
		}
		f.text("msg_1", mode+":"+prompt)
		f.result(mode + ":" + prompt)
	case "login":
		f.emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Not logged in · Please run /login"})
	case "edit":
		path := os.Getenv("CLAUDEAGENT_FILE")
		f.step(100, 1000, 10, 20, "Editing")
		f.text("msg_1", "Editing the file.")
		input := map[string]any{"file_path": path, "old_string": "hello", "new_string": "goodbye"}
		f.toolUse("toolu_1", "Edit", input)
		decision := f.ask("perm-1", map[string]any{"subtype": "can_use_tool", "tool_name": "Edit", "input": input, "tool_use_id": "toolu_1"})
		content, isError := "", false
		if decision["behavior"] == "allow" {
			data, _ := os.ReadFile(path)
			_ = os.WriteFile(path, []byte(strings.Replace(string(data), "hello", "goodbye", 1)), 0o600)
			content = "The file " + path + " has been updated."
		} else {
			content, _ = decision["message"].(string)
			isError = true
		}
		f.toolResult("toolu_1", content, isError)
		f.batch("batch-1", cwd, map[string]any{"tool_name": "Edit", "tool_input": input, "tool_use_id": "toolu_1", "tool_response": map[string]any{"filePath": path}})
		f.step(5, 1100, 0, 7, "Done.")
		f.text("msg_2", "Done.")
		f.result("Done.")
	case "refused":
		input := map[string]any{"command": "git push"}
		f.toolUse("toolu_9", "Bash", input)
		reason := os.Getenv("CLAUDEAGENT_REASON")
		if reason == "" {
			reason = "rule"
		}
		if reason == "hook" {
			// Unreal's own Read Only gate refused it first.
			answer := f.ask("gate-9", map[string]any{"subtype": "hook_callback", "callback_id": gateHook, "input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": input, "tool_use_id": "toolu_9"}})
			if hookOutput(answer, "permissionDecision") != "deny" {
				fmt.Fprintln(os.Stderr, "gate did not deny")
				return 2
			}
		}
		f.emit(map[string]any{"type": "system", "subtype": "permission_denied", "tool_name": "Bash", "tool_use_id": "toolu_9", "decision_reason_type": reason, "message": "Permission to use Bash with command git push has been denied."})
		f.toolResult("toolu_9", "Permission to use Bash with command git push has been denied.", true)
		answer := f.batch("batch-9", cwd, map[string]any{"tool_name": "Bash", "tool_input": input, "tool_use_id": "toolu_9", "tool_response": "Permission to use Bash with command git push has been denied."})
		saw := "saw: " + hookOutput(answer, "additionalContext")
		f.text("msg_2", saw)
		f.result(saw)
	case "subagent":
		// Claude runs the Host's subagent command: Unreal passes the gate and
		// allows it itself, in every mode.
		input := map[string]any{"command": os.Getenv("CLAUDEAGENT_CLI") + " subagent wait"}
		gate := hookOutput(f.ask("gate-s", map[string]any{"subtype": "hook_callback", "callback_id": gateHook, "input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": input, "tool_use_id": "toolu_s"}}), "permissionDecision")
		if gate == "" {
			gate = "pass"
		}
		use := f.ask("perm-s", map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": input, "tool_use_id": "toolu_s"})
		text := fmt.Sprintf("env:%s:%s gate:%s use:%v", os.Getenv(SubagentSocketEnv), os.Getenv(SubagentSessionEnv), gate, use["behavior"])
		f.text("msg_1", text)
		f.result(text)
	case "gate":
		command := os.Getenv("CLAUDEAGENT_COMMAND")
		if command == "" {
			command = "python3 -c 1"
		}
		answer := f.ask("gate-1", map[string]any{"subtype": "hook_callback", "callback_id": gateHook, "input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": command}, "tool_use_id": "toolu_g"}})
		decision := hookOutput(answer, "permissionDecision")
		if decision == "" {
			decision = "pass"
		}
		f.result("gate:" + decision)
	}
	for f.next() != nil {
	}
	return 0
}
