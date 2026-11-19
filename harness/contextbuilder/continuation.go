package contextbuilder

import (
	"encoding/json/v2"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Disposable, canonical-replay-derived dependency state. Building or attempting
// a request never acknowledges receipts. Only a later successful ModelResponse
// at its recorded Turn boundary does. A new user input is not an acknowledgement.
type continuationCall struct {
	receipt            string
	terminal, received bool
}
type continuationGroup struct {
	calls    map[string]*continuationCall
	pending  int
	portable bool
}

type continuationReceipt struct {
	id, group string
	call      *continuationCall
}

func (b *builder) trackContinuationReceipt(group, callID, id string, running bool) {
	g := b.continuations[group]
	if g == nil {
		return
	}
	call := g.calls[callID]
	if call == nil {
		return
	}
	if call.terminal && call.received {
		g.pending++
		b.unsettledGroups[group] = g
	}
	call.receipt, call.terminal, call.received = id, !running, false
	b.unreadReceipts = append(b.unreadReceipts, continuationReceipt{id, group, call})
}

func (b *builder) acknowledgeContinuation(response llm.Response) {
	if response.Failure != nil {
		return
	}
	record, ok := b.source.Data.(sessionstore.ModelResponse)
	if !ok {
		return
	}
	bound, ok := b.turnBounds[record.TurnID]
	if !ok {
		return
	}
	b.deliveredThrough = max(b.deliveredThrough, bound)
	// Receipt IDs increase in projection order. A response can acknowledge
	// only the receipts visible at its Turn, and a newer update supersedes an
	// older one. Process each receipt once even during full history replay.
	acknowledged := 0
	for _, receipt := range b.unreadReceipts {
		if receipt.id > bound {
			break
		}
		acknowledged++
		call := receipt.call
		if call.receipt == receipt.id {
			call.received = true
			if call.terminal {
				group := b.continuations[receipt.group]
				group.pending--
				if group.pending == 0 {
					delete(b.unsettledGroups, receipt.group)
				}
			}
		}
	}
	clear(b.unreadReceipts[:acknowledged])
	b.unreadReceipts = b.unreadReceipts[acknowledged:]
	delete(b.turnBounds, record.TurnID)
}

func (b *builder) requiredContinuations() (map[string]bool, error) {
	result := make(map[string]bool, len(b.unsettledGroups))
	for key, group := range b.unsettledGroups {
		for _, call := range group.calls {
			if call.receipt == "" {
				return nil, &contextengine.Error{Code: "continuation_receipt_missing"}
			}
		}
		result[key] = true
	}
	return result, nil
}

// A redaction is a request projection, never a rewrite of the canonical result.
// It contains fixed vocabulary and actual call/Operation states, not IDs, raw
// errors, arguments, paths, secret text, or an invented aborted/completed state.
func (b *builder) redactedReceipt(running bool) ([]llm.ToolResultOutput, bool) {
	status, present := b.source.Data.(sessionstore.ToolCallStatus)
	state := "completed"
	// WaitingFor records dependencies even in terminal canonical receipts. It
	// says nothing about liveness. Aggregate actual states with fixed precedence,
	// independent of Operation enumeration: active, rejected, unknown, failed,
	// canceled, completed. An absent receipt always remains unavailable.
	active, rejected, unknown, failed, canceled := running, status.Status.Denial != nil, false, status.Status.Error != "", false
	var operations []string
	for _, op := range status.Operations {
		value := "unknown"
		switch op.Status {
		case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
			value = "running"
		case operation.StatusCompleted:
			value = "completed"
		case operation.StatusFailed:
			value = "failed"
		case operation.StatusCanceled:
			value = "canceled"
		}
		operations = append(operations, value)
		active = active || value == "running"
		rejected = rejected || op.Denial != nil
		unknown = unknown || value == "unknown"
		failed = failed || value == "failed"
		canceled = canceled || value == "canceled"
	}
	// Without IDs these are a sorted multiset of actual states, retaining one
	// entry per Operation; they do not claim correspondence to an execution ID.
	slices.Sort(operations)
	switch {
	case !present:
		state = "unavailable"
	case active:
		state = "running"
	case rejected:
		state = "rejected"
	case unknown:
		state = "unknown"
	case failed:
		state = "failed"
	case canceled:
		state = "canceled"
	}
	data, _ := json.Marshal(struct {
		Redacted        bool     `json:"redacted"`
		ReceiptPresent  bool     `json:"receipt_present"`
		State           string   `json:"state"`
		OperationStates []string `json:"operation_states"`
	}{true, present, state, operations})
	return []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(data)}}, present
}
