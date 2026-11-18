// Package contexttools exposes Session context actions as native model tools.
package contexttools

import (
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const Guidance = "Use StateShow for current Session state and RecordShow or RecordFind for canonical evidence. Record your explicit decisions, plans, user rules, and child reviews with the State tools. Start a question to the user with " + contextstate.QuestionMarker + ". Use StateAnswer to link the open question to the user message that answers it."

func Extensions(disallowed []string) []tool.Extension {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	list := map[string]any{"type": "array", "items": str}
	rejected := map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "properties": map[string]any{"alternative": str, "reason": str},
		"required": []string{"alternative", "reason"}, "additionalProperties": false,
	}}
	definitions := []struct {
		action      contextstate.Action
		description string
		properties  map[string]any
		required    []string
	}{
		{contextstate.Show, "Show the current Session state and its evidence locators.", map[string]any{}, []string{}},
		{contextstate.Decide, "Record an explicit coordinator decision. Supply its reason, evidence locators, alternatives and related IDs when available.", map[string]any{
			"id": str, "decision": str, "why": str, "basis": list, "rejected": rejected, "plan": list, "children": list, "requests": list,
		}, []string{"decision"}},
		{contextstate.SetPlan, "Add or update an ordered plan step by ID. Completed and dropped steps settle into counts and locators.", map[string]any{
			"id": str, "text": str, "position": integer, "owner": str, "depends_on": list, "decisions": list,
			"status": map[string]any{"type": "string", "enum": []string{"todo", "doing", "done", "dropped"}},
		}, []string{"id"}},
		{contextstate.AddRule, "Record a rule the user explicitly gave. Supply an exact excerpt and its user message locator.", map[string]any{"id": str, "excerpt": str, "source": str}, []string{"excerpt", "source"}},
		{contextstate.Accept, "Record acceptance of an owned child. The same review is used by /analyze. The reason is optional.", map[string]any{"handle": str, "why": str}, []string{"handle"}},
		{contextstate.Reject, "Record rejection of an owned child. The same review is used by /analyze. The reason is optional.", map[string]any{"handle": str, "why": str}, []string{"handle"}},
		{contextstate.AskUser, "Record a user question or link its assistant message locator. Ask the user in your reply to make the question visible.", map[string]any{"id": str, "text": str, "source": str}, []string{}},
		{contextstate.AnswerUser, "Mark a question answered by linking its ID to the exact user message locator that answers it.", map[string]any{"id": str, "source": str}, []string{"id", "source"}},
		{contextstate.ShowRecord, "Read a canonical record or its selected section or range. Set page_bytes to receive a continuation locator.", map[string]any{"locator": str, "page_bytes": integer}, []string{"locator"}},
		{contextstate.FindRecord, "Find canonical evidence in this Session and its children by case-sensitive text, kind, handle or RFC3339 time range.", map[string]any{
			"text": str, "kind": str, "handle": str, "from": str, "before": str,
		}, []string{}},
	}
	var out []tool.Extension
	for _, d := range definitions {
		name := string(d.action)
		out = append(out, tool.Extension{
			Definition: tool.Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: name, Description: d.description,
				Parameters: map[string]any{"type": "object", "properties": d.properties, "required": d.required, "additionalProperties": false}}},
			Translator: translator{action: d.action}, Enabled: !slices.Contains(disallowed, name),
		})
	}
	return out
}

type translator struct{ action contextstate.Action }

func (t translator) Translate(c tool.Context, call llm.ToolCall) tool.CallStatus {
	cmd := contextstate.Command{Action: t.action, Args: []byte(call.Arguments)}
	data, err := json.Marshal(cmd)
	if err != nil {
		return tool.CallStatus{Error: "invalid context command arguments"}
	}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: contextstate.OperationType, Version: 1, Data: data})
	if err != nil {
		return tool.CallStatus{Error: err.Error()}
	}
	return tool.CallStatus{WaitingFor: []operation.ID{c.Submit(spec)}}
}

func (translator) TranslateResult(id string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	output := func(text string) llm.ToolResult {
		return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}
	}
	if status.Error != "" {
		return output(status.Error), nil
	}
	if len(operations) != 1 {
		return llm.ToolResult{}, fmt.Errorf("context command requires one operation")
	}
	op := operations[0]
	if op.Denial != nil {
		return output(op.Denial.Error()), nil
	}
	job, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return llm.ToolResult{}, err
	}
	switch op.Status {
	case operation.StatusCompleted:
		return output(string(job.Handle)), nil
	case operation.StatusFailed:
		var reason string
		if json.Unmarshal(job.Handle, &reason) == nil && reason != "" {
			return output(reason), nil
		}
		return output(job.TerminalError), nil
	case operation.StatusCanceled:
		return output("Context command canceled."), nil
	default:
		return output("Context command pending."), nil
	}
}
