package ast

import (
	"encoding/json/v2"
	"errors"
	engine "github.com/unreallabsai/unreal-agent/harness/ast"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type translator struct{ edit bool }

func Configure(current tool.StaticTranslators) tool.StaticTranslators {
	current.ASTGrep = translator{}
	current.ASTEdit = translator{edit: true}
	return current
}
func (t translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var raw map[string]any
	if json.Unmarshal([]byte(call.Arguments), &raw) != nil {
		return tool.ErrorStatus("invalid AST arguments", 1024)
	}
	if _, ok := raw["version"]; ok {
		return tool.ErrorStatus("version is executor-owned", 1024)
	}
	var request engine.Request
	if json.Unmarshal([]byte(call.Arguments), &request, json.RejectUnknownMembers(true)) != nil {
		return tool.ErrorStatus("invalid AST arguments", 1024)
	}
	request.Version = engine.Version
	if !t.edit && (request.Replacement != nil || request.Apply || request.ExpectedMatches != nil) {
		return tool.ErrorStatus("ast_grep cannot rewrite", 1024)
	}
	if t.edit && request.Replacement == nil {
		return tool.ErrorStatus("replacement must be explicit", 1024)
	}
	spec, err := engine.Spec(request)
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
			return llm.ToolResult{}, errors.New("AST result requires one operation")
		}
		state, err := operation.DecodeRemoteJobState(operations[0])
		if err != nil {
			return llm.ToolResult{}, err
		}
		if state.Plan.Type != engine.PlanType || state.Plan.Version != engine.Version {
			return llm.ToolResult{}, operation.ErrUnsupported
		}
		text = "Structural operation is pending."
		if len(state.Handle) > 0 {
			var result engine.Result
			if err = json.Unmarshal(state.Handle, &result); err != nil {
				return llm.ToolResult{}, err
			}
			if result.Version != engine.Version {
				return llm.ToolResult{}, operation.ErrUnsupported
			}
			encoded, _ := json.Marshal(result)
			text = string(encoded)
		}
	}
	return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}, nil
}
