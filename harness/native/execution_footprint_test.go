package native

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
)

func TestNativeInitialExecutionBasisReachesMutationWithoutRecovery(t *testing.T) {
	h, ctx, root := recoveryBasisFixture(t)
	a, err := h.PrepareExecution(ctx, basisWrite(t, "synthetic-initial", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.ExecutionBasis()) == 0 || len(a.RecoveryBasis()) != 0 {
		t.Fatal("fixture must have its own basis and no uncertain predecessor")
	}
	guarded, err := h.recoveryExecutionContext(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	// The same production context must reject an actual executor request
	// whose target differs, even though no RecoveryBasis/Fence is present.
	changed := mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: "b", Content: []byte("must-not-write")}}}
	if got := h.executor.Files.Apply(guarded, changed); got.Code != mutation.Invalid {
		t.Fatal("initial native basis was not enforced by Mutation")
	}
	if _, err := os.Stat(filepath.Join(root, "b")); !os.IsNotExist(err) {
		t.Fatal("wrong initial target executed")
	}
	r, _, err := recoveryRequest(a)
	if err != nil {
		t.Fatal(err)
	}
	got := h.executor.Execute(guarded, "synthetic-initial-execution", r)
	if got.Code != "applied" || got.Mutation == nil || got.Mutation.Code != mutation.Applied {
		t.Fatal("valid initial execution was blocked")
	}
	data, err := os.ReadFile(filepath.Join(root, "a"))
	if err != nil || string(data) != "synthetic" {
		t.Fatal("valid initial write did not execute")
	}
}
