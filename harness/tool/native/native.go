package native

import (
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type translator struct{ action string }

func New(action string) tool.Translator { return translator{action: action} }
func Configure(current tool.StaticTranslators) tool.StaticTranslators {
	current.Read = New("read")
	current.Write = New("write")
	current.Edit = New("edit")
	current.Grep = New("grep")
	current.Glob = New("glob")
	return current
}
func (t translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var fields map[string]any
	if json.Unmarshal([]byte(call.Arguments), &fields) != nil {
		return tool.ErrorStatus("invalid arguments", 1024)
	}
	if _, ok := fields["version"]; ok {
		return tool.ErrorStatus("version is executor-owned", 1024)
	}
	if _, ok := fields["action"]; ok {
		return tool.ErrorStatus("action is tool-owned", 1024)
	}
	required := []string{"path"}
	if t.action == "write" {
		required = append(required, "content")
	}
	if t.action == "edit" {
		required = append(required, "old_text", "new_text")
	}
	if t.action == "grep" {
		required = append(required, "pattern")
	}
	for _, name := range required {
		if _, ok := fields[name].(string); !ok {
			return tool.ErrorStatus("required string field missing: "+name, 1024)
		}
	}
	if t.action == "write" || t.action == "edit" {
		expected, ok := fields["expected"].(map[string]any)
		if !ok {
			return tool.ErrorStatus("expected revision must be an object", 1024)
		}
		if _, ok := expected["exists"].(bool); !ok {
			return tool.ErrorStatus("expected.exists must be explicit", 1024)
		}
	}
	var request files.Request
	if err := json.Unmarshal([]byte(call.Arguments), &request, json.RejectUnknownMembers(true)); err != nil {
		return tool.ErrorStatus("invalid file tool arguments", 1024)
	}
	request.Version = files.Version
	request.Action = t.action
	spec, err := files.Spec(request)
	if err != nil {
		return tool.ErrorStatus(err.Error(), 1024)
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}
func (t translator) TranslateResult(id string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	text := status.Error
	if len(operations) == 1 && operations[0].Denial != nil {
		text = operations[0].Denial.Error()
	}
	if text == "" {
		if len(operations) != 1 {
			return llm.ToolResult{}, fmt.Errorf("native result requires one operation")
		}
		state, err := operation.DecodeRemoteJobState(operations[0])
		if err != nil {
			return llm.ToolResult{}, err
		}
		if state.Plan.Type != files.PlanType || state.Plan.Version != files.Version {
			return llm.ToolResult{}, operation.ErrUnsupported
		}
		if len(state.Handle) == 0 {
			text = "File operation is pending."
		} else {
			var result files.Result
			if err = json.Unmarshal(state.Handle, &result); err != nil {
				return llm.ToolResult{}, err
			}
			if result.Version != files.Version {
				return llm.ToolResult{}, operation.ErrUnsupported
			}
			// This bounded typed payload never comes from truncated TerminalResult.
			text = string(files.EncodeResult(result))
		}
	}
	return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}, nil
}
