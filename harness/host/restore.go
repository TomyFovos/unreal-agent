package host

import (
	"context"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type restoreStartupReader interface {
	Items(context.Context, session.ID, sessionstore.Sequence, int) (sessionstore.Page, error)
	Operations(context.Context, session.ID) ([]operation.Operation, error)
}

type serializedHistoryView struct {
	session *Session
	view    sessionstore.HistoryView
	once    sync.Once
	err     error
}

func (v *serializedHistoryView) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	return v.view.Items(ctx, id, after, limit)
}
func (v *serializedHistoryView) Close() error {
	v.once.Do(func() { v.err = v.view.Close(); v.session.mu.Unlock() })
	return v.err
}

// OpenHistoryView runs only during Coordinator restoration. The same Session
// mutex that serializes every canonical write is held until Close; it is released
// before permission reconciliation, recovery appends or Operation dispatch.
func (w *serializedStore) OpenHistoryView(ctx context.Context, id session.ID) (sessionstore.HistoryView, error) {
	w.mu.Lock()
	if id != w.ID {
		w.mu.Unlock()
		return nil, ErrConflict
	}
	view, err := w.store.OpenRestoreView(ctx, id)
	if err != nil {
		w.mu.Unlock()
		return nil, err
	}
	return &serializedHistoryView{session: w.Session, view: view}, nil
}
