package tui

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestNativeReceiptReasonIsClosedAndNeverRaw(t *testing.T) {
	for _, reason := range []string{string(mutation.ReasonWorkspaceBoundary), "private-sensitive\x1b[31m"} {
		state, _ := json.Marshal(operation.RemoteJobState{Plan: operation.RemoteJobPlan{Type: files.PlanType, Version: files.Version, Data: []byte(`{}`)}, Handle: []byte(`{"reason":` + strconvQuote(reason) + `,"content":"private-sensitive"}`)})
		op := operation.Operation{Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob, MaxOutputLength: 4096, State: state}
		got := operationReason(op)
		if reason == string(mutation.ReasonWorkspaceBoundary) {
			if got != "native reason=workspace_boundary" {
				t.Fatal(got)
			}
		} else if got != "" {
			t.Fatal("unrecognized raw reason displayed", got)
		}
	}
}

func strconvQuote(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
