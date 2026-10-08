package coordinator

import (
	"context"
	"errors"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type bridgeReply struct {
	outcomes []llm.ToolOutcome
	request  llm.Request
	budget   int64
	err      error
}
type bridgeRequest struct {
	ctx      context.Context
	origin   session.TurnID
	response llm.Response
	reply    chan bridgeReply
	begin    bool
	refresh  bool
	reserve  int64
}

func (current *coordinator) refreshToolContext(ctx context.Context, origin session.TurnID) func(context.Context, int64) (llm.Request, int64, error) {
	return func(callCtx context.Context, reserve int64) (llm.Request, int64, error) {
		request := bridgeRequest{ctx: callCtx, origin: origin, reply: make(chan bridgeReply, 1), refresh: true, reserve: reserve}
		select {
		case current.bridgeRequests <- request:
		case <-ctx.Done():
			return llm.Request{}, 0, ctx.Err()
		case <-callCtx.Done():
			return llm.Request{}, 0, callCtx.Err()
		}
		select {
		case reply := <-request.reply:
			return reply.request, reply.budget, reply.err
		case <-ctx.Done():
			return llm.Request{}, 0, ctx.Err()
		case <-callCtx.Done():
			return llm.Request{}, 0, callCtx.Err()
		}
	}
}

func (current *coordinator) beginToolRendezvous(ctx context.Context, origin session.TurnID) func(context.Context) error {
	return func(callCtx context.Context) error {
		request := bridgeRequest{ctx: callCtx, origin: origin, reply: make(chan bridgeReply, 1), begin: true}
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

// A disposable replay index rebuilt from public canonical ToolCalls/receipts.
// It is not a second operation history and never authorizes re-execution.
type canonicalBridgeCall struct {
	call    llm.ToolCall
	outcome *llm.ToolOutcome
}

func (current *coordinator) flushDeferredContext() error {
	for _, item := range current.deferredContext {
		if b, ok := current.dependencies.ContextBuilder.(interface{ SetHistoryItem(sessionstore.Item) }); ok {
			b.SetHistoryItem(item)
		}
		input := item.Data.(inbox.Input)
		var err error
		if input.Kind == inbox.InputExternal {
			err = current.dependencies.ContextBuilder.AddExternalInput(input)
		} else {
			err = current.dependencies.ContextBuilder.(interface{ AddPeerInput(inbox.Input) error }).AddPeerInput(input)
		}
		if err != nil {
			return err
		}
	}
	current.deferredContext = nil
	return nil
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
		request.reply <- bridgeReply{}
		return nil
	}
	if request.refresh {
		var built contextbuilder.Result
		var err error
		if b, ok := current.dependencies.ContextBuilder.(interface {
			BuildWithReserve(int64) (contextbuilder.Result, error)
		}); ok {
			built, err = b.BuildWithReserve(request.reserve)
		} else {
			built, err = current.dependencies.ContextBuilder.Build()
		}
		reply := bridgeReply{request: built.Request, err: err}
		if built.Report.Context != nil {
			reply.budget = built.Report.Context.Budget.Input
			if current.dependencies.ContextBuilt != nil {
				current.dependencies.ContextBuilt(*built.Report.Context)
			}
		}
		request.reply <- reply
		return nil
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
			if !ok || call.CallID == "" || seen[call.CallID] {
				return errors.New("invalid tool rendezvous identity")
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
	if len(request.response.Output) == 1 && len(round.calls) == 1 {
		call := request.response.Output[0].Data.(llm.ToolCall)
		if prior := current.structuredCalls[call.CallID]; prior != nil {
			if prior.call.Name != call.Name || prior.call.Arguments != call.Arguments {
				request.reply <- bridgeReply{err: llm.ErrToolIdentityConflict}
				return nil
			}
			if prior.outcome == nil {
				request.reply <- bridgeReply{err: llm.ErrToolRecoveryRequired}
				return nil
			}
			request.reply <- bridgeReply{outcomes: []llm.ToolOutcome{*prior.outcome}}
			return nil
		}
	}
	current.bridge, current.bridgeActive = round, true
	if _, err := current.handleModelResponse(ctx, sessionstore.ModelResponse{TurnID: round.turn, Response: request.response}); err != nil {
		return err
	}
	current.state.selectionBoundary, current.state.callModel = false, false
	return current.dispatchOperationsToManager(ctx)
}

func (current *coordinator) rememberBridgeResult(status sessionstore.ToolCallStatus, result llm.ToolResult, translator tool.ResultTranslator) {
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
	if prior := current.structuredCalls[status.CallID]; prior != nil {
		prior.outcome = &outcome
	}
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
	turn := session.Turn{
		ID: session.TurnID(uuid.New().String()), PreviousTurnID: current.state.currentTurnID,
		Type: session.TurnRegular, RuntimeRevision: current.modelRevision,
		ToolContinuation: current.modelOrigin, InputWatermark: current.modelInputs,
	}
	item, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: turn})
	if err != nil {
		return err
	}
	if err = current.storeItemInSessionStore(ctx, item); err != nil {
		return err
	}
	current.state.callModel = false
	current.bridge = nil
	round.request.reply <- bridgeReply{outcomes: outcomes}
	return nil
}
