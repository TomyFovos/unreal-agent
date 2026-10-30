package host

import (
	"fmt"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestInspectBeforeTailAndPredecessors(t *testing.T) {
	s := &Session{Generation: "g"}
	for i := range 3000 {
		s.items = append(s.items, sessionstore.Item{Sequence: sessionstore.Sequence(i + 1), Kind: sessionstore.ItemTurn, Data: session.Turn{ID: session.TurnID(fmt.Sprintf("turn-%d", i)), Type: session.TurnRegular}})
	}
	page, err := s.InspectBefore(0, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.History.Items) != 128 || page.History.Items[0].Sequence != 2873 || page.History.NextAfter != 3000 || page.History.More || !page.History.Older {
		t.Fatal("wrong tail", page.History)
	}
	first := page.History.Items[0].Sequence
	total := 128
	for page.History.Older {
		page, err = s.InspectBefore(first, 128)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.History.Items) == 0 {
			t.Fatal("no progress")
		}
		if page.History.Items[len(page.History.Items)-1].Sequence != first-1 {
			t.Fatal("page overlap/gap")
		}
		for i, item := range page.History.Items {
			if item.Sequence != page.History.Items[0].Sequence+sessionstore.Sequence(i) {
				t.Fatal("page order")
			}
		}
		first = page.History.Items[0].Sequence
		total += len(page.History.Items)
	}
	if first != 1 || total != 3000 {
		t.Fatal(first, total)
	}
	empty, err := s.InspectBefore(1, 128)
	if err != nil || len(empty.History.Items) != 0 || empty.History.Older {
		t.Fatal("exclusive beginning", err)
	}
	// Pages and summary carry no mutable aliases into the Host.
	page.HistoryState.TurnID = "changed"
	latest, _ := s.InspectBefore(0, 1)
	if latest.HistoryState.TurnID != "turn-2999" {
		t.Fatal("summary aliases Host")
	}
}

func TestHistoryTailStateSurvivesOmittedPrefix(t *testing.T) {
	s := &Session{}
	add := func(kind sessionstore.ItemKind, data any) {
		if input, ok := data.(inbox.Input); ok && input.ID == "" {
			input.ID = inbox.ID(fmt.Sprintf("input-%d", len(s.items)+1))
			data = input
		}
		s.items = append(s.items, sessionstore.Item{Sequence: sessionstore.Sequence(len(s.items) + 1), Kind: kind, Data: data})
	}
	add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Workspace":"/synthetic","Provider":{"provider":"claude-code","model":{"id":"m"}}}`)})
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputPeer, Payload: []byte(`{"Version":1,"Sender":"child","Target":"s","Handle":"worker","Kind":"ended","Text":"background"}`)})
	for range 300 {
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "t", Response: llm.Response{Model: "m", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2}, Generation: &llm.GenerationStatistics{Model: "m"}}})
	}
	v, err := s.InspectBefore(0, 128)
	if err != nil {
		t.Fatal(err)
	}
	h := v.HistoryState
	if !v.History.Background || !h.Background || h.Selection == nil || h.Selection.Model != "m" || h.CacheTotal.InputTokens != 900 || h.CacheTotal.OutputTokens != 600 || h.UsageResponses != 300 || h.UsagePartial {
		t.Fatalf("incorrect tail state: %+v", h)
	}
	v.HistoryState.Configuration[0] = ' '
	again, err := s.InspectBefore(0, 128)
	if err != nil || again.HistoryState.Configuration[0] != '{' {
		t.Fatal("configuration alias", err)
	}
	forward, err := s.Inspect(0, 1)
	if err != nil || forward.History.Items[0].Sequence != 1 || forward.History.NextAfter != 1 || !forward.History.More {
		t.Fatal("changed forward semantics")
	}
}

func TestHistoryReceiptsAcrossPageBoundary(t *testing.T) {
	s := &Session{}
	at := time.Unix(100, 0)
	s.items = []sessionstore.Item{
		{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call", Name: "Read", Arguments: `{"path":"file.go"}`}}}}}},
		{Sequence: 2, Kind: sessionstore.ItemToolCallStatus, RecordedAt: at, Data: sessionstore.ToolCallStatus{CallID: "call", Operations: []operation.Operation{{ID: "op", ToolName: "Read", Status: operation.StatusReady}}}},
		{Sequence: 3, Kind: sessionstore.ItemToolCallStatus, RecordedAt: at.Add(time.Second), Data: sessionstore.ToolCallStatus{CallID: "call", Operations: []operation.Operation{{ID: "op", ToolName: "Read", Status: operation.StatusCompleted}}}},
	}
	v, err := s.InspectBefore(2, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.History.Items) != 1 || len(v.HistoryReceipts) != 1 {
		t.Fatal("missing page call status", v.HistoryReceipts)
	}
	r := v.HistoryReceipts[0]
	if r.Status.CallID != "call" || r.Status.Operations[0].Status != operation.StatusCompleted || !r.StartedAt.Equal(at) || !r.At.Equal(at.Add(time.Second)) {
		t.Fatal("wrong latest receipt", r)
	}
	v.HistoryReceipts[0].Status.Operations[0].ID = "mutated"
	again, err := s.InspectBefore(2, 128)
	if err != nil || again.HistoryReceipts[0].Status.Operations[0].ID != "op" {
		t.Fatal("receipt aliases Host", err)
	}
}

func TestHistoryProgressCountsAsReport(t *testing.T) {
	for _, kind := range []string{"message", "progress"} {
		t.Run(kind, func(t *testing.T) {
			s := &Session{items: []sessionstore.Item{{Sequence: 1, Kind: sessionstore.ItemInput, Data: inbox.Input{
				ID: "report", Kind: inbox.InputPeer,
				Payload: []byte(fmt.Sprintf(`{"Version":1,"Sender":"child","Target":"s","Handle":"child-op","Kind":%q,"Text":"working"}`, kind)),
			}}}}
			// WorkPrefix describes the folded run crossing a page boundary.
			s.items = append(s.items, sessionstore.Item{Sequence: 2, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "t", Type: session.TurnRegular}})
			view, err := s.InspectBefore(0, 1)
			if err != nil {
				t.Fatal(err)
			}
			work := view.History.WorkPrefix
			if !view.History.Background || work == nil || work.Entries != 1 || len(work.Reports) != 1 || work.Reports[0] != "child" {
				t.Fatalf("progress missing from background reports: %+v", view.History)
			}
		})
	}
}

func TestHistoryPrefixWorkAndAttentionBeyondTail(t *testing.T) {
	s := &Session{}
	add := func(kind sessionstore.ItemKind, data any) {
		if input, ok := data.(inbox.Input); ok && input.ID == "" {
			input.ID = inbox.ID(fmt.Sprintf("input-%d", len(s.items)+1))
			data = input
		}
		s.items = append(s.items, sessionstore.Item{Sequence: sessionstore.Sequence(len(s.items) + 1), Kind: kind, Data: data})
	}
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"question"`)})
	add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "\x1b[31m【要確認】\x1b[0mApprove this\nsecond line"}}}}})
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputPeer, Payload: []byte(`{"Version":1,"Sender":"child","Target":"s","Handle":"worker","Kind":"ended","Text":"background"}`)})
	for i := range 300 {
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(fmt.Sprintf("t-%d", i)), Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "background body"}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: fmt.Sprintf("c-%d", i), Name: "Read", Arguments: `{}`}}}}})
	}
	v, err := s.InspectBefore(0, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.HistoryState.Attention) != 1 || v.HistoryState.Attention[0] != "Approve this" {
		t.Fatal("omitted attention lost", v.HistoryState.Attention)
	}
	prefix := v.History.WorkPrefix
	if prefix == nil || prefix.Entries != 301 || prefix.Tools != 300 || len(prefix.Turns) != 300 || len(prefix.Reports) != 1 || prefix.Reports[0] != "child" || prefix.Through != 303 {
		t.Fatalf("incorrect prefix: %+v", prefix)
	}
	// Loading further back must retain the totals for the same folded run.
	older, err := s.InspectBefore(v.History.Items[0].Sequence, 128)
	if err != nil {
		t.Fatal(err)
	}
	if older.History.WorkPrefix == nil || older.History.WorkPrefix.Tools != 300 || len(older.History.WorkPrefix.Turns) != 300 {
		t.Fatal("prefix totals depend on page")
	}
	prefix.Turns[0] = "mutated"
	prefix.Reports[0] = "mutated"
	v.HistoryState.Attention[0] = "mutated"
	again, err := s.InspectBefore(0, 128)
	if err != nil || again.History.WorkPrefix.Turns[0] != "t-0" || again.History.WorkPrefix.Reports[0] != "child" || again.HistoryState.Attention[0] != "Approve this" {
		t.Fatal("display metadata aliases Host", err)
	}
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"approved"`)})
	answered, err := s.InspectBefore(0, 128)
	if err != nil || len(answered.HistoryState.Attention) != 0 {
		t.Fatal("answered attention remains", err)
	}
}
