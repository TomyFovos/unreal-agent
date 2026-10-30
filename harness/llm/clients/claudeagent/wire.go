package claudeagent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"strings"
	"sync"
)

// frame is one stream-json line from the claude CLI. Only the fields this
// adapter acts on are decoded; everything else is ignored.
type frame struct {
	Type            string         `json:"type"`
	Subtype         string         `json:"subtype"`
	RequestID       string         `json:"request_id"`
	Request         jsontext.Value `json:"request"`
	Message         jsontext.Value `json:"message"`
	Event           jsontext.Value `json:"event"`
	ParentToolUseID *string        `json:"parent_tool_use_id"`
	SessionID       string         `json:"session_id"`
	// IsReplay marks Claude Code's echo of a user message Unreal sent.
	IsReplay bool `json:"isReplay"`

	// system/init
	Model           string `json:"model"`
	APIKeySource    string `json:"apiKeySource"`
	CompactMetadata *struct {
		Trigger    string `json:"trigger"`
		PreTokens  int64  `json:"pre_tokens"`
		PostTokens *int64 `json:"post_tokens"`
	} `json:"compact_metadata"`

	// system/permission_denied
	ToolName           string `json:"tool_name"`
	ToolUseID          string `json:"tool_use_id"`
	DecisionReasonType string `json:"decision_reason_type"`

	// result
	IsError    bool     `json:"is_error"`
	Result     string   `json:"result"`
	Errors     []string `json:"errors"`
	StopReason string   `json:"stop_reason"`
	Usage      *usage   `json:"usage"`
}

type usage struct {
	InputTokens              int64  `json:"input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	ReasoningTokens          *int64 `json:"reasoning_tokens"`
	ThinkingTokens           *int64 `json:"thinking_tokens"`
}

func (u usage) reasoningTokens() (int64, bool) {
	if u.ReasoningTokens != nil {
		return *u.ReasoningTokens, true
	}
	if u.ThinkingTokens != nil {
		return *u.ThinkingTokens, true
	}
	return 0, false
}

func (u usage) inputTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type message struct {
	ID      string  `json:"id"`
	Content []block `json:"content"`
}

type block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
	IsError   bool           `json:"is_error"`
}

type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	// message_start carries the step's input usage; message_delta its output.
	Message struct {
		Usage *usage `json:"usage"`
	} `json:"message"`
	Usage *usage `json:"usage"`
}

// controlRequest is a request the CLI sends to its host.
type controlRequest struct {
	Subtype    string         `json:"subtype"`
	ToolName   string         `json:"tool_name"`
	Input      jsontext.Value `json:"input"`
	ToolUseID  string         `json:"tool_use_id"`
	CallbackID string         `json:"callback_id"`
}

// hookInput is the JSON a host hook receives, as a settings hook would on stdin.
type hookInput struct {
	HookEventName string         `json:"hook_event_name"`
	Cwd           string         `json:"cwd"`
	ToolName      string         `json:"tool_name"`
	ToolInput     jsontext.Value `json:"tool_input"`
	ToolUseID     string         `json:"tool_use_id"`
	ToolCalls     []batchCall    `json:"tool_calls"`
}

type batchCall struct {
	ToolName     string         `json:"tool_name"`
	ToolInput    jsontext.Value `json:"tool_input"`
	ToolUseID    string         `json:"tool_use_id"`
	ToolResponse jsontext.Value `json:"tool_response"`
}

// writer serializes frames to the CLI's stdin; hooks answer concurrently.
type writer struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *writer) send(v any) error {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(append(data, '\n'))
	return err
}

func (w *writer) reply(id string, response any) error {
	return w.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": response}})
}

func (w *writer) fail(id, message string) error {
	return w.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": message}})
}

// contentText flattens a tool_result content value, which is either a string
// or a list of blocks, into the text the model read.
func contentText(value jsontext.Value) string {
	if len(value) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	var blocks []block
	if json.Unmarshal(value, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "image":
				parts = append(parts, "[image]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(value)
}
