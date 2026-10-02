package operation_test

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestManagerPermissionDenialsAreTerminalValuesAndDoNotStopManager(t *testing.T) {
	manager := operation.NewLocalOperationManagerWithPolicy(t.Context(), permission.DenyAll())
	current := newShellOperation(t, "denied-shell", operation.ShellInput{Shell: testShellPath, Command: "exit 99"}, t.TempDir(), 64)
	if err := manager.Add(current); err != nil {
		t.Fatalf("denial escaped Add: %v", err)
	}
	failed := receiveTerminalOperation(t, manager.Updates(), current.ID)
	if failed.Status != operation.StatusFailed || failed.Denial == nil || failed.Denial.Code != permission.Denied {
		t.Fatalf("failure=%#v", failed)
	}
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	spec, err := operation.NewValueSpec(jsontext.Value("42"))
	if err != nil {
		t.Fatal(err)
	}
	next := operation.Operation{ID: "value", Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State}
	if err := manager.Add(next); err != nil {
		t.Fatal(err)
	}
	completed := receiveTerminalOperation(t, manager.Updates(), next.ID)
	if completed.Status != operation.StatusCompleted {
		t.Fatal("manager did not continue")
	}
}

func TestManagerBlocksProcessRestrictionsBeforeAnyEffects(t *testing.T) {
	policy, err := permission.New(permission.Config{Tools: []string{"Bash"}, ProcessMode: permission.ProcessUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	manager := operation.NewLocalOperationManagerWithPolicy(t.Context(), policy)
	base := t.TempDir()
	current := newShellOperation(t, "sandbox-missing", operation.ShellInput{Shell: testShellPath, Command: "printf bypass"}, base, 128)
	// A forged origin cannot turn Bash into a harmless tool.
	current.ToolName = "Bash"
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	failed := receiveTerminalOperation(t, manager.Updates(), current.ID)
	if failed.Denial == nil || failed.Denial.Code != permission.Unsupported {
		t.Fatalf("failure=%#v", failed)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatal("denied shell created capture files")
	}
}

func TestResumeRechecksCurrentPolicyForSkillAndViewImage(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	policy, err := permission.New(permission.Config{Tools: []string{"SkillUse", "ViewImage"}, ReadRoots: []string{allowed}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	manager := operation.NewLocalOperationManagerWithPolicy(t.Context(), policy)
	skill := newSkillUseOperation(t, "old-skill", filepath.Join(outside, "SKILL.md"))
	// Simulate a persisted in-progress operation from a formerly broad policy.
	skill.Status = operation.StatusAwaiting
	imageSpec, err := operation.NewViewImageSpec(filepath.Join(outside, "image.png"), operation.ViewImageConfig{MaxSize: 1024, MaxWidth: 100, MaxHeight: 100})
	if err != nil {
		t.Fatal(err)
	}
	image := operation.Operation{ID: "old-image", Type: imageSpec.Type, Version: imageSpec.Version, Status: operation.StatusAwaiting, State: imageSpec.State}
	for _, current := range []operation.Operation{skill, image} {
		if err := manager.Add(current); err != nil {
			t.Fatal(err)
		}
		failed := receiveTerminalOperation(t, manager.Updates(), current.ID)
		if failed.Status != operation.StatusFailed || failed.Denial == nil || failed.Denial.Capability != "filesystem" {
			t.Fatalf("failure=%#v", failed)
		}
	}
}

type deniedRemoteHandler struct{ updates chan operation.Operation }

func (*deniedRemoteHandler) RemoteJobPlanType() operation.RemoteJobPlanType       { return "permission-test" }
func (*deniedRemoteHandler) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return 1 }
func (*deniedRemoteHandler) AddRemoteJob(operation.Operation) error {
	return &permission.Error{Code: permission.Unsupported, Capability: "process", Reason: "sandbox unavailable"}
}
func (*deniedRemoteHandler) CancelRemoteJob(operation.ID, string) error     { return nil }
func (h *deniedRemoteHandler) RemoteJobUpdates() <-chan operation.Operation { return h.updates }

func TestRemoteHandlerPermissionFailureIsNotFatalAddError(t *testing.T) {
	handler := &deniedRemoteHandler{updates: make(chan operation.Operation)}
	manager := operation.NewLocalOperationManagerWithPolicy(t.Context(), permission.Unrestricted(), handler)
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "permission-test", Version: 1, Data: jsontext.Value("{}")})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "denied-remote", Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady, State: spec.State}
	if err := manager.Add(current); err != nil {
		t.Fatal(err)
	}
	failed := receiveTerminalOperation(t, manager.Updates(), current.ID)
	if failed.Status != operation.StatusFailed || failed.Denial == nil || failed.Denial.Code != permission.Unsupported {
		t.Fatalf("failure=%#v", failed)
	}
}
