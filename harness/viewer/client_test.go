package viewer

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"sync"
	"testing"
	"time"
)

type fakeReader struct {
	mu            sync.Mutex
	subscriptions []host.Subscription
	pages         map[sessionstore.Sequence]host.View
	subscribed    int
	canceled      int
}

func (f *fakeReader) Inspect(_ context.Context, _ session.ID, after sessionstore.Sequence, _ int) (host.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.pages[after]
	if !ok {
		return host.View{}, ErrInvalidPage
	}
	return v, nil
}
func (f *fakeReader) Subscribe(_ context.Context, _ session.ID, _ sessionstore.Sequence, _, _ int) (host.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.subscriptions) == 0 {
		return host.Subscription{}, errors.New("offline")
	}
	s := f.subscriptions[0]
	f.subscriptions = f.subscriptions[1:]
	f.subscribed++
	s.Cancel = func() { f.mu.Lock(); f.canceled++; f.mu.Unlock() }
	return s, nil
}
func events(values ...host.Event) <-chan host.Event {
	ch := make(chan host.Event, len(values))
	for _, v := range values {
		ch <- v
	}
	close(ch)
	return ch
}
func TestWatchGapReconnectAndPagedCanonicalDedup(t *testing.T) {
	first := view("s", "g1", 1, response(1, "a", 10, 2))
	second := view("s", "g2", 5, response(1, "a", 10, 2))
	second.History.More = true
	final := view("s", "g2", 7, response(2, "b", 20, 4))
	reader := &fakeReader{
		subscriptions: []host.Subscription{
			{Initial: first, Events: events(host.Event{Generation: "g1", Revision: 3, Kind: "gap"})},
			{Initial: second, Events: events(host.Event{Generation: "g1", Revision: 999, Kind: "stopped"}, host.Event{Generation: "g2", Revision: 6, Kind: "item", Item: ptr(response(2, "b", 20, 4))}, host.Event{Generation: "g2", Revision: 8, Kind: "stopped"})},
		},
		pages: map[sessionstore.Sequence]host.View{1: final},
	}
	c := NewClient(reader, nil, nil)
	c.RetryDelay = time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	notices := 0
	must(t, c.Watch(ctx, "s", func(n Notice) { notices++ }))
	got := row(t, c.Model, "s")
	if got.Usage.Input != 30 || got.Usage.Responses != 2 || got.Cursor != 2 || got.Generation != "g2" {
		t.Fatalf("%+v", got)
	}
	if reader.subscribed != 2 || reader.canceled != 2 || notices < 3 {
		t.Fatalf("lifecycle %d %d %d", reader.subscribed, reader.canceled, notices)
	}
	if got.Runtime != RuntimeUnknown {
		t.Fatal("liveness survives disconnect")
	}
	page, err := c.History(t.Context(), "s", 1, 10)
	must(t, err)
	if len(page.Items) != 1 || row(t, c.Model, "s").Usage.Input != 30 {
		t.Fatal("history page altered aggregate")
	}
}
func TestWatchCancellationAndDuplicateWatch(t *testing.T) {
	reader := &fakeReader{subscriptions: []host.Subscription{{Initial: view("s", "g", 0), Events: make(chan host.Event)}}}
	c := NewClient(reader, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- c.Watch(ctx, "s", func(Notice) { close(started) }) }()
	<-started
	if err := c.Watch(t.Context(), "s", nil); !errors.Is(err, ErrWatching) {
		t.Fatal(err)
	}
	if err := c.Refresh(t.Context(), "s"); !errors.Is(err, ErrWatching) {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if reader.canceled != 1 {
		t.Fatal("subscription leaked")
	}
}

type fakeControls struct {
	mu      sync.Mutex
	calls   []ControlRequest
	actions []string
	entered chan struct{}
	release chan struct{}
	err     error
}

func (f *fakeControls) call(ctx context.Context, action string, r ControlRequest) (host.Receipt, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.actions = append(f.actions, action)
	err := f.err
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return host.Receipt{}, context.Cause(ctx)
		}
	}
	return host.Receipt{ID: r.InputID, Sequence: 10}, err
}
func (f *fakeControls) SteerChild(ctx context.Context, r ControlRequest) (host.Receipt, error) {
	return f.call(ctx, "steer", r)
}
func (f *fakeControls) CancelChild(ctx context.Context, r ControlRequest) (host.Receipt, error) {
	return f.call(ctx, "cancel", r)
}
func (f *fakeControls) ResumeChild(ctx context.Context, r ControlRequest) error {
	_, err := f.call(ctx, "resume", r)
	return err
}
func controlClient(t *testing.T, controls *fakeControls) *Client {
	t.Helper()
	m := New(Options{DecodeChild: childDecoder})
	must(t, m.Replace("parent", parentView(operation.StatusAwaiting)))
	return NewClient(nil, m, controls)
}
func TestControlsStableReceiptNoOptimisticStateAndConflict(t *testing.T) {
	f := &fakeControls{}
	c := controlClient(t, f)
	receipt, err := c.Steer(t.Context(), "child", "request", "please inspect")
	must(t, err)
	again, err := c.Steer(t.Context(), "child", "request", "please inspect")
	must(t, err)
	if receipt != again || len(f.calls) != 1 {
		t.Fatal("duplicate forwarded")
	}
	got := f.calls[0]
	if got.ParentID != "parent" || got.ParentGeneration != "parent-generation" || got.OperationID != "op-child" || got.ChildID != "child" || got.InputID != "request" {
		t.Fatalf("%+v", got)
	}
	if _, err = c.Cancel(t.Context(), "child", "request", "different"); !errors.Is(err, host.ErrConflict) {
		t.Fatal(err)
	}
	_, err = c.Cancel(t.Context(), "child", "cancel", "stop")
	must(t, err)
	if row(t, c.Model, "child").ParentOperationStatus != operation.StatusAwaiting {
		t.Fatal("optimistically canceled")
	}
	c.Model.Disconnect("parent")
	if _, err = c.Steer(t.Context(), "child", "new", "text"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
func TestDuplicateResumeAndRetryAfterFailure(t *testing.T) {
	f := &fakeControls{entered: make(chan struct{}, 1), release: make(chan struct{})}
	c := controlClient(t, f)
	done := make(chan error, 1)
	go func() { done <- c.Resume(t.Context(), "child", "resume") }()
	<-f.entered
	if err := c.Resume(t.Context(), "child", "another"); !errors.Is(err, ErrControlPending) {
		t.Fatal(err)
	}
	close(f.release)
	must(t, <-done)
	must(t, c.Resume(t.Context(), "child", "resume"))
	if len(f.calls) != 1 {
		t.Fatal("resume duplicated")
	}
	f.entered = nil
	f.err = errors.New("transport disconnected")
	if err := c.Resume(t.Context(), "child", "retry"); err == nil {
		t.Fatal("expected error")
	}
	f.err = nil
	must(t, c.Resume(t.Context(), "child", "retry"))
	if len(f.calls) != 3 || f.calls[1].InputID != f.calls[2].InputID {
		t.Fatal("retry identity lost")
	}
}

func TestCommittedRetryAfterTerminalAndDisconnect(t *testing.T) {
	f := &fakeControls{}
	c := controlClient(t, f)
	want, err := c.Steer(t.Context(), "child", "request", "finish now")
	must(t, err)
	must(t, c.Model.Replace("parent", parentView(operation.StatusCompleted)))
	c.Model.Disconnect("parent")
	got, err := c.Steer(t.Context(), "child", "request", "finish now")
	must(t, err)
	if got != want || len(f.calls) != 1 {
		t.Fatal("committed retry lost its receipt")
	}
	if _, err = c.Steer(t.Context(), "child", "request", "changed"); !errors.Is(err, host.ErrConflict) {
		t.Fatal("changed retry was accepted", err)
	}
	if _, err = c.Steer(t.Context(), "child", "new", "finish now"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("new terminal control was accepted", err)
	}
}
