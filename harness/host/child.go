package host

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type sessionContextKey struct{}

// SessionFromContext is available to the Host runtime factory and its handlers.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(*Session)
	return s, ok
}
func (s *Session) Finish() *sessionstore.FinishRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finish == nil {
		return nil
	}
	r := clone(*s.finish)
	return &r
}

// CommitFinish commits a canonical report before a child can stop. The
// invoking Finish operation is the only permitted unfinished operation.
func (s *Session) CommitFinish(ctx context.Context, id operation.ID, result sessionstore.FinishResult) error {
	r := sessionstore.FinishRecord{Version: 1, OperationID: id, Result: result}
	if err := r.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle != "child" {
		return fmt.Errorf("Finish requires child lifecycle")
	}
	if s.finish != nil {
		a, _ := json.Marshal(s.finish)
		b, _ := json.Marshal(r)
		if string(a) == string(b) {
			return nil
		}
		return ErrConflict
	}
	op, ok := s.operations[id]
	if !ok || op.ToolName != "Finish" || terminal(op.Status) {
		return fmt.Errorf("Finish operation is not active")
	}
	for other, value := range s.operations {
		if other != id && !terminal(value.Status) {
			return fmt.Errorf("Finish requires no unfinished operations")
		}
	}
	return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &r})
}
func terminal(s operation.Status) bool {
	return s == operation.StatusCompleted || s == operation.StatusFailed || s == operation.StatusCanceled
}

// CancelOperation acknowledges durable intent. Executor cancellation follows
// only after the Coordinator persists the canceling operation checkpoint.
func (s *Session) CancelOperation(ctx context.Context, generation string, inputID inbox.ID, id operation.ID) (Receipt, error) {
	data, err := json.Marshal(inbox.ControlMessage{Mode: inbox.CancelOperation, Parameters: inbox.CancelRequest{OperationID: string(id)}})
	if err != nil {
		return Receipt{}, err
	}
	return s.Submit(ctx, generation, inbox.Input{ID: inputID, Kind: inbox.InputControl, Payload: data})
}

// SubmitPeer binds canonical provenance to a specific authenticated channel.
func (s *Session) SubmitPeer(ctx context.Context, generation, sender, handle string, input inbox.Input) (Receipt, error) {
	peer, err := input.DecodePeerMessage()
	if err != nil {
		return Receipt{}, err
	}
	if peer.Sender != sender || peer.Target != string(s.ID) || peer.Handle != handle {
		return Receipt{}, fmt.Errorf("peer channel identity mismatch")
	}
	return s.submit(ctx, generation, input)
}

// SubmitOperation commits an inert host command with caller-stable input and
// operation identities. The Coordinator dispatches only after AppendInput.
func (s *Session) SubmitOperation(ctx context.Context, generation string, inputID inbox.ID, id operation.ID, name string, spec operation.Spec) (Receipt, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return Receipt{}, err
	}
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(id), ToolName: name, Spec: data}})
	if err != nil {
		return Receipt{}, err
	}
	return s.Submit(ctx, generation, inbox.Input{ID: inputID, Kind: inbox.InputControl, Payload: payload})
}
