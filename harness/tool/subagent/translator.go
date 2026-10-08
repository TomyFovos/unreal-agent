// Package subagent contains pure model-call translation; execution is owned by
// harness/subagent and the Host.
package subagent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	runtime "github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type translator struct {
	action    string
	owner     session.ID
	templates map[string]runtime.Template
	resolve   func(runtime.Template, *runtime.RuntimeRequest) (runtime.Template, error)
}

// BindRuntime adds optional, provider-neutral runtime arguments. Translation is
// still inert: only the existing Operation Manager executes the resulting plan.
func BindRuntime(extensions []tool.Extension, ctx context.Context, owner *host.Session, resolve runtime.RuntimeResolver) {
	if resolve == nil {
		return
	}
	for i := range extensions {
		t, ok := extensions[i].Translator.(*translator)
		if !ok || t.action != "start" {
			continue
		}
		t.resolve = func(base runtime.Template, r *runtime.RuntimeRequest) (runtime.Template, error) {
			return resolve(ctx, owner, base, r)
		}
		params := extensions[i].Definition.Tool.Parameters
		params["properties"].(map[string]any)["runtime"] = map[string]any{"type": "object", "properties": map[string]any{"provider": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "effort": map[string]any{"type": "string"}}, "required": []string{"provider", "model", "effort"}, "additionalProperties": false}
	}
}

// Extensions returns codecs for all five tools; child mode advertises only
// SendParent and Finish, and parent mode only start/send/cancel.
func Extensions(owner session.ID, templates map[string]runtime.Template, child bool) ([]tool.Extension, error) {
	data, err := json.Marshal(templates)
	if err != nil {
		return nil, err
	}
	var copied map[string]runtime.Template
	if err = json.Unmarshal(data, &copied); err != nil {
		return nil, err
	}
	definitions := []struct {
		name, action, description string
		properties                map[string]any
		required                  []string
		isChild                   bool
	}{
		{"SubagentStart", "start", "Start an independent child agent. The operation remains running; ready messages expose its handle. End your turn to wait for events.", map[string]any{"template": map[string]any{"type": "string"}, "task": map[string]any{"type": "string"}}, []string{"template", "task"}, false},
		{"SubagentSend", "send", "Send a message to an owned child using its operation handle. Acknowledgement means durable receipt, not task completion.", map[string]any{"handle": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}, []string{"handle", "text"}, false},
		{"SubagentCancel", "cancel", "Cancel an owned child by operation handle.", map[string]any{"handle": map[string]any{"type": "string"}}, []string{"handle"}, false},
		{"SendParent", "parent", "Send a progress update or question to the parent; end the turn to wait for its reply.", map[string]any{"text": map[string]any{"type": "string"}}, []string{"text"}, true},
		{"Finish", "finish", "Finish delegated work with a canonical report, only after every other operation has completed. Call Finish alone; it stops this child.", map[string]any{"status": map[string]any{"type": "string", "enum": []string{"completed", "failed"}}, "summary": map[string]any{"type": "string"}, "changedFiles": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "tests": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "blockers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, []string{"status", "summary", "changedFiles", "tests", "blockers"}, true},
	}
	var result []tool.Extension
	for _, d := range definitions {
		result = append(result, tool.Extension{Definition: tool.Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: d.name, Description: d.description, Parameters: map[string]any{"type": "object", "properties": d.properties, "required": d.required, "additionalProperties": false}}}, Translator: &translator{action: d.action, owner: owner, templates: copied}, Enabled: d.isChild == child})
	}
	return result, nil
}
func (t *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var args struct {
		Template     string                  `json:"template"`
		Task         string                  `json:"task"`
		Handle       operation.ID            `json:"handle"`
		Text         string                  `json:"text"`
		Status       string                  `json:"status"`
		Summary      string                  `json:"summary"`
		ChangedFiles []string                `json:"changedFiles"`
		Tests        []string                `json:"tests"`
		Blockers     []string                `json:"blockers"`
		Runtime      *runtime.RuntimeRequest `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true)); err != nil {
		return tool.ErrorStatus("Invalid subagent arguments", 0)
	}
	p := runtime.Plan{Version: 1, Action: t.action, ParentID: t.owner, Handle: args.Handle, Text: args.Text}
	if t.action == "start" {
		config, ok := t.templates[args.Template]
		if !ok {
			return tool.ErrorStatus("Unknown child template", 0)
		}
		p.Template = args.Template
		if t.resolve != nil {
			var e error
			config, e = t.resolve(config, args.Runtime)
			if e != nil {
				return tool.ErrorStatus(e.Error(), 0)
			}
			p.RuntimeBound = true
		} else if args.Runtime != nil {
			return tool.ErrorStatus("Child runtime selection unsupported", 0)
		}
		p.Configuration = &config
		p.Text = args.Task
	}
	if t.action == "finish" {
		eligibility, ok := ctx.(interface{ CanFinish() bool })
		if !ok || !eligibility.CanFinish() {
			return tool.ErrorStatus("Finish requires no other unfinished calls or operations", 0)
		}
		p.Result = &sessionstore.FinishResult{Status: args.Status, Summary: args.Summary, ChangedFiles: args.ChangedFiles, Tests: args.Tests, Blockers: args.Blockers}
	}
	spec, err := runtime.NewSpec(p)
	if err != nil {
		return tool.ErrorStatus(err.Error(), 0)
	}
	id := ctx.Submit(spec)
	return tool.CallStatus{WaitingFor: []operation.ID{id}}
}
func (t *translator) TranslateResult(id string, status tool.CallStatus, ops []operation.Operation) (llm.ToolResult, error) {
	text := status.Error
	if text == "" {
		if len(ops) != 1 {
			return llm.ToolResult{}, fmt.Errorf("expected one subagent operation")
		}
		op := ops[0]
		s, err := operation.DecodeRemoteJobState(op)
		if err != nil {
			return llm.ToolResult{}, err
		}
		if op.Denial != nil {
			text = op.Denial.Error()
		} else {
			value := struct {
				Handle  operation.ID
				Status  operation.Status
				Message string
				Result  *runtime.Handle
			}{Handle: op.ID, Status: op.Status, Message: s.TerminalResult}
			if s.TerminalError != "" {
				value.Message = s.TerminalError
			}
			if t.action == "start" && len(s.Handle) > 0 {
				handle, err := runtime.DecodeHandle(op)
				if err != nil {
					return llm.ToolResult{}, err
				}
				value.Result = &handle
			}
			data, err := json.Marshal(value)
			if err != nil {
				return llm.ToolResult{}, err
			}
			text = string(data)
		}
	}
	return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}, nil
}
