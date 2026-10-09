package contextbuilder

import (
	"encoding/json/v2"
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
	calls           map[string]*continuationCall
	chain, portable bool
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
	for _, group := range b.continuations {
		closed := true
		for _, call := range group.calls {
			if call.receipt != "" && call.receipt <= bound {
				call.received = true
			}
			closed = closed && call.terminal && call.received
		}
		if closed {
			// A successfully consumed, terminal dependency becomes bounded past
			// history even if this response starts another independent tool call.
			// Unfinished or unreceived updates remain mandatory across that boundary.
			group.chain = false
		}
	}
	delete(b.turnBounds, record.TurnID)
}

func (b *builder) requiredContinuations() (map[string]bool, error) {
	result := map[string]bool{}
	for key, group := range b.continuations {
		required := group.chain
		for _, call := range group.calls {
			required = required || !call.terminal || !call.received
		}
		if !required {
			continue
		}
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
	if !present {
		state = "unavailable"
	}
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
		if value != "completed" {
			state = value
		}
	}
	if status.Status.Error != "" {
		state = "failed"
	}
	if status.Status.Denial != nil {
		state = "rejected"
	}
	if running || len(status.Status.WaitingFor) > 0 {
		state = "running"
	}
	data, _ := json.Marshal(struct {
		Redacted        bool     `json:"redacted"`
		ReceiptPresent  bool     `json:"receipt_present"`
		State           string   `json:"state"`
		OperationStates []string `json:"operation_states"`
	}{true, present, state, operations})
	return []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string(data)}}, present
}
