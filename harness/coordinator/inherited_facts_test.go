package coordinator

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// The parent producer/executor is real. Fork inspection uses the exact
// HistoricalOperation shape emitted by localfile.inheritItem; the storage and
// child coordinator dispatch manager are fixture components, with no model I/O.
func TestInheritedTerminalFactsBeforeEpochPreserveForkRestrictions(t *testing.T) {
	for _, uncertain := range []bool{true, false} {
		t.Run(map[bool]string{true: "terminal_indeterminate", false: "terminal_applied"}[uncertain], func(t *testing.T) {
			f, store, workspace := newBasisFaultFixture(t)
			preparer := f.current.dependencies.Operations.(*basisFaultPreparer)
			handler := preparer.handler
			request := files.Request{Version: files.Version, Action: "write", Path: "a.txt", Expected: &mutation.Revision{}, Content: "synthetic-parent-content"}
			spec, err := files.Spec(request)
			if err != nil {
				t.Fatal(err)
			}
			a, err := handler.PrepareExecution(preparer.ctx, operation.Operation{ID: "synthetic-parent-a", ToolName: "write", Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: spec.MaxOutputLength})
			if err != nil || len(a.ExecutionBasis()) == 0 {
				t.Fatal("actual producer failed to capture execution basis", err)
			}
			if err = handler.AddRemoteJob(a); err != nil {
				t.Fatal(err)
			}
			select {
			case a = <-handler.RemoteJobUpdates():
			case <-time.After(2 * time.Second):
				t.Fatal("native parent execution did not complete")
			}
			if a.Status != operation.StatusCompleted {
				t.Fatal("parent write failed")
			}
			if content, err := os.ReadFile(filepath.Join(workspace, "a.txt")); err != nil || string(content) != "synthetic-parent-content" {
				t.Fatal("actual native parent write missing")
			}
			if uncertain {
				state, err := operation.DecodeRemoteJobState(a)
				if err != nil {
					t.Fatal(err)
				}
				state.Handle = files.EncodeResult(files.Result{Version: files.Version, Code: string(mutation.Indeterminate), Path: "a.txt", Mutation: &mutation.Result{Version: mutation.Version, Code: mutation.Indeterminate, Targets: []mutation.TargetResult{{Path: "a.txt", Code: mutation.Indeterminate}}}})
				state.TerminalResult = string(mutation.Indeterminate)
				a.State, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				a.Status = operation.StatusFailed
			}
			if operationUncertain(a) != uncertain {
				t.Fatal("fixture did not represent intended typed outcome")
			}
			arguments, err := json.Marshal(map[string]any{"path": "a.txt", "content": "synthetic-parent-content", "expected": mutation.Revision{}})
			if err != nil {
				t.Fatal(err)
			}
			response := admissionResponse(f.current.state.currentTurnID, llm.ToolCall{CallID: "synthetic-inherited-a", Name: "write", Arguments: string(arguments)})
			if _, err = f.current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: response}); err != nil {
				t.Fatal(err)
			}
			status := sessionstore.ToolCallStatus{TurnID: response.TurnID, CallID: "synthetic-inherited-a", Status: tool.CallStatus{WaitingFor: []operation.ID{a.ID}}, Inheritance: &sessionstore.InheritedOperationFacts{InheritedFacts: []sessionstore.HistoricalOperation{sessionstore.HistoricalOperation(operation.CloneExecutionMetadata(a))}}}
			if _, err = f.current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: status}); err != nil {
				t.Fatal(err)
			}
			if len(f.current.state.toolCalls) != 0 {
				t.Fatal("terminal inspection did not retire inherited call")
			}
			if store.epochs != 0 {
				t.Fatal("fixture accidentally depended on an earlier epoch")
			}
			if _, err = f.current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemFork, Data: sessionstore.Fork{ParentID: "synthetic-parent", PreviousTurnID: response.TurnID}}); err != nil {
				t.Fatal(err)
			}
			if f.current.quarantined(a.ID) != uncertain {
				t.Fatal("fork lost uncertain terminal fact or poisoned a certain receipt")
			}
			if len(f.current.state.operations) != 0 || len(f.manager.adds) != 0 {
				t.Fatal("parent execution authority or dispatch survived fork")
			}
			if uncertain {
				if !bytes.Equal(f.current.conversation.effects[a.ID], a.ExecutionBasis()) {
					t.Fatal("fork lost canonical effect footprint")
				}
				bSpec, err := files.Spec(files.Request{Version: files.Version, Action: "write", Path: "b.txt", Expected: &mutation.Revision{}, Content: "independent-literal"})
				if err != nil {
					t.Fatal(err)
				}
				b, denial := f.current.prepareRecoveryOperation(t.Context(), operation.Operation{ID: "synthetic-child-b", ToolName: "write", Type: bSpec.Type, Version: bSpec.Version, Status: operation.StatusReady, State: bSpec.State, MaxOutputLength: bSpec.MaxOutputLength})
				if denial != nil || len(b.RecoveryBasis()) != 1 || b.RecoveryBasis()[0].ID != a.ID {
					t.Fatal("finite independent preflight lost the inherited restriction")
				}
				if !f.current.quarantined(a.ID) {
					t.Fatal("independent B was mistaken for proof of A success")
				}
				if _, err = os.Stat(filepath.Join(workspace, "b.txt")); !os.IsNotExist(err) {
					t.Fatal("preflight executed child mutation")
				}
			}
		})
	}
}
