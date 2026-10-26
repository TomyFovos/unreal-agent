package coordinator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativetool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

type basisFaultPreparer struct {
	*fakeOperationManager
	handler *files.Handler
	ctx     context.Context
}

func (m *basisFaultPreparer) PrepareExecution(ctx context.Context, op operation.Operation) (operation.Operation, error) {
	return m.handler.PrepareExecution(ctx, op)
}
func (m *basisFaultPreparer) PrepareRecovery(ctx context.Context, op operation.Operation, effects []operation.UncertainEffect) (operation.Operation, error) {
	return m.handler.PrepareRecovery(ctx, op, effects)
}
func (m *basisFaultPreparer) PlanRecovery(ctx context.Context, op operation.Operation, effects []operation.UncertainEffect) (operation.Operation, error) {
	return m.handler.PlanRecovery(ctx, op, effects)
}

func newBasisFaultFixture(t *testing.T) (*toolAdmissionFixture, *conversationStore, string) {
	t.Helper()
	f, store := newConversationFixture(t)
	workspace := t.TempDir()
	policy, err := permission.New(permission.Config{Tools: []string{"read", "write", "edit"}, ReadRoots: []string{workspace}, WriteRoots: []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policy.Close() })
	ctx := permission.WithPolicy(t.Context(), policy)
	service, err := mutation.New(mutation.Config{Root: workspace, StateDir: filepath.Join(t.TempDir(), "mutation"), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := files.NewHandler(ctx, files.Executor{Files: service}, "synthetic-basis-fault")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handler.Close() })
	f.registry = tool.NewRegistry(nativetool.Configure(tool.StaticTranslators{}), tool.ReadName, tool.WriteName, tool.EditName)
	f.current.dependencies.Tools = f.registry
	f.current.dependencies.Operations = &basisFaultPreparer{fakeOperationManager: f.manager, handler: handler, ctx: ctx}
	return f, store, workspace
}

// This fake worker uses the real producer under the fixture's explicit current
// policy, then drives the owner's real durable preparation acknowledgement.
// Admission itself must contain no filesystem-derived basis.
func basisFaultPrepare(t *testing.T, f *toolAdmissionFixture, op operation.Operation) operation.Operation {
	t.Helper()
	m := f.current.dependencies.Operations.(*basisFaultPreparer)
	if !op.RequiresPreparation() || len(op.ExecutionBasis()) != 0 {
		t.Fatal("fixture admission performed preparation before the worker lease")
	}
	prepared, err := m.handler.PrepareExecution(m.ctx, op)
	if err != nil || len(prepared.ExecutionBasis()) == 0 {
		t.Fatal("authorized real producer did not capture the execution basis", err)
	}
	acks := 0
	var ackErr error
	err = f.current.commitOperationPreparation(m.ctx, operation.PreparedOperation{Operation: prepared, Acknowledge: func(err error) { acks++; ackErr = err }})
	if err != nil || acks != 1 || ackErr != nil {
		t.Fatal("authorized preparation was not durably acknowledged", err, ackErr, acks)
	}
	return operation.CloneExecutionMetadata(f.current.state.operations[op.ID])
}

func basisFaultResponse(turn session.TurnID) sessionstore.ModelResponse {
	return admissionResponse(turn, llm.ToolCall{CallID: "synthetic-basis-call", Name: "write", Arguments: `{"path":"a.txt","content":"synthetic","expected":{"exists":false}}`})
}

func basisFaultFreshOwner(t *testing.T, f *toolAdmissionFixture, s *conversationStore, ops []operation.Operation) *coordinator {
	t.Helper()
	s.resume.Operations = ops
	fresh := newTestCoordinator(s.fakeStore, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), f.registry)
	fresh.dependencies.Sessions = s
	fresh.dependencies.LLM = resultRecoveryAdapter{}
	fresh.dependencies.ContextBuilder.SetModel(llm.Model{ID: "synthetic-model"})
	fresh.dependencies.ContextBuilder.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("claude-native")
	if err := fresh.restore(t.Context()); err != nil {
		t.Fatal("fresh owner failed canonical recovery", err)
	}
	return fresh
}

func basisFaultNoExecution(t *testing.T, f *toolAdmissionFixture, workspace string) {
	t.Helper()
	if len(f.manager.adds) != 0 {
		t.Fatal("fault crossed the Operation dispatch boundary")
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("fault test performed a filesystem mutation")
	}
}

// Storage and execution worker are fake here; the execution-footprint producer
// is the real native Handler. Actual write/lost-result disk Host coverage lives
// in provider/TestClaudeQuarantineHostIndependentWriteEditBatchRestartAndFork.
func TestRecoveryBasisEpochAppendFaultPreservesTypedEvidence(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after_unknown"}[after], func(t *testing.T) {
			f, s, workspace := newBasisFaultFixture(t)
			statuses, err := f.current.handleModelResponse(t.Context(), basisFaultResponse(f.current.state.currentTurnID))
			if err != nil || len(statuses) != 1 || len(statuses[0].Operations) != 1 {
				t.Fatal("real producer did not yield a canonical ready operation", err)
			}
			a := basisFaultPrepare(t, f, statuses[0].Operations[0])
			if len(a.ExecutionBasis()) == 0 {
				t.Fatal("new ExecutionBasis was not durable before the effect boundary")
			}
			f.current.observeUncertainty(a, true)
			fault := errors.New("synthetic epoch append failure")
			if after {
				s.failAfter = fault
			} else {
				s.failBefore = fault
			}
			if err := f.current.appendConversationEpoch(t.Context(), "quarantine"); !errors.Is(err, fault) {
				t.Fatal("required append fault did not occur")
			}
			if !f.current.conversation.appendFailed || f.current.conversation.epoch.Number != 0 || !f.current.quarantined(a.ID) {
				t.Fatal("failed append activated an epoch or erased uncertainty")
			}
			before := s.epochs
			s.failBefore, s.failAfter = nil, nil
			if err := f.current.appendConversationEpoch(t.Context(), "quarantine"); !errors.Is(err, llm.ErrToolRecoveryRequired) || s.epochs != before {
				t.Fatal("old owner retried an unknown append outcome")
			}
			fresh := basisFaultFreshOwner(t, f, s, []operation.Operation{a})
			if !fresh.quarantined(a.ID) || fresh.conversation.epoch.Number != 1 || !bytes.Equal(fresh.conversation.effects[a.ID], a.ExecutionBasis()) || s.epochs != 1 {
				t.Fatal("fresh owner lost or duplicated canonical typed uncertainty")
			}
			if err := fresh.dispatchOperationsToManager(t.Context()); err != nil {
				t.Fatal(err)
			}
			if manager := fresh.dependencies.Operations.(*fakeOperationManager); len(manager.adds) != 0 {
				t.Fatal("fresh owner replayed the uncertain operation")
			}
			found := false
			for _, item := range s.items {
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Conversation != nil {
					for _, effect := range record.Conversation.UncertainEffects {
						if effect.ID == a.ID && bytes.Equal(effect.Basis, a.ExecutionBasis()) {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatal("canonical epoch omitted the original execution basis")
			}
			basisFaultNoExecution(t, f, workspace)
		})
	}
}

type basisCheckpointFaultStore struct {
	*conversationStore
	site      string
	after     bool
	fault     error
	committed map[operation.ID]operation.Operation
}

func (s *basisCheckpointFaultStore) AppendToolCallStatus(ctx context.Context, id session.ID, status sessionstore.ToolCallStatus) error {
	if s.site == "status" && !s.after {
		return s.fault
	}
	if err := s.conversationStore.AppendToolCallStatus(ctx, id, status); err != nil {
		return err
	}
	for _, op := range status.Operations {
		s.committed[op.ID] = operation.CloneExecutionMetadata(op)
	}
	if s.site == "status" {
		return s.fault
	}
	return nil
}
func (s *basisCheckpointFaultStore) SaveOperation(ctx context.Context, id session.ID, op operation.Operation) error {
	if s.site == "operation" && !s.after {
		return s.fault
	}
	if err := s.conversationStore.SaveOperation(ctx, id, op); err != nil {
		return err
	}
	s.committed[op.ID] = operation.CloneExecutionMetadata(op)
	if s.site == "operation" {
		return s.fault
	}
	return nil
}

func TestRecoveryBasisOperationCheckpointFaultNeverDispatches(t *testing.T) {
	for _, site := range []string{"status", "operation"} {
		for _, after := range []bool{false, true} {
			t.Run(site+map[bool]string{false: "_before", true: "_after_unknown"}[after], func(t *testing.T) {
				f, s, workspace := newBasisFaultFixture(t)
				fault := errors.New("synthetic checkpoint failure")
				wrapped := &basisCheckpointFaultStore{conversationStore: s, after: after, fault: fault, committed: map[operation.ID]operation.Operation{}}
				if site == "status" {
					wrapped.site = site
				}
				f.current.dependencies.Sessions = wrapped
				statuses, err := f.current.handleModelResponse(t.Context(), basisFaultResponse(f.current.state.currentTurnID))
				if site == "status" {
					if !errors.Is(err, fault) {
						t.Fatal("status append fault did not occur")
					}
				} else {
					if err != nil || len(statuses) != 1 || len(statuses[0].Operations) != 1 {
						t.Fatal("initial canonical operation missing", err)
					}
					a := statuses[0].Operations[0]
					if !a.RequiresPreparation() || len(a.ExecutionBasis()) != 0 {
						t.Fatal("admission crossed the authorized preparation boundary")
					}
					m := f.current.dependencies.Operations.(*basisFaultPreparer)
					prepared, prepareErr := m.handler.PrepareExecution(m.ctx, a)
					if prepareErr != nil || len(prepared.ExecutionBasis()) == 0 {
						t.Fatal("real native metadata producer was not used", prepareErr)
					}
					wrapped.site = site
					// Fault the actual durable metadata checkpoint. A channel
					// receipt alone must never be interpreted as its success.
					acks := 0
					var ackErr error
					if err := f.current.commitOperationPreparation(m.ctx, operation.PreparedOperation{Operation: prepared, Acknowledge: func(err error) { acks++; ackErr = err }}); !errors.Is(err, fault) || acks != 1 || !errors.Is(ackErr, fault) {
						t.Fatal("operation save fault did not fail the durable ACK", err, ackErr, acks)
					}
				}
				basisFaultNoExecution(t, f, workspace)
				wrapped.site = ""
				var persisted []operation.Operation
				for _, op := range wrapped.committed {
					// The first canonical proposal is inert and explicitly
					// requires preparation. Only a successful/unknown-after
					// authorized metadata append may contain a finite basis.
					wantBasis := site == "operation" && after
					if !op.RequiresPreparation() || (len(op.ExecutionBasis()) != 0) != wantBasis {
						t.Fatal("committed metadata does not match the faulted preparation phase")
					}
					persisted = append(persisted, op)
				}
				if site == "status" && !after && len(persisted) != 0 {
					t.Fatal("pre-append fault committed an operation")
				}
				if (site != "status" || after) && len(persisted) != 1 {
					t.Fatal("post-append committed operation not retained")
				}
				fresh := basisFaultFreshOwner(t, f, s, persisted)
				for _, op := range persisted {
					if !fresh.quarantined(op.ID) || !bytes.Equal(fresh.conversation.effects[op.ID], op.ExecutionBasis()) {
						t.Fatal("unfinished checkpoint did not recover with its original metadata")
					}
				}
				if err := fresh.dispatchOperationsToManager(t.Context()); err != nil {
					t.Fatal(err)
				}
				if manager := fresh.dependencies.Operations.(*fakeOperationManager); len(manager.adds) != 0 {
					t.Fatal("restore executed a faulted operation")
				}
				basisFaultNoExecution(t, f, workspace)
			})
		}
	}
}

type basisDelayedHandler struct {
	adds    chan operation.Operation
	updates chan operation.Operation
	cancels chan operation.ID
}

func (*basisDelayedHandler) RemoteJobPlanType() operation.RemoteJobPlanType { return files.PlanType }
func (*basisDelayedHandler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion {
	return files.Version
}
func (h *basisDelayedHandler) AddRemoteJob(op operation.Operation) error    { h.adds <- op; return nil }
func (h *basisDelayedHandler) RemoteJobUpdates() <-chan operation.Operation { return h.updates }
func (h *basisDelayedHandler) CancelRemoteJob(id operation.ID, _ string) error {
	h.cancels <- id
	return nil
}

func TestRecoveryBasisDelayedWorkerCannotReplaceExecutionEvidence(t *testing.T) {
	for _, mode := range []string{"own_basis", "blocked_basis", "dependencies"} {
		t.Run(mode, func(t *testing.T) {
			f, _, workspace := newBasisFaultFixture(t)
			statuses, err := f.current.handleModelResponse(t.Context(), basisFaultResponse(f.current.state.currentTurnID))
			if err != nil || len(statuses) != 1 || len(statuses[0].Operations) != 1 {
				t.Fatal("real execution basis producer failed", err)
			}
			original := basisFaultPrepare(t, f, statuses[0].Operations[0])
			original = operation.WithDependencies(original, []operation.ID{"synthetic-prior"})
			original = operation.WithRecoveryBasis(original, []operation.UncertainEffect{{ID: "synthetic-blocked", Basis: original.ExecutionBasis().Clone()}})
			h := &basisDelayedHandler{adds: make(chan operation.Operation, 1), updates: make(chan operation.Operation, 1), cancels: make(chan operation.ID, 1)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			manager := operation.NewLocalOperationManagerWithPolicy(ctx, permission.Unrestricted(), h)
			defer func() { cancel(); <-manager.Done() }()
			if err := manager.Add(original); err != nil {
				t.Fatal(err)
			}
			var late operation.Operation
			select {
			case late = <-h.adds:
			case <-time.After(time.Second):
				t.Fatal("negative worker did not receive the original operation")
			}
			late.Status = operation.StatusCompleted
			switch mode {
			case "own_basis":
				late.Execution.ExecutionBasis = nil
			case "blocked_basis":
				late.RecoveryBasis()[0].Basis = nil
			case "dependencies":
				late.Execution.Dependencies = []operation.ID{"synthetic-other"}
			}
			if operation.SameExecutionMetadata(original, late) {
				t.Fatal("negative fixture did not change protected evidence")
			}
			h.updates <- late
			var rejected operation.Operation
			select {
			case rejected = <-manager.Updates():
			case <-time.After(time.Second):
				t.Fatal("invalid late evidence was not classified")
			}
			if rejected.ID != original.ID || rejected.Status != operation.StatusFailed || !operation.SameExecutionMetadata(original, rejected) {
				t.Fatal("late worker overwrote canonical execution evidence")
			}
			state, err := operation.DecodeRemoteJobState(rejected)
			if err != nil || state.TerminalError == "" {
				t.Fatal("rejected worker has no typed terminal failure")
			}
			select {
			case id := <-h.cancels:
				if id != original.ID {
					t.Fatal("wrong worker canceled")
				}
			case <-time.After(time.Second):
				t.Fatal("invalid worker was not canceled")
			}
			basisFaultNoExecution(t, f, workspace)
		})
	}
}
