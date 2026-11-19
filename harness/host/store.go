package host

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type serializedStore struct{ *Session }

var _ sessionstore.Store = (*serializedStore)(nil)

func (w *serializedStore) AddObserver(f sessionstore.Observer) sessionstore.ObserverID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AddObserver(f)
}
func (w *serializedStore) RemoveObserver(id sessionstore.ObserverID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.store.RemoveObserver(id)
}
func (w *serializedStore) Create(ctx context.Context, id session.ID) (sessionstore.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.Create(ctx, id)
}
func (w *serializedStore) ListSessions(ctx context.Context) ([]sessionstore.SessionInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.ListSessions(ctx)
}
func (w *serializedStore) Inspect(ctx context.Context, id session.ID) (sessionstore.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.Inspect(ctx, id)
}
func (w *serializedStore) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.Items(ctx, id, after, limit)
}
func (w *serializedStore) AppendInput(ctx context.Context, id session.ID, in inbox.Input) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendInput(ctx, id, in)
}
func (w *serializedStore) AppendInputRecorded(ctx context.Context, id session.ID, in inbox.Input) (sessionstore.Sequence, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendInputRecorded(ctx, id, in)
}
func (w *serializedStore) AppendTurn(ctx context.Context, id session.ID, t session.Turn) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendTurn(ctx, id, t)
}
func (w *serializedStore) AppendModelResponse(ctx context.Context, id session.ID, r sessionstore.ModelResponse) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendModelResponse(ctx, id, r)
}

// Conversation decisions use the same single Session writer and observer
// boundary as existing canonical records, never a second persistence path.
func (w *serializedStore) AppendHostRecord(ctx context.Context, id session.ID, r sessionstore.HostRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendHostRecord(ctx, id, r)
}
func (w *serializedStore) AppendHostRecordRecorded(ctx context.Context, id session.ID, r sessionstore.HostRecord) (sessionstore.Item, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendHostRecordRecorded(ctx, id, r)
}
func (w *serializedStore) AppendToolCallStatus(ctx context.Context, id session.ID, s sessionstore.ToolCallStatus) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.AppendToolCallStatus(ctx, id, s)
}
func (w *serializedStore) SaveOperation(ctx context.Context, id session.ID, op operation.Operation) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	locator, err := w.store.SaveOperationRecorded(ctx, id, op)
	if err != nil {
		return err
	}
	op = cloneSavedOperationCheckpoint(op, w.operations[op.ID])
	w.setOperation(op)
	w.broadcast(w.operationEvent(op, locator))
	return nil
}
func (w *serializedStore) Resume(ctx context.Context, id session.ID) (sessionstore.ResumeState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.Resume(ctx, id)
}
func (w *serializedStore) Fork(ctx context.Context, id, parent session.ID, turn session.TurnID) (sessionstore.Snapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.Fork(ctx, id, parent, turn)
}
