package native

import (
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
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

func (t translator) ResultFailed(status tool.CallStatus, operations []operation.Operation) bool {
	if status.Error != "" || status.Denial != nil || len(operations) != 1 || operations[0].Denial != nil || operations[0].Status != operation.StatusCompleted {
		return true
	}
	// The Registry owns t.action; the executor owns the plan and typed receipt.
	// ToolName can be empty in legacy records, but must agree when present.
	if operations[0].ToolName != "" && operations[0].ToolName != t.action {
		return true
	}
	state, err := operation.DecodeRemoteJobState(operations[0])
	if err != nil || state.Plan.Type != files.PlanType || state.Plan.Version != files.Version {
		return true
	}
	var request files.Request
	if json.Unmarshal(state.Plan.Data, &request) != nil || request.Version != files.Version || request.Action != t.action {
		return true
	}
	var result files.Result
	if json.Unmarshal(state.Handle, &result) != nil || result.Version != files.Version || result.Denial != nil {
		return true
	}
	// These receipts preserve the request's literal path. Search results use
	// individual match paths instead and do not carry a result-level Path.
	if t.action == "read" || t.action == "write" || t.action == "edit" {
		if request.Path == "" || result.Path != request.Path {
			return true
		}
	}
	switch t.action {
	case "read":
		return result.Mutation != nil || !(result.Code == "ok" || result.Code == "binary" && result.Binary)
	case "grep", "glob":
		return result.Mutation != nil || result.Code != "ok"
	case "write", "edit":
		// "binary" is a successful Read observation, but an Edit preparation
		// refusal. Mutations require actual applied evidence, not just an outer
		// success code or a completed Operation (which also covers no_match).
		m := result.Mutation
		if result.Code != "applied" || m == nil || m.Version != mutation.Version || m.Code != mutation.Applied || m.Denial != nil || len(m.Targets) != 1 || result.Path == "" {
			return true
		}
		target := m.Targets[0]
		return target.Code != mutation.Applied || target.Path != result.Path || !target.Revision.Exists || target.Revision.Validate() != nil
	default:
		return true
	}
}
