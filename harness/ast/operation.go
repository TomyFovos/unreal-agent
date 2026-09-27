package ast

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"sync"
)

const PlanType operation.RemoteJobPlanType = "structural_code"

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
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	closed    bool
	closeOnce sync.Once
	executor  Executor
	namespace string
	mu        sync.Mutex
	jobs      map[operation.ID]context.CancelFunc
	updates   chan operation.Operation
}

func NewHandler(ctx context.Context, executor Executor, sessionID string) (*Handler, error) {
	if executor.Files == nil || sessionID == "" {
		return nil, errors.New("AST handler requires file service and stable session ID")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Handler{ctx: ctx, cancel: cancel, executor: executor, namespace: sessionID, jobs: make(map[operation.ID]context.CancelFunc), updates: make(chan operation.Operation)}, nil
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
	if h.closed {
		h.mu.Unlock()
		return errors.New("handler is closed")
	}
	if _, ok := h.jobs[current.ID]; ok {
		h.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.jobs[current.ID] = cancel
	h.workers.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.workers.Done()
		defer func() { h.mu.Lock(); delete(h.jobs, current.ID); h.mu.Unlock() }()
		defer cancel()
		result := Result{Version: Version, Code: "canceled"}
		if current.Status != operation.StatusCanceling {
			result = h.executor.Execute(ctx, h.namespace+"/"+string(current.ID), request)
		}
		current.Denial = result.Denial
		state.Handle, _ = json.Marshal(result)
		state.TerminalResult = result.Code
		status := operation.StatusFailed
		switch result.Code {
		case "ok", "no_match", "applied":
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

// Close cancels and drains all AST work before session ownership is released.
func (h *Handler) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.cancel()
		h.mu.Unlock()
		h.workers.Wait()
		close(h.updates)
	})
	return nil
}
