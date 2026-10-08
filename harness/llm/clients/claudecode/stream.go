package claudecode

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"math"
	"strings"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type wireUsage struct {
	Input      *int64 `json:"input_tokens"`
	Cached     *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	Output     *int64 `json:"output_tokens"`
}

type block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
}
type wireMessage struct {
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	Content    []block    `json:"content"`
	StopReason string     `json:"stop_reason"`
	Usage      *wireUsage `json:"usage"`
}

type wireEvent struct {
	Type         string      `json:"type"`
	Message      wireMessage `json:"message"`
	ContentBlock block       `json:"content_block"`
	Delta        struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

type streamRecord struct {
	Type            string           `json:"type"`
	Subtype         string           `json:"subtype"`
	Model           string           `json:"model"`
	PermissionMode  jsontext.Value   `json:"permissionMode"`
	Status          jsontext.Value   `json:"status"`
	Tools           []string         `json:"tools"`
	MCP             []jsontext.Value `json:"mcp_servers"`
	Agents          []string         `json:"agents"`
	Skills          []string         `json:"skills"`
	Plugins         []jsontext.Value `json:"plugins"`
	Message         wireMessage      `json:"message"`
	ParentToolUseID string           `json:"parent_tool_use_id"`
	SubagentType    string           `json:"subagent_type"`
	ToolUseResult   jsontext.Value   `json:"tool_use_result"`
	Result          string           `json:"result"`
	IsError         bool             `json:"is_error"`
	Errors          []string         `json:"errors"`
	Error           jsontext.Value   `json:"error"`
	RateLimit       struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
	Usage *wireUsage `json:"usage"`
	Event wireEvent  `json:"event"`
}

func parseStream(input io.Reader, opt llm.RequestOptions) (llm.Response, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 8192), 1<<20)
	r := llm.Response{Stop: llm.StopComplete}
	initialized, finished := false, false
	text := ""
	stopReason := ""
	total, events := 0, 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line)
		events++
		if total > 32<<20 || events > 65536 || finished {
			if !initialized {
				return llm.Response{}, malformedStream(streamInitUnknownShape)
			}
			return llm.Response{}, &Error{Code: "malformed_stream"}
		}
		if recognized, err := permissionDeniedEvent(line, false, nil); recognized {
			return llm.Response{}, err
		}
		var v streamRecord
		if err := json.Unmarshal(line, &v); err != nil {
			return llm.Response{}, streamDecodeFailure(line, err, initialized)
		}
		if v.ParentToolUseID != "" || v.SubagentType != "" || len(v.ToolUseResult) > 0 && string(v.ToolUseResult) != "null" {
			return llm.Response{}, toolStreamFailure()
		}
		switch v.Type {
		case "system":
			// Agent/skill/plugin catalog metadata does not attest to executable
			// components. SDKSystemMessage.plugins has name/path/version only.
			// The adapter requires safe-mode at launch; available tools/MCP and
			// actual execution events are checked separately. Catalog contents
			// are discarded and never enter the response or canonical history.
			if len(v.Tools) > 0 {
				reason := streamInitToolsNonempty
				if v.Subtype == "status" {
					reason = streamStatusToolsNonempty
				}
				return llm.Response{}, initializationFailure(reason, len(v.Tools))
			}
			if len(v.MCP) > 0 {
				reason := streamInitMCPNonempty
				if v.Subtype == "status" {
					reason = streamStatusMCPNonempty
				}
				return llm.Response{}, initializationFailure(reason, len(v.MCP))
			}
			switch v.Subtype {
			case "init":
				if initialized {
					return llm.Response{}, initializationFailure(streamInitDuplicate, 0)
				}
				// Required fields must explicitly attest to empty tools/MCP.
				// Missing or null arrays do not prove an empty available set.
				if v.Tools == nil || v.MCP == nil || !publicID(v.Model) {
					return llm.Response{}, initRequiredFieldFailure(line, v)
				}
				if err := streamPermissionMode(v.PermissionMode, true); err != nil {
					return llm.Response{}, err
				}
				initialized, r.Model = true, v.Model
			case "status":
				if !initialized {
					return llm.Response{}, malformedStream(streamStatusBeforeInit)
				}
				if len(v.Status) == 0 {
					return llm.Response{}, malformedStream(streamStatusMissingRequired)
				}
				if err := streamPermissionMode(v.PermissionMode, false); err != nil {
					return llm.Response{}, err
				}
				var status *string
				if json.Unmarshal(v.Status, &status) != nil {
					return llm.Response{}, malformedStream(streamStatusWrongFieldType)
				}
				if status != nil && *status != "requesting" && *status != "compacting" {
					return llm.Response{}, malformedStream(streamStatusUnknownValue)
				}
				// Lifecycle metadata is neither assistant text nor canonical
				// history. Do not forward compact errors or internal session IDs.
			case "task_started", "task_progress", "task_updated", "task_notification":
				return llm.Response{}, toolStreamFailure()
			case "api_retry", "thinking_tokens", "informational", "notification", "session_state_changed":
				if err := validateSystemMetadata(line, v, initialized); err != nil {
					return llm.Response{}, err
				}
				// Metadata is discarded, never progress, usage, context, a
				// canonical event, or an instruction to change Session state.
			default:
				return llm.Response{}, unknownSystemShape(line)
			}
		case "stream_event":
			if !initialized {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			if err := textOnlyEvent(v.Event); err != nil {
				return llm.Response{}, err
			}
			if v.Event.Type == "content_block_delta" && v.Event.Delta.Type == "text_delta" && opt.Progress != nil {
				opt.Progress(llm.Progress{Attempt: 1, Delta: v.Event.Delta.Text})
			}
			// thinking/signature/redacted-thinking events are discarded. They are
			// never emitted through Progress, Response, Usage.Raw or errors.
		case "assistant":
			if len(v.Error) > 0 && string(v.Error) != "null" {
				return llm.Response{}, assistantFailure(v.Error)
			}
			if !initialized {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			if !publicID(v.Message.ID) || !publicID(v.Message.Model) {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			if r.ID != "" && r.ID != v.Message.ID {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			r.ID, r.Model = v.Message.ID, v.Message.Model
			var parts []string
			for _, b := range v.Message.Content {
				switch b.Type {
				case "text":
					parts = append(parts, b.Text)
				case "thinking", "redacted_thinking": // private; ignore
				default:
					return llm.Response{}, toolStreamFailure()
				}
			}
			text += strings.Join(parts, "")
			if len(text) > 8<<20 {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			if v.Message.StopReason != "" {
				if v.Message.StopReason == "tool_use" {
					return llm.Response{}, toolStreamFailure()
				}
				stopReason = v.Message.StopReason
			}
		case "result":
			if v.IsError || v.Subtype != "success" {
				return llm.Response{}, streamFailure(v)
			}
			if !initialized {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			finished = true
			if text == "" {
				text = v.Result
			}
			if len(text) > 8<<20 {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			var err error
			r.Usage, err = normalizeUsage(v.Usage)
			if err != nil {
				return llm.Response{}, err
			}
		case "error":
			return llm.Response{}, streamFailure(v)
		case "rate_limit_event":
			switch v.RateLimit.Status {
			case "allowed", "allowed_warning": // metadata only; no quota estimates
			case "rejected":
				return llm.Response{}, &Error{Code: "rate_limited"}
			default:
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
		case "user", "tool_progress", "tool_use_summary":
			return llm.Response{}, toolStreamFailure()
		default:
			// The supported text-only contract does not accept control/tool/hook
			// messages. Do not interpret arbitrary output as a tool request.
			if !initialized {
				return llm.Response{}, malformedStream(streamInitUnknownShape)
			}
			return llm.Response{}, &Error{Code: "malformed_stream"}
		}
	}
	if scanner.Err() != nil || !finished {
		if !initialized {
			return llm.Response{}, malformedStream(streamInitUnknownShape)
		}
		return llm.Response{}, &Error{Code: "malformed_stream"}
	}
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if stopReason == "max_tokens" {
		r.Stop = llm.StopMaxOutputTokens
	} else if stopReason == "refusal" {
		r.Stop = llm.StopRefused
	}
	r.Output = []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}
	return r, nil
}

func streamPermissionMode(raw jsontext.Value, required bool) error {
	if len(raw) == 0 && !required {
		return nil
	}
	reason, wrongType := streamInitPermissionMode, streamInitWrongFieldType
	if !required {
		reason, wrongType = streamStatusPermissionMode, streamStatusWrongFieldType
	} else if len(raw) == 0 {
		return malformedStream(streamInitMissingRequired)
	} else if raw.Kind() == 'n' {
		return malformedStream(streamInitNullRequired)
	}
	var mode string
	if json.Unmarshal(raw, &mode) != nil {
		return malformedStream(wrongType)
	}
	if mode == "" {
		return malformedStream(reason)
	}
	// permissionMode describes authorization decisions, not tool availability
	// or who answers prompts. --permission-prompts none is a separate transport
	// setting; the isolated invocation still reports the standard default mode.
	// No other permission mode is part of this adapter's supported contract.
	if mode != "default" {
		return initializationFailure(reason, 0)
	}
	return nil
}

func textOnlyEvent(event wireEvent) error {
	blocks := append([]block{event.ContentBlock}, event.Message.Content...)
	for _, b := range blocks {
		switch b.Type {
		case "", "text", "thinking", "redacted_thinking":
		default:
			return toolStreamFailure()
		}
	}
	if event.Delta.Type == "input_json_delta" || event.Delta.StopReason == "tool_use" || event.Message.StopReason == "tool_use" {
		return toolStreamFailure()
	}
	switch event.Type {
	case "message_start", "message_delta", "message_stop", "content_block_stop", "ping":
	case "content_block_start":
		if event.ContentBlock.Type == "" {
			return &Error{Code: "malformed_stream"}
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta", "thinking_delta", "signature_delta":
		default:
			return &Error{Code: "malformed_stream"}
		}
	default:
		return &Error{Code: "malformed_stream"}
	}
	return nil
}

func publicID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.:/", r)) {
			return false
		}
	}
	return true
}

func normalizeUsage(w *wireUsage) (llm.Usage, error) {
	u := llm.Usage{Unknown: []llm.UsageField{llm.UsageReasoning}}
	if w == nil {
		w = &wireUsage{}
	} else if w.Input != nil || w.Cached != nil || w.CacheWrite != nil || w.Output != nil {
		u.Raw = jsontext.Value(`{"source":"claude-code"}`)
	}
	fields := []struct {
		value  *int64
		field  llm.UsageField
		target *int64
	}{
		{w.Input, llm.UsageInput, &u.InputTokens}, {w.Cached, llm.UsageCachedInput, &u.CachedInputTokens},
		{w.CacheWrite, llm.UsageCacheWriteInput, &u.CacheWriteInputTokens}, {w.Output, llm.UsageOutput, &u.OutputTokens},
	}
	for _, f := range fields {
		if f.value == nil {
			u.Unknown = append(u.Unknown, f.field)
			continue
		}
		if *f.value < 0 {
			return llm.Usage{}, &Error{Code: "malformed_stream"}
		}
		*f.target = *f.value
	}
	for _, v := range []int64{u.CachedInputTokens, u.CacheWriteInputTokens} {
		if u.InputTokens > math.MaxInt64-v {
			return llm.Usage{}, &Error{Code: "malformed_stream"}
		}
		u.InputTokens += v
	}
	// Total input includes uncached, cache-read and cache-write. Missing any of
	// those components makes the total partial too.
	if (w.Cached == nil || w.CacheWrite == nil) && w.Input != nil {
		u.Unknown = append(u.Unknown, llm.UsageInput)
	}
	return u, nil
}

func streamFailure(v streamRecord) error {
	// Only classify; never retain or print backend error strings. Substrings are
	// not a tool protocol and cannot cause execution or canonical tool records.
	var codeText string
	var e struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(v.Error, &codeText) != nil {
		_ = json.Unmarshal(v.Error, &e)
	}
	parts := append(v.Errors, codeText, e.Type, e.Code, e.Message, v.Result, v.Subtype)
	if len(v.Error) > 0 {
		for _, b := range v.Message.Content {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
	}
	s := strings.ToLower(strings.Join(parts, " "))
	code := "subprocess_failure"
	switch {
	case strings.Contains(s, "not logged in"), strings.Contains(s, "not signed in"), strings.Contains(s, "authentication_error"), strings.Contains(s, "authentication_failed"), strings.Contains(s, "oauth token has expired"), strings.Contains(s, "please run /login"):
		code = "external_reauth_required"
	case strings.Contains(s, "rate_limit"), strings.Contains(s, "rate limit"), strings.Contains(s, "usage limit"), strings.Contains(s, "hit your limit"):
		code = "rate_limited"
	case strings.Contains(s, "invalid effort"), strings.Contains(s, "unsupported effort"), strings.Contains(s, "effort is not supported"):
		code = "invalid_effort"
	case strings.Contains(s, "invalid model"), strings.Contains(s, "model not found"), strings.Contains(s, "model_not_found"), strings.Contains(s, "model is not available"), strings.Contains(s, "not_found_error"):
		code = "invalid_model"
	case strings.Contains(s, "subscription"), strings.Contains(s, "oauth_org_not_allowed"), strings.Contains(s, "billing"), strings.Contains(s, "credit balance"):
		code = "subscription_unavailable"
	}
	return &Error{Code: code}
}
