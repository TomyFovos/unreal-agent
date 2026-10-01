package lsp

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestReviewAwaitingCodeActionReceiptRecovery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		receipt, denied bool
	}{
		{"applied-receipt", true, false}, {"missing-receipt", false, false}, {"current-permission-denied", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, root := newTestManager(t, "utf-16")
			wantFile := "old value\n"
			if tc.receipt {
				applied := manager.mutation.Execute(t.Context(), "session/edit", mutation.Request{Version: mutation.Version,
					Changes: []mutation.Change{{Path: "a.go", Expected: mutation.RevisionOf([]byte(wantFile)), Content: []byte("applied already")}}})
				if applied.Code != mutation.Applied {
					t.Fatal(applied)
				}
				wantFile = "applied already"
			}
			inert := &inertContext{}
			Translator{}.Translate(inert, llm.ToolCall{Name: ToolName, Arguments: `{"language":"go","action":"apply_code_action","action_id":"lost-previous-generation-action"}`})
			if len(inert.specs) != 1 {
				t.Fatal("code action was not translated")
			}
			spec := inert.specs[0]
			ctx := t.Context()
			if tc.denied {
				ctx = permission.WithPolicy(ctx, permission.DenyAll())
			}
			handler, err := NewHandler(ctx, manager, "session")
			if err != nil {
				t.Fatal(err)
			}
			defer handler.Close()
			current := operation.Operation{ID: "edit", ToolName: ToolName, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
			if err := handler.AddRemoteJob(current); err != nil {
				t.Fatal(err)
			}
			select {
			case update := <-handler.RemoteJobUpdates():
				state, err := operation.DecodeRemoteJobState(update)
				if err != nil {
					t.Fatal(err)
				}
				var result Result
				if err := json.Unmarshal(state.Handle, &result); err != nil {
					t.Fatal(err)
				}
				if result.Action != "apply_code_action" {
					t.Fatalf("wrong recovery action: %+v", result)
				}
				if tc.denied {
					if update.Status != operation.StatusFailed || update.Denial == nil || result.Mutation != nil {
						t.Fatalf("permission bypass: %+v", update)
					}
				} else if tc.receipt {
					if update.Status != operation.StatusCompleted || result.Code != "applied" || result.Mutation == nil || result.Mutation.Code != mutation.Applied {
						t.Fatalf("receipt ignored: %+v", result)
					}
				} else if update.Status != operation.StatusFailed || result.Code != "indeterminate" || result.Mutation != nil {
					t.Fatalf("unknown outcome was replayed: %+v", result)
				}
				if len(manager.servers) != 0 {
					t.Fatal("recovery launched or replayed an LSP server")
				}
				data, err := os.ReadFile(filepath.Join(root, "a.go"))
				if err != nil || string(data) != wantFile {
					t.Fatalf("recovery changed committed file: %v %q", err, data)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no code-action recovery outcome")
			}
		})
	}
}
