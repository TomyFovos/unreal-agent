package claudeagent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
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
	Model        string `json:"model"`
	APIKeySource string `json:"apiKeySource"`

	// rate_limit_event
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"`

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

	// APIError can identify an exhausted quota on assistant and result frames.
	APIError          jsontext.Value `json:"api_error"`
	IsAPIErrorMessage bool           `json:"isApiErrorMessage"`

	// system/api_retry and system/api_error. Error may be a string or object;
	// only its numeric status is recorded in retry activity.
	Attempt      int              `json:"attempt"`
	MaxRetries   int              `json:"max_retries"`
	RetryDelayMS int64            `json:"retry_delay_ms"`
	ErrorStatus  *int             `json:"error_status"`
	Error        jsontext.Value   `json:"error"`
	NoResponse   *noResponseRetry `json:"no_response"`
}

type rateLimitInfo struct {
	Status      string   `json:"status"`
	Window      string   `json:"rateLimitType"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    int64    `json:"resetsAt"`
}

type noResponseRetry struct {
	WaitedMS    int64 `json:"waited_ms"`
	RetryWaitMS int64 `json:"retry_wait_ms"`
}

type usage struct {
	InputTokens                   int64  `json:"input_tokens"`
	CacheCreationInputTokens      *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens          *int64 `json:"cache_read_input_tokens"`
	OutputTokens                  int64  `json:"output_tokens"`
	ReasoningTokens               *int64 `json:"reasoning_tokens"`
	ThinkingTokens                *int64 `json:"thinking_tokens"`
	inputReported, outputReported bool
}

func (u *usage) UnmarshalJSON(data []byte) error {
	type wireUsage usage
	var v struct {
		wireUsage
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*u = usage(v.wireUsage)
	if v.InputTokens != nil {
		u.InputTokens, u.inputReported = *v.InputTokens, true
	}
	if v.OutputTokens != nil {
		u.OutputTokens, u.outputReported = *v.OutputTokens, true
	}
	return nil
}

func (u *usage) requestUsage() *llm.RequestUsage {
	if u == nil {
		return &llm.RequestUsage{}
	}
	r := &llm.RequestUsage{Read: u.CacheReadInputTokens, Write: u.CacheCreationInputTokens}
	if u.inputReported {
		r.Uncached = &u.InputTokens
		if r.Read != nil && r.Write != nil {
			r.Input = new(u.inputTokens())
		}
	}
	return r
}

func (u *usage) reportedOutput() *int64 {
	if u != nil && u.outputReported {
		return &u.OutputTokens
	}
	return nil
}

// Assistant fragments can repeat the message ID and update only some counters.
func (u usage) merge(next usage) usage {
	if next.inputReported {
		u.InputTokens, u.inputReported = next.InputTokens, true
	}
	if next.outputReported {
		u.OutputTokens, u.outputReported = next.OutputTokens, true
	}
	if next.CacheReadInputTokens != nil {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
	if next.CacheCreationInputTokens != nil {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	if next.ReasoningTokens != nil {
		u.ReasoningTokens = next.ReasoningTokens
	}
	if next.ThinkingTokens != nil {
		u.ThinkingTokens = next.ThinkingTokens
	}
	return u
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
	read, write := u.cacheTokens()
	return u.InputTokens + read + write
}

func (u usage) cacheTokens() (read, write int64) {
	if u.CacheReadInputTokens != nil {
		read = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		write = *u.CacheCreationInputTokens
	}
	return
}

type message struct {
	Model   string  `json:"model"`
	Usage   *usage  `json:"usage"`
	ID      string  `json:"id"`
	Content []block `json:"content"`
}

type block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Thinking  string         `json:"thinking"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
	IsError   bool           `json:"is_error"`
}

type streamEvent struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
	} `json:"content_block"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Thinking   string `json:"thinking"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	// message_start carries the step's input usage; message_delta its output.
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *usage `json:"usage"`
	} `json:"message"`
	Usage *usage `json:"usage"`
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
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
	AgentID       string         `json:"agent_id"`
	Cwd           string         `json:"cwd"`
	ToolName      string         `json:"tool_name"`
	ToolInput     jsontext.Value `json:"tool_input"`
	ToolUseID     string         `json:"tool_use_id"`
	ToolCalls     []batchCall    `json:"tool_calls"`
	parent        string
}

// observedText reads textual results, including the content envelope that the
// Agent hook can report. Opaque objects are not transcript text.
func observedText(value jsontext.Value) string {
	if value.Kind() == jsontext.KindBeginObject {
		var result struct {
			Content jsontext.Value `json:"content"`
		}
		if json.Unmarshal(value, &result) != nil {
			return ""
		}
		value = result.Content
	}
	text, _ := decodeContentText(value)
	return text
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
	if text, ok := decodeContentText(value); ok {
		return text
	}
	return string(value)
}

func decodeContentText(value jsontext.Value) (string, bool) {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text, true
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
		return strings.Join(parts, "\n"), true
	}
	return "", false
}
