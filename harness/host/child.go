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
)

type sessionContextKey struct{}

// commitUnreportedFinish closes a child after work and waking inputs drain.
func (s *Session) commitUnreportedFinish(ctx context.Context, turn session.TurnID, reportFromReply bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finish != nil || turn == "" || s.awaitingParentReply() {
		return nil
	}
	for i := len(s.items) - 1; i >= 0; i-- {
		r, ok := s.items[i].Data.(sessionstore.ModelResponse)
		if !ok || r.TurnID != turn {
			continue
		}
		if reportFromReply {
			if result, ok := childReplyReport(r.Response); ok {
				f := sessionstore.FinishRecord{Version: 1, ModelTurnID: turn, Result: result}
				return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &f})
			}
		}
		reason := "child ended its turn without Finish"
		if r.Response.Stop != "" {
			reason += " (stop=" + string(r.Response.Stop) + ")"
		}
		if r.Response.Failure != nil {
			reason += ": " + r.Response.Failure.Code
		}
		f := sessionstore.FinishRecord{Version: 1, ModelTurnID: turn, Result: sessionstore.FinishResult{Status: "failed", Summary: reason, Blockers: []string{reason}}}
		return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &f})
	}
	return nil // No canonical response yet, including an explicit user interrupt.
}

func childReplyReport(response llm.Response) (sessionstore.FinishResult, bool) {
	if response.Stop != llm.StopComplete || response.Failure != nil {
		return sessionstore.FinishResult{}, false
	}
	var parts []string
	for _, out := range response.Output {
		if out.Type == llm.ItemToolCall {
			return sessionstore.FinishResult{}, false
		}
		if m, ok := out.Data.(llm.Message); ok && m.Role == llm.RoleAssistant {
			parts = append(parts, m.Text)
		}
	}
	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if summary == "" {
		return sessionstore.FinishResult{}, false
	}
	result := sessionstore.FinishResult{Status: "completed", Summary: summary}
	line, _, _ := strings.Cut(summary, "\n")
	if strings.EqualFold(strings.TrimSpace(line), "Status: failed") {
		result.Status = "failed"
	}
	return result, true
}

// Only SendParent's decision/question kind establishes a reply wait. Progress
// does not. Compare the first durable request with incoming parent messages;
// an answer can arrive before the sending Operation's terminal checkpoint.
// Caller holds s.mu; no text is interpreted as a scheduling instruction.
func (s *Session) awaitingParentReply() bool {
	var lineage struct{ ParentID string }
	_ = json.Unmarshal(s.historyState.Configuration, &lineage)
	seen := map[operation.ID]bool{}
	waiting := false
	request := func(op operation.Operation) {
		if seen[op.ID] || op.Type != operation.TypeRemoteJob {
			return
		}
		seen[op.ID] = true
		latest, ok := s.operations[op.ID]
		if !ok || latest.Status != operation.StatusCompleted {
			return
		}
		state, err := operation.DecodeRemoteJobState(latest)
		var plan struct {
			Action   string
			Progress bool
		}
		if err == nil && state.Plan.Type == "subagent" && json.Unmarshal(state.Plan.Data, &plan) == nil && plan.Action == "parent" && !plan.Progress {
			var receipt struct {
				Answer string `json:"answer"`
			}
			if json.Unmarshal(state.Handle, &receipt) == nil && receipt.Answer != "" {
				return // The SendParent receipt already answered this question.
			}
			waiting = true
		}
	}
	for _, item := range s.items {
		switch d := item.Data.(type) {
		case inbox.Input:
			if d.Kind == inbox.InputPeer {
				if peer, err := d.DecodePeerMessage(); err == nil && peer.Kind == "message" && peer.Sender == lineage.ParentID {
					waiting = false
				}
			}
			if d.Kind == inbox.InputExternal {
				waiting = false
			}
			if d.Kind == inbox.InputControl {
				if op, err := sessionstore.DecodeOperationIntent(d); err == nil {
					request(op)
				}
			}
		case sessionstore.ToolCallStatus:
			for _, op := range d.Operations {
				request(op)
			}
		}
	}
	return waiting
}

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
			if call, ok := out.Data.(llm.ToolCall); ok && !call.Incomplete {
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

// WithCapabilityPolicy bounds ctx by what this Session may ever do: the
// operator maximum when permission modes are on, else its fixed policy.
func (s *Session) WithCapabilityPolicy(ctx context.Context) context.Context {
	if c := permission.ControllerFromContext(s.ctx); c != nil {
		// Bind every occurrence of this owner's controller, including one
		// already present in ctx. Leaving that leaf in its current mode would
		// incorrectly narrow preflight to the Ask/Read Only baseline.
		return c.MaximumContext(permission.WithPolicy(ctx, permission.FromContext(s.ctx)))
	}
	return s.WithExecutionPolicy(ctx)
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

// HasCommittedInput reads the canonical input identity index without replaying
// history. It is used to deduplicate lifecycle notices across Host restarts.
func (s *Session) HasCommittedInput(id inbox.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submissions[id] != nil && s.submissions[id].committed
}

// HasChildReport checks canonical parent delivery, including legacy batches
// whose input identity names another child. It never scans/clones the history.
func (s *Session) HasChildReport(id operation.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childReports.Has(id)
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
	return s.cancelOperation(ctx, generation, inputID, id, "user")
}

func (s *Session) CancelChildOperation(ctx context.Context, generation string, inputID inbox.ID, id operation.ID) (Receipt, error) {
	return s.cancelOperation(ctx, generation, inputID, id, "coordinator")
}

func (s *Session) cancelOperation(ctx context.Context, generation string, inputID inbox.ID, id operation.ID, by string) (Receipt, error) {
	data, err := json.Marshal(inbox.ControlMessage{Mode: inbox.CancelOperation, Parameters: inbox.CancelRequest{OperationID: string(id), By: by}})
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
