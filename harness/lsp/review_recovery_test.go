package lsp

import (
	"encoding/json"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewAwaitingRenameRecoversKnownAppliedReceipt(t *testing.T) {
	manager, root := newTestManager(t, "utf-16")
	applied := manager.mutation.Execute(t.Context(), "session/edit", mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: "a.go", Expected: mutation.RevisionOf([]byte("old value\n")), Content: []byte("applied already")}}})
	if applied.Code != mutation.Applied {
		t.Fatal(applied)
	}
	inert := &inertContext{}
	Translator{}.Translate(inert, llm.ToolCall{Name: ToolName, Arguments: `{"language":"go","action":"rename","path":"a.go","new_name":"new"}`})
	spec := inert.specs[0]
	handler, err := NewHandler(t.Context(), manager, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	current := operation.Operation{ID: "edit", ToolName: ToolName, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-handler.RemoteJobUpdates():
		state, err := operation.DecodeRemoteJobState(update)
		if err != nil {
			t.Fatal(err)
		}
		var result Result
		if err = json.Unmarshal(state.Handle, &result); err != nil {
			t.Fatal(err)
		}
		if update.Status != operation.StatusCompleted || result.Code != "applied" || result.Mutation == nil {
			t.Fatalf("proven applied receipt ignored: %+v", result)
		}
		if len(manager.servers) != 0 {
			t.Fatal("recovery replayed LSP request before consulting receipt")
		}
		data, err := os.ReadFile(filepath.Join(root, "a.go"))
		if err != nil || string(data) != "applied already" {
			t.Fatalf("recovery changed committed file: %v %q", err, data)
		}
	case <-time.After(time.Second):
		t.Fatal("no recovered outcome")
	}
}

func TestReviewAwaitingReceiptRequiresCurrentPermission(t *testing.T) {
	manager, _ := newTestManager(t, "utf-16")
	applied := manager.mutation.Execute(t.Context(), "session/edit", mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: "a.go", Expected: mutation.RevisionOf([]byte("old value\n")), Content: []byte("applied already")}}})
	if applied.Code != mutation.Applied {
		t.Fatal(applied)
	}
	inert := &inertContext{}
	Translator{}.Translate(inert, llm.ToolCall{Name: ToolName, Arguments: `{"language":"go","action":"rename","path":"a.go","new_name":"new"}`})
	spec := inert.specs[0]
	handler, err := NewHandler(permission.WithPolicy(t.Context(), permission.DenyAll()), manager, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	current := operation.Operation{ID: "edit", ToolName: ToolName, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusAwaiting}
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-handler.RemoteJobUpdates():
		if update.Status != operation.StatusFailed || update.Denial == nil || len(manager.servers) != 0 {
			t.Fatalf("permission bypass: %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("no denied recovery outcome")
	}
}
