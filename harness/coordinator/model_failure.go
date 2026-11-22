package coordinator

import (
	"context"
	"log/slog"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

// Inputs in a failed request are still unacknowledged. They stay in context
// for the next request, but cannot wake the model again by themselves.
type failedModelTurn struct {
	cause                error
	inputs, peers, quiet int
}

func (current *coordinator) failModelTurn(ctx context.Context, turn session.TurnID, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !current.hasStartedChild() {
		return cause
	}
	if record := current.dependencies.RecordActivity; record != nil {
		activity := llm.ActivityRecord{Kind: llm.ActivityModelFailure, ID: string(turn), EndedAt: time.Now().UTC(), Reason: secretguard.RedactSensitive(cause.Error())}
		if err := record(ctx, activity); err != nil {
			slog.Warn("record model failure", "error", err)
		}
	}
	current.noteModelFailure(turn, cause)
	return nil
}

func (current *coordinator) hasStartedChild() bool {
	// A child can finish before the failed model response is processed. Its
	// report still needs the live Coordinator, even with a terminal start.
	for _, op := range current.state.operations {
		if op.ToolName == "SubagentStart" && subagentOperation(op) {
			return true
		}
	}
	return false
}

func (current *coordinator) noteModelFailure(turn session.TurnID, cause error) {
	state := &current.state
	inputs, peers, quiet := state.availableInputs, state.peerInputs, state.quietInputs
	if turn != "" {
		if turn != state.currentTurnID {
			return
		}
		inputs, peers, quiet = state.currentTurnInputs, state.turnPeerInputs, state.turnQuietInputs
	}
	current.failedModel = &failedModelTurn{cause: cause, inputs: inputs - state.deliveredInputs, peers: peers, quiet: quiet}
	state.callModel = false
}
