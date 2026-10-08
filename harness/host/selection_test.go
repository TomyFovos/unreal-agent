package host

import (
	"context"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"testing"
	"time"
)

func selectionFactory(requests chan llm.Request, responses chan llm.Response) Factory {
	return func(ctx context.Context, _ session.ID) (Runtime, error) {
		base := sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "model-a", Name: "A", Effort: llm.ReasoningEffortMedium}
		active := base
		if prior := ActiveRuntimeSelection(ctx); prior != nil {
			active = *prior
		}
		b := contextbuilder.NewBuilder()
		b.SetModel(llm.Model{ID: active.Model, ReasoningEffort: active.Effort})
		m := modelFunc(func(ctx context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
			select {
			case requests <- r:
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			}
			select {
			case response := <-responses:
				return response, nil
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			}
		})
		return Runtime{Selection: &base, ApplySelection: func(s sessionstore.RuntimeSelection) error {
			b.SetModel(llm.Model{ID: s.Model, ReasoningEffort: s.Effort})
			return nil
		}, Builder: b, LLM: m, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}
}
func selectionRequest(t *testing.T, requests chan llm.Request, model string, effort llm.ReasoningEffort) {
	t.Helper()
	select {
	case r := <-requests:
		if r.Model.ID != model || r.Model.ReasoningEffort != effort {
			t.Fatal("wrong request selection", r.Model)
		}
	case <-timeout(t).Done():
		t.Fatal("no model request")
	}
}
func selectionChoice() sessionstore.RuntimeSelection {
	return sessionstore.RuntimeSelection{Version: 1, RequestID: "change-1", Provider: "openai-codex", Model: "model-b", Name: "B", Effort: llm.ReasoningEffortHigh}
}
func selectionOptions(id session.ID) Options {
	return Options{ID: id, Configuration: []byte(`{"Provider":{"provider":"openai-codex","model":{"id":"model-a"}},"ReasoningEffort":"medium"}`)}
}
func selectionSubmit(t *testing.T, s *Session, id string) {
	t.Helper()
	if _, err := s.Submit(timeout(t), s.Generation, input(id, "prompt-sensitive")); err != nil {
		t.Fatal(err)
	}
}
func TestCanonicalRuntimeSelectionDefersToolLoopAndResumes(t *testing.T) {
	requests, responses := make(chan llm.Request, 8), make(chan llm.Response, 8)
	f := selectionFactory(requests, responses)
	dir := t.TempDir()
	h, err := New(t.Context(), Config{Directory: dir, Build: f})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), selectionOptions("model-session"))
	if err != nil {
		t.Fatal(err)
	}
	selectionSubmit(t, s, "input-1")
	selectionRequest(t, requests, "model-a", llm.ReasoningEffortMedium)
	choice := selectionChoice()
	if err = s.SelectRuntime(t.Context(), s.Generation, 0, choice); err != nil {
		t.Fatal(err)
	}
	if err = s.SelectRuntime(t.Context(), s.Generation, 0, choice); err != nil {
		t.Fatal("idempotent retry failed", err)
	}
	conflict := choice
	conflict.RequestID = "other-change"
	if err = s.SelectRuntime(t.Context(), s.Generation, 0, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("stale choice overwritten", err)
	}
	responses <- llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-1", Name: "unavailable", Arguments: "{}"}}}}
	selectionRequest(t, requests, "model-a", llm.ReasoningEffortMedium)
	active, pending := s.RuntimeSelection()
	if active.Model != "model-a" || pending == nil || pending.Model != "model-b" {
		t.Fatal("mid-loop activation", active, pending)
	}
	responses <- llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}}}
	// The next input cannot interrupt a still executing old request: wait for
	// the canonical final response before submitting it.
	waitSelection(t, func() bool {
		v, _ := s.Inspect(0, 128)
		n := 0
		for _, i := range v.History.Items {
			if i.Kind == sessionstore.ItemModelResponse {
				n++
			}
		}
		return n == 2
	})
	selectionSubmit(t, s, "input-2")
	selectionRequest(t, requests, "model-b", llm.ReasoningEffortHigh)
	responses <- llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}}}
	stop(t, s)
	v, _ := s.Inspect(0, 128)
	var turns []session.Turn
	for _, i := range v.History.Items {
		if turn, ok := i.Data.(session.Turn); ok {
			turns = append(turns, turn)
		}
	}
	if len(turns) != 3 || turns[0].RuntimeRevision != 0 || turns[1].RuntimeRevision != 0 || turns[2].RuntimeRevision != 1 {
		t.Fatal("prior turn selection rewritten", turns)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, err := New(t.Context(), Config{Directory: dir, Build: f})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	s2, err := h2.Resume(t.Context(), selectionOptions(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	active, pending = s2.RuntimeSelection()
	if active.Model != "model-b" || active.Revision != 1 || pending != nil {
		t.Fatal("resume lost selection", active, pending)
	}
	selectionSubmit(t, s2, "input-3")
	selectionRequest(t, requests, "model-b", llm.ReasoningEffortHigh)
	responses <- llm.Response{}
	stop(t, s2)
}

func waitSelection(t *testing.T, ready func() bool) {
	t.Helper()
	ctx := timeout(t)
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatal("canonical response did not commit")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCanonicalRuntimeSelectionPendingSurvivesInterruptedRequest(t *testing.T) {
	requests, responses := make(chan llm.Request, 8), make(chan llm.Response, 8)
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: selectionFactory(requests, responses)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), selectionOptions("pending"))
	if err != nil {
		t.Fatal(err)
	}
	selectionSubmit(t, s, "input-1")
	selectionRequest(t, requests, "model-a", llm.ReasoningEffortMedium)
	if err = s.SelectRuntime(t.Context(), s.Generation, 0, selectionChoice()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Stop(timeout(t), s.Generation, inbox.StopHard, "test interruption"); err != nil {
		t.Fatal(err)
	}
	if err = s.Wait(timeout(t)); err != nil {
		t.Fatal(err)
	}
	s, err = h.Resume(t.Context(), selectionOptions(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequest(t, requests, "model-a", llm.ReasoningEffortMedium)
	active, pending := s.RuntimeSelection()
	if active.Revision != 0 || pending == nil || pending.Revision != 1 {
		t.Fatal("pending choice lost or activated within interrupted loop")
	}
	responses <- llm.Response{}
	waitSelection(t, func() bool {
		v, _ := s.Inspect(0, 128)
		for _, i := range v.History.Items {
			if i.Kind == sessionstore.ItemModelResponse {
				return true
			}
		}
		return false
	})
	selectionSubmit(t, s, "input-2")
	selectionRequest(t, requests, "model-b", llm.ReasoningEffortHigh)
	responses <- llm.Response{}
	stop(t, s)
}

func TestCanonicalRuntimeSelectionPreservesExistingSettingsControls(t *testing.T) {
	requests, responses := make(chan llm.Request, 4), make(chan llm.Response, 4)
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: selectionFactory(requests, responses)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), selectionOptions("legacy-settings"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(timeout(t), s.Generation, inbox.Input{ID: "settings-1", Kind: inbox.InputControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	for n := range 2 {
		if n == 1 {
			s, err = h.Resume(t.Context(), selectionOptions(s.ID))
			if err != nil {
				t.Fatal(err)
			}
		}
		selectionSubmit(t, s, "legacy-input-"+string(rune('a'+n)))
		selectionRequest(t, requests, "model-a", llm.ReasoningEffortHigh)
		responses <- llm.Response{}
		stop(t, s)
	}
}
