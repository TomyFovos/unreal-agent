package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

var errConversationRecoveryLimit = errors.New("model conversation recovery limit reached")

// All fields are disposable derivatives of canonical records. No permission,
// tool arguments, private thinking or receipt bodies are duplicated here.
type conversationState struct {
	epoch        sessionstore.ConversationEpoch
	attempts     uint32
	input        string
	retired      map[toolCallKey]bool
	quarantined  map[operation.ID]bool
	needsEpoch   bool
	appendFailed bool
	pending      map[operation.ID]bool
	overflow     bool
	effects      map[operation.ID]jsontext.Value
}

type conversationBuilder interface {
	PrepareConversationSnapshot() (string, error)
	ApplyConversationEpoch(sessionstore.ConversationEpoch) error
}

// nativeProviderID is the removed Claude Native route; Sessions recorded with
// it still replay their conversation epochs.
const nativeProviderID = "claude-native"

func (current *coordinator) nativeConversation() bool {
	return llm.RecoveryProvider(current.dependencies.LLM) == nativeProviderID
}

func (current *coordinator) conversationState() *conversationState {
	if current.conversation == nil {
		current.conversation = &conversationState{retired: map[toolCallKey]bool{}, quarantined: map[operation.ID]bool{}, pending: map[operation.ID]bool{}, effects: map[operation.ID]jsontext.Value{}}
	}
	return current.conversation
}

// First activation after a legacy runtime may expose terminal execution facts
// which predate any Native epoch. This is transition work, never a generation
// history scan, and live calls/approval waits remain owned by normal execution.
func (current *coordinator) activateNativeConversation() error {
	if !current.nativeConversation() || current.conversation != nil {
		return nil
	}
	state := current.conversationState()
	budget := 32 << 20
	for _, op := range current.state.operations {
		if !operationIsTerminal(op.Status) {
			continue
		}
		if len(op.State) > budget {
			current.markUncertain(op.ID)
		} else {
			budget -= len(op.State)
			current.observeUncertainty(op, true)
		}
		if state.overflow {
			return llm.ErrToolRecoveryRequired
		}
	}
	return nil
}

func (current *coordinator) modelToolCalls() int {
	if current.conversation == nil || len(current.conversation.retired) == 0 {
		return len(current.state.toolCalls)
	}
	n := 0
	for key := range current.state.toolCalls {
		if !current.conversation.retired[key] {
			n++
		}
	}
	return n
}

func (current *coordinator) quarantined(id operation.ID) bool {
	return current.conversation != nil && current.conversation.quarantined[id]
}

func (current *coordinator) retiredCall(key toolCallKey) bool {
	return current.conversation != nil && current.conversation.retired[key]
}

func (current *coordinator) applyConversationEpoch(epoch sessionstore.ConversationEpoch) error {
	if err := epoch.Validate(); err != nil {
		return err
	}
	builder, ok := current.dependencies.ContextBuilder.(conversationBuilder)
	if !ok {
		return llm.ErrToolRecoveryRequired
	}
	state := current.conversationState()
	if epoch.Number != state.epoch.Number+1 {
		return llm.ErrToolRecoveryRequired
	}
	if err := builder.ApplyConversationEpoch(epoch); err != nil {
		return err
	}
	state.epoch, state.attempts, state.input = epoch, epoch.Attempt, epoch.InputID
	state.needsEpoch = false
	state.quarantined = make(map[operation.ID]bool, len(epoch.Quarantined))
	state.effects = make(map[operation.ID]jsontext.Value, len(epoch.UncertainEffects))
	for _, effect := range epoch.UncertainEffects {
		state.effects[effect.ID] = effect.Basis.Clone()
	}
	for _, id := range epoch.Quarantined {
		state.quarantined[id] = true
	}
	if epoch.Reason != "strict_restore" {
		// Retire the old *wire batch*, never its canonical request or Operations.
		// Its eventual terminal facts may still be appended by reconciliation.
		for key := range current.state.toolCalls {
			state.retired[key] = true
		}
		current.clearToolGrace()
	}
	// A committed reconstruction decision is evidence of the specific typed
	// continuity failure, rather than an unanswered transport outcome. Keep the
	// existing bounded recovery contract, including a crash after this record.
	// Quarantine decisions cannot discharge an unknown model request.
	if (epoch.Reason == "continuity" || epoch.Reason == "strict_restore") && epoch.TurnID == current.state.replayTurn {
		current.state.replayTurn = ""
		current.state.replayNotified = false
	}
	current.state.callModel = true
	return nil
}

func (current *coordinator) appendConversationEpoch(ctx context.Context, reason string) error {
	builder, ok := current.dependencies.ContextBuilder.(conversationBuilder)
	if !ok {
		return llm.ErrToolRecoveryRequired
	}
	state := current.conversationState()
	if state.appendFailed || state.overflow {
		return llm.ErrToolRecoveryRequired
	}
	input := current.actionScope
	if input == "" {
		input = "recovery"
	}
	// Only a durable successful ModelResponse resets consecutive attempts.
	// New peer/input IDs cannot manufacture an unlimited retry budget.
	if state.attempts >= 3 {
		return errConversationRecoveryLimit
	}
	current.drainUncertainty()
	if err := current.conversationLimitError(); err != nil {
		return err
	}
	snapshot, err := builder.PrepareConversationSnapshot()
	if err != nil {
		return err
	}
	ids := make([]operation.ID, 0, len(state.quarantined))
	for id := range state.quarantined {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) > 128 || len(current.state.toolCalls) > 128 {
		return llm.ErrToolRecoveryRequired
	}
	if len(ids) != 0 {
		var note strings.Builder
		note.WriteString("\nUncertain canonical Operations: outcomes are NOT confirmed; do not retry these effects under new IDs. Only Host-proven independent work may proceed; uncertain outcomes must not be used as prerequisites.\n")
		for _, id := range ids {
			fmt.Fprintf(&note, "Operation %s: unresolved outcome.\n", id)
		}
		// The producer reserves the other half of the bound for these owner facts.
		if len(snapshot)+note.Len() > sessionstore.MaxConversationSnapshotBytes {
			return llm.ErrToolRecoveryRequired
		}
		snapshot += note.String()
	}
	epoch := sessionstore.ConversationEpoch{Number: state.epoch.Number + 1, Provider: nativeProviderID, TurnID: current.state.currentTurnID, InputID: input, Attempt: state.attempts + 1, Reason: reason, Snapshot: snapshot, Quarantined: ids}
	for _, id := range ids {
		if basis := state.effects[id]; len(basis) != 0 {
			epoch.UncertainEffects = append(epoch.UncertainEffects, operation.UncertainEffect{ID: id, Basis: basis.Clone()})
		}
	}
	record := sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostConversationEpoch, Conversation: &epoch}
	if err := record.Validate(); err != nil {
		return err
	}
	writer, ok := current.dependencies.Sessions.(interface {
		AppendHostRecord(context.Context, session.ID, sessionstore.HostRecord) error
	})
	if !ok {
		return llm.ErrToolRecoveryRequired
	}
	// This is the acceptance point. An ambiguous append poisons this owner;
	// only a fresh writer may discover whether the record was committed.
	if err := writer.AppendHostRecord(ctx, current.dependencies.SessionID, record); err != nil {
		state.appendFailed = true
		return fmt.Errorf("persist model conversation decision: %w", err)
	}
	return current.applyConversationEpoch(epoch)
}

func (current *coordinator) recoverConversation(ctx context.Context, cause error) (bool, error) {
	if !current.nativeConversation() || (!llm.NeedsConversationRecovery(cause) && !errors.Is(cause, llm.ErrToolRecoveryRequired)) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	reason := "continuity"
	// Stage two (official drop) is unavailable until model/beta/auth-route
	// capability is established. Never guess it from a model name or HTTP400.
	return true, current.appendConversationEpoch(ctx, reason)
}

func (current *coordinator) observeUncertainty(op operation.Operation, restored bool) {
	if (!current.nativeConversation() && current.conversation == nil) || (!restored && !operationIsTerminal(op.Status)) {
		return
	}
	uncertain := operationUncertain(op)
	if !uncertain {
		if current.conversation != nil {
			delete(current.conversation.quarantined, op.ID)
			delete(current.conversation.effects, op.ID)
		}
		return
	}
	state := current.conversationState()
	if !state.quarantined[op.ID] {
		current.markUncertain(op.ID)
	}
	// Restore can first see an unprepared proposal, then its later durable
	// preparation checkpoint. Refresh only from this canonical Operation,
	// never from a model plan or a speculative worker result.
	delete(state.effects, op.ID)
	if len(op.ExecutionBasis()) > 0 && len(op.ExecutionBasis()) <= 256<<10 {
		state.effects[op.ID] = op.ExecutionBasis().Clone()
	}
}

func (current *coordinator) markUncertain(id operation.ID) {
	state := current.conversationState()
	state.needsEpoch = true
	if !state.quarantined[id] && len(state.quarantined) >= 128 {
		// Poison this owner rather than grow an unbounded secondary index or
		// discard an unknown effect and continue. Canonical evidence is intact.
		state.overflow = true
		return
	}
	state.quarantined[id] = true
	if state.effects == nil {
		state.effects = map[operation.ID]jsontext.Value{}
	}
	if op, ok := current.state.operations[id]; ok && len(op.ExecutionBasis()) > 0 && len(op.ExecutionBasis()) <= 256<<10 {
		state.effects[id] = op.ExecutionBasis().Clone()
	}
}

func (current *coordinator) conversationLimitError() error {
	if current.conversation != nil && current.conversation.overflow {
		return llm.ErrToolRecoveryRequired
	}
	return nil
}

// queueUncertainty retains IDs, not result bodies. Successful native Tool
// Results already receive the strict Translator proof; unbound/unproven results
// are inspected lazily before another effect or conversation recovery.
func (current *coordinator) queueUncertainty(op operation.Operation) {
	if (!current.nativeConversation() && current.conversation == nil) || !operationIsTerminal(op.Status) {
		return
	}
	state := current.conversationState()
	if len(state.pending) >= 128 && !state.pending[op.ID] {
		for id := range state.pending {
			current.inspectPendingUncertainty(id)
			break
		}
	}
	state.pending[op.ID] = true
}

func (current *coordinator) inspectPendingUncertainty(id operation.ID) {
	state := current.conversation
	if state == nil {
		return
	}
	delete(state.pending, id)
	op, exists := current.state.operations[id]
	if !exists {
		op = operation.Operation{ID: id}
	}
	current.observeUncertainty(op, true)
}

func (current *coordinator) drainUncertainty() {
	if current.conversation == nil {
		return
	}
	budget := 32 << 20
	for id := range current.conversation.pending {
		op, exists := current.state.operations[id]
		if !exists || len(op.State) > budget {
			// A bounded owner must not synchronously decode an arbitrary aggregate.
			// Unexamined facts stay unknown, never silently successful.
			delete(current.conversation.pending, id)
			current.markUncertain(id)
			continue
		}
		budget -= len(op.State)
		current.inspectPendingUncertainty(id)
	}
}

// Legacy internal callers use this only as a closed admission query. Actual
// execution always calls prepareRecoveryOperation with the Operation policy.
func (current *coordinator) quarantineDenial(candidate operation.Operation) *permission.Error {
	_, denial := current.prepareRecoveryOperation(context.Background(), candidate)
	return denial
}
