package host

import (
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/displaytext"
)

// HistoryWork summarizes a contiguous folded background run. Prefix pointers
// share one run inside the Host; only the run crossing a page boundary is sent.
// Through lets the terminal add new live entries without counting the prefix
// again, while Turns preserves deduplication across that boundary.
type HistoryWork struct {
	Entries, Tools int
	Turns          []session.TurnID
	Reports        []session.ID
	Through        sessionstore.Sequence
}

func (s *Session) displayPart(role, text string, turn session.TurnID, tools int, peer, kind string, sequence sessionstore.Sequence) {
	h := &s.historyState
	if role == "you" {
		h.Background = false
		h.Attention = nil
	}
	if role == "peer" {
		h.Background = true
		if kind == "ready" {
			return
		}
	}
	if role == "agent" {
		if message, ok := displaytext.Attention(text); ok {
			h.Attention = append(h.Attention, message)
		}
	}
	if role != "peer" && !(h.Background && role == "agent" && !displaytext.ForHuman(text)) {
		s.historyWorkCurrent = nil
		s.historyWorkTurns = nil
		s.historyWorkReports = nil
		return
	}
	if s.historyWorkCurrent == nil {
		s.historyWorkCurrent = &HistoryWork{}
		s.historyWorkTurns = map[session.TurnID]bool{}
		s.historyWorkReports = map[session.ID]bool{}
	}
	work := s.historyWorkCurrent
	work.Entries++
	work.Tools += tools
	work.Through = sequence
	if turn != "" && !s.historyWorkTurns[turn] {
		s.historyWorkTurns[turn] = true
		work.Turns = append(work.Turns, turn)
	}
	if peer != "" && !s.historyWorkReports[session.ID(peer)] {
		s.historyWorkReports[session.ID(peer)] = true
		work.Reports = append(work.Reports, session.ID(peer))
	}
}

// observeDisplay folds the same visible roles as the terminal, without retaining
// response bodies. Configuration, turns and statuses do not break a folded run.
func (s *Session) observeDisplay(item sessionstore.Item) {
	part := func(role, text string, turn session.TurnID, tools int, peer, kind string) {
		s.displayPart(role, text, turn, tools, peer, kind, item.Sequence)
	}
	switch d := item.Data.(type) {
	case inbox.Input:
		switch d.Kind {
		case inbox.InputExternal:
			var text string
			if json.Unmarshal(d.Payload, &text) == nil {
				part("you", "", "", 0, "", "")
			}
		case inbox.InputPeer:
			if peer, err := d.DecodePeerMessage(); err == nil {
				part("peer", "", "", 0, peer.Sender, peer.Kind)
			}
		case inbox.InputCrash:
			part("host", "", "", 0, "", "")
		case inbox.InputControl:
			if control, err := d.DecodeControlMessage(); err == nil {
				switch control.Mode {
				case inbox.DispatchOperation:
					part("agent", "", "", 1, "", "")
				case inbox.StopHard, inbox.StopWhenIdle, inbox.InterruptAgent, inbox.CancelChildren, inbox.CancelOperation:
					part("host", "", "", 0, "", "")
				case inbox.UpdateSettings:
					if control.Parameters.(inbox.Settings).ReasoningEffort != "" {
						part("host", "", "", 0, "", "")
					}
				}
			}
		}
	case sessionstore.HostRecord:
		if d.Kind == sessionstore.HostContext && d.Context != nil && d.Context.Conversation != nil {
			r := d.Context.Conversation
			if r.Compacted || r.Interrupted || r.Error != "" {
				part("host", "", "", 0, "", "")
			}
		}
	case sessionstore.ModelResponse:
		var messages []string
		tools := 0
		for _, output := range d.Response.Output {
			if message, ok := output.Data.(llm.Message); ok && output.Type == llm.ItemMessage && (message.Role == "" || message.Role == llm.RoleAssistant) && (message.Phase == "" || message.Phase == "commentary" || message.Phase == "final" || message.Phase == "final_answer") {
				messages = append(messages, displaytext.Message(message.Text))
			}
			if _, ok := output.Data.(llm.ToolCall); ok {
				tools++
			}
		}
		if len(messages) == 0 && tools > 0 {
			messages = append(messages, "")
		}
		for i, text := range messages {
			calls := 0
			if i == len(messages)-1 {
				calls = tools
			}
			part("agent", text, d.TurnID, calls, "", "")
		}
		if d.Response.Failure != nil {
			part("error", "", "", 0, "", "")
		}
	}
}
