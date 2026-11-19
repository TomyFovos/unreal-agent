package host

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
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
	v.History.Items, err = observationItems(ctx, s.items, len(s.items)-max(recent, 0))
	if err != nil {
		return View{}, err
	}
	return v, nil
}

// ObservePages streams a fixed history prefix in bounded pages. The viewer
// subscribes from the final cursor to obtain Operations and any later commits.
// Items are Session-owned and immutable after append, so encoding a page does
// not hold the Session lock or delay a live turn.
func (s *Session) ObservePages(ctx context.Context, recent int, send func(View) error) error {
	defer startupPhase(ctx, s.ID, "viewer.observe")()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	items := s.items
	v := clone(View{Session: s.snapshot, Generation: s.Generation, Revision: s.revision, Running: s.running,
		ProjectInstructions: s.instructions})
	if s.err != nil {
		v.Failure = s.err.Error()
	}
	s.mu.Unlock()
	const pageSize = 1024
	fullFrom := len(items) - min(max(recent, 0), len(items))
	for start := 0; ; {
		end := min(start+pageSize, len(items))
		page, err := observationItems(ctx, items[start:end], fullFrom-start)
		if err != nil {
			return err
		}
		v.History = HistoryPage{Items: page, NextAfter: sessionstore.Sequence(end), More: end < len(items)}
		if err = send(v); err != nil || !v.History.More {
			return err
		}
		start = end
	}
}

func observationItems(ctx context.Context, source []sessionstore.Item, fullFrom int) ([]HistoryItem, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	items := make([]HistoryItem, len(source))
	for i, item := range source {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		if i < fullFrom {
			items[i] = observationItem(item)
		} else {
			items[i] = clone(ProjectHistoryItem(item))
		}
	}
	return items, nil
}

// observationItem retains the exact inputs to viewer statistics, activity,
// selection, operation timing and tool-target linkage. It is never persisted or
// used to restore execution, and it does not modify the Session-owned values.
func observationItem(item sessionstore.Item) HistoryItem {
	switch data := item.Data.(type) {
	case session.Turn:
		data.BatchRecords = slices.Clone(data.BatchRecords)
		if data.ContextAdmission != nil {
			admission := *data.ContextAdmission
			data.ContextAdmission = &admission
		}
		item.Data = data
		return HistoryItem(item)
	case sessionstore.ModelResponse:
		response := llm.Response{ID: data.Response.ID, Model: data.Response.Model, Usage: usageMetadata(data.Response.Usage)}
		if data.Response.Generation != nil {
			generation := *data.Response.Generation
			response.Generation = &generation
		}
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
		return HistoryItem(item)
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
			context := &sessionstore.ContextRecord{}
			if usage := data.Context.Usage; usage != nil {
				metadata := usageMetadata(*usage)
				context.Usage = &metadata
			}
			if data.Context.Conversation != nil {
				context.Conversation = &llm.ConversationRecord{}
				if usage := data.Context.Conversation.Usage; usage != nil {
					metadata := usageMetadata(*usage)
					context.Conversation.Usage = &metadata
				}
			}
			data.Context = context
			item.Data = data
		}
	}
	return clone(ProjectHistoryItem(item))
}

func usageMetadata(usage llm.Usage) llm.Usage {
	usage.Unknown = slices.Clone(usage.Unknown)
	if len(usage.Raw) != 0 {
		// The viewer only distinguishes reported usage from an absent value.
		if string(usage.Raw) == "null" {
			usage.Raw = []byte("null")
		} else {
			usage.Raw = []byte("{}")
		}
	}
	return usage
}

// Retain the first grapheme crossing the viewer's 160-rune activity boundary.
// The viewer then applies its normal short() exactly once. Returning an already
// ellipsized string would change output at some Unicode grapheme boundaries.
func observationText(text string) string {
	// One additional ASCII rune settles the boundary of the crossing
	// grapheme. Fall back to Unicode segmentation if it might be extended.
	var prefix [162]byte
	size := 0
	for field := range strings.FieldsSeq(text) {
		if size != 0 {
			prefix[size] = ' '
			size++
		}
		for i := 0; i < min(len(field), len(prefix)-size); i++ {
			if field[i] >= utf8.RuneSelf {
				return unicodeObservationText(text)
			}
			prefix[size+i] = field[i]
		}
		size += min(len(field), len(prefix)-size)
		if size == len(prefix) {
			return string(prefix[:161])
		}
	}
	return string(prefix[:size])
}

func unicodeObservationText(text string) string {
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
