package coordinator

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const historyPageSize = 256

const (
	slurpIdleTimeout = time.Millisecond
	slurpMaxItems    = 100
)

const toolCallRunGracePeriod = time.Second

const interruptedOperationResult = "中断されたため結果は不明。必要なら状態を確かめてから続けること"

type coordinator struct {
	dependencies   Dependencies
	state          loopState
	stop           stopState
	cancelModel    context.CancelFunc
	pendingCancels map[operation.ID]string
	bridgeRequests chan bridgeRequest
	bridge         *bridgeRound
	bridgeActive   bool
	// backgroundTurn: the model call in flight was started by agents'
	// messages, not by new user input (turnExternals counts those seen).
	backgroundTurn     bool
	waitingForChildren bool
	turnExternals      int
	modelOrigin        session.TurnID
	modelRevision      uint64
	modelInputs        int
	deferredContext    []sessionstore.Item
	contextRetry       bool
	forceCompaction    bool
	restoredInputIDs   map[inbox.ID]bool
	idleRequest        llm.Request
	idleOptions        llm.RequestOptions
	idlePending        bool
	cancelIdle         context.CancelCauseFunc
	idleDone           chan struct{}
	modelRetry         <-chan time.Time
}

type stopState struct {
	request               inbox.ControlMessage
	cancellationRequested bool // Once set, no new nonterminal operations may enter coordinator state.
}

type loopState struct {
	currentTurnID   session.TurnID
	currentTurnType session.TurnType
	toolCalls       map[toolCallKey]toolCallState
	operations      map[operation.ID]operation.Operation
	availableInputs int
	deliveredInputs int
	// quietInputs counts inputs since the latest regular turn began that the
	// next model call consumes but that do not call the model themselves:
	// a child's ready notice or progress update.
	quietInputs       int
	currentTurnInputs int
	callModel         bool
	grace             <-chan time.Time
	graceToolCalls    map[toolCallKey]struct{}
	instructionsBound bool
	selectionBoundary bool
	externalInputs    int

	// interrupted holds the model after /stop main until the user's next
	// input: externalInputs then exceeds interruptExternals.
	interrupted        bool
	interruptExternals int
}

type toolCallState struct {
	toolCall   llm.ToolCall
	status     *tool.CallStatus
	operations map[operation.ID]struct{}
}

type toolCallKey struct {
	turnID session.TurnID
	callID string
}

type toolCallContext struct {
	canFinish  bool
	operations []operation.Operation
}

type modelResponseResult struct {
	turnID   session.TurnID
	response llm.Response
	err      error
}

var _ Coordinator = (*coordinator)(nil)

func newLoopState() loopState {
	return loopState{
		selectionBoundary: true,
		toolCalls:         make(map[toolCallKey]toolCallState),
		operations:        make(map[operation.ID]operation.Operation),
		graceToolCalls:    make(map[toolCallKey]struct{}),
	}
}

func (current *coordinator) Run(ctx context.Context) error {
	if current.finished() {
		return nil
	}
	if current.dependencies.ToolHeartbeatInterval < 0 {
		return fmt.Errorf("tool heartbeat interval must not be negative")
	}
	if b, ok := current.dependencies.ContextBuilder.(interface{ ObserveHistory(sessionstore.Item) }); ok {
		id := current.dependencies.Sessions.AddObserver(func(id session.ID, item sessionstore.Item) {
			if id == current.dependencies.SessionID {
				b.ObserveHistory(item)
			}
		})
		defer current.dependencies.Sessions.RemoveObserver(id)
	}
	if err := current.restore(ctx); err != nil {
		return err
	}

	return current.runLoop(ctx)
}

func (current *coordinator) runLoop(ctx context.Context) error {
	if request := current.dependencies.RestoredStop; request != nil {
		current.acceptStop(*request)
	}
	if err := current.processInputs(ctx, current.dependencies.InitialInputs); err != nil {
		return err
	}
	if current.stop.request.Mode == inbox.StopHard {
		// The session writer lock proves the old owner is gone. Do not dispatch
		// recovered effects merely to cancel them immediately afterwards.
		for _, op := range current.state.operations {
			if operationIsTerminal(op.Status) {
				continue
			}
			canceled, err := operation.CancelUndispatched(op)
			if err != nil {
				return err
			}
			if err = current.handleOperationUpdate(ctx, canceled); err != nil {
				return err
			}
		}
		if _, err := current.scheduleToolCalls(ctx); err != nil {
			return err
		}
		for _, op := range current.state.operations {
			if operationIsTerminal(op.Status) {
				continue
			}
			canceled, err := operation.CancelUndispatched(op)
			if err != nil {
				return err
			}
			if err = current.handleOperationUpdate(ctx, canceled); err != nil {
				return err
			}
		}
		if _, err := current.reconcileToolCalls(ctx); err != nil {
			return err
		}
		return nil
	}
	// No old local executor is owned by this restored Coordinator. Complete
	// accepted targeted cancellations before an initial-phase actor can restart.
	for _, op := range current.state.operations {
		if op.Status != operation.StatusCanceling || op.Type == operation.TypeRemoteJob {
			continue
		}
		canceled, err := operation.CancelUndispatched(op)
		if err != nil {
			return err
		}
		if err = current.handleOperationUpdate(ctx, canceled); err != nil {
			return err
		}
	}
	modelContext, cancelModels := context.WithCancel(ctx)
	defer cancelModels()
	defer current.interruptModel()
	defer current.stopIdle("session_stopped")
	modelResponses := make(chan modelResponseResult)
	current.bridgeRequests = make(chan bridgeRequest)

	inboxOutput := current.dependencies.Inbox.Output()
	operationUpdates := current.dependencies.Operations.Updates()
	statuses, err := current.scheduleToolCalls(ctx)
	if err != nil {
		return err
	}
	if _, err := current.reconcileToolCalls(ctx); err != nil {
		return err
	}
	if err := current.dispatchOperationsToManager(ctx); err != nil {
		return err
	}
	if !current.held() && (current.state.callModel || toolCallStatusesRequireModelResponse(statuses) || current.wakingInputs() > 0) {
		err = current.requestModelResponse(modelContext, modelResponses)
		if err != nil {
			return err
		}
	}

	if current.stop.request.Mode == inbox.StopWhenIdle {
		call, err := current.processEvents(ctx)
		if err != nil {
			return err
		}
		if call {
			if err = current.requestModelResponse(modelContext, modelResponses); err != nil {
				return err
			}
		}
		if current.isIdle() {
			return nil
		}
	}
	var heartbeat <-chan time.Time
	for {
		if !current.isWaitingForOnlyToolCalls() {
			heartbeat = nil
		} else if heartbeat == nil && current.dependencies.ToolHeartbeatInterval > 0 {
			heartbeat = time.After(current.dependencies.ToolHeartbeatInterval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-current.idleDone:
			current.cancelIdle = nil
			current.idleDone = nil

		case <-current.modelRetry:
			current.modelRetry = nil
			current.state.callModel = true

		case received, open := <-inboxOutput:
			if !open {
				return closedInputError(ctx, "inbox output")
			}
			if err := current.processInputs(ctx, []inbox.Input{received}); err != nil {
				return err
			}

		case received, open := <-operationUpdates:
			if !open {
				return closedInputError(ctx, "operation updates")
			}
			if err := current.processOperations(ctx, []operation.Operation{received}); err != nil {
				return err
			}

		case <-heartbeat:
			if err := current.postHeartbeat(ctx); err != nil {
				return err
			}

		case <-current.state.grace:
			current.clearToolGrace()

		case received := <-current.bridgeRequests:
			if err := current.acceptBridgeRequest(ctx, received); err != nil {
				return err
			}

		case received := <-modelResponses:
			if current.cancelModel == nil || received.turnID != current.modelOrigin {
				continue
			}
			received.turnID = current.state.currentTurnID
			if !current.contextRetry && llm.IsContextOverflow(received.err, received.response) {
				if b, ok := current.dependencies.ContextBuilder.(interface{ UsesAppendOnlyContext() bool }); ok && b.UsesAppendOnlyContext() {
					current.contextRetry, current.forceCompaction = true, true
					if err := current.requestModelResponse(modelContext, modelResponses); err != nil {
						return err
					}
					continue
				}
			}
			current.contextRetry = false
			if err := current.processModelResponse(ctx, received); err != nil {
				return err
			}
		}

		callModel, err := current.processEvents(ctx)
		if err != nil {
			return err
		}
		if current.stop.request.Mode == inbox.StopHard {
			stopped, err := current.handleStop(ctx)
			if err != nil {
				return err
			}
			if stopped {
				return ctx.Err()
			}
			continue
		}
		if current.finished() && !current.hasPendingOperations() {
			return nil
		}
		if callModel {
			err = current.requestModelResponse(modelContext, modelResponses)
			if err != nil {
				return err
			}
			current.clearToolGrace()
			// heartbeat is cleared immediately at the top of the loop, because now we're waiting for the model response
		}
		if current.stop.request.Mode == inbox.StopWhenIdle && current.isIdle() {
			return ctx.Err()
		}
		if current.isIdle() && !current.held() && current.stop.request.Mode == "" {
			current.startIdle(modelContext)
		}
	}
}

func (current *coordinator) startIdle(ctx context.Context) {
	if !current.idlePending || current.cancelIdle != nil {
		return
	}
	idle, ok := current.dependencies.LLM.(llm.IdleAdapter)
	if !ok {
		return
	}
	current.idlePending = false
	idleCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	current.cancelIdle, current.idleDone = cancel, done
	request, options := current.idleRequest, current.idleOptions
	go func() {
		defer close(done)
		idle.Idle(idleCtx, request, options)
	}()
}

func (current *coordinator) stopIdle(reason string) {
	if current.cancelIdle != nil {
		current.cancelIdle(errors.New(reason))
		<-current.idleDone
		current.cancelIdle, current.idleDone = nil, nil
	}
}

func (current *coordinator) processEvents(ctx context.Context) (bool, error) {
	inputs, err := slurpChannel(ctx, current.dependencies.Inbox.Output())
	if err != nil {
		return false, fmt.Errorf("slurp inbox: %w", err)
	}
	if err := current.processInputs(ctx, inputs); err != nil {
		return false, err
	}
	updates, err := slurpChannel(ctx, current.dependencies.Operations.Updates())
	if err != nil {
		return false, fmt.Errorf("slurp operation updates: %w", err)
	}
	if err := current.processOperations(ctx, updates); err != nil {
		return false, err
	}

	if _, err := current.reconcileToolCalls(ctx); err != nil {
		return false, err
	}
	if err := current.deliverBridgeResults(ctx); err != nil {
		return false, err
	}
	if current.cancelModel != nil && current.bridgeActive {
		return false, nil
	}
	if llm.RequiresToolBatch(current.dependencies.LLM) && len(current.state.toolCalls) != 0 {
		return false, nil
	}
	if current.held() {
		return false, nil
	}
	// Peer inputs are already durable. Keep accepting inputs while maintenance
	// finishes; only user input or explicit control cancels it.
	if current.idleDone != nil || current.modelRetry != nil {
		return false, nil
	}
	current.state.interrupted = false
	return current.state.callModel || (current.wakingInputs() > 0 && current.cancelModel == nil && len(current.state.graceToolCalls) == 0), nil
}

// held reports whether /stop main keeps the model waiting for the user.
func (current *coordinator) held() bool {
	return current.state.interrupted && current.state.externalInputs == current.state.interruptExternals
}

// interruptAgent ends the agent's current work (/stop main): its model call
// stops and its own operations are canceled, while child agents run on. The
// model is called again for the user's next input, with what arrived since.
func (current *coordinator) interruptAgent(ctx context.Context, reason string) error {
	current.stopIdle("user_interrupt")
	current.modelRetry = nil
	current.interruptModel()
	current.state.interrupted, current.state.interruptExternals = true, current.state.externalInputs
	for id, op := range current.state.operations {
		if operationIsTerminal(op.Status) || subagentOperation(op) {
			continue
		}
		if err := current.cancelOperation(ctx, id, reason); err != nil {
			return err
		}
	}
	return nil
}

// cancelChildren cancels every child agent this Session started (/stop sub).
func (current *coordinator) cancelChildren(ctx context.Context, reason string) error {
	for id, op := range current.state.operations {
		if operationIsTerminal(op.Status) || !subagentOperation(op) {
			continue
		}
		if err := current.cancelOperation(ctx, id, reason); err != nil {
			return err
		}
	}
	return nil
}

func subagentOperation(op operation.Operation) bool {
	if op.Type != operation.TypeRemoteJob {
		return false
	}
	state, err := operation.DecodeRemoteJobState(op)
	return err == nil && state.Plan.Type == "subagent"
}

func (current *coordinator) processInputs(ctx context.Context, inputs []inbox.Input) error {
	return current.handleInboxInputs(ctx, inputs)
}

func (current *coordinator) processOperations(ctx context.Context, updates []operation.Operation) error {
	for _, update := range updates {
		if err := current.handleOperationUpdate(ctx, update); err != nil {
			return err
		}
	}
	return nil
}

func (current *coordinator) processModelResponse(ctx context.Context, modelResponse modelResponseResult) error {
	current.interruptModel()
	if modelResponse.err != nil {

		if err := ctx.Err(); err != nil {
			return err
		}
		var retryable interface{ RetryableModelFailure() bool }
		if current.dependencies.RetryModelFailures && errors.As(modelResponse.err, &retryable) && retryable.RetryableModelFailure() {
			if record := current.dependencies.RecordActivity; record != nil {
				if err := record(ctx, llm.ActivityRecord{Kind: "model_retry", ID: string(modelResponse.turnID), StartedAt: time.Now().UTC(), Reason: modelResponse.err.Error()}); err != nil {
					slog.Warn("record model retry", "error", err)
				}
			}
			// The failed response has not executed tools. Retry from canonical
			// history in this Session; committed Operations are never replayed.
			current.state.callModel = false
			current.modelRetry = time.After(2 * time.Second)
			return nil
		}
		return fmt.Errorf("call model for turn %q: %w", modelResponse.turnID, modelResponse.err)
	}
	statuses, err := current.handleModelResponse(ctx, sessionstore.ModelResponse{
		TurnID:   modelResponse.turnID,
		Response: modelResponse.response,
	})
	if err != nil {
		return err
	}
	for _, status := range statuses {
		for _, value := range status.Operations {
			if err := current.dispatchOperationToManager(ctx, value); err != nil {
				return err
			}
		}
	}
	if !current.state.callModel && len(statuses) > 0 {
		for _, status := range statuses {
			current.state.graceToolCalls[toolCallKey{turnID: status.TurnID, callID: status.CallID}] = struct{}{}
		}
		current.state.grace = time.After(toolCallRunGracePeriod)
	}
	if current.dependencies.AfterResponse != nil {
		if err := current.dependencies.AfterResponse(ctx, modelResponse.turnID); err != nil {
			return err
		}
	}
	current.idlePending = true
	return nil
}

func (current *coordinator) clearToolGrace() {
	clear(current.state.graceToolCalls)
	current.state.grace = nil
}

func (current *coordinator) handleStop(ctx context.Context) (bool, error) {
	if !current.stop.cancellationRequested {
		current.interruptModel()
		if err := current.cancelOperations(ctx); err != nil {
			return false, err
		}
		current.stop.cancellationRequested = true
	}
	return !current.hasPendingOperations(), nil
}

func (current *coordinator) isIdle() bool {
	return current.cancelModel == nil && current.wakingInputs() == 0 &&
		len(current.state.toolCalls) == 0 && !current.hasPendingOperations()
}

func (current *coordinator) isWaitingForOnlyToolCalls() bool {
	return current.cancelModel == nil && current.stop.request.Mode != inbox.StopHard &&
		current.wakingInputs() == 0 && len(current.state.toolCalls) != 0
}

func (current *coordinator) postHeartbeat(ctx context.Context) error {
	calls := make([]llm.ToolCall, 0, len(current.state.toolCalls))
	for _, call := range current.state.toolCalls {
		calls = append(calls, call.toolCall)
	}
	slices.SortFunc(calls, func(a, b llm.ToolCall) int {
		return cmp.Compare(a.CallID, b.CallID)
	})
	runningCalls, err := json.Marshal(calls)
	if err != nil {
		return fmt.Errorf("encode heartbeat tool calls: %w", err)
	}
	payload, err := json.Marshal(inbox.ControlMessage{
		Mode:   inbox.Heartbeat,
		Reason: fmt.Sprintf("Heartbeat: waited %g seconds for tool calls.\nRunning: %s", current.dependencies.ToolHeartbeatInterval.Seconds(), runningCalls),
	})
	if err != nil {
		return fmt.Errorf("encode heartbeat: %w", err)
	}
	return current.dependencies.Inbox.Submit(ctx, inbox.Input{
		ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload,
	})
}

func (current *coordinator) interruptModel() {
	current.bridge, current.bridgeActive = nil, false
	current.waitingForChildren = false
	if current.cancelModel != nil {
		current.cancelModel()
		current.cancelModel = nil
	}
}

func (current *coordinator) acceptStop(request inbox.ControlMessage) {
	if current.stop.request.Mode == inbox.StopHard {
		return
	}
	current.stop.request = request
}

func (current *coordinator) pendingInputs() int {

	return current.state.availableInputs - current.state.deliveredInputs
}

// wakingInputs are the pending inputs that call the model.
func (current *coordinator) wakingInputs() int {
	if current.held() {
		return 0
	}
	return max(current.pendingInputs()-current.state.quietInputs, 0)
}

// quietPeer reports whether a peer input waits for the parent's next turn:
// a child's ready notice or progress update.
func quietPeer(input inbox.Input) bool {
	peer, err := input.DecodePeerMessage()
	return err == nil && (peer.Kind == "ready" || peer.Kind == "progress")
}

func (current *coordinator) hasPendingOperations() bool {
	for _, value := range current.state.operations {
		if !operationIsTerminal(value.Status) {
			return true
		}
	}
	return false
}

func (current *coordinator) cancelOperations(ctx context.Context) error {
	// Commit the whole stop decision before effects. On any storage error no
	// cancellation effect is delivered and recovery can finish the intent.
	for id, value := range current.state.operations {
		if operationIsTerminal(value.Status) || value.Status == operation.StatusCanceling {
			continue
		}
		value.Status = operation.StatusCanceling
		if err := current.storeOperationInSessionStore(ctx, value); err != nil {
			return err
		}
		current.state.operations[id] = value
	}
	var result error
	for id, value := range current.state.operations {
		if operationIsTerminal(value.Status) {
			continue
		}
		if err := current.dependencies.Operations.Cancel(id, current.stop.request.Reason); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (current *coordinator) requestModelResponse(
	ctx context.Context,
	results chan<- modelResponseResult,
) error {
	if current.idleDone != nil {
		return nil
	}
	if current.finished() {
		return nil
	}

	// Reconciliation may discover an uncertain terminal result after the event
	// phase. Persist its projection boundary before applying the batch barrier.

	// Startup/resume can request a generation before processEvents. Apply the
	// same transport barrier here, before context/Turn creation or network I/O.
	if llm.RequiresToolBatch(current.dependencies.LLM) && len(current.state.toolCalls) != 0 {
		return nil
	}
	boundary := current.state.selectionBoundary && current.cancelModel == nil && len(current.state.toolCalls) == 0 && !current.hasPendingOperations()
	if !current.forceCompaction {
		current.contextRetry = false
	}
	current.interruptModel()
	var runtimeRevision uint64
	if current.dependencies.BeforeTurn != nil {
		var err error
		runtimeRevision, err = current.dependencies.BeforeTurn(ctx, boundary)
		if err != nil {
			return err
		}
	}

	if err := current.flushDeferredContext(); err != nil {
		return err
	}
	if b, ok := current.dependencies.ContextBuilder.(interface {
		PrepareContext(context.Context, llm.Adapter, llm.RequestOptions, func(sessionstore.ContextRecord) error, bool) error
	}); ok {
		persist := func(r sessionstore.ContextRecord) error {
			store, ok := current.dependencies.Sessions.(interface {
				AppendHostRecord(context.Context, session.ID, sessionstore.HostRecord) error
			})
			if !ok {
				return fmt.Errorf("session store cannot persist context records")
			}
			record := sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &r}
			if err := store.AppendHostRecord(ctx, current.dependencies.SessionID, record); err != nil {
				return err
			}
			_, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemHostRecord, Data: record})
			return err
		}
		if err := b.PrepareContext(ctx, current.dependencies.LLM, llm.RequestOptions{CacheKey: string(current.dependencies.SessionID)}, persist, current.forceCompaction); err != nil {
			return fmt.Errorf("prepare context: %w", err)
		}
		current.forceCompaction = false
	}
	built, err := current.dependencies.ContextBuilder.Build()
	if err != nil {
		return fmt.Errorf("build model request: %w", err)
	}
	if built.Report.Context != nil && current.dependencies.ContextBuilt != nil {
		current.dependencies.ContextBuilt(*built.Report.Context)
	}
	turn := session.Turn{
		ID:              session.TurnID(uuid.New().String()),
		PreviousTurnID:  current.state.currentTurnID,
		Type:            session.TurnRegular,
		RuntimeRevision: runtimeRevision,
	}

	item, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemTurn,
		Data: turn,
	})
	if err != nil {
		return err
	}
	if err := current.storeItemInSessionStore(ctx, item); err != nil {
		return err
	}

	requestContext, cancel := context.WithCancel(ctx)
	current.cancelModel = cancel
	// A turn no new user input started runs in the background (agents'
	// messages); the user's next input takes over from it.
	current.backgroundTurn = current.state.externalInputs == current.turnExternals
	current.turnExternals = current.state.externalInputs
	current.modelOrigin, current.modelRevision, current.modelInputs = turn.ID, turn.RuntimeRevision, current.state.currentTurnInputs
	current.state.callModel = false
	current.idlePending = false
	current.idleRequest = built.Request
	current.idleOptions = llm.RequestOptions{
		CacheKey: string(current.dependencies.SessionID),
		Identity: llm.RequestIdentity{SessionID: string(current.dependencies.SessionID), TurnID: string(turn.ID), RuntimeRevision: turn.RuntimeRevision},
	}
	go func() {
		response, err := current.dependencies.LLM.Respond(requestContext, built.Request, llm.RequestOptions{
			CacheKey:   string(current.dependencies.SessionID),
			Identity:   llm.RequestIdentity{SessionID: string(current.dependencies.SessionID), TurnID: string(turn.ID), RuntimeRevision: turn.RuntimeRevision},
			Tools:      current.toolRendezvous(requestContext, turn.ID),
			BeginTools: current.beginToolRendezvous(requestContext, turn.ID),
			Steering:   current.steeringRendezvous(requestContext, turn.ID),
		})
		select {
		case results <- modelResponseResult{
			turnID:   turn.ID,
			response: response,
			err:      err,
		}:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (current *coordinator) handleInboxInput(ctx context.Context, input inbox.Input) error {
	if input.Kind == inbox.InputExternal {
		current.stopIdle("user_input")
		current.modelRetry = nil
	}
	item, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemInput,
		Data: input,
	})
	if err != nil {
		return err
	}
	if err := current.storeItemInSessionStore(ctx, item); err != nil {
		return err
	}
	if input.Kind == inbox.InputControl {
		request, err := input.DecodeControlMessage()
		if err != nil {
			return err
		}
		switch request.Mode {
		case inbox.DispatchOperation:
			current.stopIdle("user_operation")
			op, err := sessionstore.DecodeOperationIntent(input)
			if err != nil {
				return err
			}
			return current.dispatchOperationToManager(ctx, op)
		case inbox.CancelOperation:
			return current.cancelOperation(ctx, operation.ID(request.Parameters.(inbox.CancelRequest).OperationID), request.Reason)
		case inbox.UpdateSettings:
			return nil
		case inbox.InterruptAgent:
			return current.interruptAgent(ctx, request.Reason)
		case inbox.CancelChildren:
			return current.cancelChildren(ctx, request.Reason)
		case inbox.StopHard, inbox.StopWhenIdle:
			current.stopIdle("user_stop")
			current.acceptStop(request)
		}
	}
	current.clearToolGrace()
	return nil
}

func (current *coordinator) handleInboxInputs(ctx context.Context, inputs []inbox.Input) error {
	for _, input := range inputs {
		if err := current.handleInboxInput(ctx, input); err != nil {
			return err
		}
		if input.Kind == inbox.InputExternal || input.Kind == inbox.InputPeer && !quietPeer(input) {
			current.state.callModel = true
		}
		// The user comes first: a background turn or child wait of a provider that holds
		// inputs until its turn ends (Claude Code) yields, and its messages
		// are answered again with the user's input.
		if input.Kind == inbox.InputExternal && (current.backgroundTurn || current.waitingForChildren) && current.bridgeActive && current.cancelModel != nil {
			current.interruptModel()
		}
	}
	return nil
}

func slurpChannel[T any](
	ctx context.Context,
	output <-chan T,
) ([]T, error) {
	var inputs []T
	idle := time.NewTimer(slurpIdleTimeout)
	defer idle.Stop()
	for len(inputs) < slurpMaxItems {
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case input, open := <-output:
			if !open {
				return inputs, nil
			}
			inputs = append(inputs, input)
			idle.Reset(slurpIdleTimeout)
		case <-idle.C:
			return inputs, nil
		}
	}
	return inputs, nil
}

func (current *coordinator) handleModelResponse(
	ctx context.Context,
	response sessionstore.ModelResponse,
) ([]scheduledToolCall, error) {
	item := sessionstore.Item{
		Kind: sessionstore.ItemModelResponse,
		Data: response,
	}
	if err := current.storeItemInSessionStore(ctx, item); err != nil {
		return nil, err
	}
	if _, err := current.addItemToLocalState(item); err != nil {
		return nil, err
	}
	if current.state.currentTurnType == session.TurnCompaction && response.TurnID == current.state.currentTurnID {
		return nil, nil
	}
	statuses, err := current.scheduleToolCalls(ctx)
	if err != nil {
		return nil, err
	}
	current.state.callModel = toolCallStatusesRequireModelResponse(statuses)
	return statuses, nil
}

func (current *coordinator) handleOperationUpdate(
	ctx context.Context,
	update operation.Operation,
) error {
	if prior, ok := current.state.operations[update.ID]; ok {

		// The owner serializes outcomes. A committed terminal is immutable; an
		// accepted cancellation cannot be overwritten by a late worker completion.
		if operationIsTerminal(prior.Status) {
			return nil
		}
		if prior.Status == operation.StatusCanceling {
			if operationIsTerminal(update.Status) {
				if update.Type == operation.TypeRemoteJob && update.Status != operation.StatusCanceled {
					step, err := operation.CancelRemoteJob(update)
					if err != nil {
						return err
					}
					update = *step.Operation
				} else {
					update.Status = operation.StatusCanceled
				}
			} else {
				update.Status = operation.StatusCanceling
			}
		}
	}
	current.addOperationToLocalState(update)
	if err := current.storeOperationInSessionStore(ctx, update); err != nil {
		return err
	}
	return nil
}

func (current *coordinator) restore(ctx context.Context) error {
	current.pendingCancels = map[operation.ID]string{}
	current.restoredInputIDs = map[inbox.ID]bool{}
	if err := current.loadHistory(ctx); err != nil {
		return err
	}
	// A native call whose owner ended before recording any result has a fixed
	// unknown receipt. Resume does not execute that abandoned call again.
	if b, ok := current.dependencies.ContextBuilder.(interface{ UsesAppendOnlyContext() bool }); ok && b.UsesAppendOnlyContext() {
		for key, call := range current.state.toolCalls {
			if call.status != nil {
				continue
			}
			status := sessionstore.ToolCallStatus{TurnID: key.turnID, CallID: key.callID, Status: tool.CallStatus{Error: interruptedOperationResult}}
			item := sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: status}
			if err := current.storeItemInSessionStore(ctx, item); err != nil {
				return err
			}
			if _, err := current.addItemToLocalState(item); err != nil {
				return err
			}
			current.state.callModel = true
		}
	}
	for _, value := range current.dependencies.Restored.Operations {
		current.addOperationToLocalState(value)
	}
	// The previous executor lifetime ended. Close its unfinished operations once,
	// without dispatching them again; normal reconciliation publishes the result.
	for _, value := range current.state.operations {
		if !operationIsTerminal(value.Status) {
			if notice := current.dependencies.InterruptedInput; notice != nil {
				input, err := notice(value)
				if err != nil {
					return err
				}
				// Before the terminal checkpoint: if this Host also dies here,
				// the stable input ID prevents another notice on restoration.
				if input != nil && !current.restoredInputIDs[input.ID] {
					if err := current.handleInboxInputs(ctx, []inbox.Input{*input}); err != nil {
						return err
					}
				}
			}
			value.Status = operation.StatusCanceled
			if subagentOperation(value) {
				var err error
				value, err = operation.CancelUndispatched(value)
				if err != nil {
					return err
				}
			}
			// Persist the unknown receipt before closing the operation so another
			// crash cannot lose its explanation. This status grants no execution.
			for key, call := range current.state.toolCalls {
				if _, ok := call.operations[value.ID]; ok && call.status != nil && call.status.Error != interruptedOperationResult {
					status := *call.status
					status.Error = interruptedOperationResult
					if err := current.storeItemInSessionStore(ctx, sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: key.turnID, CallID: key.callID, Status: status}}); err != nil {
						return err
					}
					call.status = &status
					current.state.toolCalls[key] = call
				}
			}
			if err := current.handleOperationUpdate(ctx, value); err != nil {
				return err
			}
		} else if b, ok := current.dependencies.ContextBuilder.(interface{ ObserveOperation(operation.Operation) }); ok {
			b.ObserveOperation(value)
		}
	}
	// There is no executing model call after restoration. When its previous
	// attempt ended without a response and all effects are settled, an explicit
	// pending selection can recover that request using the chosen provider.
	if len(current.state.toolCalls) == 0 && !current.hasPendingOperations() {
		current.state.selectionBoundary = true
	}
	return nil
}

func (current *coordinator) loadHistory(ctx context.Context) (result error) {
	var history interface {
		Items(context.Context, session.ID, sessionstore.Sequence, int) (sessionstore.Page, error)
	} = current.dependencies.Sessions
	if store, ok := current.dependencies.Sessions.(sessionstore.HistoryViewStore); ok {
		view, err := store.OpenHistoryView(ctx, current.dependencies.SessionID)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := view.Close(); closeErr != nil {
				result = errors.Join(result, closeErr)
			}
		}()
		history = view
	}
	after := sessionstore.BeforeFirst
	for {
		page, err := history.Items(
			ctx,
			current.dependencies.SessionID,
			after,
			historyPageSize,
		)
		if err != nil {
			return fmt.Errorf("load session history after %d: %w", after, err)
		}
		for _, item := range page.Items {
			if item.Sequence != after+1 {
				return llm.ErrToolRecoveryRequired
			}
			if err := current.restoreItem(item); err != nil {
				return fmt.Errorf("load session item %d: %w", item.Sequence, err)
			}
			after = item.Sequence
		}
		if page.NextAfter != after {
			return llm.ErrToolRecoveryRequired
		}
		if !page.More {
			return nil
		}
		if len(page.Items) == 0 {
			return fmt.Errorf("load session history did not advance after %d", after)
		}
	}
}

func (current *coordinator) restoreItem(item sessionstore.Item) error {
	if input, ok := item.Data.(inbox.Input); ok && current.restoredInputIDs != nil {
		current.restoredInputIDs[input.ID] = true
	}
	status, ok := item.Data.(sessionstore.ToolCallStatus)
	if item.Kind == sessionstore.ItemToolCallStatus && ok && toolCallRequiresTranslator(status) {
		call, exists := current.state.toolCalls[toolCallKey{turnID: status.TurnID, callID: status.CallID}]
		if exists {
			if _, available := tool.ResolveHistory(current.dependencies.Tools, call.toolCall.Name); !available {
				return fmt.Errorf("tool %q required by recorded call %q is not available", call.toolCall.Name, status.CallID)
			}
		}
	}
	_, err := current.addItemToLocalState(item)
	if err == nil {
		if turn, ok := item.Data.(session.Turn); ok && turn.Type == session.TurnRegular && turn.ToolContinuation == "" {
			current.turnExternals = current.state.externalInputs
		}
		if turn, ok := item.Data.(session.Turn); ok && turn.ToolContinuation != "" && turn.ExternalInputWatermark > 0 {
			current.turnExternals = turn.ExternalInputWatermark
		}
		if b, ok := current.dependencies.ContextBuilder.(interface{ ObserveHistory(sessionstore.Item) }); ok {
			b.ObserveHistory(item)
		}
	}
	return err
}

func toolCallRequiresTranslator(status sessionstore.ToolCallStatus) bool {
	return status.Status.Error == "" || len(status.Status.WaitingFor) != 0 || len(status.Operations) != 0
}

func (current *coordinator) addItemToLocalState(
	item sessionstore.Item,
) (sessionstore.Item, error) {
	if b, ok := current.dependencies.ContextBuilder.(interface{ SetHistoryItem(sessionstore.Item) }); ok {
		b.SetHistoryItem(item)
	}
	switch item.Kind {
	case sessionstore.ItemHostRecord:
		r, ok := item.Data.(sessionstore.HostRecord)
		if !ok {
			return sessionstore.Item{}, fmt.Errorf("invalid host record")
		}
		if r.Kind == sessionstore.HostContext && r.Context != nil {
			if b, ok := current.dependencies.ContextBuilder.(interface {
				ApplyContext(sessionstore.ContextRecord)
			}); ok {
				b.ApplyContext(*r.Context)
			}
		}
		if b, ok := current.dependencies.ContextBuilder.(interface{ SetHistoryRuntime(string, string) }); ok {
			if r.Kind == "configuration" {
				if s := sessionstore.SelectionFromConfiguration(r.Configuration); s != nil {
					b.SetHistoryRuntime(s.Provider, s.Model)
				}
			}
			if r.Kind == sessionstore.HostRuntimeApplied && r.Selection != nil {
				b.SetHistoryRuntime(r.Selection.Provider, r.Selection.Model)
			}
		}
		// The creation-time snapshot is replayed, never rediscovered, so resume,
		// restart, and fork keep the revision the session started with.

		if r.Kind == sessionstore.HostProjectInstructions {
			if current.state.instructionsBound {
				return sessionstore.Item{}, fmt.Errorf("duplicate project instructions record")
			}
			if err := current.dependencies.ContextBuilder.SetProjectInstructions(*r.ProjectInstructions); err != nil {
				return sessionstore.Item{}, fmt.Errorf("bind project instructions: %w", err)
			}
			current.state.instructionsBound = true
		}
	case sessionstore.ItemFork:
		if _, ok := item.Data.(sessionstore.Fork); !ok {
			return sessionstore.Item{}, fmt.Errorf(
				"fork data is %T, want sessionstore.Fork",
				item.Data,
			)
		}
		// FIXME: Forks leave inherited calls without results and retain pending-input accounting.
		clear(current.state.toolCalls)
		clear(current.state.operations)
		clear(current.pendingCancels)
		current.clearToolGrace()

	case sessionstore.ItemInput:
		input, ok := item.Data.(inbox.Input)
		if !ok {
			return sessionstore.Item{}, fmt.Errorf(
				"input data is %T, want inbox.Input",
				item.Data,
			)
		}
		if err := input.Validate(); err != nil {
			return sessionstore.Item{}, fmt.Errorf("invalid input: %w", err)
		}
		deferProjection := current.bridgeActive && current.cancelModel != nil && (input.Kind == inbox.InputExternal || input.Kind == inbox.InputPeer)
		if deferProjection {
			current.deferredContext = append(current.deferredContext, item)
		}
		if input.Kind == inbox.InputExternal {
			if !deferProjection {
				if err := current.dependencies.ContextBuilder.AddExternalInput(input); err != nil {
					return sessionstore.Item{}, fmt.Errorf(
						"add input %q to context: %w",
						input.ID,
						err,
					)
				}
			}
			current.state.availableInputs++
			current.state.externalInputs++
		}
		if input.Kind == inbox.InputPeer {
			peerBuilder, ok := current.dependencies.ContextBuilder.(interface{ AddPeerInput(inbox.Input) error })
			if !ok {
				return sessionstore.Item{}, fmt.Errorf("context builder does not support peer provenance")
			}
			if !deferProjection {
				if err := peerBuilder.AddPeerInput(input); err != nil {
					return sessionstore.Item{}, err
				}
			}
			current.state.availableInputs++
			if quietPeer(input) {
				current.state.quietInputs++
			}
		}
		if input.Kind == inbox.InputControl {
			request, err := input.DecodeControlMessage()
			if err != nil {
				return sessionstore.Item{}, err
			}
			// Work a stop ended stays ended: after a restart, the model waits
			// for the user's next input instead of resending the interrupted
			// request (seen on devlore after /stop session and resume).
			// The ended request is abandoned, so a pending /model choice
			// applies to the next one.
			if request.Mode == inbox.StopHard || request.Mode == inbox.InterruptAgent {
				current.state.interrupted, current.state.interruptExternals = true, current.state.externalInputs
				current.state.selectionBoundary = true
			}
			if request.Mode == inbox.DispatchOperation {
				op, err := sessionstore.DecodeOperationIntent(input)
				if err != nil {
					return sessionstore.Item{}, err
				}
				// Inherited restrictions have no child-owned Operation for the
				// Store's ownership check. Reject retyping before either local
				// state or canonical input can replace the original evidence.

				current.addOperationToLocalState(op)
			}
			if request.Mode == inbox.CancelOperation {
				if current.pendingCancels == nil {
					current.pendingCancels = map[operation.ID]string{}
				}
				current.pendingCancels[operation.ID(request.Parameters.(inbox.CancelRequest).OperationID)] = request.Reason
			}
			current.dependencies.ContextBuilder.AddControlMessage(request)
			if request.Mode == inbox.Heartbeat {
				current.state.availableInputs++
			}
		}

	case sessionstore.ItemTurn:
		turn, ok := item.Data.(session.Turn)
		if !ok {
			return sessionstore.Item{}, fmt.Errorf(
				"turn data is %T, want session.Turn",
				item.Data,
			)
		}
		current.state.currentTurnID = turn.ID
		current.state.currentTurnType = turn.Type
		if turn.Type == session.TurnRegular {
			current.state.selectionBoundary = false
		}
		current.state.currentTurnInputs = current.state.availableInputs
		if turn.Type == session.TurnRegular && turn.ToolContinuation == "" {
			current.state.quietInputs = 0 // this request carries every input so far
		}

		if turn.ToolContinuation != "" {
			if turn.InputWatermark < 0 || turn.InputWatermark > current.state.availableInputs {
				return sessionstore.Item{}, errors.New("invalid tool continuation input watermark")
			}
			current.state.currentTurnInputs = turn.InputWatermark
		}
		current.dependencies.ContextBuilder.Commit()

	case sessionstore.ItemModelResponse:
		response, ok := item.Data.(sessionstore.ModelResponse)
		if !ok {
			return sessionstore.Item{}, fmt.Errorf(
				"model response data is %T, want sessionstore.ModelResponse",
				item.Data,
			)
		}
		if current.state.currentTurnType == session.TurnCompaction && response.TurnID == current.state.currentTurnID {
			return item, nil
		}

		// The complete output includes messages, reasoning, and tool calls.
		current.dependencies.ContextBuilder.AddModelResponse(response.Response)
		if response.TurnID == current.state.currentTurnID {
			current.state.deliveredInputs = current.state.currentTurnInputs

			current.state.selectionBoundary = true
			for _, out := range response.Response.Output {
				if out.Type == llm.ItemToolCall {
					current.state.selectionBoundary = false
				}
			}
		}
		current.addToolCallsToLocalState(response)

	case sessionstore.ItemToolCallStatus:
		status, ok := item.Data.(sessionstore.ToolCallStatus)
		if !ok {
			return sessionstore.Item{}, fmt.Errorf(
				"tool-call status data is %T, want sessionstore.ToolCallStatus",
				item.Data,
			)
		}

		for _, value := range status.Operations {
			current.addOperationToLocalState(value)

		}
		current.addToolCallOperationsToLocalState(status)
		if err := current.addToolResultToLocalState(status); err != nil {
			return sessionstore.Item{}, err
		}

	default:
		return sessionstore.Item{}, fmt.Errorf("unsupported item kind %q", item.Kind)
	}

	return item, nil
}

func (current *coordinator) addToolCallsToLocalState(response sessionstore.ModelResponse) {
	for _, output := range response.Response.Output {
		if output.Type != llm.ItemToolCall {
			continue
		}
		call := output.Data.(llm.ToolCall)
		current.state.toolCalls[toolCallKey{
			turnID: response.TurnID,
			callID: call.CallID,
		}] = toolCallState{
			toolCall:   call,
			operations: make(map[operation.ID]struct{}),
		}
	}
}

func (current *coordinator) addToolCallOperationsToLocalState(
	status sessionstore.ToolCallStatus,
) {
	call, exists := current.state.toolCalls[toolCallKey{
		turnID: status.TurnID,
		callID: status.CallID,
	}]
	if !exists {
		return
	}
	statusValue := status.Status
	call.status = &statusValue
	for _, id := range status.Status.WaitingFor {
		call.operations[id] = struct{}{}
	}
	current.state.toolCalls[toolCallKey{
		turnID: status.TurnID,
		callID: status.CallID,
	}] = call
}

func (current *coordinator) finishToolCall(
	turnID session.TurnID,
	callID string,
) {
	key := toolCallKey{turnID: turnID, callID: callID}
	delete(current.state.toolCalls, key)

	delete(current.state.graceToolCalls, key)
	if len(current.state.graceToolCalls) == 0 {
		current.clearToolGrace()
	}
	current.state.availableInputs++
}

func (current *coordinator) toolCallOperationsAreTerminal(
	turnID session.TurnID,
	callID string,
) bool {
	call := current.state.toolCalls[toolCallKey{
		turnID: turnID,
		callID: callID,
	}]
	for id := range call.operations {
		value, exists := current.state.operations[id]
		if !exists || !operationIsTerminal(value.Status) {
			return false
		}
	}
	return true
}

func (current *coordinator) addToolResultToLocalState(
	status sessionstore.ToolCallStatus,
) error {
	call, exists := current.state.toolCalls[toolCallKey{
		turnID: status.TurnID,
		callID: status.CallID,
	}]
	if !exists {
		return nil
	}
	translator, exists := current.dependencies.Tools.Resolve(call.toolCall.Name)
	if toolCallRequiresTranslator(status) {
		translator, exists = tool.ResolveHistory(current.dependencies.Tools, call.toolCall.Name)
	}
	if !exists {
		if !toolCallRequiresTranslator(status) {
			if err := current.projectToolResult(status, llm.ToolResult{CallID: status.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: status.Status.Error}}}, nil, false); err != nil {
				return err
			}
			current.rememberBridgeResult(status, llm.ToolResult{CallID: status.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: status.Status.Error}}}, nil)
			current.finishToolCall(status.TurnID, status.CallID)
		}
		return nil
	}

	operations := make([]operation.Operation, 0, len(status.Status.WaitingFor))
	for _, id := range status.Status.WaitingFor {
		if _, exists := call.operations[id]; !exists {
			return nil
		}
		value, exists := current.state.operations[id]
		if !exists {
			return nil
		}
		operations = append(operations, value)
	}
	var result llm.ToolResult
	var err error
	if status.Status.Error == interruptedOperationResult {
		result = llm.ToolResult{CallID: status.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: status.Status.Error}}}
	} else {
		result, err = translator.TranslateResult(status.CallID, status.Status, operations)
	}
	if err != nil {
		return fmt.Errorf("add tool call %q result to context: %w", status.CallID, err)
	}
	running := !current.toolCallOperationsAreTerminal(status.TurnID, status.CallID)
	if err := current.projectToolResult(status, result, translator, running); err != nil {
		return err
	}
	if !running {
		current.rememberBridgeResult(status, result, translator)
		current.finishToolCall(status.TurnID, status.CallID)
	}
	return nil
}

func (current *coordinator) addOperationToLocalState(
	value operation.Operation,
) operation.Operation {
	current.state.operations[value.ID] = value
	return current.state.operations[value.ID]
}

func (current *coordinator) scheduleToolCalls(
	ctx context.Context,
) ([]scheduledToolCall, error) {
	statuses := make([]scheduledToolCall, 0)
	for key, call := range current.state.toolCalls {
		if call.status != nil {
			continue
		}
		status, err := current.scheduleToolCall(ctx, key, call.toolCall)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, toolCallHandoff(status))
	}
	return statuses, nil
}

func (current *coordinator) scheduleToolCall(
	ctx context.Context,
	key toolCallKey,
	call llm.ToolCall,
) (sessionstore.ToolCallStatus, error) {
	translator, exists := current.dependencies.Tools.Resolve(call.Name)
	toolContext := &toolCallContext{canFinish: len(current.state.toolCalls) == 1 && !current.hasPendingOperations()}
	var status tool.CallStatus
	if err := permission.FromContext(ctx).CheckTool(call.Name); err != nil {
		status = tool.CallStatus{Error: err.Error(), Denial: permission.Failure(err)}
	} else if exists {
		status = translator.Translate(toolContext, call)
	} else {
		status = tool.ErrorStatus(fmt.Sprintf("tool %q is not available", call.Name), 0)
	}

	operations := make([]operation.Operation, 0, len(toolContext.operations))
	for _, value := range toolContext.operations {
		value.ToolName = call.Name
		operations = append(operations, current.addOperationToLocalState(value))
	}
	toolCallStatus := sessionstore.ToolCallStatus{
		TurnID:     key.turnID,
		CallID:     key.callID,
		Status:     status,
		Operations: operations,
	}
	item, err := current.addItemToLocalState(sessionstore.Item{
		Kind: sessionstore.ItemToolCallStatus,
		Data: toolCallStatus,
	})
	if err != nil {
		return sessionstore.ToolCallStatus{}, err
	}
	if err := current.storeItemInSessionStore(ctx, item); err != nil {
		return sessionstore.ToolCallStatus{}, err
	}
	return toolCallStatus, nil
}

func (current *toolCallContext) Submit(spec operation.Spec) operation.ID {
	id := operation.ID(uuid.New().String())
	current.operations = append(current.operations, operation.WithIdempotency(operation.WithDependencies(operation.Operation{
		MaxOutputLength: spec.MaxOutputLength,
		ID:              id,
		Type:            spec.Type,
		Version:         spec.Version,
		Status:          operation.StatusReady,
		State:           spec.State,
	}, spec.DependencyIDs()), spec.Idempotency))
	return id
}

func (current *coordinator) reconcileToolCalls(
	ctx context.Context,
) ([]scheduledToolCall, error) {
	completed := make([]scheduledToolCall, 0)
	for key := range current.state.toolCalls {

		if !current.toolCallOperationsAreTerminal(key.turnID, key.callID) {
			continue
		}
		call := current.state.toolCalls[key]
		if call.status == nil {
			return nil, fmt.Errorf("reconcile untranslated tool call %q in turn %q", key.callID, key.turnID)
		}
		operations := make([]operation.Operation, 0, len(call.status.WaitingFor))
		for _, id := range call.status.WaitingFor {
			operations = append(operations, current.state.operations[id])
		}
		status := sessionstore.ToolCallStatus{
			TurnID:     key.turnID,
			CallID:     key.callID,
			Status:     *call.status,
			Operations: operations,
		}
		item, err := current.addItemToLocalState(sessionstore.Item{
			Kind: sessionstore.ItemToolCallStatus,
			Data: status,
		})
		if err != nil {
			return nil, err
		}
		if err := current.storeItemInSessionStore(ctx, item); err != nil {
			return nil, err
		}
		if _, exists := current.state.toolCalls[key]; !exists {
			completed = append(completed, toolCallHandoff(status))
		}
	}
	return completed, nil
}

func (current *coordinator) storeItemInSessionStore(
	ctx context.Context,
	item sessionstore.Item,
) error {
	switch item.Kind {
	case sessionstore.ItemInput:
		input := item.Data.(inbox.Input)
		if err := current.dependencies.Sessions.AppendInput(
			ctx,
			current.dependencies.SessionID,
			input,
		); err != nil {
			return fmt.Errorf("store input %q: %w", input.ID, err)
		}

	case sessionstore.ItemTurn:
		turn := item.Data.(session.Turn)
		if err := current.dependencies.Sessions.AppendTurn(
			ctx,
			current.dependencies.SessionID,
			turn,
		); err != nil {
			return fmt.Errorf("store turn %q: %w", turn.ID, err)
		}

	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse)
		if err := current.dependencies.Sessions.AppendModelResponse(
			ctx,
			current.dependencies.SessionID,
			response,
		); err != nil {
			return fmt.Errorf("store turn %q response: %w", response.TurnID, err)
		}

	case sessionstore.ItemToolCallStatus:
		status := item.Data.(sessionstore.ToolCallStatus)
		if err := current.dependencies.Sessions.AppendToolCallStatus(
			ctx,
			current.dependencies.SessionID,
			status,
		); err != nil {
			return fmt.Errorf("store tool call %q status: %w", status.CallID, err)
		}

	default:
		return fmt.Errorf("unsupported local item kind %q", item.Kind)
	}
	return nil
}

func (current *coordinator) storeOperationInSessionStore(
	ctx context.Context,
	value operation.Operation,
) error {
	if err := current.dependencies.Sessions.SaveOperation(
		ctx,
		current.dependencies.SessionID,
		value,
	); err != nil {
		return fmt.Errorf("store operation %q: %w", value.ID, err)
	}
	if b, ok := current.dependencies.ContextBuilder.(interface{ ObserveOperation(operation.Operation) }); ok {
		b.ObserveOperation(value)
	}

	return nil
}

func (current *coordinator) dispatchOperationsToManager(ctx context.Context) error {
	for _, value := range current.state.operations {
		if err := current.dispatchOperationToManager(ctx, value); err != nil {
			return err
		}
	}
	return nil
}

func (current *coordinator) dispatchOperationToManager(ctx context.Context, value operation.Operation) error {
	if latest, ok := current.state.operations[value.ID]; ok {
		value = latest
	}

	if _, requested := current.pendingCancels[value.ID]; requested && !operationIsTerminal(value.Status) && value.Status != operation.StatusCanceling {
		value.Status = operation.StatusCanceling
		if err := current.storeOperationInSessionStore(ctx, value); err != nil {
			return err
		}
		current.addOperationToLocalState(value)
	}
	if value.Status == operation.StatusCanceling && value.Type != operation.TypeRemoteJob {
		canceled, err := operation.CancelUndispatched(value)
		if err != nil {
			return err
		}
		return current.handleOperationUpdate(ctx, canceled)
	}
	if operationIsTerminal(value.Status) {
		return nil
	}
	value.State = value.State.Clone()
	value = operation.CloneExecutionMetadata(value)
	if err := current.dependencies.Operations.Add(value); err != nil {
		return fmt.Errorf("dispatch operation %q: %w", value.ID, err)
	}
	return nil
}

func operationIsTerminal(status operation.Status) bool {
	switch status {
	case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
		return true
	default:
		return false
	}
}

func closedInputError(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s closed", name)
}

func (current *toolCallContext) CanFinish() bool { return current.canFinish }
func (current *coordinator) finished() bool {
	return current.dependencies.Finished != nil && current.dependencies.Finished()
}
func (current *coordinator) cancelOperation(ctx context.Context, id operation.ID, reason string) error {
	op, ok := current.state.operations[id]
	if !ok || operationIsTerminal(op.Status) {
		return nil
	}
	if op.Status != operation.StatusCanceling {
		op.Status = operation.StatusCanceling
		if err := current.storeOperationInSessionStore(ctx, op); err != nil {
			return err
		}
		current.addOperationToLocalState(op)
	}
	return current.dependencies.Operations.Cancel(id, reason)
}
