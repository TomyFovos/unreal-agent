package host

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"strings"
	"unicode/utf8"
)

type sessionContextKey struct{}

func (s *Session) commitTextFinish(ctx context.Context, turn session.TurnID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle != "child" {
		return fmt.Errorf("text completion requires child lifecycle")
	}
	if s.finish != nil {
		return nil
	}
	for _, op := range s.operations {
		if !terminal(op.Status) {
			return fmt.Errorf("text completion requires no unfinished operations")
		}
	}
	for i := len(s.items) - 1; i >= 0; i-- {
		r, ok := s.items[i].Data.(sessionstore.ModelResponse)
		if !ok || r.TurnID != turn {
			continue
		}
		if r.Response.Failure != nil {
			return fmt.Errorf("text child response failed")
		}
		var text []string
		for _, out := range r.Response.Output {
			if out.Type == llm.ItemToolCall {
				return fmt.Errorf("text child tools unsupported")
			}
			if m, ok := out.Data.(llm.Message); ok && m.Role == llm.RoleAssistant {
				text = append(text, m.Text)
			}
		}
		summary := strings.TrimSpace(strings.Join(text, "\n"))
		if summary == "" {
			summary = "Text-only child completed without text output"
		}
		if len(summary) > 32768 {
			summary = summary[:32768]
			for !utf8.ValidString(summary) {
				summary = summary[:len(summary)-1]
			}
		}
		result := sessionstore.FinishResult{Status: "completed", Summary: summary}
		switch r.Response.Stop {
		case llm.StopRefused:
			result.Status, result.Blockers = "failed", []string{"model response refused"}
		case llm.StopMaxOutputTokens:
			result.Status, result.Blockers = "failed", []string{"model response reached its output limit"}
		}
		f := sessionstore.FinishRecord{Version: 1, ModelTurnID: turn, Result: result}
		return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &f})
	}
	return fmt.Errorf("text child response is not canonical")
}

// SessionFromContext is available to the Host runtime factory and its handlers.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(*Session)
	return s, ok
}

// WithExecutionPolicy scopes authorization to the actual owning Session; a
// control/extension context cannot widen its permissions.
func (s *Session) WithExecutionPolicy(ctx context.Context) context.Context {
	return permission.WithPolicy(ctx, permission.FromContext(s.ctx))
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

// OperationIntent returns a copy of a caller-owned durable command for retries.
// It has no execution effects and does not replace its runtime on replay.
func (s *Session) OperationIntent(id inbox.ID) (inbox.OperationIntent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.submissions[id]
	if p == nil {
		return inbox.OperationIntent{}, false
	}
	r, e := p.input.DecodeControlMessage()
	if e != nil || r.Mode != inbox.DispatchOperation {
		return inbox.OperationIntent{}, false
	}
	v := r.Parameters.(inbox.OperationIntent)
	v.Spec = append(v.Spec[:0:0], v.Spec...)
	return v, true
}
