package native

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func recoveryBasisFixture(t *testing.T) (*Handler, context.Context, string) {
	t.Helper()
	root := t.TempDir()
	policy, err := permission.New(permission.Config{Tools: []string{"read", "write", "edit"}, ReadRoots: []string{root}, WriteRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policy.Close() })
	ctx := permission.WithPolicy(t.Context(), policy)
	s, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "mutation"), Authorize: func(c context.Context, p string, w bool) error { return permission.FromContext(c).CheckPath(p, w) }})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(ctx, Executor{Files: s}, "synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, ctx, root
}
func basisWrite(t *testing.T, id, path string) operation.Operation {
	t.Helper()
	spec, err := Spec(Request{Version: Version, Action: "write", Path: path, Expected: &mutation.Revision{}, Content: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: operation.ID(id), ToolName: "write", Status: operation.StatusReady, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength}
}
func TestNativeRecoveryBasisRequiresBoundHistoricalEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "unknown-fields", "wrong-plan", "same-path", "different-root", "parent-replaced", "target-alias"} {
		t.Run(mode, func(t *testing.T) {
			h, ctx, root := recoveryBasisFixture(t)
			if err := os.Mkdir(filepath.Join(root, "parent"), 0700); err != nil {
				t.Fatal(err)
			}
			a, err := h.PrepareExecution(ctx, basisWrite(t, "a", "parent/a"))
			if err != nil {
				t.Fatal(err)
			}
			b := basisWrite(t, "b", "parent/b")
			blocked := []operation.UncertainEffect{{ID: a.ID, Basis: a.ExecutionBasis().Clone()}}
			switch mode {
			case "missing":
				blocked[0].Basis = nil
			case "malformed":
				blocked[0].Basis = []byte(`{"version":`)
			case "unknown-fields":
				var fields map[string]any
				if err := json.Unmarshal(blocked[0].Basis, &fields); err != nil {
					t.Fatal(err)
				}
				fields["grant"] = true
				blocked[0].Basis, _ = json.Marshal(fields)
			case "wrong-plan":
				b, err = h.PrepareExecution(ctx, b)
				if err != nil {
					t.Fatal(err)
				}
				state, _ := operation.DecodeRemoteJobState(b)
				var r Request
				json.Unmarshal(state.Plan.Data, &r)
				r.Path = "parent/changed"
				state.Plan.Data, _ = json.Marshal(r)
				b.State, _ = json.Marshal(state)
			case "same-path":
				b = basisWrite(t, "b", "parent/a")
			case "different-root":
				var basis executionBasis
				if json.Unmarshal(blocked[0].Basis, &basis) != nil {
					t.Fatal("bad fixture")
				}
				basis.Footprint.Root = t.TempDir()
				blocked[0].Basis, _ = json.Marshal(basis)
			case "parent-replaced":
				if err := os.Rename(filepath.Join(root, "parent"), filepath.Join(root, "retired-parent")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "parent"), 0700); err != nil {
					t.Fatal(err)
				}
			case "target-alias":
				if err := os.WriteFile(filepath.Join(root, "parent/a"), []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(root, "parent/a"), filepath.Join(root, "parent/b")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = h.PrepareRecovery(ctx, b, blocked); err == nil {
				t.Fatal("missing/conflicting historical proof granted a new effect")
			}
			if _, err = os.Stat(filepath.Join(root, "parent/b")); mode != "target-alias" && !os.IsNotExist(err) {
				t.Fatal("preflight changed the candidate")
			}
		})
	}
}
func TestNativeRecoveryBasisFreshSameDirectoryIsNotADenial(t *testing.T) {
	h, ctx, _ := recoveryBasisFixture(t)
	a, err := h.PrepareExecution(ctx, basisWrite(t, "a", "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.PrepareRecovery(ctx, basisWrite(t, "b", "b"), []operation.UncertainEffect{{ID: a.ID, Basis: a.ExecutionBasis()}})
	if err != nil || len(b.ExecutionBasis()) == 0 || len(b.RecoveryBasis()) != 1 {
		t.Fatal("fresh independent target must acquire finite execution evidence", err)
	}
	if _, err = h.recoveryExecutionContext(ctx, b); err != nil {
		t.Fatal(err)
	}
	// A bounded change cannot be relabeled as another native action by swapping
	// a tool label, plan or metadata while keeping the old proof.
	b.ToolName = "edit"
	if _, err = h.recoveryExecutionContext(ctx, b); err == nil {
		t.Fatal("mismatched action accepted execution metadata")
	}
}
