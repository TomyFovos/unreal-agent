package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const ToolName = "LSP"
const PlanType operation.RemoteJobPlanType = "lsp"
const PlanVersion operation.RemoteJobPlanVersion = 1

func Definition() tool.Definition {
	return tool.Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: ToolName, Description: "Query a managed language server for definitions, references, diagnostics, symbols, hover, safe rename and edit-only code actions. Positions default to UTF-16. Results report generation/encoding; restart/shutdown require generation. ExecuteCommand is unsupported.", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []string{"definition", "references", "diagnostics", "document_symbols", "workspace_symbols", "hover", "rename", "code_actions", "apply_code_action", "restart", "shutdown"}},
			"language": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
			"position": map[string]any{"type": "object", "properties": map[string]any{"line": map[string]any{"type": "integer", "minimum": 0}, "character": map[string]any{"type": "integer", "minimum": 0}}, "required": []string{"line", "character"}},
			"range":    map[string]any{"type": "object"}, "encoding": map[string]any{"type": "string", "enum": []string{"utf-8", "utf-16", "utf-32"}},
			"query": map[string]any{"type": "string"}, "new_name": map[string]any{"type": "string"}, "generation": map[string]any{"type": "string"}, "action_id": map[string]any{"type": "string"},
		}, "required": []string{"action", "language"},
	}}}
}

type Translator struct{}

func (Translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var request Request
	if json.Unmarshal([]byte(call.Arguments), &request) != nil {
		return tool.CallStatus{Error: "invalid LSP arguments"}
	}
	if err := request.Validate(); err != nil {
		return tool.CallStatus{Error: err.Error()}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return tool.CallStatus{Error: "invalid LSP request"}
	}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: PlanType, Version: PlanVersion, Data: data})
	if err != nil {
		return tool.CallStatus{Error: err.Error()}
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}
func (Translator) TranslateResult(callID string, status tool.CallStatus, operations []operation.Operation) (llm.ToolResult, error) {
	output := status.Error
	if output == "" {
		if len(operations) != 1 {
			return llm.ToolResult{}, errors.New("LSP result requires one operation")
		}
		current := operations[0]
		if current.Denial != nil {
			encoded, _ := json.Marshal(Result{Version: 1, Code: string(current.Denial.Code), Denial: current.Denial})
			output = string(encoded)
		} else {
			state, err := operation.DecodeRemoteJobState(current)
			if err != nil {
				return llm.ToolResult{}, err
			}
			if len(state.Handle) > 0 {
				output = string(state.Handle)
			} else if state.TerminalError != "" {
				output = state.TerminalError
			} else {
				output = `{"version":1,"code":"running"}`
			}
		}
	}
	return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: output}}}, nil
}

type Handler struct {
	ctx       context.Context
	cancel    context.CancelFunc
	manager   *Manager
	namespace string
	mu        sync.Mutex
	jobs      map[operation.ID]context.CancelFunc
	workers   sync.WaitGroup
	updates   chan operation.Operation
}

func NewHandler(ctx context.Context, manager *Manager, sessionID string) (*Handler, error) {
	if manager == nil || sessionID == "" {
		return nil, errors.New("LSP handler requires resource owner and session identity")
	}
	life, cancel := context.WithCancel(ctx)
	return &Handler{ctx: life, cancel: cancel, manager: manager, namespace: sessionID, jobs: make(map[operation.ID]context.CancelFunc), updates: make(chan operation.Operation)}, nil
}
func (h *Handler) RemoteJobPlanType() operation.RemoteJobPlanType       { return PlanType }
func (h *Handler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return PlanVersion }
func (h *Handler) RemoteJobUpdates() <-chan operation.Operation         { return h.updates }
func (h *Handler) Close() error {
	h.mu.Lock()
	h.cancel()
	h.mu.Unlock()
	h.workers.Wait()
	return nil
}
func (h *Handler) CancelRemoteJob(id operation.ID, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cancel := h.jobs[id]; cancel != nil {
		cancel()
	}
	return nil
}
func (h *Handler) AddRemoteJob(current operation.Operation) error {
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		return err
	}
	if state.Plan.Type != PlanType || state.Plan.Version != PlanVersion {
		return operation.ErrUnsupported
	}
	var request Request
	if err = json.Unmarshal(state.Plan.Data, &request); err != nil {
		return err
	}
	if err = request.Validate(); err != nil {
		return err
	}
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		return h.ctx.Err()
	}
	if _, exists := h.jobs[current.ID]; exists {
		h.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.jobs[current.ID] = cancel
	h.workers.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.workers.Done()
		defer cancel()
		result := Result{Version: 1, Action: request.Action}
		if current.Status == operation.StatusCanceling {
			result.Code = "canceled"
		} else if current.Status != operation.StatusReady && (request.Action == "rename" || request.Action == "apply_code_action") {
			result.Code = "indeterminate"
			result.Message = "edit operation was interrupted; it is not automatically repeated"
		} else {
			if err := operation.Authorize(permission.FromContext(ctx), current); err != nil {
				result = resultError(result, err)
			} else {
				if current.Status == operation.StatusReady {
					step, err := operation.UpdateRemoteJob(current, state, operation.StatusAwaiting)
					if err != nil {
						return
					}
					select {
					case h.updates <- *step.Operation:
						current = *step.Operation
					case <-h.ctx.Done():
						return
					}
				}
				result = h.manager.Execute(ctx, h.namespace+"/"+string(current.ID), request)
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			result = Result{Version: 1, Code: "failed", Message: "could not encode LSP result"}
			encoded, _ = json.Marshal(result)
		}
		state.Handle = encoded
		state.TerminalResult = result.Code
		status := operation.StatusFailed
		if result.Code == "ok" || result.Code == "applied" {
			status = operation.StatusCompleted
		}
		if result.Code == "canceled" {
			status = operation.StatusCanceled
		}
		current.Denial = result.Denial
		step, err := operation.UpdateRemoteJob(current, state, status)
		if err != nil {
			return
		}
		select {
		case h.updates <- *step.Operation:
		case <-h.ctx.Done():
		}
	}()
	return nil
}
