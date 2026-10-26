package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	bashtool "github.com/unreallabsai/unreal-agent/harness/tool/bash"
)

type resultRecoveryAdapter struct{}

func (resultRecoveryAdapter) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	panic("synthetic result classification test must not call a model")
}
func (resultRecoveryAdapter) RequiresFullToolBatch() bool { return true }
func (resultRecoveryAdapter) ConversationRecoveryProvider() string {
	return nativeProviderID
}

type resultOnlyTranslator struct{}

func (resultOnlyTranslator) TranslateResult(id string, _ tool.CallStatus, _ []operation.Operation) (llm.ToolResult, error) {
	return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "synthetic"}}}, nil
}

func TestConversationMalformedTerminalResultCannotClearQuarantine(t *testing.T) {
	for _, test := range []struct {
		name       string
		op         operation.Operation
		translator tool.ResultTranslator
	}{
		{"shell", operation.Operation{ID: "synthetic-invalid-shell", ToolName: "Bash", Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusCompleted, MaxOutputLength: 256, State: jsontext.Value(`{"Input":{},"BaseDirectory":"","Result":{"ExitCode":0}}`)}, bashtool.New(bashtool.Config{Shell: "/bin/sh", Directory: "/synthetic", BaseDirectory: "/synthetic-output"})},
		{"value", operation.Operation{ID: "synthetic-invalid-value", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusCompleted, State: jsontext.Value(`{}`)}, resultOnlyTranslator{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := contextbuilder.NewBuilder()
			current := &coordinator{dependencies: Dependencies{LLM: resultRecoveryAdapter{}, ContextBuilder: builder}, state: newLoopState()}
			current.state.operations[test.op.ID] = test.op
			current.queueUncertainty(test.op)
			status := sessionstore.ToolCallStatus{CallID: "synthetic-call", Status: tool.CallStatus{WaitingFor: []operation.ID{test.op.ID}}, Operations: []operation.Operation{test.op}}
			// The legacy failure classifier alone cannot prove either receipt.
			// Their explicit terminal status/zero ExitCode must not erase the
			// stronger typed uncertainty result.
			if !operationUncertain(test.op) {
				t.Fatal("negative fixture did not reach the uncertain evidence boundary")
			}
			result := llm.ToolResult{CallID: status.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "synthetic"}}}
			if err := current.projectToolResult(status, result, test.translator, false); err != nil {
				t.Fatal("synthetic projection failed")
			}
			if !current.quarantined(test.op.ID) || current.conversation.pending[test.op.ID] || !current.conversation.needsEpoch {
				t.Fatal("uncertain result was not removed from pending and quarantined")
			}
			built, err := builder.Build()
			if err != nil {
				t.Fatal("synthetic context build failed")
			}
			found := false
			for _, item := range built.Request.Input {
				if projected, ok := item.Data.(llm.ToolResult); ok && projected.CallID == status.CallID {
					found = true
					if projected.State != llm.ToolResultUnknown {
						t.Fatal("malformed terminal receipt was projected as definite success")
					}
				}
			}
			if !found {
				t.Fatal("negative result was silently dropped rather than classified")
			}
		})
	}
}

func TestConversationValidatedShellAndValueRemainDefinite(t *testing.T) {
	for _, test := range []struct {
		name  string
		op    operation.Operation
		state llm.ToolResultState
	}{
		{"shell_success", operation.Operation{ID: "synthetic-valid-shell", ToolName: "Bash", Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusCompleted, MaxOutputLength: 256, State: quarantineTestJSON(operation.ShellState{Input: operation.ShellInput{Shell: "/bin/sh", Directory: "/synthetic"}, BaseDirectory: "/synthetic-output", Phase: operation.ShellPhaseReadErr, Result: &operation.ShellResult{ExitCode: 0, Out: "synthetic"}})}, llm.ToolResultSucceeded},
		{"shell_failed_test", operation.Operation{ID: "synthetic-exit-shell", ToolName: "Bash", Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusCompleted, MaxOutputLength: 256, State: quarantineTestJSON(operation.ShellState{Input: operation.ShellInput{Shell: "/bin/sh", Directory: "/synthetic"}, BaseDirectory: "/synthetic-output", Phase: operation.ShellPhaseReadErr, Result: &operation.ShellResult{ExitCode: 7}})}, llm.ToolResultFailed},
		{"constant_value", operation.Operation{ID: "synthetic-valid-value", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusCompleted, State: quarantineTestJSON(operation.ValueState{Value: jsontext.Value(`true`)})}, llm.ToolResultSucceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := contextbuilder.NewBuilder()
			current := &coordinator{dependencies: Dependencies{LLM: resultRecoveryAdapter{}, ContextBuilder: builder}, state: newLoopState()}
			current.state.operations[test.op.ID] = test.op
			current.queueUncertainty(test.op)
			status := sessionstore.ToolCallStatus{CallID: "synthetic-call", Status: tool.CallStatus{WaitingFor: []operation.ID{test.op.ID}}, Operations: []operation.Operation{test.op}}
			var translator tool.ResultTranslator = resultOnlyTranslator{}
			if test.op.Type == operation.TypeShell {
				translator = bashtool.New(bashtool.Config{Shell: "/bin/sh", Directory: "/synthetic", BaseDirectory: "/synthetic-output"})
			}
			if err := current.projectToolResult(status, llm.ToolResult{CallID: status.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "synthetic"}}}, translator, false); err != nil {
				t.Fatal("validated synthetic result projection failed")
			}
			if current.quarantined(test.op.ID) || current.conversation.pending[test.op.ID] {
				t.Fatal("definite result was unnecessarily quarantined or left pending")
			}
			built, err := builder.Build()
			if err != nil {
				t.Fatal("synthetic context build failed")
			}
			found := false
			for _, item := range built.Request.Input {
				if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == status.CallID {
					found = true
					if result.State != test.state {
						t.Fatal("typed terminal success/failure changed during inspection")
					}
				}
			}
			if !found {
				t.Fatal("validated result not projected")
			}
		})
	}
}

type resultEpochStore struct {
	sessionstore.Store
	records []sessionstore.HostRecord
}

func (s *resultEpochStore) AppendHostRecord(_ context.Context, _ session.ID, record sessionstore.HostRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	s.records = append(s.records, record)
	return nil
}

type resultNoExecutionManager struct {
	adds []operation.Operation
}

func (m *resultNoExecutionManager) Add(op operation.Operation) error {
	m.adds = append(m.adds, op)
	return nil
}
func (*resultNoExecutionManager) Cancel(operation.ID, string) error { return nil }
func (*resultNoExecutionManager) Updates() <-chan operation.Operation {
	return nil
}

func TestConversationForkKeepsSharedWorkspaceRestrictionsWithoutExecutionAuthority(t *testing.T) {
	b := contextbuilder.NewBuilder()
	b.SetModel(llm.Model{ID: "synthetic-model"})
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(nativeProviderID)
	if err := b.AddExternalInput(inbox.Input{ID: "synthetic-parent-input", Kind: inbox.InputExternal, Payload: jsontext.Value(`"synthetic shared workspace goal"`)}); err != nil {
		t.Fatal("synthetic input rejected")
	}
	store, manager := &resultEpochStore{}, &resultNoExecutionManager{}
	current := &coordinator{dependencies: Dependencies{SessionID: "synthetic-child", LLM: resultRecoveryAdapter{}, ContextBuilder: b, Sessions: store, Operations: manager}, state: newLoopState()}
	parent := quarantineTestNative("write", "shared.txt")
	parent.ID = "synthetic-parent-operation"
	current.state.operations[parent.ID] = parent
	current.state.currentTurnID = "synthetic-parent-turn"
	current.state.toolCalls[toolCallKey{turnID: "synthetic-parent-turn", callID: "synthetic-parent-call"}] = toolCallState{toolCall: llm.ToolCall{CallID: "synthetic-parent-call", Name: "write", Arguments: `{"path":"shared.txt","content":"synthetic","expected":{"exists":false}}`}, operations: map[operation.ID]struct{}{parent.ID: {}}}
	epoch := sessionstore.ConversationEpoch{Number: 1, Provider: nativeProviderID, TurnID: "synthetic-parent-turn", InputID: "synthetic-parent-input", Attempt: 1, Reason: "quarantine", Snapshot: "synthetic shared workspace uncertainty", Quarantined: []operation.ID{parent.ID}}
	if _, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostConversationEpoch, Conversation: &epoch}}); err != nil {
		t.Fatal("synthetic canonical parent epoch rejected")
	}
	if _, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemFork, Data: sessionstore.Fork{ParentID: "synthetic-parent", PreviousTurnID: "synthetic-parent-turn"}}); err != nil {
		t.Fatal("synthetic fork marker rejected")
	}
	if len(current.state.operations) != 0 || len(current.state.toolCalls) != 0 {
		t.Fatal("fork inherited parent Operation execution authority")
	}
	if err := current.dispatchOperationsToManager(t.Context()); err != nil || len(manager.adds) != 0 {
		t.Fatal("fork dispatched a parent Operation")
	}
	write := quarantineTestNative("write", "different-literal-path.txt")
	write.ID = "synthetic-child-write"
	read := quarantineTestNative("read", "shared.txt")
	read.ID = "synthetic-child-read"
	if !current.quarantined(parent.ID) || current.quarantineDenial(write) == nil || current.quarantineDenial(read) != nil {
		t.Fatal("shared-workspace fork lost the uncertain-effect restriction or blocked safe observation")
	}
	current.state.currentTurnID = "synthetic-child-turn"
	current.actionScope = "synthetic-child-input"
	if err := current.appendConversationEpoch(t.Context(), "quarantine"); err != nil {
		t.Fatal("synthetic child epoch could not preserve inherited restrictions")
	}
	if len(store.records) != 1 || store.records[0].Conversation == nil {
		t.Fatal("child restriction was not recorded canonically")
	}
	child := store.records[0].Conversation
	if child.Number != 1 || len(child.Quarantined) != 1 || child.Quarantined[0] != parent.ID || !current.quarantined(parent.ID) || current.quarantineDenial(write) == nil || current.quarantineDenial(read) != nil {
		t.Fatal("new child epoch erased shared-workspace uncertainty")
	}
	if len(current.state.operations) != 0 || len(manager.adds) != 0 {
		t.Fatal("retaining restrictions restored parent execution authority")
	}
}

func TestConversationControlIntentCannotRetypeRestrictedOperation(t *testing.T) {
	for _, kind := range []string{"inherited_quarantine", "pending"} {
		t.Run(kind, func(t *testing.T) {
			store, manager := &fakeStore{}, &resultNoExecutionManager{}
			current := &coordinator{dependencies: Dependencies{SessionID: "synthetic-child", LLM: resultRecoveryAdapter{}, ContextBuilder: contextbuilder.NewBuilder(), Sessions: store, Operations: manager}, state: newLoopState()}
			id := operation.ID("synthetic-restricted-operation")
			state := current.conversationState()
			if kind == "inherited_quarantine" {
				// A fork inherits only the shared-workspace restriction, never
				// its parent's executable Operation body.
				state.quarantined[id] = true
			} else {
				original := quarantineTestNative("write", "shared.txt")
				original.ID, original.Status = id, operation.StatusCompleted
				current.state.operations[id] = original
				state.pending[id] = true
			}
			before := make(map[operation.ID]operation.Operation, len(current.state.operations))
			for key, op := range current.state.operations {
				before[key] = op
			}
			read := quarantineTestNative("read", "shared.txt")
			input := inbox.Input{ID: "synthetic-forged-read-intent", Kind: inbox.InputControl, Payload: quarantineTestJSON(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(id), ToolName: read.ToolName, Spec: quarantineTestJSON(operation.Spec{Type: read.Type, Version: read.Version, State: read.State, MaxOutputLength: read.MaxOutputLength})}})}
			if _, err := sessionstore.DecodeOperationIntent(input); err != nil {
				t.Fatal("negative fixture is not a valid operation intent")
			}
			err := current.handleInboxInput(t.Context(), input)
			if !errors.Is(err, llm.ErrToolIdentityConflict) {
				t.Errorf("restricted Operation ID did not produce the identity sentinel: %v", err)
			}
			if len(store.appendedInputs) != 0 || len(store.savedOperations) != 0 {
				t.Error("rejected retyping was appended to canonical history")
			}
			if !reflect.DeepEqual(before, current.state.operations) {
				t.Error("rejected intent overwrote or created an Operation body")
			}
			if kind == "inherited_quarantine" && !state.quarantined[id] || kind == "pending" && !state.pending[id] {
				t.Error("rejected intent erased the unresolved restriction")
			}
			if len(manager.adds) != 0 {
				t.Error("restricted Operation ID was dispatched")
			}
		})
	}
}
