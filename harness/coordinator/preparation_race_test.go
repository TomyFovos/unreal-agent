package coordinator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// A successful save may finish after the lease's error ACK has already won.
// This wrapper blocks the real owner's preparation checkpoint, not admission or
// a terminal save, so the exact interleaving is controlled without sleeps.
type preparationSaveBarrier struct {
	*conversationStore
	entered chan operation.Operation
	release chan struct{}
	once    sync.Once
}

func (s *preparationSaveBarrier) SaveOperation(ctx context.Context, id session.ID, op operation.Operation) error {
	if len(op.ExecutionBasis()) != 0 {
		s.once.Do(func() { s.entered <- operation.CloneExecutionMetadata(op); <-s.release })
	}
	return s.conversationStore.SaveOperation(ctx, id, op)
}

func TestPreparationSaveRaceRetainsCommittedBasisAfterLeaseRevocation(t *testing.T) {
	f, store, workspace := newBasisFaultFixture(t)
	preparer := f.current.dependencies.Operations.(*basisFaultPreparer)
	maximum := permission.FromContext(preparer.ctx)
	controller := permission.NewController(t.Context(), maximum, maximum, time.Minute)
	defer controller.Close()
	key, releaseControl := controller.AcquireControl()
	defer releaseControl()
	revision := controller.Snapshot().Revision
	challenge, err := controller.PrepareFull(key, revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.SetMode(key, revision, permission.FullAccess, challenge); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(permission.WithController(preparer.ctx, controller))
	manager := operation.NewLocalOperationManager(runCtx, preparer.handler)
	defer func() { cancel(); <-manager.Done() }()
	f.current.dependencies.Operations = manager
	statuses, err := f.current.handleModelResponse(preparer.ctx, basisFaultResponse(f.current.state.currentTurnID))
	if err != nil || len(statuses) != 1 || len(statuses[0].Operations) != 1 {
		t.Fatal("canonical proposal unavailable", err)
	}
	initial := statuses[0].Operations[0]
	if !initial.RequiresPreparation() || len(initial.ExecutionBasis()) != 0 {
		t.Fatal("proposal already contains worker evidence")
	}
	barrier := &preparationSaveBarrier{conversationStore: store, entered: make(chan operation.Operation, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer unblock()
	f.current.dependencies.Sessions = barrier
	if err = manager.Add(initial); err != nil {
		t.Fatal(err)
	}
	var prepared operation.PreparedOperation
	waiting := true
	for waiting {
		select {
		case update := <-manager.Updates():
			if err = f.current.handleOperationUpdate(preparer.ctx, update); err != nil {
				t.Fatal(err)
			}
		case prepared = <-manager.Preparations():
			waiting = false
		case <-time.After(5 * time.Second):
			t.Fatal("authorized worker did not prepare")
		}
	}
	if prepared.Operation.ID != initial.ID || len(prepared.Operation.ExecutionBasis()) == 0 {
		t.Fatal("real native preparation missing")
	}
	committed := make(chan error, 1)
	go func() { committed <- f.current.commitOperationPreparation(preparer.ctx, prepared) }()
	select {
	case offered := <-barrier.entered:
		if !bytes.Equal(offered.ExecutionBasis(), prepared.Operation.ExecutionBasis()) {
			t.Fatal("wrong metadata checkpoint blocked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not reach blocked preparation save")
	}
	// Revoke the actual Controller permit without canceling the Manager or owner
	// contexts. Its AfterFunc error ACK must settle the worker while Save blocks.
	controller.Close()
	var stale operation.Operation
	select {
	case stale = <-manager.Updates():
	case <-time.After(5 * time.Second):
		t.Fatal("lease error ACK did not settle canceled worker while owner save blocked")
	}
	if stale.ID != initial.ID || stale.Status != operation.StatusCanceled || len(stale.ExecutionBasis()) != 0 {
		t.Fatal("fixture did not produce the real pre-commit stale terminal")
	}
	if _, err = os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("revoked worker executed before durable success ACK")
	}
	unblock()
	select {
	case err = <-committed:
		if err != nil {
			t.Fatal("released save did not commit successfully", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner success ACK deadlocked against prior lease error ACK")
	}
	canonical := f.current.state.operations[initial.ID]
	if !operation.SameExecutionMetadata(canonical, prepared.Operation) {
		t.Fatal("successful save did not install prepared metadata")
	}
	if err = f.current.handleOperationUpdate(preparer.ctx, stale); err != nil {
		t.Fatal(err)
	}
	final := f.current.state.operations[initial.ID]
	if final.Status != operation.StatusCanceled || !operation.SameExecutionMetadata(final, prepared.Operation) {
		t.Fatal("late canceled worker erased already committed preparation evidence")
	}
	if len(store.savedOperations) == 0 {
		t.Fatal("no canonical Operation checkpoints")
	}
	persisted := store.savedOperations[len(store.savedOperations)-1]
	if persisted.ID != initial.ID || persisted.Status != operation.StatusCanceled || !operation.SameExecutionMetadata(persisted, prepared.Operation) {
		t.Fatal("canonical terminal save lost prepared basis")
	}
	if _, err = os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("late ACK or terminal handling executed the revoked proposal")
	}
}
