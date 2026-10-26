package coordinator

import (
	"context"
	"encoding/json/v2"
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
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativetool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

// T02 uses the actual Coordinator, Permission, native executor, Mutation and
// disk canonical Store. A's executor finished but its final Operation checkpoint
// was lost: its physical bytes are not substituted for a canonical receipt.
func TestQuarantineIndependentNativeWriteEditT02(t *testing.T) {
	for _, action := range []string{"write", "edit"} {
		t.Run(action, func(t *testing.T) {
			workspace := t.TempDir()
			policy, err := permission.New(permission.Config{Tools: []string{"read", "write", "edit"}, ReadRoots: []string{workspace}, WriteRoots: []string{workspace}})
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), policy))
			defer cancel()
			service, err := mutation.New(mutation.Config{Root: workspace, StateDir: filepath.Join(t.TempDir(), "mutation"), Authorize: func(c context.Context, p string, w bool) error { return permission.FromContext(c).CheckPath(p, w) }})
			if err != nil {
				t.Fatal(err)
			}
			executor := files.Executor{Files: service}
			if action == "edit" {
				if err = os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("before"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			handler, err := files.NewHandler(ctx, executor, "test-session")
			if err != nil {
				t.Fatal(err)
			}
			defer handler.Close()
			manager := operation.NewLocalOperationManagerWithPolicy(ctx, policy, handler)
			defer func() { cancel(); <-manager.Done() }()
			store, err := localfile.New(filepath.Join(t.TempDir(), "canonical"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Create(ctx, "test-session"); err != nil {
				t.Fatal(err)
			}
			if err = store.AppendTurn(ctx, "test-session", session.Turn{ID: "turn-a", Type: session.TurnRegular}); err != nil {
				t.Fatal(err)
			}
			tools := tool.NewRegistry(nativetool.Configure(tool.StaticTranslators{}), tool.ReadName, tool.WriteName, tool.EditName)
			current := New(Dependencies{SessionID: "test-session", Inbox: newTestInbox(t), Sessions: store, ContextBuilder: contextbuilder.NewBuilder(), LLM: resultRecoveryAdapter{}, Tools: tools, Operations: manager}).(*coordinator)
			current.state.currentTurnID = "turn-a"
			initialArgs, _ := json.Marshal(map[string]any{"path": "a.txt", "expected": mutation.Revision{}, "content": "A physically changed; no canonical terminal receipt"})
			initial := admissionResponse("turn-a")
			initial.Response.Output = append(initial.Response.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "original-a", Name: "write", Arguments: string(initialArgs)}})
			aStatuses, err := current.handleModelResponse(ctx, initial)
			if err != nil || len(aStatuses) != 1 || len(aStatuses[0].Operations) != 1 {
				t.Fatal("fixture A proposal must become canonical before execution", err)
			}
			a := aStatuses[0].Operations[0]
			if err = current.dispatchOperationsToManager(ctx); err != nil {
				t.Fatal(err)
			}
			// Native metadata is now produced by the authorized worker. The
			// manual owner must perform the same durable ACK as Run's loop.
			select {
			case prepared := <-manager.Preparations():
				if prepared.Operation.ID != a.ID || len(prepared.Operation.ExecutionBasis()) == 0 {
					t.Fatal("fixture A authorized preparation missing")
				}
				if err = current.commitOperationPreparation(ctx, prepared); err != nil {
					t.Fatal(err)
				}
				a = current.state.operations[a.ID]
			case <-time.After(5 * time.Second):
				t.Fatal("fixture A did not reach durable preparation boundary")
			}
			select {
			case update := <-manager.Updates():
				if update.ID != a.ID || update.Status != operation.StatusCompleted {
					t.Fatal("fixture A must execute at receipt-loss boundary")
				}
				// Simulate process death before this terminal notification is
				// saved; do not apply update to Coordinator/canonical Store.
			case <-time.After(5 * time.Second):
				t.Fatal("fixture A did not execute")
			}
			current.observeUncertainty(a, true)
			current.conversation.retired[toolCallKey{turnID: "turn-a", callID: "original-a"}] = true
			if err = store.AppendTurn(ctx, "test-session", session.Turn{ID: "turn-b", PreviousTurnID: "turn-a", Type: session.TurnRegular}); err != nil {
				t.Fatal(err)
			}
			current.state.currentTurnID = "turn-b"
			expected := mutation.Revision{}
			args := map[string]any{"path": "b.txt", "expected": expected, "content": "B independently created"}
			if action == "edit" {
				expected = mutation.RevisionOf([]byte("before"))
				args = map[string]any{"path": "b.txt", "expected": expected, "old_text": "before", "new_text": "B independently edited"}
			}
			raw, _ := json.Marshal(args)
			response := admissionResponse("turn-b")
			response.Response.Output = append(response.Response.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "independent-b", Name: action, Arguments: string(raw)}})
			statuses, err := current.handleModelResponse(ctx, response)
			if err != nil {
				t.Fatal(err)
			}
			if len(statuses) != 1 || len(statuses[0].Operations) != 1 {
				t.Fatalf("T02 independent B must reach real Mutation; handoffs=%#v", statuses)
			}
			proposalPage, err := store.Items(ctx, "test-session", sessionstore.BeforeFirst, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(proposalPage.Items) == 0 {
				t.Fatal("T02 independent B proposal was not persisted")
			}
			canonicalB, ok := proposalPage.Items[len(proposalPage.Items)-1].Data.(sessionstore.ToolCallStatus)
			if !ok || canonicalB.CallID != "independent-b" || canonicalB.Status.Denial != nil || len(canonicalB.Operations) != 1 || canonicalB.Operations[0].ID != statuses[0].Operations[0].ID {
				t.Fatalf("T02 independent B lost its successful canonical proposal: %#v", canonicalB)
			}
			bID := statuses[0].Operations[0].ID
			if err = current.dispatchOperationsToManager(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case prepared := <-manager.Preparations():
				if prepared.Operation.ID != bID || len(prepared.Operation.ExecutionBasis()) == 0 || len(prepared.Operation.RecoveryBasis()) != 1 {
					t.Fatal("independent B authorized preparation missing")
				}
				if err = current.commitOperationPreparation(ctx, prepared); err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("independent B did not reach durable preparation boundary")
			}
			select {
			case update := <-manager.Updates():
				if update.ID != bID {
					t.Fatalf("quarantined A or foreign Operation executed: %s", update.ID)
				}
				if err = current.handleOperationUpdate(ctx, update); err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("independent B did not terminate")
			}
			if _, err = current.reconcileToolCalls(ctx); err != nil {
				t.Fatal(err)
			}
			op := current.state.operations[bID]
			if op.Status != operation.StatusCompleted || op.Denial != nil {
				t.Fatalf("B did not complete: %s", op.Status)
			}
			snap, err := service.Snapshot(ctx, "b.txt")
			if err != nil {
				t.Fatal(err)
			}
			want := "B independently created"
			if action == "edit" {
				want = "B independently edited"
			}
			if string(snap.Data) != want || snap.Revision != mutation.RevisionOf([]byte(want)) {
				t.Fatal("B physical bytes/revision differ from expected")
			}
			if !current.quarantined(a.ID) || current.state.operations[a.ID].Status != operation.StatusReady {
				t.Fatal("A uncertainty was cleared or falsely completed")
			}
			page, err := store.Items(ctx, "test-session", sessionstore.BeforeFirst, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range page.Items {
				if s, ok := item.Data.(sessionstore.ToolCallStatus); ok && s.CallID == "independent-b" && len(s.Operations) == 1 && s.Operations[0].ID == bID && s.Operations[0].Status == operation.StatusCompleted {
					found = true
				}
			}
			if !found {
				t.Fatal("canonical terminal B receipt missing")
			}
			t.Logf("T02 action=%s A_id=%s A_initial_executions=1 A_additional_executions=0 B_id=%s B_revision=%s canonical_receipt=true", action, a.ID, bID, snap.Revision.Digest)
		})
	}
}
