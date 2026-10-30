package host

import (
	"context"
	"encoding/json/v2"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Observe captures the metadata needed by a viewer in one revision. The recent
// transcript remains complete; older message bodies, reasoning and context
// projections are omitted from this disposable observation. Inspect still reads
// the complete canonical transcript when a caller explicitly requests history.
func (s *Session) Observe(ctx context.Context, recent int) (View, error) {
	defer startupPhase(ctx, s.ID, "viewer.observe")()
	if err := context.Cause(ctx); err != nil {
		return View{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.view(sessionstore.Sequence(len(s.items)), 1)
	if err != nil {
		return View{}, err
	}
	start := len(s.items) - min(max(recent, 0), len(s.items))
	items := make([]HistoryItem, len(s.items))
	for i, item := range s.items {
		if err := context.Cause(ctx); err != nil {
			return View{}, err
		}
		if i < start {
			item = observationItem(item)
		}
		items[i] = ProjectHistoryItem(item)
	}
	v.History.Items = clone(items)
	return v, nil
}

// observationItem retains the exact inputs to viewer statistics, activity,
// selection, operation timing and tool-target linkage. It is never persisted or
// used to restore execution, and it does not modify the Session-owned values.
func observationItem(item sessionstore.Item) sessionstore.Item {
	switch data := item.Data.(type) {
	case sessionstore.ModelResponse:
		response := llm.Response{ID: data.Response.ID, Model: data.Response.Model, Usage: data.Response.Usage}
		for _, output := range data.Response.Output {
			switch value := output.Data.(type) {
			case llm.Message:
				if value.Role == llm.RoleAssistant {
					value.Text = observationText(value.Text)
					response.Output = append(response.Output, llm.Item{Type: llm.ItemMessage, Data: value})
				}
			case llm.ToolCall:
				response.Output = append(response.Output, llm.Item{Type: llm.ItemToolCall, Data: value})
			}
		}
		data.Response = response
		item.Data = data
	case inbox.Input:
		if data.Kind == inbox.InputExternal {
			var text string
			if json.Unmarshal(data.Payload, &text) == nil {
				data.Payload, _ = json.Marshal(observationText(text))
			} else {
				data.Payload = nil
			}
		} else if data.Kind == inbox.InputCrash {
			data.Payload = nil
		}
		item.Data = data
	case sessionstore.ToolCallStatus:
		ops := make([]operation.Operation, len(data.Operations))
		for i, op := range data.Operations {
			ops[i] = operation.Operation{ID: op.ID, Status: op.Status}
		}
		data.Operations = ops
		item.Data = data
	case sessionstore.HostRecord:
		if data.Kind == sessionstore.HostContext && data.Context != nil {
			context := &sessionstore.ContextRecord{Usage: data.Context.Usage}
			if data.Context.Conversation != nil {
				context.Conversation = &llm.ConversationRecord{Usage: data.Context.Conversation.Usage}
			}
			data.Context = context
			item.Data = data
		}
	}
	return item
}

// Retain the first grapheme crossing the viewer's 160-rune activity boundary.
// The viewer then applies its normal short() exactly once. Returning an already
// ellipsized string would change output at some Unicode grapheme boundaries.
func observationText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= 160 {
		return text
	}
	g := uniseg.NewGraphemes(text)
	runes := 0
	for g.Next() {
		runes += utf8.RuneCountInString(g.Str())
		if runes > 160 {
			_, end := g.Positions()
			return strings.Clone(text[:end])
		}
	}
	return text
}
