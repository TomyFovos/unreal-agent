package contexttools

import (
	"context"
	"encoding/json/v2"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

type Handler struct {
	owner   *host.Session
	ctx     context.Context
	cancel  context.CancelFunc
	updates chan operation.Operation
	mu      sync.Mutex
	jobs    map[operation.ID]context.CancelFunc
	workers sync.WaitGroup
	close   sync.Once
}

func NewHandler(ctx context.Context, owner *host.Session) *Handler {
	ctx, cancel := context.WithCancel(ctx)
	return &Handler{owner: owner, ctx: ctx, cancel: cancel, updates: make(chan operation.Operation), jobs: make(map[operation.ID]context.CancelFunc)}
}

func (h *Handler) RemoteJobPlanType() operation.RemoteJobPlanType       { return contextstate.OperationType }
func (h *Handler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return 1 }
func (h *Handler) RemoteJobUpdates() <-chan operation.Operation         { return h.updates }

func (h *Handler) AddRemoteJob(op operation.Operation) error {
	return h.AddRemoteJobWithContext(h.ctx, op)
}

func (h *Handler) AddRemoteJobWithContext(ctx context.Context, op operation.Operation) error {
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return err
	}
	var cmd contextstate.Command
	if state.Plan.Type != h.RemoteJobPlanType() || state.Plan.Version != h.RemoteJobPlanVersion() ||
		json.Unmarshal(state.Plan.Data, &cmd, json.RejectUnknownMembers(true)) != nil || string(cmd.Action) != op.ToolName || !contextstate.IsTool(op.ToolName) {
		return operation.ErrUnsupported
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.ctx.Err(); err != nil {
		return err
	}
	if _, exists := h.jobs[op.ID]; exists {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	h.jobs[op.ID] = cancel
	h.workers.Go(func() {
		defer stop()
		defer cancel()
		defer func() {
			h.mu.Lock()
			delete(h.jobs, op.ID)
			h.mu.Unlock()
		}()
		status := operation.StatusCanceled
		if op.Status != operation.StatusCanceling && ctx.Err() == nil {
			out, err := h.owner.ContextCommand(ctx, h.owner.Generation, string(op.ID), cmd)
			if err == nil {
				state.Handle, err = out.JSON(cmd.Action)
			}
			status = operation.StatusCompleted
			if err != nil {
				status = operation.StatusFailed
				state.Handle, _ = json.Marshal(secretguard.RedactSensitive(err.Error()))
			}
		}
		step, err := operation.UpdateRemoteJob(op, state, status)
		if err == nil {
			select {
			case h.updates <- *step.Operation:
			case <-h.ctx.Done():
			}
		}
	})
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

func (h *Handler) Close() error {
	h.close.Do(func() {
		h.mu.Lock()
		h.cancel()
		h.mu.Unlock()
		h.workers.Wait()
		close(h.updates)
	})
	return nil
}
