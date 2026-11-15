package host

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestUnreportedChildEndsAndResumeDoesNotReinfer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response llm.Response
	}{
		{"reasoning_limit", llm.Response{Stop: llm.StopMaxOutputTokens, Output: []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"type":"reasoning","summary":[]}`)}}}, Usage: llm.Usage{OutputTokens: 128000, ReasoningTokens: 119}}},
		{"text_without_finish", llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "partial work"}}}}},
		{"empty", llm.Response{Stop: llm.StopComplete}},
		{"refused", llm.Response{Stop: llm.StopRefused}},
		{"reasoning_only", llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
				calls.Add(1)
				return tc.response, nil
			}))})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			o := Options{ID: "child", Lifecycle: "child", Initial: []inbox.Input{input("task", "work")}}
			s, err := h.Create(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Wait(timeout(t)); err != nil {
				t.Fatal(err)
			}
			finish := s.Finish()
			if finish == nil || finish.Result.Status != "failed" || finish.ModelTurnID == "" || finish.OperationID != "" || !strings.Contains(finish.Result.Summary, string(tc.response.Stop)) || calls.Load() != 1 {
				t.Fatalf("unreported completion=%+v calls=%d", finish, calls.Load())
			}
			view, _ := s.Inspect(0, 128)
			if len(view.Operations) != 0 {
				t.Fatal("invented Finish tool operation")
			}
			o.Initial = nil
			s, err = h.Resume(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Wait(timeout(t)); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("resume repeated a terminal response")
			}
			view, _ = s.Inspect(0, 128)
			finishes := 0
			for _, item := range view.History.Items {
				if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Finish != nil {
					finishes++
				}
			}
			if finishes != 1 {
				t.Fatal("duplicate canonical finish", finishes)
			}
		})
	}
}

func TestParentReplyWaitUsesMessageKindAndRequestOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress bool
		reply    string
		status   operation.Status
		wait     bool
	}{
		{"question", false, "", operation.StatusCompleted, true},
		{"progress", true, "", operation.StatusCompleted, false},
		{"fast_parent_reply", false, "parent", operation.StatusCompleted, false},
		{"other_child_is_not_parent", false, "other-child", operation.StatusCompleted, true},
		{"failed_request", false, "", operation.StatusFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"Action": "parent", "Progress": tc.progress})
			spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			op := operation.Operation{ID: "question", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
			s := &Session{operations: map[operation.ID]operation.Operation{}, historyState: HistoryState{Configuration: []byte(`{"ParentID":"parent"}`)}}
			s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{Operations: []operation.Operation{op}}})
			if tc.reply != "" {
				payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: tc.reply, Target: "child", Handle: "start", Kind: "message", Text: "answer"})
				s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "reply", Kind: inbox.InputPeer, Payload: payload}})
			}
			state, _ := operation.DecodeRemoteJobState(op)
			step, err := operation.UpdateRemoteJob(op, state, tc.status)
			if err != nil {
				t.Fatal(err)
			}
			s.operations[op.ID] = *step.Operation
			s.items = append(s.items, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{Operations: []operation.Operation{*step.Operation}}})
			if waiting := s.awaitingParentReply(); waiting != tc.wait {
				t.Fatal("incorrect reply wait", waiting)
			}
		})
	}
}
