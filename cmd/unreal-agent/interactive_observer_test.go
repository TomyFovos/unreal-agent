//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type interactiveObserver interface {
	Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)
	Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error)
}

type interactivePhase string

const (
	interactiveUnspecified interactivePhase = "unspecified"
	bridgeInitialFinal     interactivePhase = "bridge_initial_final"
	bridgeStopIdle         interactivePhase = "bridge_stop_idle"
	bridgeResumeFinal      interactivePhase = "bridge_resume_final"
	bridgeChildrenFinal    interactivePhase = "bridge_children_final"
)

// Keep fixture diagnostics closed and payload-free. In particular, a failed
// predicate must not expose IDs, arguments, file contents, or provider text.
type interactiveObservation struct {
	Running, More, FailurePresent                                                              bool
	HistoryItems, Responses, Operations, CompletedOperations                                   int
	ReadyOperations, AwaitingOperations, FailedOperations, CanceledOperations, OtherOperations int
	ProgressPresent, ProgressDone                                                              bool
	ProgressAttempt                                                                            int
}

func describeInteractiveObservation(v host.View) interactiveObservation {
	description := interactiveObservation{
		Running: v.Running, More: v.History.More, FailurePresent: v.Failure != "",
		HistoryItems: len(v.History.Items), Responses: responseCount(v), Operations: len(v.Operations),
	}
	for _, op := range v.Operations {
		switch op.Status {
		case operation.StatusCompleted:
			description.CompletedOperations++
		case operation.StatusReady:
			description.ReadyOperations++
		case operation.StatusAwaiting:
			description.AwaitingOperations++
		case operation.StatusFailed:
			description.FailedOperations++
		case operation.StatusCanceled:
			description.CanceledOperations++
		default:
			description.OtherOperations++
		}
	}
	if v.Progress != nil {
		description.ProgressPresent = true
		description.ProgressDone = v.Progress.Done
		description.ProgressAttempt = int(min(v.Progress.Attempt, 1_000_000))
	}
	return description
}

// Match the production TUI's snapshot/cursor/resync observation contract without
// its rendering. Keep the original interaction deadline and predicate. Every
// accepted completion is checked against a fresh full Inspect, never an event.
func inspectSubscribedInteractive(t *testing.T, reader interactiveObserver, id session.ID, ready func(host.View) bool) host.View {
	t.Helper()
	return inspectSubscribedInteractivePhase(t, interactiveUnspecified, reader, id, ready)
}

type interactiveDiagnostics struct {
	ExpectedOperations, ExpectedResponses int
	CanonicalOnly                         bool
	Fixture                               func(context.Context) interactiveFixtureObservation
}

// Only closed enums and bounded scalar counts enter observer diagnostics.
// Unknown process exit status remains unknown; result intent is not process exit.
type interactiveFixtureObservation struct {
	Starts, Inputs, ReceiptChecks, ResultIntents, ExitIntents      int
	ParentStarts, ChildStarts, LastGeneration                      int
	ProcessExitStatusKnown                                         bool
	ExitIntentStatusKnown                                          bool
	ExitIntentStatus                                               int
	ChildSessions, ChildFinished, ChildFailed, ChildRunningUnknown int
	CaptureFailed                                                  bool
}

type interactiveTrace struct {
	Stage                                                         string
	Subscriptions, Inspections, Wakeups, Resyncs, IgnoredProgress int
}

type diagnosticObserver struct {
	interactiveObserver
	trace         *interactiveTrace
	canonicalOnly bool
}

func (d diagnosticObserver) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	d.trace.Stage = "subscribe"
	d.trace.Subscriptions = min(d.trace.Subscriptions+1, 1_000_000)
	return d.interactiveObserver.Subscribe(ctx, id, after, limit, capacity)
}
func (d diagnosticObserver) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	d.trace.Stage = "inspect_incremental"
	if after == 0 {
		d.trace.Stage = "inspect_verify"
	}
	d.trace.Inspections = min(d.trace.Inspections+1, 1_000_000)
	return d.interactiveObserver.Inspect(ctx, id, after, limit)
}
func noteInteractiveEvent(reader interactiveObserver, resync bool) {
	if d, ok := reader.(diagnosticObserver); ok {
		d.trace.Wakeups = min(d.trace.Wakeups+1, 1_000_000)
		if resync {
			d.trace.Resyncs = min(d.trace.Resyncs+1, 1_000_000)
		}
	}
}
func noteInteractiveWait(reader interactiveObserver) {
	if d, ok := reader.(diagnosticObserver); ok {
		d.trace.Stage = "wait_event"
	}
}

func captureInteractiveDiagnostics(ctx context.Context, expected interactiveDiagnostics) interactiveFixtureObservation {
	if expected.Fixture == nil {
		return interactiveFixtureObservation{}
	}
	diagnosticContext, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	// A diagnostic reader is observational only. Do not let even a broken
	// reader extend the interaction deadline; its bounded result channel
	// cannot publish a late snapshot as accepted completion.
	result := make(chan interactiveFixtureObservation, 1)
	go func() { result <- expected.Fixture(diagnosticContext) }()
	select {
	case out := <-result:
		if diagnosticContext.Err() != nil {
			out.CaptureFailed = true
		}
		return out
	case <-diagnosticContext.Done():
		return interactiveFixtureObservation{CaptureFailed: true}
	}
}

// Progress is ephemeral and cannot satisfy the explicitly canonical-only final
// and stopped predicates. Every other event remains a wakeup to Inspect.
func ignoreInteractiveProgress(reader interactiveObserver, event host.Event) bool {
	if d, ok := reader.(diagnosticObserver); ok && d.canonicalOnly && event.Kind == "progress" {
		d.trace.IgnoredProgress = min(d.trace.IgnoredProgress+1, 1_000_000)
		return true
	}
	return false
}

func inspectObservedView(ctx context.Context, reader interactiveObserver, id session.ID, after sessionstore.Sequence) (host.View, error) {
	if err := context.Cause(ctx); err != nil {
		return host.View{}, err
	}
	view, err := reader.Inspect(ctx, id, after, 256)
	if cause := context.Cause(ctx); cause != nil {
		return host.View{}, cause
	}
	return view, err
}

func interactiveReady(ctx context.Context, view host.View, ready func(host.View) bool) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	accepted := ready(view)
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	return accepted, nil
}

func inspectSubscribedInteractivePhase(t *testing.T, phase interactivePhase, reader interactiveObserver, id session.ID, ready func(host.View) bool, options ...interactiveDiagnostics) host.View {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	var last interactiveObservation
	var expected interactiveDiagnostics
	if len(options) == 1 {
		expected = options[0]
	}
	switch phase {
	case bridgeInitialFinal, bridgeStopIdle, bridgeResumeFinal, bridgeChildrenFinal:
	default:
		phase = interactiveUnspecified
	}
	trace := interactiveTrace{Stage: "start"}
	view, err := waitSubscribedView(ctx, diagnosticObserver{interactiveObserver: reader, trace: &trace, canonicalOnly: expected.CanonicalOnly}, id, func(v host.View) bool {
		last = describeInteractiveObservation(v)
		return ready(v)
	})
	fixture := captureInteractiveDiagnostics(t.Context(), expected)
	if err != nil {
		reason := "observation_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "deadline"
		}
		if errors.Is(err, context.Canceled) {
			reason = "cancel"
		}
		t.Fatalf("interactive observation failed phase=%s reason=%s deadline_ms=5000 elapsed_ms=%d expected_operations=%d expected_responses=%d last=%+v trace=%+v fixture=%+v", phase, reason, time.Since(started).Milliseconds(), expected.ExpectedOperations, expected.ExpectedResponses, last, trace, fixture)
	}
	if fixture.CaptureFailed {
		t.Fatalf("interactive diagnostics incomplete phase=%s last=%+v trace=%+v fixture=%+v", phase, last, trace, fixture)
	}
	if phase != interactiveUnspecified {
		t.Logf("interactive condition complete phase=%s deadline_ms=5000 elapsed_ms=%d expected_operations=%d expected_responses=%d last=%+v trace=%+v fixture=%+v", phase, time.Since(started).Milliseconds(), expected.ExpectedOperations, expected.ExpectedResponses, last, trace, fixture)
	}
	return view
}

func waitSubscribedView(ctx context.Context, reader interactiveObserver, id session.ID, ready func(host.View) bool) (host.View, error) {
	for ctx.Err() == nil {
		sub, err := reader.Subscribe(ctx, id, 0, 256, 32)
		if err != nil {
			return host.View{}, err
		}
		view, done, err := observeInteractiveSubscription(ctx, reader, id, sub, ready)
		if cause := context.Cause(ctx); cause != nil {
			return host.View{}, cause
		}
		if err != nil || done {
			return view, err
		}
		// A gap or disconnect invalidates incremental state. Subscribe again
		// from zero for an atomic initial snapshot, under the same deadline.
	}
	return host.View{}, context.Cause(ctx)
}

func observeInteractiveSubscription(ctx context.Context, reader interactiveObserver, id session.ID, sub host.Subscription, ready func(host.View) bool) (host.View, bool, error) {
	defer sub.Cancel()
	view := sub.Initial
	verify := func() (host.View, bool, error) {
		if err := context.Cause(ctx); err != nil {
			return host.View{}, false, err
		}
		for view.History.More {
			next, err := inspectObservedView(ctx, reader, id, view.History.NextAfter)
			if err != nil {
				return host.View{}, false, err
			}
			if err = appendObservedHistory(&view, next); err != nil {
				return host.View{}, false, err
			}
		}
		accepted, err := interactiveReady(ctx, view, ready)
		if err != nil || !accepted {
			return host.View{}, false, err
		}
		final, err := inspectObservedView(ctx, reader, id, 0)
		if err != nil {
			return host.View{}, false, err
		}
		// A fresh authoritative snapshot has the same bounded page size as the
		// incremental reads. Its first page alone cannot verify a predicate
		// whose committed result lives on a later page. Complete this snapshot
		// now; a completed Host need not emit another wakeup event.
		for final.History.More {
			next, err := inspectObservedView(ctx, reader, id, final.History.NextAfter)
			if err != nil {
				return host.View{}, false, err
			}
			if err = appendObservedHistory(&final, next); err != nil {
				return host.View{}, false, err
			}
		}
		accepted, err = interactiveReady(ctx, final, ready)
		if err != nil {
			return host.View{}, false, err
		}
		if accepted {
			return final, true, nil
		}
		view = final
		return host.View{}, false, nil
	}
	if final, done, err := verify(); done || err != nil {
		return final, done, err
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return host.View{}, false, err
		}
		noteInteractiveWait(reader)
		select {
		case <-ctx.Done():
			return host.View{}, false, context.Cause(ctx)
		case event, open := <-sub.Events:
			noteInteractiveEvent(reader, !open || event.Kind == "gap" || event.Kind == "disconnected")
			if !open || event.Kind == "gap" || event.Kind == "disconnected" {
				return host.View{}, false, nil
			}
			if ignoreInteractiveProgress(reader, event) {
				continue
			}
			// Coalesce hints; Inspect supplies committed history and the latest
			// operations. Even a final-looking event cannot satisfy ready.
			drain := true
			for drain {
				select {
				case <-ctx.Done():
					return host.View{}, false, context.Cause(ctx)
				case next, open := <-sub.Events:
					noteInteractiveEvent(reader, !open || next.Kind == "gap" || next.Kind == "disconnected")
					if !open || next.Kind == "gap" || next.Kind == "disconnected" {
						return host.View{}, false, nil
					}
					ignoreInteractiveProgress(reader, next)
				default:
					drain = false
				}
			}
			next, err := inspectObservedView(ctx, reader, id, view.History.NextAfter)
			if err != nil {
				return host.View{}, false, err
			}
			if err = appendObservedHistory(&view, next); err != nil {
				return host.View{}, false, err
			}
			if final, done, err := verify(); done || err != nil {
				return final, done, err
			}
		}
	}
}

func appendObservedHistory(view *host.View, next host.View) error {
	if next.Generation != view.Generation || next.Revision < view.Revision {
		return errors.New("interactive snapshot identity regressed")
	}
	after := view.History.NextAfter
	for _, item := range next.History.Items {
		if item.Sequence <= after {
			return errors.New("interactive history cursor regressed")
		}
		after = item.Sequence
	}
	if next.History.NextAfter < after || next.History.More && next.History.NextAfter <= view.History.NextAfter {
		return errors.New("interactive history page did not advance")
	}
	items := append(view.History.Items, next.History.Items...)
	*view = next
	view.History.Items = items
	return nil
}
