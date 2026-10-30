package host

import (
	"context"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type serializedHistoryView struct {
	session  *Session
	view     sessionstore.HistoryView
	once     sync.Once
	err      error
	restored func()
}

func (v *serializedHistoryView) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	return v.view.Items(ctx, id, after, limit)
}
func (v *serializedHistoryView) Close() error {
	v.once.Do(func() {
		v.err = v.view.Close()
		v.session.mu.Unlock()
		if v.restored != nil {
			v.restored()
		}
	})
	return v.err
}

// OpenHistoryView runs only during Coordinator restoration. The same Session
// mutex that serializes every canonical write is held until Close; it is released
// before permission reconciliation, recovery appends or Operation dispatch.
func (w *serializedStore) OpenHistoryView(ctx context.Context, id session.ID) (sessionstore.HistoryView, error) {
	defer startupPhase(ctx, id, "host.coordinator_read")()
	w.mu.Lock()
	if id != w.ID {
		w.mu.Unlock()
		return nil, ErrConflict
	}
	var view sessionstore.HistoryView
	var err error
	if w.items != nil {
		view, err = w.store.OpenHistoryItems(ctx, id, w.items)
	} else {
		view, err = w.store.OpenRestoreView(ctx, id)
	}
	if err != nil {
		w.mu.Unlock()
		return nil, err
	}
	return &serializedHistoryView{session: w.Session, view: view, restored: startupPhase(ctx, id, "host.coordinator_restore")}, nil
}
