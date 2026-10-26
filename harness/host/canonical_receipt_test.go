package host

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativeTools "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

type receiptInspectorManager struct {
	*operation.LocalOperationManager
	inspect func(context.Context, operation.Operation) (operation.CanonicalReceiptInspection, error)
	ack     func(context.Context, operation.Operation) error
	adds    atomic.Int64
}

func (m *receiptInspectorManager) Add(op operation.Operation) error {
	m.adds.Add(1)
	return m.LocalOperationManager.Add(op)
}
func (m *receiptInspectorManager) InspectCanonicalReceipt(ctx context.Context, op operation.Operation) (operation.CanonicalReceiptInspection, error) {
	return m.inspect(ctx, op)
}
func (m *receiptInspectorManager) AcknowledgeCanonicalOperation(ctx context.Context, op operation.Operation) error {
	if m.ack != nil {
		return m.ack(ctx, op)
	}
	return nil
}

func receiptFixture(t *testing.T) (operation.Operation, operation.Operation) {
	t.Helper()
	spec, err := files.Spec(files.Request{Version: files.Version, Action: "read", Path: "fixture.txt"})
	if err != nil {
		t.Fatal(err)
	}
	prior := operation.Operation{ID: "accepted-read", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusFailed, ToolName: "read"}
	prior = operation.WithExecutionBasis(prior, []byte(`{"synthetic_receipt_basis":1}`))
	state, err := operation.DecodeRemoteJobState(prior)
	if err != nil {
		t.Fatal(err)
	}
	state.Handle = files.EncodeResult(files.Result{Version: files.Version, Path: "fixture.txt", Code: "indeterminate"})
	prior.State, _ = json.Marshal(state)
	terminal := detachedReceiptOperation(prior)
	terminal.Status = operation.StatusCompleted
	state.Handle = files.EncodeResult(files.Result{Version: files.Version, Path: "fixture.txt", Code: "ok", Content: "synthetic observed result"})
	terminal.State, _ = json.Marshal(state)
	return prior, terminal
}

func seededReceiptStore(t *testing.T, prior operation.Operation) (string, *localfile.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(t.Context(), "receipt-session"); err != nil {
		t.Fatal(err)
	}
	if err = store.AppendTurn(t.Context(), "receipt-session", session.Turn{ID: "receipt-turn", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{CallID: "receipt-call", Name: "read", Arguments: `{"path":"fixture.txt"}`}
	if err = store.AppendModelResponse(t.Context(), "receipt-session", sessionstore.ModelResponse{TurnID: "receipt-turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}); err != nil {
		t.Fatal(err)
	}
	if err = store.AppendToolCallStatus(t.Context(), "receipt-session", sessionstore.ToolCallStatus{TurnID: "receipt-turn", CallID: call.CallID, Status: tool.CallStatus{WaitingFor: []operation.ID{prior.ID}}, Operations: []operation.Operation{prior}}); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveOperation(t.Context(), "receipt-session", prior); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

func receiptHost(t *testing.T, dir string, configure func(*receiptInspectorManager)) (*Host, **receiptInspectorManager) {
	t.Helper()
	var current *receiptInspectorManager
	h, err := New(t.Context(), Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
		current = &receiptInspectorManager{LocalOperationManager: operation.NewLocalOperationManager(ctx)}
		configure(current)
		return Runtime{Builder: contextbuilder.NewBuilder(), LLM: modelFunc(echo), Tools: tool.NewRegistry(nativeTools.Configure(tool.StaticTranslators{})), Operations: current}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, &current
}

func TestHostP2CanonicalReceiptStartupRestoresDurableTerminal(t *testing.T) {
	prior, terminal := receiptFixture(t)
	dir, store := seededReceiptStore(t, prior)
	var inspections, acks int
	h, manager := receiptHost(t, dir, func(m *receiptInspectorManager) {
		m.inspect = func(_ context.Context, accepted operation.Operation) (operation.CanonicalReceiptInspection, error) {
			inspections++
			if !canonicalReceiptEqual(prior, accepted) {
				t.Fatal("inspection did not use accepted canonical operation")
			}
			return operation.CanonicalReceiptInspection{State: operation.CanonicalReceiptTerminal, Operation: terminal}, nil
		}
		m.ack = func(ctx context.Context, got operation.Operation) error {
			acks++
			values, err := store.Operations(ctx, "receipt-session")
			if err != nil || len(values) != 1 || !canonicalReceiptEqual(values[0], terminal) || !canonicalReceiptEqual(got, terminal) {
				t.Fatal("ACK preceded complete canonical terminal save")
			}
			return nil
		}
	})
	s, err := h.Resume(t.Context(), Options{ID: "receipt-session"})
	if err != nil {
		t.Fatal(err)
	}
	values, err := store.Operations(t.Context(), "receipt-session")
	if err != nil || len(values) != 1 || !canonicalReceiptEqual(values[0], terminal) || inspections != 1 || acks != 1 || (*manager).adds.Load() != 0 {
		t.Fatalf("canonical inspection recovery failed: inspections=%d ACKs=%d Adds=%d err=%v", inspections, acks, (*manager).adds.Load(), err)
	}
	view, err := s.Inspect(0, 4096)
	if err != nil || len(view.Operations) != 1 || !canonicalReceiptEqual(view.Operations[0], terminal) {
		t.Fatalf("owner view kept stale UNKNOWN: %v", err)
	}
	stop(t, s)
}

func TestHostP2CanonicalReceiptMismatchAndUnknownHold(t *testing.T) {
	for _, variant := range []string{"unknown", "error", "basis", "plan", "tool", "id", "nonterminal", "no-handle"} {
		t.Run(variant, func(t *testing.T) {
			prior, terminal := receiptFixture(t)
			dir, store := seededReceiptStore(t, prior)
			acks := 0
			h, manager := receiptHost(t, dir, func(m *receiptInspectorManager) {
				m.inspect = func(context.Context, operation.Operation) (operation.CanonicalReceiptInspection, error) {
					state := operation.CanonicalReceiptTerminal
					switch variant {
					case "unknown":
						state = operation.CanonicalReceiptUnknown
					case "error":
						return operation.CanonicalReceiptInspection{}, errors.New("synthetic inspection failure")
					case "basis":
						terminal = operation.WithExecutionBasis(terminal, []byte(`{"different":true}`))
					case "plan":
						remote, _ := operation.DecodeRemoteJobState(terminal)
						remote.Plan.Data = []byte(`{"version":1,"action":"read","path":"other.txt"}`)
						terminal.State, _ = json.Marshal(remote)
					case "tool":
						terminal.ToolName = "write"
					case "id":
						terminal.ID = "other-operation"
					case "nonterminal":
						terminal.Status = operation.StatusAwaiting
					case "no-handle":
						remote, _ := operation.DecodeRemoteJobState(terminal)
						remote.Handle = nil
						terminal.State, _ = json.Marshal(remote)
					}
					return operation.CanonicalReceiptInspection{State: state, Operation: terminal}, nil
				}
				m.ack = func(context.Context, operation.Operation) error { acks++; return nil }
			})
			s, err := h.Resume(t.Context(), Options{ID: "receipt-session"})
			if err != nil {
				t.Fatal(err)
			}
			values, err := store.Operations(t.Context(), "receipt-session")
			if err != nil || len(values) != 1 || !canonicalReceiptEqual(values[0], prior) || acks != 0 || (*manager).adds.Load() != 0 {
				t.Fatalf("unknown/mismatch changed canonical or executed: ACKs=%d Adds=%d err=%v", acks, (*manager).adds.Load(), err)
			}
			stop(t, s)
		})
	}
}

func TestHostP2CanonicalReceiptACKFailureLeavesDurableTerminal(t *testing.T) {
	prior, terminal := receiptFixture(t)
	dir, store := seededReceiptStore(t, prior)
	want := errors.New("synthetic ACK lost")
	h, _ := receiptHost(t, dir, func(m *receiptInspectorManager) {
		m.inspect = func(context.Context, operation.Operation) (operation.CanonicalReceiptInspection, error) {
			return operation.CanonicalReceiptInspection{State: operation.CanonicalReceiptTerminal, Operation: terminal}, nil
		}
		m.ack = func(context.Context, operation.Operation) error { return want }
	})
	if _, err := h.Resume(t.Context(), Options{ID: "receipt-session"}); !errors.Is(err, want) {
		t.Fatalf("ACK failure not surfaced: %v", err)
	}
	values, err := store.Operations(t.Context(), "receipt-session")
	if err != nil || len(values) != 1 || !canonicalReceiptEqual(values[0], terminal) {
		t.Fatalf("durable terminal lost on ACK failure: %v", err)
	}
}

func TestHostP2CanonicalReceiptSaveFailureDoesNotACK(t *testing.T) {
	prior, terminal := receiptFixture(t)
	dir, _ := seededReceiptStore(t, prior)
	acks := 0
	h, _ := receiptHost(t, dir, func(m *receiptInspectorManager) {
		m.inspect = func(context.Context, operation.Operation) (operation.CanonicalReceiptInspection, error) {
			if err := os.Rename(filepath.Join(dir, "receipt-session.session.jsonl"), filepath.Join(dir, "preserved-input.jsonl")); err != nil {
				t.Fatal(err)
			}
			return operation.CanonicalReceiptInspection{State: operation.CanonicalReceiptTerminal, Operation: terminal}, nil
		}
		m.ack = func(context.Context, operation.Operation) error { acks++; return nil }
	})
	if _, err := h.Resume(t.Context(), Options{ID: "receipt-session"}); err == nil || acks != 0 {
		t.Fatalf("failed save ACKed: err=%v ACKs=%d", err, acks)
	}
}

func TestHostP2CanonicalReceiptOptOutAndInspectionBound(t *testing.T) {
	prior, _ := receiptFixture(t)
	// The ordinary manager is deliberately not an inspector; this returns
	// before touching nil Store, allocating indexes or iterating accepted ops.
	plain := &Session{runtime: Runtime{Operations: operation.NewLocalOperationManager(t.Context())}}
	if _, err := plain.reconcileCanonicalReceipts(t.Context(), sessionstore.ResumeState{}, []operation.Operation{prior}); err != nil {
		t.Fatal(err)
	}
	count := 0
	manager := &receiptInspectorManager{inspect: func(_ context.Context, op operation.Operation) (operation.CanonicalReceiptInspection, error) {
		count++
		op.State[0] = '!'
		return operation.CanonicalReceiptInspection{State: operation.CanonicalReceiptUnknown}, nil
	}}
	selected := &Session{runtime: Runtime{Operations: manager}}
	accepted := make([]operation.Operation, maxStartupReceiptInspections+1)
	for i := range accepted {
		accepted[i] = prior
	}
	if _, err := selected.reconcileCanonicalReceipts(t.Context(), sessionstore.ResumeState{}, accepted); err != nil {
		t.Fatal(err)
	}
	if count != maxStartupReceiptInspections || !prior.State.IsValid() {
		t.Fatal("inspection bound or detached canonical evidence violated")
	}
}
