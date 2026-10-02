package dap

import (
	"encoding/json/v2"
	"errors"
	debug "github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const Name = "DAP"

type translator struct{ generation string }

func New(generation string) tool.Translator { return &translator{generation: generation} }
func (t *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	if len(call.Arguments) > debug.MaxResultBytes {
		return tool.ErrorStatus("DAP request too large", 1024)
	}
	var request debug.Request
	if json.Unmarshal([]byte(call.Arguments), &request, json.RejectUnknownMembers(true)) != nil {
		return tool.ErrorStatus("invalid DAP arguments", 1024)
	}
	if request.OwnerGeneration != "" || request.Version != 0 {
		return tool.ErrorStatus("DAP runtime fields are set by the Host", 1024)
	}
	request.OwnerGeneration = t.generation
	request.Version = 1
	spec, err := debug.NewSpec(request)
	if err != nil {
		return tool.ErrorStatus(err.Error(), 1024)
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}
func (t *translator) TranslateResult(callID string, status tool.CallStatus, ops []operation.Operation) (llm.ToolResult, error) {
	result := llm.ToolResult{CallID: callID}
	text := status.Error
	if status.Denial != nil {
		text = status.Denial.Error()
	}
	if text == "" {
		if len(ops) != 1 {
			return result, errors.New("DAP result must contain one operation")
		}
		if ops[0].Denial != nil {
			text = ops[0].Denial.Error()
		} else {
			state, err := operation.DecodeRemoteJobState(ops[0])
			if err != nil {
				return result, err
			}
			if state.Plan.Type != debug.PlanType || state.Plan.Version != debug.PlanVersion {
				return result, errors.New("unsupported DAP result")
			}
			switch ops[0].Status {
			case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
				text = "Debugger operation is running."
			default:
				var outcome debug.Result
				if len(state.Handle) > debug.MaxResultBytes || json.Unmarshal(state.Handle, &outcome, json.RejectUnknownMembers(true)) != nil || outcome.Version != 1 {
					return result, errors.New("invalid typed DAP result")
				}
				data, err := json.Marshal(outcome)
				if err != nil {
					return result, err
				}
				text = string(data)
			}
		}
	}
	result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}
	return result, nil
}
func Definition() tool.Definition {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	integer := func() map[string]any { return map[string]any{"type": "integer", "minimum": 0} }
	return tool.Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: Name, Description: "Operate a Host-owned debugger. Use returned handle/generation and current stop_epoch for frame/variable references. Launch and attach require a configured adapter. Expired sessions need a new explicit start; uncertain commands are never replayed.", Parameters: map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"handle", "command"},
		"properties": map[string]any{
			"handle":    map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{"id": str(), "generation": str()}, "additionalProperties": false},
			"command":   map[string]any{"type": "string", "enum": []string{"launch", "attach", "setBreakpoints", "threads", "stackTrace", "scopes", "variables", "evaluate", "continue", "pause", "next", "stepIn", "stepOut", "inspect", "disconnect"}},
			"start":     map[string]any{"type": "object", "required": []string{"adapter"}, "additionalProperties": false, "properties": map[string]any{"adapter": str(), "program": str(), "arguments": map[string]any{"type": "array", "items": str()}, "directory": str(), "process_id": integer(), "stop_on_entry": map[string]any{"type": "boolean"}}},
			"thread_id": integer(), "frame_id": integer(), "variables_reference": integer(), "stop_epoch": integer(), "expression": str(), "source": str(), "offset": integer(), "count": map[string]any{"type": "integer", "minimum": 1, "maximum": debug.MaxItems}, "terminate": map[string]any{"type": "boolean"},
			"breakpoints": map[string]any{"type": "array", "maxItems": debug.MaxItems, "items": map[string]any{"type": "object", "required": []string{"line"}, "additionalProperties": false, "properties": map[string]any{"line": map[string]any{"type": "integer", "minimum": 1}, "column": integer()}}},
		}}}}
}
