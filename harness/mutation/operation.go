package mutation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

const PlanType operation.RemoteJobPlanType = "file_mutation"

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
	service   *Service
	namespace string
	mu        sync.Mutex
	jobs      map[operation.ID]context.CancelFunc
	updates   chan operation.Operation
}

func NewHandler(ctx context.Context, service *Service, sessionID string) (*Handler, error) {
	if service == nil || sessionID == "" {
		return nil, errors.New("mutation handler requires service and stable session ID")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Handler{ctx: ctx, cancel: cancel, service: service, namespace: sessionID, jobs: make(map[operation.ID]context.CancelFunc), updates: make(chan operation.Operation)}, nil
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
		var result Result
		if current.Status == operation.StatusCanceling {
			result = baseResult(request, Canceled)
		} else {
			result = h.service.Execute(ctx, h.namespace+"/"+string(current.ID), request)
		}
		current.Denial = result.Denial
		encoded, _ := json.Marshal(result)
		// Typed state is never truncated. Renderers may bound prose separately.
		state.Handle = encoded
		state.TerminalResult = string(result.Code)
		status := operation.StatusFailed
		if result.Code == Applied {
			status = operation.StatusCompleted
		}
		if result.Code == Canceled {
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

// Close cancels and drains all execution before the owning Session lock is released.
// Add and WaitGroup.Add share the closed mutex boundary, so Close cannot miss work.
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
