package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/ast"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestRunnerFileChangesUseAppliedTargets(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	p := newRunnerProjection(&output)
	p.workspace = t.TempDir()
	write := func(kind operation.RemoteJobPlanType, action string, status operation.Status, targets []mutation.TargetResult) error {
		plan, _ := json.Marshal(native.Request{Version: 1, Action: action, Path: "same.txt", Expected: &mutation.Revision{}})
		handle, _ := json.Marshal(native.Result{Version: 1, Code: "applied", Mutation: &mutation.Result{Version: 1, Targets: targets}})
		state, _ := json.Marshal(operation.RemoteJobState{Plan: operation.RemoteJobPlan{Type: kind, Version: 1, Data: plan}, Handle: handle})
		op := operation.Operation{ID: "operation", Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob, Status: status, MaxOutputLength: operation.DefaultMaxOutputLength, State: state}
		return p.observe("parent", host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: "write", Operations: []operation.Operation{op}}}, false)
	}
	for _, code := range []mutation.Code{mutation.Stale, mutation.Denied, mutation.Failed, mutation.Canceled, mutation.NotApplied, mutation.Indeterminate} {
		if err := write(native.PlanType, "write", operation.StatusFailed, []mutation.TargetResult{{Path: "same.txt", Code: code}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.changed) != 0 || strings.Contains(output.String(), `"Kind":"file_change"`) {
		t.Fatal("failed write counted as a change")
	}
	if err := write(ast.PlanType, "", operation.StatusCompleted, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.changed) != 0 {
		t.Fatal("AST preview counted as a change")
	}
	if err := write(ast.PlanType, "", operation.StatusFailed, []mutation.TargetResult{{Path: "same.txt", Code: mutation.Applied}, {Path: "other.txt", Code: mutation.Failed}}); err != nil {
		t.Fatal(err)
	}
	if err := write(native.PlanType, "write", operation.StatusCompleted, []mutation.TargetResult{{Path: filepath.Join(p.workspace, "same.txt"), Code: mutation.Applied}}); err != nil {
		t.Fatal(err)
	}
	if len(p.changed) != 1 || strings.Count(output.String(), `"Kind":"file_change"`) != 1 || !p.changed["same.txt"] {
		t.Fatal("partial result or duplicate normalization incorrect")
	}
}

func TestRunnerChildHistoryIsPagedWithoutTruncation(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "child"); err != nil {
		t.Fatal(err)
	}
	for i := range 260 {
		turn := session.Turn{ID: session.TurnID(fmt.Sprintf("turn-%d", i)), Type: session.TurnRegular}
		if i > 0 {
			turn.PreviousTurnID = session.TurnID(fmt.Sprintf("turn-%d", i-1))
		}
		if err := store.AppendTurn(t.Context(), "child", turn); err != nil {
			t.Fatal(err)
		}
		response := sessionstore.ModelResponse{TurnID: turn.ID, Response: llm.Response{ID: fmt.Sprintf("r-%d", i), Usage: llm.Usage{InputTokens: 1}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "CHILD PRIVATE ANSWER"}}}}}
		if err := store.AppendModelResponse(t.Context(), "child", response); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	p := newRunnerProjection(&output)
	p.directory, p.children = directory, []session.ID{"child"}
	if err := p.aggregateChildren(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.usage.InputTokens != 260 || strings.Count(output.String(), `"Kind":"model_response"`) != 260 || strings.Contains(output.String(), "CHILD PRIVATE ANSWER") {
		t.Fatal("child pagination lost usage or exposed assistant messages")
	}
}

type failSynthetic struct {
	kind string
	err  error
}

func (w failSynthetic) Write(raw []byte) (int, error) {
	var header struct{ Kind string }
	json.Unmarshal(raw, &header)
	if header.Kind == w.kind {
		return 0, w.err
	}
	return len(raw), nil
}

func TestRunnerSyntheticWriteErrorsPropagate(t *testing.T) {
	t.Parallel()
	want := errors.New("output failed")
	p := newRunnerProjection(failSynthetic{kind: "session_end", err: want})
	if err := p.finish(false); !errors.Is(err, want) {
		t.Fatal("terminal write error lost", err)
	}
	p = newRunnerProjection(failSynthetic{kind: "model_response", err: want})
	if err := p.observe("parent", host.HistoryItem{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{}}}, false); !errors.Is(err, want) {
		t.Fatal("child projection write error lost", err)
	}
}

func TestRunnerTerminationOnErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"invalid request", "provider failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			input := `{"prompt":"hello"}`
			if mode == "invalid request" {
				input = `{"prompt":`
			}
			if mode == "canceled" {
				cancel()
			}
			client := &fakeClient{respond: func(context.Context, llm.Request) (llm.Response, error) {
				return llm.Response{}, errors.New("fake provider failed")
			}}
			var output bytes.Buffer
			code := RunMain(ctx, []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string {
				if name == llmAPIKeyEnvironment {
					return "secret"
				}
				return ""
			}, func() []string { return nil }, strings.NewReader(input), &output, io.Discard, testConfig(client))
			want := 1
			if mode == "canceled" {
				want = 130
			}
			if code != want || !bytes.HasSuffix(output.Bytes(), []byte("{\"type\":\"session_end\",\"is_error\":true}\n")) {
				t.Fatalf("code=%d output=%s", code, output.String())
			}
		})
	}
}
