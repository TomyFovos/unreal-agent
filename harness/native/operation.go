package native

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"sync"
)

const PlanType operation.RemoteJobPlanType = "native_file"

func Spec(request Request) (operation.Spec, error) {
	if err := request.Validate(); err != nil {
		return operation.Spec{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return operation.Spec{}, err
	}
	return operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: PlanType, Version: Version, Data: encoded})
}

type Handler struct {
	ctx       context.Context
	executor  Executor
	namespace string
	mu        sync.Mutex
	jobs      map[operation.ID]context.CancelFunc
	updates   chan operation.Operation
}

func NewHandler(ctx context.Context, executor Executor, sessionID string) (*Handler, error) {
	if executor.Files == nil || sessionID == "" {
		return nil, errors.New("native handler requires file service and stable session ID")
	}
	return &Handler{ctx: ctx, executor: executor, namespace: sessionID, jobs: make(map[operation.ID]context.CancelFunc), updates: make(chan operation.Operation)}, nil
}
func (h *Handler) RemoteJobPlanType() operation.RemoteJobPlanType       { return PlanType }
func (h *Handler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return Version }
func (h *Handler) RemoteJobUpdates() <-chan operation.Operation         { return h.updates }
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
	if state.Plan.Type != PlanType || state.Plan.Version != Version {
		return operation.ErrUnsupported
	}
	var request Request
	if err = json.Unmarshal(state.Plan.Data, &request); err != nil {
		return err
	}
	h.mu.Lock()
	if _, ok := h.jobs[current.ID]; ok {
		h.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.jobs[current.ID] = cancel
	h.mu.Unlock()
	go func() {
		defer cancel()
		result := Result{Version: Version, Code: "canceled"}
		if current.Status != operation.StatusCanceling {
			result = h.executor.Execute(ctx, h.namespace+"/"+string(current.ID), request)
		}
		current.Denial = result.Denial
		state.Handle = EncodeResult(result)
		state.TerminalResult = result.Code
		status := operation.StatusFailed
		switch result.Code {
		case "ok", "applied", "binary", "no_match":
			status = operation.StatusCompleted
		case "canceled":
			status = operation.StatusCanceled
		}
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
