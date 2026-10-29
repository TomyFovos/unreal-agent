package native

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func outcomeOperation(t *testing.T, action string, result files.Result, status operation.Status) operation.Operation {
	t.Helper()
	request := files.Request{Version: files.Version, Action: action, Path: "fixture.txt"}
	if action == "write" || action == "edit" {
		request.Expected = &mutation.Revision{}
		request.Content, request.OldText, request.NewText = "new", "old", "new"
		if action == "edit" {
			revision := mutation.RevisionOf([]byte("old"))
			request.Expected = &revision
		}
	}
	spec, err := files.Spec(request)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "synthetic-outcome", ToolName: action, Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, State: spec.State, Status: status}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		t.Fatal(err)
	}
	state.Handle = files.EncodeResult(result)
	step, err := operation.UpdateRemoteJob(op, state, status)
	if err != nil {
		t.Fatal(err)
	}
	return *step.Operation
}

// A valid receipt must belong to the request that this translator executed,
// including when a legacy record has no ToolName or a ToolName is misleading.

func appliedOutcome() files.Result {
	return files.Result{Version: files.Version, Code: "applied", Path: "fixture.txt", Mutation: &mutation.Result{Version: mutation.Version, Code: mutation.Applied, Targets: []mutation.TargetResult{{Path: "fixture.txt", Code: mutation.Applied, Revision: mutation.RevisionOf([]byte("new"))}}}}
}

func TestNativeActionAwareOutcome(t *testing.T) {
	for _, action := range []string{"read", "grep", "glob", "write", "edit"} {
		for _, code := range []string{"ok", "binary", "applied", "no_match", "stale", "permission_denied", "indeterminate", "unknown"} {
			t.Run(action+"/"+code, func(t *testing.T) {
				result := files.Result{Version: files.Version, Code: code, Path: "fixture.txt", Binary: code == "binary"}
				if code == "applied" {
					result = appliedOutcome()
				}
				success := action == "read" && (code == "ok" || code == "binary") || (action == "grep" || action == "glob") && code == "ok" || (action == "write" || action == "edit") && code == "applied"
				op := outcomeOperation(t, action, result, operation.StatusCompleted)
				before, _ := json.Marshal(op)
				got := New(action).(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op})
				if got != !success {
					t.Fatalf("action-aware classification: Failed=%t want=%t", got, !success)
				}
				after, _ := json.Marshal(op)
				if string(before) != string(after) {
					t.Fatal("classification mutated canonical receipt")
				}
			})
		}
	}
}
