package host

import (
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func inputLocator(id session.ID, seq sessionstore.Sequence, kind inbox.InputKind) record.Locator {
	switch kind {
	case inbox.InputExternal:
		return record.Input(string(id), uint64(seq), record.UserMessage)
	case inbox.InputPeer:
		return record.Input(string(id), uint64(seq), record.PeerMessage)
	default:
		return record.Item(string(id), uint64(seq))
	}
}

func (s *Session) operationEvent(op operation.Operation, checkpoint record.Locator) Event {
	handle := string(operationRecordHandle(op, ""))
	refs := []record.Reference{{Locator: checkpoint, Operation: string(op.ID), Handle: handle}}
	for _, stream := range operation.RecordStreams(op) {
		refs = append(refs, record.Reference{Locator: record.Output(string(s.ID), string(op.ID), stream), Operation: string(op.ID), Handle: handle})
	}
	return Event{Kind: "operation", Operation: &op, Records: refs}
}

// RecordReferences returns the same locator metadata as a live observation
// for a history item obtained after attaching or resuming a Session.
func (s *Session) RecordReferences(item HistoryItem) []record.Reference {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.Sequence == 0 || item.Sequence > sessionstore.Sequence(len(s.items)) {
		return nil
	}
	return s.itemReferences(s.items[item.Sequence-1])
}

func (s *Session) itemReferences(item sessionstore.Item) []record.Reference {
	a := record.Address{Session: string(s.ID), Kind: record.SessionItem, Sequence: uint64(item.Sequence)}
	ref := record.Reference{Locator: a.Locator()}
	var related []record.Reference
	switch v := item.Data.(type) {
	case session.Turn:
		ref.Turn = string(v.ID)
		for _, locator := range v.BatchRecords {
			related = append(related, record.Reference{Locator: locator, Turn: ref.Turn})
		}
	case inbox.Input:
		ref.Locator = inputLocator(s.ID, item.Sequence, v.Kind)
		ref.Input = string(v.ID)
		if peer, err := v.DecodePeerMessage(); err == nil {
			ref.Handle = peer.Handle
			if peer.ReportLocator != "" {
				related = append(related, record.Reference{Locator: peer.ReportLocator, Handle: peer.Handle})
			}
		} else if op, err := sessionstore.DecodeOperationIntent(v); err == nil {
			ref.Operation = string(op.ID)
			ref.Handle = string(operationRecordHandle(op, ""))
		}
	case sessionstore.ModelResponse:
		ref.Turn = string(v.TurnID)
		for i, output := range v.Response.Output {
			if call, ok := output.Data.(llm.ToolCall); ok {
				a.Kind, a.Part = record.ToolCall, i
				related = append(related, record.Reference{Locator: a.Locator(), Turn: ref.Turn, Call: call.CallID})
			}
		}
	case sessionstore.ToolCallStatus:
		ref.Turn, ref.Call = string(v.TurnID), v.CallID
		for _, op := range v.Operations {
			related = append(related, record.Reference{Locator: ref.Locator, Turn: ref.Turn, Call: ref.Call,
				Operation: string(op.ID), Handle: string(operationRecordHandle(op, ""))})
		}
	case sessionstore.HostRecord:
		if v.Artifact != nil {
			ref.Locator = v.Artifact.Locator(s.ID, item.Sequence)
		}
		if v.Finish != nil {
			var lineage struct {
				ChildID     session.ID
				OperationID operation.ID
			}
			if json.Unmarshal(s.historyState.Configuration, &lineage) == nil && lineage.ChildID == s.ID && lineage.OperationID != "" {
				a.Kind, a.Handle = record.ChildReport, string(lineage.OperationID)
				ref.Locator, ref.Handle = a.Locator(), a.Handle
			}
			ref.Turn, ref.Operation = string(v.Finish.ModelTurnID), string(v.Finish.OperationID)
		}
	}
	return append([]record.Reference{ref}, related...)
}
