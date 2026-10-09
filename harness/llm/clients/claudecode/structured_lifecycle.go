package claudecode

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type generationOwnerKey struct{}

const structuredTerminationGrace = 2 * time.Second

type structuredParseResult struct {
	proposal actionProposal
	response llm.Response
	err      error
}

// A candidate is not an accepted proposal. Only validated physical EOF and
// process-group cleanup together permit return to the Host tool rendezvous.
// Generation deadlines end at the candidate; owner cancellation never does.
func collectStructuredGeneration(generation, owner context.Context, cmd *exec.Cmd, reader, writer *os.File, schema *actionSchema, model llm.Model, input string, probe bool, grace time.Duration, cancel context.CancelFunc, progress ...func(llm.Progress)) (actionProposal, llm.Response, error) {
	if grace <= 0 {
		grace = structuredTerminationGrace
	}
	start := time.Now()
	candidateReady := make(chan struct{})
	var candidate <-chan struct{} = candidateReady
	var once sync.Once
	var candidateSeen atomic.Bool
	var candidateAt time.Time
	parsed := make(chan structuredParseResult, 1)
	waited := make(chan error, 1)
	go func() {
		p, r, err := runStructuredProtocolFinished(reader, writer, schema, input, probe, func() { once.Do(func() { candidateAt = time.Now(); candidateSeen.Store(true); close(candidateReady) }) }, progress...)
		parsed <- structuredParseResult{p, r, err}
	}()
	go func() { err := cmd.Wait(); _ = killProcessGroup(cmd); waited <- err }()
	var output structuredParseResult
	var waitErr error
	var parseDone, processDone, ending, signaled bool
	var termination, deadline *time.Timer
	var terminate, expires <-chan time.Time
	generationDone := generation.Done()
	beginEnd := func() {
		if ending {
			return
		}
		ending = true
		generationDone = nil
		candidate = nil
		_ = writer.Close() // SDK request-stream EOF is the first exit request.
		termination = time.NewTimer(grace / 2)
		terminate = termination.C
		deadline = time.NewTimer(grace)
		expires = deadline.C
	}
	defer func() {
		if termination != nil {
			termination.Stop()
		}
		if deadline != nil {
			deadline.Stop()
		}
	}()
	fail := func(err error) (actionProposal, llm.Response, error) {
		cancel()
		_ = killProcessGroup(cmd)
		// Closing an unread pipe is allowed only on failure, never to manufacture
		// a successful EOF. No candidate survives this path.
		_ = reader.Close()
		_ = writer.Close()
		if !processDone {
			select {
			case <-waited:
				processDone = true
			case <-time.After(structuredTerminationGrace):
				return actionProposal{}, llm.Response{}, structuredError("structured_process_reap_timeout", structuredReapDeadline)
			}
		}
		if !parseDone {
			select {
			case <-parsed:
			case <-time.After(structuredTerminationGrace):
			}
		}
		return actionProposal{}, llm.Response{}, err
	}
	for {
		if owner.Err() != nil {
			return fail(owner.Err())
		}
		if parseDone && processDone {
			if output.err != nil {
				return fail(output.err)
			}
			if waitErr != nil && !(signaled && terminatedByOwner(waitErr)) {
				return fail(&Error{Code: "subprocess_failure"})
			}
			if candidateSeen.Load() {
				output.response.Generation = &llm.GenerationStatistics{Model: model.ID, Tokens: llm.ReportedTokens(output.response.Usage), DurationMicros: candidateAt.Sub(start).Microseconds(), Source: "client_clock"}
			}
			return output.proposal, output.response, nil
		}
		select {
		case <-owner.Done():
			return fail(owner.Err())
		case <-generationDone:
			if candidateSeen.Load() {
				beginEnd()
				continue
			}
			return fail(structuredError("structured_generation_timeout", structuredGenerationDeadline))
		case <-candidate:
			beginEnd()
		case output = <-parsed:
			parseDone = true
			parsed = nil
			if output.err != nil {
				return fail(output.err)
			}
			beginEnd() // also handles initialize-only probes, with no Action.
		case waitErr = <-waited:
			processDone = true
			waited = nil
		case <-terminate:
			terminate = nil
			if !processDone {
				signaled = true
				_ = cmd.Cancel()
			}
		case <-expires:
			return fail(structuredError("structured_termination_timeout", structuredTerminationDeadline))
		}
	}
}
