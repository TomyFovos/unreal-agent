package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

type bridgeReply struct {
	outcomes []llm.ToolOutcome
	err      error
	batch    llm.ToolBatchContinuation
}
type bridgeRequest struct {
	ctx                context.Context
	origin             session.TurnID
	response           llm.Response
	reply              chan bridgeReply
	begin              bool
	waitingForChildren bool
	steering           bool
	mainBatch          bool
	inputTokens        int64
}

func (current *coordinator) steeringRendezvous(ctx context.Context, origin session.TurnID) func(context.Context) ([]llm.BatchInput, error) {
	afterBatch := current.toolBatchRendezvous(ctx, origin, false)
	return func(callCtx context.Context) ([]llm.BatchInput, error) {
		batch, err := afterBatch(callCtx, 0)
		return batch.Inputs, err
	}
}

func (current *coordinator) toolBatchRendezvous(ctx context.Context, origin session.TurnID, mainBatch bool) func(context.Context, int64) (llm.ToolBatchContinuation, error) {
	return func(callCtx context.Context, inputTokens int64) (llm.ToolBatchContinuation, error) {
		request := bridgeRequest{ctx: callCtx, origin: origin, reply: make(chan bridgeReply, 1), steering: true, mainBatch: mainBatch, inputTokens: inputTokens}
		select {
		case current.bridgeRequests <- request:
		case <-ctx.Done():
			return llm.ToolBatchContinuation{}, ctx.Err()
		case <-callCtx.Done():
			return llm.ToolBatchContinuation{}, callCtx.Err()
		}
		select {
		case reply := <-request.reply:
			return reply.batch, reply.err
		case <-ctx.Done():
			return llm.ToolBatchContinuation{}, ctx.Err()
		case <-callCtx.Done():
			return llm.ToolBatchContinuation{}, callCtx.Err()
		}
	}
}

// The next model response acknowledges the watermark of this continuation.
func (current *coordinator) deliverSteering(ctx context.Context, request bridgeRequest) error {
	if request.mainBatch && current.pendingRuntimeChange() {
		// The old client still owns the hook reply. Persist its handoff now and
		// apply the latest selection when it exits, before building the window.
		if err := current.persistContext(ctx, sessionstore.ContextRecord{Kind: sessionstore.ContextSelectionContinuation}); err != nil {
			return err
		}
		request.reply <- bridgeReply{batch: llm.ToolBatchContinuation{RotateWindow: true}}
		return nil
	}
	if b, ok := current.dependencies.ContextBuilder.(interface {
		PrepareToolWindow(context.Context, func(sessionstore.ContextRecord) error, int64, func() error) (bool, error)
	}); ok && request.inputTokens > 0 {
		rotated, err := b.PrepareToolWindow(ctx, func(r sessionstore.ContextRecord) error {
			return current.persistContext(ctx, r)
		}, request.inputTokens, func() error {
			if len(current.deferredContext) == 0 {
				return nil
			}
			// Include these inputs in the fresh window's size without delivering
			// them to the private conversation about to stop.
			return current.persistContext(ctx, sessionstore.ContextRecord{Kind: "deferred_inputs"})
		})
		if err != nil {
			if contextengine.IsBudgetError(err) {
				request.reply <- bridgeReply{err: err}
				current.interruptModel()
				return current.failModelTurn(ctx, "", fmt.Errorf("rebuild context after tool batch: %w", err))
			}
			return err
		}
		if rotated {
			// Pending inbox inputs go to the fresh request. The old conversation
			// will stop before it can read any additional context.
			request.reply <- bridgeReply{batch: llm.ToolBatchContinuation{RotateWindow: true}}
			return nil
		}
	}
	queued := len(current.deferredContext)
	inputs, records, err := current.projectDeferredContext(true)
	if err != nil {
		return err
	}
	if len(inputs) > 0 {
		current.modelInputs += queued
		current.turnExternals = current.state.externalInputs
		if err := current.commitBridgeContinuation(ctx, true, records); err != nil {
			return err
		}
	}
	request.reply <- bridgeReply{batch: llm.ToolBatchContinuation{Inputs: inputs}}
	return nil
}

func safeSteeringText(text string) string {
	text = secretguard.Redact(text)
	return secretguard.Redact(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, text))
}

func (current *coordinator) beginToolRendezvous(ctx context.Context, origin session.TurnID) func(context.Context, bool) error {
	return func(callCtx context.Context, waitingForChildren bool) error {
		request := bridgeRequest{ctx: callCtx, origin: origin, reply: make(chan bridgeReply, 1), begin: true, waitingForChildren: waitingForChildren}
		select {
		case current.bridgeRequests <- request:
		case <-ctx.Done():
			return ctx.Err()
		case <-callCtx.Done():
			return callCtx.Err()
		}
		select {
		case reply := <-request.reply:
			return reply.err
		case <-ctx.Done():
			return ctx.Err()
		case <-callCtx.Done():
			return callCtx.Err()
		}
	}
}

type bridgeRound struct {
	request bridgeRequest
	turn    session.TurnID
	calls   []string
	results map[string]llm.ToolOutcome
}

type deferredInput struct {
	sessionstore.Item
	peer inbox.PeerMessage
}

// Project in inbox order at a tool-batch or regular request boundary. A ready
// notice contributes to scheduling only and has no model-visible text.
func (current *coordinator) projectDeferredContext(batch bool) ([]llm.BatchInput, []record.Locator, error) {
	var inputs []llm.BatchInput
	var records []record.Locator
	var peers, quiet int
	for _, queued := range current.deferredContext {
		item := queued.Item
		if b, ok := current.dependencies.ContextBuilder.(interface{ SetHistoryItem(sessionstore.Item) }); ok {
			b.SetHistoryItem(item)
		}
		input := item.Data.(inbox.Input)
		retain := func(kind record.Kind) {
			if item.Sequence != 0 {
				records = append(records, record.Input(string(current.dependencies.SessionID), uint64(item.Sequence), kind))
			}
		}
		if input.Kind == inbox.InputPeer {
			text, err := current.projectPeerInput(input, queued.peer, batch)
			if err != nil {
				return nil, nil, err
			}
			if !batch {
				continue
			}
			if text != "" {
				inputs = append(inputs, llm.BatchInput{Text: secretguard.Redact(text), Peer: true})
				retain(record.PeerMessage)
			}
			peers++
			if quietPeerKind(queued.peer.Kind) {
				quiet++
			}
			continue
		}
		if err := current.dependencies.ContextBuilder.AddExternalInput(input); err != nil {
			return nil, nil, err
		}
		if batch {
			var text string
			if err := json.Unmarshal(input.Payload, &text); err != nil {
				return nil, nil, err
			}
			inputs = append(inputs, llm.BatchInput{Text: safeSteeringText(text)})
			retain(record.UserMessage)
		}
	}
	if !batch || len(inputs) > 0 {
		current.deferredContext = nil
	}
	if len(inputs) > 0 {
		current.state.turnPeerInputs += peers
		current.state.turnQuietInputs += quiet
	}
	return inputs, records, nil
}

// This rendezvous is provider neutral. Only the Coordinator goroutine mutates
// history/Operations; a protocol callback cannot run a Translator or Executor.
func (current *coordinator) toolRendezvous(ctx context.Context, origin session.TurnID) func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
	return func(callCtx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		request := bridgeRequest{ctx: callCtx, origin: origin, response: response, reply: make(chan bridgeReply, 1)}
		select {
		case current.bridgeRequests <- request:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-callCtx.Done():
			return nil, callCtx.Err()
		}
		select {
		case reply := <-request.reply:
			return reply.outcomes, reply.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-callCtx.Done():
			return nil, callCtx.Err()
		}
	}
}

func (current *coordinator) acceptBridgeRequest(ctx context.Context, request bridgeRequest) error {
	if request.ctx.Err() != nil || current.cancelModel == nil || request.origin != current.modelOrigin || current.bridge != nil {
		request.reply <- bridgeReply{err: errors.New("inactive tool rendezvous")}
		return nil
	}
	if request.begin {
		current.bridgeActive = true
		current.waitingForChildren = request.waitingForChildren
		if (request.waitingForChildren || current.backgroundTurn) && current.state.externalInputs > current.turnExternals {
			current.interruptModel()
			request.reply <- bridgeReply{err: context.Canceled}
			return nil
		}
		request.reply <- bridgeReply{}
		return nil
	}
	if request.steering {
		if err := current.drainInbox(ctx); err != nil {
			return err
		}
		if current.cancelModel == nil || current.stop.request.Mode == inbox.StopHard || request.ctx.Err() != nil {
			request.reply <- bridgeReply{err: context.Canceled}
			return nil
		}
		if err := current.recordChildCompletions(ctx); err != nil {
			return err
		}
		return current.deliverSteering(ctx, request)
	}
	round := &bridgeRound{request: request, turn: current.state.currentTurnID, results: map[string]llm.ToolOutcome{}}
	seen := map[string]bool{}
	for _, item := range request.response.Output {
		switch item.Type {
		case llm.ItemMessage:
			m, ok := item.Data.(llm.Message)
			if !ok || m.Role != llm.RoleAssistant {
				return errors.New("invalid public tool rendezvous response")
			}
		case llm.ItemToolCall:
			call, ok := item.Data.(llm.ToolCall)
			if !ok || call.CallID == "" {
				request.reply <- bridgeReply{err: llm.ErrToolIdentityConflict}
				return nil
			}
			if seen[call.CallID] {
				request.reply <- bridgeReply{err: llm.ErrToolIdentityConflict}
				return nil
			}
			seen[call.CallID] = true
			round.calls = append(round.calls, call.CallID)
		default:
			return errors.New("private or unsupported tool rendezvous output")
		}
	}
	if len(round.calls) == 0 {
		return errors.New("empty tool rendezvous")
	}
	if err := current.clearUsageWait(ctx); err != nil {
		return err
	}
	current.bridge, current.bridgeActive = round, true
	if _, err := current.handleModelResponse(ctx, sessionstore.ModelResponse{TurnID: round.turn, Response: request.response}); err != nil {
		if errors.Is(err, llm.ErrToolIdentityConflict) || errors.Is(err, llm.ErrToolRecoveryRequired) {
			current.bridge = nil
			request.reply <- bridgeReply{err: err}
			return nil
		}
		return err
	}
	current.state.callModel = false
	return current.dispatchOperationsToManager(ctx)
}

func (current *coordinator) rememberBridgeResult(status sessionstore.ToolCallStatus, result llm.ToolResult, translator tool.ResultTranslator) {
	if current.bridge == nil {
		return // no active callback consumer
	}
	failed := status.Status.Error != ""
	if classifier, ok := translator.(tool.ResultFailureClassifier); ok {
		failed = failed || classifier.ResultFailed(status.Status, status.Operations)
	}
	for _, op := range status.Operations {
		failed = failed || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled
		if op.Type == operation.TypeShell {
			state, err := operation.DecodeShellState(op)
			failed = failed || err != nil || state.Result != nil && state.Result.ExitCode != 0
		}
	}
	outcome := llm.ToolOutcome{Result: result, Failed: failed}
	if current.bridge != nil && current.bridge.turn == status.TurnID {
		current.bridge.results[status.CallID] = outcome
	}
}

func (current *coordinator) deliverBridgeResults(ctx context.Context) error {
	round := current.bridge
	if round == nil || len(round.results) != len(round.calls) {
		return nil
	}
	outcomes := make([]llm.ToolOutcome, 0, len(round.calls))
	for _, id := range round.calls {
		outcomes = append(outcomes, round.results[id])
	}
	current.modelInputs += len(outcomes)
	if err := current.commitBridgeContinuation(ctx, false, nil); err != nil {
		return err
	}
	current.bridge = nil
	round.request.reply <- bridgeReply{outcomes: outcomes}
	return nil
}

func (current *coordinator) commitBridgeContinuation(ctx context.Context, batchInputs bool, records []record.Locator) error {
	turn := session.Turn{
		ID: session.TurnID(uuid.New().String()), PreviousTurnID: current.state.currentTurnID,
		Type: session.TurnRegular, RuntimeRevision: current.modelRevision,
		ToolContinuation: current.modelOrigin, InputWatermark: current.modelInputs,
		ExternalInputWatermark: current.turnExternals, BatchInputs: batchInputs, BatchRecords: records,
	}
	item, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: turn})
	if err != nil {
		return err
	}
	if err = current.storeItemInSessionStore(ctx, item); err != nil {
		return err
	}
	current.state.callModel = false
	return nil
}
