package host

import (
	"encoding/json/jsontext"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// HistoryState carries current display metadata and outstanding question heads.
// A terminal can attach at the tail without replaying old message bodies.
// The Host derives it once while restoring/appending canonical items.
type HistoryState struct {
	Attention                                    []string
	Configuration                                jsontext.Value                 `json:",omitzero"`
	Selection, PendingSelection                  *sessionstore.RuntimeSelection `json:",omitzero"`
	TurnID                                       session.TurnID
	LatestKind                                   sessionstore.ItemKind
	LatestAt                                     time.Time
	LastUserAt                                   time.Time
	WaitingForModel, MainInterrupted, Background bool
	CacheLatest, CacheTotal                      llm.Usage
	UsageResponses                               int
	UsagePartial                                 bool
	Generations                                  []llm.GenerationStatistics
}

// HistoryReceipt supplies the latest call linkage for calls in a backward
// page, including a status recorded after that page. It contains no new work.
type HistoryReceipt struct {
	Status        sessionstore.ToolCallStatus
	At, StartedAt time.Time
}

func (s *Session) indexHistory() {
	for len(s.historyBackground) < len(s.items) {
		item := s.items[len(s.historyBackground)]
		h := &s.historyState
		s.historyBackground = append(s.historyBackground, h.Background)
		s.historyWorkBefore = append(s.historyWorkBefore, s.historyWorkCurrent)
		s.observeDisplay(item)
		h.LatestKind, h.LatestAt = item.Kind, item.RecordedAt
		addUsage := func(u llm.Usage) {
			h.CacheLatest = u
			h.UsageResponses++
			total := &h.CacheTotal
			known := len(u.Raw) > 0 && string(u.Raw) != "null" || u.InputTokens != 0 || u.OutputTokens != 0 || u.CachedInputTokens != 0 || u.CacheWriteInputTokens != 0 || u.ReasoningTokens != 0
			h.UsagePartial = h.UsagePartial || !known || len(u.Unknown) > 0
			for _, f := range u.Unknown {
				if !slices.Contains(total.Unknown, f) {
					total.Unknown = append(total.Unknown, f)
				}
			}
			if known {
				total.Raw = jsontext.Value(`{}`)
				total.InputTokens += u.InputTokens
				total.CachedInputTokens += u.CachedInputTokens
				total.CacheWriteInputTokens += u.CacheWriteInputTokens
				total.OutputTokens += u.OutputTokens
				total.ReasoningTokens += u.ReasoningTokens
			}
		}
		if item.Kind == sessionstore.ItemFork {
			h.Configuration = nil
			h.Selection = nil
			h.PendingSelection = nil
		}
		switch d := item.Data.(type) {
		case session.Turn:
			h.TurnID = d.ID
		case sessionstore.HostRecord:
			switch d.Kind {
			case "configuration":
				h.Configuration = projectConfiguration(d.Configuration)
				if h.Selection == nil {
					h.Selection = sessionstore.SelectionFromConfiguration(d.Configuration)
				}
			case sessionstore.HostRuntimeSelection:
				h.PendingSelection = d.Selection
			case sessionstore.HostRuntimeApplied:
				h.Selection = d.Selection
				if h.PendingSelection != nil && d.Selection != nil && h.PendingSelection.Revision <= d.Selection.Revision {
					h.PendingSelection = nil
				}
			case sessionstore.HostContext:
				if d.Context != nil && d.Context.Usage != nil {
					addUsage(*d.Context.Usage)
				}
			}
		case inbox.Input:
			if d.Kind == inbox.InputExternal {
				h.LastUserAt = item.RecordedAt
				h.WaitingForModel = true
				h.MainInterrupted = false
			}
			if control, err := d.DecodeControlMessage(); err == nil {
				if control.Mode == inbox.InterruptAgent || control.Mode == inbox.StopHard {
					h.MainInterrupted = true
					h.WaitingForModel = false
				}
				if control.Mode == inbox.UpdateSettings && h.Selection != nil {
					choice := *h.Selection
					choice.Effort = control.Parameters.(inbox.Settings).ReasoningEffort
					h.Selection = &choice
				}
			}
		case sessionstore.ModelResponse:
			addUsage(d.Response.Usage)
			h.WaitingForModel = false
			var stats *llm.GenerationStatistics
			if d.Response.Generation != nil && d.Response.Generation.Valid() {
				stats = d.Response.Generation
			} else if d.Response.Failure == nil {
				stats = &llm.GenerationStatistics{Model: d.Response.Model, Tokens: llm.ReportedTokens(d.Response.Usage)}
			}
			if stats != nil {
				h.Generations = append(h.Generations, *stats)
				if len(h.Generations) > 8 {
					h.Generations = append([]llm.GenerationStatistics(nil), h.Generations[len(h.Generations)-8:]...)
				}
			}
		case sessionstore.ToolCallStatus:
			if s.historyReceipts == nil {
				s.historyReceipts = make(map[string]HistoryReceipt)
			}
			receipt := s.historyReceipts[d.CallID]
			receipt.Status, receipt.At = d, item.RecordedAt
			ready, blocked := false, d.Status.Error != "" || d.Status.Denial != nil
			for _, op := range d.Operations {
				ready = ready || op.Status == operation.StatusReady
				blocked = blocked || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled || op.Status == operation.StatusCanceling
			}
			if ready && !blocked && receipt.StartedAt.IsZero() {
				receipt.StartedAt = item.RecordedAt
			}
			s.historyReceipts[d.CallID] = receipt
			for _, op := range d.Operations {
				if !h.MainInterrupted && (op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled) {
					h.WaitingForModel = true
				}
			}
		}
	}
}

// InspectBefore returns up to limit items strictly before before, in canonical
// ascending order. Zero means the current tail. Forward More/NextAfter keep
// their usual meanings; Older describes the omitted prefix independently.
func (s *Session) InspectBefore(before sessionstore.Sequence, limit int) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := len(s.items)
	if before > 0 && before-1 < sessionstore.Sequence(end) {
		end = int(before - 1)
	}
	start := max(0, end-limit)
	v, err := s.view(sessionstore.Sequence(start), limit)
	if err != nil {
		return View{}, err
	}
	// Limit the final page to the exclusive boundary, even if new items exist.
	count := end - start
	if len(v.History.Items) > count {
		v.History.Items = v.History.Items[:count]
	}
	v.History.NextAfter = sessionstore.Sequence(end)
	v.History.More = end < len(s.items)
	v.History.Older = start > 0
	s.indexHistory()
	if start < len(s.historyBackground) {
		v.History.Background = s.historyBackground[start]
	} else {
		v.History.Background = s.historyState.Background
	}
	if start < len(s.historyWorkBefore) && s.historyWorkBefore[start] != nil {
		prefix := clone(*s.historyWorkBefore[start])
		v.History.WorkPrefix = &prefix
	}
	state := clone(s.historyState)
	v.HistoryState = &state
	seen := make(map[string]bool)
	for _, item := range v.History.Items {
		if response, ok := item.Data.(sessionstore.ModelResponse); ok {
			for _, output := range response.Response.Output {
				if call, ok := output.Data.(llm.ToolCall); ok && !seen[call.CallID] {
					seen[call.CallID] = true
					if receipt, ok := s.historyReceipts[call.CallID]; ok {
						v.HistoryReceipts = append(v.HistoryReceipts, clone(receipt))
					}
				}
			}
		}
	}
	return v, nil
}
