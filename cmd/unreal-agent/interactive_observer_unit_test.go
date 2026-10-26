//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type observerFixture struct {
	inspect   func(sessionstore.Sequence) (host.View, error)
	subscribe func() host.Subscription
	cursors   []sessionstore.Sequence
	subs      int
	cancels   int
}

func (f *observerFixture) Inspect(_ context.Context, _ session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if limit != 256 {
		return host.View{}, errors.New("unexpected inspect limit")
	}
	f.cursors = append(f.cursors, after)
	return f.inspect(after)
}
func (f *observerFixture) Subscribe(_ context.Context, _ session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	if after != 0 || limit != 256 || capacity != 32 {
		return host.Subscription{}, errors.New("unexpected subscription bounds")
	}
	f.subs++
	sub := f.subscribe()
	sub.Cancel = func() { f.cancels++ }
	return sub, nil
}
func observedPage(start, end sessionstore.Sequence, more bool) host.View {
	v := host.View{Generation: "synthetic", Revision: uint64(end), Running: true, History: host.HistoryPage{NextAfter: end, More: more}}
	for seq := start; seq <= end && seq > 0; seq++ {
		v.History.Items = append(v.History.Items, host.HistoryItem{Sequence: seq})
	}
	return v
}

func TestSubscribedObserverEventsAreOnlyWakeups(t *testing.T) {
	events := make(chan host.Event, 2)
	events <- host.Event{Kind: "item", Item: &host.HistoryItem{Sequence: 99}}
	f := &observerFixture{subscribe: func() host.Subscription {
		return host.Subscription{Initial: observedPage(0, 0, false), Events: events}
	}}
	f.inspect = func(after sessionstore.Sequence) (host.View, error) {
		switch len(f.cursors) {
		case 1:
			events <- host.Event{Kind: "item"}
			return observedPage(1, 1, false), nil
		case 2:
			return observedPage(2, 2, false), nil
		default:
			return observedPage(1, 2, false), nil
		}
	}
	v, err := waitSubscribedView(t.Context(), f, "fixture", func(v host.View) bool { return len(v.History.Items) == 2 })
	if err != nil || len(v.History.Items) != 2 || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{0, 1, 0}) || f.cancels != 1 {
		t.Fatal("event payload replaced canonical inspect", err, f.cursors, f.cancels)
	}
}

func TestSubscribedObserverGapAndDisconnectResync(t *testing.T) {
	for _, kind := range []string{"gap", "disconnected", "closed"} {
		t.Run(kind, func(t *testing.T) {
			events := make(chan host.Event, 1)
			if kind == "closed" {
				close(events)
			} else {
				events <- host.Event{Kind: kind}
			}
			f := &observerFixture{}
			f.subscribe = func() host.Subscription {
				v := observedPage(1, 1, false)
				if f.subs == 2 {
					v = observedPage(1, 3, false)
				}
				return host.Subscription{Initial: v, Events: events}
			}
			f.inspect = func(sessionstore.Sequence) (host.View, error) { return observedPage(1, 3, false), nil }
			v, err := waitSubscribedView(t.Context(), f, "fixture", func(v host.View) bool { return len(v.History.Items) == 3 })
			if err != nil || len(v.History.Items) != 3 || f.subs != 2 || f.cancels != 2 || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{0}) {
				t.Fatal("lost-event state was not fully resynchronized", err, f.subs, f.cursors)
			}
		})
	}
}

func TestSubscribedObserverPagesAndFinalVerification(t *testing.T) {
	f := &observerFixture{subscribe: func() host.Subscription {
		return host.Subscription{Initial: observedPage(1, 1, true)}
	}}
	f.inspect = func(after sessionstore.Sequence) (host.View, error) {
		if after == 1 {
			return observedPage(2, 2, false), nil
		}
		v := observedPage(1, 2, false)
		v.Running = false // Only this authoritative snapshot may be returned.
		return v, nil
	}
	v, err := waitSubscribedView(t.Context(), f, "fixture", func(v host.View) bool { return len(v.History.Items) == 2 })
	if err != nil || v.Running || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{1, 0}) {
		t.Fatal("partial/initial snapshot accepted without final inspect", err, f.cursors)
	}
}

func TestSubscribedObserverFinalVerificationPagesBeyondInitialLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	f := &observerFixture{subscribe: func() host.Subscription {
		return host.Subscription{Initial: observedPage(1, 256, true)}
	}}
	f.inspect = func(after sessionstore.Sequence) (host.View, error) {
		switch after {
		case 0:
			return observedPage(1, 256, true), nil
		case 256:
			v := observedPage(257, 257, false)
			v.Running = false
			return v, nil
		default:
			return host.View{}, errors.New("unexpected history cursor")
		}
	}
	v, err := waitSubscribedView(ctx, f, "fixture", func(v host.View) bool { return len(v.History.Items) == 257 })
	if err != nil || v.Running || v.History.More || len(v.History.Items) != 257 || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{256, 0, 256}) || f.cancels != 1 {
		t.Fatal("authoritative final page was not fully inspected without a new event", err, f.cursors, f.cancels)
	}
}

func TestSubscribedObserverFinalVerificationRejectsIncompletePage(t *testing.T) {
	for _, mode := range []string{"inspect_failure", "cancel", "non_advancing_page"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			f := &observerFixture{subscribe: func() host.Subscription {
				return host.Subscription{Initial: observedPage(1, 257, false)}
			}}
			failure := errors.New("synthetic final page failure")
			f.inspect = func(after sessionstore.Sequence) (host.View, error) {
				if after == 0 {
					return observedPage(1, 256, true), nil
				}
				switch mode {
				case "cancel":
					cancel()
					return host.View{}, context.Canceled
				case "non_advancing_page":
					return observedPage(0, 256, true), nil
				default:
					return host.View{}, failure
				}
			}
			v, err := waitSubscribedView(ctx, f, "fixture", func(v host.View) bool { return len(v.History.Items) == 257 })
			if err == nil || len(v.History.Items) != 0 || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{0, 256}) || f.cancels != 1 {
				t.Fatal("incomplete authoritative snapshot was accepted", err, f.cursors, f.cancels)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "inspect_failure" && !errors.Is(err, failure) {
				t.Fatal("final page failure was replaced", err)
			}
		})
	}
}

func TestInteractiveObservationContainsOnlyClosedScalarState(t *testing.T) {
	v := observedPage(1, 2, true)
	v.Generation = "synthetic-private-generation"
	v.Failure = "synthetic-private-failure-body"
	v.Operations = []operation.Operation{{ID: "synthetic-private-operation", ToolName: "synthetic-private-tool", Status: operation.StatusCompleted}, {Status: operation.StatusAwaiting}}
	description := describeInteractiveObservation(v)
	if !description.Running || !description.More || !description.FailurePresent || description.HistoryItems != 2 || description.Operations != 2 || description.CompletedOperations != 1 {
		t.Fatal("safe observation lost required state", description)
	}
	value := reflect.ValueOf(description)
	for i := range value.NumField() {
		if kind := value.Field(i).Kind(); kind != reflect.Bool && kind != reflect.Int {
			t.Fatal("observation includes an unbounded diagnostic field")
		}
	}
}

func TestSubscribedObserverTimeoutCancellationAndInspectFailure(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "inspect_failure", "initial_not_authoritative"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			events := make(chan host.Event, 1)
			initial := observedPage(0, 0, false)
			if mode == "inspect_failure" {
				events <- host.Event{Kind: "item"}
			}
			if mode == "initial_not_authoritative" {
				initial = observedPage(1, 1, false)
			}
			f := &observerFixture{subscribe: func() host.Subscription { return host.Subscription{Initial: initial, Events: events} }}
			failure := errors.New("synthetic inspect failure")
			f.inspect = func(sessionstore.Sequence) (host.View, error) {
				if mode == "inspect_failure" {
					return host.View{}, failure
				}
				return observedPage(0, 0, false), nil
			}
			if mode == "cancel" {
				cancel()
			}
			v, err := waitSubscribedView(ctx, f, "fixture", func(v host.View) bool { return len(v.History.Items) == 1 })
			want := error(context.DeadlineExceeded)
			if mode == "cancel" {
				want = context.Canceled
			} else if mode == "inspect_failure" {
				want = failure
			}
			if !errors.Is(err, want) || len(v.History.Items) != 0 || f.subs != f.cancels {
				t.Fatal("observer accepted incomplete work or leaked subscription", err, f.subs, f.cancels)
			}
		})
	}
}

func TestObservedHistoryRejectsRegressedSnapshots(t *testing.T) {
	for _, mode := range []string{"generation", "revision", "duplicate", "cursor", "no_page_progress"} {
		t.Run(mode, func(t *testing.T) {
			v := observedPage(1, 1, false)
			next := observedPage(2, 2, false)
			switch mode {
			case "generation":
				next.Generation = "different"
			case "revision":
				next.Revision = 0
			case "duplicate":
				next.History.Items[0].Sequence = 1
			case "cursor":
				next.History.NextAfter = 1
			case "no_page_progress":
				next = observedPage(0, 1, true)
			}
			if err := appendObservedHistory(&v, next); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}

func TestInteractiveRPCPredicateReceivesObserverDeadline(t *testing.T) {
	waitInteractiveContext(t, func(ctx context.Context) bool {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("RPC predicate did not receive the bounded observer deadline")
		}
		return true
	})
}

// One actual Unix HTTP Inspect remains blocked until its request is canceled.
// No successful view or second polling attempt can mask this failure.
func TestInteractiveSingleBlockedGatewayInspectHonorsDeadline(t *testing.T) {
	socket := filepath.Join(privateCLIDirectory(t), "blocked.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started, canceled := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/inspect" || calls.Add(1) != 1 {
			t.Error("unexpected Inspect RPC")
		}
		// Consume the bounded request before waiting: net/http cannot monitor
		// peer EOF while an unread request body still owns the connection read.
		if _, err := io.Copy(io.Discard, io.LimitReader(r.Body, 4096)); err != nil {
			t.Error("synthetic Inspect request read failed")
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	client := gateway.NewClient(socket)
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- waitInteractiveCondition(ctx, func(rpcContext context.Context) bool {
			_, err := client.Inspect(rpcContext, "synthetic", 0, 256)
			return err == nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocked RPC never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation was not preserved")
		}
	case <-time.After(time.Second):
		t.Fatal("single Inspect ignored observer cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("gateway transport did not cancel blocked request")
	}
	if calls.Load() != 1 {
		t.Fatal("blocked Inspect was polled or retried")
	}
}

func TestInteractiveSingleBlockedInspectHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	err := waitInteractiveCondition(ctx, func(rpcContext context.Context) bool {
		calls++
		if rpcContext != ctx {
			t.Error("RPC received another context")
		}
		<-rpcContext.Done()
		return false
	})
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("single blocked Inspect exceeded/circumvented deadline", calls, err)
	}
}

func TestInteractiveDiagnosticCaptureHasOwnBoundedContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var called atomic.Bool
	out := captureInteractiveDiagnostics(ctx, interactiveDiagnostics{Fixture: func(ctx context.Context) interactiveFixtureObservation {
		called.Store(true)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 250*time.Millisecond {
			t.Error("diagnostic context is not bounded")
		}
		<-ctx.Done()
		return interactiveFixtureObservation{}
	}})
	if !called.Load() || !out.CaptureFailed {
		t.Fatal("blocked/expired diagnostic was accepted as complete")
	}
}

func TestInteractiveBrokenDiagnosticCannotBlockObserver(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() { close(release); <-finished }()
	out := captureInteractiveDiagnostics(ctx, interactiveDiagnostics{Fixture: func(context.Context) interactiveFixtureObservation {
		close(started)
		<-release // Deliberately broken reader ignores cancellation.
		close(finished)
		return interactiveFixtureObservation{}
	}})
	select {
	case <-started:
	default:
		t.Fatal("broken diagnostic did not actually start")
	}
	if !out.CaptureFailed {
		t.Fatal("stalled diagnostic was accepted as complete")
	}
}

func TestCanonicalOnlyObserverProgressDoesNotReadHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	events := make(chan host.Event, 64)
	for range 64 {
		events <- host.Event{Kind: "progress", Progress: &host.Progress{Text: "SYNTHETIC_PRIVATE_DRAFT"}}
	}
	f := &observerFixture{subscribe: func() host.Subscription { return host.Subscription{Initial: observedPage(0, 0, false), Events: events} }}
	f.inspect = func(sessionstore.Sequence) (host.View, error) { return observedPage(0, 0, false), nil }
	trace := interactiveTrace{}
	_, err := waitSubscribedView(ctx, diagnosticObserver{interactiveObserver: f, trace: &trace, canonicalOnly: true}, "fixture", func(host.View) bool { return false })
	if !errors.Is(err, context.DeadlineExceeded) || len(f.cursors) != 0 || trace.Wakeups != 64 || trace.IgnoredProgress != 64 || f.cancels != 1 {
		t.Fatal("progress-only hints triggered canonical Inspect or fabricated completion", len(f.cursors), trace.Wakeups, f.cancels, err)
	}
}

func TestCanonicalOnlyObserverKeepsMixedCanonicalWakeupsAndFreshVerification(t *testing.T) {
	for _, kind := range []string{"item", "operation", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			events := make(chan host.Event, 3)
			events <- host.Event{Kind: "progress"}
			events <- host.Event{Kind: kind}
			events <- host.Event{Kind: "progress"}
			f := &observerFixture{subscribe: func() host.Subscription { return host.Subscription{Initial: observedPage(0, 0, false), Events: events} }}
			f.inspect = func(sessionstore.Sequence) (host.View, error) { return observedPage(1, 1, false), nil }
			trace := interactiveTrace{}
			v, err := waitSubscribedView(t.Context(), diagnosticObserver{interactiveObserver: f, trace: &trace, canonicalOnly: true}, "fixture", func(v host.View) bool { return len(v.History.Items) == 1 })
			if err != nil || len(v.History.Items) != 1 || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{0, 0}) || f.cancels != 1 {
				t.Fatal("canonical mixed hint/fresh verification lost", err, f.cursors, f.cancels)
			}
		})
	}
}

func TestCanonicalOnlyObserverGapResyncAndCompletedInitialNeedNoFurtherWake(t *testing.T) {
	for _, gap := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed-initial", true: "progress-gap"}[gap], func(t *testing.T) {
			events := make(chan host.Event, 2)
			events <- host.Event{Kind: "progress"}
			events <- host.Event{Kind: "gap"}
			f := &observerFixture{}
			f.subscribe = func() host.Subscription {
				if gap && f.subs == 1 {
					return host.Subscription{Initial: observedPage(0, 0, false), Events: events}
				}
				return host.Subscription{Initial: observedPage(1, 1, false)}
			}
			f.inspect = func(sessionstore.Sequence) (host.View, error) { return observedPage(1, 1, false), nil }
			trace := interactiveTrace{}
			v, err := waitSubscribedView(t.Context(), diagnosticObserver{interactiveObserver: f, trace: &trace, canonicalOnly: true}, "fixture", func(v host.View) bool { return len(v.History.Items) == 1 })
			want := 1
			if gap {
				want = 2
			}
			if err != nil || len(v.History.Items) != 1 || f.subs != want || f.cancels != want || !reflect.DeepEqual(f.cursors, []sessionstore.Sequence{0}) {
				t.Fatal("canonical resync/final completion requires another event", err, f.subs, f.cancels, f.cursors)
			}
		})
	}
}

func TestSubscribedObserverRejectsLateSuccessfulSnapshots(t *testing.T) {
	for _, stage := range []string{"subscribe", "inspect", "predicate"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &observerFixture{}
			f.subscribe = func() host.Subscription {
				if stage == "subscribe" {
					cancel()
				}
				return host.Subscription{Initial: observedPage(1, 1, false)}
			}
			f.inspect = func(sessionstore.Sequence) (host.View, error) {
				if stage == "inspect" {
					cancel()
				}
				return observedPage(1, 1, false), nil
			}
			v, err := waitSubscribedView(ctx, f, "fixture", func(v host.View) bool {
				if stage == "predicate" {
					cancel()
				}
				return len(v.History.Items) == 1
			})
			if !errors.Is(err, context.Canceled) || len(v.History.Items) != 0 || f.cancels != 1 {
				t.Fatal("late successful snapshot escaped observer cancellation", err, len(v.History.Items), f.cancels)
			}
		})
	}
}
