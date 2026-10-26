package native

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
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
func TestNativeOutcomeBindsRequestActionAndPath(t *testing.T) {
	for _, action := range []string{"read", "write", "edit", "grep", "glob"} {
		result := files.Result{Version: files.Version, Code: "ok"}
		if action == "read" {
			result.Path = "fixture.txt"
		} else if action == "write" || action == "edit" {
			result = appliedOutcome()
		}
		for _, toolName := range []string{"", action} {
			name := toolName
			if name == "" {
				name = "legacy"
			}
			t.Run(action+"/"+name, func(t *testing.T) {
				op := outcomeOperation(t, action, result, operation.StatusCompleted)
				op.ToolName = toolName
				if New(action).(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
					t.Fatal("consistent request and receipt reported failure")
				}
			})
		}
		for _, translatorAction := range []string{"read", "write", "edit", "grep", "glob"} {
			if action == translatorAction {
				continue
			}
			for _, toolName := range []string{"", translatorAction} {
				name := toolName
				if name == "" {
					name = "legacy"
				}
				t.Run(action+"_as_"+translatorAction+"/"+name, func(t *testing.T) {
					op := outcomeOperation(t, action, result, operation.StatusCompleted)
					op.ToolName = toolName
					assertOutcomeFailedUnchanged(t, translatorAction, op)
				})
			}
		}
	}
	for _, action := range []string{"read", "write", "edit"} {
		for _, path := range []string{"", "different.txt"} {
			name := path
			if name == "" {
				name = "missing"
			}
			t.Run(action+"/receipt_path/"+name, func(t *testing.T) {
				result := files.Result{Version: files.Version, Code: "ok", Path: path}
				if action != "read" {
					result = appliedOutcome()
					result.Path, result.Mutation.Targets[0].Path = path, path
				}
				assertOutcomeFailedUnchanged(t, action, outcomeOperation(t, action, result, operation.StatusCompleted))
			})
		}
	}
}

func assertOutcomeFailedUnchanged(t *testing.T, action string, op operation.Operation) {
	t.Helper()
	before, err := json.Marshal(op, jsontext.AllowDuplicateNames(true))
	if err != nil {
		t.Fatal(err)
	}
	if !New(action).(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
		t.Fatal("inconsistent request/receipt reported success")
	}
	after, err := json.Marshal(op, jsontext.AllowDuplicateNames(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("classification mutated canonical Operation/receipt")
	}
}

func TestNativeOutcomeRejectsInvalidRequestData(t *testing.T) {
	for name, data := range map[string]string{
		"null":               `null`,
		"scalar":             `"request"`,
		"array":              `[]`,
		"missing_version":    `{"action":"read","path":"fixture.txt"}`,
		"wrong_version":      `{"version":2,"action":"read","path":"fixture.txt"}`,
		"missing_action":     `{"version":1,"path":"fixture.txt"}`,
		"unknown_action":     `{"version":1,"action":"future","path":"fixture.txt"}`,
		"wrong_action":       `{"version":1,"action":"glob","path":"fixture.txt"}`,
		"missing_path":       `{"version":1,"action":"read"}`,
		"empty_path":         `{"version":1,"action":"read","path":""}`,
		"different_path":     `{"version":1,"action":"read","path":"different.txt"}`,
		"wrong_version_type": `{"version":"1","action":"read","path":"fixture.txt"}`,
		"wrong_action_type":  `{"version":1,"action":[],"path":"fixture.txt"}`,
		"wrong_path_type":    `{"version":1,"action":"read","path":{}}`,
		"duplicate_action":   `{"version":1,"action":"glob","action":"read","path":"fixture.txt"}`,
	} {
		t.Run(name, func(t *testing.T) {
			op := outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "ok", Path: "fixture.txt"}, operation.StatusCompleted)
			state, err := operation.DecodeRemoteJobState(op)
			if err != nil {
				t.Fatal(err)
			}
			// Encode state directly to retain valid-but-wrong request types and
			// duplicate members that the native executor could never produce.
			state.Plan.Data = jsontext.Value(data)
			op.State, err = json.Marshal(state, jsontext.AllowDuplicateNames(true))
			if err != nil {
				t.Fatal(err)
			}
			assertOutcomeFailedUnchanged(t, "read", op)
		})
	}
	for _, field := range []string{"Type", "Version"} {
		t.Run("outer_plan_"+field, func(t *testing.T) {
			op := outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "ok", Path: "fixture.txt"}, operation.StatusCompleted)
			state, err := operation.DecodeRemoteJobState(op)
			if err != nil {
				t.Fatal(err)
			}
			if field == "Type" {
				state.Plan.Type = "foreign"
			} else {
				state.Plan.Version++
			}
			op.State, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcomeFailedUnchanged(t, "read", op)
		})
	}
	t.Run("truncated_json", func(t *testing.T) {
		op := outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "ok", Path: "fixture.txt"}, operation.StatusCompleted)
		op.State = append(op.State, '{')
		if !New("read").(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
			t.Fatal("malformed state reported success")
		}
	})
}

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

func TestNativeUnconfirmedOrContradictoryOutcomeNeverSucceeds(t *testing.T) {
	for _, status := range []operation.Status{operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling, operation.StatusCanceled, operation.StatusFailed, ""} {
		t.Run("status/"+string(status), func(t *testing.T) {
			op := outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "ok"}, status)
			if !New("read").(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
				t.Fatal("unconfirmed or unsuccessful Operation reported success")
			}
		})
	}
	for name, change := range map[string]func(*files.Result){
		"missing_mutation":        func(r *files.Result) { r.Mutation = nil },
		"wrong_mutation_version":  func(r *files.Result) { r.Mutation.Version++ },
		"uncertain_mutation":      func(r *files.Result) { r.Mutation.Code = mutation.Indeterminate },
		"denied_mutation":         func(r *files.Result) { r.Mutation.Denial = &permission.Error{Code: permission.Denied} },
		"missing_target":          func(r *files.Result) { r.Mutation.Targets = nil },
		"multiple_targets":        func(r *files.Result) { r.Mutation.Targets = append(r.Mutation.Targets, r.Mutation.Targets[0]) },
		"empty_path":              func(r *files.Result) { r.Path = ""; r.Mutation.Targets[0].Path = "" },
		"unapplied_target":        func(r *files.Result) { r.Mutation.Targets[0].Code = mutation.NotApplied },
		"absent_target":           func(r *files.Result) { r.Mutation.Targets[0].Revision = mutation.Revision{} },
		"invalid_target_revision": func(r *files.Result) { r.Mutation.Targets[0].Revision.Digest = "invalid" },
		"different_target":        func(r *files.Result) { r.Mutation.Targets[0].Path = "different.txt" },
		"denied_result":           func(r *files.Result) { r.Denial = &permission.Error{Code: permission.Denied} },
		"wrong_result_version":    func(r *files.Result) { r.Version++ },
	} {
		for _, action := range []string{"write", "edit"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				result := appliedOutcome()
				change(&result)
				op := outcomeOperation(t, action, result, operation.StatusCompleted)
				if !New(action).(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
					t.Fatal("contradictory mutation evidence reported success")
				}
			})
		}
	}
	op := outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "binary", Binary: true}, operation.StatusCompleted)
	if !New("edit").(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
		t.Fatal("a read receipt was accepted as successful edit evidence")
	}
	for _, action := range []string{"read", "grep", "glob"} {
		result := appliedOutcome()
		result.Code = "ok"
		op := outcomeOperation(t, action, result, operation.StatusCompleted)
		if !New(action).(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
			t.Fatal("reading tool accepted mutation evidence")
		}
	}
	op = outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "binary"}, operation.StatusCompleted)
	if !New("read").(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
		t.Fatal("binary code without binary observation reported success")
	}
	op = outcomeOperation(t, "read", files.Result{Version: files.Version, Code: "ok", Path: "fixture.txt"}, operation.StatusCompleted)
	op.ToolName = ""
	if New("read").(tool.ResultFailureClassifier).ResultFailed(tool.CallStatus{}, []operation.Operation{op}) {
		t.Fatal("legacy empty ToolName lost valid Read classification")
	}
}
