//go:build linux || darwin

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Capture only closed producer phases and canonical child completion counts.
// This runs once after observation, with a separate bounded diagnostic budget;
// it cannot change the success predicate or extend the interaction deadline.
func bridgeDiagnostics(t *testing.T, f *testclaude.Fixture, directory string, last *host.View, operations, responses int) interactiveDiagnostics {
	return interactiveDiagnostics{ExpectedOperations: operations, ExpectedResponses: responses, CanonicalOnly: true, Fixture: func(ctx context.Context) interactiveFixtureObservation {
		p, err := f.ProgressContext(ctx)
		out := interactiveFixtureObservation{Starts: p.Starts, Inputs: p.Inputs, ReceiptChecks: p.ReceiptChecks, ResultIntents: p.ResultIntents, ExitIntents: p.ExitIntents, ParentStarts: p.ParentStarts, ChildStarts: p.ChildStarts, LastGeneration: p.LastGeneration, ExitIntentStatusKnown: p.ExitIntentStatusKnown, ExitIntentStatus: p.ExitIntentStatus, CaptureFailed: err != nil}
		if directory == "" || last == nil {
			return out
		}
		for _, op := range last.Operations {
			plan, e := subagent.DecodePlan(op)
			if e != nil || plan.Action != "start" {
				continue
			}
			if out.ChildSessions == 2 {
				out.CaptureFailed = true
				break
			}
			out.ChildSessions++
			child, e := subagent.ReadChild(ctx, filepath.Join(directory, "sessions"), plan.ChildID, 0, 256)
			if e != nil {
				out.CaptureFailed = true
				continue
			}
			if child.Finish == nil {
				out.ChildRunningUnknown++
				continue
			}
			out.ChildFinished++
			if child.Finish.Result.Status == "failed" {
				out.ChildFailed++
			}
		}
		return out
	}}
}
