package claudecode

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

type bridgeMessage struct {
	response llm.Response
	used     bool
}
type bridgeCall struct {
	call      llm.ToolCall
	name      string
	message   *bridgeMessage
	requested bool
	requestID string
	result    any
}
type bridgeCompletion struct {
	id     string
	result llm.ToolOutcome
	err    error
}
type bridgeLine struct {
	data []byte
	err  error
}

// The official SDK stdio control transport. All tool bodies are routed to the
// host's rendezvous; this file has no filesystem/shell/child executor.
func runBridgeProtocol(ctx context.Context, reader io.ReadCloser, writer io.Writer, input string, tools []bridgeTool, limits ToolBridgeConfig, opt llm.RequestOptions, probe bool) (llm.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	defer func() { cancel(); reader.Close(); workers.Wait() }()
	lines := make(chan bridgeLine, 1)
	workers.Go(func() {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 8192), 1<<20)
		total, count := 0, 0
		for scanner.Scan() {
			data := append([]byte(nil), scanner.Bytes()...)
			total += len(data)
			count++
			if total > 128<<20 || count > 131072 {
				select {
				case lines <- bridgeLine{err: &Error{Code: "malformed_stream"}}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case lines <- bridgeLine{data: data}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case lines <- bridgeLine{err: &Error{Code: "malformed_stream"}}:
		case <-ctx.Done():
		}
	})

	names := map[string]bridgeTool{}
	for _, t := range tools {
		names["mcp__"+bridgeServer+"__"+t.Name] = t
	}
	calls := map[string]*bridgeCall{}
	var order []string
	var pending []bridgeWire
	requests := map[string]bridgeWire{}
	receiptCalls := map[string]string{}
	rpcOwners, requestOwners := map[string]string{}, map[string]string{}
	results := make(chan bridgeCompletion, 1)
	active := ""
	var activeCancel context.CancelFunc
	defer func() {
		if activeCancel != nil {
			activeCancel()
		}
	}()
	initialized, controlReady, attested, sent, listed := false, false, false, false, false
	permissionsRequested, permissionsReady := false, false
	var last *bridgeMessage
	messageIDs := map[string]bool{}
	model := ""
	blockTool := false
	rounds := 0
	var accounted []llm.Usage
	startup := time.NewTimer(15 * time.Second)
	defer startup.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	statusPending := false
	if err := bridgeControl(writer, "unreal_initialize", map[string]any{"subtype": "initialize", "hooks": map[string]any{}, "sdkMcpServers": []string{bridgeServer}, "sdkMcpServerConfigs": map[string]any{bridgeServer: map[string]any{"timeout": limits.TimeoutMillis}}}); err != nil {
		return llm.Response{}, err
	}

	replyPending := func() error {
		kept := pending[:0]
		for _, f := range pending {
			var match *bridgeCall
			matches := 0
			for _, id := range order {
				call := calls[id]
				if call.name == f.Request.Message.Params.Name && sameArguments(jsontext.Value(call.call.Arguments), f.Request.Message.Params.Arguments) {
					// Prefer an unconsumed current request; old completed calls are
					// only used for the SAME transport identity, never by argument.
					if call.requested && requests[f.ID].ID == f.ID {
						continue
					}
					if call.result == nil && !call.requested {
						match, matches = call, matches+1
					}
				}
			}
			if matches > 1 {
				return &Error{Code: "bridge_ambiguous_call"}
			}
			if match == nil {
				kept = append(kept, f)
				continue
			}
			if active != "" {
				kept = append(kept, f)
				continue
			}
			if rounds >= limits.MaxToolRounds {
				return &Error{Code: "bridge_round_limit"}
			}
			opt.InputBudget -= contextengine.Estimate(match.call) + contextengine.Estimate(match.message.response.Output)
			if opt.InputBudget < 1024 {
				return &Error{Code: "bridge_context_budget"}
			}
			match.requested = true
			match.requestID = requestOwners[f.ID]
			active = match.call.CallID
			rounds++
			response := llm.Response{ID: uuid.New().String(), Model: match.message.response.Model, Stop: llm.StopComplete, Usage: unknownUsage()}
			if !match.message.used {
				response.Output = append(response.Output, match.message.response.Output...)
				response.Usage = match.message.response.Usage
				accounted = append(accounted, response.Usage)
				match.message.used = true
			}
			response.Output = append(response.Output, llm.Item{Type: llm.ItemToolCall, Data: match.call})
			callCtx, stop := context.WithTimeout(ctx, time.Duration(limits.TimeoutMillis)*time.Millisecond)
			activeCancel = stop
			workers.Go(func() {
				outcomes, err := opt.Tools(callCtx, response)
				if errors.Is(err, context.DeadlineExceeded) {
					err = &Error{Code: "bridge_tool_timeout"}
				}
				if err == nil && (len(outcomes) != 1 || outcomes[0].Result.CallID != match.call.CallID) {
					err = &Error{Code: "bridge_receipt_invalid"}
				}
				completion := bridgeCompletion{id: match.call.CallID, err: err}
				if err == nil {
					completion.result = outcomes[0]
				}
				select {
				case results <- completion:
				case <-ctx.Done():
				}
			})
			kept = append(kept, f)
		}
		pending = kept
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		case <-startup.C:
			if !sent {
				return llm.Response{}, &Error{Code: "bridge_unavailable"}
			}
		case <-poll.C:
			if controlReady && !sent && !statusPending {
				if err := bridgeControl(writer, "unreal_status", map[string]any{"subtype": "mcp_status"}); err != nil {
					return llm.Response{}, err
				}
				statusPending = true
			}
		case complete := <-results:
			if complete.err != nil {
				return llm.Response{}, safeBridgeCallbackError(complete.err)
			}
			call := calls[complete.id]
			if call == nil || active != complete.id {
				return llm.Response{}, &Error{Code: "bridge_receipt_invalid"}
			}
			content := make([]map[string]any, 0, len(complete.result.Result.Output))
			remaining := min(limits.ResultBytes, int(opt.InputBudget)-256)
			if remaining < 1024 {
				return llm.Response{}, &Error{Code: "bridge_context_budget"}
			}
			for _, output := range complete.result.Result.Output {
				if output.Kind == llm.ToolResultImage {
					image, err := bridgeImage(output.Value, remaining)
					if err != nil {
						return llm.Response{}, err
					}
					if image == nil {
						content = append(content, map[string]any{"type": "text", "text": "Unreal image receipt withheld: bounded result budget exceeded; the canonical receipt remains with Unreal."})
						complete.result.Failed = true
					} else {
						remaining -= int(contextengine.Estimate(image))
						content = append(content, image)
					}
					continue
				}
				if output.Kind != llm.ToolResultText {
					return llm.Response{}, &Error{Code: "bridge_receipt_invalid"}
				}
				text := boundedBridgeText(output.Value, max(remaining, 1024))
				if secretguard.Sensitive(output.Value) {
					text = "Unreal receipt withheld: protected content. The canonical receipt remains with Unreal."
					complete.result.Failed = true
				}
				if remaining < len(text) {
					return llm.Response{}, &Error{Code: "bridge_receipt_invalid"}
				}
				remaining -= len(text)
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			call.result = map[string]any{"content": content, "isError": complete.result.Failed}
			cost := contextengine.Estimate(call.result)
			if cost > opt.InputBudget {
				return llm.Response{}, &Error{Code: "bridge_context_budget"}
			}
			opt.InputBudget -= cost
			kept := pending[:0]
			for _, f := range pending {
				if requestOwners[f.ID] == call.requestID {
					if err := rpcResult(writer, f, call.result); err != nil {
						return llm.Response{}, err
					}
					requests[f.ID] = f
					// Associate the receipt with the opaque control request identity.
					receiptCalls[f.ID] = complete.id
				} else {
					kept = append(kept, f)
				}
			}
			pending = kept
			activeCancel()
			activeCancel = nil
			active = ""
			if err := replyPending(); err != nil {
				return llm.Response{}, err
			}
		case line := <-lines:
			if line.err != nil {
				return llm.Response{}, line.err
			}
			if recognized, err := permissionDeniedEvent(line.data, true, names); recognized {
				return llm.Response{}, err
			}
			var f bridgeWire
			if json.Unmarshal(line.data, &f) != nil {
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
			if f.Type == "control_response" {
				if f.Response.Subtype != "success" {
					return llm.Response{}, &Error{Code: "bridge_unavailable"}
				}
				switch f.Response.ID {
				case "unreal_initialize":
					if controlReady {
						return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
					}
					controlReady = true
				case "unreal_status":
					statusPending = false
					if len(f.Response.Data.Servers) > 1 {
						return llm.Response{}, toolStreamFailure()
					}
					if len(f.Response.Data.Servers) == 1 {
						s := f.Response.Data.Servers[0]
						if s.Name != bridgeServer || s.Source != "sdk" {
							return llm.Response{}, toolStreamFailure()
						}
						if s.Status == "connected" && listed {
							attested = true
						}
						if s.Status == "failed" || s.Status == "needs-auth" || s.Status == "disabled" {
							return llm.Response{}, &Error{Code: "bridge_unavailable"}
						}
					}
				case "unreal_permissions":
					if !permissionsRequested || permissionsReady {
						return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
					}
					if err := validateBridgePermissions(f.Response.Data.Permissions, names); err != nil {
						return llm.Response{}, err
					}
					permissionsReady = true
				default:
					return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
				}
				if attested && controlReady && !permissionsRequested {
					if err := bridgeControl(writer, "unreal_permissions", map[string]any{"subtype": "list_permission_rules"}); err != nil {
						return llm.Response{}, err
					}
					permissionsRequested = true
				}
				if attested && controlReady && permissionsReady && !sent {
					if probe {
						return llm.Response{}, nil
					}
					if err := writeBridgeFrame(writer, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": input}, "parent_tool_use_id": nil}); err != nil {
						return llm.Response{}, err
					}
					sent = true
					startup.Stop()
					if opt.Progress != nil {
						opt.Progress(llm.Progress{Attempt: 1, Reset: true})
					}
				}
				continue
			}
			if f.Type == "control_request" {
				if f.ID == "" || len(f.ID) > 256 || f.Request.Subtype != "mcp_message" || f.Request.Server != bridgeServer || f.Request.Message.Version != "2.0" {
					return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
				}
				if old, ok := requests[f.ID]; ok {
					if !sameBridgeRequest(old, f) {
						return llm.Response{}, &Error{Code: "bridge_duplicate_conflict"}
					}
					if call := calls[receiptCalls[f.ID]]; call != nil && call.result != nil {
						if err := rpcResult(writer, f, call.result); err != nil {
							return llm.Response{}, err
						}
					}
					continue
				}
				switch f.Request.Message.Method {
				case "initialize":
					v := f.Request.Message.Params.Version
					if !slices.Contains([]string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}, v) {
						return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
					}
					if err := rpcResult(writer, f, map[string]any{"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": bridgeServer, "version": "1.0.0"}}); err != nil {
						return llm.Response{}, err
					}
				case "notifications/initialized", "notifications/cancelled":
					if f.Request.Message.Method == "notifications/cancelled" {
						return llm.Response{}, &Error{Code: "bridge_tool_canceled"}
					}
					if err := rpcResult(writer, f, map[string]any{}); err != nil {
						return llm.Response{}, err
					}
				case "tools/list":
					listed = true
					if err := rpcResult(writer, f, map[string]any{"tools": tools}); err != nil {
						return llm.Response{}, err
					}
				case "tools/call":
					if !sent || !initialized {
						return llm.Response{}, toolStreamFailure()
					}
					if _, ok := names["mcp__"+bridgeServer+"__"+f.Request.Message.Params.Name]; !ok {
						return llm.Response{}, &Error{Code: "bridge_unknown_tool"}
					}
					var args map[string]any
					if json.Unmarshal(f.Request.Message.Params.Arguments, &args) != nil || args == nil {
						return llm.Response{}, &Error{Code: "bridge_arguments_invalid"}
					}
					if len(pending) > 128 || len(requests) > 1024 {
						return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
					}
					key, err := bridgeRPCIdentity(f.Request.Message.ID)
					if err != nil {
						return llm.Response{}, err
					}
					owner := rpcOwners[key]
					if owner != "" {
						if !sameBridgeRequest(requests[owner], f) {
							return llm.Response{}, &Error{Code: "bridge_duplicate_conflict"}
						}
					} else {
						owner, rpcOwners[key] = f.ID, f.ID
					}
					requestOwners[f.ID] = owner
					requests[f.ID] = f
					if call := calls[receiptCalls[owner]]; call != nil && call.result != nil {
						receiptCalls[f.ID] = receiptCalls[owner]
						if err := rpcResult(writer, f, call.result); err != nil {
							return llm.Response{}, err
						}
						continue
					}
					pending = append(pending, f)
					if err := replyPending(); err != nil {
						return llm.Response{}, err
					}
				default:
					return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
				}
				continue
			}
			if f.Type == "control_cancel_request" {
				return llm.Response{}, &Error{Code: "bridge_tool_canceled"}
			}
			if !sent {
				return llm.Response{}, &Error{Code: "bridge_protocol_failure"}
			}
			var v streamRecord
			if json.Unmarshal(line.data, &v) != nil {
				return llm.Response{}, streamDecodeFailure(line.data, errors.New("schema"), initialized)
			}
			if v.ParentToolUseID != "" || v.SubagentType != "" || v.Type != "user" && len(v.ToolUseResult) > 0 && string(v.ToolUseResult) != "null" {
				return llm.Response{}, toolStreamFailure()
			}
			switch v.Type {
			case "system":
				if err := bridgeSystem(line.data, v, initialized, names); err != nil {
					return llm.Response{}, err
				}
				if v.Subtype == "init" {
					initialized = true
					model = v.Model
				}
			case "stream_event":
				if !initialized {
					return llm.Response{}, &Error{Code: "malformed_stream"}
				}
				switch v.Event.Type {
				case "content_block_start":
					blockTool = v.Event.ContentBlock.Type == "tool_use"
					if blockTool {
						if _, ok := names[v.Event.ContentBlock.Name]; !ok {
							return llm.Response{}, toolStreamFailure()
						}
					} else if err := textOnlyEvent(v.Event); err != nil {
						return llm.Response{}, err
					}
				case "content_block_delta":
					if v.Event.Delta.Type == "input_json_delta" {
						if !blockTool {
							return llm.Response{}, toolStreamFailure()
						}
					} else if err := textOnlyEvent(v.Event); err != nil {
						return llm.Response{}, err
					}
					if v.Event.Delta.Type == "text_delta" && opt.Progress != nil {
						opt.Progress(llm.Progress{Attempt: 1, Delta: v.Event.Delta.Text})
					}
				case "message_delta": // A custom tool stop reason is expected.
					if v.Event.Delta.StopReason != "tool_use" {
						if err := textOnlyEvent(v.Event); err != nil {
							return llm.Response{}, err
						}
					}
				case "content_block_stop":
					blockTool = false
				default:
					if err := textOnlyEvent(v.Event); err != nil {
						return llm.Response{}, err
					}
				}
			case "assistant":
				if len(v.Error) > 0 && string(v.Error) != "null" {
					return llm.Response{}, assistantFailure(v.Error)
				}
				if !initialized || !publicID(v.Message.ID) || !publicID(v.Message.Model) || messageIDs[v.Message.ID] {
					return llm.Response{}, &Error{Code: "malformed_stream"}
				}
				messageIDs[v.Message.ID] = true
				usage, err := normalizeUsage(v.Message.Usage)
				if err != nil {
					return llm.Response{}, err
				}
				message := &bridgeMessage{response: llm.Response{ID: uuid.New().String(), Model: v.Message.Model, Stop: llm.StopComplete, Usage: usage}}
				var text strings.Builder
				for _, b := range v.Message.Content {
					switch b.Type {
					case "text":
						text.WriteString(b.Text)
					case "thinking", "redacted_thinking": // discard private state
					case "tool_use":
						t, ok := names[b.Name]
						if !ok || !publicID(b.ID) || calls[b.ID] != nil {
							return llm.Response{}, toolStreamFailure()
						}
						var args map[string]any
						if json.Unmarshal(b.Input, &args) != nil || args == nil {
							return llm.Response{}, &Error{Code: "bridge_arguments_invalid"}
						}
						calls[b.ID] = &bridgeCall{call: llm.ToolCall{CallID: b.ID, Name: t.original, Arguments: string(b.Input)}, name: t.Name, message: message}
						order = append(order, b.ID)
					default:
						return llm.Response{}, toolStreamFailure()
					}
				}
				if text.Len() > 8<<20 {
					return llm.Response{}, &Error{Code: "malformed_stream"}
				}
				if text.Len() > 0 {
					message.response.Output = []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text.String()}}}
				}
				if v.Message.StopReason == "max_tokens" {
					message.response.Stop = llm.StopMaxOutputTokens
				}
				if v.Message.StopReason == "refusal" {
					message.response.Stop = llm.StopRefused
				}
				last, model = message, v.Message.Model
				if err := replyPending(); err != nil {
					return llm.Response{}, err
				}
			case "user":
				// SDK-generated tool-result echoes are not a second canonical
				// receipt. Accept only results whose host receipt was delivered.
				if len(v.Message.Content) == 0 {
					return llm.Response{}, toolStreamFailure()
				}
				for _, b := range v.Message.Content {
					if b.Type != "tool_result" || calls[b.ToolUseID] == nil || calls[b.ToolUseID].result == nil {
						return llm.Response{}, toolStreamFailure()
					}
				}
			case "tool_progress", "tool_use_summary":
				// Never guess an execution owner from an uncorrelated event.
				var p struct {
					ID   string   `json:"tool_use_id"`
					IDs  []string `json:"preceding_tool_use_ids"`
					Name string   `json:"tool_name"`
				}
				if json.Unmarshal(line.data, &p) != nil {
					return llm.Response{}, toolStreamFailure()
				}
				if v.Type == "tool_progress" {
					p.IDs = []string{p.ID}
					if _, ok := names[p.Name]; !ok {
						return llm.Response{}, toolStreamFailure()
					}
				}
				if len(p.IDs) == 0 {
					return llm.Response{}, toolStreamFailure()
				}
				for _, id := range p.IDs {
					if calls[id] == nil || !calls[id].requested {
						return llm.Response{}, toolStreamFailure()
					}
				}
			case "rate_limit_event":
				if v.RateLimit.Status == "rejected" {
					return llm.Response{}, &Error{Code: "rate_limited"}
				}
				if v.RateLimit.Status != "allowed" && v.RateLimit.Status != "allowed_warning" {
					return llm.Response{}, &Error{Code: "malformed_stream"}
				}
			case "result":
				if v.IsError || v.Subtype != "success" {
					return llm.Response{}, streamFailure(v)
				}
				if !initialized || active != "" || len(pending) > 0 || last == nil || last.used {
					return llm.Response{}, &Error{Code: "malformed_stream"}
				}
				for _, call := range calls {
					if call.result == nil {
						return llm.Response{}, &Error{Code: "malformed_stream"}
					}
				}
				r := last.response
				if len(r.Output) == 0 && v.Result != "" {
					r.Output = []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: v.Result}}}
				}
				r.Model = model
				// Result usage is the request aggregate, not another response's
				// usage. Subtract already committed per-model round observations.
				var err error
				r.Usage, err = remainingBridgeUsage(v.Usage, accounted)
				if err != nil {
					return llm.Response{}, err
				}
				return r, nil
			case "error":
				return llm.Response{}, streamFailure(v)
			default:
				return llm.Response{}, &Error{Code: "malformed_stream"}
			}
		}
	}
}

// Convert only the already materialized, canonical ViewImage receipt. This
// adapter never fetches an URL or opens an image file itself.
func bridgeImage(value string, budget int) (map[string]any, error) {
	header, data, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
		return nil, &Error{Code: "bridge_receipt_invalid"}
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if !slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, mime) {
		return nil, &Error{Code: "bridge_receipt_invalid"}
	}
	if len(data)+256 > budget {
		return nil, nil
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(data); err != nil || data == "" {
		return nil, &Error{Code: "bridge_receipt_invalid"}
	}
	return map[string]any{"type": "image", "data": data, "mimeType": mime}, nil
}

// MCP's JSON-RPC request identity and the CLI control-envelope identity are
// distinct. Retries may replace the envelope while retaining the same RPC id.
// Never deduplicate a new request merely because its arguments match an old one.
func bridgeRPCIdentity(raw jsontext.Value) (string, error) {
	if len(raw) == 0 || len(raw) > 256 {
		return "", &Error{Code: "bridge_protocol_failure"}
	}
	// MCP RequestId is a string or integer. Keep integer identities exact;
	// float decoding could merge different IDs above 2^53 and hide a new call.
	var id any
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" {
			return "", &Error{Code: "bridge_protocol_failure"}
		}
		id = value
	} else {
		var value int64
		if (raw[0] < '0' || raw[0] > '9') && raw[0] != '-' || json.Unmarshal(raw, &value) != nil {
			return "", &Error{Code: "bridge_protocol_failure"}
		}
		id = value
	}
	b, err := json.Marshal(id, json.Deterministic(true))
	if err != nil {
		return "", &Error{Code: "bridge_protocol_failure"}
	}
	return string(b), nil
}

func sameBridgeRequest(a, b bridgeWire) bool {
	x, e1 := bridgeRPCIdentity(a.Request.Message.ID)
	y, e2 := bridgeRPCIdentity(b.Request.Message.ID)
	return e1 == nil && e2 == nil && x == y && a.Request.Message.Method == b.Request.Message.Method && a.Request.Message.Params.Name == b.Request.Message.Params.Name && sameArguments(a.Request.Message.Params.Arguments, b.Request.Message.Params.Arguments)
}

func unknownUsage() llm.Usage { u, _ := normalizeUsage(nil); return u }
func remainingBridgeUsage(w *wireUsage, previous []llm.Usage) (llm.Usage, error) {
	u, err := normalizeUsage(w)
	if err != nil {
		return u, err
	}
	for _, p := range previous {
		for _, field := range []struct {
			kind  llm.UsageField
			value *int64
			used  int64
		}{{llm.UsageInput, &u.InputTokens, p.InputTokens}, {llm.UsageCachedInput, &u.CachedInputTokens, p.CachedInputTokens}, {llm.UsageCacheWriteInput, &u.CacheWriteInputTokens, p.CacheWriteInputTokens}, {llm.UsageOutput, &u.OutputTokens, p.OutputTokens}} {
			if slices.Contains(p.Unknown, field.kind) || slices.Contains(u.Unknown, field.kind) {
				if !slices.Contains(u.Unknown, field.kind) {
					u.Unknown = append(u.Unknown, field.kind)
				}
				*field.value = 0
			} else {
				*field.value -= field.used
				if *field.value < 0 {
					return llm.Usage{}, &Error{Code: "malformed_stream"}
				}
			}
		}
	}
	return u, nil
}

func safeBridgeCallbackError(err error) error {
	if contextengine.IsBudgetError(err) {
		return &Error{Code: "bridge_context_budget"}
	}
	if errors.Is(err, llm.ErrToolIdentityConflict) {
		return &Error{Code: "bridge_duplicate_conflict"}
	}
	if errors.Is(err, llm.ErrToolRecoveryRequired) {
		return &Error{Code: "bridge_recovery_required"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "bridge_tool_timeout"}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var safe *Error
	if errors.As(err, &safe) {
		return safe
	}
	// No internal filesystem/actor errors leak into provider or history.
	return &Error{Code: "bridge_host_failure"}
}

func bridgeSystem(line []byte, v streamRecord, initialized bool, names map[string]bridgeTool) error {
	for _, name := range v.Tools {
		if _, ok := names[name]; !ok {
			return initializationFailure(streamInitToolsNonempty, len(v.Tools))
		}
	}
	for _, raw := range v.MCP {
		var server struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if json.Unmarshal(raw, &server) != nil || server.Name != bridgeServer || server.Status != "connected" {
			return initializationFailure(streamInitMCPNonempty, len(v.MCP))
		}
	}
	if len(v.MCP) > 1 {
		return initializationFailure(streamInitMCPNonempty, len(v.MCP))
	}
	switch v.Subtype {
	case "permission_denied":
		_, err := permissionDeniedEvent(line, true, names)
		return err
	case "init":
		if initialized {
			return initializationFailure(streamInitDuplicate, 0)
		}
		if v.Tools == nil || v.MCP == nil || !publicID(v.Model) {
			return initRequiredFieldFailure(line, v)
		}
		return streamPermissionMode(v.PermissionMode, true)
	case "status":
		if !initialized {
			return malformedStream(streamStatusBeforeInit)
		}
		if err := streamPermissionMode(v.PermissionMode, false); err != nil {
			return err
		}
		var status *string
		if len(v.Status) == 0 {
			return malformedStream(streamStatusMissingRequired)
		}
		if json.Unmarshal(v.Status, &status) != nil {
			return malformedStream(streamStatusWrongFieldType)
		}
		if status != nil && *status != "requesting" && *status != "compacting" {
			return malformedStream(streamStatusUnknownValue)
		}
		return nil
	case "api_retry", "thinking_tokens", "informational", "notification", "session_state_changed":
		return validateSystemMetadata(line, v, initialized)
	case "task_started", "task_progress", "task_updated", "task_notification":
		return toolStreamFailure()
	default:
		return unknownSystemShape(line)
	}
}
