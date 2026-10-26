package native

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type outcomeCollector struct {
	id   operation.ID
	name string
	ops  []operation.Operation
}

func (c *outcomeCollector) Submit(s operation.Spec) operation.ID {
	c.ops = append(c.ops, operation.WithIdempotency(operation.Operation{ID: c.id, ToolName: c.name, Type: s.Type, Version: s.Version, State: s.State, MaxOutputLength: s.MaxOutputLength, Status: operation.StatusReady}, s.Idempotency))
	return c.id
}

// Executes real native Operations in a synthetic workspace, without a model,
// provider, external credential, or the user's canonical Session Store.
func TestNativeOutcomeMatchesActualMutationReceipts(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	policy, err := permission.New(permission.Config{Tools: []string{"read", "write", "edit"}, ReadRoots: []string{workspace}, WriteRoots: []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	ctx = permission.WithPolicy(ctx, policy)
	service, err := mutation.New(mutation.Config{Root: workspace, StateDir: filepath.Join(root, "mutation"), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := files.NewHandler(ctx, files.Executor{Files: service}, "synthetic-outcome-session")
	if err != nil {
		t.Fatal(err)
	}
	manager := operation.NewLocalOperationManagerWithPolicy(ctx, policy, handler)
	defer func() {
		cancel()
		if err := handler.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-manager.Done():
		case <-time.After(2 * time.Second):
			t.Error("native outcome manager cleanup incomplete")
		}
	}()
	registry := tool.NewRegistry(Configure(tool.StaticTranslators{}), tool.ReadName, tool.WriteName, tool.EditName)
	count := 0
	run := func(action string, args map[string]any, wantCode string, wantFailed bool) files.Result {
		t.Helper()
		count++
		id := fmt.Sprintf("synthetic-outcome-%d", count)
		translator, ok := registry.Resolve(action)
		if !ok {
			t.Fatal("native Registry entry missing")
		}
		if err := policy.CheckTool(action); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		collector := &outcomeCollector{id: operation.ID(id), name: action}
		status := translator.Translate(collector, llm.ToolCall{CallID: id, Name: action, Arguments: string(encoded)})
		if status.Error != "" || len(collector.ops) != 1 || len(status.WaitingFor) != 1 || status.WaitingFor[0] != collector.id {
			t.Fatal("native translation did not bind one Operation")
		}
		if err := manager.Add(collector.ops[0]); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case op, open := <-manager.Updates():
				if !open || op.ID != collector.id {
					t.Fatal("unexpected native Operation receipt")
				}
				if op.Status != operation.StatusCompleted && op.Status != operation.StatusFailed && op.Status != operation.StatusCanceled {
					continue
				}
				state, err := operation.DecodeRemoteJobState(op)
				if err != nil {
					t.Fatal(err)
				}
				var result files.Result
				if err := json.Unmarshal(state.Handle, &result); err != nil {
					t.Fatal(err)
				}
				failed := translator.(tool.ResultFailureClassifier).ResultFailed(status, []operation.Operation{op})
				if result.Code != wantCode || failed != wantFailed {
					t.Fatalf("actual %s receipt code=%s Failed=%t; want %s/%t", action, result.Code, failed, wantCode, wantFailed)
				}
				translated, err := translator.TranslateResult(id, status, []operation.Operation{op})
				if err != nil || translated.CallID != id {
					t.Fatal("native receipt lost Tool Call identity")
				}
				t.Logf("native %s Operation=%s status=%s receipt=%s Failed=%t", action, op.ID, op.Status, result.Code, failed)
				return result
			case <-ctx.Done():
				t.Fatal("native outcome Operation timed out")
			}
		}
	}
	run("write", map[string]any{"path": "sample.txt", "expected": mutation.Revision{}, "content": "before\n"}, "applied", false)
	before := run("read", map[string]any{"path": "sample.txt"}, "ok", false)
	if before.Revision == nil || before.Content != "before\n" {
		t.Fatal("Read did not observe the applied write")
	}
	run("edit", map[string]any{"path": "sample.txt", "expected": before.Revision, "old_text": "before", "new_text": "after"}, "applied", false)
	after := run("read", map[string]any{"path": "sample.txt"}, "ok", false)
	if after.Revision == nil || after.Content != "after\n" {
		t.Fatal("Read did not observe the applied edit")
	}
	run("edit", map[string]any{"path": "sample.txt", "expected": before.Revision, "old_text": "after", "new_text": "stale"}, "stale", true)
	run("edit", map[string]any{"path": "sample.txt", "expected": after.Revision, "old_text": "absent", "new_text": "unmatched"}, "no_match", true)
	data, err := os.ReadFile(filepath.Join(workspace, "sample.txt"))
	if err != nil || string(data) != "after\n" || count != 6 {
		t.Fatal("failed edit changed the file or Operation count")
	}
}
