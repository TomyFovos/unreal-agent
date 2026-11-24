// Package subagent contains pure model-call translation; execution is owned by
// harness/subagent and the Host.
package subagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

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
	child     bool
	templates map[string]runtime.Template
	resolve   func(string, runtime.Template, *runtime.RuntimeRequest) (runtime.Template, error)
}

// BindRuntime exposes model selection wherever the Host binds child runtimes.
func BindRuntime(extensions []tool.Extension, ctx context.Context, owner *host.Session, resolve runtime.RuntimeResolver) {
	if resolve == nil {
		return
	}
	for i := range extensions {
		t, ok := extensions[i].Translator.(*translator)
		if !ok || t.action != "start" {
			continue
		}
		t.resolve = func(name string, base runtime.Template, r *runtime.RuntimeRequest) (runtime.Template, error) {
			return resolve(ctx, owner, name, base, r)
		}
		props := extensions[i].Definition.Tool.Parameters["properties"].(map[string]any)
		props["model"] = map[string]any{"type": "string", "description": "Model ID in an available provider catalog. Omit to use the role's selection."}
		props["provider"] = map[string]any{"type": "string", "description": "Registered provider ID. Required when multiple providers offer the model."}
		props["effort"] = map[string]any{"type": "string", "description": "Reasoning effort offered by the selected model."}
		if !t.child {
			props["runtime"] = map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"provider": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "effort": map[string]any{"type": "string"}},
				"required":             []string{"provider", "model", "effort"},
				"additionalProperties": false,
			}
		}
	}
}

// Extensions returns codecs for the subagent tools. A child advertises SendParent
// and Finish, and a parent start/send/cancel/approve/review; a child with worker
// templates is both.
func Extensions(owner session.ID, templates map[string]runtime.Template, child bool) ([]tool.Extension, error) {
	data, err := json.Marshal(templates)
	if err != nil {
		return nil, err
	}
	var copied map[string]runtime.Template
	if err = json.Unmarshal(data, &copied); err != nil {
		return nil, err
	}
	brief := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"title":               map[string]any{"type": "string", "description": "One-line task title"},
		"purpose":             map[string]any{"type": "string", "description": "What the work will help decide"},
		"inputs":              map[string]any{"type": "string", "description": "Paths, references and sections to read"},
		"outputs":             map[string]any{"type": "string", "description": "Artifacts, formats and destinations"},
		"verification":        map[string]any{"type": "string", "description": "Checks to run and expected results"},
		"acceptance_criteria": map[string]any{"type": "string", "description": "What the parent will check before accepting, including evidence, completeness, verification, format and scope as applicable"},
	}}
	definitions := []struct {
		name, action, description string
		properties                map[string]any
		required                  []string
		isChild                   bool
	}{
		{"SubagentStart", "start", "Start a child. Supply optional brief fields and/or task instructions. The harness adds launch conditions and report guidance. End the turn to wait.", map[string]any{"template": map[string]any{"type": "string"}, "task": map[string]any{"type": "string"}, "brief": brief}, []string{"template"}, false},
		{"SubagentSend", "send", "Message an owned child by operation handle; acknowledgement confirms receipt only.", map[string]any{"handle": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}, []string{"handle", "text"}, false},
		{"SubagentCancel", "cancel", "Cancel an owned child by operation handle.", map[string]any{"handle": map[string]any{"type": "string"}}, []string{"handle"}, false},
		{"SubagentApprove", "approve", "Allow or deny an owned child request; escalate to your parent/user if unsure.", map[string]any{"handle": map[string]any{"type": "string"}, "request": map[string]any{"type": "string"}, "allow": map[string]any{"type": "boolean"}}, []string{"handle", "request", "allow"}, false},
		{"SubagentReview", "review", "Optionally record your accept/reject assessment of an owned child for /analyze. Reason is optional; this does not change the child's finish.", map[string]any{"handle": map[string]any{"type": "string"}, "decision": map[string]any{"type": "string", "enum": []string{"accept", "reject"}}, "reason": map[string]any{"type": "string"}}, []string{"handle", "decision"}, false},
		{"SendParent", "parent", "Message the parent. For one stored delegation brief field, send exactly 'brief <field>' to get its value in the tool result. End the turn when awaiting a parent reply.", map[string]any{"text": map[string]any{"type": "string"}, "progress": map[string]any{"type": "boolean", "description": "True: progress does not wake the parent. False (default): a brief lookup returns locally and other questions wake the parent."}}, []string{"text"}, true},
		{"Finish", "finish", "Record the final report and stop this child. Call alone after all other calls/operations finish.", map[string]any{"status": map[string]any{"type": "string", "enum": []string{"completed", "failed"}}, "summary": map[string]any{"type": "string"}, "changedFiles": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "tests": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "blockers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, []string{"status", "summary", "changedFiles", "tests", "blockers"}, true},
	}
	var result []tool.Extension
	for _, d := range definitions {
		enabled := d.isChild == child || !d.isChild && len(copied) > 0
		result = append(result, tool.Extension{Definition: tool.Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: d.name, Description: d.description, Parameters: map[string]any{"type": "object", "properties": d.properties, "required": d.required, "additionalProperties": false}}}, Translator: &translator{action: d.action, owner: owner, child: child, templates: copied}, Enabled: enabled})
	}
	return result, nil
}
func (t *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var args struct {
		Template     string                  `json:"template"`
		Task         string                  `json:"task"`
		Brief        runtime.BriefFields     `json:"brief"`
		Handle       operation.ID            `json:"handle"`
		Text         string                  `json:"text"`
		Progress     bool                    `json:"progress"`
		Status       string                  `json:"status"`
		Summary      string                  `json:"summary"`
		ChangedFiles []string                `json:"changedFiles"`
		Tests        []string                `json:"tests"`
		Blockers     []string                `json:"blockers"`
		Runtime      *runtime.RuntimeRequest `json:"runtime"`
		Provider     string                  `json:"provider"`
		Model        string                  `json:"model"`
		Effort       llm.ReasoningEffort     `json:"effort"`
		Request      string                  `json:"request"`
		Allow        bool                    `json:"allow"`
		Decision     string                  `json:"decision"`
		Reason       string                  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args, json.RejectUnknownMembers(true)); err != nil {
		detail := "malformed JSON; send an object matching the tool schema"
		var semantic *json.SemanticError
		if errors.As(err, &semantic) {
			field := string(semantic.JSONPointer)
			if field == "" {
				field = "(root)"
			}
			detail = "field " + field + ": use the type and fields in the tool schema"
			if errors.Is(err, json.ErrUnknownName) {
				detail = "unknown field " + field + "; remove it and use the tool schema"
			}
		}
		if t.action == "finish" {
			detail += "; Finish fields: status, summary, changedFiles, tests, blockers"
		}
		return tool.ErrorStatus("Invalid subagent arguments: "+detail, 0)
	}
	p := runtime.Plan{Version: 1, Action: t.action, ParentID: t.owner, Handle: args.Handle, Text: args.Text}
	if t.action == "parent" {
		p.Progress = args.Progress
	}
	if t.action == "approve" {
		p.Request, p.Allow = args.Request, args.Allow
	}
	if t.action == "review" {
		p.Review, p.Text = args.Decision, args.Reason
	}
	if t.action == "start" {
		config, ok := t.templates[args.Template]
		if !ok {
			return tool.ErrorStatus("Unknown child template", 0)
		}
		p.Template = args.Template
		request := args.Runtime
		if t.child {
			// Older nested calls used a removed runtime field. Keep their
			// role-based behavior when replaying those calls.
			request = nil
		}
		if args.Provider != "" || args.Model != "" || args.Effort != "" {
			if args.Runtime != nil {
				return tool.ErrorStatus("Use provider, model and effort fields or runtime, separately", 0)
			}
			request = &runtime.RuntimeRequest{Provider: args.Provider, Model: args.Model, Effort: args.Effort}
		}
		if t.resolve != nil {
			var e error
			config, e = t.resolve(args.Template, config, request)
			if e != nil {
				return tool.ErrorStatus(e.Error(), 0)
			}
			p.RuntimeBound = true
		} else if request != nil {
			return tool.ErrorStatus("Child runtime selection unsupported", 0)
		}
		p.Configuration = &config
		p.Text = args.Task
		p.Brief = args.Brief
	}
	if t.action == "finish" {
		eligibility, ok := ctx.(interface{ CanFinish() bool })
		if !ok || !eligibility.CanFinish() {
			message := "Finish requires no other unfinished calls or operations. Wait for their results, then call Finish alone"
			if waiting, ok := ctx.(interface{ FinishWaitingFor() []string }); ok {
				if targets := waiting.FinishWaitingFor(); len(targets) > 0 {
					message += ": " + strings.Join(targets, ", ")
				}
			}
			return tool.ErrorStatus(message, 0)
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
			if t.action == "parent" && op.Status == operation.StatusCompleted && len(s.Handle) > 0 {
				var receipt runtime.ParentMessageReceipt
				if json.Unmarshal(s.Handle, &receipt) == nil && receipt.Answer != "" {
					return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: receipt.Answer}}}, nil
				}
			}
			value := struct {
				Handle  operation.ID
				Status  operation.Status
				Message string
				Result  *runtime.Handle
				Preview string `json:",omitzero"`
			}{Handle: op.ID, Status: op.Status, Message: s.TerminalResult}
			if s.TerminalError != "" {
				value.Message = s.TerminalError
			}
			if t.action == "start" && len(s.Handle) > 0 {
				handle, err := runtime.DecodeHandle(op)
				if err != nil {
					return llm.ToolResult{}, err
				}
				if handle.Brief != nil {
					plan, err := runtime.DecodePlan(op)
					if err != nil {
						return llm.ToolResult{}, err
					}
					value.Preview = handle.Brief.Render(plan.Text)
					handle.Brief = nil // the preview carries the same facts
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
