package host

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativeTool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

func TestToolEpochSurvivesHostRestartAndResume(t *testing.T) {
	dir := t.TempDir()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(t.Context(), "epoch"); err != nil {
		t.Fatal(err)
	}
	if err = store.AppendInput(t.Context(), "epoch", input("task", "Preserve the original task")); err != nil {
		t.Fatal(err)
	}
	var previous session.TurnID
	for i := range 3 {
		id := session.TurnID(fmt.Sprintf("tool-%d", i))
		call := llm.ToolCall{CallID: string(id), Name: "read", Arguments: fmt.Sprintf(`{"path":"file%d.go"}`, i)}
		if err = store.AppendTurn(t.Context(), "epoch", session.Turn{ID: id, PreviousTurnID: previous, Type: session.TurnRegular}); err != nil {
			t.Fatal(err)
		}
		if err = store.AppendModelResponse(t.Context(), "epoch", sessionstore.ModelResponse{TurnID: id, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}); err != nil {
			t.Fatal(err)
		}
		if err = store.AppendToolCallStatus(t.Context(), "epoch", sessionstore.ToolCallStatus{TurnID: id, CallID: string(id), Status: tool.CallStatus{Error: strings.Repeat("Historical diagnostic output. ", 150)}}); err != nil {
			t.Fatal(err)
		}
		previous = session.TurnID(string(id) + "-ack")
		if err = store.AppendTurn(t.Context(), "epoch", session.Turn{ID: previous, PreviousTurnID: id, Type: session.TurnRegular}); err != nil {
			t.Fatal(err)
		}
		if err = store.AppendModelResponse(t.Context(), "epoch", sessionstore.ModelResponse{TurnID: previous, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Received diagnostics."}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	requests := make(chan llm.Request, 2)
	build := func(ctx context.Context, _ session.ID) (Runtime, error) {
		b := contextbuilder.NewBuilder()
		e, err := contextbuilder.EnableCompaction(b, contextengine.Config{ToolResultKeepTokens: 1, ToolResultMinPruneTokens: 1000}, contextengine.Constraints{})
		if err != nil {
			return Runtime{}, err
		}
		b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("openai-codex")
		b.SetModel(llm.Model{ID: "epoch-model"})
		return Runtime{ContextEngine: e, Builder: b, Tools: tool.NewRegistry(nativeTool.Configure(tool.StaticTranslators{}), "read"), Operations: operation.NewLocalOperationManager(ctx), LLM: modelFunc(func(_ context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
			requests <- request
			return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Continued."}}}}, nil
		})}, nil
	}
	var before []byte
	for pass := range 2 {
		owner, err := New(t.Context(), Config{Directory: dir, Build: build})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { owner.Close() })
		s, err := owner.Resume(t.Context(), Options{ID: "epoch", Lifecycle: "interactive"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Submit(timeout(t), s.Generation, input(fmt.Sprintf("continue-%d", pass), "Continue")); err != nil {
			t.Fatal(err)
		}
		var request llm.Request
		select {
		case request = <-requests:
		case <-timeout(t).Done():
			t.Fatal("missing request after resume")
		}
		encoded, err := json.Marshal(request.Input)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 {
			before = encoded
		} else if !bytes.HasPrefix(encoded, bytes.TrimSuffix(before, []byte("]"))) {
			t.Fatal("Host restart changed the recorded projection")
		}
		waitSelection(t, func() bool {
			view, _ := s.Inspect(0, 4096)
			responses := 0
			for _, item := range view.History.Items {
				if _, ok := item.Data.(sessionstore.ModelResponse); ok {
					responses++
				}
			}
			return responses == 7+pass
		})
		stop(t, s)
		if err = owner.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_, items, _, err := store.ReadSnapshot(t.Context(), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	prunes, originals := 0, 0
	for _, item := range items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil && r.Context.Kind == "prune" {
			prunes++
			if len(r.Context.Projections) != 2 {
				t.Fatal("wrong durable prune batch")
			}
		}
		if _, ok := item.Data.(sessionstore.ToolCallStatus); ok {
			originals++
		}
	}
	if prunes != 1 || originals != 3 || bytes.Count(before, []byte("Output omitted")) != 2 {
		t.Fatal("prune/original records changed across restart", prunes, originals)
	}
}
