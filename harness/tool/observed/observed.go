// Package observed records tool calls that Claude Code ran itself (U21).
// Unreal never executes them. The result Claude Code reported becomes a value
// Operation, so the call has a canonical receipt like every other tool call.
package observed

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// Arguments is the canonical tool call: which Claude Code tool ran, with what.
type Arguments struct {
	Tool  string         `json:"tool"`
	Input jsontext.Value `json:"input,omitzero"`
}

// Result is what Claude Code reported to the model for the call.
type Result struct {
	Tool    string `json:"tool"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitzero"`
}

// Results hands reported results from the Claude adapter to the Translator.
// The adapter puts a result before submitting the call; Translate takes it.
type Results struct {
	mu      sync.Mutex
	pending map[string]Result
}

func NewResults() *Results { return &Results{pending: map[string]Result{}} }

func (r *Results) Put(callID string, result Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[callID] = result
}

// Drop discards results whose calls were never submitted, such as when the
// rendezvous failed.
func (r *Results) Drop(callIDs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range callIDs {
		delete(r.pending, id)
	}
}

func (r *Results) take(callID string) (Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, ok := r.pending[callID]
	delete(r.pending, callID)
	return result, ok
}

type Translator struct{ results *Results }

func New(results *Results) *Translator { return &Translator{results: results} }

func (t *Translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var arguments Arguments
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil || arguments.Tool == "" {
		return tool.CallStatus{Error: "claude_code call has no tool name"}
	}
	if t.results == nil {
		return tool.CallStatus{Error: "Claude Code results are not recorded in this session"}
	}
	result, ok := t.results.take(call.CallID)
	if !ok {
		return tool.CallStatus{Error: "Claude Code reported no result for this call"}
	}
	result.Tool = arguments.Tool
	content := strings.ToValidUTF8(result.Content, "�")
	if result.IsError {
		// A denied or failed call is a failed receipt carrying the reason Claude
		// read, so the TUI shows it as failed rather than as a completed record.
		if strings.TrimSpace(content) == "" {
			content = "Claude Code reported that the call failed"
		}
		return tool.ErrorStatus(content, operation.DefaultMaxOutputLength)
	}
	result.Content, _ = operation.BoundOutput(content, operation.DefaultMaxOutputLength)
	value, err := json.Marshal(result)
	if err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("encode Claude Code result: %v", err)}
	}
	spec, err := operation.NewValueSpec(value)
	if err != nil {
		return tool.CallStatus{Error: err.Error()}
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}

func (t *Translator) TranslateResult(callID string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	text := func(value string) llm.ToolResult {
		return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: value}}}
	}
	if status.Error != "" {
		return text(status.Error), nil
	}
	if len(operations) != 1 {
		return llm.ToolResult{}, fmt.Errorf("claude_code call %q has %d operations, want 1", callID, len(operations))
	}
	result, err := decode(operations[0])
	if err != nil {
		return llm.ToolResult{}, fmt.Errorf("decode claude_code call %q result: %w", callID, err)
	}
	return text(result.Content), nil
}

func (t *Translator) ResultFailed(status tool.CallStatus, operations []operation.Operation) bool {
	if status.Error != "" || len(operations) != 1 {
		return true
	}
	result, err := decode(operations[0])
	return err != nil || result.IsError
}

func decode(current operation.Operation) (Result, error) {
	value, err := operation.DecodeValue(current)
	if err != nil {
		return Result{}, err
	}
	var result Result
	if err = json.Unmarshal(value, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}
