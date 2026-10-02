package sessionstore

import (
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func DecodeOperationIntent(input inbox.Input) (operation.Operation, error) {
	control, err := input.DecodeControlMessage()
	if err != nil {
		return operation.Operation{}, err
	}
	if control.Mode != inbox.DispatchOperation {
		return operation.Operation{}, fmt.Errorf("not an operation intent")
	}
	intent := control.Parameters.(inbox.OperationIntent)
	var spec operation.Spec
	if err = json.Unmarshal(intent.Spec, &spec, json.RejectUnknownMembers(true)); err != nil {
		return operation.Operation{}, err
	}
	if spec.Type == "" || spec.Version == 0 || !spec.State.IsValid() {
		return operation.Operation{}, fmt.Errorf("invalid operation spec")
	}
	return operation.Operation{ID: operation.ID(intent.OperationID), ToolName: intent.ToolName, Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, Idempotency: spec.Idempotency, MaxOutputLength: spec.MaxOutputLength}, nil
}
