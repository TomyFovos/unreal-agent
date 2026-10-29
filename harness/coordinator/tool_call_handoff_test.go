package coordinator

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"unsafe"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// A scheduling result is owner-local dispatch/grace information. It must not
// carry another full copy of canonical receipt and inherited-fact fields.
func TestToolCallSchedulingHandoffBound(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the reviewed allocation classes are for 64-bit targets")
	}
	t.Logf("toolchain=%s CallStatus=%d ToolCallStatus=%d", runtime.Version(), unsafe.Sizeof(tool.CallStatus{}), unsafe.Sizeof(sessionstore.ToolCallStatus{}))
	for _, method := range []any{(*coordinator).handleModelResponse, (*coordinator).scheduleToolCalls, (*coordinator).reconcileToolCalls} {
		value := reflect.TypeOf(method).Out(0).Elem()
		t.Logf("handoff=%s size=%d", value, value.Size())
		if value.Size() > 64 {
			t.Errorf("owner scheduling handoff copies %d bytes; dispatch and grace need at most 64", value.Size())
		}
	}
}

func TestToolCallHandoffPreservesCanonicalValueAndMetadata(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		for _, state := range []struct {
			name string
			call tool.CallStatus
			want bool
		}{
			{name: "no_operations", want: true},
			{name: "waiting", call: tool.CallStatus{WaitingFor: []operation.ID{"synthetic-operation"}}},
			{name: "error", call: tool.CallStatus{Error: "synthetic error"}, want: true},
			{name: "denied", call: tool.CallStatus{Error: "synthetic denial", Denial: &permission.Error{Code: permission.Denied}}, want: true},
			{name: "truncated_error", call: tool.CallStatus{Error: "synthetic...", ErrorTruncated: true}, want: true},
		} {
			t.Run(state.name+map[bool]string{false: "/plain", true: "/inherited"}[inherited], func(t *testing.T) {
				op := operation.Operation{ID: "synthetic-operation", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusReady}
				if inherited {
					op = operation.WithDependencies(op, []operation.ID{"synthetic-dependency"})
					op = operation.WithIdempotency(op, jsontext.Value(`{"synthetic":3}`))
				}
				canonical := sessionstore.ToolCallStatus{TurnID: "synthetic-turn", CallID: "synthetic-call", Status: state.call, Operations: []operation.Operation{op}}

				item := sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: canonical}
				before, err := json.Marshal(item)
				if err != nil {
					t.Fatal(err)
				}
				handoff := toolCallHandoff(canonical)
				if handoff.TurnID != canonical.TurnID || handoff.CallID != canonical.CallID || !reflect.DeepEqual(handoff.Operations, canonical.Operations) || toolCallStatusesRequireModelResponse([]scheduledToolCall{handoff}) != state.want {
					t.Fatal("scheduling lost dispatch, identity, or model-response meaning")
				}
				handoff.TurnID, handoff.CallID = "other-turn", "other-call"
				// The existing Manager/worker boundary owns a detached metadata
				// snapshot. The compact handoff does not bypass that copy.
				worker := operation.CloneExecutionMetadata(handoff.Operations[0])
				if inherited {
					worker.DependencyIDs()[0] = "other-dependency"
					worker.Idempotency()[2] = 'X'
				} else if worker.Execution != nil || worker.Idempotence != nil {
					t.Fatal("ordinary handoff acquired recovery metadata")
				}
				after, err := json.Marshal(item)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("scheduling or worker metadata changed canonical bytes", err)
				}
				if _, ok := item.Data.(sessionstore.ToolCallStatus); !ok || reflect.TypeOf(canonical.Status) != reflect.TypeFor[tool.CallStatus]() {
					t.Fatal("canonical observer value types changed")
				}
			})
		}
	}
	if toolCallStatusesRequireModelResponse(nil) {
		t.Fatal("empty scheduling batch requires a model response")
	}
}

func TestToolCallHandoffWholeBatchPersistsBeforeDispatch(t *testing.T) {
	for _, failLastStatus := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "last_status_save_fails"}[failLastStatus], func(t *testing.T) {
			f := newToolAdmissionFixture(t)
			want := errors.New("synthetic last status save failure")
			appendHistory := f.store.onAppendToolCallStatus
			f.store.onAppendToolCallStatus = func(status sessionstore.ToolCallStatus) {
				appendHistory(status)
				if failLastStatus && len(f.store.appendedStatuses) == 2 {
					f.store.appendStatusErr = want
				}
			}
			f.manager.addError = func(op operation.Operation) error {
				if len(f.store.appendedStatuses) != 2 {
					t.Error("operation dispatched before both canonical statuses persisted")
				}
				matches := 0
				for _, status := range f.store.appendedStatuses {
					for _, saved := range status.Operations {
						if saved.ID == op.ID && reflect.DeepEqual(saved, op) {
							matches++
						}
					}
				}
				if matches != 1 {
					t.Error("dispatch differs from the unique canonical operation value")
				}
				return nil
			}
			response := admissionResponse(f.current.state.currentTurnID, admissionCall("synthetic-first", `{}`), admissionCall("synthetic-second", `{}`))
			err := f.current.processModelResponse(t.Context(), modelResponseResult{turnID: response.TurnID, response: response.Response})
			if failLastStatus {
				if !errors.Is(err, want) || len(f.manager.adds) != 0 {
					t.Fatal("failed batch persistence dispatched work or lost the failure", err)
				}
			} else if err != nil || len(f.manager.adds) != 2 || len(f.current.state.graceToolCalls) != 2 || f.current.state.grace == nil {
				t.Fatal("complete batch lost dispatch or grace behavior", err)
			}
		})
	}
}
