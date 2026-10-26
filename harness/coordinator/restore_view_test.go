package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type restoreProbeStore struct {
	*fakeStore
	opened, closed, reads      int
	active                     bool
	openErr, readErr, closeErr error
}

func (s *restoreProbeStore) OpenHistoryView(context.Context, session.ID) (sessionstore.HistoryView, error) {
	s.opened++
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.active = true
	return restoreProbeView{s}, nil
}

type restoreProbeView struct{ store *restoreProbeStore }

func (v restoreProbeView) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	v.store.reads++
	if v.store.readErr != nil {
		return sessionstore.Page{}, v.store.readErr
	}
	if err := context.Cause(ctx); err != nil {
		return sessionstore.Page{}, err
	}
	return v.store.fakeStore.Items(ctx, id, after, limit)
}
func (v restoreProbeView) Close() error {
	v.store.active = false
	v.store.closed++
	return v.store.closeErr
}

func TestRestoreHistoryViewClosesBeforeReconciliation(t *testing.T) {
	base := emptyFakeStore()
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.CancelOperation, Reason: "synthetic cancel", Parameters: inbox.CancelRequest{OperationID: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	base.items = []sessionstore.Item{{Sequence: 1, Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "cancel", Kind: inbox.InputControl, Payload: payload}}}
	base.resume.Operations = []operation.Operation{{ID: "pending", Status: operation.StatusReady}}
	store := &restoreProbeStore{fakeStore: base}
	base.onSaveOperation = func(op operation.Operation) {
		if store.active || store.closed != 1 {
			t.Fatal("reconciliation wrote inside read boundary")
		}
		if op.Status != operation.StatusCanceling {
			t.Fatal("cancel checkpoint changed")
		}
	}
	current := newTestCoordinator(base, newTestInbox(t), &fakeOperationManager{}, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	current.dependencies.Sessions = store
	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.opened != 1 || store.closed != 1 || store.reads != 1 || len(base.savedOperations) != 1 {
		t.Fatalf("boundary or reconciliation missing: %+v saves=%d", store, len(base.savedOperations))
	}
}
func TestRestoreHistoryViewFaultsCloseAndNeverReconcile(t *testing.T) {
	fault := errors.New("synthetic boundary fault")
	for _, kind := range []string{"open", "page", "close", "cancel", "malformed-item"} {
		t.Run(kind, func(t *testing.T) {
			base := emptyFakeStore()
			base.resume.Operations = []operation.Operation{{ID: "pending", Status: operation.StatusReady}}
			store := &restoreProbeStore{fakeStore: base}
			ctx := t.Context()
			switch kind {
			case "open":
				store.openErr = fault
			case "page":
				store.readErr = fault
			case "close":
				store.closeErr = fault
			case "cancel":
				c, cancel := context.WithCancelCause(ctx)
				cancel(fault)
				ctx = c
			case "malformed-item":
				base.items = []sessionstore.Item{{Sequence: 2, Kind: sessionstore.ItemInput}}
			}
			current := newTestCoordinator(base, newTestInbox(t), &fakeOperationManager{}, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
			current.dependencies.Sessions = store
			if err := current.restore(ctx); err == nil {
				t.Fatal("unsafe restore succeeded")
			}
			wantClose := 1
			if kind == "open" {
				wantClose = 0
			}
			if store.active || store.closed != wantClose || len(base.savedOperations) != 0 {
				t.Fatal("failed read boundary leaked or reconciled", store.closed, len(base.savedOperations))
			}
		})
	}
}
func TestRestoreHistoryViewLegacyStoreFallback(t *testing.T) {
	base := emptyFakeStore()
	current := newTestCoordinator(base, newTestInbox(t), &fakeOperationManager{}, contextbuilder.NewBuilder(), tool.NewRegistry(tool.StaticTranslators{}))
	if err := current.restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(base.itemRequests) != 1 {
		t.Fatal("legacy Store was not read")
	}
}
