package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func quarantineTestJSON(value any) jsontext.Value {
	raw, err := json.Marshal(value)
	if err != nil {
		panic("invalid synthetic fixture")
	}
	return raw
}

func quarantineTestNative(action, path string) operation.Operation {
	request := files.Request{Version: files.Version, Action: action, Path: path}
	if action == "write" || action == "edit" {
		request.Expected = &mutation.Revision{}
		if action == "edit" {
			request.OldText, request.NewText = "before", "after"
		}
	}
	return operation.Operation{ID: "synthetic-" + operation.ID(action), ToolName: action, Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob, Status: operation.StatusReady, MaxOutputLength: operation.DefaultMaxOutputLength,
		State: quarantineTestJSON(operation.RemoteJobState{Plan: operation.RemoteJobPlan{Type: files.PlanType, Version: files.Version, Data: quarantineTestJSON(request)}})}
}

func quarantineTestRemote(op operation.Operation, change func(*operation.RemoteJobState)) operation.Operation {
	var state operation.RemoteJobState
	if json.Unmarshal(op.State, &state) != nil {
		panic("invalid synthetic state")
	}
	change(&state)
	op.State = quarantineTestJSON(state)
	return op
}

func quarantineTestReceipt(op operation.Operation, result mutation.Result) operation.Operation {
	return quarantineTestRemote(op, func(state *operation.RemoteJobState) {
		var request files.Request
		if json.Unmarshal(state.Plan.Data, &request) != nil {
			panic("invalid synthetic plan")
		}
		state.Handle = quarantineTestJSON(files.Result{Version: files.Version, Code: string(result.Code), Path: request.Path, Mutation: &result})
	})
}

func quarantineTestShell() operation.Operation {
	return operation.Operation{ID: "synthetic-bash", ToolName: "Bash", Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusReady, MaxOutputLength: operation.DefaultMaxOutputLength,
		State: quarantineTestJSON(operation.ShellState{Input: operation.ShellInput{Shell: "/bin/sh", Command: "synthetic-not-executed", Directory: "/synthetic"}, BaseDirectory: "/synthetic-output"})}
}

func TestQuarantineObservationsRequireTypedExecutionProof(t *testing.T) {
	for _, action := range []string{"read", "grep", "glob"} {
		t.Run(action, func(t *testing.T) {
			op := quarantineTestNative(action, "synthetic.txt")
			if !operationObservational(op) || operationUncertain(op) {
				t.Fatal("typed native observation was treated as a side effect")
			}
			op.ToolName = ""
			if !operationObservational(op) {
				t.Fatal("legacy empty tool label hid a valid observational plan")
			}
		})
	}
	value := operation.Operation{ID: "synthetic-value", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusReady, State: quarantineTestJSON(operation.ValueState{Value: jsontext.Value(`{"constant":true}`)})}
	if !operationObservational(value) || operationUncertain(value) {
		t.Fatal("valid constant Value execution was not recognized as pure")
	}
	for name, op := range map[string]operation.Operation{
		"write":           quarantineTestNative("write", "synthetic.txt"),
		"edit":            quarantineTestNative("edit", "synthetic.txt"),
		"Bash":            quarantineTestShell(),
		"unknown":         {ID: "synthetic-custom", Type: "custom", Version: 1, State: jsontext.Value(`{}`)},
		"malformed_value": {ID: "synthetic-value", Type: operation.TypeValue, Version: operation.VersionValue, State: jsontext.Value(`{"Value":{},"command":"side-effect"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if operationObservational(op) {
				t.Fatal("side effect/unknown execution was accepted as an observation")
			}
		})
	}
}

func TestQuarantineObservationsRejectSpoofingAndMalformedPlans(t *testing.T) {
	valid := quarantineTestNative("read", "synthetic.txt")
	wrongAction := quarantineTestNative("write", "synthetic.txt")
	wrongAction.ToolName = "read"
	wrongTool := valid
	wrongTool.ToolName = "write"
	wrongVersion := valid
	wrongVersion.Version++
	duplicate := valid
	duplicate.State = jsontext.Value(strings.Replace(string(valid.State), `"action":"read"`, `"action":"read","action":"write"`, 1))
	for name, op := range map[string]operation.Operation{
		"action_spoof":      wrongAction,
		"tool_mismatch":     wrongTool,
		"operation_version": wrongVersion,
		"plan_version":      quarantineTestRemote(valid, func(s *operation.RemoteJobState) { s.Plan.Version++ }),
		"foreign_handler":   quarantineTestRemote(valid, func(s *operation.RemoteJobState) { s.Plan.Type = "foreign" }),
		"unknown_plan_field": quarantineTestRemote(valid, func(s *operation.RemoteJobState) {
			s.Plan.Data = jsontext.Value(`{"version":1,"action":"read","path":"synthetic.txt","command":"side-effect"}`)
		}),
		"missing_path":  quarantineTestRemote(valid, func(s *operation.RemoteJobState) { s.Plan.Data = jsontext.Value(`{"version":1,"action":"read"}`) }),
		"duplicate_key": duplicate,
	} {
		t.Run(name, func(t *testing.T) {
			if operationObservational(op) {
				t.Fatal("malformed or spoofed plan passed the observational boundary")
			}
		})
	}
}

func TestQuarantineUncertainTerminalAndRecoveredStates(t *testing.T) {
	write := quarantineTestNative("write", "synthetic.txt")
	write.Status = operation.StatusCompleted
	validResult := mutation.Result{Version: mutation.Version, Code: mutation.Applied, Targets: []mutation.TargetResult{{Path: "synthetic.txt", Code: mutation.Applied, Revision: mutation.RevisionOf([]byte("synthetic-after"))}}}
	applied := quarantineTestReceipt(write, validResult)
	if operationUncertain(applied) {
		t.Fatal("typed applied mutation evidence was ignored")
	}
	stale := quarantineTestReceipt(write, mutation.Result{Version: mutation.Version, Code: mutation.Stale, Targets: []mutation.TargetResult{{Path: "synthetic.txt", Code: mutation.Stale}}})
	stale.Status = operation.StatusFailed
	if operationUncertain(stale) {
		t.Fatal("definite stale rejection treated as unknown execution")
	}
	edit := quarantineTestNative("edit", "synthetic.txt")
	edit.Status = operation.StatusCompleted
	noMatch := quarantineTestReceipt(edit, mutation.Result{Version: mutation.Version, Code: "no_match"})
	if operationUncertain(noMatch) {
		t.Fatal("typed edit preparation refusal treated as unknown execution")
	}
	indeterminate := quarantineTestReceipt(write, mutation.Result{Version: mutation.Version, Code: mutation.Indeterminate})
	indeterminate.Status = operation.StatusFailed
	for name, op := range map[string]operation.Operation{
		"recovered_ready":       quarantineTestNative("write", "synthetic.txt"),
		"recovered_edit":        quarantineTestNative("edit", "synthetic.txt"),
		"indeterminate_receipt": indeterminate,
		"missing_receipt":       write,
		"corrupt_receipt":       quarantineTestRemote(write, func(s *operation.RemoteJobState) { s.Handle = jsontext.Value(`null`) }),
		"applied_without_mutation": quarantineTestRemote(write, func(s *operation.RemoteJobState) {
			s.Handle = quarantineTestJSON(files.Result{Version: files.Version, Path: "synthetic.txt", Code: "applied"})
		}),
		"mismatched_result_path": quarantineTestRemote(applied, func(s *operation.RemoteJobState) {
			var r files.Result
			_ = json.Unmarshal(s.Handle, &r)
			r.Path = "other.txt"
			s.Handle = quarantineTestJSON(r)
		}),
		"mismatched_nested_code": quarantineTestRemote(applied, func(s *operation.RemoteJobState) {
			var r files.Result
			_ = json.Unmarshal(s.Handle, &r)
			r.Mutation.Code = mutation.Stale
			s.Handle = quarantineTestJSON(r)
		}),
		"invalid_applied_revision": quarantineTestRemote(applied, func(s *operation.RemoteJobState) {
			var r files.Result
			_ = json.Unmarshal(s.Handle, &r)
			r.Mutation.Targets[0].Revision = mutation.Revision{}
			s.Handle = quarantineTestJSON(r)
		}),
		"indeterminate_target": quarantineTestRemote(applied, func(s *operation.RemoteJobState) {
			var r files.Result
			_ = json.Unmarshal(s.Handle, &r)
			r.Mutation.Targets[0].Code = mutation.Indeterminate
			s.Handle = quarantineTestJSON(r)
		}),
		"unknown_terminal_handler": {ID: "synthetic-unknown", Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob, Status: operation.StatusCompleted, MaxOutputLength: operation.DefaultMaxOutputLength, State: quarantineTestJSON(operation.RemoteJobState{Plan: operation.RemoteJobPlan{Type: "custom", Version: 1, Data: jsontext.Value(`{}`)}, Handle: jsontext.Value(`{"success":true}`)})},
	} {
		t.Run(name, func(t *testing.T) {
			if !operationUncertain(op) {
				t.Fatal("missing/uncertain execution evidence became definite")
			}
		})
	}
}

func TestQuarantineShellUsesTypedExecutionState(t *testing.T) {
	base := quarantineTestShell()
	var state operation.ShellState
	_ = json.Unmarshal(base.State, &state)
	state.Phase, state.ProcessGroupID = operation.ShellPhaseProcess, 42
	interrupted := base
	interrupted.Status = operation.StatusFailed
	interrupted.State = quarantineTestJSON(state)
	if !operationUncertain(interrupted) {
		t.Fatal("interrupted Bash execution was treated as a definite no-op")
	}
	interrupted.Status = operation.StatusCanceled
	interrupted.Denial = &permission.Error{Code: permission.Denied, Capability: "approval", Reason: "synthetic"}
	interrupted.Permission = &permission.Decision{Mode: permission.ReadOnly, State: "denied"}
	if !operationUncertain(interrupted) {
		t.Fatal("denial erased evidence of a possibly started process")
	}
	state.ProcessGroupID = 0
	state.Result = &operation.ShellResult{ExitCode: 7}
	completed := base
	completed.Status = operation.StatusCompleted
	completed.State = quarantineTestJSON(state)
	if operationUncertain(completed) {
		t.Fatal("definite nonzero process exit was treated as unknown")
	}
}

func TestQuarantinePreAdmissionDenialDoesNotInventUncertainty(t *testing.T) {
	for _, kind := range []string{"write", "Bash"} {
		for _, decision := range []string{"denied", "timeout", "canceled", "revoked", "expired"} {
			t.Run(kind+"/"+decision, func(t *testing.T) {
				op := quarantineTestNative("write", "synthetic.txt")
				if kind == "Bash" {
					op = quarantineTestShell()
				}
				op.Status = operation.StatusFailed
				op.Denial = &permission.Error{Code: permission.Denied, Capability: "approval", Reason: "synthetic admission rejection"}
				op.Permission = &permission.Decision{Mode: permission.Ask, State: decision}
				if operationUncertain(op) != (decision == "expired") {
					t.Fatal("pre-admission rejection and expired prior-Host evidence were confused")
				}
			})
		}
	}
}

func TestQuarantineIndependentObservationsAndConservativeEffects(t *testing.T) {
	prior := quarantineTestNative("write", "synthetic.txt")
	unresolved := []operation.Operation{prior}
	for _, action := range []string{"read", "grep", "glob"} {
		t.Run(action, func(t *testing.T) {
			observation := quarantineTestNative(action, "synthetic.txt")
			if !operationsIndependent(observation, unresolved) {
				t.Fatal("observation was blocked by uncertainty it can safely inspect")
			}
		})
	}
	for name, candidate := range map[string]operation.Operation{
		"same_path_write":           quarantineTestNative("write", "synthetic.txt"),
		"different_literal_write":   quarantineTestNative("write", "other.txt"),
		"symlink_alias_may_overlap": quarantineTestNative("edit", "alias/synthetic.txt"),
		"Bash":                      quarantineTestShell(),
		"unknown":                   {ID: "synthetic-unknown", Type: "custom", Version: 1, State: jsontext.Value(`{}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if operationsIndependent(candidate, unresolved) {
				t.Fatal("new side effect was admitted without a proven independent scope")
			}
		})
	}
	observation := quarantineTestNative("read", "other.txt")
	observation.ID = prior.ID
	if operationsIndependent(observation, unresolved) {
		t.Fatal("quarantine allowed an unresolved identity to change its operation")
	}
	if !operationsIndependent(quarantineTestNative("write", "other.txt"), nil) {
		t.Fatal("normal execution was blocked without an unresolved effect")
	}
}

func TestQuarantineDirectMutationPartialEvidence(t *testing.T) {
	request := mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: "a.txt"}, {Path: "b.txt"}}}
	result := mutation.Result{Version: mutation.Version, Code: mutation.Failed, Targets: []mutation.TargetResult{
		{Path: "a.txt", Code: mutation.Applied, Revision: mutation.RevisionOf([]byte("synthetic-applied"))},
		{Path: "b.txt", Code: mutation.Failed},
	}}
	op := operation.Operation{ID: "synthetic-multi", ToolName: "edit", Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob, Status: operation.StatusFailed, MaxOutputLength: operation.DefaultMaxOutputLength,
		State: quarantineTestJSON(operation.RemoteJobState{Plan: operation.RemoteJobPlan{Type: mutation.PlanType, Version: mutation.Version, Data: quarantineTestJSON(request)}, Handle: quarantineTestJSON(result)})}
	if operationUncertain(op) {
		t.Fatal("complete typed partial-application evidence was lost")
	}
	result.Targets[1].Code = mutation.Indeterminate
	op = quarantineTestRemote(op, func(s *operation.RemoteJobState) { s.Handle = quarantineTestJSON(result) })
	if !operationUncertain(op) {
		t.Fatal("uncertain target was hidden by outer failed code")
	}
}

func TestQuarantineActualMutationUnfinishedIntentIsNotReapplied(t *testing.T) {
	root, receipts := t.TempDir(), t.TempDir()
	if err := os.Chmod(receipts, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("synthetic-before")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), original, 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := permission.New(permission.Config{Tools: []string{"write"}, ReadRoots: []string{root}, WriteRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	ctx := permission.WithPolicy(t.Context(), policy)
	checks := 0
	service, err := mutation.New(mutation.Config{Root: root, StateDir: receipts, Authorize: func(_ context.Context, path string, write bool) error { checks++; return policy.CheckPath(path, write) }})
	if err != nil {
		t.Fatal(err)
	}
	request := mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: "a.txt", Expected: mutation.RevisionOf(original), Content: []byte("synthetic-after")}}}
	id := "synthetic-session/synthetic-operation"
	identity := sha256.Sum256(quarantineTestJSON(request))
	key := sha256.Sum256([]byte(id))
	// This is an explicitly synthetic crash fixture: only the durable intent,
	// not a completed receipt, exists. No real Session or receipt store is read.
	intent := quarantineTestJSON(map[string]any{"version": mutation.Version, "digest": hex.EncodeToString(identity[:])})
	if err := os.WriteFile(filepath.Join(receipts, hex.EncodeToString(key[:])+".receipt"), intent, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result := service.Execute(ctx, id, request)
		if result.Code != mutation.Indeterminate {
			t.Fatal("unfinished durable intent was reinterpreted as executable work")
		}
		op := quarantineTestNative("write", "a.txt")
		op = quarantineTestRemote(op, func(state *operation.RemoteJobState) {
			state.Plan.Data = quarantineTestJSON(files.Request{Version: files.Version, Action: "write", Path: "a.txt", Expected: &request.Changes[0].Expected, Content: string(request.Changes[0].Content)})
		})
		op.Status = operation.StatusFailed
		op = quarantineTestReceipt(op, result)
		if !operationUncertain(op) || operationsIndependent(quarantineTestNative("write", "other.txt"), []operation.Operation{op}) || !operationsIndependent(quarantineTestNative("read", "a.txt"), []operation.Operation{op}) {
			t.Fatal("real indeterminate result bypassed quarantine or blocked safe observation")
		}
	}
	result, found, err := service.Recover(ctx, id)
	if err != nil || !found || result.Code != mutation.Indeterminate || checks != 0 {
		t.Fatal("evidence-only recovery unexpectedly applied or prepared a mutation")
	}
	current, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(current) != string(original) {
		t.Fatal("unfinished intent changed the synthetic workspace")
	}
}

func TestQuarantineMalformedAndBoundedStateFailsClosed(t *testing.T) {
	for name, raw := range map[string]jsontext.Value{
		"malformed": jsontext.Value(`{"broken":`),
		"duplicate": jsontext.Value(`{"Plan":{},"Plan":{}}`),
		"depth":     jsontext.Value(strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)),
		"size":      jsontext.Value(strings.Repeat(" ", quarantineMaxStateBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			op := quarantineTestNative("write", "synthetic.txt")
			op.Status = operation.StatusFailed
			op.State = raw
			if !operationUncertain(op) || operationObservational(op) {
				t.Fatal("malformed or oversized state passed quarantine")
			}
		})
	}
}
